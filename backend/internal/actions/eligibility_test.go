package actions

import (
	"context"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

const eligibilityRevision = "0123456789abcdef0123456789abcdef01234567"

func eligibilityService(t *testing.T, targets []models.RemediationTarget) (*Service, string) {
	t.Helper()
	pattern := models.PatternAnalysis{
		JobID: "periodic-x", Systemic: true, SuggestedFix: "Implement MissingHelper.",
		SourceRef: "example/repo@" + eligibilityRevision, RemediationTargets: targets,
		FileLinks: map[string]string{"main.go": "https://github.com/example/repo/blob/" + eligibilityRevision + "/main.go"},
	}
	models.AssignPatternIdentity(&pattern)
	dataDir := t.TempDir()
	writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pattern.JobID, PatternAnalyses: []models.PatternAnalysis{pattern}})
	service := NewService(&project.Config{AI: &project.AI{SourceRepo: &project.SourceRepo{Owner: "example", Name: "repo"}}}, dataDir, AIConfig{})
	return service, pattern.ID
}

func TestActionEligibilityClassifiesStructuredTargets(t *testing.T) {
	t.Run("missing targets", func(t *testing.T) {
		service, id := eligibilityService(t, nil)
		got, err := service.ActionEligibility(t.Context(), id)
		if err != nil || got.State != EligibilityMoreEvidenceRequired {
			t.Fatalf("eligibility = %+v, err=%v", got, err)
		}
	})

	t.Run("malformed investigation", func(t *testing.T) {
		service, id := eligibilityService(t, []models.RemediationTarget{{Intent: models.RemediationIntentInvestigate, Path: "main.go"}})
		got, err := service.ActionEligibility(t.Context(), id)
		if err != nil || got.State != EligibilityMoreEvidenceRequired {
			t.Fatalf("eligibility = %+v, err=%v", got, err)
		}
	})

	t.Run("investigation", func(t *testing.T) {
		service, id := eligibilityService(t, []models.RemediationTarget{{Intent: models.RemediationIntentInvestigate}})
		called := false
		service.sourceVerifier = func(context.Context, actionverify.Reader, actionverify.Input) (actionverify.Result, error) {
			called = true
			return actionverify.Result{}, nil
		}
		got, err := service.ActionEligibility(t.Context(), id)
		if err != nil || got.State != EligibilityInvestigationRequired || called {
			t.Fatalf("eligibility = %+v, called=%t, err=%v", got, called, err)
		}
	})

	for _, test := range []struct {
		name  string
		state string
		want  string
	}{
		{name: "actionable", state: actionverify.StateUnresolved, want: EligibilityActionable},
		{name: "already present", state: actionverify.StateAlreadyPresent, want: EligibilityAlreadyPresent},
		{name: "inconclusive", state: actionverify.StateInconclusive, want: EligibilityMoreEvidenceRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, id := eligibilityService(t, []models.RemediationTarget{{Intent: models.RemediationIntentAddSymbol, Symbol: "MissingHelper", Path: "main.go"}})
			service.sourceVerifier = func(context.Context, actionverify.Reader, actionverify.Input) (actionverify.Result, error) {
				return actionverify.Result{State: test.state, Reason: test.name}, nil
			}
			got, err := service.ActionEligibility(t.Context(), id)
			if err != nil || got.State != test.want {
				t.Fatalf("eligibility = %+v, err=%v", got, err)
			}
		})
	}
}
