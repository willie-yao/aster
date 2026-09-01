// Package sharedfailure provides the Module used when a failure observed
// across several open pull requests is escalated as one subject. It reuses the
// universal seed prompt and appends the correlation the deterministic pass
// already established.
//
// No diff is supplied. A failure reported by several independent pull requests
// is by construction not explained by any one of their changes, and a model
// handed a diff will readily invent a connection to it. The prompt directs the
// investigation at the shared cause instead.
package sharedfailure

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/ai/modules/universal"
	"github.com/willie-yao/aster/backend/internal/models"
)

// ModuleName is the module identity. It is part of the agentic cache key, so a
// shared failure analysis never collides with the dashboard's universal
// analysis of the same build, or with a single pull request's escalation.
const ModuleName = "sharedfailure"

// maxListedPulls bounds how many affected pull requests the prompt names.
const maxListedPulls = 20

// Subject is the failure observed across several open pull requests.
type Subject struct {
	// BaseRef, JobName, and TestName are the correlation key.
	BaseRef  string
	JobName  string
	TestName string
	// PullNumbers are the affected open pull requests.
	PullNumbers []int
	// BuildLevel marks a job that failed without reporting a failing test, so
	// the prompt does not refer to a test that does not exist.
	BuildLevel bool
	// EvidencePull is the pull request whose build supplied the artifacts under
	// investigation. It is named so the model can tell which of the affected
	// pull requests it is actually reading.
	EvidencePull int
}

// Module implements ai.Module for one shared failure.
type Module struct {
	subject  Subject
	fallback *universal.Module
}

// New constructs the module for one shared failure subject.
func New(subject Subject) *Module {
	return &Module{subject: subject, fallback: universal.New()}
}

// Name returns the module identity used in cache keys and logs.
func (m *Module) Name() string { return ModuleName }

// AnalysisPrompt returns the universal seed prompt followed by the shared
// failure context. Investigation instructions are unchanged so the analysis is
// gated by the same deterministic critique rules as every other one.
func (m *Module) AnalysisPrompt(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string {
	var sb strings.Builder
	sb.WriteString(m.fallback.AnalysisPrompt(ctx, client, run, tc, consecutive))
	sb.WriteString(m.sharedContext())
	return sb.String()
}

// sharedContext renders the correlation block appended to the seed prompt.
func (m *Module) sharedContext() string {
	var sb strings.Builder
	sb.WriteString("\n---\n\nShared failure context\n\n")

	pulls := m.subject.orderedPulls()
	fmt.Fprintf(&sb, "%s is failing the same way on %d open pull requests",
		m.subject.subjectName(), len(pulls))
	if m.subject.BaseRef != "" {
		fmt.Fprintf(&sb, " targeting %s", m.subject.BaseRef)
	}
	sb.WriteString(".\n\n")

	if len(pulls) > 0 {
		sb.WriteString("Affected pull requests: ")
		sb.WriteString(pullList(pulls))
		sb.WriteString(".\n")
	}
	if m.subject.EvidencePull > 0 {
		fmt.Fprintf(&sb,
			"The artifacts under investigation come from the build on pull request #%d, which was chosen only because it is the most recent.\n",
			m.subject.EvidencePull)
	}

	sb.WriteString(`
The same failure on several open pull requests usually has a cause they share
rather than a cause in any one change. Diagnose that shared cause.
Infrastructure, environment, image and dependency versions, quota, and the base
branch itself are the candidates worth weighing first.

These pull requests were correlated only by base branch, job, and test, so they
are not established to be independent: one may be stacked on another, or two may
carry the same change. Treat the correlation as an observation, not as proof
that no change is responsible.

Do not attribute the failure to any pull request, including the one whose
artifacts you are reading. Investigate the build artifacts exactly as you would
for any other failure, and cite the artifact paths and log lines that establish
the root cause. If the artifacts only show what failed and not why it failed
everywhere, say what they show and stop.
`)
	return sb.String()
}

// subjectName names what failed, since a build-level failure has no useful
// test name.
func (s Subject) subjectName() string {
	if s.BuildLevel || strings.TrimSpace(s.TestName) == "" {
		return s.JobName
	}
	return "This test"
}

// orderedPulls returns the affected pull requests in ascending order.
func (s Subject) orderedPulls() []int {
	out := make([]int, 0, len(s.PullNumbers))
	for _, number := range s.PullNumbers {
		if number > 0 {
			out = append(out, number)
		}
	}
	sort.Ints(out)
	return out
}

// pullList renders the affected pull requests, truncating a long list so the
// prompt stays readable.
func pullList(numbers []int) string {
	listed := numbers
	if len(listed) > maxListedPulls {
		listed = listed[:maxListedPulls]
	}
	labels := make([]string, len(listed))
	for i, number := range listed {
		labels[i] = fmt.Sprintf("#%d", number)
	}
	out := strings.Join(labels, ", ")
	if len(numbers) > len(listed) {
		out += fmt.Sprintf(", and %d more", len(numbers)-len(listed))
	}
	return out
}
