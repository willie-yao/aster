package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

func TestEvidenceAttestorBindsScopeAndPath(t *testing.T) {
	attestor := newEvidenceAttestor("secret")
	token := attestor.issue("scope-a", "Artifacts/Manager.log")
	path, ok := attestor.verify("scope-a", token)
	if !ok || path != "artifacts/manager.log" {
		t.Fatalf("verify = %q, %t", path, ok)
	}
	if _, ok := attestor.verify("scope-b", token); ok {
		t.Fatal("token verified for another scope")
	}
	if _, ok := attestor.verify("scope-a", token+"x"); ok {
		t.Fatal("tampered token verified")
	}
}

func TestAttachEvidenceTokenRequiresSuccessfulContentRead(t *testing.T) {
	attestor := newEvidenceAttestor("secret")
	payload := map[string]interface{}{"path": "build-log.txt", "content": "failure"}
	attachEvidenceToken(attestor, "scope", "read_artifact", 1, payload)
	token, _ := payload["evidence_token"].(string)
	if _, ok := attestor.verify("scope", token); !ok {
		t.Fatal("successful read did not receive evidence token")
	}

	failed := map[string]interface{}{"path": "build-log.txt", "error": "not found"}
	attachEvidenceToken(attestor, "scope", "read_artifact", 1, failed)
	if _, ok := failed["evidence_token"]; ok {
		t.Fatal("failed read received evidence token")
	}
}

func TestArtifactReadTokenValidatesExactAnalysis(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "logs", "job", "1", "build-log.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("controller failed"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := newBuildResolver(storage.Config{Provider: storage.ProviderLocal, Base: root}, "", "logs/job/1/")
	if err != nil {
		t.Fatal(err)
	}
	env, _, err := resolver.aiEnv("", "logs/job/1/", "scope", storage.Config{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	filesystem.Register(registry)
	result := registry.Dispatch(context.Background(), env, "read_artifact", json.RawMessage(`{"path":"build-log.txt"}`))
	attestor := newEvidenceAttestor("secret")
	attachEvidenceToken(attestor, "scope", "read_artifact", 1, result.Payload)
	token, _ := result.Payload["evidence_token"].(string)
	if token == "" {
		t.Fatal("read result has no evidence token")
	}

	validationEnv, _, err := resolver.toolEnvFor("", "logs/job/1/", "scope", storage.Config{})
	if err != nil {
		t.Fatal(err)
	}
	validationEnv.evidence = attestor
	analysis := orka.AnalysisValidation{
		Summary: "summary", RootCause: "controller failed", Severity: "High",
		SuggestedFix: "fix controller", RelevantFiles: []string{"build-log.txt"},
	}
	response := runValidation(t, validationEnv, analysis, []string{token}, "scope", "")
	if response.Code != http.StatusOK {
		t.Fatalf("validation response = %d %s", response.Code, response.Body.String())
	}
}
