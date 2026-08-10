package causalcritic

import (
	"context"
	"regexp"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type criticEvidenceBrowser struct{}

func (criticEvidenceBrowser) BuildRoot() string { return "fixture" }
func (criticEvidenceBrowser) List(context.Context, string) (*artifacts.Listing, error) {
	return &artifacts.Listing{}, nil
}
func (criticEvidenceBrowser) ListTree(context.Context, int) ([]string, bool, error) {
	return nil, false, nil
}
func (criticEvidenceBrowser) Read(context.Context, string, int, int) ([]byte, int64, error) {
	return nil, 0, nil
}
func (criticEvidenceBrowser) Tail(context.Context, string, int, int) (*artifacts.TailResult, error) {
	return nil, nil
}
func (criticEvidenceBrowser) Grep(_ context.Context, file string, re *regexp.Regexp, _, _, _, _ int) (*artifacts.GrepResult, error) {
	return &artifacts.GrepResult{Matches: []artifacts.GrepMatch{{LineNo: 50, Context: []string{"  before", "> exact cited error", "  after"}}}}, nil
}

func TestEnsureCitedEvidenceAddsMissingBoundedExcerpt(t *testing.T) {
	input := criticInput(t)
	bundle := input.Bundle
	bundle.Excerpts = bundle.Excerpts[:0]
	var err error
	bundle, err = agentanalysis.NewEvidenceBundle(bundle.Request, bundle.Source, bundle.Scan, bundle.Plan,
		[]agentanalysis.EvidenceExcerpt{{Path: "other.log", Kind: "tail", Content: "unrelated"}}, bundle.SkillSetHash)
	if err != nil {
		t.Fatal(err)
	}
	citation := models.EvidenceCitation{Path: "build-log.txt", LineStart: 50, LineEnd: 50, Quote: "exact cited error"}
	got, err := EnsureCitedEvidence(t.Context(), criticEvidenceBrowser{}, bundle, []models.EvidenceCitation{citation})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Excerpts) != 2 || citationOccurrences(citation, got.Excerpts) != 1 {
		t.Fatalf("bundle = %+v", got.Excerpts)
	}
}
