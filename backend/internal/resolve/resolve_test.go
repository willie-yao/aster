package resolve

import (
	"path/filepath"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func pattern(id string, builds ...string) models.PatternAnalysis {
	return models.PatternAnalysis{ID: id, Systemic: true, SharedBuilds: builds}
}

func TestWatermark(t *testing.T) {
	// Highest build id wins regardless of slice order.
	got := Watermark(pattern("x", "2065378387123245056", "2069829458465918976", "2068712116877004800"))
	if want := "2069829458465918976"; got != want {
		t.Fatalf("Watermark = %q, want %q", got, want)
	}
	if got := Watermark(pattern("x")); got != "" {
		t.Fatalf("Watermark with no builds = %q, want empty", got)
	}
}

func TestPrune_ReopensOnNewerFailingBuild(t *testing.T) {
	s := &State{Resolved: map[string]Entry{
		"a": {Watermark: "2069829458465918976"},
	}}
	// Same pattern now has a strictly newer failing build -> recurrence.
	patterns := []models.PatternAnalysis{
		pattern("a", "2069829458465918976", "2070999999999999999"),
	}
	out, changed := s.Prune(patterns)
	if !changed {
		t.Fatal("expected changed=true when a newer build recurs")
	}
	if out.IsResolved("a") {
		t.Fatal("pattern a should have been re-opened (dropped)")
	}
}

func TestPrune_KeepsWhenNoNewerBuild(t *testing.T) {
	s := &State{Resolved: map[string]Entry{
		"a": {Watermark: "2069829458465918976"},
	}}
	// Only the same (or older) builds are present: still fixed.
	patterns := []models.PatternAnalysis{
		pattern("a", "2069829458465918976", "2065378387123245056"),
	}
	out, changed := s.Prune(patterns)
	if changed {
		t.Fatal("expected changed=false when no newer build")
	}
	if !out.IsResolved("a") {
		t.Fatal("pattern a should stay resolved")
	}
}

func TestPrune_KeepsWhenPatternAbsent(t *testing.T) {
	s := &State{Resolved: map[string]Entry{
		"a": {Watermark: "2069829458465918976"},
	}}
	// Pattern no longer present (aged out): keep the resolution.
	out, changed := s.Prune([]models.PatternAnalysis{pattern("b", "1")})
	if changed {
		t.Fatal("expected changed=false when pattern absent")
	}
	if !out.IsResolved("a") {
		t.Fatal("resolution for an absent pattern should be kept")
	}
}

func TestPrune_EmptyWatermarkReopensOnAnyOccurrence(t *testing.T) {
	s := &State{Resolved: map[string]Entry{"a": {Watermark: ""}}}
	out, changed := s.Prune([]models.PatternAnalysis{pattern("a", "1")})
	if !changed || out.IsResolved("a") {
		t.Fatal("empty watermark should re-open on any current occurrence")
	}
}

func TestWatermark_TrimsAndSkipsJunk(t *testing.T) {
	// Whitespace-padded and non-numeric ids: trim the valid ones, skip junk.
	got := Watermark(pattern("x", " 100 ", "abc", "250"))
	if got != "250" {
		t.Fatalf("Watermark = %q, want 250", got)
	}
}

func TestPrune_ReopensDespiteWhitespaceInNewerBuild(t *testing.T) {
	// A newer failing build with stray whitespace must still be detected, or a
	// recurred pattern would stay wrongly hidden.
	s := &State{Resolved: map[string]Entry{"a": {Watermark: "100"}}}
	out, changed := s.Prune([]models.PatternAnalysis{pattern("a", " 250 ")})
	if !changed || out.IsResolved("a") {
		t.Fatal("whitespace-padded newer build should re-open the pattern")
	}
}

func TestPrune_KeepsWhenOnlyUnparseableOlderContext(t *testing.T) {
	// Valid watermark, current builds all older/equal: stays resolved.
	s := &State{Resolved: map[string]Entry{"a": {Watermark: "250"}}}
	out, changed := s.Prune([]models.PatternAnalysis{pattern("a", "100", "250")})
	if changed || !out.IsResolved("a") {
		t.Fatal("no newer build: should stay resolved")
	}
}

func TestLoadSave_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := &State{Resolved: map[string]Entry{
		"a": {ResolvedBy: "willie-yao", Note: "fixed by test-infra #123", Watermark: "42", Subject: "job x"},
	}}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := filepath.Abs(filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	got := Load(dir)
	e, ok := got.Resolved["a"]
	if !ok || e.ResolvedBy != "willie-yao" || e.Note == "" || e.Watermark != "42" {
		t.Fatalf("round-trip mismatch: %+v", got.Resolved)
	}
}

func TestLoad_MissingFileIsEmpty(t *testing.T) {
	got := Load(t.TempDir())
	if got == nil || got.Resolved == nil || len(got.Resolved) != 0 {
		t.Fatalf("missing file should load empty non-nil state, got %+v", got)
	}
}
