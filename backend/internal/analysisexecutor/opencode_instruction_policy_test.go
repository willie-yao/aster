package analysisexecutor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestOpenCode1182InstructionPolicyCompatibility(t *testing.T) {
	bin := os.Getenv("OPENCODE_1_18_2_BIN")
	if bin == "" {
		t.Skip("set OPENCODE_1_18_2_BIN to the exact patched OpenCode 1.18.2 executable")
	}
	workDir := t.TempDir()
	sourceRoot := filepath.Join(workDir, agentanalysis.WorkspaceSourcesDir, "primary")
	artifactRoot := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	for _, dir := range []string{
		filepath.Join(sourceRoot, "nested", "deeper"),
		filepath.Join(artifactRoot, "nested", "deeper"),
		filepath.Join(workDir, agentanalysis.WorkspaceResultDir),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	instructionFiles := map[string]string{
		filepath.Join(sourceRoot, "AGENTS.md"):                        "ASTER_UNTRUSTED_INSTRUCTION_CANARY_SOURCE_AGENTS\n",
		filepath.Join(sourceRoot, "nested", "CLAUDE.md"):              "ASTER_UNTRUSTED_INSTRUCTION_CANARY_SOURCE_CLAUDE\n",
		filepath.Join(sourceRoot, "nested", "deeper", "CONTEXT.md"):   "ASTER_UNTRUSTED_INSTRUCTION_CANARY_SOURCE_CONTEXT\n",
		filepath.Join(artifactRoot, "AGENTS.md"):                      "ASTER_UNTRUSTED_INSTRUCTION_CANARY_ARTIFACT_AGENTS\n",
		filepath.Join(artifactRoot, "nested", "CLAUDE.md"):            "ASTER_UNTRUSTED_INSTRUCTION_CANARY_ARTIFACT_CLAUDE\n",
		filepath.Join(artifactRoot, "nested", "deeper", "CONTEXT.md"): "ASTER_UNTRUSTED_INSTRUCTION_CANARY_ARTIFACT_CONTEXT\n",
	}
	ordinaryFiles := map[string]string{
		filepath.Join(artifactRoot, "root.log"):                     "ASTER_ORDINARY_ARTIFACT_ROOT\n",
		filepath.Join(artifactRoot, "nested", "nested.log"):         "ASTER_ORDINARY_ARTIFACT_NESTED\n",
		filepath.Join(artifactRoot, "nested", "deeper", "deep.log"): "ASTER_ORDINARY_ARTIFACT_DEEP\n",
		filepath.Join(sourceRoot, "root.go"):                        "package root // ASTER_ORDINARY_SOURCE_ROOT\n",
		filepath.Join(sourceRoot, "nested", "nested.go"):            "package nested // ASTER_ORDINARY_SOURCE_NESTED\n",
		filepath.Join(sourceRoot, "nested", "deeper", "deep.go"):    "package deeper // ASTER_ORDINARY_SOURCE_DEEP\n",
	}
	for path, content := range instructionFiles {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range ordinaryFiles {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runExecutorGit(t, sourceRoot, "init")
	runExecutorGit(t, sourceRoot, "config", "user.email", "fixture@example.test")
	runExecutorGit(t, sourceRoot, "config", "user.name", "Fixture")
	runExecutorGit(t, sourceRoot, "add", ".")
	runExecutorGit(t, sourceRoot, "commit", "-m", "fixture")
	beforeRevision := strings.TrimSpace(runExecutorGit(t, sourceRoot, "rev-parse", "HEAD"))
	beforeTree := strings.TrimSpace(runExecutorGit(t, sourceRoot, "rev-parse", "HEAD^{tree}"))
	beforeSource := instructionPolicyFileDigests(t, sourceRoot)
	beforeArtifacts := instructionPolicyFileDigests(t, artifactRoot)

	var handlerMu sync.Mutex
	var captured [][]byte
	var handlerErr error
	calls := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerMu.Lock()
		defer handlerMu.Unlock()
		data, err := io.ReadAll(io.LimitReader(r.Body, maxOpenCodeAPIResponseBytes+1))
		if err != nil || len(data) > maxOpenCodeAPIResponseBytes {
			handlerErr = fmt.Errorf("read provider request: bytes=%d err=%v", len(data), err)
			http.Error(w, "invalid synthetic request", http.StatusBadRequest)
			return
		}
		captured = append(captured, bytes.Clone(data))
		for _, forbidden := range []string{
			"ASTER_UNTRUSTED_INSTRUCTION_CANARY_", "Instructions from:", "AGENTS.md", "CLAUDE.md", "CONTEXT.md",
		} {
			if bytes.Contains(data, []byte(forbidden)) {
				handlerErr = fmt.Errorf("provider request contains ambient instruction data %q", forbidden)
				http.Error(w, "ambient instruction data", http.StatusBadRequest)
				return
			}
		}
		calls++
		switch calls {
		case 1:
			writeSyntheticOpenAIToolCalls(t, w, []syntheticOpenAIToolCall{
				{Name: "read", Arguments: map[string]any{"filePath": "artifacts/root.log"}},
				{Name: "read", Arguments: map[string]any{"filePath": "artifacts/nested/nested.log"}},
				{Name: "read", Arguments: map[string]any{"filePath": "artifacts/nested/deeper/deep.log"}},
				{Name: "read", Arguments: map[string]any{"filePath": "sources/primary/root.go"}},
				{Name: "read", Arguments: map[string]any{"filePath": "sources/primary/nested/nested.go"}},
				{Name: "read", Arguments: map[string]any{"filePath": "sources/primary/nested/deeper/deep.go"}},
			})
		case 2:
			writeSyntheticOpenAIText(t, w, "Ordinary evidence inspected.")
		case 3:
			writeSyntheticOpenAIStream(t, w, "StructuredOutput", map[string]any{
				"version": 1, "contract_version": agentanalysis.WorkspaceContractVersion,
				"summary":      "Ordinary artifact and source evidence were read without ambient repository instructions.",
				"is_transient": false, "root_cause": "The ordinary deep artifact and source markers were inspected.",
				"severity": "Low", "suggested_fix": "", "relevant_file_ids": []string{"source-003"},
				"artifact_evidence_ids": []string{"artifact-003"}, "source_evidence_ids": []string{"source-003"},
				"unresolved_details": []string{},
			})
		default:
			handlerErr = fmt.Errorf("unexpected provider request %d", calls)
			http.Error(w, "unexpected provider request", http.StatusBadRequest)
		}
	}))
	defer gateway.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	result, err := defaultRunOpenCode(ctx, OpenCodeSpec{
		Bin: bin, WorkDir: workDir, HomeDir: t.TempDir(), TempDir: t.TempDir(),
		Provider: testGatewayProvider(gateway.URL+"/v1/chat/completions", "synthetic-model"),
		Prompt:   "Read the six ordinary source and artifact files specified by the evidence task, then return one structured result.",
		MaxSteps: 8, ModelContextTokens: 200000, ModelOutputTokens: 8192,
	})
	handlerMu.Lock()
	observedHandlerErr := handlerErr
	observedCalls := calls
	observedCaptured := append([][]byte(nil), captured...)
	handlerMu.Unlock()
	if observedHandlerErr != nil {
		t.Fatal(observedHandlerErr)
	}
	if err != nil {
		t.Fatalf("err=%v result=%+v", err, result)
	}
	if observedCalls != 3 || len(observedCaptured) != 3 {
		t.Fatalf("calls=%d captured=%d", observedCalls, len(observedCaptured))
	}
	joined := bytes.Join(observedCaptured, nil)
	for _, marker := range ordinaryFiles {
		if !bytes.Contains(joined, []byte(strings.TrimSpace(marker))) {
			t.Fatalf("ordinary file content did not reach the model: %q", marker)
		}
	}
	if len(result.EvidenceHandles) != 6 {
		t.Fatalf("evidence handles=%+v", result.EvidenceHandles)
	}
	files, err := agentanalysis.SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := agentanalysis.NewWorkspaceManifest(
		ai.FailureAnalysisRequest{JobID: "periodic::instruction-policy", BuildPrefix: "logs/instruction-policy/1/", Build: models.BuildInfo{BuildID: "1", JobName: "instruction-policy"}, TestCase: models.TestCase{Name: "TestInstructionPolicy", Status: "failed", FailureMessage: "synthetic failure"}},
		sourceinvestigation.Repository{Owner: "synthetic", Name: "instruction-policy", Revision: beforeRevision},
		"Inspect ordinary evidence.", files,
	)
	if err != nil {
		t.Fatal(err)
	}
	analysis, validation, err := agentanalysis.ParseWorkspaceAnalysis(string(result.Structured), result.EvidenceHandles, manifest, artifactRoot, filepath.Join(workDir, agentanalysis.WorkspaceSourcesDir))
	if err != nil {
		t.Fatal(err)
	}
	if validation.Status != agentanalysis.WorkspaceResultAccepted || len(analysis.EvidenceCitations) != 1 || len(analysis.SourceCitations) != 1 {
		t.Fatalf("analysis=%+v validation=%+v", analysis, validation)
	}
	artifactCitation := analysis.EvidenceCitations[0]
	artifactContent, ok := ordinaryFiles[filepath.Join(artifactRoot, filepath.FromSlash(artifactCitation.Path))]
	if !ok || !strings.Contains(artifactContent, artifactCitation.Quote) {
		t.Fatalf("artifact citation was not reconstructed from ordinary evidence: %+v", artifactCitation)
	}
	sourceCitation := analysis.SourceCitations[0]
	sourceContent, ok := ordinaryFiles[filepath.Join(sourceRoot, filepath.FromSlash(sourceCitation.Path))]
	if !ok || !strings.Contains(sourceContent, sourceCitation.Quote) || !sourceCitation.Verified {
		t.Fatalf("source citation was not reconstructed from ordinary evidence: %+v", sourceCitation)
	}
	if after := strings.TrimSpace(runExecutorGit(t, sourceRoot, "rev-parse", "HEAD")); after != beforeRevision {
		t.Fatalf("source revision changed: before=%s after=%s", beforeRevision, after)
	}
	if after := strings.TrimSpace(runExecutorGit(t, sourceRoot, "rev-parse", "HEAD^{tree}")); after != beforeTree {
		t.Fatalf("source tree changed: before=%s after=%s", beforeTree, after)
	}
	if status := strings.TrimSpace(runExecutorGit(t, sourceRoot, "status", "--porcelain")); status != "" {
		t.Fatalf("source workspace changed: %s", status)
	}
	if after := instructionPolicyFileDigests(t, sourceRoot); !equalStringMap(beforeSource, after) {
		t.Fatalf("source bytes changed: before=%v after=%v", beforeSource, after)
	}
	if after := instructionPolicyFileDigests(t, artifactRoot); !equalStringMap(beforeArtifacts, after) {
		t.Fatalf("artifact bytes changed: before=%v after=%v", beforeArtifacts, after)
	}
}

func instructionPolicyFileDigests(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(relative)] = hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func equalStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
