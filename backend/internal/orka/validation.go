package orka

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// AnalysisValidation is the exact final analysis validated by validate_analysis.
type AnalysisValidation struct {
	Summary       string   `json:"summary"`
	RootCause     string   `json:"root_cause"`
	Severity      string   `json:"severity"`
	IsTransient   bool     `json:"is_transient"`
	SuggestedFix  string   `json:"suggested_fix"`
	RelevantFiles []string `json:"relevant_files"`
}

// EvidenceText is the consumer-skill trigger input for this analysis.
func (a AnalysisValidation) EvidenceText() string {
	return strings.Join([]string{
		a.RootCause,
		a.Summary,
		a.SuggestedFix,
		strings.Join(a.RelevantFiles, "\n"),
	}, "\n")
}

// AnalysisValidationToken authenticates the canonical validated result.
func AnalysisValidationToken(key, taskName string, a AnalysisValidation, gcsBytes int) string {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(taskName) == "" {
		return ""
	}
	data := canonicalAnalysisValidation(a)
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("orka-analysis-validation-v2\x00"))
	_, _ = mac.Write([]byte(taskName))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(data)
	_, _ = fmt.Fprintf(mac, "\x00%d", gcsBytes)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyAnalysisValidationToken reports whether token authenticates a.
func VerifyAnalysisValidationToken(key, taskName string, a AnalysisValidation, gcsBytes int, token string) bool {
	expected := AnalysisValidationToken(key, taskName, a, gcsBytes)
	return len(token) == len(expected) && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

// ValidationKeyHash fingerprints the private validation key in Task identity.
func ValidationKeyHash(key string) string {
	sum := sha256.Sum256([]byte("orka-analysis-validation-key-v1\x00" + key))
	return hex.EncodeToString(sum[:])
}

func canonicalAnalysisValidation(a AnalysisValidation) []byte {
	a.Summary = strings.TrimSpace(a.Summary)
	a.RootCause = strings.TrimSpace(a.RootCause)
	a.Severity = strings.TrimSpace(a.Severity)
	a.SuggestedFix = strings.TrimSpace(a.SuggestedFix)
	files := append([]string(nil), a.RelevantFiles...)
	for i := range files {
		files[i] = strings.TrimSpace(files[i])
	}
	sort.Strings(files)
	a.RelevantFiles = files
	data, _ := json.Marshal(a)
	return data
}
