package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/artifacts"
)

// fakeBrowser is a small in-memory Browser shared across filesystem tests.
type fakeBrowser struct {
	dirs             map[string][]string
	files            map[string][]byte
	grepContextLines []int
	grepMaxMatches   []int
	grepResult       *artifacts.GrepResult
	grepErr          error
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

func (b *fakeBrowser) Grep(_ context.Context, p string, _ *regexp.Regexp, contextLines, maxMatches, _, _ int) (*artifacts.GrepResult, error) {
	d, ok := b.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	b.grepContextLines = append(b.grepContextLines, contextLines)
	b.grepMaxMatches = append(b.grepMaxMatches, maxMatches)
	if b.grepErr != nil {
		return nil, b.grepErr
	}
	if b.grepResult != nil {
		return b.grepResult, nil
	}
	return &artifacts.GrepResult{FileSize: int64(len(d))}, nil
}

func TestGrepArtifactContextLinesDefaultAndExplicitZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want int
	}{
		{name: "omitted", args: map[string]interface{}{"path": "build-log.txt", "pattern": "failure"}, want: 2},
		{name: "explicit zero", args: map[string]interface{}{"path": "build-log.txt", "pattern": "failure", "context_lines": 0}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("failure\n")}}
			raw, _ := json.Marshal(tc.args)
			result := (&grepTool{}).Dispatch(context.Background(), &tools.Env{Browser: browser}, raw)
			if _, failed := result.Payload["error"]; failed {
				t.Fatalf("payload=%v", result.Payload)
			}
			if len(browser.grepContextLines) != 1 || browser.grepContextLines[0] != tc.want {
				t.Fatalf("context lines=%v, want %d", browser.grepContextLines, tc.want)
			}
		})
	}
}

func TestGrepArtifactRetainsContentFreeCallTelemetry(t *testing.T) {
	browser := &fakeBrowser{
		files: map[string][]byte{"build-log.txt": []byte("before\nmatch\nafter\n")},
		grepResult: &artifacts.GrepResult{
			FileSize: 19, TotalMatches: 1, BytesScanned: 19,
			Matches: []artifacts.GrepMatch{{LineNo: 2, Context: []string{"  1: before", "> 2: match", "  3: after"}}},
		},
	}
	raw, _ := json.Marshal(map[string]interface{}{"path": "build-log.txt", "pattern": "private model query"})
	result := (&grepTool{}).Dispatch(context.Background(), &tools.Env{Browser: browser}, raw)
	observation, ok := result.Observation.(tools.GrepCallObservation)
	if !ok {
		t.Fatalf("observation=%T", result.Observation)
	}
	if observation.SelectorID != artifactGrepSelector || observation.PathFilter != "build-log.txt" || observation.ContextLines != 2 || observation.MaxMatches != 30 || observation.MatchCount != 1 || observation.FilesScanned != 1 || observation.Outcome != tools.GrepOutcomeMatched {
		t.Fatalf("observation=%+v", observation)
	}
	want := []tools.GrepRangeObservation{{SelectorID: artifactGrepSelector, Path: "build-log.txt", LineStart: 1, LineEnd: 3}}
	if !reflect.DeepEqual(observation.ReturnedRanges, want) {
		t.Fatalf("ranges=%+v, want %+v", observation.ReturnedRanges, want)
	}
	encoded, _ := json.Marshal(observation)
	if strings.Contains(string(encoded), "private model query") {
		t.Fatalf("observation retained regex: %s", encoded)
	}
}

func TestGrepArtifactRedactsPlainFilterButRetainsCanonicalRange(t *testing.T) {
	browser := &fakeBrowser{
		files: map[string][]byte{"Makefile": []byte("target:\n")},
		grepResult: &artifacts.GrepResult{
			FileSize: 8, TotalMatches: 1, BytesScanned: 8,
			Matches: []artifacts.GrepMatch{{LineNo: 1, Context: []string{"> 1: target:"}}},
		},
	}
	raw, _ := json.Marshal(map[string]interface{}{"path": "Makefile", "pattern": "target"})
	result := (&grepTool{}).Dispatch(context.Background(), &tools.Env{Browser: browser}, raw)
	observation := result.Observation.(tools.GrepCallObservation)
	if observation.PathFilter != "" || !observation.PathFilterRedacted || len(observation.ReturnedRanges) != 1 || observation.ReturnedRanges[0].Path != "Makefile" {
		t.Fatalf("observation=%+v", observation)
	}
}

func TestGrepArtifactRetainsZeroMatchAndErrorTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pattern      string
		outcome      string
		browserError bool
	}{
		{name: "zero matches", pattern: "missing", outcome: tools.GrepOutcomeZeroMatches},
		{name: "invalid regex", pattern: "(", outcome: tools.GrepOutcomeError},
		{name: "browser error", pattern: "match", outcome: tools.GrepOutcomeError, browserError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			browser := &fakeBrowser{files: map[string][]byte{"build-log.txt": []byte("line\n")}}
			if tc.browserError {
				browser.grepErr = errors.New("read failed")
			}
			raw, _ := json.Marshal(map[string]interface{}{"path": "build-log.txt", "pattern": tc.pattern})
			result := (&grepTool{}).Dispatch(context.Background(), &tools.Env{Browser: browser}, raw)
			observation, ok := result.Observation.(tools.GrepCallObservation)
			if !ok || observation.Outcome != tc.outcome || observation.MatchCount != 0 || len(observation.ReturnedRanges) != 0 {
				t.Fatalf("observation=%T %+v", result.Observation, result.Observation)
			}
			if tc.browserError && (observation.FilesAttempted != 1 || observation.FileReadErrors != 1) {
				t.Fatalf("browser error observation=%+v", observation)
			}
		})
	}
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
	read := (&readTool{}).Dispatch(context.Background(), env, raw)
	mustNoError(t, "read_artifact", read.Payload)
	if content, _ := read.Payload["content"].(string); read.ContentBytes != len(content) || read.ContentBytes == 0 {
		t.Fatalf("read_artifact content bytes = %d, content length = %d", read.ContentBytes, len(content))
	}

	raw, _ = json.Marshal(map[string]interface{}{"pattern": ".*", "max_results": "10", "max_dirs": "50"})
	mustNoError(t, "find_artifacts", (&findTool{}).Dispatch(context.Background(), env, raw).Payload)
}
