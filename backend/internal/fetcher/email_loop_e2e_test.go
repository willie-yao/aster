package fetcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/notify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediation"
	runtimepkg "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

const (
	emailLoopJob       = "periodic-email-loop"
	emailLoopPresubmit = "pull-email-loop"
	emailLoopRepo      = "example/repo"
	emailLoopPRURL     = "https://github.com/example/repo/pull/7"
)

type emailLoopSender struct {
	mu       sync.Mutex
	messages []notify.Message
}

func (s *emailLoopSender) Send(_ context.Context, message notify.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	message.To = append([]mail.Address(nil), message.To...)
	s.messages = append(s.messages, message)
	return nil
}

func (s *emailLoopSender) snapshot() []notify.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notify.Message(nil), s.messages...)
}

func (s *emailLoopSender) subjects() []string {
	messages := s.snapshot()
	out := make([]string, 0, len(messages))
	for _, message := range messages {
		out = append(out, message.Subject)
	}
	return out
}

type emailLoopFixAgent struct{}

func (emailLoopFixAgent) Generate(_ context.Context, spec runtimepkg.GenerateSpec) (runtimepkg.GenerateResult, error) {
	if spec.Repo.Ref != "base-sha" {
		return runtimepkg.GenerateResult{}, fmt.Errorf("fix ref = %q", spec.Repo.Ref)
	}
	return runtimepkg.GenerateResult{
		Files: map[string]string{"config/fix.yaml": "fixed: true\n"},
		Diff:  "diff --git a/config/fix.yaml b/config/fix.yaml\n+fixed: true\n",
	}, nil
}

type emailLoopFixPRClient struct {
	mu          sync.Mutex
	openCalls   int
	searchCalls int
}

func (c *emailLoopFixPRClient) OpenPR(_ context.Context, _ ghpr.Request) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.openCalls++
	return emailLoopPRURL, nil
}

func (c *emailLoopFixPRClient) SearchOpenPR(context.Context, string, string, string, string) (int, string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.searchCalls++
	return 0, "", false, nil
}

func (*emailLoopFixPRClient) ResolveBase(context.Context, string, string) (ghpr.Base, error) {
	return ghpr.Base{Branch: "main", HeadSHA: "base-sha", TreeSHA: "base-tree"}, nil
}

func (c *emailLoopFixPRClient) opens() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.openCalls
}

func (c *emailLoopFixPRClient) searches() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.searchCalls
}

type emailLoopGitHubTransport struct {
	mu       sync.Mutex
	merged   bool
	requests map[string]int
}

func newEmailLoopGitHubTransport() *emailLoopGitHubTransport {
	return &emailLoopGitHubTransport{requests: map[string]int{}}
}

func (t *emailLoopGitHubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.requests[request.Method+" "+request.URL.Path]++
	merged := t.merged
	t.mu.Unlock()

	var body any
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/search/issues":
		body = map[string]any{"items": []any{}}
	case request.Method == http.MethodGet && request.URL.Path == "/repos/example/repo/pulls/7":
		state := "open"
		mergeSHA := "synthetic-1"
		mergedAt := ""
		if merged {
			state = "closed"
			mergeSHA = "merge-1"
			mergedAt = "2026-07-21T01:00:00Z"
		}
		body = map[string]any{
			"number": 7, "html_url": emailLoopPRURL, "state": state, "merged": merged,
			"merge_commit_sha": mergeSHA, "merged_at": mergedAt,
			"head": map[string]any{"sha": "head-1", "ref": "fix-email-loop", "repo": map[string]any{"full_name": "example-fork/repo"}},
			"base": map[string]any{"sha": "base-sha", "ref": "main", "repo": map[string]any{"full_name": emailLoopRepo}},
		}
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/example/repo/compare/merge-1..."):
		body = map[string]string{"status": "ahead"}
	default:
		return nil, fmt.Errorf("unexpected GitHub request: %s %s", request.Method, request.URL.String())
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
		Request:    request,
	}, nil
}

func (t *emailLoopGitHubTransport) setMerged(merged bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.merged = merged
}

func (t *emailLoopGitHubTransport) count(method, path string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requests[method+" "+path]
}

type emailLoopScenario struct {
	t          *testing.T
	dataDir    string
	bucketDir  string
	cfg        *project.Config
	backend    storage.Backend
	catalog    *jobconfig.Catalog
	sender     *emailLoopSender
	github     *emailLoopGitHubTransport
	fixPR      *emailLoopFixPRClient
	pattern    models.PatternAnalysis
	baseDetail models.JobDetail
}

func newEmailLoopScenario(t *testing.T) *emailLoopScenario {
	t.Helper()
	for _, key := range []string{"AI_TOKEN", "AI_ENDPOINT", "AI_MODEL", "ISSUE_TOKEN", "GITHUB_TOKEN", "EMAIL_SMTP_PASSWORD"} {
		t.Setenv(key, "")
	}
	t.Setenv("FIX_TOKEN", "test-token")
	dataDir := t.TempDir()
	bucketDir := t.TempDir()
	backend, err := storage.NewLocalBackend(bucketDir, "https://prow.test")
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	cfg := &project.Config{
		ID: "email-loop", Name: "Email Loop",
		Branding: project.Branding{Title: "Email Loop", BasePath: "/", SiteURL: "https://dashboard.test", SourceRepo: project.SourceRepo{Owner: "example", Name: "repo"}},
		AI: &project.AI{FixPRs: &project.FixPRs{
			Enabled: true, Repo: &project.SourceRepo{Owner: "example", Name: "repo"},
			AuthorName: "Test Maintainer", AuthorEmail: "maintainer@example.test", CritiqueRetries: &zero,
			AgentRuntime: &project.FixAgentRuntime{Type: "orka"},
		}},
		Notifications: &project.Notifications{Email: &project.EmailNotifications{
			Enabled: true, ActionLinks: true, From: "dashboard@example.test", To: []string{"maintainer@example.test"},
			SMTP: project.EmailSMTP{Host: "smtp.example.test", Port: 25, TLS: project.EmailTLSNone},
		}},
	}
	pattern := models.PatternAnalysis{
		JobID: emailLoopJob, Subject: emailLoopJob, GeneratedAt: "2026-07-21T00:00:00Z",
		BuildsAnalyzed: 3, Systemic: true, Confidence: "high",
		SharedRootCause: "the controller repeatedly times out applying configuration",
		SharedBuilds:    []string{"102", "101", "100"}, SuggestedFix: "serialize controller updates",
		Summary: "The same controller update failed in three builds.", RelevantFiles: []string{"config/fix.yaml"},
	}
	pattern.ID = models.PatternID(pattern)
	failure := models.TestCase{
		Name: "should reconcile", SuiteName: "email-loop", ClassName: "controller", Status: "failed",
		FailureMessage: "timed out applying controller configuration", JUnitFile: "junit.xml",
	}
	detail := models.JobDetail{Name: emailLoopJob, JobID: emailLoopJob, JobType: models.JobTypePeriodic, Repo: emailLoopRepo, PatternAnalyses: []models.PatternAnalysis{pattern}}
	for _, buildID := range []string{"102", "101", "100"} {
		detail.Runs = append(detail.Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{
				BuildID: buildID, JobName: emailLoopJob, Result: "FAILURE", Commit: "old-" + buildID,
				RepoRefs: map[string]string{emailLoopRepo: "main"}, JUnitComplete: true,
			},
			TestCases: []models.TestCase{failure}, TestsTotal: 1, TestsFailed: 1,
		})
	}
	catalog := &jobconfig.Catalog{Revision: "email-loop-v1", Jobs: map[string]jobconfig.JobDefinition{
		emailLoopJob: {
			Name: emailLoopJob, JobType: models.JobTypePeriodic,
			Refs: []jobconfig.RepoRef{{Org: "example", Repo: "repo", BaseRef: "main"}},
		},
		models.JobIDFor(models.JobTypePresubmit, emailLoopRepo, emailLoopPresubmit): {
			Name: emailLoopPresubmit, JobType: models.JobTypePresubmit, Repo: emailLoopRepo,
			RerunCommand: "/test email-loop", Branches: []string{"^main$"},
		},
	}}
	scenario := &emailLoopScenario{
		t: t, dataDir: dataDir, bucketDir: bucketDir, cfg: cfg, backend: backend, catalog: catalog,
		sender: &emailLoopSender{}, github: newEmailLoopGitHubTransport(), fixPR: &emailLoopFixPRClient{},
		pattern: pattern, baseDetail: detail,
	}
	scenario.writePresubmitBuild("90", "1", "historical-head", true)
	return scenario
}

func (s *emailLoopScenario) installFactories() {
	s.t.Helper()
	oldEmail := newEmailSender
	oldRuntime := newBatchFixRuntime
	oldManager := newBatchFixManager
	newEmailSender = func(notify.SMTPConfig) (notify.Sender, error) { return s.sender, nil }
	newBatchFixRuntime = func(*project.FixAgentRuntime) (runtimepkg.AgentRuntime, error) { return emailLoopFixAgent{}, nil }
	newBatchFixManager = func(_ string, stateFile string, opts fixpr.Options) *fixpr.Manager {
		return fixpr.NewManager(s.fixPR, stateFile, opts)
	}
	s.t.Cleanup(func() {
		newEmailSender = oldEmail
		newBatchFixRuntime = oldRuntime
		newBatchFixManager = oldManager
	})
}

func (s *emailLoopScenario) pipeline() *pipeline {
	return &pipeline{
		opts: Options{OutDir: s.dataDir}, cfg: s.cfg,
		client: &http.Client{Transport: s.github}, backend: s.backend, jobCatalog: s.catalog,
	}
}

func (s *emailLoopScenario) run(details []models.JobDetail) {
	s.t.Helper()
	report := models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{s.pattern}}
	if err := s.pipeline().runSideEffects(context.Background(), &refreshResult{details: details, flakiness: report}); err != nil {
		s.t.Fatal(err)
	}
}

func (s *emailLoopScenario) runTwice(t *testing.T, details []models.JobDetail, wantMessages int, wantStatus string) {
	t.Helper()
	s.run(details)
	s.assertState(t, wantStatus)
	if got := len(s.sender.snapshot()); got != wantMessages {
		t.Fatalf("messages after transition = %d, want %d: %v", got, wantMessages, s.sender.subjects())
	}
	opens := s.fixPR.opens()
	searches := s.fixPR.searches()
	s.run(details)
	s.assertState(t, wantStatus)
	if got := len(s.sender.snapshot()); got != wantMessages {
		t.Fatalf("messages after unchanged rerun = %d, want %d: %v", got, wantMessages, s.sender.subjects())
	}
	if s.fixPR.opens() != opens {
		t.Fatalf("fix PR opens changed from %d to %d", opens, s.fixPR.opens())
	}
	if s.fixPR.searches() != searches {
		t.Fatalf("fix PR searches changed from %d to %d", searches, s.fixPR.searches())
	}
}

func (s *emailLoopScenario) assertState(t *testing.T, wantStatus string) *remediation.Attempt {
	t.Helper()
	state, err := remediation.LoadForRepo(s.dataDir, emailLoopRepo)
	if err != nil {
		t.Fatal(err)
	}
	entry := state.Remediations[s.pattern.ID]
	if entry == nil || len(entry.Attempts) != 1 {
		t.Fatalf("remediation entry = %+v", entry)
	}
	attempt := &entry.Attempts[0]
	if attempt.Status != wantStatus || attempt.PRNumber != 7 || attempt.URL != emailLoopPRURL {
		t.Fatalf("attempt = %+v, want status %q", attempt, wantStatus)
	}
	if attempt.LastTransition != "" && attempt.LastEmailedTransitionIndex != attempt.TransitionIndex {
		t.Fatalf("email transition was not persisted: %+v", attempt)
	}
	data, err := os.ReadFile(filepath.Join(s.dataDir, remediation.PublicFileName))
	if err != nil {
		t.Fatal(err)
	}
	var public remediation.PublicState
	if err := json.Unmarshal(data, &public); err != nil {
		t.Fatal(err)
	}
	publicEntry, ok := public.Remediations[s.pattern.ID]
	if !ok || publicEntry.Attempt == nil || publicEntry.Attempt.Status != wantStatus {
		t.Fatalf("public remediation = %+v, want status %q", publicEntry, wantStatus)
	}
	assertPublicRemediationShape(t, data, s.pattern.ID)
	return attempt
}

func assertPublicRemediationShape(t *testing.T, data []byte, patternID string) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	assertAllowedKeys(t, "root", root, "remediations")
	remediations, ok := root["remediations"].(map[string]any)
	if !ok {
		t.Fatalf("public remediations has type %T", root["remediations"])
	}
	entry, ok := remediations[patternID].(map[string]any)
	if !ok {
		t.Fatalf("public remediation %q has type %T", patternID, remediations[patternID])
	}
	assertAllowedKeys(t, "remediation", entry,
		"id", "subject", "job_id", "job_name", "job_type", "issue", "attempt", "updated_at")
	if issue, exists := entry["issue"]; exists {
		issueObject, ok := issue.(map[string]any)
		if !ok {
			t.Fatalf("public issue has type %T", issue)
		}
		assertAllowedKeys(t, "issue", issueObject, "number", "url", "state")
	}
	attempt, ok := entry["attempt"].(map[string]any)
	if !ok {
		t.Fatalf("public attempt has type %T", entry["attempt"])
	}
	assertAllowedKeys(t, "attempt", attempt,
		"number", "pr_number", "url", "target_repo", "head_sha", "merge_sha", "status", "pr_state",
		"outcome", "outcome_reason", "observations")
	observations, exists := attempt["observations"]
	if !exists {
		return
	}
	list, ok := observations.([]any)
	if !ok {
		t.Fatalf("public observations has type %T", observations)
	}
	for i, value := range list {
		observation, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("public observation %d has type %T", i, value)
		}
		assertAllowedKeys(t, fmt.Sprintf("observation[%d]", i), observation,
			"build_id", "job_name", "job_type", "result", "outcome", "reason", "prow_url", "started_at", "completed_at")
	}
}

func assertAllowedKeys(t *testing.T, name string, object map[string]any, allowed ...string) {
	t.Helper()
	allow := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allow[key] = true
	}
	for key := range object {
		if !allow[key] {
			t.Fatalf("public %s contains unexpected key %q: %+v", name, key, object)
		}
	}
}

func (s *emailLoopScenario) writePresubmitBuild(buildID, pullNumber, headSHA string, passed bool) {
	s.t.Helper()
	repoPath := "example_repo"
	entry := "pr-logs/pull/" + repoPath + "/" + pullNumber + "/" + emailLoopPresubmit + "/" + buildID
	s.writeObject("pr-logs/directory/"+emailLoopPresubmit+"/"+buildID+".txt", entry)
	base := entry + "/"
	result := "FAILURE"
	state := "failure"
	passedJSON := "false"
	status := "failed"
	failure := `<failure message="timed out applying controller configuration">timed out applying controller configuration</failure>`
	if passed {
		result = "SUCCESS"
		state = "success"
		passedJSON = "true"
		status = "passed"
		failure = ""
	}
	s.writeObject(base+"started.json", `{"timestamp":1,"repos":{"example/repo":"main"},"repo-commit":"`+headSHA+`"}`)
	s.writeObject(base+"finished.json", `{"timestamp":2,"passed":`+passedJSON+`,"result":"`+result+`","revision":"`+headSHA+`"}`)
	s.writeObject(base+"prowjob.json", `{"spec":{"type":"presubmit","job":"`+emailLoopPresubmit+`","rerun_command":"/test email-loop","refs":{"org":"example","repo":"repo","base_ref":"main","pulls":[{"number":`+pullNumber+`,"sha":"`+headSHA+`"}]}},"status":{"state":"`+state+`","build_id":"`+buildID+`","url":"https://prow.test/view/`+buildID+`","startTime":"2026-07-21T00:00:00Z","completionTime":"2026-07-21T00:01:00Z"}}`)
	s.writeObject(base+"artifacts/junit.xml", `<testsuite name="email-loop"><testcase name="should reconcile" classname="controller" status="`+status+`">`+failure+`</testcase></testsuite>`)
}

func (s *emailLoopScenario) writeObject(path, body string) {
	s.t.Helper()
	full := filepath.Join(s.bucketDir, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *emailLoopScenario) withPeriodicRuns(runs ...models.BuildResult) []models.JobDetail {
	detail := s.baseDetail
	detail.Runs = append(append([]models.BuildResult(nil), runs...), s.baseDetail.Runs...)
	return []models.JobDetail{detail}
}

func emailLoopPassingRun(buildID, commit string) models.BuildResult {
	return models.BuildResult{
		BuildInfo: models.BuildInfo{
			BuildID: buildID, JobName: emailLoopJob, Result: "SUCCESS", Passed: true, Commit: commit,
			RepoRefs: map[string]string{emailLoopRepo: "main"}, JUnitComplete: true,
		},
		TestCases:  []models.TestCase{{Name: "should reconcile", SuiteName: "email-loop", ClassName: "controller", Status: "passed", JUnitFile: "junit.xml"}},
		TestsTotal: 1, TestsPassed: 1,
	}
}

func emailLoopFailingRun(buildID, commit string) models.BuildResult {
	return models.BuildResult{
		BuildInfo: models.BuildInfo{
			BuildID: buildID, JobName: emailLoopJob, Result: "FAILURE", Commit: commit,
			RepoRefs: map[string]string{emailLoopRepo: "main"}, JUnitComplete: true,
		},
		TestCases: []models.TestCase{{
			Name: "should reconcile", SuiteName: "email-loop", ClassName: "controller", Status: "failed",
			FailureMessage: "timed out applying controller configuration", JUnitFile: "junit.xml",
		}},
		TestsTotal: 1, TestsFailed: 1,
	}
}

func TestEmailRemediationLoopE2E(t *testing.T) {
	scenario := newEmailLoopScenario(t)
	scenario.installFactories()
	baseDetails := []models.JobDetail{scenario.baseDetail}

	t.Run("initial failure", func(t *testing.T) {
		scenario.runTwice(t, baseDetails, 2, remediation.StatusAwaitingPresubmit)
		messages := scenario.sender.snapshot()
		if !strings.Contains(messages[0].TextBody, "/job/"+emailLoopJob) ||
			!strings.Contains(messages[0].TextBody, "action=create-issue") ||
			!strings.Contains(messages[0].TextBody, "action=propose-fix") {
			t.Fatalf("pattern email action links missing: %s", messages[0].TextBody)
		}
		fixState := statefile.Load[fixpr.TrackedFix](filepath.Join(scenario.dataDir, "fix_pr_state.json"), emailLoopRepo, "fix PRs")
		if len(fixState.Tracked) != 1 || scenario.fixPR.opens() != 1 {
			t.Fatalf("tracked fixes=%+v opens=%d", fixState.Tracked, scenario.fixPR.opens())
		}
		for _, name := range []string{"notification_state.json", remediation.FileName, remediation.CatalogFileName} {
			if _, err := os.Stat(filepath.Join(scenario.dataDir, name)); err != nil {
				t.Fatalf("missing persisted state %s: %v", name, err)
			}
		}
		if scenario.github.count(http.MethodGet, "/search/issues") != 1 {
			t.Fatalf("marker searches = %d, want 1", scenario.github.count(http.MethodGet, "/search/issues"))
		}
	})

	t.Run("presubmit pass", func(t *testing.T) {
		scenario.writePresubmitBuild("103", "7", "head-1", true)
		scenario.runTwice(t, baseDetails, 3, remediation.StatusPremergeVerified)
	})

	t.Run("merged observing", func(t *testing.T) {
		scenario.github.setMerged(true)
		scenario.runTwice(t, baseDetails, 4, remediation.StatusObserving)
	})

	t.Run("periodic clean", func(t *testing.T) {
		details := scenario.withPeriodicRuns(
			emailLoopPassingRun("104", "post-merge-104"),
			emailLoopPassingRun("103", "post-merge-103"),
		)
		scenario.runTwice(t, details, 5, remediation.StatusVerifiedFixed)
		attempt := scenario.assertState(t, remediation.StatusVerifiedFixed)
		clean := 0
		for _, observation := range attempt.Observations {
			if observation.JobType == models.JobTypePeriodic && observation.Outcome == remediation.OutcomePassed {
				clean++
			}
		}
		if clean != 2 {
			t.Fatalf("clean periodic observations = %d, want 2: %+v", clean, attempt.Observations)
		}
	})

	t.Run("same cause recurrence", func(t *testing.T) {
		details := scenario.withPeriodicRuns(
			emailLoopFailingRun("105", "post-merge-105"),
			emailLoopPassingRun("104", "post-merge-104"),
			emailLoopPassingRun("103", "post-merge-103"),
		)
		attempt := scenario.runRecurrenceTwice(t, details)
		messages := scenario.sender.snapshot()
		wantJobURL := "https://dashboard.test/job/" + emailLoopJob
		if !strings.Contains(messages[len(messages)-1].TextBody, emailLoopPRURL) ||
			!strings.Contains(messages[len(messages)-1].TextBody, wantJobURL) {
			t.Fatalf("recurrence email links missing: %s", messages[len(messages)-1].TextBody)
		}
		if attempt.Outcome != remediation.OutcomeSameCause {
			t.Fatalf("recurrence outcome = %+v", attempt)
		}
		if scenario.fixPR.opens() != 1 {
			t.Fatalf("recurrence opened another fix PR: %d", scenario.fixPR.opens())
		}
	})

	wantSubjects := []string{
		"[Email Loop] Systemic recurring failure: " + emailLoopJob,
		"[Email Loop] Remediation awaiting presubmit: " + emailLoopJob,
		"[Email Loop] Remediation passed presubmit verification: " + emailLoopJob,
		"[Email Loop] Remediation observing post-merge runs: " + emailLoopJob,
		"[Email Loop] Remediation verified fixed: " + emailLoopJob,
		"[Email Loop] Remediation same failure still present: " + emailLoopJob,
	}
	if got := scenario.sender.subjects(); !slices.Equal(got, wantSubjects) {
		t.Fatalf("subjects = %#v, want %#v", got, wantSubjects)
	}
	if scenario.github.count(http.MethodGet, "/search/issues") != 1 {
		t.Fatalf("final marker searches = %d, want 1", scenario.github.count(http.MethodGet, "/search/issues"))
	}
	data, err := os.ReadFile(filepath.Join(scenario.dataDir, remediation.PublicFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"post-merge-105", "timed out applying controller configuration", "email-loop\\u0000controller"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("public remediation state leaked %q: %s", secret, data)
		}
	}
}

func (s *emailLoopScenario) runRecurrenceTwice(t *testing.T, details []models.JobDetail) *remediation.Attempt {
	t.Helper()
	s.run(details)
	s.assertState(t, remediation.StatusStillFailingSameCause)
	if got := len(s.sender.snapshot()); got != 6 {
		t.Fatalf("messages after recurrence = %d, want 6", got)
	}
	s.run(details)
	attempt := s.assertState(t, remediation.StatusStillFailingSameCause)
	if got := len(s.sender.snapshot()); got != 6 {
		t.Fatalf("messages after recurrence rerun = %d, want 6", got)
	}
	return attempt
}
