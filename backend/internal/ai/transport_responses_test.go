package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai/transport"
	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
)

func TestResponsesTraceRecordsRetryCount(t *testing.T) {
	shrinkCallDelay(t)
	calls := 0
	wireBytes := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		wireBytes += len(body)
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-retry","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`))
	}))
	defer s.Close()
	c := NewClientWithOptions(Options{API: APIResponses, Endpoint: s.URL, Model: "m"})
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", APIMode: APIResponses})
	ctx := withAnalysisTrace(context.Background(), trace)
	if _, err := c.callModel(ctx, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	event := store.Snapshot().Traces[0].Events[0]
	if event.Attempts != 2 || event.ResponseID != "resp-retry" || event.ServiceTier != "" || event.WireRequestBytes != wireBytes {
		t.Fatalf("event = %+v", event)
	}
}

func TestResponsesFlexFallsBackToAutoAndTracesEchoedTier(t *testing.T) {
	shrinkCallDelay(t)
	var tiers []string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		tier, _ := request["service_tier"].(string)
		tiers = append(tiers, tier)
		calls++
		if calls <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Resource unavailable, try again later"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp-flex","status":"completed","service_tier":"auto","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`))
	}))
	defer server.Close()

	api := transport.NewClient(
		modelprovider.Config{API: APIResponses, Endpoint: server.URL, Model: "m"},
		"", nil, modelprovider.ServiceTierFlex, recordServiceTierFallback,
	)
	client := &Client{
		api: api, transport: throttledTransport{api}, apiMode: APIResponses,
		model: "m", serviceTier: modelprovider.ServiceTierFlex, cache: NewCache(t.TempDir()),
	}
	store := NewTraceStore()
	trace := store.Start(TraceMetadata{JobID: "job", APIMode: APIResponses})
	ctx := withAnalysisTrace(context.Background(), trace)
	response, err := client.callModel(ctx, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	trace.Finish("success", nil)
	if response.Attempts != 3 || response.ServiceTier != modelprovider.ServiceTierAuto {
		t.Fatalf("response = %+v", response)
	}
	if want := []string{modelprovider.ServiceTierFlex, modelprovider.ServiceTierFlex, modelprovider.ServiceTierAuto}; !slices.Equal(tiers, want) {
		t.Fatalf("tiers = %v, want %v", tiers, want)
	}
	events := store.Snapshot().Traces[0].Events
	if len(events) != 2 || events[0].Kind != "service_tier" || events[0].Outcome != "fallback" ||
		events[0].Status != modelprovider.ServiceTierAuto || events[1].Kind != "model_request" ||
		events[1].ServiceTier != modelprovider.ServiceTierAuto {
		t.Fatalf("trace = %+v", events)
	}
}

func TestResponsesFlexFallbackTracesFailedAndCancelledRetries(t *testing.T) {
	shrinkCallDelay(t)
	for _, cancelled := range []bool{false, true} {
		name := "provider failure"
		if cancelled {
			name = "cancelled before retry"
		}
		t.Run(name, func(t *testing.T) {
			var tiers []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				tiers = append(tiers, request["service_tier"].(string))
				if len(tiers) <= 2 {
					w.Header().Set("Retry-After", "0")
					if cancelled && len(tiers) == 2 {
						w.Header().Set("Retry-After", "60")
					}
					http.Error(w, "Resource unavailable", http.StatusTooManyRequests)
					return
				}
				http.Error(w, "private provider body", http.StatusServiceUnavailable)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			callbacks := 0
			api := transport.NewClient(
				modelprovider.Config{API: APIResponses, Endpoint: server.URL, Model: "m"},
				"", nil, modelprovider.ServiceTierFlex,
				func(ctx context.Context, tier string) {
					callbacks++
					recordServiceTierFallback(ctx, tier)
					if cancelled {
						cancel()
					}
				},
			)
			client := &Client{model: "m", apiMode: APIResponses, transport: throttledTransport{api}}
			recorder, err := aiusage.NewRecorder("", aiusage.RecorderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, operation := aiusage.Begin(ctx, recorder, aiusage.Metadata{
				LogicalID: "fallback", Origin: aiusage.OriginFetcher, Feature: aiusage.FeatureFailureAnalysis,
			})
			store := NewTraceStore()
			trace := store.Start(TraceMetadata{JobID: "job", APIMode: APIResponses})
			response, err := client.callModel(withAnalysisTrace(ctx, trace), nil, nil, nil)
			if err == nil || callbacks != 1 {
				t.Fatalf("error=%v callbacks=%d", err, callbacks)
			}
			wantAttempts, wantStatus, wantCode := 3, http.StatusServiceUnavailable, "http_error"
			wantTiers := []string{modelprovider.ServiceTierFlex, modelprovider.ServiceTierFlex, modelprovider.ServiceTierAuto}
			if cancelled {
				wantAttempts, wantStatus, wantCode = 2, 0, "context_canceled"
				wantTiers = wantTiers[:2]
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context cancellation", err)
				}
			}
			if response == nil || response.Attempts != wantAttempts || response.HTTPStatus != wantStatus ||
				!slices.Equal(tiers, wantTiers) {
				t.Fatalf("response=%+v tiers=%v", response, tiers)
			}
			trace.Finish("error", err)
			operation.Finish(aiusage.OutcomeError)
			events := store.Snapshot().Traces[0].Events
			if len(events) != 2 || events[0].Kind != "service_tier" || events[0].Outcome != "fallback" ||
				events[0].Status != modelprovider.ServiceTierAuto || events[1].Kind != "model_request" ||
				events[1].Outcome != "error" || events[1].ErrorCode != wantCode || events[1].Attempts != wantAttempts {
				t.Fatalf("trace = %+v", events)
			}
			totals := recorder.Snapshot().Days[0].Totals
			if totals.ModelRequests != 1 || totals.UnreportedRequests != 1 || totals.Failures != 1 {
				t.Fatalf("usage totals = %+v", totals)
			}
		})
	}
}

func TestServiceTierRejectsNonOpenAIEndpointBeforeProviderIO(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client := NewClientWithOptions(Options{
		API: APIResponses, Endpoint: server.URL, Model: "m", ServiceTier: modelprovider.ServiceTierFlex,
	})
	if _, err := client.callModel(context.Background(), nil, nil, nil); err == nil || !strings.Contains(err.Error(), "api.openai.com") {
		t.Fatalf("error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("provider calls = %d, want 0", calls)
	}
}

func TestServiceTierConfiguration(t *testing.T) {
	valid := NewClientWithOptions(Options{
		API: APIResponses, Endpoint: "https://api.openai.com/v1/responses", Model: "m", ServiceTier: " FLEX ",
	})
	if err := valid.ValidateConfiguration(); err != nil || valid.ServiceTier() != modelprovider.ServiceTierFlex {
		t.Fatalf("valid flex client: tier=%q err=%v", valid.ServiceTier(), err)
	}
	for _, opts := range []Options{
		{API: APIChatCompletions, Endpoint: "https://api.openai.com/v1/chat/completions", Model: "m", ServiceTier: modelprovider.ServiceTierFlex},
		{API: APIResponses, Endpoint: "https://api.githubcopilot.com/responses", Model: "m", ServiceTier: modelprovider.ServiceTierFlex},
	} {
		if err := NewClientWithOptions(opts).ValidateConfiguration(); err == nil {
			t.Fatalf("invalid service tier configuration passed: %+v", opts)
		}
	}
}
