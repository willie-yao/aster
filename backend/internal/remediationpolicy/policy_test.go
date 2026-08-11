package remediationpolicy

import (
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestReasonAdmissionConversionClaims(t *testing.T) {
	actionable := []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "getPreUpgradeFunc",
		RequiredCall: "example/asomigration.DeleteWebhookConfigurations", Path: "test/e2e/capi_test.go",
	}}
	for _, text := range []string{
		"Delete the ASO mutating and validating webhook configurations so CRD conversion no longer calls ASO.",
		"Remove the admission webhook configuration to bypass conversion.",
		"Clear the validating webhook configuration to disable CRD conversion.",
		"Unset the mutating webhook configuration and turn off conversion.",
		"Drop the admission webhook configuration to stop conversion.",
		"Remove the admission webhook configuration to prevent the API server from calling conversion.",
		"Delete the ASO mutating and validating webhook configurations so the API server stops calling the CRD conversion webhook.",
		"Delete the ASO mutating and validating webhook configurations so the API server will not call the CRD conversion webhook.",
		"Delete the ASO mutating and validating webhook configurations so CRD conversion ceases to call ASO.",
		"Delete the ASO mutating and validating webhook configurations so CRD conversion cannot call ASO.",
		"Delete the ASO mutating and validating webhook configurations so conversion calls to ASO are prevented.",
		"Delete the ASO mutating and validating webhook configurations so the conversion webhook is no longer invoked.",
		"Delete the admission webhook configurations so conversion no longer calls ASO, but do not disable conversion before cleanup completes.",
		"Do not disable the admission webhook directly, delete the admission webhook configuration so conversion no longer calls ASO.",
		"Delete the admission webhook configurations to prevent the API server from invoking conversion, avoiding an upgrade failure.",
		"Delete the admission webhook configuration so conversion no longer calls ASO but is not disabled.",
		"Delete the admission webhook configuration so the API server won't invoke conversion.",
		"Delete the admission webhook configuration so the API server skips conversion.",
		"Delete the admission webhook configuration so conversion requests no longer reach ASO.",
		"Do not disable the admission webhooks directly, delete their configurations so conversion no longer calls ASO.",
		"Delete the admission webhook configuration so the API server cannot send ConversionReview objects to ASO.",
		"Remove the admission webhook configuration so ConversionReview objects are no longer delivered to ASO.",
	} {
		if got := Reason(text, actionable); got != UnsafeConversionReason {
			t.Errorf("unsafe recommendation accepted: %q -> %q", text, got)
		}
	}
}

func TestReasonPreservesSafeWebhookChanges(t *testing.T) {
	actionable := []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "getPreUpgradeFunc",
		RequiredCall: "example/asomigration.DeleteWebhookConfigurations", Path: "test/e2e/capi_test.go",
	}}
	for _, text := range []string{
		"Delete the obsolete admission webhook configurations while keeping the CRD conversion webhook available until provider deletion completes.",
		"Remove the obsolete admission webhook configuration while keeping CRD conversion available.",
		"Delete the obsolete admission webhook configurations while keeping CRD conversion available. Do not disable CRD conversion.",
		"Delete the obsolete admission webhook configurations; CRD conversion is not disabled and remains available.",
		"Delete the obsolete admission webhook configurations while keeping conversion available. Do not skip conversion.",
		"Remove the conversion webhook certificate dependency while preserving the Webhook conversion strategy.",
		"Add shutdown coordination so conversion remains available until all stored objects are migrated.",
		"Keep conversion available and prevent conversion outages during shutdown.",
		"Delete the obsolete admission webhook configurations. Add shutdown coordination to prevent conversion calls from failing during provider deletion.",
		"Delete the obsolete admission webhook configurations while keeping conversion available and preventing API server conversion calls from failing.",
		"Remove the conversion webhook retry override while keeping conversion available.",
		"Delete the obsolete admission webhook configuration while keeping conversion available so API server conversion calls will not fail.",
		"Delete the obsolete admission webhook configuration while preserving conversion availability so API server conversion calls cannot fail.",
	} {
		if got := Reason(text, actionable); got != "" {
			t.Errorf("safe recommendation rejected: %q -> %q", text, got)
		}
	}
}

func TestReasonRejectsDestructiveStructuredTargets(t *testing.T) {
	for _, target := range []models.RemediationTarget{
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "ClearConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "UnsetConversionStrategy", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "BypassConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "TurnOffConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "DisableWebhookConversion", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example/migration.BypassWebhookConversion", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "StopCRDConversion", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "RetryDeleteConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "ShutdownDeleteConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "DeleteConversionWebhookCertificateAndConfiguration", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "RemoveConversionWebhookTimeoutAndWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "DisableRetryAndConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.Delete", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.DeleteCertificateAndConfiguration", Path: "reconcile.go"},
		{Intent: models.RemediationIntentRemoveConfiguration, Path: "crd.yaml", Value: "spec.conversion.webhook.clientConfig=service"},
		{Intent: models.RemediationIntentSetConfiguration, Path: "crd.yaml", Value: "spec.conversion.strategy=None"},
		{Intent: models.RemediationIntentSetConfiguration, Path: "crd.yaml", Value: "conversionWebhook.enabled=0"},
		{Intent: models.RemediationIntentSetConfiguration, Path: "crd.yaml", Value: "webhookConversion.enabled=false"},
		{Intent: models.RemediationIntentRemoveConfiguration, Path: "crd.yaml", Value: "webhookConversion.clientConfig=service"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "BYPASS_CONVERSION_WEBHOOK", Value: "true"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "DISABLE_WEBHOOK_CONVERSION", Value: "true"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "RETRY_DELETE_CONVERSION_WEBHOOK", Value: "true"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "RETRY_CONVERSION_WEBHOOK_DELETE", Value: "true"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "SHUTDOWN_CONVERSION_WEBHOOK_REMOVE", Value: "true"},
	} {
		if got := Reason("neutral wording", []models.RemediationTarget{target}); got != UnsafeConversionReason {
			t.Errorf("unsafe target accepted: %+v -> %q", target, got)
		}
	}
}

func TestReasonPreservesSafeStructuredDependencies(t *testing.T) {
	for _, target := range []models.RemediationTarget{
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "RemoveCertificateDependencyFromConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "RemoveConversionWebhookCertificateDependency", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "DisableRetryForConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "ConversionWebhookDisableRetry", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.RemoveCertificateDependency", Path: "reconcile.go"},
		{Intent: models.RemediationIntentSetConfiguration, Path: "crd.yaml", Value: "conversionRetry.enabled=false"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "DISABLE_CONVERSION_TIMEOUT_OVERRIDE", Value: "true"},
		{Intent: models.RemediationIntentSetConfiguration, Path: "crd.yaml", Value: "retry.conversionWebhook.enabled=false"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "DISABLE_RETRY_FOR_CONVERSION_WEBHOOK", Value: "true"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "CONVERSION_WEBHOOK_DISABLE_RETRY", Value: "true"},
	} {
		if got := Reason("Preserve conversion availability.", []models.RemediationTarget{target}); got != "" {
			t.Errorf("safe structured target rejected: %+v -> %q", target, got)
		}
	}
}

func TestReasonAllowsInvestigationOnly(t *testing.T) {
	if got := Reason("Delete the conversion webhook.", []models.RemediationTarget{{Intent: models.RemediationIntentInvestigate}}); got != "" {
		t.Fatalf("investigation-only result rejected: %q", got)
	}
}
