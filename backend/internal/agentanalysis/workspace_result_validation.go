package agentanalysis

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/willie-yao/aster/backend/internal/models"
)

const (
	WorkspaceResultAccepted             = "accepted"
	WorkspaceResultAcceptedWithWarnings = "accepted_with_warnings"
	WorkspaceResultRejected             = "rejected"

	WorkspaceInvalidResultJSON        = "invalid_result_json"
	WorkspaceInvalidResultVersion     = "invalid_result_version"
	WorkspaceInvalidAnalysisText      = "invalid_analysis_text"
	WorkspaceInvalidArtifactCount     = "invalid_artifact_citation_count"
	WorkspaceInvalidArtifactPath      = "invalid_artifact_path"
	WorkspaceInvalidArtifactLineRange = "invalid_artifact_line_range"
	WorkspaceInvalidArtifactOverlap   = "invalid_artifact_overlap"
	WorkspaceInvalidSourceCount       = "invalid_source_citation_count"
	WorkspaceInvalidSourcePath        = "invalid_source_path"
	WorkspaceInvalidSourceLineRange   = "invalid_source_line_range"
	WorkspaceInvalidSourceOverlap     = "invalid_source_overlap"
	WorkspaceInvalidRelevantFile      = "invalid_relevant_file"
	WorkspaceInvalidClassification    = "invalid_classification"

	// WorkspaceResultRejectedReason is the content-free failure text for a rejected result.
	WorkspaceResultRejectedReason = "workspace analysis result rejected"
)

// WorkspaceResultValidation records bounded deterministic result validation.
type WorkspaceResultValidation struct {
	Status string   `json:"status,omitempty"`
	Codes  []string `json:"codes,omitempty"`
}

type workspaceInvalidResultError struct {
	code string
}

func (e *workspaceInvalidResultError) Error() string {
	return fmt.Sprintf("%s: %s", ErrInvalidResult, e.code)
}

func (e *workspaceInvalidResultError) Unwrap() error { return ErrInvalidResult }

func invalidWorkspaceResult(code string) error {
	return &workspaceInvalidResultError{code: code}
}

// WorkspaceInvalidResultCode returns the bounded validator code carried by err.
func WorkspaceInvalidResultCode(err error) string {
	var target *workspaceInvalidResultError
	if errors.As(err, &target) {
		return target.code
	}
	return ""
}

func rejectedWorkspaceResult(err error) WorkspaceResultValidation {
	code := WorkspaceInvalidResultCode(err)
	if code == "" {
		code = WorkspaceInvalidResultJSON
	}
	return WorkspaceResultValidation{Status: WorkspaceResultRejected, Codes: []string{code}}
}

func acceptedWorkspaceResult(warnings map[string]bool) WorkspaceResultValidation {
	if len(warnings) == 0 {
		return WorkspaceResultValidation{Status: WorkspaceResultAccepted}
	}
	codes := make([]string, 0, len(warnings))
	for code := range warnings {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return WorkspaceResultValidation{Status: WorkspaceResultAcceptedWithWarnings, Codes: codes}
}

func mergeWorkspaceResultValidation(first, second WorkspaceResultValidation) WorkspaceResultValidation {
	warnings := map[string]bool{}
	for _, validation := range []WorkspaceResultValidation{first, second} {
		for _, code := range validation.Codes {
			warnings[code] = true
		}
	}
	return acceptedWorkspaceResult(warnings)
}

func validateWorkspaceResultValidation(validation WorkspaceResultValidation, allowEmpty bool) error {
	if validation.Status == "" {
		if allowEmpty && len(validation.Codes) == 0 {
			return nil
		}
		return fmt.Errorf("workspace result validation status is required")
	}
	if len(validation.Codes) > 16 {
		return fmt.Errorf("workspace result validation codes exceed the bound")
	}
	if !slices.IsSorted(validation.Codes) || hasDuplicateStrings(validation.Codes) {
		return fmt.Errorf("workspace result validation codes are not canonical")
	}
	for _, code := range validation.Codes {
		if !validWorkspaceResultValidationCode(code) {
			return fmt.Errorf("workspace result validation code is invalid")
		}
	}
	switch validation.Status {
	case WorkspaceResultAccepted:
		if len(validation.Codes) != 0 {
			return fmt.Errorf("accepted workspace result contains warnings")
		}
	case WorkspaceResultAcceptedWithWarnings:
		if len(validation.Codes) == 0 {
			return fmt.Errorf("workspace result warnings are empty")
		}
	case WorkspaceResultRejected:
		if len(validation.Codes) != 1 {
			return fmt.Errorf("rejected workspace result must contain one reason code")
		}
	default:
		return fmt.Errorf("workspace result validation status is invalid")
	}
	return nil
}

func validWorkspaceResultValidationCode(value string) bool {
	switch value {
	case WorkspaceInvalidResultJSON,
		WorkspaceInvalidResultVersion,
		WorkspaceInvalidAnalysisText,
		WorkspaceInvalidArtifactCount,
		WorkspaceInvalidArtifactPath,
		WorkspaceInvalidArtifactLineRange,
		WorkspaceInvalidArtifactOverlap,
		WorkspaceInvalidSourceCount,
		WorkspaceInvalidSourcePath,
		WorkspaceInvalidSourceLineRange,
		WorkspaceInvalidSourceOverlap,
		WorkspaceInvalidRelevantFile,
		WorkspaceInvalidClassification:
		return true
	default:
		return false
	}
}

func hasDuplicateStrings(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] == values[index] {
			return true
		}
	}
	return false
}

// WorkspaceAnalysisDisposition maps one accepted workspace result to the public
// display disposition without granting publication or action authority.
func WorkspaceAnalysisDisposition(analysis WorkspaceAnalysis, validation WorkspaceResultValidation, requireSourceEvidence bool) (string, []string) {
	if validation.Status == WorkspaceResultRejected || validation.Status == "" {
		return "", nil
	}
	warnings := map[string]bool{}
	grounded := len(analysis.EvidenceCitations) > 0
	if !grounded {
		warnings[models.AnalysisWarningArtifactGrounding] = true
	}
	if requireSourceEvidence {
		verified := len(analysis.SourceCitations) > 0
		for _, citation := range analysis.SourceCitations {
			verified = verified && citation.Verified
		}
		if !verified {
			grounded = false
			warnings[models.AnalysisWarningSourceGrounding] = true
		}
	}
	for _, code := range validation.Codes {
		switch code {
		case WorkspaceInvalidArtifactCount, WorkspaceInvalidArtifactPath,
			WorkspaceInvalidArtifactLineRange, WorkspaceInvalidArtifactOverlap:
			grounded = false
			warnings[models.AnalysisWarningArtifactGrounding] = true
		case WorkspaceInvalidSourceCount, WorkspaceInvalidSourcePath,
			WorkspaceInvalidSourceLineRange, WorkspaceInvalidSourceOverlap,
			WorkspaceInvalidRelevantFile:
			grounded = false
			warnings[models.AnalysisWarningSourceGrounding] = true
		case WorkspaceInvalidClassification:
			grounded = false
			warnings[models.AnalysisWarningClassification] = true
		case WorkspaceInvalidAnalysisText:
			grounded = false
			warnings[models.AnalysisWarningInvestigation] = true
		default:
			return "", nil
		}
	}
	if len(analysis.UnresolvedDetails) > 0 {
		warnings[models.AnalysisWarningInvestigation] = true
	}
	codes := make([]string, 0, len(warnings))
	for code := range warnings {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	if grounded {
		return models.AnalysisDispositionGrounded, codes
	}
	return models.AnalysisDispositionPreliminary, codes
}
