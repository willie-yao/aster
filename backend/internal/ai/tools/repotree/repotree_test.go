package repotree

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
)

// fakeRepo is an in-memory RepoReader. reads counts ReadFile calls so tests can
// assert the Cache prevents refetching.
type fakeRepo struct {
	files map[string]string
	reads int
}

func (r *fakeRepo) ListTree(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(r.files))
	for p := range r.files {
		out = append(out, p)
	}
	return out, nil
}

func (r *fakeRepo) ReadFile(_ context.Context, path string) (string, bool, error) {
	r.reads++
	c, ok := r.files[path]
	return c, ok, nil
}

func envFor(repo *fakeRepo) *tools.Env {
	return &tools.Env{Repo: repo, Cache: tools.NewCache()}
}

func dispatch(t *testing.T, tool tools.Tool, env *tools.Env, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, _ := json.Marshal(args)
	res := tool.Dispatch(context.Background(), env, raw)
	if res.Payload == nil {
		t.Fatalf("%s: nil payload", tool.Name())
	}
	return res.Payload
}

func sampleRepo() *fakeRepo {
	return &fakeRepo{files: map[string]string{
		"README.md":                "hello\n",
		"config/dev.yaml":          "replicas: 1\nimage: foo:v1\n",
		"config/prod.yaml":         "replicas: 3\nimage: foo:v1\n",
		"pkg/cloud/scope.go":       "package cloud\n\nfunc New() {}\n",
		"pkg/cloud/services/vm.go": "package services\n\n// timeout bug here\nvar timeout = 600\n",
	}}
}

func TestListRepoTree_RootAndSubdir(t *testing.T) {
	env := envFor(sampleRepo())
	tool := &listTool{}

	root := dispatch(t, tool, env, map[string]interface{}{"path": ""})
	dirs, _ := root["dirs"].([]string)
	if len(dirs) != 2 || dirs[0] != "config" || dirs[1] != "pkg" {
		t.Errorf("root dirs = %v, want [config pkg]", dirs)
	}
	files, _ := root["files"].([]string)
	if len(files) != 1 || files[0] != "README.md" {
		t.Errorf("root files = %v, want [README.md]", files)
	}

	sub := dispatch(t, tool, env, map[string]interface{}{"path": "config"})
	sf, _ := sub["files"].([]string)
	if len(sf) != 2 || sf[0] != "dev.yaml" || sf[1] != "prod.yaml" {
		t.Errorf("config files = %v, want [dev.yaml prod.yaml]", sf)
	}
	if sub["dir"] != "config/" {
		t.Errorf("dir = %v, want config/", sub["dir"])
	}
}

func TestReadRepoFile_RangeAndCache(t *testing.T) {
	repo := sampleRepo()
	env := envFor(repo)
	tool := &readTool{}

	p := dispatch(t, tool, env, map[string]interface{}{"path": "config/dev.yaml"})
	if p["content"] != "replicas: 1\nimage: foo:v1\n" {
		t.Errorf("content = %q", p["content"])
	}
	if p["file_size"].(int) != len("replicas: 1\nimage: foo:v1\n") {
		t.Errorf("file_size = %v", p["file_size"])
	}
	// Second read is served from the cache: no extra ReadFile call.
	if repo.reads != 1 {
		t.Fatalf("reads = %d after first read, want 1", repo.reads)
	}
	dispatch(t, tool, env, map[string]interface{}{"path": "config/dev.yaml"})
	if repo.reads != 1 {
		t.Errorf("reads = %d after cached read, want still 1", repo.reads)
	}

	// Offset/length slices the content.
	sl := dispatch(t, tool, env, map[string]interface{}{"path": "config/dev.yaml", "offset": 10, "length": 6})
	if sl["content"] != "1\nimag" {
		t.Errorf("sliced content = %q, want \"1\\nimag\"", sl["content"])
	}
}

func TestReadRepoFile_NotFound(t *testing.T) {
	env := envFor(sampleRepo())
	res := (&readTool{}).Dispatch(context.Background(), env, mustJSON(map[string]interface{}{"path": "nope.txt"}))
	if _, hasErr := res.Payload["error"]; !hasErr {
		t.Errorf("expected error payload for missing file, got %v", res.Payload)
	}
}

func TestGrepRepo_FindsSymbolAndReportsLocation(t *testing.T) {
	env := envFor(sampleRepo())
	p := dispatch(t, &grepTool{}, env, map[string]interface{}{
		"pattern":   "timeout",
		"path_glob": "*.go",
	})
	raw, _ := json.Marshal(p["matches"])
	var got []map[string]interface{}
	_ = json.Unmarshal(raw, &got)
	if len(got) == 0 {
		t.Fatalf("expected a match for 'timeout', got none (payload=%v)", p)
	}
	if got[0]["path"] != "pkg/cloud/services/vm.go" {
		t.Errorf("match path = %v, want pkg/cloud/services/vm.go", got[0]["path"])
	}
	if int(got[0]["line"].(float64)) != 3 {
		t.Errorf("match line = %v, want 3", got[0]["line"])
	}
}

func TestGrepRepo_GlobNarrowsScope(t *testing.T) {
	env := envFor(sampleRepo())
	// image: appears in both yaml files but no go files. Narrow to config.
	p := dispatch(t, &grepTool{}, env, map[string]interface{}{
		"pattern":   "image:",
		"path_glob": "config/",
	})
	if p["files_scanned"].(int) == 0 {
		t.Fatal("expected to scan config files")
	}
	raw, _ := json.Marshal(p["matches"])
	var got []map[string]interface{}
	_ = json.Unmarshal(raw, &got)
	if len(got) != 2 {
		t.Errorf("image: matches = %d, want 2 (dev + prod)", len(got))
	}
}

func TestGrepRepo_InvalidRegex(t *testing.T) {
	env := envFor(sampleRepo())
	res := (&grepTool{}).Dispatch(context.Background(), env, mustJSON(map[string]interface{}{"pattern": "("}))
	if _, hasErr := res.Payload["error"]; !hasErr {
		t.Errorf("expected error payload for bad regex, got %v", res.Payload)
	}
}

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"", "anything/at/all.go", true},
		{"config", "pkg/config/x.yaml", true},
		{"config", "pkg/other/x.yaml", false},
		{"*.go", "pkg/cloud/scope.go", true},
		{"*.go", "config/dev.yaml", false},
		{"*.go", "pkg/x.go.md", false},
		{"config/*.yaml", "config/dev.yaml", true},
		{"config/*.yaml", "pkg/scope.go", false},
	}
	for _, tc := range cases {
		re, err := globToRegexp(tc.glob)
		if err != nil {
			t.Fatalf("globToRegexp(%q) error: %v", tc.glob, err)
		}
		got := re == nil || re.MatchString(tc.path)
		if got != tc.want {
			t.Errorf("glob %q vs %q = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}

func mustJSON(v map[string]interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
