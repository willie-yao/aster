package fetcher

import (
	"context"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/willie-yao/aster/backend/internal/fixpr"
	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/notify"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/prow/jobconfig"
	runtimepkg "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/statefile"
	"github.com/willie-yao/aster/backend/internal/storage"
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
	results := make([]runtimepkg.CommandResult, 0, len(spec.CommandPolicy.Commands))
	for _, command := range spec.CommandPolicy.Commands {
		results = append(results, runtimepkg.CommandResult{Argv: command.Argv})
	}
	return runtimepkg.GenerateResult{
		Files:          map[string]string{"config/fix.yaml": "fixed: true\n"},
		Diff:           "diff --git a/config/fix.yaml b/config/fix.yaml\n+fixed: true\n",
		CommandResults: results,
		BaseSHA:        spec.Repo.Ref,
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

func (c *emailLoopFixPRClient) SearchAnyPR(ctx context.Context, owner, repo, token, marker string) (int, string, bool, error) {
	return c.SearchOpenPR(ctx, owner, repo, token, marker)
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

// emailLoopGitHubTransport fails any GitHub request. Side effects reach GitHub
// only through the injected fix-PR client, so a live call means a background
// poller came back.
type emailLoopGitHubTransport struct{}

func (emailLoopGitHubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected GitHub request: %s %s", request.Method, request.URL.String())
}

type emailLoopScenario struct {
	t          *testing.T
	dataDir    string
	cfg        *project.Config
	backend    storage.Backend
	catalog    *jobconfig.Catalog
	sender     *emailLoopSender
	github     emailLoopGitHubTransport
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
			AgentRuntime: &project.FixAgentRuntime{
				Type: "agent-sandbox", OutputLimitBytes: 64 << 10,
				AllowedCommands: []project.FixAgentCommand{{Argv: []string{"git", "diff", "--cached", "--check"}, Timeout: "1m"}},
			},
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
		t: t, dataDir: dataDir, cfg: cfg, backend: backend, catalog: catalog,
		sender: &emailLoopSender{}, fixPR: &emailLoopFixPRClient{},
		pattern: pattern, baseDetail: detail,
	}
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

func (s *emailLoopScenario) runTwice(t *testing.T, details []models.JobDetail, wantMessages int) {
	t.Helper()
	s.run(details)
	if got := len(s.sender.snapshot()); got != wantMessages {
		t.Fatalf("messages after first run = %d, want %d: %v", got, wantMessages, s.sender.subjects())
	}
	opens := s.fixPR.opens()
	searches := s.fixPR.searches()
	s.run(details)
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

func TestEmailNotificationAndFixPRE2E(t *testing.T) {
	scenario := newEmailLoopScenario(t)
	scenario.installFactories()
	baseDetails := []models.JobDetail{scenario.baseDetail}

	t.Run("systemic failure notifies and drafts one fix PR", func(t *testing.T) {
		scenario.runTwice(t, baseDetails, 1)
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
		if _, err := os.Stat(filepath.Join(scenario.dataDir, "notification_state.json")); err != nil {
			t.Fatalf("missing persisted notification state: %v", err)
		}
	})

	t.Run("later passing runs recover without post-merge attribution", func(t *testing.T) {
		details := scenario.withPeriodicRuns(
			emailLoopPassingRun("105", "later-105"),
			emailLoopPassingRun("104", "later-104"),
			emailLoopPassingRun("103", "later-103"),
		)
		scenario.run(details)
		if got := scenario.fixPR.opens(); got != 1 {
			t.Fatalf("recovery opened another fix PR: %d", got)
		}
	})

	for _, subject := range scenario.sender.subjects() {
		if strings.Contains(subject, "Remediation") {
			t.Fatalf("remediation lifecycle email sent: %q", subject)
		}
	}
	for _, name := range []string{"remediation_state.json", "remediation_prow_catalog.json", "remediations.json"} {
		if _, err := os.Stat(filepath.Join(scenario.dataDir, name)); !os.IsNotExist(err) {
			t.Fatalf("removed feature wrote %s: %v", name, err)
		}
	}
}
