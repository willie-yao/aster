package e2e

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/artifacts"
)

type benchmarkEvidenceGroup struct {
	id                 string
	pathREs            []*regexp.Regexp
	contentREs         []*regexp.Regexp
	causalSignals      []benchSignal
	oracleContextLines *int
}

type benchmarkEvidenceCoverage struct {
	selected []string
	hit      []string
	missed   []string
	sources  map[string][]string
}

type benchmarkEvidenceRecorder struct {
	groups   []benchmarkEvidenceGroup
	mu       sync.Mutex
	selected map[string]bool
	hit      map[string]bool
	sources  map[string]map[string]bool
}

func newBenchmarkEvidenceRecorder(groups []benchmarkEvidenceGroup) *benchmarkEvidenceRecorder {
	return &benchmarkEvidenceRecorder{groups: groups, selected: map[string]bool{}, hit: map[string]bool{}, sources: map[string]map[string]bool{}}
}

func (r *benchmarkEvidenceRecorder) selectPath(path string) {
	if r == nil || len(r.groups) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, group := range r.groups {
		if matchesBenchmarkEvidence(group.pathREs, []byte(path)) {
			r.selected[group.id] = true
		}
	}
}

func (r *benchmarkEvidenceRecorder) observe(ctx context.Context, path string, content []byte) {
	source := string(ai.EvidenceReadSourceFromContext(ctx))
	if source == "" {
		source = "unknown"
	}
	r.observeSource(path, content, source)
}

func (r *benchmarkEvidenceRecorder) observeSource(path string, content []byte, source string) {
	if r == nil || len(r.groups) == 0 {
		return
	}
	for _, group := range r.groups {
		if !matchesBenchmarkEvidence(group.pathREs, []byte(path)) {
			continue
		}
		if len(group.contentREs) > 0 && !matchesBenchmarkEvidence(group.contentREs, content) {
			continue
		}
		r.mu.Lock()
		r.hit[group.id] = true
		if r.sources[group.id] == nil {
			r.sources[group.id] = map[string]bool{}
		}
		r.sources[group.id][source] = true
		r.mu.Unlock()
	}
}

func matchesBenchmarkEvidence(patterns []*regexp.Regexp, value []byte) bool {
	for _, pattern := range patterns {
		if pattern.Match(value) {
			return true
		}
	}
	return false
}

func (r *benchmarkEvidenceRecorder) coverage() benchmarkEvidenceCoverage {
	if r == nil {
		return benchmarkEvidenceCoverage{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := benchmarkEvidenceCoverage{sources: map[string][]string{}}
	for _, group := range r.groups {
		if r.selected[group.id] {
			out.selected = append(out.selected, group.id)
		}
		if r.hit[group.id] {
			out.hit = append(out.hit, group.id)
			for source := range r.sources[group.id] {
				out.sources[group.id] = append(out.sources[group.id], source)
			}
			sort.Strings(out.sources[group.id])
		} else {
			out.missed = append(out.missed, group.id)
		}
	}
	sort.Strings(out.selected)
	sort.Strings(out.hit)
	sort.Strings(out.missed)
	return out
}

type benchmarkEvidenceFactory struct {
	inner    artifacts.Factory
	recorder *benchmarkEvidenceRecorder
}

func (f benchmarkEvidenceFactory) ForBuild(buildPrefix, displayName string) artifacts.Browser {
	return &benchmarkEvidenceBrowser{Browser: f.inner.ForBuild(buildPrefix, displayName), recorder: f.recorder}
}

type benchmarkEvidenceBrowser struct {
	artifacts.Browser
	recorder *benchmarkEvidenceRecorder
}

func (b *benchmarkEvidenceBrowser) Read(ctx context.Context, file string, offset, length int) ([]byte, int64, error) {
	b.recorder.selectPath(file)
	content, size, err := b.Browser.Read(ctx, file, offset, length)
	if err == nil {
		b.recorder.observe(ctx, file, content)
	}
	return content, size, err
}

func (b *benchmarkEvidenceBrowser) Tail(ctx context.Context, file string, lines, maxBytes int) (*artifacts.TailResult, error) {
	b.recorder.selectPath(file)
	result, err := b.Browser.Tail(ctx, file, lines, maxBytes)
	if err == nil && result != nil {
		b.recorder.observe(ctx, file, result.Content)
	}
	return result, err
}

func (b *benchmarkEvidenceBrowser) Grep(ctx context.Context, file string, re *regexp.Regexp, contextLines, maxMatches, maxLineLen, maxBytes int) (*artifacts.GrepResult, error) {
	b.recorder.selectPath(file)
	result, err := b.Browser.Grep(ctx, file, re, contextLines, maxMatches, maxLineLen, maxBytes)
	if err == nil && result != nil {
		var content strings.Builder
		for _, match := range result.Matches {
			for _, line := range match.Context {
				content.WriteString(line)
				content.WriteByte('\n')
			}
		}
		b.recorder.observe(ctx, file, []byte(content.String()))
	}
	return result, err
}

type benchmarkEvidenceStubBrowser struct {
	readContent []byte
	tailContent []byte
	grepContent string
	err         error
}

func (b *benchmarkEvidenceStubBrowser) BuildRoot() string { return "build" }
func (b *benchmarkEvidenceStubBrowser) List(context.Context, string) (*artifacts.Listing, error) {
	return &artifacts.Listing{Files: []artifacts.FileInfo{{Name: "log.txt", Size: 1}}}, b.err
}
func (b *benchmarkEvidenceStubBrowser) ListTree(context.Context, int) ([]string, bool, error) {
	return []string{"log.txt"}, false, b.err
}
func (b *benchmarkEvidenceStubBrowser) Read(context.Context, string, int, int) ([]byte, int64, error) {
	return slices.Clone(b.readContent), int64(len(b.readContent)), b.err
}
func (b *benchmarkEvidenceStubBrowser) Tail(context.Context, string, int, int) (*artifacts.TailResult, error) {
	return &artifacts.TailResult{Content: slices.Clone(b.tailContent), FileSize: int64(len(b.tailContent))}, b.err
}
func (b *benchmarkEvidenceStubBrowser) Grep(context.Context, string, *regexp.Regexp, int, int, int, int) (*artifacts.GrepResult, error) {
	return &artifacts.GrepResult{Matches: []artifacts.GrepMatch{{LineNo: 1, Context: []string{b.grepContent}}}}, b.err
}

type benchmarkEvidenceStubFactory struct{ browser artifacts.Browser }

func (f benchmarkEvidenceStubFactory) ForBuild(string, string) artifacts.Browser { return f.browser }

func TestBenchmarkEvidenceBrowserPreservesResults(t *testing.T) {
	inner := &benchmarkEvidenceStubBrowser{
		readContent: []byte("read signal"), tailContent: []byte("tail signal"), grepContent: "grep signal",
	}
	recorder := newBenchmarkEvidenceRecorder([]benchmarkEvidenceGroup{
		{id: "read", pathREs: []*regexp.Regexp{regexp.MustCompile(`read\.log$`)}, contentREs: []*regexp.Regexp{regexp.MustCompile(`read signal`)}},
		{id: "tail", pathREs: []*regexp.Regexp{regexp.MustCompile(`tail\.log$`)}, contentREs: []*regexp.Regexp{regexp.MustCompile(`tail signal`)}},
		{id: "grep", pathREs: []*regexp.Regexp{regexp.MustCompile(`grep\.log$`)}, contentREs: []*regexp.Regexp{regexp.MustCompile(`grep signal`)}},
	})
	factory := benchmarkEvidenceFactory{inner: benchmarkEvidenceStubFactory{browser: inner}, recorder: recorder}
	browser := factory.ForBuild("prefix", "display")

	read, size, err := browser.Read(t.Context(), "read.log", 0, 100)
	if err != nil || string(read) != "read signal" || size != int64(len(read)) {
		t.Fatalf("read result changed: content=%q size=%d err=%v", read, size, err)
	}
	tail, err := browser.Tail(t.Context(), "tail.log", 10, 100)
	if err != nil || string(tail.Content) != "tail signal" {
		t.Fatalf("tail result changed: result=%+v err=%v", tail, err)
	}
	grep, err := browser.Grep(t.Context(), "grep.log", regexp.MustCompile("signal"), 0, 10, 100, 1000)
	if err != nil || len(grep.Matches) != 1 || grep.Matches[0].Context[0] != "grep signal" {
		t.Fatalf("grep result changed: result=%+v err=%v", grep, err)
	}
	coverage := recorder.coverage()
	if !slices.Equal(coverage.hit, []string{"grep", "read", "tail"}) || len(coverage.missed) != 0 {
		t.Fatalf("coverage = %+v", coverage)
	}
	for _, id := range coverage.hit {
		if !slices.Equal(coverage.sources[id], []string{"unknown"}) {
			t.Fatalf("sources[%s] = %v", id, coverage.sources[id])
		}
	}
}

func TestBenchmarkEvidenceBrowserRecordsOnlySuccessfulMatchingReads(t *testing.T) {
	groups := []benchmarkEvidenceGroup{
		{id: "target", pathREs: []*regexp.Regexp{regexp.MustCompile(`target\.log$`)}, contentREs: []*regexp.Regexp{regexp.MustCompile(`initiating error`)}},
	}
	t.Run("wrong content", func(t *testing.T) {
		recorder := newBenchmarkEvidenceRecorder(groups)
		browser := &benchmarkEvidenceBrowser{Browser: &benchmarkEvidenceStubBrowser{readContent: []byte("terminal timeout")}, recorder: recorder}
		_, _, _ = browser.Read(t.Context(), "target.log", 0, 100)
		if coverage := recorder.coverage(); len(coverage.hit) != 0 || !slices.Equal(coverage.missed, []string{"target"}) {
			t.Fatalf("coverage = %+v", coverage)
		}
	})
	t.Run("failed read", func(t *testing.T) {
		recorder := newBenchmarkEvidenceRecorder(groups)
		browser := &benchmarkEvidenceBrowser{Browser: &benchmarkEvidenceStubBrowser{readContent: []byte("initiating error"), err: errors.New("read failed")}, recorder: recorder}
		_, _, _ = browser.Read(t.Context(), "target.log", 0, 100)
		if coverage := recorder.coverage(); len(coverage.hit) != 0 || !slices.Equal(coverage.missed, []string{"target"}) {
			t.Fatalf("coverage = %+v", coverage)
		}
	})
}

func TestCrossProjectEvidenceGroupsMatchKnownExamples(t *testing.T) {
	cases, err := loadBenchmarkManifest("testdata/benchmarks/cross-project-eval.json")
	if err != nil {
		t.Fatal(err)
	}
	examples := map[string][]struct {
		path    string
		content string
	}{
		"secrets-store-csi-image-scan": {
			{path: "build-log.txt", content: "Total: 4 (MEDIUM: 1, HIGH: 3, CRITICAL: 0)"},
			{path: "build-log.txt", content: "trivy image --exit-code 1 --ignore-unfixed"},
		},
		"kueue-was-podgroup-api-mismatch": {
			{path: "build-log.txt", content: "runtime-config: scheduling.k8s.io/v1alpha3=true"},
			{path: "artifacts/kind-control-plane/pods/kube-system_kube-scheduler/kube-scheduler/0.log", content: "failed to list *v1beta1.PodGroup: the server could not find the requested resource"},
			{path: "artifacts/kind-control-plane/pods/kube-system_kube-scheduler/kube-scheduler/0.log", content: "sched-handler-sync check failed: handlers are not fully synchronized"},
			{path: "build-log.txt", content: "timed out waiting for the condition on deployments/kubeflow-trainer-controller-manager"},
			{path: "build-log.txt", content: "timed out waiting for the condition on nodes/kind-worker2"},
		},
		"gcp-pd-csi-windows-mount-visibility": {
			{path: "build-log.txt", content: `Get-Item : Could not find item C:\mnt\volume1`},
			{path: "artifacts/pd-csi-driver/csi-gce-pd-node-win-gce-pd-driver.log", content: "NodePublishVolume succeeded on volume projects/example"},
		},
	}
	for _, bc := range cases {
		recorder := newBenchmarkEvidenceRecorder(bc.evidenceGroups)
		for _, example := range examples[bc.name] {
			recorder.observe(t.Context(), example.path, []byte(example.content))
		}
		if coverage := recorder.coverage(); len(coverage.missed) != 0 || len(coverage.hit) != len(bc.evidenceGroups) {
			t.Errorf("case %s coverage = %+v", bc.name, coverage)
		}
	}
}

func cloneBenchmarkEvidenceSources(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for id, sources := range in {
		out[id] = append([]string(nil), sources...)
	}
	return out
}
