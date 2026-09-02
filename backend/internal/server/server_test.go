package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/auth"
	"github.com/willie-yao/aster/backend/internal/output"
	"github.com/willie-yao/aster/backend/internal/statefile"
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

// preV5NoListFS models the rollback server's hiding rules. Its basename list
// intentionally does not know about the new action-write audit ledger.
type preV5NoListFS struct{ fs http.FileSystem }

func (f preV5NoListFS) Open(name string) (http.File, error) {
	if invalidDataPath(name) || hiddenDataPath(name) || hiddenDataBasename(name) {
		return nil, os.ErrNotExist
	}
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if info.IsDir() {
		file.Close()
		return nil, os.ErrNotExist
	}
	return file, nil
}

func TestActionWriteAuditPathIsHiddenByPreV5Rules(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, ".action-write-audit/state.json", `{"records":{}}`)
	writeFile(t, dataDir, "action_write_audit.json", `{"legacy":"exposed"}`)
	legacy := preV5NoListFS{fs: http.Dir(dataDir)}

	if file, err := legacy.Open("/.action-write-audit/state.json"); !errors.Is(err, os.ErrNotExist) {
		if file != nil {
			file.Close()
		}
		t.Fatalf("dot-prefixed audit path was visible to pre-v5 rules: %v", err)
	}
	file, err := legacy.Open("/action_write_audit.json")
	if err != nil {
		t.Fatalf("root audit fixture should demonstrate the old exposure: %v", err)
	}
	file.Close()
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

type spyFileSystem struct {
	opened []string
}

func (s *spyFileSystem) Open(name string) (http.File, error) {
	s.opened = append(s.opened, name)
	return nil, os.ErrNotExist
}

func mixedCase(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		if r >= 'a' && r <= 'z' {
			if upper {
				r -= 'a' - 'A'
			}
			upper = !upper
		}
		b.WriteRune(r)
	}
	return b.String()
}

func encodedUppercaseName(name string) string {
	upper := strings.ToUpper(name)
	for i := range len(upper) {
		if upper[i] >= 'A' && upper[i] <= 'Z' {
			return upper[:i] + "%" + fmt.Sprintf("%02X", upper[i]) + upper[i+1:]
		}
	}
	return upper
}

func TestNoListFSRejectsPrivatePathVariantsBeforeOpen(t *testing.T) {
	for _, name := range output.NonPublishedFiles {
		name := name
		t.Run(name, func(t *testing.T) {
			mixed := mixedCase(name)
			variants := []struct {
				name        string
				method      string
				path        string
				rangeHeader string
			}{
				{name: "lowercase", method: http.MethodGet, path: "/data/" + strings.ToLower(name)},
				{name: "uppercase", method: http.MethodGet, path: "/data/" + strings.ToUpper(name)},
				{name: "mixed_case", method: http.MethodGet, path: "/data/" + mixed},
				{name: "head", method: http.MethodHead, path: "/data/" + mixed},
				{name: "range", method: http.MethodGet, path: "/data/" + mixed, rangeHeader: "bytes=0-3"},
				{name: "encoded_case", method: http.MethodGet, path: "/data/" + encodedUppercaseName(name)},
				{name: "unicode_case_fold", method: http.MethodGet, path: "/data/" + strings.Replace(name, "s", "ſ", 1)},
				{name: "backslash", method: http.MethodGet, path: "/data/%5C" + mixed},
				{name: "nested_backslash", method: http.MethodGet, path: "/data/jobs%5C" + mixed},
				{name: "nested", method: http.MethodGet, path: "/data/jobs/" + mixed},
				{name: "dot_directory", method: http.MethodGet, path: "/data/.private/" + mixed},
				{name: "encoded_dot_directory", method: http.MethodGet, path: "/data/%2Eprivate/" + mixed},
			}
			for _, variant := range variants {
				variant := variant
				t.Run(variant.name, func(t *testing.T) {
					spy := &spyFileSystem{}
					handler := http.StripPrefix("/data/", http.FileServer(noListFS{fs: spy}))
					req := httptest.NewRequest(variant.method, variant.path, nil)
					if variant.rangeHeader != "" {
						req.Header.Set("Range", variant.rangeHeader)
					}
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, req)
					if recorder.Code != http.StatusNotFound {
						t.Fatalf("status = %d, want 404", recorder.Code)
					}
					if len(spy.opened) != 0 {
						t.Fatalf("underlying filesystem opened %v", spy.opened)
					}
				})
			}
		})
	}
}

func TestNoListFSRejectsMalformedPathsBeforeOpen(t *testing.T) {
	for _, target := range []string{
		"/data/%00dashboard.json",
		"/data/%09dashboard.json",
		"/data/%7Fdashboard.json",
		"/data/%FFdashboard.json",
	} {
		spy := &spyFileSystem{}
		handler := http.StripPrefix("/data/", http.FileServer(noListFS{fs: spy}))
		req := httptest.NewRequest(http.MethodGet, target, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400 or 404", target, recorder.Code)
		}
		if len(spy.opened) != 0 {
			t.Errorf("%s opened underlying filesystem paths %v", target, spy.opened)
		}
	}
}

// TestHandler_HidesOperationalFiles verifies the AI cache and write-automation
// state files are not served, so operational metadata never leaks via /data.
func TestHandler_HidesOperationalFiles(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "dashboard.json", `{"jobs":[]}`)
	writeFile(t, dataDir, "ai_cache.json", `{"secret":"cache"}`)
	writeFile(t, dataDir, "ai_traces.json", `{"traces":[]}`)
	writeFile(t, dataDir, "orka_analysis.json", `{"contract_hash":"private"}`)
	writeFile(t, dataDir, "issue_state.json", `{"tracked":{}}`)
	writeFile(t, dataDir, "fix_pr_state.json", `{"tracked":{}}`)
	writeFile(t, dataDir, "analysis_correction_state.json", `{"corrections":{}}`)
	writeFile(t, dataDir, "action_request_state.json", `{"requests":{}}`)
	writeFile(t, dataDir, "action_preview_state.json", `{"previews":{}}`)
	writeFile(t, dataDir, ".action-write-audit/state.json", `{"records":{}}`)
	writeFile(t, dataDir, "remediation_state.json", `{"version":1,"remediations":{}}`)
	writeFile(t, dataDir, "remediation_prow_catalog.json", `{"tests":{}}`)
	writeFile(t, dataDir, ".analysis-chat/sessions.json", `{"sessions":{}}`)
	writeFile(t, dataDir, ".remediation-investigations/cache.json", `{"version":1}`)
	writeFile(t, dataDir, "recurrence_ledger.json", `{"version":1,"entries":{}}`)

	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// The dashboard is served; the operational files 404.
	if resp, _ := http.Get(srv.URL + "/data/dashboard.json"); resp.StatusCode != http.StatusOK {
		t.Errorf("dashboard.json status = %d, want 200", resp.StatusCode)
	}
	for _, name := range []string{"ai_cache.json", "ai_traces.json", "issue_state.json", "fix_pr_state.json", "orka_analysis.json", "analysis_correction_state.json", "action_request_state.json", "action_preview_state.json", ".action-write-audit/state.json", "remediation_state.json", "remediation_prow_catalog.json", ".analysis-chat/sessions.json", ".remediation-investigations/cache.json", "recurrence_ledger.json"} {
		resp, err := http.Get(srv.URL + "/data/" + name)
		if err != nil {
			t.Fatalf("GET %s: %v", name, err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404 (operational file must not be served)", name, resp.StatusCode)
		}
	}
}

func TestEnsureAnalysisTraceEngineUsesLegacyFallback(t *testing.T) {
	traces := ai.AnalysisTraceFile{}
	ensureAnalysisTraceEngine(&traces)
	if traces.Engine == nil || traces.Engine.Version != "legacy" || traces.Engine.Commit != "unknown" || traces.Engine.ImageTag != "unknown" {
		t.Fatalf("legacy trace engine = %+v", traces.Engine)
	}
}

func TestHandler_AnalysisHealthAuthenticatedAndFiltered(t *testing.T) {
	dataDir := t.TempDir()
	producer := ai.TraceEngine{Version: "v1.2.3", Commit: "0123456789abcdef", ImageTag: "sha-0123456"}
	traces := ai.AnalysisTraceFile{Version: 1, GeneratedAt: "2026-07-22T00:00:00Z", Engine: &producer, Traces: []ai.AnalysisTrace{
		{JobID: "job-a", BuildID: "1", TestName: "Test A", Outcome: "success", Events: []ai.TraceEvent{{Sequence: 1, Kind: "model_request", ResponseID: "resp-a"}}},
		{JobID: "job-b", BuildID: "2", TestName: "Test B", Outcome: "error", Events: []ai.TraceEvent{{Sequence: 1, Kind: "model_request", ResponseID: "resp-b"}}},
	}}
	if err := statefile.WriteJSON(filepath.Join(dataDir, output.AITraceFilename), traces); err != nil {
		t.Fatal(err)
	}
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/analysis-health")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/analysis-health?job_id=job-b&response_id=resp-b", nil)
	req.Header.Set("Authorization", "ok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("filtered status=%d cache=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var got ai.AnalysisTraceFile
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(got.Traces) != 1 || got.Traces[0].JobID != "job-b" {
		t.Fatalf("filtered traces = %+v", got.Traces)
	}
	if got.Engine == nil || *got.Engine != producer {
		t.Fatalf("trace producer = %+v", got.Engine)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/analysis-health/download", nil)
	req.Header.Set("Authorization", "ok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="analysis-health.json"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	_ = resp.Body.Close()

	resp, err = http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var caps Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !caps.Features.AnalysisHealth || caps.Features.Actions || caps.Auth == nil {
		t.Fatalf("capabilities = %+v", caps)
	}
}

func TestHandler_AnalysisHealthMissing(t *testing.T) {
	dataDir := t.TempDir()
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/analysis-health", nil)
	req.Header.Set("Authorization", "ok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	_ = resp.Body.Close()
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
	if got.Engine.Version != "dev" || got.Engine.Commit != "dev" || got.Engine.ImageTag != "dev" {
		t.Fatalf("Engine = %+v, want dev fallback", got.Engine)
	}
	if got.Features.Actions || got.Features.AnalysisHealth {
		t.Errorf("Features = %+v, want all false at read parity", got.Features)
	}
}

func TestHandler_ReadOnlyRoutesRejectUnsupportedMethods(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{"version":1}`)
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{"/api/capabilities", "/data/manifest.json"} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s status = %d, want 405", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
			t.Errorf("POST %s Allow = %q, want GET, HEAD", path, got)
		}
	}
}

func TestHandler_ReadOnlyRoutesPreserveHeadAndRange(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{"version":1}`)
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{"/api/capabilities", "/data/manifest.json"} {
		req, err := http.NewRequest(http.MethodHead, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("HEAD %s: %v", path, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("HEAD %s status = %d, want 200", path, resp.StatusCode)
		}
		if body != "" {
			t.Errorf("HEAD %s body = %q, want empty", path, body)
		}
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/data/manifest.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range GET: %v", err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("range status = %d, want 206", resp.StatusCode)
	}
	if got := readBody(t, resp); got != `{"ve` {
		t.Errorf("range body = %q, want %q", got, `{"ve`)
	}
}

func TestHandler_StaticRoutesRejectUnsupportedMethods(t *testing.T) {
	dataDir := t.TempDir()
	staticDir := t.TempDir()
	writeFile(t, staticDir, "index.html", "<!doctype html><title>app</title>")
	writeFile(t, staticDir, "assets/index-wlRzmcMX.js", "console.log('asset')")

	h, err := Handler(Options{DataDir: dataDir, StaticDir: staticDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace,
	} {
		for _, path := range []string{"/", "/flaky", "/job/example", "/assets/index-wlRzmcMX.js"} {
			t.Run(method+" "+path, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
				if recorder.Code != http.StatusMethodNotAllowed {
					t.Errorf("status = %d, want 405", recorder.Code)
				}
				if got := recorder.Header().Get("Allow"); got != "GET, HEAD" {
					t.Errorf("Allow = %q, want GET, HEAD", got)
				}
			})
		}
	}
}

func TestHandler_StaticRoutesPreserveGetHeadAndRange(t *testing.T) {
	dataDir := t.TempDir()
	staticDir := t.TempDir()
	indexBody := "<!doctype html><title>app</title>"
	assetBody := "console.log('asset')"
	writeFile(t, staticDir, "index.html", indexBody)
	writeFile(t, staticDir, "assets/index-wlRzmcMX.js", assetBody)
	writeFile(t, staticDir, "spa-index-redirect.js", "redirect()")

	h, err := Handler(Options{DataDir: dataDir, StaticDir: staticDir, Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	for _, testCase := range []struct {
		path         string
		body         string
		cacheControl string
	}{
		{path: "/", body: indexBody, cacheControl: "no-cache"},
		{path: "/flaky", body: indexBody, cacheControl: "no-cache"},
		{path: "/job/example", body: indexBody, cacheControl: "no-cache"},
		{path: "/not-found", body: indexBody, cacheControl: "no-cache"},
		{path: "/spa-index-redirect.js", body: "redirect()", cacheControl: "no-cache"},
		{path: "/assets/index-wlRzmcMX.js", body: assetBody, cacheControl: "public, max-age=31536000, immutable"},
	} {
		t.Run("GET "+testCase.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, testCase.path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			if got := recorder.Body.String(); got != testCase.body {
				t.Errorf("body = %q, want %q", got, testCase.body)
			}
			if got := recorder.Header().Get("Cache-Control"); got != testCase.cacheControl {
				t.Errorf("Cache-Control = %q, want %q", got, testCase.cacheControl)
			}
		})

		t.Run("HEAD "+testCase.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, testCase.path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			if recorder.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", recorder.Body.String())
			}
			if got := recorder.Header().Get("Cache-Control"); got != testCase.cacheControl {
				t.Errorf("Cache-Control = %q, want %q", got, testCase.cacheControl)
			}
		})
	}

	for _, testCase := range []struct {
		path         string
		body         string
		cacheControl string
	}{
		{path: "/job/example", body: "<!do", cacheControl: "no-cache"},
		{path: "/assets/index-wlRzmcMX.js", body: "cons", cacheControl: "public, max-age=31536000, immutable"},
	} {
		t.Run("range "+testCase.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, testCase.path, nil)
			req.Header.Set("Range", "bytes=0-3")
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusPartialContent {
				t.Fatalf("status = %d, want 206", recorder.Code)
			}
			if got := recorder.Body.String(); got != testCase.body {
				t.Errorf("body = %q, want %q", got, testCase.body)
			}
			if recorder.Header().Get("Content-Range") == "" {
				t.Error("Content-Range is empty")
			}
			if got := recorder.Header().Get("Cache-Control"); got != testCase.cacheControl {
				t.Errorf("Cache-Control = %q, want %q", got, testCase.cacheControl)
			}
		})
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

	// Unknown API routes return a real 404 instead of the SPA document.
	for _, testCase := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api"},
		{method: http.MethodGet, path: "/api/not-a-real-endpoint"},
		{method: http.MethodGet, path: "/api/auth/not-a-real-endpoint"},
		{method: http.MethodPost, path: "/api/not-a-real-endpoint"},
	} {
		req, err := http.NewRequest(testCase.method, srv.URL+testCase.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", testCase.method, testCase.path, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", testCase.method, testCase.path, resp.StatusCode)
		}
		if strings.Contains(body, "<title>app</title>") {
			t.Errorf("%s %s returned index.html", testCase.method, testCase.path)
		}
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
	writeFile(t, dataDir, "dashboard.json", `{}`)
	staticDir := t.TempDir()
	writeFile(t, staticDir, "index.html", "<!doctype html><title>app</title>")
	writeFile(t, staticDir, "assets/app.js", "console.log(1)")
	h, err := Handler(Options{DataDir: dataDir, StaticDir: staticDir, Capabilities: DefaultCapabilities(), HSTSEnabled: true})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	want := map[string]string{
		"Content-Security-Policy":   contentSecurityPolicy,
		"Permissions-Policy":        permissionsPolicy,
		"X-Frame-Options":           "DENY",
		"X-Content-Type-Options":    "nosniff",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"Strict-Transport-Security": "max-age=31536000",
	}
	for _, path := range []string{
		"/healthz",
		"/api/capabilities",
		"/data/dashboard.json",
		"/job/example",
		"/assets/app.js",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		for k, v := range want {
			if got := resp.Header.Get(k); got != v {
				t.Errorf("GET %s: %s = %q, want %q", path, k, got, v)
			}
		}
	}
}

func TestHandler_HSTSDisabledForLocalHTTP(t *testing.T) {
	h, err := Handler(Options{DataDir: t.TempDir(), Capabilities: DefaultCapabilities()})
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q, want empty", got)
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
	gotID, gotOwner, gotToken, gotInstruction, gotConfirmToken   string
	gotResolveID, gotResolveLogin, gotResolveNote                string
	gotUnresolveID                                               string
	gotResolveCauseID, gotResolveCauseLogin, gotResolveCauseNote string
	gotUnresolveCauseID                                          string
}

func (f *fakeRunner) PreviewIssue(ctx context.Context, id, owner, token, instruction string) (actions.PreviewResult, error) {
	if id == "missing" {
		return actions.PreviewResult{}, actions.ErrNotFound
	}
	f.gotID, f.gotOwner, f.gotToken, f.gotInstruction = id, owner, token, instruction
	return actions.PreviewResult{Token: "ptok", Kind: "issue", Title: "T", Body: "B"}, nil
}
func (f *fakeRunner) PreviewFix(ctx context.Context, id, owner, token, instruction string) (actions.PreviewResult, error) {
	f.gotID, f.gotOwner, f.gotToken, f.gotInstruction = id, owner, token, instruction
	return actions.PreviewResult{Token: "ptok", Kind: "fix", Title: "T", Body: "B", Diff: "d"}, nil
}
func (f *fakeRunner) Confirm(ctx context.Context, token, owner, userToken string) (string, error) {
	if token == "missing" {
		return "", actions.ErrPreviewNotFound
	}
	f.gotConfirmToken, f.gotOwner, f.gotToken = token, owner, userToken
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
func (f *fakeRunner) ResolveCause(signature, login, note string) error {
	if signature == "missing" {
		return actions.ErrNotFound
	}
	f.gotResolveCauseID, f.gotResolveCauseLogin, f.gotResolveCauseNote = signature, login, note
	return nil
}
func (f *fakeRunner) UnresolveCause(signature string) error {
	if signature == "missing" {
		return actions.ErrNotFound
	}
	f.gotUnresolveCauseID = signature
	return nil
}
func (f *fakeRunner) ActionEligibility(_ context.Context, id string) (actions.Eligibility, error) {
	if id == "missing" {
		return actions.Eligibility{}, actions.ErrNotFound
	}
	f.gotID = id
	return actions.Eligibility{State: actions.EligibilityActionable, Code: actions.ReasonActionable, Reason: "verified"}, nil
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
	"/api/causes/cni409/resolve",
	"/api/causes/cni409/unresolve",
}

func TestWriteHandlersUseStrictBoundedJSON(t *testing.T) {
	type endpoint struct {
		name       string
		allowEmpty bool
		validBody  string
		unknown    string
		wantStatus int
		build      func(*int) http.Handler
	}
	endpoints := []endpoint{
		{
			name: "preview", allowEmpty: true, validBody: `{"instruction":"tighten it"}`, unknown: `{"instruction":"x","extra":true}`, wantStatus: http.StatusOK,
			build: func(calls *int) http.Handler {
				return previewHandler(time.Second, func(context.Context, string, string, string, string) (actions.PreviewResult, error) {
					*calls++
					return actions.PreviewResult{}, nil
				})
			},
		},
		{
			name: "confirm", validBody: `{"token":"preview-token"}`, unknown: `{"token":"preview-token","extra":true}`, wantStatus: http.StatusOK,
			build: func(calls *int) http.Handler {
				return confirmHandler(time.Second, func(context.Context, string, string, string) (string, error) {
					*calls++
					return "https://example.test/result", nil
				})
			},
		},
		{
			name: "action request", allowEmpty: true, validBody: `{"instruction":"tighten it"}`, unknown: `{"instruction":"x","extra":true}`, wantStatus: http.StatusAccepted,
			build: func(calls *int) http.Handler {
				return createActionRequestHandler(func(string, string, string, string, string, string) (actions.ActionRequestView, error) {
					*calls++
					return actions.ActionRequestView{}, nil
				}, true)
			},
		},
		{
			name: "resolve", allowEmpty: true, validBody: `{"note":"fixed"}`, unknown: `{"note":"fixed","extra":true}`, wantStatus: http.StatusNoContent,
			build: func(calls *int) http.Handler {
				return resolveHandler(func(string, string, string) error {
					*calls++
					return nil
				})
			},
		},
	}
	for _, endpoint := range endpoints {
		for _, testCase := range []struct {
			name string
			body string
			bad  bool
		}{
			{name: "valid", body: endpoint.validBody},
			{name: "empty", bad: !endpoint.allowEmpty},
			{name: "malformed", body: `{"broken":`, bad: true},
			{name: "unknown field", body: endpoint.unknown, bad: true},
			{name: "trailing value", body: endpoint.validBody + `{}`, bad: true},
		} {
			t.Run(endpoint.name+"/"+testCase.name, func(t *testing.T) {
				calls := 0
				handler := auth.Middleware(fakeAuth{}, endpoint.build(&calls))
				request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(testCase.body))
				request.Header.Set("Authorization", "ok")
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if testCase.bad {
					if recorder.Code != http.StatusBadRequest || calls != 0 {
						t.Fatalf("status=%d calls=%d body=%q", recorder.Code, calls, recorder.Body.String())
					}
					return
				}
				if recorder.Code != endpoint.wantStatus || calls != 1 {
					t.Fatalf("status=%d calls=%d body=%q", recorder.Code, calls, recorder.Body.String())
				}
			})
		}
	}
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
	if caps.Features.AnalysisCritiqueVersion != ai.CurrentCritiqueVersion() {
		t.Fatalf("critique version = %d, want %d", caps.Features.AnalysisCritiqueVersion, ai.CurrentCritiqueVersion())
	}
	if !caps.Features.ActionEligibility || len(caps.Features.ActionEligibilityReasonCodes) != len(actions.ReasonCodes()) {
		t.Fatalf("action eligibility contract = %+v", caps.Features)
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

	unauthEligibility, _ := http.Get(srv.URL + "/api/failures/abc/eligibility")
	if unauthEligibility.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated eligibility = %d, want 401", unauthEligibility.StatusCode)
	}
	if !strings.Contains(unauthEligibility.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("unauthenticated eligibility cache=%q", unauthEligibility.Header.Get("Cache-Control"))
	}
	_ = unauthEligibility.Body.Close()

	missingEligibilityReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/failures/missing/eligibility", nil)
	missingEligibilityReq.Header.Set("Authorization", "ok")
	missingEligibility, err := http.DefaultClient.Do(missingEligibilityReq)
	if err != nil {
		t.Fatal(err)
	}
	if missingEligibility.StatusCode != http.StatusNotFound || !strings.Contains(missingEligibility.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("missing eligibility = %d cache=%q", missingEligibility.StatusCode, missingEligibility.Header.Get("Cache-Control"))
	}
	_ = missingEligibility.Body.Close()

	eligibilityReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/failures/abc/eligibility", nil)
	eligibilityReq.Header.Set("Authorization", "ok")
	eligibilityResp, err := http.DefaultClient.Do(eligibilityReq)
	if err != nil {
		t.Fatal(err)
	}
	var eligibility actions.Eligibility
	if err := json.NewDecoder(eligibilityResp.Body).Decode(&eligibility); err != nil {
		t.Fatal(err)
	}
	_ = eligibilityResp.Body.Close()
	if eligibilityResp.StatusCode != http.StatusOK || eligibility.State != actions.EligibilityActionable || eligibility.Code != actions.ReasonActionable || !strings.Contains(eligibilityResp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("eligibility response = %d %+v cache=%q", eligibilityResp.StatusCode, eligibility, eligibilityResp.Header.Get("Cache-Control"))
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
	if runner.gotID != "abc" || runner.gotOwner != "alice" || runner.gotToken != "tok" || runner.gotInstruction != "tighten it" {
		t.Errorf("runner got id=%q owner=%q token=%q instruction=%q, want abc/alice/tok/tighten it", runner.gotID, runner.gotOwner, runner.gotToken, runner.gotInstruction)
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
	if runner.gotConfirmToken != "ptok" || runner.gotOwner != "alice" || runner.gotToken != "tok" {
		t.Errorf("confirm got preview=%q owner=%q write token=%q", runner.gotConfirmToken, runner.gotOwner, runner.gotToken)
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
	// Cause scope is addressed by causal-group signature and routed to the
	// cause-scoped runner methods, never the pattern-scoped ones.
	if r := do("/api/causes/cni409/resolve", "ok", `{"note":"fixed by test-infra #99"}`); r.StatusCode != http.StatusNoContent {
		t.Errorf("cause resolve status = %d, want 204", r.StatusCode)
	}
	if runner.gotResolveCauseID != "cni409" || runner.gotResolveCauseLogin != "alice" || runner.gotResolveCauseNote != "fixed by test-infra #99" {
		t.Errorf("cause resolve got id=%q login=%q note=%q", runner.gotResolveCauseID, runner.gotResolveCauseLogin, runner.gotResolveCauseNote)
	}
	if runner.gotResolveID != "abc" {
		t.Errorf("cause resolve leaked into pattern scope: id=%q", runner.gotResolveID)
	}
	if r := do("/api/causes/missing/resolve", "ok", `{}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("cause resolve not-found status = %d, want 404", r.StatusCode)
	}
	if r := do("/api/causes/cni409/unresolve", "ok", ``); r.StatusCode != http.StatusNoContent {
		t.Errorf("cause unresolve status = %d, want 204", r.StatusCode)
	}
	if runner.gotUnresolveCauseID != "cni409" {
		t.Errorf("cause unresolve got id=%q, want cni409", runner.gotUnresolveCauseID)
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

type fakeAsyncRunner struct {
	fakeRunner
	request      actions.ActionRequestView
	supersedesID string
	cancelStatus string
}

func (f *fakeAsyncRunner) CreateRequest(failureID, kind, login, userToken, instruction, supersedesID string) (actions.ActionRequestView, error) {
	if failureID == "missing" {
		return actions.ActionRequestView{}, actions.ErrNotFound
	}
	f.request = actions.ActionRequestView{ID: "request-1", FailureID: failureID, Kind: kind, Owner: login, Status: actions.RequestPending}
	f.gotID, f.gotToken, f.gotInstruction = failureID, userToken, instruction
	f.supersedesID = supersedesID
	return f.request, nil
}
func (f *fakeAsyncRunner) GetRequest(id, login string) (actions.ActionRequestView, error) {
	if id != f.request.ID || login != f.request.Owner {
		return actions.ActionRequestView{}, actions.ErrRequestNotFound
	}
	return f.request, nil
}
func (f *fakeAsyncRunner) ConfirmRequest(_ context.Context, id, login, token string) (string, error) {
	if id != f.request.ID || login != f.request.Owner {
		return "", actions.ErrRequestNotFound
	}
	f.gotConfirmToken, f.gotToken = id, token
	return "https://github.com/o/r/issues/1", nil
}
func (f *fakeAsyncRunner) CancelRequest(_ context.Context, id, login string) (actions.ActionRequestView, error) {
	if id != f.request.ID || login != f.request.Owner {
		return actions.ActionRequestView{}, actions.ErrRequestNotFound
	}
	f.request.Status = f.cancelStatus
	if f.request.Status == "" {
		f.request.Status = actions.RequestCancelled
	}
	return f.request, nil
}

func TestHandler_AsyncActionRequestFlow(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)
	runner := &fakeAsyncRunner{}
	h, err := Handler(Options{DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, Actions: runner, AuthMode: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	request := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "ok")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	created := request(http.MethodPost, "/api/failures/pattern-1/create-issue/requests", `{"instruction":"mention IPv6","supersedes_request_id":"request-old"}`)
	if created.StatusCode != http.StatusAccepted {
		t.Fatalf("create status=%d body=%s", created.StatusCode, readBody(t, created))
	}
	_ = created.Body.Close()
	if runner.gotID != "pattern-1" || runner.gotInstruction != "mention IPv6" || runner.supersedesID != "request-old" {
		t.Fatalf("create got id=%q instruction=%q supersedes=%q", runner.gotID, runner.gotInstruction, runner.supersedesID)
	}

	malformed := request(http.MethodPost, "/api/failures/pattern-2/create-issue/requests", `{"instruction":`)
	if malformed.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed create status=%d body=%s", malformed.StatusCode, readBody(t, malformed))
	}
	_ = malformed.Body.Close()
	if runner.gotID != "pattern-1" {
		t.Fatalf("malformed create reached runner with id=%q", runner.gotID)
	}

	oversizedBody := `{"instruction":"` + strings.Repeat("x", 8192) + `"}`
	oversized := request(http.MethodPost, "/api/failures/pattern-2/create-issue/requests", oversizedBody)
	if oversized.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized create status=%d body=%s", oversized.StatusCode, readBody(t, oversized))
	}
	_ = oversized.Body.Close()
	if runner.gotID != "pattern-1" {
		t.Fatalf("oversized create reached runner with id=%q", runner.gotID)
	}

	got := request(http.MethodGet, "/api/action-requests/request-1", "")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d body=%s", got.StatusCode, readBody(t, got))
	}
	_ = got.Body.Close()

	confirmed := request(http.MethodPost, "/api/action-requests/request-1/confirm", "")
	if confirmed.StatusCode != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirmed.StatusCode, readBody(t, confirmed))
	}
	_ = confirmed.Body.Close()

	runner.cancelStatus = actions.RequestCancelling
	cancelled := request(http.MethodPost, "/api/action-requests/request-1/cancel", "")
	if cancelled.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status=%d body=%s", cancelled.StatusCode, readBody(t, cancelled))
	}
	var cancellingView actions.ActionRequestView
	if err := json.NewDecoder(cancelled.Body).Decode(&cancellingView); err != nil {
		t.Fatal(err)
	}
	_ = cancelled.Body.Close()
	if cancellingView.Status != actions.RequestCancelling {
		t.Fatalf("cancel view = %+v", cancellingView)
	}

	capsResp, _ := http.Get(srv.URL + "/api/capabilities")
	var caps Capabilities
	_ = json.NewDecoder(capsResp.Body).Decode(&caps)
	_ = capsResp.Body.Close()
	if !caps.Features.ActionRequests {
		t.Fatalf("capabilities = %+v", caps)
	}
}

func TestWriteActionErrorMapsPendingConfirmation(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeActionError(recorder, "confirm", "alice", actions.ErrPreviewPending)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "already being processed") {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	writeActionError(recorder, "confirm", "alice", actions.ErrPreviewSuperseded)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "replaced") {
		t.Fatalf("superseded response = %d %q", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	writeActionError(recorder, "confirm", "alice", actions.ErrPreviewOutcomeUnknown)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "outcome is unknown") {
		t.Fatalf("unknown outcome response = %d %q", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	writeActionError(recorder, "confirm", "alice", actions.ErrPreviewTargetChanged)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("target drift response = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestWriteActionErrorHidesInternalDetails(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeActionError(recorder, "request", "alice", errors.New("provider secret https://private.example/v1\nsecond line"))
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "private.example") || strings.Contains(body, "provider secret") {
		t.Fatalf("internal error leaked: %q", body)
	}
}

func TestSafeOperatorErrorRedactsURLsAndNewlines(t *testing.T) {
	got := safeOperatorError(errors.New("failed https://private.example/token\nnext"))
	if strings.Contains(got, "private.example") || strings.Contains(got, "\n") {
		t.Fatalf("safe error = %q", got)
	}
}

func TestWriteActionErrorLogsSanitizedInconclusiveCause(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)

	recorder := httptest.NewRecorder()
	err := fmt.Errorf("%w: archive request failed at https://private.example/token", actions.ErrRemediationInconclusive)
	writeActionError(recorder, "request", "alice", err)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "source verification was inconclusive") {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(logs.String(), "source verification inconclusive") || strings.Contains(logs.String(), "private.example") {
		t.Fatalf("operator log = %q", logs.String())
	}
}

func TestWriteActionErrorUsesStructuredReasonContract(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		err    error
		status int
		code   actions.ReasonCode
	}{
		{name: "already present", err: actions.ErrRemediationAlreadyPresent, status: http.StatusConflict, code: actions.ReasonAlreadyPresent},
		{name: "source inconclusive", err: actions.ErrRemediationInconclusive, status: http.StatusConflict, code: actions.ReasonSourceVerificationInconclusive},
		{name: "unsafe draft", err: actions.ErrPreviewRejected, status: http.StatusUnprocessableEntity, code: actions.ReasonContractGenerationFailed},
		{name: "restored preview unavailable", err: actions.ErrPreviewNotFound, status: http.StatusNotFound, code: actions.ReasonEvidenceUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeActionError(recorder, "id", "alice", testCase.err)
			if recorder.Code != testCase.status || recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("status=%d headers=%v", recorder.Code, recorder.Header())
			}
			var response actionReasonResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Code != testCase.code || response.Message != actions.ReasonMessage(testCase.code) {
				t.Fatalf("response=%+v", response)
			}
		})
	}
}

func TestHandlerDisablesFixRoutesAndCapabilityIndependently(t *testing.T) {
	dataDir := t.TempDir()
	writeFile(t, dataDir, "manifest.json", `{}`)
	runner := &fakeAsyncRunner{}
	h, err := Handler(Options{
		DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, Actions: runner,
		AuthMode: "dev", DisableFixActions: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	capResponse, err := http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var caps Capabilities
	if err := json.NewDecoder(capResponse.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	_ = capResponse.Body.Close()
	if !caps.Features.Actions || caps.Features.FixPRs {
		t.Fatalf("capabilities = %+v", caps.Features)
	}

	post := func(path string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "ok")
		req.Header.Set("Origin", srv.URL)
		response, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		return response
	}
	for _, path := range []string{
		"/api/failures/pattern/propose-fix/preview",
		"/api/failures/pattern/propose-fix/requests",
	} {
		response := post(path)
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s", path, response.StatusCode, readBody(t, response))
		}
		_ = response.Body.Close()
	}
	issue := post("/api/failures/pattern/create-issue/requests")
	if issue.StatusCode != http.StatusAccepted {
		t.Fatalf("issue status=%d body=%s", issue.StatusCode, readBody(t, issue))
	}
	_ = issue.Body.Close()
}
