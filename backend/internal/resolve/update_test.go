package resolve

import "testing"

func TestRemoveMatchingPreservesConcurrentEntries(t *testing.T) {
	dir := t.TempDir()
	if err := (&State{Resolved: map[string]Entry{"old": {Watermark: "1"}}}).Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := Update(dir, func(state *State) bool {
		state.Resolved["admin"] = Entry{Watermark: "2"}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMatching(dir, map[string]Entry{"old": {Watermark: "1"}}); err != nil {
		t.Fatal(err)
	}
	state := Load(dir)
	if state.IsResolved("old") || !state.IsResolved("admin") {
		t.Fatalf("state = %+v", state)
	}
}

func TestRemoveMatchingPreservesSameIDReplacement(t *testing.T) {
	dir := t.TempDir()
	original := Entry{Watermark: "1", ResolvedBy: "old"}
	if err := (&State{Resolved: map[string]Entry{"pattern": original}}).Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := Update(dir, func(state *State) bool {
		state.Resolved["pattern"] = Entry{Watermark: "2", ResolvedBy: "admin"}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMatching(dir, map[string]Entry{"pattern": original}); err != nil {
		t.Fatal(err)
	}
	if got := Load(dir).Resolved["pattern"]; got.ResolvedBy != "admin" || got.Watermark != "2" {
		t.Fatalf("entry = %+v", got)
	}
}
