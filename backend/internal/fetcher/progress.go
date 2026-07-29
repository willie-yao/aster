package fetcher

import (
	"context"
	"errors"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
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

func (p *pipeline) planProgressAnalyses(total int) {
	if p.progress != nil {
		p.progress.PlanAnalyses(total)
	}
}

func (p *pipeline) startProgressAnalysis() {
	if p.progress != nil {
		p.progress.StartAnalysis()
	}
}

func (p *pipeline) finishProgressAnalysis(outcome fetchprogress.Outcome) {
	if p.progress != nil {
		p.progress.FinishAnalysis(outcome)
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
