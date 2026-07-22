package orka

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

const (
	artifactTreeMaxPaths = 500
	artifactTreeMaxBytes = 48 * 1024
	evidencePlanMaxBytes = 24 * 1024
)

var artifactTreeNoiseExt = map[string]bool{
	".png": true, ".svg": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".gz": true, ".tar": true, ".tgz": true, ".zip": true, ".bz2": true,
}

// ArtifactTreeSeed lists bounded model-readable paths for the initial prompt.
func ArtifactTreeSeed(ctx context.Context, browser artifacts.Browser) (string, error) {
	if browser == nil {
		return "", nil
	}
	raw, rawTruncated, err := browser.ListTree(ctx, artifactTreeMaxPaths*2)
	if err != nil {
		return "", err
	}
	return ArtifactTreeSeedFromPaths(raw, rawTruncated), nil
}

// ArtifactTreeSeedFromPaths renders a bounded prompt seed from a prior tree listing.
func ArtifactTreeSeedFromPaths(raw []string, rawTruncated bool) string {
	paths := make([]string, 0, len(raw))
	for _, artifactPath := range raw {
		if artifactTreeNoiseExt[strings.ToLower(path.Ext(artifactPath))] {
			continue
		}
		paths = append(paths, artifactPath)
	}
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)
	truncated := rawTruncated
	if len(paths) > artifactTreeMaxPaths {
		paths = paths[:artifactTreeMaxPaths]
		truncated = true
	}

	var lines strings.Builder
	kept := 0
	for _, artifactPath := range paths {
		if lines.Len()+len(artifactPath)+1 > artifactTreeMaxBytes {
			truncated = true
			break
		}
		lines.WriteString(artifactPath)
		lines.WriteByte('\n')
		kept++
	}
	if kept == 0 {
		return ""
	}

	var seed strings.Builder
	fmt.Fprintf(&seed, "Artifact paths for this build (%d file(s)). Use these exact paths with the artifact tools. Do not guess paths or rediscover entries already listed here:\n", kept)
	seed.WriteString(lines.String())
	if truncated {
		seed.WriteString("... [list truncated; use list_artifacts only for subtrees not shown above]\n")
	}
	return seed.String()
}

// EvidencePlanPrompt renders matched recipes and exact artifact candidates.
func EvidencePlanPrompt(plan []skills.PlannedSkill, treeTruncated bool) string {
	prompt, _ := RenderEvidencePlan(plan, treeTruncated)
	return prompt
}

// RenderEvidencePlan returns the prompt plus whether it fully covers the initial matches.
func RenderEvidencePlan(plan []skills.PlannedSkill, treeTruncated bool) (string, bool) {
	if len(plan) == 0 {
		return "", false
	}
	complete := !treeTruncated
	var out strings.Builder
	out.WriteString("## Required evidence plan\n\n")
	out.WriteString("The dashboard matched these diagnostic recipes from the failure signal. Before broad searches or submit_analysis, read at least one candidate path from every listed evidence group. Keep every returned evidence_token. Use required_evidence if the diagnosis changes or a group has no candidate.\n")
	if treeTruncated {
		out.WriteString("The candidate scan was truncated, so missing candidates may still exist deeper in the artifact tree.\n")
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
		}
		if len(plannedSkill.RequiredEvidence) > 0 {
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
				section.WriteString("  Candidate paths: none found in the bounded tree; use required_evidence and list the relevant subtree.\n")
				continue
			}
			section.WriteString("  Candidate paths:\n")
			for _, candidate := range group.CandidatePaths {
				fmt.Fprintf(&section, "  - %s\n", candidate)
			}
		}
		if out.Len()+section.Len() > evidencePlanMaxBytes {
			complete = false
			out.WriteString("\n... [additional matched evidence plans omitted by prompt budget; before submit_analysis, call required_evidence with the original failure signal from this Task prompt]\n")
			break
		}
		out.WriteString(section.String())
	}
	return strings.TrimSpace(out.String()), complete
}

// WithEvidencePlan prepends the deterministic evidence checklist to a Task prompt.
func WithEvidencePlan(prompt, plan string) string {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return prompt
	}
	return plan + "\n\n---\n\n" + prompt
}

// WithArtifactTreeSeed prepends deterministic build context to a failure prompt.
func WithArtifactTreeSeed(prompt, seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return prompt
	}
	return seed + "\n\n---\n\n" + prompt
}
