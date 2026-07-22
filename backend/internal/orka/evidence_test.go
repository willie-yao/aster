package orka

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

type treeSeedBrowser struct {
	paths     []string
	truncated bool
	err       error
}

func (b treeSeedBrowser) BuildRoot() string { return "build" }

func (b treeSeedBrowser) List(context.Context, string) (*artifacts.Listing, error) {
	return nil, nil
}

func (b treeSeedBrowser) ListTree(context.Context, int) ([]string, bool, error) {
	return append([]string(nil), b.paths...), b.truncated, b.err
}

func (b treeSeedBrowser) Read(context.Context, string, int, int) ([]byte, int64, error) {
	return nil, 0, nil
}

func (b treeSeedBrowser) Tail(context.Context, string, int, int) (*artifacts.TailResult, error) {
	return nil, nil
}

func (b treeSeedBrowser) Grep(context.Context, string, *regexp.Regexp, int, int, int, int) (*artifacts.GrepResult, error) {
	return nil, nil
}

func TestArtifactTreeSeedPrioritizesReadablePaths(t *testing.T) {
	seed, err := ArtifactTreeSeed(context.Background(), treeSeedBrowser{
		paths: []string{
			"artifacts/screenshot.png",
			"build-log.txt",
			"artifacts/clusters/c1/machines/node/kubelet.log",
		},
		truncated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"build-log.txt", "artifacts/clusters/c1/machines/node/kubelet.log", "list truncated"} {
		if !strings.Contains(seed, want) {
			t.Errorf("seed missing %q: %s", want, seed)
		}
	}
	if strings.Contains(seed, "screenshot.png") {
		t.Fatalf("seed included non-text artifact: %s", seed)
	}
	if strings.Index(seed, "artifacts/clusters") > strings.Index(seed, "build-log.txt") {
		t.Fatalf("seed paths are not sorted: %s", seed)
	}
}

func TestWithArtifactTreeSeed(t *testing.T) {
	if got := WithArtifactTreeSeed("failure", ""); got != "failure" {
		t.Fatalf("empty seed changed prompt: %q", got)
	}
	got := WithArtifactTreeSeed("failure", "tree")
	if !strings.HasPrefix(got, "tree\n\n---\n\n") || !strings.HasSuffix(got, "failure") {
		t.Fatalf("seeded prompt = %q", got)
	}
}

func TestEvidencePlanPromptIncludesCandidateChecklist(t *testing.T) {
	plan := []skills.PlannedSkill{{
		ID: "providerid", Name: "Provider initialization", Procedure: "Compare Machine and Node.",
		RequiredEvidence: []skills.PlannedEvidenceGroup{
			{ID: "machine-state", Description: "Machine state", CandidatePaths: []string{"artifacts/machine.yaml"}},
			{ID: "node-state", Description: "Node state"},
		},
	}}
	prompt, complete := RenderEvidencePlan(plan, true)
	if complete {
		t.Fatal("truncated plan was marked complete")
	}
	for _, want := range []string{
		"Required evidence plan", "Provider initialization", "Compare Machine and Node",
		"machine-state", "artifacts/machine.yaml", "node-state", "none found", "scan was truncated",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("plan prompt missing %q: %s", want, prompt)
		}
	}
}

func TestWithEvidencePlan(t *testing.T) {
	if got := WithEvidencePlan("failure", ""); got != "failure" {
		t.Fatalf("empty plan changed prompt: %q", got)
	}
	got := WithEvidencePlan("failure", "plan")
	if !strings.HasPrefix(got, "plan\n\n---\n\n") || !strings.HasSuffix(got, "failure") {
		t.Fatalf("planned prompt = %q", got)
	}
}

func TestEvidencePlanPromptRequiresLookupAfterBudgetTruncation(t *testing.T) {
	plan := []skills.PlannedSkill{
		{ID: "oversized", Procedure: strings.Repeat("x", evidencePlanMaxBytes)},
		{ID: "later", RequiredEvidence: []skills.PlannedEvidenceGroup{{ID: "logs"}}},
	}
	prompt, complete := RenderEvidencePlan(plan, false)
	if complete {
		t.Fatal("budget-truncated plan was marked complete")
	}
	for _, want := range []string{"plans omitted", "before submit_analysis", "call required_evidence", "original failure signal"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("truncation prompt missing %q: %s", want, prompt)
		}
	}
}

func TestEvidencePlanPromptKeepsProcedureWithoutApplicableGroups(t *testing.T) {
	prompt, complete := RenderEvidencePlan([]skills.PlannedSkill{{
		ID: "conditional", Procedure: "Inspect the matching subtype.",
	}}, false)
	if !complete {
		t.Fatal("matched procedure without applicable groups was marked incomplete")
	}
	for _, want := range []string{"conditional", "Inspect the matching subtype", "no conditional groups apply"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("conditional plan missing %q: %s", want, prompt)
		}
	}
}

func TestRenderEvidencePlanMarksCandidateCoverageComplete(t *testing.T) {
	plan := []skills.PlannedSkill{{
		ID: "complete",
		RequiredEvidence: []skills.PlannedEvidenceGroup{
			{ID: "one", CandidatePaths: []string{"one.log"}},
			{ID: "two", CandidatePaths: []string{"two.log"}},
		},
	}}
	_, complete := RenderEvidencePlan(plan, false)
	if !complete {
		t.Fatal("fully resolved evidence plan was marked incomplete")
	}
}
