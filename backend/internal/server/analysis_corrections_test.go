package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/corrections"
)

type fakeCorrectionRunner struct {
	preview      corrections.Preview
	confirmed    corrections.PublicCorrection
	revoked      corrections.PublicCorrection
	previewErr   error
	confirmErr   error
	revokeErr    error
	sessionID    string
	requestID    string
	owner        string
	token        string
	correctionID string
}

func (f *fakeCorrectionRunner) Preview(sessionID, requestID, owner string) (corrections.Preview, error) {
	f.sessionID, f.requestID, f.owner = sessionID, requestID, owner
	if f.previewErr != nil {
		return corrections.Preview{}, f.previewErr
	}
	return f.preview, nil
}

func (f *fakeCorrectionRunner) Confirm(token, owner string) (corrections.PublicCorrection, error) {
	f.token, f.owner = token, owner
	if f.confirmErr != nil {
		return corrections.PublicCorrection{}, f.confirmErr
	}
	return f.confirmed, nil
}

func (f *fakeCorrectionRunner) Revoke(id, owner string) (corrections.PublicCorrection, error) {
	f.correctionID, f.owner = id, owner
	if f.revokeErr != nil {
		return corrections.PublicCorrection{}, f.revokeErr
	}
	return f.revoked, nil
}

func TestHandlerAnalysisCorrectionFlow(t *testing.T) {
	runner := &fakeCorrectionRunner{
		preview:   corrections.Preview{Token: "preview-1", Proposed: analysischat.Revision{RootCause: "new cause"}},
		confirmed: corrections.PublicCorrection{ID: "correction-1", Status: corrections.StatusActive},
		revoked:   corrections.PublicCorrection{ID: "correction-1", Status: corrections.StatusRevoked},
	}
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisCorrections: runner,
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
	if body := readBody(t, capabilities); !strings.Contains(body, `"analysis_corrections":true`) {
		t.Fatalf("capabilities = %s", body)
	}

	request := func(path, body string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "ok")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	preview := request("/api/analysis-chat/sessions/session-1/requests/request-1/correction/preview", "")
	if preview.StatusCode != http.StatusOK || !strings.Contains(readBody(t, preview), `"token":"preview-1"`) {
		t.Fatalf("preview status=%d", preview.StatusCode)
	}
	if runner.sessionID != "session-1" || runner.requestID != "request-1" || runner.owner != "alice" {
		t.Fatalf("preview runner = %+v", runner)
	}

	confirmed := request("/api/analysis-corrections/confirm", `{"token":"preview-1"}`)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(readBody(t, confirmed), `"status":"active"`) {
		t.Fatalf("confirm status=%d", confirmed.StatusCode)
	}
	if runner.token != "preview-1" || runner.owner != "alice" {
		t.Fatalf("confirm runner = %+v", runner)
	}

	revoked := request("/api/analysis-corrections/correction-1/revoke", "")
	if revoked.StatusCode != http.StatusOK || !strings.Contains(readBody(t, revoked), `"status":"revoked"`) {
		t.Fatalf("revoke status=%d", revoked.StatusCode)
	}
	if runner.correctionID != "correction-1" || runner.owner != "alice" {
		t.Fatalf("revoke runner = %+v", runner)
	}
}

func TestHandlerAnalysisCorrectionsRejectCrossOrigin(t *testing.T) {
	handler, err := Handler(Options{
		DataDir: t.TempDir(), Capabilities: DefaultCapabilities(), Auth: fakeAuth{}, AuthMode: "dev",
		AnalysisCorrections: &fakeCorrectionRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/analysis-chat/sessions/session-1/requests/request-1/correction/preview",
		"/api/analysis-corrections/confirm",
		"/api/analysis-corrections/correction-1/revoke",
	} {
		req := httptest.NewRequest(http.MethodPost, "https://dashboard.example"+path, strings.NewReader(`{"token":"preview-1"}`))
		req.Header.Set("Authorization", "ok")
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("POST %s status=%d body=%q", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestWriteAnalysisCorrectionErrorMapping(t *testing.T) {
	for _, testCase := range []struct {
		err  error
		want int
	}{
		{corrections.ErrPreviewNotFound, http.StatusNotFound},
		{corrections.ErrCorrectionNotFound, http.StatusNotFound},
		{corrections.ErrPreviewExpired, http.StatusConflict},
		{corrections.ErrCorrectionState, http.StatusConflict},
		{analysischat.ErrAnalysisChanged, http.StatusConflict},
		{analysischat.ErrAnalysisNotFound, http.StatusNotFound},
		{analysischat.ErrInvalidRequest, http.StatusBadRequest},
		{corrections.ErrCorrectionLimit, http.StatusTooManyRequests},
		{errors.New("private provider body"), http.StatusInternalServerError},
	} {
		recorder := httptest.NewRecorder()
		writeAnalysisCorrectionError(recorder, "id", "alice", testCase.err)
		if recorder.Code != testCase.want {
			t.Fatalf("error %v status=%d want=%d", testCase.err, recorder.Code, testCase.want)
		}
		if strings.Contains(recorder.Body.String(), "private provider body") {
			t.Fatal("internal correction error leaked")
		}
	}
}

var _ AnalysisCorrectionRunner = (*fakeCorrectionRunner)(nil)
