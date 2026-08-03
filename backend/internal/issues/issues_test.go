package issues

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// fakeGitHub is an in-memory GitHub issues API for tests.
type fakeGitHub struct {
	*httptest.Server
	mu          sync.Mutex
	issues      map[int]*fakeIssue
	comments    map[int][]string
	nextNum     int
	createCalls int
}

type fakeIssue struct {
	Number int
	Title  string
	Body   string
	State  string
	Labels []string
}

var hex16 = regexp.MustCompile(`[0-9a-f]{16}`)

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		issues:   map[int]*fakeIssue{},
		comments: map[int][]string{},
		nextNum:  100,
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/search/issues"):
		token := hex16.FindString(r.URL.Query().Get("q"))
		type item struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Body    string `json:"body"`
		}
		var items []item
		if token != "" {
			for _, is := range f.issues {
				if is.State == "open" && strings.Contains(is.Body, token) {
					items = append(items, item{Number: is.Number, HTMLURL: f.url(is.Number), Body: is.Body})
				}
			}
		}
		writeJSON(w, 200, map[string]any{"items": items})

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		num := pathInt(r.URL.Path, "/issues/", "/comments")
		var in struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(body, &in)
		f.comments[num] = append(f.comments[num], in.Body)
		writeJSON(w, 201, map[string]any{"id": 1})

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
		f.createCalls++
		var in struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		_ = json.Unmarshal(body, &in)
		num := f.nextNum
		f.nextNum++
		f.issues[num] = &fakeIssue{Number: num, Title: in.Title, Body: in.Body, State: "open", Labels: in.Labels}
		writeJSON(w, 201, map[string]any{"number": num, "html_url": f.url(num)})

	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/issues/"):
		num := pathIntSuffix(r.URL.Path, "/issues/")
		if is, ok := f.issues[num]; ok {
			var in struct {
				State string `json:"state"`
			}
			_ = json.Unmarshal(body, &in)
			if in.State != "" {
				is.State = in.State
			}
		}
		writeJSON(w, 200, map[string]any{"number": num})

	default:
		http.Error(w, "unexpected: "+r.Method+" "+r.URL.Path, 500)
	}
}

func (f *fakeGitHub) url(n int) string { return f.URL + "/issues/" + strconv.Itoa(n) }

// seedOpenIssue inserts a pre-existing open issue.
func (f *fakeGitHub) seedOpenIssue(body string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	num := f.nextNum
	f.nextNum++
	f.issues[num] = &fakeIssue{Number: num, Body: body, State: "open"}
	return num
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func pathInt(path, after, before string) int {
	s := path
	if i := strings.Index(s, after); i >= 0 {
		s = s[i+len(after):]
	}
	if i := strings.Index(s, before); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.Atoi(s)
	return n
}

func pathIntSuffix(path, after string) int {
	if i := strings.LastIndex(path, after); i >= 0 {
		n, _ := strconv.Atoi(path[i+len(after):])
		return n
	}
	return 0
}

func newTestClient(f *fakeGitHub) *Client {
	c := NewClient("test-token", "owner", "repo")
	c.apiBase = f.URL
	return c
}

func defaultOpts() Options {
	return Options{
		CommentOnRecovery: true,
		CloseOnRecovery:   false,
		MaxNewPerRun:      5,
		RecoverPrefixes:   []string{KeyPrefixPattern, KeyPrefixPersistent},
	}
}

// newTestManager builds a Manager against the fake, scoped to "owner/repo".
func newTestManager(t *testing.T, f *fakeGitHub, opts Options) *Manager {
	t.Helper()
	return NewManager(newTestClient(f), filepath.Join(t.TempDir(), "s.json"), "owner/repo", opts)
}

func spec(key string) IssueSpec {
	return IssueSpec{
		Key:   key,
		Title: "title for " + key,
		Body:  "body\n\n" + markerFor(key) + "\n",
	}
}

func TestReconcile_CreatesNewIssue(t *testing.T) {
	f := newFakeGitHub(t)
	m := newTestManager(t, f, defaultOpts())

	stats, err := m.Reconcile(context.Background(), []IssueSpec{spec("pattern::job-a")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Created != 1 {
		t.Errorf("Created = %d, want 1", stats.Created)
	}
	if _, ok := m.state.Tracked["pattern::job-a"]; !ok {
		t.Error("expected key tracked in state after create")
	}
	if f.createCalls != 1 {
		t.Errorf("create API calls = %d, want 1", f.createCalls)
	}
}

func TestReconcile_SkipsWhenAlreadyTracked(t *testing.T) {
	f := newFakeGitHub(t)
	m := newTestManager(t, f, defaultOpts())
	s := []IssueSpec{spec("pattern::job-a")}

	if _, err := m.Reconcile(context.Background(), s); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Second reconcile with the same finding must not create a new issue.
	stats, err := m.Reconcile(context.Background(), s)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if stats.Created != 0 {
		t.Errorf("second Created = %d, want 0", stats.Created)
	}
	if f.createCalls != 1 {
		t.Errorf("total create calls = %d, want 1", f.createCalls)
	}
}

func TestReconcile_AdoptsExistingWhenStateLost(t *testing.T) {
	f := newFakeGitHub(t)
	// Simulate a prior run: an open issue already carries this key's marker,
	// but local state is empty (cache evicted).
	key := "persistent::job-b::TestThing"
	existing := f.seedOpenIssue("old body\n" + markerFor(key))
	m := newTestManager(t, f, defaultOpts())

	stats, err := m.Reconcile(context.Background(), []IssueSpec{spec(key)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Created != 0 {
		t.Errorf("Created = %d, want 0 (should adopt, not create)", stats.Created)
	}
	if stats.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", stats.Adopted)
	}
	if got := m.state.Tracked[key].Number; got != existing {
		t.Errorf("adopted issue number = %d, want %d", got, existing)
	}
	if f.createCalls != 0 {
		t.Errorf("create calls = %d, want 0", f.createCalls)
	}
}

func TestReconcile_RecoveryComments(t *testing.T) {
	f := newFakeGitHub(t)
	m := newTestManager(t, f, defaultOpts())
	key := "pattern::job-c"

	if _, err := m.Reconcile(context.Background(), []IssueSpec{spec(key)}); err != nil {
		t.Fatalf("file: %v", err)
	}
	num := m.state.Tracked[key].Number

	// Missing finding triggers a recovery comment and untracks it.
	stats, err := m.Reconcile(context.Background(), nil)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if stats.Recovered != 1 {
		t.Errorf("Recovered = %d, want 1", stats.Recovered)
	}
	if _, ok := m.state.Tracked[key]; ok {
		t.Error("expected key removed from state after recovery")
	}
	if len(f.comments[num]) != 1 {
		t.Errorf("recovery comments on #%d = %d, want 1", num, len(f.comments[num]))
	}
	// A subsequent empty run must not comment again.
	if _, err := m.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("third: %v", err)
	}
	if len(f.comments[num]) != 1 {
		t.Errorf("comments after second empty run = %d, want 1 (no repeat)", len(f.comments[num]))
	}
}

func TestReconcile_RecoveryCloses(t *testing.T) {
	f := newFakeGitHub(t)
	opts := defaultOpts()
	opts.CloseOnRecovery = true
	m := newTestManager(t, f, opts)
	key := "pattern::job-d"

	_, _ = m.Reconcile(context.Background(), []IssueSpec{spec(key)})
	num := m.state.Tracked[key].Number
	if _, err := m.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if f.issues[num].State != "closed" {
		t.Errorf("issue #%d state = %q, want closed", num, f.issues[num].State)
	}
}

func TestReconcile_RetiresTrackedIssueAfterFindingDisappears(t *testing.T) {
	f := newFakeGitHub(t)
	opts := defaultOpts()
	key := "pattern::job-retired"
	opts.RetireKeys = map[string]bool{key: true}
	m := newTestManager(t, f, opts)

	if _, err := m.Reconcile(context.Background(), []IssueSpec{spec(key)}); err != nil {
		t.Fatalf("file: %v", err)
	}
	num := m.state.Tracked[key].Number
	f.issues[num].State = "closed"
	if _, err := m.Reconcile(context.Background(), []IssueSpec{spec(key)}); err != nil {
		t.Fatalf("active: %v", err)
	}
	if _, ok := m.state.Tracked[key]; !ok {
		t.Fatal("active finding must remain tracked")
	}
	if f.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", f.createCalls)
	}

	stats, err := m.Reconcile(context.Background(), nil)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if stats.Recovered != 0 {
		t.Fatalf("Recovered = %d, want 0", stats.Recovered)
	}
	if _, ok := m.state.Tracked[key]; ok {
		t.Fatal("retired finding must be removed from tracking")
	}
	if len(f.comments[num]) != 0 {
		t.Fatalf("comments on #%d = %d, want 0", num, len(f.comments[num]))
	}
	if f.issues[num].State != "closed" {
		t.Fatalf("issue #%d state = %q, want unchanged", num, f.issues[num].State)
	}
}

func TestReconcile_KeepOpenTakesPriorityOverRetire(t *testing.T) {
	f := newFakeGitHub(t)
	opts := defaultOpts()
	key := "pattern::job-pending"
	opts.KeepOpenKeys = map[string]bool{key: true}
	opts.RetireKeys = map[string]bool{key: true}
	m := newTestManager(t, f, opts)

	if _, err := m.Reconcile(context.Background(), []IssueSpec{spec(key)}); err != nil {
		t.Fatalf("file: %v", err)
	}
	if _, err := m.Reconcile(context.Background(), nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := m.state.Tracked[key]; !ok {
		t.Fatal("pending remediation must remain tracked")
	}
}

func TestReconcile_MaxNewPerRun(t *testing.T) {
	f := newFakeGitHub(t)
	opts := defaultOpts()
	opts.MaxNewPerRun = 2
	m := newTestManager(t, f, opts)

	stats, err := m.Reconcile(context.Background(), []IssueSpec{
		spec("k1"), spec("k2"), spec("k3"), spec("k4"),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Created != 2 {
		t.Errorf("Created = %d, want 2 (capped)", stats.Created)
	}
	if f.createCalls != 2 {
		t.Errorf("create calls = %d, want 2", f.createCalls)
	}
}

func TestState_RoundTrip(t *testing.T) {
	f := newFakeGitHub(t)
	path := filepath.Join(t.TempDir(), "s.json")
	m := NewManager(newTestClient(f), path, "owner/repo", defaultOpts())
	if _, err := m.Reconcile(context.Background(), []IssueSpec{spec("pattern::job-e")}); err != nil {
		t.Fatalf("file: %v", err)
	}
	if err := m.SaveState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Fresh manager loads the same state and does NOT re-create.
	m2 := NewManager(newTestClient(f), path, "owner/repo", defaultOpts())
	if _, ok := m2.state.Tracked["pattern::job-e"]; !ok {
		t.Fatal("expected loaded state to contain the tracked key")
	}
	stats, err := m2.Reconcile(context.Background(), []IssueSpec{spec("pattern::job-e")})
	if err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	if stats.Created != 0 {
		t.Errorf("Created after reload = %d, want 0", stats.Created)
	}
}

func TestState_DiscardedOnRepoChange(t *testing.T) {
	f := newFakeGitHub(t)
	path := filepath.Join(t.TempDir(), "s.json")
	prior := State{Repo: "other/repo", Tracked: map[string]TrackedIssue{
		"pattern::job-a": {Number: 42},
	}}
	data, _ := json.Marshal(prior)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// A manager for owner/repo must ignore that state (numbers are meaningless
	// here) and create fresh rather than skip.
	m := NewManager(newTestClient(f), path, "owner/repo", defaultOpts())
	if _, ok := m.state.Tracked["pattern::job-a"]; ok {
		t.Fatal("state from a different repo must be discarded on load")
	}
	stats, err := m.Reconcile(context.Background(), []IssueSpec{spec("pattern::job-a")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Created != 1 {
		t.Errorf("Created = %d, want 1 (stale cross-repo state ignored)", stats.Created)
	}
}

func TestReconcile_RecoveryScopedToEnabledPrefixes(t *testing.T) {
	f := newFakeGitHub(t)
	opts := defaultOpts()
	opts.RecoverPrefixes = []string{KeyPrefixPersistent} // patterns NOT recoverable
	m := newTestManager(t, f, opts)
	key := "pattern::job-a"

	if _, err := m.Reconcile(context.Background(), []IssueSpec{spec(key)}); err != nil {
		t.Fatalf("file: %v", err)
	}
	num := m.state.Tracked[key].Number

	// Finding absent, but its prefix isn't in RecoverPrefixes (e.g. the
	// patterns trigger is off / wasn't evaluated): must NOT comment or untrack.
	stats, err := m.Reconcile(context.Background(), nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if stats.Recovered != 0 {
		t.Errorf("Recovered = %d, want 0 (prefix not enabled)", stats.Recovered)
	}
	if _, ok := m.state.Tracked[key]; !ok {
		t.Error("key should remain tracked when its prefix isn't recoverable")
	}
	if len(f.comments[num]) != 0 {
		t.Errorf("comments = %d, want 0", len(f.comments[num]))
	}
}

func TestBuildSpecs_PatternsAndPersistent(t *testing.T) {
	report := models.FlakinessReport{
		RecurringPatterns: []models.PatternAnalysis{
			{Subject: "job-x", JobID: "job-x", Systemic: true, Confidence: "high", BuildsAnalyzed: 5,
				SharedRootCause: "etcd timeout", SuggestedFix: "bigger VM", SharedBuilds: []string{"111", "222"}},
			{Subject: "job-y", JobID: "job-y", Systemic: false, Confidence: "low"}, // excluded
		},
		PersistentFailures: []models.TestFlakiness{
			{JobID: "job-z", JobName: "job-z", TestName: "TestFoo", ConsecutiveFailures: 4,
				LastFailure: &models.TestFailureInfo{BuildID: "999", FailureMessage: "boom"}},
			{JobID: "job-w", JobName: "job-w", TestName: "TestBar", ConsecutiveFailures: 2}, // below floor
		},
	}
	details := []models.JobDetail{{
		JobID: "job-z",
		Runs: []models.BuildResult{{
			TestCases: []models.TestCase{{
				Name: "TestFoo", Status: "failed",
				AIAnalysis: &models.AIAnalysis{RootCause: "the bug"},
			}},
		}},
	}}

	specs := BuildSpecs(BuildInput{
		Report:       report,
		JobDetails:   details,
		Triggers:     []string{project.IssueTriggerPatterns, project.IssueTriggerPersistent},
		Labels:       []string{"prow-dashboard"},
		DashboardURL: "https://example.io/dash/",
	})

	if len(specs) != 2 {
		t.Fatalf("got %d specs, want 2 (1 systemic pattern + 1 persistent>=3)", len(specs))
	}
	byKey := map[string]IssueSpec{}
	for _, s := range specs {
		byKey[s.Key] = s
	}
	pat, ok := byKey["pattern::job-x"]
	if !ok {
		t.Fatal("missing pattern spec for job-x")
	}
	if !strings.Contains(pat.Body, markerFor(pat.Key)) {
		t.Error("pattern body must embed its marker")
	}
	if !strings.Contains(pat.Body, "etcd timeout") || !strings.Contains(pat.Body, "bigger VM") {
		t.Error("pattern body should include root cause and suggested fix")
	}
	if !strings.Contains(pat.Body, "https://example.io/dash/job/job-x?run=111") {
		t.Errorf("pattern body should link affected builds; body:\n%s", pat.Body)
	}
	per, ok := byKey["persistent::job-z::TestFoo"]
	if !ok {
		t.Fatal("missing persistent spec for job-z/TestFoo")
	}
	if !strings.Contains(per.Body, "the bug") {
		t.Error("persistent body should include the AI root cause")
	}
	if !strings.Contains(per.Body, markerFor(per.Key)) {
		t.Error("persistent body must embed its marker")
	}
}

type structuredCompleter struct {
	raw string
	err error
}

func (c structuredCompleter) CompleteStructured(_ context.Context, _, _ string, _ ai.ResponseFormat, validate ai.StructuredValidator) error {
	if c.err != nil {
		return c.err
	}
	return validate(json.RawMessage(c.raw))
}

func TestReviseBodyRejectsObservedKimiReasoning(t *testing.T) {
	spec := IssueSpec{Key: "pattern::job-a", Title: "safe title", Body: "## What happened\nsafe body\n\n" + markerFor("pattern::job-a"), Labels: []string{"kind/bug"}}
	raw := `{"body":"The user wants me to revise this.\nSo I need to keep the current structure.\nLet me draft it.\nThis looks good.\n\n## What happened\nunsafe replacement"}`
	got, err := ReviseBody(context.Background(), structuredCompleter{raw: raw}, spec, "tighten it")
	if !errors.Is(err, ErrRevisionRejected) {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(got, spec) {
		t.Fatalf("unsafe revision changed draft: %+v", got)
	}
}

func TestReviseBodyAddsMarkerAfterValidation(t *testing.T) {
	key := "pattern::job-a"
	spec := IssueSpec{Key: key, Title: "title", Body: "old\n\n" + markerFor(key)}
	got, err := ReviseBody(context.Background(), structuredCompleter{raw: `{"body":"## What happened\nrevised safely"}`}, spec, "tighten it")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Body, "revised safely") || strings.Count(got.Body, markerFor(key)) != 1 {
		t.Fatalf("revised body = %q", got.Body)
	}
}

func TestBuildSpecs_TriggerSelection(t *testing.T) {
	report := models.FlakinessReport{
		RecurringPatterns:  []models.PatternAnalysis{{Subject: "j", JobID: "j", Systemic: true, Confidence: "high"}},
		PersistentFailures: []models.TestFlakiness{{JobID: "j", JobName: "j", TestName: "T", ConsecutiveFailures: 5}},
	}
	only := BuildSpecs(BuildInput{Report: report, Triggers: []string{project.IssueTriggerPatterns}, DashboardURL: "https://x"})
	if len(only) != 1 || !strings.HasPrefix(only[0].Key, "pattern::") {
		t.Errorf("patterns-only trigger should yield 1 pattern spec, got %+v", only)
	}
}

func TestMarker_StableAndKeyed(t *testing.T) {
	if first, second := markerFor("a"), markerFor("a"); first != second {
		t.Error("marker must be stable for the same key")
	}
	if markerFor("a") == markerFor("b") {
		t.Error("marker must differ for different keys")
	}
	if !strings.Contains(markerFor("a"), markerToken("a")) {
		t.Error("marker comment should contain the search token")
	}
}

// TestReconcile_SingleSpecNoRecoverPrefixes documents the on-demand actions
// contract: filing one issue with no recover prefixes must not recover, close,
// or drop any other tracked finding.
func TestReconcile_SingleSpecNoRecoverPrefixes(t *testing.T) {
	f := newFakeGitHub(t)
	m := newTestManager(t, f, Options{MaxNewPerRun: 1}) // no RecoverPrefixes

	// Pre-track an unrelated finding as if a prior batch run filed it.
	m.state.Tracked["pattern::other-job"] = TrackedIssue{Number: 42, URL: "u", FirstFiledAt: now()}

	stats, err := m.Reconcile(context.Background(), []IssueSpec{spec("pattern::job-a")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Created != 1 {
		t.Errorf("Created = %d, want 1", stats.Created)
	}
	if stats.Recovered != 0 {
		t.Errorf("Recovered = %d, want 0 (single-spec create must not recover others)", stats.Recovered)
	}
	if _, ok := m.state.Tracked["pattern::other-job"]; !ok {
		t.Error("unrelated tracked finding was dropped by a single-spec create")
	}
}

// stubFiller reformats to a template-like body that drops the marker, to prove
// Reconcile re-adds it.
type stubFiller struct{}

func (stubFiller) FillIssue(_ context.Context, title, _ string) (string, string) {
	return title + " (templated)", "**What happened**: filled from template"
}

func TestReconcile_TemplateFillerPreservesMarker(t *testing.T) {
	f := newFakeGitHub(t)
	opts := defaultOpts()
	opts.TemplateFiller = stubFiller{}
	m := newTestManager(t, f, opts)

	key := "pattern::job-a"
	if _, err := m.Reconcile(context.Background(), []IssueSpec{spec(key)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The created issue body must still carry the dedup marker so a later run
	// adopts it instead of filing a duplicate.
	var created *fakeIssue
	for _, is := range f.issues {
		created = is
	}
	if created == nil {
		t.Fatal("no issue created")
	}
	if !strings.Contains(created.Body, markerFor(key)) {
		t.Errorf("marker missing from templated body: %q", created.Body)
	}
	if !strings.Contains(created.Body, "filled from template") {
		t.Errorf("template fill not applied: %q", created.Body)
	}
	if !strings.Contains(created.Title, "templated") {
		t.Errorf("title not templated: %q", created.Title)
	}
}

func TestBuildSpecsSkipsRetainedPattern(t *testing.T) {
	pattern := models.PatternAnalysis{Subject: "job", JobID: "job", Systemic: true, Confidence: "high"}
	details := []models.JobDetail{{JobID: "job", PatternRefresh: &models.PatternRefreshStatus{State: models.PatternRefreshRetained}}}
	specs := BuildSpecs(BuildInput{Report: models.FlakinessReport{RecurringPatterns: []models.PatternAnalysis{pattern}}, JobDetails: details, Triggers: []string{project.IssueTriggerPatterns}})
	if len(specs) != 0 {
		t.Fatalf("specs = %+v", specs)
	}
}

func TestManagerForgetDropsTransientBuildIssueState(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "issue-state.json")
	fake := newFakeGitHub(t)
	manager := NewManager(newTestClient(fake), stateFile, "o/r", Options{})
	manager.state.Tracked["build::one"] = TrackedIssue{Number: 1, URL: "https://example/1"}
	manager.Forget("build::one")
	if err := manager.SaveState(); err != nil {
		t.Fatal(err)
	}
	reloaded := NewManager(newTestClient(fake), stateFile, "o/r", Options{})
	if _, found := reloaded.state.Tracked["build::one"]; found {
		t.Fatal("transient build issue remained in persistent state")
	}
}

func TestCreateIssueMissingIdentityIsOutcomeUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	client := NewClient("token", "o", "r")
	client.apiBase = srv.URL
	client.httpClient = srv.Client()
	if _, _, err := client.CreateIssue(t.Context(), "title", "body", nil); !errors.Is(err, ErrWriteOutcomeUnknown) {
		t.Fatalf("missing identity error = %v", err)
	}
}
