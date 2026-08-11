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

func TestWorkspaceStageRequestBindsManifest(t *testing.T) {
	_, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := NewWorkspaceStageRequest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceStageRequest(stage, manifest); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*WorkspaceStageRequest){
		"manifest":     func(value *WorkspaceStageRequest) { value.ManifestHash = strings.Repeat("0", 64) },
		"source":       func(value *WorkspaceStageRequest) { value.Source.Revision = strings.Repeat("1", 40) },
		"artifacts":    func(value *WorkspaceStageRequest) { value.Artifacts[0].SHA256 = strings.Repeat("2", 64) },
		"build prefix": func(value *WorkspaceStageRequest) { value.BuildPrefix = "other/" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := stage
			candidate.Artifacts = slices.Clone(stage.Artifacts)
			mutate(&candidate)
			if err := ValidateWorkspaceStageRequest(candidate, manifest); err == nil {
				t.Fatal("tampered stage request was accepted")
			}
		})
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
  "contract_version": "agent-analysis-workspace-v5",
  "summary": "The controller rejected the request.",
  "is_transient": false,
  "root_cause": "The specific failure occurred before cleanup.",
  "severity": "High",
  "suggested_fix": "Correct the request before retrying.",
  "relevant_files": ["pkg/controller.go"],
  "evidence_citations": [{"path":"logs/build.log","line_start":2,"line_end":2}],
  "source_citations": [{"path":"pkg/controller.go","line_start":3,"line_end":3}],
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
		{"path": "missing.log", "line_start": 1, "line_end": 1},
		{"path": "../escape.log", "line_start": 1, "line_end": 1},
	} {
		base["evidence_citations"] = []any{citation}
		data, _ := json.Marshal(base)
		if _, err := ParseWorkspaceAnalysis(string(data), manifest, artifactRoot, sourceRoot); err == nil {
			t.Fatalf("citation=%v was accepted", citation)
		}
	}
}

func TestSafeWorkspaceSourcePathRejectsGitMetadataCaseInsensitive(t *testing.T) {
	for _, path := range []string{".git/config", ".GIT/config", ".Git/config", ".gIt"} {
		if safeWorkspaceSourcePath(path) {
			t.Fatalf("Git metadata path %q was accepted", path)
		}
	}
	if !safeWorkspaceSourcePath("pkg/controller.go") {
		t.Fatal("regular source path was rejected")
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
  "contract_version": "agent-analysis-workspace-v5",
  "summary": "summary",
  "is_transient": false,
  "root_cause": "cause",
  "severity": "High",
  "suggested_fix": "fix",
  "relevant_files": [".git/config"],
  "evidence_citations": [{"path":"logs/build.log","line_start":2,"line_end":2}],
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
	execution, err := NewWorkspaceExecutionRequest(manifest, gateway, 5*time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := WorkspaceInstruction(execution, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "logs/build.log") || !strings.Contains(prompt, "Inspect this project.") || !strings.Contains(prompt, "Omit the leading `artifacts/`") || !strings.Contains(prompt, "StructuredOutput") || strings.Contains(prompt, "artifact-only-marker") || strings.Contains(prompt, `"source/path.go"`) {
		t.Fatalf("unexpected prompt: %s", prompt)
	}
	tampered := execution
	tampered.MaxSteps++
	if err := ValidateWorkspaceExecutionRequest(tampered); err == nil {
		t.Fatal("tampered request was accepted")
	}
	tampered = execution
	tampered.ResultSchemaHash = strings.Repeat("0", 64)
	if err := ValidateWorkspaceExecutionRequest(tampered); err == nil {
		t.Fatal("tampered result schema was accepted")
	}
	for name, mutate := range map[string]func(*WorkspaceExecutionRequest){
		"context": func(value *WorkspaceExecutionRequest) { value.ModelContextTokens++ },
		"output":  func(value *WorkspaceExecutionRequest) { value.ModelOutputTokens++ },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := execution
			mutate(&candidate)
			if err := ValidateWorkspaceExecutionRequest(candidate); err == nil {
				t.Fatal("tampered model limit was accepted")
			}
		})
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

func TestVerifySourceWorkspaceAcceptsMetadataOnlyChanges(t *testing.T) {
	sourceRoot, _, _, source := workspaceTestInputs(t)
	path := filepath.Join(sourceRoot, "pkg", "controller.go")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := info.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := gitWorkspaceOutput(t.Context(), sourceRoot, "diff-files", "--quiet", "--"); err == nil {
		t.Fatal("metadata-only fixture did not invalidate the index stat cache")
	}
	if _, err := gitWorkspaceOutput(t.Context(), sourceRoot, "diff", "--no-ext-diff", "--no-textconv", "--quiet", "--"); err != nil {
		t.Fatalf("content-aware diff rejected metadata-only change: %v", err)
	}
	if err := VerifySourceWorkspace(t.Context(), sourceRoot, source.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestVerifySourceWorkspaceRejectsStagedCheckout(t *testing.T) {
	sourceRoot, _, _, source := workspaceTestInputs(t)
	path := filepath.Join(sourceRoot, "pkg", "controller.go")
	if err := os.WriteFile(path, []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, sourceRoot, "add", "pkg/controller.go")
	if err := os.WriteFile(path, []byte("package controller\n\nfunc reconcile() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySourceWorkspace(t.Context(), sourceRoot, source.Revision); err == nil {
		t.Fatal("staged source modification hidden by restored working-tree content was accepted")
	}
}

func TestVerifySourceWorkspaceRejectsIgnoredFile(t *testing.T) {
	sourceRoot, _, _, source := workspaceTestInputs(t)
	if err := os.WriteFile(filepath.Join(sourceRoot, "ignored.txt"), []byte("ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySourceWorkspace(context.Background(), sourceRoot, source.Revision); err == nil {
		t.Fatal("ignored source file was accepted")
	}
}

func TestVerifySourceWorkspaceRejectsAssumeUnchangedModification(t *testing.T) {
	sourceRoot, _, _, source := workspaceTestInputs(t)
	runWorkspaceGit(t, sourceRoot, "update-index", "--assume-unchanged", "pkg/controller.go")
	if err := os.WriteFile(filepath.Join(sourceRoot, "pkg", "controller.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySourceWorkspace(context.Background(), sourceRoot, source.Revision); err == nil {
		t.Fatal("assume-unchanged source modification was accepted")
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
	if err := os.WriteFile(filepath.Join(sourceRoot, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
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

func TestVerifySourceWorkspaceAcceptsTrackedInternalSymlink(t *testing.T) {
	sourceRoot, _, _, source := workspaceTestInputs(t)
	if err := os.Symlink(filepath.Join("pkg", "controller.go"), filepath.Join(sourceRoot, "controller-link.go")); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, sourceRoot, "add", "controller-link.go")
	runWorkspaceGit(t, sourceRoot, "commit", "-qm", "add safe symlink")
	source.Revision = strings.TrimSpace(runWorkspaceGit(t, sourceRoot, "rev-parse", "HEAD"))
	if err := VerifySourceWorkspace(t.Context(), sourceRoot, source.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestVerifySourceWorkspaceRejectsUnsafeTrackedSymlinks(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name: "absolute",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink(filepath.Join(string(filepath.Separator), "tmp", "outside"), filepath.Join(root, "unsafe")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "escaping",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink(filepath.Join("..", "outside"), filepath.Join(root, "unsafe")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "chained escape",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Symlink("inner", filepath.Join(root, "unsafe")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join("..", "outside"), filepath.Join(root, "inner")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sourceRoot, _, _, source := workspaceTestInputs(t)
			test.setup(t, sourceRoot)
			runWorkspaceGit(t, sourceRoot, "add", "unsafe")
			if test.name == "chained escape" {
				runWorkspaceGit(t, sourceRoot, "add", "inner")
			}
			runWorkspaceGit(t, sourceRoot, "commit", "-qm", "add unsafe symlink")
			source.Revision = strings.TrimSpace(runWorkspaceGit(t, sourceRoot, "rev-parse", "HEAD"))
			if err := VerifySourceWorkspace(t.Context(), sourceRoot, source.Revision); err == nil {
				t.Fatal("unsafe tracked symlink was accepted")
			}
		})
	}
}

func TestVerifySourceWorkspaceRejectsDirectorySymlinkCycles(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string) []string
	}{
		{
			name: "self directory",
			setup: func(t *testing.T, root string) []string {
				t.Helper()
				if err := os.Symlink(".", filepath.Join(root, "loop")); err != nil {
					t.Fatal(err)
				}
				return []string{"loop"}
			},
		},
		{
			name: "parent directory",
			setup: func(t *testing.T, root string) []string {
				t.Helper()
				if err := os.Symlink("..", filepath.Join(root, "pkg", "up")); err != nil {
					t.Fatal(err)
				}
				return []string{"pkg/up"}
			},
		},
		{
			name: "direct symlink cycle",
			setup: func(t *testing.T, root string) []string {
				t.Helper()
				if err := os.Symlink("b", filepath.Join(root, "a")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("a", filepath.Join(root, "b")); err != nil {
					t.Fatal(err)
				}
				return []string{"a", "b"}
			},
		},
		{
			name: "long directory cycle",
			setup: func(t *testing.T, root string) []string {
				t.Helper()
				for _, directory := range []string{"a-dir", "b-dir"} {
					if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(root, directory, "keep"), []byte("keep\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(filepath.Join("..", "b-dir"), filepath.Join(root, "a-dir", "to-b")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join("..", "a-dir"), filepath.Join(root, "b-dir", "to-a")); err != nil {
					t.Fatal(err)
				}
				return []string{"a-dir", "b-dir"}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sourceRoot, _, _, source := workspaceTestInputs(t)
			paths := test.setup(t, sourceRoot)
			runWorkspaceGit(t, sourceRoot, append([]string{"add"}, paths...)...)
			runWorkspaceGit(t, sourceRoot, "commit", "-qm", "add directory symlink cycle")
			source.Revision = strings.TrimSpace(runWorkspaceGit(t, sourceRoot, "rev-parse", "HEAD"))
			if err := VerifySourceWorkspace(t.Context(), sourceRoot, source.Revision); err == nil {
				t.Fatal("directory symlink cycle was accepted")
			}
		})
	}
}

func TestVerifySourceWorkspaceRejectsSubmoduleMode(t *testing.T) {
	sourceRoot, _, _, source := workspaceTestInputs(t)
	head := strings.TrimSpace(runWorkspaceGit(t, sourceRoot, "rev-parse", "HEAD"))
	runWorkspaceGit(t, sourceRoot, "update-index", "--add", "--cacheinfo", "160000,"+head+",submodule")
	runWorkspaceGit(t, sourceRoot, "commit", "-qm", "add gitlink")
	source.Revision = strings.TrimSpace(runWorkspaceGit(t, sourceRoot, "rev-parse", "HEAD"))
	if err := VerifySourceWorkspace(t.Context(), sourceRoot, source.Revision); err == nil {
		t.Fatal("submodule index mode was accepted")
	}
}

func TestParseWorkspaceAnalysisCanonicalizesExactRanges(t *testing.T) {
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	if err := os.WriteFile(filepath.Join(artifactRoot, "crlf.log"), []byte("first\r\nsecond\r\nthird\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	raw := workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{map[string]any{"path": "crlf.log", "line_start": 1, "line_end": 2}}, nil)
	analysis, err := ParseWorkspaceAnalysis(raw, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := analysis.EvidenceCitations[0].Quote; got != "first\nsecond" {
		t.Fatalf("canonical quote = %q", got)
	}
}

func TestParseWorkspaceAnalysisRejectsAdversarialCitations(t *testing.T) {
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	longLines := strings.Repeat("line\n", maxCitationLines+1)
	if err := os.WriteFile(filepath.Join(artifactRoot, "long.log"), []byte(longLines), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "binary.log"), []byte{'x', 0, 'y'}, 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"zero range":      workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{map[string]any{"path": "logs/build.log", "line_start": 0, "line_end": 1}}, nil),
		"reversed range":  workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{map[string]any{"path": "logs/build.log", "line_start": 3, "line_end": 2}}, nil),
		"past eof":        workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 20}}, nil),
		"oversized range": workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{map[string]any{"path": "long.log", "line_start": 1, "line_end": maxCitationLines + 1}}, nil),
		"binary file":     workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{map[string]any{"path": "binary.log", "line_start": 1, "line_end": 1}}, nil),
		"overlap": workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{
			map[string]any{"path": "logs/build.log", "line_start": 1, "line_end": 2},
			map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 3},
		}, nil),
		"duplicate": workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{
			map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 2},
			map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 2},
		}, nil),
		"paraphrased quote": strings.Replace(workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 2}}, nil), `"line_end":2`, `"line_end":2,"quote":"paraphrase"`, 1),
		"duplicate field":   strings.Replace(workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 2}}, nil), `"summary":"summary"`, `"summary":"summary","summary":"other"`, 1),
		"malformed":         `{"version":1`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseWorkspaceAnalysis(raw, manifest, artifactRoot, sourceRoot); err == nil {
				t.Fatal("adversarial result was accepted")
			}
		})
	}
}

func TestValidateWorkspaceAnalysisRejectsNonCanonicalQuote(t *testing.T) {
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	raw := workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 2}}, nil)
	analysis, err := ParseWorkspaceAnalysis(raw, manifest, artifactRoot, sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	analysis.EvidenceCitations[0].Quote = "paraphrase"
	if _, err := ValidateWorkspaceAnalysis(analysis, manifest, artifactRoot, sourceRoot); err == nil {
		t.Fatal("non-canonical quote was accepted")
	}
}

func workspaceModelAnalysisJSON(contract string, evidence, source []any) string {
	if source == nil {
		source = []any{}
	}
	value := map[string]any{
		"version": 1, "contract_version": contract, "summary": "summary", "is_transient": false,
		"root_cause": "cause", "severity": "High", "suggested_fix": "fix", "relevant_files": []string{},
		"evidence_citations": evidence, "source_citations": source, "unresolved_details": []string{},
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func TestParseWorkspaceAnalysisRejectsSourceSymlinkAliasOverlap(t *testing.T) {
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	if err := os.Symlink("controller.go", filepath.Join(sourceRoot, "pkg", "alias.go")); err != nil {
		t.Fatal(err)
	}
	runWorkspaceGit(t, sourceRoot, "add", "pkg/alias.go")
	runWorkspaceGit(t, sourceRoot, "commit", "-qm", "add source alias")
	source.Revision = strings.TrimSpace(runWorkspaceGit(t, sourceRoot, "rev-parse", "HEAD"))
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	evidence := []any{map[string]any{"path": "logs/build.log", "line_start": 2, "line_end": 2}}
	sourceCitations := []any{
		map[string]any{"path": "pkg/controller.go", "line_start": 1, "line_end": 1},
		map[string]any{"path": "pkg/alias.go", "line_start": 1, "line_end": 1},
	}
	if _, err := ParseWorkspaceAnalysis(workspaceModelAnalysisJSON(WorkspaceContractVersion, evidence, sourceCitations), manifest, artifactRoot, sourceRoot); err == nil {
		t.Fatal("source symlink alias overlap was accepted")
	}
}
