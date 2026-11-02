package ai

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/transport"
)

type agentLoopTestTransport func(context.Context, transport.Request) (*transport.Response, error)

func (f agentLoopTestTransport) Complete(ctx context.Context, request transport.Request) (*transport.Response, error) {
	return f(ctx, request)
}

func TestAgenticLoopReservesFinalizationTime(t *testing.T) {
	for _, tc := range []struct {
		name               string
		timeout            time.Duration
		wantInvestigations int
		wantFinalizations  int
	}{
		{name: "tools-only slow rounds", timeout: 85 * time.Second, wantInvestigations: 2, wantFinalizations: 1},
		{name: "enough time for voluntary final", timeout: 5 * time.Minute, wantInvestigations: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				investigations, finalizations := 0, 0
				client := newAgenticTestClient(t, "http://example.test")
				client.transport = agentLoopTestTransport(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
					forced := request.ToolChoice != nil && request.ToolChoice.Name == analysisFinalizeToolName
					if forced {
						finalizations++
						deadline, ok := ctx.Deadline()
						if !ok || time.Until(deadline) <= 25*time.Second {
							t.Fatalf("finalization started without enough time: deadline=%v", deadline)
						}
					} else {
						investigations++
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(25 * time.Second):
					}
					if forced {
						return &transport.Response{HasMessage: true, Message: transport.Message{ToolCalls: []transport.ToolCall{{
							ID: "submit", Type: "function", Function: transport.FunctionCall{Name: analysisFinalizeToolName, Arguments: quotaCleanFinalJSON},
						}}}}, nil
					}
					if tc.wantFinalizations == 0 && investigations == tc.wantInvestigations {
						return &transport.Response{HasMessage: true, Message: transport.Message{Content: new(quotaCleanFinalJSON)}}, nil
					}
					return &transport.Response{HasMessage: true, Message: transport.Message{ToolCalls: []transport.ToolCall{{
						ID: fmt.Sprint(investigations), Type: "function", Function: transport.FunctionCall{Name: "list_artifacts", Arguments: `{"path":""}`},
					}}}}, nil
				})
				in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
					MaxIters: 15, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: tc.timeout,
				})
				started := time.Now()
				_, analysis, err := client.doAnalyzeAgentic(t.Context(), in, "agentic:test:time-reserve", "sys", "user")
				if err != nil {
					t.Fatal(err)
				}
				if analysis.RootCause != "Azure subscription quota exhaustion prevented the worker virtual machine from being created." {
					t.Fatalf("final analysis = %+v", analysis)
				}
				if investigations != tc.wantInvestigations || finalizations != tc.wantFinalizations {
					t.Fatalf("requests: investigations=%d finalizations=%d, want %d/%d", investigations, finalizations, tc.wantInvestigations, tc.wantFinalizations)
				}
				if time.Since(started) >= tc.timeout {
					t.Fatal("analysis exhausted its deadline")
				}
			})
		})
	}
}

func TestAgenticLoopTimeReserveRetainsStrongerDraft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		investigations, finalizations := 0, 0
		client := newAgenticTestClient(t, "http://example.test")
		client.transport = agentLoopTestTransport(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
			if request.ToolChoice != nil && request.ToolChoice.Name == analysisFinalizeToolName {
				finalizations++
				time.Sleep(25 * time.Second)
				return &transport.Response{HasMessage: true, Message: transport.Message{ToolCalls: []transport.ToolCall{{
					ID: "submit", Type: "function", Function: transport.FunctionCall{Name: analysisFinalizeToolName, Arguments: providerIDPuntFinalJSON},
				}}}}, nil
			}
			investigations++
			if investigations > 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			time.Sleep(40 * time.Second)
			return &transport.Response{HasMessage: true, Message: transport.Message{
				Content: new(quotaCleanFinalJSON),
				ToolCalls: []transport.ToolCall{{
					ID: "list", Type: "function", Function: transport.FunctionCall{Name: "list_artifacts", Arguments: `{"path":""}`},
				}},
			}}, nil
		})
		in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
			MaxIters: 15, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 85 * time.Second,
		})
		summary, analysis, err := client.doAnalyzeAgentic(t.Context(), in, "agentic:test:time-reserve-retained-draft", "sys", "user")
		if err != nil {
			t.Fatal(err)
		}
		if summary.Summary != "quota blocked" || !analysis.CritiquePassed {
			t.Fatalf("stronger draft lost: summary=%+v analysis=%+v", summary, analysis)
		}
		if investigations != 1 || finalizations != 1 {
			t.Fatalf("requests: investigations=%d finalizations=%d, want 1/1", investigations, finalizations)
		}
	})
}

func TestAgenticLoopFirstRequestTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client := newAgenticTestClient(t, "http://example.test")
		client.transport = agentLoopTestTransport(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
			calls++
			if request.ToolChoice != nil {
				t.Fatal("timed-out analysis requested finalization")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		})
		in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
			MaxIters: 15, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 10 * time.Second,
		})
		_, _, err := client.doAnalyzeAgentic(t.Context(), in, "agentic:test:first-timeout", "sys", "user")
		if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
			t.Fatalf("error=%v requests=%d, want first-request deadline error", err, calls)
		}
	})
}

func TestAgenticLoopCancellationDoesNotFinalize(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		client := newAgenticTestClient(t, "http://example.test")
		client.transport = agentLoopTestTransport(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
			calls++
			if request.ToolChoice != nil {
				t.Fatal("canceled analysis requested finalization")
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			time.Sleep(6 * time.Second)
			cancel()
			return &transport.Response{HasMessage: true, Message: transport.Message{ToolCalls: []transport.ToolCall{{
				ID: "list", Type: "function", Function: transport.FunctionCall{Name: "list_artifacts", Arguments: `{"path":""}`},
			}}}}, nil
		})
		in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
			MaxIters: 15, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 10 * time.Second,
		})
		_, _, err := client.doAnalyzeAgentic(ctx, in, "agentic:test:canceled", "sys", "user")
		if !errors.Is(err, context.Canceled) || calls != 2 {
			t.Fatalf("error=%v requests=%d, want cancellation after investigation", err, calls)
		}
	})
}

func TestAgenticLoopFollowupFailureRetainsDraft(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timedOut bool
	}{
		{name: "provider error"},
		{name: "deadline exceeded", timedOut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := newAgenticTestClient(t, "http://example.test")
				client.transport = agentLoopTestTransport(func(ctx context.Context, request transport.Request) (*transport.Response, error) {
					calls++
					if request.ToolChoice != nil {
						t.Fatal("retained draft requested finalization")
					}
					if calls > 1 {
						if tc.timedOut {
							<-ctx.Done()
							return nil, ctx.Err()
						}
						return nil, errors.New("provider unavailable")
					}
					time.Sleep(time.Second)
					return &transport.Response{HasMessage: true, Message: transport.Message{
						Content: new(quotaCleanFinalJSON),
						ToolCalls: []transport.ToolCall{{
							ID: "list", Type: "function", Function: transport.FunctionCall{Name: "list_artifacts", Arguments: `{"path":""}`},
						}},
					}}, nil
				})
				in := newTestAgenticInputs(t, &fakeBrowser{}, AgenticOptions{
					MaxIters: 15, ModelByteBudget: 100_000, GCSByteBudget: 100_000, Timeout: 30 * time.Second,
				})
				summary, analysis, err := client.doAnalyzeAgentic(t.Context(), in, "agentic:test:followup-error", "sys", "user")
				if err != nil {
					t.Fatal(err)
				}
				if summary.Summary != "quota blocked" || !analysis.CritiquePassed || calls != 2 {
					t.Fatalf("retained draft: summary=%+v analysis=%+v requests=%d", summary, analysis, calls)
				}
			})
		})
	}
}
