package orka

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

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type fakeSourceReader struct {
	files map[string]string
	err   error
}

type errorSourceResultAPI struct {
	err error
}

func (r errorSourceResultAPI) Result(context.Context, string, string) (string, bool, error) {
	return "", false, r.err
}

func (f fakeSourceReader) ReadFile(_ context.Context, _ sourceinvestigation.Repository, file string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	content, ok := f.files[file]
	if !ok {
		return "", errors.New("not found")
	}
	return content, nil
}

func sourceRequest() sourceinvestigation.Request {
	return sourceinvestigation.Request{
		ID: "source-1", Timeout: time.Minute,
		Subject: sourceinvestigation.Subject{
			SessionID: "session-1", ChatRequestID: "chat-1",
			Repository: sourceinvestigation.Repository{
				Owner: "example", Name: "repo", Revision: "0123456789abcdef0123456789abcdef01234567",
			},
			JobID: "periodic-demo", Build: models.BuildInfo{BuildID: "123"},
			TestCase: models.TestCase{Name: "TestCluster", FailureMessage: "timed out", AIAnalysis: &models.AIAnalysis{
				GeneratedAt: "2026-07-24T12:00:00Z", RootCause: "retry loop stalled", SuggestedFix: "bound retries",
			}},
			Question: "Could the retry loop be responsible?", Answer: "The artifacts suggest it is.",
			AnalysisGeneratedAt: "2026-07-24T12:00:00Z",
		},
	}
}

func sourceOuterResult(t *testing.T, summary string) StructuredResult {
	t.Helper()
	return StructuredResult{
		Version: 1, Summary: summary,
		BaseSHA: sourceRequest().Subject.Repository.Revision,
	}
}

func TestSourceInvestigationTaskNameUsesSafeDigest(t *testing.T) {
	request := sourceRequest()
	var previous string
	for _, version := range []string{"v1+guard@prod", strings.Repeat("x", 400)} {
		name := SourceInvestigationTaskName(request, SourceInvestigationOptions{Version: version})
		if problems := validation.IsDNS1123Subdomain(name); len(problems) != 0 {
			t.Errorf("version %q produced invalid Task name %q: %v", version, name, problems)
		}
		if len(name) != len("source-")+16 {
			t.Errorf("Task name %q length = %d", name, len(name))
		}
		if name == previous {
			t.Fatalf("different versions produced the same Task name %q", name)
		}
		previous = name
	}
}

func TestValidateSourceInvestigationAPI(t *testing.T) {
	for _, raw := range []string{"http://orka:8080", "https://orka.example/api"} {
		if err := validateSourceInvestigationAPI(raw); err != nil {
			t.Errorf("valid API %q: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"orka:8080", "ftp://orka.example", "https://user:pass@orka.example",
		"https://orka.example?token=secret", "https://orka.example#fragment",
	} {
		if err := validateSourceInvestigationAPI(raw); err == nil {
			t.Errorf("invalid API %q was accepted", raw)
		}
	}
}

func TestNewSourceInvestigatorFromEnvRejectsInvalidAPI(t *testing.T) {
	_, err := NewSourceInvestigatorFromEnv(SourceInvestigationFromEnvConfig{AgentRef: "reader", API: "orka:8080"})
	if err == nil || !strings.Contains(err.Error(), "absolute http or https URL") {
		t.Fatalf("NewSourceInvestigatorFromEnv error = %v", err)
	}
}

func TestSourceInvestigatorPreservesPollingContextErrors(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error()+"/phase", func(t *testing.T) {
			runner := &SourceInvestigator{
				kube: &fakeTaskAPI{phaseErr: fmt.Errorf("kubernetes request: %w", cause)},
				opts: SourceInvestigationOptions{PollEvery: time.Millisecond},
			}
			_, err := runner.waitSourceTerminal(t.Context(), "source-task")
			if !errors.Is(err, cause) || errors.Is(err, sourceinvestigation.ErrUnavailable) {
				t.Fatalf("waitSourceTerminal error = %v", err)
			}
		})
		t.Run(cause.Error()+"/result", func(t *testing.T) {
			runner := &SourceInvestigator{
				results: errorSourceResultAPI{err: fmt.Errorf("result request: %w", cause)},
				opts:    SourceInvestigationOptions{PollEvery: time.Millisecond},
			}
			_, err := runner.waitSourceResult(t.Context(), "source-task")
			if !errors.Is(err, cause) || errors.Is(err, sourceinvestigation.ErrUnavailable) {
				t.Fatalf("waitSourceResult error = %v", err)
			}
		})
	}
}

func TestSourceInvestigatorHappyPath(t *testing.T) {
	inner, err := json.Marshal(map[string]any{
		"version": 1, "state": "actionable_code_change",
		"target":     map[string]any{"intent": "modify_symbol", "path": "pkg/retry.go", "symbol": "retry"},
		"finding":    "The loop retries the same terminal error.",
		"confidence": "high", "relationship": "supports",
		"direction": "Stop retrying after the terminal error.",
		"citations": []map[string]any{{
			"path": "pkg/retry.go", "line_start": 2, "line_end": 3,
			"quote": "if terminal(err)",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	results, done := resultServer(t, sourceOuterResult(t, string(inner)))
	defer done()
	kube := &fakeTaskAPI{phases: []string{"Running", "Succeeded"}}
	runner := &SourceInvestigator{
		kube: kube, results: results,
		reader: fakeSourceReader{files: map[string]string{"pkg/retry.go": "func retry(err error) {\n\tif terminal(err) {\n\t\treturn\n\t}\n}\n"}},
		opts:   SourceInvestigationOptions{Namespace: "orka-system", AgentRef: "source-reader", MaxTurns: 20, PollEvery: time.Millisecond},
	}

	result, err := runner.Investigate(t.Context(), sourceRequest())
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(result.Citations) != 1 || !result.Citations[0].Verified {
		t.Fatalf("citations = %+v", result.Citations)
	}
	if kube.deleteCalls < 2 || !kube.deleted {
		t.Fatalf("successful Task cleanup calls=%d deleted=%v", kube.deleteCalls, kube.deleted)
	}
	taskSpec := kube.applied["spec"].(map[string]any)
	agentRuntime := taskSpec["agentRuntime"].(map[string]any)
	workspace := agentRuntime["workspace"].(map[string]any)
	if workspace["ref"] != sourceRequest().Subject.Repository.Revision {
		t.Fatalf("workspace ref = %v", workspace["ref"])
	}
	if agentRuntime["allowBash"] != false {
		t.Fatalf("allowBash = %v", agentRuntime["allowBash"])
	}
	metadata := kube.applied["metadata"].(map[string]any)
	annotations := metadata["annotations"].(map[string]any)
	if annotations[orkaWorkspaceInitAnnotation] != "true" {
		t.Fatalf("annotations = %+v", annotations)
	}
	if annotations[orkaAgentReadOnlyAnnotation] != "true" {
		t.Fatalf("read-only annotations = %+v", annotations)
	}
	if strings.Contains(taskSpec["prompt"].(string), "BOT_TOKEN") {
		t.Fatalf("prompt contains a write credential name")
	}
}

func TestSourceInvestigatorRejectsUnverifiedCitationAndWorkspaceChanges(t *testing.T) {
	inner := `{"version":1,"state":"actionable_code_change","target":{"intent":"modify_symbol","path":"pkg/retry.go","symbol":"retry"},"finding":"finding","confidence":"medium","relationship":"refines","direction":"inspect","citations":[{"path":"pkg/retry.go","line_start":1,"line_end":1,"quote":"missing"}]}`
	for _, tc := range []struct {
		name   string
		outer  StructuredResult
		reader SourceReader
	}{
		{name: "quote", outer: sourceOuterResult(t, inner), reader: fakeSourceReader{files: map[string]string{"pkg/retry.go": "different\n"}}},
		{name: "diff", outer: func() StructuredResult {
			r := sourceOuterResult(t, inner)
			r.Diff = "diff"
			r.Files = []string{"x"}
			return r
		}(), reader: fakeSourceReader{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results, done := resultServer(t, tc.outer)
			defer done()
			runner := &SourceInvestigator{
				kube: &fakeTaskAPI{phases: []string{"Succeeded"}}, results: results, reader: tc.reader,
				opts: SourceInvestigationOptions{AgentRef: "reader", PollEvery: time.Millisecond},
			}
			if _, err := runner.Investigate(t.Context(), sourceRequest()); !errors.Is(err, sourceinvestigation.ErrInvalidResult) {
				t.Fatalf("Investigate = %v", err)
			}
		})
	}
}

func TestSourceInvestigatorCleansUpAmbiguousApplyFailure(t *testing.T) {
	kube := &fakeTaskAPI{
		applyErr:   errors.New("request timed out"),
		phases:     []string{"Running"},
		deleteErrs: []error{errors.New("temporary delete failure")},
	}
	runner := &SourceInvestigator{
		kube: kube, results: &ResultClient{}, reader: fakeSourceReader{},
		opts: SourceInvestigationOptions{AgentRef: "reader", PollEvery: time.Millisecond},
	}
	_, err := runner.Investigate(t.Context(), sourceRequest())
	if !errors.Is(err, sourceinvestigation.ErrUnavailable) {
		t.Fatalf("Investigate = %v", err)
	}
	if kube.deleteCalls < 3 || !kube.deleted {
		t.Fatalf("cleanup calls=%d deleted=%v", kube.deleteCalls, kube.deleted)
	}
}

func TestSourceInvestigatorRecordsCleanupFailure(t *testing.T) {
	deleteErrs := make([]error, 10)
	for i := range deleteErrs {
		deleteErrs[i] = errors.New("delete denied")
	}
	kube := &fakeTaskAPI{applyErr: errors.New("request timed out"), phases: []string{"Running"}, deleteErrs: deleteErrs}
	runner := &SourceInvestigator{
		kube: kube, results: &ResultClient{}, reader: fakeSourceReader{},
		opts: SourceInvestigationOptions{AgentRef: "reader", PollEvery: time.Millisecond},
	}
	_, err := runner.Investigate(t.Context(), sourceRequest())
	if !errors.Is(err, sourceinvestigation.ErrUnavailable) || !strings.Contains(err.Error(), "cleaning up source Task") {
		t.Fatalf("Investigate = %v", err)
	}
	if kube.deleteCalls != 10 {
		t.Fatalf("cleanup calls = %d, want 10", kube.deleteCalls)
	}
}

func TestSourceInvestigatorRecordsVerifiedResultCleanupFailure(t *testing.T) {
	inner := `{"version":1,"state":"actionable_code_change","target":{"intent":"modify_symbol","path":"pkg/retry.go","symbol":"retry"},"finding":"finding","confidence":"medium","relationship":"refines","direction":"inspect","citations":[{"path":"pkg/retry.go","line_start":1,"line_end":1,"quote":"retry"}]}`
	results, done := resultServer(t, sourceOuterResult(t, inner))
	defer done()
	deleteErrs := make([]error, 10)
	for i := range deleteErrs {
		deleteErrs[i] = errors.New("delete denied")
	}
	kube := &fakeTaskAPI{phases: []string{"Running", "Succeeded"}, deleteErrs: deleteErrs}
	runner := &SourceInvestigator{
		kube: kube, results: results, reader: fakeSourceReader{files: map[string]string{"pkg/retry.go": "retry\n"}},
		opts: SourceInvestigationOptions{AgentRef: "reader", PollEvery: time.Millisecond},
	}
	result, err := runner.Investigate(t.Context(), sourceRequest())
	if !errors.Is(err, sourceinvestigation.ErrUnavailable) || !strings.Contains(err.Error(), "after verified result") {
		t.Fatalf("Investigate = %+v, %v", result, err)
	}
	if result.Finding != "" || kube.deleteCalls != 10 {
		t.Fatalf("result=%+v cleanup calls=%d", result, kube.deleteCalls)
	}
}

func TestSourceInvestigatorClassifiesTaskAndEnvelopeFailures(t *testing.T) {
	t.Run("terminal task", func(t *testing.T) {
		kube := &fakeTaskAPI{phases: []string{"Running", "Failed"}}
		runner := &SourceInvestigator{
			kube: kube, results: &ResultClient{}, reader: fakeSourceReader{},
			opts: SourceInvestigationOptions{AgentRef: "reader", PollEvery: time.Millisecond},
		}
		if _, err := runner.Investigate(t.Context(), sourceRequest()); !errors.Is(err, sourceinvestigation.ErrUnavailable) {
			t.Fatalf("Investigate = %v", err)
		}
		if kube.deleteCalls < 2 || !kube.deleted {
			t.Fatalf("terminal cleanup calls=%d deleted=%v", kube.deleteCalls, kube.deleted)
		}
	})

	t.Run("malformed envelope", func(t *testing.T) {
		results, done := rawResultServer(t, "{")
		defer done()
		runner := &SourceInvestigator{
			kube: &fakeTaskAPI{phases: []string{"Succeeded"}}, results: results, reader: fakeSourceReader{},
			opts: SourceInvestigationOptions{AgentRef: "reader", PollEvery: time.Millisecond},
		}
		if _, err := runner.Investigate(t.Context(), sourceRequest()); !errors.Is(err, sourceinvestigation.ErrInvalidResult) {
			t.Fatalf("Investigate = %v", err)
		}
	})
}

func TestParseSourceResultRejectsProse(t *testing.T) {
	_, err := parseSourceResult("result: " + `{"version":1}`)
	if !errors.Is(err, sourceinvestigation.ErrInvalidResult) {
		t.Fatalf("parseSourceResult = %v", err)
	}
}

func TestGitHubSourceReaderPinsPathAndStripsCrossHostAuthorization(t *testing.T) {
	var redirectedAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("line one\nline two\n"))
	}))
	defer target.Close()
	var requestedPath, initialAuth string
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		initialAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer front.Close()
	reader := NewGitHubSourceReader(front.URL, "read-token")
	repo := sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: "0123456789abcdef0123456789abcdef01234567"}
	content, err := reader.ReadFile(t.Context(), repo, "pkg/retry.go")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/example/repo/0123456789abcdef0123456789abcdef01234567/pkg/retry.go"
	if requestedPath != wantPath || content != "line one\nline two\n" {
		t.Fatalf("path=%q content=%q", requestedPath, content)
	}
	if initialAuth != "Bearer read-token" || redirectedAuth != "" {
		t.Fatalf("authorization initial=%q redirected=%q", initialAuth, redirectedAuth)
	}
	if _, err := reader.ReadFile(t.Context(), repo, ".."); err == nil {
		t.Fatal("ReadFile accepted the repository parent path")
	}
}

func TestBuildSourcePromptUsesTypedFailureSubject(t *testing.T) {
	request := sourceRequest()
	request.Subject.TestCase.Source = models.TestCaseSourceBuild
	request.Subject.TestCase.Name = "Prow job execution"
	prompt := buildSourcePrompt(request.Subject)
	if !strings.Contains(prompt, `"failure_subject":"Prow job execution"`) || !strings.Contains(prompt, `"source":"build"`) {
		t.Fatalf("build source prompt lacks typed subject: %s", prompt)
	}
	if strings.Contains(prompt, `"test_name"`) {
		t.Fatalf("build source prompt used test-only label: %s", prompt)
	}
}
