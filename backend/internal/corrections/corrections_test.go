package corrections

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysischat"
)

type fakeSource struct {
	candidate    analysischat.CorrectionCandidate
	candidateErr error
	validateErr  error
	validations  atomic.Int32
}

func (f *fakeSource) CorrectionCandidate(sessionID, owner, requestID string) (analysischat.CorrectionCandidate, error) {
	if f.candidateErr != nil {
		return analysischat.CorrectionCandidate{}, f.candidateErr
	}
	candidate := f.candidate
	candidate.SessionID = sessionID
	candidate.RequestID = requestID
	return candidate, nil
}

func (f *fakeSource) ValidateCorrectionCandidate(candidate analysischat.CorrectionCandidate) error {
	f.validations.Add(1)
	return f.validateErr
}

func correctionCandidate() analysischat.CorrectionCandidate {
	return analysischat.CorrectionCandidate{
		Analysis: analysischat.AnalysisRef{
			JobID: "periodic-demo", BuildID: "123", TestName: "TestCluster",
			SuiteName: "suite", ClassName: "class", JUnitFile: "junit.xml",
			AnalysisGeneratedAt: "2026-07-24T12:00:00Z",
		},
		Original:  analysischat.Revision{RootCause: "old cause", SuggestedFix: "old fix"},
		Proposed:  analysischat.Revision{RootCause: "new cause", SuggestedFix: "new fix"},
		Citations: []analysischat.Citation{{Path: "build-log.txt", LineStart: 4, LineEnd: 4, Quote: "first failure"}},
	}
}

func TestServicePreviewConfirmAndRevoke(t *testing.T) {
	dir := t.TempDir()
	source := &fakeSource{candidate: correctionCandidate()}
	service, err := NewService(dir, source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview("session-1", "request-1", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if preview.Token == "" || preview.Proposed.RootCause != "new cause" || len(preview.Citations) != 1 {
		t.Fatalf("preview = %+v", preview)
	}
	replayed, err := service.Preview("session-1", "request-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Token != preview.Token {
		t.Fatalf("idempotent preview token = %q, want %q", replayed.Token, preview.Token)
	}
	confirmed, err := service.Confirm(preview.Token, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != StatusActive || confirmed.Revision.RootCause != "new cause" || confirmed.CorrectedBy != "alice" {
		t.Fatalf("confirmed = %+v", confirmed)
	}
	confirmedAgain, err := service.Confirm(preview.Token, "alice")
	if err != nil || confirmedAgain.ID != confirmed.ID {
		t.Fatalf("idempotent confirm = %+v err=%v", confirmedAgain, err)
	}
	if source.validations.Load() != 1 {
		t.Fatalf("validations = %d, want 1", source.validations.Load())
	}
	public := readPublicState(t, dir)
	if got := public.Corrections[analysisKey(preview.Analysis)]; got.ID != confirmed.ID || got.Status != StatusActive {
		t.Fatalf("public correction = %+v", got)
	}
	revoked, err := service.Revoke(confirmed.ID, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Status != StatusRevoked || revoked.RevokedBy != "bob" || revoked.RevokedAt == "" {
		t.Fatalf("revoked = %+v", revoked)
	}
	public = readPublicState(t, dir)
	if got := public.Corrections[analysisKey(preview.Analysis)]; got.Status != StatusRevoked {
		t.Fatalf("public revoked correction = %+v", got)
	}
}

func TestServiceRejectsOwnerAndStaleAnalysis(t *testing.T) {
	dir := t.TempDir()
	source := &fakeSource{candidate: correctionCandidate()}
	service, err := NewService(dir, source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview("session-1", "request-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Confirm(preview.Token, "bob"); !errors.Is(err, ErrPreviewNotFound) {
		t.Fatalf("cross-owner confirm error = %v", err)
	}
	source.validateErr = analysischat.ErrAnalysisChanged
	if _, err := service.Confirm(preview.Token, "alice"); !errors.Is(err, analysischat.ErrAnalysisChanged) {
		t.Fatalf("stale analysis confirm error = %v", err)
	}
	if len(readPublicState(t, dir).Corrections) != 0 {
		t.Fatal("stale proposal was published")
	}
}

func TestServiceExpiresPreview(t *testing.T) {
	dir := t.TempDir()
	var nowNanos atomic.Int64
	start := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	nowNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, nowNanos.Load()) }
	service, err := NewService(dir, &fakeSource{candidate: correctionCandidate()}, Options{PreviewTTL: time.Minute, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.Preview("session-1", "request-1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	nowNanos.Store(start.Add(time.Minute).UnixNano())
	if _, err := service.Confirm(preview.Token, "alice"); !errors.Is(err, ErrPreviewExpired) {
		t.Fatalf("expired preview confirm error = %v", err)
	}
}

func TestServiceSupersedesPriorCorrection(t *testing.T) {
	dir := t.TempDir()
	source := &fakeSource{candidate: correctionCandidate()}
	first, err := NewService(dir, source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	preview1, _ := first.Preview("session-1", "request-1", "alice")
	correction1, err := first.Confirm(preview1.Token, "alice")
	if err != nil {
		t.Fatal(err)
	}
	source.candidate.Proposed.RootCause = "newer cause"
	second, err := NewService(dir, source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	preview2, _ := second.Preview("session-2", "request-2", "alice")
	correction2, err := second.Confirm(preview2.Token, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if correction2.ID == correction1.ID {
		t.Fatal("replacement correction reused prior ID")
	}
	public := readPublicState(t, dir)
	if got := public.Corrections[analysisKey(preview2.Analysis)]; got.ID != correction2.ID || got.Revision.RootCause != "newer cause" {
		t.Fatalf("replacement public correction = %+v", got)
	}
	private := readPrivateState(t, dir)
	if private.Corrections[correction1.ID].Status != statusSuperseded {
		t.Fatalf("prior correction status = %q", private.Corrections[correction1.ID].Status)
	}
}

func readPublicState(t *testing.T, dir string) PublicState {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, PublicFileName))
	if err != nil {
		t.Fatal(err)
	}
	var state PublicState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func readPrivateState(t *testing.T, dir string) state {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	var state state
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}
