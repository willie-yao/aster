package ai

import (
	"context"
	"strings"
	"testing"
)

// fakeRepoReader is a tools.RepoReader test double over a fixed file set.
type fakeRepoReader struct {
	files map[string]string
	calls int
}

func (r *fakeRepoReader) ListTree(ctx context.Context) ([]string, error) {
	r.calls++
	out := make([]string, 0, len(r.files))
	for p := range r.files {
		out = append(out, p)
	}
	return out, nil
}

func (r *fakeRepoReader) ReadFile(ctx context.Context, path string) (string, bool, error) {
	body, ok := r.files[path]
	return body, ok, nil
}

func TestAnnotateUnverifiedPaths(t *testing.T) {
	exists := func(p string) bool {
		return p == "templates/test/ci/cluster-template-prow-azl3.yaml" ||
			p == "cluster-template-prow-azl3.yaml"
	}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "real path untouched",
			in:   "Edit templates/test/ci/cluster-template-prow-azl3.yaml to raise the timeout.",
			want: "Edit templates/test/ci/cluster-template-prow-azl3.yaml to raise the timeout.",
		},
		{
			name: "fabricated path annotated",
			in:   "Update templates/cluster-template-azure-linux.yaml to fix it.",
			want: "Update templates/cluster-template-azure-linux.yaml (unverified path) to fix it.",
		},
		{
			name: "bare fabricated filename annotated",
			in:   "Update cluster-template-azure-linux.yaml to fix it.",
			want: "Update cluster-template-azure-linux.yaml (unverified path) to fix it.",
		},
		{
			name: "prose without paths untouched",
			in:   "The etcd join hits a context deadline exceeded during scale-up.",
			want: "The etcd join hits a context deadline exceeded during scale-up.",
		},
		{
			name: "annotates each distinct fake path once",
			in:   "See a/b/one.go and a/b/one.go and c/two.yaml.",
			want: "See a/b/one.go (unverified path) and a/b/one.go (unverified path) and c/two.yaml (unverified path).",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := annotateUnverifiedPaths(tc.in, exists); got != tc.want {
				t.Errorf("annotateUnverifiedPaths:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestGuardPatternPaths_GroundedTree verifies the guard flags a path missing
// from the repo tree while leaving a real one intact, using the tree branch of
// patternPathVerifier.
func TestGuardPatternPaths_GroundedTree(t *testing.T) {
	s := &Service{
		patternRepo: &fakeRepoReader{files: map[string]string{
			"templates/test/ci/cluster-template-prow-azl3.yaml": "kind: Cluster",
			"test/e2e/azure_test.go":                            "package e2e",
		}},
	}
	p := &patternResponse{
		Systemic:        true,
		SharedRootCause: "The azl3 flavor deadlocks on etcd join.",
		SuggestedFix:    "Raise the timeout in templates/cluster-template-azure-linux.yaml or the azl3 input in test/e2e/azure_test.go.",
		Summary:         "8/10 builds share the cause.",
	}
	s.guardPatternPaths(context.Background(), p)

	if !strings.Contains(p.SuggestedFix, "cluster-template-azure-linux.yaml (unverified path)") {
		t.Errorf("expected fabricated path annotated, got: %q", p.SuggestedFix)
	}
	if strings.Contains(p.SuggestedFix, "azure_test.go (unverified path)") {
		t.Errorf("real path must not be annotated, got: %q", p.SuggestedFix)
	}
}

// TestGuardPatternPaths_NoRepo is a no-op when nothing can verify.
func TestGuardPatternPaths_NoRepo(t *testing.T) {
	s := &Service{}
	orig := "Fix templates/cluster-template-azure-linux.yaml."
	p := &patternResponse{SuggestedFix: orig}
	s.guardPatternPaths(context.Background(), p)
	if p.SuggestedFix != orig {
		t.Errorf("guard must be a no-op without a repo; got: %q", p.SuggestedFix)
	}
}

// TestPatternRepoTree_MemoizedOnce verifies the tree is listed once per run.
func TestPatternRepoTree_MemoizedOnce(t *testing.T) {
	fake := &fakeRepoReader{files: map[string]string{"a.go": "x"}}
	s := &Service{patternRepo: fake}
	for i := 0; i < 3; i++ {
		if _, err := s.patternRepoTree(context.Background()); err != nil {
			t.Fatalf("patternRepoTree: %v", err)
		}
	}
	if fake.calls != 1 {
		t.Errorf("ListTree calls = %d, want 1 (memoized)", fake.calls)
	}
}

// TestAnalyzePattern_GroundedToolLoop runs the grounded branch end-to-end: the
// model calls a repo tool, then returns a verdict that names one real and one
// fabricated path. The loop must complete and the guard must annotate only the
// fabricated path.
func TestAnalyzePattern_GroundedToolLoop(t *testing.T) {
	shrinkCallDelay(t)
	srv := newScriptedChatServer(t)
	// Round 1: investigate the repo. Round 2: final verdict.
	srv.push(200, chatRespToolCall("call_1", "list_repo_tree", map[string]interface{}{"path": ""}))
	verdict := `{"systemic":true,"confidence":"high","shared_root_cause":"azl3 etcd-join deadlock",` +
		`"shared_builds":["abuild","bbuild"],` +
		`"suggested_fix":"Raise the timeout in templates/cluster-template-azure-linux.yaml or test/e2e/azure_test.go.",` +
		`"summary":"most builds share the cause"}`
	srv.push(200, chatRespFinal(verdict))

	client := newAgenticTestClient(t, srv.URL)
	s := NewService(client, &stubModule{name: "kubernetes"}, "sys", nil)
	s.SetSourceRepo("willie-yao", "cluster-api-provider-azure")
	s.SetPatternRepoReader(&fakeRepoReader{files: map[string]string{
		"test/e2e/azure_test.go": "package e2e",
	}})

	pa, err := s.AnalyzePattern(context.Background(), "job", "the-job", patternFailures(2))
	if err != nil {
		t.Fatalf("AnalyzePattern: %v", err)
	}
	if pa == nil || !pa.Systemic {
		t.Fatalf("expected systemic verdict, got %+v", pa)
	}
	if !strings.Contains(pa.SuggestedFix, "cluster-template-azure-linux.yaml (unverified path)") {
		t.Errorf("fabricated path should be annotated, got: %q", pa.SuggestedFix)
	}
	if strings.Contains(pa.SuggestedFix, "azure_test.go (unverified path)") {
		t.Errorf("real path must not be annotated, got: %q", pa.SuggestedFix)
	}
}
