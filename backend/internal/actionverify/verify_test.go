package actionverify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationpolicy"
)

type fakeReader struct {
	archive Archive
	err     error
}

func (f fakeReader) ReadSourceArchive(context.Context) (Archive, error) {
	return f.archive, f.err
}

func (f fakeReader) ReadFile(_ context.Context, path string) (string, bool, error) {
	if content, ok := f.archive.GoFiles[path]; ok {
		return content, true, nil
	}
	content, ok := f.archive.Files[path]
	return content, ok, nil
}

func archive(files map[string]string, extraPaths ...string) Archive {
	paths := map[string]bool{}
	goFiles := map[string]string{}
	stored := map[string]string{}
	for file, content := range files {
		paths[file] = true
		stored[file] = content
		if strings.HasSuffix(file, ".go") {
			goFiles[file] = content
		}
	}
	for _, file := range extraPaths {
		paths[file] = true
	}
	return Archive{Paths: paths, GoFiles: goFiles, Files: stored}
}

func TestVerifyStates(t *testing.T) {
	t.Run("unresolved", func(t *testing.T) {
		result := verify(t, fakeReader{archive: archive(map[string]string{
			"pkg/main.go": "package pkg\n",
		})}, Input{Proposal: "Implement `MissingHelper`.", RelevantFiles: []string{"pkg/main.go"}})
		if result.State != StateUnresolved {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("already present", func(t *testing.T) {
		result := verify(t, fakeReader{archive: archive(map[string]string{
			"pkg/fix.go": "package pkg\nfunc ExistingFix(){}\nfunc use(){ ExistingFix() }\n",
		})}, Input{Proposal: "Implement `ExistingFix()`.", RelevantFiles: []string{"pkg/fix.go"}})
		if result.State != StateAlreadyPresent {
			t.Fatalf("result = %+v", result)
		}
	})
	t.Run("inconclusive", func(t *testing.T) {
		result := verify(t, fakeReader{archive: archive(map[string]string{
			"pkg/fix.go": "package pkg\nfunc ExistingFix(){}\n",
		})}, Input{Proposal: "Implement `ExistingFix`.", RelevantFiles: []string{"pkg/fix.go"}})
		if result.State != StateInconclusive {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestVerifyCAPZAlreadyContainsLabelMigration(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"internal/asomigration/labels.go": "package asomigration\nfunc LabelCRDsForClusterctlUpgrade() error { return nil }\n",
		"test/e2e/capi_test.go":           "//go:build e2e\n\npackage e2e\nimport \"sigs.k8s.io/cluster-api-provider-azure/internal/asomigration\"\nfunc test(){ _ = asomigration.LabelCRDsForClusterctlUpgrade() }\n",
	})}, Input{
		Proposal:      "Implement `LabelCRDsForClusterctlUpgrade`.",
		RelevantFiles: []string{"internal/asomigration/labels.go", "test/e2e/capi_test.go"},
	})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredCAPZRemediationUsesRealPatternWording(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod":                          "module sigs.k8s.io/cluster-api-provider-azure\n",
		"internal/asomigration/labels.go": "package asomigration\nfunc LabelCRDsForClusterctlUpgrade() error { return nil }\n",
		"test/e2e/capi_test.go":           "//go:build e2e\n\npackage e2e\nimport \"sigs.k8s.io/cluster-api-provider-azure/internal/asomigration\"\nfunc test(){ _ = asomigration.LabelCRDsForClusterctlUpgrade() }\n",
	})}, Input{
		Proposal: "Add a PreUpgrade hook in the verified source location that labels all ASO-managed CRDs with cluster.x-k8s.io/provider: infrastructure-azure before clusterctl upgrade begins. Reuse or implement the labeling logic in the verified source location.",
		Targets: []models.RemediationTarget{{
			Intent: models.RemediationIntentAddSymbol,
			Symbol: "LabelCRDsForClusterctlUpgrade",
			Path:   "internal/asomigration/labels.go",
		}},
	})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredAddSymbolReferencesPackageDeclarations(t *testing.T) {
	for name, test := range map[string]struct {
		files  map[string]string
		target models.RemediationTarget
	}{
		"constant": {
			files: map[string]string{
				"pkg/symbol.go": "package pkg\nconst ExistingValue = 1\n",
				"pkg/use.go":    "package pkg\nvar observed = ExistingValue\n",
			},
			target: models.RemediationTarget{Intent: models.RemediationIntentAddSymbol, Symbol: "ExistingValue", Path: "pkg/symbol.go"},
		},
		"constant map key": {
			files: map[string]string{
				"pkg/symbol.go": "package pkg\nconst ExistingKey = \"x\"\n",
				"pkg/use.go":    "package pkg\nvar observed = map[string]int{ExistingKey: 1}\n",
			},
			target: models.RemediationTarget{Intent: models.RemediationIntentAddSymbol, Symbol: "ExistingKey", Path: "pkg/symbol.go"},
		},
		"type from another package": {
			files: map[string]string{
				"go.mod":      "module example/repo\n",
				"pkg/type.go": "package pkg\ntype ExistingType struct{}\n",
				"cmd/use.go":  "package cmd\nimport p \"example/repo/pkg\"\nvar observed p.ExistingType\n",
			},
			target: models.RemediationTarget{Intent: models.RemediationIntentAddSymbol, Symbol: "ExistingType", Path: "pkg/type.go"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := verify(t, fakeReader{archive: archive(test.files)}, Input{Targets: []models.RemediationTarget{test.target}})
			if result.State != StateAlreadyPresent {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestVerifyStructuredRecursiveAddSymbolIsInconclusive(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/symbol.go": "package pkg\nfunc ExistingFix() { ExistingFix() }\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentAddSymbol,
		Symbol: "ExistingFix",
		Path:   "pkg/symbol.go",
	}}})
	if result.State != StateInconclusive {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredAddMethodRequiresReceiverMetadata(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/method.go": "package pkg\ntype target struct{}\nfunc (target) ExistingMethod() {}\nfunc use(value target) { value.ExistingMethod() }\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentAddSymbol,
		Symbol: "ExistingMethod",
		Path:   "pkg/method.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "package-level") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyExistingSymbolIsActionable(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"controllers/helpers.go": "package controllers\nfunc MachinePoolModelHasChanged() bool { return false }\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol,
		Symbol: "MachinePoolModelHasChanged",
		Path:   "controllers/helpers.go",
	}}})
	if result.State != StateUnresolved {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyChecksRequiredCall(t *testing.T) {
	const source = `package e2e
import "example/asomigration"
func getPreUpgradeFunc() func() {
	return func() { asomigration.LabelCRDsForClusterctlUpgrade() }
}
`
	files := map[string]string{
		"go.mod":                    "module example\n",
		"test/e2e/capi_test.go":     source,
		"asomigration/migration.go": "package asomigration\nfunc LabelCRDsForClusterctlUpgrade() {}\nfunc DeleteWebhookConfigurations() {}\n",
	}
	for name, test := range map[string]struct {
		requiredCall string
		want         string
	}{
		"already present": {requiredCall: "example/asomigration.LabelCRDsForClusterctlUpgrade", want: StateAlreadyPresent},
		"still missing":   {requiredCall: "example/asomigration.DeleteWebhookConfigurations", want: StateUnresolved},
	} {
		t.Run(name, func(t *testing.T) {
			result := verify(t, fakeReader{archive: archive(files)}, Input{Targets: []models.RemediationTarget{{
				Intent: models.RemediationIntentModifySymbol, Symbol: "getPreUpgradeFunc", RequiredCall: test.requiredCall, Path: "test/e2e/capi_test.go",
			}}})
			if result.State != test.want {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestVerifyStructuredModifyDoesNotMatchUnrelatedReceiver(t *testing.T) {
	const source = `package controllers
import migration "example/migration"
type worker struct{}
func (worker) ApplyFix() {}
func reconcile(value worker) { value.ApplyFix() }
var _ = migration.ApplyFix
`
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod": "module example\n", "controllers/reconcile.go": source,
		"migration/fix.go": "package migration\nfunc ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example/migration.ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateUnresolved {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyDoesNotMatchShadowedDirectCall(t *testing.T) {
	const source = `package controllers
func ApplyFix() {}
func reconcile(ApplyFix func()) { ApplyFix() }
`
	result := verify(t, fakeReader{archive: archive(map[string]string{"controllers/reconcile.go": source})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateUnresolved {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyResolvesDefaultImportPackageName(t *testing.T) {
	const source = `package controllers
import "example.com/migration/v2"
func reconcile() { migration.ApplyFix() }
`
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod":                   "module example.com/migration\n",
		"controllers/reconcile.go": source,
		"v2/fix.go":                "package migration\nfunc ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/migration/v2.ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyDotImportIsInconclusive(t *testing.T) {
	const source = `package controllers
import . "example.com/migration"
func reconcile() { ApplyFix() }
`
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod": "module example.com\n", "controllers/reconcile.go": source,
		"migration/fix.go": "package migration\nfunc ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/migration.ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "identity cannot be proven") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyDifferentImportIsUnresolved(t *testing.T) {
	const source = `package controllers
import (
	"example.com/migration"
	other "example.com/other"
)
func reconcile() { other.ApplyFix() }
var _ = migration.Other
`
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod": "module example.com\n", "controllers/reconcile.go": source,
		"migration/fix.go": "package migration\nfunc ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/migration.ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateUnresolved {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyMixedPackageIsInconclusive(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
		"controllers/helpers.go":   "package controllers\nfunc waitForReady() {}\n",
		"controllers/other.go":     "package other\nfunc unrelated() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "same-package") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyExternalTestPackageCompanion(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"controllers/controllers.go":    "package controllers\nfunc production() {}\n",
		"controllers/reconcile_test.go": "package controllers_test\nfunc reconcile() { waitForReady() }\n",
		"controllers/helpers_test.go":   "package controllers_test\nfunc waitForReady() {}\n",
		"controllers/internal_test.go":  "package controllers\nfunc internalHelper() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "controllers/reconcile_test.go",
	}}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyTestOnlyInternalPackageCompanion(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/reconcile_test.go": "package pkg\nfunc reconcile() { waitForReady() }\nfunc waitForReady() {}\n",
		"pkg/external_test.go":  "package pkg_test\nfunc external() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "pkg/reconcile_test.go",
	}}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyTestOnlyExternalPackageCompanion(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/internal_test.go":  "package pkg\nfunc internal() {}\n",
		"pkg/reconcile_test.go": "package pkg_test\nfunc reconcile() { waitForReady() }\nfunc waitForReady() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "pkg/reconcile_test.go",
	}}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyTestOnlyThirdPackageIsInconclusive(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/reconcile_test.go": "package pkg\nfunc reconcile() { waitForReady() }\nfunc waitForReady() {}\n",
		"pkg/external_test.go":  "package pkg_test\nfunc external() {}\n",
		"pkg/other_test.go":     "package other\nfunc other() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "pkg/reconcile_test.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "same-package") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyTestOnlyBasePackageEndingTest(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/reconcile_test.go": "package pkg_test\nfunc reconcile() { waitForReady() }\nfunc waitForReady() {}\n",
		"pkg/external_test.go":  "package pkg_test_test\nfunc external() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "pkg/reconcile_test.go",
	}}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyTestOnlyExternalForBaseEndingTest(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/internal_test.go":  "package pkg_test\nfunc internal() {}\n",
		"pkg/reconcile_test.go": "package pkg_test_test\nfunc reconcile() { waitForReady() }\nfunc waitForReady() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "pkg/reconcile_test.go",
	}}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyTestOnlyAmbiguousAdjacentPackages(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/reconcile_test.go": "package pkg_test\nfunc reconcile() { waitForReady() }\nfunc waitForReady() {}\n",
		"pkg/base_test.go":      "package pkg\nfunc base() {}\n",
		"pkg/external_test.go":  "package pkg_test_test\nfunc external() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "pkg/reconcile_test.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "same-package") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyProductionPackageEndingTest(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"controllers/production.go":     "package controllers_test\nfunc production() {}\n",
		"controllers/reconcile_test.go": "package controllers_test\nfunc reconcile() { waitForReady() }\n",
		"controllers/helpers_test.go":   "package controllers_test\nfunc waitForReady() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "controllers/reconcile_test.go",
	}}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyProductionPackageEndingTestRejectsUnexpectedBase(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"controllers/production.go":     "package controllers_test\nfunc production() {}\n",
		"controllers/unexpected.go":     "package controllers\nfunc unexpected() {}\n",
		"controllers/reconcile_test.go": "package controllers_test\nfunc reconcile() { waitForReady() }\n",
		"controllers/helpers_test.go":   "package controllers_test\nfunc waitForReady() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "waitForReady", Path: "controllers/reconcile_test.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "same-package") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyMissingSamePackageCalleeIsInconclusive(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "same-package") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyMissingImportedCalleeIsInconclusive(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod":                   "module example.com/project\n",
		"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
		"migration/doc.go":         "package migration\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/migration.ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "imported call") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyExternalDependencyIsInconclusive(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod":                   "module example.com/project\nrequire external.example/migration v1.0.0\n",
		"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "external.example/migration.ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "pinned repository") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyImportedAliasResolves(t *testing.T) {
	const source = `package controllers
import mig "example.com/project/migration"
func reconcile() { mig.ApplyFix() }
`
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod": "module example.com/project\n", "controllers/reconcile.go": source,
		"migration/fix.go": "package migration\nfunc ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/migration.ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyRequiredCallRejectsReceiverMethod(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod":                   "module example.com/project\n",
		"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
		"migration/fix.go":         "package migration\ntype worker struct{}\nfunc (worker) ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/migration.ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateInconclusive {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyRequiredCallRejectsAmbiguousDeclaration(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
		"controllers/first.go":     "package controllers\nfunc ApplyFix() {}\n",
		"controllers/second.go":    "package controllers\nfunc ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateInconclusive {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyAmbiguousImportIsInconclusive(t *testing.T) {
	const source = `package controllers
import (
  migration "example.com/project/migration"
  migration "example.com/other"
)
func reconcile() { migration.ApplyFix() }
`
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod": "module example.com/project\n", "controllers/reconcile.go": source,
		"migration/fix.go": "package migration\nfunc ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "example.com/project/migration.ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "identity cannot be proven") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyResolvesSamePackageCallAcrossFiles(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"controllers/reconcile.go": "package controllers\nfunc reconcile() { ApplyFix() }\n",
		"controllers/fix.go":       "package controllers\nfunc ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "ApplyFix", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyRequiredCallNeedsFunction(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/value.go": "package pkg\nvar Existing = true\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "Existing", RequiredCall: "Apply", Path: "pkg/value.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "not a function or method") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyRequiredCallRejectsAmbiguousMethods(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/method.go": "package pkg\ntype first struct{}\ntype second struct{}\nfunc (first) reconcile() { ApplyFix() }\nfunc (second) reconcile() {}\nfunc ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "ApplyFix", Path: "pkg/method.go",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "multiple methods") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredConfigurationStates(t *testing.T) {
	const template = "templates/test/e2e/data/shared/v1beta1/cluster-template-dra.yaml"
	for name, test := range map[string]struct {
		content string
		value   string
		want    string
	}{
		"missing gate": {
			content: "featureGates:\n  - ExistingGate=true\n",
			value:   "DRAWorkloadResourceClaims=true",
			want:    StateUnresolved,
		},
		"applied gate": {
			content: "featureGates:\n  - DRAWorkloadResourceClaims=true\n  - GenericWorkload=true\n",
			value:   "GenericWorkload=true",
			want:    StateAlreadyPresent,
		},
		"YAML mapping applied": {
			content: "featureGates:\n  GenericWorkload: true\n",
			value:   "GenericWorkload=true",
			want:    StateAlreadyPresent,
		},
		"later YAML document applied": {
			content: "kind: First\n---\nfeatureGates:\n  GenericWorkload: true\n",
			value:   "GenericWorkload=true",
			want:    StateAlreadyPresent,
		},
		"inline comment is not applied": {
			content: "featureGates: [] # GenericWorkload=true was removed\n",
			value:   "GenericWorkload=true",
			want:    StateUnresolved,
		},
		"quoted comment marker remains data": {
			content: "featureGates:\n  - \"GenericWorkload=true#strict\"\n",
			value:   "GenericWorkload=true",
			want:    StateUnresolved,
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := verify(t, fakeReader{archive: archive(map[string]string{template: test.content})}, Input{
				Targets: []models.RemediationTarget{{Intent: models.RemediationIntentSetConfiguration, Path: template, Value: test.value}},
			})
			if result.State != test.want {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestVerifyStructuredJSONConfigurationMapping(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"config/features.json": `{"GenericWorkload":true}`,
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentSetConfiguration,
		Path:   "config/features.json",
		Value:  "GenericWorkload=true",
	}}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredMalformedConfigurationIsInconclusive(t *testing.T) {
	for name, test := range map[string]struct {
		filePath string
		content  string
	}{
		"malformed JSON": {filePath: "config/features.json", content: `{"other":true`},
		"trailing JSON":  {filePath: "config/features.json", content: `{"other":true} trailing`},
		"malformed YAML": {filePath: "config/features.yaml", content: "featureGates: [\n"},
	} {
		t.Run(name, func(t *testing.T) {
			result := verify(t, fakeReader{archive: archive(map[string]string{test.filePath: test.content})}, Input{Targets: []models.RemediationTarget{{
				Intent: models.RemediationIntentSetConfiguration,
				Path:   test.filePath,
				Value:  "GenericWorkload=true",
			}}})
			if result.State != StateInconclusive {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestVerifyStructuredINICommentsAndUnknownFormats(t *testing.T) {
	for name, test := range map[string]struct {
		filePath string
		content  string
		want     string
	}{
		"commented INI value": {
			filePath: "config/features.ini",
			content:  "# GenericWorkload=true\n",
			want:     StateUnresolved,
		},
		"unknown format": {
			filePath: "config/features.data",
			content:  "# GenericWorkload=true\n",
			want:     StateInconclusive,
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := verify(t, fakeReader{archive: archive(map[string]string{test.filePath: test.content})}, Input{Targets: []models.RemediationTarget{{
				Intent: models.RemediationIntentSetConfiguration,
				Path:   test.filePath,
				Value:  "GenericWorkload=true",
			}}})
			if result.State != test.want {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestVerifyStructuredInvestigationRemainsInconclusive(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{})}, Input{Targets: []models.RemediationTarget{{Intent: models.RemediationIntentInvestigate}}})
	if result.State != StateInconclusive {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredConfigurationRequiresAssignment(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"templates/dra.yaml": "NotGenericWorkload: true\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentSetConfiguration,
		Path:   "templates/dra.yaml",
		Value:  "GenericWorkload",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "metadata") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredSymbolRequiresGoPath(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"config/features.yaml": "Fix: true\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol,
		Path:   "config/features.yaml",
		Symbol: "Fix",
	}}})
	if result.State != StateInconclusive || !strings.Contains(result.Reason, "metadata") {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyRequiresExplicitSymbol(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{"main.go": "package main\n"})}, Input{
		Proposal: "Implement MissingHelper.", RelevantFiles: []string{"main.go"},
	})
	if result.State != StateInconclusive {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyMissingGroundedPathIsInconclusive(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{"main.go": "package main\n"})}, Input{
		Proposal: "Implement `MissingHelper`.", RelevantFiles: []string{"missing.go"},
	})
	if result.State != StateInconclusive {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyUnrelatedTestCallDoesNotCount(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/fix.go":      "package pkg\nfunc ExistingFix(){}\n",
		"pkg/fix_test.go": "package pkg\nfunc TestFix(){ ExistingFix() }\n",
	})}, Input{Proposal: "Add a call to `ExistingFix`.", RelevantFiles: []string{"pkg/fix.go"}})
	if result.State != StateInconclusive {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyGroundedTestCallMayCount(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/fix.go":      "package pkg\nfunc ExistingFix(){}\n",
		"pkg/fix_test.go": "package pkg\nfunc TestFix(){ ExistingFix() }\n",
	})}, Input{
		Proposal: "Add a call to `ExistingFix`.", RelevantFiles: []string{"pkg/fix.go", "pkg/fix_test.go"},
	})
	if result.State != StateAlreadyPresent {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyBuildConstraintAndCallbackAreInconclusive(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"constraint": {
			"pkg/main.go":        "package pkg\n",
			"pkg/fix_windows.go": "package pkg\nfunc ExistingFix(){}\nfunc use(){ ExistingFix() }\n",
		},
		"callback": {
			"pkg/main.go": "package pkg\nfunc ExistingFix(){}\nfunc register(func()){}\nfunc init(){ register(ExistingFix) }\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := verify(t, fakeReader{archive: archive(files)}, Input{
				Proposal: "Implement `ExistingFix`.", RelevantFiles: []string{"pkg/main.go"},
			})
			if result.State != StateInconclusive {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestVerifyArchiveErrorIsReturned(t *testing.T) {
	_, err := Verify(context.Background(), fakeReader{err: errors.New("archive failed")}, Input{
		Proposal: "Implement `ExistingFix`.", RelevantFiles: []string{"main.go"},
	})
	if err == nil {
		t.Fatal("expected archive error")
	}
}

func verify(t *testing.T, reader Reader, input Input) Result {
	t.Helper()
	result, err := Verify(context.Background(), reader, input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestVerifyRecursiveDefinitionIsInconclusive(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/fix.go": "package pkg\nfunc ExistingFix(){ ExistingFix() }\n",
	})}, Input{Proposal: "Add a call to `ExistingFix`.", RelevantFiles: []string{"pkg/fix.go"}})
	if result.State != StateInconclusive {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyIgnoresBacktickedArtifactNames(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"pkg/main.go": "package pkg\n",
	})}, Input{
		Proposal:      "Implement `MissingHelper`. Evidence: `junit.xml` and `sigs.k8s.io/module/x.go`.",
		RelevantFiles: []string{"pkg/main.go"},
	})
	if result.State != StateUnresolved {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyProwJobEnvironment(t *testing.T) {
	const file = "config/jobs/kubernetes-sigs/cluster-api-provider-azure/periodics.yaml"
	config := `periodics:
- name: periodic-capz
  spec:
    containers:
    - env:
      - name: AKS_MGMT_KUBERNETES_VERSION
        value: v1.33.2
`
	base := models.RemediationTarget{
		Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra",
		Revision: strings.Repeat("a", 40), Path: file, Job: "periodic-capz", Container: "test",
		Name: "AKS_MGMT_KUBERNETES_VERSION",
	}
	for name, test := range map[string]struct {
		value string
		want  string
	}{
		"different value is unresolved": {value: "v1.34.1", want: StateUnresolved},
		"same value is present":         {value: "v1.33.2", want: StateAlreadyPresent},
	} {
		t.Run(name, func(t *testing.T) {
			target := base
			target.Value = test.value
			result := verify(t, fakeReader{archive: archive(map[string]string{file: config})}, Input{Targets: []models.RemediationTarget{target}})
			if result.State != test.want {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestVerifyProwJobEnvironmentFailsClosed(t *testing.T) {
	const file = "config/jobs/example/periodics.yaml"
	base := models.RemediationTarget{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: strings.Repeat("a", 40), Path: file, Job: "periodic-capz", Container: "test", Name: "VERSION", Value: "v2"}
	for name, config := range map[string]string{
		"duplicate job": `periodics:
- name: periodic-capz
  spec: {containers: [{name: test}]}
- name: periodic-capz
  spec: {containers: [{name: test}]}
`,
		"duplicate env": `periodics:
- name: periodic-capz
  spec:
    containers:
    - name: test
      env: [{name: VERSION, value: v1}, {name: VERSION, value: v1}]
`,
		"multiple unnamed containers": `periodics:
- name: periodic-capz
  spec:
    containers:
    - env: [{name: VERSION, value: v1}]
    - env: [{name: VERSION, value: v1}]
`,
		"value from": `periodics:
- name: periodic-capz
  spec:
    containers:
    - name: test
      env: [{name: VERSION, valueFrom: {secretKeyRef: {name: version, key: value}}}]
`,
	} {
		t.Run(name, func(t *testing.T) {
			result := verify(t, fakeReader{archive: archive(map[string]string{file: config})}, Input{Targets: []models.RemediationTarget{base}})
			if result.State != StateInconclusive {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

type fileOnlyReader struct{ fakeReader }

func (fileOnlyReader) ReadSourceArchive(context.Context) (Archive, error) {
	return Archive{}, errors.New("archive should not be read")
}

func TestVerifyProwJobEnvironmentDoesNotDownloadRepositoryArchive(t *testing.T) {
	const file = "config/jobs/example/periodics.yaml"
	reader := fileOnlyReader{fakeReader{archive: archive(map[string]string{file: `periodics:
- name: periodic-capz
  spec:
    containers:
    - name: test
      env: [{name: VERSION, value: v1}]
`})}}
	result := verify(t, reader, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: strings.Repeat("a", 40),
		Path: file, Job: "periodic-capz", Container: "test", Name: "VERSION", Value: "v2",
	}}})
	if result.State != StateUnresolved {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyProwJobEnvironmentPreservesExactScalarWhitespace(t *testing.T) {
	const file = "config/jobs/example/periodics.yaml"
	config := `periodics:
- name: periodic-capz
  spec:
    containers:
    - name: test
      env:
      - name: VERSION
        value: " v2 "
`
	target := models.RemediationTarget{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: strings.Repeat("a", 40), Path: file, Job: "periodic-capz", Container: "test", Name: "VERSION", Value: "v2"}
	result := verify(t, fakeReader{archive: archive(map[string]string{file: config})}, Input{Targets: []models.RemediationTarget{target}})
	if result.State != StateUnresolved {
		t.Fatalf("trimmed value was treated as exact: %+v", result)
	}
	target.Value = " v2 "
	result = verify(t, fakeReader{archive: archive(map[string]string{file: config})}, Input{Targets: []models.RemediationTarget{target}})
	if result.State != StateAlreadyPresent {
		t.Fatalf("exact spaced value was not recognized: %+v", result)
	}
}

func TestVerifyProwJobEnvironmentRejectsDuplicateYAMLKeys(t *testing.T) {
	const file = "config/jobs/example/periodics.yaml"
	target := models.RemediationTarget{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: strings.Repeat("a", 40), Path: file, Job: "periodic-capz", Container: "test", Name: "VERSION", Value: "v2"}
	for name, config := range map[string]string{
		"top-level section": `periodics: []
periodics:
- name: periodic-capz
  spec: {containers: [{name: test}]}
`,
		"nested env value": `periodics:
- name: periodic-capz
  spec:
    containers:
    - name: test
      env:
      - name: VERSION
        value: v1
        value: v2
`,
	} {
		t.Run(name, func(t *testing.T) {
			result := verify(t, fakeReader{archive: archive(map[string]string{file: config})}, Input{Targets: []models.RemediationTarget{target}})
			if result.State != StateInconclusive || !strings.Contains(result.Reason, "duplicate key") {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestVerifyProwJobEnvironmentRejectsNonStringValue(t *testing.T) {
	const file = "config/jobs/example/periodics.yaml"
	config := `periodics:
- name: periodic-capz
  spec:
    containers:
    - name: test
      env: [{name: VERSION, value: 123}]
`
	target := models.RemediationTarget{Intent: models.RemediationIntentSetJobEnvironment, Repository: "kubernetes/test-infra", Revision: strings.Repeat("a", 40), Path: file, Job: "periodic-capz", Container: "test", Name: "VERSION", Value: "123"}
	result := verify(t, fakeReader{archive: archive(map[string]string{file: config})}, Input{Targets: []models.RemediationTarget{target}})
	if result.State != StateInconclusive {
		t.Fatalf("result = %+v", result)
	}
}

func TestInvalidTargetReasonRejectsRequiredCallOutsideModify(t *testing.T) {
	for name, target := range map[string]models.RemediationTarget{
		"add symbol":    {Intent: models.RemediationIntentAddSymbol, Symbol: "Fix", RequiredCall: "Apply", Path: "fix.go"},
		"configuration": {Intent: models.RemediationIntentSetConfiguration, RequiredCall: "Apply", Path: "config.yaml", Value: "Enabled=true"},
		"Prow job": {
			Intent: models.RemediationIntentSetJobEnvironment, RequiredCall: "Apply", Repository: "kubernetes/test-infra",
			Revision: strings.Repeat("a", 40), Path: "config/jobs/example/jobs.yaml", Job: "periodic-example", Container: "test", Name: "VERSION", Value: "v2",
		},
		"investigate": {Intent: models.RemediationIntentInvestigate, RequiredCall: "Apply"},
	} {
		t.Run(name, func(t *testing.T) {
			if reason := InvalidTargetReason(target); reason == "" {
				t.Fatalf("target was accepted: %+v", target)
			}
		})
	}
}

func TestVerifyRequiredCallFailureDoesNotExposeSourceContent(t *testing.T) {
	const sentinel = "PRIVATE_SOURCE_SENTINEL"
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"controllers/reconcile.go": "package controllers\n// " + sentinel + "\nfunc reconcile() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile", RequiredCall: "fabricatedHelper", Path: "controllers/reconcile.go",
	}}})
	if result.State != StateInconclusive || strings.Contains(result.Reason, sentinel) {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyResolvesNearestNestedModule(t *testing.T) {
	result := verify(t, fakeReader{archive: archive(map[string]string{
		"go.mod":                         "module example.com/root\n",
		"tools/go.mod":                   "module example.com/tools\n",
		"tools/controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
		"tools/migration/fix.go":         "package migration\nfunc ApplyFix() {}\n",
	})}, Input{Targets: []models.RemediationTarget{{
		Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile",
		RequiredCall: "example.com/tools/migration.ApplyFix", Path: "tools/controllers/reconcile.go",
	}}})
	if result.State != StateUnresolved {
		t.Fatalf("result = %+v", result)
	}
}

func TestVerifyStructuredModifyRejectsCrossModulePackage(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		childModule string
	}{
		{name: "different child module", childModule: "other.example/sub"},
		{name: "matching child module without dependency mapping", childModule: "example.com/root/sub"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := verify(t, fakeReader{archive: archive(map[string]string{
				"go.mod":                   "module example.com/root\n",
				"controllers/reconcile.go": "package controllers\nfunc reconcile() {}\n",
				"sub/go.mod":               "module " + testCase.childModule + "\n",
				"sub/pkg/fix.go":           "package pkg\nfunc ApplyFix() {}\n",
			})}, Input{Targets: []models.RemediationTarget{{
				Intent: models.RemediationIntentModifySymbol, Symbol: "reconcile",
				RequiredCall: "example.com/root/sub/pkg.ApplyFix", Path: "controllers/reconcile.go",
			}}})
			if result.State != StateInconclusive {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestVerifyEnforcesAdmissionConversionPolicyBeforeSourceRead(t *testing.T) {
	target := models.RemediationTarget{
		Intent: models.RemediationIntentModifySymbol, Symbol: "getPreUpgradeFunc",
		RequiredCall: "example/asomigration.DeleteWebhookConfigurations", Path: "test/e2e/capi_test.go",
	}
	result, err := Verify(t.Context(), fakeReader{err: errors.New("source should not be read")}, Input{
		Proposal: "Delete the ASO mutating and validating webhook configurations so CRD conversion no longer calls ASO.",
		Targets:  []models.RemediationTarget{target},
	})
	if err != nil || result.State != StateInconclusive || result.Reason != remediationpolicy.UnsafeConversionReason {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
