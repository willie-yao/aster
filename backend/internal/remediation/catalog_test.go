package remediation

import (
	"context"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prow/jobconfig"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/storage"
)

type memoryBackend struct{ objects map[string]string }

func (m memoryBackend) Open(_ context.Context, path string) (io.ReadCloser, int64, error) {
	value, ok := m.objects[path]
	if !ok {
		return nil, 0, storage.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(value)), int64(len(value)), nil
}
func (m memoryBackend) ReadRange(context.Context, string, int64, int64) ([]byte, int64, error) {
	return nil, 0, io.EOF
}
func (m memoryBackend) ReadTail(context.Context, string, int64) ([]byte, int64, error) {
	return nil, 0, io.EOF
}
func (m memoryBackend) List(_ context.Context, prefix string) (*storage.Listing, error) {
	dirs := map[string]bool{}
	files := map[string]bool{}
	for path := range m.objects {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			dirs[rest[:i+1]] = true
		} else if rest != "" {
			files[rest] = true
		}
	}
	out := &storage.Listing{}
	for dir := range dirs {
		out.Dirs = append(out.Dirs, dir)
	}
	for file := range files {
		out.Files = append(out.Files, storage.Object{Name: file})
	}
	sort.Strings(out.Dirs)
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	return out, nil
}
func (m memoryBackend) ListTree(_ context.Context, prefix string, max int) ([]string, bool, error) {
	var paths []string
	for path := range m.objects {
		if strings.HasPrefix(path, prefix) {
			paths = append(paths, strings.TrimPrefix(path, prefix))
		}
	}
	sort.Strings(paths)
	if len(paths) > max {
		return paths[:max], true, nil
	}
	return paths, false, nil
}
func (m memoryBackend) WebURL(path string) string  { return path }
func (m memoryBackend) ProwURL(path string) string { return path }

func TestBuildCoverageCatalog(t *testing.T) {
	b := memoryBackend{objects: map[string]string{
		"pr-logs/directory/pull-e2e/10.txt":                               "pr-logs/pull/example_project/42/pull-e2e/10",
		"pr-logs/pull/example_project/42/pull-e2e/10/started.json":        `{"timestamp":1}`,
		"pr-logs/pull/example_project/42/pull-e2e/10/finished.json":       `{"timestamp":2,"passed":true,"result":"SUCCESS"}`,
		"pr-logs/pull/example_project/42/pull-e2e/10/artifacts/junit.xml": `<testsuite name="suite"><testcase name="test" classname="class"/></testsuite>`,
	}}
	catalog := &jobconfig.Catalog{Revision: "sha", Jobs: map[string]jobconfig.JobDefinition{
		"example/project/pull-e2e": {
			Name: "pull-e2e", JobType: "presubmit", Repo: "example/project",
			Branches: []string{"^main$"}, SkipBranches: []string{"^release-"},
		},
	}}
	got, err := BuildCoverageCatalog(context.Background(), b, catalog, []string{"example/project"})
	if err != nil {
		t.Fatal(err)
	}
	jobs := got.Tests["suite\x00class\x00test"]
	if len(jobs) != 1 || jobs[0].JobName != "pull-e2e" || jobs[0].RerunCommand != "/test pull-e2e" {
		t.Fatalf("coverage = %+v", got)
	}
	if len(jobs[0].Branches) != 1 || jobs[0].Branches[0] != "^main$" || len(jobs[0].SkipBranches) != 1 {
		t.Fatalf("branch selectors = %+v", jobs[0])
	}
	definition := got.Jobs["example/project/pull-e2e"]
	if definition.RerunCommand != "/test pull-e2e" || len(definition.Branches) != 1 {
		t.Fatalf("job definition = %+v", definition)
	}
	if len(got.Tests["name\x00test"]) != 1 {
		t.Fatalf("name fallback missing: %+v", got.Tests)
	}
}
