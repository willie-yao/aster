package remediationinvestigation

import (
	"errors"
	"regexp"
	"strings"
)

type resultError struct {
	code string
	err  error
}

func (e *resultError) Error() string { return "remediation investigation result rejected: " + e.code }
func (e *resultError) Unwrap() error { return e.err }

// ErrorCode returns a bounded content-free remediation investigation error code.
func ErrorCode(err error) string {
	var resultErr *resultError
	if errors.As(err, &resultErr) {
		return resultErr.code
	}
	return ""
}

func validationErrorCode(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if match := regexp.MustCompile(`unknown field path ([A-Za-z0-9_\[\].-]{1,160})`).FindStringSubmatch(message); len(match) == 2 {
		path := strings.NewReplacer("[", "_", "]", "", ".", "_").Replace(match[1])
		return "unknown_field_" + strings.ToLower(path)
	}
	if match := regexp.MustCompile(`unknown field "([A-Za-z0-9_-]{1,64})"`).FindStringSubmatch(message); len(match) == 2 {
		return "unknown_field_" + match[1]
	}
	for _, item := range []struct{ contains, code string }{
		{"duplicate field", "duplicate_field"},
		{"unknown field", "unknown_field"},
		{"result field", "missing_field"},
		{"result version must be the integer", "invalid_version"},
		{"result version", "version"},
		{"cause assessment", "cause_assessment"},
		{"typed non-actionable reason", "non_actionable_reason"},
		{"non_actionable_reason to null", "candidate_non_actionable_conflict"},
		{"must support or refine", "unsupported_cause"},
		{"candidate field", "candidate_missing_field"},
		{"candidate kind", "candidate_kind"},
		{"candidate target type", "candidate_kind"},
		{"required-call candidate", "required_call_target"},
		{"symbol-addition candidate", "symbol_target"},
		{"prow environment candidate", "prow_environment_target"},
		{"configuration field", "configuration_target"},
		{"engine-issued source evidence ID", "missing_source_evidence"},
		{"was not issued by the investigation ledger", "unknown_evidence_id"},
		{"duplicate evidence ID", "duplicate_evidence_id"},
		{"evidence ID", "evidence_id"},
		{"evidence catalog", "evidence_catalog"},
		{"evidence", "evidence"},
		{"decode remediation investigation result", "decode"},
	} {
		if strings.Contains(message, item.contains) {
			return item.code
		}
	}
	return "invalid_result"
}
