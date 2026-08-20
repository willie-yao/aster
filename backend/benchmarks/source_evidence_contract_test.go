package benchmarks

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/artifacts"
)

const (
	maxBenchmarkSourceReads     = 512
	maxBenchmarkSourceCitations = 64
)

type benchmarkSourceRange struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
	Path       string `json:"path"`
	LineStart  int    `json:"line_start"`
	LineEnd    int    `json:"line_end"`
}

type benchmarkSourceRead struct {
	benchmarkSourceRange
	Tool    string `json:"tool"`
	Outcome string `json:"outcome"`
}

type benchmarkSourceCitation struct {
	benchmarkSourceRange
	Emitted  bool `json:"emitted"`
	Verified bool `json:"verified"`
}

func benchmarkSourceReadsFromInProcess(bc benchCase, values []ai.SourceEvidenceObservation) ([]benchmarkSourceRead, error) {
	out := make([]benchmarkSourceRead, 0, len(values))
	for _, value := range values {
		repository, revision, err := benchmarkSourceIdentity(bc, value.SourceID)
		if err != nil {
			return nil, err
		}
		out = append(out, benchmarkSourceRead{benchmarkSourceRange: benchmarkSourceRange{Repository: repository, Revision: revision, Path: value.Path, LineStart: value.LineStart, LineEnd: value.LineEnd}, Tool: value.Tool, Outcome: "succeeded"})
	}
	return canonicalBenchmarkSourceReads(out)
}

func benchmarkSourceReadsFromSandbox(bc benchCase, values []agentanalysis.WorkspaceSourceReadTelemetry) ([]benchmarkSourceRead, error) {
	out := make([]benchmarkSourceRead, 0, len(values))
	for _, value := range values {
		repository, revision, err := benchmarkSourceIdentity(bc, value.SourceID)
		if err != nil {
			return nil, err
		}
		out = append(out, benchmarkSourceRead{benchmarkSourceRange: benchmarkSourceRange{Repository: repository, Revision: revision, Path: value.Path, LineStart: value.LineStart, LineEnd: value.LineEnd}, Tool: value.Tool, Outcome: "succeeded"})
	}
	return canonicalBenchmarkSourceReads(out)
}

func canonicalBenchmarkSourceRefs(values []benchmarkSourceRef) ([]benchmarkSourceRef, error) {
	if len(values) == 0 || len(values) > 8 {
		return nil, fmt.Errorf("benchmark source ref count must be 1..8")
	}
	seenIDs := map[string]bool{}
	seenIdentities := map[string]bool{}
	out := append([]benchmarkSourceRef(nil), values...)
	for _, value := range out {
		if !benchmarkSourceIDRE.MatchString(value.ID) || !benchmarkRepoRE.MatchString(value.Repository) || !benchmarkCommitRE.MatchString(value.Revision) {
			return nil, fmt.Errorf("benchmark source ref is invalid")
		}
		if seenIDs[value.ID] {
			return nil, fmt.Errorf("benchmark source ref id %q is duplicated", value.ID)
		}
		identity := value.Repository + "\x00" + value.Revision
		if seenIdentities[identity] {
			return nil, fmt.Errorf("benchmark source ref identity is duplicated")
		}
		seenIDs[value.ID] = true
		seenIdentities[identity] = true
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func benchmarkSourceRefByID(bc benchCase, sourceID string) (benchmarkSourceRef, bool) {
	for _, source := range bc.sourceRefs {
		if source.ID == sourceID {
			return source, true
		}
	}
	return benchmarkSourceRef{}, false
}

func benchmarkPrimarySourceRef(bc benchCase) (benchmarkSourceRef, bool) {
	return benchmarkSourceRefByID(bc, bc.primarySourceID)
}

func benchmarkSourceIdentity(bc benchCase, sourceID string) (string, string, error) {
	source, ok := benchmarkSourceRefByID(bc, sourceID)
	if !ok {
		return "", "", fmt.Errorf("benchmark source id %q is not configured", sourceID)
	}
	return source.Repository, source.Revision, nil
}

func canonicalBenchmarkSourceReads(values []benchmarkSourceRead) ([]benchmarkSourceRead, error) {
	if len(values) > maxBenchmarkSourceReads {
		return nil, fmt.Errorf("benchmark source reads exceed the bound")
	}
	seen := map[string]bool{}
	out := make([]benchmarkSourceRead, 0, len(values))
	for _, value := range values {
		if err := validateBenchmarkSourceRange(value.benchmarkSourceRange); err != nil || value.Outcome != "succeeded" || value.Tool != "read_repo_file" && value.Tool != "grep_repo" && value.Tool != "read" && value.Tool != "grep" {
			return nil, fmt.Errorf("benchmark source read is invalid")
		}
		key := benchmarkSourceRangeKey(value.benchmarkSourceRange) + "\x00" + value.Tool + "\x00" + value.Outcome
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return benchmarkSourceReadLess(out[i], out[j]) })
	return out, nil
}

func canonicalBenchmarkSourceCitations(values []benchmarkSourceCitation) ([]benchmarkSourceCitation, error) {
	if len(values) > maxBenchmarkSourceCitations {
		return nil, fmt.Errorf("benchmark source citations exceed the bound")
	}
	seen := map[string]benchmarkSourceCitation{}
	for _, value := range values {
		if err := validateBenchmarkSourceRange(value.benchmarkSourceRange); err != nil || value.Verified && !value.Emitted {
			return nil, fmt.Errorf("benchmark source citation is invalid")
		}
		key := benchmarkSourceRangeKey(value.benchmarkSourceRange)
		if prior, ok := seen[key]; ok {
			if prior.Emitted != value.Emitted || prior.Verified != value.Verified {
				return nil, fmt.Errorf("benchmark source citation is conflicting")
			}
			continue
		}
		seen[key] = value
	}
	out := make([]benchmarkSourceCitation, 0, len(seen))
	for _, value := range seen {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool {
		return benchmarkSourceRangeLess(out[i].benchmarkSourceRange, out[j].benchmarkSourceRange)
	})
	return out, nil
}

func canonicalBenchmarkExpectedSourceRanges(values []benchmarkSourceRange) ([]benchmarkSourceRange, error) {
	if len(values) > 8 {
		return nil, fmt.Errorf("benchmark expected source ranges exceed the bound")
	}
	seen := map[string]bool{}
	out := make([]benchmarkSourceRange, 0, len(values))
	for _, value := range values {
		if err := validateBenchmarkSourceRange(value); err != nil {
			return nil, err
		}
		key := benchmarkSourceRangeKey(value)
		if seen[key] {
			return nil, fmt.Errorf("benchmark expected source range is duplicated")
		}
		seen[key] = true
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return benchmarkSourceRangeLess(out[i], out[j]) })
	return out, nil
}

func validateBenchmarkSourceRange(value benchmarkSourceRange) error {
	path, err := artifacts.SafePath(strings.TrimSpace(value.Path))
	if err != nil || path == "" || path != value.Path || strings.Contains(path, "\\") || !benchmarkRepoRE.MatchString(value.Repository) || !benchmarkCommitRE.MatchString(value.Revision) || value.LineStart < 1 || value.LineEnd < value.LineStart || value.LineEnd-value.LineStart+1 > 2000 {
		return fmt.Errorf("benchmark source range is invalid")
	}
	return nil
}

func benchmarkExpectedSourceReadCoverage(expected []benchmarkSourceRange, reads []benchmarkSourceRead) (int, int) {
	hits := 0
	for _, want := range expected {
		var intervals [][2]int
		for _, read := range reads {
			if read.Repository == want.Repository && read.Revision == want.Revision && read.Path == want.Path && read.Outcome == "succeeded" {
				intervals = append(intervals, [2]int{read.LineStart, read.LineEnd})
			}
		}
		sort.Slice(intervals, func(i, j int) bool {
			if intervals[i][0] != intervals[j][0] {
				return intervals[i][0] < intervals[j][0]
			}
			return intervals[i][1] < intervals[j][1]
		})
		coveredThrough := want.LineStart - 1
		for _, interval := range intervals {
			if interval[1] < want.LineStart || interval[0] > coveredThrough+1 {
				continue
			}
			if interval[0] <= coveredThrough+1 && interval[1] > coveredThrough {
				coveredThrough = interval[1]
			}
			if coveredThrough >= want.LineEnd {
				hits++
				break
			}
		}
	}
	return hits, len(expected)
}

func benchmarkSourceCitationCounts(values []benchmarkSourceCitation) (emitted, verified int) {
	for _, value := range values {
		if value.Emitted {
			emitted++
		}
		if value.Verified {
			verified++
		}
	}
	return
}

func benchmarkSourceRangeKey(value benchmarkSourceRange) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", value.Repository, value.Revision, value.Path, value.LineStart, value.LineEnd)
}

func benchmarkSourceRangeLess(left, right benchmarkSourceRange) bool {
	if left.Repository != right.Repository {
		return left.Repository < right.Repository
	}
	if left.Revision != right.Revision {
		return left.Revision < right.Revision
	}
	if left.Path != right.Path {
		return left.Path < right.Path
	}
	if left.LineStart != right.LineStart {
		return left.LineStart < right.LineStart
	}
	return left.LineEnd < right.LineEnd
}

func benchmarkSourceReadLess(left, right benchmarkSourceRead) bool {
	if benchmarkSourceRangeKey(left.benchmarkSourceRange) != benchmarkSourceRangeKey(right.benchmarkSourceRange) {
		return benchmarkSourceRangeLess(left.benchmarkSourceRange, right.benchmarkSourceRange)
	}
	if left.Tool != right.Tool {
		return left.Tool < right.Tool
	}
	return left.Outcome < right.Outcome
}

func TestBenchmarkExpectedSourceReadCoverage(t *testing.T) {
	revision := strings.Repeat("a", 40)
	expected := []benchmarkSourceRange{{Repository: "owner/repo", Revision: revision, Path: "pkg/file.go", LineStart: 10, LineEnd: 20}}
	tests := map[string]struct {
		reads []benchmarkSourceRead
		hits  int
	}{
		"exact":   {[]benchmarkSourceRead{{benchmarkSourceRange: benchmarkSourceRange{Repository: "owner/repo", Revision: revision, Path: "pkg/file.go", LineStart: 10, LineEnd: 20}, Tool: "read", Outcome: "succeeded"}}, 1},
		"partial": {[]benchmarkSourceRead{{benchmarkSourceRange: benchmarkSourceRange{Repository: "owner/repo", Revision: revision, Path: "pkg/file.go", LineStart: 10, LineEnd: 19}, Tool: "read", Outcome: "succeeded"}}, 0},
		"adjacent": {[]benchmarkSourceRead{
			{benchmarkSourceRange: benchmarkSourceRange{Repository: "owner/repo", Revision: revision, Path: "pkg/file.go", LineStart: 10, LineEnd: 14}, Tool: "grep", Outcome: "succeeded"},
			{benchmarkSourceRange: benchmarkSourceRange{Repository: "owner/repo", Revision: revision, Path: "pkg/file.go", LineStart: 15, LineEnd: 20}, Tool: "read", Outcome: "succeeded"},
		}, 1},
		"wrong revision": {[]benchmarkSourceRead{{benchmarkSourceRange: benchmarkSourceRange{Repository: "owner/repo", Revision: strings.Repeat("b", 40), Path: "pkg/file.go", LineStart: 1, LineEnd: 30}, Tool: "read", Outcome: "succeeded"}}, 0},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			hits, total := benchmarkExpectedSourceReadCoverage(expected, test.reads)
			if hits != test.hits || total != 1 {
				t.Fatalf("coverage = %d/%d, want %d/1", hits, total, test.hits)
			}
		})
	}
}

func TestCanonicalBenchmarkSourceEvidenceRejectsOverflowAndConflicts(t *testing.T) {
	revision := strings.Repeat("a", 40)
	rangeValue := benchmarkSourceRange{Repository: "owner/repo", Revision: revision, Path: "pkg/file.go", LineStart: 1, LineEnd: 1}
	citations := []benchmarkSourceCitation{{benchmarkSourceRange: rangeValue, Emitted: true}, {benchmarkSourceRange: rangeValue, Emitted: true, Verified: true}}
	if _, err := canonicalBenchmarkSourceCitations(citations); err == nil {
		t.Fatal("conflicting duplicate citation was accepted")
	}
	reads := make([]benchmarkSourceRead, maxBenchmarkSourceReads+1)
	for index := range reads {
		reads[index] = benchmarkSourceRead{benchmarkSourceRange: benchmarkSourceRange{Repository: "owner/repo", Revision: revision, Path: fmt.Sprintf("pkg/%03d.go", index), LineStart: 1, LineEnd: 1}, Tool: "read", Outcome: "succeeded"}
	}
	if _, err := canonicalBenchmarkSourceReads(reads); err == nil {
		t.Fatal("source read overflow was accepted")
	}
}

func TestCanonicalBenchmarkSourceRefsSupportsSameRepositoryRevisions(t *testing.T) {
	firstRevision := strings.Repeat("a", 40)
	secondRevision := strings.Repeat("b", 40)
	refs, err := canonicalBenchmarkSourceRefs([]benchmarkSourceRef{
		{ID: "server", Repository: "kubernetes/kubernetes", Revision: secondRevision},
		{ID: "client", Repository: "kubernetes/kubernetes", Revision: firstRevision},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].ID != "client" || refs[1].ID != "server" {
		t.Fatalf("source refs = %+v", refs)
	}

	for name, values := range map[string][]benchmarkSourceRef{
		"duplicate id": {
			{ID: "client", Repository: "kubernetes/kubernetes", Revision: firstRevision},
			{ID: "client", Repository: "kubernetes/kubernetes", Revision: secondRevision},
		},
		"duplicate identity": {
			{ID: "client", Repository: "kubernetes/kubernetes", Revision: firstRevision},
			{ID: "server", Repository: "kubernetes/kubernetes", Revision: firstRevision},
		},
		"mutable revision": {
			{ID: "client", Repository: "kubernetes/kubernetes", Revision: "main"},
		},
		"invalid id": {
			{ID: "Client", Repository: "kubernetes/kubernetes", Revision: firstRevision},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := canonicalBenchmarkSourceRefs(values); err == nil {
				t.Fatal("invalid source refs were accepted")
			}
		})
	}
}

func TestBenchmarkSourceObservationsNormalizeBySourceID(t *testing.T) {
	clientRevision := strings.Repeat("a", 40)
	serverRevision := strings.Repeat("b", 40)
	bc := benchCase{
		primarySourceID: "client",
		sourceRefs: []benchmarkSourceRef{
			{ID: "client", Repository: "kubernetes/kubernetes", Revision: clientRevision},
			{ID: "server", Repository: "kubernetes/kubernetes", Revision: serverRevision},
		},
	}
	inProcess, err := benchmarkSourceReadsFromInProcess(bc, []ai.SourceEvidenceObservation{{SourceID: "server", Tool: "read_repo_file", Path: "pkg/file.go", LineStart: 10, LineEnd: 20}})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := benchmarkSourceReadsFromSandbox(bc, []agentanalysis.WorkspaceSourceReadTelemetry{{SourceID: "server", Tool: "read", Path: "pkg/file.go", LineStart: 10, LineEnd: 20}})
	if err != nil {
		t.Fatal(err)
	}
	for name, reads := range map[string][]benchmarkSourceRead{"in-process": inProcess, "sandbox": sandbox} {
		if len(reads) != 1 || reads[0].Repository != "kubernetes/kubernetes" || reads[0].Revision != serverRevision {
			t.Fatalf("%s reads = %+v", name, reads)
		}
		hits, total := benchmarkExpectedSourceReadCoverage([]benchmarkSourceRange{{Repository: "kubernetes/kubernetes", Revision: serverRevision, Path: "pkg/file.go", LineStart: 10, LineEnd: 20}}, reads)
		if hits != 1 || total != 1 {
			t.Fatalf("%s coverage = %d/%d", name, hits, total)
		}
	}
	if _, err := benchmarkSourceReadsFromInProcess(bc, []ai.SourceEvidenceObservation{{SourceID: "unknown", Tool: "read_repo_file", Path: "x", LineStart: 1, LineEnd: 1}}); err == nil {
		t.Fatal("unknown source id was accepted")
	}
}
