package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actions"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
)

type fakeChatFixRunner struct {
	sessionID       string
	owner           string
	requestID       string
	patternID       string
	sourceRequestID string
	userToken       string
	instruction     string
	err             error
}

func (f *fakeChatFixRunner) PreviewChatFix(
	_ context.Context,
	sessionID, owner, requestID, patternID, sourceRequestID, userToken, instruction string,
) (actions.PreviewResult, error) {
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	f.patternID, f.sourceRequestID = patternID, sourceRequestID
	f.userToken, f.instruction = userToken, instruction
	if f.err != nil {
		return actions.PreviewResult{}, f.err
	}
	return actions.PreviewResult{Token: "preview-token", Kind: "fix", Title: "Fix retry", Diff: "diff"}, nil
}

func TestHandlerChatFixPreview(t *testing.T) {
	runner := &fakeChatFixRunner{}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		Actions: &fakeRunner{}, AnalysisChat: &fakeAnalysisChatRunner{}, ChatFix: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	capabilities, err := http.Get(server.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	if body := readBody(t, capabilities); !strings.Contains(body, `"chat_fix":true`) {
		t.Fatalf("capabilities = %s", body)
	}

	req, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/api/analysis-chat/sessions/session-1/requests/chat-1/fix/preview",
		strings.NewReader(`{"pattern_id":"pattern-1","source_request_id":"source-1","instruction":"keep compatibility"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "ok")
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(readBody(t, response), `"token":"preview-token"`) {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if runner.sessionID != "session-1" || runner.owner != "alice" || runner.requestID != "chat-1" ||
		runner.patternID != "pattern-1" || runner.sourceRequestID != "source-1" ||
		runner.userToken != "tok" || runner.instruction != "keep compatibility" {
		t.Fatalf("runner = %+v", runner)
	}
}

func TestHandlerChatFixRejectsInvalidBodies(t *testing.T) {
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		Actions: &fakeRunner{}, AnalysisChat: &fakeAnalysisChatRunner{}, ChatFix: &fakeChatFixRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`,
		`{"pattern_id":"pattern","extra":true}`,
		`{"pattern_id":"pattern"}{}`,
		`{"pattern_id":"pattern","assistant_answer":"client text is forbidden"}`,
		`{"pattern_id":"` + strings.Repeat("x", maxChatFixPatternBytes+1) + `"}`,
		`{"pattern_id":"pattern","source_request_id":"` + strings.Repeat("x", maxChatFixRequestIDBytes+1) + `"}`,
		`{"pattern_id":"pattern","instruction":"` + strings.Repeat("x", maxChatFixInputBytes+1) + `"}`,
	} {
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/analysis-chat/sessions/session/requests/request/fix/preview",
			strings.NewReader(body),
		)
		req.Header.Set("Authorization", "ok")
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %q status = %d", body, recorder.Code)
		}
	}
}

func TestWriteChatFixErrorMapping(t *testing.T) {
	for _, testCase := range []struct {
		err  error
		want int
	}{
		{analysischat.ErrSessionNotFound, http.StatusNotFound},
		{actions.ErrPatternMismatch, http.StatusConflict},
		{analysischat.ErrAnalysisChanged, http.StatusConflict},
		{analysischat.ErrRequestPending, http.StatusConflict},
		{analysischat.ErrInvalidRequest, http.StatusBadRequest},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
	} {
		recorder := httptest.NewRecorder()
		writeChatFixError(recorder, "session", "alice", testCase.err)
		if recorder.Code != testCase.want {
			t.Errorf("error %v status = %d, want %d", testCase.err, recorder.Code, testCase.want)
		}
	}
}

func TestHandlerRejectsChatFixWithoutDependencies(t *testing.T) {
	if _, err := Handler(Options{DataDir: t.TempDir(), ChatFix: &fakeChatFixRunner{}}); err == nil {
		t.Fatal("chat fix was accepted without chat and actions")
	}
}

var _ ChatFixRunner = (*fakeChatFixRunner)(nil)
