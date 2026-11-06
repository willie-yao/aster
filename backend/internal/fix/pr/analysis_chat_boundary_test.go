package pr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/storage"
)

func TestAnalysisChatEmittedQuotesFitFixGeneration(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name     string
		count    int
		contents string
		locator  string
		ranged   bool
	}{
		{name: "full citation budget", count: 16, contents: strings.Repeat("q", 5000) + "terminal condition", locator: "terminal condition"},
		{name: "SHA at end of long JSON line", count: 1, contents: `{"padding":"` + strings.Repeat("x", 13270) + `","commit":"` + sha + `"}`, locator: sha, ranged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifactRoot := t.TempDir()
			const prefix = "logs/periodic-demo/123/"
			if err := os.MkdirAll(filepath.Join(artifactRoot, prefix), 0o700); err != nil {
				t.Fatal(err)
			}
			var calls []map[string]any
			var citations []analysischat.Citation
			for i := range tc.count {
				path := fmt.Sprintf("%s-%02d.log", strings.Repeat("a", 120), i)
				if err := os.WriteFile(filepath.Join(artifactRoot, prefix, path), []byte(tc.contents+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				args, err := json.Marshal(map[string]any{"path": path, "offset": 0, "length": len(tc.contents) + 1})
				if err != nil {
					t.Fatal(err)
				}
				calls = append(calls, map[string]any{"id": fmt.Sprintf("read-%d", i), "type": "function", "function": map[string]any{"name": "read_artifact", "arguments": string(args)}})
				citation := analysischat.Citation{Path: path, Quote: tc.locator}
				if tc.ranged {
					citation.LineStart, citation.LineEnd = 1, 1
				}
				citations = append(citations, citation)
			}
			answer := strings.Repeat("s", 8<<10)
			final, err := json.Marshal(map[string]any{"answer": answer, "citations": citations})
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				message := map[string]any{"role": "assistant", "content": string(final)}
				finish := "stop"
				if requests.Add(1) == 1 {
					message = map[string]any{"role": "assistant", "tool_calls": calls}
					finish = "tool_calls"
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": finish, "message": message}}}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			backend, err := storage.NewLocalBackend(artifactRoot, "")
			if err != nil {
				t.Fatal(err)
			}
			registry := tools.NewRegistry()
			filesystem.Register(registry)
			enabled, err := registry.Enable([]string{"filesystem"})
			if err != nil {
				t.Fatal(err)
			}
			client := ai.NewClientWithOptions(ai.Options{API: ai.APIChatCompletions, Endpoint: server.URL, Model: "fixture"})
			agent, err := ai.NewAnalysisChatAgent(client, ai.ComposeAnalysisChatSystemPrompt("Project knowledge."), registry, enabled, artifacts.NewBackendFactory(backend, "fixture"), ai.AnalysisChatOptions{
				MaxIters: 4, MaxToolCalls: 16, ContextByteBudget: 512 << 10, Timeout: 20 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			reply, err := agent.Reply(t.Context(), analysischat.Turn{
				SessionID: "boundary", JobID: "periodic-demo", BuildPrefix: prefix, Question: "What stopped?",
				Build:    models.BuildInfo{BuildID: "123", JobName: "periodic-demo"},
				TestCase: models.TestCase{Name: "TestCluster", AIAnalysis: &models.AIAnalysis{RootCause: "The controller stopped."}},
			})
			if err != nil || reply.Unverified || len(reply.Citations) != tc.count {
				t.Fatalf("reply citations=%d unverified=%v err=%v", len(reply.Citations), reply.Unverified, err)
			}
			context := GenerationContext{UserRequest: strings.Repeat("u", maxUserRequestBytes), AssistantAnswer: reply.Answer}
			for _, citation := range reply.Citations {
				if !utf8.ValidString(citation.Quote) || !strings.Contains(citation.Quote, tc.locator) || !strings.Contains(tc.contents, citation.Quote) {
					t.Fatalf("citation lost its verbatim locator: %+v", citation)
				}
				context.ArtifactCitations = append(context.ArtifactCitations, Evidence{Path: citation.Path, LineStart: citation.LineStart, LineEnd: citation.LineEnd, Quote: citation.Quote})
			}
			if err := context.Validate(); err != nil {
				t.Fatalf("actual analysis-chat excerpts cannot generate a fix: %v", err)
			}
		})
	}
}
