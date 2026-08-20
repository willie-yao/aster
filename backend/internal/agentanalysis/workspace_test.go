package agentanalysis

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
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
		"manifest":           func(value *WorkspaceStageRequest) { value.ManifestHash = strings.Repeat("0", 64) },
		"source":             func(value *WorkspaceStageRequest) { value.Source.Revision = strings.Repeat("1", 40) },
		"artifacts":          func(value *WorkspaceStageRequest) { value.ArtifactManifestSHA256 = strings.Repeat("2", 64) },
		"build prefix":       func(value *WorkspaceStageRequest) { value.BuildPrefix = "other/" },
		"input mode policy":  func(value *WorkspaceStageRequest) { value.InputSourceModePolicy = WorkspaceSourceModeIgnoreExecutable },
		"output mode policy": func(value *WorkspaceStageRequest) { value.OutputSourceModePolicy = WorkspaceSourceModeIgnoreExecutable },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := stage
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
	handles := workspaceDefaultHandles(t, sourceRoot, artifactRoot)
	artifactID := workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 2)
	sourceID := workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 3)
	raw := workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{workspaceCitationSelection(artifactID)}, []any{workspaceCitationSelection(sourceID)})
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	value["summary"] = "The controller rejected the request."
	value["root_cause"] = "The specific failure occurred before cleanup."
	value["suggested_fix"] = "Correct the request before retrying."
	value["relevant_file_ids"] = []string{sourceID}
	value["unresolved_details"] = []string{"The caller configuration is unavailable."}
	data, _ := json.Marshal(value)
	analysis, validation, err := ParseWorkspaceAnalysis(string(data), handles, manifest, artifactRoot, workspaceTestSourcesRoot(t, sourceRoot))
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != WorkspaceResultAccepted || len(validation.Codes) != 0 {
		t.Fatalf("validation=%+v", validation)
	}
	if len(analysis.EvidenceCitations) != 1 || analysis.EvidenceCitations[0].LineStart != 2 || len(analysis.SourceCitations) != 1 || !analysis.SourceCitations[0].Verified {
		t.Fatalf("analysis=%+v", analysis)
	}
	mapped := analysis.FailureAnalysisResult("2026-08-10T22:00:00Z", "test-model", 1234, WorkspaceUsage{Available: true, ModelRequests: 2, InputTokens: 100, OutputTokens: 20})
	if mapped.Summary == nil || mapped.Analysis == nil || mapped.Analysis.Mode != "agent-sandbox-opencode" || mapped.Analysis.EvidenceCitations[0].Path != "logs/build.log" || mapped.Analysis.ModelRequests != 2 {
		t.Fatalf("mapped=%+v", mapped)
	}
}

func TestParseWorkspaceAnalysisWarnsOnUngroundedEvidenceIDs(t *testing.T) {
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	handles := workspaceDefaultHandles(t, sourceRoot, artifactRoot)
	for _, id := range []string{"artifact-999", workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 1)} {
		raw := workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{workspaceCitationSelection(id)}, nil)
		analysis, validation, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, workspaceTestSourcesRoot(t, sourceRoot))
		want := []string{WorkspaceInvalidArtifactCount, WorkspaceInvalidArtifactPath}
		if err != nil || validation.Status != WorkspaceResultAcceptedWithWarnings || !slices.Equal(validation.Codes, want) || len(analysis.EvidenceCitations) != 0 {
			t.Fatalf("evidence ID %q analysis=%+v validation=%+v err=%v", id, analysis, validation, err)
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
	provider := testGatewayProvider("https://model-gateway.prow-ai.svc.cluster.local:8443/v1", "test-model")
	execution, err := NewWorkspaceExecutionRequest(manifest, provider, 5*time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Version != WorkspaceRequestVersion {
		t.Fatalf("workspace request version = %d", execution.Version)
	}
	encoded, err := json.Marshal(execution)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"existing_secret", "token_key", "credential_value", "credential_hash"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("workspace request contains forbidden credential metadata %q", forbidden)
		}
	}
	prompt, err := WorkspaceInstruction(execution, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "logs/build.log") || !strings.Contains(prompt, "Inspect this project.") || !strings.Contains(prompt, "content-free evidence") || !strings.Contains(prompt, "StructuredOutput") || strings.Contains(prompt, "artifact-only-marker") || strings.Contains(prompt, `"source/path.go"`) {
		t.Fatalf("unexpected prompt: %s", prompt)
	}
	tampered := execution
	tampered.MaxSteps = 2
	tampered.Hash, err = workspaceRequestDigest(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceExecutionRequest(tampered); err == nil {
		t.Fatal("two-step request was accepted")
	}
	tampered = execution
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
		"context":     func(value *WorkspaceExecutionRequest) { value.ModelContextTokens++ },
		"output":      func(value *WorkspaceExecutionRequest) { value.ModelOutputTokens++ },
		"mode policy": func(value *WorkspaceExecutionRequest) { value.SourceModePolicy = WorkspaceSourceModeIgnoreExecutable },
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

func TestWorkspaceExecutionRequestAcceptsResponsesWithoutVersionChange(t *testing.T) {
	_, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := NewWorkspaceExecutionRequest(manifest, testGatewayProvider("https://model-gateway.prow-ai.svc.cluster.local:8443/v1/chat/completions", "test-model"), 5*time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	responses, err := NewWorkspaceExecutionRequest(manifest, testResponsesProvider("https://api.openai.com/v1/responses", "test-model"), 5*time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if responses.Version != WorkspaceRequestVersion || responses.Hash == chat.Hash || responses.ModelProvider.API != "responses" {
		t.Fatalf("chat=%+v responses=%+v", chat.ModelProvider, responses.ModelProvider)
	}
	highProvider := testResponsesProvider("https://api.openai.com/v1/responses", "test-model")
	highProvider.ReasoningEffort = modelprovider.ReasoningEffortHigh
	high, err := NewWorkspaceExecutionRequest(manifest, highProvider, 5*time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if high.Hash == responses.Hash || high.ModelProvider.ReasoningEffort != modelprovider.ReasoningEffortHigh {
		t.Fatalf("responses=%+v high=%+v", responses.ModelProvider, high.ModelProvider)
	}
}

func TestWorkspaceExecutionRequestSourceEvidenceFloorIsHashed(t *testing.T) {
	sourceRoot, artifactRoot, failure, source := workspaceTestInputs(t)
	_ = sourceRoot
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(failure, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	provider := testGatewayProvider("https://model-gateway.prow-ai.svc.cluster.local:8443/v1", "test-model")
	artifactOnly, err := NewWorkspaceExecutionRequestWithSourceEvidence(manifest, WorkspaceSourceModePreserve, false, provider, time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	sourceRequired, err := NewWorkspaceExecutionRequestWithSourceEvidence(manifest, WorkspaceSourceModePreserve, true, provider, time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if artifactOnly.RequireSourceEvidence || !sourceRequired.RequireSourceEvidence || artifactOnly.Hash == sourceRequired.Hash || sourceRequired.Version != WorkspaceRequestVersion {
		t.Fatalf("artifactOnly=%+v sourceRequired=%+v", artifactOnly, sourceRequired)
	}
	if _, err := NewWorkspaceExecutionRequestWithSourceEvidence(manifest, WorkspaceSourceModePreserve, true, provider, time.Minute, 4, 200000, 8192, 128<<10); err == nil {
		t.Fatal("source-required request without a corrective-turn budget was accepted")
	}
}

func TestWorkspaceInstructionSourceEvidenceRequirementIsGeneric(t *testing.T) {
	_, artifactRoot, failure, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(failure, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewWorkspaceExecutionRequestWithSourceEvidence(manifest, WorkspaceSourceModePreserve, true, testGatewayProvider("https://model-gateway.prow-ai.svc.cluster.local:8443/v1", "test-model"), time.Minute, 20, 200000, 8192, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := WorkspaceInstruction(request, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Required source grounding") || !strings.Contains(prompt, "canonical source evidence handle") {
		t.Fatalf("source requirement missing: %s", prompt)
	}
	for _, forbidden := range []string{"test/k8s-integration/main.go", "Windows snapshot", "expected diagnosis", "expected signal"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt leaked %q: %s", forbidden, prompt)
		}
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
	handles := workspaceTestHandles(t, sourceRoot, artifactRoot, WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "crlf.log", LineStart: 1, LineEnd: 2})
	raw := workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{workspaceCitationSelection("artifact-001"), workspaceCitationSelection("artifact-002")}, nil)
	analysis, _, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, workspaceTestSourcesRoot(t, sourceRoot))
	if err != nil {
		t.Fatal(err)
	}
	if len(analysis.EvidenceCitations) != 2 || analysis.EvidenceCitations[0].Quote != "first" || analysis.EvidenceCitations[1].Quote != "second" {
		t.Fatalf("canonical citations = %+v", analysis.EvidenceCitations)
	}
}

func TestParseWorkspaceAnalysisRejectsAdversarialCitations(t *testing.T) {
	sourceRoot, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	handles := workspaceDefaultHandles(t, sourceRoot, artifactRoot)
	valid := workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{workspaceCitationSelection(workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 2))}, nil)
	tests := map[string]string{
		"object selection": strings.Replace(valid, `"artifact_evidence_ids":["artifact-002"]`, `"artifact_evidence_ids":[{"evidence_id":"artifact-002"}]`, 1),
		"duplicate field":  strings.Replace(valid, `"summary":"summary"`, `"summary":"summary","summary":"other"`, 1),
		"malformed":        `{"version":1`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, workspaceTestSourcesRoot(t, sourceRoot)); err == nil {
				t.Fatal("adversarial result was accepted")
			}
		})
	}
	for name, raw := range map[string]string{
		"unknown evidence id": workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{workspaceCitationSelection("artifact-999")}, nil),
		"wrong evidence root": workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{workspaceCitationSelection("source-001")}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			analysis, validation, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, workspaceTestSourcesRoot(t, sourceRoot))
			if err != nil || validation.Status != WorkspaceResultAcceptedWithWarnings || len(analysis.EvidenceCitations) != 0 {
				t.Fatalf("analysis=%+v validation=%+v err=%v", analysis, validation, err)
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
	handles := workspaceDefaultHandles(t, sourceRoot, artifactRoot)
	raw := workspaceModelAnalysisJSON(WorkspaceContractVersion, []any{workspaceCitationSelection(workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 2))}, nil)
	analysis, _, err := ParseWorkspaceAnalysis(raw, handles, manifest, artifactRoot, workspaceTestSourcesRoot(t, sourceRoot))
	if err != nil {
		t.Fatal(err)
	}
	analysis.EvidenceCitations[0].Quote = "paraphrase"
	if _, _, err := ValidateWorkspaceAnalysis(analysis, manifest, artifactRoot, workspaceTestSourcesRoot(t, sourceRoot)); err == nil {
		t.Fatal("non-canonical quote was accepted")
	}
}

func workspaceModelAnalysisJSON(contract string, evidence, source []any) string {
	if source == nil {
		source = []any{}
	}
	value := map[string]any{
		"version": 1, "contract_version": contract, "summary": "summary", "is_transient": false,
		"root_cause": "cause", "severity": "High", "suggested_fix": "fix", "relevant_file_ids": []string{},
		"artifact_evidence_ids": evidence, "source_evidence_ids": source, "unresolved_details": []string{},
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func TestParseWorkspaceAnalysisDropsSourceSymlinkAliasOverlap(t *testing.T) {
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
	handles := workspaceTestHandles(t, sourceRoot, artifactRoot,
		WorkspaceEvidenceRange{Root: WorkspaceArtifactsDir, Path: "logs/build.log", LineStart: 2, LineEnd: 2},
		WorkspaceEvidenceRange{Root: WorkspaceSourceDir, SourceID: "primary", Path: "pkg/controller.go", LineStart: 1, LineEnd: 1},
		WorkspaceEvidenceRange{Root: WorkspaceSourceDir, SourceID: "primary", Path: "pkg/alias.go", LineStart: 1, LineEnd: 1},
	)
	evidence := []any{workspaceCitationSelection(workspaceHandleID(t, handles, WorkspaceArtifactsDir, "logs/build.log", 2))}
	sourceCitations := []any{
		workspaceCitationSelection(workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/controller.go", 1)),
		workspaceCitationSelection(workspaceHandleID(t, handles, WorkspaceSourceDir, "pkg/alias.go", 1)),
	}
	analysis, validation, err := ParseWorkspaceAnalysis(workspaceModelAnalysisJSON(WorkspaceContractVersion, evidence, sourceCitations), handles, manifest, artifactRoot, workspaceTestSourcesRoot(t, sourceRoot))
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != WorkspaceResultAcceptedWithWarnings || !slices.Equal(validation.Codes, []string{WorkspaceInvalidSourceOverlap}) || len(analysis.SourceCitations) != 1 {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
}

func TestWorkspaceGitEnvironmentExcludesProviderCredential(t *testing.T) {
	t.Setenv("PROW_AI_MODEL_PROVIDER_TOKEN", strings.Repeat("fixture-provider-credential-", 2))
	for _, value := range workspaceGitEnvironment() {
		if strings.HasPrefix(value, "PROW_AI_MODEL_PROVIDER_TOKEN=") {
			t.Fatal("workspace verifier inherited the provider credential")
		}
	}
}

func TestWorkspaceLargeArtifactManifestUsesCompactStageRequest(t *testing.T) {
	_, _, request, source := workspaceTestInputs(t)
	files := make([]WorkspaceFile, 0, maxWorkspaceFiles)
	for index := 0; index < maxWorkspaceFiles; index++ {
		files = append(files, WorkspaceFile{Path: fmt.Sprintf("artifacts/logs/%04d.log", index), Size: 1024, SHA256: strings.Repeat("a", 64)})
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := NewWorkspaceExecutionRequest(manifest, testGatewayProvider("https://gateway.example.internal/v1/chat/completions", "model"), time.Minute, 20, 250000, 16384, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := NewWorkspaceStageRequest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	publishStage, err := NewWorkspaceRemoteStageRequest(manifest, "https://storage.googleapis.com/bucket/build", WorkspaceSourceModePreserve)
	if err != nil {
		t.Fatal(err)
	}
	publish, err := NewWorkspacePublishRequest(publishStage, files, "analysis-large-fixture")
	if err != nil {
		t.Fatal(err)
	}
	executionJSON, _ := json.Marshal(execution)
	stageJSON, _ := json.Marshal(stage)
	publishJSON, _ := json.Marshal(publish)
	if len(executionJSON) > maxWorkspaceRequestBytes || len(stageJSON) > maxWorkspaceStageBytes || len(publishJSON) > WorkspacePublishRawMaxBytes || base64.StdEncoding.EncodedLen(len(publishJSON)) > WorkspacePublishEncodedMaxBytes || bytes.Contains(stageJSON, []byte("artifacts/logs/0001.log")) {
		t.Fatalf("encoded sizes execution=%d stage=%d publish=%d", len(executionJSON), len(stageJSON), len(publishJSON))
	}
	if stage.ArtifactFiles != len(files) || stage.ArtifactBytes != int64(len(files))*1024 || stage.ArtifactManifestSHA256 == "" {
		t.Fatalf("stage=%+v", stage)
	}
}

func TestWorkspaceManifestCanonicalizesMultipleSources(t *testing.T) {
	_, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	client := WorkspaceSourceRef{ID: "client", Repository: source}
	server := WorkspaceSourceRef{ID: "server", Repository: source}
	server.Repository.Revision = strings.Repeat("b", 40)
	first, err := NewWorkspaceManifestWithSources(request, []WorkspaceSourceRef{server, client}, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewWorkspaceManifestWithSources(request, []WorkspaceSourceRef{client, server}, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || !slices.Equal(first.Sources, []WorkspaceSourceRef{client, server}) {
		t.Fatalf("first=%+v second=%+v", first.Sources, second.Sources)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["sources"]; !ok {
		t.Fatal("multi-source wire field is absent")
	}
	if _, ok := wire["source"]; ok {
		t.Fatal("legacy singular source wire field remains")
	}
	changed := server
	changed.Repository.Revision = strings.Repeat("c", 40)
	third, err := NewWorkspaceManifestWithSources(request, []WorkspaceSourceRef{client, changed}, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	if third.Hash == first.Hash {
		t.Fatal("source catalog revision did not change the manifest hash")
	}
}

func TestWorkspaceManifestRejectsInvalidSourceCatalogs(t *testing.T) {
	_, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	valid := WorkspaceSourceRef{ID: "primary", Repository: source}
	for name, sources := range map[string][]WorkspaceSourceRef{
		"empty":              nil,
		"invalid ID":         {{ID: "Client_Latest", Repository: source}},
		"duplicate ID":       {valid, valid},
		"duplicate identity": {valid, {ID: "secondary", Repository: source}},
		"too many": func() []WorkspaceSourceRef {
			result := make([]WorkspaceSourceRef, 0, WorkspaceMaxSources+1)
			for index := 0; index <= WorkspaceMaxSources; index++ {
				repository := source
				repository.Revision = fmt.Sprintf("%040x", index+1)
				result = append(result, WorkspaceSourceRef{ID: fmt.Sprintf("source-%d", index), Repository: repository})
			}
			return result
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewWorkspaceManifestWithSources(request, sources, "Inspect this project.", files); err == nil {
				t.Fatal("invalid source catalog was accepted")
			}
		})
	}
}

func TestWorkspaceStageRequestCarriesPerSourcePolicies(t *testing.T) {
	_, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	other := source
	other.Revision = strings.Repeat("b", 40)
	manifest, err := NewWorkspaceManifestWithSources(request, []WorkspaceSourceRef{
		{ID: "client", Repository: source},
		{ID: "server", Repository: other},
	}, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	input := []WorkspaceSourceMode{{SourceID: "client", Policy: WorkspaceSourceModePreserve}, {SourceID: "server", Policy: WorkspaceSourceModeIgnoreExecutable}}
	output := []WorkspaceSourceMode{{SourceID: "client", Policy: WorkspaceSourceModeIgnoreExecutable}, {SourceID: "server", Policy: WorkspaceSourceModePreserve}}
	stage, err := NewWorkspaceStageRequestWithPolicies(manifest, input, output)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stage.Sources, manifest.Sources) || !slices.Equal(stage.InputSourceModePolicies, input) || !slices.Equal(stage.OutputSourceModePolicies, output) {
		t.Fatalf("stage=%+v", stage)
	}
	tampered := stage
	tampered.OutputSourceModePolicies = slices.Clone(stage.OutputSourceModePolicies)
	tampered.OutputSourceModePolicies[1].SourceID = "client"
	if err := ValidateWorkspaceStageRequest(tampered, manifest); err == nil {
		t.Fatal("non-canonical per-source policies were accepted")
	}
}

func TestWorkspaceManifestWireRejectsLegacySingularSource(t *testing.T) {
	_, artifactRoot, request, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(request, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	wire["source"] = wire["sources"].([]any)[0].(map[string]any)["repository"]
	data, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var decoded WorkspaceManifest
	if err := json.Unmarshal(data, &decoded); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error=%v", err)
	}
}
