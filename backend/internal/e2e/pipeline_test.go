package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aitest"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetcher"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// fixtureBucket is the committed artifact tree the local storage provider reads.
func fixtureBucket(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "bucket"))
	if err != nil {
		t.Fatalf("abs bucket: %v", err)
	}
	return abs
}

// writeProject creates a temp project dir with a project.yaml pointing the local
// storage provider at the fixture bucket, plus the prompt, and returns the dir.
// extraAI, when non-empty, is appended under the ai: block.
func writeProject(t *testing.T, extraAI string) string {
	t.Helper()
	dir := t.TempDir()
	yaml := `id: example
name: Example E2E
short_name: EXAMPLE
storage:
  provider: local
  base: ` + fixtureBucket(t) + `
discovery:
  source: bucket
branding:
  title: Example Dashboard
  base_path: /example
  site_url: https://example.github.io/example
  source_repo:
    owner: example-org
    name: example-repo
categories:
  - match: e2e
    id: e2e
    label: E2E
`
	if extraAI != "" {
		yaml += "ai:\n" + extraAI
	}
	if err := os.WriteFile(filepath.Join(dir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir prompts: %v", err)
	}
	src, err := os.ReadFile(filepath.Join("testdata", "prompts", "system.md"))
	if err != nil {
		t.Fatalf("read prompt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "system.md"), src, 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	return dir
}

// runPipeline runs fetcher.Run against the fixtures and returns the output dir.
func runPipeline(t *testing.T, projectDir string, enableAI bool) string {
	t.Helper()
	// Clear env that fetcher.Run reads, so a developer's environment can't make
	// the pipeline reach email, GitHub, or a real AI endpoint.
	for _, k := range []string{"EMAIL_SMTP_PASSWORD", "ISSUE_TOKEN", "FIX_TOKEN", "GITHUB_TOKEN", "AI_ENDPOINT", "AI_MODEL"} {
		t.Setenv(k, "")
	}
	outDir := t.TempDir()
	err := fetcher.Run(context.Background(), fetcher.Options{
		ProjectDir:   projectDir,
		OutDir:       outDir,
		BuildsPerJob: 5,
		Workers:      2,
		Timeout:      2 * time.Minute,
		EnableAI:     enableAI,
	})
	if err != nil {
		t.Fatalf("fetcher.Run: %v", err)
	}
	return outDir
}

func loadJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// TestPipeline_NoAI runs the full discover -> fetch -> junit -> aggregate ->
// output pipeline against local fixtures with AI off, asserting the published
// JSON reflects the fixture's one failing and one passing build.
func TestPipeline_NoAI(t *testing.T) {
	out := runPipeline(t, writeProject(t, ""), false)

	var dash models.Dashboard
	loadJSON(t, filepath.Join(out, "dashboard.json"), &dash)
	if len(dash.Jobs) != 1 {
		t.Fatalf("dashboard jobs = %d, want 1", len(dash.Jobs))
	}
	job := dash.Jobs[0]
	if job.Name != "periodic-example-e2e-main" {
		t.Errorf("job name = %q", job.Name)
	}
	if job.Category != "e2e" {
		t.Errorf("category = %q, want e2e", job.Category)
	}

	// The per-job detail file carries both builds and the failing test case.
	matches, _ := filepath.Glob(filepath.Join(out, "jobs", "*.json"))
	if len(matches) != 1 {
		t.Fatalf("job files = %v, want 1", matches)
	}
	var detail models.JobDetail
	loadJSON(t, matches[0], &detail)
	if len(detail.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(detail.Runs))
	}
	var failed, passed int
	var failingTest string
	for _, r := range detail.Runs {
		for _, tc := range r.TestCases {
			switch tc.Status {
			case "failed":
				failed++
				failingTest = tc.Name
			case "passed":
				passed++
			}
		}
	}
	if failed != 1 || passed != 1 {
		t.Errorf("test cases: failed=%d passed=%d, want 1/1", failed, passed)
	}
	if !strings.Contains(failingTest, "control plane nodes") {
		t.Errorf("failing test name = %q", failingTest)
	}

	// Output index files exist.
	for _, f := range []string{"flakiness.json", "search-index.json", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing output %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "notification_state.json")); !os.IsNotExist(err) {
		t.Errorf("disabled notifications wrote state: %v", err)
	}
}

// TestPipeline_WithAI runs the full pipeline with AI enabled, driving the
// agentic loop with a scripted model that reads the fixture build log and
// returns a grounded analysis. It asserts the analysis is published and that
// the tool call actually exercised the local storage backend.
func TestPipeline_WithAI(t *testing.T) {
	script := aitest.NewScriptServer(t)
	// Iters 0-1: the model reads the build log twice (exercises the local
	// backend and clears the min_tool_calls floor); iter 2: it returns the
	// final analysis JSON with a concrete remediation.
	script.PushToolCall("c1", "read_artifact", map[string]any{"path": "build-log.txt"})
	script.PushToolCall("c2", "tail_artifact", map[string]any{"path": "build-log.txt"})
	script.PushFinal(`{"summary":"Control plane provisioning timed out","is_transient":false,` +
		`"root_cause":"Only 2 of 3 control plane machines registered before the 600s timeout",` +
		`"severity":"High","suggested_fix":"Raise the control-plane bootstrap timeout above 600s so all three machines have time to register",` +
		`"relevant_files":["build-log.txt"]}`)
	// The semantic judge reviews the accepted draft; return no objections so it
	// publishes as-is.
	script.PushFinal(`{"objections":[]}`)

	t.Setenv("AI_TOKEN", "test-token")
	aiBlock := "  endpoint: \"" + script.URL + "\"\n  model: \"script-model\"\n  tools: [filesystem]\n"
	out := runPipeline(t, writeProject(t, aiBlock), true)

	matches, _ := filepath.Glob(filepath.Join(out, "jobs", "*.json"))
	if len(matches) != 1 {
		t.Fatalf("job files = %v, want 1", matches)
	}
	var detail models.JobDetail
	loadJSON(t, matches[0], &detail)

	var analyzed int
	for _, r := range detail.Runs {
		for _, tc := range r.TestCases {
			if tc.Status != "failed" {
				continue
			}
			if tc.AISummary == nil || tc.AIAnalysis == nil {
				t.Fatalf("failed test %q missing AI summary/analysis", tc.Name)
			}
			analyzed++
			if !strings.Contains(tc.AIAnalysis.RootCause, "control plane") {
				t.Errorf("root cause = %q", tc.AIAnalysis.RootCause)
			}
			if tc.AIAnalysis.Severity != "High" {
				t.Errorf("severity = %q, want High", tc.AIAnalysis.Severity)
			}
			if tc.AIAnalysis.ToolCalls < 1 {
				t.Errorf("tool calls = %d, want >=1 (local backend exercised)", tc.AIAnalysis.ToolCalls)
			}
			if tc.AIAnalysis.GCSBytes < 1 {
				t.Errorf("gcs bytes = %d, want >=1 (the build-log read must have fetched bytes)", tc.AIAnalysis.GCSBytes)
			}
		}
	}
	if analyzed != 1 {
		t.Errorf("analyzed failures = %d, want 1", analyzed)
	}
	if script.ChatCalls() < 2 {
		t.Errorf("chat calls = %d, want >=2 (tool turn + final)", script.ChatCalls())
	}
}
