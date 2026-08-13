package actionverify

import (
	"context"
	"testing"
)

type findingReader struct{ archive Archive }

func (r findingReader) ReadSourceArchive(context.Context) (Archive, error) { return r.archive, nil }

func TestVerifyFindingSourceRequiresUniqueDeclaredSymbol(t *testing.T) {
	reader := findingReader{archive: Archive{
		Paths:   map[string]bool{"pkg/controller.go": true},
		GoFiles: map[string]string{"pkg/controller.go": "package pkg\nfunc reconcileDelete() {}\n"}, Files: map[string]string{},
	}}
	if _, err := VerifyFindingSource(t.Context(), reader, "change the branch", []string{"pkg/controller.go"}); err == nil {
		t.Fatal("finding without symbol was accepted")
	}
	result, err := VerifyFindingSource(t.Context(), reader, "Update `reconcileDelete`.", []string{"pkg/controller.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Symbols) != 1 || result.Symbols[0].Name != "reconcileDelete" || result.Symbols[0].Path != "pkg/controller.go" {
		t.Fatalf("result = %+v", result)
	}
	if _, err := VerifyFindingSource(t.Context(), reader, "Add `InventedFix`.", []string{"pkg/controller.go"}); err == nil {
		t.Fatal("absent symbol was accepted")
	}
}

func TestVerifyFindingSourceRejectsAmbiguousDeclaration(t *testing.T) {
	reader := findingReader{archive: Archive{
		Paths: map[string]bool{"pkg/a.go": true, "pkg/b.go": true},
		GoFiles: map[string]string{
			"pkg/a.go": "package pkg\nfunc reconcileDelete() {}\n",
			"pkg/b.go": "package pkg\nfunc reconcileDelete() {}\n",
		}, Files: map[string]string{},
	}}
	if _, err := VerifyFindingSource(t.Context(), reader, "Update `reconcileDelete`.", []string{"pkg/a.go", "pkg/b.go"}); err == nil {
		t.Fatal("ambiguous symbol was accepted")
	}
}
