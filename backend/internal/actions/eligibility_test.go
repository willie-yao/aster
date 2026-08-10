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

	t.Run("modify symbol without required call", func(t *testing.T) {
		service, id := eligibilityService(t, []models.RemediationTarget{{
			Intent: models.RemediationIntentModifySymbol, Symbol: "Reconcile", Path: "controllers/reconcile.go",
		}})
		called := false
		service.sourceVerifier = func(context.Context, actionverify.Reader, actionverify.Input) (actionverify.Result, error) {
			called = true
			return actionverify.Result{}, nil
		}
		got, err := service.ActionEligibility(t.Context(), id)
		if err != nil || got.State != EligibilityMoreEvidenceRequired || called {
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

func TestActionEligibilityAllowsPinnedTestInfraEnvironmentTarget(t *testing.T) {
	const revision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	target := models.RemediationTarget{
		Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: revision,
		Path: "config/jobs/kubernetes-sigs/cluster-api-provider-azure/periodics.yaml", Job: "periodic-capz", Container: "test",
		Name: "AKS_MGMT_KUBERNETES_VERSION", Value: "v1.34.1",
	}
	pattern := models.PatternAnalysis{JobID: "periodic-capz", Systemic: true, SuggestedFix: "update the Prow job environment", RemediationTargets: []models.RemediationTarget{target}}
	models.AssignPatternIdentity(&pattern)
	dataDir := t.TempDir()
	writeJobDetail(t, dataDir, "periodic-capz.json", models.JobDetail{JobID: pattern.JobID, PatternAnalyses: []models.PatternAnalysis{pattern}})
	cfg := &project.Config{Branding: project.Branding{SourceRepo: project.SourceRepo{Owner: "example", Name: "source"}}, AI: &project.AI{SourceRepo: &project.SourceRepo{Owner: "example", Name: "source"}, FixPRs: &project.FixPRs{
		AllowedRepositories: []project.FixRepository{{Owner: "kubernetes", Name: "test-infra", PathPrefixes: []string{"config/jobs/kubernetes-sigs/cluster-api-provider-azure/"}}},
	}}}
	service := NewService(cfg, dataDir, AIConfig{})
	service.sourceVerifier = func(_ context.Context, _ actionverify.Reader, input actionverify.Input) (actionverify.Result, error) {
		if len(input.Targets) != 1 || input.Targets[0] != target {
			t.Fatalf("targets = %+v", input.Targets)
		}
		return actionverify.Result{State: actionverify.StateUnresolved, Reason: "verified"}, nil
	}
	got, err := service.ActionEligibility(t.Context(), pattern.ID)
	if err != nil || got.State != EligibilityActionable {
		t.Fatalf("eligibility=%+v err=%v", got, err)
	}
}

func TestActionEligibilityBlocksInactivePatternLifecycle(t *testing.T) {
	for _, test := range []struct {
		state models.PatternLifecycleState
		want  string
	}{
		{state: models.PatternLifecycleRecovered, want: EligibilityRecovered},
		{state: models.PatternLifecycleObserving, want: EligibilityAlreadyPresent},
		{state: models.PatternLifecycleVerifiedFixed, want: EligibilityAlreadyPresent},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			pattern := models.PatternAnalysis{
				JobID: "periodic-x", Systemic: true, SuggestedFix: "fix", Lifecycle: &models.PatternLifecycle{State: test.state, Reason: "not actionable"},
				RemediationTargets: []models.RemediationTarget{{Intent: models.RemediationIntentAddSymbol, Symbol: "Fix", Path: "fix.go"}},
			}
			models.AssignPatternIdentity(&pattern)
			dataDir := t.TempDir()
			writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pattern.JobID, PatternAnalyses: []models.PatternAnalysis{pattern}})
			service := NewService(&project.Config{}, dataDir, AIConfig{})
			got, err := service.ActionEligibility(t.Context(), pattern.ID)
			if err != nil || got.State != test.want || got.Reason != "not actionable" {
				t.Fatalf("eligibility=%+v err=%v", got, err)
			}
		})
	}
}

func TestActionEligibilityRequiresProvenRequiredCall(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		files map[string]string
		call  string
		want  string
	}{
		{
			name:  "fabricated helper",
			files: map[string]string{"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n"},
			call:  "fabricatedHelper", want: EligibilityMoreEvidenceRequired,
		},
		{
			name: "existing helper missing call",
			files: map[string]string{
				"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
				"controllers/fix.go":       "package controllers\nfunc applyFix() {}\n",
			},
			call: "applyFix", want: EligibilityActionable,
		},
		{
			name: "existing call",
			files: map[string]string{
				"controllers/reconcile.go": "package controllers\nfunc reconcile() { applyFix() }\n",
				"controllers/fix.go":       "package controllers\nfunc applyFix() {}\n",
			},
			call: "applyFix", want: EligibilityAlreadyPresent,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, id := eligibilityService(t, []models.RemediationTarget{{
				Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: testCase.call, Path: "controllers/reconcile.go",
			}})
			reader := fakeActionSourceReader(testCase.files)
			service.sourceVerifier = func(ctx context.Context, _ actionverify.Reader, input actionverify.Input) (actionverify.Result, error) {
				return actionverify.Verify(ctx, reader, input)
			}
			got, err := service.ActionEligibility(t.Context(), id)
			if err != nil || got.State != testCase.want {
				t.Fatalf("eligibility=%+v err=%v", got, err)
			}
		})
	}
}

func TestActionEligibilityEnforcesAdmissionConversionPolicy(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	target := models.RemediationTarget{
		Intent: models.RemediationIntentModifySymbol, Symbol: "getPreUpgradeFunc",
		RequiredCall: "example/asomigration.DeleteWebhookConfigurations", Path: "test/e2e/capi_test.go",
	}
	for _, testCase := range []struct {
		name   string
		fix    string
		want   string
		called bool
	}{
		{
			name: "unsafe causal claim",
			fix:  "Delete the ASO mutating and validating webhook configurations so CRD conversion no longer calls ASO.",
			want: EligibilityMoreEvidenceRequired,
		},
		{
			name: "safe cleanup",
			fix:  "Delete the obsolete admission webhook configurations while keeping the CRD conversion webhook available until provider deletion completes.",
			want: EligibilityActionable, called: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pattern := models.PatternAnalysis{
				JobID: "periodic-x", Systemic: true, SuggestedFix: testCase.fix,
				SourceRef: "example/repo@" + revision, RemediationTargets: []models.RemediationTarget{target},
			}
			models.AssignPatternIdentity(&pattern)
			dataDir := t.TempDir()
			writeJobDetail(t, dataDir, "periodic-x.json", models.JobDetail{JobID: pattern.JobID, PatternAnalyses: []models.PatternAnalysis{pattern}})
			service := NewService(&project.Config{AI: &project.AI{SourceRepo: &project.SourceRepo{Owner: "example", Name: "repo"}}}, dataDir, AIConfig{})
			called := false
			service.sourceVerifier = func(context.Context, actionverify.Reader, actionverify.Input) (actionverify.Result, error) {
				called = true
				return actionverify.Result{State: actionverify.StateUnresolved}, nil
			}
			got, err := service.ActionEligibility(t.Context(), pattern.ID)
			if err != nil || got.State != testCase.want || called != testCase.called {
				t.Fatalf("eligibility=%+v called=%t err=%v", got, called, err)
			}
		})
	}
}
