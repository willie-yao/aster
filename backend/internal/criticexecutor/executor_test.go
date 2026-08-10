package criticexecutor

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/causalcritic"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

func executorInput(t *testing.T) causalcritic.Input {
	t.Helper()
	bundle, err := agentanalysis.NewEvidenceBundle(
		ai.FailureAnalysisRequest{
			JobID: "periodic::job", BuildPrefix: "logs/job/1/", Build: models.BuildInfo{BuildID: "1", JobName: "job"},
			TestCase: models.TestCase{Name: "TestFailure", Status: "failed", FailureMessage: "API unavailable"},
		},
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		agentanalysis.ArtifactScan{PathCount: 1}, nil,
		[]agentanalysis.EvidenceExcerpt{{Path: "build-log.txt", Kind: "tail", Content: "API widgets.example.io/v2 returned 404 unsupported\nlater the controller became Ready\n"}},
		strings.Repeat("b", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	input, err := causalcritic.NewInput(bundle, agentanalysis.AuthoritativeSnapshot{
		Summary: "The controller was not ready.", RootCause: "Readiness caused the failure.", Severity: "High", SuggestedFix: "Inspect readiness.",
		EvidenceCitations: []models.EvidenceCitation{{Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: "API widgets.example.io/v2 returned 404 unsupported"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func executorRequest(t *testing.T, endpoint string) causalcritic.ExecutionRequest {
	t.Helper()
	request := causalcritic.ExecutionRequest{
		SchemaVersion: causalcritic.ExecutionSchemaVersion, ContractVersion: causalcritic.ContractVersion, Input: executorInput(t),
		ModelGateway:   engineruntime.ModelGatewayConfig{Endpoint: endpoint, Model: "configured-model", ProtocolVersion: "openai-chat-completions-v1"},
		TimeoutSeconds: 30, OutputLimit: causalcritic.DefaultOutputLimit,
	}
	if err := causalcritic.ValidateExecutionRequest(request); err != nil {
		t.Fatal(err)
	}
	return request
}

func internalGatewayTestClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target.Host)
	}
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = target.Hostname()
	return &http.Client{Transport: transport}
}

func TestExecuteUsesCredentialFreeGatewayAndReportedUsage(t *testing.T) {
	var request causalcritic.ExecutionRequest
	var sawAuthorization, sawAPIKey bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization") != ""
		sawAPIKey = r.Header.Get("X-API-Key") != "" || r.Header.Get("Api-Key") != ""
		review := causalcritic.Review{
			SchemaVersion: causalcritic.ReviewSchemaVersion, ContractVersion: causalcritic.ContractVersion,
			PairHash: request.Input.PairHash, Verdict: "pass", Findings: []causalcritic.Finding{}, Confidence: "medium",
		}
		content, _ := json.Marshal(review)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "gateway-reported-model", "provider": "gateway-provider", "cost_usd": "0.0012",
			"choices":       []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(content)}}},
			"usage":         map[string]any{"prompt_tokens": 120, "completion_tokens": 18, "prompt_tokens_details": map[string]any{"cached_tokens": 20}},
			"copilot_usage": map[string]any{"total_nano_aiu": 12345},
		})
	}))
	defer server.Close()
	request = executorRequest(t, "https://gateway.models.svc.cluster.local/v1")
	result := Execute(t.Context(), request, Options{HTTPClient: internalGatewayTestClient(t, server)})
	if result.TerminalState != engineruntime.TerminalSucceeded || result.Review == nil || result.Usage.Status != "reported" || result.Usage.Model != "gateway-reported-model" || result.Usage.Provider != "github-copilot" || result.Usage.NanoAIU != 12345 || result.Usage.InputTokens != 120 || result.Usage.CachedInputTokens != 20 || result.Usage.CostUSD != "0.0012" {
		t.Fatalf("result = %+v", result)
	}
	if sawAuthorization || sawAPIKey {
		t.Fatalf("gateway received credential headers: authorization=%v api-key=%v", sawAuthorization, sawAPIKey)
	}
}

func TestExecuteRejectsMalformedReview(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "gateway-model", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"verdict":"pass"}`}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 1},
		})
	}))
	defer server.Close()
	request := executorRequest(t, "https://gateway.models.svc.cluster.local/v1")
	result := Execute(t.Context(), request, Options{HTTPClient: internalGatewayTestClient(t, server)})
	if result.TerminalState != engineruntime.TerminalFailed || result.Review != nil || !strings.Contains(result.FailureReason, "deterministic validation") {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecuteCancellation(t *testing.T) {
	request := executorRequest(t, "https://gateway.models.svc.cluster.local/v1")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := Execute(ctx, request, Options{HTTPClient: &http.Client{Timeout: time.Second}})
	if result.TerminalState != engineruntime.TerminalCancelled {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecuteRejectsGatewayRedirect(t *testing.T) {
	targetCalled := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalled = true }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	request := executorRequest(t, "https://gateway.models.svc.cluster.local/v1")
	result := Execute(t.Context(), request, Options{HTTPClient: internalGatewayTestClient(t, redirect)})
	if result.TerminalState != engineruntime.TerminalFailed || targetCalled {
		t.Fatalf("result=%+v targetCalled=%v", result, targetCalled)
	}
}
