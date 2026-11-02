package actions

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/issues"
)

func TestPreviewStateRejectsInvalidStructure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		mutate func(*previewState)
	}{
		{name: "truncated", body: `{"version":5,"previews":{"unknown":`},
		{name: "null", body: `null`},
		{name: "empty object", body: `{}`},
		{name: "null previews", body: `{"version":5,"previews":null}`},
		{name: "nil record", mutate: func(s *previewState) { s.Previews["broken"] = nil }},
		{name: "unknown kind", mutate: func(s *previewState) { s.Previews["unknown"].Kind = "future-action" }},
		{name: "unknown status", mutate: func(s *previewState) { s.Previews["unknown"].Status = "future-status" }},
		{name: "invalid creation time", mutate: func(s *previewState) { s.Previews["unknown"].CreatedAt = "invalid" }},
		{name: "invalid owner binding", mutate: func(s *previewState) { s.Previews["unknown"].InitiatedBy = "other" }},
		{name: "missing unknown draft", mutate: func(s *previewState) { s.Previews["unknown"].Issue = nil }},
		{name: "missing action identity", mutate: func(s *previewState) { s.Previews["unknown"].Issue.Key = "" }},
		{name: "missing confirmed result", mutate: func(s *previewState) { s.Previews["unknown"].Status = previewStatusDone }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newPreviewStore(t.TempDir())
			before := []byte(tc.body)
			if tc.mutate != nil {
				now := time.Now().UTC().Format(time.RFC3339Nano)
				state := freshPreviewState()
				state.Previews["unknown"] = &persistedPreview{
					Owner: tokenHash("alice"), InitiatedBy: "alice", InitiatedAt: now, CreatedAt: now,
					Kind: "issue", Status: previewStatusUnknown, Issue: &issues.IssueSpec{Key: "pattern::unknown"},
				}
				tc.mutate(state)
				var err error
				before, err = json.Marshal(state)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(store.path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.stash("alice", &previewEntry{kind: "issue"}); err == nil {
				t.Fatal("invalid preview state admitted another preview")
			}
			after, err := os.ReadFile(store.path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("invalid state changed: err=%v", err)
			}
		})
	}
}

func TestGeneratingPreviewReservationSurvivesValidation(t *testing.T) {
	store := newPreviewStore(t.TempDir())
	token, _, acquired, err := store.reserveIdempotent("alice", "request-hash", "generation-hash", time.Hour)
	if err != nil || !acquired {
		t.Fatalf("reservation acquired=%t err=%v", acquired, err)
	}
	state, changed, err := store.load()
	if err != nil || changed {
		t.Fatalf("load changed=%t err=%v", changed, err)
	}
	record := state.Previews[tokenHash(token)]
	if record == nil || record.Status != previewStatusGenerating || record.Fix != nil {
		t.Fatalf("reservation = %+v", record)
	}
}
