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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/filesystem"
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

func (b *fakeBrowser) Grep(_ context.Context, p string, re *regexp.Regexp, _, maxMatches, _ int) (*artifacts.GrepResult, error) {
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

// TestAgentic_CacheMiss_StaleTransientOnPersistence verifies the on-disk agentic
// cache invalidates a transient entry once the failure is persistent: a second
// call with ConsecutiveFailures>=threshold re-analyzes instead of serving the
// cached transient verdict.
func TestAgentic_CacheMiss_StaleTransientOnPersistence(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	transient := `{"summary":"flaky","is_transient":true,"root_cause":"infra","severity":"Low","suggested_fix":"re-run","relevant_files":[]}`
	real := `{"summary":"real","is_transient":false,"root_cause":"bug","severity":"High","suggested_fix":"fix it","relevant_files":[]}`
	srv.push(200, chatRespFinal(transient)) // first call: caches transient
	srv.push(200, chatRespFinal(real))      // re-analysis after invalidation

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{}
	opts := AgenticOptions{MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second}

	// First call: not yet persistent, transient verdict is accepted and cached.
	in1 := newTestAgenticInputs(t, browser, opts)
	if _, _, err := client.doAnalyzeAgentic(context.Background(), in1, "agentic:test:staletransient", "sys", "user"); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call: now persistent. The cached transient entry must be a miss.
	in2 := newTestAgenticInputs(t, browser, opts)
	in2.ConsecutiveFailures = transientPersistThreshold
	_, a2, err := client.doAnalyzeAgentic(context.Background(), in2, "agentic:test:staletransient", "sys", "user")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if a2.CacheHit {
		t.Error("expected a cache miss (re-analysis) for a persistent failure with a cached transient verdict")
	}
	if got := atomic.LoadInt32(&srv.calls); got != 2 {
		t.Errorf("expected 2 server calls (initial + re-analysis), got %d", got)
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
	if analysis.RootCause != "found in build-log.txt line 42" {
		t.Errorf("expected post-nudge root cause, got %q", analysis.RootCause)
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

const puntyFinalJSON = `{"summary":"shallow","is_transient":false,"root_cause":"third CP machine cloud-init empty","severity":"High","suggested_fix":"Check the AzureMachine status conditions. Verify cloud-init script execution. Investigate Azure activity logs.","relevant_files":[]}`

const cleanFinalJSON = `{"summary":"deep","is_transient":false,"root_cause":"third CP machine cloud-init empty due to vnet peering mismatch","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 142 to match the staging vnet peering name; reapply and retry.","relevant_files":["kustomize/cluster-template.yaml"]}`

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
	if !strings.Contains(analysis.SuggestedFix, "kustomize/cluster-template.yaml") {
		t.Errorf("expected concrete fix, got %q", analysis.SuggestedFix)
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

// TestAgentic_Critique_ExhaustedAcceptedNotCached verifies repeated punt
// drafts publish the last answer but skip caching.
func TestAgentic_Critique_ExhaustedAcceptedNotCached(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Three punts: original plus 2 retries. All fail critique, so the last
	// answer is accepted without a cache write.
	srv.push(200, chatRespFinal(puntyFinalJSON))
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
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count = %d, want 3 (original + 2 retries)", got)
	}
	if summary.Summary != "shallow" {
		t.Errorf("expected punt-shaped final to be published, got %q", summary.Summary)
	}

	// Critique-failing answers are not cached. A second analysis consumes
	// all three new server responses.
	srv.push(200, chatRespFinal(puntyFinalJSON))
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
	if summary.Summary != "deep" {
		t.Errorf("expected clean re-emit after tool call, got %q", summary.Summary)
	}
	if !analysis.CritiquePassed {
		t.Errorf("CritiquePassed = false, want true (critique should have passed after tool-call retry)")
	}
	if got := atomic.LoadInt32(&srv.calls); got != 3 {
		t.Errorf("call count = %d, want 3 (punt + tool + clean)", got)
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

// readThenCleanFinalJSON cites build-log.txt, which the model has read.
const readThenCleanFinalJSON = `{"summary":"deep","is_transient":false,"root_cause":"build-log.txt:42 shows the vnet peering name mismatch","severity":"High","suggested_fix":"Update kustomize/cluster-template.yaml line 142 to match the staging vnet peering name; reapply.","relevant_files":["build-log.txt"]}`

// TestAgentic_HallucinationRetry verifies an unread-citation critique can
// recover after the model reads and cites a matching artifact.
func TestAgentic_HallucinationRetry(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Round 1: model emits final citing manager.log (never read).
	srv.push(200, chatRespFinal(hallucinatedFinalJSON))
	// Round 2: after critique feedback, model reads build-log.txt.
	srv.push(200, chatRespToolCall("c1", "read_artifact", map[string]interface{}{
		"path": "build-log.txt", "offset": 0, "length": 256,
	}))
	// Round 3: re-emit citing build-log.txt, which passes.
	srv.push(200, chatRespFinal(readThenCleanFinalJSON))

	client := newAgenticTestClient(t, srv.URL)
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte("vnet peering mismatch on line 42\n")},
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

// TestAgentic_CacheInvalidatedByCritiqueVersionBump verifies cache entries with
// stale critique versions are invalidated.
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

	// Simulate a stale cache entry by rewriting CritiqueVersion to 0.
	raw, ok := client.Cache().Get(key)
	if !ok {
		t.Fatalf("first call should have written cache entry")
	}
	var cached agenticCacheData
	if err := json.Unmarshal(raw, &cached); err != nil {
		t.Fatalf("unmarshal cache: %v", err)
	}
	cached.CritiqueVersion = 0
	if err := client.Cache().Set(key, cached); err != nil {
		t.Fatalf("re-write cache: %v", err)
	}

	// Second call rejects the v0 entry and re-analyzes.
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

// TestAgentic_CacheInvalidatedBySkillSetHashChange verifies recipe edits
// invalidate entries stamped with a different SkillSetHash.
func TestAgentic_CacheInvalidatedBySkillSetHashChange(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)

	// Clean draft so critique passes and the entry gets cached. The
	// recipe does not trigger on this draft, so no skill-evidence
	// check fires; the entry caches with the current SkillSetHash.
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
		MaxIters:           5,
		ModelByteBudget:    100_000,
		GCSByteBudget:      100_000,
		Timeout:            30 * time.Second,
		CritiqueMaxRetries: 2,
	}
	in := newTestAgenticInputs(t, &fakeBrowser{}, opts)
	in.Skills = set

	const key = "agentic:test:skillhash-invalidate"
	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !analysis.CritiquePassed {
		t.Fatalf("first call: expected CritiquePassed=true, got %+v", analysis)
	}
	if analysis.SkillSetHash != set.Hash() {
		t.Fatalf("first call: SkillSetHash = %q, want %q", analysis.SkillSetHash, set.Hash())
	}

	// A second call with the same set should serve from cache.
	before := atomic.LoadInt32(&srv.calls)
	_, _, err = client.doAnalyzeAgentic(context.Background(), in, key, "sys", "user")
	if err != nil {
		t.Fatalf("second call (same set): %v", err)
	}
	if atomic.LoadInt32(&srv.calls) != before {
		t.Errorf("second call with same set hit model; expected cache hit")
	}

	// Simulate a consumer recipe edit by loading a set with a different hash.
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

	srv.push(200, chatRespFinal(cleanFinal))
	before2 := atomic.LoadInt32(&srv.calls)
	in2 := newTestAgenticInputs(t, &fakeBrowser{}, opts)
	in2.Skills = edited
	_, analysis2, err := client.doAnalyzeAgentic(context.Background(), in2, key, "sys", "user")
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if atomic.LoadInt32(&srv.calls) == before2 {
		t.Error("expected re-analysis after SkillSetHash change (server hit), got cache hit")
	}
	if analysis2.SkillSetHash != edited.Hash() {
		t.Errorf("third call: SkillSetHash = %q, want %q (post-edit)", analysis2.SkillSetHash, edited.Hash())
	}
}

func TestLimitToolCalls(t *testing.T) {
	three := []agToolCall{{ID: "a"}, {ID: "b"}, {ID: "c"}}
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

// TestAgentic_SkillEvidencePresentButUnread_InjectionRescues verifies that when
// required evidence exists but the draft never read it, the always-on critique
// fetches and injects it, letting the re-grounded draft pass.
func TestAgentic_SkillEvidencePresentButUnread_InjectionRescues(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	final := `{"summary":"webhook cert","is_transient":false,"root_cause":"x509 webhook validation failure prevented cluster creation","severity":"High","suggested_fix":"Regenerate the webhook serving certificate and redeploy the controller.","relevant_files":[]}`
	// Identical finals keep the scripted server deterministic across the
	// injection-triggered re-finalize rounds.
	srv.push(200, chatRespFinal(final))
	srv.push(200, chatRespFinal(final))
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
	// Build tree contains the required evidence, unread by the draft; injection
	// fetches it and marks it read so the re-critique passes.
	browser := &fakeBrowser{files: map[string][]byte{
		"build-log.txt":                 []byte("x509 error"),
		"config/webhook/manifests.yaml": []byte("webhooks:"),
	}}
	opts := AgenticOptions{
		MaxIters: 5, ModelByteBudget: 100_000, GCSByteBudget: 100_000,
		Timeout: 30 * time.Second, CritiqueMaxRetries: 0,
	}
	in := newTestAgenticInputs(t, browser, opts)
	in.Skills = set

	_, analysis, err := client.doAnalyzeAgentic(context.Background(), in,
		"agentic:test:skill-present-unread", "sys", "user")
	if err != nil {
		t.Fatalf("doAnalyzeAgentic: %v", err)
	}
	if !analysis.CritiquePassed {
		t.Errorf("present-but-unread required evidence should be injected and rescue the draft (CritiquePassed=true)")
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
	if c.httpClient.Timeout != 0 {
		t.Fatalf("chat client must have no fixed Timeout, got %v", c.httpClient.Timeout)
	}

	// Tight deadline (< server delay): the context must cancel the call.
	tightCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.callChatWithTools(tightCtx, nil, nil, nil); err == nil {
		t.Fatal("expected a context-deadline error under a tight deadline, got nil")
	}

	// Generous deadline (> server delay): the same slow endpoint succeeds.
	okCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	resp, err := c.callChatWithTools(okCtx, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected success within a generous deadline, got %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == nil {
		t.Fatal("expected a final content message")
	}
}
