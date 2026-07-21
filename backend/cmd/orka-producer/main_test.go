package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestOrkaTaskIgnoredAIFields(t *testing.T) {
	retries := 3
	cfg := &project.Config{AI: &project.AI{
		Endpoint:    "https://example.test/v1/chat/completions",
		Model:       "model",
		Headers:     map[string]string{"X-Route": "value"},
		Concurrency: 4,
		Agentic: project.Agentic{
			MaxIters:       30,
			Timeout:        10 * time.Minute,
			MinToolCalls:   5,
			MinGCSBytes:    500000,
			SingleToolCall: true,
			Tools:          []string{"filesystem"},
			Critique:       project.AgenticCritique{MaxRetries: &retries},
		},
	}}

	got := orkaTaskIgnoredAIFields(cfg)
	want := []string{
		"ai.endpoint", "ai.model", "ai.headers", "ai.concurrency",
		"ai.max_iters", "ai.timeout", "ai.single_tool_call", "ai.critique.max_retries",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ignored fields = %v, want %v", got, want)
	}
	for _, shared := range []string{"ai.tools", "ai.min_tool_calls", "ai.min_gcs_bytes"} {
		if strings.Contains(strings.Join(got, ","), shared) {
			t.Fatalf("shared Orka field %q reported ignored: %v", shared, got)
		}
	}
}

func TestResolveToolsIncludesQualityTools(t *testing.T) {
	names, k8sEnabled := resolveTools([]string{"filesystem"})
	if k8sEnabled {
		t.Fatal("filesystem-only config enabled k8s tools")
	}
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
	}
	for _, want := range qualityTools {
		if !seen[want] {
			t.Fatalf("missing quality tool %q", want)
		}
	}
}

func TestResolveToolsKeepsQualityToolsForExplicitNames(t *testing.T) {
	names, _ := resolveTools([]string{"read-artifact"})
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
	}
	if !seen["read-artifact"] || !seen["submit-analysis"] || !seen["verify-timeline"] {
		t.Fatalf("resolved tools = %v, want explicit and mandatory quality tools", names)
	}
}

func TestBuildToolNameSeparatesConsumerScopes(t *testing.T) {
	projectA := orka.ProjectScopeID("a", "gcs", "bucket", "", "", "")
	projectB := orka.ProjectScopeID("b", "gcs", "bucket", "", "", "")
	scopeA := orka.BuildScopeID(projectA, "job", "1", "logs/job/1/")
	scopeB := orka.BuildScopeID(projectB, "job", "1", "logs/job/1/")
	nameA := buildToolName("read-artifact", orka.ToolScopeID(scopeA, "contract"))
	nameB := buildToolName("read-artifact", orka.ToolScopeID(scopeB, "contract"))
	if nameA == nameB {
		t.Fatalf("consumer-scoped Tool names collided: %q", nameA)
	}
}

func TestCloneSkillAwareToolsCarrySkillContract(t *testing.T) {
	for _, tool := range []string{"required-evidence", "submit-analysis"} {
		t.Run(tool, func(t *testing.T) {
			base := map[string]any{
				"metadata": map[string]any{"name": tool},
				"spec":     map[string]any{"http": map[string]any{"url": "http://artifact-tool/tool/" + tool}},
			}
			got := cloneToolForBuild(base, tool, "project", "scope", "logs/job/1/", "bucket", "orka-system", nil, "encoded-skills", "validation-key", 123, "artifact-tool-auth", "token")
			spec := got["spec"].(map[string]any)
			headers := spec["http"].(map[string]any)["headers"].(map[string]any)
			if headers[orka.ToolScopeHeader] != "scope" {
				t.Fatalf("scope header = %+v", headers)
			}
			metadata := got["metadata"].(map[string]any)
			labels := metadata["labels"].(map[string]any)
			if labels[orka.ProjectLabel] != "project" || labels[orka.BuildLabel] != "scope" {
				t.Fatalf("labels = %+v", labels)
			}
			annotations := metadata["annotations"].(map[string]any)
			if annotations["orka.ai/tool-alias"] != strings.ReplaceAll(tool, "-", "_") {
				t.Fatalf("tool alias = %+v", annotations)
			}
			if tool == "required-evidence" && annotations["orka.ai/cache-identical-calls"] != "true" {
				t.Fatalf("cache annotation = %+v", annotations)
			}
			if tool == "submit-analysis" {
				if _, found := annotations["orka.ai/cache-identical-calls"]; found {
					t.Fatalf("submission Tool must not be cached: %+v", annotations)
				}
			}
			if headers["X-Prow-AI-Skills"] != "encoded-skills" {
				t.Fatalf("headers = %+v", headers)
			}
			if tool == "submit-analysis" && headers[orka.ValidationKeyHeader] != "validation-key" {
				t.Fatalf("validation key header = %+v", headers)
			}
			if tool == "submit-analysis" && headers[orka.MinGCSBytesHeader] != "123" {
				t.Fatalf("minimum GCS byte header = %+v", headers)
			}
			auth := spec["http"].(map[string]any)["authSecretRef"].(map[string]any)
			if auth["name"] != "artifact-tool-auth" || auth["key"] != "token" {
				t.Fatalf("authSecretRef = %+v", auth)
			}
		})
	}
}

func TestQualityToolsIncludeDiffLastPassing(t *testing.T) {
	names, _ := resolveTools([]string{"filesystem"})
	for _, name := range names {
		if name == "diff-last-passing" {
			return
		}
	}
	t.Fatalf("resolved tools = %v, want diff-last-passing", names)
}

func TestLoadOrCreateValidationKeyReusesManifestKey(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreateValidationKey(dir)
	if err != nil || first == "" {
		t.Fatalf("first key = %q, err = %v", first, err)
	}
	if err := os.WriteFile(filepath.Join(dir, orka.AnalysisManifestFile), []byte(`{"validation_key":"persisted-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateValidationKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != "persisted-key" {
		t.Fatalf("reused key = %q, want persisted-key", second)
	}
}

func TestTaskToolNamesUseTaskSpecificSubmission(t *testing.T) {
	got := taskToolNames([]string{"read-artifact", "submit-analysis", "recurrence"}, "build-scope", "az-analysis-task")
	if got[0] != buildToolName("read-artifact", "build-scope") || got[2] != buildToolName("recurrence", "build-scope") {
		t.Fatalf("build-scoped tools = %v", got)
	}
	if got[1] != submissionToolName("az-analysis-task") {
		t.Fatalf("submission tool = %q, want task-specific name", got[1])
	}
}

type fakeTaskApplyClient struct {
	events     []string
	phases     map[string][]string
	phaseCalls map[string]int
	states     map[string]orka.TaskState
}

func (f *fakeTaskApplyClient) Apply(_ context.Context, gvr schema.GroupVersionResource, _ string, obj map[string]any) error {
	name := obj["metadata"].(map[string]any)["name"].(string)
	f.events = append(f.events, "apply:"+gvr.Resource+":"+name)
	return nil
}

func (f *fakeTaskApplyClient) TaskState(_ context.Context, _, name string) (orka.TaskState, error) {
	return f.states[name], nil
}

func (f *fakeTaskApplyClient) DeleteTask(_ context.Context, _ string, name, resourceVersion string) error {
	f.events = append(f.events, "delete:tasks:"+name+":"+resourceVersion)
	delete(f.states, name)
	return nil
}

func (f *fakeTaskApplyClient) TaskPhase(_ context.Context, _, name string) (string, error) {
	if f.phaseCalls == nil {
		f.phaseCalls = map[string]int{}
	}
	sequence := f.phases[name]
	if len(sequence) == 0 {
		return "", nil
	}
	index := f.phaseCalls[name]
	if index >= len(sequence) {
		index = len(sequence) - 1
	}
	phase := sequence[index]
	f.phaseCalls[name]++
	f.events = append(f.events, "phase:"+name+":"+phase)
	return phase, nil
}

func TestApplyObjectsUsesTaskWaves(t *testing.T) {
	client := &fakeTaskApplyClient{phases: map[string][]string{
		"task-1": {"Running", "Succeeded"},
		"task-2": {"Succeeded"},
	}}
	tools := []namedObj{testNamedObj("tool-1")}
	tasks := []namedObj{testNamedObj("task-1"), testNamedObj("task-2"), testNamedObj("task-3")}
	if err := applyObjects(context.Background(), client, "orka-system", tools, tasks, 2, time.Millisecond, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	requireEventBefore(t, client.events, "apply:tools:tool-1", "apply:tasks:task-1")
	requireEventBefore(t, client.events, "phase:task-1:Succeeded", "apply:tasks:task-3")
	if client.phaseCalls["task-3"] != 0 {
		t.Fatalf("final wave phase calls = %d, want 0", client.phaseCalls["task-3"])
	}
}

func TestApplyObjectsUnlimitedDoesNotWait(t *testing.T) {
	client := &fakeTaskApplyClient{phases: map[string][]string{"task-1": {"Running"}}}
	tasks := []namedObj{testNamedObj("task-1"), testNamedObj("task-2")}
	if err := applyObjects(context.Background(), client, "orka-system", nil, tasks, 0, time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	if len(client.phaseCalls) != 0 {
		t.Fatalf("phase calls = %v, want none", client.phaseCalls)
	}
}

func TestApplyObjectsStopsWhenWaveTimesOut(t *testing.T) {
	client := &fakeTaskApplyClient{phases: map[string][]string{"task-1": {"Running"}}}
	tasks := []namedObj{testNamedObj("task-1"), testNamedObj("task-2")}
	err := applyObjects(context.Background(), client, "orka-system", nil, tasks, 1, time.Millisecond, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "task-1") || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("error = %v, want task-1 timeout", err)
	}
	if eventIndex(client.events, "apply:tasks:task-2") >= 0 {
		t.Fatalf("events = %v, later wave was applied after timeout", client.events)
	}
}

func TestApplyObjectsRecreatesNonSuccessfulTaskAfterPlacementChange(t *testing.T) {
	oldExecution := map[string]any{"nodeSelector": map[string]any{"agentpool": "old"}}
	newExecution := map[string]any{"nodeSelector": map[string]any{"agentpool": "new"}}
	client := &fakeTaskApplyClient{states: map[string]orka.TaskState{
		"task-1": {Exists: true, Phase: "Failed", Execution: oldExecution, ResourceVersion: "1", UID: "uid-1"},
	}}
	task := testNamedObj("task-1")
	task.obj["spec"] = map[string]any{"execution": newExecution}
	if err := applyObjects(context.Background(), client, "orka-system", nil, []namedObj{task}, 1, time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	requireEventBefore(t, client.events, "delete:tasks:task-1:1", "apply:tasks:task-1")
}

func TestApplyObjectsReusesSuccessfulTaskAfterPlacementChange(t *testing.T) {
	client := &fakeTaskApplyClient{states: map[string]orka.TaskState{
		"task-1": {
			Exists: true, Phase: "Succeeded",
			Execution: map[string]any{"nodeSelector": map[string]any{"agentpool": "old"}},
		},
	}}
	task := testNamedObj("task-1")
	task.obj["spec"] = map[string]any{"execution": map[string]any{"nodeSelector": map[string]any{"agentpool": "new"}}}
	if err := applyObjects(context.Background(), client, "orka-system", nil, []namedObj{task}, 1, time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
	if eventIndex(client.events, "delete:tasks:task-1") >= 0 || eventIndex(client.events, "apply:tasks:task-1") >= 0 {
		t.Fatalf("events = %v, successful Task should be reused", client.events)
	}
}

func TestApplyObjectsValidatesWaveSettings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		max        int
		poll       time.Duration
		wave       time.Duration
		wantSubstr string
	}{
		{name: "negative max", max: -1, poll: time.Second, wave: time.Second, wantSubstr: "between 0 and 1000"},
		{name: "oversized max", max: 1001, poll: time.Second, wave: time.Second, wantSubstr: "between 0 and 1000"},
		{name: "zero poll", max: 1, wave: time.Second, wantSubstr: "task-poll"},
		{name: "zero timeout", max: 1, poll: time.Second, wantSubstr: "wave-timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := applyObjects(context.Background(), &fakeTaskApplyClient{}, "orka-system", nil, nil, tc.max, tc.poll, tc.wave)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %v, want %q", err, tc.wantSubstr)
			}
		})
	}
}

func testNamedObj(name string) namedObj {
	return namedObj{name: name, obj: map[string]any{"metadata": map[string]any{"name": name}}}
}

func requireEventBefore(t *testing.T, events []string, first, second string) {
	t.Helper()
	firstIndex := eventIndex(events, first)
	secondIndex := eventIndex(events, second)
	if firstIndex < 0 || secondIndex < 0 || firstIndex >= secondIndex {
		t.Fatalf("events = %v, want %q before %q", events, first, second)
	}
}

func eventIndex(events []string, want string) int {
	for i, event := range events {
		if event == want {
			return i
		}
	}
	return -1
}
