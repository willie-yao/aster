package models

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/willie-yao/aster/backend/internal/textutil"
)

// FailureIdentityKey combines test provenance, failure file, and normalized error.
// Build-level stand-ins and empty messages do not identify a failure.
func FailureIdentityKey(failure *TestCase, normalize func(string) string) string {
	if failure == nil || failure.Source == TestCaseSourceBuild {
		return ""
	}
	name := strings.TrimSpace(failure.Name)
	message := strings.TrimSpace(normalize(failure.FailureMessage))
	if name == "" || message == "" {
		return ""
	}
	file, _, _ := strings.Cut(strings.TrimSpace(failure.FailureLocation), ":")
	return strings.Join([]string{
		strings.TrimSpace(failure.SuiteName),
		strings.TrimSpace(failure.ClassName),
		name,
		file,
		message,
	}, "\x00")
}

// FailureRecurrenceSignature identifies one analyzed symptom across runs.
// It is separate from verdict identity and tolerates run-varying numbers.
func FailureRecurrenceSignature(detail JobDetail, failure *TestCase) string {
	subject := strings.TrimSpace(detail.JobID)
	if subject == "" {
		subject = strings.TrimSpace(detail.Name)
	}
	if subject == "" || failure == nil || !AnalysisHasUsableDiagnosis(failure.AIAnalysis) {
		return ""
	}
	key := FailureIdentityKey(failure, textutil.NormalizeErrorRecurrence)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("recurrence\x00" + subject + "\x00" + key))
	return hex.EncodeToString(sum[:8])
}
