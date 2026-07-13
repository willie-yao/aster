package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q, want immutable long cache", got)
	}

	// A client-side route falls back to index.html.
	resp, err = http.Get(srv.URL + "/job/periodic-x/test/foo")
	if err != nil {
		t.Fatalf("GET deep link: %v", err)
	}
	if got := readBody(t, resp); got != "<!doctype html><title>app</title>" {
		t.Errorf("deep-link body = %q, want index.html", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index fallback Cache-Control = %q, want no-cache", got)
	}
}

// TestHandler_DataNoCache verifies the frequently-rewritten data files are
// served no-cache so browsers revalidate instead of serving a stale copy.
func TestHandler_DataNoCache(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "dashboard.json", `{"generated_at":"now"}`)

	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/data/dashboard.json")
	if err != nil {
		t.Fatalf("GET dashboard.json: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("data Cache-Control = %q, want no-cache", got)
	}
}

// TestHandler_SecurityHeaders verifies the hardening response headers are set on
// every route.
func TestHandler_SecurityHeaders(t *testing.T) {
	dataDir := t.TempDir()
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	resp.Body.Close()
	want := map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
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

// fakeRunner records calls and returns canned drafts/URLs, or a not-found error
// for the "missing" id/token.
type fakeRunner struct {
	gotID, gotToken, gotInstruction, gotConfirmToken string
	gotResolveID, gotResolveLogin, gotResolveNote    string
	gotUnresolveID                                   string
}

func (f *fakeRunner) PreviewIssue(ctx context.Context, id, token, instruction string) (actions.PreviewResult, error) {
	if id == "missing" {
		return actions.PreviewResult{}, actions.ErrNotFound
	}
	f.gotID, f.gotToken, f.gotInstruction = id, token, instruction
	return actions.PreviewResult{Token: "ptok", Kind: "issue", Title: "T", Body: "B"}, nil
}
func (f *fakeRunner) PreviewFix(ctx context.Context, id, token, instruction string) (actions.PreviewResult, error) {
	f.gotID, f.gotToken, f.gotInstruction = id, token, instruction
	return actions.PreviewResult{Token: "ptok", Kind: "fix", Title: "T", Body: "B", Diff: "d"}, nil
}
func (f *fakeRunner) Confirm(ctx context.Context, token, userToken string) (string, error) {
	if token == "missing" {
		return "", actions.ErrPreviewNotFound
	}
	f.gotConfirmToken, f.gotToken = token, userToken
	return "https://github.com/o/r/issues/1", nil
}
func (f *fakeRunner) Resolve(id, login, note string) error {
	if id == "missing" {
		return actions.ErrNotFound
	}
	f.gotResolveID, f.gotResolveLogin, f.gotResolveNote = id, login, note
	return nil
}
func (f *fakeRunner) Unresolve(id string) error {
	if id == "missing" {
		return actions.ErrNotFound
	}
	f.gotUnresolveID = id
	return nil
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
	r, _ := http.Post(srv.URL+"/api/failures/x/create-issue/preview", "application/json", nil)
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when actions disabled", r.StatusCode)
	}
}

// notAdminAuth authenticates the request but denies authorization, so handlers
// see a 403.
type notAdminAuth struct{}

func (notAdminAuth) Authenticate(ctx context.Context, r *http.Request) (*auth.Identity, error) {
	return nil, auth.ErrNotAdmin
}

// writeEndpoints is every state-changing route the server exposes.
var writeEndpoints = []string{
	"/api/failures/abc/create-issue/preview",
	"/api/failures/abc/propose-fix/preview",
	"/api/actions/confirm",
	"/api/failures/abc/resolve",
	"/api/failures/abc/unresolve",
}

// TestHandler_WriteEndpointsRejectUnauthorized verifies every write endpoint
// rejects a request with no credential (401) and a non-admin credential (403),
// so no state-changing route is reachable without an authorized admin.
func TestHandler_WriteEndpointsRejectUnauthorized(t *testing.T) {
	post := func(h http.Handler, path string) int {
		srv := httptest.NewServer(h)
		defer srv.Close()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(`{"token":"x","note":"y"}`))
		// Same-origin so the CSRF guard is not what rejects the request; auth is.
		req.Header.Set("Origin", srv.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)

	noAuth, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, Actions: &fakeRunner{}})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	nonAdmin, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: notAdminAuth{}, Actions: &fakeRunner{}})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	for _, ep := range writeEndpoints {
		if got := post(noAuth, ep); got != http.StatusUnauthorized {
			t.Errorf("%s without credential = %d, want 401", ep, got)
		}
		if got := post(nonAdmin, ep); got != http.StatusForbidden {
			t.Errorf("%s with non-admin = %d, want 403", ep, got)
		}
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

	do := func(path, authz, body string) *http.Response {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, rdr)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if r := do("/api/failures/abc/create-issue/preview", "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth status = %d, want 401", r.StatusCode)
	}
	r := do("/api/failures/abc/create-issue/preview", "ok", `{"instruction":"tighten it"}`)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.StatusCode)
	}
	var draft map[string]string
	json.NewDecoder(r.Body).Decode(&draft)
	r.Body.Close()
	if draft["token"] == "" {
		t.Error("expected token in preview response")
	}
	if runner.gotID != "abc" || runner.gotToken != "tok" || runner.gotInstruction != "tighten it" {
		t.Errorf("runner got id=%q token=%q instruction=%q, want abc/tok/tighten it", runner.gotID, runner.gotToken, runner.gotInstruction)
	}
	if r := do("/api/failures/missing/create-issue/preview", "ok", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("not-found status = %d, want 404", r.StatusCode)
	}
	r = do("/api/actions/confirm", "ok", `{"token":"ptok"}`)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("confirm status = %d, want 200", r.StatusCode)
	}
	var confirmed map[string]string
	json.NewDecoder(r.Body).Decode(&confirmed)
	r.Body.Close()
	if confirmed["url"] == "" {
		t.Error("expected url in confirm response")
	}
	if runner.gotConfirmToken != "ptok" {
		t.Errorf("confirm got token=%q, want ptok", runner.gotConfirmToken)
	}
	if r := do("/api/actions/confirm", "ok", `{}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("blank-token status = %d, want 400", r.StatusCode)
	}
	// An unknown/expired token maps to 404.
	if r := do("/api/actions/confirm", "ok", `{"token":"missing"}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("expired-token status = %d, want 404", r.StatusCode)
	}
	if r := do("/api/failures/abc/resolve", "ok", `{"note":"fixed by test-infra #99"}`); r.StatusCode != http.StatusNoContent {
		t.Errorf("resolve status = %d, want 204", r.StatusCode)
	}
	if runner.gotResolveID != "abc" || runner.gotResolveLogin != "alice" || runner.gotResolveNote != "fixed by test-infra #99" {
		t.Errorf("resolve got id=%q login=%q note=%q", runner.gotResolveID, runner.gotResolveLogin, runner.gotResolveNote)
	}
	if r := do("/api/failures/missing/resolve", "ok", `{}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("resolve not-found status = %d, want 404", r.StatusCode)
	}
	if r := do("/api/failures/abc/unresolve", "ok", ``); r.StatusCode != http.StatusNoContent {
		t.Errorf("unresolve status = %d, want 204", r.StatusCode)
	}
	if runner.gotUnresolveID != "abc" {
		t.Errorf("unresolve got id=%q, want abc", runner.gotUnresolveID)
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
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/failures/abc/create-issue/preview", nil)
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

// TestCSRFGuard_TrustedOrigin verifies the guard accepts a cross-host Origin
// only when it is in the trusted set (the reverse-proxy case), while keeping
// strict same-origin behavior otherwise.
func TestCSRFGuard_TrustedOrigin(t *testing.T) {
	trusted := trustedOriginSet([]string{"https://dash.example.net"})
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	guard := csrfGuard(trusted, ok)

	cases := []struct {
		name   string
		host   string
		origin string
		want   int
	}{
		{name: "no origin passes", host: "10.0.0.1", origin: "", want: http.StatusOK},
		{name: "same host passes", host: "dash.example.net", origin: "https://dash.example.net", want: http.StatusOK},
		{name: "trusted cross-host passes", host: "10.0.0.1", origin: "https://dash.example.net", want: http.StatusOK},
		{name: "untrusted cross-host rejected", host: "10.0.0.1", origin: "https://evil.example", want: http.StatusForbidden},
		{name: "malformed origin rejected", host: "10.0.0.1", origin: "://bad", want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://ignored/api/actions/confirm", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			guard.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestTrustedOriginSet_NormalizesToHosts(t *testing.T) {
	set := trustedOriginSet([]string{"https://a.example:8443", "  ", "b.example", ""})
	for _, want := range []string{"a.example:8443", "b.example"} {
		if _, ok := set[want]; !ok {
			t.Errorf("trusted set missing %q (got %v)", want, set)
		}
	}
	if len(set) != 2 {
		t.Errorf("set size = %d, want 2 (%v)", len(set), set)
	}
}
