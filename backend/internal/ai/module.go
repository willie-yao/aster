package ai

import (
	"context"
	"net/http"

	"github.com/willie-yao/aster/backend/internal/models"
)

// Module supplies the per-failure prompt and a stable name for cache keys.
// System prompts are composed separately by ComposeSystemPrompt. The universal
// implementation performs no upfront evidence fetching because the agentic loop
// discovers evidence through tools.
type Module interface {
	// Name uniquely identifies the module. Used for logging and cache keys.
	Name() string

	// AnalysisPrompt returns the user message seeding a single agentic
	// analysis: the failing test's context. The agentic loop is expected to
	// fetch any further evidence via tools.
	AnalysisPrompt(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string
}
