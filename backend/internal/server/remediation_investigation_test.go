package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/causalfixpreview"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationinvestigation"
)

type fakeCausalRemediationRunner struct {
	startView models.PatternRemediationInvestigationSummary
	getView   models.PatternRemediationInvestigationSummary
	err       error
	ref       remediationinvestigation.OperationRef
	owner     string
	requestID string
	refresh   bool
}

func (f *fakeCausalRemediationRunner) Start(_ context.Context, ref remediationinvestigation.OperationRef, owner, requestID string, refresh bool) (models.PatternRemediationInvestigationSummary, error) {
	f.ref, f.owner, f.requestID, f.refresh = ref, owner, requestID, refresh
	return f.startView, f.err
}
func (f *fakeCausalRemediationRunner) Get(_ context.Context, ref remediationinvestigation.OperationRef) (models.PatternRemediationInvestigationSummary, error) {
	f.ref = ref
	return f.getView, f.err
}

func TestCausalRemediationInvestigationRoutesAreAuthenticatedAndCapabilityGated(t *testing.T) {
	dataDir := t.TempDir()
	runner := &fakeCausalRemediationRunner{
		startView: models.PatternRemediationInvestigationSummary{CausalGroupID: "group", CausalGroupHash: strings.Repeat("b", 64), State: models.PatternRemediationQueued},
		getView: models.PatternRemediationInvestigationSummary{
			CausalGroupID: "group", CausalGroupHash: strings.Repeat("b", 64), State: models.PatternRemediationActionable,
			Reason: "A source-grounded implementation target passed deterministic verification.", CompletedAt: "2026-08-12T00:00:00Z",
			Target: &models.PatternRemediationTargetSummary{Kind: "add_required_call", Repository: "example/repo", Revision: strings.Repeat("a", 40), Path: "controllers/reconcile.go", Symbol: "reconcile", RequiredCall: "applyFix"},
		},
	}
	handler, err := Handler(Options{
		DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: auth.NewDevAuthenticator("alice", ""), AuthMode: "dev",
		CausalRemediationInvestigation: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	capRequest := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
	capResponse := httptest.NewRecorder()
	handler.ServeHTTP(capResponse, capRequest)
	var capabilities Capabilities
	if err := json.Unmarshal(capResponse.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	if !capabilities.Features.CausalRemediationInvestigation || !capabilities.Features.CausalRemediationInvestigationAuthenticated {
		t.Fatalf("capabilities=%+v", capabilities)
	}

	path := "/api/jobs/job/patterns/pattern/causal-groups/group/remediation-investigation"
	body := `{"pattern_hash":"` + strings.Repeat("a", 64) + `","causal_group_hash":"` + strings.Repeat("b", 64) + `","refresh":true}`
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Host = "example.test"
	request.Header.Set("Origin", "http://example.test")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(causalRemediationIdempotencyHeader, "request-one")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || runner.owner != "alice" || runner.requestID != "request-one" || !runner.refresh {
		t.Fatalf("code=%d runner=%+v body=%s", response.Code, runner, response.Body.String())
	}
	if runner.ref.JobID != "job" || runner.ref.PatternID != "pattern" || runner.ref.CausalGroupID != "group" {
		t.Fatalf("ref=%+v", runner.ref)
	}

	get := httptest.NewRequest(http.MethodGet, path+"?pattern_hash="+strings.Repeat("a", 64)+"&causal_group_hash="+strings.Repeat("b", 64), nil)
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || strings.Contains(getResponse.Body.String(), "evidence") || strings.Contains(getResponse.Body.String(), "provider") {
		t.Fatalf("code=%d body=%s", getResponse.Code, getResponse.Body.String())
	}
	if cacheControl := getResponse.Header().Get("Cache-Control"); cacheControl != "private, no-store" {
		t.Fatalf("Cache-Control=%q", cacheControl)
	}
}

func TestCausalRemediationInvestigationRejectsInvalidAndStaleRequests(t *testing.T) {
	runner := &fakeCausalRemediationRunner{err: remediationinvestigation.ErrOperationStale}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: auth.NewDevAuthenticator("alice", ""), AuthMode: "dev",
		CausalRemediationInvestigation: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/jobs/job/patterns/pattern/causal-groups/group/remediation-investigation"
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"pattern_hash":"hash","causal_group_hash":"hash"}`))
	request.Host = "example.test"
	request.Header.Set("Origin", "http://example.test")
	request.Header.Set(causalRemediationIdempotencyHeader, "request")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "stale") {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}

	runner.err = nil
	invalid := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"pattern_hash":"hash","causal_group_hash":"hash","private":true}`))
	invalid.Host = "example.test"
	invalid.Header.Set("Origin", "http://example.test")
	invalid.Header.Set(causalRemediationIdempotencyHeader, "request")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", invalidResponse.Code, invalidResponse.Body.String())
	}

	missingKey := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"pattern_hash":"hash","causal_group_hash":"hash"}`))
	missingKey.Host = "example.test"
	missingKey.Header.Set("Origin", "http://example.test")
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missingKey)
	if missingResponse.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", missingResponse.Code)
	}
}

func TestCausalRemediationErrorContractDoesNotExposePrivateErrors(t *testing.T) {
	status, message := causalRemediationErrorDetails(errors.New("private source excerpt and provider body"))
	if status != http.StatusServiceUnavailable || strings.Contains(message, "private") || strings.Contains(message, "provider") {
		t.Fatalf("status=%d message=%q", status, message)
	}
}

type fakeCausalFixPreviewRunner struct {
	preview          causalfixpreview.Preview
	err              error
	ref              remediationinvestigation.OperationRef
	owner, requestID string
}

func (f *fakeCausalFixPreviewRunner) Preview(_ context.Context, ref remediationinvestigation.OperationRef, owner, requestID string) (causalfixpreview.Preview, error) {
	f.ref, f.owner, f.requestID = ref, owner, requestID
	return f.preview, f.err
}
func TestCausalFixPreviewIsAuthenticatedCSRFProtectedAndNonConfirmable(t *testing.T) {
	runner := &fakeCausalFixPreviewRunner{preview: causalfixpreview.Preview{Summary: "safe", BaseRevision: strings.Repeat("a", 40), ChangedFiles: []string{"controller.go"}, Diff: "diff"}}
	handler, err := Handler(Options{DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: auth.NewDevAuthenticator("alice", ""), AuthMode: "dev", CausalRemediationInvestigation: &fakeCausalRemediationRunner{}, CausalFixPreview: runner})
	if err != nil {
		t.Fatal(err)
	}
	capRes := httptest.NewRecorder()
	handler.ServeHTTP(capRes, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	var caps Capabilities
	_ = json.Unmarshal(capRes.Body.Bytes(), &caps)
	if !caps.Features.CausalRemediationFixPreview {
		t.Fatalf("caps=%+v", caps)
	}
	path := "/api/jobs/job/patterns/pattern/causal-groups/group/remediation-investigation/fix-preview"
	body := `{"pattern_hash":"` + strings.Repeat("b", 64) + `","causal_group_hash":"` + strings.Repeat("c", 64) + `"}`
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = "example.test"
	req.Header.Set("Origin", "http://example.test")
	req.Header.Set("Idempotency-Key", "id")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || runner.owner != "alice" || strings.Contains(res.Body.String(), "token") || strings.Contains(res.Body.String(), "confirm") {
		t.Fatalf("code=%d runner=%+v body=%s", res.Code, runner, res.Body.String())
	}
	evil := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	evil.Host = "example.test"
	evil.Header.Set("Origin", "https://evil.example")
	evil.Header.Set("Idempotency-Key", "id2")
	evilRes := httptest.NewRecorder()
	handler.ServeHTTP(evilRes, evil)
	if evilRes.Code != http.StatusForbidden {
		t.Fatalf("csrf code=%d", evilRes.Code)
	}
}
