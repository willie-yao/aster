package aitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ScriptServer is an httptest chat-completions server that returns queued
// responses in FIFO order regardless of request content. Use it for hermetic,
// scripted end-to-end scenarios where the request sequence is deterministic
// (e.g. a single failure's agentic loop). For high-fidelity replay of recorded
// runs use ReplayServer instead. GET requests to /v1/models return 404 so the
// engine's context-window probe falls back to its default budget.
type ScriptServer struct {
	*httptest.Server
	t *testing.T

	mu        sync.Mutex
	responses []string
	idx       int
	chatCalls int
}

// NewScriptServer creates a script server. Queue responses with Push* before
// the engine calls it.
func NewScriptServer(t *testing.T) *ScriptServer {
	t.Helper()
	s := &ScriptServer{t: t}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// Push queues a raw chat-completion response body.
func (s *ScriptServer) Push(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, body)
}

// PushToolCall queues an assistant response that invokes one tool.
func (s *ScriptServer) PushToolCall(id, name string, args map[string]any) {
	a, _ := json.Marshal(args)
	aStr, _ := json.Marshal(string(a))
	s.Push(fmt.Sprintf(
		`{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}}]}`,
		id, name, aStr,
	))
}

// PushFinal queues a tools-free assistant response carrying the given content
// (typically the final analysis JSON).
func (s *ScriptServer) PushFinal(content string) {
	c, _ := json.Marshal(content)
	s.Push(fmt.Sprintf(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%s}}]}`, c))
}

// ChatCalls returns how many chat-completion (POST) requests were served.
func (s *ScriptServer) ChatCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chatCalls
}

func (s *ScriptServer) handle(w http.ResponseWriter, r *http.Request) {
	// The context-window probe is a GET to /v1/models; 404 makes the engine
	// fall back to its default byte budget.
	if r.Method == http.MethodGet || strings.Contains(r.URL.Path, "/models") {
		http.Error(w, "no models endpoint", http.StatusNotFound)
		return
	}
	_, _ = io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatCalls++
	if s.idx >= len(s.responses) {
		s.t.Errorf("aitest: ScriptServer ran out of scripted responses at chat call %d", s.chatCalls)
		http.Error(w, "no scripted response", http.StatusInternalServerError)
		return
	}
	body := s.responses[s.idx]
	s.idx++
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}
