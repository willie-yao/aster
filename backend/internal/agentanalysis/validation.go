package agentanalysis

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const (
	maxResultBytes        = 128 << 10
	maxSummaryBytes       = 2 << 10
	maxRootCauseBytes     = 24 << 10
	maxSuggestedFixBytes  = 12 << 10
	maxUnresolvedBytes    = 4 << 10
	maxRelevantFiles      = 20
	maxEvidenceCitations  = 20
	maxSourceCitations    = 10
	maxUnresolvedDetails  = 20
	maxCitationQuoteBytes = 2 << 10
	maxCitationLines      = 200
)

// EvidenceCitation identifies exact lines in one frozen evidence excerpt.
type EvidenceCitation struct {
	ExcerptID string `json:"excerpt_id"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Quote     string `json:"quote"`
}

// Analysis is the validated private experimental result.
type Analysis struct {
	Summary           string                         `json:"summary"`
	IsTransient       bool                           `json:"is_transient"`
	RootCause         string                         `json:"root_cause"`
	Severity          string                         `json:"severity"`
	SuggestedFix      string                         `json:"suggested_fix"`
	RelevantFiles     []string                       `json:"relevant_files,omitempty"`
	EvidenceCitations []EvidenceCitation             `json:"evidence_citations,omitempty"`
	SourceCitations   []sourceinvestigation.Citation `json:"source_citations,omitempty"`
	UnresolvedDetails []string                       `json:"unresolved_details,omitempty"`
}

type analysisEnvelope struct {
	Version           int                      `json:"version"`
	ContractVersion   string                   `json:"contract_version"`
	Summary           string                   `json:"summary"`
	IsTransient       *bool                    `json:"is_transient"`
	RootCause         string                   `json:"root_cause"`
	Severity          string                   `json:"severity"`
	SuggestedFix      string                   `json:"suggested_fix"`
	RelevantFiles     []string                 `json:"relevant_files"`
	EvidenceCitations []EvidenceCitation       `json:"evidence_citations"`
	SourceCitations   []sourceCitationEnvelope `json:"source_citations"`
	UnresolvedDetails []string                 `json:"unresolved_details"`
}

type sourceCitationEnvelope struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Quote     string `json:"quote"`
}

func parseAndValidateAnalysis(ctx context.Context, raw string, bundle EvidenceBundle, reader sourceinvestigation.Reader) (Analysis, error) {
	if raw == "" || len(raw) > maxResultBytes || !utf8.ValidString(raw) || strings.IndexByte(raw, 0) >= 0 {
		return Analysis{}, newShadowResultError(ShadowStatusMalformedResult, fmt.Errorf("output is empty, invalid, or oversized"))
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return Analysis{}, newShadowResultError(ShadowStatusMalformedResult, err)
	}
	decoder := json.NewDecoder(io.LimitReader(strings.NewReader(raw), maxResultBytes+1))
	decoder.DisallowUnknownFields()
	var parsed analysisEnvelope
	if err := decoder.Decode(&parsed); err != nil {
		return Analysis{}, newShadowResultError(ShadowStatusMalformedResult, fmt.Errorf("decode output: %v", err))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Analysis{}, newShadowResultError(ShadowStatusMalformedResult, fmt.Errorf("output contains trailing data"))
	}
	if parsed.Version != ResultSchemaVersion || parsed.ContractVersion != ContractVersion {
		return Analysis{}, fmt.Errorf("%w: unsupported result or contract version", ErrInvalidResult)
	}
	if parsed.IsTransient == nil {
		return Analysis{}, fmt.Errorf("%w: is_transient is required", ErrInvalidResult)
	}
	analysis := Analysis{
		Summary: strings.TrimSpace(parsed.Summary), IsTransient: *parsed.IsTransient,
		RootCause: strings.TrimSpace(parsed.RootCause), Severity: strings.TrimSpace(parsed.Severity),
		SuggestedFix: strings.TrimSpace(parsed.SuggestedFix), RelevantFiles: slices.Clone(parsed.RelevantFiles),
		EvidenceCitations: slices.Clone(parsed.EvidenceCitations), UnresolvedDetails: slices.Clone(parsed.UnresolvedDetails),
	}
	for i := range analysis.UnresolvedDetails {
		analysis.UnresolvedDetails[i] = strings.TrimSpace(analysis.UnresolvedDetails[i])
	}
	for _, citation := range parsed.SourceCitations {
		analysis.SourceCitations = append(analysis.SourceCitations, sourceinvestigation.Citation{
			Path: strings.TrimSpace(citation.Path), LineStart: citation.LineStart,
			LineEnd: citation.LineEnd, Quote: citation.Quote,
		})
	}
	if err := validateAnalysisText(analysis); err != nil {
		return Analysis{}, err
	}
	if err := validateEvidenceCitations(analysis.EvidenceCitations, bundle.Excerpts); err != nil {
		return Analysis{}, err
	}
	if len(analysis.SourceCitations) > 0 {
		if len(analysis.SourceCitations) > maxSourceCitations {
			return Analysis{}, fmt.Errorf("%w: source citations exceed %d", ErrInvalidResult, maxSourceCitations)
		}
		verified, err := sourceinvestigation.VerifyCitations(ctx, reader, bundle.Source, analysis.SourceCitations)
		if err != nil {
			return Analysis{}, fmt.Errorf("%w: %v", ErrInvalidResult, err)
		}
		analysis.SourceCitations = verified
	}
	if len(analysis.EvidenceCitations) == 0 {
		return Analysis{}, fmt.Errorf("%w: at least one verified artifact citation is required", ErrInvalidResult)
	}
	relevantFiles, err := validateRelevantFiles(analysis.RelevantFiles, analysis, bundle.Excerpts)
	if err != nil {
		return Analysis{}, err
	}
	analysis.RelevantFiles = relevantFiles
	return analysis, nil
}

func rejectDuplicateJSONFields(raw string) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate JSON field %q", key)
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delim)
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateAnalysisText(analysis Analysis) error {
	fields := []struct {
		name  string
		value string
		max   int
	}{
		{"summary", analysis.Summary, maxSummaryBytes},
		{"root_cause", analysis.RootCause, maxRootCauseBytes},
		{"suggested_fix", analysis.SuggestedFix, maxSuggestedFixBytes},
	}
	for _, field := range fields {
		if field.value == "" || !utf8.ValidString(field.value) || len(field.value) > field.max {
			return fmt.Errorf("%w: %s is empty, invalid, or oversized", ErrInvalidResult, field.name)
		}
	}
	switch analysis.Severity {
	case "Critical", "High", "Medium", "Low", "Transient-Ignore":
	default:
		return fmt.Errorf("%w: unsupported severity %q", ErrInvalidResult, analysis.Severity)
	}
	if analysis.IsTransient != (analysis.Severity == "Transient-Ignore") {
		return fmt.Errorf("%w: transient classification and severity disagree", ErrInvalidResult)
	}
	if len(analysis.UnresolvedDetails) > maxUnresolvedDetails {
		return fmt.Errorf("%w: unresolved details exceed %d", ErrInvalidResult, maxUnresolvedDetails)
	}
	for i, detail := range analysis.UnresolvedDetails {
		if strings.TrimSpace(detail) == "" || !utf8.ValidString(detail) || len(detail) > maxUnresolvedBytes {
			return fmt.Errorf("%w: unresolved detail %d is empty, invalid, or oversized", ErrInvalidResult, i)
		}
	}
	return nil
}

func validateEvidenceCitations(citations []EvidenceCitation, excerpts []EvidenceExcerpt) error {
	if len(citations) > maxEvidenceCitations {
		return fmt.Errorf("%w: evidence citations exceed %d", ErrInvalidResult, maxEvidenceCitations)
	}
	byID := make(map[string]EvidenceExcerpt, len(excerpts))
	for _, excerpt := range excerpts {
		byID[excerpt.ID] = excerpt
	}
	seen := map[string]bool{}
	for i, citation := range citations {
		if citation.ExcerptID == "" || citation.ExcerptID != strings.TrimSpace(citation.ExcerptID) {
			return fmt.Errorf("%w: evidence citation %d has invalid excerpt id", ErrInvalidResult, i)
		}
		excerpt, ok := byID[citation.ExcerptID]
		if !ok {
			return fmt.Errorf("%w: evidence citation %d references unknown excerpt %q", ErrInvalidResult, i, citation.ExcerptID)
		}
		citation.Quote = strings.ReplaceAll(citation.Quote, "\r\n", "\n")
		if strings.TrimSpace(citation.Quote) == "" || len(citation.Quote) > maxCitationQuoteBytes {
			return fmt.Errorf("%w: evidence citation %d quote is empty or oversized", ErrInvalidResult, i)
		}
		lines := strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n")
		quoteLines := strings.Split(citation.Quote, "\n")
		if len(quoteLines) > maxCitationLines {
			return fmt.Errorf("%w: evidence citation %d quote spans too many lines", ErrInvalidResult, i)
		}
		lineStart, lineEnd, ok := findEvidenceQuoteRange(lines, quoteLines, citation.LineStart, citation.LineEnd)
		if !ok {
			return fmt.Errorf("%w: evidence citation %d quote does not match excerpt", ErrInvalidResult, i)
		}
		citation.LineStart, citation.LineEnd = lineStart, lineEnd
		citations[i] = citation
		key := fmt.Sprintf("%s:%d:%d:%s", citation.ExcerptID, citation.LineStart, citation.LineEnd, citation.Quote)
		if seen[key] {
			return fmt.Errorf("%w: duplicate evidence citation %d", ErrInvalidResult, i)
		}
		seen[key] = true
	}
	return nil
}

type evidenceLineRange struct {
	start int
	end   int
}

func findEvidenceQuoteRange(lines, quoteLines []string, hintedStart, hintedEnd int) (int, int, bool) {
	if len(quoteLines) == 0 || len(quoteLines) > len(lines) {
		return 0, 0, false
	}
	matches := make([]evidenceLineRange, 0, 1)
	for start := 0; start+len(quoteLines) <= len(lines); start++ {
		matched := true
		for offset, quoteLine := range quoteLines {
			excerptLine := lines[start+offset]
			if strings.TrimSpace(quoteLine) == "" {
				matched = excerptLine == quoteLine
			} else {
				matched = strings.Contains(excerptLine, quoteLine)
			}
			if !matched {
				break
			}
		}
		if matched {
			matches = append(matches, evidenceLineRange{start: start + 1, end: start + len(quoteLines)})
		}
	}
	if len(matches) == 1 {
		return matches[0].start, matches[0].end, true
	}
	if hintedStart > 0 && hintedEnd >= hintedStart {
		hinted := matches[:0]
		for _, match := range matches {
			if match.start >= hintedStart && match.end <= hintedEnd {
				hinted = append(hinted, match)
			}
		}
		if len(hinted) == 1 {
			return hinted[0].start, hinted[0].end, true
		}
	}
	return 0, 0, false
}

func validateRelevantFiles(files []string, analysis Analysis, excerpts []EvidenceExcerpt) ([]string, error) {
	if len(files) > maxRelevantFiles {
		return nil, fmt.Errorf("%w: relevant files exceed %d", ErrInvalidResult, maxRelevantFiles)
	}
	grounded := map[string]bool{}
	byID := map[string]string{}
	for _, excerpt := range excerpts {
		byID[excerpt.ID] = excerpt.Path
	}
	for _, citation := range analysis.EvidenceCitations {
		grounded[byID[citation.ExcerptID]] = true
	}
	for _, citation := range analysis.SourceCitations {
		grounded[citation.Path] = true
	}
	seen := map[string]bool{}
	groundedFiles := make([]string, 0, len(files))
	for i, file := range files {
		file = strings.TrimSpace(file)
		clean, err := artifacts.SafePath(file)
		if err != nil || clean == "" || clean != file {
			return nil, fmt.Errorf("%w: relevant file %d has unsafe path %q", ErrInvalidResult, i, file)
		}
		if seen[file] {
			return nil, fmt.Errorf("%w: duplicate relevant file %q", ErrInvalidResult, file)
		}
		seen[file] = true
		if grounded[file] {
			groundedFiles = append(groundedFiles, file)
		}
	}
	return groundedFiles, nil
}
