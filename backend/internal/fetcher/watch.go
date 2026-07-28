package fetcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
)

type watchPipeline interface {
	fullPass(context.Context) ([]models.ProwJob, error)
	watchPass(context.Context, []models.ProwJob) error
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
	_, err := p.refreshWithAnalysisContext(fetchCtx, analysisCtx, jobs)
	return err
}

// RunWatch runs one pass at a time and keeps lightweight refreshes free of side effects.
func RunWatch(ctx context.Context, opts Options, watchInterval, reconcileInterval time.Duration) error {
	if watchInterval <= 0 || reconcileInterval <= 0 {
		return fmt.Errorf("watch and reconcile intervals must be positive (got watch=%s reconcile=%s)", watchInterval, reconcileInterval)
	}
	p, err := setupPipeline(opts)
	if err != nil {
		return haltSystemicWatchFailure(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := runWatchLoop(ctx, p, watchInterval, reconcileInterval, realWatchClock); err != nil {
		return haltSystemicWatchFailure(ctx, err)
	}
	return nil
}

func runWatchLoop(ctx context.Context, p watchPipeline, watchInterval, reconcileInterval time.Duration, clock watchClock) error {
	jobs, err := p.fullPass(ctx)
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
			newJobs, err := p.fullPass(ctx)
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
			continue
		}
		if len(jobs) == 0 {
			newJobs, err := p.fullPass(ctx)
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
			continue
		}
		if err := p.watchPass(ctx, jobs); err != nil {
			if isSystemicWatchFailure(err) {
				return err
			}
			log.Printf("⚠ refresh failed: %v", err)
		}
		nextWatch = clock.now().Add(watchInterval)
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
