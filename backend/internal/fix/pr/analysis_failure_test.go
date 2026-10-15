package pr

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/actiondraft"
	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/redact"
	"github.com/willie-yao/aster/backend/internal/runtime"
)

const exactAnalysisRevision = "0123456789abcdef0123456789abcdef01234567"

func validAnalysisFailure() AnalysisFailure {
	return AnalysisFailure{
		ID: "analysis::id", Project: "capz", JobID: "periodic-capz", JobName: "periodic-capz", BuildID: "123",
		TestName: "TestCluster", AnalysisGeneratedAt: "2026-08-13T01:00:00Z", AnalysisHash: "analysis-hash",
		RootCause: "the reconciler omitted the terminal state", SuggestedFix: "update the reconciler branch",
		FailureMessage: "cluster failed", FailureBody: "expected Ready",
		AssistantAnswer:  "The artifact shows the terminal branch never calls `markReady`.",
		ChatResponseHash: "chat-hash", PreviewRequestHash: "preview-hash",
		ArtifactCitations: []Evidence{{Path: "artifacts/junit_01.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
		SourceRepository:  "up/stream", SourceBranch: "main", TargetBranch: "main",
		FailureRevision: exactAnalysisRevision, GenerationBaseRevision: exactAnalysisRevision,
		SourceHints: []string{"controllers/cluster_controller.go"},
	}
}

func TestGenerateAnalysisPreviewUsesExactSourceAndCreatesNoWrite(t *testing.T) {
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})
	manager.opts.Agent.GitToken = ""
	fix, err := manager.GenerateAnalysisPreview(t.Context(), validAnalysisFailure(), "preserve compatibility")
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.opened) != 0 {
		t.Fatalf("preview performed GitHub write: %+v", pr.opened)
	}
	if agent.spec.Repo.Ref != exactAnalysisRevision || agent.spec.ExpectedBaseSHA != exactAnalysisRevision || agent.spec.Repo.Token != "" {
		t.Fatalf("runtime spec = %+v", agent.spec)
	}
	for _, want := range []string{"exact failed test or CI build", "TestCluster", "ArtifactCitations", "SourceHints", "cluster failed", "expected Ready", "preserve compatibility", "Do not delete or rename files."} {
		if !strings.Contains(agent.spec.Instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, agent.spec.Instruction)
		}
	}
	if strings.Contains(agent.spec.Instruction, "recurs systematically") {
		t.Fatalf("instruction claimed recurrence: %s", agent.spec.Instruction)
	}
	snapshot := fix.Snapshot()
	if !snapshot.RequireBaseCurrent || snapshot.Base.HeadSHA != exactAnalysisRevision || !strings.HasPrefix(snapshot.Key, "fix-analysis::") {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestGenerateAnalysisPreviewCarriesBuildFailureToAgent(t *testing.T) {
	failure := validAnalysisFailure()
	failure.Source, failure.TestName = "build", "Prow job execution"
	failure.SourceBranch, failure.TargetBranch = "release-1.24", "release-1.24"
	failure.FailureMessage, failure.FailureBody = "exit 127", "hack/ensure-azcli.sh: az: command not found"
	failure.ArtifactCitations = []Evidence{{Path: "build-log.txt", Quote: failure.FailureBody}}
	pr := &fakePR{base: ghpr.Base{Branch: failure.SourceBranch, HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "Guard az CLI installation.")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"Source":"build"`, "before a failed JUnit case was reported", "exit 127",
		"hack/ensure-azcli.sh", "release-1.24", "Guard az CLI installation.",
	} {
		if !strings.Contains(agent.spec.Instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, agent.spec.Instruction)
		}
	}
	if strings.Contains(fix.Description, "Analyzed JUnit test") ||
		!strings.Contains(fix.Description, "Prow job execution") ||
		!fix.Snapshot().RequireBaseCurrent || fix.Snapshot().Base.Branch != "release-1.24" || len(pr.opened) != 0 {
		t.Fatalf("fix=%+v writes=%v", fix, pr.opened)
	}
}

func TestGenerateAnalysisPreviewQualifiesEvidenceWarnings(t *testing.T) {
	failure := validAnalysisFailure()
	failure.EvidenceWarnings = []string{"citation 2 line range was not returned"}
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"EvidenceWarnings", "citation 2 line range was not returned", "evidence qualification warnings", "warned claims"} {
		if !strings.Contains(agent.spec.Instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, agent.spec.Instruction)
		}
	}
	if !strings.Contains(fix.Description, "Evidence qualification") || !strings.Contains(fix.Description, "warned claims remain hypotheses") {
		t.Fatalf("description = %s", fix.Description)
	}
}

func TestGenerateAnalysisPreviewInvestigatesUnverifiedUncitedAnswerWithoutHints(t *testing.T) {
	failure := validAnalysisFailure()
	failure.ArtifactCitations = nil
	failure.SourceHints = nil
	failure.AssistantUnverified = true
	failure.AssistantUnverifiedReason = "artifact access ended before verification"
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})

	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"explicitly unverified", "artifact access ended before verification",
		"ArtifactCitations\":null", "SourceHints\":null", "Search the repository as needed",
		"Make no change when no code in this repository is causally involved",
	} {
		if !strings.Contains(agent.spec.Instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, agent.spec.Instruction)
		}
	}
	if !strings.Contains(fix.Description, "explicitly unverified") ||
		!strings.Contains(fix.Body, "selected chat hypothesis") {
		t.Fatalf("description=%q body=%q", fix.Description, fix.Body)
	}
}

func TestValidateAnalysisFailureBoundsRawFailureAndHints(t *testing.T) {
	failure := validAnalysisFailure()
	failure.ArtifactCitations = nil
	failure.SourceHints = nil
	if err := validateAnalysisFailure(failure); err != nil {
		t.Fatalf("investigative context error = %v", err)
	}

	failure.FailureBody = strings.Repeat("x", maxContextTextBytes+1)
	if err := validateAnalysisFailure(failure); err == nil {
		t.Fatal("oversized raw failure body was accepted")
	}
	failure.FailureBody = "expected Ready"
	failure.SourceHints = []string{"z.go", "a.go"}
	if err := validateAnalysisFailure(failure); err == nil {
		t.Fatal("unsorted source hints were accepted")
	}
}

func TestGenerateAnalysisPreviewAllowsEmptyOriginalSuggestedFix(t *testing.T) {
	failure := validAnalysisFailure()
	failure.SuggestedFix = ""
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})

	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || !strings.Contains(agent.spec.Instruction, failure.AssistantAnswer) {
		t.Fatalf("agent calls=%d instruction=%q", agent.calls, agent.spec.Instruction)
	}
	if fix == nil || len(pr.opened) != 0 {
		t.Fatalf("fix=%+v opened=%+v", fix, pr.opened)
	}
	if fix.Preview.Rationale != "Edits `templates/cluster.yaml`; see the proposed diff." {
		t.Fatalf("rationale = %q", fix.Preview.Rationale)
	}
}

func TestAnalysisPreviewDescribesGeneratedChange(t *testing.T) {
	failure := validAnalysisFailure()
	failure.SuggestedFix = "Publish the missing image to the region."
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	agent.res.AgentSummary = "Select a disk type the region offers (`templates/cluster.yaml:12`).\n\nCaveat: the region list is not verified."
	manager := newManager(t, pr, agent, Options{})

	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "keep the retry scoped to reconciliation")
	if err != nil {
		t.Fatal(err)
	}
	want := "Select a disk type the region offers (`templates/cluster.yaml:12`). Caveat: the region list is not verified."
	if fix.Preview.Rationale != want || !strings.Contains(fix.Description, "**Proposed change:** "+want) {
		t.Fatalf("rationale=%q description=%q", fix.Preview.Rationale, fix.Description)
	}
	if strings.Contains(fix.Description, failure.SuggestedFix) || strings.Contains(fix.Description, "keep the retry scoped") {
		t.Fatalf("description described the published suggestion or instruction: %s", fix.Description)
	}

	agent.res.AgentSummary = "I need to check the template. Let me draft the change."
	fix, err = manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if fix.Preview.Rationale != "Edits `templates/cluster.yaml`; see the proposed diff." {
		t.Fatalf("unusable summary rationale = %q", fix.Preview.Rationale)
	}
}

func TestAnalysisPreviewBoundsLongTestTitles(t *testing.T) {
	failure := validAnalysisFailure()
	failure.TestName = "[It] " + strings.Repeat("very long test name ", 20)
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	manager := newManager(t, pr, goodAgent(), Options{})
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := actiondraft.ValidateTitleBody(fix.Title, fix.Description); err != nil {
		t.Fatalf("title %q: %v", fix.Title, err)
	}
}

func TestGenerateAnalysisPreviewUsesCurrentGenerationBase(t *testing.T) {
	failure := validAnalysisFailure()
	failure.FailureRevision = "a866aca055bcaa205648e81d15c67668179fdfab"
	failure.GenerationBaseRevision = "c83d69ab8c572a4c00816076222d65262ee690cc"
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: failure.GenerationBaseRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})

	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.spec.Repo.Ref != failure.GenerationBaseRevision || agent.spec.ExpectedBaseSHA != failure.GenerationBaseRevision {
		t.Fatalf("runtime spec = %+v", agent.spec)
	}
	if agent.spec.Repo.Ref == failure.FailureRevision {
		t.Fatal("generation used the failure revision")
	}
	for _, want := range []string{failure.FailureRevision, failure.GenerationBaseRevision, "historical failure revision", "current full commit", "do not assume it still applies", "investigate before deciding"} {
		if !strings.Contains(agent.spec.Instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, agent.spec.Instruction)
		}
	}
	if snapshot := fix.Snapshot(); snapshot.Base.HeadSHA != failure.GenerationBaseRevision || !snapshot.RequireBaseCurrent {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if len(pr.opened) != 0 {
		t.Fatalf("preview performed GitHub write: %+v", pr.opened)
	}
}

func TestAnalysisPreviewRejectsRewrittenBaseAndAllowsSourceCheckRetry(t *testing.T) {
	failure := validAnalysisFailure()
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: strings.Repeat("b", 40), TreeSHA: "tree-b"}, pinnedTree: "tree-a"}
	manager := newManager(t, pr, goodAgent(), Options{})
	if _, err := manager.GenerateAnalysisPreview(t.Context(), failure, ""); !errors.Is(err, ErrPreviewBaseChanged) {
		t.Fatalf("generation drift error = %v", err)
	}

	pr.base = ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree-a"}
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	pr.base = ghpr.Base{Branch: "main", HeadSHA: strings.Repeat("c", 40), TreeSHA: "tree-c"}
	if _, err := manager.OpenFromPreview(t.Context(), fix); !errors.Is(err, ErrPreviewBaseChanged) {
		t.Fatalf("confirmation drift error = %v", err)
	}
	if len(pr.opened) != 0 {
		t.Fatalf("drifted confirmation wrote PR: %+v", pr.opened)
	}
	pr.resolveErr = errors.New("403 Forbidden: API rate limit exceeded")
	if _, err := manager.OpenFromPreview(t.Context(), fix); !errors.Is(err, ErrPreviewSourceUnavailable) ||
		!errors.Is(err, pr.resolveErr) || errors.Is(err, ErrPreviewBaseChanged) {
		t.Fatalf("base check failure = %v", err)
	}
	pr.resolveErr = nil
	pr.base = fix.Snapshot().Base
	if _, err := manager.OpenFromPreview(t.Context(), fix); err != nil || len(pr.opened) != 1 {
		t.Fatalf("retry after base check recovery = %v, writes=%d", err, len(pr.opened))
	}
}

func TestAnalysisPreviewRequiresExplicitMatchingTargetBranch(t *testing.T) {
	manager := newManager(t, &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}, goodAgent(), Options{})
	for _, mutate := range []func(*AnalysisFailure){
		func(f *AnalysisFailure) { f.SourceBranch = "" },
		func(f *AnalysisFailure) { f.TargetBranch = "" },
	} {
		failure := validAnalysisFailure()
		mutate(&failure)
		if _, err := manager.GenerateAnalysisPreview(t.Context(), failure, ""); err == nil || !strings.Contains(err.Error(), "context is incomplete") {
			t.Fatalf("missing branch error = %v", err)
		}
	}

	failure := validAnalysisFailure()
	failure.SourceBranch = "release 1.24\n/cherry-pick main"
	if _, err := manager.GenerateAnalysisPreview(t.Context(), failure, ""); err == nil || !strings.Contains(err.Error(), "valid branch names") {
		t.Fatalf("invalid branch error = %v", err)
	}

	failure = validAnalysisFailure()
	failure.TargetBranch = "release-1.25"
	if _, err := manager.GenerateAnalysisPreview(t.Context(), failure, ""); !errors.Is(err, ErrPreviewBaseChanged) {
		t.Fatalf("mismatched resolved branch error = %v", err)
	}
}

func TestAnalysisPreviewChecksChangedFilesAndAncestryOnBaseAdvance(t *testing.T) {
	lookupErr := errors.New("502 Bad Gateway")
	for _, stage := range []string{"before generation", "during generation", "confirmation"} {
		for _, tc := range []struct {
			name     string
			contains bool
			paths    []string
			err      error
			want     error
		}{
			{name: "unrelated files", contains: true, paths: []string{"go.mod", "go.sum"}},
			{name: "patched file", contains: true, paths: []string{"templates/cluster.yaml"}, want: ErrPreviewBaseChanged},
			{name: "rewritten branch", want: ErrPreviewBaseChanged},
			{name: "compare unavailable", err: lookupErr, want: ErrPreviewSourceUnavailable},
		} {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				failure := validAnalysisFailure()
				initial := ghpr.Base{Branch: "main", HeadSHA: failure.GenerationBaseRevision, TreeSHA: "tree-a"}
				advanced := ghpr.Base{Branch: "main", HeadSHA: strings.Repeat("c", 40), TreeSHA: "tree-c"}
				pr := &fakePR{
					base: initial, pinnedTree: initial.TreeSHA,
					compareContains: tc.contains, comparePaths: tc.paths, compareErr: tc.err,
				}
				switch stage {
				case "before generation":
					pr.base = advanced
				case "during generation":
					pr.bases = []ghpr.Base{initial, advanced}
				}
				agent := goodAgent()
				manager := newManager(t, pr, agent, Options{})
				fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
				if stage == "confirmation" {
					if err != nil {
						t.Fatal(err)
					}
					pr.base = advanced
					_, err = manager.OpenFromPreview(t.Context(), RestoreGeneratedFix(fix.Snapshot()))
				}
				if !errors.Is(err, tc.want) || tc.want == ErrPreviewSourceUnavailable && errors.Is(err, ErrPreviewBaseChanged) {
					t.Fatalf("error=%v want=%v", err, tc.want)
				}
				if tc.err != nil && !errors.Is(err, tc.err) {
					t.Fatalf("compare cause lost: %v", err)
				}
				wantComparison := [2]string{initial.HeadSHA, advanced.HeadSHA}
				if agent.calls != 1 || len(pr.comparisons) == 0 || pr.comparisons[0] != wantComparison {
					t.Fatalf("agent calls=%d comparisons=%v", agent.calls, pr.comparisons)
				}
				if tc.want != nil {
					if len(pr.opened) != 0 {
						t.Fatalf("rejected preview wrote PR: %+v", pr.opened)
					}
					return
				}
				if fix.Snapshot().Base != initial || agent.spec.Repo.Ref != initial.HeadSHA {
					t.Fatalf("pinned base changed: %+v agent=%+v", fix.Snapshot().Base, agent.spec.Repo)
				}
				if stage != "confirmation" {
					if _, err := manager.OpenFromPreview(t.Context(), fix); err != nil {
						t.Fatal(err)
					}
				}
				if len(pr.opened) != 1 || pr.opened[0].Base == nil || *pr.opened[0].Base != initial {
					t.Fatalf("PR did not use pinned base: %+v", pr.opened)
				}
			})
		}
	}
}

func TestAnalysisPreviewRejectsMissingTargetBranch(t *testing.T) {
	initial := ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}
	pr := &fakePR{base: initial}
	manager := newManager(t, pr, goodAgent(), Options{})
	fix, err := manager.GenerateAnalysisPreview(t.Context(), validAnalysisFailure(), "")
	if err != nil {
		t.Fatal(err)
	}
	pr.resolveErr = ghpr.ErrBranchNotFound
	if _, err := manager.OpenFromPreview(t.Context(), fix); !errors.Is(err, ErrPreviewBaseChanged) {
		t.Fatalf("missing branch error=%v", err)
	}
	if len(pr.opened) != 0 {
		t.Fatal("missing target branch wrote PR")
	}
}

func TestAnalysisPreviewUsesAnyStateDedupBeforeWrite(t *testing.T) {
	pr := &fakePR{
		base:        ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"},
		searchFound: true, searchURL: "https://github.com/up/stream/pull/9",
	}
	manager := newManager(t, pr, goodAgent(), Options{})
	fix, err := manager.GenerateAnalysisPreview(t.Context(), validAnalysisFailure(), "")
	if err != nil {
		t.Fatal(err)
	}
	url, err := manager.OpenFromPreview(t.Context(), fix)
	if err != nil || url != pr.searchURL || pr.searchAnyCalls != 1 || len(pr.opened) != 0 {
		t.Fatalf("url=%q err=%v searchAny=%d opened=%d", url, err, pr.searchAnyCalls, len(pr.opened))
	}
}

func TestAnalysisPreviewDedupIdentityIncludesSelectedChatAndRequest(t *testing.T) {
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	manager := newManager(t, pr, goodAgent(), Options{})
	first, err := manager.GenerateAnalysisPreview(t.Context(), validAnalysisFailure(), "")
	if err != nil {
		t.Fatal(err)
	}
	changedChat := validAnalysisFailure()
	changedChat.ChatResponseHash = "other-chat"
	second, err := manager.GenerateAnalysisPreview(t.Context(), changedChat, "")
	if err != nil {
		t.Fatal(err)
	}
	changedRequest := validAnalysisFailure()
	changedRequest.PreviewRequestHash = "other-preview"
	third, err := manager.GenerateAnalysisPreview(t.Context(), changedRequest, "")
	if err != nil {
		t.Fatal(err)
	}
	changedTarget := validAnalysisFailure()
	changedTarget.SourceBranch = "release"
	fifth, err := manager.GenerateAnalysisPreview(t.Context(), changedTarget, "")
	if err != nil {
		t.Fatal(err)
	}
	changedBranch := validAnalysisFailure()
	changedBranch.SourceBranch, changedBranch.TargetBranch = "release", "release"
	pr.base.Branch = "release"
	fourth, err := manager.GenerateAnalysisPreview(t.Context(), changedBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, fix := range []*GeneratedFix{first, second, third, fourth, fifth} {
		keys[fix.Snapshot().Key] = true
	}
	if len(keys) != 5 {
		t.Fatal("selected chat, preview, tested branch, or target branch identity did not change Fix PR dedup key")
	}
}

func TestAnalysisPreviewCritiqueErrorWarns(t *testing.T) {
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{Critique: &fakeCompleter{critiqueErr: errors.New("review endpoint down")}, CritiqueRetries: 1})
	fix, err := manager.GenerateAnalysisPreview(t.Context(), validAnalysisFailure(), "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || !slices.Contains(fix.Warnings, patchCritiqueUnavailableWarning) {
		t.Fatalf("calls=%d warnings=%v", agent.calls, fix.Warnings)
	}
}

func TestAnalysisPreviewCritiqueConcernWarnsWithoutRetry(t *testing.T) {
	failure := validAnalysisFailure()
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{Critique: &fakeCompleter{critique: `{"issues":["patch needs a narrower condition"]}`}, CritiqueRetries: 3})
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || !slices.Contains(fix.Warnings, patchCritiqueWarning("patch needs a narrower condition")) {
		t.Fatalf("calls=%d warnings=%v", agent.calls, fix.Warnings)
	}
}

func TestAnalysisPreviewFailedAuthenticCommandsWarnAndRemainConfirmable(t *testing.T) {
	failure := validAnalysisFailure()
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	commands := sandboxVerificationCommands()
	results := sandboxCommandResults()
	results[0].ExitCode = 1
	agent.res.BaseSHA = exactAnalysisRevision
	agent.res.CommandResults = results
	manager := newManager(t, pr, agent, Options{})
	manager.opts.Agent.RequireCommandResults = true
	manager.opts.Agent.CommandPolicy.Commands = commands
	reconstructions := 0
	manager.opts.ReconstructPatch = func(context.Context, runtime.RepoRef, string) (map[string]string, string, error) {
		reconstructions++
		return agent.res.Files, agent.res.Diff, nil
	}
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || fix.Preview.Verify.Status != VerifyFailed || !slices.Contains(fix.Warnings, patchVerifyWarning) {
		t.Fatalf("calls=%d verify=%+v warnings=%v", agent.calls, fix.Preview.Verify, fix.Warnings)
	}
	restored := RestoreGeneratedFix(fix.Snapshot())
	if restored.executionVerification == nil || restored.executionVerification.Results[0].ExitCode != 1 {
		t.Fatalf("restored verification = %+v", restored.executionVerification)
	}
	if _, err := manager.OpenFromPreview(t.Context(), restored); err != nil {
		t.Fatal(err)
	}
	if len(pr.opened) != 1 || reconstructions != 1 {
		t.Fatalf("opened=%d reconstructions=%d", len(pr.opened), reconstructions)
	}

	drifted := RestoreGeneratedFix(fix.Snapshot())
	drifted.executionVerification.Results[0].Argv = []string{"go", "test", "./wrong"}
	if _, err := manager.OpenFromPreview(t.Context(), drifted); err == nil || !strings.Contains(err.Error(), "allowed argv") {
		t.Fatalf("integrity error = %v", err)
	}
}

func TestAnalysisPreviewTimedOutAuthenticCommandWarnsWithoutRetry(t *testing.T) {
	failure := validAnalysisFailure()
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}}
	agent := goodAgent()
	commands := sandboxVerificationCommands()
	results := sandboxCommandResults()
	results[0].ExitCode = -1
	results[0].TimedOut = true
	results[0].DurationMs = commands[0].TimeoutSeconds * 1000
	agent.res.BaseSHA = exactAnalysisRevision
	agent.res.CommandResults = results
	manager := newManager(t, pr, agent, Options{})
	manager.opts.Agent.RequireCommandResults = true
	manager.opts.Agent.CommandPolicy.Commands = commands
	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.calls != 1 || fix.Preview.Verify.Status != VerifyFailed || !slices.Contains(fix.Warnings, patchVerifyWarning) {
		t.Fatalf("calls=%d verify=%+v warnings=%v", agent.calls, fix.Preview.Verify, fix.Warnings)
	}
}

func TestAnalysisGenerationFailureRetainsOnlySafeDiagnostic(t *testing.T) {
	commands := sandboxVerificationCommands()
	results := sandboxCommandResults()
	results[0].Stdout = "private stdout"
	results[0].Stderr = "private stderr"
	agent := &fakeAgentRuntime{res: runtime.ExecutionResult{
		TerminalState: runtime.TerminalSucceeded, BaseSHA: exactAnalysisRevision,
		Files: map[string]string{}, CommandResults: results,
		StdoutSummary: "No deterministic repository edit was available.\nhttps://private.example/path token=secret-value",
	}}
	config := &AgentConfig{Runtime: agent, RequireCommandResults: true, CommandPolicy: runtime.CommandPolicy{Commands: commands}}
	_, err := generateAnalysisWithAgent(t.Context(), genParams{
		owner: "up", repo: "stream", maxFiles: 2, agent: config,
	}, validAnalysisFailure())
	if err == nil {
		t.Fatal("expected no-change failure")
	}
	if err.Error() != "the coding agent completed, but no repository change was generated" || strings.Contains(err.Error(), "external or operational") {
		t.Fatalf("no-change error = %q", err)
	}
	diagnostic, ok := AnalysisFailureDiagnosticOf(err)
	if !ok || diagnostic.Category != AnalysisFailureNoReviewablePatch || diagnostic.Detail != AnalysisFailureDetailNoRepositoryChange || diagnostic.TerminalState != runtime.TerminalSucceeded {
		t.Fatalf("diagnostic = %+v ok=%v", diagnostic, ok)
	}
	if agent.calls != 1 || len(diagnostic.CommandResults) != len(commands) {
		t.Fatalf("calls=%d diagnostic=%+v", agent.calls, diagnostic)
	}
	if diagnostic.OperatorSummary != "No deterministic repository edit was available. [redacted-url] token=[redacted]" {
		t.Fatalf("operator summary = %q", diagnostic.OperatorSummary)
	}
	for _, result := range diagnostic.CommandResults {
		if result.Stdout != "" || result.Stderr != "" {
			t.Fatalf("diagnostic retained command output: %+v", result)
		}
	}
}

func TestAnalysisGenerationFailureClassifiesScopeAndHardOutcomes(t *testing.T) {
	commands := sandboxVerificationCommands()
	results := sandboxCommandResults()
	tests := []struct {
		name        string
		result      runtime.ExecutionResult
		err         error
		maxFiles    int
		want        AnalysisFailureCategory
		wantDetail  AnalysisFailureDetail
		wantSummary bool
	}{
		{
			name: "too broad", maxFiles: 1, want: AnalysisFailureNoReviewablePatch, wantDetail: AnalysisFailureDetailReviewScopeExceeded,
			result: runtime.ExecutionResult{TerminalState: runtime.TerminalSucceeded, BaseSHA: exactAnalysisRevision,
				ChangedFiles: []string{"a", "b"}, Files: map[string]string{"a": "1", "b": "2"}, Diff: "diff", CommandResults: results},
		},
		{
			name: "runtime", maxFiles: 2, want: AnalysisFailureRuntimeInfrastructure, wantSummary: true,
			result: runtime.ExecutionResult{TerminalState: runtime.TerminalFailed, FailureCode: runtime.ExecutionFailureRuntime,
				FailureReason: "coding agent failed: UnknownError: stream decode failed"},
			err: runtime.ErrUnavailable,
		},
		{
			name: "provider credential", maxFiles: 2, want: AnalysisFailureProviderCredential,
			wantDetail: AnalysisFailureDetailProviderForbidden, wantSummary: true,
			result: runtime.ExecutionResult{TerminalState: runtime.TerminalFailed, FailureCode: runtime.ExecutionFailureProviderCredential,
				ProviderError:  &runtime.ProviderErrorDetail{StatusCode: 403, Message: "Forbidden", AuthSecretName: "agent-sandbox-model", AuthSecretKey: "AI_TOKEN", Endpoint: "https://api.githubcopilot.com/chat/completions", Model: "gpt-fixture"},
				CommandResults: results},
			err: errors.New("agent Sandbox execution failed: model provider refused the sandbox request (HTTP 403)"),
		},
		{
			name: "provider request", maxFiles: 2, want: AnalysisFailureProviderRequest, wantSummary: true,
			result: runtime.ExecutionResult{TerminalState: runtime.TerminalFailed, FailureCode: runtime.ExecutionFailureProviderRequest,
				ProviderError: &runtime.ProviderErrorDetail{StatusCode: 400, Code: "unsupported_api_for_model",
					Message: "This model requires /responses", AuthSecretName: "agent-sandbox-model", AuthSecretKey: "AI_TOKEN",
					Endpoint: "https://api.githubcopilot.com/chat/completions", Model: "gpt-fixture"},
				CommandResults: results},
			err: errors.New("agent Sandbox execution failed: model provider rejected the request (HTTP 400 unsupported_api_for_model)"),
		},
		{
			name: "review scope wire outcome", maxFiles: 2, want: AnalysisFailureNoReviewablePatch, wantDetail: AnalysisFailureDetailReviewScopeExceeded,
			result: runtime.ExecutionResult{TerminalState: runtime.TerminalFailed, FailureCode: runtime.ExecutionFailureReviewScope,
				CommandResults: results},
			err: errors.New("agent Sandbox execution failed"),
		},
		{
			name: "result contract", maxFiles: 2, want: AnalysisFailureResultContract,
			result: runtime.ExecutionResult{TerminalState: runtime.TerminalFailed, ChangedFiles: []string{"../../private runtime text"}, CommandResults: results},
			err:    runtime.ErrMalformedResult,
		},
		{
			name: "safety", maxFiles: 2, want: AnalysisFailureSafetyIntegrity,
			result: runtime.ExecutionResult{TerminalState: runtime.TerminalFailed, FailureCode: runtime.ExecutionFailureSafetyIntegrity, CommandResults: results},
			err:    errors.New("agent Sandbox execution failed"),
		},
		{
			name: "timeout", maxFiles: 2, want: AnalysisFailureTimedOut, wantSummary: true,
			result: runtime.ExecutionResult{TerminalState: runtime.TerminalTimedOut, FailureReason: "coding agent deadline exceeded"},
			err:    context.DeadlineExceeded,
		},
		{
			name: "cancelled", maxFiles: 2, want: AnalysisFailureCancelled,
			result: runtime.ExecutionResult{TerminalState: runtime.TerminalCancelled},
			err:    runtime.ErrCancelled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.result
			result.StdoutSummary = "private agent output"
			agent := &fakeAgentRuntime{res: result, err: tt.err}
			config := &AgentConfig{Runtime: agent, RequireCommandResults: true, CommandPolicy: runtime.CommandPolicy{Commands: commands}}
			_, err := generateAnalysisWithAgent(t.Context(), genParams{
				owner: "up", repo: "stream", maxFiles: tt.maxFiles, agent: config,
			}, validAnalysisFailure())
			if err == nil {
				t.Fatal("expected generation failure")
			}
			diagnostic, ok := AnalysisFailureDiagnosticOf(err)
			if !ok || diagnostic.Category != tt.want || agent.calls != 1 {
				t.Fatalf("diagnostic=%+v ok=%v calls=%d err=%v", diagnostic, ok, agent.calls, err)
			}
			if diagnostic.Detail != tt.wantDetail {
				t.Fatalf("detail = %q, want %q", diagnostic.Detail, tt.wantDetail)
			}
			if len(diagnostic.ChangedFiles) != 0 {
				t.Fatalf("diagnostic exposed changed files: %v", diagnostic.ChangedFiles)
			}
			if tt.wantSummary {
				if diagnostic.OperatorSummary == "" {
					t.Fatal("diagnostic omitted operator summary")
				}
			} else if diagnostic.OperatorSummary != "" {
				t.Fatalf("diagnostic exposed agent summary: %q", diagnostic.OperatorSummary)
			}
		})
	}
}

func TestAnalysisGenerationTimeoutRetainsSafePartialDiagnostic(t *testing.T) {
	commands := sandboxVerificationCommands()
	for _, tc := range []struct {
		name string
		edit func(*runtime.CommandResult)
		want int
	}{
		{name: "valid prefix", want: 1, edit: func(*runtime.CommandResult) {}},
		{name: "wrong argv", edit: func(result *runtime.CommandResult) { result.Argv = []string{"unexpected"} }},
		{name: "negative duration", edit: func(result *runtime.CommandResult) { result.DurationMs = -1 }},
		{name: "excessive duration", edit: func(result *runtime.CommandResult) { result.DurationMs = 31_000_000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results := sandboxCommandResults()[:1]
			results[0].ExitCode = -1
			results[0].TimedOut = true
			results[0].Stdout, results[0].Stderr = "private stdout", "private stderr"
			tc.edit(&results[0])
			agent := &fakeAgentRuntime{
				res: runtime.ExecutionResult{
					TerminalState: runtime.TerminalTimedOut, CommandResults: results,
					FailureReason: "coding agent deadline exceeded",
					StdoutSummary: "investigated pinned source",
					StderrSummary: "partial progress https://private.example/path token=fixture-secret",
				},
				err: errors.Join(context.DeadlineExceeded, runtime.ErrCleanupPending),
			}
			config := &AgentConfig{Runtime: agent, RequireCommandResults: true, CommandPolicy: runtime.CommandPolicy{Commands: commands}}
			_, err := generateAnalysisWithAgent(t.Context(), genParams{owner: "up", repo: "stream", maxFiles: 2, agent: config}, validAnalysisFailure())
			diagnostic, ok := AnalysisFailureDiagnosticOf(err)
			if !ok || diagnostic.Category != AnalysisFailureTimedOut || diagnostic.TerminalState != runtime.TerminalTimedOut ||
				len(diagnostic.CommandResults) != tc.want {
				t.Fatalf("diagnostic=%+v err=%v", diagnostic, err)
			}
			want := "coding agent deadline exceeded partial progress [redacted-url] token=[redacted] investigated pinned source"
			if diagnostic.OperatorSummary != want {
				t.Fatalf("summary = %q, want %q", diagnostic.OperatorSummary, want)
			}
			for _, result := range diagnostic.CommandResults {
				if result.Stdout != "" || result.Stderr != "" {
					t.Fatalf("retained raw command output: %+v", result)
				}
			}
		})
	}
}

func TestProviderRequestOperatorSummaryBoundsAndRedacts(t *testing.T) {
	detail := &runtime.ProviderErrorDetail{
		StatusCode: 400, Code: "model_not_supported", Message: strings.Repeat("provider-detail-", 50) + " token=fixture-secret",
		ProviderID: "github-copilot", AuthSecretName: "agent-sandbox-model", AuthSecretKey: "AI_TOKEN",
		Endpoint: "https://api.githubcopilot.com/chat/completions?token=fixture-secret", Model: "gpt-fixture",
	}
	summary := providerRequestOperatorSummary(detail)
	for _, want := range []string{
		"HTTP 400 model_not_supported", "Secret agent-sandbox-model/AI_TOKEN",
		"endpoint https|api.githubcopilot.com/chat/completions", "model gpt-fixture", "Provider github-copilot",
		"Provider message: provider-detail-",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q: %q", want, summary)
		}
	}
	if len(summary) > providerOperatorSummaryBytes || redact.OperatorText(summary) != summary || strings.Contains(summary, "fixture-secret") {
		t.Fatalf("summary is not bounded and redacted: len=%d summary=%q", len(summary), summary)
	}
	detail.ProviderID = strings.Repeat("provider-", 60)
	detail.AuthSecretName = strings.Repeat("secret-", 36)
	detail.AuthSecretKey = strings.Repeat("key-", 63)
	detail.Endpoint = "https://" + strings.Repeat("endpoint", 80) + ".example/v1/chat/completions"
	detail.Model = strings.Repeat("model-", 80)
	detail.Code = strings.Repeat("code-", 60)
	summary = providerRequestOperatorSummary(detail)
	if len(summary) > providerOperatorSummaryBytes || redact.OperatorText(summary) != summary {
		t.Fatalf("pathological summary exceeded bound: len=%d summary=%q", len(summary), summary)
	}
}

// A release-branch failure has to generate against, and open against, its own
// branch. Resolving the default branch drafts a patch for the wrong snapshot.
func TestAnalysisPreviewPinsAndTargetsFailureBranch(t *testing.T) {
	releaseHead := "8caa35df8680f64693a3f76ea3d35c2349ab4828"
	failure := validAnalysisFailure()
	failure.SourceBranch, failure.TargetBranch = "release-1.25", "release-1.25"
	failure.GenerationBaseRevision = releaseHead
	pr := &fakePR{base: ghpr.Base{Branch: "release-1.25", HeadSHA: releaseHead, TreeSHA: "releasetree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})

	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.spec.Repo.Ref != releaseHead {
		t.Fatalf("generation ref = %s, want the release branch head", agent.spec.Repo.Ref)
	}
	url, err := manager.OpenFromPreview(t.Context(), fix)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.opened) != 1 || pr.opened[0].Base == nil || pr.opened[0].Base.Branch != "release-1.25" {
		t.Fatalf("url=%q opened=%+v", url, pr.opened)
	}
	for _, branch := range pr.resolveBranches {
		if branch != "release-1.25" {
			t.Fatalf("resolved branches = %v, want only the failure branch", pr.resolveBranches)
		}
	}
}

func TestAnalysisPreviewTargetsDefaultBranchAndSuggestsBackport(t *testing.T) {
	mainHead := "c83d69ab8c572a4c00816076222d65262ee690cc"
	failure := validAnalysisFailure()
	failure.SourceBranch, failure.TargetBranch = "release-1.24", "main"
	failure.FailureRevision, failure.GenerationBaseRevision = "a866aca055bcaa205648e81d15c67668179fdfab", mainHead
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: mainHead, TreeSHA: "maintree"}}
	agent := goodAgent()
	manager := newManager(t, pr, agent, Options{})

	fix, err := manager.GenerateAnalysisPreview(t.Context(), failure, "")
	if err != nil {
		t.Fatal(err)
	}
	if agent.spec.Repo.Ref != mainHead {
		t.Fatalf("generation ref = %s, want the default branch head", agent.spec.Repo.Ref)
	}
	for _, want := range []string{
		`observed on branch "release-1.24" at revision ` + failure.FailureRevision,
		`patching branch "main" at generation base ` + mainHead,
		"code may differ", `If the causal logic is not present on branch "main", make no change`,
	} {
		if !strings.Contains(agent.spec.Instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, agent.spec.Instruction)
		}
	}
	for _, want := range []string{
		"observed on `release-1.24` at `" + failure.FailureRevision + "`", "This change targets `main`",
		"comment `/cherry-pick release-1.24` to backport it", "on branch `release-1.24`", "on branch `main`",
	} {
		if !strings.Contains(fix.Description, want) || !strings.Contains(fix.Body, want) {
			t.Fatalf("description or body missing %q:\n%s\n%s", want, fix.Description, fix.Body)
		}
	}
	for line := range strings.SplitSeq(fix.Body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "/cherry-pick") {
			t.Fatalf("PR body contains a bare Prow command: %q", line)
		}
	}
	if _, err := manager.OpenFromPreview(t.Context(), fix); err != nil {
		t.Fatal(err)
	}
	if len(pr.opened) != 1 || pr.opened[0].Base == nil || pr.opened[0].Base.Branch != "main" {
		t.Fatalf("opened=%+v", pr.opened)
	}
	for _, branch := range pr.resolveBranches {
		if branch != "main" {
			t.Fatalf("resolved branches = %v, want only the target branch", pr.resolveBranches)
		}
	}

	tested := validAnalysisFailure()
	pr.base = ghpr.Base{Branch: "main", HeadSHA: exactAnalysisRevision, TreeSHA: "tree"}
	fix, err = manager.GenerateAnalysisPreview(t.Context(), tested, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fix.Description, "cherry-pick") || strings.Contains(agent.spec.Instruction, "make no change") {
		t.Fatalf("same-branch target suggested a backport: %s\n%s", fix.Description, agent.spec.Instruction)
	}
}

func TestProviderCredentialOperatorSummaryDistinguishesStatusAndRedactsMessage(t *testing.T) {
	base := runtime.ProviderErrorDetail{
		Message:        "request failed Authorization: Bearer ghp-fixture-secret token=second-secret",
		ProviderID:     "github-copilot",
		AuthSecretName: "agent-sandbox-model",
		AuthSecretKey:  "AI_TOKEN",
		Endpoint:       "https://api.githubcopilot.com/chat/completions",
		Model:          "gpt-fixture",
	}

	unauthorized := base
	unauthorized.StatusCode = 401
	unauthorizedSummary := providerCredentialOperatorSummary(&unauthorized)
	for _, want := range []string{
		"HTTP 401: credential rejected; check it.", "Secret agent-sandbox-model/AI_TOKEN",
		"endpoint https|api.githubcopilot.com/chat/completions", "model gpt-fixture", "Provider github-copilot",
	} {
		if !strings.Contains(unauthorizedSummary, want) {
			t.Fatalf("401 summary missing %q: %s", want, unauthorizedSummary)
		}
	}

	forbidden := base
	forbidden.StatusCode = 403
	forbiddenSummary := providerCredentialOperatorSummary(&forbidden)
	for _, want := range []string{"HTTP 403: request refused", "provider entitlement", "organization policy", "quota", "proxy or mesh authorization"} {
		if !strings.Contains(forbiddenSummary, want) {
			t.Fatalf("403 summary missing %q: %s", want, forbiddenSummary)
		}
	}
	if strings.Contains(forbiddenSummary, "invalid credential") || strings.Contains(forbiddenSummary, "credential rejected") {
		t.Fatalf("403 summary overstates credential failure: %s", forbiddenSummary)
	}
	for _, secret := range []string{"ghp-fixture-secret", "second-secret"} {
		if strings.Contains(unauthorizedSummary, secret) || strings.Contains(forbiddenSummary, secret) {
			t.Fatalf("provider summary disclosed %q: 401=%q 403=%q", secret, unauthorizedSummary, forbiddenSummary)
		}
	}
	for _, statusCode := range []int{401, 403} {
		detail := base
		detail.StatusCode = statusCode
		detail.Code = "model_access_denied"
		summary := providerCredentialOperatorSummary(&detail)
		want := fmt.Sprintf("HTTP %d model_access_denied:", statusCode)
		if !strings.Contains(summary, want) || len(summary) > providerOperatorSummaryBytes {
			t.Fatalf("provider code missing or unbounded: %q", summary)
		}
	}
}

func TestProviderCredentialOperatorSummaryPreservesLongMessageThroughAPIRedaction(t *testing.T) {
	message := strings.Repeat("provider-detail-", 25)
	if len(message) != 400 {
		t.Fatalf("fixture message is %d bytes", len(message))
	}
	for _, statusCode := range []int{401, 403} {
		t.Run(fmt.Sprintf("HTTP_%d", statusCode), func(t *testing.T) {
			summary := providerCredentialOperatorSummary(&runtime.ProviderErrorDetail{
				StatusCode: statusCode, Message: message, ProviderID: "github-copilot",
				AuthSecretName: "capz-aster-fix-model", AuthSecretKey: "AI_TOKEN",
				Endpoint: "https://api.githubcopilot.com/chat/completions", Model: "gpt-fixture",
			})
			visible := redact.OperatorText(summary)
			if visible != summary || len(summary) != providerOperatorSummaryBytes {
				t.Fatalf("summary did not fill the bounded output: len=%d summary=%q visible=%q", len(summary), summary, visible)
			}
			messageStart := strings.Index(summary, " Provider message: ") + len(" Provider message: ")
			if messageStart < len(" Provider message: ") {
				t.Fatalf("summary omitted provider message label: %q", summary)
			}
			renderedMessage := summary[messageStart:]
			minimumPrefix := providerMessageSummaryBytes - len("…")
			if len(renderedMessage) < providerMessageSummaryBytes ||
				!strings.HasPrefix(renderedMessage, message[:minimumPrefix]) || !strings.HasSuffix(renderedMessage, "…") {
				t.Fatalf("provider message budget was not preserved: len=%d message=%q", len(renderedMessage), renderedMessage)
			}
			for _, reference := range []string{
				"capz-aster-fix-model", "AI_TOKEN",
				"https", "api.githubcopilot.com/chat/completions", "gpt-fixture", "github-copilot",
			} {
				if !strings.Contains(visible, reference) {
					t.Fatalf("operator summary omitted %q: %q", reference, visible)
				}
			}
			if statusCode == 403 && !strings.Contains(visible, "Check credential access or provider entitlement, organization policy, quota, and proxy or mesh authorization.") {
				t.Fatalf("403 summary omitted advisory: %q", visible)
			}
		})
	}
}

func TestProviderConfigSummaryDistinguishesLongSecretNames(t *testing.T) {
	prefix := "shared-analysis-fix-secret-"
	suffixes := []string{"-primary", "-fallback"}
	summaries := make([]string, 0, len(suffixes))
	for _, suffix := range suffixes {
		name := prefix + strings.Repeat("a", 253-len(prefix)-len(suffix)) + suffix
		summary := providerCredentialOperatorSummary(&runtime.ProviderErrorDetail{
			StatusCode: 403, Message: strings.Repeat("provider-detail-", 25), ProviderID: strings.Repeat("p", 200),
			AuthSecretName: name, AuthSecretKey: "AI_TOKEN",
			Endpoint: "https://" + strings.Repeat("endpoint", 80) + ".example/chat/completions",
			Model:    strings.Repeat("model", 100),
		})
		if !strings.Contains(summary, suffix) {
			t.Fatalf("summary lost distinguishing Secret suffix %q: %q", suffix, summary)
		}
		summaries = append(summaries, summary)
	}
	if summaries[0] == summaries[1] {
		t.Fatalf("distinct Secret names rendered identically: %q", summaries[0])
	}
}

func TestProviderCredentialOperatorSummaryBoundsPathologicalInputs(t *testing.T) {
	summary := providerCredentialOperatorSummary(&runtime.ProviderErrorDetail{
		StatusCode: 403, Message: strings.Repeat("provider-detail-", 100), ProviderID: strings.Repeat("p", 500),
		AuthSecretName: strings.Repeat("s", 253), AuthSecretKey: strings.Repeat("k", 253),
		Endpoint: "https://" + strings.Repeat("endpoint", 100) + ".example/chat/completions",
		Model:    strings.Repeat("model", 200),
	})
	visible := redact.OperatorText(summary)
	if len(summary) > providerOperatorSummaryBytes || visible != summary {
		t.Fatalf("pathological summary escaped its bound: len=%d summary=%q visible=%q", len(summary), summary, visible)
	}
	for _, component := range []string{"Secret ", "/", "endpoint ", "model ", "Provider ", "Provider message: ", "proxy or mesh authorization"} {
		if !strings.Contains(summary, component) {
			t.Fatalf("pathological summary omitted %q: %q", component, summary)
		}
	}
}
