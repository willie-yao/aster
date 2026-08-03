package fixpr

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	maxContextTextBytes     = 8 << 10
	maxContextPathBytes     = 1024
	maxContextQuoteBytes    = 4 << 10
	maxContextCitations     = 16
	maxGenerationContextLen = 64 << 10
)

// Evidence is one bounded citation passed to fix generation.
type Evidence struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start,omitempty"`
	LineEnd   int    `json:"line_end,omitempty"`
	Quote     string `json:"quote"`
}

// RevisionContext is one evidence-backed replacement proposed by analysis chat.
type RevisionContext struct {
	RootCause    string `json:"root_cause"`
	SuggestedFix string `json:"suggested_fix"`
}

// SourceContext is one independently verified source investigation result.
type SourceContext struct {
	Finding   string     `json:"finding"`
	Revision  string     `json:"revision"`
	Citations []Evidence `json:"citations"`
}

// GenerationContext is the selected bounded context added to one fix request.
type GenerationContext struct {
	AssistantAnswer   string           `json:"assistant_answer"`
	ProposedRevision  *RevisionContext `json:"proposed_revision,omitempty"`
	ArtifactCitations []Evidence       `json:"artifact_citations"`
	Source            *SourceContext   `json:"source_investigation,omitempty"`
}

// Validate rejects incomplete or oversized fix context.
func (c GenerationContext) Validate() error {
	if strings.TrimSpace(c.AssistantAnswer) == "" || len(c.AssistantAnswer) > maxContextTextBytes {
		return fmt.Errorf("assistant answer must be 1-%d bytes", maxContextTextBytes)
	}
	if len(c.ArtifactCitations) == 0 || len(c.ArtifactCitations) > maxContextCitations {
		return fmt.Errorf("artifact citations must contain 1-%d entries", maxContextCitations)
	}
	if err := validateEvidence(c.ArtifactCitations); err != nil {
		return fmt.Errorf("artifact citations: %w", err)
	}
	if c.ProposedRevision != nil {
		if strings.TrimSpace(c.ProposedRevision.RootCause) == "" || strings.TrimSpace(c.ProposedRevision.SuggestedFix) == "" ||
			len(c.ProposedRevision.RootCause) > maxContextTextBytes || len(c.ProposedRevision.SuggestedFix) > maxContextTextBytes {
			return fmt.Errorf("proposed revision fields must be 1-%d bytes", maxContextTextBytes)
		}
	}
	if c.Source != nil {
		if !regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`).MatchString(strings.TrimSpace(c.Source.Revision)) {
			return fmt.Errorf("source revision must be a full commit SHA")
		}
		if strings.TrimSpace(c.Source.Finding) == "" || len(c.Source.Finding) > maxContextTextBytes {
			return fmt.Errorf("source finding must be 1-%d bytes", maxContextTextBytes)
		}
		if len(c.Source.Citations) == 0 || len(c.Source.Citations) > maxContextCitations {
			return fmt.Errorf("source citations must contain 1-%d entries", maxContextCitations)
		}
		if err := validateEvidence(c.Source.Citations); err != nil {
			return fmt.Errorf("source citations: %w", err)
		}
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding fix context: %w", err)
	}
	if len(encoded) > maxGenerationContextLen {
		return fmt.Errorf("fix context exceeds %d bytes", maxGenerationContextLen)
	}
	return nil
}

func validateEvidence(citations []Evidence) error {
	for _, citation := range citations {
		if strings.TrimSpace(citation.Path) == "" || len(citation.Path) > maxContextPathBytes {
			return fmt.Errorf("citation path must be 1-%d bytes", maxContextPathBytes)
		}
		if strings.TrimSpace(citation.Quote) == "" || len(citation.Quote) > maxContextQuoteBytes {
			return fmt.Errorf("citation quote must be 1-%d bytes", maxContextQuoteBytes)
		}
		if (citation.LineStart == 0) != (citation.LineEnd == 0) || citation.LineStart < 0 || citation.LineEnd < citation.LineStart {
			return fmt.Errorf("citation line range is invalid")
		}
	}
	return nil
}
