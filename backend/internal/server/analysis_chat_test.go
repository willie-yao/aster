package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
)

type fakeAnalysisChatRunner struct {
	createdRef   analysischat.AnalysisRef
	createdOwner string
	gotID        string
	gotOwner     string
	gotMessage   string
	createErr    error
	getErr       error
	sendErr      error
}

func (f *fakeAnalysisChatRunner) Create(ref analysischat.AnalysisRef, owner string) (analysischat.SessionView, error) {
	f.createdRef, f.createdOwner = ref, owner
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

func (f *fakeAnalysisChatRunner) Send(_ context.Context, id, owner, message string) (analysischat.SessionView, error) {
	f.gotID, f.gotOwner, f.gotMessage = id, owner, message
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
	if runner.createdOwner != "alice" || runner.createdRef.BuildID != "1" {
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
	if runner.gotMessage != "What proves this?" {
		t.Fatalf("message = %q", runner.gotMessage)
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
		err      error
		want     int
		wantBody string
	}{
		{analysischat.ErrAnalysisNotFound, http.StatusNotFound, "analysis not found"},
		{analysischat.ErrSessionNotFound, http.StatusNotFound, "analysis chat session not found"},
		{analysischat.ErrAnalysisChanged, http.StatusConflict, "analysis changed"},
		{analysischat.ErrInvalidRequest, http.StatusBadRequest, "invalid analysis chat request"},
		{analysischat.ErrSessionLimit, http.StatusTooManyRequests, "analysis chat session limit reached"},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, "analysis chat request timed out"},
		{errors.New("provider secret https://private.example/v1"), http.StatusBadGateway, "analysis chat could not complete the request"},
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
		if strings.Contains(recorder.Body.String(), "private.example") {
			t.Fatal("provider URL leaked to response")
		}
	}
}

var _ AnalysisChatRunner = (*fakeAnalysisChatRunner)(nil)

func TestSafeAnalysisChatErrorHidesProviderBodies(t *testing.T) {
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
