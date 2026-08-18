package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/willie-yao/aster/backend/internal/prescalation"
)

type fakeEscalationRunner struct {
	mu       sync.Mutex
	gotRef   prescalation.Ref
	gotOwner string
	gotReq   string
	view     prescalation.PullRequestView
	err      error
}

func (f *fakeEscalationRunner) Start(_ context.Context, ref prescalation.Ref, owner, requestID string) (prescalation.PullRequestView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotRef, f.gotOwner, f.gotReq = ref, owner, requestID
	if f.err != nil {
		return prescalation.PullRequestView{}, f.err
	}
	view := f.view
	if view.State == "" {
		view.State = prescalation.StateQueued
	}
	view.Ref = ref
	return view, nil
}

func (f *fakeEscalationRunner) Get(ref prescalation.Ref) (prescalation.PullRequestView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotRef = ref
	if f.err != nil {
		return prescalation.PullRequestView{}, f.err
	}
	view := f.view
	if view.State == "" {
		view.State = prescalation.StateNotStarted
	}
	view.Ref = ref
	return view, nil
}

func (f *fakeEscalationRunner) ref() prescalation.Ref {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotRef
}

const escalationPath = "/api/pull-requests/6209/checks/org%2Frepo%2Fpull-e2e/builds/100/escalation"

func escalationServer(t *testing.T, runner PullRequestEscalationRunner) *httptest.Server {
	t.Helper()
	opts := Options{DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), AuthMode: "dev"}
	if runner != nil {
		opts.Auth = fakeAuth{}
		opts.PullRequestEscalation = runner
	}
	h, err := Handler(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func startEscalation(t *testing.T, srv *httptest.Server, body, idempotency, authHeader string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+escalationPath, strings.NewReader(body))
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestEscalationRoutesAreWithheldWithoutARunner(t *testing.T) {
	srv := escalationServer(t, nil)

	resp := startEscalation(t, srv, `{"test_name":"TestA"}`, "req-1", "ok")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when escalation is not configured", resp.StatusCode)
	}

	capsResp, err := http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer capsResp.Body.Close()
	var caps Capabilities
	if err := json.NewDecoder(capsResp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if caps.Features.PullRequestEscalation {
		t.Error("the capability must not be advertised without a runner")
	}
}

func TestEscalationCapabilityIsAdvertisedWhenConfigured(t *testing.T) {
	srv := escalationServer(t, &fakeEscalationRunner{})

	resp, err := http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var caps Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if !caps.Features.PullRequestEscalation {
		t.Fatal("the capability should be advertised")
	}
}

func TestEscalationRequiresAuthentication(t *testing.T) {
	srv := escalationServer(t, &fakeEscalationRunner{})

	resp := startEscalation(t, srv, `{"test_name":"TestA"}`, "req-1", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestEscalationStartPassesTheFullSubjectAndIdentity(t *testing.T) {
	runner := &fakeEscalationRunner{}
	srv := escalationServer(t, runner)

	resp := startEscalation(t, srv, `{"test_name":"[It] creates a cluster"}`, "req-1", "ok")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	ref := runner.ref()
	if ref.PullNumber != 6209 || ref.JobID != "org/repo/pull-e2e" || ref.BuildID != "100" {
		t.Fatalf("ref = %+v", ref)
	}
	if ref.TestName != "[It] creates a cluster" {
		t.Errorf("test name = %q", ref.TestName)
	}
	if runner.gotOwner != "alice" || runner.gotReq != "req-1" {
		t.Errorf("identity = %q/%q", runner.gotOwner, runner.gotReq)
	}
}

func TestEscalationStartRequiresAnIdempotencyKey(t *testing.T) {
	srv := escalationServer(t, &fakeEscalationRunner{})

	resp := startEscalation(t, srv, `{"test_name":"TestA"}`, "", "ok")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// Dropping an unknown field would change which failure is analyzed, so a
// malformed body is rejected rather than partially applied.
func TestEscalationStartRejectsMalformedBodies(t *testing.T) {
	srv := escalationServer(t, &fakeEscalationRunner{})

	cases := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"test_name":"TestA","extra":true}`},
		{name: "missing test name", body: `{}`},
		{name: "empty test name", body: `{"test_name":"  "}`},
		{name: "not json", body: `nonsense`},
		{name: "trailing document", body: `{"test_name":"TestA"}{"test_name":"TestB"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := startEscalation(t, srv, tc.body, "req-1", "ok")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestEscalationStartReportsTerminalStateAsOK(t *testing.T) {
	runner := &fakeEscalationRunner{view: prescalation.PullRequestView{State: prescalation.StateComplete}}
	srv := escalationServer(t, runner)

	resp := startEscalation(t, srv, `{"test_name":"TestA"}`, "req-1", "ok")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an already-complete escalation", resp.StatusCode)
	}
}

func TestEscalationErrorsMapToStableStatuses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "invalid", err: prescalation.ErrInvalid, want: http.StatusBadRequest},
		{name: "not eligible", err: prescalation.ErrNotEligible, want: http.StatusConflict},
		{name: "idempotency conflict", err: prescalation.ErrIdempotencyConflict, want: http.StatusConflict},
		{name: "busy", err: prescalation.ErrBusy, want: http.StatusConflict},
		{name: "unavailable", err: prescalation.ErrUnavailable, want: http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := escalationServer(t, &fakeEscalationRunner{err: tc.err})
			resp := startEscalation(t, srv, `{"test_name":"TestA"}`, "req-1", "ok")
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestEscalationGetReturnsCurrentState(t *testing.T) {
	runner := &fakeEscalationRunner{view: prescalation.PullRequestView{
		State: prescalation.StateComplete, RootCause: "the node ran out of memory",
	}}
	srv := escalationServer(t, runner)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+escalationPath+"?test=TestA", nil)
	req.Header.Set("Authorization", "ok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var view prescalation.PullRequestView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.RootCause != "the node ran out of memory" || view.Ref.TestName != "TestA" {
		t.Fatalf("view = %+v", view)
	}
	// Escalation results are operator-private and must never be cached.
	if cache := resp.Header.Get("Cache-Control"); !strings.Contains(cache, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cache)
	}
}

func TestEscalationGetRequiresATestName(t *testing.T) {
	srv := escalationServer(t, &fakeEscalationRunner{})

	req, _ := http.NewRequest(http.MethodGet, srv.URL+escalationPath, nil)
	req.Header.Set("Authorization", "ok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// The start route is a state-changing POST, so it must reject a cross-origin
// browser request the same way every other write route does.
func TestEscalationStartRejectsForeignOrigins(t *testing.T) {
	srv := escalationServer(t, &fakeEscalationRunner{})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+escalationPath, strings.NewReader(`{"test_name":"TestA"}`))
	req.Header.Set("Idempotency-Key", "req-1")
	req.Header.Set("Authorization", "ok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a foreign origin", resp.StatusCode)
	}
}
