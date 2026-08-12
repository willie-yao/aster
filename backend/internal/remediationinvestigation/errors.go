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
		{"result version is missing", "missing_version"},
		{"result version must be the integer 1", "invalid_version"},
		{"result version", "version"},
		{"classification", "classification"},
		{"cause assessment", "cause_assessment"},
		{"non-actionable classification", "non_actionable_proposal"},
		{"actionable classification requires", "missing_proposal"},
		{"must support or refine", "unsupported_cause"},
		{"requires source evidence", "missing_source_evidence"},
		{"was not read during the evidence phase", "unread_evidence"},
		{"does not match a frozen analysis", "unknown_analysis"},
		{"does not match its frozen build", "unknown_build"},
		{"proposal target", "target_shape"},
		{"target path", "target_path"},
		{"remediation safety policy", "unsafe_remediation"},
		{"evidence", "evidence"},
		{"decode remediation investigation result", "decode"},
	} {
		if strings.Contains(message, item.contains) {
			return item.code
		}
	}
	return "invalid_result"
}
