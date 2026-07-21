package jobconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// fakeTestInfra hosts a stub of:
//   - GET /commits/master            (returns a fake commit SHA)
//   - GET /git/trees/<sha>?recursive=1 (returns a tree listing)
//   - GET /raw/<sha>/<path>          (returns the raw YAML body)
//
// files maps repo-relative paths under config/jobs/ to their raw YAML body.
// Anything in files is both included in the tree and downloadable.
type fakeTestInfra struct {
	files map[string]string

	// Knobs for testing failure paths:
	forceTruncated     bool     // tree response carries truncated=true
	forcedTreeStatus   int      // nonzero overrides tree response status
	forcedTreeBody     string   // body returned with forcedTreeStatus
	forcedCommitStatus int      // nonzero overrides /commits/master status
	failRawPath        string   // when set, returns 404 for this exact path
	extraTreeEntries   []string // extra paths emitted in the tree for coverage

	rawCalls atomic.Int64
}

const fakeSHA = "deadbeefcafef00d000000000000000000000000"

func (f *fakeTestInfra) start(t *testing.T) (rawURL, apiURL string, stop func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/raw/", func(w http.ResponseWriter, r *http.Request) {
		f.rawCalls.Add(1)
		// /raw/<sha>/<path>
		rest := strings.TrimPrefix(r.URL.Path, "/raw/")
		// strip the sha segment
		slash := strings.Index(rest, "/")
		if slash < 0 {
			http.NotFound(w, r)
			return
		}
		path := rest[slash+1:]
		if path == f.failRawPath {
			http.Error(w, "fake 404", http.StatusNotFound)
			return
		}
		body, ok := f.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/commits/master", func(w http.ResponseWriter, r *http.Request) {
		if f.forcedCommitStatus != 0 {
			w.WriteHeader(f.forcedCommitStatus)
			_, _ = w.Write([]byte("forced commit failure"))
			return
		}
		_, _ = w.Write([]byte(fakeSHA))
	})
	mux.HandleFunc("/api/git/trees/", func(w http.ResponseWriter, r *http.Request) {
		if f.forcedTreeStatus != 0 {
			w.WriteHeader(f.forcedTreeStatus)
			_, _ = w.Write([]byte(f.forcedTreeBody))
			return
		}
		type entry struct {
			Path string `json:"path"`
			Type string `json:"type"`
		}
		var entries []entry
		for path := range f.files {
			entries = append(entries, entry{Path: path, Type: "blob"})
		}
		for _, p := range f.extraTreeEntries {
			entries = append(entries, entry{Path: p, Type: "blob"})
		}
		out := map[string]any{
			"truncated": f.forceTruncated,
			"tree":      entries,
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	return srv.URL + "/raw/", srv.URL + "/api/", srv.Close
}

func setURLs(t *testing.T, raw, api string) {
	t.Helper()
	origRaw, origAPI := rawBaseURL, apiBaseURL
	rawBaseURL, apiBaseURL = raw, api
	t.Cleanup(func() {
		rawBaseURL, apiBaseURL = origRaw, origAPI
	})
}

func setToken(t *testing.T, token string) {
	t.Helper()
	prev, hadPrev := os.LookupEnv("GITHUB_TOKEN")
	if token == "" {
		_ = os.Unsetenv("GITHUB_TOKEN")
	} else {
		t.Setenv("GITHUB_TOKEN", token)
	}
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv("GITHUB_TOKEN", prev)
		} else {
			_ = os.Unsetenv("GITHUB_TOKEN")
		}
	})
}

func periodicJob(name, dashboard string) string {
	return fmt.Sprintf(`periodics:
- name: %s
  minimum_interval: 24h
  annotations:
    testgrid-dashboards: %s
    testgrid-tab-name: %s
  extra_refs:
  - org: o
    repo: r
    base_ref: main
`, name, dashboard, name)
}

func TestFetchJobConfigs_DiscoversAcrossDirectoriesAndNames(t *testing.T) {
	const dashboard = "sig-cluster-lifecycle-cluster-api-provider-azure"
	// Matching files may live in different directories with different prefixes.
	tf := &fakeTestInfra{files: map[string]string{
		"config/jobs/kubernetes-sigs/cluster-api-provider-azure/cluster-api-provider-azure-periodics-main.yaml": periodicJob("periodic-cluster-api-provider-azure-e2e", dashboard),
		"config/jobs/kubernetes/sig-scalability/sig-scalability-periodic-azure.yaml":                            periodicJob("ci-kubernetes-e2e-azure-scalability", dashboard),
		// Noise: matches the prefix/suffix filter but advertises a different dashboard.
		"config/jobs/kubernetes/sig-other/unrelated.yaml": periodicJob("unrelated", "some-other-dashboard"),
	}}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: dashboard}}
	jobs, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("FetchJobConfigs: %v", err)
	}
	if got := len(jobs); got != 2 {
		t.Fatalf("expected 2 dashboard-matching jobs, got %d (%+v)", got, jobs)
	}

	wantConfigFiles := map[string]string{
		"periodic-cluster-api-provider-azure-e2e": "config/jobs/kubernetes-sigs/cluster-api-provider-azure/cluster-api-provider-azure-periodics-main.yaml",
		"ci-kubernetes-e2e-azure-scalability":     "config/jobs/kubernetes/sig-scalability/sig-scalability-periodic-azure.yaml",
	}
	for _, j := range jobs {
		want, ok := wantConfigFiles[j.Name]
		if !ok {
			t.Errorf("unexpected job %q", j.Name)
			continue
		}
		if j.ConfigFile != want {
			t.Errorf("%s ConfigFile = %q, want %q", j.Name, j.ConfigFile, want)
		}
	}
}

func TestFetchJobConfigs_OnlyConfigJobsYAMLsAreDownloaded(t *testing.T) {
	const dashboard = "d"
	tf := &fakeTestInfra{
		files: map[string]string{
			"config/jobs/k/a.yaml": periodicJob("a", dashboard),
		},
		// Tree includes paths outside config/jobs/ and non-yaml files;
		// the tree filter must exclude them, so no raw call is made.
		extraTreeEntries: []string{
			"config/testgrids/foo.yaml",
			"prow/config.yaml",
			"config/jobs/k/README.md",
			"config/jobs/k/a.yaml.template",
		},
	}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: dashboard}}
	jobs, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("FetchJobConfigs: %v", err)
	}
	if got := len(jobs); got != 1 {
		t.Fatalf("expected 1 job, got %d", got)
	}
	if got := tf.rawCalls.Load(); got != 1 {
		t.Errorf("raw downloads = %d, want 1 (only the single config/jobs/*.yaml)", got)
	}
}

func TestFetchJobConfigs_TruncatedTreeIsError(t *testing.T) {
	tf := &fakeTestInfra{
		files:          map[string]string{"config/jobs/k/a.yaml": periodicJob("a", "d")},
		forceTruncated: true,
	}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: "d"}}
	_, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err == nil {
		t.Fatal("expected error for truncated tree, got nil")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error should mention truncated; got: %v", err)
	}
}

func TestFetchJobConfigs_TreeHTTPErrorSurfacesBody(t *testing.T) {
	tf := &fakeTestInfra{
		files:            map[string]string{},
		forcedTreeStatus: http.StatusForbidden,
		forcedTreeBody:   `{"message":"API rate limit exceeded"}`,
	}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: "d"}}
	_, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err == nil {
		t.Fatal("expected HTTP 403 to surface, got nil")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("error should include status and response body; got: %v", err)
	}
}

func TestFetchJobConfigs_CommitResolutionFails(t *testing.T) {
	tf := &fakeTestInfra{
		files:              map[string]string{},
		forcedCommitStatus: http.StatusServiceUnavailable,
	}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: "d"}}
	_, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err == nil {
		t.Fatal("expected error when commit resolution fails, got nil")
	}
	if !strings.Contains(err.Error(), "master SHA") {
		t.Errorf("error should mention master SHA resolution; got: %v", err)
	}
}

func TestFetchJobConfigs_RawDownloadFailureCancelsBatch(t *testing.T) {
	// One file 404s; the whole discovery must fail with a clear per-file
	// error rather than silently dropping that file.
	tf := &fakeTestInfra{
		files: map[string]string{
			"config/jobs/k/a.yaml": periodicJob("a", "d"),
			"config/jobs/k/b.yaml": periodicJob("b", "d"),
			"config/jobs/k/c.yaml": periodicJob("c", "d"),
		},
		failRawPath: "config/jobs/k/b.yaml",
	}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: "d"}}
	_, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err == nil {
		t.Fatal("expected error when a candidate file fails to download, got nil")
	}
	if !strings.Contains(err.Error(), "config/jobs/k/b.yaml") || !strings.Contains(err.Error(), "404") {
		t.Errorf("error should name the failing file and status; got: %v", err)
	}
}

func TestFetchJobConfigs_ZeroMatchErrors(t *testing.T) {
	// Tree returns files but none advertise the wanted dashboard.
	tf := &fakeTestInfra{files: map[string]string{
		"config/jobs/k/a.yaml": periodicJob("a", "other"),
	}}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)
	setToken(t, "fake-token")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: "wanted"}}
	_, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err == nil {
		t.Fatal("expected zero-match error, got nil")
	}
	if !strings.Contains(err.Error(), "no jobs labeled with dashboard") || !strings.Contains(err.Error(), `"wanted"`) {
		t.Errorf("error missing expected fragments: %v", err)
	}
}

func TestFetchJobConfigs_AnonymousIsAllowed(t *testing.T) {
	// GITHUB_TOKEN is optional, so a local dev with no env still works.
	tf := &fakeTestInfra{files: map[string]string{
		"config/jobs/k/a.yaml": periodicJob("a", "d"),
	}}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)
	setToken(t, "")

	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: "d"}}
	jobs, err := FetchJobConfigs(context.Background(), http.DefaultClient, cfg)
	if err != nil {
		t.Fatalf("FetchJobConfigs without token: %v", err)
	}
	if len(jobs) != 1 {
		t.Errorf("expected 1 job, got %d", len(jobs))
	}
}

func TestDerivePeriodicPrefix(t *testing.T) {
	cases := []struct {
		name string
		jobs []models.ProwJob
		want string
	}{
		{
			name: "majority shared prefix",
			jobs: []models.ProwJob{
				{Name: "periodic-cluster-api-provider-azure-e2e", JobType: models.JobTypePeriodic},
				{Name: "periodic-cluster-api-provider-azure-conformance", JobType: models.JobTypePeriodic},
				{Name: "periodic-cluster-api-provider-azure-upgrade", JobType: models.JobTypePeriodic},
				{Name: "ci-kubernetes-e2e-azure-scalability", JobType: models.JobTypePeriodic},
			},
			want: "periodic-cluster-api-provider-azure-",
		},
		{
			name: "no majority returns empty",
			jobs: []models.ProwJob{
				{Name: "periodic-foo-x", JobType: models.JobTypePeriodic},
				{Name: "periodic-bar-y", JobType: models.JobTypePeriodic},
			},
			want: "",
		},
		{
			name: "presubmits ignored",
			jobs: []models.ProwJob{
				{Name: "pull-foo-bar", JobType: models.JobTypePresubmit},
				{Name: "pull-foo-baz", JobType: models.JobTypePresubmit},
				{Name: "periodic-foo-bar", JobType: models.JobTypePeriodic},
				{Name: "periodic-foo-baz", JobType: models.JobTypePeriodic},
			},
			want: "periodic-foo-",
		},
		{
			name: "no periodics returns empty",
			jobs: []models.ProwJob{
				{Name: "pull-foo-bar", JobType: models.JobTypePresubmit},
			},
			want: "",
		},
		{
			name: "longest majority prefix wins",
			jobs: []models.ProwJob{
				{Name: "periodic-foo-bar-x", JobType: models.JobTypePeriodic},
				{Name: "periodic-foo-bar-y", JobType: models.JobTypePeriodic},
				{Name: "periodic-foo-bar-z", JobType: models.JobTypePeriodic},
			},
			want: "periodic-foo-bar-",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DerivePeriodicPrefix(tc.jobs); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFetchCatalogWithoutTargetKeepsOnlyDashboardJobs(t *testing.T) {
	const dashboard = "d"
	tf := &fakeTestInfra{files: map[string]string{
		"config/jobs/a.yaml": periodicJob("included", dashboard),
		"config/jobs/b.yaml": periodicJob("excluded", "other"),
	}}
	raw, api, stop := tf.start(t)
	defer stop()
	setURLs(t, raw, api)
	cfg := &project.Config{TestGrid: project.TestGrid{Dashboard: dashboard}}
	_, catalog, err := FetchJobConfigsAndCatalog(context.Background(), http.DefaultClient, cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Jobs) != 1 {
		t.Fatalf("catalog = %+v", catalog.Jobs)
	}
}
