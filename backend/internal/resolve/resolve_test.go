package resolve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
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

func causePattern(id string, groups ...models.PatternCausalGroup) models.PatternAnalysis {
	return models.PatternAnalysis{ID: id, Systemic: true, CausalGroups: groups}
}

func group(signature string, builds ...string) models.PatternCausalGroup {
	return models.PatternCausalGroup{Signature: signature, Builds: builds}
}

func TestCauseWatermark(t *testing.T) {
	got := CauseWatermark(group("sig", "2065378387123245056", "2069829458465918976", "2068712116877004800"))
	if want := "2069829458465918976"; got != want {
		t.Fatalf("CauseWatermark = %q, want %q", got, want)
	}
	if got := CauseWatermark(group("sig")); got != "" {
		t.Fatalf("CauseWatermark with no builds = %q, want empty", got)
	}
}

func TestPrune_ReopensCauseOnNewerFailingBuild(t *testing.T) {
	s := &State{Resolved: map[string]Entry{}, Causes: map[string]Entry{
		"sig": {Watermark: "2069829458465918976"},
	}}
	out, changed := s.Prune([]models.PatternAnalysis{
		causePattern("a", group("sig", "2069829458465918976", "2070999999999999999")),
	})
	if !changed {
		t.Fatal("expected changed=true when a newer build recurs")
	}
	if out.IsCauseResolved("sig") {
		t.Fatal("cause sig should have been re-opened (dropped)")
	}
}

func TestPrune_KeepsCauseWhenNoNewerBuild(t *testing.T) {
	s := &State{Resolved: map[string]Entry{}, Causes: map[string]Entry{
		"sig": {Watermark: "2069829458465918976"},
	}}
	out, changed := s.Prune([]models.PatternAnalysis{
		causePattern("a", group("sig", "2069829458465918976", "2065378387123245056")),
	})
	if changed {
		t.Fatal("expected changed=false when no newer build")
	}
	if !out.IsCauseResolved("sig") {
		t.Fatal("cause sig should stay resolved")
	}
}

// A cause whose builds have aged out is published nowhere, so its resolution is
// retained rather than dropped: the cause shows nothing anyway, and it may
// return within a later window.
func TestPrune_KeepsCauseWhenAbsent(t *testing.T) {
	s := &State{Resolved: map[string]Entry{}, Causes: map[string]Entry{
		"sig": {Watermark: "2069829458465918976"},
	}}
	out, changed := s.Prune([]models.PatternAnalysis{
		causePattern("a", group("other", "2070999999999999999")),
	})
	if changed {
		t.Fatal("expected changed=false when the cause is not published")
	}
	if !out.IsCauseResolved("sig") {
		t.Fatal("cause sig should stay resolved")
	}
}

// Resolving one cause must leave its siblings and the pattern untouched.
func TestPrune_CauseAndPatternScopesAreIndependent(t *testing.T) {
	s := &State{
		Resolved: map[string]Entry{"a": {Watermark: "2069829458465918976"}},
		Causes:   map[string]Entry{"sig": {Watermark: "2069829458465918976"}},
	}
	// The pattern recurs, the cause does not.
	out, changed := s.Prune([]models.PatternAnalysis{{
		ID: "a", Systemic: true,
		SharedBuilds: []string{"2070999999999999999"},
		CausalGroups: []models.PatternCausalGroup{group("sig", "2069829458465918976")},
	}})
	if !changed {
		t.Fatal("expected changed=true when the pattern recurs")
	}
	if out.IsResolved("a") {
		t.Fatal("pattern a should have been re-opened")
	}
	if !out.IsCauseResolved("sig") {
		t.Fatal("cause sig should stay resolved")
	}
}

// An unsigned group has no durable identity, so it can never match a cause
// resolution and must not contribute its builds to another signature.
func TestPrune_IgnoresUnsignedGroups(t *testing.T) {
	s := &State{Resolved: map[string]Entry{}, Causes: map[string]Entry{
		"": {Watermark: "1"},
	}}
	out, changed := s.Prune([]models.PatternAnalysis{
		causePattern("a", group("", "2070999999999999999")),
	})
	if changed {
		t.Fatal("expected changed=false: an unsigned group matches nothing")
	}
	if !out.IsCauseResolved("") {
		t.Fatal("the entry should have been retained untouched")
	}
}

func TestLoadFillsCausesMap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(`{"resolved":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if state := Load(dir); state.Causes == nil {
		t.Fatal("Causes should be non-nil so callers never nil-check it")
	}
}

func TestRemoveMatchingDropsStagedCause(t *testing.T) {
	dir := t.TempDir()
	staged := Entry{Watermark: "1", ResolvedBy: "alice"}
	if err := (&State{Resolved: map[string]Entry{}, Causes: map[string]Entry{"sig": staged, "keep": {Watermark: "2"}}}).Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMatching(dir, &State{Causes: map[string]Entry{"sig": staged}}); err != nil {
		t.Fatal(err)
	}
	state := Load(dir)
	if state.IsCauseResolved("sig") || !state.IsCauseResolved("keep") {
		t.Fatalf("state = %+v", state)
	}
}

// A cause rewritten between staging and removal is a fresh acknowledgement and
// must survive, exactly as the pattern scope behaves.
func TestRemoveMatchingPreservesRewrittenCause(t *testing.T) {
	dir := t.TempDir()
	original := Entry{Watermark: "1", ResolvedBy: "old"}
	if err := (&State{Resolved: map[string]Entry{}, Causes: map[string]Entry{"sig": original}}).Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := Update(dir, func(state *State) bool {
		state.Causes["sig"] = Entry{Watermark: "2", ResolvedBy: "admin"}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMatching(dir, &State{Causes: map[string]Entry{"sig": original}}); err != nil {
		t.Fatal(err)
	}
	if got := Load(dir).Causes["sig"]; got.ResolvedBy != "admin" || got.Watermark != "2" {
		t.Fatalf("entry = %+v", got)
	}
}
