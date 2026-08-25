package ai

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

// newLinkStub returns a server for file-existence HEAD checks and vanity
// `?go-get=1` meta lookups.
// exists is keyed by "/owner/repo/HEAD/path"; vanity is keyed by the module
// import path and maps to its GitHub repo URL.
func newLinkStub(t *testing.T, exists map[string]bool, vanity map[string]string) (*httptest.Server, *int32) {
	t.Helper()
	var goGetCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("go-get") == "1" {
			atomic.AddInt32(&goGetCalls, 1)
			mod := strings.TrimPrefix(r.URL.Path, "/")
			if repo, ok := vanity[mod]; ok {
				fmt.Fprintf(w, `<html><head><meta name="go-import"
					content="%s git %s"></head></html>`, mod, repo)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodHead {
			t.Errorf("file check expected HEAD, got %s", r.Method)
		}
		if exists[r.URL.Path] {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &goGetCalls
}

func withStub(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oraw, ogg := rawContentBase, goGetScheme
	rawContentBase = srv.URL
	goGetScheme = srv.URL + "/"
	t.Cleanup(func() { rawContentBase, goGetScheme = oraw, ogg })
}

// withRawBase points raw content checks at a stub without touching go-get.
func withRawBase(t *testing.T, base string) {
	t.Helper()
	old := rawContentBase
	rawContentBase = base
	t.Cleanup(func() { rawContentBase = old })
}

// TestResolveFileLinks_GenericResolution checks the generic resolver: project
// repo for repo-relative paths, vanity-import resolution via go-get, and
// owner/repo/path references, all gated on existence so broken links are
// dropped, with no project- or ecosystem-specific knowledge in the code.
func TestResolveFileLinks_GenericResolution(t *testing.T) {
	exists := map[string]bool{
		// Project repo source file.
		"/kubernetes-sigs/cluster-api-provider-azure/HEAD/azure/services/vm/spec.go": true,
		// Dot-named project dir must resolve to the project repo, not vanity.
		"/kubernetes-sigs/cluster-api-provider-azure/HEAD/.github/workflows/ci.yaml": true,
		// Upstream CAPI file reached through all supported reference forms.
		"/kubernetes-sigs/cluster-api/HEAD/test/framework/controlplane_helpers.go": true,
	}
	vanity := map[string]string{
		"sigs.k8s.io/cluster-api": "https://github.com/kubernetes-sigs/cluster-api",
	}
	srv, _ := newLinkStub(t, exists, vanity)
	withStub(t, srv)

	s := &Service{}
	s.sourceRepoOwner, s.sourceRepoName = "kubernetes-sigs", "cluster-api-provider-azure"

	tc := &models.TestCase{
		AISummary: &models.AISummary{Summary: "noise calico-system/calico-kube-controllers v1.34.8"},
		AIAnalysis: &models.AIAnalysis{
			RelevantFiles: []string{
				// Repo-relative path exists in the project repo.
				"azure/services/vm/spec.go",
				// Dot-named project dir resolves to the project repo.
				".github/workflows/ci.yaml",
				// Repo-relative path outside the project repo is dropped.
				"test/e2e/clusterctl_upgrade_test.go (lines 1-9)",
				// owner/repo/path GitHub form resolves to cluster-api.
				"kubernetes-sigs/cluster-api/test/framework/controlplane_helpers.go",
				// Explicit github.com/owner/repo/path resolves to cluster-api.
				"github.com/kubernetes-sigs/cluster-api/test/framework/controlplane_helpers.go",
			},
			// Vanity import path in prose resolves via go-get.
			RootCause:    "fails in sigs.k8s.io/cluster-api/test/framework/controlplane_helpers.go:42",
			SuggestedFix: "n/a",
		},
	}

	links := s.resolveFileLinks(context.Background(), srv.Client(), tc)

	want := map[string]string{
		"azure/services/vm/spec.go": "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/HEAD/azure/services/vm/spec.go",
		".github/workflows/ci.yaml": "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/HEAD/.github/workflows/ci.yaml",
		"kubernetes-sigs/cluster-api/test/framework/controlplane_helpers.go":            "https://github.com/kubernetes-sigs/cluster-api/blob/HEAD/test/framework/controlplane_helpers.go",
		"github.com/kubernetes-sigs/cluster-api/test/framework/controlplane_helpers.go": "https://github.com/kubernetes-sigs/cluster-api/blob/HEAD/test/framework/controlplane_helpers.go",
		"sigs.k8s.io/cluster-api/test/framework/controlplane_helpers.go":                "https://github.com/kubernetes-sigs/cluster-api/blob/HEAD/test/framework/controlplane_helpers.go",
	}
	for k, v := range want {
		if links[k] != v {
			t.Errorf("links[%q] = %q, want %q", k, links[k], v)
		}
	}
	if _, ok := links["test/e2e/clusterctl_upgrade_test.go"]; ok {
		t.Errorf("unverified path must be dropped")
	}
	if _, ok := links["calico-system/calico-kube-controllers"]; ok {
		t.Errorf("resource name must not be linked")
	}
	for k := range links {
		if strings.Contains(k, "(") {
			t.Errorf("link key %q should have annotation stripped", k)
		}
	}
}

// TestResolveFileLinks_CachesChecks ensures repeated file checks and go-get
// lookups are memoized across analyses in a run.
func TestResolveFileLinks_CachesChecks(t *testing.T) {
	exists := map[string]bool{"/o/r/HEAD/test/x.go": true}
	srv, _ := newLinkStub(t, exists, nil)
	withStub(t, srv)

	s := &Service{}
	s.sourceRepoOwner, s.sourceRepoName = "o", "r"
	var headCalls int32
	client := srv.Client()

	mk := func() *models.TestCase {
		return &models.TestCase{AIAnalysis: &models.AIAnalysis{RelevantFiles: []string{"test/x.go"}}}
	}
	_ = headCalls
	_ = client
	s.resolveFileLinks(context.Background(), srv.Client(), mk())
	before := s.cachedCount()
	s.resolveFileLinks(context.Background(), srv.Client(), mk())
	after := s.cachedCount()
	if before == 0 || after != before {
		t.Errorf("expected verification cached after first run (before=%d after=%d)", before, after)
	}
}

// cachedCount reports the number of memoized link checks (test helper).
func (s *Service) cachedCount() int {
	n := 0
	s.linkVerifyCache.Range(func(_, _ any) bool { n++; return true })
	return n
}

func TestResolveFileLinksAtExactBuildCommit(t *testing.T) {
	sha := strings.Repeat("a", 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/kubernetes-sigs/cluster-api-provider-azure/"+sha+"/test/e2e/capi_test.go" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	old := rawContentBase
	rawContentBase = srv.URL
	t.Cleanup(func() { rawContentBase = old })

	r := NewFileLinkResolver("kubernetes-sigs", "cluster-api-provider-azure")
	tc := &models.TestCase{AIAnalysis: &models.AIAnalysis{RelevantFiles: []string{"test/e2e/capi_test.go"}}}
	links := r.ResolveAtRef(context.Background(), srv.Client(), tc, sha)
	want := "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/" + sha + "/test/e2e/capi_test.go"
	if links["test/e2e/capi_test.go"] != want {
		t.Fatalf("links = %v, want %s", links, want)
	}
}

func TestResolveFileLinksAtExactBuildCommitVerifiesSearchSuggestions(t *testing.T) {
	sha := strings.Repeat("b", 40)
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		switch r.URL.Path {
		case "/kubernetes-sigs/cluster-api-provider-azure/" + sha + "/internal/asomigration/labels.go":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	old := rawContentBase
	rawContentBase = srv.URL
	t.Cleanup(func() { rawContentBase = old })

	r := NewFileLinkResolver("kubernetes-sigs", "cluster-api-provider-azure")
	tc := &models.TestCase{AIAnalysis: &models.AIAnalysis{SearchSuggestions: []string{
		"internal/asomigration/labels.go", "../unsafe.go", "missing/file.go", "internal/asomigration/labels.go",
	}}}
	links := r.ResolveAtRef(context.Background(), srv.Client(), tc, sha)
	want := "https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/" + sha + "/internal/asomigration/labels.go"
	if links["internal/asomigration/labels.go"] != want || len(links) != 1 {
		t.Fatalf("links=%v want=%s", links, want)
	}
	for _, path := range requested {
		if strings.Contains(path, "unsafe") {
			t.Fatalf("unsafe path was requested: %v", requested)
		}
	}
	if len(requested) != 2 || !strings.HasSuffix(requested[0], "/internal/asomigration/labels.go") || !strings.HasSuffix(requested[1], "/missing/file.go") {
		t.Fatalf("request order=%v", requested)
	}
}

// linkTestCase builds a cache-hit analysis citing one source path, as a fresh
// publication pass sees it before links are resolved.
func linkTestCase(paths ...string) *models.TestCase {
	return &models.TestCase{
		Name: "[It] creates a cluster",
		AIAnalysis: &models.AIAnalysis{
			GeneratedAt: "2026-08-16T07:11:06Z", RootCause: "cni rollout timed out", SuggestedFix: "n/a",
			Mode: AgenticMode, CritiquePassed: true, RelevantFiles: paths,
		},
	}
}

// TestVerifyGitHubFileAtRef_OnlyDefiniteAnswersArePersisted pins the tri-state
// contract: a rate limit or a 5xx leaves the question open and must not reach
// the durable store, while 2xx and 404 are definite. Every outcome is memoized
// for the run so a degraded endpoint is probed once, not once per citation.
func TestVerifyGitHubFileAtRef_OnlyDefiniteAnswersArePersisted(t *testing.T) {
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name      string
		status    int
		want      linkVerification
		persisted bool
	}{
		{name: "rate limited", status: http.StatusForbidden, want: linkUnverified},
		{name: "too many requests", status: http.StatusTooManyRequests, want: linkUnverified},
		{name: "server error", status: http.StatusInternalServerError, want: linkUnverified},
		{name: "bad gateway", status: http.StatusBadGateway, want: linkUnverified},
		{name: "absent", status: http.StatusNotFound, want: linkAbsent, persisted: true},
		{name: "present", status: http.StatusOK, want: linkPresent, persisted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&requests, 1)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			withRawBase(t, srv.URL)

			store := NewCache("")
			s := &Service{}
			s.sourceRepoOwner, s.sourceRepoName = "o", "r"
			s.linkVerifyStore = store

			for range 2 {
				if got := s.verifyGitHubFileAtRef(t.Context(), srv.Client(), "o", "r", sha, "test/e2e/cni.go"); got != tc.want {
					t.Fatalf("verification = %v, want %v", got, tc.want)
				}
			}
			if got := atomic.LoadInt32(&requests); got != 1 {
				t.Errorf("issued %d probes for one path, want 1", got)
			}
			key := LinkVerificationCacheKey("o", "r", sha, "test/e2e/cni.go")
			if _, persisted := store.LoadLinkVerification(key); persisted != tc.persisted {
				t.Errorf("persisted = %v, want %v", persisted, tc.persisted)
			}
		})
	}
}

// TestVerifyGitHubFileAtRef_TransportErrorIsUnverified covers a dial failure,
// which the old boolean contract reported as "file absent".
func TestVerifyGitHubFileAtRef_TransportErrorIsUnverified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := srv.Client()
	base := srv.URL
	srv.Close()
	withRawBase(t, base)

	sha := strings.Repeat("a", 40)
	store := NewCache("")
	s := &Service{}
	s.linkVerifyStore = store
	if got := s.verifyGitHubFileAtRef(t.Context(), client, "o", "r", sha, "test/e2e/cni.go"); got != linkUnverified {
		t.Fatalf("verification = %v, want linkUnverified", got)
	}
	if _, persisted := store.LoadLinkVerification(LinkVerificationCacheKey("o", "r", sha, "test/e2e/cni.go")); persisted {
		t.Errorf("transport failure must not be persisted")
	}
}

// TestResolveFileLinksAtRef_RecoversAfterUnverifiedPass checks that an
// inconclusive pass leaves no residue: the next pass re-probes and, once the
// answer is definite, the link is published and pinned.
func TestResolveFileLinksAtRef_RecoversAfterUnverifiedPass(t *testing.T) {
	sha := strings.Repeat("e", 40)
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withRawBase(t, srv.URL)

	store := NewCache("")
	resolve := func() map[string]string {
		r := NewFileLinkResolver("o", "r")
		r.SetVerificationStore(store)
		return r.ResolveAtRef(t.Context(), srv.Client(), linkTestCase("test/e2e/cni.go"), sha)
	}
	if links := resolve(); len(links) != 0 {
		t.Fatalf("links = %v, want none while verification is unavailable", links)
	}
	healthy.Store(true)
	want := "https://github.com/o/r/blob/" + sha + "/test/e2e/cni.go"
	if links := resolve(); links["test/e2e/cni.go"] != want {
		t.Fatalf("links = %v, want %s once verification recovers", links, want)
	}
}

// TestResolveFileLinksAtRef_RetainsPublishedLinkWhenUnverified covers reuse of
// an earlier result that already carries links: a path published at the same
// immutable revision survives a pass where GitHub cannot answer, while a
// verified absence still drops it.
func TestResolveFileLinksAtRef_RetainsPublishedLinkWhenUnverified(t *testing.T) {
	sha := strings.Repeat("b", 40)
	published := "https://github.com/o/r/blob/" + sha + "/test/e2e/cni.go"
	var status atomic.Int32
	status.Store(http.StatusForbidden)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	withRawBase(t, srv.URL)

	resolve := func() map[string]string {
		tc := linkTestCase("test/e2e/cni.go")
		tc.AIAnalysis.FileLinks = map[string]string{"test/e2e/cni.go": published}
		return NewFileLinkResolver("o", "r").ResolveAtRef(t.Context(), srv.Client(), tc, sha)
	}
	if links := resolve(); links["test/e2e/cni.go"] != published {
		t.Fatalf("links = %v, want the published link retained", links)
	}

	status.Store(http.StatusNotFound)
	if links := resolve(); len(links) != 0 {
		t.Fatalf("links = %v, want a verified absence to drop the link", links)
	}
}

// TestLoadLinkVerification_AbsenceExpires keeps a spurious 404 from hiding real
// evidence for the whole cache lifetime, while a verified presence stays pinned.
func TestLoadLinkVerification_AbsenceExpires(t *testing.T) {
	store := NewCache("")
	stale := time.Now().Add(-linkAbsenceTTL - time.Hour)
	for _, tc := range []struct {
		name    string
		key     string
		data    string
		present bool
		want    bool
	}{
		{name: "stale absence", key: "filelink:v1:absent", data: `{"present":false}`, want: false},
		{name: "stale presence", key: "filelink:v1:present", data: `{"present":true}`, present: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.StoreEntry(CacheEntry{Key: tc.key, CreatedAt: stale, Data: []byte(tc.data)}); err != nil {
				t.Fatal(err)
			}
			present, ok := store.LoadLinkVerification(tc.key)
			if ok != tc.want || present != tc.present {
				t.Fatalf("present=%v ok=%v, want present=%v ok=%v", present, ok, tc.present, tc.want)
			}
		})
	}
}

// TestResolveFileLinksAtRef_StableContentHashAcrossFlakyPass reproduces the
// reported failure: consecutive publications of one unchanged cache-hit
// analysis must publish the same links, and so the same content hash, even
// when GitHub stops answering between passes.
func TestResolveFileLinksAtRef_StableContentHashAcrossFlakyPass(t *testing.T) {
	sha := strings.Repeat("c", 40)
	var healthy atomic.Bool
	healthy.Store(true)
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withRawBase(t, srv.URL)

	store := NewCache(t.TempDir())
	// Each pass builds a fresh resolver, as a new publication pass does.
	publish := func() *models.TestCase {
		tc := linkTestCase("test/e2e/cni.go", "test/e2e/azure_test.go")
		r := NewFileLinkResolver("o", "r")
		r.SetVerificationStore(store)
		tc.AIAnalysis.FileLinks = r.ResolveAtRef(t.Context(), srv.Client(), tc, sha)
		return tc
	}

	first := publish()
	if len(first.AIAnalysis.FileLinks) != 2 {
		t.Fatalf("first pass links = %v, want both paths", first.AIAnalysis.FileLinks)
	}
	afterFirst := atomic.LoadInt32(&requests)

	healthy.Store(false)
	second := publish()
	if !maps.Equal(first.AIAnalysis.FileLinks, second.AIAnalysis.FileLinks) {
		t.Fatalf("links changed across passes: %v then %v", first.AIAnalysis.FileLinks, second.AIAnalysis.FileLinks)
	}
	if got, want := models.TestAnalysisContentHash(*second), models.TestAnalysisContentHash(*first); got != want {
		t.Fatalf("content hash = %s, want %s", got, want)
	}
	if got := atomic.LoadInt32(&requests); got != afterFirst {
		t.Errorf("second pass issued %d extra GitHub requests, want 0", got-afterFirst)
	}
}

// TestResolveFileLinksAtRef_DoesNotPersistMutableRefs keeps HEAD answers out of
// the durable store, where they could go stale as the branch moves.
func TestResolveFileLinksAtRef_DoesNotPersistMutableRefs(t *testing.T) {
	srv, _ := newLinkStub(t, map[string]bool{"/o/r/HEAD/test/x.go": true}, nil)
	withStub(t, srv)

	store := NewCache("")
	s := &Service{}
	s.sourceRepoOwner, s.sourceRepoName = "o", "r"
	s.linkVerifyStore = store
	if links := s.resolveFileLinks(t.Context(), srv.Client(), linkTestCase("test/x.go")); len(links) != 1 {
		t.Fatalf("links = %v, want the HEAD path resolved", links)
	}
	if len(store.EntriesWithPrefix(LinkVerificationKeyPrefix)) != 0 {
		t.Errorf("HEAD verification must not be persisted")
	}
}

// TestResolveFileLinks_DependencyStringsNeverResolveAgainstTheProject covers the
// collision route: a dependency path, or a repository slug that is itself
// path-shaped such as "nats-io/nats.go", must not be verified as a
// project-relative path. If it were, a project file that happens to sit at the
// same path would gain a verified link and become an actionable Fix source for
// a file the analysis never meant.
func TestResolveFileLinks_DependencyStringsNeverResolveAgainstTheProject(t *testing.T) {
	// Both collide with a real file in the project repository.
	exists := map[string]bool{
		"/o/r/HEAD/test/e2e/framework/pod.go": true,
		"/o/r/HEAD/nats-io/nats.go":           true,
		"/o/r/HEAD/pkg/project_owned.go":      true,
	}
	srv, _ := newLinkStub(t, exists, nil)
	withStub(t, srv)

	s := &Service{}
	s.sourceRepoOwner, s.sourceRepoName = "o", "r"
	tc := &models.TestCase{AIAnalysis: &models.AIAnalysis{
		RelevantFiles: []string{"pkg/project_owned.go"},
		RootCause:     "the dependency nats-io/nats.go mishandles test/e2e/framework/pod.go",
		SuggestedFix:  "Track the upstream change.",
		CauseLocation: &models.AnalysisCauseLocation{
			Repository: "nats-io/nats.go", External: true,
			Files: []string{"test/e2e/framework/pod.go"},
		},
	}}

	links := s.resolveFileLinks(context.Background(), srv.Client(), tc)

	for _, foreign := range []string{"test/e2e/framework/pod.go", "nats-io/nats.go"} {
		if link, ok := links[foreign]; ok {
			t.Errorf("dependency string %q was verified as a project path: %s", foreign, link)
		}
	}
	// A genuine project path in the same analysis still resolves.
	if links["pkg/project_owned.go"] == "" {
		t.Fatalf("project path lost its verified link: %v", links)
	}
}

// TestResolveFileLinks_DependencyExclusionSurvivesEquivalentSpellings stops an
// equivalent spelling of a dependency path from slipping past the exclusion.
// The pinned-revision resolver strips a "./" prefix itself, so an exclusion
// that compared raw strings would let that spelling through and verify the
// dependency path against the project.
func TestResolveFileLinks_DependencyExclusionSurvivesEquivalentSpellings(t *testing.T) {
	const shared = "test/e2e/framework/pod.go"
	sha := strings.Repeat("a", 40)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The project genuinely contains a file at the same relative path.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	withRawBase(t, srv.URL)

	for _, spelling := range []string{shared, "./" + shared, "Test/E2E/Framework/Pod.go"} {
		s := &Service{}
		s.sourceRepoOwner, s.sourceRepoName = "o", "r"
		tc := &models.TestCase{AIAnalysis: &models.AIAnalysis{
			RelevantFiles: []string{spelling},
			CauseLocation: &models.AnalysisCauseLocation{
				Repository: "nats-io/nats.go", External: true, Files: []string{shared},
			},
		}}
		for key, link := range s.resolveFileLinksAtRef(context.Background(), srv.Client(), tc, sha) {
			t.Errorf("spelling %q produced a project link: %s -> %s", spelling, key, link)
		}
	}

	// The same resolver still links a genuine project path in the same analysis.
	s := &Service{}
	s.sourceRepoOwner, s.sourceRepoName = "o", "r"
	tc := &models.TestCase{AIAnalysis: &models.AIAnalysis{
		RelevantFiles: []string{"pkg/project_owned.go"},
		CauseLocation: &models.AnalysisCauseLocation{
			Repository: "nats-io/nats.go", External: true, Files: []string{shared},
		},
	}}
	if links := s.resolveFileLinksAtRef(context.Background(), srv.Client(), tc, sha); links["pkg/project_owned.go"] == "" {
		t.Fatalf("project path lost its verified link: %v", links)
	}
}

// TestResolveFileLinks_VerifiedProjectPathOutranksAnExternalHint covers the
// contradictory case end to end. A path the analysis actually read from the
// project at the pinned revision was proven to be a project file, so it keeps
// its verified link; the model additionally listing it as a dependency file
// does not unproved that read, and the structured ownership is what reports the
// dependency.
func TestResolveFileLinks_VerifiedProjectPathOutranksAnExternalHint(t *testing.T) {
	const shared = "test/e2e/framework/pod.go"
	exists := map[string]bool{"/o/r/HEAD/" + shared: true}
	srv, _ := newLinkStub(t, exists, nil)
	withStub(t, srv)

	// normalizeCauseLocation drops a hint the project read proved, so the
	// published location no longer claims the path is foreign.
	location := normalizeCauseLocation(&models.AnalysisCauseLocation{
		Repository: "nats-io/nats.go", Files: []string{shared},
	}, "o", "r", []string{shared})

	s := &Service{}
	s.sourceRepoOwner, s.sourceRepoName = "o", "r"
	tc := &models.TestCase{AIAnalysis: &models.AIAnalysis{
		RelevantFiles: []string{shared}, CauseLocation: location,
	}}

	if links := s.resolveFileLinks(context.Background(), srv.Client(), tc); links[shared] == "" {
		t.Fatalf("verified project read lost its link to a contradictory hint: %v", links)
	}
	if location == nil || !location.External || len(location.Files) != 0 {
		t.Fatalf("published location = %+v, want external ownership with no contradictory hint", location)
	}
}

// TestResolveFileLinks_ProjectCauseKeepsItsLinks confirms the exclusion is
// scoped to dependency ownership and never suppresses an own-repo cause.
func TestResolveFileLinks_ProjectCauseKeepsItsLinks(t *testing.T) {
	exists := map[string]bool{"/o/r/HEAD/pkg/project_owned.go": true}
	srv, _ := newLinkStub(t, exists, nil)
	withStub(t, srv)

	s := &Service{}
	s.sourceRepoOwner, s.sourceRepoName = "o", "r"
	tc := &models.TestCase{AIAnalysis: &models.AIAnalysis{
		RelevantFiles: []string{"pkg/project_owned.go"},
		CauseLocation: &models.AnalysisCauseLocation{
			Repository: "o/r", Files: []string{"pkg/project_owned.go"},
		},
	}}

	if links := s.resolveFileLinks(context.Background(), srv.Client(), tc); links["pkg/project_owned.go"] == "" {
		t.Fatalf("project-owned cause lost its verified link: %v", links)
	}
}
