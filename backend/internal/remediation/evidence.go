package remediation

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aggregator"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/junit"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/resolve"
)

func patternMatchesDetail(pattern models.PatternAnalysis, detail models.JobDetail) bool {
	if pattern.JobID != "" {
		return detail.JobID == pattern.JobID
	}
	return detail.Name == pattern.Subject
}

// EvidenceForPattern captures deterministic failures from the pattern's builds.
func EvidenceForPattern(pattern models.PatternAnalysis, details []models.JobDetail) Evidence {
	buildSet := make(map[string]struct{}, len(pattern.SharedBuilds))
	for _, buildID := range pattern.SharedBuilds {
		buildSet[buildID] = struct{}{}
	}
	tests := map[string]*TestEvidence{}
	for _, detail := range details {
		if !patternMatchesDetail(pattern, detail) {
			continue
		}
		for _, run := range detail.Runs {
			if len(buildSet) > 0 {
				if _, ok := buildSet[run.BuildID]; !ok {
					continue
				}
			}
			for _, test := range run.TestCases {
				if test.Status != "failed" {
					continue
				}
				identity := junit.Identity(test)
				entry := tests[identity]
				if entry == nil {
					entry = &TestEvidence{
						Identity: identity, Name: test.Name, SuiteName: test.SuiteName,
						ClassName: test.ClassName,
					}
					tests[identity] = entry
				}
				message := test.FailureMessage + "\n" + test.FailureBody
				hash := aggregator.HashError(aggregator.NormalizeErrorMessage(message))
				if entry.ErrorHash == "" {
					entry.ErrorHash = hash
				}
				entry.BuildIDs = appendUnique(entry.BuildIDs, run.BuildID)
				entry.JUnitFiles = appendUnique(entry.JUnitFiles, test.JUnitFile)
			}
		}
		break
	}
	outTests := make([]TestEvidence, 0, len(tests))
	for _, test := range tests {
		sort.Strings(test.BuildIDs)
		sort.Strings(test.JUnitFiles)
		outTests = append(outTests, *test)
	}
	sort.Slice(outTests, func(i, j int) bool { return outTests[i].Identity < outTests[j].Identity })
	rootCause := strings.TrimSpace(pattern.SharedRootCause)
	sum := sha256.Sum256([]byte(strings.ToLower(rootCause)))
	return Evidence{
		PatternID: pattern.ID, RootCause: rootCause, RootCauseHash: hex.EncodeToString(sum[:8]),
		BuildWatermark: resolve.Watermark(pattern),
		AffectedBuilds: append([]string(nil), pattern.SharedBuilds...), Tests: outTests,
		EvidenceCreated: nowString(),
	}
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func classificationForPattern(pattern models.PatternAnalysis, details []models.JobDetail) string {
	evidence := EvidenceForPattern(pattern, details)
	persistent := false
	for _, detail := range details {
		if !patternMatchesDetail(pattern, detail) {
			continue
		}
		for _, test := range evidence.Tests {
			info := aggregator.ClassifyFailure(test.Name, runsForTestIdentity(detail.Runs, test.Identity), 3)
			if info.Classification == models.ClassificationFlaky {
				return string(models.ClassificationFlaky)
			}
			if info.Classification == models.ClassificationPersistent {
				persistent = true
			}
		}
		break
	}
	if persistent {
		return string(models.ClassificationPersistent)
	}
	return "pattern"
}

func runsForTestIdentity(runs []models.BuildResult, identity string) []models.BuildResult {
	out := make([]models.BuildResult, 0, len(runs))
	for _, run := range runs {
		filtered := run
		filtered.TestCases = nil
		for _, test := range run.TestCases {
			for _, candidate := range junit.Identities(test) {
				if candidate == identity {
					filtered.TestCases = append(filtered.TestCases, test)
					break
				}
			}
		}
		out = append(out, filtered)
	}
	return out
}

// UntrackedPatterns excludes findings already represented by a remediation attempt.
func UntrackedPatterns(state *State, patterns []models.PatternAnalysis, details []models.JobDetail) []models.PatternAnalysis {
	if state == nil || len(state.Remediations) == 0 {
		return append([]models.PatternAnalysis(nil), patterns...)
	}
	out := make([]models.PatternAnalysis, 0, len(patterns))
	for _, pattern := range patterns {
		id := pattern.ID
		if id == "" {
			id = models.PatternID(pattern)
		}
		tracked := false
		currentEvidence := EvidenceForPattern(pattern, details)
		for _, entry := range state.Remediations {
			if entry == nil || len(entry.Attempts) == 0 {
				continue
			}
			if entry.ID == id || entry.FindingID == id ||
				(entry.JobID == pattern.JobID && evidenceOverlaps(entry.Evidence, currentEvidence)) {
				tracked = true
				break
			}
		}
		if !tracked {
			out = append(out, pattern)
		}
	}
	return out
}
