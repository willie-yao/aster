package orka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/willie-yao/aster/backend/internal/runtime"
)

// fakeTaskAPI records the applied Task and returns scripted phases in order (the
// last entry repeats). applyErr, when set, fails Apply.
type fakeTaskAPI struct {
	applied     map[string]any
	applyErr    error
	phases      []string
	phaseErr    error
	calls       int
	deleted     bool
	deleteCalls int
	deleteErrs  []error
	uid         string
	annotations map[string]string
	preexisting bool
	attempts    int
}

func (f *fakeTaskAPI) Apply(_ context.Context, gvr schema.GroupVersionResource, _ string, obj map[string]any) error {
	_ = gvr
	if f.deleted && len(f.phases) > 1 {
		f.calls++
	}
	f.applied = obj
	f.deleted = false
	return f.applyErr
}

func (f *fakeTaskAPI) Delete(context.Context, schema.GroupVersionResource, string, string) error {
	f.deleteCalls++
	if len(f.deleteErrs) > 0 {
		err := f.deleteErrs[0]
		f.deleteErrs = f.deleteErrs[1:]
		if err != nil {
			return err
		}
	}
	f.deleted = true
	return nil
}

func (f *fakeTaskAPI) TaskPhase(_ context.Context, _, name string) (string, error) {
	if f.deleted {
		return "", apierrors.NewNotFound(schema.GroupResource{Group: "core.orka.ai", Resource: "tasks"}, name)
	}
	if f.phaseErr != nil {
		return "", f.phaseErr
	}
	i := f.calls
	f.calls++
	if i >= len(f.phases) {
		i = len(f.phases) - 1
	}
	if len(f.phases) == 0 {
		return "Succeeded", nil
	}
	return f.phases[i], nil
}

func (f *fakeTaskAPI) TaskState(_ context.Context, _, _ string) (TaskState, error) {
	if f.deleted {
		return TaskState{}, nil
	}
	exists := f.applied != nil || f.preexisting
	if !exists {
		return TaskState{}, nil
	}
	phase := "Running"
	if len(f.phases) > 0 {
		i := f.calls
		if i >= len(f.phases) {
			i = len(f.phases) - 1
		}
		phase = f.phases[i]
	}
	annotations := map[string]string{}
	for key, value := range f.annotations {
		annotations[key] = value
	}
	if metadata, _ := f.applied["metadata"].(map[string]any); metadata != nil {
		if values, _ := metadata["annotations"].(map[string]any); values != nil {
			for key, value := range values {
				annotations[key], _ = value.(string)
			}
		}
	}
	uid := f.uid
	if uid == "" {
		uid = "uid-1"
	}
	return TaskState{Exists: true, Phase: phase, UID: uid, ResourceVersion: "1", Annotations: annotations, Attempts: f.attempts}, nil
}

func (f *fakeTaskAPI) DeleteTaskIfIdentity(_ context.Context, _, _ string, uid, _ string) (bool, error) {
	wantUID := f.uid
	if wantUID == "" {
		wantUID = "uid-1"
	}
	if uid != wantUID {
		return false, nil
	}
	f.deleteCalls++
	if len(f.deleteErrs) > 0 {
		err := f.deleteErrs[0]
		f.deleteErrs = f.deleteErrs[1:]
		if err != nil {
			return false, err
		}
	}
	f.deleted = true
	return true, nil
}

// resultServer returns an httptest server that serves a StructuredResult (as the
// {"result": "<json>"} envelope) for any task, plus a ResultClient for it.
func resultServer(t *testing.T, sr StructuredResult) (*ResultClient, func()) {
	t.Helper()
	if sr.Version == 0 {
		sr.Version = 1
	}
	inner, err := json.Marshal(sr)
	if err != nil {
		t.Fatal(err)
	}
	return rawResultServer(t, string(inner))
}

func rawResultServer(t *testing.T, raw string) (*ResultClient, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/result") {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"result": raw}) //nolint:errcheck
	}))
	return NewResultClient(srv.URL, ""), srv.Close
}

func spec() runtime.GenerateSpec {
	return runtime.GenerateSpec{
		Repo:        runtime.RepoRef{Owner: "o", Name: "n", Ref: "0123456789abcdef0123456789abcdef01234567"},
		Instruction: "fix the etcd disk type",
		MaxTurns:    30,
		AllowBash:   true,
		Timeout:     15 * time.Minute,
	}
}

// stubApply records the diff it was asked to reconstruct and returns canned
// files, so tests never touch git or the network.
func stubApply(files map[string]string, gotDiff *string) func(context.Context, runtime.RepoRef, string) (map[string]string, string, error) {
	return func(_ context.Context, _ runtime.RepoRef, diff string) (map[string]string, string, error) {
		if gotDiff != nil {
			*gotDiff = diff
		}
		return files, diff, nil
	}
}

func TestAgentRuntime_HappyPath(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Running", "Succeeded"}}
	results, done := resultServer(t, StructuredResult{
		Summary: "pin etcd disk to Premium_LRS",
		BaseSHA: "0123456789abcdef0123456789abcdef01234567",
		Diff:    "diff --git a/x b/x\n+Premium_LRS\n",
		Files:   []string{"x"},
	})
	defer done()

	var gotDiff string
	r := &AgentRuntime{
		kube: kube, results: results,
		opts:      AgentOptions{Namespace: "orka-system", AgentRef: "codex-fixer", PollEvery: time.Millisecond},
		applyDiff: stubApply(map[string]string{"x": "Premium_LRS\n"}, &gotDiff),
	}

	res, err := r.Generate(context.Background(), spec())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Files["x"] != "Premium_LRS\n" {
		t.Errorf("reconstructed files wrong: %v", res.Files)
	}
	if res.Output != "pin etcd disk to Premium_LRS" {
		t.Errorf("summary not carried: %q", res.Output)
	}
	if !strings.Contains(gotDiff, "Premium_LRS") {
		t.Errorf("diff not passed to applyDiff: %q", gotDiff)
	}
	if !res.Telemetry.TaskFinalized || !res.Telemetry.ResultAvailable || !res.Telemetry.FinalizationChecked || !res.Telemetry.FinalizationValid || !res.Telemetry.CleanupCompleted || res.Telemetry.UsageStatus != "unavailable_from_agent_runtime" {
		t.Fatalf("telemetry = %+v", res.Telemetry)
	}

	// The applied Task is generation-only: type agent, no pushBranch, pinned ref.
	taskSpec, _ := kube.applied["spec"].(map[string]any)
	if taskSpec["type"] != "agent" {
		t.Errorf("task type = %v, want agent", taskSpec["type"])
	}
	if taskSpec["prompt"] != "fix the etcd disk type" {
		t.Errorf("original instruction not preserved: %q", taskSpec["prompt"])
	}
	agentRef, _ := taskSpec["agentRef"].(map[string]any)
	if agentRef["name"] != "codex-fixer" || agentRef["namespace"] != "orka-system" {
		t.Errorf("agentRef is not pinned: %v", agentRef)
	}
	ar, _ := taskSpec["agentRuntime"].(map[string]any)
	ws, _ := ar["workspace"].(map[string]any)
	if ws["gitRepo"] != "https://github.com/o/n.git" || ws["ref"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("workspace is not pinned: %v", ws)
	}
	if _, hasPush := ws["pushBranch"]; hasPush {
		t.Errorf("generation-only Task must not set pushBranch: %v", ws)
	}
	if ar["allowBash"] != true || ar["maxTurns"] != int64(30) {
		t.Errorf("agentRuntime overrides not set: %v", ar)
	}
	if taskSpec["timeout"] != "15m0s" || taskSpec["priority"] != int64(500) {
		t.Errorf("Task execution bounds not pinned: %v", taskSpec)
	}
	retry, _ := taskSpec["retryPolicy"].(map[string]any)
	if retry["maxRetries"] != int64(0) {
		t.Errorf("retry policy not pinned: %v", retry)
	}
	metadata := kube.applied["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	annotations := metadata["annotations"].(map[string]any)
	if labels[ManagedByLabel] != FixerManagedByValue || labels[agentContractLabel] != fixContractLabelValue || annotations[fixContractAnnotation] != agentContractVersion {
		t.Errorf("Task identity metadata not pinned: labels=%v annotations=%v", labels, annotations)
	}
}

func TestAgentRuntime_NoDiffIsNotFixable(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Succeeded"}}
	results, done := resultServer(t, StructuredResult{Summary: "no code change needed", BaseSHA: "0123456789abcdef0123456789abcdef01234567", Diff: "", Files: []string{}})
	defer done()
	r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "codex-fixer", PollEvery: time.Millisecond},
		applyDiff: stubApply(nil, nil)}

	res, err := r.Generate(context.Background(), spec())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(res.Files) != 0 {
		t.Errorf("empty diff must yield no files, got %v", res.Files)
	}
	if res.Output != "no code change needed" {
		t.Errorf("summary should still surface: %q", res.Output)
	}
}

func TestAgentRuntimeRejectsNonJSONResult(t *testing.T) {
	valid := `{"version":1,"summary":"fix","baseSHA":"0123456789abcdef0123456789abcdef01234567","diff":"","files":[]}`
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "malformed", raw: `{"version":`},
		{name: "prose", raw: "Here is the result: " + valid},
		{name: "code fence", raw: "```json\n" + valid + "\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results, done := rawResultServer(t, tc.raw)
			defer done()
			r := &AgentRuntime{
				kube:    &fakeTaskAPI{phases: []string{"Succeeded"}},
				results: results,
				opts:    AgentOptions{AgentRef: "opencode-fixer", PollEvery: time.Millisecond},
			}
			if _, err := r.Generate(context.Background(), spec()); err == nil || !errors.Is(err, runtime.ErrMalformedResult) {
				t.Fatalf("error = %v, want malformed result failure", err)
			}
		})
	}
}

func TestAgentRuntime_TaskFailed(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Running", "Failed"}}
	results, done := resultServer(t, StructuredResult{})
	defer done()
	r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "codex-fixer", PollEvery: time.Millisecond},
		applyDiff: stubApply(nil, nil)}

	if _, err := r.Generate(context.Background(), spec()); err == nil || !strings.Contains(err.Error(), "ended Failed") {
		t.Errorf("expected a Failed-phase error, got %v", err)
	}
}

func TestAgentRuntime_TaskCancelled(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Running", "Cancelled"}}
	results, done := resultServer(t, StructuredResult{})
	defer done()
	r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "codex-fixer", PollEvery: time.Millisecond}, applyDiff: stubApply(nil, nil)}
	result, err := r.Generate(context.Background(), spec())
	if !errors.Is(err, runtime.ErrCancelled) || !result.Telemetry.TaskFinalized {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestAgentRuntime_ApplyUnavailable(t *testing.T) {
	kube := &fakeTaskAPI{applyErr: fmt.Errorf("no route to host")}
	results, done := resultServer(t, StructuredResult{})
	defer done()
	r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "codex-fixer", PollEvery: time.Millisecond},
		applyDiff: stubApply(nil, nil)}

	s := spec()
	s.ExecutionID = "request-apply-unknown"
	_, err := r.Generate(context.Background(), s)
	if err == nil || !isUnavailable(err) {
		t.Errorf("apply failure should surface ErrUnavailable, got %v", err)
	}
	if !kube.deleted {
		t.Fatal("accepted Task with a lost apply response was not cleaned")
	}
}

func TestAgentRuntime_ContextCancelledWhilePolling(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Running"}} // never terminal
	results, done := resultServer(t, StructuredResult{})
	defer done()
	r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "codex-fixer", PollEvery: time.Millisecond},
		applyDiff: stubApply(nil, nil)}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.Generate(ctx, spec()); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("expected a poll-timeout error, got %v", err)
	}
}

func TestAgentRuntime_ValidatesInput(t *testing.T) {
	r := &AgentRuntime{kube: &fakeTaskAPI{}, opts: AgentOptions{}}
	if _, err := r.Generate(context.Background(), runtime.GenerateSpec{Instruction: "x"}); err == nil {
		t.Error("missing repo should error")
	}
	if _, err := r.Generate(context.Background(), runtime.GenerateSpec{Repo: runtime.RepoRef{Owner: "o", Name: "n", Ref: "r"}}); err == nil {
		t.Error("empty instruction should error")
	}
}

func TestAgentRuntimeRejectsMutableRevision(t *testing.T) {
	r := &AgentRuntime{kube: &fakeTaskAPI{}, results: &delayedResultAPI{}, opts: AgentOptions{AgentRef: "agent"}}
	s := spec()
	s.Repo.Ref = "main"
	if _, err := r.Generate(context.Background(), s); err == nil || !strings.Contains(err.Error(), "commit SHA") {
		t.Fatalf("mutable revision error = %v", err)
	}
}

func TestAgentRuntimeRejectsUnsafeBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*runtime.GenerateSpec, *AgentOptions)
		want string
	}{
		{name: "turns", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.MaxTurns = 1001 }, want: "max turns"},
		{name: "timeout", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.Timeout = 31 * time.Minute }, want: "timeout"},
		{name: "retries", edit: func(_ *runtime.GenerateSpec, o *AgentOptions) { o.MaxRetries = 3 }, want: "retries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := spec()
			opts := AgentOptions{AgentRef: "agent"}
			tc.edit(&s, &opts)
			r := &AgentRuntime{kube: &fakeTaskAPI{}, results: &delayedResultAPI{}, opts: opts}
			if _, err := r.Generate(context.Background(), s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe bounds error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAgentTaskName_ContentAddressedAndStable(t *testing.T) {
	baseSpec := spec()
	opts := AgentOptions{AgentRef: "codex-fixer", Version: "v1"}
	a := AgentTaskName(baseSpec, opts)
	b := AgentTaskName(baseSpec, opts)
	if a != b {
		t.Errorf("same inputs must give the same name: %s vs %s", a, b)
	}
	changed := baseSpec
	changed.Instruction = "different"
	if c := AgentTaskName(changed, opts); c == a {
		t.Error("different instruction did not change the name")
	}
	opts.AgentRef = "other-agent"
	if c := AgentTaskName(baseSpec, opts); c == a {
		t.Error("different AgentRef did not change the name")
	}
	if !strings.HasPrefix(a, "fix-") {
		t.Errorf("name should be fix-prefixed: %s", a)
	}
	prompt := AgentTaskName(baseSpec, AgentOptions{AgentRef: "codex-fixer", Version: "v1", Purpose: AgentPurposePromptAuthor})
	if prompt == a || !strings.HasPrefix(prompt, "prompt-") {
		t.Fatalf("prompt purpose name = %q, fix name = %q", prompt, a)
	}
	withSkills := baseSpec
	withSkills.Skills = map[string]string{"alpha": "first", "beta": "second"}
	reordered := baseSpec
	reordered.Skills = map[string]string{"beta": "second", "alpha": "first"}
	if AgentTaskName(withSkills, opts) != AgentTaskName(reordered, opts) {
		t.Fatal("skill map ordering changed the Task name")
	}
	changedSkill := baseSpec
	changedSkill.Skills = map[string]string{"alpha": "changed", "beta": "second"}
	if AgentTaskName(withSkills, opts) == AgentTaskName(changedSkill, opts) {
		t.Fatal("skill content did not change the Task name")
	}
	candidateA := baseSpec
	candidateA.Skills = map[string]string{"alpha": "x", "beta": "y"}
	candidateB := baseSpec
	candidateB.Skills = map[string]string{"alpha": "x\x00beta\x00y"}
	if AgentTaskName(candidateA, opts) == AgentTaskName(candidateB, opts) {
		t.Fatal("length-delimited skill identities collided")
	}
}

func TestAgentRuntimeTransfersSkillsThroughPrompt(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Succeeded"}}
	results, done := resultServer(t, StructuredResult{
		BaseSHA: "0123456789abcdef0123456789abcdef01234567",
		Diff:    "diff --git a/prompts/system.md b/prompts/system.md\n+prompt\n",
		Files:   []string{"prompts/system.md"},
	})
	defer done()
	s := spec()
	s.AllowBash = false
	s.Skills = map[string]string{"system-prompt-generation": "---\nname: system-prompt-generation\n---\nWrite the prompt.\n"}
	r := &AgentRuntime{
		kube: kube, results: results,
		opts:      AgentOptions{AgentRef: "prompt-author", Purpose: AgentPurposePromptAuthor, PollEvery: time.Millisecond},
		applyDiff: stubApply(map[string]string{"prompts/system.md": "prompt\n"}, nil),
	}
	res, err := r.Generate(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files["prompts/system.md"] != "prompt\n" {
		t.Fatalf("result=%+v", res)
	}
	taskSpec := kube.applied["spec"].(map[string]any)
	prompt, _ := taskSpec["prompt"].(string)
	if !strings.Contains(prompt, "## Engine skill: system-prompt-generation") || !strings.Contains(prompt, "Write the prompt") || !strings.HasSuffix(prompt, s.Instruction) {
		t.Fatalf("Task prompt did not carry the engine skill and instruction: %q", prompt)
	}
	if _, ok := taskSpec["skills"]; ok {
		t.Fatalf("agent Task used unsupported dynamic skills: %+v", taskSpec)
	}
	metadata := kube.applied["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	annotations := metadata["annotations"].(map[string]any)
	if labels[ManagedByLabel] != PromptAuthorManagedByValue || labels[agentContractLabel] != promptContractLabelValue || annotations[promptContractAnnotation] != agentContractVersion {
		t.Fatalf("prompt Task metadata labels=%v annotations=%v", labels, annotations)
	}
}

func TestAgentRuntimeRejectsUnsupportedExecutionPolicyBeforeClusterWrite(t *testing.T) {
	tests := []struct {
		name string
		edit func(*runtime.GenerateSpec, *AgentOptions)
		want string
	}{
		{name: "clone URL", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.Repo.CloneURL = "https://mirror.invalid/repo.git" }, want: "clone URL"},
		{name: "instruction UTF-8", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.Instruction = string([]byte{0xff}) }, want: "instruction must be valid UTF-8"},
		{name: "model", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.Model = "model" }, want: "model is owned"},
		{name: "native model", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.NativeModel = "provider/model" }, want: "native model"},
		{name: "ambient auth", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.UseAmbientAuth = true }, want: "ambient"},
		{name: "endpoint", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.Endpoint = "https://model.invalid/v1" }, want: "endpoint"},
		{name: "token", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.Token = "secret" }, want: "token"},
		{name: "headers", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.ExtraHeaders = map[string]string{"x": "secret"} }, want: "headers"},
		{name: "network", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.NetworkDomains = []string{"model.invalid:443"} }, want: "network policy"},
		{name: "purpose", edit: func(_ *runtime.GenerateSpec, o *AgentOptions) { o.Purpose = "unknown" }, want: "purpose"},
		{name: "git secret", edit: func(_ *runtime.GenerateSpec, o *AgentOptions) { o.GitSecret = "Bad_Name" }, want: "git secret"},
		{name: "fix git secret", edit: func(_ *runtime.GenerateSpec, o *AgentOptions) { o.GitSecret = "source-read" }, want: "fix generation does not permit"},
		{name: "skill name", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.Skills = map[string]string{"Bad_Name": "content"} }, want: "skill name"},
		{name: "long skill name", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) {
			s.Skills = map[string]string{strings.Repeat("a", maxAgentSkillNameBytes+1): "content"}
		}, want: "skill name is"},
		{name: "empty skill", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) { s.Skills = map[string]string{"empty": " \n"} }, want: "empty"},
		{name: "skill UTF-8", edit: func(s *runtime.GenerateSpec, _ *AgentOptions) {
			s.Skills = map[string]string{"invalid": string([]byte{0xff})}
		}, want: "must be valid UTF-8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := spec()
			opts := AgentOptions{AgentRef: "agent"}
			tc.edit(&s, &opts)
			kube := &fakeTaskAPI{}
			r := &AgentRuntime{kube: kube, results: &delayedResultAPI{}, opts: opts}
			if _, err := r.Generate(context.Background(), s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if kube.applied != nil {
				t.Fatalf("invalid policy wrote a cluster resource: task=%v", kube.applied)
			}
		})
	}
}

func TestAgentRuntimeRejectsExpandedTaskBeforeClusterWrite(t *testing.T) {
	s := spec()
	s.Skills = map[string]string{"expanded": strings.Repeat("<", maxAgentSkillBytes)}
	kube := &fakeTaskAPI{}
	r := &AgentRuntime{
		kube: kube, results: &delayedResultAPI{},
		opts: AgentOptions{AgentRef: "agent", Purpose: AgentPurposePromptAuthor},
	}
	if _, err := r.Generate(context.Background(), s); err == nil || !strings.Contains(err.Error(), "encoded agent Task") {
		t.Fatalf("expanded Task error = %v", err)
	}
	if kube.applied != nil {
		t.Fatalf("oversized Task reached Kubernetes: %+v", kube.applied)
	}
}

func TestNormalizeAgentSkillsCountsRenderedFraming(t *testing.T) {
	content := strings.Repeat("x", maxAgentSkillBytes)
	_, err := normalizeAgentSkills(map[string]string{
		"alpha": content,
		"beta":  content,
		"gamma": content,
		"delta": content,
	})
	if err == nil || !strings.Contains(err.Error(), "skill contents") {
		t.Fatalf("aggregate skill framing error = %v", err)
	}
}

func TestAgentRuntimePreservesResultWhenCleanupIsPending(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Succeeded"}, deleteErrs: []error{errors.New("delete denied")}}
	results, done := resultServer(t, StructuredResult{
		BaseSHA: "0123456789abcdef0123456789abcdef01234567", Diff: "diff", Files: []string{"x"},
	})
	defer done()
	s := spec()
	s.Skills = map[string]string{"prompt": "content"}
	r := &AgentRuntime{
		kube: kube, results: results, opts: AgentOptions{AgentRef: "agent", PollEvery: time.Millisecond},
		applyDiff: stubApply(map[string]string{"x": "value"}, nil),
	}
	res, err := r.Generate(context.Background(), s)
	if !errors.Is(err, runtime.ErrCleanupPending) {
		t.Fatalf("error = %v", err)
	}
	if res.Files["x"] != "value" {
		t.Fatalf("result=%+v", res)
	}
}

func TestAgentRuntimeRejectsMismatchedResultIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result StructuredResult
		want   string
	}{
		{name: "version", result: StructuredResult{Version: 2, BaseSHA: "0123456789abcdef0123456789abcdef01234567", Diff: "x", Files: []string{"x"}}, want: "version"},
		{name: "base", result: StructuredResult{BaseSHA: "other", Diff: "x", Files: []string{"x"}}, want: "baseSHA"},
		{name: "push", result: StructuredResult{BaseSHA: "0123456789abcdef0123456789abcdef01234567", Diff: "x", Files: []string{"x"}, PushBranch: "automation/fix"}, want: "pushed branch"},
		{name: "files", result: StructuredResult{BaseSHA: "0123456789abcdef0123456789abcdef01234567", Diff: "x", Files: []string{"other"}}, want: "reported files"},
		{name: "unsafe", result: StructuredResult{BaseSHA: "0123456789abcdef0123456789abcdef01234567", Diff: "x", Files: []string{"../x"}}, want: "unsafe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := &fakeTaskAPI{phases: []string{"Succeeded"}}
			results, done := resultServer(t, tc.result)
			defer done()
			r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "codex-fixer", PollEvery: time.Millisecond}, applyDiff: stubApply(map[string]string{"x": "value"}, nil)}
			if _, err := r.Generate(context.Background(), spec()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestResultClient_NotAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewResultClient(srv.URL, "")
	if _, ok, err := c.Result(context.Background(), "orka-system", "t"); ok || err != nil {
		t.Errorf("404 should be not-available with no error, got ok=%v err=%v", ok, err)
	}
}

func TestResultClientClassifiesSafeHTTPErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte("private response material"))
			}))
			defer server.Close()

			_, _, err := NewResultClient(server.URL, "token").Result(t.Context(), "orka-system", "task")
			var httpErr *ResultHTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != status {
				t.Fatalf("error = %#v", err)
			}
			if strings.Contains(err.Error(), "private response material") {
				t.Fatalf("error exposed response body: %v", err)
			}
			wantAuth := status == http.StatusUnauthorized || status == http.StatusForbidden
			if got := IsResultAuthorizationError(err); got != wantAuth {
				t.Fatalf("IsResultAuthorizationError = %v, want %v", got, wantAuth)
			}
		})
	}
}

func TestResultClientSendsNamespaceAndToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("namespace") != "orka-system" {
			t.Fatalf("namespace query = %q", r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"baseSHA":"sha"}`})
	}))
	defer server.Close()
	client := NewResultClient(server.URL, "token")
	if _, ok, err := client.Result(context.Background(), "orka-system", "task/name"); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestResultClientReloadsFileToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]string{"result": `{"baseSHA":"sha"}`})
	}))
	defer server.Close()

	client := NewFileResultClient(server.URL, path)
	if _, ok, err := client.Result(t.Context(), "orka-system", "task"); err != nil || !ok {
		t.Fatalf("first result ok=%v err=%v", ok, err)
	}
	if err := os.WriteFile(path, []byte("second-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := client.Result(t.Context(), "orka-system", "task"); err != nil || !ok {
		t.Fatalf("second result ok=%v err=%v", ok, err)
	}
	want := []string{"Bearer first-token", "Bearer second-token"}
	if fmt.Sprint(authorizations) != fmt.Sprint(want) {
		t.Fatalf("authorizations = %q, want %q", authorizations, want)
	}
}

func TestResultClientFileTokenErrorsAreSafe(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "credential-material")
		client := NewFileResultClient("http://orka.invalid", path)
		_, _, err := client.Result(t.Context(), "orka-system", "task")
		if err == nil || !strings.Contains(err.Error(), "read Orka result API token file") {
			t.Fatalf("error = %v", err)
		}
		if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "credential-material") {
			t.Fatalf("error exposed token path: %v", err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte(" \n\t"), 0o600); err != nil {
			t.Fatal(err)
		}
		client := NewFileResultClient("http://orka.invalid", path)
		_, _, err := client.Result(t.Context(), "orka-system", "task")
		if err == nil || err.Error() != "orka result API token file is empty" {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestResultTokenSourcePrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORKA_API_TOKEN", "environment-token")
	t.Setenv("ORKA_API_TOKEN_FILE", path)

	for _, tc := range []struct {
		name     string
		explicit string
		want     string
	}{
		{name: "explicit", explicit: "explicit-token", want: "explicit-token"},
		{name: "environment", want: "environment-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := resultTokenSourceFromEnv(tc.explicit)
			got, err := source.Token()
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("token = %q, want %q", got, tc.want)
			}
		})
	}

	t.Setenv("ORKA_API_TOKEN", "")
	got, err := resultTokenSourceFromEnv("").Token()
	if err != nil {
		t.Fatal(err)
	}
	if got != "file-token" {
		t.Fatalf("file token = %q", got)
	}

	t.Setenv("ORKA_API_TOKEN_FILE", "")
	standard, ok := resultTokenSourceFromEnv("").(fileResultTokenSource)
	if !ok || standard.path != serviceAccountToken {
		t.Fatalf("standard source = %#v", standard)
	}
}

func TestAgentRuntimeConstructorUsesDelegatedServiceAccount(t *testing.T) {
	configureTestKubeconfig(t)
	t.Setenv("ORKA_API_TOKEN", "static-token-must-not-be-used")
	t.Setenv("ORKA_API_TOKEN_FILE", "")
	runtime, err := NewAgentRuntimeFromEnv(FromEnvConfig{
		AgentRef: "fixer", API: "http://orka.invalid",
		DelegatedServiceAccountNamespace: "dashboard",
		DelegatedServiceAccountName:      "dashboard-fix",
		PodName:                          "dashboard-server-abc",
		PodUID:                           "pod-uid",
	})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := runtime.results.(*ResultClient)
	if !ok {
		t.Fatalf("result client = %T", runtime.results)
	}
	tokens, ok := client.tokens.(*boundServiceAccountTokenSource)
	if !ok {
		t.Fatalf("token source = %T", client.tokens)
	}
	if tokens.config.Namespace != "dashboard" || tokens.config.Name != "dashboard-fix" || tokens.config.PodName != "dashboard-server-abc" || tokens.config.PodUID != "pod-uid" {
		t.Fatalf("delegated config = %+v", tokens.config)
	}
}

func TestAgentRuntimeConstructorRejectsPartialDelegation(t *testing.T) {
	configureTestKubeconfig(t)
	_, err := NewAgentRuntimeFromEnv(FromEnvConfig{
		AgentRef: "fixer", API: "http://orka.invalid",
		DelegatedServiceAccountName: "dashboard-fix",
	})
	if err == nil || !strings.Contains(err.Error(), "delegated ServiceAccount namespace is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestRuntimeConstructorsUseFileBackedResultClients(t *testing.T) {
	configureTestKubeconfig(t)
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORKA_API_TOKEN", "")
	t.Setenv("ORKA_API_TOKEN_FILE", tokenPath)

	fixRuntime, err := NewAgentRuntimeFromEnv(FromEnvConfig{AgentRef: "fixer", API: "http://orka.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	requireFileResultClient(t, fixRuntime.results, tokenPath)

	sourceRuntime, err := NewSourceInvestigatorFromEnv(SourceInvestigationFromEnvConfig{AgentRef: "reader", API: "http://orka.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	requireFileResultClient(t, sourceRuntime.results, tokenPath)

	opts := containerAnalyzerTestOptions(t, []byte("01234567890123456789012345678901"))
	containerRuntime, err := NewContainerAnalyzer(opts)
	if err != nil {
		t.Fatal(err)
	}
	requireFileResultClient(t, containerRuntime.results, tokenPath)
}

func configureTestKubeconfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	config := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:65535
    insecure-skip-tls-verify: true
users:
- name: test
  user:
    token: test
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBECONFIG", path)
}

func requireFileResultClient(t *testing.T, api any, wantPath string) {
	t.Helper()
	client, ok := api.(*ResultClient)
	if !ok {
		t.Fatalf("result client = %T", api)
	}
	source, ok := client.tokens.(fileResultTokenSource)
	if !ok || source.path != wantPath {
		t.Fatalf("token source = %#v, want file %q", client.tokens, wantPath)
	}
}

func isUnavailable(err error) bool {
	return strings.Contains(err.Error(), runtime.ErrUnavailable.Error())
}

type delayedResultAPI struct {
	calls int
	raw   string
}

func (d *delayedResultAPI) Result(context.Context, string, string) (string, bool, error) {
	d.calls++
	return d.raw, d.calls > 1, nil
}

func TestAgentRuntimeWaitsForDurableResult(t *testing.T) {
	result, err := json.Marshal(StructuredResult{
		Version: 1, BaseSHA: "0123456789abcdef0123456789abcdef01234567", Diff: "diff", Files: []string{"x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	results := &delayedResultAPI{raw: string(result)}
	r := &AgentRuntime{
		kube: &fakeTaskAPI{phases: []string{"Succeeded"}}, results: results,
		opts:      AgentOptions{AgentRef: "codex-fixer", PollEvery: time.Millisecond},
		applyDiff: stubApply(map[string]string{"x": "value"}, nil),
	}
	if _, err := r.Generate(context.Background(), spec()); err != nil {
		t.Fatal(err)
	}
	if results.calls != 2 {
		t.Fatalf("result calls = %d, want 2", results.calls)
	}
}

func TestAgentRuntimeHonorsSpecTimeout(t *testing.T) {
	s := spec()
	s.Timeout = 5 * time.Millisecond
	r := &AgentRuntime{
		kube:    &fakeTaskAPI{phases: []string{"Running"}},
		results: &delayedResultAPI{},
		opts:    AgentOptions{AgentRef: "codex-fixer", PollEvery: time.Millisecond},
	}
	if _, err := r.Generate(context.Background(), s); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestAgentRuntimeRecreatesFailedTask(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Failed", "Succeeded"}, preexisting: true}
	results, done := resultServer(t, StructuredResult{BaseSHA: "0123456789abcdef0123456789abcdef01234567", Diff: "", Files: nil})
	defer done()
	r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "codex-fixer", MaxRetries: 1, PollEvery: time.Millisecond}}
	if _, err := r.Generate(context.Background(), spec()); err != nil {
		t.Fatal(err)
	}
	if kube.deleteCalls != 2 {
		t.Fatalf("delete calls = %d, want 2", kube.deleteCalls)
	}
	taskSpec := kube.applied["spec"].(map[string]any)
	retry := taskSpec["retryPolicy"].(map[string]any)
	if retry["maxRetries"] != int64(1) {
		t.Fatalf("retryPolicy = %+v", retry)
	}
}

func TestAgentTaskNameSeparatesActionRequestExecutions(t *testing.T) {
	first := spec()
	first.ExecutionID = "request-one"
	second := first
	second.ExecutionID = "request-two"
	if AgentTaskName(first, AgentOptions{AgentRef: "agent"}) == AgentTaskName(second, AgentOptions{AgentRef: "agent"}) {
		t.Fatal("request-scoped executions shared a Task name")
	}
	legacy := spec()
	legacyAgain := spec()
	if AgentTaskName(legacy, AgentOptions{AgentRef: "agent"}) != AgentTaskName(legacyAgain, AgentOptions{AgentRef: "agent"}) {
		t.Fatal("legacy content-addressed name is unstable")
	}
}

func TestAgentRuntimeReportsTaskIdentity(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Succeeded"}}
	results, done := resultServer(t, StructuredResult{BaseSHA: "0123456789abcdef0123456789abcdef01234567"})
	defer done()
	var observed []runtime.WorkRef
	s := spec()
	s.ExecutionID = "request-one"
	s.WorkObserver = func(_ context.Context, work runtime.WorkRef) error {
		observed = append(observed, work)
		return nil
	}
	r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "agent", PollEvery: time.Millisecond}}
	if _, err := r.Generate(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || observed[0].UID != "" || observed[1].UID != "uid-1" {
		t.Fatalf("observed work = %+v", observed)
	}
	if !kube.deleted {
		t.Fatal("successful request-scoped Task was not removed")
	}
	metadata := kube.applied["metadata"].(map[string]any)
	annotations := metadata["annotations"].(map[string]any)
	if annotations[actionRequestAnnotation] != "request-one" {
		t.Fatalf("annotations = %+v", annotations)
	}
}

func TestAgentRuntimeCleanupDeletesExactTask(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Running"}, uid: "uid-one", preexisting: true}
	r := &AgentRuntime{kube: kube, opts: AgentOptions{AgentRef: "agent", PollEvery: time.Millisecond}}
	if err := r.Cleanup(context.Background(), runtime.WorkRef{Backend: "orka", Namespace: "orka-system", Name: "fix-task", UID: "uid-one"}); err != nil {
		t.Fatal(err)
	}
	if !kube.deleted || kube.deleteCalls != 1 {
		t.Fatalf("cleanup deleted=%t calls=%d", kube.deleted, kube.deleteCalls)
	}
}

func TestAgentRuntimeCleanupRejectsReplacementUID(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Running"}, uid: "replacement", preexisting: true}
	r := &AgentRuntime{kube: kube, opts: AgentOptions{AgentRef: "agent", PollEvery: time.Millisecond}}
	err := r.Cleanup(context.Background(), runtime.WorkRef{Backend: "orka", Namespace: "orka-system", Name: "fix-task", UID: "original"})
	if !errors.Is(err, runtime.ErrWorkIdentityChanged) {
		t.Fatalf("cleanup error = %v", err)
	}
	if kube.deleteCalls != 0 {
		t.Fatalf("replacement Task was deleted: %d", kube.deleteCalls)
	}
}

func TestAgentRuntimeTimeoutCleansObservedTask(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Running"}}
	r := &AgentRuntime{kube: kube, results: &delayedResultAPI{}, opts: AgentOptions{AgentRef: "agent", PollEvery: time.Millisecond}}
	s := spec()
	s.Timeout = 5 * time.Millisecond
	s.ExecutionID = "request-timeout"
	if _, err := r.Generate(context.Background(), s); err == nil {
		t.Fatal("timed out generation succeeded")
	}
	if !kube.deleted {
		t.Fatal("timed out generation left the Task running")
	}
}

func TestAgentRuntimeRejectsTaskOwnedByAnotherRequest(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Running"}, preexisting: true, annotations: map[string]string{actionRequestAnnotation: "other-request"}}
	r := &AgentRuntime{kube: kube, results: &delayedResultAPI{}, opts: AgentOptions{AgentRef: "agent", PollEvery: time.Millisecond}}
	s := spec()
	s.ExecutionID = "this-request"
	_, err := r.Generate(context.Background(), s)
	if !errors.Is(err, runtime.ErrWorkIdentityChanged) {
		t.Fatalf("generation error = %v", err)
	}
	if kube.deleteCalls != 0 {
		t.Fatalf("another request's Task was deleted: %d", kube.deleteCalls)
	}
}

func TestAgentRuntimeCleanupAdoptsPlannedOwnedTask(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Running"}, preexisting: true, uid: "uid-one", annotations: map[string]string{actionRequestAnnotation: "request-one"}}
	r := &AgentRuntime{kube: kube, opts: AgentOptions{AgentRef: "agent", PollEvery: time.Millisecond}}
	if err := r.Cleanup(context.Background(), runtime.WorkRef{Backend: "orka", Name: "fix-task", ExecutionID: "request-one"}); err != nil {
		t.Fatal(err)
	}
	if !kube.deleted {
		t.Fatal("owned planned Task was not cleaned")
	}
}

func TestAgentRuntimeFailureAnalysisPurposeIsConstrained(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Succeeded"}, attempts: 2}
	results, done := resultServer(t, StructuredResult{BaseSHA: strings.Repeat("a", 40)})
	defer done()
	s := spec()
	s.Repo.Ref = strings.Repeat("a", 40)
	s.AllowBash = false
	r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{
		AgentRef: "analysis-agent", GitSecret: "source-readonly", Purpose: AgentPurposeFailureAnalysis, PollEvery: time.Millisecond,
	}}
	got, err := r.Generate(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", got.Attempts)
	}
	metadata := kube.applied["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]any)
	annotations := metadata["annotations"].(map[string]any)
	if labels[ManagedByLabel] != AgentAnalysisManagedByValue || labels[agentContractLabel] != string(AgentPurposeFailureAnalysis) {
		t.Fatalf("labels = %+v", labels)
	}
	if annotations[analysisContractAnnotation] != "agent-analysis-v1" || annotations["orka.ai/disable-coordination-tool-injection"] != "true" {
		t.Fatalf("annotations = %+v", annotations)
	}
	if !strings.HasPrefix(metadata["name"].(string), "analysis-") {
		t.Fatalf("Task name = %q", metadata["name"])
	}
	taskSpec := kube.applied["spec"].(map[string]any)
	agentRuntime := taskSpec["agentRuntime"].(map[string]any)
	if agentRuntime["allowBash"] != false {
		t.Fatalf("agent runtime = %+v", agentRuntime)
	}
	wantAllowed := []any{"Read", "Glob", "Grep", "Write"}
	wantDisallowed := []any{"Bash", "WebFetch", "WebSearch", "Task", "TodoWrite", "Question"}
	if !reflect.DeepEqual(agentRuntime["allowedTools"], wantAllowed) || !reflect.DeepEqual(agentRuntime["disallowedTools"], wantDisallowed) {
		t.Fatalf("tool policy = %+v", agentRuntime)
	}
	workspace := agentRuntime["workspace"].(map[string]any)
	if !reflect.DeepEqual(workspace["gitSecretRef"], map[string]any{"name": "source-readonly"}) {
		t.Fatalf("workspace = %+v", workspace)
	}
}

func TestAgentRuntimeFailureAnalysisRejectsBashBeforeClusterWrite(t *testing.T) {
	kube := &fakeTaskAPI{}
	r := &AgentRuntime{kube: kube, results: &delayedResultAPI{}, opts: AgentOptions{AgentRef: "analysis-agent", Purpose: AgentPurposeFailureAnalysis}}
	if _, err := r.Generate(t.Context(), spec()); err == nil || !strings.Contains(err.Error(), "does not permit Bash") {
		t.Fatalf("error = %v", err)
	}
	if kube.applied != nil {
		t.Fatal("invalid analysis Task reached the cluster")
	}
}

func TestAgentRuntimePreservesAttemptsOnTaskAndResultFailures(t *testing.T) {
	t.Run("Task failed", func(t *testing.T) {
		kube := &fakeTaskAPI{phases: []string{"Failed"}, attempts: 3}
		r := &AgentRuntime{kube: kube, results: &delayedResultAPI{}, opts: AgentOptions{AgentRef: "agent", PollEvery: time.Millisecond}}
		got, err := r.Generate(t.Context(), spec())
		if err == nil || got.Attempts != 3 {
			t.Fatalf("result=%+v error=%v", got, err)
		}
	})
	t.Run("invalid result", func(t *testing.T) {
		kube := &fakeTaskAPI{phases: []string{"Succeeded"}, attempts: 2}
		results, done := rawResultServer(t, "not-json")
		defer done()
		r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "agent", PollEvery: time.Millisecond}}
		got, err := r.Generate(t.Context(), spec())
		if err == nil || got.Attempts != 2 {
			t.Fatalf("result=%+v error=%v", got, err)
		}
	})
}
