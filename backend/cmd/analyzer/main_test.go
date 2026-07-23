package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aitest"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

type fakeAnalyzer struct {
	result  ai.FailureAnalysisResult
	err     error
	request ai.FailureAnalysisRequest
}

func (f *fakeAnalyzer) AnalyzeFailure(_ context.Context, _ *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	f.request = request
	return f.result, f.err
}

func analyzerTestRequest() ai.FailureAnalysisRequest {
	return ai.FailureAnalysisRequest{
		JobID:       "job",
		BuildPrefix: "logs/job/1/",
		Build:       models.BuildInfo{JobName: "job", BuildID: "1"},
		TestCase:    models.TestCase{Name: "Test A", Status: "failed", FailureMessage: "private failure"},
	}
}

func bundleValues(t *testing.T, request ai.FailureAnalysisRequest, projectDir string) map[string]string {
	t.Helper()
	return bundleValuesForContract(t, request, projectDir, analysisruntime.ContainerAnalyzerContractVersion)
}

func bundleValuesForContract(t *testing.T, request ai.FailureAnalysisRequest, projectDir, contractVersion string) map[string]string {
	t.Helper()
	if projectDir == "" {
		projectDir = writeAnalyzerProject(t, t.TempDir(), "https://model.invalid/v1/chat/completions")
	}
	data, digest, err := analysisruntime.BuildProjectBundle(projectDir, contractVersion, request)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		analysisruntime.ProjectBundleEnv:       string(data),
		analysisruntime.ProjectBundleDigestEnv: digest,
	}
}

func bundleEnv(t *testing.T, request ai.FailureAnalysisRequest) envGetter {
	t.Helper()
	values := bundleValues(t, request, "")
	return func(name string) string { return values[name] }
}

func TestAnalyzerProcessExitsNonzeroOnMalformedBundle(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestAnalyzerHelperProcess")
	cmd.Env = append(os.Environ(),
		"GO_WANT_ANALYZER_HELPER=1",
		analysisruntime.ProjectBundleEnv+"=not-json",
		analysisruntime.ProjectBundleDigestEnv+"="+strings.Repeat("0", 64),
	)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("analyzer process error = %v, want nonzero exit", err)
	}
}

func TestAnalyzerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ANALYZER_HELPER") != "1" {
		return
	}
	os.Args = []string{"analyzer"}
	main()
}

func TestRunWritesOnlyResultToStdout(t *testing.T) {
	request := analyzerTestRequest()
	fake := &fakeAnalyzer{result: ai.FailureAnalysisResult{
		Summary:  &models.AISummary{Summary: "summary"},
		Analysis: &models.AIAnalysis{RootCause: "cause", Severity: "Low"},
	}}
	var materializedProject string
	factory := func(_ context.Context, opts commandOptions, _ envGetter) (*analyzerRuntime, error) {
		materializedProject = opts.projectDir
		if _, err := os.Stat(filepath.Join(opts.projectDir, "project.yaml")); err != nil {
			return nil, err
		}
		return &analyzerRuntime{analyzer: fake, httpClient: http.DefaultClient, save: func() error { return nil }}, nil
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), nil, bundleEnv(t, request), &stdout, &stderr, factory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(materializedProject); !os.IsNotExist(err) {
		t.Fatalf("materialized project still exists: %v", err)
	}
	if strings.Contains(stdout.String(), "starting failure analysis") || !strings.Contains(stderr.String(), "starting failure analysis") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), analysisruntime.FailureAnalysisResultMarker) {
		t.Fatalf("stdout is not a framed result: %q", stdout.String())
	}
	result, err := analysisruntime.ParseFailureAnalysisResult("controller log\n" + stdout.String() + "trailing log\n")
	if err != nil {
		t.Fatalf("parse stdout result: %v\n%s", err, stdout.String())
	}
	if result.Summary == nil || result.Summary.Summary != "summary" {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(fake.request, request) {
		t.Fatalf("analyzer request = %+v, want %+v", fake.request, request)
	}
	if strings.Contains(stdout.String(), request.TestCase.FailureMessage) || strings.Contains(stderr.String(), request.TestCase.FailureMessage) {
		t.Fatal("complete failure request leaked to output")
	}
}

func TestAnalyzerRedactsProviderURLsFromDurableStderr(t *testing.T) {
	const privateURL = "https://private-model.example/v1/chat/completions?token=secret"
	fake := &fakeAnalyzer{err: errors.New("post " + privateURL + ": timeout")}
	factory := func(context.Context, commandOptions, envGetter) (*analyzerRuntime, error) {
		return &analyzerRuntime{
			analyzer: fake, httpClient: http.DefaultClient,
			save: func() error {
				log.Printf("service failure: post %s: timeout", privateURL)
				return nil
			},
		}, nil
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), nil, bundleEnv(t, analyzerTestRequest()), &stdout, &stderr, factory)
	if err == nil {
		t.Fatal("run succeeded")
	}
	if strings.Contains(stderr.String(), privateURL) || !strings.Contains(stderr.String(), "[redacted-url]") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	var rendered bytes.Buffer
	writeAnalyzerError(&rendered, err)
	if strings.Contains(rendered.String(), privateURL) || !strings.Contains(rendered.String(), "[redacted-url]") {
		t.Fatalf("rendered error = %q", rendered.String())
	}
}

func TestRunAnalyzeFailureErrorReturnsWithoutResult(t *testing.T) {
	fake := &fakeAnalyzer{err: errors.New("provider failed")}
	factory := func(context.Context, commandOptions, envGetter) (*analyzerRuntime, error) {
		return &analyzerRuntime{analyzer: fake, httpClient: http.DefaultClient, save: func() error { return nil }}, nil
	}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), nil, bundleEnv(t, analyzerTestRequest()), &stdout, &stderr, factory)
	if err == nil || !strings.Contains(err.Error(), "provider failed") {
		t.Fatalf("run error = %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunRejectsMalformedOrMismatchedBundle(t *testing.T) {
	factory := func(context.Context, commandOptions, envGetter) (*analyzerRuntime, error) {
		t.Fatal("factory called for invalid bundle")
		return nil, nil
	}
	valid := bundleValues(t, analyzerTestRequest(), "")
	future := bundleValuesForContract(t, analyzerTestRequest(), "", "dashboard-failure-analyzer-v4")
	for _, values := range []map[string]string{
		{analysisruntime.ProjectBundleEnv: "not json", analysisruntime.ProjectBundleDigestEnv: strings.Repeat("0", 64)},
		{analysisruntime.ProjectBundleEnv: valid[analysisruntime.ProjectBundleEnv]},
		{analysisruntime.ProjectBundleEnv: valid[analysisruntime.ProjectBundleEnv], analysisruntime.ProjectBundleDigestEnv: strings.Repeat("0", 64)},
		future,
	} {
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), nil, func(name string) string { return values[name] }, &stdout, &stderr, factory)
		if err == nil {
			t.Fatal("run succeeded for invalid bundle")
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty", stdout.String())
		}
	}
}

func TestRunWithScriptedModelEndpoint(t *testing.T) {
	script := aitest.NewScriptServer(t)
	script.PushToolCall("c1", "read_artifact", map[string]any{"path": "build-log.txt"})
	script.PushToolCall("c2", "tail_artifact", map[string]any{"path": "build-log.txt"})
	script.PushToolCall("c3", "read_artifact", map[string]any{"path": "artifacts/junit.xml"})
	script.PushFinal(`{"summary":"Control plane provisioning timed out","is_transient":false,"root_cause":"Only 2 of 3 control plane machines registered before the timeout","severity":"High","suggested_fix":"Raise the bootstrap timeout so all machines can register","relevant_files":[]}`)
	script.PushFinal(`{"objections":[]}`)

	root := t.TempDir()
	buildDir := filepath.Join(root, "logs", "job", "1")
	if err := os.MkdirAll(filepath.Join(buildDir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "build-log.txt"), []byte("timed out with only 2 of 3 control plane machines registered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "artifacts", "junit.xml"), []byte(`<testsuite><testcase name="Test A"><failure>timeout</failure></testcase></testsuite>`), 0o644); err != nil {
		t.Fatal(err)
	}
	projectDir := writeAnalyzerProject(t, root, script.URL)
	request := analyzerTestRequest()
	values := bundleValues(t, request, projectDir)
	values["AI_TOKEN"] = "script-token"
	values["AI_ENDPOINT"] = script.URL
	values["AI_MODEL"] = "script-model"
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"-data-dir", t.TempDir()}, func(name string) string { return values[name] }, &stdout, &stderr, loadRuntime)
	if err != nil {
		t.Fatalf("run error = %v\nstderr:\n%s", err, stderr.String())
	}
	result, err := analysisruntime.ParseFailureAnalysisResult(stdout.String())
	if err != nil {
		t.Fatal(err)
	}
	if result.Analysis == nil || result.Analysis.RootCause == "" || result.Analysis.ToolCalls < 3 {
		t.Fatalf("result = %+v", result)
	}
	if script.ChatCalls() < 5 {
		t.Fatalf("chat calls = %d, want at least 5", script.ChatCalls())
	}
	if strings.Contains(stdout.String(), "script-token") || strings.Contains(stderr.String(), "script-token") {
		t.Fatal("AI token leaked to output")
	}
}

func writeAnalyzerProject(t *testing.T, storageRoot, endpoint string) string {
	t.Helper()
	dir := t.TempDir()
	config := `id: analyzer-test
name: Analyzer Test
testgrid:
  dashboard: analyzer-test
storage:
  provider: local
  base: ` + strconvQuote(storageRoot) + `
branding:
  title: Analyzer Test
  base_path: /analyzer-test
  site_url: https://example.invalid/analyzer-test
  source_repo:
    owner: example
    name: project
ai:
  endpoint: ` + strconvQuote(endpoint) + `
  model: script-model
  tools: [filesystem]
  min_tool_calls: 2
`
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), []byte("Investigate the test failure from build artifacts.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func strconvQuote(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}
