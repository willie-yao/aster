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

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
)

type fakeAnalysisChatRunner struct {
	createdRef       analysischat.AnalysisRef
	createdOwner     string
	createdRequestID string
	gotID            string
	gotOwner         string
	gotRequestID     string
	gotMessage       string
	createErr        error
	getErr           error
	sendErr          error
}

func (f *fakeAnalysisChatRunner) Create(ref analysischat.AnalysisRef, owner, requestID string) (analysischat.SessionView, error) {
	f.createdRef, f.createdOwner, f.createdRequestID = ref, owner, requestID
	if f.createErr != nil {
		return analysischat.SessionView{}, f.createErr
	}
	return analysischat.SessionView{ID: "session-1", Analysis: ref, Messages: []analysischat.Message{}}, nil
}

func (f *fakeAnalysisChatRunner) Get(id, owner string) (analysischat.SessionView, error) {
	f.gotID, f.gotOwner = id, owner
	if f.getErr != nil {
		return analysischat.SessionView{}, f.getErr
	}
	return analysischat.SessionView{ID: id, Messages: []analysischat.Message{}}, nil
}

func (f *fakeAnalysisChatRunner) Send(_ context.Context, id, owner, requestID, message string) (analysischat.SessionView, error) {
	f.gotID, f.gotOwner, f.gotRequestID, f.gotMessage = id, owner, requestID, message
	if f.sendErr != nil {
		return analysischat.SessionView{}, f.sendErr
	}
	return analysischat.SessionView{ID: id, Messages: []analysischat.Message{{Role: "user", Content: message}}}, nil
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
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.StatusCode, readBody(t, created))
	}
	_ = created.Body.Close()
	if runner.createdOwner != "alice" || runner.createdRef.BuildID != "1" || runner.createdRequestID != "request-flow" {
		t.Fatalf("create runner state = %+v owner=%q", runner.createdRef, runner.createdOwner)
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

func TestWriteAnalysisChatErrorMapping(t *testing.T) {
	cases := []struct {
		err         error
		want        int
		wantBody    string
		wantOutcome string
	}{
		{analysischat.ErrAnalysisNotFound, http.StatusNotFound, "analysis not found", "rejected"},
		{analysischat.ErrSessionNotFound, http.StatusNotFound, "analysis chat session not found", "rejected"},
		{analysischat.ErrAnalysisChanged, http.StatusConflict, "analysis changed", "rejected"},
		{analysischat.ErrSessionBusy, http.StatusConflict, "analysis chat session is busy", "pending"},
		{analysischat.ErrIdempotencyConflict, http.StatusConflict, "analysis chat idempotency key conflict", "rejected"},
		{analysischat.ErrRequestOutcomeUnknown, http.StatusConflict, "analysis chat request outcome unknown", "unknown"},
		{analysischat.ErrInvalidRequest, http.StatusBadRequest, "invalid analysis chat request", "rejected"},
		{analysischat.ErrSessionLimit, http.StatusTooManyRequests, "analysis chat session limit reached", "rejected"},
		{analysischat.ErrRequestFailed, http.StatusBadGateway, "analysis chat could not complete the request", "failed"},
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
