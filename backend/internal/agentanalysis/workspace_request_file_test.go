package agentanalysis

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestWorkspaceExecutionRequestChunksRoundTripMaximumFileManifest(t *testing.T) {
	files := make([]WorkspaceFile, maxWorkspaceFiles)
	for index := range files {
		files[index] = WorkspaceFile{
			Path: fmt.Sprintf("logs/%04d.txt", index), Size: 1,
			SHA256: strings.Repeat(fmt.Sprintf("%x", index%16), 64),
		}
	}
	request := ai.FailureAnalysisRequest{
		JobID: "periodic::job", BuildPrefix: "logs/job/1/",
		Build:    models.BuildInfo{BuildID: "1", JobName: "job", RepoRefs: map[string]string{"example/repo": strings.Repeat("a", 40)}},
		TestCase: models.TestCase{Name: "TestFailure", Status: "failed", FailureMessage: "specific failure"},
	}
	manifest, err := NewWorkspaceManifest(request, sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := NewWorkspaceExecutionRequest(manifest, testGatewayProvider("https://gateway.example.svc.cluster.local/v1/chat/completions", "fixture"), time.Minute, 20, 200000, 64000, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(execution)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := EncodeWorkspaceExecutionRequestChunks(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != WorkspaceExecutionRequestChunkCount || chunks[1] == "" {
		t.Fatalf("chunks=%d second_empty=%t request_bytes=%d", len(chunks), chunks[1] == "", len(data))
	}
	values := map[string]string{}
	for index, value := range chunks {
		if len(value) > workspaceExecutionRequestChunkBytes {
			t.Fatalf("chunk %d bytes=%d", index, len(value))
		}
		values[WorkspaceExecutionRequestChunkEnv(index)] = value
	}
	reconstructed, err := DecodeWorkspaceExecutionRequestChunks(func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(reconstructed) != string(data) {
		t.Fatal("reconstructed request changed")
	}
	var got WorkspaceExecutionRequest
	if err := json.Unmarshal(reconstructed, &got); err != nil {
		t.Fatal(err)
	}
	if got.Hash != execution.Hash {
		t.Fatalf("request hash=%s want=%s", got.Hash, execution.Hash)
	}
}

func TestDecodeWorkspaceExecutionRequestChunksRejectsInvalidShapes(t *testing.T) {
	request := workspaceRequestFileFixture(t)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := EncodeWorkspaceExecutionRequestChunks(data)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(values []string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			for index, value := range values {
				if WorkspaceExecutionRequestChunkEnv(index) == name {
					return value, true
				}
			}
			return "", false
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func([]string) []string
	}{
		{name: "missing", mutate: func(values []string) []string { return values[:len(values)-1] }},
		{name: "sparse", mutate: func(values []string) []string {
			values[0], values[1] = "", values[0]
			return values
		}},
		{name: "invalid base64", mutate: func(values []string) []string { values[0] = "!"; return values }},
		{name: "oversized", mutate: func(values []string) []string {
			values[0] = strings.Repeat("A", workspaceExecutionRequestChunkBytes+1)
			return values
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := append([]string(nil), chunks...)
			if _, err := DecodeWorkspaceExecutionRequestChunks(lookup(tc.mutate(values))); err == nil {
				t.Fatal("invalid chunks were accepted")
			}
		})
	}
}

func TestWorkspaceExecutionRequestFileRoundTrip(t *testing.T) {
	root := t.TempDir()
	request := workspaceRequestFileFixture(t)
	if err := WriteWorkspaceExecutionRequestFile(root, request); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(WorkspaceExecutionRequestPath(root))
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatalf("mode=%v err=%v", info, err)
	}
	got, err := ReadWorkspaceExecutionRequestFile(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash != request.Hash {
		t.Fatalf("request hash=%s want=%s", got.Hash, request.Hash)
	}
}

func TestWorkspaceExecutionRequestFileRejectsUnsafeInputs(t *testing.T) {
	request := workspaceRequestFileFixture(t)
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("nonempty root", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "existing"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := WriteWorkspaceExecutionRequestFile(root, request); err == nil {
			t.Fatal("nonempty request root was accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(t.TempDir(), "request.json")
		if err := os.WriteFile(target, data, 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, WorkspaceExecutionRequestPath(root)); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadWorkspaceExecutionRequestFile(root); err == nil {
			t.Fatal("symlink request file was accepted")
		}
	})
	t.Run("trailing data", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(WorkspaceExecutionRequestPath(root), append(data, []byte("{}")...), 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadWorkspaceExecutionRequestFile(root); err == nil || !strings.Contains(err.Error(), "trailing data") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("tampered hash", func(t *testing.T) {
		root := t.TempDir()
		tampered := request
		tampered.MaxSteps++
		data, err := json.Marshal(tampered)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(WorkspaceExecutionRequestPath(root), data, 0o400); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadWorkspaceExecutionRequestFile(root); err == nil {
			t.Fatal("tampered request was accepted")
		}
	})
}

func workspaceRequestFileFixture(t *testing.T) WorkspaceExecutionRequest {
	t.Helper()
	_, artifactRoot, failure, source := workspaceTestInputs(t)
	files, err := SnapshotArtifactWorkspace(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := NewWorkspaceManifest(failure, source, "Inspect this project.", files)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewWorkspaceExecutionRequest(manifest, testGatewayProvider("https://gateway.example.svc.cluster.local/v1/chat/completions", "fixture"), time.Minute, 20, 200000, 64000, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
