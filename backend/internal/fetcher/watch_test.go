package fetcher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/models"
)

type fakeWatchPipeline struct {
	mu             sync.Mutex
	events         []string
	fullCalls      int
	watchCalls     int
	active         int
	maxActive      int
	fullResults    []error
	watchErrors    []error
	advance        func(time.Duration)
	passTime       time.Duration
	progressEvents []string
	schedules      [][2]time.Time
}

func (p *fakeWatchPipeline) fullPass(context.Context) ([]models.ProwJob, error) {
	p.begin("full")
	defer p.end()
	p.mu.Lock()
	call := p.fullCalls
	p.fullCalls++
	var err error
	if call < len(p.fullResults) {
		err = p.fullResults[call]
	}
	p.mu.Unlock()
	if p.advance != nil {
		p.advance(p.passTime)
	}
	if err != nil {
		return nil, err
	}
	return []models.ProwJob{{Name: "job", JobID: "job"}}, nil
}

func (p *fakeWatchPipeline) watchPass(context.Context, []models.ProwJob) error {
	p.begin("watch")
	defer p.end()
	p.mu.Lock()
	call := p.watchCalls
	p.watchCalls++
	var err error
	if call < len(p.watchErrors) {
		err = p.watchErrors[call]
	}
	p.mu.Unlock()
	if p.advance != nil {
		p.advance(p.passTime)
	}
	return err
}

func (p *fakeWatchPipeline) begin(event string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
	p.active++
	if p.active > p.maxActive {
		p.maxActive = p.active
	}
}

func (p *fakeWatchPipeline) end() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active--
}

func (p *fakeWatchPipeline) beginWatchPass(passType fetchprogress.PassType) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.progressEvents = append(p.progressEvents, "begin:"+string(passType))
}

func (p *fakeWatchPipeline) finishWatchPass(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	p.progressEvents = append(p.progressEvents, "finish:"+outcome)
}

func (p *fakeWatchPipeline) setWatchSchedule(nextWatch, nextReconcile time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.schedules = append(p.schedules, [2]time.Time{nextWatch, nextReconcile})
}

type fakeWatchTime struct {
	now       time.Time
	waits     []time.Time
	maxWaits  int
	cancel    context.CancelFunc
	waitError error
}

func (c *fakeWatchTime) clock() watchClock {
	return watchClock{
		now: func() time.Time { return c.now },
		wait: func(ctx context.Context, deadline time.Time) error {
			if len(c.waits) >= c.maxWaits {
				c.cancel()
				return ctx.Err()
			}
			c.waits = append(c.waits, deadline)
			c.now = deadline
			if c.waitError != nil {
				return c.waitError
			}
			return nil
		},
	}
}

func (c *fakeWatchTime) advance(d time.Duration) { c.now = c.now.Add(d) }

func TestRunWatchLoopRunsInitialWatchAndReconcilePassesSerially(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	clock := &fakeWatchTime{now: time.Unix(0, 0), maxWaits: 3, cancel: cancel}
	pipeline := &fakeWatchPipeline{}
	err := runWatchLoop(ctx, pipeline, 5*time.Minute, 12*time.Minute, clock.clock())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runWatchLoop error = %v", err)
	}
	want := []string{"full", "watch", "watch", "full"}
	if fmt.Sprint(pipeline.events) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", pipeline.events, want)
	}
	if pipeline.maxActive != 1 {
		t.Fatalf("maximum overlapping passes = %d, want 1", pipeline.maxActive)
	}
	if pipeline.fullCalls != 2 || pipeline.watchCalls != 2 {
		t.Fatalf("full=%d watch=%d", pipeline.fullCalls, pipeline.watchCalls)
	}
	wantProgress := []string{
		"finish:success",
		"begin:lightweight-watch", "finish:success",
		"begin:lightweight-watch", "finish:success",
		"begin:reconcile", "finish:success",
	}
	if fmt.Sprint(pipeline.progressEvents) != fmt.Sprint(wantProgress) {
		t.Fatalf("progress events = %v, want %v", pipeline.progressEvents, wantProgress)
	}
	if len(pipeline.schedules) != 4 {
		t.Fatalf("schedule updates = %d, want 4", len(pipeline.schedules))
	}
}

func TestRunWatchLoopRetriesTransientFailureAfterCompletionInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	clock := &fakeWatchTime{now: time.Unix(0, 0), maxWaits: 2, cancel: cancel}
	pipeline := &fakeWatchPipeline{
		watchErrors: []error{errors.New("refresh unavailable")},
		advance:     clock.advance,
		passTime:    7 * time.Minute,
	}
	err := runWatchLoop(ctx, pipeline, 5*time.Minute, time.Hour, clock.clock())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runWatchLoop error = %v", err)
	}
	if pipeline.watchCalls != 2 {
		t.Fatalf("watch calls = %d, want 2", pipeline.watchCalls)
	}
	if len(clock.waits) != 2 || clock.waits[1].Sub(clock.waits[0]) != 12*time.Minute {
		t.Fatalf("wait deadlines = %v, want pass duration plus watch interval", clock.waits)
	}
}

func TestRunWatchLoopRetriesInitialFailureWithoutSideEffectsOnWatchPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	clock := &fakeWatchTime{now: time.Unix(0, 0), maxWaits: 2, cancel: cancel}
	pipeline := &fakeWatchPipeline{fullResults: []error{errors.New("discovery unavailable"), nil}}
	err := runWatchLoop(ctx, pipeline, time.Minute, time.Hour, clock.clock())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runWatchLoop error = %v", err)
	}
	want := []string{"full", "full", "watch"}
	if fmt.Sprint(pipeline.events) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", pipeline.events, want)
	}
}
