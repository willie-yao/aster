package actionverify

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/remediationpolicy"
)

// FindingSymbol identifies one uniquely grounded source declaration.
type FindingSymbol struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// FindingResult is deterministic source grounding for one selected finding.
type FindingResult struct {
	Files    []string        `json:"files"`
	Symbols  []FindingSymbol `json:"symbols"`
	Warnings []string        `json:"warnings,omitempty"`
}

const (
	findingWarningPolicyConcern = "Selected finding prose triggered a text-only remediation policy concern."
	findingWarningNoSymbol      = "No uniquely declared source symbol was found in the verified files."
	findingWarningAmbiguous     = "Multiple plausible local symbols exist in the verified files."
	findingWarningUnresolved    = "Backticked identifiers not declared in the verified files"
)

// VerifyFindingSource grounds exact-JUnit finding symbols in verified source files.
func VerifyFindingSource(proposal string, relevantFiles []string, sourceFiles map[string]string) (FindingResult, error) {
	symbols := explicitSymbols(proposal)
	files := compact(relevantFiles)
	if len(files) == 0 {
		return FindingResult{}, fmt.Errorf("finding has no verified source paths")
	}
	slices.Sort(files)
	files = slices.Compact(files)
	result := FindingResult{Files: files, Symbols: make([]FindingSymbol, 0, len(symbols))}
	if remediationpolicy.RelationshipTextWarning(proposal) != "" {
		result.Warnings = append(result.Warnings, findingWarningPolicyConcern)
	}
	counts := make(map[string]int, len(symbols))
	paths := make(map[string]string, len(symbols))
	for _, file := range files {
		content, ok := sourceFiles[file]
		if !ok {
			return FindingResult{}, fmt.Errorf("verified source path %s was not loaded", file)
		}
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		declarations, err := findingSymbolDeclarationCounts(file, content, symbols)
		if err != nil {
			return FindingResult{}, fmt.Errorf("verified source %s could not be parsed", file)
		}
		for symbol, count := range declarations {
			if count == 0 {
				continue
			}
			previous := counts[symbol]
			counts[symbol] += count
			if previous == 0 && count == 1 {
				paths[symbol] = file
			} else {
				delete(paths, symbol)
			}
		}
	}
	missing := make([]string, 0, len(symbols))
	ambiguous := false
	for _, symbol := range symbols {
		switch counts[symbol] {
		case 0:
			missing = append(missing, symbol)
		case 1:
			result.Symbols = append(result.Symbols, FindingSymbol{Name: symbol, Path: paths[symbol]})
		default:
			ambiguous = true
		}
	}
	if len(result.Symbols) == 0 {
		result.Warnings = append(result.Warnings, findingWarningNoSymbol)
	}
	if len(result.Symbols) > 1 {
		ambiguous = true
	}
	if ambiguous {
		result.Warnings = append(result.Warnings, findingWarningAmbiguous)
	}
	if len(missing) > 0 {
		result.Warnings = append(result.Warnings, findingMissingSymbolsWarning(missing))
	}
	sort.Slice(result.Symbols, func(i, j int) bool {
		if result.Symbols[i].Name != result.Symbols[j].Name {
			return result.Symbols[i].Name < result.Symbols[j].Name
		}
		return result.Symbols[i].Path < result.Symbols[j].Path
	})
	return result, nil
}

func findingSymbolDeclarationCounts(filePath, content string, symbols []string) (map[string]int, error) {
	targets := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		targets[symbol] = true
	}
	counts := make(map[string]int, len(symbols))
	file, err := parser.ParseFile(token.NewFileSet(), filePath, content, 0)
	if err != nil {
		return nil, err
	}
	for _, declaration := range file.Decls {
		switch value := declaration.(type) {
		case *ast.FuncDecl:
			if targets[value.Name.Name] {
				counts[value.Name.Name]++
			}
		case *ast.GenDecl:
			for _, spec := range value.Specs {
				switch item := spec.(type) {
				case *ast.TypeSpec:
					if targets[item.Name.Name] {
						counts[item.Name.Name]++
					}
					structure, ok := item.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, field := range structure.Fields.List {
						for _, name := range field.Names {
							if targets[name.Name] {
								counts[name.Name]++
							}
						}
					}
				case *ast.ValueSpec:
					for _, name := range item.Names {
						if targets[name.Name] {
							counts[name.Name]++
						}
					}
				}
			}
		}
	}
	return counts, nil
}

func findingMissingSymbolsWarning(symbols []string) string {
	quoted := make([]string, len(symbols))
	for index, symbol := range symbols {
		quoted[index] = "`" + symbol + "`"
	}
	return findingWarningUnresolved + ": " + strings.Join(quoted, ", ") + "."
}
