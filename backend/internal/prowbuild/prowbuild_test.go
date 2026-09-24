package prowbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/buildsource"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/storage"
)

// fakeBackend is an in-memory storage.Backend for testing the Prow-layout
// logic without HTTP.
type fakeBackend struct {
	objects     map[string]string
	listErr     error
	listTreeErr error
	listCalls   []string
	listErrors  map[string]error
	openErrors  map[string]error
}

func (f *fakeBackend) Open(_ context.Context, path string) (io.ReadCloser, int64, error) {
	if err := f.openErrors[path]; err != nil {
		return nil, 0, err
	}
	body, ok := f.objects[path]
	if !ok {
		return nil, 0, storage.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(body)), int64(len(body)), nil
}

func (f *fakeBackend) ReadRange(_ context.Context, path string, offset, length int64) ([]byte, int64, error) {
	body, ok := f.objects[path]
	if !ok {
		return nil, 0, io.EOF
	}
	if offset >= int64(len(body)) {
		return nil, int64(len(body)), nil
	}
	end := offset + length
	if end > int64(len(body)) {
		end = int64(len(body))
	}
	return []byte(body[offset:end]), int64(len(body)), nil
}

func (f *fakeBackend) ReadTail(_ context.Context, path string, maxBytes int64) ([]byte, int64, error) {
	body, ok := f.objects[path]
	if !ok {
		return nil, 0, io.EOF
	}
	if int64(len(body)) > maxBytes {
		return []byte(body[int64(len(body))-maxBytes:]), int64(len(body)), nil
	}
	return []byte(body), int64(len(body)), nil
}

func (f *fakeBackend) List(_ context.Context, prefix string) (*storage.Listing, error) {
	f.listCalls = append(f.listCalls, prefix)
	if err := f.listErrors[prefix]; err != nil {
		return nil, err
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	dirs := map[string]bool{}
	files := map[string]bool{}
	for name := range f.objects {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		sub := strings.TrimPrefix(name, prefix)
		if sub == "" {
			continue
		}
		if i := strings.Index(sub, "/"); i >= 0 {
			dirs[sub[:i+1]] = true
		} else {
			files[sub] = true
		}
	}
	out := &storage.Listing{}
	for d := range dirs {
		out.Dirs = append(out.Dirs, d)
	}
	for fl := range files {
		out.Files = append(out.Files, storage.Object{Name: fl})
	}
	sort.Strings(out.Dirs)
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	return out, nil
}

func TestDiscoverExactJobsUsesDirectIndexes(t *testing.T) {
	b := &fakeBackend{objects: map[string]string{
		"logs/periodic-a/1/started.json":                    "x",
		"logs/unrelated/1/started.json":                     "x",
		"pr-logs/directory/pull-e2e/9.txt":                  "pr-logs/pull/example_project/3/pull-e2e/9",
		"pr-logs/directory/unrelated-presubmit/10.txt":      "pr-logs/pull/example_project/4/unrelated-presubmit/10",
		"pr-logs/pull/example_project/3/pull-e2e/9/prowjob": "x",
	}}
	jobs, err := DiscoverExactJobs(context.Background(), b, true, []string{"periodic-a", "pull-e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].Name != "periodic-a" || jobs[0].JobType != models.JobTypePeriodic ||
		jobs[1].Name != "pull-e2e" || jobs[1].JobType != models.JobTypePresubmit || jobs[1].Repo != "example/project" {
		t.Fatalf("exact jobs = %+v", jobs)
	}
	for _, prefix := range b.listCalls {
		if prefix == "logs/" || prefix == "pr-logs/directory/" {
			t.Fatalf("exact discovery enumerated bucket root %q", prefix)
		}
	}
}

func TestDiscoverExactJobsRejectsMissingName(t *testing.T) {
	b := &fakeBackend{objects: map[string]string{"logs/present/1/started.json": "x"}}
	_, err := DiscoverExactJobs(context.Background(), b, false, []string{"present", "missing"})
	if err == nil || !strings.Contains(err.Error(), "exact bucket job(s) not found: missing") {
		t.Fatalf("missing exact job error = %v", err)
	}
}

func TestDiscoverExactJobsPropagatesPresubmitErrors(t *testing.T) {
	sentinel := errors.New("storage unavailable")
	tests := []struct {
		name string
		b    *fakeBackend
	}{
		{
			name: "list",
			b: &fakeBackend{objects: map[string]string{}, listErrors: map[string]error{
				"pr-logs/directory/pull-e2e/": sentinel,
			}},
		},
		{
			name: "read",
			b: &fakeBackend{
				objects: map[string]string{"pr-logs/directory/pull-e2e/9.txt": "index"},
				openErrors: map[string]error{
					"pr-logs/directory/pull-e2e/9.txt": sentinel,
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DiscoverExactJobs(context.Background(), tc.b, true, []string{"pull-e2e"})
			if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), `resolving exact presubmit job "pull-e2e"`) {
				t.Fatalf("exact presubmit error = %v", err)
			}
		})
	}
}

func (f *fakeBackend) ListTree(_ context.Context, prefix string, max int) ([]string, bool, error) {
	if f.listTreeErr != nil {
		return nil, false, f.listTreeErr
	}
	var out []string
	for name := range f.objects {
		if strings.HasPrefix(name, prefix) {
			out = append(out, strings.TrimPrefix(name, prefix))
		}
	}
	sort.Strings(out)
	if len(out) > max {
		return out[:max], true, nil
	}
	return out, false, nil
}

func (f *fakeBackend) WebURL(path string) string  { return "https://web/" + path }
func (f *fakeBackend) ProwURL(path string) string { return "https://prow/" + path }

var _ storage.Backend = (*fakeBackend)(nil)

func TestFetchBuildInfo_RunningAndFinished(t *testing.T) {
	b := &fakeBackend{objects: map[string]string{
		"logs/job/100/started.json":  `{"timestamp":1000,"repos":{"example/project":"main"},"repo-commit":"abc"}`,
		"logs/job/100/finished.json": `{"timestamp":1060,"passed":true,"result":"SUCCESS","revision":"main"}`,
		"logs/job/200/started.json":  `{"timestamp":2000}`,
	}}
	ctx := context.Background()

	loc := BuildLocation{JobLocation: JobLocation{JobType: models.JobTypePeriodic}, JobName: "job", BuildID: "100"}
	info, err := FetchBuildInfo(ctx, b, loc)
	if err != nil {
		t.Fatal(err)
	}
	if info.Result != "SUCCESS" || !info.Passed || info.DurationSeconds != 60 || info.Commit != "abc" || info.Revision != "main" {
		t.Errorf("finished build: %+v", info)
	}
	if info.RepoRefs["example/project"] != "main" {
		t.Errorf("repo refs = %+v", info.RepoRefs)
	}
	if info.WebURL != "https://web/logs/job/100/" || info.BuildLogURL != "https://web/logs/job/100/build-log.txt" {
		t.Errorf("urls: web=%q log=%q", info.WebURL, info.BuildLogURL)
	}

	// Missing finished.json means PENDING.
	loc.BuildID = "200"
	info, err = FetchBuildInfo(ctx, b, loc)
	if err != nil {
		t.Fatal(err)
	}
	if info.Result != "PENDING" {
		t.Errorf("running build Result = %q, want PENDING", info.Result)
	}
}

func TestFetchBuildInfoPinsCloneRecords(t *testing.T) {
	sha := "ade57a2918a2c76ab56d9cda31b7319b0bff6aa0"
	other := "457ebaa229bda1f835c508c402391711adc9c32f"
	record := func(org, repo, branch, revision string) string {
		return fmt.Sprintf(`{"refs":{"org":%q,"repo":%q,"base_ref":%q},"final_sha":%q}`, org, repo, branch, revision)
	}
	for _, tc := range []struct {
		name    string
		refs    string
		records string
		want    string
		other   string
	}{
		{name: "two repositories", refs: `{"example/project":"main","example/other":"master"}`,
			records: "[" + record("", "", "", "") + "," + record("example", "project", "main", sha) + "," +
				record("example", "other", "master", other) + "]", want: "main:" + sha, other: "master:" + other},
		{name: "duplicate identical records", refs: `{"example/project":"main"}`,
			records: "[" + record("example", "project", "main", sha) + "," + record("example", "project", "main", sha) + "]",
			want:    "main:" + sha},
		{name: "conflicting records", refs: `{"example/project":"main"}`,
			records: "[" + record("example", "project", "main", sha) + "," + record("example", "project", "main", other) + "]",
			want:    "main"},
		{name: "conflicting branches", refs: `{"example/project":"main"}`,
			records: "[" + record("example", "project", "main", sha) + "," + record("example", "project", "release", other) + "]",
			want:    "main"},
		{name: "pull checkout conflicts with branch record", refs: `{"example/project":"main"}`,
			records: "[" + record("example", "project", "main", sha) +
				`,{"refs":{"org":"example","repo":"project","base_ref":"release","pulls":[{"number":42}]},"final_sha":"` + other + `"}]`,
			want: "main"},
		{name: "pulls present", refs: `{"example/project":"main"}`,
			records: `[{"refs":{"org":"example","repo":"project","base_ref":"main","pulls":[{"number":42}]},"final_sha":"` + sha + `"}]`,
			want:    "main"},
		{name: "failed record", refs: `{"example/project":"main"}`,
			records: `[{"refs":{"org":"example","repo":"project","base_ref":"main"},"failed":true,"final_sha":"` + sha + `"}]`,
			want:    "main"},
		{name: "short SHA", refs: `{"example/project":"main"}`,
			records: "[" + record("example", "project", "main", sha[:8]) + "]", want: "main"},
		{name: "empty record", refs: `{"example/project":"main"}`,
			records: `[{"refs":{"org":"","repo":""}},` + record("example", "other", "main", sha) + "]",
			want:    "main"},
		{name: "missing file", refs: `{"example/project":"main"}`, want: "main"},
		{name: "unparseable file", refs: `{"example/project":"main"}`, records: "{", want: "main"},
		{name: "different base ref", refs: `{"example/project":"main"}`,
			records: "[" + record("example", "project", "release", sha) + "]", want: "main"},
		{name: "composite checkout", refs: `{"example/project":"main:0123456789abcdef0123456789abcdef01234567,42:457ebaa229bda1f835c508c402391711adc9c32f"}`,
			records: "[" + record("example", "project", "main", sha) + "]",
			want:    "main:0123456789abcdef0123456789abcdef01234567,42:457ebaa229bda1f835c508c402391711adc9c32f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			buildDir := filepath.Join(root, "logs", "job", "100")
			if err := os.MkdirAll(buildDir, 0o755); err != nil {
				t.Fatal(err)
			}
			started := `{"timestamp":1000,"repos":` + tc.refs + `}`
			if err := os.WriteFile(filepath.Join(buildDir, "started.json"), []byte(started), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.records != "" {
				if err := os.WriteFile(filepath.Join(buildDir, "clone-records.json"), []byte(tc.records), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			backend, err := storage.New(storage.Config{Provider: storage.ProviderLocal, Base: root}, nil)
			if err != nil {
				t.Fatal(err)
			}
			info, err := FetchBuildInfo(t.Context(), backend, BuildLocation{
				JobLocation: JobLocation{JobType: models.JobTypePeriodic}, JobName: "job", BuildID: "100",
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := info.RepoRefs["example/project"]; got != tc.want {
				t.Errorf("project ref = %q, want %q", got, tc.want)
			}
			if tc.other != "" {
				if got := info.RepoRefs["example/other"]; got != tc.other {
					t.Errorf("other ref = %q, want %q", got, tc.other)
				}
				if source, ok := buildsource.Resolve(*info, "example", "project"); !ok || source.Revision != sha {
					t.Errorf("multi-repo source = %+v, resolved=%v", source, ok)
				}
			}
		})
	}
}

func TestPinBuildRepoRefsSkipsPresubmit(t *testing.T) {
	backend := &fakeBackend{objects: map[string]string{
		"pr-logs/pull/example_project/42/job/100/clone-records.json": `[{"refs":{"org":"example","repo":"project","base_ref":"main"},"final_sha":"ade57a2918a2c76ab56d9cda31b7319b0bff6aa0"}]`,
	}}
	info := &models.BuildInfo{RepoRefs: map[string]string{"example/project": "main"}}
	loc := BuildLocation{
		JobLocation: JobLocation{JobType: models.JobTypePresubmit, Repo: "example/project"},
		JobName:     "job", BuildID: "100", PullNumber: "42",
	}
	if PinBuildRepoRefs(t.Context(), backend, loc, info) || info.RepoRefs["example/project"] != "main" {
		t.Fatalf("presubmit ref changed: %+v", info.RepoRefs)
	}
}

func TestDiscoverJUnitPathsCompleteTree(t *testing.T) {
	b := &fakeBackend{objects: map[string]string{
		"logs/job/1/artifacts/junit.xml":          "x",
		"logs/job/1/artifacts/junit_runner.xml":   "x",
		"logs/job/1/artifacts/results.xml":        "x",
		"logs/job/1/artifacts/sub/junit.deep.xml": "x",
	}}
	got, complete, truncated, err := DiscoverJUnitPathsWithStatus(context.Background(), b,
		BuildLocation{JobLocation: JobLocation{JobType: models.JobTypePeriodic}, JobName: "job", BuildID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"logs/job/1/artifacts/junit.xml", "logs/job/1/artifacts/junit_runner.xml", "logs/job/1/artifacts/sub/junit.deep.xml"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("junit paths = %v, want %v", got, want)
	}
	if !complete || truncated {
		t.Errorf("complete=%v truncated=%v, want true false", complete, truncated)
	}
}

func TestListRecentBuilds_Periodic(t *testing.T) {
	b := &fakeBackend{objects: map[string]string{
		"logs/job/100/started.json": "x",
		"logs/job/103/started.json": "x",
		"logs/job/101/started.json": "x",
	}}
	builds, err := ListRecentBuilds(context.Background(), b,
		&models.ProwJob{Name: "job", JobType: models.JobTypePeriodic}, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first, capped at 2.
	if len(builds) != 2 || builds[0].ID != "103" || builds[1].ID != "101" {
		t.Errorf("periodic builds = %+v", builds)
	}
}

func TestListRecentBuilds_Presubmit(t *testing.T) {
	b := &fakeBackend{objects: map[string]string{
		// Relative body (k8s GCS style).
		"pr-logs/directory/job/500.txt": "pr-logs/pull/istio_istio/42/job/500",
		"pr-logs/directory/job/499.txt": "pr-logs/pull/other_repo/9/job/499", // wrong repo, skipped
		// Absolute URL body (Istio S3 style).
		"pr-logs/directory/job/498.txt": "s3://istio-prow/pr-logs/pull/istio_istio/7/job/498",
	}}
	builds, err := ListRecentBuilds(context.Background(), b,
		&models.ProwJob{Name: "job", JobType: models.JobTypePresubmit, Repo: "istio/istio"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != 2 {
		t.Fatalf("presubmit builds = %+v, want 2 (cross-repo filtered)", builds)
	}
	if builds[0].ID != "500" || builds[0].PullNumber != "42" {
		t.Errorf("newest build = %+v", builds[0])
	}
	if builds[1].ID != "498" || builds[1].PullNumber != "7" {
		t.Errorf("absolute-URL build = %+v", builds[1])
	}
}

func TestDiscoverJobs_BucketDriven(t *testing.T) {
	b := &fakeBackend{objects: map[string]string{
		"logs/periodic-a/1/started.json":     "x",
		"logs/integ-ambient/1/started.json":  "x",
		"pr-logs/directory/integ-cni/9.txt":  "s3://istio-prow/pr-logs/pull/istio_istio/3/integ-cni/9",
		"pr-logs/directory/unit-tests/8.txt": "pr-logs/pull/istio_istio/3/unit-tests/8",
	}}
	ctx := context.Background()

	jobs, err := DiscoverJobs(ctx, b, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Errorf("periodic discovery = %+v, want 2", jobs)
	}

	jobs, err = DiscoverJobs(ctx, b, true, []string{"integ-"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, j := range jobs {
		names = append(names, j.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "integ-ambient,integ-cni" {
		t.Errorf("filtered discovery = %v, want [integ-ambient integ-cni]", names)
	}
	// The presubmit job's repo is resolved from its index entry.
	for _, j := range jobs {
		if j.JobID == "" {
			t.Errorf("job %q has empty JobID", j.Name)
		}
		// Bucket discovery must populate TabName when testgrid-tab-name is absent.
		if j.TabName != j.Name {
			t.Errorf("job %q TabName = %q, want = Name", j.Name, j.TabName)
		}
		if j.JobType == models.JobTypePresubmit && j.Repo != "istio/istio" {
			t.Errorf("presubmit repo = %q, want istio/istio", j.Repo)
		}
	}
}

func TestListPullBuilds(t *testing.T) {
	b := &fakeBackend{objects: map[string]string{
		"pr-logs/pull/example_project/42/pull-e2e/100/started.json": "x",
		"pr-logs/pull/example_project/42/pull-e2e/105/started.json": "x",
		"pr-logs/pull/example_project/7/pull-e2e/110/started.json":  "x",
	}}
	builds, err := ListPullBuilds(context.Background(), b, "example/project", "42", "pull-e2e", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != 2 || builds[0].ID != "105" || builds[1].ID != "100" {
		t.Fatalf("builds = %+v", builds)
	}
	for _, build := range builds {
		if build.PullNumber != "42" {
			t.Errorf("pull number = %q", build.PullNumber)
		}
	}
}

func TestDiscoverJUnitPathsFindsRootJUnitBeforeTreeCap(t *testing.T) {
	objects := map[string]string{
		"logs/job/1/artifacts/junit.e2e_suite.1.xml": "x",
		"logs/job/1/artifacts/results.xml":           "x",
	}
	for i := range 2001 {
		objects[fmt.Sprintf("logs/job/1/artifacts/clusters/%04d/log.txt", i)] = "x"
	}
	b := &fakeBackend{objects: objects}
	paths, complete, truncated, err := DiscoverJUnitPathsWithStatus(context.Background(), b,
		BuildLocation{JobLocation: JobLocation{JobType: models.JobTypePeriodic}, JobName: "job", BuildID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"logs/job/1/artifacts/junit.e2e_suite.1.xml"}
	if strings.Join(paths, ",") != strings.Join(want, ",") || complete || !truncated {
		t.Fatalf("paths=%v complete=%v truncated=%v, want %v false true", paths, complete, truncated, want)
	}
	usable, err := DiscoverJUnitPaths(context.Background(), b,
		BuildLocation{JobLocation: JobLocation{JobType: models.JobTypePeriodic}, JobName: "job", BuildID: "1"})
	if err != nil || strings.Join(usable, ",") != strings.Join(want, ",") {
		t.Fatalf("usable=%v err=%v, want %v", usable, err, want)
	}
}

func TestDiscoverJUnitPathsListingFailures(t *testing.T) {
	loc := BuildLocation{JobLocation: JobLocation{JobType: models.JobTypePeriodic}, JobName: "job", BuildID: "1"}
	objects := map[string]string{
		"logs/job/1/artifacts/junit.xml":          "x",
		"logs/job/1/artifacts/sub/junit.deep.xml": "x",
	}

	t.Run("root listing", func(t *testing.T) {
		b := &fakeBackend{objects: objects, listErr: errors.New("root unavailable")}
		paths, complete, truncated, err := DiscoverJUnitPathsWithStatus(context.Background(), b, loc)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"logs/job/1/artifacts/junit.xml", "logs/job/1/artifacts/sub/junit.deep.xml"}
		if strings.Join(paths, ",") != strings.Join(want, ",") || !complete || truncated {
			t.Fatalf("paths=%v complete=%v truncated=%v, want %v true false", paths, complete, truncated, want)
		}
	})

	t.Run("recursive listing", func(t *testing.T) {
		b := &fakeBackend{objects: objects, listTreeErr: errors.New("tree unavailable")}
		paths, complete, truncated, err := DiscoverJUnitPathsWithStatus(context.Background(), b, loc)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"logs/job/1/artifacts/junit.xml"}
		if strings.Join(paths, ",") != strings.Join(want, ",") || complete || truncated {
			t.Fatalf("paths=%v complete=%v truncated=%v, want %v false false", paths, complete, truncated, want)
		}
	})

	t.Run("both listings", func(t *testing.T) {
		rootErr := errors.New("root unavailable")
		treeErr := errors.New("tree unavailable")
		b := &fakeBackend{objects: objects, listErr: rootErr, listTreeErr: treeErr}
		paths, complete, truncated, err := DiscoverJUnitPathsWithStatus(context.Background(), b, loc)
		if err == nil {
			t.Fatal("expected listing error")
		}
		if len(paths) != 0 || complete || truncated {
			t.Fatalf("paths=%v complete=%v truncated=%v", paths, complete, truncated)
		}
		if !errors.Is(err, rootErr) || !errors.Is(err, treeErr) || !strings.Contains(err.Error(), "logs/job/1/artifacts/") {
			t.Fatalf("error = %v, want both causes and artifact path", err)
		}
	})
}

// TestListRecentBuildsPresubmitHonorsCancellation pins that presubmit
// enumeration is interruptible. Each candidate costs an index read whose failure
// is reported as "not this repo", so without an explicit check a cancelled
// context reads the whole index and reports success with nothing found. Callers
// that bound this work by deadline depend on it returning instead.
func TestListRecentBuildsPresubmitHonorsCancellation(t *testing.T) {
	objects := map[string]string{}
	for i := 100; i < 160; i++ {
		id := strconv.Itoa(i)
		objects["pr-logs/directory/pull-e2e/"+id+".txt"] = "pr-logs/pull/example_project/3/pull-e2e/" + id
	}
	b := &fakeBackend{objects: objects}
	job := &models.ProwJob{Name: "pull-e2e", JobType: models.JobTypePresubmit, Repo: "example/project"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	builds, err := ListRecentBuilds(ctx, b, job, 40)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(builds) != 0 {
		t.Errorf("builds = %d, want none from a cancelled enumeration", len(builds))
	}

	// A live context still enumerates normally.
	live, err := ListRecentBuilds(context.Background(), b, job, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 5 || live[0].ID != "159" {
		t.Errorf("live enumeration = %d builds starting at %v, want 5 starting at 159", len(live), live)
	}
}

// cancelOnOpenBackend cancels the enumeration context from inside a candidate
// read, reproducing a deadline that expires mid-flight rather than between
// candidates.
type cancelOnOpenBackend struct {
	*fakeBackend
	cancel func()
}

func (c *cancelOnOpenBackend) Open(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	c.cancel()
	return nil, 0, context.Canceled
}

// TestListRecentBuildsPresubmitHonorsCancellationDuringResolution pins the
// narrow window the loop-top check cannot see. With a single candidate there is
// no next iteration to catch the cancellation, and a read cancelled in flight
// fails exactly as a foreign build does, so without a recheck the enumeration
// would report an empty result as a complete success.
func TestListRecentBuildsPresubmitHonorsCancellationDuringResolution(t *testing.T) {
	objects := map[string]string{
		"pr-logs/directory/pull-e2e/100.txt": "pr-logs/pull/example_project/3/pull-e2e/100",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &cancelOnOpenBackend{fakeBackend: &fakeBackend{objects: objects}, cancel: cancel}
	job := &models.ProwJob{Name: "pull-e2e", JobType: models.JobTypePresubmit, Repo: "example/project"}

	builds, err := ListRecentBuilds(ctx, b, job, 40)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled from a read cancelled in flight", err)
	}
	if len(builds) != 0 {
		t.Errorf("builds = %d, want none", len(builds))
	}
}
