package onboard

import (
	"bytes"
	"context"
	"encoding/json"
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
	var modelRequest string
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servePromptSourceRevision(w, r) {
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+aiToken {
			t.Fatalf("model authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		modelRequest = string(body)
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, validPromptBody())
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
	prompt, drafted, err := buildSystemPrompt(context.Background(), opts, data, input, &logs)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}
	if !drafted || !strings.Contains(prompt, validPromptBody()) {
		t.Fatalf("prompt was not drafted:\n%s", prompt)
	}
	for _, want := range []string{"DISCOVERED PROW JOBS", "SOURCE 1: docs/", "kind markdown"} {
		if !strings.Contains(modelRequest, want) {
			t.Errorf("model request missing %q: %s", want, modelRequest)
		}
	}
	all := modelRequest + prompt + logs.String()
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
	prompt, drafted, err := buildSystemPrompt(context.Background(), opts, data, promptDraftInput{
		ProjectName: data.Name,
		SourceRepo:  Repo{Owner: "example", Name: "project", FullName: "example/project"},
	}, &logs)
	if err != nil {
		t.Fatal(err)
	}
	if drafted || modelCalls != 0 || !strings.Contains(prompt, "## Unresolved details") {
		t.Fatalf("drafted=%v modelCalls=%d prompt=%s", drafted, modelCalls, prompt)
	}
	if !strings.Contains(logs.String(), "no meaningful source evidence") {
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
	prompt, drafted, err := buildSystemPrompt(context.Background(), opts, data, promptDraftInput{
		ProjectName: data.Name,
		SourceRepo:  Repo{Owner: "example", Name: "project", FullName: "example/project"},
	}, &logs)
	if err != nil {
		t.Fatal(err)
	}
	if drafted || modelCalls != 0 || !strings.Contains(prompt, "## Unresolved details") {
		t.Fatalf("drafted=%v modelCalls=%d", drafted, modelCalls)
	}
	if strings.Contains(logs.String(), "private-source-content") {
		t.Fatalf("private content leaked into logs: %s", logs.String())
	}
}

func TestBuildSystemPromptModelFailureFallsBack(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if servePromptSourceRevision(w, r) {
			return
		}
		http.Error(w, "provider detail", http.StatusInternalServerError)
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
			_, _ = w.Write([]byte(`{"tree":[{"path":"README.md","type":"blob","size":20}]}`))
		case "/example/project/" + promptSourceTestSHA + "/README.md":
			_, _ = w.Write([]byte("artifact docs"))
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
	prompt, drafted, err := buildSystemPrompt(context.Background(), opts, data, promptDraftInput{
		ProjectName: data.Name,
		SourceRepo:  Repo{Owner: "example", Name: "project", FullName: "example/project"},
	}, &logs)
	if err != nil {
		t.Fatal(err)
	}
	if drafted || !strings.Contains(prompt, "## Unresolved details") || !strings.Contains(logs.String(), "prompt generation failed") {
		t.Fatalf("drafted=%v logs=%s", drafted, logs.String())
	}
}
