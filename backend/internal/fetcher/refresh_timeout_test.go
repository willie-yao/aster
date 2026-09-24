package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/aggregator"
	"github.com/willie-yao/aster/backend/internal/fetchprogress"
	"github.com/willie-yao/aster/backend/internal/issues"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/notify"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/statefile"
	"github.com/willie-yao/aster/backend/internal/storage"
)

type failingJobListBackend struct {
	storage.Backend
	jobName string
}

func (b failingJobListBackend) List(ctx context.Context, prefix string) (*storage.Listing, error) {
	if prefix == "logs/"+b.jobName+"/" {
		return nil, errors.New("listing unavailable")
	}
	return b.Backend.List(ctx, prefix)
}

func listingFailureFixture(t *testing.T) (*pipeline, string, models.JobDetail) {
	t.Helper()
	dataDir, bucketDir := t.TempDir(), t.TempDir()
	for _, id := range []string{"1", "2", "3"} {
		writeFixtureFile(t, bucketDir, "logs/outage/"+id+"/started.json", `{"timestamp":1}`)
		writeFixtureFile(t, bucketDir, "logs/outage/"+id+"/finished.json", `{"timestamp":2,"passed":false,"result":"FAILURE"}`)
		writeFixtureFile(t, bucketDir, "logs/outage/"+id+"/artifacts/junit.xml", `<testsuite name="suite"><testcase name="fails" classname="suite"><failure message="still failing">still failing</failure></testcase></testsuite>`)
	}
	for _, id := range []string{"1", "2"} {
		writeFixtureFile(t, bucketDir, "logs/healthy/"+id+"/started.json", `{"timestamp":1}`)
		writeFixtureFile(t, bucketDir, "logs/healthy/"+id+"/finished.json", `{"timestamp":2,"passed":true,"result":"SUCCESS"}`)
		writeFixtureFile(t, bucketDir, "logs/healthy/"+id+"/artifacts/junit.xml", `<testsuite name="suite"><testcase name="passes" classname="suite"/></testsuite>`)
	}
	p := refreshLifecyclePipeline(t, dataDir, bucketDir, nil)
	p.enableAI = false
	p.opts.BuildsPerJob = 3
	p.opts.Workers = 2
	p.backend = failingJobListBackend{Backend: p.backend, jobName: "outage"}

	prior := models.JobDetail{JobID: "outage", Name: "outage", JobType: models.JobTypePeriodic}
	for _, id := range []string{"3", "2", "1"} {
		prior.Runs = append(prior.Runs, models.BuildResult{
			BuildInfo:  models.BuildInfo{BuildID: id, JobName: "outage", Started: time.Unix(1000, 0).UTC(), Result: "FAILURE", JUnitComplete: true},
			TestCases:  []models.TestCase{{Name: "fails", SuiteName: "suite", Status: "failed", FailureMessage: "still failing", JUnitFile: "junit.xml"}},
			TestsTotal: 1, TestsFailed: 1,
		})
	}
	prior.RetainedRuns = []models.BuildResult{{
		BuildInfo:  models.BuildInfo{BuildID: "0", JobName: "outage", Started: time.Unix(500, 0).UTC(), Result: "FAILURE"},
		TestsTotal: 8, TestsFailed: 5, TestsPassed: 3,
	}}
	pattern := models.PatternAnalysis{
		Subject: "outage", JobID: "outage", GeneratedAt: "2026-08-25T00:00:00Z",
		BuildsAnalyzed: 3, Systemic: true, Confidence: "high", SharedBuilds: []string{"3", "2", "1"},
		SharedRootCause: "still failing", SuggestedFix: "inspect the job", Summary: "persistent failure",
	}
	models.AssignPatternIdentity(&pattern)
	prior.PatternAnalyses = []models.PatternAnalysis{pattern}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "jobs", models.JobDataFilename("outage")), prior); err != nil {
		t.Fatal(err)
	}
	healthy := models.JobDetail{
		JobID: "healthy", Name: "healthy", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{
			BuildID: "1", JobName: "healthy", Started: time.Unix(1000, 0).UTC(), Passed: true, Result: "SUCCESS", JUnitComplete: true,
		}}},
	}
	if err := statefile.WriteJSON(filepath.Join(dataDir, "jobs", models.JobDataFilename("healthy")), healthy); err != nil {
		t.Fatal(err)
	}
	return p, bucketDir, prior
}

func listingFailureRefresh(t *testing.T, p *pipeline) *refreshResult {
	t.Helper()
	jobs, err := p.discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.refreshDataWithAnalysisContext(t.Context(), t.Context(), jobs)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRefreshData_ListingFailurePreservesPublishedJob(t *testing.T) {
	p, _, prior := listingFailureFixture(t)
	p.progress = fetchprogress.New(p.opts.OutDir, "sha-test")
	p.progress.StartPass(fetchprogress.PassOneShot)
	res := listingFailureRefresh(t, p)

	if len(res.details) != 2 {
		t.Fatalf("details = %+v, want both discovered jobs", res.details)
	}
	details, err := loadPublishedJobDetails(p.opts.OutDir)
	if err != nil {
		t.Fatal(err)
	}
	carried := details["outage"]
	if len(carried.Runs) != 3 || len(carried.RetainedRuns) != 1 || len(carried.PatternAnalyses) != 1 {
		t.Fatalf("published outage detail = %+v", carried)
	}
	for i, run := range carried.Runs {
		if run.BuildID != prior.Runs[i].BuildID || len(run.TestCases) != 1 ||
			run.TestCases[0].FailureMessage != "still failing" || run.TestsFailed != 1 ||
			run.TestCases[0].Source == models.TestCaseSourceBuild {
			t.Fatalf("carried run %d = %+v", i, run)
		}
	}
	if history := carried.RetainedRuns[0]; history.BuildID != "0" || history.TestsTotal != 8 ||
		history.TestsFailed != 5 || history.TestsPassed != 3 || len(history.TestCases) != 0 {
		t.Fatalf("display history = %+v", history)
	}
	if got := carried.PatternAnalyses[0]; got.ID != prior.PatternAnalyses[0].ID ||
		got.SharedRootCause != prior.PatternAnalyses[0].SharedRootCause ||
		!models.PatternEvidenceAvailable(carried, got) || !models.PatternIsActive(got) {
		t.Fatalf("retained pattern = %+v", got)
	}
	if healthy := details["healthy"]; len(healthy.Runs) != 2 || healthy.Runs[0].BuildID != "2" {
		t.Fatalf("healthy job did not advance: %+v", healthy)
	}
	data, err := os.ReadFile(filepath.Join(p.opts.OutDir, "dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dashboard models.Dashboard
	if err := json.Unmarshal(data, &dashboard); err != nil {
		t.Fatal(err)
	}
	if len(dashboard.Jobs) != 2 || !slices.ContainsFunc(dashboard.Jobs, func(job models.JobSummary) bool {
		return job.JobID == "outage" && job.CurrentStatus == models.JobCurrentFailing
	}) {
		t.Fatalf("published jobs = %+v", dashboard.Jobs)
	}
	if report := res.flakiness; len(report.PersistentFailures) != 1 ||
		report.PersistentFailures[0].JobID != "outage" || len(report.BuildFailures) != 0 ||
		len(report.RecurringPatterns) != 1 {
		t.Fatalf("flakiness report = %+v", report)
	}
	if progress := p.progress.Snapshot(); progress.Builds.Fetched != 1 || progress.Builds.Cached != 1 {
		t.Fatalf("build progress = %+v, carried runs must not count as fetched or cached", progress.Builds)
	}

	p.backend = p.backend.(failingJobListBackend).Backend
	p.progress.StartPass(fetchprogress.PassOneShot)
	recovered := listingFailureRefresh(t, p)
	if len(recovered.details) != 2 {
		t.Fatalf("successful listing details = %+v", recovered.details)
	}
	if progress := p.progress.Snapshot(); progress.Builds.Cached != 5 || progress.Builds.Fetched != 0 {
		t.Fatalf("successful listing did not reuse completed runs: %+v", progress.Builds)
	}
	t.Run("no prior detail", func(t *testing.T) {
		p, bucketDir, _ := listingFailureFixture(t)
		writeFixtureFile(t, bucketDir, "logs/new/1/started.json", `{"timestamp":1}`)
		p.backend = failingJobListBackend{Backend: p.backend, jobName: "new"}
		res := listingFailureRefresh(t, p)
		if slices.ContainsFunc(res.details, func(d models.JobDetail) bool { return d.JobID == "new" }) {
			t.Fatalf("unpublished job gained fabricated history: %+v", res.details)
		}
	})
	t.Run("undiscovered job is pruned", func(t *testing.T) {
		p, _, _ := listingFailureFixture(t)
		p.cfg.Discovery.JobFilters = []string{"healthy"}
		listingFailureRefresh(t, p)
		details, err := loadPublishedJobDetails(p.opts.OutDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(details) != 1 || details["healthy"].JobID != "healthy" {
			t.Fatalf("jobs after discovery removal = %+v", details)
		}
	})
}

func TestRefreshData_ListingFailureDoesNotRecoverFindings(t *testing.T) {
	p, _, prior := listingFailureFixture(t)
	jobs, err := p.discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	report := aggregator.ComputeFlakinessReport(map[string][]models.BuildResult{"outage": prior.Runs}, jobs, time.Now(), aggregator.Settings{})
	sender := &countingNotifySender{}
	stateFile := filepath.Join(p.opts.OutDir, "notification_state.json")
	from, to, err := notify.ParseAddresses(p.cfg.Notifications.Email.From, p.cfg.Notifications.Email.To)
	if err != nil {
		t.Fatal(err)
	}
	notifier := notify.NewNotifier(sender, from, to, stateFile, p.cfg.Name, p.cfg.Branding.SiteURL, p.backend.ProwURL("logs/"), false)
	initial, err := notifier.ProcessFailures(t.Context(), report, []models.JobDetail{prior})
	if err != nil || initial.NewAlerts != 1 {
		t.Fatalf("initial alert = %+v error=%v", initial, err)
	}
	if err := notifier.SaveState(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ISSUE_TOKEN", "test-token")
	manager := &recordingScheduledIssueManager{}
	var keepOpen map[string]bool
	oldFactory := newBatchIssueManager
	newBatchIssueManager = func(_ *issues.Client, _, _ string, opts issues.Options) scheduledIssueManager {
		keepOpen = opts.KeepOpenKeys
		return manager
	}
	t.Cleanup(func() { newBatchIssueManager = oldFactory })
	p.cfg.Branding.SourceRepo = project.SourceRepo{Owner: "example", Name: "repo"}
	p.cfg.Issues = &project.Issues{Enabled: true, Triggers: []string{project.IssueTriggerPatterns, project.IssueTriggerPersistent}}

	for _, failListing := range []bool{true, false} {
		if !failListing {
			p.backend = p.backend.(failingJobListBackend).Backend
		}
		res := listingFailureRefresh(t, p)
		if err := processIssues(t.Context(), p.cfg, res.flakiness, res.details, p.opts.OutDir); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(manager.activeKeys, issues.KeyPrefixPersistent+"outage::fails") ||
			!keepOpen[issues.KeyPrefixPattern+"outage"] {
			t.Fatalf("active=%v keep-open=%v, published findings were treated as recovered", manager.activeKeys, keepOpen)
		}
		notifier = notify.NewNotifier(sender, from, to, stateFile, p.cfg.Name, p.cfg.Branding.SiteURL, p.backend.ProwURL("logs/"), false)
		stats, err := notifier.ProcessFailures(t.Context(), res.flakiness, res.details)
		if err != nil || stats.Recoveries != 0 || stats.NewAlerts != 0 || stats.PatternAlerts != 0 {
			t.Fatalf("listing failure=%t: notification stats=%+v error=%v", failListing, stats, err)
		}
		if err := notifier.SaveState(); err != nil {
			t.Fatal(err)
		}
	}
	if sender.calls.Load() != 1 || manager.recoverCalls != 2 {
		t.Fatalf("email sends=%d issue recover calls=%d", sender.calls.Load(), manager.recoverCalls)
	}
}

func TestRefreshData_ListingFailureCarriesPendingRun(t *testing.T) {
	p, bucketDir, prior := listingFailureFixture(t)
	p.opts.BuildsPerJob = 4
	pending := models.BuildResult{BuildInfo: models.BuildInfo{
		BuildID: "4", JobName: "outage", Started: time.Unix(2000, 0).UTC(), Result: "PENDING",
	}}
	prior.Runs = append([]models.BuildResult{pending}, prior.Runs...)
	if err := statefile.WriteJSON(filepath.Join(p.opts.OutDir, "jobs", models.JobDataFilename("outage")), prior); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, bucketDir, "logs/outage/4/started.json", `{"timestamp":3}`)
	p.progress = fetchprogress.New(p.opts.OutDir, "sha-test")
	p.progress.StartPass(fetchprogress.PassOneShot)
	listingFailureRefresh(t, p)
	details, err := loadPublishedJobDetails(p.opts.OutDir)
	if err != nil {
		t.Fatal(err)
	}
	carried := details["outage"]
	if len(carried.Runs) != 4 || carried.Runs[0].Result != "PENDING" ||
		slices.ContainsFunc(carried.RetainedRuns, func(run models.BuildResult) bool { return run.BuildID == "4" }) {
		t.Fatalf("pending run was changed or moved to display history: %+v", carried)
	}

	writeFixtureFile(t, bucketDir, "logs/outage/4/finished.json", `{"timestamp":4,"passed":true,"result":"SUCCESS"}`)
	writeFixtureFile(t, bucketDir, "logs/outage/4/artifacts/junit.xml", `<testsuite name="suite"><testcase name="passes" classname="suite"/></testsuite>`)
	p.backend = p.backend.(failingJobListBackend).Backend
	p.progress.StartPass(fetchprogress.PassOneShot)
	listingFailureRefresh(t, p)
	details, err = loadPublishedJobDetails(p.opts.OutDir)
	if err != nil {
		t.Fatal(err)
	}
	if run := details["outage"].Runs[0]; run.BuildID != "4" || run.Result != "SUCCESS" {
		t.Fatalf("pending run was not refetched: %+v", run)
	}
	if progress := p.progress.Snapshot(); progress.Builds.Fetched != 1 {
		t.Fatalf("pending run fetch progress = %+v", progress.Builds)
	}
}

// TestRefreshData_TimedOutPassPreservesPublishedJobs pins that a pass which ran
// out of time does not publish. Publication prunes job files absent from the
// refresh, so a timed-out pass that published its partial view would delete the
// jobs it never reached, discarding their cached builds and the pattern analyses
// retention depends on.
func TestRefreshData_TimedOutPassPreservesPublishedJobs(t *testing.T) {
	dataDir := t.TempDir()
	jobsDir := filepath.Join(dataDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	published := models.JobDetail{
		JobID: "job", Name: "job", JobType: models.JobTypePeriodic,
		Runs: []models.BuildResult{{BuildInfo: models.BuildInfo{
			BuildID: "1", Started: time.Unix(1000, 0).UTC(), Passed: true,
			Result: "SUCCESS", JUnitComplete: true,
		}}},
	}
	if err := statefile.WriteJSON(filepath.Join(jobsDir, models.JobDataFilename("job")), published); err != nil {
		t.Fatal(err)
	}

	backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &pipeline{
		opts:    Options{OutDir: dataDir, BuildsPerJob: 1, Workers: 1},
		cfg:     &project.Config{Branding: project.Branding{Title: "T", BasePath: "/", SiteURL: "https://example.test"}},
		backend: backend,
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = p.refreshDataWithAnalysisContext(expired, expired,
		[]models.ProwJob{{Name: "job", JobID: "job", JobType: models.JobTypePeriodic}})
	if err == nil {
		t.Fatal("a timed-out pass reported success and published its partial view")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want a cancellation", err)
	}

	data, readErr := os.ReadFile(filepath.Join(jobsDir, models.JobDataFilename("job")))
	if readErr != nil {
		t.Fatalf("published job detail was deleted by a timed-out pass: %v", readErr)
	}
	var reloaded models.JobDetail
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Runs) != 1 || reloaded.Runs[0].BuildID != "1" {
		t.Errorf("published runs = %+v, want the untouched build 1", reloaded.Runs)
	}
}
