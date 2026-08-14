package prowbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

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
	for i := 0; i < 2001; i++ {
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
