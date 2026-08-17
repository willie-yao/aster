// Package pullrequest provides the Module used when a failure is escalated
// from a pull request. It reuses the universal seed prompt and appends the
// pull request's changed files as locating context.
//
// The model is never asked whether the pull request caused the failure. That
// judgment is computed deterministically from where the failure occurs and
// what the pull request changed, because a model handed a diff will readily
// invent a connection to it. The prompt says so explicitly.
package pullrequest

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
// pull request analysis never collides with the dashboard's universal analysis
// of the same failure.
const ModuleName = "pullrequest"

const (
	// maxListedFiles bounds how many changed paths the prompt names.
	maxListedFiles = 60
	// maxPatchBytes bounds the total patch text the prompt carries.
	maxPatchBytes = 24 * 1024
)

// ChangedFile is one file a pull request modifies. Patch is optional: the
// caller drops it for generated paths and when a budget is exhausted.
type ChangedFile struct {
	Path      string
	Status    string
	Generated bool
	Patch     string
}

// Subject is the pull request a failure was observed on.
type Subject struct {
	Number  int
	HeadSHA string
	BaseRef string
	// Files is the pull request's changed-file set.
	Files []ChangedFile
	// FilesTruncated reports that Files is incomplete, so the prompt must not
	// invite the model to conclude anything from a file's absence.
	FilesTruncated bool
}

// Module implements ai.Module for one pull request.
type Module struct {
	subject  Subject
	fallback *universal.Module
}

// New constructs the module for one pull request subject.
func New(subject Subject) *Module {
	return &Module{subject: subject, fallback: universal.New()}
}

// Name returns the module identity used in cache keys and logs.
func (m *Module) Name() string { return ModuleName }

// AnalysisPrompt returns the universal seed prompt followed by the pull
// request's changed files. Investigation instructions are unchanged so the
// analysis is gated by the same critique and judge rules as every other one.
func (m *Module) AnalysisPrompt(ctx context.Context, client *http.Client, run *models.BuildResult, tc *models.TestCase, consecutive int) string {
	var sb strings.Builder
	sb.WriteString(m.fallback.AnalysisPrompt(ctx, client, run, tc, consecutive))
	sb.WriteString(m.changeContext())
	return sb.String()
}

// changeContext renders the pull request block appended to the seed prompt.
func (m *Module) changeContext() string {
	var sb strings.Builder
	sb.WriteString("\n---\n\nPull request context\n\n")
	fmt.Fprintf(&sb, "This failure was observed on a presubmit build for pull request #%d", m.subject.Number)
	if m.subject.BaseRef != "" {
		fmt.Fprintf(&sb, " targeting %s", m.subject.BaseRef)
	}
	if m.subject.HeadSHA != "" {
		fmt.Fprintf(&sb, " at head %s", m.subject.HeadSHA)
	}
	sb.WriteString(".\n\n")

	files := m.orderedFiles()
	if len(files) == 0 {
		sb.WriteString("The changed-file list is unavailable, so treat this as an ordinary failure investigation.\n")
		return sb.String()
	}

	sb.WriteString("Files this pull request changes:\n")
	listed := files
	if len(listed) > maxListedFiles {
		listed = listed[:maxListedFiles]
	}
	for _, file := range listed {
		fmt.Fprintf(&sb, "- %s", file.Path)
		if file.Status != "" && file.Status != "modified" {
			fmt.Fprintf(&sb, " (%s)", file.Status)
		}
		if file.Generated {
			sb.WriteString(" (generated)")
		}
		sb.WriteString("\n")
	}
	if len(files) > len(listed) {
		fmt.Fprintf(&sb, "- ... and %d more files not listed\n", len(files)-len(listed))
	}
	if m.subject.FilesTruncated {
		sb.WriteString("\nThis list is incomplete. Do not conclude that a file is unchanged because it is absent.\n")
	}

	sb.WriteString(m.patchSection(listed))

	sb.WriteString(`
Use this list only to locate code that the failing test exercises. It is
context for your investigation, not evidence.

Do not claim the pull request caused the failure. Whether the change is
related is decided separately from what you report. Investigate the build
artifacts exactly as you would for any other failure, and cite the artifact
paths and log lines that establish the root cause. If the artifacts show a
direct causal chain that happens to reach changed code, cite that chain like
any other evidence. If they do not, say what the artifacts show and stop.
`)
	return sb.String()
}

// patchSection renders bounded patch text for files that carry one.
func (m *Module) patchSection(files []ChangedFile) string {
	var sb strings.Builder
	budget := maxPatchBytes
	rendered := 0
	for _, file := range files {
		patch := strings.TrimSpace(file.Patch)
		if patch == "" || len(patch) > budget {
			continue
		}
		if rendered == 0 {
			sb.WriteString("\nChange hunks:\n")
		}
		fmt.Fprintf(&sb, "\n--- %s\n%s\n", file.Path, patch)
		budget -= len(patch)
		rendered++
	}
	return sb.String()
}

// orderedFiles returns non-generated files first, then generated ones, each by
// path, so the listing leads with hand-written change.
func (m *Module) orderedFiles() []ChangedFile {
	out := make([]ChangedFile, 0, len(m.subject.Files))
	for _, file := range m.subject.Files {
		if strings.TrimSpace(file.Path) != "" {
			out = append(out, file)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Generated != out[b].Generated {
			return !out[a].Generated
		}
		return out[a].Path < out[b].Path
	})
	return out
}
