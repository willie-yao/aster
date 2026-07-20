package orka

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
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
}

func (f *fakeTaskAPI) Apply(_ context.Context, _ schema.GroupVersionResource, _ string, obj map[string]any) error {
	f.applied = obj
	f.deleted = false
	return f.applyErr
}

func (f *fakeTaskAPI) Delete(context.Context, schema.GroupVersionResource, string, string) error {
	f.deleted = true
	f.deleteCalls++
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

// resultServer returns an httptest server that serves a StructuredResult (as the
// {"result": "<json>"} envelope) for any task, plus a ResultClient for it.
func resultServer(t *testing.T, sr StructuredResult) (*ResultClient, func()) {
	t.Helper()
	if sr.Version == 0 {
		sr.Version = 1
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/result") {
			http.NotFound(w, r)
			return
		}
		inner, _ := json.Marshal(sr)
		json.NewEncoder(w).Encode(map[string]string{"result": string(inner)}) //nolint:errcheck
	}))
	return NewResultClient(srv.URL, ""), srv.Close
}

func spec() runtime.GenerateSpec {
	return runtime.GenerateSpec{
		Repo:        runtime.RepoRef{Owner: "o", Name: "n", Ref: "pinned-sha"},
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
		BaseSHA: "pinned-sha",
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

	// The applied Task is generation-only: type agent, no pushBranch, pinned ref.
	spec, _ := kube.applied["spec"].(map[string]any)
	if spec["type"] != "agent" {
		t.Errorf("task type = %v, want agent", spec["type"])
	}
	if spec["prompt"] != "fix the etcd disk type" {
		t.Errorf("prompt not set: %v", spec["prompt"])
	}
	ar, _ := spec["agentRuntime"].(map[string]any)
	ws, _ := ar["workspace"].(map[string]any)
	if ws["ref"] != "pinned-sha" {
		t.Errorf("workspace ref = %v, want pinned-sha", ws["ref"])
	}
	if _, hasPush := ws["pushBranch"]; hasPush {
		t.Errorf("generation-only Task must not set pushBranch: %v", ws)
	}
	if ar["allowBash"] != true || ar["maxTurns"] != int64(30) {
		t.Errorf("agentRuntime overrides not set: %v", ar)
	}
}

func TestAgentRuntime_NoDiffIsNotFixable(t *testing.T) {
	kube := &fakeTaskAPI{phases: []string{"Succeeded"}}
	results, done := resultServer(t, StructuredResult{Summary: "no code change needed", BaseSHA: "pinned-sha", Diff: ""})
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

func TestAgentRuntime_ApplyUnavailable(t *testing.T) {
	kube := &fakeTaskAPI{applyErr: fmt.Errorf("no route to host")}
	results, done := resultServer(t, StructuredResult{})
	defer done()
	r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "codex-fixer", PollEvery: time.Millisecond},
		applyDiff: stubApply(nil, nil)}

	_, err := r.Generate(context.Background(), spec())
	if err == nil || !isUnavailable(err) {
		t.Errorf("apply failure should surface ErrUnavailable, got %v", err)
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

func TestFixTaskName_ContentAddressedAndStable(t *testing.T) {
	baseSpec := spec()
	opts := AgentOptions{AgentRef: "codex-fixer", Version: "v1"}
	a := FixTaskName(baseSpec, opts)
	b := FixTaskName(baseSpec, opts)
	if a != b {
		t.Errorf("same inputs must give the same name: %s vs %s", a, b)
	}
	changed := baseSpec
	changed.Instruction = "different"
	if c := FixTaskName(changed, opts); c == a {
		t.Error("different instruction did not change the name")
	}
	opts.AgentRef = "other-agent"
	if c := FixTaskName(baseSpec, opts); c == a {
		t.Error("different AgentRef did not change the name")
	}
	if !strings.HasPrefix(a, "fix-") {
		t.Errorf("name should be fix-prefixed: %s", a)
	}
}

func TestAgentRuntimeRejectsMismatchedResultIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result StructuredResult
		want   string
	}{
		{name: "version", result: StructuredResult{Version: 2, BaseSHA: "pinned-sha", Diff: "x", Files: []string{"x"}}, want: "version"},
		{name: "base", result: StructuredResult{BaseSHA: "other", Diff: "x", Files: []string{"x"}}, want: "baseSHA"},
		{name: "files", result: StructuredResult{BaseSHA: "pinned-sha", Diff: "x", Files: []string{"other"}}, want: "reported files"},
		{name: "unsafe", result: StructuredResult{BaseSHA: "pinned-sha", Diff: "x", Files: []string{"../x"}}, want: "unsafe"},
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

func TestResolveOrkaAPITokenFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORKA_API_TOKEN_FILE", path)
	if got := resolveOrkaAPIToken(""); got != "file-token" {
		t.Fatalf("token = %q", got)
	}
	if got := resolveOrkaAPIToken("explicit"); got != "explicit" {
		t.Fatalf("explicit token = %q", got)
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
		Version: 1, BaseSHA: "pinned-sha", Diff: "diff", Files: []string{"x"},
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
	kube := &fakeTaskAPI{phases: []string{"Failed", "Succeeded"}}
	results, done := resultServer(t, StructuredResult{BaseSHA: "pinned-sha", Diff: "", Files: nil})
	defer done()
	r := &AgentRuntime{kube: kube, results: results, opts: AgentOptions{AgentRef: "codex-fixer", MaxRetries: 1, PollEvery: time.Millisecond}}
	if _, err := r.Generate(context.Background(), spec()); err != nil {
		t.Fatal(err)
	}
	if kube.deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", kube.deleteCalls)
	}
	taskSpec := kube.applied["spec"].(map[string]any)
	retry := taskSpec["retryPolicy"].(map[string]any)
	if retry["maxRetries"] != int64(1) {
		t.Fatalf("retryPolicy = %+v", retry)
	}
}
