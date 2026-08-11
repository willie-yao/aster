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
		"Delete the admission webhook configuration so no ConversionReview objects are sent to ASO.",
		"Delete the admission webhook configuration so the API server no longer posts ConversionReview objects to ASO.",
		"Delete the admission webhook configuration to prevent the API server from posting ConversionReview objects to ASO.",
		"Delete the admission webhook configuration so the API server stops posting ConversionReview objects to ASO.",
		"Delete the admission webhook configuration so zero ConversionReview objects are sent to ASO.",
		"Delete the admission webhook configuration so none of the ConversionReview objects are sent to ASO.",
		"Delete the admission webhook configuration so none of these ConversionReview objects are sent to ASO.",
		"Delete the admission webhook configuration so not a single ConversionReview object is sent to ASO.",
		"Delete the admission webhook configuration so ConversionReview delivery is eliminated.",
		"Purge the CRD conversion webhook before upgrade.",
		"Decommission the CRD conversion webhook before upgrade.",
		"Erase the CRD conversion webhook configuration before upgrade.",
		"The CRD conversion webhook is purged before upgrade.",
		"The CRD conversion webhook becomes decommissioned before upgrade.",
		"Delete the admission webhook configuration to stop ConversionReview delivery and prevent failures.",
		"Eliminate ConversionReview delivery to prevent failures.",
		"Block ConversionReview delivery to avoid failures.",
		"The conversion webhook remains unavailable.",
		"The conversion webhook remains unreachable.",
		"The conversion webhook remains purged.",
		"The conversion webhook stays offline.",
		"Do not preserve the conversion webhook.",
		"Never keep the conversion webhook available.",
		"Do not ensure the conversion webhook remains available.",
		"The conversion webhook remains available until it becomes unavailable.",
		"The conversion webhook remains available during migration and becomes unavailable afterward.",
		"The conversion webhook remains available until it becomes broken.",
		"The conversion webhook remains available during migration and gets torn down afterward.",
		"Prevent failures by stopping ConversionReview delivery.",
		"Avoid failures by disabling ConversionReview delivery.",
		"To prevent failures, stop ConversionReview delivery.",
		"Prevent failures through stopping ConversionReview delivery.",
		"Prevent failures with stopped ConversionReview delivery.",
		"The conversion webhook remains available until it fails.",
		"The conversion webhook remains available until it crashes.",
		"The conversion webhook remains available and then fails.",
		"The conversion webhook remains available while tests run, then fails.",
		"The conversion webhook remains available until it dies.",
		"The conversion webhook remains available while it hangs.",
		"The conversion webhook remains available until it serves no requests.",
		"The conversion webhook remains available until it serves zero requests.",
		"The conversion webhook remains available until it completes no migrations.",
		"The conversion webhook remains available until it fails over to nowhere.",
		"The conversion webhook remains available until it serves neither reads nor writes.",
		"The conversion webhook remains available until it serves hardly any requests.",
		"The conversion webhook remains available until it fails over to a dead endpoint.",
		"The conversion webhook remains available until it completes an invalid migration.",
		"The conversion webhook remains available until it serves only one request.",
		"The conversion webhook remains available until it serves few requests.",
		"The conversion webhook remains available until it serves failed requests.",
		"The conversion webhook remains available until it completes a corrupted migration.",
		"The conversion webhook remains available until it fails over to a broken endpoint.",
		"The conversion webhook remains available until it serves every request incorrectly.",
		"The conversion webhook remains available until it serves all requests with errors.",
		"The conversion webhook remains available until it completes an unsuccessful migration.",
		"The conversion webhook remains available until it fails over to an unhealthy backup.",
		"The conversion webhook remains available until it fails over to a degraded standby.",
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
		"Keep the CRD conversion webhook available until all stored objects are migrated.",
		"Keep conversion available and prevent conversion outages during shutdown.",
		"Delete the obsolete admission webhook configurations. Add shutdown coordination to prevent conversion calls from failing during provider deletion.",
		"Delete the obsolete admission webhook configurations while keeping conversion available and preventing API server conversion calls from failing.",
		"Remove the conversion webhook retry override while keeping conversion available.",
		"Delete the obsolete admission webhook configuration while keeping conversion available so API server conversion calls will not fail.",
		"Delete the obsolete admission webhook configuration while preserving conversion availability so API server conversion calls cannot fail.",
		"Delete the obsolete admission webhook configuration while keeping conversion available so ConversionReview requests cannot fail to reach ASO.",
		"Delete the obsolete admission webhook configuration while preserving conversion to prevent ConversionReview requests from failing to reach ASO.",
		"Delete the obsolete admission webhook configuration to prevent the API server from failing to post ConversionReview objects to ASO while preserving conversion.",
		"Delete the obsolete admission webhook configuration to prevent failures when posting ConversionReview objects to ASO while keeping conversion available.",
		"Delete the obsolete admission webhook configuration to prevent failed delivery of ConversionReview objects to ASO while keeping conversion available.",
		"Delete the obsolete admission webhook configuration to prevent ConversionReview delivery from failing while keeping conversion available.",
		"Delete the obsolete admission webhook configuration while ensuring ConversionReview delivery does not fail and keeping conversion available.",
		"Rotate the conversion webhook certificate before upgrade.",
		"Update the conversion webhook certificate before upgrade.",
		"Renew the conversion webhook certificate before upgrade.",
		"Verify the conversion webhook remains available after cleanup.",
		"Check the conversion webhook availability after cleanup.",
		"Avoid deleting the conversion webhook.",
		"Without deleting the conversion webhook, remove the admission configuration.",
		"The conversion webhook is healthy and available.",
		"The conversion webhook can continue serving.",
		"Prevent blocking ConversionReview delivery.",
		"Avoid blocking ConversionReview delivery.",
		"Prevent the API server from blocking ConversionReview delivery.",
		"Take steps to prevent any component from blocking ConversionReview delivery.",
		"Ensure ConversionReview delivery is no longer blocked.",
		"Ensure ConversionReview delivery is no longer being blocked.",
		"Do not ever delete the conversion webhook.",
		"Do not accidentally delete the conversion webhook.",
		"Never fail to preserve the conversion webhook.",
		"Do not stop preserving the conversion webhook.",
		"The conversion webhook remains available until it completes migration.",
		"The conversion webhook remains available until it serves all requests.",
		"Verify the conversion webhook remains available until it completes migration.",
		"The conversion webhook remains available until it fails over to the backup endpoint.",
		"Prevent accidental blocking of ConversionReview delivery.",
		"Prevent unintended disabling of ConversionReview delivery.",
		"The conversion webhook remains available until it keeps serving requests.",
		"The conversion webhook remains available until it serves requests without error.",
		"The conversion webhook remains available until it completes migration without failure.",
		"The conversion webhook remains available until it completes migration with no errors.",
		"The conversion webhook remains available until it fails over without downtime.",
		"The conversion webhook remains available until it successfully completes migration.",
		"The conversion webhook remains available until it reliably serves all requests.",
		"The conversion webhook remains available until it seamlessly fails over to the backup endpoint.",
		"The conversion webhook remains available until it serves requests reliably.",
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
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.Teardown", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.Destroy", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.EnsureDeleted", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "PurgeConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "DecommissionConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "EraseConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.Purge", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook/v1.Delete", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversion/webhook.Delete", Path: "reconcile.go"},
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
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "DELETE_CONVERSION_WEBHOOK_CERTIFICATE_AND_CONFIGURATION", Value: "true"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "DISABLE_CONVERSION_WEBHOOK_RETRY_AND_WEBHOOK", Value: "true"},
		{Intent: models.RemediationIntentSetConfiguration, Path: "crd.yaml", Value: "conversionWebhook.certificateAndEnabled=false"},
		{Intent: models.RemediationIntentSetConfiguration, Path: "crd.yaml", Value: "conversionWebhook.retryAndEnabled=false"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "DELETE_CONVERSION_WEBHOOK", Value: "force"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "CONVERSION_WEBHOOK_ENABLED", Value: "maybe"},
		{Intent: models.RemediationIntentSetConfiguration, Path: "crd.yaml", Value: "conversionWebhook.enabled=never"},
		{Intent: models.RemediationIntentSetConfiguration, Path: "crd.yaml", Value: "conversionWebhook.enabled=maybe"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "PRESERVE_CONVERSION_WEBHOOK", Value: "false"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "KEEP_CONVERSION_WEBHOOK_AVAILABLE", Value: "false"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "ENSURE_CONVERSION_WEBHOOK_AVAILABLE", Value: "false"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "PRESERVE_AND_DELETE_CONVERSION_WEBHOOK", Value: "true"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "KEEP_CONVERSION_WEBHOOK_AVAILABLE_AND_DISABLE", Value: "true"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "ENSURE_CONVERSION_WEBHOOK_AVAILABLE_AND_DELETE", Value: "true"},
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
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "RemoveConversionWebhookCertificateDependencies", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "RemoveCertificatesFromConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "DisableRetriesForConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.RemoveCertificateDependencies", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.EnsureCertificateDeleted", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversion/cache.DeleteEntry", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "RotateConversionWebhookCertificate", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "EnsureConversionWebhookAvailable", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook/v1.RotateCertificate", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "PreserveConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "MaintainConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "KeepConversionWebhook", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.Preserve", Path: "reconcile.go"},
		{Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/conversionwebhook.IsAvailable", Path: "reconcile.go"},
		{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Path: "config/jobs/example.yaml", Job: "job", Container: "test", Name: "CONVERSION_WEBHOOK_ENABLED", Value: "true"},
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
