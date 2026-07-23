// Package evidenceplan builds bounded initial evidence plans for failure analysis.
package evidenceplan

import (
	"fmt"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const (
	// CandidatePathLimit bounds ranked paths per evidence group.
	CandidatePathLimit = 4
	// FailureMessageBytes bounds the failure message used for recipe matching.
	FailureMessageBytes = 16 * 1024
	// FailureBodyBytes bounds the failure body used for recipe matching.
	FailureBodyBytes = 8 * 1024
	// MaxPromptBytes bounds the rendered evidence plan.
	MaxPromptBytes = 24 * 1024
)

// ScanStatus describes whether the candidate scan can prove plan completeness.
type ScanStatus struct {
	Truncated   bool
	Failed      bool
	Unavailable bool
}

// FailureSignal renders only bounded test-failure evidence for recipe matching.
func FailureSignal(tc models.TestCase) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Failed test: %s\n", tc.Name)
	if tc.FailureLocation != "" {
		fmt.Fprintf(&b, "Failure location: %s\n", tc.FailureLocation)
	}
	if tc.JUnitFile != "" {
		fmt.Fprintf(&b, "JUnit file: %s\n", tc.JUnitFile)
	}
	if message := strings.TrimSpace(tc.FailureMessage); message != "" {
		b.WriteString("Failure message:\n")
		b.WriteString(clampHeadTail(message, FailureMessageBytes))
		b.WriteByte('\n')
	}
	if body := boundedFailureBody(tc.FailureBody); body != "" {
		b.WriteString("Failure body (truncated to last 8KB):\n")
		b.WriteString(body)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// Render returns a bounded plan prompt plus whether every initial group has a
// candidate from a complete scan.
func Render(plan []skills.PlannedSkill, scan ScanStatus) (string, bool) {
	if len(plan) == 0 {
		return "", false
	}
	text := inProcessRenderText
	complete := !scan.Truncated && !scan.Failed && !scan.Unavailable
	var out strings.Builder
	out.WriteString("## Required evidence plan\n\n")
	out.WriteString(text.intro)
	out.WriteByte('\n')
	if scan.Truncated {
		out.WriteString("The candidate scan was truncated, so missing candidates may still exist deeper in the artifact tree.\n")
	}
	if scan.Failed {
		out.WriteString("The candidate scan failed, so this plan is incomplete and normal artifact tools may be needed to locate evidence.\n")
	}
	if scan.Unavailable {
		out.WriteString("The candidate scan was unavailable, so this plan is incomplete and normal artifact tools may be needed to locate evidence.\n")
	}
	for _, plannedSkill := range plan {
		var section strings.Builder
		name := strings.TrimSpace(plannedSkill.Name)
		if name == "" {
			name = plannedSkill.ID
		}
		fmt.Fprintf(&section, "\n### %s (`%s`)\n", name, plannedSkill.ID)
		if procedure := strings.TrimSpace(plannedSkill.Procedure); procedure != "" {
			section.WriteString("Procedure (diagnostic guidance only):\n")
			section.WriteString(procedure)
			section.WriteByte('\n')
		}
		if len(plannedSkill.RequiredEvidence) == 0 {
			section.WriteString("Required evidence: no conditional groups apply to the current failure signal.\n")
		} else {
			section.WriteString("Required evidence:\n")
		}
		for _, group := range plannedSkill.RequiredEvidence {
			description := strings.TrimSpace(group.Description)
			if description == "" {
				description = group.ID
			}
			fmt.Fprintf(&section, "- %s: %s\n", group.ID, description)
			if len(group.CandidatePaths) == 0 {
				complete = false
				section.WriteString(text.missingCandidate)
				continue
			}
			section.WriteString("  Candidate paths:\n")
			for _, candidate := range group.CandidatePaths {
				fmt.Fprintf(&section, "  - %s\n", candidate)
			}
		}
		if out.Len()+section.Len() > MaxPromptBytes {
			complete = false
			out.WriteString(text.omitted)
			break
		}
		out.WriteString(section.String())
	}
	return strings.TrimSpace(out.String()), complete
}

type renderText struct {
	intro            string
	missingCandidate string
	omitted          string
}

var inProcessRenderText = renderText{
	intro:            "The dashboard matched these diagnostic recipes from the failure signal. Before broad searches, read at least one candidate path from every listed evidence group with read_artifact, tail_artifact, or grep_artifact. If the diagnosis changes or a group has no candidate, continue investigating with the normal artifact tools.",
	missingCandidate: "  Candidate paths: none found in the bounded tree; use list_artifacts or find_artifacts on the relevant subtree.\n",
	omitted:          "\n... [additional matched evidence plans omitted by prompt budget; use the normal artifact tools to investigate unresolved groups from the original failure signal]\n",
}

func boundedFailureBody(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > FailureBodyBytes {
		value = strings.ToValidUTF8(value[len(value)-FailureBodyBytes:], "")
	}
	return value
}

func clampHeadTail(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	headBytes := maxBytes * 3 / 4
	tailBytes := maxBytes - headBytes
	head := strings.ToValidUTF8(value[:headBytes], "")
	tail := strings.ToValidUTF8(value[len(value)-tailBytes:], "")
	return head + fmt.Sprintf("\n... [%d bytes elided; read the JUnit artifact for the complete failure] ...\n", len(value)-len(head)-len(tail)) + tail
}
