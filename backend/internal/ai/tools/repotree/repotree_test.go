package repotree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

// fakeRepo is an in-memory RepoReader. reads counts ReadFile calls so tests can
// assert the Cache prevents refetching.
type fakeRepo struct {
	files      map[string]string
	readErrors map[string]error
	lists      int
	reads      int
}

func (r *fakeRepo) ListTree(_ context.Context) ([]string, error) {
	r.lists++
	out := make([]string, 0, len(r.files))
	for p := range r.files {
		out = append(out, p)
	}
	return out, nil
}

func (r *fakeRepo) ReadFile(_ context.Context, path string) (string, bool, error) {
	r.reads++
	if err := r.readErrors[path]; err != nil {
		return "", false, err
	}
	c, ok := r.files[path]
	return c, ok, nil
}

func envFor(repo *fakeRepo) *tools.Env {
	catalog, err := tools.NewPrimarySourceCatalog("example", "project", "1111111111111111111111111111111111111111", repo)
	if err != nil {
		panic(err)
	}
	return &tools.Env{Sources: catalog, Cache: tools.NewCache()}
}

func withPrimary(args map[string]interface{}) map[string]interface{} {
	args["source_id"] = tools.PrimarySourceID
	return args
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

	root := dispatch(t, tool, env, withPrimary(withPrimary(map[string]interface{}{"path": ""})))
	dirs, _ := root["dirs"].([]string)
	if len(dirs) != 2 || dirs[0] != "config" || dirs[1] != "pkg" {
		t.Errorf("root dirs = %v, want [config pkg]", dirs)
	}
	files, _ := root["files"].([]string)
	if len(files) != 1 || files[0] != "README.md" {
		t.Errorf("root files = %v, want [README.md]", files)
	}

	sub := dispatch(t, tool, env, withPrimary(withPrimary(map[string]interface{}{"path": "config"})))
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

	res := tool.Dispatch(context.Background(), env, mustJSON(withPrimary(map[string]interface{}{"path": "config/dev.yaml"})))
	p := res.Payload
	if p["content"] != "replicas: 1\nimage: foo:v1\n" {
		t.Errorf("content = %q", p["content"])
	}
	if res.ContentBytes != len("replicas: 1\nimage: foo:v1\n") {
		t.Errorf("content bytes = %d", res.ContentBytes)
	}
	if p["file_size"].(int) != len("replicas: 1\nimage: foo:v1\n") {
		t.Errorf("file_size = %v", p["file_size"])
	}
	observation, ok := res.Observation.(ReadObservation)
	if !ok || observation.LineStart != 1 || observation.LineEnd != 2 ||
		observation.ByteStart != 0 || observation.ByteEnd != len("replicas: 1\nimage: foo:v1\n") {
		t.Fatalf("observation = %#v", res.Observation)
	}
	// Second read is served from the cache: no extra ReadFile call.
	if repo.reads != 1 {
		t.Fatalf("reads = %d after first read, want 1", repo.reads)
	}
	cached := tool.Dispatch(context.Background(), env, mustJSON(withPrimary(map[string]interface{}{"path": "config/dev.yaml"})))
	if repo.reads != 1 {
		t.Errorf("reads = %d after cached read, want still 1", repo.reads)
	}
	if cached.ContentBytes != len("replicas: 1\nimage: foo:v1\n") {
		t.Errorf("cached content bytes = %d", cached.ContentBytes)
	}

	sl := dispatch(t, tool, env, withPrimary(withPrimary(map[string]interface{}{"path": "config/dev.yaml", "offset": 10, "length": 6})))
	if sl["content"] != "1\nimag" {
		t.Errorf("sliced content = %q, want \"1\\nimag\"", sl["content"])
	}
}

func TestReadRepoFile_NotFound(t *testing.T) {
	env := envFor(sampleRepo())
	res := (&readTool{}).Dispatch(context.Background(), env, mustJSON(withPrimary(map[string]interface{}{"path": "nope.txt"})))
	if _, hasErr := res.Payload["error"]; !hasErr {
		t.Errorf("expected error payload for missing file, got %v", res.Payload)
	}
}

func TestGrepRepo_FindsSymbolAndReportsLocation(t *testing.T) {
	env := envFor(sampleRepo())
	p := dispatch(t, &grepTool{}, env, map[string]interface{}{
		"source_id": tools.PrimarySourceID,
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

func TestGrepRepoReturnsPrivateCanonicalMatchRanges(t *testing.T) {
	env := envFor(sampleRepo())
	result := (&grepTool{}).Dispatch(context.Background(), env, mustJSON(map[string]interface{}{
		"source_id": tools.PrimarySourceID,
		"pattern":   "timeout bug", "path_glob": "*.go", "context_lines": 2,
	}))
	observation, ok := result.Observation.(GrepObservation)
	if !ok || len(observation.Matches) != 1 {
		t.Fatalf("observation=%T %+v", result.Observation, result.Observation)
	}
	match := observation.Matches[0]
	if match.SourceID != tools.PrimarySourceID || match.Path != "pkg/cloud/services/vm.go" || match.LineStart != 1 || match.LineEnd != 4 {
		t.Fatalf("match=%+v", match)
	}
	if result.ContentBytes == 0 {
		t.Fatal("content-bearing grep reported zero content bytes")
	}
}

func TestGrepRepoContextLinesDefaultAndExplicitZero(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         map[string]interface{}
		want         GrepMatchObservation
		contextLines int
	}{
		{
			name:         "omitted",
			args:         map[string]interface{}{"source_id": tools.PrimarySourceID, "pattern": "timeout bug", "path_glob": "*.go"},
			want:         GrepMatchObservation{SourceID: tools.PrimarySourceID, Path: "pkg/cloud/services/vm.go", LineStart: 1, LineEnd: 4},
			contextLines: 2,
		},
		{
			name:         "explicit zero",
			args:         map[string]interface{}{"source_id": tools.PrimarySourceID, "pattern": "timeout bug", "path_glob": "*.go", "context_lines": 0},
			want:         GrepMatchObservation{SourceID: tools.PrimarySourceID, Path: "pkg/cloud/services/vm.go", LineStart: 3, LineEnd: 3},
			contextLines: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := (&grepTool{}).Dispatch(context.Background(), envFor(sampleRepo()), mustJSON(tc.args))
			observation, ok := result.Observation.(GrepObservation)
			if !ok || len(observation.Matches) != 1 || observation.Matches[0] != tc.want {
				t.Fatalf("observation=%T %+v, want %+v", result.Observation, result.Observation, tc.want)
			}
			call := observation.Call
			wantRanges := []tools.GrepRangeObservation{{SelectorID: tc.want.SourceID, Path: tc.want.Path, LineStart: tc.want.LineStart, LineEnd: tc.want.LineEnd}}
			if call.SelectorID != tools.PrimarySourceID || call.PathFilter != "*.go" || call.ContextLines != tc.contextLines || call.MaxMatches != 30 || call.MatchCount != 1 || call.Outcome != tools.GrepOutcomeMatched || !reflect.DeepEqual(call.ReturnedRanges, wantRanges) {
				t.Fatalf("call=%+v, want ranges %+v", call, wantRanges)
			}
		})
	}
}

func TestGrepRepoRetainsZeroMatchAndErrorTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    map[string]interface{}
		outcome string
	}{
		{name: "zero matches", args: map[string]interface{}{"source_id": tools.PrimarySourceID, "pattern": "missing", "path_glob": "*.go"}, outcome: tools.GrepOutcomeZeroMatches},
		{name: "invalid regex", args: map[string]interface{}{"source_id": tools.PrimarySourceID, "pattern": "(", "path_glob": "*.go"}, outcome: tools.GrepOutcomeError},
		{name: "unknown source", args: map[string]interface{}{"source_id": "unknown", "pattern": "match", "path_glob": "*.go"}, outcome: tools.GrepOutcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := (&grepTool{}).Dispatch(context.Background(), envFor(sampleRepo()), mustJSON(tc.args))
			observation, ok := result.Observation.(GrepObservation)
			if !ok || observation.Call.Outcome != tc.outcome || observation.Call.MatchCount != 0 || len(observation.Call.ReturnedRanges) != 0 {
				t.Fatalf("observation=%T %+v", result.Observation, result.Observation)
			}
			if observation.Call.PathFilter != "*.go" || observation.Call.ContextLines != 2 || observation.Call.MaxMatches != 30 {
				t.Fatalf("call=%+v", observation.Call)
			}
		})
	}
}

func TestGrepRepoTelemetryRangesIgnoreGroundingCaps(t *testing.T) {
	t.Run("more than grounding range cap", func(t *testing.T) {
		repo := &fakeRepo{files: map[string]string{"many.go": strings.Repeat("match\n", grepEvidenceMaxHits+6)}}
		result := (&grepTool{}).Dispatch(context.Background(), envFor(repo), mustJSON(withPrimary(map[string]interface{}{
			"pattern": "match", "path_glob": "*.go", "context_lines": 0, "max_matches": 100,
		})))
		observation := result.Observation.(GrepObservation)
		if len(observation.Matches) != grepEvidenceMaxHits {
			t.Fatalf("grounding ranges=%d, want %d", len(observation.Matches), grepEvidenceMaxHits)
		}
		if len(observation.Call.ReturnedRanges) != grepEvidenceMaxHits+6 || observation.Call.MatchCount != grepEvidenceMaxHits+6 || observation.Call.ResultTruncated {
			t.Fatalf("call=%+v", observation.Call)
		}
	})

	t.Run("context exceeds grounding byte cap", func(t *testing.T) {
		repo := &fakeRepo{files: map[string]string{"large.go": "match " + strings.Repeat("x", grepEvidenceMaxBytes+1) + "\n"}}
		result := (&grepTool{}).Dispatch(context.Background(), envFor(repo), mustJSON(withPrimary(map[string]interface{}{
			"pattern": "match", "path_glob": "*.go", "context_lines": 0,
		})))
		observation := result.Observation.(GrepObservation)
		if len(observation.Matches) != 0 || len(observation.Call.ReturnedRanges) != 1 || observation.Call.MatchCount != 1 {
			t.Fatalf("observation=%+v", observation)
		}
	})
}

func TestGrepRepoReadFailuresAreReportedHonestly(t *testing.T) {
	t.Run("all reads fail", func(t *testing.T) {
		repo := &fakeRepo{files: map[string]string{"broken.go": "match\n"}, readErrors: map[string]error{"broken.go": errors.New("read failed")}}
		result := (&grepTool{}).Dispatch(context.Background(), envFor(repo), mustJSON(withPrimary(map[string]interface{}{
			"pattern": "match", "path_glob": "*.go",
		})))
		observation := result.Observation.(GrepObservation).Call
		if result.Payload["error"] == nil || observation.Outcome != tools.GrepOutcomeError || observation.FilesAttempted != 1 || observation.FilesScanned != 0 || observation.FileReadErrors != 1 {
			t.Fatalf("payload=%v observation=%+v", result.Payload, observation)
		}
	})

	t.Run("partial read failure", func(t *testing.T) {
		repo := &fakeRepo{
			files:      map[string]string{"broken.go": "match\n", "good.go": "match\n"},
			readErrors: map[string]error{"broken.go": errors.New("read failed")},
		}
		result := (&grepTool{}).Dispatch(context.Background(), envFor(repo), mustJSON(withPrimary(map[string]interface{}{
			"pattern": "match", "path_glob": "*.go",
		})))
		observation := result.Observation.(GrepObservation).Call
		if result.Payload["error"] != nil || observation.Outcome != tools.GrepOutcomeMatched || observation.FilesAttempted != 2 || observation.FilesScanned != 1 || observation.FileReadErrors != 1 || observation.MatchCount != 1 {
			t.Fatalf("payload=%v observation=%+v", result.Payload, observation)
		}
	})

	t.Run("failed reads respect file cap", func(t *testing.T) {
		files := map[string]string{}
		readErrors := map[string]error{}
		for i := 0; i < maxGrepFiles+1; i++ {
			path := fmt.Sprintf("broken-%02d.go", i)
			files[path] = "match\n"
			readErrors[path] = errors.New("read failed")
		}
		repo := &fakeRepo{files: files, readErrors: readErrors}
		result := (&grepTool{}).Dispatch(context.Background(), envFor(repo), mustJSON(withPrimary(map[string]interface{}{
			"pattern": "match", "path_glob": "*.go",
		})))
		observation := result.Observation.(GrepObservation).Call
		if result.Payload["error"] == nil || observation.FilesAttempted != maxGrepFiles || observation.FileReadErrors != maxGrepFiles || !observation.FileScanTruncated || repo.reads != maxGrepFiles {
			t.Fatalf("payload=%v observation=%+v reads=%d", result.Payload, observation, repo.reads)
		}
	})
}

func TestGrepRepo_GlobNarrowsScope(t *testing.T) {
	env := envFor(sampleRepo())
	// image: appears in both yaml files but no go files. Narrow to config.
	p := dispatch(t, &grepTool{}, env, map[string]interface{}{
		"source_id": tools.PrimarySourceID,
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
	res := (&grepTool{}).Dispatch(context.Background(), env, mustJSON(withPrimary(map[string]interface{}{"pattern": "("})))
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

func TestCompleteReadLineRange(t *testing.T) {
	content := "first\nsecond\nthird"
	for _, tc := range []struct {
		offset, end, start, finish, byteStart, byteEnd int
		ok                                             bool
	}{
		{0, len(content), 1, 3, 0, len(content), true},
		{2, 13, 2, 2, 4, 11, true},
		{1, 4, 0, 0, 0, 0, false},
	} {
		start, finish, byteStart, byteEnd, ok := completeReadLineRange(content, tc.offset, tc.end)
		if start != tc.start || finish != tc.finish || byteStart != tc.byteStart || byteEnd != tc.byteEnd || ok != tc.ok {
			t.Fatalf("%+v got %d %d %d %d %t", tc, start, finish, byteStart, byteEnd, ok)
		}
	}
}

func TestRepoToolsRequireSourceIDBeforeReaderAccess(t *testing.T) {
	repo := sampleRepo()
	env := envFor(repo)
	cases := []struct {
		name string
		tool tools.Tool
		args map[string]interface{}
	}{
		{name: "list", tool: &listTool{}, args: map[string]interface{}{"path": ""}},
		{name: "read", tool: &readTool{}, args: map[string]interface{}{"path": "README.md"}},
		{name: "grep", tool: &grepTool{}, args: map[string]interface{}{"pattern": "hello"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.tool.Dispatch(context.Background(), env, mustJSON(tc.args))
			if result.Payload["error"] != "source_id is required" {
				t.Fatalf("payload=%v", result.Payload)
			}
		})
	}
	result := (&readTool{}).Dispatch(context.Background(), env, mustJSON(map[string]interface{}{"source_id": "unknown", "path": "README.md"}))
	if result.Payload["error"] != `unknown source_id "unknown"` {
		t.Fatalf("unknown source payload=%v", result.Payload)
	}
	if repo.lists != 0 || repo.reads != 0 {
		t.Fatalf("reader accessed for invalid selector: lists=%d reads=%d", repo.lists, repo.reads)
	}
}

func TestRepoToolCacheKeysIncludeSourceID(t *testing.T) {
	client := &fakeRepo{files: map[string]string{"same.go": "client\n"}}
	server := &fakeRepo{files: map[string]string{"same.go": "server\n"}}
	catalog, err := tools.NewSourceCatalog("client", []tools.RepoSource{
		{ID: "client", Owner: "kubernetes", Name: "kubernetes", Revision: strings.Repeat("1", 40), Reader: client},
		{ID: "server", Owner: "kubernetes", Name: "kubernetes", Revision: strings.Repeat("2", 40), Reader: server},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := &tools.Env{Sources: catalog, Cache: tools.NewCache()}
	tool := &readTool{}
	clientResult := tool.Dispatch(context.Background(), env, mustJSON(map[string]interface{}{"source_id": "client", "path": "same.go"}))
	serverResult := tool.Dispatch(context.Background(), env, mustJSON(map[string]interface{}{"source_id": "server", "path": "same.go"}))
	if clientResult.Payload["content"] != "client\n" || serverResult.Payload["content"] != "server\n" {
		t.Fatalf("client=%v server=%v", clientResult.Payload, serverResult.Payload)
	}
	if client.reads != 1 || server.reads != 1 {
		t.Fatalf("reads client=%d server=%d", client.reads, server.reads)
	}
	clientAgain := tool.Dispatch(context.Background(), env, mustJSON(map[string]interface{}{"source_id": "client", "path": "same.go"}))
	if clientAgain.Payload["content"] != "client\n" || client.reads != 1 || server.reads != 1 {
		t.Fatalf("cache contamination: client=%v reads=%d/%d", clientAgain.Payload, client.reads, server.reads)
	}
}

func TestRepoToolSchemasRequireSourceID(t *testing.T) {
	for _, tool := range []tools.Tool{&listTool{}, &readTool{}, &grepTool{}} {
		required, _ := tool.Schema().Function.Parameters["required"].([]string)
		found := false
		for _, name := range required {
			found = found || name == "source_id"
		}
		if !found {
			t.Fatalf("%s required=%v", tool.Name(), required)
		}
	}
}

func TestGrepRepoDoesNotObserveTruncatedTrailingLine(t *testing.T) {
	prefix := "match " + strings.Repeat("x", grepMaxBytes)
	env := envFor(&fakeRepo{files: map[string]string{"large.txt": prefix + "\ncomplete\n"}})
	result := (&grepTool{}).Dispatch(context.Background(), env, mustJSON(withPrimary(map[string]interface{}{"pattern": "match", "path_glob": "*.txt"})))
	observation, _ := result.Observation.(GrepObservation)
	if len(observation.Matches) != 0 {
		t.Fatalf("truncated line was observed as complete: %+v", observation.Matches)
	}
}
