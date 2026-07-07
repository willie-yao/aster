package fetcher

import (
	"context"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func TestAIEndpoint_PrefersYAMLOverEnv(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "https://env.example/v1/chat")
	cfg := &project.Config{AI: &project.AI{Endpoint: "https://yaml.example/v1/chat"}}
	if got := aiEndpoint(cfg); got != "https://yaml.example/v1/chat" {
		t.Errorf("aiEndpoint: got %q, want yaml value", got)
	}
}

func TestAIEndpoint_FallsBackToEnvWhenYAMLBlank(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "https://env.example/v1/chat")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiEndpoint(cfg); got != "https://env.example/v1/chat" {
		t.Errorf("aiEndpoint: got %q, want env value", got)
	}
}

func TestAIEndpoint_EmptyWhenNothingSet(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiEndpoint(cfg); got != "" {
		t.Errorf("aiEndpoint: got %q, want empty", got)
	}
}

func TestAIEndpoint_NilAIBlock(t *testing.T) {
	t.Setenv("AI_ENDPOINT", "https://env.example/v1/chat")
	cfg := &project.Config{}
	if got := aiEndpoint(cfg); got != "https://env.example/v1/chat" {
		t.Errorf("aiEndpoint: got %q, want env value when AI block is nil", got)
	}
}

func TestAIModel_PrefersYAMLOverEnv(t *testing.T) {
	t.Setenv("AI_MODEL", "env-model")
	cfg := &project.Config{AI: &project.AI{Model: "yaml-model"}}
	if got := aiModel(cfg); got != "yaml-model" {
		t.Errorf("aiModel: got %q, want yaml value", got)
	}
}

func TestAIModel_FallsBackToEnvWhenYAMLBlank(t *testing.T) {
	t.Setenv("AI_MODEL", "env-model")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiModel(cfg); got != "env-model" {
		t.Errorf("aiModel: got %q, want env value", got)
	}
}

func TestAIModel_EmptyWhenNothingSet(t *testing.T) {
	t.Setenv("AI_MODEL", "")
	cfg := &project.Config{AI: &project.AI{}}
	if got := aiModel(cfg); got != "" {
		t.Errorf("aiModel: got %q, want empty", got)
	}
}

func TestAIModel_NilAIBlock(t *testing.T) {
	t.Setenv("AI_MODEL", "env-model")
	cfg := &project.Config{}
	if got := aiModel(cfg); got != "env-model" {
		t.Errorf("aiModel: got %q, want env value when AI block is nil", got)
	}
}

func TestCollectRecurringPatterns_FiltersAndRanks(t *testing.T) {
	jd := func(subject string, pa *models.PatternAnalysis) models.JobDetail {
		d := models.JobDetail{JobID: subject, Name: subject}
		if pa != nil {
			pa.Subject = subject
			d.PatternAnalyses = []models.PatternAnalysis{*pa}
		}
		return d
	}
	details := []models.JobDetail{
		jd("low-systemic", &models.PatternAnalysis{Systemic: true, Confidence: "low", BuildsAnalyzed: 9}),
		jd("not-systemic", &models.PatternAnalysis{Systemic: false, Confidence: "high", BuildsAnalyzed: 8}),
		jd("high-3builds", &models.PatternAnalysis{Systemic: true, Confidence: "high", BuildsAnalyzed: 3}),
		jd("high-6builds", &models.PatternAnalysis{Systemic: true, Confidence: "high", BuildsAnalyzed: 6}),
		jd("no-pattern", nil),
	}

	got := collectRecurringPatterns(details)

	// Only systemic verdicts are kept.
	if len(got) != 3 {
		t.Fatalf("got %d patterns, want 3 (systemic only)", len(got))
	}
	// Ranked by confidence desc, then builds desc: high/6, high/3, low/9.
	wantOrder := []string{"high-6builds", "high-3builds", "low-systemic"}
	for i, want := range wantOrder {
		if got[i].Subject != want {
			t.Errorf("rank %d: got %q, want %q", i, got[i].Subject, want)
		}
	}
}

func TestRunWatch_RejectsNonPositiveIntervals(t *testing.T) {
	ctx := context.Background()
	opts := Options{Collectors: NewCollectorRegistry()}
	if err := RunWatch(ctx, opts, 0, time.Hour); err == nil {
		t.Error("expected error for zero watch interval")
	}
	if err := RunWatch(ctx, opts, time.Minute, 0); err == nil {
		t.Error("expected error for zero reconcile interval")
	}
}
