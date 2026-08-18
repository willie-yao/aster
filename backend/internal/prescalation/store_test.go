package prescalation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreRoundTripsPullRequestResults(t *testing.T) {
	dir := t.TempDir()
	store := FileStore[Ref]{Dir: dir, Name: StateFileName}
	ref := Ref{PullNumber: 7, JobID: "example/project/pull-e2e", BuildID: "100", TestName: "[It] boots"}
	want := map[string]View[Ref]{
		ref.identity(): {Ref: ref, State: StateComplete, RootCause: "quota exhausted"},
	}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[ref.identity()].Ref != ref {
		t.Fatalf("loaded = %+v", got)
	}
	if got[ref.identity()].RootCause != "quota exhausted" {
		t.Errorf("root cause = %q", got[ref.identity()].RootCause)
	}
}

// The persisted ref keeps its original field names, so an upgrade does not
// strand results written by an earlier build.
func TestFileStoreKeepsThePullRequestRefWireShape(t *testing.T) {
	dir := t.TempDir()
	ref := Ref{PullNumber: 7, JobID: "job", BuildID: "100", TestName: "[It] boots"}
	store := FileStore[Ref]{Dir: dir, Name: StateFileName}
	if err := store.Save(map[string]View[Ref]{"k": {Ref: ref, State: StateComplete}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, StateFileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		Results map[string]struct {
			Ref map[string]any `json:"ref"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"pull_number", "job_id", "build_id", "test_name"} {
		if _, ok := doc.Results["k"].Ref[field]; !ok {
			t.Errorf("persisted ref missing %q", field)
		}
	}
}

// Escalation kinds key their records differently, so sharing a file would make
// one kind's results unreadable to the other. A missing name must fail loudly
// at construction rather than default into a collision.
func TestFileStoreRequiresAName(t *testing.T) {
	store := FileStore[Ref]{Dir: t.TempDir()}
	if _, err := store.Load(); err == nil {
		t.Error("Load must reject a store with no file name")
	}
	if err := store.Save(map[string]View[Ref]{}); err == nil {
		t.Error("Save must reject a store with no file name")
	}
}

func TestPullRequestAndClusterStoresAreSeparateFiles(t *testing.T) {
	if StateFileName == ClusterStateFileName {
		t.Fatal("the two escalation kinds must not share a state file")
	}
}

func TestFileStoreTreatsACorruptFileAsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, StateFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := FileStore[Ref]{Dir: dir, Name: StateFileName}.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("loaded = %+v, want an empty set", got)
	}
}
