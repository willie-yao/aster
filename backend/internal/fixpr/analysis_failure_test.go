package fixpr

import (
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
)

const exactAnalysisRevision = "0123456789abcdef0123456789abcdef01234567"

func validAnalysisFailure() AnalysisFailure {
	return AnalysisFailure{
		ID: "analysis::id", Project: "capz", JobID: "periodic-capz", JobName: "periodic-capz", BuildID: "123",
		TestName: "TestCluster", AnalysisGeneratedAt: "2026-08-13T01:00:00Z", AnalysisHash: "analysis-hash",
		RootCause: "the reconciler omitted the terminal state", SuggestedFix: "update the reconciler branch",
		AssistantAnswer:  "The artifact shows the terminal branch never calls `markReady`.",
		ChatResponseHash: "chat-hash", PreviewRequestHash: "preview-hash",
		ArtifactCitations: []Evidence{{Path: "artifacts/junit_01.xml", LineStart: 10, LineEnd: 12, Quote: "expected Ready"}},
		SourceRepository:  "up/stream", SourceRevision: exactAnalysisRevision,
		SourceFiles: []string{"controllers/cluster_controller.go"}, SourceVerification: "source-hash", FindingVerification: "finding-hash",
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
	for _, want := range []string{"exact failed JUnit analysis", "TestCluster", "ArtifactCitations", "source-hash", "preserve compatibility"} {
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

func TestAnalysisPreviewRequiresCurrentPinnedBaseAtGenerationAndConfirmation(t *testing.T) {
	failure := validAnalysisFailure()
	pr := &fakePR{base: ghpr.Base{Branch: "main", HeadSHA: strings.Repeat("b", 40), TreeSHA: "tree-b"}}
	manager := newManager(t, pr, goodAgent(), Options{})
	if _, err := manager.GenerateAnalysisPreview(t.Context(), failure, ""); err == nil || !strings.Contains(err.Error(), "no longer the current fix base") {
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
	if first.Snapshot().Key == second.Snapshot().Key || first.Snapshot().Key == third.Snapshot().Key {
		t.Fatal("selected chat or preview identity did not change Fix PR dedup key")
	}
}
