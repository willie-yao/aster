package actions

import (
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/resolve"
)

// multiCausePattern builds a published pattern with two independently signed
// causes, the shape per-cause resolution exists for.
func multiCausePattern() models.PatternAnalysis {
	pa := models.PatternAnalysis{
		JobID: "periodic-x", Subject: "periodic-x", Systemic: true,
		Recurrence:      models.PatternRecurrenceMixedCauses,
		SharedRootCause: "two unrelated causes",
		CausalGroups: []models.PatternCausalGroup{
			{
				Builds: []string{"100", "250", "175"}, Confidence: "high",
				RootCause: "CreateOrUpdate on the azure-cni DaemonSet failed with HTTP 409 Conflict",
				Signature: "cni409",
			},
			{
				Builds: []string{"300", "120"}, Confidence: "medium",
				RootCause: "etcd leader election times out",
				Signature: "etcdleader",
			},
		},
		SharedBuilds: []string{"100", "250", "175", "300", "120"},
	}
	models.AssignPatternIdentity(&pa)
	return pa
}

func TestResolveUnresolveCauseRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	pa := multiCausePattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})

	if err := s.ResolveCause("cni409", "willie-yao", "fixed by test-infra #123"); err != nil {
		t.Fatalf("ResolveCause: %v", err)
	}
	state := resolve.Load(dataDir)
	entry, ok := state.Causes["cni409"]
	if !ok {
		t.Fatal("cause should be resolved")
	}
	// The watermark is the cause's own highest build, not the pattern's.
	if entry.Watermark != "250" {
		t.Errorf("watermark = %q, want 250", entry.Watermark)
	}
	if entry.ResolvedBy != "willie-yao" || entry.Note != "fixed by test-infra #123" || entry.Subject != pa.Subject {
		t.Errorf("entry metadata wrong: %+v", entry)
	}
	if !strings.HasPrefix(entry.Cause, "CreateOrUpdate on the azure-cni DaemonSet") {
		t.Errorf("cause excerpt = %q, want the root cause", entry.Cause)
	}

	// Resolving one cause must leave its sibling and the pattern alone.
	if state.IsCauseResolved("etcdleader") {
		t.Error("sibling cause should be untouched")
	}
	if state.IsResolved(pa.ID) {
		t.Error("the pattern itself should be untouched")
	}

	if err := s.UnresolveCause("cni409"); err != nil {
		t.Fatalf("UnresolveCause: %v", err)
	}
	if resolve.Load(dataDir).IsCauseResolved("cni409") {
		t.Fatal("cause should be unresolved")
	}
}

// A group the engine could not sign has no durable identity to key a resolution
// under, so it is not addressable at cause scope at all.
func TestResolveCauseRejectsUnsignedGroup(t *testing.T) {
	dataDir := t.TempDir()
	pa := multiCausePattern()
	pa.CausalGroups[0].Signature = ""
	models.AssignPatternIdentity(&pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})

	if err := s.ResolveCause("", "willie-yao", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveCause error = %v, want ErrNotFound", err)
	}
}

func TestResolveCauseRejectsUnknownSignature(t *testing.T) {
	dataDir := t.TempDir()
	pa := multiCausePattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})

	if err := s.ResolveCause("nosuchcause", "willie-yao", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResolveCause error = %v, want ErrNotFound", err)
	}
}

// One signature on two published groups identifies more than one cause, so
// resolving it would acknowledge a cause the maintainer never saw.
func TestResolveCauseRejectsAmbiguousSignature(t *testing.T) {
	dataDir := t.TempDir()
	pa := multiCausePattern()
	pa.CausalGroups[1].Signature = pa.CausalGroups[0].Signature
	models.AssignPatternIdentity(&pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})

	err := s.ResolveCause("cni409", "willie-yao", "")
	if err == nil || !strings.Contains(err.Error(), "more than one published cause") {
		t.Fatalf("ResolveCause error = %v, want the ambiguity rejection", err)
	}
	if resolve.Load(dataDir).IsCauseResolved("cni409") {
		t.Fatal("rejected resolve should not persist")
	}
}

func TestResolveCauseRejectsInactivePattern(t *testing.T) {
	dataDir := t.TempDir()
	pa := multiCausePattern()
	pa.Lifecycle = &models.PatternLifecycle{State: models.PatternLifecycleVerifiedFixed, Reason: "verified fixed"}
	models.AssignPatternIdentity(&pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})

	if err := s.ResolveCause("cni409", "willie-yao", ""); err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("ResolveCause error = %v, want the inactive-lifecycle rejection", err)
	}
}

func TestResolveCauseRejectsGroupWithoutBuildHistory(t *testing.T) {
	dataDir := t.TempDir()
	pa := multiCausePattern()
	pa.CausalGroups[0].Builds = nil
	models.AssignPatternIdentity(&pa)
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})

	if err := s.ResolveCause("cni409", "willie-yao", ""); err == nil || !strings.Contains(err.Error(), "no build history") {
		t.Fatalf("ResolveCause error = %v, want the watermark rejection", err)
	}
}

// A cause resolution outlives the window its group was published in, so it has
// to stay clearable once the group is gone.
func TestUnresolveCauseClearsAnAgedOutCause(t *testing.T) {
	dataDir := t.TempDir()
	pa := multiCausePattern()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID, PatternAnalyses: []models.PatternAnalysis{pa}})
	s := NewService(&project.Config{}, dataDir, AIConfig{})
	if err := s.ResolveCause("cni409", "willie-yao", ""); err != nil {
		t.Fatalf("ResolveCause: %v", err)
	}

	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pa.JobID})
	if _, _, err := s.findCause("cni409"); err == nil {
		t.Fatal("cause should no longer be published")
	}
	if err := s.UnresolveCause("cni409"); err != nil {
		t.Fatalf("UnresolveCause: %v", err)
	}
	if resolve.Load(dataDir).IsCauseResolved("cni409") {
		t.Fatal("aged-out cause should be reopenable")
	}
}

func TestUnresolveCauseNotFound(t *testing.T) {
	s := NewService(&project.Config{}, t.TempDir(), AIConfig{})
	if err := s.UnresolveCause("cni409"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UnresolveCause error = %v, want ErrNotFound", err)
	}
}
