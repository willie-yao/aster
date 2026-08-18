package fixpr

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ghpr"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/statefile"
)

// fakeCompleter is the reviewer (critique) stand-in. Only the critique step
// calls Complete; an empty critique approves the change.
type fakeCompleter struct {
	critique    string // JSON {"issues":[...]}; empty -> approved
	critiqueErr error
	lastSystem  string
	lastUser    string
}

func (f *fakeCompleter) Complete(_ context.Context, system, user string) (string, error) {
	f.lastSystem, f.lastUser = system, user
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
	opened         []ghpr.Request
	openErr        error
	openURL        string
	searchURL      string
	searchFound    bool
	searchAnyCalls int
	base           ghpr.Base
	bases          []ghpr.Base
	resolveCalls   int
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

func (f *fakePR) SearchAnyPR(ctx context.Context, owner, repo, token, marker string) (int, string, bool, error) {
	f.searchAnyCalls++
	return f.SearchOpenPR(ctx, owner, repo, token, marker)
}

func (f *fakePR) ResolveBase(_ context.Context, _, _ string) (ghpr.Base, error) {
	if len(f.bases) > 0 {
		index := f.resolveCalls
		if index >= len(f.bases) {
			index = len(f.bases) - 1
		}
		f.resolveCalls++
		return f.bases[index], nil
	}
	if f.base.HeadSHA != "" {
		return f.base, nil
	}
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
// and an approving reviewer. Tests can override opts before generating.
func newManager(t *testing.T, pr prClient, agent *fakeAgentRuntime, opts Options) *Manager {
	t.Helper()
	opts.SourceOwner, opts.SourceName = "up", "stream"
	if opts.MinConfidence == "" {
		opts.MinConfidence = "high"
	}
	if opts.MaxFiles == 0 {
		opts.MaxFiles = 2
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

// openPreview drafts a fix for one pattern and opens exactly that preview,
// the path both the dashboard action and the chat fix flow take.
func openPreview(t *testing.T, m *Manager, p models.PatternAnalysis) {
	t.Helper()
	generated, err := m.GeneratePreview(context.Background(), p, "")
	if err != nil {
		t.Fatalf("GeneratePreview: %v", err)
	}
	if _, err := m.OpenFromPreview(context.Background(), generated); err != nil {
		t.Fatalf("OpenFromPreview: %v", err)
	}
}

func TestOpenFromPreview_DirectModeWhenForkFalse(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodAgent(), Options{})
	m.opts.Fork = false // direct branch + same-repo PR (source repo you own)
	openPreview(t, m, systemicPattern("etcd"))

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

func TestOpenFromPreview_OpensDraftForkPR(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodAgent(), Options{})
	openPreview(t, m, systemicPattern("etcd"))

	if len(pr.opened) != 1 {
		t.Fatalf("opened=%d, want 1", len(pr.opened))
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

func TestEligible_RejectsUnactionablePatterns(t *testing.T) {
	notSystemic := systemicPattern("a")
	notSystemic.Systemic = false
	noFix := systemicPattern("b")
	noFix.SuggestedFix = ""
	lowConf := systemicPattern("c")
	lowConf.Confidence = "low"

	for name, pattern := range map[string]models.PatternAnalysis{
		"not systemic":     notSystemic,
		"no suggested fix": noFix,
		"low confidence":   lowConf,
	} {
		if Eligible(pattern, "high") {
			t.Errorf("%s pattern was eligible", name)
		}
	}
	if !Eligible(systemicPattern("etcd"), "high") {
		t.Error("systemic high-confidence pattern with a fix was not eligible")
	}
}

func TestGeneratePreview_PinsBaseAcrossReadAndCommit(t *testing.T) {
	pr := &fakePR{}
	fa := goodAgent()
	m := newManager(t, pr, fa, Options{})
	openPreview(t, m, systemicPattern("etcd"))

	// The agent was invoked at the pinned base SHA, and OpenPR received the same
	// base, so read and commit cannot straddle a mid-run push to the branch.
	if fa.spec.Repo.Ref != "pinned-sha-123" {
		t.Errorf("agent ran at ref %q, want pinned-sha-123", fa.spec.Repo.Ref)
	}
	if len(pr.opened) != 1 || pr.opened[0].Base == nil || pr.opened[0].Base.HeadSHA != "pinned-sha-123" {
		t.Errorf("OpenPR base = %+v, want HeadSHA pinned-sha-123", pr.opened[0].Base)
	}
}

func TestOpenFromPreview_PartialSuccessStillTracks(t *testing.T) {
	pr := &fakePR{openErr: errors.New("labeling failed"), openURL: "https://github.com/up/stream/pull/9"}
	m := newManager(t, pr, goodAgent(), Options{})
	p := systemicPattern("etcd")

	generated, err := m.GeneratePreview(context.Background(), p, "")
	if err != nil {
		t.Fatalf("GeneratePreview: %v", err)
	}
	// The PR exists, so a failed follow-up (labeling) must not fail the open or
	// lose the tracking entry that prevents a duplicate PR next time.
	url, err := m.OpenFromPreview(context.Background(), generated)
	if err != nil {
		t.Fatalf("OpenFromPreview error = %v, want the opened PR reported", err)
	}
	if url != pr.openURL {
		t.Errorf("url = %q, want %q", url, pr.openURL)
	}
	if _, tracked := m.state.Tracked[KeyFor(p)]; !tracked {
		t.Errorf("partial-success PR should be tracked")
	}
}

func TestParseReviewIssues_ToleratesLiteralTabsAndNewlines(t *testing.T) {
	// A model copying a code snippet verbatim emits literal tabs/newlines inside
	// the JSON string values, which strict JSON rejects. parseReviewIssues must
	// recover by escaping them.
	raw := "{\"issues\": [\"func F() {\n\treturn\n}\"]}"
	issues, err := parseReviewIssues(raw)
	if err != nil {
		t.Fatalf("parseReviewIssues: %v", err)
	}
	if len(issues) != 1 || !strings.Contains(issues[0], "return") {
		t.Errorf("parsed issues = %+v", issues)
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
	fix := trackedGeneratedFix("https://github.com/up/stream/pull/5", &GeneratedFix{pattern: pattern})
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

func TestGeneratePreviewWithContextPassesSelectedEvidence(t *testing.T) {
	agent := goodAgent()
	reviewer := &fakeCompleter{}
	manager := newManager(t, &fakePR{}, agent, Options{Critique: reviewer, CritiqueRetries: 1})
	generationContext := validGenerationContext()
	fix, err := manager.GeneratePreviewWithContext(t.Context(), systemicPattern("etcd"), "keep compatibility", generationContext)
	if err != nil {
		t.Fatal(err)
	}
	if fix == nil || !strings.Contains(agent.spec.Instruction, `"assistant_answer":"The controller keeps retrying after bootstrap fails."`) ||
		!strings.Contains(agent.spec.Instruction, "Maintainer instruction (follow it): keep compatibility") {
		t.Fatalf("agent instruction = %q", agent.spec.Instruction)
	}
	if !strings.Contains(reviewer.lastUser, `"assistant_answer":"The controller keeps retrying after bootstrap fails."`) {
		t.Fatalf("review context = %q", reviewer.lastUser)
	}
}

func TestGeneratePreviewWithContextRejectsInvalidContextBeforeGeneration(t *testing.T) {
	agent := goodAgent()
	manager := newManager(t, &fakePR{}, agent, Options{})
	generationContext := validGenerationContext()
	generationContext.ArtifactCitations = nil
	if _, err := manager.GeneratePreviewWithContext(t.Context(), systemicPattern("etcd"), "", generationContext); err == nil {
		t.Fatal("invalid context was accepted")
	}
	if agent.spec.Instruction != "" {
		t.Fatal("agent ran before context validation")
	}
}

func TestParseJSONObjectSelectsFinalValidReviewObject(t *testing.T) {
	raw := `The change includes code like if err != nil { return retry() }.
First draft: {"issues":["stale concern"]}
Final answer:
` + "```json\n" + `{"issues":[],"provider_note":"review complete"}` + "\n```"
	issues, err := parseReviewIssues(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestParseJSONObjectHandlesBracesInsideStrings(t *testing.T) {
	raw := `reasoning {not JSON} then {"issues":["check map[string]any{\"key\": \"value\"}"]}`
	issues, err := parseReviewIssues(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !strings.Contains(issues[0], "map[string]") {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestCritiqueAgentFixRequiresIssuesField(t *testing.T) {
	completer := &fakeCompleter{critique: `{}`}
	_, err := critiqueAgentFix(t.Context(), completer, systemicPattern("etcd"), map[string]string{"a.go": "package a\n"}, "diff", nil)
	if err == nil || !strings.Contains(err.Error(), "issues field is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseReviewIssuesFallsThroughInvalidOuterWrapper(t *testing.T) {
	raw := `reasoning { final: {"issues":[]} }`
	issues, err := parseReviewIssues(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestParseReviewIssuesPrefersValidOuterResponse(t *testing.T) {
	raw := `{"issues":["outer issue contains {nested text}"]}`
	issues, err := parseReviewIssues(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0] != "outer issue contains {nested text}" {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestParseReviewIssuesIgnoresQuotedProseBrace(t *testing.T) {
	raw := `The code checks strings.Contains(s, "{"). Final: {"issues":[]}`
	issues, err := parseReviewIssues(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestParseReviewIssuesOrdersEscapedDraftByOriginalOffset(t *testing.T) {
	raw := "{\"issues\":[\"" + strings.Repeat("\n", 50) + "earlier\"]}\n{\"issues\":[]}"
	issues, err := parseReviewIssues(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("earlier escaped draft won: %#v", issues)
	}
}

func TestParseReviewIssuesRejectsOversizedResponse(t *testing.T) {
	raw := strings.Repeat("x", maxReviewResponseBytes+1) + `{"issues":[]}`
	if _, err := parseReviewIssues(raw); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
}

func TestReviewJSONCandidatesCapsVerboseBraceOutput(t *testing.T) {
	raw := strings.Repeat("{not-json}", maxReviewCandidates+20) + `{"issues":[]}`
	candidates := reviewJSONCandidates(raw)
	if len(candidates) != maxReviewCandidates {
		t.Fatalf("candidate count = %d", len(candidates))
	}
	issues, err := parseReviewIssues(raw)
	if err != nil || len(issues) != 0 {
		t.Fatalf("issues=%v err=%v", issues, err)
	}
}

func TestGenerateBuildPreviewUsesRepositoryEvidenceWithoutPatternSemantics(t *testing.T) {
	agent := goodAgent()
	manager := newManager(t, &fakePR{}, agent, Options{})
	generated, err := manager.GenerateBuildPreview(t.Context(), BuildFailure{
		ID: "build-id", JobID: "periodic-aks", JobName: "periodic-aks", BuildID: "123",
		RootCause:     "K8sVersionNotSupported rejected Kubernetes 1.33.2.",
		SuggestedFix:  "Update the AKS version selection.",
		RelevantFiles: []string{"templates/aks.yaml"}, SourceFiles: []string{"templates/aks.yaml"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if generated.pattern.ID != "" || generated.pattern.Systemic {
		t.Fatalf("build fix manufactured a pattern: %+v", generated.pattern)
	}
	if !strings.Contains(agent.spec.Instruction, "single CI build") || !strings.Contains(agent.spec.Instruction, "templates/aks.yaml") {
		t.Fatalf("build instruction = %s", agent.spec.Instruction)
	}
	if strings.Contains(strings.ToLower(generated.Body), "recurring failure") {
		t.Fatalf("build PR body claimed recurrence: %s", generated.Body)
	}
	if _, err := manager.OpenFromPreview(t.Context(), generated); err != nil {
		t.Fatal(err)
	}
	manager.Forget(generated.key)
	if err := manager.SaveState(); err != nil {
		t.Fatal(err)
	}
	reloaded := NewManager(&fakePR{}, manager.stateFile, manager.opts)
	if _, found := reloaded.state.Tracked[generated.key]; found {
		t.Fatal("build fix leaked into persistent pattern state")
	}
}

func TestGenerateBuildPreviewRejectsExternalOnlyRemediation(t *testing.T) {
	manager := newManager(t, &fakePR{}, goodAgent(), Options{})
	_, err := manager.GenerateBuildPreview(t.Context(), BuildFailure{
		ID: "build-id", JobID: "job", JobName: "job", BuildID: "1", RootCause: "external outage", SuggestedFix: "wait for provider",
	}, "")
	if err == nil || !strings.Contains(err.Error(), "verified local path") {
		t.Fatalf("external-only error = %v", err)
	}
}

func TestBuildFixAdoptsMarkerFromClosedPR(t *testing.T) {
	pr := &fakePR{searchFound: true, searchURL: "https://github.com/up/stream/pull/closed"}
	manager := newManager(t, pr, goodAgent(), Options{})
	generated, err := manager.GenerateBuildPreview(t.Context(), BuildFailure{
		ID: "build-id", JobID: "job", JobName: "job", BuildID: "1", RootCause: "cause", SuggestedFix: "fix",
		RelevantFiles: []string{"templates/cluster.yaml"}, SourceFiles: []string{"templates/cluster.yaml"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	url, err := manager.OpenFromPreview(t.Context(), generated)
	if err != nil {
		t.Fatal(err)
	}
	if url != pr.searchURL || pr.searchAnyCalls != 1 || len(pr.opened) != 0 {
		t.Fatalf("closed PR adoption url=%q any=%d opened=%d", url, pr.searchAnyCalls, len(pr.opened))
	}
}

func TestBuildFixReportsAmbiguousPRCreate(t *testing.T) {
	pr := &fakePR{openErr: ghpr.ErrWriteOutcomeUnknown}
	manager := newManager(t, pr, goodAgent(), Options{})
	generated, err := manager.GenerateBuildPreview(t.Context(), BuildFailure{
		ID: "build-id", JobID: "job", JobName: "job", BuildID: "1", RootCause: "cause", SuggestedFix: "fix",
		RelevantFiles: []string{"templates/cluster.yaml"}, SourceFiles: []string{"templates/cluster.yaml"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.OpenFromPreview(t.Context(), generated); !errors.Is(err, ErrWriteOutcomeUnknown) {
		t.Fatalf("ambiguous PR error = %v", err)
	}
}

func TestGeneratePreviewRejectsUnsafeAdmissionConversionClaimBeforeAgent(t *testing.T) {
	agent := goodAgent()
	manager := newManager(t, &fakePR{}, agent, Options{})
	pattern := systemicPattern("conversion")
	pattern.SuggestedFix = "Delete the ASO mutating and validating webhook configurations so CRD conversion no longer calls ASO."
	pattern.RemediationTargets = []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "getPreUpgradeFunc",
		RequiredCall: "example/asomigration.DeleteWebhookConfigurations", Path: "test/e2e/capi_test.go",
	}}
	if _, err := manager.GeneratePreview(t.Context(), pattern, ""); err == nil {
		t.Fatal("unsafe conversion recommendation was accepted")
	}
	if agent.spec.Instruction != "" {
		t.Fatal("agent ran before remediation policy")
	}
}
