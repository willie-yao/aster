package fixpr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ghpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

// fakeCompleter routes by system prompt: locate, edit, or critique.
type fakeCompleter struct {
	locate      string
	edit        string
	editSeq     []string // when set, returns one entry per edit call, in order
	critique    string   // JSON {"issues":[...]}; empty -> approved
	locateErr   error
	editErr     error
	critiqueErr error
	editCalls   int
}

func (f *fakeCompleter) Complete(_ context.Context, system, _ string) (string, error) {
	switch system {
	case locateSystemPrompt:
		return f.locate, f.locateErr
	case critiqueSystemPrompt:
		if f.critiqueErr != nil {
			return "", f.critiqueErr
		}
		if f.critique == "" {
			return `{"issues": []}`, nil
		}
		return f.critique, nil
	default: // editSystemPrompt
		idx := f.editCalls
		f.editCalls++
		if len(f.editSeq) > 0 {
			if idx >= len(f.editSeq) {
				idx = len(f.editSeq) - 1
			}
			return f.editSeq[idx], f.editErr
		}
		return f.edit, f.editErr
	}
}

// fakeSource serves canned file content and records the ref it was read at.
type fakeSource struct {
	files   map[string]string
	lastRef string
}

func (s *fakeSource) FileContent(_ context.Context, _, _, ref, path string) (string, bool, error) {
	s.lastRef = ref
	c, ok := s.files[path]
	return c, ok, nil
}

func (s *fakeSource) ListTree(_ context.Context, _, _, _ string) ([]string, error) {
	paths := make([]string, 0, len(s.files))
	for p := range s.files {
		paths = append(paths, p)
	}
	return paths, nil
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

// ---- generation ----

// genParamsFor builds genParams for generation-only tests (review disabled).
func genParamsFor(c Completer, src sourceReader) genParams {
	return genParams{completer: c, source: src, owner: "o", repo: "r", ref: "ref", maxFiles: 2}
}

func TestGenerateFix_HappyPath(t *testing.T) {
	c := &fakeCompleter{
		locate: `{"files": ["templates/cluster.yaml"]}`,
		edit:   `{"rationale": "use a faster disk", "edits": [{"file": "templates/cluster.yaml", "old": "diskType: StandardSSD_LRS", "new": "diskType: Premium_LRS"}]}`,
	}
	src := &fakeSource{files: map[string]string{"templates/cluster.yaml": sampleFile}}

	fix, err := generateFix(context.Background(), genParamsFor(c, src), systemicPattern("etcd"))
	if err != nil {
		t.Fatalf("generateFix: %v", err)
	}
	got := fix.files["templates/cluster.yaml"]
	if !strings.Contains(got, "diskType: Premium_LRS") || strings.Contains(got, "StandardSSD_LRS") {
		t.Errorf("edit not applied: %q", got)
	}
	if !strings.Contains(fix.diff, "- ") || !strings.Contains(fix.diff, "+ ") {
		t.Errorf("diff not rendered: %q", fix.diff)
	}
}

func TestGenerateFix_RejectsHallucinatedFile(t *testing.T) {
	// The model picks a path that is not among the real candidate files.
	c := &fakeCompleter{locate: `{"files": ["does/not/exist.yaml"]}`}
	src := &fakeSource{files: map[string]string{"templates/cluster.yaml": sampleFile}}
	if _, err := generateFix(context.Background(), genParamsFor(c, src), systemicPattern("etcd")); err == nil || !strings.Contains(err.Error(), "not a real repo file") {
		t.Errorf("expected hallucinated-file rejection, got %v", err)
	}
}

func TestGenerateFix_NoCandidates(t *testing.T) {
	c := &fakeCompleter{}
	src := &fakeSource{files: map[string]string{}}
	if _, err := generateFix(context.Background(), genParamsFor(c, src), systemicPattern("etcd")); err == nil || !strings.Contains(err.Error(), "no candidate") {
		t.Errorf("expected no-candidate error, got %v", err)
	}
}

func TestGenerateFix_DeclinesWhenFixIsUpstream(t *testing.T) {
	// The analysis pointed the fix at upstream Kubernetes source that does not
	// exist in this repo. The repo has an unrelated file so candidates are
	// non-empty, but the model correctly selects none. The error must name the
	// upstream target rather than the vague "no candidate file fit".
	src := &fakeSource{files: map[string]string{
		"test/e2e/conformance_test.go": "package e2e\n",
	}}
	p := systemicPattern("dra device plugin registration race")
	p.SuggestedFix = "add a poll in test/e2e/dra/dra.go before creating tester pods"
	p.RelevantFiles = []string{
		"k8s.io/kubernetes/test/e2e/dra/dra.go", // upstream .go, not in repo
		"pkg/kubelet/cm/dra/manager/manager.go", // upstream, not in repo
		"build-log.txt",                         // artifact, ignored
	}
	// Model honestly selects no in-repo file.
	c := &fakeCompleter{locate: `{"files": []}`}
	_, err := generateFix(context.Background(), genParamsFor(c, src), p)
	if err == nil {
		t.Fatal("expected a decline error")
	}
	if !strings.Contains(err.Error(), "outside") || !strings.Contains(err.Error(), "dra.go") {
		t.Errorf("expected an upstream-decline message naming the external file, got %v", err)
	}
}

func TestExternalSourceFiles(t *testing.T) {
	tree := []string{"test/e2e/conformance_test.go", "config/a.yaml"}
	relevant := []string{
		"k8s.io/kubernetes/test/e2e/dra/dra.go",                // external source
		"pkg/kubelet/cm/dra/manager/manager.go",                // external source
		"sigs.k8s.io/cluster-api-provider-azure/config/a.yaml", // resolves in-repo -> excluded
		"test/e2e/conformance_test.go",                         // in-repo -> excluded
		"artifacts/foo/dump.yaml",                              // artifact -> excluded
		"build-log.txt",                                        // log -> excluded
	}
	got := externalSourceFiles(relevant, tree)
	want := []string{"k8s.io/kubernetes/test/e2e/dra/dra.go", "pkg/kubelet/cm/dra/manager/manager.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGenerateFix_NoEditsRetriesThenSucceeds(t *testing.T) {
	// The model returns an empty edit set first, then a real edit after the
	// feedback nudge. critiqueRetries must allow at least one retry.
	c := &fakeCompleter{
		locate: `{"files": ["templates/cluster.yaml"]}`,
		editSeq: []string{
			`{"rationale": "n/a", "edits": []}`,
			`{"rationale": "use a faster disk", "edits": [{"file": "templates/cluster.yaml", "old": "diskType: StandardSSD_LRS", "new": "diskType: Premium_LRS"}]}`,
		},
	}
	src := &fakeSource{files: map[string]string{"templates/cluster.yaml": sampleFile}}
	gp := genParamsFor(c, src)
	gp.critiqueRetries = 1

	fix, err := generateFix(context.Background(), gp, systemicPattern("etcd"))
	if err != nil {
		t.Fatalf("generateFix: %v", err)
	}
	if !strings.Contains(fix.files["templates/cluster.yaml"], "Premium_LRS") {
		t.Errorf("retry edit not applied: %q", fix.files["templates/cluster.yaml"])
	}
	if c.editCalls != 2 {
		t.Errorf("edit calls = %d, want 2 (initial + retry)", c.editCalls)
	}
}

func TestGenerateFix_NoEditsExhaustsToClearError(t *testing.T) {
	// Always-empty edits: after retries, a clear not-auto-fixable error.
	c := &fakeCompleter{
		locate: `{"files": ["templates/cluster.yaml"]}`,
		edit:   `{"rationale": "n/a", "edits": []}`,
	}
	src := &fakeSource{files: map[string]string{"templates/cluster.yaml": sampleFile}}
	gp := genParamsFor(c, src)
	gp.critiqueRetries = 1

	_, err := generateFix(context.Background(), gp, systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "could not produce a code change") {
		t.Errorf("expected clear not-auto-fixable error, got %v", err)
	}
}

func TestGenerateFix_RejectsTooManyFiles(t *testing.T) {
	c := &fakeCompleter{locate: `{"files": ["templates/a.yaml", "templates/b.yaml", "templates/c.yaml"]}`}
	src := &fakeSource{files: map[string]string{
		"templates/a.yaml": "x", "templates/b.yaml": "y", "templates/c.yaml": "z",
	}}
	if _, err := generateFix(context.Background(), genParamsFor(c, src), systemicPattern("etcd")); err == nil || !strings.Contains(err.Error(), "max_files") {
		t.Errorf("expected max_files error, got %v", err)
	}
}

func TestRankCandidates_FiltersAndPrefers(t *testing.T) {
	tree := []string{
		"templates/test/ci/cluster-template-prow-azl3.yaml", // preferred dir + ext
		"vendor/foo/bar.go",      // no signal -> excluded
		"docs/proposals/etcd.md", // keyword but penalized dir
		"README.md",              // no signal
	}
	got := rankCandidates(tree, systemicPattern("etcd"))
	if len(got) == 0 || got[0] != "templates/test/ci/cluster-template-prow-azl3.yaml" {
		t.Errorf("expected the template path ranked first, got %v", got)
	}
	for _, p := range got {
		if strings.HasPrefix(p, "vendor/") {
			t.Errorf("vendor path should be filtered out: %v", got)
		}
	}
}

func TestRankCandidates_ExtensionAloneExcluded(t *testing.T) {
	// Paths with a matching extension but no keyword and no preferred dir must
	// not be admitted, so a weak keyword set can't flood the candidate list with
	// arbitrary files.
	tree := []string{"random/unrelated/file.go", "another/thing.yaml"}
	if got := rankCandidates(tree, systemicPattern("etcd")); len(got) != 0 {
		t.Errorf("extension-only paths should be excluded, got %v", got)
	}
}

func TestRepoRelevantFiles_MapsToRealPaths(t *testing.T) {
	tree := []string{
		"test/e2e/azure_apiversion_upgrade_test.go",
		"test/e2e/clusterctl_upgrade_test.go",
		"test/e2e/config/azure-dev.yaml",
		"main.go",
	}
	// Mirrors what an analysis actually implicates: real repo files, vendored/
	// prefixed paths, upstream module paths, and artifact logs.
	relevant := []string{
		"test/e2e/azure_apiversion_upgrade_test.go",                                  // exact
		"sigs.k8s.io/cluster-api-provider-azure/test/e2e/clusterctl_upgrade_test.go", // prefixed -> resolves
		"test/e2e/config/azure-dev.yaml",                                             // exact
		"sigs.k8s.io/cluster-api/test/framework/clusterctl/client.go",                // upstream, no repo match
		"artifacts/clusters/bootstrap/logs/manager.log",                              // artifact, dropped
		"build-log.txt", // dropped
		"/home/prow/go/pkg/mod/sigs.k8s.io/cluster-api/test@v1.13.3/framework/x.go", // dropped
	}
	got := repoRelevantFiles(relevant, tree)
	want := map[string]bool{
		"test/e2e/azure_apiversion_upgrade_test.go": true,
		"test/e2e/clusterctl_upgrade_test.go":       true,
		"test/e2e/config/azure-dev.yaml":            true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %d real paths", got, len(want))
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected path %q in %v", g, got)
		}
	}
}

func TestRepoRelevantFiles_RejectsBareBasenameSuffix(t *testing.T) {
	// A vendored path must not resolve to an unrelated root-level file by bare
	// basename; only a >=2-segment suffix (or an exact match) counts.
	tree := []string{"client.go", "test/e2e/clusterctl_upgrade_test.go"}
	relevant := []string{
		"sigs.k8s.io/cluster-api/test/framework/clusterctl/client.go", // must NOT match root client.go
		"client.go", // exact repo-relative match is fine
	}
	got := repoRelevantFiles(relevant, tree)
	if len(got) != 1 || got[0] != "client.go" {
		t.Errorf("expected only the exact client.go match, got %v", got)
	}
}

func TestRepoRelevantFiles_NormalizesDotSlash(t *testing.T) {
	// A "./main.go" prefix must still match the root file main.go exactly.
	got := repoRelevantFiles([]string{"./main.go"}, []string{"main.go"})
	if len(got) != 1 || got[0] != "main.go" {
		t.Errorf("expected ./main.go to resolve to main.go, got %v", got)
	}
}

func TestGenerateFix_DeclinesUpstreamWhenNoCandidates(t *testing.T) {
	// No in-repo file even ranks (empty candidate set) and the analysis named
	// only external source. The early return must still give the clear upstream
	// decline, not the generic "no candidate files in the repo" message.
	src := &fakeSource{files: map[string]string{"README.md": "# x\n"}}
	p := systemicPattern("dra race")
	p.RelevantFiles = []string{"k8s.io/kubernetes/test/e2e/dra/dra.go"}
	c := &fakeCompleter{} // locate never called
	_, err := generateFix(context.Background(), genParamsFor(c, src), p)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("expected an upstream-decline message, got %v", err)
	}
}

func TestMergeCandidates_SeedsFirstDeduped(t *testing.T) {
	seeds := []string{"a.go", "b.go"}
	ranked := []string{"b.go", "c.go"} // b.go overlaps
	got := mergeCandidates(seeds, ranked)
	want := []string{"a.go", "b.go", "c.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q (%v)", i, got[i], want[i], got)
		}
	}
}

func TestGenerateFix_SeedsRelevantFilesWhenRankingMisses(t *testing.T) {
	// Keyword ranking would miss this file (its name shares no keyword with the
	// failure), but the analysis named it. Seeding must surface it so the locate
	// step can pick it instead of failing with "no candidate file fit".
	target := "test/e2e/azure_apiversion_upgrade_test.go"
	src := &fakeSource{files: map[string]string{
		target:          "package e2e\n\nfunc TestUpgrade() { doThing() }\n",
		"README.md":     "# readme\n",
		"vendor/x/y.go": "package x\n",
	}}
	p := systemicPattern("aso webhook readiness race")
	p.SharedRootCause = "clusterctl upgrade races the ASO conversion webhook lifecycle"
	p.SuggestedFix = "wait for the webhook endpoints before tearing down the old provider"
	p.RelevantFiles = []string{
		"sigs.k8s.io/cluster-api-provider-azure/" + target, // resolves to target
		"artifacts/clusters/bootstrap/logs/manager.log",    // dropped
	}
	c := &fakeCompleter{
		locate: `{"files": ["test/e2e/azure_apiversion_upgrade_test.go"]}`,
		edit:   `{"rationale": "guard the webhook", "edits": [{"file": "test/e2e/azure_apiversion_upgrade_test.go", "old": "doThing()", "new": "waitForWebhook(); doThing()"}]}`,
	}
	fix, err := generateFix(context.Background(), genParamsFor(c, src), p)
	if err != nil {
		t.Fatalf("generateFix: %v", err)
	}
	if _, ok := fix.files[target]; !ok {
		t.Errorf("expected an edit to %q, got %v", target, fix.files)
	}
}

func TestValidateSyntax(t *testing.T) {
	ok := []map[string]string{
		{"a.yaml": "key: val\n"},
		{"a.yaml": "a: 1\n---\nb: 2\n"}, // multi-doc
		{"a.go": "package x\n\nfunc F() {}\n"},
		{"a.json": `{"k": 1}`},
		{"a.sh": "this is $(not validated"}, // no validator -> skipped
	}
	for _, f := range ok {
		if err := validateSyntax(f); err != nil {
			t.Errorf("validateSyntax(%v) unexpected error: %v", f, err)
		}
	}
	bad := []struct{ file, content, want string }{
		{"a.yaml", "diskType: [unclosed\n", "not valid YAML"},
		{"a.go", "package x\nfunc F( {}\n", "not valid Go"},
		{"a.json", `{"k": }`, "not valid JSON"},
	}
	for _, c := range bad {
		err := validateSyntax(map[string]string{c.file: c.content})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("validateSyntax(%s) = %v, want %q", c.file, err, c.want)
		}
	}
}

func TestGenerateFix_DropsBrokenSyntax(t *testing.T) {
	// The edit turns valid YAML into an unclosed flow sequence.
	c := &fakeCompleter{
		locate: `{"files": ["templates/cluster.yaml"]}`,
		edit:   `{"rationale": "break it", "edits": [{"file": "templates/cluster.yaml", "old": "diskType: StandardSSD_LRS", "new": "diskType: [unclosed"}]}`,
	}
	src := &fakeSource{files: map[string]string{"templates/cluster.yaml": sampleFile}}
	if _, err := generateFix(context.Background(), genParamsFor(c, src), systemicPattern("etcd")); err == nil || !strings.Contains(err.Error(), "not valid YAML") {
		t.Errorf("expected broken-YAML drop, got %v", err)
	}
}

func TestCheckGo(t *testing.T) {
	// Valid, stdlib-only Go passes.
	if err := checkGo("a.go", "package x\n\nimport \"fmt\"\n\nfunc F() { fmt.Println(\"hi\") }\n"); err != nil {
		t.Errorf("valid Go unexpected error: %v", err)
	}
	// A definite type error in a fully resolved file fails.
	if err := checkGo("a.go", "package x\n\nfunc F() { var n int = \"str\"; _ = n }\n"); err == nil || !strings.Contains(err.Error(), "type error") {
		t.Errorf("expected a type error, got %v", err)
	}
	// An unused import (file no longer compiles) is a definite error.
	if err := checkGo("a.go", "package x\n\nimport \"fmt\"\n\nfunc F() {}\n"); err == nil {
		t.Errorf("expected an unused-import error")
	}
	// A syntax error is still caught.
	if err := checkGo("a.go", "package x\nfunc F( {}\n"); err == nil || !strings.Contains(err.Error(), "not valid Go") {
		t.Errorf("expected a parse error, got %v", err)
	}

	// Inconclusive cases must be skipped (return nil) to never false-drop a good
	// fix for lack of package/dependency context.
	skipCases := map[string]string{
		"unresolved external import":  "package x\n\nimport \"k8s.io/does/not/exist\"\n\nfunc F() { exist.Bar() }\n",
		"undefined sibling symbol":    "package x\n\nfunc F() { helperFromSibling() }\n",
		"method on a sibling type":    "package x\n\ntype T struct{}\n\nfunc F() { T{}.M() }\n",
		"sibling interface satisfier": "package x\n\ntype I interface{ M() }\ntype T struct{}\n\nvar _ I = T{}\n",
		"sibling const array length":  "package x\n\ntype A [N]int\n",
		"ambiguous promoted selector": "package x\n\ntype A struct{ M int }\ntype B struct{ M int }\ntype T struct {\n\tA\n\tB\n}\n\nfunc F(t T) { _ = t.M }\n",
		"non-stdlib import disables":  "package x\n\nimport \"k8s.io/foo\"\n\nfunc F() { var n int = \"s\"; _ = n; foo.Bar() }\n",
		"build-tagged file skipped":   "//go:build linux\n\npackage x\n\nfunc F() { var n int = \"s\"; _ = n }\n",
	}
	for name, src := range skipCases {
		if err := checkGo("a.go", src); err != nil {
			t.Errorf("%s should be skipped, got %v", name, err)
		}
	}

	// A GOOS/GOARCH filename suffix is an implicit build constraint, so the file
	// is parse-only even with a blatant type error (it may target another GOOS).
	if err := checkGo("scope_linux.go", "package x\n\nfunc F() { var n int = \"s\"; _ = n }\n"); err != nil {
		t.Errorf("platform-suffixed file should be skipped, got %v", err)
	}
}

func TestHasFilenameConstraint(t *testing.T) {
	cases := map[string]bool{
		"client_linux.go":        true,
		"client_amd64.go":        true,
		"client_linux_amd64.go":  true,
		"client_windows_test.go": true,
		"client.go":              false,
		"endpoint.go":            false,
		"cluster_scope.go":       false,
		"client_test.go":         false,
		"linux.go":               false,
	}
	for name, want := range cases {
		if got := hasFilenameConstraint(name); got != want {
			t.Errorf("hasFilenameConstraint(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestGenerateFix_DropsTypeError(t *testing.T) {
	goFile := "package x\n\nfunc Reconcile() int { return wait(30) }\n\nfunc wait(s int) int { return s }\n"
	c := &fakeCompleter{
		locate: `{"files": ["controllers/r.go"]}`,
		// The edit makes wait() receive a string where an int is required.
		edit: `{"rationale": "tune", "edits": [{"file": "controllers/r.go", "old": "return wait(30)", "new": "return wait(\"30\")"}]}`,
	}
	src := &fakeSource{files: map[string]string{"controllers/r.go": goFile}}
	if _, err := generateFix(context.Background(), genParamsFor(c, src), systemicPattern("etcd")); err == nil || !strings.Contains(err.Error(), "type error") {
		t.Errorf("expected a type-error drop, got %v", err)
	}
}

// ---- critique loop ----

func critiqueParams(c Completer, src sourceReader, retries int) genParams {
	gp := genParamsFor(c, src)
	gp.critique = c
	gp.critiqueRetries = retries
	return gp
}

func TestGenerateFix_CritiqueApproves(t *testing.T) {
	c := goodCompleter() // critique defaults to {"issues": []}
	src := goodSource()
	fix, err := generateFix(context.Background(), critiqueParams(c, src, 1), systemicPattern("etcd"))
	if err != nil {
		t.Fatalf("generateFix: %v", err)
	}
	if !strings.Contains(fix.files["templates/cluster.yaml"], "Premium_LRS") {
		t.Errorf("approved fix not applied")
	}
}

func TestGenerateFix_CritiqueRejectsThenDrops(t *testing.T) {
	c := goodCompleter()
	c.critique = `{"issues": ["the new value is wrong"]}` // always rejects
	src := goodSource()
	_, err := generateFix(context.Background(), critiqueParams(c, src, 1), systemicPattern("etcd"))
	if err == nil || !strings.Contains(err.Error(), "rejected by review") {
		t.Errorf("expected review rejection, got %v", err)
	}
	// One initial attempt + one retry = 2 edit calls.
	if c.editCalls != 2 {
		t.Errorf("editCalls = %d, want 2 (initial + 1 retry)", c.editCalls)
	}
}

func TestGenerateFix_CritiqueDisabledSkipsReview(t *testing.T) {
	c := goodCompleter()
	c.critique = `{"issues": ["would reject if asked"]}`
	src := goodSource()
	// retries 0 -> review skipped, fix accepted despite the would-be issues.
	if _, err := generateFix(context.Background(), critiqueParams(c, src, 0), systemicPattern("etcd")); err != nil {
		t.Errorf("with review disabled the fix should be accepted, got %v", err)
	}
}

func TestGenerateFix_CritiqueErrorFailsClosed(t *testing.T) {
	c := goodCompleter()
	c.critiqueErr = errors.New("review endpoint down")
	src := goodSource()
	if _, err := generateFix(context.Background(), critiqueParams(c, src, 1), systemicPattern("etcd")); err == nil || !strings.Contains(err.Error(), "review failed") {
		t.Errorf("a review error should drop the fix (fail closed), got %v", err)
	}
}

func TestApplyEdits_AnchorNotFound(t *testing.T) {
	_, err := applyEdits(map[string]string{"f": "hello"}, []edit{{File: "f", Old: "absent", New: "x"}}, 2)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected anchor-not-found, got %v", err)
	}
}

func TestApplyEdits_AmbiguousAnchor(t *testing.T) {
	_, err := applyEdits(map[string]string{"f": "x x"}, []edit{{File: "f", Old: "x", New: "y"}}, 2)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected ambiguous-anchor, got %v", err)
	}
}

func TestApplyEdits_NoEffectiveChange(t *testing.T) {
	_, err := applyEdits(map[string]string{"f": "a"}, []edit{{File: "f", Old: "a", New: "a"}}, 2)
	if err == nil || !strings.Contains(err.Error(), "no effective change") {
		t.Errorf("expected no-effective-change, got %v", err)
	}
}

func TestApplyEdits_OnlyReturnsChanged(t *testing.T) {
	orig := map[string]string{"a": "line1\nline2\nline3\n", "b": "two"}
	out, err := applyEdits(orig, []edit{{File: "a", Old: "line2", New: "changed"}}, 2)
	if err != nil {
		t.Fatalf("applyEdits: %v", err)
	}
	if len(out) != 1 || !strings.Contains(out["a"], "changed") {
		t.Errorf("expected only changed file a, got %v", out)
	}
}

func TestApplyEdits_RejectsWholeFileRewrite(t *testing.T) {
	_, err := applyEdits(map[string]string{"f": "the entire file"}, []edit{{File: "f", Old: "the entire file", New: "totally new"}}, 2)
	if err == nil || !strings.Contains(err.Error(), "whole file") {
		t.Errorf("expected whole-file-rewrite rejection, got %v", err)
	}
}

func TestApplyEdits_RejectsOversizedChange(t *testing.T) {
	big := strings.Repeat("newline\n", maxChangedLinesTotal+5)
	orig := "anchor-here\n" + strings.Repeat("x\n", 200)
	_, err := applyEdits(map[string]string{"f": orig}, []edit{{File: "f", Old: "anchor-here", New: big}}, 2)
	if err == nil || !strings.Contains(err.Error(), "exceeds the cap") {
		t.Errorf("expected oversized-change rejection, got %v", err)
	}
}

// ---- reconciler ----

func newManager(t *testing.T, pr prClient, c Completer, src sourceReader, opts Options) *Manager {
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
	// Default to review on with the same completer; tests can override.
	if opts.Critique == nil {
		opts.Critique = c
	}
	if opts.CritiqueRetries == 0 {
		opts.CritiqueRetries = 1
	}
	return NewManager(pr, c, src, filepath.Join(t.TempDir(), "state.json"), opts)
}

func goodCompleter() *fakeCompleter {
	return &fakeCompleter{
		locate: `{"files": ["templates/cluster.yaml"]}`,
		edit:   `{"rationale": "faster disk", "edits": [{"file": "templates/cluster.yaml", "old": "diskType: StandardSSD_LRS", "new": "diskType: Premium_LRS"}]}`,
	}
}

func goodSource() *fakeSource {
	return &fakeSource{files: map[string]string{"templates/cluster.yaml": sampleFile}}
}

func TestReconcile_DirectModeWhenForkFalse(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{})
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
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{})
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
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{DryRun: true, PreviewFile: previewFile})
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
	// Tracked: no PR.
	pr := &fakePR{}
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{})
	p := systemicPattern("etcd")
	m.state.Tracked[keyFor(p)] = TrackedFix{URL: "x", OpenedAt: now()}
	stats, _ := m.Reconcile(context.Background(), []models.PatternAnalysis{p})
	if stats.Proposed != 0 || len(pr.opened) != 0 {
		t.Errorf("tracked pattern should be skipped: %+v", stats)
	}

	// Open PR found via search: adopt, no new PR.
	pr2 := &fakePR{searchFound: true, searchURL: "https://github.com/up/stream/pull/3"}
	m2 := newManager(t, pr2, goodCompleter(), goodSource(), Options{})
	stats2, _ := m2.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")})
	if stats2.Adopted != 1 || len(pr2.opened) != 0 {
		t.Errorf("expected adopt without opening: %+v", stats2)
	}
}

func TestReconcile_FiltersIneligibleAndCap(t *testing.T) {
	pr := &fakePR{}
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{MaxNewPerRun: 1})

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
	src := goodSource()
	m := newManager(t, pr, goodCompleter(), src, Options{})
	if _, err := m.Reconcile(context.Background(), []models.PatternAnalysis{systemicPattern("etcd")}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The file was read at the pinned base SHA, and OpenPR received that same base.
	if src.lastRef != "pinned-sha-123" {
		t.Errorf("file read at ref %q, want pinned-sha-123", src.lastRef)
	}
	if len(pr.opened) != 1 || pr.opened[0].Base == nil || pr.opened[0].Base.HeadSHA != "pinned-sha-123" {
		t.Errorf("OpenPR base = %+v, want HeadSHA pinned-sha-123", pr.opened[0].Base)
	}
}

func TestReconcile_PartialSuccessTracksAndCounts(t *testing.T) {
	pr := &fakePR{openErr: errors.New("labeling failed"), openURL: "https://github.com/up/stream/pull/9"}
	m := newManager(t, pr, goodCompleter(), goodSource(), Options{})
	p := systemicPattern("etcd")
	stats, err := m.Reconcile(context.Background(), []models.PatternAnalysis{p})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stats.Proposed != 1 {
		t.Errorf("partial success should count: %+v", stats)
	}
	if _, tracked := m.state.Tracked[keyFor(p)]; !tracked {
		t.Errorf("partial-success PR should be tracked")
	}
}

func TestParseJSONObject_ToleratesLiteralTabsAndNewlines(t *testing.T) {
	// A model copying a Go snippet verbatim emits literal tabs/newlines inside
	// the JSON string values, which strict JSON rejects. parseJSONObject must
	// recover by escaping them.
	raw := "{\"edits\": [{\"file\": \"a.go\", \"old\": \"func F() {\n\treturn\n}\", \"new\": \"func F() {\n\treturn nil\n}\"}]}"
	var v struct {
		Edits []edit `json:"edits"`
	}
	if err := parseJSONObject(raw, &v); err != nil {
		t.Fatalf("parseJSONObject: %v", err)
	}
	if len(v.Edits) != 1 || !strings.Contains(v.Edits[0].New, "return nil") {
		t.Errorf("parsed edits = %+v", v.Edits)
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
