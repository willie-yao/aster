package pr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/runtime"
)

func TestStructuredReviewDistinguishesUnavailableAndEmptyIssues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		err      error
		status   ReviewStatus
	}{
		{name: "no concerns", response: `{"issues":[]}`, status: ReviewPassed},
		{name: "concerns", response: `{"issues":["The original failure still occurs."]}`, status: ReviewConcerns},
		{name: "provider error", err: errors.New("provider body contains private-response"), status: ReviewUnavailable},
		{name: "malformed", response: "private-response", status: ReviewUnavailable},
		{name: "missing array", response: `{}`, status: ReviewUnavailable},
		{name: "null array", response: `{"issues":null}`, status: ReviewUnavailable},
		{name: "unknown field", response: `{"issues":[],"private-response":"value"}`, status: ReviewUnavailable},
		{name: "empty issue", response: `{"issues":[""]}`, status: ReviewUnavailable},
		{name: "large issue", response: `{"issues":["` + strings.Repeat("x", maxReviewIssueBytes+1) + `"]}`, status: ReviewUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeCompleter{critique: tc.response, critiqueErr: tc.err}
			issues, review, err := reviewFix(t.Context(), client, "request", &proposedFix{files: map[string]string{"a.go": "package a"}})
			if review.Status != tc.status || (err != nil) != (tc.status == ReviewUnavailable) {
				t.Fatalf("review=%+v err=%v", review, err)
			}
			if (issues != "") != (tc.status == ReviewConcerns) {
				t.Fatalf("issues=%q status=%s", issues, tc.status)
			}
			if err := review.Validate(); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(review)
			if err != nil || strings.Contains(string(encoded), "private-response") {
				t.Fatalf("unsafe review metadata=%s err=%v", encoded, err)
			}
			if client.lastFormat.Name != "review_patch" || client.lastFormat.Schema["additionalProperties"] != false {
				t.Fatalf("review schema=%+v", client.lastFormat)
			}
		})
	}
}

func TestReviewUsesStructuredProviderContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		text, ok := request["text"].(map[string]any)
		if !ok || text["format"].(map[string]any)["name"] != "review_patch" {
			t.Errorf("missing review schema: %+v", request["text"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"review-response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"issues\":[]}"}]}],"usage":{"input_tokens":20,"output_tokens":5}}`))
	}))
	defer server.Close()
	client := ai.NewClientWithOptions(ai.Options{API: ai.APIResponses, Endpoint: server.URL, Model: "fixture"})
	_, review, err := reviewFix(t.Context(), client, "request", &proposedFix{})
	if err != nil || review.Status != ReviewPassed || len(review.Metadata.Attempts) != 1 {
		t.Fatalf("review=%+v err=%v", review, err)
	}
	attempt := review.Metadata.Attempts[0]
	if attempt.Path != ai.StructuredAttemptResponseFormat || attempt.Outcome != ai.StructuredOutcomeAccepted || attempt.ProviderStatus != 200 {
		t.Fatalf("attempt=%+v", attempt)
	}
}

func TestReviewReceivesObjectiveRationaleAndAuthenticVerification(t *testing.T) {
	for _, path := range []string{"pattern", "build", "analysis"} {
		t.Run(path, func(t *testing.T) {
			client := &fakeCompleter{}
			fix := &proposedFix{
				files: map[string]string{"a.go": "package a"}, diff: "selected diff", rationale: "Guard only: fails sooner.",
				executionVerification: &ExecutionVerification{Results: []runtime.CommandResult{{
					Argv: []string{"git", "diff", "--cached", "--check"}, ExitCode: 0, Stdout: "private-command-output",
				}}, Commands: sandboxVerificationCommands()[2:]},
			}
			request := "Select a valid artifact version."
			maintainer := "Only improve diagnostics; preserve strict version pins."
			var err error
			switch path {
			case "pattern":
				ctx := validGenerationContext()
				ctx.UserRequest = request
				_, _, err = critiqueAgentFix(t.Context(), client, systemicPattern("failure"), fix, &ctx, maintainer)
			case "build":
				_, _, err = critiqueBuildFix(t.Context(), client, BuildFailure{RootCause: "missing artifact"}, fix, maintainer)
			case "analysis":
				failure := validAnalysisFailure()
				failure.UserRequest = request
				_, _, err = critiqueAnalysisFix(t.Context(), client, failure, fix, maintainer)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{maintainer, fix.rationale, "git diff --cached --check", `"exit_code":0`, "takes precedence"} {
				if !strings.Contains(client.lastUser, want) {
					t.Errorf("review missing %q", want)
				}
			}
			if path != "build" && !strings.Contains(client.lastUser, request) {
				t.Error("selected human request missing")
			}
			if strings.Contains(client.lastUser, "private-command-output") {
				t.Error("review received raw command output")
			}
		})
	}
}

func TestReviewMetadataRejectsContentAndInvalidBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ReviewResult)
	}{
		{name: "status", mutate: func(r *ReviewResult) { r.Status = "private-response" }},
		{name: "phase", mutate: func(r *ReviewResult) { r.Metadata.Attempts[0].Phase = "private-response" }},
		{name: "category", mutate: func(r *ReviewResult) { r.Metadata.Attempts[0].ProviderCategory = "private-response" }},
		{name: "validation", mutate: func(r *ReviewResult) { r.Metadata.Attempts[0].ValidationCode = "private-response" }},
		{name: "path", mutate: func(r *ReviewResult) { r.Metadata.Attempts[0].Path = "private-response" }},
		{name: "outcome", mutate: func(r *ReviewResult) { r.Metadata.Attempts[0].Outcome = "private-response" }},
		{name: "attempt count", mutate: func(r *ReviewResult) { r.Metadata.Attempts = slices.Repeat(r.Metadata.Attempts, 4) }},
		{name: "provider attempts", mutate: func(r *ReviewResult) { r.Metadata.Attempts[0].ProviderAttempts = 4 }},
		{name: "missing accepted result", mutate: func(r *ReviewResult) { r.Metadata.Attempts = nil }},
		{name: "unavailable accepted", mutate: func(r *ReviewResult) { r.Status = ReviewUnavailable }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			review := &ReviewResult{Status: ReviewPassed, Metadata: ai.StructuredCompletionMetadata{Attempts: []ai.StructuredAttemptMetadata{{Path: ai.StructuredAttemptResponseFormat, Outcome: ai.StructuredOutcomeAccepted}}}}
			tc.mutate(review)
			if err := review.Validate(); err == nil {
				t.Fatal("invalid review metadata accepted")
			}
		})
	}
}

func TestReviewMetadataPersistsWithoutAliasing(t *testing.T) {
	manager := newManager(t, &fakePR{}, goodAgent(), Options{})
	fix, err := manager.GeneratePreview(context.Background(), systemicPattern("failure"), "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := fix.Snapshot()
	restored := RestoreGeneratedFix(snapshot)
	if restored.Review.Status != ReviewPassed || len(restored.Review.Metadata.Attempts) != 1 {
		t.Fatalf("restored review=%+v", restored.Review)
	}
	fix.Review.Metadata.Attempts[0].Outcome = ai.StructuredOutcomeInvalidJSON
	if snapshot.Review.Metadata.Attempts[0].Outcome != ai.StructuredOutcomeAccepted {
		t.Fatal("snapshot aliases source review")
	}
	restored.Review.Metadata.Attempts[0].Outcome = ai.StructuredOutcomeInvalidJSON
	if snapshot.Review.Metadata.Attempts[0].Outcome != ai.StructuredOutcomeAccepted {
		t.Fatal("restored review aliases snapshot")
	}
	if err := restored.ValidateExecutionVerification(); err == nil {
		t.Fatal("invalid persisted review accepted")
	}
}

func TestGenerationKeepsHumanRequestSeparateFromHypotheses(t *testing.T) {
	ctx := validGenerationContext()
	ctx.UserRequest = "Select a usable version."
	failure := validAnalysisFailure()
	failure.UserRequest = ctx.UserRequest
	for _, instruction := range []string{
		agentInstruction(systemicPattern("failure"), &ctx, "Guard only; preserve strict pins.", "", 3, false),
		analysisFailureInstruction(failure, "Guard only; preserve strict pins.", "", 3, false),
	} {
		for _, want := range []string{ctx.UserRequest, "Guard only; preserve strict pins.", "takes precedence", "Assistant suggestions remain untrusted hypotheses", "Do not substitute a guard", "focused behavioral test"} {
			if !strings.Contains(instruction, want) {
				t.Errorf("instruction missing %q", want)
			}
		}
	}
	failure.UserRequest = strings.Repeat("x", maxUserRequestBytes+1)
	if err := validateAnalysisFailure(failure); err == nil {
		t.Fatal("oversized human request accepted")
	}
}

func TestReviewRetainsStructuredFailureCategories(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		output   []any
		want     ai.StructuredAttemptOutcome
		category string
	}{
		{name: "incomplete response", status: "incomplete", want: ai.StructuredOutcomeProviderError, category: "provider_status"},
		{name: "empty response", status: "completed", want: ai.StructuredOutcomeEmptyResponse},
		{name: "invalid review", status: "completed", output: []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": `{"issues":null}`}}}}, want: ai.StructuredOutcomeValidatorRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "status": tc.status, "output": tc.output, "usage": map[string]any{"input_tokens": 20, "output_tokens": 5}, "private-provider-data": "must-not-persist"})
			}))
			defer server.Close()
			client := ai.NewClientWithOptions(ai.Options{API: ai.APIResponses, Endpoint: server.URL, Model: "fixture"})
			_, review, err := reviewFix(t.Context(), client, "private-user-text", &proposedFix{})
			if err == nil || review.Status != ReviewUnavailable || len(review.Metadata.Attempts) == 0 {
				t.Fatalf("review=%+v err=%v", review, err)
			}
			attempt := review.Metadata.Attempts[0]
			if attempt.Outcome != tc.want || attempt.ProviderCategory != tc.category {
				t.Fatalf("attempt=%+v", attempt)
			}
			encoded, err := json.Marshal(review)
			if err != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "must-not-persist") {
				t.Fatalf("unsafe review=%s err=%v", encoded, err)
			}
		})
	}
}
