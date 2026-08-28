package fetcher

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"sort"
	"time"

	"github.com/willie-yao/aster/backend/internal/aiusage"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/models"
)

const maxPreparedCauseFindingsPerRun = 3

type preparedCauseCandidate struct {
	key  string
	ref  analysischat.AnalysisRef
	turn analysischat.Turn
	// published marks a cause on a recurring pattern the dashboard publishes, so
	// the per-run budget is spent where a maintainer can open the finding.
	published bool
	retry     bool
}

type preparedCauseRunner interface {
	Reply(context.Context, analysischat.Turn) (analysischat.Reply, error)
}

var newPreparedCauseRunner = func(ctx context.Context, p *pipeline) (preparedCauseRunner, error) {
	runtime, err := p.ensureAnalysisRuntime(ctx)
	if err != nil {
		return nil, err
	}
	return runtime.NewAnalysisChatAgent(p.backend)
}

var preparedCauseRuntimeFingerprint = func(ctx context.Context, p *pipeline) (string, error) {
	runtime, err := p.ensureAnalysisRuntime(ctx)
	if err != nil {
		return "", err
	}
	return runtime.AnalysisChatContractFingerprint(), nil
}

func (p *pipeline) prepareCauseFindings(ctx context.Context, details []models.JobDetail) {
	if p == nil || !p.enableAI || !p.opts.PrepareCauseFindings || p.aiProject == nil || ctx.Err() != nil {
		return
	}
	fingerprint, err := preparedCauseRuntimeFingerprint(ctx, p)
	if err != nil {
		log.Printf("Warning: prepared cause findings runtime unavailable: %v", err)
		return
	}
	agent, err := newPreparedCauseRunner(ctx, p)
	if err != nil {
		log.Printf("Warning: prepared cause findings agent unavailable: %v", err)
		return
	}
	generation := analysischat.PreparedCauseGeneration(fingerprint)
	path := filepath.Join(p.opts.OutDir, analysischat.PreparedCauseFindingsFilename)
	state, err := analysischat.LoadPreparedCauseFindings(path, generation)
	if err != nil {
		log.Printf("Warning: prepared cause findings cache unreadable: %v", err)
		state = analysischat.PreparedCauseFindings{Generation: generation, Findings: map[string]analysischat.PreparedCauseFinding{}, Failures: map[string]analysischat.PreparedCauseFailure{}}
	}
	eligible := map[string]struct{}{}
	candidates := make([]preparedCauseCandidate, 0)
	for _, detail := range details {
		for _, pattern := range detail.PatternAnalyses {
			if !models.PatternIsActive(pattern) {
				continue
			}
			published := pattern.Systemic
			for _, group := range pattern.CausalGroups {
				ref := analysischat.AnalysisRef{
					Scope: analysischat.ScopeCause, JobID: detail.JobID, PatternID: pattern.ID, PatternHash: pattern.ContentHash,
					CausalGroupID: group.ID, CausalGroupHash: group.ContentHash,
				}
				turn, turnErr := analysischat.PreparedCauseTurn(ref, detail)
				if turnErr != nil {
					continue
				}
				comparisonBuildID := preparedCauseComparisonBuildID(turn)
				key, keyErr := analysischat.PreparedCauseKey(ref, comparisonBuildID)
				if keyErr != nil {
					continue
				}
				eligible[key] = struct{}{}
				if cached, ok := state.Findings[key]; ok && cached.Ref == ref && !cached.Reply.Unverified && len(cached.Reply.Citations) > 0 {
					continue
				}
				retry := false
				if failure, ok := state.Failures[key]; ok {
					if attempted, parseErr := time.Parse(time.RFC3339, failure.AttemptedAt); parseErr == nil && time.Since(attempted) < 6*time.Hour {
						continue
					}
					retry = true
				}
				candidates = append(candidates, preparedCauseCandidate{key: key, ref: ref, turn: turn, published: published, retry: retry})
			}
		}
	}
	for key := range state.Findings {
		if _, ok := eligible[key]; !ok {
			delete(state.Findings, key)
		}
	}
	for key := range state.Failures {
		if _, ok := eligible[key]; !ok {
			delete(state.Failures, key)
		}
	}
	state.Generation = generation
	if err := analysischat.SavePreparedCauseFindings(path, state); err != nil {
		log.Printf("Warning: prepared cause findings cache save failed: %v", err)
		return
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].published != candidates[j].published {
			return candidates[i].published
		}
		return !candidates[i].retry && candidates[j].retry
	})
	if len(candidates) > maxPreparedCauseFindingsPerRun {
		candidates = candidates[:maxPreparedCauseFindingsPerRun]
	}
	prepared := 0
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		runCtx, operation := aiusage.Begin(ctx, p.usageRecorder, aiusage.Metadata{
			LogicalID: candidate.key, Origin: aiusage.OriginFetcher, Feature: aiusage.FeatureAnalysisChat,
			Correlation: aiusage.Correlation{JobID: candidate.ref.JobID},
		})
		reply, runErr := agent.Reply(runCtx, candidate.turn)
		outcome := aiusage.OutcomeSuccess
		if runErr != nil {
			outcome = aiusage.OutcomeError
			if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
				outcome = aiusage.OutcomeCancelled
			}
		}
		operation.Finish(outcome)
		if runErr != nil {
			state.Failures[candidate.key] = analysischat.PreparedCauseFailure{AttemptedAt: time.Now().UTC().Format(time.RFC3339)}
			_ = analysischat.SavePreparedCauseFindings(path, state)
			log.Printf("Warning: prepared cause finding %s failed: %v", candidate.ref.CausalGroupID, runErr)
			continue
		}
		if reply.Unverified || len(reply.Citations) == 0 {
			state.Failures[candidate.key] = analysischat.PreparedCauseFailure{AttemptedAt: time.Now().UTC().Format(time.RFC3339)}
			_ = analysischat.SavePreparedCauseFindings(path, state)
			log.Printf("Warning: prepared cause finding %s had no verified evidence", candidate.ref.CausalGroupID)
			continue
		}
		delete(state.Failures, candidate.key)
		state.Findings[candidate.key] = analysischat.PreparedCauseFinding{
			Ref: candidate.ref, Reply: reply, PreparedAt: time.Now().UTC().Format(time.RFC3339),
		}
		if err := analysischat.SavePreparedCauseFindings(path, state); err != nil {
			log.Printf("Warning: prepared cause finding %s was not saved: %v", candidate.ref.CausalGroupID, err)
			continue
		}
		prepared++
	}
	if prepared > 0 {
		log.Printf("💬 prepared %d cause finding(s) for immediate review", prepared)
	}
}

func preparedCauseComparisonBuildID(turn analysischat.Turn) string {
	if turn.Comparison == nil {
		return ""
	}
	return turn.Comparison.ArtifactBuild.Build.BuildID
}
