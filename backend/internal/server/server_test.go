package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
)

// writeFile writes content under dir, creating parents.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHandler_DataReadParity verifies that /data/* returns the fetcher output
// files byte-for-byte, including the jobs/ subdirectory.
func TestHandler_DataReadParity(t *testing.T) {
	dataDir := t.TempDir()
	files := map[string]string{
		"manifest.json":        `{"id":"demo"}`,
		"dashboard.json":       `{"jobs":[]}`,
		"flakiness.json":       `{"tests":[]}`,
		"search-index.json":    `{"entries":[]}`,
		"jobs/periodic-x.json": `{"job_id":"periodic-x"}`,
	}
	for rel, content := range files {
		writeFile(t, dataDir, rel, content)
	}

	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	for rel, want := range files {
		resp, err := http.Get(srv.URL + "/data/" + rel)
		if err != nil {
			t.Fatalf("GET %s: %v", rel, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", rel, resp.StatusCode)
		}
		if body != want {
			t.Errorf("GET %s: body = %q, want %q", rel, body, want)
		}
	}
}

// TestHandler_Capabilities verifies the descriptor shape served in server mode.
func TestHandler_Capabilities(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)

	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatalf("GET capabilities: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = resp.Body.Close()
	if got.Mode != "server" {
		t.Errorf("Mode = %q, want server", got.Mode)
	}
	if got.Features.Chat || got.Features.Actions {
		t.Errorf("Features = %+v, want all false at read parity", got.Features)
	}
}

// TestHandler_SPAFallback verifies deep links fall back to index.html while
// real asset files are served directly.
func TestHandler_SPAFallback(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)
	staticDir := t.TempDir()
	writeFile(t, staticDir, "index.html", "<!doctype html><title>app</title>")
	writeFile(t, staticDir, "assets/app.js", "console.log(1)")

	h, err := Handler(Options{DataDir: dataDir, StaticDir: staticDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// A real asset is served as-is.
	resp, err := http.Get(srv.URL + "/assets/app.js")
	if err != nil {
		t.Fatalf("GET asset: %v", err)
	}
	if got := readBody(t, resp); got != "console.log(1)" {
		t.Errorf("asset body = %q", got)
	}

	// A client-side route falls back to index.html.
	resp, err = http.Get(srv.URL + "/job/periodic-x/test/foo")
	if err != nil {
		t.Fatalf("GET deep link: %v", err)
	}
	if got := readBody(t, resp); got != "<!doctype html><title>app</title>" {
		t.Errorf("deep-link body = %q, want index.html", got)
	}
}

// TestHandler_NoDirectoryListing verifies /data/ does not expose a browsable
// listing of the output tree.
func TestHandler_NoDirectoryListing(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)
	writeFile(t, dataDir, "jobs/periodic-x.json", `{}`)

	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, p := range []string{"/data/", "/data/jobs/"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404 (no listing)", p, resp.StatusCode)
		}
	}
}

func TestHandler_MissingDataDir(t *testing.T) {
	if _, err := Handler(Options{DataDir: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Fatal("expected error for missing data dir")
	}
	if _, err := Handler(Options{DataDir: ""}); err == nil {
		t.Fatal("expected error for empty data dir")
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

// fakeAuth authorizes only when the Authorization header equals "ok".
type fakeAuth struct{}

func (fakeAuth) Authenticate(ctx context.Context, r *http.Request) (*auth.Identity, error) {
	if r.Header.Get("Authorization") == "ok" {
		return &auth.Identity{Login: "alice", Token: "tok"}, nil
	}
	return nil, auth.ErrNoToken
}

// fakeRunner records calls and returns a canned URL, or ErrNotFound for "missing".
type fakeRunner struct{ gotID, gotToken string }

func (f *fakeRunner) CreateIssue(ctx context.Context, id, token string) (string, error) {
	if id == "missing" {
		return "", actions.ErrNotFound
	}
	f.gotID, f.gotToken = id, token
	return "https://github.com/o/r/issues/1", nil
}
func (f *fakeRunner) ProposeFix(ctx context.Context, id, token string) (string, error) {
	return "https://github.com/o/r/pull/2", nil
}

func TestHandler_ActionsDisabledByDefault(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Capability advertises no actions.
	resp, _ := http.Get(srv.URL + "/api/capabilities")
	var caps Capabilities
	json.NewDecoder(resp.Body).Decode(&caps)
	resp.Body.Close()
	if caps.Features.Actions {
		t.Error("actions must be false when unconfigured")
	}
	// The route is not registered.
	r, _ := http.Post(srv.URL+"/api/failures/x/create-issue", "application/json", nil)
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when actions disabled", r.StatusCode)
	}
}

func TestHandler_ActionsEnabled(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)
	runner := &fakeRunner{}
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, Actions: runner})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Capability advertises actions.
	resp, _ := http.Get(srv.URL + "/api/capabilities")
	var caps Capabilities
	json.NewDecoder(resp.Body).Decode(&caps)
	resp.Body.Close()
	if !caps.Features.Actions {
		t.Error("actions must be true when configured")
	}

	do := func(path, authz string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Unauthorized.
	if r := do("/api/failures/abc/create-issue", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth status = %d, want 401", r.StatusCode)
	}
	// Authorized success returns the URL and passes the id + token through.
	r := do("/api/failures/abc/create-issue", "ok")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.StatusCode)
	}
	var body map[string]string
	json.NewDecoder(r.Body).Decode(&body)
	r.Body.Close()
	if body["url"] == "" {
		t.Error("expected url in response")
	}
	if runner.gotID != "abc" || runner.gotToken != "tok" {
		t.Errorf("runner got id=%q token=%q, want abc/tok", runner.gotID, runner.gotToken)
	}
	// Unknown failure maps to 404.
	if r := do("/api/failures/missing/create-issue", "ok"); r.StatusCode != http.StatusNotFound {
		t.Errorf("not-found status = %d, want 404", r.StatusCode)
	}
}

func TestHandler_CSRFCrossOriginRejected(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, Actions: &fakeRunner{}, AuthMode: "oauth", LoginURL: "/api/auth/login"})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// A cross-origin POST (Origin != Host) is rejected before auth runs.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/failures/abc/create-issue", nil)
	req.Header.Set("Authorization", "ok")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin status = %d, want 403", resp.StatusCode)
	}

	// The capability advertises the oauth auth info.
	cr, _ := http.Get(srv.URL + "/api/capabilities")
	var caps Capabilities
	json.NewDecoder(cr.Body).Decode(&caps)
	cr.Body.Close()
	if caps.Auth == nil || caps.Auth.Mode != "oauth" || caps.Auth.LoginURL == "" {
		t.Errorf("capability auth = %+v, want oauth with login url", caps.Auth)
	}
}
