package onboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func testPromptFallbackFailure() *promptPreparationFailure {
	return &promptPreparationFailure{
		Stage:    promptStageEvidenceExtraction,
		Category: promptFailureInvalidStructured,
		cause:    errors.New("raw provider body with private model output"),
	}
}

func TestPromptFailureWarningIsSafeAndActionable(t *testing.T) {
	failure := testPromptFallbackFailure()
	var out bytes.Buffer
	writePromptFailure(&out, "prompts/system.md generation failed", failure, "reviewable TODO template")
	text := out.String()
	for _, want := range []string{
		"stage: structured evidence extraction",
		"reason: the provider returned no valid structured response",
		"fallback: reviewable TODO template",
		"action:",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("warning missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "raw provider body") || strings.Contains(text, failure.cause.Error()) {
		t.Fatalf("warning exposed wrapped error: %s", text)
	}
}

func TestPromptDebugOutputIsSanitized(t *testing.T) {
	const token = "fixture-ai-secret"
	const githubToken = "fixture-github-secret"
	privateModel := "private-model-name"
	privateEndpoint := "https://private.example.internal/tenant/acme/responses?api-version=private#fragment"
	var out bytes.Buffer
	debug := newPromptDebugger(true, &out, token, githubToken)
	debug.provider(Options{AIAPI: "responses", AIEndpoint: privateEndpoint, AIModel: privateModel})
	debug.source(promptSource{Path: "docs/" + token + ".md", StartLine: 10, EndLine: 12, Text: "private source contents " + githubToken})
	debug.sourceSummary([]promptSource{{Text: "private source contents"}}, 7, 3)
	debug.failure(&promptPreparationFailure{
		Stage:    promptStageEvidenceExtraction,
		Category: promptFailureProviderRateLimited,
		Debug: promptFailureDebug{
			StructuredAttempt: "json-schema",
			HTTPStatus:        429,
			RetryAfter:        "12",
			RequestID:         "request-" + token,
			ValidationCode:    "content-grounding",
			ValidationField:   "evidence",
			Phase:             "revision",
			RetainedInitial:   true,
		},
	})
	text := out.String()
	for _, want := range []string{
		"api=responses",
		"endpoint_host=private.example.internal",
		"model_fingerprint=sha256:",
		"lines=10-12",
		"source_count=1",
		"matched_prow_jobs=7",
		"structured_transport_attempt=json-schema",
		"http_status=429",
		"retry_after=12",
		"validation_code=content-grounding",
		"retained_initial=true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("debug output missing %q:\n%s", want, text)
		}
	}
	for _, prohibited := range []string{token, githubToken, privateModel, "/tenant/acme", "api-version", "fragment", "private source contents"} {
		if strings.Contains(text, prohibited) {
			t.Fatalf("debug output exposed %q:\n%s", prohibited, text)
		}
	}
}

func TestPromptPlanOmitsCredentialsAndFailurePayloads(t *testing.T) {
	const token = "fixture-ai-secret"
	const rawSource = "private source contents"
	const modelOutput = "raw model output"
	failure := &promptPreparationFailure{
		Stage:    promptStageEvidenceGrounding,
		Category: promptFailureUngroundedEvidence,
		cause:    errors.New(token + " " + rawSource + " " + modelOutput),
	}
	result := newAPIFallbackResult(failure)
	plan := result.promptPlan(Options{
		AIToken: token, AIAPI: "responses",
		AIEndpoint: "https://private.example.internal/tenant/responses",
		AIModel:    "private-model",
	})
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		`"requested_mode":"api-experimental"`,
		`"final_status":"api-fallback"`,
		`"output":"todo-template"`,
		`"failure_stage":"evidence-grounding-validation"`,
		`"failure_category":"ungrounded-evidence"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plan missing %s: %s", want, text)
		}
	}
	for _, prohibited := range []string{token, rawSource, modelOutput, "private.example.internal", "private-model"} {
		if strings.Contains(text, prohibited) {
			t.Fatalf("plan exposed %q: %s", prohibited, text)
		}
	}
}

func TestBuildPlanWithPromptRecovery(t *testing.T) {
	fallback := newAPIFallbackResult(testPromptFallbackFailure())
	success := newAPIPromptResult()

	t.Run("retry uses the same reviewed provider", func(t *testing.T) {
		deps, _, _, _ := wizardDependencies("")
		prompts := &fakePromptBuilder{results: []promptPreparationResult{fallback, success}}
		deps.prompts = prompts
		ui := &queuedWizardUI{selects: []string{"retry"}}
		opts := testPromptRecoveryOptions()
		plan, err := buildPlanWithPromptRecovery(context.Background(), opts, planningContext{}, deps, ui)
		if err != nil {
			t.Fatal(err)
		}
		if prompts.calls != 2 || plan.Prompt.FinalStatus != string(promptStatusAPIDraft) {
			t.Fatalf("calls=%d prompt=%+v", prompts.calls, plan.Prompt)
		}
		if len(ui.selectPrompts) != 1 || ui.selectPrompts[0].Value != "template" {
			t.Fatalf("recovery prompt = %+v", ui.selectPrompts)
		}
		values := ui.selectPrompts[0].Options
		if len(values) != 3 || values[0].Value != "retry" || values[1].Value != "template" || values[2].Value != "cancel" {
			t.Fatalf("recovery choices = %+v", values)
		}
		if prompts.gotOpts.AIAPI != opts.AIAPI || prompts.gotOpts.AIEndpoint != opts.AIEndpoint || prompts.gotOpts.AIModel != opts.AIModel {
			t.Fatalf("retry changed provider coordinates: %+v", prompts.gotOpts)
		}
	})

	t.Run("continue defaults to the template", func(t *testing.T) {
		deps, _, _, _ := wizardDependencies("")
		prompts := &fakePromptBuilder{result: fallback}
		deps.prompts = prompts
		ui := &queuedWizardUI{selects: []string{usePromptDefault}}
		plan, err := buildPlanWithPromptRecovery(context.Background(), testPromptRecoveryOptions(), planningContext{}, deps, ui)
		if err != nil {
			t.Fatal(err)
		}
		if prompts.calls != 1 || plan.Prompt.Source != "TODO template after experimental API failure" {
			t.Fatalf("calls=%d prompt=%+v", prompts.calls, plan.Prompt)
		}
	})

	t.Run("cancel stops onboarding", func(t *testing.T) {
		deps, _, _, _ := wizardDependencies("")
		deps.prompts = &fakePromptBuilder{result: fallback}
		ui := &queuedWizardUI{selects: []string{"cancel"}}
		_, err := buildPlanWithPromptRecovery(context.Background(), testPromptRecoveryOptions(), planningContext{}, deps, ui)
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("error = %v", err)
		}
	})
}

func testPromptRecoveryOptions() Options {
	opts := testOpts()
	opts.OutDir = "out"
	opts.AIToken = "fixture-ai-token"
	opts.AIAPI = "responses"
	opts.AIEndpoint = "https://provider.example/v1/responses"
	opts.AIModel = "fixture-model"
	return opts
}

func TestRequirePromptDraftFailsBeforeWrites(t *testing.T) {
	deps, _, writer, _ := wizardDependencies("")
	failure := testPromptFallbackFailure()
	deps.prompts = &fakePromptBuilder{result: newAPIFallbackResult(failure)}
	pullRequests := deps.pullRequests.(*fakePullRequestWriter)
	opts := testPromptRecoveryOptions()
	opts.RequirePromptDraft = true
	err := run(context.Background(), opts, deps)
	var strictErr *requiredPromptDraftError
	if !errors.As(err, &strictErr) {
		t.Fatalf("error = %v", err)
	}
	if writer.writes != 0 || pullRequests.calls != 0 {
		t.Fatalf("writes=%d pull requests=%d", writer.writes, pullRequests.calls)
	}
	if strings.Contains(err.Error(), failure.cause.Error()) {
		t.Fatalf("strict error exposed raw failure: %v", err)
	}
}

func TestRequirePromptDraftPreflightStopsBeforeSourceRetrieval(t *testing.T) {
	opts := testOpts()
	opts.RequirePromptDraft = true
	data := buildScaffoldData(opts, nil)
	var out, errOut bytes.Buffer
	_, result, err := buildSystemPrompt(context.Background(), opts, data, promptDraftInput{
		ProjectName: data.Name,
		SourceRepo:  Repo{Owner: "example", Name: "project", FullName: "example/project"},
	}, &out, &errOut)
	var strictErr *requiredPromptDraftError
	if !errors.As(err, &strictErr) {
		t.Fatalf("error = %v", err)
	}
	if result.Failure == nil || result.Failure.Stage != promptStageTokenPreflight || result.Failure.Category != promptFailureMissingToken {
		t.Fatalf("result = %+v", result)
	}
	if strings.Contains(out.String(), "Drafting prompts/system.md") {
		t.Fatalf("source retrieval started before token preflight: %s", out.String())
	}
	if !strings.Contains(errOut.String(), "AI_TOKEN is not set") {
		t.Fatalf("preflight output = %s", errOut.String())
	}
}

func TestPrintReviewShowsPromptOutcomeAndSafeFailure(t *testing.T) {
	failure := &promptPreparationFailure{Stage: promptStageEvidenceExtraction, Category: promptFailureInvalidStructured}
	plans := []PromptPlan{
		newTemplatePromptResult().promptPlan(Options{}),
		newAPIPromptResult().promptPlan(Options{AIAPI: "responses", AIEndpoint: "https://provider.example/v1/responses", AIModel: "model"}),
		newAPIFallbackResult(failure).promptPlan(Options{}),
	}
	for _, prompt := range plans {
		var out bytes.Buffer
		printReview(&out, &Plan{
			SourceRepo: Repo{FullName: "example/project"}, DashboardRepo: Repo{FullName: "example/dashboard"},
			Project: project.Config{ID: "example", Name: "Example"}, Prompt: prompt, Files: map[string]string{"prompts/system.md": "template"},
		})
		if !strings.Contains(out.String(), "Prompt:               "+prompt.Source) {
			t.Fatalf("review omitted %q:\n%s", prompt.Source, out.String())
		}
	}
}

func TestPromptPreparationStageLabels(t *testing.T) {
	stages := map[promptPreparationStage]string{
		promptStageTokenPreflight:        "token preflight",
		promptStageSourceRevision:        "source revision resolution",
		promptStageSourceTree:            "source tree listing",
		promptStageSourceExcerpt:         "source excerpt retrieval",
		promptStageEvidenceExtraction:    "structured evidence extraction",
		promptStageEvidenceGrounding:     "evidence grounding validation",
		promptStageStructuredRevision:    "structured revision",
		promptStageFinalPromptValidation: "final rendering and prompt validation",
	}
	if len(stages) != 8 {
		t.Fatalf("stages = %d", len(stages))
	}
	for stage, want := range stages {
		if got := stage.label(); got != want {
			t.Errorf("stage %q label = %q, want %q", stage, got, want)
		}
	}
}

func TestPromptPlanRecordsReviewedTimeout(t *testing.T) {
	result := newAPIPromptResult()
	plan := result.promptPlan(Options{
		AIAPI: "chat_completions", AIEndpoint: "https://provider.example/v1/chat/completions",
		AIModel: "model", PromptTimeout: 30 * time.Minute,
	})
	if plan.Timeout != "30m0s" {
		t.Fatalf("timeout = %q", plan.Timeout)
	}
	if err := validatePromptPlan(plan); err != nil {
		t.Fatal(err)
	}
	var review bytes.Buffer
	printReview(&review, &Plan{
		SourceRepo: Repo{FullName: "example/project"}, DashboardRepo: Repo{FullName: "example/dashboard"},
		Project: project.Config{ID: "example", Name: "Example"}, Prompt: plan,
		Files: map[string]string{"prompts/system.md": "prompt"},
	})
	if !strings.Contains(review.String(), "Prompt timeout:       30m0s") {
		t.Fatalf("review omitted prompt timeout:\n%s", review.String())
	}
	plan.Timeout = "30 seconds"
	if err := validatePromptPlan(plan); err == nil {
		t.Fatal("invalid prompt timeout was accepted")
	}
}
