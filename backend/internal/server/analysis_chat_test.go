package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeAnalysisChatRunner struct {
	createdRef       analysischat.AnalysisRef
	createdOwner     string
	createdRequestID string
	foundRef         analysischat.AnalysisRef
	foundOwner       string
	gotID            string
	gotOwner         string
	gotRequestID     string
	gotMessage       string
	createErr        error
	findErr          error
	getErr           error
	sendErr          error
	cancelErr        error
	cancelID         string
	cancelOwner      string
	cancelRequestID  string
	sendDelay        time.Duration
	view             analysischat.SessionView
}

func (f *fakeAnalysisChatRunner) Find(ref analysischat.AnalysisRef, owner string) (analysischat.SessionView, error) {
	f.foundRef, f.foundOwner = ref, owner
	if f.findErr != nil {
		return analysischat.SessionView{}, f.findErr
	}
	return analysischat.SessionView{ID: "session-1", Analysis: ref, Messages: []analysischat.Message{}, TurnsUsed: 2, MaxTurns: 10}, nil
}

func (f *fakeAnalysisChatRunner) Create(ref analysischat.AnalysisRef, owner, requestID string) (analysischat.SessionView, error) {
	f.createdRef, f.createdOwner, f.createdRequestID = ref, owner, requestID
	if f.createErr != nil {
		return analysischat.SessionView{}, f.createErr
	}
	return analysischat.SessionView{ID: "session-1", Analysis: ref, Messages: []analysischat.Message{}, TurnsUsed: 2, MaxTurns: 10}, nil
}

func (f *fakeAnalysisChatRunner) Get(id, owner string) (analysischat.SessionView, error) {
	f.gotID, f.gotOwner = id, owner
	if f.getErr != nil {
		return analysischat.SessionView{}, f.getErr
	}
	if f.view.ID != "" {
		return f.view, nil
	}
	return analysischat.SessionView{ID: id, Messages: []analysischat.Message{}, TurnsUsed: 2, MaxTurns: 10}, nil
}

func (f *fakeAnalysisChatRunner) Send(ctx context.Context, id, owner, requestID, message string) (analysischat.SessionView, error) {
	f.gotID, f.gotOwner, f.gotRequestID, f.gotMessage = id, owner, requestID, message
	if f.sendDelay > 0 {
		select {
		case <-time.After(f.sendDelay):
		case <-ctx.Done():
			return analysischat.SessionView{}, ctx.Err()
		}
	}
	if f.sendErr != nil {
		return analysischat.SessionView{}, f.sendErr
	}
	return analysischat.SessionView{ID: id, Messages: []analysischat.Message{{Role: "user", Content: message}}, TurnsUsed: 3, MaxTurns: 10}, nil
}

func (f *fakeAnalysisChatRunner) Stream(
	ctx context.Context,
	id, owner, requestID, message string,
	emit func(analysischat.Progress) error,
) (analysischat.SessionView, error) {
	if emit != nil {
		if err := emit(analysischat.Progress{
			RequestID: requestID, Phase: analysischat.PhaseInvestigating, TurnsUsed: 3, MaxTurns: 10,
		}); err != nil {
			return analysischat.SessionView{}, err
		}
	}
	return f.Send(ctx, id, owner, requestID, message)
}

func (f *fakeAnalysisChatRunner) Cancel(id, owner, requestID string) error {
	f.cancelID, f.cancelOwner, f.cancelRequestID = id, owner, requestID
	return f.cancelErr
}

func TestHandlerAnalysisChatFlow(t *testing.T) {
	dataDir := t.TempDir()
	runner := &fakeAnalysisChatRunner{}
	handler, err := Handler(Options{
		DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Get(server.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	capabilities := readBody(t, response)
	if !strings.Contains(capabilities, `"analysis_chat":true`) {
		t.Fatalf("capabilities = %s", capabilities)
	}

	response, err = http.Post(server.URL+"/api/analysis-chat/sessions", "application/json", strings.NewReader(`{"job_id":"job","build_id":"1","test_name":"Test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create status = %d", response.StatusCode)
	}
	_ = response.Body.Close()

	request := func(method, path, body string) *http.Response {
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "ok")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(analysisChatIdempotencyHeader, "request-flow")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	created := request(http.MethodPost, "/api/analysis-chat/sessions", `{"job_id":"job","build_id":"1","test_name":"Test","analysis_generated_at":"2026-07-23T12:00:00Z"}`)
	createdBody := readBody(t, created)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.StatusCode, createdBody)
	}
	if !strings.Contains(createdBody, `"turns_used":2`) || !strings.Contains(createdBody, `"max_turns":10`) {
		t.Fatalf("create body missing turn usage: %s", createdBody)
	}
	if runner.createdOwner != "alice" || runner.createdRef.BuildID != "1" || runner.createdRequestID != "request-flow" {
		t.Fatalf("create runner state = %+v owner=%q", runner.createdRef, runner.createdOwner)
	}

	found := request(http.MethodPost, "/api/analysis-chat/sessions/lookup", `{"job_id":"job","build_id":"1","test_name":"Test","analysis_generated_at":"2026-07-23T12:00:00Z"}`)
	if found.StatusCode != http.StatusOK {
		t.Fatalf("find status=%d body=%s", found.StatusCode, readBody(t, found))
	}
	_ = found.Body.Close()
	if runner.foundOwner != "alice" || runner.foundRef.BuildID != "1" {
		t.Fatalf("find runner state = %+v owner=%q", runner.foundRef, runner.foundOwner)
	}

	got := request(http.MethodGet, "/api/analysis-chat/sessions/session-1", "")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d body=%s", got.StatusCode, readBody(t, got))
	}
	_ = got.Body.Close()
	if runner.gotID != "session-1" || runner.gotOwner != "alice" {
		t.Fatalf("get runner id=%q owner=%q", runner.gotID, runner.gotOwner)
	}

	sent := request(http.MethodPost, "/api/analysis-chat/sessions/session-1/messages", `{"message":"What proves this?"}`)
	if sent.StatusCode != http.StatusOK {
		t.Fatalf("send status=%d body=%s", sent.StatusCode, readBody(t, sent))
	}
	_ = sent.Body.Close()
	if runner.gotMessage != "What proves this?" || runner.gotRequestID != "request-flow" {
		t.Fatalf("message = %q", runner.gotMessage)
	}

	streamed := request(http.MethodPost, "/api/analysis-chat/sessions/session-1/messages/stream", `{"message":"Stream this"}`)
	if streamed.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", streamed.StatusCode, readBody(t, streamed))
	}
	streamBody := readBody(t, streamed)
	if !strings.Contains(streamBody, "event: progress") || !strings.Contains(streamBody, `"phase":"investigating"`) ||
		!strings.Contains(streamBody, `"turns_used":3`) || !strings.Contains(streamBody, `"max_turns":10`) ||
		!strings.Contains(streamBody, "event: session") {
		t.Fatalf("stream body = %q", streamBody)
	}

	cancelled := request(http.MethodPost, "/api/analysis-chat/sessions/session-1/requests/request-flow/cancel", "")
	if cancelled.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel status=%d body=%s", cancelled.StatusCode, readBody(t, cancelled))
	}
	_ = cancelled.Body.Close()
	if runner.cancelID != "session-1" || runner.cancelOwner != "alice" || runner.cancelRequestID != "request-flow" {
		t.Fatalf("cancel runner id=%q owner=%q request=%q", runner.cancelID, runner.cancelOwner, runner.cancelRequestID)
	}
}

func TestHandlerAnalysisChatLookupMissingSession(t *testing.T) {
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: &fakeAnalysisChatRunner{findErr: analysischat.ErrSessionNotFound},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/analysis-chat/sessions/lookup", strings.NewReader(`{"job_id":"job","build_id":"1","test_name":"Test"}`))
	req.Header.Set("Authorization", "ok")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("lookup status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q, want private, no-store", got)
	}
}

func TestHandlerAnalysisChatRequiresIdempotencyKey(t *testing.T) {
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: &fakeAnalysisChatRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		path string
		body string
	}{
		{path: "/api/analysis-chat/sessions", body: `{"job_id":"job","build_id":"1","test_name":"Test"}`},
		{path: "/api/analysis-chat/sessions/session-1/messages", body: `{"message":"question"}`},
	} {
		req := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		req.Header.Set("Authorization", "ok")
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "missing idempotency key") {
			t.Fatalf("POST %s status=%d body=%q", testCase.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestHandlerAnalysisChatRejectsMalformedAndCrossOriginRequests(t *testing.T) {
	dataDir := t.TempDir()
	runner := &fakeAnalysisChatRunner{}
	handler, err := Handler(Options{
		DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: runner,
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		body   string
		origin string
		want   int
	}{
		{name: "unknown field", body: `{"job_id":"job","build_id":"1","test_name":"Test","extra":true}`, want: http.StatusBadRequest},
		{name: "trailing json", body: `{"job_id":"job","build_id":"1","test_name":"Test"}{}`, want: http.StatusBadRequest},
		{name: "cross origin", body: `{"job_id":"job","build_id":"1","test_name":"Test"}`, origin: "https://evil.example", want: http.StatusForbidden},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://dashboard.example/api/analysis-chat/sessions", strings.NewReader(testCase.body))
			req.Header.Set("Authorization", "ok")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(analysisChatIdempotencyHeader, "request-malformed")
			if testCase.origin != "" {
				req.Header.Set("Origin", testCase.origin)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != testCase.want {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestHandlerAnalysisChatStreamingRoutesRejectCrossOrigin(t *testing.T) {
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: &fakeAnalysisChatRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		path string
		body string
	}{
		{path: "/api/analysis-chat/sessions/session-1/messages/stream", body: `{"message":"question"}`},
		{path: "/api/analysis-chat/sessions/session-1/requests/request-1/cancel"},
	} {
		req := httptest.NewRequest(http.MethodPost, "https://dashboard.example"+testCase.path, strings.NewReader(testCase.body))
		req.Header.Set("Authorization", "ok")
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(analysisChatIdempotencyHeader, "request-1")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("POST %s status=%d body=%q", testCase.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestHandlerAnalysisChatStreamErrorIsSanitized(t *testing.T) {
	runner := &fakeAnalysisChatRunner{sendErr: errors.New("provider secret https://private.example/v1")}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/analysis-chat/sessions/session-1/messages/stream", strings.NewReader(`{"message":"question"}`))
	req.Header.Set("Authorization", "ok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(analysisChatIdempotencyHeader, "request-1")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "event: error") ||
		!strings.Contains(recorder.Body.String(), "analysis chat could not complete the request") {
		t.Fatalf("stream status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "private.example") {
		t.Fatal("provider URL leaked to stream")
	}
	if strings.Contains(recorder.Body.String(), `"outcome":""`) {
		t.Fatal("ambiguous stream error carried a false terminal outcome")
	}
}

func TestHandlerAnalysisChatReturnsSafeAttemptHistory(t *testing.T) {
	runner := &fakeAnalysisChatRunner{view: analysischat.SessionView{
		ID: "session-1", TurnsUsed: 3, MaxTurns: 10,
		Messages: []analysischat.Message{
			{Role: "user", RequestID: "success", Content: "successful question", CreatedAt: "2026-07-27T12:00:00Z"},
			{Role: "assistant", RequestID: "success", Content: "safe answer", CreatedAt: "2026-07-27T12:00:00Z"},
		},
		Attempts: []analysischat.Attempt{
			{RequestID: "success", Question: "successful question", Outcome: "succeeded", Turn: 1},
			{RequestID: "cancelled", Question: "cancelled question", Outcome: "cancelled", Turn: 2},
			{RequestID: "provider", Question: "provider question", Outcome: "failed", FailureKind: "provider", Turn: 3},
		},
	}}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/analysis-chat/sessions/session-1", nil)
	request.Header.Set("Authorization", "ok")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	var view analysischat.SessionView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.TurnsUsed != 3 || view.MaxTurns != 10 || len(view.Attempts) != 3 || view.Attempts[2].FailureKind != "provider" {
		t.Fatalf("attempt response = %+v", view)
	}
	for _, private := range []string{"provider error", "system prompt", "provider token", "/private/path"} {
		if strings.Contains(recorder.Body.String(), private) {
			t.Fatalf("response leaked %q: %s", private, recorder.Body.String())
		}
	}
}

func TestHandlerAnalysisChatJSONWaiterHasTurnCompletionGrace(t *testing.T) {
	runner := &fakeAnalysisChatRunner{sendDelay: 50 * time.Millisecond}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: runner, AnalysisChatTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/analysis-chat/sessions/session-1/messages", strings.NewReader(`{"message":"question"}`))
	req.Header.Set("Authorization", "ok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(analysisChatIdempotencyHeader, "request-grace")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("JSON waiter status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestWriteAnalysisChatErrorMapping(t *testing.T) {
	cases := []struct {
		err         error
		want        int
		wantBody    string
		wantOutcome string
	}{
		{analysischat.ErrAnalysisNotFound, http.StatusNotFound, "analysis not found", "rejected"},
		{analysischat.ErrSessionNotFound, http.StatusNotFound, "analysis chat session not found", "rejected"},
		{analysischat.ErrRequestNotFound, http.StatusNotFound, "analysis chat request not found", "rejected"},
		{analysischat.ErrAnalysisChanged, http.StatusConflict, "analysis changed", "rejected"},
		{analysischat.ErrSessionBusy, http.StatusConflict, "analysis chat session is busy", "pending"},
		{analysischat.ErrRequestPending, http.StatusConflict, "analysis chat request is pending", "pending"},
		{analysischat.ErrIdempotencyConflict, http.StatusConflict, "analysis chat request conflicts with existing input", "rejected"},
		{analysischat.ErrRequestOutcomeUnknown, http.StatusConflict, "analysis chat outcome is unknown", "unknown"},
		{analysischat.ErrInvalidRequest, http.StatusBadRequest, "invalid analysis chat request", "rejected"},
		{analysischat.ErrSessionLimit, http.StatusTooManyRequests, "analysis chat limit reached", "rejected"},
		{analysischat.ErrActiveTurnLimit, http.StatusTooManyRequests, "analysis chat limit reached", "rejected"},
		{analysischat.ErrRateLimit, http.StatusTooManyRequests, "analysis chat limit reached", "rejected"},
		{analysischat.ErrSourceInvestigationLimit, http.StatusTooManyRequests, "analysis chat limit reached", "rejected"},
		{analysischat.ErrSourceInvestigationActiveLimit, http.StatusTooManyRequests, "analysis chat limit reached", "rejected"},
		{sourceinvestigation.ErrInvalidResult, http.StatusBadGateway, "source investigation could not complete the request", "failed"},
		{analysischat.ErrRequestFailed, http.StatusBadGateway, "analysis chat could not complete the request", "failed"},
		{analysischat.ErrProviderRequestFailed, http.StatusBadGateway, analysischat.ErrProviderRequestFailed.Error(), "failed"},
		{analysischat.ErrResponseValidationFailed, http.StatusBadGateway, analysischat.ErrResponseValidationFailed.Error(), "failed"},
		{analysischat.ErrCitationValidationFailed, http.StatusBadGateway, analysischat.ErrCitationValidationFailed.Error(), "failed"},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, "analysis chat request timed out", "failed"},
		{errors.New("provider secret https://private.example/v1"), http.StatusBadGateway, "analysis chat could not complete the request", ""},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		writeAnalysisChatError(recorder, "session", "alice", testCase.err)
		if recorder.Code != testCase.want {
			t.Errorf("error %v status=%d want=%d", testCase.err, recorder.Code, testCase.want)
		}
		if !strings.Contains(recorder.Body.String(), testCase.wantBody) {
			t.Errorf("error %v body=%q want substring %q", testCase.err, recorder.Body.String(), testCase.wantBody)
		}
		if got := recorder.Header().Get(analysisChatOutcomeHeader); got != testCase.wantOutcome {
			t.Errorf("error %v outcome=%q want=%q", testCase.err, got, testCase.wantOutcome)
		}
		if strings.Contains(recorder.Body.String(), "private.example") {
			t.Fatal("provider URL leaked to response")
		}
	}
}

var _ AnalysisChatRunner = (*fakeAnalysisChatRunner)(nil)

func TestSafeAnalysisChatErrorHidesProviderBodies(t *testing.T) {
	if got := safeAnalysisChatError(fmt.Errorf("%w: private provider body", analysischat.ErrRequestFailed)); got != "model request failed" {
		t.Fatalf("persisted request failure log = %q", got)
	}
	for _, reason := range []string{
		`chat returned 500: private prompt body`,
		`responses status 500: private artifact body`,
		`decode response: invalid character; body=private analysis data`,
	} {
		if got := safeAnalysisChatError(errors.New(reason)); got != "model request failed" {
			t.Errorf("safe error for %q = %q", reason, got)
		}
	}
}

func TestHandlerAnalysisChatAcceptsWorstCaseEncodedBodies(t *testing.T) {
	dataDir := t.TempDir()
	runner := &fakeAnalysisChatRunner{}
	handler, err := Handler(Options{
		DataDir: dataDir, Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	refBody, err := json.Marshal(analysischat.AnalysisRef{
		JobID: strings.Repeat(`"`, 1024), BuildID: strings.Repeat(`\`, 256),
		TestName: strings.Repeat(`"`, 4096), SuiteName: strings.Repeat(`\`, 4096),
		ClassName: strings.Repeat(`"`, 4096), JUnitFile: strings.Repeat(`\`, 1024),
		AnalysisGeneratedAt: strings.Repeat(`"`, 128),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string, body []byte) *http.Response {
		req, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "ok")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(analysisChatIdempotencyHeader, "request-large")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	created := request("/api/analysis-chat/sessions", refBody)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("large encoded reference status=%d body=%s", created.StatusCode, readBody(t, created))
	}
	_ = created.Body.Close()

	messageBody, err := json.Marshal(map[string]string{"message": strings.Repeat(`\`, 4096)})
	if err != nil {
		t.Fatal(err)
	}
	sent := request("/api/analysis-chat/sessions/session-1/messages", messageBody)
	if sent.StatusCode != http.StatusOK {
		t.Fatalf("large encoded message status=%d body=%s", sent.StatusCode, readBody(t, sent))
	}
	_ = sent.Body.Close()
}

func TestHandlerAnalysisChatAcceptsPatternReference(t *testing.T) {
	runner := &fakeAnalysisChatRunner{}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/analysis-chat/sessions", strings.NewReader(
		`{"scope":"pattern","job_id":"periodic-demo","pattern_id":"pattern-1","pattern_hash":"hash-1"}`,
	))
	request.Header.Set("Authorization", "ok")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(analysisChatIdempotencyHeader, "pattern-create")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if runner.createdRef.Scope != analysischat.ScopePattern || runner.createdRef.PatternID != "pattern-1" || runner.createdRef.PatternHash != "hash-1" {
		t.Fatalf("created ref = %+v", runner.createdRef)
	}
}
