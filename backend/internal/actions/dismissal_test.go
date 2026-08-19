package actions

import (
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/resolve"
)

// causalGroupPattern builds a published causal-group pattern: the only shape the
// engine emits today, so dismissal has to work for it.
func causalGroupPattern(recurrence models.PatternRecurrence, systemic bool) models.PatternAnalysis {
	pa := models.PatternAnalysis{
		JobID: "periodic-x", Subject: "periodic-x", Systemic: systemic,
		Recurrence:      recurrence,
		SharedRootCause: "etcd leader election times out",
		CausalGroups: []models.PatternCausalGroup{{
			Builds: []string{"100", "250", "175"}, RootCause: "etcd leader election times out", Confidence: "high",
		}},
		SharedBuilds: []string{"100", "250", "175"},
	}
	models.AssignPatternIdentity(&pa)
	return pa
}

func TestResolveUnresolveCausalGroupPattern(t *testing.T) {
	dataDir := t.TempDir()
	pa := causalGroupPattern(models.PatternRecurrenceSharedCause, true)
	if models.PatternAllowsActions(pa) {
		t.Fatal("fixture should be an analysis-only causal-group pattern")
	}
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})

	if err := s.Resolve(pa.ID, "willie-yao", "fixed by test-infra #123"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	entry, ok := resolve.Load(dataDir).Resolved[pa.ID]
	if !ok {
		t.Fatal("causal-group pattern should be resolved")
	}
	if entry.Watermark != "250" {
		t.Errorf("watermark = %q, want 250", entry.Watermark)
	}
	if entry.ResolvedBy != "willie-yao" || entry.Note != "fixed by test-infra #123" || entry.Subject != pa.Subject {
		t.Errorf("entry metadata wrong: %+v", entry)
	}

	if err := s.Unresolve(pa.ID); err != nil {
		t.Fatalf("Unresolve: %v", err)
	}
	if resolve.Load(dataDir).IsResolved(pa.ID) {
		t.Fatal("causal-group pattern should be unresolved")
	}
}

func TestResolveRejectsNonSystemicCausalGroupRecurrence(t *testing.T) {
	for _, recurrence := range []models.PatternRecurrence{
		models.PatternRecurrenceUnrelated,
		models.PatternRecurrenceInsufficientEvidence,
	} {
		t.Run(string(recurrence), func(t *testing.T) {
			dataDir := t.TempDir()
			pa := causalGroupPattern(recurrence, false)
			writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
			s := NewService(&project.Config{}, dataDir, AIConfig{})
			err := s.Resolve(pa.ID, "willie-yao", "")
			if err == nil || !strings.Contains(err.Error(), "only systemic recurring patterns") {
				t.Fatalf("Resolve error = %v, want the systemic rejection", err)
			}
			if resolve.Load(dataDir).IsResolved(pa.ID) {
				t.Fatal("rejected resolve should not persist")
			}
		})
	}
}

func TestResolveRejectsInactiveCausalGroupPattern(t *testing.T) {
	dataDir := t.TempDir()
	pa := causalGroupPattern(models.PatternRecurrenceSharedCause, true)
	pa.Lifecycle = &models.PatternLifecycle{State: models.PatternLifecycleVerifiedFixed, Reason: "verified fixed"}
	models.AssignPatternIdentity(&pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})
	if err := s.Resolve(pa.ID, "willie-yao", ""); err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("Resolve error = %v, want the inactive-lifecycle rejection", err)
	}
}

// Dismissal is not a remediation-contract action, so it must stay reachable
// while drafting an issue or fix PR remains blocked for the same pattern.
func TestCausalGroupPatternStaysIneligibleForDrafting(t *testing.T) {
	pa := causalGroupPattern(models.PatternRecurrenceSharedCause, true)
	code, reason := subjectEligibilityReason(&ActionSubject{Kind: actionSubjectPattern, Pattern: &pa})
	if code != ReasonContractGenerationFailed || reason != "This causal-group result is analysis-only and cannot start an action." {
		t.Fatalf("code=%s reason=%q", code, reason)
	}
}

// A resolution outlives its pattern (resolve.State.Prune keeps entries whose
// pattern left the current set), so it has to stay clearable afterwards.
func TestUnresolveClearsAnAgedOutPattern(t *testing.T) {
	dataDir := t.TempDir()
	pa := causalGroupPattern(models.PatternRecurrenceSharedCause, true)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})
	if err := s.Resolve(pa.ID, "willie-yao", ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// The pattern ages out of the published set while its resolution is kept.
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID})
	if _, err := s.findPattern(pa.ID); err == nil {
		t.Fatal("pattern should no longer be published")
	}
	if err := s.Unresolve(pa.ID); err != nil {
		t.Fatalf("Unresolve: %v", err)
	}
	if resolve.Load(dataDir).IsResolved(pa.ID) {
		t.Fatal("aged-out pattern should be restorable")
	}
}

func TestResolveRejectsPatternWithoutBuildHistory(t *testing.T) {
	dataDir := t.TempDir()
	pa := causalGroupPattern(models.PatternRecurrenceSharedCause, true)
	pa.SharedBuilds = nil
	models.AssignPatternIdentity(&pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})
	if err := s.Resolve(pa.ID, "willie-yao", ""); err == nil || !strings.Contains(err.Error(), "no build history") {
		t.Fatalf("Resolve error = %v, want the watermark rejection", err)
	}
}
