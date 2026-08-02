package fetcher

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/evidenceplan"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const failureCohortSignatureVersion = "v1"

var (
	failureCohortWhitespace = regexp.MustCompile(`\s+`)
	failureCohortAddress    = regexp.MustCompile(`(?i)0x[0-9a-f]+`)
	failureCohortUUID       = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
)

type analysisFailureCohort struct {
	Signature string
	Work      []aiWork
}

type analysisFailureCohortTelemetry struct {
	Groups              int
	Candidates          int
	PotentialTasksSaved int
	LargestGroup        int
}

func groupAnalysisFailureCohorts(work []aiWork) []analysisFailureCohort {
	groups := make([]analysisFailureCohort, 0)
	indexes := make(map[string]int)
	for _, item := range work {
		signature, ok := analysisFailureCohortSignature(item)
		if !ok {
			continue
		}
		index, found := indexes[signature]
		if !found {
			indexes[signature] = len(groups)
			groups = append(groups, analysisFailureCohort{Signature: signature})
			index = len(groups) - 1
		}
		groups[index].Work = append(groups[index].Work, item)
	}
	out := groups[:0]
	for _, group := range groups {
		if len(group.Work) > 1 {
			out = append(out, group)
		}
	}
	return out
}

func analysisFailureCohortStats(work []aiWork) analysisFailureCohortTelemetry {
	var telemetry analysisFailureCohortTelemetry
	for _, group := range groupAnalysisFailureCohorts(work) {
		size := len(group.Work)
		telemetry.Groups++
		telemetry.Candidates += size
		telemetry.PotentialTasksSaved += size - 1
		telemetry.LargestGroup = max(telemetry.LargestGroup, size)
	}
	return telemetry
}

func analysisFailureCohortSignature(item aiWork) (string, bool) {
	if item.tc == nil || item.run == nil || item.tc.Source == models.TestCaseSourceBuild ||
		strings.TrimSpace(item.jobID) == "" || strings.TrimSpace(item.run.BuildID) == "" {
		return "", false
	}
	testCase := evidenceplan.CanonicalTestCase(*item.tc)
	message := normalizeFailureCohortText(testCase.FailureMessage, testCase.Name)
	body := normalizeFailureCohortText(testCase.FailureBody, testCase.Name)
	if message == "" && body == "" {
		return "", false
	}
	identity := strings.Join([]string{
		failureCohortSignatureVersion,
		item.jobID,
		item.run.BuildID,
		testCase.Source,
		testCase.JUnitFile,
		testCase.ClassName,
		message,
		body,
	}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:]), true
}

func normalizeFailureCohortText(value, testName string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if testName = strings.TrimSpace(testName); len(testName) >= 8 {
		value = strings.ReplaceAll(value, testName, "<test>")
	}
	value = failureCohortUUID.ReplaceAllString(value, "<uuid>")
	value = failureCohortAddress.ReplaceAllString(value, "<addr>")
	return strings.TrimSpace(failureCohortWhitespace.ReplaceAllString(value, " "))
}

type analysisExecution struct {
	Work []aiWork
}

func planAnalysisExecutions(work []aiWork) []analysisExecution {
	cohorts := groupAnalysisFailureCohorts(work)
	bySignature := make(map[string][]aiWork, len(cohorts))
	for _, cohort := range cohorts {
		bySignature[cohort.Signature] = cohort.Work
	}
	emitted := make(map[string]bool, len(cohorts))
	executions := make([]analysisExecution, 0, len(work))
	for _, item := range work {
		signature, ok := analysisFailureCohortSignature(item)
		if ok && len(bySignature[signature]) > 1 {
			if !emitted[signature] {
				executions = append(executions, analysisExecution{Work: bySignature[signature]})
				emitted[signature] = true
			}
			continue
		}
		executions = append(executions, analysisExecution{Work: []aiWork{item}})
	}
	return executions
}
