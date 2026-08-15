package fetcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/orka"
)

type watchPipeline interface {
	fullPass(context.Context) ([]models.ProwJob, error)
	watchPass(context.Context, []models.ProwJob) error
}

type watchProgressPipeline interface {
	beginWatchPass(fetchprogress.PassType)
	finishWatchPass(error)
	setWatchSchedule(time.Time, time.Time)
}

type watchClock struct {
	now  func() time.Time
	wait func(context.Context, time.Time) error
}

var realWatchClock = watchClock{
	now: time.Now,
	wait: func(ctx context.Context, deadline time.Time) error {
		delay := time.Until(deadline)
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	},
}

func (p *pipeline) watchPass(ctx context.Context, jobs []models.ProwJob) error {
	fetchCtx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()
	analysisCtx, _ := passExecutionContexts(ctx, fetchCtx, p.opts.AnalysisRuntime.Type)
	result, err := p.refreshWithAnalysisContext(fetchCtx, analysisCtx, jobs)
	if err == nil {
		p.skipProgressSideEffects()
		p.runShadowAnalysis(ctx, result)
		p.runCausalCritic(fetchCtx, result)
	}
	return err
}

// RunWatch runs one pass at a time and keeps lightweight refreshes free of side effects.
func RunWatch(ctx context.Context, opts Options, watchInterval, reconcileInterval time.Duration) error {
	if watchInterval <= 0 || reconcileInterval <= 0 {
		return fmt.Errorf("watch and reconcile intervals must be positive (got watch=%s reconcile=%s)", watchInterval, reconcileInterval)
	}
	progress, stopProgress := startFetchProgress(ctx, opts, fetchprogress.PassInitialWatch)
	defer stopProgress()
	p, err := setupPipeline(opts)
	if err != nil {
		progress.FinishFailure(fetchprogress.FailureSetup)
		return haltSystemicWatchFailure(ctx, err)
	}
	p.progress = progress
	p.configureProgressAnalysisMetadata()
	progress.CompletePhase()
	if err := ctx.Err(); err != nil {
		progress.FinishCancelled()
		return err
	}
	if err := runWatchLoop(ctx, p, watchInterval, reconcileInterval, realWatchClock); err != nil {
		outcome := progress.Snapshot().Outcome
		if errors.Is(err, context.Canceled) && (outcome == fetchprogress.OutcomeRunning || outcome == fetchprogress.OutcomeSucceeded) {
			progress.FinishCancelled()
		} else {
			progress.CancelIfRunning()
		}
		return haltSystemicWatchFailure(ctx, err)
	}
	return nil
}

func runWatchLoop(ctx context.Context, p watchPipeline, watchInterval, reconcileInterval time.Duration, clock watchClock) error {
	jobs, err := p.fullPass(ctx)
	finishWatchProgress(p, err)
	if err != nil {
		if isSystemicWatchFailure(err) {
			return err
		}
		log.Printf("⚠ initial pass failed: %v", err)
	}

	log.Printf("👀 watching: refresh every %s, reconcile every %s", watchInterval, reconcileInterval)
	completed := clock.now()
	nextWatch := completed.Add(watchInterval)
	nextReconcile := completed.Add(reconcileInterval)
	setWatchProgressSchedule(p, nextWatch, nextReconcile)
	for {
		next := nextWatch
		if nextReconcile.Before(next) {
			next = nextReconcile
		}
		if err := clock.wait(ctx, next); err != nil {
			return err
		}

		now := clock.now()
		if !now.Before(nextReconcile) {
			beginWatchProgress(p, fetchprogress.PassReconcile)
			newJobs, err := p.fullPass(ctx)
			finishWatchProgress(p, err)
			if err != nil {
				if isSystemicWatchFailure(err) {
					return err
				}
				log.Printf("⚠ reconcile failed: %v", err)
			} else {
				jobs = newJobs
			}
			completed = clock.now()
			nextReconcile = completed.Add(reconcileInterval)
			nextWatch = completed.Add(watchInterval)
			setWatchProgressSchedule(p, nextWatch, nextReconcile)
			continue
		}
		if len(jobs) == 0 {
			beginWatchProgress(p, fetchprogress.PassReconcile)
			newJobs, err := p.fullPass(ctx)
			finishWatchProgress(p, err)
			if err != nil {
				if isSystemicWatchFailure(err) {
					return err
				}
				log.Printf("⚠ discovery retry failed: %v", err)
			} else {
				jobs = newJobs
				nextReconcile = clock.now().Add(reconcileInterval)
			}
			nextWatch = clock.now().Add(watchInterval)
			setWatchProgressSchedule(p, nextWatch, nextReconcile)
			continue
		}
		beginWatchProgress(p, fetchprogress.PassLightweightWatch)
		err := p.watchPass(ctx, jobs)
		finishWatchProgress(p, err)
		if err != nil {
			if isSystemicWatchFailure(err) {
				return err
			}
			log.Printf("⚠ refresh failed: %v", err)
		}
		nextWatch = clock.now().Add(watchInterval)
		setWatchProgressSchedule(p, nextWatch, nextReconcile)
	}
}

func beginWatchProgress(p watchPipeline, passType fetchprogress.PassType) {
	if progress, ok := p.(watchProgressPipeline); ok {
		progress.beginWatchPass(passType)
	}
}

func finishWatchProgress(p watchPipeline, err error) {
	if progress, ok := p.(watchProgressPipeline); ok {
		progress.finishWatchPass(err)
	}
}

func setWatchProgressSchedule(p watchPipeline, nextWatch, nextReconcile time.Time) {
	if progress, ok := p.(watchProgressPipeline); ok {
		progress.setWatchSchedule(nextWatch, nextReconcile)
	}
}

func isSystemicWatchFailure(err error) bool {
	return analysisruntime.IsProjectBundleSourceError(err) || orka.IsResultAuthorizationError(err)
}

func haltSystemicWatchFailure(ctx context.Context, err error) error {
	if err == nil || !isSystemicWatchFailure(err) {
		return err
	}
	log.Printf("⛔ worker halted after systemic Orka failure; no further passes will be scheduled: %v", err)
	<-ctx.Done()
	return errors.Join(err, ctx.Err())
}
