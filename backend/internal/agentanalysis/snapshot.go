package agentanalysis

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/evidenceplan"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

// ErrEvidenceUnavailable marks a failure to freeze substantive artifact evidence.
var ErrEvidenceUnavailable = errors.New("agent analysis evidence unavailable")

const (
	freezeTreeMaxPaths   = 5000
	freezeMaxExcerpts    = 8
	freezeExcerptLines   = 200
	freezeExcerptBytes   = 64 << 10
	freezeCandidateLimit = 4
)

// FreezeEvidence creates one deterministic bounded artifact snapshot.
func FreezeEvidence(
	ctx context.Context,
	browser artifacts.Browser,
	request ai.FailureAnalysisRequest,
	source sourceinvestigation.Repository,
	set *skills.Set,
) (EvidenceBundle, error) {
	if browser == nil {
		return EvidenceBundle{}, fmt.Errorf("%w: artifact browser is not configured", ErrEvidenceUnavailable)
	}
	rawPaths, truncated, err := browser.ListTree(ctx, freezeTreeMaxPaths)
	if err != nil {
		return EvidenceBundle{}, fmt.Errorf("%w: list artifact tree", ErrEvidenceUnavailable)
	}
	paths := canonicalArtifactPaths(rawPaths)
	scan := ArtifactScan{
		PathCount: len(paths), Truncated: truncated,
		Digest: hashString(strings.Join(paths, "\n")),
	}
	signal := evidenceplan.FailureSignal(request.TestCase)
	var plan []skills.PlannedSkill
	if set != nil && strings.TrimSpace(signal) != "" {
		plan = set.Plan(signal, paths, freezeCandidateLimit)
	}
	candidates := selectEvidenceCandidates(request, plan, paths)
	excerpts := make([]EvidenceExcerpt, 0, min(len(candidates), freezeMaxExcerpts))
	for _, candidate := range candidates {
		if len(excerpts) >= freezeMaxExcerpts {
			break
		}
		result, readErr := browser.Tail(ctx, candidate, freezeExcerptLines, freezeExcerptBytes)
		if readErr != nil || result == nil {
			if ctx.Err() != nil {
				return EvidenceBundle{}, ctx.Err()
			}
			continue
		}
		content := strings.ToValidUTF8(string(result.Content), "")
		content = strings.ReplaceAll(content, "\r\n", "\n")
		if strings.TrimSpace(content) == "" || strings.IndexByte(content, 0) >= 0 {
			continue
		}
		excerpts = append(excerpts, EvidenceExcerpt{
			Path: candidate, Kind: "tail", Content: content,
			Truncated: result.FileSize > int64(len(result.Content)) || result.LinesReturned >= freezeExcerptLines,
		})
	}
	if len(excerpts) == 0 {
		return EvidenceBundle{}, fmt.Errorf("%w: no bounded artifact excerpt could be read", ErrEvidenceUnavailable)
	}
	skillHash := ""
	if set != nil {
		skillHash = set.Hash()
	}
	return NewEvidenceBundle(request, source, scan, plan, excerpts, skillHash)
}

func canonicalArtifactPaths(values []string) []string {
	seen := map[string]bool{}
	paths := make([]string, 0, min(len(values), freezeTreeMaxPaths))
	for _, value := range values {
		clean, err := artifacts.SafePath(strings.TrimSpace(value))
		if err != nil || clean == "" || seen[clean] {
			continue
		}
		seen[clean] = true
		paths = append(paths, clean)
		if len(paths) == freezeTreeMaxPaths {
			break
		}
	}
	sort.Strings(paths)
	return paths
}

func selectEvidenceCandidates(request ai.FailureAnalysisRequest, plan []skills.PlannedSkill, paths []string) []string {
	available := make(map[string]bool, len(paths))
	byBase := map[string][]string{}
	for _, artifactPath := range paths {
		available[artifactPath] = true
		base := strings.ToLower(path.Base(artifactPath))
		byBase[base] = append(byBase[base], artifactPath)
	}
	var candidates []string
	seen := map[string]bool{}
	add := func(candidate string) {
		candidate, err := artifacts.SafePath(strings.TrimSpace(candidate))
		if err != nil || candidate == "" || !available[candidate] || seen[candidate] || len(candidates) >= freezeMaxExcerpts {
			return
		}
		seen[candidate] = true
		candidates = append(candidates, candidate)
	}
	add(request.TestCase.JUnitFile)
	if junitBase := strings.ToLower(path.Base(strings.TrimSpace(request.TestCase.JUnitFile))); junitBase != "." && junitBase != "" {
		for _, candidate := range byBase[junitBase] {
			add(candidate)
		}
	}
	for _, planned := range plan {
		for _, group := range planned.RequiredEvidence {
			for _, candidate := range group.CandidatePaths {
				add(candidate)
			}
		}
	}
	for _, base := range []string{"build-log.txt", "prowjob.json", "finished.json", "started.json"} {
		for _, candidate := range byBase[base] {
			add(candidate)
		}
	}
	if len(candidates) < freezeMaxExcerpts {
		type rankedPath struct {
			path  string
			score int
		}
		tokens := failureTokens(evidenceplan.FailureSignal(request.TestCase))
		ranked := make([]rankedPath, 0, len(paths))
		for _, artifactPath := range paths {
			ext := strings.ToLower(path.Ext(artifactPath))
			score := 0
			switch ext {
			case ".log":
				score += 40
			case ".xml":
				score += 30
			case ".txt":
				score += 20
			case ".json", ".yaml", ".yml":
				score += 10
			default:
				continue
			}
			lower := strings.ToLower(artifactPath)
			for _, token := range tokens {
				if strings.Contains(lower, token) {
					score += len(token)
				}
			}
			ranked = append(ranked, rankedPath{path: artifactPath, score: score})
		}
		sort.Slice(ranked, func(i, j int) bool {
			if ranked[i].score != ranked[j].score {
				return ranked[i].score > ranked[j].score
			}
			return ranked[i].path < ranked[j].path
		})
		for _, candidate := range ranked {
			add(candidate.path)
		}
	}
	return candidates
}

func failureTokens(value string) []string {
	seen := map[string]bool{}
	var out []string
	for _, token := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(token) < 4 || seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}
