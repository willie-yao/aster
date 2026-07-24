package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeSourceInvestigationRunner struct {
	sessionID     string
	owner         string
	requestID     string
	chatRequestID string
	cancelled     bool
}

func (f *fakeSourceInvestigationRunner) SourceInvestigation(
	_ context.Context, sessionID, owner, requestID, chatRequestID string,
) (sourceinvestigation.View, error) {
	f.sessionID, f.owner, f.requestID, f.chatRequestID = sessionID, owner, requestID, chatRequestID
	return sourceinvestigation.View{ID: requestID, SessionID: sessionID, ChatRequestID: chatRequestID, Status: sourceinvestigation.StatusSucceeded}, nil
}

func (f *fakeSourceInvestigationRunner) StreamSourceInvestigation(
	ctx context.Context, sessionID, owner, requestID, chatRequestID string,
	emit func(sourceinvestigation.Progress) error,
) (sourceinvestigation.View, error) {
	if emit != nil {
		if err := emit(sourceinvestigation.Progress{RequestID: requestID, Phase: sourceinvestigation.PhaseInvestigating}); err != nil {
			return sourceinvestigation.View{}, err
		}
	}
	return f.SourceInvestigation(ctx, sessionID, owner, requestID, chatRequestID)
}

func (f *fakeSourceInvestigationRunner) GetSourceInvestigation(sessionID, owner, requestID string) (sourceinvestigation.View, error) {
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	return sourceinvestigation.View{ID: requestID, SessionID: sessionID, Status: sourceinvestigation.StatusPending}, nil
}

func (f *fakeSourceInvestigationRunner) CancelSourceInvestigation(sessionID, owner, requestID string) error {
	f.sessionID, f.owner, f.requestID, f.cancelled = sessionID, owner, requestID, true
	return nil
}

func TestHandlerSourceInvestigationFlow(t *testing.T) {
	runner := &fakeSourceInvestigationRunner{}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: &fakeAnalysisChatRunner{}, SourceInvestigation: runner,
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
	body := readBody(t, response)
	if !strings.Contains(body, `"source_investigation":true`) {
		t.Fatalf("capabilities = %s", body)
	}

	request := func(method, path, body string) *http.Response {
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "ok")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(analysisChatIdempotencyHeader, "source-1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	streamed := request(http.MethodPost, "/api/analysis-chat/sessions/session-1/source-investigations/stream", `{"chat_request_id":"chat-1"}`)
	if streamed.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", streamed.StatusCode, readBody(t, streamed))
	}
	streamBody := readBody(t, streamed)
	if !strings.Contains(streamBody, "event: progress") || !strings.Contains(streamBody, "event: investigation") {
		t.Fatalf("stream body = %q", streamBody)
	}
	if runner.owner != "alice" || runner.chatRequestID != "chat-1" || runner.requestID != "source-1" {
		t.Fatalf("runner state = %+v", runner)
	}

	got := request(http.MethodGet, "/api/analysis-chat/sessions/session-1/source-investigations/source-1", "")
	if got.StatusCode != http.StatusOK {
		t.Fatalf("get status=%d body=%s", got.StatusCode, readBody(t, got))
	}
	_ = got.Body.Close()

	cancelled := request(http.MethodPost, "/api/analysis-chat/sessions/session-1/source-investigations/source-1/cancel", "")
	if cancelled.StatusCode != http.StatusNoContent || !runner.cancelled {
		t.Fatalf("cancel status=%d state=%+v", cancelled.StatusCode, runner)
	}
	_ = cancelled.Body.Close()
}

func TestHandlerSourceInvestigationRequiresBoundedInput(t *testing.T) {
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisChat: &fakeAnalysisChatRunner{}, SourceInvestigation: &fakeSourceInvestigationRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"chat_request_id":"chat","extra":true}`, `{"chat_request_id":"chat"}{}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/analysis-chat/sessions/session/source-investigations", strings.NewReader(body))
		req.Header.Set("Authorization", "ok")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(analysisChatIdempotencyHeader, "source")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %q status=%d", body, recorder.Code)
		}
	}
}

var _ SourceInvestigationRunner = (*fakeSourceInvestigationRunner)(nil)
