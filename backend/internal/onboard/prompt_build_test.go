package onboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildSystemPromptUsesEvidenceWithoutLeakingTokens(t *testing.T) {
	const aiToken = "fixture-ai-secret"
	const githubToken = "fixture-github-secret"
	var modelRequests []string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servePromptSourceRevision(w, r) {
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+aiToken {
			t.Fatalf("model authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		modelRequests = append(modelRequests, string(body))
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, validPromptEvidenceJSON())
	}))
	defer model.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servePromptSourceRevision(w, r) {
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+githubToken {
			t.Fatalf("source authorization = %q", got)
		}
		switch r.URL.Path {
		case "/repos/example/project":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/example/project/git/trees/" + promptSourceTestSHA:
			_ = json.NewEncoder(w).Encode(map[string]any{"tree": []map[string]any{{"path": "docs/" + aiToken + ".md", "type": "blob", "size": maxPromptSourceBytes + 100}}})
		case "/example/project/" + promptSourceTestSHA + "/docs/" + aiToken + ".md":
			prefix := strings.Repeat("x", maxPromptSourceBytes-len(aiToken)/2)
			fmt.Fprintf(w, "%s%s %s", prefix, aiToken, githubToken)
		default:
			http.NotFound(w, r)
		}
	}))
	defer source.Close()
	oldAPI, oldRaw := githubAPIBaseURL, githubRawBaseURL
	githubAPIBaseURL, githubRawBaseURL = source.URL, source.URL
	t.Cleanup(func() { githubAPIBaseURL, githubRawBaseURL = oldAPI, oldRaw })

	opts := testOpts()
	opts.AIToken, opts.GitHubToken = aiToken, githubToken
	opts.AIEndpoint, opts.AIModel = model.URL, "fixture-model"
	data := buildScaffoldData(opts, nil)
	input := promptDraftInput{
		ProjectName: data.Name,
		SourceRepo:  Repo{Owner: "example", Name: "project", FullName: "example/project"},
		Jobs: []promptJobSummary{{
			Name: "periodic-" + githubToken, Type: "periodic", ConfigFile: "config/jobs.yaml",
			Repo: "example/project", Branches: []string{"main"}, Dashboards: []string{"dashboard-a"},
		}},
	}
	var logs bytes.Buffer
	prompt, result, err := buildSystemPrompt(context.Background(), opts, data, input, &logs, &logs)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}
	if result.Status != promptStatusAPIDraft || !strings.Contains(prompt, "## Architecture") || !strings.Contains(prompt, "## Unresolved details") {
		t.Fatalf("prompt was not drafted:\n%s", prompt)
	}
	if len(modelRequests) != 2 {
		t.Fatalf("model requests = %d, want extraction and revision", len(modelRequests))
	}
	for _, want := range []string{"DISCOVERED PROW JOBS", "SOURCE 1: docs/", "kind markdown"} {
		if !strings.Contains(modelRequests[0], want) {
			t.Errorf("extraction request missing %q: %s", want, modelRequests[0])
		}
	}
	all := strings.Join(modelRequests, "") + prompt + logs.String()
	for _, token := range []string{aiToken, githubToken} {
		if strings.Contains(all, token) {
			t.Fatalf("credential %q leaked into prompt path", token)
		}
	}
}

func TestBuildSystemPromptEmptyCorpusSkipsModel(t *testing.T) {
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servePromptSourceRevision(w, r) {
			return
		}
		modelCalls++
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer model.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servePromptSourceRevision(w, r) {
			return
		}
		switch r.URL.Path {
		case "/repos/example/project":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/example/project/git/trees/" + promptSourceTestSHA:
			_, _ = w.Write([]byte(`{"tree":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer source.Close()
	oldAPI, oldRaw := githubAPIBaseURL, githubRawBaseURL
	githubAPIBaseURL, githubRawBaseURL = source.URL, source.URL
	t.Cleanup(func() { githubAPIBaseURL, githubRawBaseURL = oldAPI, oldRaw })

	opts := testOpts()
	opts.AIToken, opts.AIEndpoint, opts.AIModel = "fixture-token", model.URL, "fixture-model"
	data := buildScaffoldData(opts, nil)
	var logs bytes.Buffer
	prompt, result, err := buildSystemPrompt(context.Background(), opts, data, promptDraftInput{
		ProjectName: data.Name,
		SourceRepo:  Repo{Owner: "example", Name: "project", FullName: "example/project"},
	}, &logs, &logs)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != promptStatusFallback || modelCalls != 0 || !strings.Contains(prompt, "## Unresolved details") {
		t.Fatalf("result=%+v modelCalls=%d prompt=%s", result, modelCalls, prompt)
	}
	if !strings.Contains(logs.String(), "no usable bounded source excerpts were found") {
		t.Fatalf("missing fallback log: %s", logs.String())
	}
}

func TestBuildSystemPromptSourceFailureFallsBackSafely(t *testing.T) {
	modelCalls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { modelCalls++ }))
	defer model.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servePromptSourceRevision(w, r) {
			return
		}
		http.Error(w, "private-source-content", http.StatusInternalServerError)
	}))
	defer source.Close()
	oldAPI := githubAPIBaseURL
	githubAPIBaseURL = source.URL
	t.Cleanup(func() { githubAPIBaseURL = oldAPI })

	opts := testOpts()
	opts.AIToken, opts.AIEndpoint, opts.AIModel = "fixture-token", model.URL, "fixture-model"
	data := buildScaffoldData(opts, nil)
	var logs bytes.Buffer
	prompt, result, err := buildSystemPrompt(context.Background(), opts, data, promptDraftInput{
		ProjectName: data.Name,
		SourceRepo:  Repo{Owner: "example", Name: "project", FullName: "example/project"},
	}, &logs, &logs)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != promptStatusFallback || modelCalls != 0 || !strings.Contains(prompt, "## Unresolved details") {
		t.Fatalf("result=%+v modelCalls=%d", result, modelCalls)
	}
	if strings.Contains(logs.String(), "private-source-content") {
		t.Fatalf("private content leaked into logs: %s", logs.String())
	}
}

func TestBuildSystemPromptModelFailureFallsBack(t *testing.T) {
	const aiToken = "fixture-ai-secret"
	const sourceContents = "artifact docs with private source detail"
	const providerBody = "private provider detail with raw model output"
	const privateModel = "private-model-name"
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.Header().Set("X-Request-Id", "request-"+aiToken)
		http.Error(w, providerBody, http.StatusServiceUnavailable)
	}))
	defer model.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servePromptSourceRevision(w, r) {
			return
		}
		switch r.URL.Path {
		case "/repos/example/project":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/example/project/git/trees/" + promptSourceTestSHA:
			_, _ = w.Write([]byte(`{"tree":[{"path":"README.md","type":"blob","size":60}]}`))
		case "/example/project/" + promptSourceTestSHA + "/README.md":
			_, _ = w.Write([]byte(sourceContents))
		default:
			http.NotFound(w, r)
		}
	}))
	defer source.Close()
	oldAPI, oldRaw := githubAPIBaseURL, githubRawBaseURL
	githubAPIBaseURL, githubRawBaseURL = source.URL, source.URL
	t.Cleanup(func() { githubAPIBaseURL, githubRawBaseURL = oldAPI, oldRaw })

	opts := testOpts()
	opts.AIToken = aiToken
	opts.AIEndpoint = model.URL + "/private/tenant/chat/completions?api-version=secret#fragment"
	opts.AIModel = privateModel
	opts.PromptDebug = true
	data := buildScaffoldData(opts, nil)
	var out, errOut bytes.Buffer
	prompt, result, err := buildSystemPrompt(context.Background(), opts, data, promptDraftInput{
		ProjectName: data.Name,
		SourceRepo:  Repo{Owner: "example", Name: "project", FullName: "example/project"},
	}, &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != promptStatusFallback || !strings.Contains(prompt, "## Unresolved details") {
		t.Fatalf("result=%+v out=%s err=%s", result, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "[warn]") || !strings.Contains(errOut.String(), "prompts/system.md generation failed") {
		t.Fatalf("warnings were not isolated to Terminal.Err: out=%s err=%s", out.String(), errOut.String())
	}
	for _, want := range []string{"endpoint_host=127.0.0.1", "model_fingerprint=sha256:", "structured_transport_attempt=json-schema", "http_status=503", "retry_after=12"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("debug output missing %q: %s", want, errOut.String())
		}
	}
	all := out.String() + errOut.String()
	for _, prohibited := range []string{aiToken, sourceContents, providerBody, "raw model output", privateModel, "/private/tenant", "api-version=secret", "fragment"} {
		if strings.Contains(all, prohibited) {
			t.Fatalf("diagnostics exposed %q: %s", prohibited, all)
		}
	}
}

func TestBuildSystemPromptCancellationPropagates(t *testing.T) {
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	}))
	defer source.Close()
	oldAPI := githubAPIBaseURL
	githubAPIBaseURL = source.URL
	t.Cleanup(func() { githubAPIBaseURL = oldAPI })

	opts := testOpts()
	opts.AIToken, opts.AIEndpoint, opts.AIModel = "fixture-token", "https://provider.example/chat/completions", "fixture-model"
	data := buildScaffoldData(opts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := buildSystemPrompt(ctx, opts, data, promptDraftInput{
		ProjectName: data.Name,
		SourceRepo:  Repo{Owner: "example", Name: "project", FullName: "example/project"},
	}, io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
