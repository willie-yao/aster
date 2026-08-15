package models

import "testing"

func TestPatternID_StableAndDistinct(t *testing.T) {
	a := PatternAnalysis{JobID: "periodic-x", SharedRootCause: "etcd timeout"}
	b := PatternAnalysis{JobID: "periodic-x", SharedRootCause: "etcd timeout"}
	if PatternID(a) != PatternID(b) {
		t.Error("same job + cause must yield the same ID")
	}
	c := PatternAnalysis{JobID: "periodic-x", SharedRootCause: "oom killed"}
	if PatternID(a) == PatternID(c) {
		t.Error("different cause must yield a different ID")
	}
	d := PatternAnalysis{JobID: "periodic-y", SharedRootCause: "etcd timeout"}
	if PatternID(a) == PatternID(d) {
		t.Error("different job must yield a different ID")
	}
}

func TestPatternID_URLSafeAndNonEmpty(t *testing.T) {
	// Even with empty root cause, an ID is produced from the fallback fields.
	id := PatternID(PatternAnalysis{JobID: "job", Summary: "some summary"})
	if id == "" {
		t.Fatal("expected a non-empty ID")
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Errorf("ID %q is not hex/URL-safe", id)
		}
	}
}
