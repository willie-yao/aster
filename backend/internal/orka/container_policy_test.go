package orka

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestContainerAdapterDoesNotOwnAnalysisPolicy(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	orkaDir := filepath.Dir(file)
	root := filepath.Clean(filepath.Join(orkaDir, "..", ".."))
	files := []string{
		filepath.Join(orkaDir, "container_analyzer.go"),
		filepath.Join(orkaDir, "container_task.go"),
		filepath.Join(orkaDir, "container_lifecycle.go"),
		filepath.Join(root, "cmd", "analyzer", "main.go"),
	}
	forbiddenImports := []string{
		"/internal/ai/evidenceplan", "/internal/ai/modules", "/internal/ai/skills", "/internal/ai/tools",
	}
	forbiddenIdentifiers := map[string]bool{
		"BasePrompt": true, "ResponseFormatFooter": true, "AgenticOptions": true,
		"Critique": true, "belowCurrentAgenticFloor": true,
	}
	for _, path := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			value := strings.Trim(imp.Path.Value, `"`)
			for _, forbidden := range forbiddenImports {
				if strings.Contains(value, forbidden) {
					t.Errorf("%s imports analysis policy package %s", path, value)
				}
			}
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && forbiddenIdentifiers[ident.Name] {
				t.Errorf("%s owns analysis policy identifier %s", path, ident.Name)
			}
			return true
		})
	}
}
