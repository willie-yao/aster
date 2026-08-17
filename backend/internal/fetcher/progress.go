package fetcher

import (
	"context"
	"errors"
	"time"

	"github.com/willie-yao/aster/backend/internal/fetchprogress"
)

func startFetchProgress(ctx context.Context, opts Options, passType fetchprogress.PassType) (*fetchprogress.Tracker, context.CancelFunc) {
	tracker := fetchprogress.New(opts.OutDir, opts.Version)
	tracker.StartPass(passType)
	heartbeatCtx, cancel := context.WithCancel(ctx)
	go tracker.RunHeartbeats(heartbeatCtx)
	return tracker, cancel
}

func finishProgressPass(tracker *fetchprogress.Tracker, err error, watch bool) {
	if tracker == nil {
		return
	}
	if err == nil {
		tracker.FinishSuccess(watch)
		return
	}
	if errors.Is(err, context.Canceled) {
		tracker.FinishCancelled()
		return
	}
	tracker.FinishFailure(failureCategoryForPhase(tracker.Snapshot().Phase))
}

func failureCategoryForPhase(phase fetchprogress.Phase) fetchprogress.FailureCategory {
	switch phase {
	case fetchprogress.PhaseSetup:
		return fetchprogress.FailureSetup
	case fetchprogress.PhaseDiscovery:
		return fetchprogress.FailureDiscovery
	case fetchprogress.PhaseArtifacts:
		return fetchprogress.FailureArtifacts
	case fetchprogress.PhaseAggregation:
		return fetchprogress.FailureAggregation
	case fetchprogress.PhaseAnalysisPlanning, fetchprogress.PhaseAnalysis:
		return fetchprogress.FailureAnalysis
	case fetchprogress.PhasePatterns:
		return fetchprogress.FailurePatterns
	case fetchprogress.PhasePublication:
		return fetchprogress.FailurePublication
	case fetchprogress.PhaseSideEffects:
		return fetchprogress.FailureSideEffects
	default:
		return fetchprogress.FailureUnknown
	}
}

func (p *pipeline) startProgressPhase(phase fetchprogress.Phase) {
	if p.progress != nil {
		p.progress.StartPhase(phase)
	}
}

func (p *pipeline) completeProgressPhase() {
	if p.progress != nil {
		p.progress.CompletePhase()
	}
}

func (p *pipeline) setProgressJobs(total int) {
	if p.progress != nil {
		p.progress.SetJobs(total)
	}
}

func (p *pipeline) finishProgressJob(cached, fetched int) {
	if p.progress != nil {
		p.progress.FinishJob(cached, fetched)
	}
}

func (p *pipeline) markProgressChecked() {
	if p.progress != nil {
		p.progress.MarkChecked()
	}
}

func (p *pipeline) markProgressPublished() {
	if p.progress != nil {
		p.progress.MarkPublished()
	}
}

func (p *pipeline) skipProgressPatterns() {
	if p.progress != nil {
		p.progress.SkipPatterns()
	}
}

func (p *pipeline) skipProgressSideEffects() {
	if p.progress != nil {
		p.progress.SkipSideEffects()
	}
}

func (p *pipeline) setProgressFollowUp(component fetchprogress.FollowUpComponent, state fetchprogress.FollowUpState, reason fetchprogress.FollowUpReason, code fetchprogress.FollowUpFailureCode) {
	if p.progress != nil {
		p.progress.SetFollowUp(component, state, reason, code)
	}
}

func (p *pipeline) beginWatchPass(passType fetchprogress.PassType) {
	if p.progress != nil {
		p.progress.StartPass(passType)
	}
}

func (p *pipeline) finishWatchPass(err error) {
	finishProgressPass(p.progress, err, true)
}

func (p *pipeline) setWatchSchedule(nextWatch, nextReconcile time.Time) {
	if p.progress != nil {
		p.progress.SetSchedule(nextWatch, nextReconcile)
	}
}

func (p *pipeline) configureProgressAnalysisMetadata() {
	if p == nil || p.progress == nil || p.aiProject == nil || p.aiProject.SkillSet == nil {
		return
	}
	profiles := make([]string, 0, len(p.aiProject.ProfileSelection.Profiles()))
	for _, profile := range p.aiProject.ProfileSelection.Profiles() {
		profiles = append(profiles, string(profile))
	}
	source := p.aiProject.AnalysisSource
	mode := "anonymous"
	if githubReadToken() != "" {
		mode = "authenticated"
	}
	configured := source.Owner != "" && source.Name != ""
	if !configured {
		mode = ""
	}
	refStrategy := ""
	if configured {
		refStrategy = "default-branch"
	}
	p.progress.SetAnalysisMetadata(fetchprogress.SourceGrounding{
		Configured: configured, Mode: mode, Owner: source.Owner, Repository: source.Name,
		RefStrategy: refStrategy,
	}, fetchprogress.SkillBundle{
		Profiles: profiles, EngineCount: p.aiProject.SkillSet.EngineCount(),
		ConsumerCount:         p.aiProject.SkillSet.ConsumerCount(),
		ConsumerBundlePresent: p.aiProject.SkillSet.ConsumerBundlePresent(),
		IDs:                   p.aiProject.SkillSet.IDs(), Hash: p.aiProject.SkillSet.Hash(),
	})
}

func (p *pipeline) planProgressAnalyses(total, buildSubjects int) {
	if p.progress != nil {
		p.progress.PlanAnalyses(total, buildSubjects)
	}
}

func (p *pipeline) startProgressAnalysis(buildSubject bool) {
	if p.progress != nil {
		p.progress.StartAnalysis(buildSubject)
	}
}

func (p *pipeline) finishProgressAnalysis(buildSubject bool, outcome fetchprogress.Outcome) {
	if p.progress != nil {
		p.progress.FinishAnalysis(buildSubject, outcome)
	}
}

func (p *pipeline) cancelQueuedProgressAnalyses() {
	if p.progress != nil {
		p.progress.CancelQueuedAnalyses()
	}
}

func (p *pipeline) markProgressAnalysisCheckpoint() {
	if p.progress != nil {
		p.progress.MarkAnalysisCheckpoint()
	}
}

func (p *pipeline) skipProgressAnalysis() {
	if p.progress != nil {
		p.progress.SkipAnalysis()
	}
}
