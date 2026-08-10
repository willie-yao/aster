package causalcritic

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const (
	criticCitationScanBytes = 8 << 20
	criticBundleMaxBytes    = 72 << 10
)

// EnsureCitedEvidence adds bounded artifact excerpts when authoritative quotes
// are absent from the initial frozen bundle. It may evict lower-priority,
// non-cited excerpts to preserve the bounded executor request.
func EnsureCitedEvidence(ctx context.Context, browser artifacts.Browser, bundle agentanalysis.EvidenceBundle, citations []models.EvidenceCitation) (agentanalysis.EvidenceBundle, error) {
	if err := agentanalysis.ValidateEvidenceBundle(bundle); err != nil {
		return agentanalysis.EvidenceBundle{}, err
	}
	excerpts := append([]agentanalysis.EvidenceExcerpt(nil), bundle.Excerpts...)
	for index, citation := range citations {
		if citationGroundedInCompleteExcerpt(citation, excerpts) {
			continue
		}
		anchor, lineOffset := firstQuoteLine(citation.Quote)
		if anchor == "" {
			return agentanalysis.EvidenceBundle{}, fmt.Errorf("citation %d has no searchable quote", index)
		}
		result, err := browser.Grep(ctx, citation.Path, regexp.MustCompile(regexp.QuoteMeta(anchor)), 8, 256, 2048, criticCitationScanBytes)
		if err != nil {
			return agentanalysis.EvidenceBundle{}, fmt.Errorf("freeze cited evidence %s: %w", citation.Path, err)
		}
		targetLine := citation.LineStart + lineOffset
		var matched []string
		if result != nil {
			for _, candidate := range result.Matches {
				if candidate.LineNo == targetLine {
					matched = candidate.Context
					break
				}
			}
		}
		if len(matched) == 0 {
			return agentanalysis.EvidenceBundle{}, fmt.Errorf("citation %d quote was not found at line %d in %s", index, targetLine, citation.Path)
		}
		content := strings.Join(matched, "\n")
		if strings.TrimSpace(content) == "" {
			return agentanalysis.EvidenceBundle{}, fmt.Errorf("citation %d produced empty evidence", index)
		}
		excerpts = append(excerpts, agentanalysis.EvidenceExcerpt{Path: citation.Path, Kind: "grep", Content: content})
	}
	return fitCriticEvidenceBundle(bundle, excerpts, citations)
}

func fitCriticEvidenceBundle(original agentanalysis.EvidenceBundle, excerpts []agentanalysis.EvidenceExcerpt, citations []models.EvidenceCitation) (agentanalysis.EvidenceBundle, error) {
	base := append([]agentanalysis.EvidenceExcerpt(nil), excerpts...)
	protected := make([]bool, len(base))
	for index, excerpt := range base {
		for _, citation := range citations {
			if citationOccurrences(citation, []agentanalysis.EvidenceExcerpt{excerpt}) > 0 && !excerpt.Truncated {
				protected[index] = true
			}
		}
	}
	for {
		fitted, fitErr := agentanalysis.NewEvidenceBundle(original.Request, original.Source, original.Scan, original.Plan, base, original.SkillSetHash)
		if fitErr == nil {
			if data, marshalErr := json.Marshal(fitted); marshalErr == nil && len(data) <= criticBundleMaxBytes {
				return fitted, nil
			}
		}
		removed := false
		for index := len(base) - 1; index >= 0; index-- {
			if protected[index] || len(base) == 1 {
				continue
			}
			base = append(base[:index], base[index+1:]...)
			protected = append(protected[:index], protected[index+1:]...)
			removed = true
			break
		}
		if !removed {
			return agentanalysis.EvidenceBundle{}, fmt.Errorf("cited evidence does not fit the bounded critic bundle")
		}
	}
}

func citationOccurrences(citation models.EvidenceCitation, excerpts []agentanalysis.EvidenceExcerpt) int {
	quoteLines := strings.Split(strings.ReplaceAll(citation.Quote, "\r\n", "\n"), "\n")
	count := 0
	for _, excerpt := range excerpts {
		if excerpt.Path != citation.Path {
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
			if matched {
				count++
			}
		}
	}
	return count
}

func citationGroundedInCompleteExcerpt(citation models.EvidenceCitation, excerpts []agentanalysis.EvidenceExcerpt) bool {
	_, _, _, ok := locateGroundedCitation(citation, agentanalysis.EvidenceBundle{Excerpts: excerpts})
	return ok
}

func firstQuoteLine(quote string) (string, int) {
	for index, line := range strings.Split(strings.ReplaceAll(quote, "\r\n", "\n"), "\n") {
		if value := strings.TrimSpace(line); value != "" {
			return value, index
		}
	}
	return "", 0
}
