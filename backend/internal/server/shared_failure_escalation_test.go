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

type fakeSharedFailureRunner struct {
	mu       sync.Mutex
	gotRef   prescalation.ClusterRef
	gotOwner string
	gotReq   string
	view     prescalation.ClusterView
	err      error
}

func (f *fakeSharedFailureRunner) Start(_ context.Context, ref prescalation.ClusterRef, owner, requestID string) (prescalation.ClusterView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotRef, f.gotOwner, f.gotReq = ref, owner, requestID
	if f.err != nil {
		return prescalation.ClusterView{}, f.err
	}
	view := f.view
	if view.State == "" {
		view.State = prescalation.StateQueued
	}
	view.Ref = ref
	return view, nil
}

func (f *fakeSharedFailureRunner) Get(ref prescalation.ClusterRef) (prescalation.ClusterView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotRef = ref
	if f.err != nil {
		return prescalation.ClusterView{}, f.err
	}
	view := f.view
	if view.State == "" {
		view.State = prescalation.StateNotStarted
	}
	view.Ref = ref
	return view, nil
}

func (f *fakeSharedFailureRunner) ref() prescalation.ClusterRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotRef
}

const sharedFailurePath = "/api/shared-failures/a1b2c3d4e5f60718/escalation"

func sharedFailureServer(t *testing.T, runner SharedFailureEscalationRunner) *httptest.Server {
	t.Helper()
	opts := Options{DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), AuthMode: "dev"}
	if runner != nil {
		opts.Auth = fakeAuth{}
		opts.SharedFailureEscalation = runner
	}
	h, err := Handler(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func startSharedFailure(t *testing.T, srv *httptest.Server, body, idempotency, authHeader string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+sharedFailurePath, strings.NewReader(body))
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

func capabilitiesOf(t *testing.T, srv *httptest.Server) Capabilities {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var caps Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	return caps
}

func TestSharedFailureRoutesAreWithheldWithoutARunner(t *testing.T) {
	srv := sharedFailureServer(t, nil)

	resp := startSharedFailure(t, srv, "", "req-1", "ok")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when shared failure escalation is not configured", resp.StatusCode)
	}
	if capabilitiesOf(t, srv).Features.SharedFailureEscalation {
		t.Error("the capability must not be advertised without a runner")
	}
}

// The two escalation kinds are separate services, so one constructing must not
// advertise controls for the other.
func TestSharedFailureCapabilityIsIndependentOfPullRequestEscalation(t *testing.T) {
	opts := Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), AuthMode: "dev",
		Auth: fakeAuth{}, PullRequestEscalation: &fakeEscalationRunner{},
	}
	h, err := Handler(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	caps := capabilitiesOf(t, srv)
	if !caps.Features.PullRequestEscalation {
		t.Error("pull request escalation should be advertised")
	}
	if caps.Features.SharedFailureEscalation {
		t.Error("shared failure escalation must not ride on the pull request capability")
	}
}

func TestSharedFailureStartPassesTheIdentifiedSubject(t *testing.T) {
	runner := &fakeSharedFailureRunner{}
	srv := sharedFailureServer(t, runner)

	resp := startSharedFailure(t, srv, "", "req-1", "ok")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if got := runner.ref(); got.ID != "a1b2c3d4e5f60718" {
		t.Errorf("ref = %+v, want the path id", got)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.gotReq != "req-1" {
		t.Errorf("idempotency key = %q", runner.gotReq)
	}
	if runner.gotOwner == "" {
		t.Error("the authenticated owner should reach the runner")
	}
}

func TestSharedFailureStartRequiresAnIdempotencyKey(t *testing.T) {
	srv := sharedFailureServer(t, &fakeSharedFailureRunner{})

	resp := startSharedFailure(t, srv, "", "", "ok")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 without an idempotency key", resp.StatusCode)
	}
}

func TestSharedFailureStartRequiresAuthentication(t *testing.T) {
	srv := sharedFailureServer(t, &fakeSharedFailureRunner{})

	resp := startSharedFailure(t, srv, "", "req-1", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// The subject comes entirely from the path, so a body carrying content is a
// client sending a request this endpoint would silently misread.
func TestSharedFailureStartRejectsAnUnexpectedBody(t *testing.T) {
	srv := sharedFailureServer(t, &fakeSharedFailureRunner{})

	for _, body := range []string{`{"test_name":"TestA"}`, `{"id":"other"}`, "not json"} {
		resp := startSharedFailure(t, srv, body, "req-1", "ok")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
	// An empty object stays acceptable for clients that always send one.
	resp := startSharedFailure(t, srv, `{}`, "req-1", "ok")
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("empty object: status = %d, want 202", resp.StatusCode)
	}
}

func TestSharedFailureCompletedStartReportsOK(t *testing.T) {
	runner := &fakeSharedFailureRunner{
		view: prescalation.ClusterView{State: prescalation.StateComplete, RootCause: "quota exhausted"},
	}
	srv := sharedFailureServer(t, runner)

	resp := startSharedFailure(t, srv, "", "req-1", "ok")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a terminal state", resp.StatusCode)
	}
	var view prescalation.ClusterView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.RootCause != "quota exhausted" || view.Ref.ID != "a1b2c3d4e5f60718" {
		t.Fatalf("view = %+v", view)
	}
}

// A shared failure is refused because a pull request can already analyze it,
// which is a different reason than the pull request endpoint's, so it must not
// be reported with that endpoint's wording.
func TestSharedFailureIneligibleReportsItsOwnReason(t *testing.T) {
	srv := sharedFailureServer(t, &fakeSharedFailureRunner{err: prescalation.ErrNotEligible})

	resp := startSharedFailure(t, srv, "", "req-1", "ok")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	body := make([]byte, 256)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "can be analyzed from an affected pull request") {
		t.Errorf("body = %q", body[:n])
	}
}

func TestSharedFailureGetReturnsCurrentState(t *testing.T) {
	runner := &fakeSharedFailureRunner{view: prescalation.ClusterView{State: prescalation.StateRunning}}
	srv := sharedFailureServer(t, runner)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+sharedFailurePath, nil)
	req.Header.Set("Authorization", "ok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var view prescalation.ClusterView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.State != prescalation.StateRunning {
		t.Fatalf("state = %q", view.State)
	}
}

func TestSharedFailureBusyReportsConflict(t *testing.T) {
	srv := sharedFailureServer(t, &fakeSharedFailureRunner{err: prescalation.ErrBusy})

	resp := startSharedFailure(t, srv, "", "req-1", "ok")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}
