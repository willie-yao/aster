package actionverify

import (
	"slices"
	"testing"
)

func TestVerifyFindingSourceGroundsOnlyLocalSymbols(t *testing.T) {
	files := map[string]string{
		"test/e2e/cni.go":        "package e2e\nfunc InstallCNIManifest() {}\n",
		"test/e2e/azure_test.go": "package e2e\nfunc EnsureCloudProviderAzure() {}\n",
	}
	finding := "`InstallCNIManifest` calls `CreateOrUpdate`, which returns a `StatusError` after `Get`, `Update`, and `resourceVersion` handling."
	result, err := VerifyFindingSource(finding, []string{"test/e2e/azure_test.go", "test/e2e/cni.go"}, files)
	if err != nil {
		t.Fatal(err)
	}
	want := []FindingSymbol{{Name: "InstallCNIManifest", Path: "test/e2e/cni.go"}}
	if !slices.Equal(result.Symbols, want) {
		t.Fatalf("symbols = %+v, want %+v", result.Symbols, want)
	}
	if !slices.Contains(result.Warnings, findingWarningExternal) {
		t.Fatalf("warnings = %v", result.Warnings)
	}
}

func TestVerifyFindingSourceWarningOnlyGroundingOutcomes(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		proposal string
		files    map[string]string
		warnings []string
	}{
		{
			name: "no grounded symbol", proposal: "Change the terminal branch near `ExternalHelper`.",
			files:    map[string]string{"pkg/controller.go": "package pkg\nfunc reconcileDelete() {}\n"},
			warnings: []string{findingWarningNoSymbol, findingWarningExternal},
		},
		{
			name: "ambiguous symbol", proposal: "Update `reconcileDelete`.",
			files: map[string]string{
				"pkg/a.go": "package pkg\nfunc reconcileDelete() {}\n",
				"pkg/b.go": "package pkg\nfunc reconcileDelete() {}\n",
			}, warnings: []string{findingWarningNoSymbol, findingWarningAmbiguous},
		},
		{
			name: "text policy", proposal: "Delete the ASO mutating and validating webhook configurations so CRD conversion no longer calls ASO.",
			files:    map[string]string{"pkg/controller.go": "package pkg\nfunc reconcileDelete() {}\n"},
			warnings: []string{findingWarningPolicyConcern, findingWarningNoSymbol},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := VerifyFindingSource(testCase.proposal, mapKeys(testCase.files), testCase.files)
			if err != nil {
				t.Fatal(err)
			}
			for _, warning := range testCase.warnings {
				if !slices.Contains(result.Warnings, warning) {
					t.Fatalf("warnings = %v, missing %q", result.Warnings, warning)
				}
			}
		})
	}
}

func TestVerifyFindingSourceMissingVerifiedFileFails(t *testing.T) {
	if _, err := VerifyFindingSource("Update `reconcileDelete`.", []string{"pkg/controller.go"}, map[string]string{}); err == nil {
		t.Fatal("missing verified source file was accepted")
	}
}

func mapKeys(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	return out
}
