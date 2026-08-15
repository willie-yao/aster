// Package causalcritic defines the private independent causal-review contract.
package causalcritic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/models"
)

const (
	InputSchemaVersion  = 1
	ReviewSchemaVersion = 1
	ContractVersion     = "causal-critic-v1"

	maxInputBytes          = 92 << 10
	maxDraftSummaryBytes   = 4 << 10
	maxDraftRootCauseBytes = 12 << 10
	maxDraftFixBytes       = 8 << 10
	maxDigestLines         = 24
	maxLineBytes           = 768
	maxFindings            = 6
	maxFindingDetailBytes  = 512
	maxFindingRefs         = 4
	maxGuidanceBytes       = 2 << 10
)

var (
	ErrInvalidInput  = errors.New("invalid causal critic input")
	ErrInvalidReview = errors.New("invalid causal critic review")

	errorLineRE   = regexp.MustCompile(`(?i)\b(error|failed|failure|forbidden|denied|timeout|timed out|not found|unavailable|invalid|mismatch|unsupported|refused|conflict|panic|fatal)\b`)
	successLineRE = regexp.MustCompile(`(?i)\b(recovered|succeeded|successful|healthy|reconciled|ready|available|completed|connected|running|synced|synchronized)\b`)
	negativeRE    = regexp.MustCompile(`(?i)\b(not|never|failed|failure|unable|unavailable|timeout|timed out)\b.{0,48}\b(ready|available|completed|connected|running|synced|synchronized|recovered|successful|healthy|reconciled)\b`)
	versionRE     = regexp.MustCompile(`\bv[0-9]+(?:alpha|beta)?[0-9]+\b`)
	statusCodeRE  = regexp.MustCompile(`\b[1-5][0-9]{2}\b`)
	specificRE    = regexp.MustCompile(`[A-Za-z0-9]+(?:[./:_-][A-Za-z0-9]+)+`)
)

const (
	FindingDownstreamSymptomSelected     = "downstream_symptom_selected"
	FindingSpecificErrorIgnored          = "specific_error_ignored"
	FindingSuccessCounterevidenceIgnored = "success_counterevidence_ignored"
	FindingOwnershipNotEstablished       = "ownership_not_established"
	FindingCausalLinkUnsupported         = "causal_link_unsupported"
)

var allowedFindingClasses = map[string]bool{
	FindingDownstreamSymptomSelected:     true,
	FindingSpecificErrorIgnored:          true,
	FindingSuccessCounterevidenceIgnored: true,
	FindingOwnershipNotEstablished:       true,
	FindingCausalLinkUnsupported:         true,
}

// EvidenceReference identifies immutable lines in the frozen evidence bundle.
type EvidenceReference struct {
	ExcerptID string `json:"excerpt_id"`
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
}

// EvidenceLine is one bounded line selected deterministically by the dashboard.
type EvidenceLine struct {
	Reference EvidenceReference `json:"reference"`
	Text      string            `json:"text"`
}

// Draft contains only the authoritative diagnosis fields reviewed by the critic.
type Draft struct {
	Summary           string                    `json:"summary"`
	IsTransient       bool                      `json:"is_transient"`
	RootCause         string                    `json:"root_cause"`
	Severity          string                    `json:"severity"`
	SuggestedFix      string                    `json:"suggested_fix"`
	EvidenceCitations []models.EvidenceCitation `json:"evidence_citations,omitempty"`
}

// Input seals one authoritative draft to one frozen evidence identity.
type Input struct {
	SchemaVersion          int                          `json:"schema_version"`
	ContractVersion        string                       `json:"contract_version"`
	EvidenceHash           string                       `json:"evidence_hash"`
	DraftHash              string                       `json:"draft_hash"`
	PairHash               string                       `json:"pair_hash"`
	Bundle                 agentanalysis.EvidenceBundle `json:"bundle"`
	Draft                  Draft                        `json:"draft"`
	CitedEvidence          []EvidenceLine               `json:"cited_evidence,omitempty"`
	HighSpecificityErrors  []EvidenceLine               `json:"high_specificity_errors,omitempty"`
	SuccessCounterevidence []EvidenceLine               `json:"success_counterevidence,omitempty"`
}

// Finding is one generic, evidence-referenced causal objection.
type Finding struct {
	Class      string              `json:"class"`
	Detail     string              `json:"detail"`
	References []EvidenceReference `json:"references"`
}

// Review is the only successful model output accepted by the dashboard.
type Review struct {
	SchemaVersion          int       `json:"schema_version"`
	ContractVersion        string    `json:"contract_version"`
	PairHash               string    `json:"pair_hash"`
	Verdict                string    `json:"verdict"`
	Findings               []Finding `json:"findings"`
	AlternativeExplanation string    `json:"alternative_explanation,omitempty"`
	RevisionGuidance       string    `json:"revision_guidance,omitempty"`
	Confidence             string    `json:"confidence"`
}

// NewInput creates the exact paired input used by an independent critic trial.
func NewInput(bundle agentanalysis.EvidenceBundle, authoritative agentanalysis.AuthoritativeSnapshot) (Input, error) {
	draft := Draft{
		Summary: authoritative.Summary, IsTransient: authoritative.IsTransient,
		RootCause: authoritative.RootCause, Severity: authoritative.Severity,
		SuggestedFix:      authoritative.SuggestedFix,
		EvidenceCitations: slices.Clone(authoritative.EvidenceCitations),
	}
	return buildInput(bundle, draft)
}

func buildInput(bundle agentanalysis.EvidenceBundle, draft Draft) (Input, error) {
	if err := agentanalysis.ValidateEvidenceBundle(bundle); err != nil {
		return Input{}, validationError(ValidationInputEvidence, ErrInvalidInput, "%v", err)
	}
	draft = canonicalDraft(draft)
	if err := validateDraft(draft, bundle); err != nil {
		return Input{}, withValidationCode(ValidationInputDraft, err)
	}
	draftHash, err := digest(draft)
	if err != nil {
		return Input{}, validationError(ValidationInputIdentity, ErrInvalidInput, "hash draft: %v", err)
	}
	input := Input{
		SchemaVersion: InputSchemaVersion, ContractVersion: ContractVersion,
		EvidenceHash: bundle.Hash, DraftHash: draftHash, PairHash: pairDigest(bundle.Hash, draftHash),
		Bundle: bundle, Draft: draft,
	}
	input.CitedEvidence = citedEvidenceLines(bundle, draft.EvidenceCitations)
	input.HighSpecificityErrors = selectEvidenceLines(bundle, errorLineRE, nil, 8)
	input.SuccessCounterevidence = selectEvidenceLines(bundle, successLineRE, negativeRE, 6)
	data, err := json.Marshal(input)
	if err != nil || len(data) > maxInputBytes {
		return Input{}, validationError(ValidationInputSize, ErrInvalidInput, "encoded input exceeds %d bytes", maxInputBytes)
	}
	return input, nil
}

// ValidateInput verifies all hashes, citations, and dashboard-selected digests.
func ValidateInput(input Input) error {
	if input.SchemaVersion != InputSchemaVersion || input.ContractVersion != ContractVersion {
		return validationError(ValidationInputSchema, ErrInvalidInput, "unsupported schema or contract version")
	}
	rebuilt, err := buildInput(input.Bundle, input.Draft)
	if err != nil {
		return err
	}
	left, _ := json.Marshal(input)
	right, _ := json.Marshal(rebuilt)
	if string(left) != string(right) {
		return validationError(ValidationInputIdentity, ErrInvalidInput, "paired input identity or evidence digest changed")
	}
	return nil
}

// ValidateReview accepts only bounded generic findings grounded in the paired bundle.
func ValidateReview(review Review, input Input) error {
	if err := ValidateInput(input); err != nil {
		return err
	}
	if review.SchemaVersion != ReviewSchemaVersion || review.ContractVersion != ContractVersion || review.PairHash != input.PairHash {
		return validationError(ValidationReviewIdentity, ErrInvalidReview, "schema, contract, or pair identity mismatch")
	}
	if review.Verdict != "pass" && review.Verdict != "object" {
		return validationError(ValidationReviewVerdict, ErrInvalidReview, "unsupported verdict %q", review.Verdict)
	}
	if review.Findings == nil || len(review.Findings) > maxFindings || review.Verdict == "pass" && len(review.Findings) != 0 || review.Verdict == "object" && len(review.Findings) == 0 {
		return validationError(ValidationReviewFindings, ErrInvalidReview, "verdict and findings disagree")
	}
	if review.Verdict == "pass" && (review.AlternativeExplanation != "" || review.RevisionGuidance != "") {
		return validationError(ValidationReviewGuidance, ErrInvalidReview, "passing review must not include alternative or revision guidance")
	}
	seen := map[string]bool{}
	for index, finding := range review.Findings {
		class := strings.TrimSpace(finding.Class)
		detail := strings.TrimSpace(finding.Detail)
		if class != finding.Class || detail != finding.Detail || !allowedFindingClasses[class] || detail == "" || !utf8.ValidString(finding.Detail) || len(finding.Detail) > maxFindingDetailBytes {
			return validationError(ValidationReviewFinding, ErrInvalidReview, "finding %d is invalid", index)
		}
		if len(finding.References) == 0 || len(finding.References) > maxFindingRefs {
			return validationError(ValidationReviewReference, ErrInvalidReview, "finding %d references are invalid", index)
		}
		for _, reference := range finding.References {
			if err := validateReference(reference, input.Bundle); err != nil {
				return validationError(ValidationReviewReference, ErrInvalidReview, "finding %d: %v", index, err)
			}
		}
		keyData, _ := json.Marshal(finding)
		key := string(keyData)
		if seen[key] {
			return validationError(ValidationReviewDuplicate, ErrInvalidReview, "duplicate finding %d", index)
		}
		seen[key] = true
	}
	for name, value := range map[string]string{"alternative explanation": review.AlternativeExplanation, "revision guidance": review.RevisionGuidance} {
		if value != strings.TrimSpace(value) || !utf8.ValidString(value) || len(value) > maxGuidanceBytes {
			return validationError(ValidationReviewGuidance, ErrInvalidReview, "%s is invalid or oversized", name)
		}
	}
	switch review.Confidence {
	case "low", "medium", "high":
	default:
		return validationError(ValidationReviewConfidence, ErrInvalidReview, "unsupported confidence %q", review.Confidence)
	}
	return nil
}

func canonicalDraft(draft Draft) Draft {
	draft.Summary = strings.TrimSpace(strings.ReplaceAll(draft.Summary, "\r\n", "\n"))
	draft.RootCause = strings.TrimSpace(strings.ReplaceAll(draft.RootCause, "\r\n", "\n"))
	draft.Severity = strings.TrimSpace(draft.Severity)
	draft.SuggestedFix = strings.TrimSpace(strings.ReplaceAll(draft.SuggestedFix, "\r\n", "\n"))
	for index := range draft.EvidenceCitations {
		citation := &draft.EvidenceCitations[index]
		citation.Path = strings.TrimSpace(citation.Path)
		citation.Quote = strings.ReplaceAll(citation.Quote, "\r\n", "\n")
	}
	return draft
}

func validateDraft(draft Draft, bundle agentanalysis.EvidenceBundle) error {
	for name, field := range map[string]struct {
		value string
		limit int
	}{
		"summary": {draft.Summary, maxDraftSummaryBytes}, "root cause": {draft.RootCause, maxDraftRootCauseBytes},
		"suggested fix": {draft.SuggestedFix, maxDraftFixBytes},
	} {
		if field.value == "" || !utf8.ValidString(field.value) || len(field.value) > field.limit {
			return fmt.Errorf("%w: draft %s is empty, invalid, or oversized", ErrInvalidInput, name)
		}
	}
	switch draft.Severity {
	case "Critical", "High", "Medium", "Low", "Transient-Ignore":
	default:
		return fmt.Errorf("%w: draft severity is invalid", ErrInvalidInput)
	}
	if len(draft.EvidenceCitations) > 20 {
		return fmt.Errorf("%w: draft citations exceed 20", ErrInvalidInput)
	}
	for index, citation := range draft.EvidenceCitations {
		if err := validateCitation(citation, bundle); err != nil {
			return validationError(ValidationInputCitation, ErrInvalidInput, "citation %d: %v", index, err)
		}
	}
	return nil
}

func validateCitation(citation models.EvidenceCitation, bundle agentanalysis.EvidenceBundle) error {
	if strings.TrimSpace(citation.Path) == "" || strings.TrimSpace(citation.Quote) == "" || citation.LineStart < 1 || citation.LineEnd < citation.LineStart || citation.LineEnd-citation.LineStart >= 8 {
		return fmt.Errorf("citation shape is invalid")
	}
	_, _, _, matches := locateCitation(citation, bundle)
	if matches < 1 {
		return fmt.Errorf("citation quote is absent from the frozen path")
	}
	return nil
}

func locateCitation(citation models.EvidenceCitation, bundle agentanalysis.EvidenceBundle) (agentanalysis.EvidenceExcerpt, int, int, int) {
	quoteLines := strings.Split(strings.ReplaceAll(citation.Quote, "\r\n", "\n"), "\n")
	var matched agentanalysis.EvidenceExcerpt
	matchedStart, matchedEnd, count := 0, 0, 0
	for _, excerpt := range bundle.Excerpts {
		if excerpt.Path != citation.Path {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n")
		for start := 0; start+len(quoteLines) <= len(lines); start++ {
			ok := true
			for offset, quoteLine := range quoteLines {
				if !strings.Contains(lines[start+offset], quoteLine) {
					ok = false
					break
				}
			}
			if ok {
				matched, matchedStart, matchedEnd = excerpt, start+1, start+len(quoteLines)
				count++
			}
		}
	}
	return matched, matchedStart, matchedEnd, count
}

func locateGroundedCitation(citation models.EvidenceCitation, bundle agentanalysis.EvidenceBundle) (agentanalysis.EvidenceExcerpt, int, int, bool) {
	quoteLines := strings.Split(strings.ReplaceAll(citation.Quote, "\r\n", "\n"), "\n")
	_, lineOffset := firstQuoteLine(citation.Quote)
	targetLine := citation.LineStart + lineOffset
	for _, excerpt := range bundle.Excerpts {
		if excerpt.Path != citation.Path || excerpt.Truncated {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n")
		for start := 0; start+len(quoteLines) <= len(lines); start++ {
			matched := true
			for offset, quoteLine := range quoteLines {
				if !strings.Contains(lines[start+offset], quoteLine) {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			switch excerpt.Kind {
			case "grep":
				if strings.HasPrefix(lines[start+lineOffset], fmt.Sprintf("> %d:", targetLine)) {
					return excerpt, start + 1, start + len(quoteLines), true
				}
			case "tail":
				if start+lineOffset+1 == targetLine {
					return excerpt, start + 1, start + len(quoteLines), true
				}
			}
		}
	}
	return agentanalysis.EvidenceExcerpt{}, 0, 0, false
}

func validateReference(reference EvidenceReference, bundle agentanalysis.EvidenceBundle) error {
	if reference.ExcerptID == "" || reference.Path == "" || reference.LineStart < 1 || reference.LineEnd < reference.LineStart || reference.LineEnd-reference.LineStart >= 8 {
		return fmt.Errorf("evidence reference shape is invalid")
	}
	for _, excerpt := range bundle.Excerpts {
		if excerpt.ID == reference.ExcerptID && excerpt.Path == reference.Path {
			if reference.LineEnd <= len(strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n")) {
				return nil
			}
		}
	}
	return fmt.Errorf("evidence reference is outside the frozen bundle")
}

func citedEvidenceLines(bundle agentanalysis.EvidenceBundle, citations []models.EvidenceCitation) []EvidenceLine {
	seen := map[string]bool{}
	var out []EvidenceLine
	for _, citation := range citations {
		excerpt, start, end, ok := locateGroundedCitation(citation, bundle)
		if !ok {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n")
		for line := start; line <= end && len(out) < 10; line++ {
			appendEvidenceLine(&out, seen, excerpt, line, lines[line-1])
		}
	}
	return out
}

type rankedLine struct {
	line  EvidenceLine
	score int
}

func selectEvidenceLines(bundle agentanalysis.EvidenceBundle, include, exclude *regexp.Regexp, limit int) []EvidenceLine {
	var ranked []rankedLine
	for _, excerpt := range bundle.Excerpts {
		for index, raw := range strings.Split(strings.ReplaceAll(excerpt.Content, "\r\n", "\n"), "\n") {
			text := strings.TrimSpace(raw)
			if text == "" || !include.MatchString(text) || exclude != nil && exclude.MatchString(text) {
				continue
			}
			score := 1
			if versionRE.MatchString(text) {
				score += 4
			}
			if statusCodeRE.MatchString(text) {
				score += 3
			}
			if specificRE.MatchString(text) {
				score += 2
			}
			ranked = append(ranked, rankedLine{line: EvidenceLine{
				Reference: EvidenceReference{ExcerptID: excerpt.ID, Path: excerpt.Path, LineStart: index + 1, LineEnd: index + 1},
				Text:      boundedLine(text),
			}, score: score})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		left, right := ranked[i].line.Reference, ranked[j].line.Reference
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return left.LineStart < right.LineStart
	})
	seen := map[string]bool{}
	out := make([]EvidenceLine, 0, min(limit, maxDigestLines))
	for _, candidate := range ranked {
		key := referenceKey(candidate.line.Reference)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, candidate.line)
		if len(out) == limit || len(out) == maxDigestLines {
			break
		}
	}
	return out
}

func appendEvidenceLine(out *[]EvidenceLine, seen map[string]bool, excerpt agentanalysis.EvidenceExcerpt, line int, text string) {
	reference := EvidenceReference{ExcerptID: excerpt.ID, Path: excerpt.Path, LineStart: line, LineEnd: line}
	key := referenceKey(reference)
	if seen[key] {
		return
	}
	seen[key] = true
	*out = append(*out, EvidenceLine{Reference: reference, Text: boundedLine(strings.TrimSpace(text))})
}

func boundedLine(value string) string {
	if len(value) <= maxLineBytes {
		return value
	}
	value = value[:maxLineBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func referenceKey(reference EvidenceReference) string {
	return fmt.Sprintf("%s:%d:%d", reference.ExcerptID, reference.LineStart, reference.LineEnd)
}

func pairDigest(evidenceHash, draftHash string) string {
	return hashString(ContractVersion + "\x00" + evidenceHash + "\x00" + draftHash)
}

func digest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return hashString(string(data)), nil
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
