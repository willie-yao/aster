// Package devmock provides in-memory stand-ins for the server's authenticated
// services. They implement the same interfaces the real services do, so the
// server's routes, auth middleware, CSRF guard, capability descriptor, and wire
// formats stay exactly as deployed while the work behind them is fabricated.
//
// This exists so the operator surface (issue and fix drafting, resolution,
// analysis chat, and pull request escalation) can be developed locally without
// AI credentials, GitHub write access, or a Kubernetes cluster. It is reachable
// only through `server -mock` and must never back a real deployment.
package devmock

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

// DefaultLatency is how long a fabricated model call takes. It is deliberately
// slow enough that pending, streaming, and polling states are visible in the UI
// rather than resolving before the first render.
const DefaultLatency = 4 * time.Second

// Options configures the mock services.
type Options struct {
	// DataDir is the fetcher output directory. Resolution writes to it, and
	// chat reads published analyses out of it.
	DataDir string
	// Latency is how long a fabricated model call takes. Zero uses DefaultLatency.
	Latency time.Duration
	// Now supplies the clock. Nil uses time.Now.
	Now func() time.Time
}

func (o Options) latency() time.Duration {
	if o.Latency <= 0 {
		return DefaultLatency
	}
	return o.Latency
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now().UTC()
	}
	return o.Now().UTC()
}

// Services are the mock implementations wired into the server.
type Services struct {
	Actions                 *Actions
	AnalysisChat            *Chat
	ChatFix                 *ChatFix
	PullRequestEscalation   *PullRequestEscalation
	SharedFailureEscalation *SharedFailureEscalation
}

// New builds the mock services. cfg supplies the deterministic resolution
// behavior the real action service reads it for; it is never used to reach a
// model provider or GitHub.
func New(cfg *project.Config, opts Options) (*Services, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, fmt.Errorf("devmock: DataDir is required")
	}
	if cfg == nil {
		return nil, fmt.Errorf("devmock: project config is required")
	}
	mockActions := newActions(cfg, opts)
	chat := newChat(opts)
	return &Services{
		Actions:                 mockActions,
		AnalysisChat:            chat,
		ChatFix:                 newChatFix(mockActions, chat),
		PullRequestEscalation:   newPullRequestEscalation(opts),
		SharedFailureEscalation: newSharedFailureEscalation(opts),
	}, nil
}

// publishedAnalysis is the snapshot of one published analysis the mock reads
// out of the data directory so its answers name real files and builds.
type publishedAnalysis struct {
	FileLinks  map[string]string
	RootCause  string
	Repository *sourceinvestigation.Repository
}

// lookupAnalysis reads the published analysis for one test in one build. A
// missing file, job, build, or test yields a zero value: the mock still answers,
// just without data-derived detail.
func lookupAnalysis(dataDir, jobID, buildID, testName string) publishedAnalysis {
	if jobID == "" {
		return publishedAnalysis{}
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "jobs", models.JobDataFilename(jobID)))
	if err != nil {
		return publishedAnalysis{}
	}
	var detail models.JobDetail
	if json.Unmarshal(data, &detail) != nil {
		return publishedAnalysis{}
	}
	for _, run := range detail.Runs {
		if buildID != "" && run.BuildID != buildID {
			continue
		}
		for _, testCase := range run.TestCases {
			if testName != "" && testCase.Name != testName {
				continue
			}
			if testCase.AIAnalysis == nil {
				continue
			}
			return publishedAnalysis{
				FileLinks:  testCase.AIAnalysis.FileLinks,
				RootCause:  testCase.AIAnalysis.RootCause,
				Repository: repositoryFromFileLinks(testCase.AIAnalysis.FileLinks),
			}
		}
	}
	return publishedAnalysis{}
}

// repositoryFromFileLinks recovers the pinned source repository from a verified
// blob link. The links the fetcher publishes are already pinned to the revision
// the build used, which is the same triple a real chat session reports.
func repositoryFromFileLinks(links map[string]string) *sourceinvestigation.Repository {
	for _, raw := range sortedValues(links) {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed.Host != "github.com" {
			continue
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) < 5 || parts[2] != "blob" {
			continue
		}
		return &sourceinvestigation.Repository{Owner: parts[0], Name: parts[1], Revision: parts[3]}
	}
	return nil
}

// citedPaths returns the repository-local paths of the published file links in
// a stable order, so a mock draft does not change shape between calls.
func citedPaths(links map[string]string) []string {
	paths := make([]string, 0, len(links))
	for path := range links {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func sortedValues(links map[string]string) []string {
	values := make([]string, 0, len(links))
	for _, path := range citedPaths(links) {
		values = append(values, links[path])
	}
	return values
}

// timestamp formats a time the way the engine's API views do.
func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// mockURL is the result link a confirmed mock action reports. It resolves
// nowhere, so a mock result is never mistaken for a live write.
func mockURL(kind, id string) string {
	return fmt.Sprintf("https://example.invalid/mock/%s/%s", kind, id)
}

// previewFor builds the issue or fix draft for one failure.
func previewFor(kind, failureID, instruction string, analysis publishedAnalysis) actions.PreviewResult {
	subject := failureID
	if analysis.RootCause != "" {
		subject = analysis.RootCause
	}
	result := actions.PreviewResult{Kind: kind}
	if kind == "issue" {
		result.Title = "Recurring failure: " + truncate(subject, 80)
		result.Body = issueBody(failureID, instruction, analysis)
		return result
	}
	result.Title = "Fix recurring failure: " + truncate(subject, 80)
	result.Body = fixBody(failureID, instruction, analysis)
	result.Diff = fixDiff(analysis)
	result.VerifyStatus = "passed"
	result.VerifySummary = "go build ./... and go vet ./... passed against the pinned revision."
	return result
}

func issueBody(failureID, instruction string, analysis publishedAnalysis) string {
	var b strings.Builder
	b.WriteString("## Summary\n\nThis draft came from the local mock server. ")
	b.WriteString("No model was called and nothing will be written to GitHub.\n\n")
	b.WriteString("## Root cause\n\n")
	b.WriteString(rootCauseOrPlaceholder(analysis))
	b.WriteString("\n\n## Evidence\n\n")
	writeEvidence(&b, analysis)
	b.WriteString("\n## Pattern\n\n`" + failureID + "`\n")
	writeInstruction(&b, instruction)
	return b.String()
}

func fixBody(failureID, instruction string, analysis publishedAnalysis) string {
	var b strings.Builder
	b.WriteString("## What this changes\n\nThis draft came from the local mock server. ")
	b.WriteString("The diff below is illustrative and no pull request will be opened.\n\n")
	b.WriteString("## Why\n\n")
	b.WriteString(rootCauseOrPlaceholder(analysis))
	b.WriteString("\n\n## Files\n\n")
	writeEvidence(&b, analysis)
	b.WriteString("\n## Pattern\n\n`" + failureID + "`\n")
	writeInstruction(&b, instruction)
	return b.String()
}

func rootCauseOrPlaceholder(analysis publishedAnalysis) string {
	if analysis.RootCause != "" {
		return analysis.RootCause
	}
	return "The published analysis for this failure carries no root cause."
}

func writeEvidence(b *strings.Builder, analysis publishedAnalysis) {
	paths := citedPaths(analysis.FileLinks)
	if len(paths) == 0 {
		b.WriteString("- no verified source files were cited\n")
		return
	}
	for _, path := range paths {
		fmt.Fprintf(b, "- [%s](%s)\n", path, analysis.FileLinks[path])
	}
}

func writeInstruction(b *strings.Builder, instruction string) {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return
	}
	b.WriteString("\n## Maintainer instruction\n\n> " + instruction + "\n")
}

// fixDiff renders a unified diff against a cited file so the diff viewer has
// realistic input.
func fixDiff(analysis publishedAnalysis) string {
	path := "test/e2e/mock_test.go"
	if paths := citedPaths(analysis.FileLinks); len(paths) > 0 {
		path = paths[0]
	}
	return fmt.Sprintf(`diff --git a/%[1]s b/%[1]s
--- a/%[1]s
+++ b/%[1]s
@@ -1,7 +1,8 @@
 // Mock diff produced by the local development server.
 // It is not a real remediation and will not be applied anywhere.
 
-	waitFor(ctx, condition)
+	// Wait for the condition to settle before asserting on it.
+	waitForWithTimeout(ctx, condition, defaultConditionTimeout)
 
 	requireNoError(err)
`, path)
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return strings.TrimSpace(value[:limit]) + "..."
}
