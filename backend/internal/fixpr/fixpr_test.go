package fixpr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

// fakeCompleter is the reviewer (critique) stand-in. Only the critique step
// calls Complete; an empty critique approves the change.
type fakeCompleter struct {
	critique    string // JSON {"issues":[...]}; empty -> approved
	critiqueErr error
}

func (f *fakeCompleter) Complete(_ context.Context, _, _ string) (string, error) {
	if f.critiqueErr != nil {
		return "", f.critiqueErr
	}
	if f.critique == "" {
		return `{"issues": []}`, nil
	}
	return f.critique, nil
}

// fakePR records OpenPR calls and serves a configurable SearchOpenPR result.
type fakePR struct {
	opened      []ghpr.Request
	openErr     error
	openURL     string
	searchURL   string
	searchFound bool
}

func (f *fakePR) OpenPR(_ context.Context, req ghpr.Request) (string, error) {
	f.opened = append(f.opened, req)
	if f.openErr != nil {
		return f.openURL, f.openErr
	}
	return "https://github.com/up/stream/pull/5", nil
}

func (f *fakePR) SearchOpenPR(_ context.Context, _, _, _, _ string) (int, string, bool, error) {
	if f.searchFound {
		return 5, f.searchURL, true, nil
	}
	return 0, "", false, nil
}

func (f *fakePR) ResolveBase(_ context.Context, _, _ string) (ghpr.Base, error) {
	return ghpr.Base{Branch: "main", HeadSHA: "pinned-sha-123", TreeSHA: "basetree"}, nil
}

const sampleFile = `apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster
spec:
  machineType: Standard_D2s_v3
  diskType: StandardSSD_LRS
`

func systemicPattern(subject string) models.PatternAnalysis {
	return models.PatternAnalysis{
		Subject:         subject,
		JobID:           "job-" + subject,
		Systemic:        true,
		Confidence:      "high",
		SharedRootCause: "etcd disk too slow on StandardSSD_LRS causing join timeouts",
		SuggestedFix:    "pin the control plane disk to Premium_LRS",
		Summary:         "Most builds fail joining etcd.",
		BuildsAnalyzed:  5,
	}
}

// newManager builds a Manager wired to a fake agent runtime (the fix generator)
// and an approving reviewer. Tests can override opts before Reconcile.
func newManager(t *testing.T, pr prClient, agent *fakeAgentRuntime, opts Options) *Manager {
	t.Helper()
	opts.SourceOwner, opts.SourceName = "up", "stream"
	if opts.MinConfidence == "" {
		opts.MinConfidence = "high"
	}
	if opts.MaxFiles == 0 {
		opts.MaxFiles = 2
	}
	if opts.MaxNewPerRun == 0 {
		opts.MaxNewPerRun = 1
	}
	if opts.AuthorName == "" {
		opts.AuthorName, opts.AuthorEmail = "Jane", "jane@example.com"
	}
	// Default to fork-and-PR; tests can flip m.opts.Fork for direct mode.
	opts.Fork = true
	opts.Agent = &AgentConfig{Runtime: agent, Model: "m", Endpoint: "e", ModelToken: "t", GitToken: "g"}
	// Default to review on with an approving reviewer; tests can override.
	if opts.Critique == nil {
		opts.Critique = &fakeCompleter{}
	}
	if opts.CritiqueRetries == 0 {
		opts.CritiqueRetries = 1
	}
	return NewManager(pr, filepath.Join(t.TempDir(), "state.json"), opts)
}

func TestReconcile_DirectModeWhenForkFalse(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodAgent(), Options{})
	m.opts.Fork = false // direct branch + same-repo PR (source repo you own)
	if _, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(pr.opened) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(pr.opened))
	}
	req := pr.opened[0]
	if req.Fork {
		t.Errorf("direct mode must not fork")
	}
	if !req.Draft || !req.SignOff {
		t.Errorf("fix PR should still be draft + signoff: %+v", req)
	}
}

func TestReconcile_OpensDraftForkPR(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodAgent(), Options{})
	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Proposed != 1 || len(pr.opened) != 1 {
		t.Fatalf("stats=%+v opened=%d, want 1 proposed", stats, len(pr.opened))
	}
	req := pr.opened[0]
	if !req.Fork || !req.Draft || !req.SignOff {
		t.Errorf("fix PR must be fork+draft+signoff: %+v", req)
	}
	if req.Owner != "up" || req.Repo != "stream" {
		t.Errorf("PR target = %s/%s, want up/stream", req.Owner, req.Repo)
	}
	if req.AuthorName != "Jane" || req.AuthorEmail != "jane@example.com" {
		t.Errorf("author = %s <%s>", req.AuthorName, req.AuthorEmail)
	}
	if !strings.Contains(req.Body, "prow-ai-dashboard-fix:") {
		t.Errorf("PR body missing dedup marker")
	}
}

func TestReconcile_DryRunWritesPreviewsNoPR(t *testing.T) {
	pr := &fakePR{}
	previewFile := filepath.Join(t.TempDir(), "fix_previews.json")
	m := newManager(t, pr, goodAgent(), Options{DryRun: true, PreviewFile: previewFile})
	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Previewed != 1 || stats.Proposed != 0 {
		t.Errorf("stats=%+v, want 1 previewed 0 proposed", stats)
	}
	if len(pr.opened) != 0 {
		t.Errorf("dry-run must not open a PR")
	}
	if _, err := os.Stat(previewFile); err != nil {
		t.Errorf("previews file not written: %v", err)
	}
}

func TestReconcile_SkipsTrackedAndAdoptsOpen(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodAgent(), Options{})
	p := systemicPattern("etcd")
	m.state.Tracked[keyFor(p)] = TrackedFix{URL: "x", OpenedAt: now()}
	stats, _ := m.Reconcile(context.Background(), []models.PatternAnalysis{p})
	if stats.Proposed != 0 || len(pr.opened) != 0 {
		t.Errorf("tracked pattern should be skipped: %+v", stats)
	}

	pr2 := &fakePR{searchFound: true, searchURL: "https://github.com/up/stream/pull/3"}
	m2 := newManager(t, pr2, goodAgent(), Options{})
	stats2, _ := m2.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if stats2.Adopted != 1 || len(pr2.opened) != 0 {
		t.Errorf("expected adopt without opening: %+v", stats2)
	}
}

func TestReconcile_FiltersIneligibleAndCap(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodAgent(), Options{MaxNewPerRun: 1})

	notSystemic := systemicPattern("a")
	notSystemic.Systemic = false
	noFix := systemicPattern("b")
	noFix.SuggestedFix = ""
	lowConf := systemicPattern("c")
	lowConf.Confidence = "low"
	good1 := systemicPattern("etcd")
	good2 := systemicPattern("webhook")
	good2.SharedRootCause = "different cause"

	stats, _ := m.Reconcile(context.Background(), []models.PatternAnalysis{notSystemic, noFix, lowConf, good1, good2})
	if stats.Proposed != 1 || len(pr.opened) != 1 {
		t.Errorf("expected exactly 1 proposal (cap), got %+v / %d", stats, len(pr.opened))
	}
}

func TestReconcile_PinsBaseAcrossReadAndCommit(t *testing.T) {
	pr := &fakePR{}
	fa := goodAgent()
	m := newManager(t, pr, fa, Options{})
	if _, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The agent was invoked at the pinned base SHA, and OpenPR received the same
	// base, so read and commit cannot straddle a mid-run push to the branch.
	if fa.spec.Repo.Ref != "pinned-sha-123" {
		t.Errorf("agent ran at ref %q, want pinned-sha-123", fa.spec.Repo.Ref)
	}
	if len(pr.opened) != 1 || pr.opened[0].Base == nil || pr.opened[0].Base.HeadSHA != "pinned-sha-123" {
		t.Errorf("OpenPR base = %+v, want HeadSHA pinned-sha-123", pr.opened[0].Base)
	}
}

func TestReconcile_PartialSuccessTracksAndCounts(t *testing.T) {
	pr := &fakePR{openErr: errors.New("labeling failed"), openURL: "https://github.com/up/stream/pull/9"}
	m := newManager(t, pr, goodAgent(), Options{})
	p := systemicPattern("etcd")
	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{p})
	if err == nil || !strings.Contains(err.Error(), "labeling failed") {
		t.Fatalf("Reconcile error = %v, want follow-up failure", err)
	}
	if stats.Proposed != 1 {
		t.Errorf("partial success should count: %+v", stats)
	}
	if _, tracked := m.state.Tracked[keyFor(p)]; !tracked {
		t.Errorf("partial-success PR should be tracked")
	}
}

func TestParseJSONObject_ToleratesLiteralTabsAndNewlines(t *testing.T) {
	// A model copying a code snippet verbatim emits literal tabs/newlines inside
	// the JSON string values, which strict JSON rejects. parseJSONObject must
	// recover by escaping them.
	raw := "{\"issues\": [\"func F() {\n\treturn\n}\"]}"
	var v struct {
		Issues []string `json:"issues"`
	}
	if err := parseJSONObject(raw, &v); err != nil {
		t.Fatalf("parseJSONObject: %v", err)
	}
	if len(v.Issues) != 1 || !strings.Contains(v.Issues[0], "return") {
		t.Errorf("parsed issues = %+v", v.Issues)
	}
}

func TestEscapeStringControlChars_LeavesStructureAndEscapes(t *testing.T) {
	// Structural whitespace between tokens is untouched; an already-escaped \n
	// is not double-escaped; a literal tab inside a string is escaped.
	in := "{\n\t\"k\": \"a\\nb\tc\"\n}"
	out := escapeStringControlChars(in)
	if !strings.Contains(out, `a\nb\tc`) {
		t.Errorf("escaped = %q", out)
	}
}

func TestGeneratedFixSnapshotRoundTrip(t *testing.T) {
	original := &GeneratedFix{
		Preview: Preview{
			Subject: "subject", Rationale: "why", Diff: "diff",
			Files:  map[string]string{"a.go": "package a"},
			Verify: VerifyResult{Status: VerifyPassed, Summary: "ok"},
		},
		Title: "title", Description: "description", Body: "body",
		pattern: models.PatternAnalysis{ID: "pattern", JobID: "job", Systemic: true},
		key:     "key", base: ghpr.Base{Branch: "main", HeadSHA: "head", TreeSHA: "tree"},
	}
	snapshot := original.Snapshot()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded GeneratedFixSnapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	restored := RestoreGeneratedFix(&decoded)
	if restored.Title != original.Title || restored.key != original.key || restored.base.HeadSHA != original.base.HeadSHA {
		t.Fatalf("restored fix = %+v", restored)
	}
	if restored.Preview.Files["a.go"] != "package a" || restored.pattern.ID != "pattern" {
		t.Fatalf("restored preview = %+v pattern=%+v", restored.Preview, restored.pattern)
	}
	restored.Preview.Files["a.go"] = "changed"
	if decoded.Files["a.go"] != "package a" {
		t.Fatal("restore did not deep copy files")
	}
}

func TestTrackedFixStoresPatternSnapshot(t *testing.T) {
	pattern := systemicPattern("etcd")
	fix := trackedFix("https://github.com/up/stream/pull/5", pattern)
	if fix.Pattern.JobID != pattern.JobID || fix.Pattern.SharedRootCause != pattern.SharedRootCause {
		t.Fatalf("tracked fix = %+v", fix)
	}
}

func TestTrackedFixHasPatternSnapshot(t *testing.T) {
	if (TrackedFix{}).HasPatternSnapshot() {
		t.Fatal("empty tracked fix must be unsupported")
	}
	if !(TrackedFix{Pattern: models.PatternAnalysis{JobID: "job"}}).HasPatternSnapshot() {
		t.Fatal("job snapshot must be supported")
	}
}

func TestNewManagerDiscardsStateWithoutPatternSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := statefile.State[TrackedFix]{
		Repo:    "up/stream",
		Tracked: map[string]TrackedFix{"legacy": {URL: "https://github.com/up/stream/pull/1"}},
	}
	if err := legacy.Save(path); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(&fakePR{}, path, Options{SourceOwner: "up", SourceName: "stream"})
	if len(manager.state.Tracked) != 0 {
		t.Fatalf("tracked = %+v", manager.state.Tracked)
	}
}
