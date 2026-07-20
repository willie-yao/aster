package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

// fakeBrowser is a small in-memory Browser shared across filesystem tests.
type fakeBrowser struct {
	dirs  map[string][]string
	files map[string][]byte
}

func (b *fakeBrowser) BuildRoot() string { return "fake/build/1" }

func (b *fakeBrowser) ListTree(_ context.Context, maxPaths int) ([]string, bool, error) {
	var out []string
	for name := range b.files {
		if len(out) >= maxPaths {
			return out, true, nil
		}
		out = append(out, name)
	}
	return out, false, nil
}

func (b *fakeBrowser) List(_ context.Context, dir string) (*artifacts.Listing, error) {
	prefix := dir
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	subdirs, hasDir := b.dirs[prefix]
	var files []artifacts.FileInfo
	for name, data := range b.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if strings.Contains(rest, "/") {
			continue
		}
		files = append(files, artifacts.FileInfo{Name: rest, Size: int64(len(data))})
	}
	if !hasDir && len(files) == 0 {
		return nil, fmt.Errorf("not found: %s", dir)
	}
	return &artifacts.Listing{Dir: prefix, Dirs: subdirs, Files: files}, nil
}

func (b *fakeBrowser) Read(_ context.Context, p string, _, _ int) ([]byte, int64, error) {
	d, ok := b.files[p]
	if !ok {
		return nil, 0, fmt.Errorf("not found: %s", p)
	}
	return d, int64(len(d)), nil
}

func (b *fakeBrowser) Tail(_ context.Context, p string, _, _ int) (*artifacts.TailResult, error) {
	d, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	return &artifacts.TailResult{FileSize: int64(len(d)), Content: d}, nil
}

func (b *fakeBrowser) Grep(_ context.Context, p string, _ *regexp.Regexp, _, _, _, _ int) (*artifacts.GrepResult, error) {
	d, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	return &artifacts.GrepResult{FileSize: int64(len(d))}, nil
}

// junitTree models a typical prow build with a handful of junit XML files
// scattered across artifact subdirs plus some non-XML files that must be
// excluded by the basename pattern.
func junitTree() *fakeBrowser {
	return &fakeBrowser{
		dirs: map[string][]string{
			"":                            {"artifacts/", "build-log.txt"},
			"artifacts/":                  {"e2e/", "junit/"},
			"artifacts/e2e/":              {"clusters/"},
			"artifacts/e2e/clusters/":     {"foo/"},
			"artifacts/e2e/clusters/foo/": {},
			"artifacts/junit/":            {},
		},
		files: map[string][]byte{
			"artifacts/junit/junit_01.xml":             []byte("<testsuite/>"),
			"artifacts/junit/junit_02.xml":             []byte("<testsuite/>"),
			"artifacts/junit/README.md":                []byte("not junit"),
			"artifacts/e2e/junit_e2e.xml":              []byte("<testsuite/>"),
			"artifacts/e2e/clusters/foo/junit_foo.xml": []byte("<testsuite/>"),
			"build-log.txt":                            []byte("noise"),
		},
	}
}

func dispatchFind(t *testing.T, env *tools.Env, args interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res := (&findTool{}).Dispatch(context.Background(), env, raw)
	if res.Payload == nil {
		t.Fatalf("nil payload")
	}
	return res.Payload
}

func TestFindArtifactsRecursesAndFiltersByBasename(t *testing.T) {
	env := &tools.Env{Browser: junitTree()}
	payload := dispatchFind(t, env, map[string]interface{}{
		"pattern": `^junit.*\.xml$`,
	})

	raw, _ := json.Marshal(payload["matches"])
	var got []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal matches: %v", err)
	}
	wantPaths := map[string]bool{
		"artifacts/junit/junit_01.xml":             false,
		"artifacts/junit/junit_02.xml":             false,
		"artifacts/e2e/junit_e2e.xml":              false,
		"artifacts/e2e/clusters/foo/junit_foo.xml": false,
	}
	for _, m := range got {
		if _, ok := wantPaths[m.Path]; !ok {
			t.Errorf("unexpected match: %s", m.Path)
			continue
		}
		wantPaths[m.Path] = true
		if m.Size <= 0 {
			t.Errorf("missing size for %s", m.Path)
		}
	}
	for p, seen := range wantPaths {
		if !seen {
			t.Errorf("missed match: %s", p)
		}
	}
	if _, truncated := payload["truncated"]; truncated {
		t.Errorf("did not expect truncation: %v", payload)
	}
}

func TestFindArtifactsHonorsRootScope(t *testing.T) {
	env := &tools.Env{Browser: junitTree()}
	payload := dispatchFind(t, env, map[string]interface{}{
		"pattern": `^junit.*\.xml$`,
		"root":    "artifacts/junit/",
	})
	raw, _ := json.Marshal(payload["matches"])
	var got []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 matches under artifacts/junit/, got %d: %+v", len(got), got)
	}
	for _, m := range got {
		if !strings.HasPrefix(m.Path, "artifacts/junit/") {
			t.Errorf("match leaked out of root: %s", m.Path)
		}
	}
}

func TestFindArtifactsTruncatesByMaxResults(t *testing.T) {
	env := &tools.Env{Browser: junitTree()}
	payload := dispatchFind(t, env, map[string]interface{}{
		"pattern":     `^junit.*\.xml$`,
		"max_results": 2,
	})
	if payload["truncated"] != true {
		t.Errorf("expected truncated=true, got %v", payload)
	}
	if payload["truncated_reason"] != "max_results" {
		t.Errorf("truncated_reason = %v, want max_results", payload["truncated_reason"])
	}
}

func TestFindArtifactsTruncatesByMaxDirs(t *testing.T) {
	env := &tools.Env{Browser: junitTree()}
	payload := dispatchFind(t, env, map[string]interface{}{
		"pattern":  `^junit.*\.xml$`,
		"max_dirs": 1,
	})
	if payload["truncated"] != true {
		t.Errorf("expected truncated=true, got %v", payload)
	}
	if payload["truncated_reason"] != "max_dirs" {
		t.Errorf("truncated_reason = %v, want max_dirs", payload["truncated_reason"])
	}
}

func TestFindArtifactsInvalidRegexReturnsErrorPayload(t *testing.T) {
	env := &tools.Env{Browser: junitTree()}
	payload := dispatchFind(t, env, map[string]interface{}{"pattern": "["})
	if _, ok := payload["error"]; !ok {
		t.Errorf("expected error payload, got %v", payload)
	}
}

func TestRegisterEnablesAllFilesystemTools(t *testing.T) {
	r := tools.NewRegistry()
	Register(r)
	enabled, err := r.Enable([]string{"filesystem"})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	want := map[string]bool{
		"list_artifacts": false,
		"read_artifact":  false,
		"tail_artifact":  false,
		"grep_artifact":  false,
		"find_artifacts": false,
	}
	for _, n := range enabled {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("group enable missed tool %q", n)
		}
	}
}

// TestStringEncodedNumericArgsAreAccepted verifies filesystem tools accept
// numeric arguments encoded as either numbers or strings.
func TestStringEncodedNumericArgsAreAccepted(t *testing.T) {
	b := &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte("line1\nERROR boom\nline3\nline4\n"),
	}}
	env := &tools.Env{Browser: b}
	mustNoError := func(t *testing.T, name string, payload map[string]interface{}) {
		t.Helper()
		if e, isErr := payload["error"]; isErr {
			t.Fatalf("%s with string-encoded numeric args should not error: %v", name, e)
		}
	}

	raw, _ := json.Marshal(map[string]interface{}{"path": "build-log.txt", "lines": "2"})
	mustNoError(t, "tail_artifact", (&tailTool{}).Dispatch(context.Background(), env, raw).Payload)

	raw, _ = json.Marshal(map[string]interface{}{"path": "build-log.txt", "pattern": "ERROR", "context_lines": "1", "max_matches": "10"})
	mustNoError(t, "grep_artifact", (&grepTool{}).Dispatch(context.Background(), env, raw).Payload)

	raw, _ = json.Marshal(map[string]interface{}{"path": "build-log.txt", "offset": "0", "length": "5"})
	mustNoError(t, "read_artifact", (&readTool{}).Dispatch(context.Background(), env, raw).Payload)

	raw, _ = json.Marshal(map[string]interface{}{"pattern": ".*", "max_results": "10", "max_dirs": "50"})
	mustNoError(t, "find_artifacts", (&findTool{}).Dispatch(context.Background(), env, raw).Payload)
}
