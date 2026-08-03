package actionverify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
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
