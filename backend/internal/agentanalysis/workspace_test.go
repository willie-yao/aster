package agentanalysis

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

func TestWorkspaceManifestIsDeterministicAndVerifiable(t *testing.T) {
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	reversed := slices.Clone(files)
	slices.Reverse(reversed)
	first, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewWorkspaceManifest(request, source, "Inspect this project.", reversed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || len(first.Artifacts) != 2 || first.Artifacts[0].Path != "junit.xml" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if err := VerifySourceWorkspace(context.Background(), sourceRoot, source.Revision); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifactWorkspace(artifactRoot, first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "unexpected.log"), []byte("extra\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifactWorkspace(artifactRoot, first); err == nil {
		t.Fatal("extra artifact file was accepted")
	}
}

func TestSnapshotArtifactWorkspaceRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.log")); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotArtifactWorkspace(root); err == nil {
		t.Fatal("artifact symlink was accepted")
	}
}

func TestParseWorkspaceAnalysisValidatesCitationsAndMapsResult(t *testing.T) {
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{
  "version": 1,
  "contract_version": "agent-analysis-workspace-v1",
  "summary": "The controller rejected the request.",
  "is_transient": false,
  "root_cause": "The specific failure occurred before cleanup.",
  "severity": "High",
  "suggested_fix": "Correct the request before retrying.",
  "relevant_files": ["pkg/controller.go"],
  "evidence_citations": [{"path":"logs/build.log","line_start":2,"line_end":2,"quote":"artifact-only-marker specific failure"}],
  "source_citations": [{"path":"pkg/controller.go","line_start":3,"line_end":3,"quote":"func reconcile()"}],
  "unresolved_details": ["The caller configuration is unavailable."]
}`
	analysis, err := ParseWorkspaceAnalysis(raw, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.EvidenceCitations) != 1 || analysis.EvidenceCitations[0].LineStart != 2 || len(analysis.SourceCitations) != 1 || !analysis.SourceCitations[0].Verified {
		t.Fatalf("analysis=%+v", analysis)
	}
	mapped := analysis.FailureAnalysisResult("2026-08-10T22:00:00Z", "test-model", 1234, WorkspaceUsage{Available: true, ModelRequests: 2, InputTokens: 100, OutputTokens: 20})
	if mapped.Summary == nil || mapped.Analysis == nil || mapped.Analysis.Mode != "agent-sandbox-opencode" || mapped.Analysis.EvidenceCitations[0].Path != "logs/build.log" || mapped.Analysis.ModelRequests != 2 {
		t.Fatalf("mapped=%+v", mapped)
	}
}

func TestParseWorkspaceAnalysisRejectsUngroundedPaths(t *testing.T) {
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]any{
		"version": 1, "contract_version": WorkspaceContractVersion,
		"summary": "summary", "is_transient": false, "root_cause": "cause", "severity": "High", "suggested_fix": "fix",
		"relevant_files": []string{}, "source_citations": []any{}, "unresolved_details": []any{},
	}
	for _, citation := range []map[string]any{
		{"path": "missing.log", "line_start": 1, "line_end": 1, "quote": "missing"},
		{"path": "../escape.log", "line_start": 1, "line_end": 1, "quote": "missing"},
	} {
		base["evidence_citations"] = []any{citation}
		data, _ := json.Marshal(base)
		if _, err := ParseWorkspaceAnalysis(string(data), manifest, artifactRoot, sourceRoot); err == nil {
			t.Fatalf("citation=%v was accepted", citation)
		}
	}
}

func TestParseWorkspaceAnalysisRejectsGitMetadataCitation(t *testing.T) {
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	raw := `{
  "version": 1,
  "contract_version": "agent-analysis-workspace-v1",
  "summary": "summary",
  "is_transient": false,
  "root_cause": "cause",
  "severity": "High",
  "suggested_fix": "fix",
  "relevant_files": [".git/config"],
  "evidence_citations": [{"path":"logs/build.log","line_start":2,"line_end":2,"quote":"artifact-only-marker specific failure"}],
  "source_citations": [{"path":".git/config","line_start":1,"line_end":1,"quote":"core"}],
  "unresolved_details": []
}`
	if _, err := ParseWorkspaceAnalysis(raw, manifest, artifactRoot, sourceRoot); err == nil {
		t.Fatal("Git metadata citation was accepted")
	}
}

func TestWorkspaceExecutionRequestBindsPromptAndRuntime(t *testing.T) {
	_, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	gateway := engineruntime.ModelGatewayConfig{Endpoint: "https://model-gateway.prow-ai.svc.cluster.local:8443/v1", Model: "test-model", ProtocolVersion: "openai-chat-completions-v1"}
	execution, err := NewWorkspaceExecutionRequest(manifest, gateway, 5*time.Minute, 20, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := WorkspaceInstruction(execution, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "logs/build.log") || !strings.Contains(prompt, "Inspect this project.") || strings.Contains(prompt, "artifact-only-marker") {
		t.Fatalf("unexpected prompt: %s", prompt)
	}
	tampered := execution
	tampered.MaxSteps++
	if err := ValidateWorkspaceExecutionRequest(tampered); err == nil {
		t.Fatal("tampered request was accepted")
	}
}

func TestVerifySourceWorkspaceRejectsDirtyCheckout(t *testing.T) {
	sourceRoot, _, _, source := workspaceTestInputs(t)
	if err := os.WriteFile(filepath.Join(sourceRoot, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySourceWorkspace(context.Background(), sourceRoot, source.Revision); err == nil {
		t.Fatal("dirty source workspace was accepted")
	}
}

func workspaceTestInputs(t *testing.T) (string, string, ai.FailureAnalysisRequest, sourceinvestigation.Repository) {
	t.Helper()
	sourceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sourceRoot, "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "pkg", "controller.go"), []byte("package controller\n\nfunc reconcile() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, sourceRoot, "init", "-q")
	runWorkspaceGit(t, sourceRoot, "config", "user.name", "Test")
	runWorkspaceGit(t, sourceRoot, "config", "user.email", "test@example.com")
	runWorkspaceGit(t, sourceRoot, "config", "commit.gpgsign", "false")
	runWorkspaceGit(t, sourceRoot, "add", ".")
	runWorkspaceGit(t, sourceRoot, "commit", "-qm", "fixture")
	revision := strings.TrimSpace(runWorkspaceGit(t, sourceRoot, "rev-parse", "HEAD"))

	artifactRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(artifactRoot, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "logs", "build.log"), []byte("setup\nartifact-only-marker specific failure\ncleanup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "junit.xml"), []byte("<testsuite/>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := ai.FailureAnalysisRequest{
		JobID: "periodic::job", BuildPrefix: "logs/job/1/",
		Build:    models.BuildInfo{BuildID: "1", JobName: "job", RepoRefs: map[string]string{"example/repo": revision}},
		TestCase: models.TestCase{Name: "TestFailure", Status: "failed", FailureMessage: "specific failure", JUnitFile: "junit.xml"},
	}
	return sourceRoot, artifactRoot, request, sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: revision}
}

func runWorkspaceGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
