package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
	runtimepkg "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

type orkaPatternAnalyzer struct{}

func (orkaPatternAnalyzer) AnalyzePattern(_ context.Context, _, subject string, failures []ai.PatternFailure) (*models.PatternAnalysis, error) {
	builds := make([]string, 0, len(failures))
	for _, failure := range failures {
		builds = append(builds, failure.BuildID)
	}
	return &models.PatternAnalysis{
		Subject:         subject,
		GeneratedAt:     "2026-07-18T00:00:00Z",
		BuildsAnalyzed:  len(failures),
		Systemic:        true,
		Confidence:      "high",
		SharedRootCause: "the controller repeatedly writes stale configuration",
		SharedBuilds:    builds,
		SuggestedFix:    "serialize controller updates",
		Summary:         "The same controller update failed in three builds.",
		RelevantFiles:   []string{"config/controller.yaml"},
	}, nil
}

type orkaFixPRClient struct{}

func (orkaFixPRClient) OpenPR(context.Context, ghpr.Request) (string, error) {
	return "", fmt.Errorf("dry-run opened a PR")
}

func (orkaFixPRClient) SearchOpenPR(context.Context, string, string, string, string) (int, string, bool, error) {
	return 0, "", false, nil
}

func (orkaFixPRClient) ResolveBase(context.Context, string, string) (ghpr.Base, error) {
	return ghpr.Base{Branch: "main", HeadSHA: "base-sha", TreeSHA: "tree-sha"}, nil
}

type orkaFixAgent struct{}

func (orkaFixAgent) Generate(_ context.Context, spec runtimepkg.GenerateSpec) (runtimepkg.GenerateResult, error) {
	if spec.Repo.Ref != "base-sha" {
		return runtimepkg.GenerateResult{}, fmt.Errorf("agent ref = %q", spec.Repo.Ref)
	}
	return runtimepkg.GenerateResult{
		Files: map[string]string{"config/controller.yaml": "serialized: true\n"},
		Diff:  "diff --git a/config/controller.yaml b/config/controller.yaml\n+serialized: true\n",
	}, nil
}

func TestOrkaPipelineProducesFixPreview(t *testing.T) {
	backendDir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Dir(backendDir)
	projectDir := writeProject(t, "")
	if err := os.MkdirAll(filepath.Join(projectDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	skill := `id: stale-controller
triggers: ["(?i)stale.*configuration"]
required_evidence:
  - id: controller-config
    any_of: ['config/controller\.yaml']
procedure: Inspect the controller configuration and its update ordering.
`
	if err := os.WriteFile(filepath.Join(projectDir, "skills", "stale-controller.yaml"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	tasksDir := t.TempDir()
	toolsDir := t.TempDir()

	detail := models.JobDetail{Name: "periodic-controller", JobID: "periodic-controller"}
	for _, buildID := range []string{"103", "102", "101"} {
		detail.Runs = append(detail.Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: buildID, Result: "FAILURE", Passed: false},
			TestCases: []models.TestCase{{
				Name:            "should reconcile",
				Status:          "failed",
				FailureMessage:  "timed out waiting for controller update",
				FailureLocation: "test/e2e/controller.go:44",
			}},
		})
	}
	if err := output.WriteJobDetail(dataDir, detail); err != nil {
		t.Fatal(err)
	}
	if err := output.WriteDashboard(dataDir, models.Dashboard{Jobs: []models.JobSummary{{
		ProwJob: models.ProwJob{Name: detail.Name, JobID: detail.JobID},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := output.WriteFlakinessReport(dataDir, models.FlakinessReport{}); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	producer := filepath.Join(binDir, "orka-producer")
	ingestor := filepath.Join(binDir, "orka-ingestor")
	runCommand(t, backendDir, "go", "build", "-o", producer, "./cmd/orka-producer")
	runCommand(t, backendDir, "go", "build", "-o", ingestor, "./cmd/orka-ingestor")
	runCommand(t, backendDir, producer,
		"-data="+dataDir,
		"-project-dir="+projectDir,
		"-tool-manifests="+filepath.Join(repoDir, "experimental", "orka", "manifests"),
		"-tasks-out="+tasksDir,
		"-tools-out="+toolsDir,
	)

	manifest, err := orka.LoadAnalysisManifest(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	results := map[string]string{}
	for _, run := range detail.Runs {
		ref, err := manifest.TaskRef(detail.JobID, run, 0, run.TestCases[0])
		if err != nil {
			t.Fatal(err)
		}
		validated := orka.AnalysisValidation{
			Summary: "stale controller configuration", RootCause: "the controller wrote stale configuration",
			Severity: "High", SuggestedFix: "serialize the update", RelevantFiles: []string{"config/controller.yaml"},
		}
		result := map[string]any{
			"summary": validated.Summary, "root_cause": validated.RootCause, "severity": validated.Severity,
			"is_transient": false, "suggested_fix": validated.SuggestedFix, "relevant_files": validated.RelevantFiles,
			"gcs_bytes": 100, "validation_token": orka.AnalysisValidationToken(manifest.ValidationKey, ref.Name, validated, 100),
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		results[ref.Name] = string(data)
	}
	if matches, _ := filepath.Glob(filepath.Join(tasksDir, "*.yaml")); len(matches) != len(results) {
		t.Fatalf("produced tasks = %d, want %d", len(matches), len(results))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("namespace") != "orka-system" {
			t.Errorf("namespace = %q", r.URL.Query().Get("namespace"))
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
		name, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimSuffix(path, "/result"), "/events"))
		if strings.HasSuffix(r.URL.Path, "/events") {
			writeOrkaAcceptedEvents(w)
			return
		}
		result, ok := results[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": result})
	}))
	defer server.Close()

	runCommand(t, backendDir, ingestor,
		"-data="+dataDir,
		"-project-dir="+projectDir,
		"-api="+server.URL,
		"-namespace=orka-system",
		"-provider=copilot",
		"-model=claude-sonnet-4.6",
		"-version=v1",
		"-wait=0",
		"-pattern-wait=0",
	)

	previewFile := filepath.Join(dataDir, "fix_previews.json")
	stats, err := orka.FinalizePatternsAndRun(context.Background(), dataDir, orkaPatternAnalyzer{}, func(ctx context.Context) error {
		var report models.FlakinessReport
		loadJSON(t, filepath.Join(dataDir, "flakiness.json"), &report)
		if len(report.RecurringPatterns) != 1 || report.RecurringPatterns[0].ID == "" {
			t.Fatalf("recurring patterns = %+v", report.RecurringPatterns)
		}
		manager := fixpr.NewManager(orkaFixPRClient{}, filepath.Join(dataDir, "fix_pr_state.json"), fixpr.Options{
			SourceOwner: "example-org", SourceName: "example-repo", MinConfidence: "high",
			MaxFiles: 3, MaxNewPerRun: 1, DryRun: true, PreviewFile: previewFile,
			Agent: &fixpr.AgentConfig{Runtime: orkaFixAgent{}},
		})
		fixStats, err := manager.Reconcile(ctx, report.RecurringPatterns)
		if err != nil {
			return err
		}
		if fixStats.Previewed != 1 {
			t.Fatalf("fix stats = %+v", fixStats)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.PatternAnalyses != 1 || stats.RecurringPatterns != 1 {
		t.Fatalf("finalize stats = %+v", stats)
	}

	var previews []fixpr.Preview
	loadJSON(t, previewFile, &previews)
	if len(previews) != 1 || !strings.Contains(previews[0].Diff, "serialized: true") {
		t.Fatalf("fix previews = %+v", previews)
	}
}

func runCommand(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	blocked := map[string]bool{
		"AI_TOKEN": true, "AI_ENDPOINT": true, "AI_MODEL": true,
		"FIX_TOKEN": true, "ISSUE_TOKEN": true, "EMAIL_SMTP_PASSWORD": true,
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !blocked[name] {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func writeOrkaAcceptedEvents(w http.ResponseWriter) {
	base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	events := []map[string]any{
		{"seq": 1, "type": "TaskStarted", "createdAt": base},
		{"seq": 2, "type": "ToolCallStarted", "toolName": "read-artifact", "toolCallID": "call-1", "createdAt": base.Add(time.Second)},
		{"seq": 3, "type": "ToolCallStarted", "toolName": "recurrence", "toolCallID": "call-2", "createdAt": base.Add(2 * time.Second)},
		{"seq": 4, "type": "ToolCallStarted", "toolName": "required-evidence", "toolCallID": "call-3", "createdAt": base.Add(3 * time.Second)},
		{"seq": 5, "type": "ToolCallCompleted", "toolName": "required-evidence", "toolCallID": "call-3", "content": map[string]any{"resultLength": 120}, "createdAt": base.Add(4 * time.Second)},
		{"seq": 6, "type": "ToolCallStarted", "toolName": "validate-analysis", "toolCallID": "call-4", "createdAt": base.Add(5 * time.Second)},
		{"seq": 7, "type": "ToolCallCompleted", "toolName": "validate-analysis", "toolCallID": "call-4", "content": map[string]any{"resultLength": 80}, "createdAt": base.Add(6 * time.Second)},
		{"seq": 8, "type": "ModelRequestCompleted", "provider": "copilot", "model": "claude-sonnet-4.6", "inputTokens": 100, "outputTokens": 20, "content": map[string]any{"apiMode": "chat_completions", "responseID": "chatcmpl-e2e"}, "createdAt": base.Add(7 * time.Second)},
		{"seq": 9, "type": "TaskSucceeded", "createdAt": base.Add(8 * time.Second)},
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"latestSeq": 9, "events": events})
}
