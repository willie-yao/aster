package resolve

import "testing"

func TestRemoveIDsPreservesConcurrentEntries(t *testing.T) {
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
	if err := RemoveIDs(dir, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	state := Load(dir)
	if state.IsResolved("old") || !state.IsResolved("admin") {
		t.Fatalf("state = %+v", state)
	}
}
