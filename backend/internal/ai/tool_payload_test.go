package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/ai/tools/repotree"
	"github.com/willie-yao/aster/backend/internal/ai/transport"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/storage"
)

func boundedToolState(t *testing.T, browser artifacts.Browser) *agentState {
	t.Helper()
	registry, enabled := newTestRegistry(t)
	return &agentState{
		browser: browser, registry: registry, enabledTools: enabled,
		opts: AgenticOptions{ModelByteBudget: 1_000_000, GCSByteBudget: 1_000_000}, startTime: time.Now(),
		readArtifactsFull: map[string]bool{}, readArtifactsBase: map[string]bool{},
		analysisEvidence: map[string]*analysisChatEvidence{}, analysisEvidenceBudget: analysisChatEvidenceFallbackMaxBytes,
		sourceEvidenceByPath: map[analysisChatSourceEvidenceKey]*analysisChatEvidence{},
	}
}

func assertBoundedToolEnvelope(t *testing.T, envelope string, payload map[string]any) {
	t.Helper()
	if len(envelope) > agenticToolBudget || !json.Valid([]byte(envelope)) {
		t.Fatalf("invalid or oversized envelope: bytes=%d valid=%v", len(envelope), json.Valid([]byte(envelope)))
	}
	if decoded := modelVisibleToolPayload(envelope); decoded == nil || !reflect.DeepEqual(decoded, payload) {
		t.Fatal("returned payload differs from model-visible JSON")
	}
}

func TestBoundedLocalBackendTailRetainsFinalFailure(t *testing.T) {
	root := t.TempDir()
	content := "excluded leading evidence\n" + strings.Repeat(strings.Repeat(`"quoted\log"`, 12)+"\n", 180) + "FINAL failure: node never joined"
	if err := os.Mkdir(filepath.Join(root, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "build", "build-log.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := storage.NewLocalBackend(root, "")
	if err != nil {
		t.Fatal(err)
	}
	browser := artifacts.NewUncachedBackendBrowser(backend, "bucket", "build/", "test")
	rawTail, err := browser.Tail(t.Context(), "build-log.txt", 500, 32*1024-256)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawTail.Content), "excluded leading evidence") {
		t.Fatal("test tail fetch did not include the leading evidence")
	}
	state := boundedToolState(t, browser)
	envelope, payload := dispatchAgenticToolWithPayload(t.Context(), state, transport.ToolCall{
		Function: transport.FunctionCall{Name: "tail_artifact", Arguments: `{"path":"build-log.txt"}`},
	}, nil)
	assertBoundedToolEnvelope(t, envelope, payload)
	visible, _ := payload["content"].(string)
	if payload["truncated"] != true || !strings.HasSuffix(visible, "FINAL failure: node never joined") {
		t.Fatalf("tail lost final failure: bytes=%d truncated=%v", len(visible), payload["truncated"])
	}
	if strings.Contains(visible, "excluded leading evidence") || len(visible) >= len(rawTail.Content) {
		t.Fatal("tail did not exclude leading content")
	}
	if payload["lines_returned"] != float64(len(strings.Split(visible, "\n"))) ||
		payload["file_size"] != float64(len(content)) || state.gcsBytes != len(rawTail.Content) {
		t.Fatalf("tail accounting: lines=%v size=%v fetched=%d", payload["lines_returned"], payload["file_size"], state.gcsBytes)
	}
	entry := state.analysisEvidence["build-log.txt"]
	if entry == nil || !strings.Contains(strings.Join(entry.Segments, "\n"), "FINAL failure") ||
		strings.Contains(strings.Join(entry.Segments, "\n"), "excluded leading evidence") {
		t.Fatalf("tail evidence does not match visible content: %+v", entry)
	}
	if _, count := attributeAnalysisChatQuote(entry, "excluded leading evidence"); count != 0 {
		t.Fatal("excluded leading tail content grounded a citation")
	}
	if _, count := attributeAnalysisChatQuote(entry, "FINAL failure: node never joined"); count != 1 {
		t.Fatal("retained final failure could not ground a citation")
	}
}

func TestBoundedArtifactReadMeasuresEscapedJSON(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("\"\x00\\\n", 4000),
		strings.Repeat("\u2028\u2029", 3000) + "世界",
	} {
		path := strings.Repeat(`long"quoted\path`, 100) + "/log.txt"
		state := boundedToolState(t, &fakeBrowser{files: map[string][]byte{path: []byte(text)}})
		args, err := json.Marshal(map[string]any{"path": path, "offset": 3, "length": 16384})
		if err != nil {
			t.Fatal(err)
		}
		envelope, payload := dispatchAgenticToolWithPayload(t.Context(), state, transport.ToolCall{
			Function: transport.FunctionCall{Name: "read_artifact", Arguments: string(args)},
		}, nil)
		assertBoundedToolEnvelope(t, envelope, payload)
		visible, _ := payload["content"].(string)
		if visible == "" || payload["truncated"] != true || !utf8.ValidString(visible) {
			t.Fatalf("read prefix missing or invalid: bytes=%d truncated=%v", len(visible), payload["truncated"])
		}
		if payload["length"] != float64(len(visible)) || payload["next_offset"] != float64(3+len(visible)) ||
			payload["file_size"] != float64(len(text)) || state.gcsBytes != min(len(text)-3, 16384) {
			t.Fatalf("read accounting: length=%v next=%v size=%v fetched=%d",
				payload["length"], payload["next_offset"], payload["file_size"], state.gcsBytes)
		}
		if !strings.HasPrefix(text[3:], visible) {
			t.Fatal("read did not retain a source prefix")
		}
	}
}

func TestBoundedToolErrorsNeverCreateEvidence(t *testing.T) {
	for _, name := range []string{"disabled", "missing", "tail line too long", "grep match too long", "model budget", "GCS budget"} {
		t.Run(name, func(t *testing.T) {
			state := boundedToolState(t, &fakeBrowser{files: map[string][]byte{
				"build-log.txt": []byte(strings.Repeat(`"\`, agenticToolBudget)),
			}})
			call := transport.ToolCall{Function: transport.FunctionCall{Name: "tail_artifact", Arguments: `{"path":"build-log.txt"}`}}
			switch name {
			case "disabled":
				state.enabledTools = []string{}
			case "missing":
				call.Function.Arguments = `{"path":"missing.log"}`
			case "grep match too long":
				// A long path alone can make even one complete match too large.
				path := strings.Repeat("p", agenticToolBudget)
				state.browser = &fakeBrowser{files: map[string][]byte{path: []byte("MATCH\n")}}
				args, _ := json.Marshal(map[string]any{"path": path, "pattern": "MATCH"})
				call.Function.Name, call.Function.Arguments = "grep_artifact", string(args)
			case "model budget":
				state.modelBytes = state.opts.ModelByteBudget
			case "GCS budget":
				state.gcsBytes = state.opts.GCSByteBudget
			}
			envelope, payload := dispatchAgenticToolWithPayload(t.Context(), state, call, nil)
			assertBoundedToolEnvelope(t, envelope, payload)
			if _, failed := payload["error"]; !failed {
				t.Fatal("expected an explicit tool error")
			}
			if len(state.readArtifactsFull) != 0 || len(state.evidenceArtifactsFull) != 0 ||
				len(state.evidenceContentByPath) != 0 || len(state.analysisEvidence) != 0 {
				t.Fatal("failed tool earned read or evidence credit")
			}
		})
	}
}

func TestBoundedUnhandledPayloadAndErrors(t *testing.T) {
	for _, payload := range []map[string]any{
		{"files": []string{strings.Repeat("x", agenticToolBudget)}},
		{"error": strings.Repeat(`"\`, agenticToolBudget)},
		{"invalid": make(chan int)},
	} {
		result := tools.Result{Payload: payload}
		boundToolResult("list_artifacts", &result)
		envelope := marshalToolPayload(result.Payload)
		visible := modelVisibleToolPayload(envelope)
		assertBoundedToolEnvelope(t, envelope, visible)
		if _, failed := visible["error"]; !failed {
			t.Fatal("unhandled payload did not return a bounded error")
		}
	}
	for _, raw := range []string{`{"content":"unfinished`, strings.Repeat("x", agenticToolBudget+1)} {
		envelope := capJSON(raw)
		assertBoundedToolEnvelope(t, envelope, modelVisibleToolPayload(envelope))
	}
	envelope := toolErrJSON(strings.Repeat(`"\`, agenticToolBudget))
	assertBoundedToolEnvelope(t, envelope, modelVisibleToolPayload(envelope))
}

type boundedListingBrowser struct {
	*fakeBrowser
	listing *artifacts.Listing
}

func (b *boundedListingBrowser) List(context.Context, string) (*artifacts.Listing, error) {
	return b.listing, nil
}

func TestBoundedToolListingsRetainOrderedPrefixes(t *testing.T) {
	for _, tool := range []string{"list_artifacts", "list_repo_tree"} {
		for _, test := range []struct {
			name        string
			dirs, files int
			truncated   bool
		}{
			{name: "dirs-only", dirs: 100, truncated: true},
			{name: "files-only", files: 100, truncated: true},
			{name: "mixed-directories-fill", dirs: 100, files: 20, truncated: true},
			{name: "mixed-files-fill", dirs: 3, files: 100, truncated: true},
			{name: "small", dirs: 2, files: 3},
			{name: "empty"},
		} {
			t.Run(tool+"/"+test.name, func(t *testing.T) {
				listing := &artifacts.Listing{Dir: "logs/", Dirs: []string{}, Files: []artifacts.FileInfo{}}
				sourceFiles := map[string]string{}
				for i := range test.dirs {
					name := fmt.Sprintf("dir-%03d-%s", i, strings.Repeat(`"quoted\name"`, 40))
					listing.Dirs = append(listing.Dirs, name+"/")
					sourceFiles["logs/"+name+"/entry.go"] = ""
				}
				for i := range test.files {
					name := fmt.Sprintf("file-%03d-%s.txt", i, strings.Repeat(`"quoted\name"`, 40))
					listing.Files = append(listing.Files, artifacts.FileInfo{Name: name, Size: int64(1000 + i)})
					sourceFiles["logs/"+name] = ""
				}
				state := boundedToolState(t, &boundedListingBrowser{listing: listing})
				if tool == "list_repo_tree" {
					state = boundedSourceToolState(t, "selected", "", "")
					state.sources = testSourceCatalog(t, "selected", tools.RepoSource{
						ID: "selected", Owner: "example", Name: "project", Revision: strings.Repeat("1", 40),
						Reader: &fakeSourceRepo{files: sourceFiles},
					})
				}
				observations := 0
				state.sourceObserver = func(SourceEvidenceObservation) { observations++ }
				call := transport.ToolCall{Function: transport.FunctionCall{
					Name: tool, Arguments: `{"path":"logs","source_id":"selected"}`,
				}}
				full := state.registry.Dispatch(t.Context(), &tools.Env{
					Browser: state.browser, Sources: state.sources,
				}, tool, json.RawMessage(call.Function.Arguments))
				raw, err := json.Marshal(full.Payload)
				if err != nil {
					t.Fatal(err)
				}
				expected := modelVisibleToolPayload(string(raw))
				expectedDirs, _ := expected["dirs"].([]any)
				expectedFiles, _ := expected["files"].([]any)

				envelope, payload := dispatchAgenticToolWithPayload(t.Context(), state, call, nil)
				assertBoundedToolEnvelope(t, envelope, payload)
				if _, failed := payload["error"]; failed {
					t.Fatal("listing lost useful navigation entries")
				}
				if (payload["truncated"] == true) != test.truncated {
					t.Fatalf("listing truncated = %v, want %v", payload["truncated"], test.truncated)
				}
				if payload["dir"] != expected["dir"] || payload["source_id"] != expected["source_id"] {
					t.Fatal("listing lost directory or source identity")
				}
				dirs, _ := payload["dirs"].([]any)
				files, _ := payload["files"].([]any)
				if len(dirs) > len(expectedDirs) || len(files) > len(expectedFiles) {
					t.Fatal("listing gained entries")
				}
				for i, entry := range dirs {
					if !reflect.DeepEqual(entry, expectedDirs[i]) {
						t.Fatalf("directory %d is not the original prefix entry", i)
					}
				}
				for i, entry := range files {
					if !reflect.DeepEqual(entry, expectedFiles[i]) {
						t.Fatalf("file %d lost its original name or size", i)
					}
				}
				if len(files) > 0 && len(dirs) != len(expectedDirs) {
					t.Fatal("files displaced higher-priority directories")
				}
				if len(state.readArtifactsFull) != 0 || len(state.readSourceFull) != 0 ||
					len(state.analysisEvidence) != 0 || len(state.sourceEvidenceByPath) != 0 ||
					len(state.sourceContentByPath) != 0 || len(state.evidenceArtifactsFull) != 0 ||
					len(state.evidenceContentByPath) != 0 || state.evidenceRevision != 0 ||
					state.gcsBytes != 0 || observations != 0 {
					t.Fatal("listing earned content-read or evidence credit")
				}
				if !test.truncated {
					for key, value := range expected {
						if !reflect.DeepEqual(payload[key], value) {
							t.Fatalf("small listing changed field %q", key)
						}
					}
					return
				}
				if len(dirs)+len(files) == 0 || len(dirs)+len(files) >= test.dirs+test.files {
					t.Fatal("oversized listing did not retain a useful proper prefix")
				}
				if len(dirs) < len(expectedDirs) {
					payload["dirs"] = expectedDirs[:len(dirs)+1]
				} else {
					payload["files"] = expectedFiles[:len(files)+1]
				}
				if toolPayloadFits(payload) {
					t.Fatal("bounded listing unnecessarily dropped an entry that fits")
				}
			})
		}
	}
}

func TestBoundedToolListingsRejectUnrepresentableEntries(t *testing.T) {
	for _, tool := range []string{"list_artifacts", "list_repo_tree"} {
		for _, test := range []struct {
			name    string
			payload map[string]any
		}{
			{name: "metadata", payload: map[string]any{
				"dir": strings.Repeat("x", agenticToolBudget), "dirs": []string{"small/"}, "files": []string{},
			}},
			{name: "directory", payload: map[string]any{
				"dirs": []string{strings.Repeat("x", agenticToolBudget)}, "files": []string{"small.txt"},
			}},
			{name: "file", payload: map[string]any{
				"dirs": []string{}, "files": []string{strings.Repeat("x", agenticToolBudget)},
			}},
		} {
			t.Run(tool+"/"+test.name, func(t *testing.T) {
				payload := test.payload
				if tool == "list_artifacts" {
					files, _ := payload["files"].([]string)
					entries := make([]map[string]any, 0, len(files))
					for _, file := range files {
						entries = append(entries, map[string]any{"name": file, "size": 1})
					}
					payload["files"] = entries
				}
				payload["source_id"] = "selected"
				addToolEnvelopeMetadata(boundedToolState(t, nil), payload)
				result := tools.Result{Payload: payload}
				boundToolResult(tool, &result)
				envelope := marshalToolPayload(result.Payload)
				visible := modelVisibleToolPayload(envelope)
				assertBoundedToolEnvelope(t, envelope, visible)
				if _, failed := visible["error"]; !failed || visible["truncated"] != true {
					t.Fatal("unrepresentable listing did not retain the bounded error fallback")
				}
			})
		}
	}
}

func TestUnderBudgetToolPayloadsAreUnchanged(t *testing.T) {
	for _, name := range []string{"read_artifact", "tail_artifact", "grep_artifact", "read_repo_file", "grep_repo", "list_artifacts"} {
		t.Run(name, func(t *testing.T) {
			payload := map[string]any{
				"path": "log.txt", "file_size": 5, "offset": 0, "length": 5, "content": "hello",
				"matches": []map[string]any{{"line": 1, "context": []string{"hello"}}},
			}
			before, _ := json.Marshal(payload)
			observation := repotree.ReadObservation{SourceID: "primary", Path: "log.txt", LineStart: 1, LineEnd: 1}
			result := tools.Result{Payload: payload, Observation: observation, BytesFetched: 5}
			boundToolResult(name, &result)
			after, _ := json.Marshal(result.Payload)
			if string(before) != string(after) || !reflect.DeepEqual(result.Observation, observation) || result.BytesFetched != 5 {
				t.Fatal("under-budget result changed")
			}
		})
	}
}

func boundedSourceToolState(t *testing.T, sourceID, path, text string) *agentState {
	t.Helper()
	state := boundedToolState(t, nil)
	repotree.Register(state.registry)
	var err error
	state.enabledTools, err = state.registry.Enable([]string{"filesystem", "repotree"})
	if err != nil {
		t.Fatal(err)
	}
	state.sources = testSourceCatalog(t, sourceID, tools.RepoSource{
		ID: sourceID, Owner: "example", Name: "project", Revision: strings.Repeat("1", 40),
		Reader: &fakeSourceRepo{files: map[string]string{path: text}},
	})
	return state
}

func TestBoundedSourceReadPreservesCoordinatesAndContinuation(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run(fmt.Sprintf("newline-%q", newline), func(t *testing.T) {
			line := strings.Repeat("\"\x00\\", 200)
			content := "header" + newline + strings.Repeat(line+newline, 20) + "hidden final source line" + newline
			state := boundedSourceToolState(t, "selected", "pkg/source.go", content)
			var observations []SourceEvidenceObservation
			state.sourceObserver = func(value SourceEvidenceObservation) { observations = append(observations, value) }
			args, _ := json.Marshal(map[string]any{
				"source_id": "selected", "path": "pkg/source.go", "offset": len("header" + newline), "length": 16384,
			})
			call := transport.ToolCall{Function: transport.FunctionCall{Name: "read_repo_file", Arguments: string(args)}}
			envelope, payload := dispatchAgenticToolWithPayload(t.Context(), state, call, nil)
			assertBoundedToolEnvelope(t, envelope, payload)
			if payload["truncated"] != true || payload["source_id"] != "selected" || payload["line_start"] != float64(2) {
				t.Fatalf("source read metadata = %v", payload)
			}
			end := int(payload["line_end"].(float64))
			next := len("header"+newline) + (end-1)*len(line+newline)
			if end < 2 || end >= 21 || payload["next_offset"] != float64(next) ||
				payload["length"] != float64(next-len("header"+newline)) || payload["file_size"] != float64(len(content)) {
				t.Fatalf("source read coordinates: end=%d next=%v length=%v size=%v",
					end, payload["next_offset"], payload["length"], payload["file_size"])
			}
			entry := state.sourceEvidenceByPath[analysisChatSourceEvidenceKey{SourceID: "selected", Path: "pkg/source.go"}]
			if entry == nil || len(entry.Lines) != end-1 || entry.Lines[end] != line || entry.Lines[end+1] != "" {
				t.Fatalf("source lines do not match trimmed observation: %+v", entry)
			}
			if len(observations) != 1 || observations[0].SourceID != "selected" ||
				observations[0].LineStart != 2 || observations[0].LineEnd != end {
				t.Fatalf("source observations = %+v", observations)
			}
			if strings.Contains(strings.Join(state.sourceContentByPath["pkg/source.go"], "\n"), "hidden final source line") {
				t.Fatal("hidden source line entered grounding content")
			}
			source := &analysisChatSourceCitationContext{Catalog: state.sources, Evidence: state.sourceEvidenceByPath}
			citation := analysischat.Citation{
				Repository: "example/project", Revision: strings.Repeat("1", 40), Path: "pkg/source.go",
				LineStart: end, LineEnd: end, Quote: line,
			}
			if failure := validateAnalysisChatSourceCitation(&citation, source, 0); failure != nil {
				t.Fatalf("retained source coordinates failed validation: %+v", failure)
			}
			citation.LineStart, citation.LineEnd, citation.Quote = 22, 22, "hidden final source line"
			if failure := validateAnalysisChatSourceCitation(&citation, source, 0); failure == nil {
				t.Fatal("hidden source coordinates passed validation")
			}
			args, _ = json.Marshal(map[string]any{
				"source_id": "selected", "path": "pkg/source.go", "offset": next, "length": 16384,
			})
			call.Function.Arguments = string(args)
			_, continued := dispatchAgenticToolWithPayload(t.Context(), state, call, nil)
			if continued["line_start"] != float64(end+1) {
				t.Fatalf("continuation skipped or repeated lines: %v", continued["line_start"])
			}
		})
	}
}

func TestBoundedPartialSourceLineHasNoInventedCoordinates(t *testing.T) {
	text := strings.Repeat("\"\x00\\", 10000)
	state := boundedSourceToolState(t, "primary", "long.go", text)
	envelope, payload := dispatchAgenticToolWithPayload(t.Context(), state, transport.ToolCall{
		Function: transport.FunctionCall{Name: "read_repo_file", Arguments: `{"source_id":"primary","path":"long.go","length":16384}`},
	}, nil)
	assertBoundedToolEnvelope(t, envelope, payload)
	visible, _ := payload["content"].(string)
	if visible == "" || payload["partial_line"] != true || payload["truncated"] != true ||
		payload["line_start"] != nil || payload["line_end"] != nil {
		t.Fatalf("partial source metadata: bytes=%d partial=%v truncated=%v", len(visible), payload["partial_line"], payload["truncated"])
	}
	if payload["length"] != float64(len(visible)) || payload["next_offset"] != float64(len(visible)) {
		t.Fatalf("partial source offsets: length=%v next=%v", payload["length"], payload["next_offset"])
	}
	entry := state.sourceEvidenceByPath[analysisChatSourceEvidenceKey{SourceID: "primary", Path: "long.go"}]
	if entry == nil || len(entry.Lines) != 0 || entry.Segments[0] != visible {
		t.Fatal("partial line acquired invented coordinates or hidden text")
	}
}

func TestBoundedSourceGrepDropsHiddenObservations(t *testing.T) {
	content := "MATCH retained\n" + strings.Repeat("MATCH "+strings.Repeat("x", 900)+"\n", 98) + "MATCH hidden source\n"
	state := boundedSourceToolState(t, "selected", "pkg/source.go", content)
	var observations []SourceEvidenceObservation
	state.sourceObserver = func(value SourceEvidenceObservation) { observations = append(observations, value) }
	envelope, payload := dispatchAgenticToolWithPayload(t.Context(), state, transport.ToolCall{
		Function: transport.FunctionCall{
			Name: "grep_repo", Arguments: `{"source_id":"selected","pattern":"MATCH","context_lines":0,"max_matches":100}`,
		},
	}, nil)
	assertBoundedToolEnvelope(t, envelope, payload)
	matches := analysisChatEvidenceMatches(payload["matches"])
	if payload["truncated"] != true || payload["source_id"] != "selected" || len(matches) == 0 || len(matches) >= 100 {
		t.Fatal("source grep did not retain bounded matches")
	}
	if len(observations) != len(matches) {
		t.Fatalf("source observations=%d visible matches=%d", len(observations), len(matches))
	}
	for i, observation := range observations {
		if observation.SourceID != "selected" || observation.Path != "pkg/source.go" ||
			observation.LineStart != i+1 || observation.LineEnd != i+1 {
			t.Fatalf("source observation %d = %+v", i, observation)
		}
	}
	entry := state.sourceEvidenceByPath[analysisChatSourceEvidenceKey{SourceID: "selected", Path: "pkg/source.go"}]
	if entry == nil || len(entry.Segments) != len(matches) || strings.Contains(strings.Join(entry.Segments, "\n"), "hidden source") {
		t.Fatal("source evidence includes hidden grep content")
	}
	source := &analysisChatSourceCitationContext{Catalog: state.sources, Evidence: state.sourceEvidenceByPath}
	for _, quote := range []string{"MATCH retained", "MATCH hidden source"} {
		citation := analysischat.Citation{
			Repository: "example/project", Revision: strings.Repeat("1", 40), Path: "pkg/source.go", Quote: quote,
		}
		failure := validateAnalysisChatSourceCitation(&citation, source, 0)
		if (failure == nil) != (quote == "MATCH retained") {
			t.Fatalf("source citation %q failure = %+v", quote, failure)
		}
	}
}

func TestBoundedNumberedSourceLineThatCannotFitReturnsError(t *testing.T) {
	state := boundedSourceToolState(t, "primary", "long.go", strings.Repeat("\x00", 16000)+"\n")
	var observations []SourceEvidenceObservation
	state.sourceObserver = func(value SourceEvidenceObservation) { observations = append(observations, value) }
	envelope, payload := dispatchAgenticToolWithPayload(t.Context(), state, transport.ToolCall{
		Function: transport.FunctionCall{Name: "read_repo_file", Arguments: `{"source_id":"primary","path":"long.go","length":16384}`},
	}, nil)
	assertBoundedToolEnvelope(t, envelope, payload)
	if _, failed := payload["error"]; !failed || payload["truncated"] != true {
		t.Fatal("a numbered source line that cannot fit did not return an explicit error")
	}
	if len(observations) != 0 || len(state.readSourceFull) != 0 ||
		len(state.sourceEvidenceByPath) != 0 || len(state.sourceContentByPath) != 0 {
		t.Fatal("unreturned source content earned evidence credit")
	}
}

func TestUnreadEvidenceMetadataCannotRestoreHiddenContent(t *testing.T) {
	state := boundedToolState(t, &fakeBrowser{files: map[string][]byte{"log.txt": []byte(strings.Repeat("\x00", 16384))}})
	state.skillSet = loadSkillsForTest(t, map[string]string{"pending": `
id: pending
triggers: ["failure"]
required_evidence:
  - id: unread
    any_of: ["other\\.log$"]
`})
	state.initialFailureSignal = "failure"
	state.initialEvidencePlan = []skills.PlannedSkill{{
		ID:               "pending",
		RequiredEvidence: []skills.PlannedEvidenceGroup{{ID: "unread", CandidatePaths: []string{"other.log"}}},
	}}
	if len(state.unreadEvidenceGroupIDs()) != 1 {
		t.Fatal("test setup has no unread evidence metadata")
	}
	envelope, payload := dispatchAgenticToolWithPayload(t.Context(), state, transport.ToolCall{
		Function: transport.FunctionCall{Name: "read_artifact", Arguments: `{"path":"log.txt","length":16384}`},
	}, nil)
	assertBoundedToolEnvelope(t, envelope, payload)
	if payload["unread_evidence_groups"] != nil || payload["content"] == "" || payload["truncated"] != true {
		t.Fatal("optional evidence metadata replaced the bounded content")
	}
	if len(envelope) < agenticToolBudget-8 {
		t.Fatalf("read left unexplained serialized headroom: %d bytes", len(envelope))
	}
}

func TestChatEvidenceRejectionPrecedesAllEvidenceRecording(t *testing.T) {
	state := boundedToolState(t, &fakeBrowser{files: map[string][]byte{"log.txt": []byte("hidden rejected read\n")}})
	evidence := map[string]*analysisChatEvidence{}
	call := transport.ToolCall{Function: transport.FunctionCall{Name: "read_artifact", Arguments: `{"path":"log.txt"}`}}
	envelope, payload := dispatchAgenticToolWithPayload(t.Context(), state, call, func(payload map[string]any) string {
		if _, accepted := recordAnalysisChatEvidence(evidence, call, payload, 1); !accepted {
			return "analysis chat evidence budget exhausted"
		}
		return ""
	})
	assertBoundedToolEnvelope(t, envelope, payload)
	if _, failed := payload["error"]; !failed || state.gcsBytes == 0 {
		t.Fatal("rejected read did not return an error with actual fetch accounting")
	}
	if len(evidence) != 0 || len(state.readArtifactsFull) != 0 || len(state.evidenceArtifactsFull) != 0 ||
		len(state.evidenceContentByPath) != 0 || len(state.analysisEvidence) != 0 {
		t.Fatal("budget-rejected envelope retained hidden evidence")
	}
	if issue := evidenceCitationIssue(models.EvidenceCitation{Path: "log.txt", Quote: "hidden rejected read"}, evidence); issue == "" {
		t.Fatal("budget-rejected content grounded a citation")
	}
}

func TestAnalysisChatOversizedGrepCannotCiteHiddenMatch(t *testing.T) {
	shrinkCallDelay(t)
	server := newScriptedChatServer(t)
	server.push(200, chatRespToolCall("grep", "grep_artifact", map[string]any{
		"path": "build-log.txt", "pattern": "MATCH", "context_lines": 0, "max_matches": 100,
	}))
	server.push(200, chatRespFinal(`{"answer":"The hidden failure caused it.","assessment":"supports","citations":[{"path":"build-log.txt","line_start":100,"line_end":100,"quote":"hidden failure"}],"proposed_revision":null}`))
	server.push(200, chatRespFinal(`{"answer":"The retained failure is visible.","assessment":"supports","citations":[{"path":"build-log.txt","line_start":1,"line_end":1,"quote":"retained failure"}],"proposed_revision":null}`))
	content := "MATCH retained failure\n" + strings.Repeat("MATCH "+strings.Repeat("x", 900)+"\n", 98) + "MATCH hidden failure\n"
	agent := newAnalysisChatAgentForTest(t, server.URL, &fakeBrowser{files: map[string][]byte{
		"build-log.txt": []byte(content),
	}}, AnalysisChatOptions{MaxIters: 4, Timeout: 10 * time.Second})
	reply, err := agent.Reply(context.Background(), analysisChatTurn())
	if err != nil {
		t.Fatal(err)
	}
	if reply.Unverified || len(reply.Citations) != 1 || reply.Citations[0].LineStart != 1 || reply.Answer != "The retained failure is visible." {
		t.Fatalf("chat accepted hidden evidence or lost retained content: %+v", reply)
	}
	if len(server.requests) != 3 {
		t.Fatalf("chat did not correct the hidden citation: requests=%d", len(server.requests))
	}
}
