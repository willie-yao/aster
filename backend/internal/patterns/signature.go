package patterns

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/aggregator"
	"github.com/willie-yao/aster/backend/internal/models"
)

// CausalGroupSignature derives the durable identity of one causal group from the
// failures its builds actually produced, so the same cause keeps one identity
// across passes even though model prose and the member build set both change.
//
// The preimage is the job plus the dominant (test provenance, failure location,
// normalized error message) pair among the group's builds. Normalization is
// aggregator.NormalizeErrorSignature, which strips timestamps but preserves
// numbers, because a status or exit code is often the only thing separating two
// causes that need different answers. The full normalized message is hashed
// rather than the truncated aggregator.HashError, whose 32 bits are sized for
// display dedup rather than for gating a remediation verdict.
//
// It returns "" when no member build is still in the window with an analyzed
// failure, when that failure is a build-level stand-in whose message is a
// constant, or when it carries no message to key on. Callers preserve any prior
// signature rather than losing the cause's identity.
//
// The identity is a strong symptom fingerprint, not a proof of shared cause. Two
// groups on the same job that resolve to one signature both lose it, and reuse of
// a stored verdict is capped, so an identical symptom with a different cause
// costs a re-investigation rather than a wrong answer.
func CausalGroupSignature(detail models.JobDetail, group models.PatternCausalGroup) string {
	subject := signatureSubject(detail)
	if subject == "" {
		return ""
	}
	dominant := dominantFailureKey(detail, group)
	if dominant == "" {
		return ""
	}
	return signatureOf("", subject, dominant)
}

// BuildRecurrenceSignature derives the identity a failure is counted under when
// measuring how long it has been recurring. It keys on the same test provenance
// and failure location as CausalGroupSignature, but normalizes the message with
// aggregator.NormalizeErrorRecurrence, which collapses every run of digits.
//
// The two identities differ on purpose. A verdict is a conclusion about a cause,
// so its identity keeps numbers: answering "status 401" must never answer
// "status 503". Counting recurrence is a question about history, and a message
// like "Timed out after 3600.001s" carries a number that changes every run, so a
// number-preserving identity would mint a new cause each time and report every
// long-lived flake as brand new. Recall is what matters here, and the cost of
// over-grouping is a count that is too generous rather than a wrong answer
// applied to the wrong failure.
//
// It returns "" when the run has no analyzed failure, when that failure is a
// build-level stand-in whose message is a constant, or when it carries no
// message to key on.
func BuildRecurrenceSignature(detail models.JobDetail, run *models.BuildResult) string {
	subject := signatureSubject(detail)
	if subject == "" {
		return ""
	}
	key := failureKey(RepresentativeAnalyzedFailure(run), aggregator.NormalizeErrorRecurrence)
	if key == "" {
		return ""
	}
	return signatureOf(recurrenceDomain, subject, key)
}

// recurrenceDomain keeps recurrence identities out of the verdict key space. A
// message with no digits normalizes identically under both rules, so without a
// domain the two would hash to one ledger entry and a group's builds would
// inflate the count published for the recurrence identity.
const recurrenceDomain = "recurrence"

func signatureOf(domain, subject, key string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + subject + "\x00" + key))
	return hex.EncodeToString(sum[:8])
}

// ApplyCausalGroupSignatures assigns durable signatures to a freshly correlated
// job's causal groups. A group whose builds have aged out keeps the signature it
// was published with, so a cause that returns is still recognized as the same cause.
func ApplyCausalGroupSignatures(detail *models.JobDetail) {
	assignCausalGroupSignatures(detail, false)
}

// BackfillCausalGroupSignatures fills in signatures a retained pattern is missing
// without recomputing the ones it already carries. A retained verdict keeps its
// original grouping while its builds age out of the window, so recomputing could
// flip the dominant failure and break the continuity the signature exists to hold.
func BackfillCausalGroupSignatures(detail *models.JobDetail) {
	assignCausalGroupSignatures(detail, true)
}

func assignCausalGroupSignatures(detail *models.JobDetail, onlyMissing bool) {
	if detail == nil {
		return
	}
	groups := allCausalGroups(detail)
	effective := make([]string, len(groups))
	counts := map[string]int{}
	for index, group := range groups {
		effective[index] = effectiveSignature(*detail, *group, onlyMissing)
		if effective[index] != "" {
			counts[effective[index]]++
		}
	}
	for index, group := range groups {
		// Two groups resolving to one signature means it identifies more than one
		// cause, and a verdict recorded under it could answer the wrong one.
		// Neither keeps it: losing memory costs a re-investigation, conflating
		// causes gives a wrong answer.
		if signature := effective[index]; counts[signature] == 1 {
			group.Signature = signature
		} else {
			group.Signature = ""
		}
	}
}

// effectiveSignature recomputes a signature, or keeps the published one when the
// group's builds have aged out and it can no longer be derived.
func effectiveSignature(detail models.JobDetail, group models.PatternCausalGroup, onlyMissing bool) string {
	if onlyMissing && group.Signature != "" {
		return group.Signature
	}
	if computed := CausalGroupSignature(detail, group); computed != "" {
		return computed
	}
	return group.Signature
}

func allCausalGroups(detail *models.JobDetail) []*models.PatternCausalGroup {
	var out []*models.PatternCausalGroup
	for i := range detail.PatternAnalyses {
		groups := detail.PatternAnalyses[i].CausalGroups
		for j := range groups {
			out = append(out, &groups[j])
		}
	}
	return out
}

// signatureSubject mirrors models.PatternID's job-then-subject fallback so a job
// without a stable id still yields one signature per subject.
func signatureSubject(detail models.JobDetail) string {
	if jobID := strings.TrimSpace(detail.JobID); jobID != "" {
		return jobID
	}
	return strings.TrimSpace(detail.Name)
}

// dominantFailureKey returns the most frequent (test, normalized message) pair
// across the group's builds, breaking ties on the key itself so the result is
// deterministic regardless of build ordering. A failure with no discriminating
// evidence is skipped.
func dominantFailureKey(detail models.JobDetail, group models.PatternCausalGroup) string {
	runs := make(map[string]*models.BuildResult, len(detail.Runs))
	for index := range detail.Runs {
		runs[detail.Runs[index].BuildID] = &detail.Runs[index]
	}
	counts := map[string]int{}
	for _, buildID := range group.Builds {
		run, ok := runs[strings.TrimSpace(buildID)]
		if !ok {
			continue
		}
		if key := failureKey(RepresentativeAnalyzedFailure(run), aggregator.NormalizeErrorSignature); key != "" {
			counts[key]++
		}
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// failureKey is the discriminating preimage of one analyzed failure. normalize
// decides how much of the message survives, which is what separates the
// verdict-bearing identity from the recurrence-counting one. It returns "" for a
// failure with no evidence worth keying on.
func failureKey(failure *models.TestCase, normalize func(string) string) string {
	if failure == nil || failure.Source == models.TestCaseSourceBuild {
		// A build-level stand-in carries a constant synthesized name and
		// message (see models.NewProwJobExecutionFailure), so every such
		// failure in a job would key to one signature regardless of cause.
		return ""
	}
	name := strings.TrimSpace(failure.Name)
	normalized := strings.TrimSpace(normalize(failure.FailureMessage))
	if name == "" || normalized == "" {
		return ""
	}
	// Suite, class, and failure location are stable per test and separate
	// same-named tests in different suites or shards, so they cost no recall
	// and narrow what one identity can cover.
	return strings.Join([]string{
		strings.TrimSpace(failure.SuiteName),
		strings.TrimSpace(failure.ClassName),
		name,
		FailureLocationFile(failure.FailureLocation),
		normalized,
	}, "\x00")
}
