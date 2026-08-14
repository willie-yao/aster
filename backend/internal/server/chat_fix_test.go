package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/analysischat"
)

type fakeChatFixRunner struct {
	sessionID       string
	owner           string
	requestID       string
	patternID       string
	patternHash     string
	sourceRequestID string
	userToken       string
	instruction     string
	err             error
	requestCreated  bool
}

type blockingHTTPChatFixRunner struct {
	fakeRunner
	fakeChatFixRunner

	mu             sync.Mutex
	request        actions.ActionRequestView
	generatorCalls int
	backgroundCtx  context.Context
	started        chan struct{}
	release        chan struct{}
}

func newBlockingHTTPChatFixRunner() *blockingHTTPChatFixRunner {
	return &blockingHTTPChatFixRunner{started: make(chan struct{}), release: make(chan struct{})}
}

func (r *blockingHTTPChatFixRunner) CreateAnalysisFixRequest(
	_, owner, _, _, _ string,
) (actions.ActionRequestView, error) {
	r.mu.Lock()
	if r.request.ID != "" {
		view := r.request
		r.mu.Unlock()
		return view, nil
	}
	now := time.Now().UTC()
	r.request = actions.ActionRequestView{
		ID: "http-lifecycle-request", FailureID: "analysis", Kind: "analysis-fix", Owner: owner,
		Status: actions.RequestPending, Stage: actions.RequestStageVerifying,
		CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}
	r.generatorCalls++
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	r.backgroundCtx = ctx
	view := r.request
	r.mu.Unlock()

	go func() {
		defer cancel()
		close(r.started)
		select {
		case <-r.release:
			r.mu.Lock()
			r.request.Status = actions.RequestReady
			r.request.Stage = actions.RequestStageDrafting
			r.request.Preview = &actions.PreviewResult{
				Token: "preview-token", Kind: "fix", Title: "Fix conflict",
				Body: "## Summary\nRetry the conflict.\n", Diff: "diff --git a/test/e2e/cni.go b/test/e2e/cni.go",
			}
			r.request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			r.mu.Unlock()
		case <-ctx.Done():
		}
	}()
	return view, nil
}

func (r *blockingHTTPChatFixRunner) CreateRequest(_, _, _, _, _, _ string) (actions.ActionRequestView, error) {
	return actions.ActionRequestView{}, errors.New("unexpected direct action request")
}

func (r *blockingHTTPChatFixRunner) GetRequest(id, owner string) (actions.ActionRequestView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.request.ID != id || r.request.Owner != owner {
		return actions.ActionRequestView{}, actions.ErrRequestNotFound
	}
	return r.request, nil
}

func (r *blockingHTTPChatFixRunner) ConfirmRequest(context.Context, string, string, string) (string, error) {
	return "", errors.New("unexpected confirmation")
}

func (r *blockingHTTPChatFixRunner) CancelRequest(context.Context, string, string) (actions.ActionRequestView, error) {
	return actions.ActionRequestView{}, errors.New("unexpected cancellation")
}

func (r *blockingHTTPChatFixRunner) snapshot() (actions.ActionRequestView, int, context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.request, r.generatorCalls, r.backgroundCtx
}

func (f *fakeChatFixRunner) PreviewChatFix(
	_ context.Context,
	sessionID, owner, requestID, patternID, patternHash, sourceRequestID, userToken, instruction string,
) (actions.PreviewResult, error) {
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	f.patternID, f.patternHash, f.sourceRequestID = patternID, patternHash, sourceRequestID
	f.userToken, f.instruction = userToken, instruction
	if f.err != nil {
		return actions.PreviewResult{}, f.err
	}
	return actions.PreviewResult{Token: "preview-token", Kind: "fix", Title: "Fix retry", Diff: "diff"}, nil
}

func (f *fakeChatFixRunner) CreateAnalysisFixRequest(
	sessionID, owner, requestID, userToken, instruction string,
) (actions.ActionRequestView, error) {
	f.sessionID, f.owner, f.requestID = sessionID, owner, requestID
	f.userToken, f.instruction, f.requestCreated = userToken, instruction, true
	if f.err != nil {
		return actions.ActionRequestView{}, f.err
	}
	return actions.ActionRequestView{
		ID: "action-request", FailureID: "analysis", Kind: "analysis-fix", Owner: owner,
		Status: actions.RequestPending, CreatedAt: "2026-08-14T00:00:00Z", UpdatedAt: "2026-08-14T00:00:00Z", ExpiresAt: "2026-08-14T01:00:00Z",
	}, nil
}

func TestHandlerChatFixPreview(t *testing.T) {
	runner := &fakeChatFixRunner{}
	capabilities := DefaultCapabilities()
	capabilities.Features.JUnitChatFix = true
	capabilities.Features.ChatFixMinConfidence = "medium"
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: capabilities, Auth: fakeAuth{}, AuthMode: "dev",
		Actions: &fakeRunner{}, AnalysisChat: &fakeAnalysisChatRunner{}, ChatFix: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	capabilitiesResponse, err := http.Get(server.URL + "/api/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, capabilitiesResponse)
	if !strings.Contains(body, `"chat_fix":true`) || !strings.Contains(body, `"junit_chat_fix":true`) || !strings.Contains(body, `"chat_fix_min_confidence":"medium"`) {
		t.Fatalf("capabilities = %s", body)
	}

	req, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/api/analysis-chat/sessions/session-1/requests/chat-1/fix/preview",
		strings.NewReader(`{"pattern_id":"pattern-1","pattern_hash":"hash-1","source_request_id":"source-1","instruction":"keep compatibility"}`),
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
		runner.patternID != "pattern-1" || runner.patternHash != "hash-1" || runner.sourceRequestID != "source-1" ||
		runner.userToken != "tok" || runner.instruction != "keep compatibility" {
		t.Fatalf("runner = %+v", runner)
	}
}

func TestHandlerExactJUnitChatFixPreview(t *testing.T) {
	runner := &fakeChatFixRunner{}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		Actions: &fakeRunner{}, AnalysisChat: &fakeAnalysisChatRunner{}, ChatFix: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/analysis-chat/sessions/session/requests/request/fix/requests", strings.NewReader(`{"instruction":"keep the API stable"}`))
	req.Header.Set("Authorization", "ok")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusAccepted || !strings.Contains(recorder.Body.String(), `"id":"action-request"`) {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !runner.requestCreated || runner.patternID != "" || runner.patternHash != "" || runner.sourceRequestID != "" || runner.instruction != "keep the API stable" {
		t.Fatalf("runner = %+v", runner)
	}
}

func TestHandlerExactJUnitChatFixRequestRejectsLegacyFields(t *testing.T) {
	runner := &fakeChatFixRunner{}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		Actions: &fakeRunner{}, AnalysisChat: &fakeAnalysisChatRunner{}, ChatFix: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/analysis-chat/sessions/session/requests/request/fix/requests", strings.NewReader(`{"pattern_id":"pattern"}`))
	req.Header.Set("Authorization", "ok")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest || runner.requestCreated {
		t.Fatalf("status=%d runner=%+v", recorder.Code, runner)
	}
}

func TestHandlerExactJUnitChatFixRequestRequiresAuthentication(t *testing.T) {
	runner := &fakeChatFixRunner{}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		Actions: &fakeRunner{}, AnalysisChat: &fakeAnalysisChatRunner{}, ChatFix: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/analysis-chat/sessions/session/requests/request/fix/requests", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized || runner.requestCreated {
		t.Fatalf("status=%d runner=%+v", recorder.Code, runner)
	}
}

func TestExactJUnitChatFixRequestSurvivesHTTPDisconnect(t *testing.T) {
	runner := newBlockingHTTPChatFixRunner()
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		Actions: runner, AnalysisChat: &fakeAnalysisChatRunner{}, ChatFix: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		server.URL+"/api/analysis-chat/sessions/session/requests/chat-request/fix/requests",
		strings.NewReader(`{"instruction":"keep compatibility"}`),
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
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("admission status=%d body=%s", response.StatusCode, readBody(t, response))
	}
	var admitted actions.ActionRequestView
	if err := json.NewDecoder(response.Body).Decode(&admitted); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if admitted.ID != "http-lifecycle-request" || admitted.Status != actions.RequestPending {
		t.Fatalf("admitted=%+v", admitted)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("background generation did not start")
	}

	cancelRequest()
	_, calls, backgroundCtx := runner.snapshot()
	select {
	case <-backgroundCtx.Done():
		t.Fatal("originating HTTP cancellation canceled background generation")
	default:
	}
	if calls != 1 {
		t.Fatalf("generator calls=%d", calls)
	}

	get := func() actions.ActionRequestView {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, server.URL+"/api/action-requests/"+admitted.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "ok")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status read=%d body=%s", response.StatusCode, readBody(t, response))
		}
		var view actions.ActionRequestView
		if err := json.NewDecoder(response.Body).Decode(&view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	if pending := get(); pending.Status != actions.RequestPending {
		t.Fatalf("pending=%+v", pending)
	}

	close(runner.release)
	deadline := time.Now().Add(time.Second)
	var ready actions.ActionRequestView
	for time.Now().Before(deadline) {
		ready = get()
		if ready.Status == actions.RequestReady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ready.Status != actions.RequestReady || ready.Preview == nil || ready.Preview.Token != "preview-token" {
		t.Fatalf("ready=%+v", ready)
	}

	repeat, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/api/analysis-chat/sessions/session/requests/chat-request/fix/requests",
		strings.NewReader(`{"instruction":"keep compatibility"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	repeat.Header.Set("Authorization", "ok")
	repeat.Header.Set("Content-Type", "application/json")
	repeatResponse, err := http.DefaultClient.Do(repeat)
	if err != nil {
		t.Fatal(err)
	}
	defer repeatResponse.Body.Close()
	if repeatResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("repeat status=%d body=%s", repeatResponse.StatusCode, readBody(t, repeatResponse))
	}
	var recovered actions.ActionRequestView
	if err := json.NewDecoder(repeatResponse.Body).Decode(&recovered); err != nil {
		t.Fatal(err)
	}
	_, calls, _ = runner.snapshot()
	if recovered.ID != admitted.ID || recovered.Status != actions.RequestReady || calls != 1 {
		t.Fatalf("recovered=%+v calls=%d", recovered, calls)
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
		`{"pattern_id":"pattern"}`,
		`{"pattern_id":"pattern","pattern_hash":"hash","extra":true}`,
		`{"pattern_id":"pattern","pattern_hash":"hash"}{}`,
		`{"pattern_id":"pattern","pattern_hash":"hash","assistant_answer":"client text is forbidden"}`,
		`{"pattern_id":"` + strings.Repeat("x", maxChatFixPatternBytes+1) + `","pattern_hash":"hash"}`,
		`{"pattern_id":"pattern","pattern_hash":"` + strings.Repeat("x", maxChatFixPatternHash+1) + `"}`,
		`{"pattern_id":"pattern","pattern_hash":"hash","source_request_id":"` + strings.Repeat("x", maxChatFixRequestIDBytes+1) + `"}`,
		`{"pattern_id":"pattern","pattern_hash":"hash","instruction":"` + strings.Repeat("x", maxChatFixInputBytes+1) + `"}`,
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
		{analysischat.ErrAnalysisNotFound, http.StatusNotFound},
		{analysischat.ErrSessionNotFound, http.StatusNotFound},
		{actions.ErrPatternMismatch, http.StatusConflict},
		{actions.ErrPreviewTargetChanged, http.StatusConflict},
		{actions.ErrPreviewPending, http.StatusConflict},
		{analysischat.ErrAnalysisChanged, http.StatusConflict},
		{analysischat.ErrPatternChanged, http.StatusConflict},
		{analysischat.ErrRequestPending, http.StatusConflict},
		{analysischat.ErrInvalidRequest, http.StatusBadRequest},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
		{fmt.Errorf("%w: no code change", actions.ErrPreviewRejected), http.StatusUnprocessableEntity},
		{errors.New("opening /private/chat/state.json: denied"), http.StatusInternalServerError},
	} {
		recorder := httptest.NewRecorder()
		writeChatFixError(recorder, "session", "alice", testCase.err)
		if recorder.Code != testCase.want {
			t.Errorf("error %v status = %d, want %d", testCase.err, recorder.Code, testCase.want)
		}
		if testCase.want == http.StatusInternalServerError && strings.Contains(recorder.Body.String(), "/private/chat") {
			t.Fatalf("private path leaked: %q", recorder.Body.String())
		}
	}
}

func TestHandlerRejectsChatFixWithoutDependencies(t *testing.T) {
	if _, err := Handler(Options{DataDir: t.TempDir(), ChatFix: &fakeChatFixRunner{}}); err == nil {
		t.Fatal("chat fix was accepted without chat and actions")
	}
}

var _ ChatFixRunner = (*fakeChatFixRunner)(nil)
var _ ChatFixRequestRunner = (*fakeChatFixRunner)(nil)
var _ ActionRunner = (*blockingHTTPChatFixRunner)(nil)
var _ ActionRequestRunner = (*blockingHTTPChatFixRunner)(nil)
var _ ChatFixRunner = (*blockingHTTPChatFixRunner)(nil)
var _ ChatFixRequestRunner = (*blockingHTTPChatFixRunner)(nil)
