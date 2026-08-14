// Package universal provides the project-agnostic Module implementation. It
// performs no upfront evidence fetching; the agentic loop discovers everything
// through registered tools. The prompt is intentionally minimal, just enough to
// point the agent at the right build and failing test.
package universal

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/willie-yao/aster/backend/internal/models"
)

// Module implements ai.Module: it builds the per-failure seed prompt for the
// agentic loop.
type Module struct{}

// New constructs the universal module.
func New() *Module { return &Module{} }

// Name returns "universal".
func (m *Module) Name() string { return "universal" }

// AnalysisPrompt builds a minimal per-failure prompt. No build log tail, error
// grep, or machine logs are preloaded; the agent is expected to fetch what it
// needs through the available tools.
//
// The failure message and body are size-capped to keep the prompt small; the
// agent can tail_artifact the junit file itself if it needs more.
func (m *Module) AnalysisPrompt(_ context.Context, _ *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string {
	// Caps stop giant ginkgo failure messages from overflowing the model window
	// on the first request.
	const failureBodyTail = 8 * 1024
	const failureMessageCap = 16 * 1024

	var sb strings.Builder
	if tc.Source == models.TestCaseSourceBuild {
		fmt.Fprintf(&sb, "Prow build failure to investigate. No failed JUnit test case was reported, so use build-log.txt as the primary failure evidence, identify the earliest causal error before later cleanup or teardown failures, and do not invent a test assertion.\n\n")
		fmt.Fprintf(&sb, "Failure subject: %s\n", tc.Name)
	} else {
		fmt.Fprintf(&sb, "Test failure to investigate.\n\n")
		fmt.Fprintf(&sb, "Test name: %s\n", tc.Name)
	}
	if tc.JUnitFile != "" {
		fmt.Fprintf(&sb, "JUnit file: %s\n", tc.JUnitFile)
	}
	if run != nil {
		if run.BuildID != "" {
			fmt.Fprintf(&sb, "Build: %s\n", run.BuildID)
		}
		if run.WebURL != "" {
			fmt.Fprintf(&sb, "Build URL: %s\n", run.WebURL)
		}
	}
	if consecutive > 1 {
		fmt.Fprintf(&sb, "Consecutive failures on this test: %d (persistent, not flaky).\n", consecutive)
	}

	if msg := strings.TrimSpace(tc.FailureMessage); msg != "" {
		sb.WriteString("\nFailure message:\n")
		sb.WriteString(clampHeadTail(msg, failureMessageCap))
		sb.WriteString("\n")
	}
	if body := strings.TrimSpace(tc.FailureBody); body != "" {
		sb.WriteString("\nFailure body (truncated to last 8KB):\n")
		if len(body) > failureBodyTail {
			body = body[len(body)-failureBodyTail:]
		}
		sb.WriteString(body)
		sb.WriteString("\n")
	}

	sb.WriteString(`
Use the available tools to investigate. Suggested starting points:
- list_artifacts("") to see the build's top-level layout
- tail_artifact("build-log.txt", 200) for the failing run's stdout
- grep_artifact for specific error strings you find

Tier-2 tools (when available) speed up Kubernetes-shaped digs:
- discover_clusters / list_cluster_machines / get_machine_log
- discover_controllers / get_controller_log

Cite the actual file paths and log lines that reveal the root cause. Do not
speculate. If the evidence is incomplete after exhausting reasonable tool
calls, say what you determined and what remains unknown.
`)

	return sb.String()
}

// clampHeadTail keeps the head and tail of s within max bytes with an elision
// marker between, so both the opening assertion and closing summary survive.
// Cuts are trimmed to valid UTF-8. Returns s unchanged if max<=0 or it fits.
func clampHeadTail(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	head := max * 3 / 4
	tail := max - head
	h := strings.ToValidUTF8(s[:head], "")
	t := strings.ToValidUTF8(s[len(s)-tail:], "")
	return h + fmt.Sprintf("\n... [%d bytes elided to fit the context window; read the junit/build-log artifact for the full message] ...\n", len(s)-len(h)-len(t)) + t
}
