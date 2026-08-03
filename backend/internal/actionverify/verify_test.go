package actionverify

import (
	"context"
	"errors"
	"testing"
)

type fakeReader struct {
	archive Archive
	err     error
}

func (f fakeReader) ReadSourceArchive(context.Context) (Archive, error) {
	return f.archive, f.err
}

func archive(files map[string]string, extraPaths ...string) Archive {
	paths := map[string]bool{}
	for path := range files {
		paths[path] = true
	}
	for _, path := range extraPaths {
		paths[path] = true
	}
	return Archive{Paths: paths, GoFiles: files}
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
