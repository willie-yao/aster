package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/evidenceplan"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/repotree"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

// newTestRegistry returns a filesystem registry so tests hit real dispatch.
// K8s tools are omitted because fakeBrowser does not model discovery paths.
func newTestRegistry(t *testing.T) (*tools.Registry, []string) {
	t.Helper()
	r := tools.NewRegistry()
	filesystem.Register(r)
	enabled, err := r.Enable([]string{"filesystem"})
	if err != nil {
		t.Fatalf("registry.Enable: %v", err)
	}
	return r, enabled
}

type fakeSourceRepo struct{ files map[string]string }

func (r *fakeSourceRepo) ListTree(context.Context) ([]string, error) {
	var paths []string
	for path := range r.files {
		paths = append(paths, path)
	}
	return paths, nil
}

func (r *fakeSourceRepo) ReadFile(_ context.Context, path string) (string, bool, error) {
	content, ok := r.files[path]
	return content, ok, nil
}

// newTestAgenticInputs builds per-call agentic inputs for tests.
func newTestAgenticInputs(t *testing.T, browser artifacts.Browser, opts AgenticOptions) AgenticInputs {
	t.Helper()
	registry, enabled := newTestRegistry(t)
	return AgenticInputs{
		Browser:      browser,
		Opts:         opts,
		Registry:     registry,
		EnabledTools: enabled,
		Cache:        tools.NewCache(),
	}
}

// fakeBrowser is an in-memory artifacts.Browser for agentic tests.
type fakeBrowser struct {
	files map[string][]byte
	dirs  map[string][]string
}

type treeResponse struct {
	paths     []string
	truncated bool
	err       error
}

type trackingBrowser struct {
	*fakeBrowser
	treeResponses []treeResponse
	listTreeCalls int
	tailCalls     []string
	tailMaxBytes  []int
	tailErrors    map[string]error
	emptyTails    map[string]bool
}

func (b *trackingBrowser) ListTree(ctx context.Context, maxPaths int) ([]string, bool, error) {
	b.listTreeCalls++
	if len(b.treeResponses) > 0 {
		response := b.treeResponses[0]
		b.treeResponses = b.treeResponses[1:]
		return append([]string(nil), response.paths...), response.truncated, response.err
	}
	return b.fakeBrowser.ListTree(ctx, maxPaths)
}

func (b *trackingBrowser) Tail(ctx context.Context, p string, lines, maxBytes int) (*artifacts.TailResult, error) {
	b.tailCalls = append(b.tailCalls, p)
	b.tailMaxBytes = append(b.tailMaxBytes, maxBytes)
	if err := b.tailErrors[p]; err != nil {
		return nil, err
	}
	if b.emptyTails[p] {
		return &artifacts.TailResult{}, nil
	}
	result, err := b.fakeBrowser.Tail(ctx, p, lines, maxBytes)
	if err != nil || result == nil || maxBytes <= 0 || len(result.Content) <= maxBytes {
		return result, err
	}
	copy := *result
	copy.Content = append([]byte(nil), result.Content[len(result.Content)-maxBytes:]...)
	return &copy, nil
}

func (b *fakeBrowser) BuildRoot() string { return "fake/build/1" }

func (b *fakeBrowser) ListTree(_ context.Context, maxPaths int) ([]string, bool, error) {
	var out []string
	for name := range b.files {
		if len(out) >= maxPaths {
			return out, true, nil
		}
		out = append(out, name)
	}
	return out, false, nil
}

func (b *fakeBrowser) List(_ context.Context, dir string) (*artifacts.Listing, error) {
	dir = strings.TrimSuffix(dir, "/")
	res := &artifacts.Listing{Dir: dir}
	if d, ok := b.dirs[dir]; ok {
		res.Dirs = d
	}
	prefix := dir + "/"
	if dir == "" {
		prefix = ""
	}
	for name, data := range b.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if strings.Contains(rest, "/") {
			continue
		}
		res.Files = append(res.Files, artifacts.FileInfo{Name: rest, Size: int64(len(data))})
	}
	return res, nil
}

func (b *fakeBrowser) Read(_ context.Context, p string, offset, length int) ([]byte, int64, error) {
	data, ok := b.files[p]
	if !ok {
		return nil, 0, fmt.Errorf("not found: %s", p)
	}
	if offset > len(data) {
		return nil, int64(len(data)), nil
	}
	end := offset + length
	if end > len(data) {
		end = len(data)
	}
	return data[offset:end], int64(len(data)), nil
}

func (b *fakeBrowser) Tail(_ context.Context, p string, lines, _ int) (*artifacts.TailResult, error) {
	data, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	content := strings.Join(all, "\n")
	return &artifacts.TailResult{
		FileSize:      int64(len(data)),
		LinesReturned: len(all),
		Content:       []byte(content),
	}, nil
}

func (b *fakeBrowser) Grep(_ context.Context, p string, re *regexp.Regexp, _, maxMatches, _, _ int) (*artifacts.GrepResult, error) {
	data, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	var matches []artifacts.GrepMatch
	for i, line := range strings.Split(string(data), "\n") {
		if re.MatchString(line) {
			matches = append(matches, artifacts.GrepMatch{LineNo: i + 1, Context: []string{fmt.Sprintf("> %d: %s", i+1, line)}})
		}
	}
	total := len(matches)
	if maxMatches > 0 && len(matches) > maxMatches {
		matches = matches[:maxMatches]
	}
	return &artifacts.GrepResult{
		FileSize:     int64(len(data)),
		TotalMatches: total,
		Matches:      matches,
		Truncated:    total > len(matches),
		BytesScanned: int64(len(data)),
	}, nil
}

// scriptedChatServer returns an httptest.Server with queued chat responses.
// Each request pops one response; tests can push custom status codes.
type scriptedChatServer struct {
	*httptest.Server
	mu        sync.Mutex
	responses []string
	statuses  []int
	calls     int32
	requests  [][]byte
}

func newScriptedChatServer(t *testing.T) *scriptedChatServer {
	t.Helper()
	s := &scriptedChatServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.calls, 1)
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.responses) == 0 {
			http.Error(w, "no scripted response", http.StatusInternalServerError)
			return
		}
		body := s.responses[0]
		s.responses = s.responses[1:]
		status := http.StatusOK
		if len(s.statuses) > 0 {
			status = s.statuses[0]
			s.statuses = s.statuses[1:]
		}
		// Capture the request body so tests can assert echoed history.
		reqBody, _ := io.ReadAll(r.Body)
		s.requests = append(s.requests, reqBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *scriptedChatServer) push(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, body)
	s.statuses = append(s.statuses, status)
}

// chatRespFinal builds a JSON chat-completion response with no tool calls.
func chatRespFinal(content string) string {
	c, _ := json.Marshal(content)
	return fmt.Sprintf(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%s}}]}`, c)
}

// chatRespToolCall builds a chat-completion response that invokes one tool.
func chatRespToolCall(id, name string, args map[string]interface{}) string {
	a, _ := json.Marshal(args)
	aStr, _ := json.Marshal(string(a))
	return fmt.Sprintf(
		`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}}]}`,
		id, name, aStr,
	)
}

// chatRespTwoToolCalls builds a parallel tool-call response for SingleToolCall tests.
func chatRespTwoToolCalls(id1, name1, id2, name2 string) string {
	mk := func(id, name string) string {
		args, _ := json.Marshal(`{"path":""}`)
		return fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}`, id, name, args)
	}
	return fmt.Sprintf(
		`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[%s,%s]}}]}`,
		mk(id1, name1), mk(id2, name2),
	)
}

func newAgenticTestClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	c := NewClientWithOptions(Options{
		Token:    "test-token",
		CacheDir: t.TempDir(),
		Endpoint: serverURL,
		Model:    "claude-test",
	})
	return c
}

// shrinkCallDelay temporarily reduces callDelay for the duration of a test
// so agentic tests with multiple iterations don't add seconds of latency.
func shrinkCallDelay(t *testing.T) {
	t.Helper()
	old := callDelay
	callDelay = 1 * time.Millisecond
	t.Cleanup(func() { callDelay = old })
}

func TestAgentic_HappyPath_ToolThenFinalJSON(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: model calls list_artifacts.
	srv.push(200, chatRespToolCall("call_1", "list_artifacts", map[string]interface{}{"path": ""}))
	// Round 2: model returns final JSON.
	final := `{"summary":"DNS lookup failed","is_transient":false,"root_cause":"resolver pointed at stale nameserver","severity":"High","suggested_fix":"Update /etc/resolv.conf","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte("hello\nworld\n")},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}

	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:job:1:abc", "system", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "DNS lookup failed" {
		t.Errorf("summary mismatch: %q", summary.Summary)
	}
	if analysis.Mode != AgenticMode {
		t.Errorf("mode = %q, want %q", analysis.Mode, AgenticMode)
	}
	if analysis.RootCause != "resolver pointed at stale nameserver" {
		t.Errorf("root cause mismatch: %q", analysis.RootCause)
	}
	if atomic.LoadInt32(&srv.calls) != 2 {
		t.Errorf("call count = %d, want 2", srv.calls)
	}
	if analysis.ToolCalls != 1 {
		t.Errorf("tool_calls = %d, want 1", analysis.ToolCalls)
	}
	if analysis.ContextBytes <= 0 {
		t.Errorf("expected positive context_bytes, got %d", analysis.ContextBytes)
	}
	if analysis.CacheHit {
		t.Error("expected cache_hit=false on first call")
	}
	if analysis.ElapsedMs < 0 {
		t.Errorf("expected non-negative elapsed_ms, got %d", analysis.ElapsedMs)
	}
}

func TestAgenticTraceRecordsModelToolAndCritique(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 1024}))
	srv.push(200, chatRespFinal(`{"summary":"failure","is_transient":false,"root_cause":"build-log.txt contains the initiating error","severity":"High","suggested_fix":"fix the configuration","relevant_files":["build-log.txt"]}`))

	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("initiating error\n")}}
	opts := AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}
	if _, _, err := client.doAnalyzeAgentic(ctx, newTestAgenticInputs(t, browser, opts), "agentic:test:trace", "sys", "user"); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)

	counts := map[string]int{}
	for _, event := range store.Snapshot().Traces[0].Events {
		counts[event.Kind]++
		if event.Kind == "tool_call" && (event.Tool != "read_artifact" || event.Outcome != "success" || event.Bytes == 0) {
			t.Fatalf("tool event = %+v", event)
		}
	}
	if counts["model_request"] != 2 || counts["tool_call"] != 1 || counts["critique"] != 1 {
		t.Fatalf("event counts = %+v", counts)
	}
}

func TestAgentic_CacheHit(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"cached","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}

	if _, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:cached", "sys", "user"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, a2, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:cached", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Errorf("expected 1 server call (second was cache hit), got %d", got)
	}
	if !a2.CacheHit {
		t.Error("expected cache_hit=true on second (cached) call")
	}
	if a2.ToolCalls != 0 || a2.ContextBytes != 0 || a2.GCSBytes != 0 {
		t.Errorf("expected zero counters on cache hit (no state), got tool_calls=%d context_bytes=%d gcs_bytes=%d",
			a2.ToolCalls, a2.ContextBytes, a2.GCSBytes)
	}
	if a2.Mode != AgenticMode {
		t.Errorf("cache-hit mode = %q, want %q", a2.Mode, AgenticMode)
	}
}

func TestAgentic_CacheHit_TransientVerdictAfterPersistence(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	transient := `{"summary":"flaky","is_transient":true,"root_cause":"infra","severity":"Low","suggested_fix":"re-run","relevant_files":[]}`
	srv.push(200, chatRespFinal(transient))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}

	in1 := newTestAgenticInputs(t, browser, opts)
	_, first, err := client.doAnalyzeAgentic(context.Background(), in1, "agentic:test:staletransient", "sys", "user")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	in2 := newTestAgenticInputs(t, browser, opts)
	in2.ConsecutiveFailures = transientPersistThreshold
	summary, second, err := client.doAnalyzeAgentic(context.Background(), in2, "agentic:test:staletransient", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !second.CacheHit || !summary.IsTransient || second.RootCause != first.RootCause {
		t.Fatalf("cached transient result = summary=%+v analysis=%+v", summary, second)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("server calls = %d, want 1", got)
	}
}

func TestAgentic_ToolsUnsupported_FirstCall(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// 400 with a body that mentions "function calling".
	srv.push(400, `{"error":{"message":"function calling not supported by this model"}}`)

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}
	_, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:nope", "sys", "user")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrToolsUnsupported) {
		t.Fatalf("expected ErrToolsUnsupported, got: %v", err)
	}
}

func TestAgentic_FinalizeRound_JSONRepair(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Round 1: model returns prose without valid JSON.
	srv.push(200, chatRespFinal("I think it was DNS but I'm not sure."))
	// Finalize round: model returns valid JSON.
	final := `{"summary":"DNS lookup failed","is_transient":false,"root_cause":"resolver","severity":"High","suggested_fix":"fix","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:repair", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "DNS lookup failed" {
		t.Errorf("summary = %q", summary.Summary)
	}
	if analysis.Mode != AgenticMode {
		t.Errorf("mode = %q", analysis.Mode)
	}
}

func TestAgentic_BudgetExhausted_SynthesizesFallback(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Round 1: model returns unparseable prose. Finalize round will also return unparseable prose.
	srv.push(200, chatRespFinal("not json"))
	srv.push(200, chatRespFinal("still not json"))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{MaxIters: 1, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:fallback", "sys", "user")
	if err != nil {
		t.Fatalf("expected fallback synthesis, not error, got: %v", err)
	}
	if summary == nil || analysis == nil {
		t.Fatal("expected synthesized outputs")
	}
	if analysis.Mode != AgenticMode {
		t.Errorf("mode = %q", analysis.Mode)
	}
	srv.push(200, chatRespFinal("still not json"))
	srv.push(200, chatRespFinal("still not json"))
	before := atomic.LoadInt32(&srv.calls)
	if _, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, &fakeBrowser{}, opts), "agentic:test:fallback", "sys", "user"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if atomic.LoadInt32(&srv.calls) == before {
		t.Error("fallback should not have been cached (expected server hit on retry)")
	}
}

func TestIsToolsUnsupportedError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain 500", fmt.Errorf("chat returned 500: server error"), false},
		{"400 no tools msg", fmt.Errorf("chat returned 400: bad request"), false},
		{"400 + tools", fmt.Errorf("chat returned 400: tools_choice not supported"), true},
		{"400 + tools are unsupported", fmt.Errorf("chat returned 400: tools are not supported by this model"), true},
		{"400 + function calling", fmt.Errorf("chat returned 400: function calling not supported"), true},
		{"422 + function_call", fmt.Errorf("chat returned 422: function_call invalid"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isToolsUnsupportedError(tc.err); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTryParseAnalysis(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"empty", "", false},
		{"whitespace", "   \n  ", false},
		{"plain prose", "no json here", false},
		{"valid json", `{"summary":"x","root_cause":"y","severity":"High"}`, true},
		{"json with ```", "```json\n{\"summary\":\"x\",\"root_cause\":\"y\"}\n```", true},
		{"empty fields", `{"summary":"","root_cause":""}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := tryParseAnalysis(tc.in)
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v", ok, tc.ok)
			}
		})
	}
}

// TestAgentic_MinToolCalls_NudgeForcesInvestigation verifies a tools-free final
// is rejected until the min-tool-call floor is met.
func TestAgentic_MinToolCalls_NudgeForcesInvestigation(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: model tries to finalize immediately with no tool calls.
	final1 := `{"summary":"made up","is_transient":false,"root_cause":"premature guess","severity":"High","suggested_fix":"x","relevant_files":[]}`
	srv.push(200, chatRespFinal(final1))
	// Round 2: after the nudge, model reads build-log.txt (the artifact it
	// will cite), satisfying both the floor and the critique's read check.
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 16384}))
	// Round 3: model finalizes with the post-investigation answer.
	final2 := `{"summary":"real cause","is_transient":false,"root_cause":"found in build-log.txt line 42","severity":"High","suggested_fix":"fix it","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(final2))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte("the error\n")},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second, MinToolCalls: 1}

	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:nudge1", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count = %d, want 3 (nudged round + tool round + final)", got)
	}
	if summary.Summary != "real cause" {
		t.Errorf("expected post-nudge final, got summary=%q", summary.Summary)
	}
	if analysis.RootCause != "found in build-log.txt the cited artifact evidence" {
		t.Errorf("expected unsupported line claim to be removed, got %q", analysis.RootCause)
	}
	if analysis.ToolCalls != 1 {
		t.Errorf("tool_calls = %d, want 1", analysis.ToolCalls)
	}

	_, _, err = client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:nudge1", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count after cache hit = %d, want 3 (no extra server hit)", got)
	}
}

// TestAgentic_MinToolCalls_RejectedFinalNotReusedAfterMaxIters verifies rejected
// tools-free JSON is not reused when MaxIters is exhausted.
func TestAgentic_MinToolCalls_RejectedFinalNotReusedAfterMaxIters(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: tools-free valid JSON is rejected because calls=0 < min=2.
	rejected := `{"summary":"REJECTED","is_transient":false,"root_cause":"premature","severity":"High","suggested_fix":"x","relevant_files":[]}`
	srv.push(200, chatRespFinal(rejected))
	// Rounds 2+: after the nudge, model only calls tools and never finalizes.
	// MaxIters=3 means we get exactly 2 more chat calls. Both are tool calls.
	srv.push(200, chatRespToolCall("call_1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespToolCall("call_2", "list_artifacts", map[string]interface{}{"path": ""}))
	// Loop exits via MaxIters; runFinalizeRound fires. Force a successful
	// finalize so we land in the cache-write path.
	final := `{"summary":"FINAL","is_transient":false,"root_cause":"from finalize round","severity":"High","suggested_fix":"y","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{dirs: map[string][]string{"": {"artifacts"}}}
	opts := AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second, MinToolCalls: 2}

	summary, _, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:notreused", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary == "REJECTED" {
		t.Errorf("rejected pre-nudge JSON leaked into final output: got %q", summary.Summary)
	}
	if summary.Summary != "FINAL" {
		t.Errorf("expected finalize-round output, got %q", summary.Summary)
	}
}

func TestAgentic_UnmetFloorDraftIsFallbackAfterUnparseableFinalize(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	fallback := `{"summary":"fallback","is_transient":false,"root_cause":"controller configuration mismatch","severity":"High","suggested_fix":"Update the controller configuration.","relevant_files":[]}`
	srv.push(200, chatRespFinal(fallback))
	srv.push(200, chatRespFinal("not json"))

	selected := 0
	in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
		MaxIters: 1, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, MinToolCalls: 2,
	})
	in.DraftSelectionObserver = func(attempt int) { selected = attempt }
	client := newAgenticTestClient(t, srv.URL)
	key := "agentic:test:floor-fallback"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if analysis.RootCause != "controller configuration mismatch" || selected != 1 {
		t.Fatalf("unmet-floor fallback lost: selected=%d analysis=%+v", selected, analysis)
	}
	if _, ok := client.Cache().Get(key); ok {
		t.Fatal("below-floor fallback was cached")
	}
}

// bigPayload returns deterministic bytes for MinGCSBytes floor tests.
func bigPayload(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte('A' + (i % 26))
	}
	return out
}

// TestAgentic_MinGCSBytes_NudgeForcesMoreReading verifies acceptance waits for
// reads that cross the byte floor.
func TestAgentic_MinGCSBytes_NudgeForcesMoreReading(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: list_artifacts (BytesFetched=0).
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	// Round 2: tools-free finalize with gcsBytes still 0.
	premature := `{"summary":"shallow","is_transient":false,"root_cause":"unknown","severity":"Medium","suggested_fix":"x","relevant_files":[]}`
	srv.push(200, chatRespFinal(premature))
	// Round 3: after the nudge, read_artifact returns 16 KB so gcsBytes
	// crosses the 15 KB floor.
	srv.push(200, chatRespToolCall("c2", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 16384}))
	// Round 4: tools-free with substantive content.
	final := `{"summary":"deep","is_transient":false,"root_cause":"found in build-log.txt:42","severity":"High","suggested_fix":"fix","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": bigPayload(30_000)},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	opts := AgenticOptions{MaxIters: 6, ModelByteBudget: 200_000, GCSByteBudget: 200_000, Timeout: 30 * time.Second, MinGCSBytes: 15_000}

	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:gcsnudge", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "deep" {
		t.Errorf("expected post-nudge final, got summary=%q", summary.Summary)
	}
	if analysis.GCSBytes < 15_000 {
		t.Errorf("gcs_bytes = %d, want >= 15000 (floor must have been met before acceptance)", analysis.GCSBytes)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 4 {
		t.Errorf("call count = %d, want 4 (list + premature final + read + final)", got)
	}

	_, _, err = client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:gcsnudge", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 4 {
		t.Errorf("call count after cache hit = %d, want 4 (no extra server hit)", got)
	}
}

func TestAgentic_EvidencePlanCoverageSatisfiesGCSFloorAndSurvivesReload(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	path := "artifacts/issuer.yaml"
	srv.push(200, chatRespToolCall("call_1", "tail_artifact", map[string]interface{}{"path": path, "lines": 200}))
	srv.push(200, chatRespFinal(`{"summary":"x509","is_transient":false,"root_cause":"x509 issuer mismatch shown in artifacts/issuer.yaml","severity":"High","suggested_fix":"Update the issuer with the correct CA and redeploy.","relevant_files":["artifacts/issuer.yaml"]}`))

	set := loadAgenticSkillsForTest(t, map[string]string{
		"x509": `
id: x509
triggers: ["x509"]
required_evidence:
  - id: issuer
    any_of: ["issuer\\.yaml$"]
`,
	})
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{path: []byte("kind: Issuer\n")}}}
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		MinToolCalls: 1, MinGCSBytes: 50_000, CritiqueMaxRetries: 2,
	}
	cacheDir := t.TempDir()
	newClient := func() *Client {
		return NewClientWithOptions(Options{Token: "test-token", CacheDir: cacheDir, Endpoint: srv.URL, Model: "claude-test"})
	}
	input := func() AgenticInputs {
		in := newTestAgenticInputs(t, browser, opts)
		in.Skills = set
		in.FailureSignal = "x509 failure"
		return in
	}

	client := newClient()
	const key = "agentic:test:evidence-coverage"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), input(), key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.EvidencePlanCovered {
		t.Fatalf("EvidencePlanCovered = false: %+v", analysis)
	}
	if analysis.GCSBytes >= opts.MinGCSBytes {
		t.Fatalf("GCSBytes = %d, want below floor %d", analysis.GCSBytes, opts.MinGCSBytes)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("model calls = %d, want tool plus final only", got)
	}
	if err := client.Cache().Save(); err != nil {
		t.Fatalf("save cache: %v", err)
	}

	reloaded := newClient()
	_, cached, err := reloaded.doAnalyzeAgentic(context.Background(), input(), key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !cached.CacheHit || !cached.EvidencePlanCovered {
		t.Fatalf("reloaded analysis = %+v, want marked cache hit", cached)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("model calls after reload = %d, want 2", got)
	}
}

func TestAgentic_CompleteSparseEvidencePlanSatisfiesGCSFloor(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 4096}))
	final := `{"summary":"profiled failure","is_transient":false,"root_cause":"The profiled failure is proven by build-log.txt.","severity":"High","suggested_fix":"Correct the rejected configuration and rerun the job.","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(final))

	set := loadAgenticSkillsForTest(t, map[string]string{
		"profiled": `
id: profiled
triggers: ["profiled"]
required_evidence:
  - id: build-log
    any_of: ["build-log\\.txt$"]
  - id: junit
    any_of: ["junit.*\\.xml$"]
`,
	})
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("initiating failure\n")}}
	opts := AgenticOptions{
		MaxIters: 4, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		MinToolCalls: 1, MinGCSBytes: 50_000, CritiqueMaxRetries: 1,
	}
	in := newTestAgenticInputs(t, browser, opts)
	in.Skills = set
	in.FailureSignal = "profiled failure"
	client := newAgenticTestClient(t, srv.URL)
	const key = "agentic:test:complete-sparse-plan"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.EvidencePlanCovered || analysis.GCSFloorRetryExhausted || analysis.GCSBytes >= opts.MinGCSBytes {
		t.Fatalf("analysis = %+v", analysis)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("model calls = %d, want one read and one final", got)
	}
	_, cached, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !cached.CacheHit || !cached.EvidencePlanCovered || atomic.LoadInt32(&srv.calls) != 2 {
		t.Fatalf("cached analysis = %+v calls=%d", cached, atomic.LoadInt32(&srv.calls))
	}
}

func TestAgentic_GCSFloorOnlyRetryIsCappedAndReusable(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 1024}))
	final := `{"summary":"configuration rejected","is_transient":false,"root_cause":"build-log.txt contains the configuration rejection.","severity":"High","suggested_fix":"Correct the rejected configuration and rerun the job.","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(final))
	srv.push(200, chatRespFinal(final))

	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": bigPayload(1024)}}
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		MinToolCalls: 1, MinGCSBytes: 50_000, CritiqueMaxRetries: 1,
	}
	client := newAgenticTestClient(t, srv.URL)
	in := newTestAgenticInputs(t, browser, opts)
	const key = "agentic:test:gcs-floor-retry-cap"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if analysis.GCSBytes >= opts.MinGCSBytes || analysis.EvidencePlanCovered || !analysis.GCSFloorRetryExhausted || !analysis.CritiquePassed {
		t.Fatalf("analysis = %+v", analysis)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Fatalf("model calls = %d, want read, initial final, and one byte-floor retry", got)
	}
	srv.mu.Lock()
	retryRequest := append([]byte(nil), srv.requests[2]...)
	srv.mu.Unlock()
	if !strings.Contains(string(retryRequest), "GCS evidence") {
		t.Fatalf("byte-floor retry prompt missing from request: %s", retryRequest)
	}

	_, cached, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !cached.CacheHit || !cached.GCSFloorRetryExhausted || atomic.LoadInt32(&srv.calls) != 3 {
		t.Fatalf("cached analysis = %+v calls=%d", cached, atomic.LoadInt32(&srv.calls))
	}
}

func TestAgentic_GCSFloorRetryMarkerSurvivesForcedFinalization(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "build-log.txt", "offset": 0, "length": 1024}))
	premature := `{"summary":"configuration rejected","is_transient":false,"root_cause":"build-log.txt contains the configuration rejection.","severity":"High","suggested_fix":"Correct the rejected configuration and rerun the job.","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(premature))
	srv.push(200, chatRespToolCall("call_2", "list_artifacts", map[string]interface{}{"path": ""}))
	final := `{"summary":"configuration rejected","is_transient":false,"root_cause":"build-log.txt contains the configuration rejection.","severity":"High","suggested_fix":"Correct the rejected configuration and rerun the job.","relevant_files":["build-log.txt"]}`
	srv.push(200, chatRespFinal(final))

	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": bigPayload(1024)}}
	opts := AgenticOptions{
		MaxIters: 2, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		MinToolCalls: 1, MinGCSBytes: 50_000, CritiqueMaxRetries: 1,
	}
	client := newAgenticTestClient(t, srv.URL)
	in := newTestAgenticInputs(t, browser, opts)
	const key = "agentic:test:gcs-floor-forced-finalize"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.GCSFloorRetryExhausted || analysis.GCSBytes >= opts.MinGCSBytes || !analysis.CritiquePassed {
		t.Fatalf("analysis = %+v", analysis)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 4 {
		t.Fatalf("model calls = %d, want read, initial final, last-iteration tool, and forced final", got)
	}
	_, cached, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !cached.CacheHit || !cached.GCSFloorRetryExhausted || atomic.LoadInt32(&srv.calls) != 4 {
		t.Fatalf("cached analysis = %+v calls=%d", cached, atomic.LoadInt32(&srv.calls))
	}
}

func TestAgentic_OldCacheWithoutEvidenceMarkerRetainsGCSFloor(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "read_artifact", map[string]interface{}{"path": "build-log.txt"}))
	srv.push(200, chatRespFinal(`{"summary":"fresh","is_transient":false,"root_cause":"build-log.txt contains the initiating failure","severity":"High","suggested_fix":"Correct the failing configuration and rerun the job.","relevant_files":["build-log.txt"]}`))

	cacheDir := t.TempDir()
	newClient := func() *Client {
		return NewClientWithOptions(Options{Token: "test-token", CacheDir: cacheDir, Endpoint: srv.URL, Model: "claude-test"})
	}
	client := newClient()
	const key = "agentic:test:old-evidence-marker"
	if err := client.Cache().Set(key, agenticCacheData{
		analysisResponse: analysisResponse{
			Summary: "old", RootCause: "old", Severity: "Low", SuggestedFix: "Retry.",
		},
		ToolCalls: 1, GCSBytes: 1,
		CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		ModelHash: client.modelFingerprint(), PromptHash: PromptFingerprint("sys"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.Cache().Save(); err != nil {
		t.Fatal(err)
	}

	reloaded := newClient()
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("initiating failure\n")}}
	opts := AgenticOptions{
		MaxIters: 4, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		MinGCSBytes: 10,
	}
	_, analysis, err := reloaded.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if analysis.CacheHit {
		t.Fatal("old unmarked cache entry unexpectedly bypassed byte floor")
	}
	if analysis.EvidencePlanCovered {
		t.Fatal("unprofiled analysis unexpectedly marked evidence coverage")
	}
	if analysis.GCSBytes < opts.MinGCSBytes {
		t.Fatalf("GCSBytes = %d, want at least %d", analysis.GCSBytes, opts.MinGCSBytes)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("model calls = %d, want reanalysis", got)
	}
}

func TestAgentic_EvidencePlanCoverageDoesNotBypassMinToolCalls(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	path := "artifacts/issuer.yaml"
	final := `{"summary":"x509","is_transient":false,"root_cause":"x509 issuer mismatch shown in artifacts/issuer.yaml","severity":"High","suggested_fix":"Update the issuer with the correct CA and redeploy.","relevant_files":["artifacts/issuer.yaml"]}`
	srv.push(200, chatRespToolCall("call_1", "tail_artifact", map[string]interface{}{"path": path, "lines": 200}))
	srv.push(200, chatRespFinal(final))
	srv.push(200, chatRespToolCall("call_2", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespFinal(final))

	set := loadAgenticSkillsForTest(t, map[string]string{
		"x509": `
id: x509
triggers: ["x509"]
required_evidence:
  - id: issuer
    any_of: ["issuer\\.yaml$"]
`,
	})
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{path: []byte("kind: Issuer\n")}}}
	in := newTestAgenticInputs(t, browser, AgenticOptions{
		MaxIters: 6, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		MinToolCalls: 2, MinGCSBytes: 50_000, CritiqueMaxRetries: 2,
	})
	in.Skills = set
	in.FailureSignal = "x509 failure"
	_, analysis, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(context.Background(), in, "agentic:test:evidence-coverage-calls", "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.EvidencePlanCovered || analysis.ToolCalls != 2 {
		t.Fatalf("analysis = %+v, want covered plan and two calls", analysis)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 4 {
		t.Fatalf("model calls = %d, want read, premature final, list, final", got)
	}
	srv.mu.Lock()
	nudgeRequest := append([]byte(nil), srv.requests[2]...)
	srv.mu.Unlock()
	if !strings.Contains(string(nudgeRequest), "only 1 tool call(s)") {
		t.Fatalf("nudge did not preserve tool-call floor: %s", nudgeRequest)
	}
	if strings.Contains(string(nudgeRequest), "GCS evidence") {
		t.Fatalf("covered byte floor was included in nudge: %s", nudgeRequest)
	}
}

func TestEvidencePlanCoverageRequiresCompleteInitialScan(t *testing.T) {
	set := loadAgenticSkillsForTest(t, map[string]string{
		"profiled": `
id: profiled
triggers: ["profiled"]
required_evidence:
  - id: log
    any_of: ["failure\\.log$"]
  - id: absent
    any_of: ["junit.*\\.xml$"]
`,
	})
	signal := "profiled failure"
	paths := []string{"logs/failure.log"}
	plan := set.Plan(signal, paths, evidenceplan.CandidatePathLimit)
	base := agentState{
		skillSet: set, initialFailureSignal: signal, initialEvidencePlan: plan,
		initialArtifactTree:   artifactTreeSnapshot{paths: paths},
		evidenceArtifactsFull: map[string]bool{"logs/failure.log": true},
	}
	if !base.evidencePlanCovered() {
		t.Fatal("complete initial scan and read should cover plan")
	}
	cases := []struct {
		name   string
		mutate func(*agentState)
	}{
		{name: "skills unavailable", mutate: func(state *agentState) { state.skillSet = nil }},
		{name: "no matched recipe", mutate: func(state *agentState) {
			state.initialFailureSignal = "unrelated failure"
			state.initialEvidencePlan = set.Plan(state.initialFailureSignal, paths, evidenceplan.CandidatePathLimit)
		}},
		{name: "empty plan", mutate: func(state *agentState) { state.initialEvidencePlan = nil }},
		{name: "scan failed", mutate: func(state *agentState) { state.initialArtifactTree.failed = true }},
		{name: "scan truncated", mutate: func(state *agentState) { state.initialArtifactTree.truncated = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := base
			tc.mutate(&state)
			if state.evidencePlanCovered() {
				t.Fatal("incomplete plan state unexpectedly covered")
			}
			if !evalFloors(&state, AgenticOptions{MinGCSBytes: 1}).gcsUnmet {
				t.Fatal("incomplete plan state unexpectedly bypassed GCS floor")
			}
		})
	}
}

func TestFloorStatusTraceStatus(t *testing.T) {
	tests := []struct {
		name string
		in   floorStatus
		want string
	}{
		{name: "met", in: floorStatus{}, want: ""},
		{name: "tool calls", in: floorStatus{callsUnmet: true}, want: "tool_calls"},
		{name: "GCS bytes", in: floorStatus{gcsUnmet: true}, want: "gcs_bytes"},
		{name: "both", in: floorStatus{callsUnmet: true, gcsUnmet: true}, want: "tool_calls+gcs_bytes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.traceStatus(); got != tc.want {
				t.Fatalf("traceStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDispatchAgenticToolEvidenceReadsRequireNonEmptyContent(t *testing.T) {
	registry, enabled := newTestRegistry(t)
	browser := &fakeBrowser{files: map[string][]byte{
		"logs/content.log":    []byte("failure\n"),
		"logs/empty.log":      {},
		"logs/whitespace.log": []byte(" \n\t"),
		"logs/grep.log":       []byte("healthy\n"),
		"logs/blank.log":      []byte("\n"),
	}}
	cases := []struct {
		name string
		tool string
		args map[string]interface{}
		want bool
	}{
		{name: "non-empty read", tool: "read_artifact", args: map[string]interface{}{"path": "logs/content.log"}, want: true},
		{name: "empty read", tool: "read_artifact", args: map[string]interface{}{"path": "logs/empty.log"}},
		{name: "whitespace read", tool: "tail_artifact", args: map[string]interface{}{"path": "logs/whitespace.log"}},
		{name: "failed read", tool: "read_artifact", args: map[string]interface{}{"path": "logs/missing.log"}},
		{name: "grep with matches", tool: "grep_artifact", args: map[string]interface{}{"path": "logs/grep.log", "pattern": "healthy"}, want: true},
		{name: "grep without matches", tool: "grep_artifact", args: map[string]interface{}{"path": "logs/grep.log", "pattern": "failure"}},
		{name: "grep blank match", tool: "grep_artifact", args: map[string]interface{}{"path": "logs/blank.log", "pattern": "^$", "context_lines": 0}},
		{name: "listing only", tool: "list_artifacts", args: map[string]interface{}{"path": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arguments, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			state := &agentState{
				browser: browser, registry: registry, enabledTools: enabled,
				opts:                  AgenticOptions{ModelByteBudget: 100_000, GCSByteBudget: 100_000},
				evidenceArtifactsFull: map[string]bool{},
			}
			dispatchAgenticTool(context.Background(), state, modelToolCall{
				ID: "call", Type: "function", Function: modelFunction{Name: tc.tool, Arguments: string(arguments)},
			})
			path, _ := tc.args["path"].(string)
			got := state.evidenceArtifactsFull[NormalizeArtifactCitation(path)]
			if got != tc.want {
				t.Fatalf("evidence read recorded = %t, want %t; reads=%v", got, tc.want, state.evidenceArtifactsFull)
			}
		})
	}
}

// TestAgToolDocs_AntiPuntAnchors pins the anti-punt language in agToolDocs
// that drives weaker models to investigate via tools rather than emit
// investigation TODOs in suggested_fix.
func TestAgToolDocs_AntiPuntAnchors(t *testing.T) {
	required := []string{
		"Investigation is YOUR job",
		"diagnostic or information-gathering task",
		"still cannot identify a concrete remediation",
	}
	for _, s := range required {
		if !strings.Contains(agToolDocs, s) {
			t.Errorf("agToolDocs missing required anchor %q\nfull text:\n%s", s, agToolDocs)
		}
	}
}

// TestAgToolDocs_TransientTriageAnchors pins the transient-triage step that
// tells the agent to honor the project's known-transient classes and stop
// before drilling, so the anti-punt / deep-investigation framing does not
// override the consumer's transient rules and flag infra flake as a real bug.
func TestAgToolDocs_TransientTriageAnchors(t *testing.T) {
	required := []string{
		"Triage for a known transient FIRST",
		"set is_transient=true and stop",
		"manufacture a remediation for infrastructure flake",
		"rule out a known-transient class",
		"classify by EVIDENCE, not by the string",
		"absence of a specific cause favors transient",
	}
	for _, s := range required {
		if !strings.Contains(agToolDocs, s) {
			t.Errorf("agToolDocs missing transient-triage anchor %q\nfull text:\n%s", s, agToolDocs)
		}
	}
}

// TestEnginePrompts_ProjectAgnostic guards the engine/consumer split. Engine
// prompts must not name project-specific artifacts or components.
func TestEnginePrompts_ProjectAgnostic(t *testing.T) {
	forbidden := []string{
		"CAPZ", "CAPI", "AzureMachine", "azureserviceoperator",
		"artifacts/clusters", "cloud-init", "etcd-join",
	}
	corpus := map[string]string{
		"BasePrompt": BasePrompt,
		"agToolDocs": agToolDocs,
		"floorsNudge": formatFloorsNudge(
			&agentState{calls: 0, gcsBytes: 0},
			AgenticOptions{MinToolCalls: 5, MinGCSBytes: 500_000},
		),
	}
	for name, text := range corpus {
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s contains project-specific token %q; move it to the consumer prompts/system.md", name, f)
			}
		}
	}
}

func TestBasePromptDoesNotContainKubernetesDiagnosticProfile(t *testing.T) {
	if len(BasePrompt) > 2200 {
		t.Fatalf("BasePrompt grew to %d bytes; keep optional diagnostics in skills", len(BasePrompt))
	}
	for _, token := range []string{
		"providerID", "cloud-node-manager", "kube-proxy",
		"CrashLoopBackOff", "MachineDeployment", "ClusterIP",
	} {
		if strings.Contains(BasePrompt, token) {
			t.Errorf("BasePrompt contains Kubernetes diagnostic token %q; keep it in the conditional profile", token)
		}
	}
}

// TestForceFinalizePrompt_JSONOnlyAnchor pins the JSON-only instruction used
// when forced finalization retries after evidence injection.
func TestForceFinalizePrompt_JSONOnlyAnchor(t *testing.T) {
	for _, s := range []string{
		"Output ONLY the JSON object",
		"must start with { and end with }",
	} {
		if !strings.Contains(agForceFinalizePrompt, s) {
			t.Errorf("agForceFinalizePrompt missing JSON-only anchor %q\nfull text:\n%s", s, agForceFinalizePrompt)
		}
	}
}

// TestResponseFormatFooter_TransientAnchors pins guidance that defers to the
// project's named transient classes.
func TestResponseFormatFooter_TransientAnchors(t *testing.T) {
	required := []string{
		"even if you could keep digging",
		"infrastructure flake is not a code bug",
	}
	for _, s := range required {
		if !strings.Contains(ResponseFormatFooter, s) {
			t.Errorf("ResponseFormatFooter missing transient anchor %q\nfull text:\n%s", s, ResponseFormatFooter)
		}
	}
	// A blanket false bias would override the consumer's transient list.
	forbidden := "When in doubt, set is_transient=false"
	if strings.Contains(ResponseFormatFooter, forbidden) {
		t.Errorf("ResponseFormatFooter reintroduced the blanket bias %q", forbidden)
	}
}

// TestResponseFormatFooter_AntiPuntAnchors pins the tightening of the
// suggested_fix and root_cause schema descriptions. The footer is shared
// by agentic and non-agentic consumers, so wording must stay tool-neutral:
// it forbids diagnostic tasks in suggested_fix without assuming tools are
// available. Tool-specific enforcement lives in agToolDocs.
func TestResponseFormatFooter_AntiPuntAnchors(t *testing.T) {
	required := []string{
		"concrete remediation",
		"Do not list diagnostic or information-gathering tasks",
		"trace the chain back to the underlying cause",
		"No remediation possible from available evidence",
	}
	for _, s := range required {
		if !strings.Contains(ResponseFormatFooter, s) {
			t.Errorf("ResponseFormatFooter missing required anchor %q\nfull text:\n%s", s, ResponseFormatFooter)
		}
	}

	// Forbidden: tool-specific language would be literally false in the
	// generic / prebuilt-evidence consumer mode that shares this footer.
	toolSpecific := []string{"using your tools", "with the tools"}
	for _, s := range toolSpecific {
		if strings.Contains(ResponseFormatFooter, s) {
			t.Errorf("ResponseFormatFooter leaked tool-specific phrase %q (keep tool wording in agToolDocs)\nfull text:\n%s", s, ResponseFormatFooter)
		}
	}
	// The literal fill-in-the-blank example was copied verbatim into
	// suggested_fix by weaker models, so it must not return.
	if strings.Contains(ResponseFormatFooter, "X but not Y") {
		t.Errorf("ResponseFormatFooter reintroduced the copyable 'X but not Y' placeholder")
	}
}

// TestResponseFormatFooter_DepthAnchors pins the depth signals in the
// root_cause schema description that push the model toward a multi-step
// causal chain with quoted log lines and cited artifact paths.
func TestResponseFormatFooter_DepthAnchors(t *testing.T) {
	required := []string{
		"Full causal chain",
		"At least 3-5 sentences",
		"Quote the exact log line",
		"cite the artifact path",
		"verification step",
	}
	for _, s := range required {
		if !strings.Contains(ResponseFormatFooter, s) {
			t.Errorf("ResponseFormatFooter missing required depth anchor %q\nfull text:\n%s", s, ResponseFormatFooter)
		}
	}
}

// A "punt-shaped" suggested_fix is a diagnostic / information-gathering
// TODO list ("Check X. Verify Y. Investigate Z.") instead of a concrete
// remediation. Critique catches this pattern and re-prompts the model.
// See critique.go for the regex; these tests exercise the loop integration.

const puntyFinalJSON = `{"summary":"shallow","is_transient":false,"root_cause":"third CP machine cloud-init empty due to vnet peering mismatch","severity":"High","suggested_fix":"Check the AzureMachine status conditions. Verify cloud-init script execution. Investigate Azure activity logs.","relevant_files":[]}`

const cleanFinalJSON = `{"summary":"deep","is_transient":false,"root_cause":"third CP machine cloud-init empty due to vnet peering mismatch","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 142 to match the staging vnet peering name; reapply and retry.","relevant_files":["kustomize/cluster-template.yaml"]}`

const providerIDPuntFinalJSON = `{"summary":"providerID blocked","is_transient":false,"root_cause":"The worker Node registered, but providerID remained empty because cloud-node-manager could not reach the Kubernetes API.","severity":"High","suggested_fix":"Check the cloud-node-manager logs and verify the API connection.","relevant_files":[]}`

const quotaCleanFinalJSON = `{"summary":"quota blocked","is_transient":false,"root_cause":"Azure subscription quota exhaustion prevented the worker virtual machine from being created.","severity":"High","suggested_fix":"Increase the regional virtual machine quota and recreate the worker.","relevant_files":[]}`

// TestAgentic_Critique_FailRetryPass exercises the happy retry path: the
// model returns a punt-shaped final, critique fails, the loop appends
// feedback and re-prompts, the model returns a clean fix, critique passes,
// and the answer is cached.
func TestAgentic_Critique_FailRetryPass(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: punt-shaped final fails critique and re-prompts.
	srv.push(200, chatRespFinal(puntyFinalJSON))
	// Round 2: after critique feedback, clean final passes critique.
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{
		MaxIters:           5,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		Timeout:            30 * time.Second,
		CritiqueMaxRetries: 2,
	}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:critique-pass", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Errorf("call count = %d, want 2 (punt + retry)", got)
	}
	if summary.Summary != "deep" {
		t.Errorf("expected clean final, got summary=%q", summary.Summary)
	}
	if !strings.Contains(analysis.SuggestedFix, "verified project automation") || len(analysis.RelevantFiles) != 0 || !slices.Contains(analysis.SearchSuggestions, "kustomize/cluster-template.yaml") {
		t.Errorf("expected filtered remediation, got fix=%q relevant=%v suggestions=%v", analysis.SuggestedFix, analysis.RelevantFiles, analysis.SearchSuggestions)
	}

	// Critique-passing answer must be cached: second call hits cache, no
	// extra server hit.
	_, hit, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:critique-pass", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Errorf("call count after cache hit = %d, want 2 (no extra server hit)", got)
	}
	if !hit.CacheHit {
		t.Errorf("CacheHit = false, want true")
	}
}

func TestBoundedCritiqueRepairDeniesWithoutTimeHeadroom(t *testing.T) {
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job"})
	ctx := withAnalysisTrace(context.Background(), trace)
	parsed, ok := tryParseAnalysis(providerIDPuntFinalJSON)
	if !ok {
		t.Fatal("test draft did not parse")
	}
	out := critiqueDraft(parsed, map[string]bool{}, map[string]bool{}, nil, 0)
	state := &agentState{
		deadline: time.Now().Add(100 * time.Millisecond), recentModelRequest: 100 * time.Millisecond,
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{}, evidenceArtifactsFull: map[string]bool{},
		bestDraft: &critiqueDraftCandidate{parsed: parsed, content: providerIDPuntFinalJSON, quality: critiqueQualityFor(out), attempt: 1},
	}
	retries := &critiqueRetryBudget{max: 1}
	got := (&Client{}).runBoundedCritiqueRepair(ctx, state, nil, providerIDPuntFinalJSON, nil, parsed, out, AgenticOptions{}, retries)
	trace.Finish("success", nil)
	if got.RootCause != parsed.RootCause || retries.used != 0 {
		t.Fatalf("time denial changed draft or spent retry: got=%+v retries=%d", got, retries.used)
	}
	found := false
	for _, event := range store.Snapshot().Traces[0].Events {
		if event.Kind == "critique_retry_denied" && event.RetryDeniedReason == "time_headroom" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing time_headroom denial trace")
	}
}

func TestAgentic_CritiqueRetryCannotReplaceDiagnosisWithoutEvidence(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(providerIDPuntFinalJSON))
	srv.push(200, chatRespFinal(quotaCleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	selected := 0
	in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
		MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 1,
	})
	in.DraftSelectionObserver = func(attempt int) { selected = attempt }
	key := "agentic:test:preserve-providerid-diagnosis"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if !strings.Contains(analysis.RootCause, "providerID") || strings.Contains(analysis.RootCause, "quota") {
		t.Fatalf("wrong retry replaced the diagnosis: %q", analysis.RootCause)
	}
	if analysis.CritiquePassed || selected != 1 {
		t.Fatalf("selection telemetry = selected:%d critique_passed:%v", selected, analysis.CritiquePassed)
	}
	if _, ok := client.Cache().Get(key); ok {
		t.Fatal("selected failing draft was cached")
	}
}

func TestAgentic_CritiqueRetryCanChangeDiagnosisAfterEvidenceRead(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	initial := `{"summary":"providerID blocked","is_transient":false,"root_cause":"quota.log may show why providerID remained empty","severity":"High","suggested_fix":"Check quota.log before changing the deployment.","relevant_files":[]}`
	srv.push(200, chatRespFinal(initial))
	srv.push(200, chatRespFinal(quotaCleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	selected := 0
	in := newTestAgenticInputs(t, &fakeBrowser{files: map[string][]byte{"quota.log": []byte("regional quota exhausted")}}, AgenticOptions{
		MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 1,
	})
	in.DraftSelectionObserver = func(attempt int) { selected = attempt }
	key := "agentic:test:replace-after-evidence"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if !strings.Contains(analysis.RootCause, "quota") || !analysis.CritiquePassed || selected != 2 {
		t.Fatalf("evidence-backed retry was not selected: selected=%d analysis=%+v", selected, analysis)
	}
	_, hit, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil || !hit.CacheHit {
		t.Fatalf("selected passing draft was not cached: hit=%v err=%v", hit.CacheHit, err)
	}
}

func TestAgentic_CritiqueRetryWithMoreIssuesLoses(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(providerIDPuntFinalJSON))
	worse := `{"summary":"providerID blocked","is_transient":false,"root_cause":"The worker Node registered, but providerID remained empty because manager.log shows cloud-node-manager could not reach the Kubernetes API.","severity":"High","suggested_fix":"Check the cloud-node-manager logs.","relevant_files":[]}`
	srv.push(200, chatRespFinal(worse))

	selected := 0
	in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
		MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 1,
	})
	in.DraftSelectionObserver = func(attempt int) { selected = attempt }
	_, analysis, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(context.Background(), in,
		"agentic:test:more-issues-loses", "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if selected != 1 || analysis.RootCause != "The worker Node registered, but providerID remained empty because cloud-node-manager could not reach the Kubernetes API." {
		t.Fatalf("worse retry selected: selected=%d root=%q", selected, analysis.RootCause)
	}
}

func TestAgentic_CritiqueRetryTieKeepsInitial(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(providerIDPuntFinalJSON))
	tied := `{"summary":"same diagnosis","is_transient":false,"root_cause":"The worker Node registered, but providerID remained empty because cloud-node-manager could not reach the Kubernetes API.","severity":"High","suggested_fix":"Verify the cloud-node-manager API connection.","relevant_files":[]}`
	srv.push(200, chatRespFinal(tied))

	selected := 0
	in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
		MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 1,
	})
	in.DraftSelectionObserver = func(attempt int) { selected = attempt }
	_, analysis, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(context.Background(), in,
		"agentic:test:tie-keeps-initial", "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if selected != 1 || analysis.SuggestedFix != "Check the cloud-node-manager logs and verify the API connection." {
		t.Fatalf("tied retry selected: selected=%d fix=%q", selected, analysis.SuggestedFix)
	}
}

func TestAgentic_FailingPostInjectionDraftKeepsInitial(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	initial := `{"summary":"webhook cert","is_transient":false,"root_cause":"x509 webhook validation failure prevented cluster creation","severity":"High","suggested_fix":"Regenerate the webhook serving certificate and redeploy the controller.","relevant_files":[]}`
	revised := `{"summary":"webhook cert","is_transient":false,"root_cause":"manager.log shows an x509 webhook validation failure prevented cluster creation","severity":"High","suggested_fix":"Check the webhook certificate.","relevant_files":[]}`
	srv.push(200, chatRespFinal(initial))
	srv.push(200, chatRespFinal(revised))

	set := loadAgenticSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook-tls
triggers: ["x509"]
required_evidence:
  - id: webhook-config
    any_of: ["config/webhook/.*\\.yaml"]
`,
	})
	selected := 0
	in := newTestAgenticInputs(t, &fakeBrowser{files: map[string][]byte{
		"config/webhook/manifests.yaml": []byte("webhooks:"),
		"manager.log":                   []byte("unrelated"),
	}}, AgenticOptions{
		MaxIters: 1, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 1,
	})
	in.Skills = set
	in.DraftSelectionObserver = func(attempt int) { selected = attempt }
	client := newAgenticTestClient(t, srv.URL)
	key := "agentic:test:failing-post-injection"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if selected != 1 || analysis.RootCause != "x509 webhook validation failure prevented cluster creation" {
		t.Fatalf("failing post-injection draft selected: selected=%d root=%q", selected, analysis.RootCause)
	}
	if analysis.CritiquePassed {
		t.Fatal("selected initial draft unexpectedly passed critique")
	}
	if _, ok := client.Cache().Get(key); ok {
		t.Fatal("selected failing draft was cached")
	}
}

func TestCompareCritiqueQuality(t *testing.T) {
	base := critiqueQuality{PuntCount: 1}
	tests := []struct {
		name string
		a    critiqueQuality
		b    critiqueQuality
		want int
	}{
		{name: "passing", a: critiqueQuality{Passed: true}, b: base, want: 1},
		{name: "transient conflict", a: base, b: critiqueQuality{TransientConflict: true}, want: 1},
		{name: "unread citations", a: base, b: critiqueQuality{UnreadCitationCount: 1}, want: 1},
		{name: "missing evidence", a: base, b: critiqueQuality{MissingEvidenceCount: 1}, want: 1},
		{name: "punts", a: critiqueQuality{}, b: base, want: 1},
		{name: "tie", a: base, b: base, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareCritiqueQuality(tc.a, tc.b); got != tc.want {
				t.Fatalf("compareCritiqueQuality() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRootCauseMateriallyChanged(t *testing.T) {
	base := "The worker Node registered, but providerID remained empty because cloud-node-manager could not reach the Kubernetes API."
	if rootCauseMateriallyChanged(base, "  the worker node registered, but providerID remained empty because cloud-node-manager could not reach the Kubernetes API. ") {
		t.Fatal("case and whitespace changed the diagnosis")
	}
	if rootCauseMateriallyChanged("cloud-node-manager could not reach the Kubernetes API.", "cloud-node-manager could not reach the Kubernetes API") {
		t.Fatal("terminal punctuation changed the diagnosis")
	}
	if !rootCauseMateriallyChanged(base, base+" The controller log confirms the same API reachability failure.") {
		t.Fatal("diagnosis-token addition was not material")
	}
	if !rootCauseMateriallyChanged(base, "Azure subscription quota exhaustion prevented virtual machine creation.") {
		t.Fatal("different diagnosis was not material")
	}
	if !rootCauseMateriallyChanged("Azure quota exhaustion prevented virtual machine creation.", "Azure authentication failure prevented virtual machine creation.") {
		t.Fatal("near-overlap diagnosis replacement was not material")
	}
	if !rootCauseMateriallyChanged("Azure quota exhaustion prevented virtual machine creation.", "Azure prevented virtual machine creation.") {
		t.Fatal("diagnosis-token deletion was not material")
	}
	if !rootCauseMateriallyChanged("Azure quota exhaustion prevented virtual machine creation.", "Azure quota exhaustion did not prevent virtual machine creation; authentication failure did.") {
		t.Fatal("diagnosis negation was not material")
	}
	if !rootCauseMateriallyChanged("quota exhaustion caused authentication failure", "authentication failure caused quota exhaustion") {
		t.Fatal("causal reordering was not material")
	}
}

func TestRecordEvidenceReadRevisionCountsUniquePaths(t *testing.T) {
	state := &agentState{}
	state.recordEvidenceRead("logs/a.log")
	state.recordEvidenceRead("logs/a.log")
	state.recordEvidenceRead("logs/b.log")
	if state.evidenceRevision != 2 || len(state.evidenceArtifactsFull) != 2 {
		t.Fatalf("evidence ledger = revision:%d paths:%v", state.evidenceRevision, state.evidenceArtifactsFull)
	}
}

func TestAgentic_DraftObserverAbsentIsNoOp(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{
		MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 2,
	}
	_, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:draft-observer-absent", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("call count = %d, want 1", got)
	}
	if !analysis.CritiquePassed {
		t.Fatal("observer absence changed critique acceptance")
	}
}

func TestAgentic_DraftObserverReceivesCopiesInOrder(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 2,
	}
	var observations []DraftObservation
	in := newTestAgenticInputs(t, &fakeBrowser{}, opts)
	in.DraftObserver = func(observation DraftObservation) {
		snapshot := observation
		snapshot.RelevantFiles = append([]string(nil), observation.RelevantFiles...)
		observations = append(observations, snapshot)
		observation.RootCause = "observer mutation"
		observation.SuggestedFix = "observer mutation"
		if len(observation.RelevantFiles) > 0 {
			observation.RelevantFiles[0] = "observer mutation"
		}
	}
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in,
		"agentic:test:draft-observer-order", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations = %d, want 2: %+v", len(observations), observations)
	}
	if observations[0].Attempt != 1 || observations[0].Phase != "initial" || observations[0].PuntCount == 0 {
		t.Fatalf("initial observation = %+v", observations[0])
	}
	if observations[1].Attempt != 2 || observations[1].Phase != "critique_retry" || observations[1].PuntCount != 0 {
		t.Fatalf("revised observation = %+v", observations[1])
	}
	if analysis.RootCause != observations[1].RootCause || strings.Contains(analysis.SuggestedFix, "observer mutation") {
		t.Fatalf("observer mutation affected runtime: analysis=%+v observation=%+v", analysis, observations[1])
	}
	if len(analysis.RelevantFiles) != 0 || !slices.Contains(analysis.SearchSuggestions, "kustomize/cluster-template.yaml") {
		t.Fatalf("published file classification: relevant=%v suggestions=%v", analysis.RelevantFiles, analysis.SearchSuggestions)
	}

	_, hit, err := client.doAnalyzeAgentic(context.Background(), in,
		"agentic:test:draft-observer-order", "sys", "user")
	if err != nil {
		t.Fatalf("cache hit: %v", err)
	}
	if !hit.CacheHit || len(observations) != 2 || atomic.LoadInt32(&srv.calls) != 2 {
		t.Fatalf("observer changed cache behavior: hit=%v observations=%d calls=%d", hit.CacheHit, len(observations), atomic.LoadInt32(&srv.calls))
	}
}

func TestAgentic_DraftObserverContentIsNotTraced(t *testing.T) {
	shrinkCallDelay(t)
	const secret = "DRAFT_ONLY_SENTINEL_42f974"
	final := `{"summary":"` + secret + `","is_transient":false,"root_cause":"` + secret + `","severity":"Low","suggested_fix":"Replace the invalid setting.","relevant_files":[]}`
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(final))

	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
		MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 2,
	})
	in.DraftObserver = func(observation DraftObservation) {
		if !strings.Contains(observation.RootCause, secret) {
			t.Errorf("observer root cause = %q, want sentinel", observation.RootCause)
		}
	}
	_, _, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(ctx, in,
		"agentic:test:draft-observer-trace", "sys", "user")
	trace.Finish("success", err)
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ai_traces.json")
	if err := store.Save(path); err != nil {
		t.Fatalf("save trace: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("trace persisted draft content: %s", data)
	}
}

// TestAgentic_Critique_ExhaustedAcceptedNotCached verifies repeated punt
// drafts publish the last answer but skip caching.
func TestAgentic_Critique_ExhaustedAcceptedNotCached(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// One bounded repair follows the original. The selected failing draft is not cached.
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal(puntyFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{
		MaxIters:           5,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		Timeout:            30 * time.Second,
		CritiqueMaxRetries: 2,
	}
	summary, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:critique-exhausted", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Errorf("call count = %d, want 2 (original + bounded repair)", got)
	}
	if summary.Summary != "shallow" {
		t.Errorf("expected punt-shaped final to be published, got %q", summary.Summary)
	}

	// Critique-failing answers are not cached. A second analysis consumes
	// two new server responses.
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal(puntyFinalJSON))
	before := atomic.LoadInt32(&srv.calls)
	_, _, err = client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		"agentic:test:critique-exhausted", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if atomic.LoadInt32(&srv.calls) == before {
		t.Error("punt-shaped final should not have been cached (expected server hits on retry)")
	}
}

func TestAgentic_CritiqueZeroRetriesMakesNoRepairRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		final   string
		browser artifacts.Browser
	}{
		{name: "punt", final: puntyFinalJSON, browser: &fakeBrowser{}},
		{name: "unread citation", final: hallucinatedFinalJSON, browser: &fakeBrowser{files: map[string][]byte{"manager.log": []byte("controller failed")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shrinkCallDelay(t)
			srv := newScriptedChatServer(t)
			srv.push(200, chatRespFinal(tc.final))
			client := newAgenticTestClient(t, srv.URL)
			key := "agentic:test:zero-retry:" + tc.name
			_, analysis, err := client.doAnalyzeAgentic(context.Background(),
				newTestAgenticInputs(t, tc.browser, AgenticOptions{
					MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
					Timeout: 30 * time.Second, CritiqueMaxRetries: 0,
				}), key, "sys", "user")
			if err != nil {
				t.Fatalf("doAnalyzeAgentic: %v", err)
			}
			if got := atomic.LoadInt32(&srv.calls); got != 1 {
				t.Fatalf("call count = %d, want 1", got)
			}
			if analysis.CritiquePassed {
				t.Fatal("critique-failing result unexpectedly passed")
			}
			if _, ok := client.Cache().Get(key); !ok {
				t.Fatal("zero-retry critique result was not cached")
			}
			before := atomic.LoadInt32(&srv.calls)
			_, cached, err := client.doAnalyzeAgentic(context.Background(),
				newTestAgenticInputs(t, tc.browser, AgenticOptions{
					MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
					Timeout: 30 * time.Second, CritiqueMaxRetries: 0,
				}), key, "sys", "user")
			if err != nil {
				t.Fatalf("cached doAnalyzeAgentic: %v", err)
			}
			if got := atomic.LoadInt32(&srv.calls); got != before {
				t.Fatalf("cached call count = %d, want %d", got, before)
			}
			if !cached.CacheHit || cached.CritiquePassed || cached.CritiqueVersion != currentCritiqueVersion {
				t.Fatalf("cached analysis = %+v", cached)
			}
		})
	}
}

func TestAgentic_CritiqueBudgetSharedAcrossLoopAndPostLoop(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(puntyFinalJSON))
	final := `{"summary":"webhook cert","is_transient":false,"root_cause":"x509 webhook validation failure prevented cluster creation","severity":"High","suggested_fix":"Regenerate the webhook serving certificate and redeploy the controller.","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))
	srv.push(200, chatRespFinal(final))

	set := loadAgenticSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook-tls
triggers: ["x509"]
required_evidence:
  - id: webhook-config
    any_of: ["config/webhook/.*\\.yaml"]
`,
	})
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{
		"config/webhook/manifests.yaml": []byte("webhooks:"),
	}}}
	in := newTestAgenticInputs(t, browser, AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 1,
	})
	in.Skills = set
	key := "agentic:test:shared-critique-budget"
	_, analysis, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("call count = %d, want 2", got)
	}
	if len(browser.tailCalls) != 0 {
		t.Fatalf("post-loop repair ignored exhausted budget: %v", browser.tailCalls)
	}
	if analysis.CritiquePassed {
		t.Fatal("missing-evidence draft unexpectedly passed")
	}
}

func TestAgentic_PostLoopEvidenceRepairUsesSharedBudget(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	final := `{"summary":"webhook cert","is_transient":false,"root_cause":"x509 webhook validation failure prevented cluster creation","severity":"High","suggested_fix":"Regenerate the webhook serving certificate and redeploy the controller.","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))
	srv.push(200, chatRespFinal(final))

	set := loadAgenticSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook-tls
triggers: ["x509"]
required_evidence:
  - id: webhook-config
    any_of: ["config/webhook/.*\\.yaml"]
`,
	})
	browser := &fakeBrowser{files: map[string][]byte{
		"config/webhook/manifests.yaml": []byte("webhooks:"),
	}}
	in := newTestAgenticInputs(t, browser, AgenticOptions{
		MaxIters: 1, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 1,
	})
	in.Skills = set
	in.FailureSignal = "x509"
	var observations []DraftObservation
	in.DraftObserver = func(observation DraftObservation) { observations = append(observations, observation) }
	client := newAgenticTestClient(t, srv.URL)
	key := "agentic:test:post-loop-shared-budget"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Fatalf("call count = %d, want 3", got)
	}
	if !analysis.CritiquePassed || !analysis.EvidencePlanCovered {
		t.Fatalf("repaired analysis = %+v", analysis)
	}
	if len(observations) != 2 || observations[0].Phase != "finalize" || observations[1].Phase != "critique_retry" {
		t.Fatalf("observations = %+v", observations)
	}
	_, hit, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatalf("cache hit: %v", err)
	}
	if !hit.CacheHit || atomic.LoadInt32(&srv.calls) != 3 {
		t.Fatalf("passing repaired result was not cached: hit=%v calls=%d", hit.CacheHit, atomic.LoadInt32(&srv.calls))
	}
}

func TestAgentic_UnparseableEvidenceRepairCannotExceedBudget(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	final := `{"summary":"webhook cert","is_transient":false,"root_cause":"x509 webhook validation failure prevented cluster creation","severity":"High","suggested_fix":"Regenerate the webhook serving certificate and redeploy the controller.","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))
	srv.push(200, chatRespFinal("not json"))
	srv.push(200, chatRespFinal(final))

	set := loadAgenticSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook-tls
triggers: ["x509"]
required_evidence:
  - id: webhook-config
    any_of: ["config/webhook/.*\\.yaml"]
`,
	})
	in := newTestAgenticInputs(t, &fakeBrowser{files: map[string][]byte{
		"config/webhook/manifests.yaml": []byte("webhooks:"),
	}}, AgenticOptions{
		MaxIters: 1, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 1,
	})
	in.Skills = set
	client := newAgenticTestClient(t, srv.URL)
	key := "agentic:test:unparseable-repair-budget"
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Fatalf("call count = %d, want 3", got)
	}
	if summary.Summary != "webhook cert" || analysis.CritiquePassed {
		t.Fatalf("unparseable repair did not retain the prior uncached draft: summary=%+v analysis=%+v", summary, analysis)
	}
	if _, ok := client.Cache().Get(key); ok {
		t.Fatal("failing retained draft was cached")
	}
}

func TestAgentic_UnparseableInLoopRepairCannotExceedBudget(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal("not json"))
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	key := "agentic:test:unparseable-in-loop-budget"
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
			MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
			Timeout: 30 * time.Second, CritiqueMaxRetries: 1,
		}), key, "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("call count = %d, want 2", got)
	}
	if summary.Summary != "shallow" || analysis.CritiquePassed {
		t.Fatalf("unparseable repair did not retain prior draft: summary=%+v analysis=%+v", summary, analysis)
	}
	if _, ok := client.Cache().Get(key); ok {
		t.Fatal("failing retained draft was cached")
	}
}

func TestAgentic_BlankInLoopRepairCannotExceedBudget(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal(""))
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	key := "agentic:test:blank-in-loop-budget"
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
			MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
			Timeout: 30 * time.Second, CritiqueMaxRetries: 1,
		}), key, "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("call count = %d, want 2", got)
	}
	if summary.Summary != "shallow" || analysis.CritiquePassed {
		t.Fatalf("blank repair did not retain prior draft: summary=%+v analysis=%+v", summary, analysis)
	}
	if _, ok := client.Cache().Get(key); ok {
		t.Fatal("failing retained draft was cached")
	}
}

func TestAgentic_TwoUnparseableInLoopRepairsRetainPriorDraft(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespFinal("not json"))
	srv.push(200, chatRespFinal("still not json"))
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	key := "agentic:test:two-unparseable-in-loop-repairs"
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
			MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
			Timeout: 30 * time.Second, CritiqueMaxRetries: 2,
		}), key, "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("call count = %d, want 2", got)
	}
	if summary.Summary != "shallow" || analysis.CritiquePassed {
		t.Fatalf("two unparseable repairs did not retain prior draft: summary=%+v analysis=%+v", summary, analysis)
	}
	if analysis.SuggestedFix == "Unable to parse structured response" {
		t.Fatalf("published synthesized parse failure instead of prior draft: %+v", analysis)
	}
	if _, ok := client.Cache().Get(key); ok {
		t.Fatal("failing retained draft was cached")
	}
}

func TestAgentic_ToolRepairUsesRemainingBudgetAfterUnparseableFinalize(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(puntyFinalJSON))
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespToolCall("c2", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespToolCall("c3", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespFinal("not json"))
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{dirs: map[string][]string{"": {"artifacts"}}}, AgenticOptions{
			MaxIters: 1, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
			Timeout: 30 * time.Second, CritiqueMaxRetries: 2,
		}), "agentic:test:tool-repair-unparseable-finalize", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("call count = %d, want 2", got)
	}
	if summary.Summary != "shallow" || analysis.CritiquePassed {
		t.Fatalf("bounded repair unexpectedly reopened the tool loop: summary=%+v analysis=%+v", summary, analysis)
	}
}

func TestCritiqueRetryBudgetIsNotCached(t *testing.T) {
	data, err := json.Marshal(agenticCacheData{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(data)), "retry") {
		t.Fatalf("agentic cache shape contains retry state: %s", data)
	}
}

// TestAgentic_Critique_FinalizeRoundOutputCritiqued verifies forced-finalize
// output is critique-checked before publish and cache.
func TestAgentic_Critique_FinalizeRoundOutputCritiqued(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// MaxIters=2: model only calls tools, so MaxIters triggers runFinalizeRound.
	srv.push(200, chatRespToolCall("c1", "list_artifacts", map[string]interface{}{"path": ""}))
	srv.push(200, chatRespToolCall("c2", "list_artifacts", map[string]interface{}{"path": ""}))
	// runFinalizeRound: model emits a clean (non-punt) final.
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{dirs: map[string][]string{"": {"artifacts"}}}
	opts := AgenticOptions{
		MaxIters:           2,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		Timeout:            30 * time.Second,
		CritiqueMaxRetries: 2,
	}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts),
		"agentic:test:finalize-clean", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "deep" {
		t.Errorf("expected finalize-round clean answer, got %q", summary.Summary)
	}
	// The clean answer must be stamped onto AIAnalysis for build-cache reuse.
	if !analysis.CritiquePassed {
		t.Errorf("CritiquePassed = false, want true (finalize-round clean answer must be critique-checked)")
	}

	// Critique-passing finalize-round answers cache normally.
	_, hit, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts),
		"agentic:test:finalize-clean", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count after cache hit = %d, want 3 (no extra server hit)", got)
	}
	if !hit.CacheHit {
		t.Errorf("CacheHit = false, want true")
	}
	if !hit.CritiquePassed {
		t.Errorf("cache-hit CritiquePassed = false, want true (cached telemetry must round-trip)")
	}
}

// TestAgentic_Critique_RetryAllowsToolCallThenFinal verifies critique retry
// budget covers a tool round followed by a new final answer.
func TestAgentic_Critique_RetryAllowsToolCallThenFinal(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: punt-shaped final fails critique and re-prompts.
	srv.push(200, chatRespFinal(puntyFinalJSON))
	// Round 2: model reads an artifact in response to critique feedback.
	srv.push(200, chatRespToolCall("c1", "read_artifact", map[string]interface{}{
		"path": "build-log.txt", "offset": 0, "length": 256,
	}))
	// Round 3: model re-emits with a clean final that passes critique.
	srv.push(200, chatRespFinal(cleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte("the error context\n")},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	// MaxIters=1 leaves only one initial iteration; the critique retry budget
	// must cover the tool call and clean final.
	opts := AgenticOptions{
		MaxIters:           1,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		Timeout:            30 * time.Second,
		CritiqueMaxRetries: 1,
	}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts),
		"agentic:test:critique-toolcall", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "shallow" {
		t.Errorf("expected prior draft after non-evidence tool response, got %q", summary.Summary)
	}
	if analysis.CritiquePassed {
		t.Errorf("CritiquePassed = true, want false")
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Errorf("call count = %d, want 2 (punt + bounded finalize)", got)
	}
}

func TestAgentic_BoundedRepairAllowsOneToolTurnThenFinal(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	initial := `{"summary":"s","is_transient":false,"root_cause":"build-log.txt shows the controller failed","severity":"High","suggested_fix":"Update the controller configuration.","relevant_files":[]}`
	srv.push(200, chatRespFinal(initial))
	srv.push(200, chatRespToolCall("c1", "read_artifact", map[string]interface{}{"path": "build-log.txt"}))
	clean := `{"summary":"deep","is_transient":false,"root_cause":"controller configuration mismatch","severity":"High","suggested_fix":"Update the controller configuration.","relevant_files":[]}`
	srv.push(200, chatRespFinal(clean))
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("mismatch")}}, treeResponses: []treeResponse{{truncated: true}, {truncated: true}}}
	client := newAgenticTestClient(t, srv.URL)
	key := "agentic:test:bounded-tool-turn"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second, CritiqueMaxRetries: 1}), key, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Fatalf("call count = %d, want 3", got)
	}
	if !analysis.CritiquePassed || analysis.RootCause != "controller configuration mismatch" {
		t.Fatalf("bounded Tool repair failed: %+v", analysis)
	}
	if _, ok := client.Cache().Get(key); !ok {
		t.Fatal("passing bounded repair was not cached")
	}
}

// TestCritiqueDraft_FeedbackTruncatesLongFix verifies long quoted fixes are
// truncated while matched phrases remain listed.
func TestCritiqueDraft_FeedbackTruncatesLongFix(t *testing.T) {
	prefix := "Check the AzureMachine status. "
	long := prefix + strings.Repeat("Additional details and context. ", 200)
	if len(long) <= feedbackQuoteLimit {
		t.Fatalf("test setup: long fix is too short (%d <= %d)", len(long), feedbackQuoteLimit)
	}
	out := critiqueDraft(analysisResponse{SuggestedFix: long}, nil, nil, nil, 0)
	if out.Passed {
		t.Fatalf("expected punt")
	}
	if !strings.Contains(out.Feedback, "… [truncated]") {
		t.Errorf("Feedback missing truncation marker for long fix\nlen(feedback)=%d", len(out.Feedback))
	}
	// Truncated quote is bounded; the rest of the feedback template is
	// fixed-size, so the total length should not grow linearly with the
	// input. Empirical bound: template ~1500 chars + quote limit + match
	// list. Leave generous slack.
	if got := len(out.Feedback); got > feedbackQuoteLimit+3000 {
		t.Errorf("Feedback unexpectedly long: %d chars (limit ~%d)", got, feedbackQuoteLimit+3000)
	}
}

// hallucinatedFinalJSON has a clean suggested_fix but cites unread manager.log.
const hallucinatedFinalJSON = `{"summary":"deep","is_transient":false,"root_cause":"manager.log shows the controller failed to reconcile the AzureMachine","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 142 to match the staging vnet peering name; reapply.","relevant_files":[]}`

// readThenCleanFinalJSON cites build-log.txt at the exact line returned by grep.
const readThenCleanFinalJSON = `{"summary":"deep","is_transient":false,"root_cause":"build-log.txt:42 shows the vnet peering name mismatch","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 142 to match the staging vnet peering name; reapply.","relevant_files":["build-log.txt"],"evidence_citations":[{"path":"build-log.txt","line_start":42,"line_end":42,"quote":"vnet peering mismatch"}]}`

// TestAgentic_HallucinationRetry verifies an unread-citation critique can
// recover after the model reads and cites a matching artifact.
func TestAgentic_HallucinationRetry(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: model emits final citing manager.log (never read).
	srv.push(200, chatRespFinal(hallucinatedFinalJSON))
	// Round 2: after critique feedback, model greps the exact build-log line.
	srv.push(200, chatRespToolCall("c1", "grep_artifact", map[string]interface{}{
		"path": "build-log.txt", "pattern": "vnet peering mismatch", "context_lines": 0,
	}))
	// Round 3: re-emit citing build-log.txt, which passes.
	srv.push(200, chatRespFinal(readThenCleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte(strings.Repeat("\n", 41) + "vnet peering mismatch on line 42\n")},
		dirs:  map[string][]string{"": {"artifacts"}},
	}
	opts := AgenticOptions{
		MaxIters:           2,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		Timeout:            30 * time.Second,
		CritiqueMaxRetries: 2,
	}
	summary, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts),
		"agentic:test:halluc-retry", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if summary.Summary != "deep" {
		t.Errorf("expected clean re-emit after hallucination retry, got %q", summary.Summary)
	}
	if !analysis.CritiquePassed {
		t.Errorf("CritiquePassed = false, want true")
	}
	if analysis.CritiqueVersion != currentCritiqueVersion {
		t.Errorf("CritiqueVersion = %d, want %d", analysis.CritiqueVersion, currentCritiqueVersion)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count = %d, want 3 (hallucination + read + clean)", got)
	}
}

// TestAgentic_CacheInvalidatedByCritiqueVersionBump verifies entries accepted
// before ranked planning and candidate-directed repair are re-analyzed.
func TestAgentic_CacheInvalidatedByCritiqueVersionBump(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// First call: clean fix with no citations passes critique.
	noCitations := `{"summary":"deep","is_transient":false,"root_cause":"vnet peering misconfigured","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 42; reapply.","relevant_files":["kustomize/cluster-template.yaml"]}`
	srv.push(200, chatRespFinal(noCitations))

	client := newAgenticTestClient(t, srv.URL)
	opts := AgenticOptions{
		MaxIters:           5,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		Timeout:            30 * time.Second,
		CritiqueMaxRetries: 2,
	}
	const key = "agentic:test:version-invalidate"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		key, "sys", "user")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !analysis.CritiquePassed || analysis.CritiqueVersion != currentCritiqueVersion {
		t.Fatalf("first call: CritiquePassed=%v CritiqueVersion=%d, want %d", analysis.CritiquePassed, analysis.CritiqueVersion, currentCritiqueVersion)
	}

	// Simulate an entry accepted under the immediately preceding contract.
	raw, ok := client.Cache().Get(key)
	if !ok {
		t.Fatalf("first call should have written cache entry")
	}
	var cached agenticCacheData
	if err := json.Unmarshal(raw, &cached); err != nil {
		t.Fatalf("unmarshal cache: %v", err)
	}
	cached.CritiqueVersion = currentCritiqueVersion - 1
	if err := client.Cache().Set(key, cached); err != nil {
		t.Fatalf("re-write cache: %v", err)
	}

	// Second call rejects the pre-ranked-plan entry and re-analyzes.
	srv.push(200, chatRespFinal(noCitations))
	before := atomic.LoadInt32(&srv.calls)
	_, analysis2, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, &fakeBrowser{}, opts),
		key, "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if atomic.LoadInt32(&srv.calls) == before {
		t.Error("expected re-analysis after CritiqueVersion bump (server hit), got cache hit")
	}
	if analysis2.CritiqueVersion != currentCritiqueVersion {
		t.Errorf("post-re-analysis CritiqueVersion = %d, want %d", analysis2.CritiqueVersion, currentCritiqueVersion)
	}
}

// loadAgenticSkillsForTest creates a temp dir with one or more YAML
// recipes and returns the loaded skills.Set, ready for stamping onto
// AgenticInputs. Mirrors loadSkillsForTest in critique_test.go but
// kept package-local-private so the two test files don't have to
// share a helper.
func loadAgenticSkillsForTest(t *testing.T, recipes map[string]string) *skills.Set {
	t.Helper()
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range recipes {
		p := filepath.Join(skillsDir, name+".yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	set, err := skills.Load(dir)
	if err != nil {
		t.Fatalf("skills.Load: %v", err)
	}
	return set
}

func TestAgentic_CacheRetainedBySkillSetHashChange(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	cleanFinal := `{"summary":"deep","is_transient":false,"root_cause":"vnet peering misconfigured","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 42; reapply.","relevant_files":["kustomize/cluster-template.yaml"]}`
	srv.push(200, chatRespFinal(cleanFinal))

	client := newAgenticTestClient(t, srv.URL)
	set := loadAgenticSkillsForTest(t, map[string]string{
		"unrelated": `
id: unrelated-recipe
triggers: ["never-matches-this-draft"]
required_evidence:
  - id: g
    any_of: ["x"]
`,
	})
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 2,
	}
	in := newTestAgenticInputs(t, &fakeBrowser{}, opts)
	in.Skills = set

	const key = "agentic:test:skillhash-retained"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !analysis.CritiquePassed || analysis.SkillSetHash != set.Hash() {
		t.Fatalf("first analysis = %+v, skill hash = %q", analysis, set.Hash())
	}

	edited := loadAgenticSkillsForTest(t, map[string]string{
		"unrelated": `
id: unrelated-recipe
triggers: ["still-does-not-match-this-draft-but-different-pattern"]
required_evidence:
  - id: g
    any_of: ["x"]
`,
	})
	if edited.Hash() == set.Hash() {
		t.Fatal("test setup: edited skill set should have different hash")
	}

	in2 := newTestAgenticInputs(t, &fakeBrowser{}, opts)
	in2.Skills = edited
	_, analysis2, err := client.doAnalyzeAgentic(context.Background(), in2, key, "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !analysis2.CacheHit {
		t.Fatal("skill-set change missed the cache")
	}
	if analysis2.SkillSetHash != set.Hash() {
		t.Fatalf("cached skill provenance = %q, want %q", analysis2.SkillSetHash, set.Hash())
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("server calls = %d, want 1", got)
	}
}

func TestLimitToolCalls(t *testing.T) {
	three := []modelToolCall{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	t.Run("disabled passes through", func(t *testing.T) {
		kept, dropped := limitToolCalls(three, false)
		if len(kept) != 3 || dropped != 0 {
			t.Errorf("got kept=%d dropped=%d, want 3/0", len(kept), dropped)
		}
	})
	t.Run("enabled keeps first only", func(t *testing.T) {
		kept, dropped := limitToolCalls(three, true)
		if len(kept) != 1 || kept[0].ID != "a" || dropped != 2 {
			t.Errorf("got kept=%v dropped=%d, want [a]/2", kept, dropped)
		}
	})
	t.Run("enabled single call unchanged", func(t *testing.T) {
		kept, dropped := limitToolCalls(three[:1], true)
		if len(kept) != 1 || dropped != 0 {
			t.Errorf("got kept=%d dropped=%d, want 1/0", len(kept), dropped)
		}
	})
	t.Run("empty is safe", func(t *testing.T) {
		kept, dropped := limitToolCalls(nil, true)
		if len(kept) != 0 || dropped != 0 {
			t.Errorf("got kept=%d dropped=%d, want 0/0", len(kept), dropped)
		}
	})
}

// TestAgentic_SingleToolCall_EchoesOneToolCall verifies that when the model
// returns two parallel tool calls and SingleToolCall is on, the loop executes
// only the first and the echoed history sent on the next request carries a
// single tool_call for chat templates that reject multiple.
func TestAgentic_SingleToolCall_EchoesOneToolCall(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Round 1: model emits two tool calls at once.
	srv.push(200, chatRespTwoToolCalls("call_1", "list_artifacts", "call_2", "list_artifacts"))
	// Round 2: model finalizes.
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("x")}}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second, SingleToolCall: true}

	_, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:job:1:stc", "system", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if analysis.ToolCalls != 1 {
		t.Errorf("tool_calls = %d, want 1 (second parallel call dropped)", analysis.ToolCalls)
	}
	// The second request must carry the echoed assistant message with exactly
	// one tool_call (not two), so a single-tool-call template would accept it.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(srv.requests))
	}
	if n := countAssistantToolCalls(t, srv.requests[1]); n != 1 {
		t.Errorf("echoed history has %d tool_calls in the assistant turn, want 1", n)
	}
	// The request should also advertise parallel_tool_calls=false so
	// compliant endpoints emit a single call at generation time.
	if ptc := requestParallelToolCalls(t, srv.requests[0]); ptc == nil || *ptc != false {
		t.Errorf("request parallel_tool_calls = %v, want false", ptc)
	}
}

// TestAgentic_ParallelToolCalls_DefaultEchoesBoth confirms the default executes
// and echoes both parallel tool calls.
func TestAgentic_ParallelToolCalls_DefaultEchoesBoth(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespTwoToolCalls("call_1", "list_artifacts", "call_2", "list_artifacts"))
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("x")}}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}

	_, analysis, err := client.doAnalyzeAgentic(context.Background(), newTestAgenticInputs(t, browser, opts), "agentic:test:job:1:par", "system", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if analysis.ToolCalls != 2 {
		t.Errorf("tool_calls = %d, want 2 (both parallel calls executed by default)", analysis.ToolCalls)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if n := countAssistantToolCalls(t, srv.requests[1]); n != 2 {
		t.Errorf("echoed history has %d tool_calls, want 2 by default", n)
	}
	// Default must NOT send parallel_tool_calls so parallel-capable
	// providers keep their default behavior.
	if ptc := requestParallelToolCalls(t, srv.requests[0]); ptc != nil {
		t.Errorf("request parallel_tool_calls = %v, want omitted by default", *ptc)
	}
}

// requestParallelToolCalls returns the parallel_tool_calls field from a
// captured chat request body, or nil when the field was omitted.
func requestParallelToolCalls(t *testing.T, body []byte) *bool {
	t.Helper()
	var req struct {
		ParallelToolCalls *bool `json:"parallel_tool_calls"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	return req.ParallelToolCalls
}

// countAssistantToolCalls parses a captured chat request body and returns the
// number of tool_calls on the last assistant message in the conversation.
func countAssistantToolCalls(t *testing.T, body []byte) int {
	t.Helper()
	var req struct {
		Messages []struct {
			Role      string        `json:"role"`
			ToolCalls []interface{} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	n := 0
	for _, m := range req.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			n = len(m.ToolCalls)
		}
	}
	return n
}

// TestAgentic_EvidenceInjection_FetchesCitedUnreadArtifact verifies that when
// a critique-failing draft cites an artifact it never read, the critique retry
// fetches that artifact and embeds its content in the retry request.
func TestAgentic_EvidenceInjection_FetchesCitedUnreadArtifact(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	citePath := "artifacts/clusters/c1/machines/m1/cloud-init-output.log"
	// Round 1: cites an unread artifact; clean fix so only the unread-
	// citation check fails.
	round1 := `{"summary":"s","is_transient":false,"root_cause":"cloud-init failed per ` + citePath + `","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 10; reapply.","relevant_files":[]}`
	// Round 2: clean draft with no unread citation.
	round2 := `{"summary":"deep","is_transient":false,"root_cause":"cloud-init failed; vnet peering mismatch confirmed","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 10; reapply.","relevant_files":[]}`
	srv.push(200, chatRespFinal(round1))
	srv.push(200, chatRespFinal(round2))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{
		citePath: []byte("boot start\nINJECT_ME_MARKER cloud-init error: vnet peering mismatch\nboot end\n"),
	}}
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		CritiqueMaxRetries: 2,
	}
	_, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts), "agentic:test:ei", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("call count = %d, want 2 (draft + injected retry)", got)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) < 2 {
		t.Fatalf("expected 2 requests, got %d", len(srv.requests))
	}
	retry := string(srv.requests[1])
	if !strings.Contains(retry, "INJECT_ME_MARKER") {
		t.Errorf("retry request should embed the fetched artifact content")
	}
	if !strings.Contains(retry, "engine fetched evidence") {
		t.Errorf("retry request should carry the injection header")
	}
}

// TestAgentic_EvidenceInjection_PostLoopRetry verifies evidence injection also
// works for forced-finalize retries.
func TestAgentic_EvidenceInjection_PostLoopRetry(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	citePath := "artifacts/clusters/c1/machines/m1/cloud-init-output.log"
	// Iter 1: a tool call keeps the loop from getting a tools-free final.
	srv.push(200, chatRespToolCall("call_1", "list_artifacts", map[string]interface{}{"path": ""}))
	// Iter 2: another tool call reaches MaxIters and forces finalization.
	srv.push(200, chatRespToolCall("call_2", "list_artifacts", map[string]interface{}{"path": ""}))
	// Forced finalize: draft cites an unread artifact (clean fix).
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"cloud-init failed per `+citePath+`","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 1; reapply.","relevant_files":[]}`))
	// Injection-driven finalize retry: clean, grounded draft.
	srv.push(200, chatRespFinal(`{"summary":"deep","is_transient":false,"root_cause":"cloud-init failed; vnet mismatch confirmed from the injected log","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 1; reapply.","relevant_files":[]}`))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{
		citePath: []byte("POSTLOOP_MARKER cloud-init error: vnet peering mismatch\n"),
	}}
	opts := AgenticOptions{
		MaxIters: 2, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		CritiqueMaxRetries: 2,
	}
	_, analysis, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts), "agentic:test:ei-postloop", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	// Injection-driven finalize must embed the fetched artifact content.
	last := string(srv.requests[len(srv.requests)-1])
	if !strings.Contains(last, "POSTLOOP_MARKER") {
		t.Errorf("post-loop injection retry should embed the fetched artifact content")
	}
	if analysis.RootCause == "" || !strings.Contains(analysis.RootCause, "vnet mismatch confirmed") {
		t.Errorf("expected the re-grounded draft to be published, got %q", analysis.RootCause)
	}
}

// TestAgentic_EvidenceInjection_ResolvesBareBasename verifies unread bare
// basenames are resolved by walking the artifact tree.
func TestAgentic_EvidenceInjection_ResolvesBareBasename(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Round 1: cites a bare basename with a clean fix.
	round1 := `{"summary":"s","is_transient":false,"root_cause":"failure visible in controller-manager.log","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 1; reapply.","relevant_files":[]}`
	round2 := `{"summary":"deep","is_transient":false,"root_cause":"reconcile error confirmed; vnet mismatch","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 1; reapply.","relevant_files":[]}`
	srv.push(200, chatRespFinal(round1))
	srv.push(200, chatRespFinal(round2))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{
			"artifacts/controller-manager.log": []byte("WALKED_MARKER reconcile error: vnet peering mismatch\n"),
		},
		dirs: map[string][]string{"": {"artifacts/"}},
	}
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		CritiqueMaxRetries: 2,
	}
	_, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts), "agentic:test:ei-basename", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) < 2 || !strings.Contains(string(srv.requests[1]), "WALKED_MARKER") {
		t.Errorf("retry should embed the walk-resolved artifact content")
	}
}

// TestAgentic_EvidenceInjection_PrefetchesSkillEvidence verifies missing skill
// evidence can be resolved, fetched, and injected on retry.
func TestAgentic_EvidenceInjection_PrefetchesSkillEvidence(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Draft triggers the x509 skill but reads no evidence.
	round1 := `{"summary":"s","is_transient":false,"root_cause":"x509 webhook failure","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 1; reapply.","relevant_files":[]}`
	round2 := `{"summary":"deep","is_transient":false,"root_cause":"x509 webhook failure grounded in the cert-manager config","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 1; reapply.","relevant_files":[]}`
	srv.push(200, chatRespFinal(round1))
	srv.push(200, chatRespFinal(round2))

	set := loadSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook
triggers: ["x509"]
required_evidence:
  - id: webhook-config
    any_of: ["issuer\\.yaml"]
`,
	})
	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{
			"artifacts/cert-manager/issuer.yaml": []byte("SKILL_MARKER kind: Issuer\n"),
		},
		dirs: map[string][]string{"": {"artifacts/"}, "artifacts": {"cert-manager/"}},
	}
	in := newTestAgenticInputs(t, browser, AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		CritiqueMaxRetries: 2,
	})
	in.Skills = set
	_, _, err := client.doAnalyzeAgentic(context.Background(), in, "agentic:test:ei-skill", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) < 2 || !strings.Contains(string(srv.requests[1]), "SKILL_MARKER") {
		t.Errorf("retry should embed the skill-required evidence content")
	}
}

// TestResolveEvidenceByWalk_BoundedAndMultiTarget unit-tests the resolver: it
// resolves multiple predicates from one recursive listing and leaves unmatched
// predicates empty.
func TestResolveEvidenceByWalk_BoundedAndMultiTarget(t *testing.T) {
	browser := &fakeBrowser{
		files: map[string][]byte{
			"artifacts/a/foo.log": []byte("x"),
			"artifacts/b/bar.log": []byte("y"),
		},
		dirs: map[string][]string{"": {"artifacts/"}, "artifacts": {"a/", "b/"}},
	}
	preds := []func(string) bool{
		func(p string) bool { return strings.HasSuffix(p, "foo.log") },
		func(p string) bool { return strings.HasSuffix(p, "bar.log") },
		func(p string) bool { return strings.HasSuffix(p, "missing.log") },
	}
	got := resolveEvidenceByWalk(context.Background(), browser, preds)
	if got[0] != "artifacts/a/foo.log" {
		t.Errorf("pred0 = %q, want artifacts/a/foo.log", got[0])
	}
	if got[1] != "artifacts/b/bar.log" {
		t.Errorf("pred1 = %q, want artifacts/b/bar.log", got[1])
	}
	if got[2] != "" {
		t.Errorf("pred2 (missing) = %q, want empty", got[2])
	}
}

// TestAgentic_SeedArtifactTree_InjectsPaths verifies that the build's
// artifact path list is always prepended to the task prompt so the model
// sees exact paths up front.
func TestAgentic_SeedArtifactTree_InjectsPaths(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("x"),
		"artifacts/clusters/c1/machines/m1/cloud-init-output.log": []byte("y"),
		"artifacts/junit_01.png":                                  []byte("noise"),
	}}
	opts := AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}

	_, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts), "agentic:test:seed", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	req := string(srv.requests[0])
	for _, want := range []string{"Artifact paths for this build", "artifacts/clusters/c1/machines/m1/cloud-init-output.log", "do NOT guess paths", "do NOT spend tool calls"} {
		if !strings.Contains(req, want) {
			t.Errorf("seeded prompt missing %q", want)
		}
	}
	if strings.Contains(req, "junit_01.png") {
		t.Errorf("seeded prompt should drop non-text noise (.png) but listed it")
	}
}

// TestAgentic_SeedArtifactTree_NoOpOnEmptyTree confirms seeding degrades to a
// no-op (no seed header) when the build has no listable artifacts.
func TestAgentic_SeedArtifactTree_NoOpOnEmptyTree(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`))
	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{} // no files: ListTree returns nothing
	opts := AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}

	_, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts), "agentic:test:noseed", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if strings.Contains(string(srv.requests[0]), "Artifact paths for this build") {
		t.Errorf("empty tree must not inject the seed header")
	}
}

// TestAgentic_SeedArtifactTree_ByteCapped verifies the seed is bounded by byte
// budget, not just path count, so long paths cannot overflow the first request.
func TestAgentic_SeedArtifactTree_ByteCapped(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"r","severity":"Low","suggested_fix":"f","relevant_files":[]}`))
	client := newAgenticTestClient(t, srv.URL)

	// 60 long, sortable paths total about 30KB. With a small model budget the
	// seed byte budget is about 3KB, so only the first handful fit.
	files := map[string][]byte{}
	for i := 0; i < 60; i++ {
		p := fmt.Sprintf("artifacts/clusters/c/p%03d/%s.log", i, strings.Repeat("x", 470))
		files[p] = []byte("y")
	}
	browser := &fakeBrowser{files: files}
	opts := AgenticOptions{MaxIters: 3, ModelByteBudget: 20_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}

	_, _, err := client.doAnalyzeAgentic(context.Background(),
		newTestAgenticInputs(t, browser, opts), "agentic:test:seedcap", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	req := string(srv.requests[0])
	if !strings.Contains(req, "artifacts/clusters/c/p000/") {
		t.Errorf("seed should include the first (sorted) path")
	}
	if strings.Contains(req, "artifacts/clusters/c/p059/") {
		t.Errorf("seed should be byte-truncated; last path must not be present")
	}
	if !strings.Contains(req, "list truncated; use list_artifacts") {
		t.Errorf("byte-truncated seed must carry the truncation note")
	}
}

func TestAgentic_SkillEvidenceAbsentFromBuild_StillCaches(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Triggers the "x509" recipe but is otherwise clean (concrete fix, no
	// artifact citations). The required evidence is absent from the build.
	final := `{"summary":"webhook cert","is_transient":false,"root_cause":"x509 webhook validation failure prevented cluster creation","severity":"High","suggested_fix":"Regenerate the webhook serving certificate and redeploy the controller.","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	set := loadAgenticSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook-tls
triggers: ["x509"]
required_evidence:
  - id: webhook-config
    any_of: ["config/webhook/.*\\.yaml"]
`,
	})
	// Build tree lacks config/webhook/*.yaml, so evidence is absent.
	browser := &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("x509 error"),
		"artifacts/clusters/bootstrap/logs/capz-system/capz-controller-manager/m.log": []byte("y"),
	}}
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 2,
	}
	in := newTestAgenticInputs(t, browser, opts)
	in.Skills = set

	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in,
		"agentic:test:skill-absent", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if !analysis.CritiquePassed {
		t.Errorf("absent required evidence should not block critique; got CritiquePassed=false")
	}
}

// TestAgentic_SkillEvidencePresentButUnread_ZeroRetriesDoesNotRepair verifies
// that max_retries=0 does not perform a post-loop evidence repair.
func TestAgentic_SkillEvidencePresentButUnread_ZeroRetriesDoesNotRepair(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"webhook cert","is_transient":false,"root_cause":"x509 webhook validation failure prevented cluster creation","severity":"High","suggested_fix":"Regenerate the webhook serving certificate and redeploy the controller.","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	client := newAgenticTestClient(t, srv.URL)
	set := loadAgenticSkillsForTest(t, map[string]string{
		"webhook": `
id: webhook-tls
triggers: ["x509"]
required_evidence:
  - id: webhook-config
    any_of: ["config/webhook/.*\\.yaml"]
`,
	})
	// Build tree contains required evidence, but zero retries must leave it unread.
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{
		"build-log.txt":                 []byte("x509 error"),
		"config/webhook/manifests.yaml": []byte("webhooks:"),
	}}}
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 0,
	}
	in := newTestAgenticInputs(t, browser, opts)
	in.Skills = set
	var observations []DraftObservation
	in.DraftObserver = func(observation DraftObservation) {
		observations = append(observations, observation)
	}

	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in,
		"agentic:test:skill-present-unread", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if analysis.CritiquePassed {
		t.Error("max_retries=0 unexpectedly repaired missing evidence")
	}
	if got := atomic.LoadInt32(&srv.calls); got != 1 {
		t.Fatalf("call count = %d, want 1", got)
	}
	if len(browser.tailCalls) != 0 {
		t.Fatalf("zero retry budget fetched repair evidence: %v", browser.tailCalls)
	}
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want only the initial draft: %+v", len(observations), observations)
	}
	if observations[0].Phase != "initial" || observations[0].MissingGroupCount != 1 || observations[0].EvidenceReads != 0 {
		t.Errorf("initial observation = %+v", observations[0])
	}
}

// TestChatClient_BoundedByContextNotFixedTimeout verifies chat calls are bounded
// only by the caller's context, not by a fixed client timeout.
func TestChatClient_BoundedByContextNotFixedTimeout(t *testing.T) {
	shrinkCallDelay(t)
	const serverDelay = 150 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(serverDelay):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatRespFinal("ok"))
	}))
	t.Cleanup(srv.Close)
	c := newAgenticTestClient(t, srv.URL)

	// The client must carry no fixed timeout; the context is the only bound.
	if c.api.httpClient.Timeout != 0 {
		t.Fatalf("chat client must have no fixed Timeout, got %v", c.api.httpClient.Timeout)
	}

	// Tight deadline (< server delay): the context must cancel the call.
	tightCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.callModel(tightCtx, nil, nil, nil); err == nil {
		t.Fatal("expected a context-deadline error under a tight deadline, got nil")
	}

	// Generous deadline (> server delay): the same slow endpoint succeeds.
	okCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	resp, err := c.callModel(okCtx, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected success within a generous deadline, got %v", err)
	}
	if !resp.HasMessage || resp.Message.Content == nil {
		t.Fatal("expected a final content message")
	}
}

func TestAgentic_InitialEvidencePlanUsesOneTreeListing(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal(`{"summary":"s","is_transient":false,"root_cause":"independent bug","severity":"Low","suggested_fix":"Update config.yaml and redeploy.","relevant_files":[]}`))

	set := loadAgenticSkillsForTest(t, map[string]string{
		"ranked": `
id: ranked
triggers: ["ranked failure"]
required_evidence:
  - id: logs
    description: Ranked logs
    any_of: ["logs/.*\\.log$"]
procedure: Read the ranked logs.
`,
	})
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{
		"logs/other.log":     []byte("other"),
		"logs/preferred.log": []byte("preferred"),
	}}}
	in := newTestAgenticInputs(t, browser, AgenticOptions{
		MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
	})
	in.Skills = set
	in.FailureSignal = "ranked failure preferred"
	_, _, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(
		context.Background(), in, "agentic:test:initial-plan", "sys", "ORIGINAL_USER_PROMPT",
	)
	if err != nil {
		t.Fatal(err)
	}
	if browser.listTreeCalls != 1 {
		t.Fatalf("ListTree calls = %d, want 1 for seed and plan", browser.listTreeCalls)
	}
	srv.mu.Lock()
	request := string(srv.requests[0])
	srv.mu.Unlock()
	for _, want := range []string{"Required evidence plan", "Read the ranked logs", "logs/preferred.log", "Artifact paths for this build", "ORIGINAL_USER_PROMPT"} {
		if !strings.Contains(request, want) {
			t.Errorf("initial request missing %q", want)
		}
	}
	if strings.Index(request, "Required evidence plan") > strings.Index(request, "Artifact paths for this build") || strings.Index(request, "Artifact paths for this build") > strings.Index(request, "ORIGINAL_USER_PROMPT") {
		t.Errorf("initial request sections are out of order: %s", request)
	}
	if strings.Index(request, "logs/preferred.log") > strings.Index(request, "logs/other.log") {
		t.Errorf("ranked candidate was not first: %s", request)
	}
	for _, forbidden := range []string{"submit_analysis", "evidence_token", "required_evidence"} {
		if strings.Contains(request, forbidden) {
			t.Errorf("in-process request contains Orka term %q", forbidden)
		}
	}
}

func TestBuildEvidenceInjectionUsesRankedCandidatesInGroupOrder(t *testing.T) {
	set := loadAgenticSkillsForTest(t, map[string]string{
		"ranked": `
id: ranked
triggers: ["ranked failure"]
required_evidence:
  - id: first
    any_of: ["first/.*\\.log$"]
  - id: second
    any_of: ["second/.*\\.log$"]
`,
	})
	signal := "ranked failure alpha beta"
	paths := []string{
		"first/unrelated.log", "first/alpha.log",
		"second/unrelated.log", "second/beta.log",
	}
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{
		"first/unrelated.log":  []byte("WRONG_FIRST"),
		"first/alpha.log":      []byte("FIRST_MARKER"),
		"second/unrelated.log": []byte("WRONG_SECOND"),
		"second/beta.log":      []byte("SECOND_MARKER"),
	}}}
	matched := set.Match(signal)[0]
	state := &agentState{
		browser: browser, initialEvidencePlan: set.Plan(signal, paths, evidenceplan.CandidatePathLimit),
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{},
	}
	out := critiqueOutcome{MissingSkillEvidence: []skillEvidenceMiss{{Skill: matched, Missing: matched.RequiredEvidence}}}
	injection := (&Client{}).buildEvidenceInjection(context.Background(), state, out)
	wantCalls := []string{"first/alpha.log", "second/beta.log"}
	if strings.Join(browser.tailCalls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("tail calls = %v, want %v", browser.tailCalls, wantCalls)
	}
	if strings.Index(injection, "FIRST_MARKER") > strings.Index(injection, "SECOND_MARKER") {
		t.Fatalf("group order was not preserved: %s", injection)
	}
	if strings.Contains(injection, "WRONG_") {
		t.Fatalf("repair ignored ranked candidates: %s", injection)
	}
}

func TestBuildEvidenceInjectionDeduplicatesCandidatePaths(t *testing.T) {
	set := loadAgenticSkillsForTest(t, map[string]string{
		"shared": `
id: shared
triggers: ["shared failure"]
required_evidence:
  - id: first
    any_of: ["shared\\.log$"]
  - id: second
    any_of: ["shared\\.log$"]
`,
	})
	signal := "shared failure"
	path := "artifacts/shared.log"
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{path: []byte("SHARED_MARKER")}}}
	matched := set.Match(signal)[0]
	state := &agentState{
		browser: browser, initialEvidencePlan: set.Plan(signal, []string{path}, evidenceplan.CandidatePathLimit),
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{},
	}
	out := critiqueOutcome{MissingSkillEvidence: []skillEvidenceMiss{{Skill: matched, Missing: matched.RequiredEvidence}}}
	injection := (&Client{}).buildEvidenceInjection(context.Background(), state, out)
	if len(browser.tailCalls) != 1 || browser.tailCalls[0] != path {
		t.Fatalf("tail calls = %v, want one %q read", browser.tailCalls, path)
	}
	if strings.Count(injection, "SHARED_MARKER") != 1 {
		t.Fatalf("shared candidate was injected more than once: %s", injection)
	}
}

func TestBuildEvidenceInjectionFallsBackForMissingCandidate(t *testing.T) {
	set := loadAgenticSkillsForTest(t, map[string]string{
		"fallback": `
id: fallback
triggers: ["fallback failure"]
required_evidence:
  - id: controller
    any_of: ["controller\\.log$"]
`,
	})
	signal := "fallback failure"
	path := "artifacts/controller.log"
	browser := &trackingBrowser{
		fakeBrowser:   &fakeBrowser{files: map[string][]byte{path: []byte("FALLBACK_MARKER")}},
		treeResponses: []treeResponse{{paths: []string{path}}},
	}
	matched := set.Match(signal)[0]
	state := &agentState{
		browser: browser, initialEvidencePlan: set.Plan(signal, []string{"build-log.txt"}, evidenceplan.CandidatePathLimit),
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{},
	}
	out := critiqueOutcome{MissingSkillEvidence: []skillEvidenceMiss{{Skill: matched, Missing: matched.RequiredEvidence}}}
	injection := (&Client{}).buildEvidenceInjection(context.Background(), state, out)
	if browser.listTreeCalls != 1 || len(browser.tailCalls) != 1 || browser.tailCalls[0] != path {
		t.Fatalf("fallback calls: ListTree=%d Tail=%v", browser.listTreeCalls, browser.tailCalls)
	}
	if !strings.Contains(injection, "FALLBACK_MARKER") {
		t.Fatalf("fallback evidence missing: %s", injection)
	}
}

func TestBuildEvidenceInjectionRejectsErrorAndEmptyReads(t *testing.T) {
	set := loadAgenticSkillsForTest(t, map[string]string{
		"read": `
id: read
triggers: ["read failure"]
required_evidence:
  - id: log
    any_of: ["failure\\.log$"]
`,
	})
	signal := "read failure"
	path := "artifacts/failure.log"
	matched := set.Match(signal)[0]
	for _, tc := range []struct {
		name  string
		err   error
		empty bool
	}{
		{name: "error", err: errors.New("read failed")},
		{name: "empty", empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			browser := &trackingBrowser{
				fakeBrowser: &fakeBrowser{files: map[string][]byte{path: []byte("content")}},
				tailErrors:  map[string]error{path: tc.err},
				emptyTails:  map[string]bool{path: tc.empty},
			}
			state := &agentState{
				browser: browser, initialEvidencePlan: set.Plan(signal, []string{path}, evidenceplan.CandidatePathLimit),
				readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{},
			}
			out := critiqueOutcome{MissingSkillEvidence: []skillEvidenceMiss{{Skill: matched, Missing: matched.RequiredEvidence}}}
			if injection := (&Client{}).buildEvidenceInjection(context.Background(), state, out); injection != "" {
				t.Fatalf("invalid read produced injection: %s", injection)
			}
			if len(state.readArtifactsFull) != 0 || state.gcsBytes != 0 || state.modelBytes != 0 {
				t.Fatalf("invalid read counted as repaired: reads=%v gcs=%d model=%d", state.readArtifactsFull, state.gcsBytes, state.modelBytes)
			}
			if len(browser.tailCalls) != 1 {
				t.Fatalf("tail calls = %v, want one bounded attempt", browser.tailCalls)
			}
		})
	}
}

func TestBuildEvidenceInjectionRespectsArtifactAndByteBounds(t *testing.T) {
	var groups []skills.EvidenceGroup
	var plannedGroups []skills.PlannedEvidenceGroup
	files := map[string][]byte{}
	for i := 0; i < evidenceInjectionMaxArtifacts+2; i++ {
		path := fmt.Sprintf("logs/group-%d.log", i)
		groups = append(groups, skills.EvidenceGroup{ID: fmt.Sprintf("group-%d", i), AnyOf: []string{fmt.Sprintf("group-%d\\.log$", i)}})
		plannedGroups = append(plannedGroups, skills.PlannedEvidenceGroup{ID: fmt.Sprintf("group-%d", i), CandidatePaths: []string{path}})
		files[path] = []byte(strings.Repeat(fmt.Sprintf("MARKER_%d", i), evidenceInjectionPerArtifactBytes))
	}
	var recipe strings.Builder
	recipe.WriteString("id: bounded\ntriggers: [bounded]\nrequired_evidence:\n")
	for _, group := range groups {
		fmt.Fprintf(&recipe, "  - id: %s\n    any_of: [%q]\n", group.ID, group.AnyOf[0])
	}
	set := loadAgenticSkillsForTest(t, map[string]string{"bounded": recipe.String()})
	matched := set.Match("bounded")[0]
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: files}}
	state := &agentState{
		browser:             browser,
		initialEvidencePlan: []skills.PlannedSkill{{ID: "bounded", RequiredEvidence: plannedGroups}},
		readArtifactsFull:   map[string]bool{}, readArtifactsBase: map[string]bool{},
	}
	out := critiqueOutcome{MissingSkillEvidence: []skillEvidenceMiss{{Skill: matched, Missing: matched.RequiredEvidence}}}
	injection := (&Client{}).buildEvidenceInjection(context.Background(), state, out)
	if len(browser.tailCalls) != evidenceInjectionMaxArtifacts {
		t.Fatalf("tail calls = %d, want %d", len(browser.tailCalls), evidenceInjectionMaxArtifacts)
	}
	for _, maxBytes := range browser.tailMaxBytes {
		if maxBytes != evidenceInjectionPerArtifactBytes {
			t.Errorf("tail max bytes = %d, want %d", maxBytes, evidenceInjectionPerArtifactBytes)
		}
	}
	if state.gcsBytes > evidenceInjectionMaxArtifacts*evidenceInjectionPerArtifactBytes || state.modelBytes > evidenceInjectionMaxArtifacts*evidenceInjectionPerArtifactBytes {
		t.Fatalf("injection bytes exceed bound: gcs=%d model=%d", state.gcsBytes, state.modelBytes)
	}
	if strings.Contains(injection, fmt.Sprintf("group-%d.log", evidenceInjectionMaxArtifacts)) {
		t.Fatalf("injection exceeded artifact count bound: %s", injection)
	}
}

func TestAgentic_StrongModelReadsPlannedEvidenceWithoutRepair(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	path := "artifacts/issuer.yaml"
	srv.push(200, chatRespToolCall("call_1", "tail_artifact", map[string]interface{}{"path": path, "lines": 200}))
	srv.push(200, chatRespFinal(`{"summary":"x509","is_transient":false,"root_cause":"x509 issuer mismatch shown in artifacts/issuer.yaml","severity":"High","suggested_fix":"Update issuer.yaml with the correct CA and redeploy.","relevant_files":["artifacts/issuer.yaml"]}`))
	set := loadAgenticSkillsForTest(t, map[string]string{
		"x509": `
id: x509
triggers: ["x509"]
required_evidence:
  - id: issuer
    any_of: ["issuer\\.yaml$"]
`,
	})
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{path: []byte("kind: Issuer\n")}}}
	in := newTestAgenticInputs(t, browser, AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
		CritiqueMaxRetries: 2,
	})
	in.Skills = set
	in.FailureSignal = "x509 failure"
	_, analysis, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(context.Background(), in, "agentic:test:strong-plan", "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.CritiquePassed {
		t.Fatalf("strong-model draft did not pass critique: %+v", analysis)
	}
	if browser.listTreeCalls != 1 || len(browser.tailCalls) != 1 || browser.tailCalls[0] != path {
		t.Fatalf("unexpected planning or repair reads: ListTree=%d Tail=%v", browser.listTreeCalls, browser.tailCalls)
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Fatalf("model calls = %d, want tool plus final only", got)
	}
}

func TestDispatchAgenticToolRejectsRegisteredButDisabledTool(t *testing.T) {
	registry, _ := newTestRegistry(t)
	state := &agentState{
		browser:  &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("secret evidence")}},
		registry: registry, enabledTools: []string{"list_artifacts"},
		opts: AgenticOptions{ModelByteBudget: 100_000, GCSByteBudget: 100_000},
	}
	envelope, payload := dispatchAgenticToolWithPayload(context.Background(), state, modelToolCall{
		ID: "call", Type: "function", Function: modelFunction{Name: "read_artifact", Arguments: `{"path":"build-log.txt"}`},
	})
	if _, ok := payload["error"]; !ok || !strings.Contains(envelope, "not enabled") {
		t.Fatalf("disabled tool result: envelope=%q payload=%v", envelope, payload)
	}
	if state.gcsBytes != 0 {
		t.Fatalf("disabled tool fetched %d bytes", state.gcsBytes)
	}
}

func TestAgenticBuildLogSelectsAKSBootstrapCauseBeforeCleanup(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespToolCall("call_1", "grep_artifact", map[string]interface{}{
		"path": "build-log.txt", "pattern": "K8sVersionNotSupported|ResourceGroupNotFound", "context_lines": 2,
	}))
	srv.push(200, chatRespToolCall("call_2", "read_artifact", map[string]interface{}{"path": "build-log.txt"}))
	srv.push(200, chatRespFinal(`{"summary":"AKS bootstrap-cluster creation failed before tests started.","is_transient":false,"root_cause":"build-log.txt shows K8sVersionNotSupported while creating the AKS bootstrap cluster because Kubernetes 1.33.2 requires Long-Term Support in AKS.","severity":"High","suggested_fix":"Update the repository configuration that selects Kubernetes 1.33.2 to use an AKS-supported version or enable the required Long-Term Support plan before creating the bootstrap cluster.","relevant_files":["build-log.txt"]}`))

	logData := []byte(`2026-07-29T10:00:00Z creating AKS bootstrap cluster
2026-07-29T10:01:00Z Code="K8sVersionNotSupported" Message="Kubernetes version 1.33.2 is not supported without Long-Term Support"
2026-07-29T10:01:01Z failed to create AKS bootstrap cluster
2026-07-29T10:10:00Z cleanup started
2026-07-29T10:10:01Z Code="ResourceGroupNotFound" Message="resource group cleanup target was not found"
`)
	client := newAgenticTestClient(t, srv.URL)
	inputs := newTestAgenticInputs(t, &fakeBrowser{files: map[string][]byte{"build-log.txt": logData}}, AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second, MinToolCalls: 2,
	})
	inputs.FailureSignal = "Prow job execution failed before JUnit results"
	_, analysis, err := client.doAnalyzeAgentic(t.Context(), inputs, "agentic:test:aks-build-log-cause", "sys", "Investigate the build failure and identify the earliest causal error before cleanup failures.")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"K8sVersionNotSupported", "1.33.2", "Long-Term Support", "AKS bootstrap cluster"} {
		if !strings.Contains(analysis.RootCause, want) {
			t.Fatalf("root cause missing %q: %s", want, analysis.RootCause)
		}
	}
	if strings.Contains(analysis.RootCause, "ResourceGroupNotFound") {
		t.Fatalf("cleanup error was reported as the root cause: %s", analysis.RootCause)
	}
	if analysis.ToolCalls != 2 || analysis.GCSBytes == 0 {
		t.Fatalf("analysis telemetry = calls:%d bytes:%d", analysis.ToolCalls, analysis.GCSBytes)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) != 3 {
		t.Fatalf("model requests = %d, want 3", len(srv.requests))
	}
	history := string(srv.requests[2])
	if !strings.Contains(history, "K8sVersionNotSupported") || !strings.Contains(history, "ResourceGroupNotFound") {
		t.Fatalf("final request did not include both causal and cleanup evidence: %s", history)
	}
}

func TestAgentic_CacheGenerationSwitchIsReversible(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"cached","is_transient":false,"root_cause":"root","severity":"Low","suggested_fix":"fix","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))
	srv.push(200, chatRespFinal(final))
	client := newAgenticTestClient(t, srv.URL)
	in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{MaxIters: 3, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second})
	key1 := AgenticCacheKeyForGeneration("universal", "1111111111111111", "job", "1", "test", "failed")
	key2 := AgenticCacheKeyForGeneration("universal", "2222222222222222", "job", "1", "test", "failed")
	if _, _, err := client.doAnalyzeAgentic(t.Context(), in, key1, "sys", "user"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.doAnalyzeAgentic(t.Context(), in, key2, "sys", "user"); err != nil {
		t.Fatal(err)
	}
	_, analysis, err := client.doAnalyzeAgentic(t.Context(), in, key1, "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.CacheHit || atomic.LoadInt32(&srv.calls) != 2 {
		t.Fatalf("cache hit=%t server calls=%d", analysis.CacheHit, atomic.LoadInt32(&srv.calls))
	}
	if _, ok := client.Cache().Get(key2); !ok {
		t.Fatal("switching generations deleted the other entry")
	}
}

func TestToolResultSnippetsCaptureReadTailAndSeparateGrepMatches(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		payload map[string]interface{}
		want    [][]string
	}{
		{name: "read", tool: "read_artifact", payload: map[string]interface{}{"content": "conversion webhook"}, want: [][]string{{"conversion webhook"}}},
		{name: "tail", tool: "tail_artifact", payload: map[string]interface{}{"content": "connection refused"}, want: [][]string{{"connection refused"}}},
		{name: "grep", tool: "grep_artifact", payload: map[string]interface{}{"matches": []map[string]interface{}{
			{"context": []string{"> 12: ManagedClustersAgentPool", "  13: conversion webhook"}},
			{"context": []string{"> 90: connection refused"}},
		}}, want: [][]string{{"ManagedClustersAgentPool", "conversion webhook"}, {"connection refused"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolResultSnippets(tc.tool, tc.payload)
			if len(got) != len(tc.want) {
				t.Fatalf("toolResultSnippets = %q, want %d snippets", got, len(tc.want))
			}
			for i, wants := range tc.want {
				for _, want := range wants {
					if !strings.Contains(got[i], want) {
						t.Fatalf("snippet[%d] = %q, want %q", i, got[i], want)
					}
				}
			}
		})
	}
}

func TestEvidenceInjectionTriesParallelCandidatesUntilContentMatches(t *testing.T) {
	set := loadAgenticSkillsForTest(t, map[string]string{
		"conversion": `
id: aso-conversion
triggers: ["conversion webhook"]
required_evidence:
  - id: failed-upgrade-log
    any_of: ["artifacts/clusters/.*/clusterctl-upgrade\\.log"]
    content_any_of: ["(?i)ManagedClustersAgentPool", "(?i)VirtualNetworks?Subnet"]
    content_all_of: ["(?i)conversion webhook", "(?i)connect: connection refused"]
`,
	})
	skill := set.Skills()[0]
	group := skill.RequiredEvidence[0]
	paths := []string{
		"artifacts/clusters/parallel-p9uqx9/clusterctl-upgrade.log",
		"artifacts/clusters/parallel-s4c1ag/clusterctl-upgrade.log",
		"artifacts/clusters/parallel-ttjjmj/clusterctl-upgrade.log",
		"artifacts/clusters/parallel-g9706x/clusterctl-upgrade.log",
	}
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{
		paths[0]: []byte("upgrade completed successfully\n"),
		paths[1]: []byte("conversion webhook health check succeeded\n"),
		paths[2]: []byte("ManagedClustersAgentPool reconciliation completed\n"),
		paths[3]: []byte("ManagedClustersAgentPool conversion webhook failed: connect: connection refused\n"),
	}}}
	state := &agentState{
		browser:           browser,
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{},
		initialEvidencePlan: []skills.PlannedSkill{{
			ID:               skill.ID,
			RequiredEvidence: []skills.PlannedEvidenceGroup{{ID: group.ID, CandidatePaths: paths}},
		}},
	}
	out := critiqueOutcome{MissingSkillEvidence: []skillEvidenceMiss{{Skill: skill, Missing: []skills.EvidenceGroup{group}}}}
	injection := (&Client{}).buildEvidenceInjection(context.Background(), state, out)
	if !reflect.DeepEqual(browser.tailCalls, paths) {
		t.Fatalf("tail candidate order = %v, want %v", browser.tailCalls, paths)
	}
	if !group.SatisfiedWithContent(state.readArtifactsFull, state.evidenceContentByPath) {
		t.Fatalf("content-aware group remained unsatisfied: reads=%v content=%v", state.readArtifactsFull, state.evidenceContentByPath)
	}
	if !strings.Contains(injection, "parallel-g9706x") || state.gcsBytes == 0 {
		t.Fatalf("injection did not include the satisfying candidate: %s", injection)
	}
}

func TestResolveEvidenceCandidatesByWalkDeterministicAndBounded(t *testing.T) {
	browser := &fakeBrowser{files: map[string][]byte{
		"logs/c.log": []byte("c"),
		"logs/a.log": []byte("a"),
		"logs/b.log": []byte("b"),
	}}
	got := resolveEvidenceCandidatesByWalk(context.Background(), browser, []func(string) bool{
		func(p string) bool { return strings.HasSuffix(p, ".log") },
	}, 2)
	want := [][]string{{"logs/a.log", "logs/b.log"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func TestRecordEvidenceContentPreservesArtifactPathCase(t *testing.T) {
	state := &agentState{}
	state.recordEvidenceContent("logs/Foo.log", "first")
	state.recordEvidenceContent("logs/foo.log", "second")
	if len(state.evidenceContentByPath) != 2 || state.evidenceContentByPath["logs/Foo.log"][0] != "first" || state.evidenceContentByPath["logs/foo.log"][0] != "second" {
		t.Fatalf("content proof identities = %+v", state.evidenceContentByPath)
	}
}

func TestEvidenceTrackingCanonicalizesSafeToolPath(t *testing.T) {
	state := &agentState{readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{}}
	state.recordSuccessfulRead("logs/./failure.log")
	state.recordEvidenceSnippets("logs/./failure.log", []string{"signal"})
	if !state.readArtifactsFull["logs/failure.log"] || !state.evidenceArtifactsFull["logs/failure.log"] {
		t.Fatalf("canonical read paths = read:%v evidence:%v", state.readArtifactsFull, state.evidenceArtifactsFull)
	}
	if !reflect.DeepEqual(state.evidenceContentByPath, map[string][]string{"logs/failure.log": {"signal"}}) {
		t.Fatalf("canonical content paths = %+v", state.evidenceContentByPath)
	}
}

func TestTruncatedToolEnvelopeCreatesNoInvisibleEvidence(t *testing.T) {
	registry, enabled := newTestRegistry(t)
	line := "MATCH " + strings.Repeat("x", 900) + "\n"
	browser := &fakeBrowser{files: map[string][]byte{"logs/large.log": []byte(strings.Repeat(line, 100))}}
	state := &agentState{
		browser: browser, registry: registry, enabledTools: enabled,
		opts:      AgenticOptions{ModelByteBudget: 200_000, GCSByteBudget: 200_000},
		startTime: time.Now(),
	}
	arguments, err := json.Marshal(map[string]interface{}{
		"path": "logs/large.log", "pattern": "MATCH", "context_lines": 0, "max_matches": 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := dispatchAgenticTool(context.Background(), state, modelToolCall{
		ID: "call", Type: "function", Function: modelFunction{Name: "grep_artifact", Arguments: string(arguments)},
	})
	if len(envelope) <= agenticToolBudget || modelVisibleToolPayload(envelope) != nil {
		t.Fatalf("expected a truncated non-decodable envelope, length=%d", len(envelope))
	}
	if len(state.evidenceArtifactsFull) != 0 || len(state.evidenceContentByPath) != 0 || len(state.analysisEvidence) != 0 {
		t.Fatalf("capped evidence tracking = paths:%v content:%v", state.evidenceArtifactsFull, state.evidenceContentByPath)
	}
}

func TestAdditionalUniqueContentAdvancesEvidenceRevision(t *testing.T) {
	state := &agentState{}
	state.recordEvidenceSnippets("logs/failure.log", []string{"first signal"})
	if state.evidenceRevision != 1 {
		t.Fatalf("first evidence revision = %d, want 1", state.evidenceRevision)
	}
	state.recordEvidenceSnippets("logs/failure.log", []string{"first signal"})
	if state.evidenceRevision != 1 {
		t.Fatalf("duplicate content advanced revision to %d", state.evidenceRevision)
	}
	state.recordEvidenceSnippets("logs/failure.log", []string{"second signal"})
	if state.evidenceRevision != 2 {
		t.Fatalf("additional same-path content revision = %d, want 2", state.evidenceRevision)
	}
}

func TestEvidenceInjectionKeepsCaseDistinctCandidates(t *testing.T) {
	set := loadAgenticSkillsForTest(t, map[string]string{"case": `
id: case-sensitive
triggers: ["failure"]
required_evidence:
  - id: log
    any_of: ["logs/foo\\.log$"]
    content_all_of: ["required signal"]
`})
	skill := set.Skills()[0]
	group := skill.RequiredEvidence[0]
	paths := []string{"logs/Foo.log", "logs/foo.log"}
	plan := set.Plan("failure", paths, evidenceplan.CandidatePathLimit)
	if len(plan) != 1 || len(plan[0].RequiredEvidence) != 1 || !reflect.DeepEqual(plan[0].RequiredEvidence[0].CandidatePaths, paths) {
		t.Fatalf("case-distinct planned candidates = %+v", plan)
	}
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{
		paths[0]: []byte("unrelated\n"),
		paths[1]: []byte("required signal\n"),
	}}}
	state := &agentState{
		browser:           browser,
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{},
		initialEvidencePlan: plan,
	}
	out := critiqueOutcome{MissingSkillEvidence: []skillEvidenceMiss{{Skill: skill, Missing: []skills.EvidenceGroup{group}}}}
	(&Client{}).buildEvidenceInjection(context.Background(), state, out)
	if !reflect.DeepEqual(browser.tailCalls, paths) {
		t.Fatalf("case-distinct candidate reads = %v, want %v", browser.tailCalls, paths)
	}
	if !group.SatisfiedWithContent(state.readArtifactsFull, state.evidenceContentByPath) {
		t.Fatalf("satisfying case-distinct candidate was skipped: %+v", state.evidenceContentByPath)
	}
}

func TestEvidenceInjectionDoesNotTreatPartialReadAsNegativeProof(t *testing.T) {
	set := loadAgenticSkillsForTest(t, map[string]string{"partial": `
id: partial
triggers: ["failure"]
required_evidence:
  - id: log
    any_of: ["logs/failure\\.log$"]
    content_all_of: ["required tail signal"]
`})
	skill := set.Skills()[0]
	group := skill.RequiredEvidence[0]
	artifactPath := "logs/failure.log"
	browser := &trackingBrowser{fakeBrowser: &fakeBrowser{files: map[string][]byte{artifactPath: []byte("required tail signal\n")}}}
	state := &agentState{
		browser:               browser,
		readArtifactsFull:     map[string]bool{artifactPath: true},
		readArtifactsBase:     map[string]bool{"failure.log": true},
		evidenceArtifactsFull: map[string]bool{artifactPath: true},
		evidenceContentByPath: map[string][]string{artifactPath: {"unrelated head content"}},
		initialEvidencePlan:   set.Plan("failure", []string{artifactPath}, evidenceplan.CandidatePathLimit),
	}
	out := critiqueOutcome{MissingSkillEvidence: []skillEvidenceMiss{{Skill: skill, Missing: []skills.EvidenceGroup{group}}}}
	(&Client{}).buildEvidenceInjection(context.Background(), state, out)
	if !reflect.DeepEqual(browser.tailCalls, []string{artifactPath}) {
		t.Fatalf("partial prior read suppressed tail repair: %v", browser.tailCalls)
	}
	if !group.SatisfiedWithContent(state.readArtifactsFull, state.evidenceContentByPath) {
		t.Fatalf("tail repair did not add positive proof: %+v", state.evidenceContentByPath)
	}
}

func TestToolResultSnippetsPreserveReturnedWhitespace(t *testing.T) {
	snippets := toolResultSnippets("read_artifact", map[string]interface{}{"content": "  ERROR\n"})
	if len(snippets) != 1 || snippets[0] != "  ERROR\n" {
		t.Fatalf("read snippet = %q", snippets)
	}
	grep := toolResultSnippets("grep_artifact", map[string]interface{}{"matches": []map[string]interface{}{{"context": []string{"> 4:   ERROR"}}}})
	if len(grep) != 1 || grep[0] != "  ERROR" {
		t.Fatalf("grep snippet = %q", grep)
	}
}

func TestRepoToolReadDoesNotCountAsGCSEvidence(t *testing.T) {
	registry := tools.NewRegistry()
	repotree.Register(registry)
	enabled, err := registry.Enable([]string{"repotree"})
	if err != nil {
		t.Fatal(err)
	}
	state := &agentState{
		repo:     &fakeSourceRepo{files: map[string]string{"test/e2e/capi_test.go": "package e2e\n"}},
		registry: registry, enabledTools: enabled, cache: tools.NewCache(),
		opts: AgenticOptions{ModelByteBudget: 100_000, GCSByteBudget: 100_000}, startTime: time.Now(),
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{},
	}
	arguments, _ := json.Marshal(map[string]interface{}{"path": "test/e2e/capi_test.go"})
	dispatchAgenticTool(context.Background(), state, modelToolCall{ID: "repo", Type: "function", Function: modelFunction{Name: "read_repo_file", Arguments: string(arguments)}})
	if state.gcsBytes != 0 {
		t.Fatalf("repo bytes counted as GCS: %d", state.gcsBytes)
	}
	if !state.readSourceFull["test/e2e/capi_test.go"] {
		t.Fatalf("repo read path not recorded: %v", state.readSourceFull)
	}
	if len(state.evidenceArtifactsFull) != 0 {
		t.Fatalf("repo read satisfied artifact evidence: %v", state.evidenceArtifactsFull)
	}
}

func TestAgenticFinalizeUnexpectedToolCallRetainsDraft(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	fallback := `{"summary":"fallback","is_transient":false,"root_cause":"controller configuration mismatch","severity":"High","suggested_fix":"Update the controller configuration.","relevant_files":[]}`
	srv.push(200, chatRespFinal(fallback))
	srv.push(200, chatRespToolCall("unexpected", "read_artifact", map[string]interface{}{"path": "build-log.txt"}))

	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
		MaxIters: 1, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, MinToolCalls: 2,
	})
	_, analysis, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(ctx, in, "agentic:test:finalize-tool-retain", "sys", "user")
	trace.Finish("success", err)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.RootCause != "controller configuration mismatch" {
		t.Fatalf("fallback draft not retained: %+v", analysis)
	}
	var sawEmpty, sawRejected, sawRetained bool
	for _, event := range store.Snapshot().Traces[0].Events {
		switch {
		case event.Kind == "finalize" && event.Outcome == "empty" && event.ErrorCode == "unexpected_tool_call":
			sawEmpty = true
		case event.Kind == "finalize_parse" && event.Outcome == "rejected" && event.ErrorCode == "empty_content":
			sawRejected = true
		case event.Kind == "finalize_recovery" && event.Outcome == "retained_draft" && event.SelectedAttempt == 1:
			sawRetained = true
		}
	}
	if !sawEmpty || !sawRejected || !sawRetained {
		t.Fatalf("finalize telemetry missing: %+v", store.Snapshot())
	}
}

func TestAgenticFinalizeUnexpectedToolCallSynthesizesWithoutDraft(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	srv.push(200, chatRespFinal("not json"))
	srv.push(200, chatRespToolCall("unexpected", "read_artifact", map[string]interface{}{"path": "build-log.txt"}))

	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{MaxIters: 1, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second})
	_, analysis, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(ctx, in, "agentic:test:finalize-tool-synthesize", "sys", "user")
	trace.Finish("success", err)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.SuggestedFix != "Unable to parse structured response" {
		t.Fatalf("expected synthesized fallback: %+v", analysis)
	}
	var sawSynthesized bool
	for _, event := range store.Snapshot().Traces[0].Events {
		if event.Kind == "finalize_recovery" && event.Outcome == "synthesized" {
			sawSynthesized = true
		}
	}
	if !sawSynthesized {
		t.Fatalf("missing synthesized recovery telemetry: %+v", store.Snapshot())
	}
}

func TestCritiqueTraceEventCountsDoNotPersistContent(t *testing.T) {
	const private = "PRIVATE_CRITIQUE_SENTINEL"
	out := critiqueOutcome{
		PuntMatches:     []string{private, private + "-2"},
		UnreadCitations: []string{private + "/unread.log"},
		CitationIssues:  []string{private + " citation", private + " line"},
		MissingSkillEvidence: []skillEvidenceMiss{{
			Skill:   skills.Skill{ID: private + "-skill"},
			Missing: []skills.EvidenceGroup{{ID: private + "-one"}, {ID: private + "-two"}},
		}},
		TransientPersistCount: 4,
	}
	event := critiqueTraceEvent("objected", out)
	if event.IssueCount != 7 || event.CritiquePunts != 2 || event.CritiqueUnread != 1 ||
		event.CritiqueCitations != 2 || event.CritiqueSkills != 1 ||
		event.CritiqueGroups != 2 || event.CritiqueTransient != 4 {
		t.Fatalf("critique trace counts = %+v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), private) {
		t.Fatalf("critique trace leaked private content: %s", encoded)
	}
}

func TestAgenticCritiqueObjectedTraceCarriesCategoryCounts(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"failure","is_transient":false,"root_cause":"controller failed","severity":"High","suggested_fix":"Investigate the logs and determine the root cause.","relevant_files":[]}`
	srv.push(200, chatRespFinal(final))

	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", BuildID: "1", TestName: "test", APIMode: APIChatCompletions})
	ctx := withAnalysisTrace(context.Background(), trace)
	in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
		MaxIters: 1, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 0,
	})
	_, _, err := newAgenticTestClient(t, srv.URL).doAnalyzeAgentic(ctx, in, "agentic:test:critique-category-trace", "sys", "user")
	trace.Finish("success", err)
	if err != nil {
		t.Fatal(err)
	}
	var objected, denied bool
	for _, event := range store.Snapshot().Traces[0].Events {
		if event.Kind == "critique" && event.Outcome == "objected" && event.IssueCount == 1 && event.CritiquePunts == 1 {
			objected = true
		}
		if event.Kind == "critique_retry_denied" && event.Outcome == "retry_budget" {
			denied = true
		}
	}
	if !objected || !denied {
		t.Fatalf("critique trace missing categories or denial: %+v", store.Snapshot())
	}
}
