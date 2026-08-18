package agentanalysis

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func TestValidatePublicGitHubSourceTree(t *testing.T) {
	source := sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}
	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "valid", body: `{"truncated":false,"tree":[{"mode":"040000","type":"tree","size":0},{"mode":"100644","type":"blob","size":12}]}`},
		{name: "truncated", body: `{"truncated":true,"tree":[]}`, wantErr: "truncated"},
		{name: "oversized", body: fmt.Sprintf(`{"truncated":false,"tree":[{"mode":"100644","type":"blob","size":%d}]}`, WorkspaceSourceMaxFileBytes+1), wantErr: "oversized"},
		{name: "submodule", body: `{"truncated":false,"tree":[{"mode":"160000","type":"commit","size":0}]}`, wantErr: "unsupported entry type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.Path, "/repos/example/repo/git/trees/") || r.URL.Query().Get("recursive") != "1" {
					t.Fatalf("request=%s", r.URL.String())
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			err := ValidatePublicGitHubSourceTree(t.Context(), server.Client(), server.URL, source)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error=%v want=%q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateWorkspaceSourceSnapshotRejectsOversizedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "large.bin")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(WorkspaceSourceMaxFileBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSourceSnapshot(t.Context(), root); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("error=%v", err)
	}
}
