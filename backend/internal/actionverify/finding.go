package actionverify

import (
	"fmt"
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
	findingWarningExternal      = "Some backticked technical identifiers are explanatory and are not declared in the verified files."
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
	missing := false
	ambiguous := false
	for _, file := range files {
		content, ok := sourceFiles[file]
		if !ok {
			return FindingResult{}, fmt.Errorf("verified source path %s was not loaded", file)
		}
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		for _, symbol := range symbols {
			declared, err := declaresSymbol(file, content, symbol)
			if err != nil {
				return FindingResult{}, fmt.Errorf("verified source %s could not be parsed", file)
			}
			if declared {
				result.Symbols = append(result.Symbols, FindingSymbol{Name: symbol, Path: file})
			}
		}
	}
	counts := make(map[string]int, len(symbols))
	for _, grounded := range result.Symbols {
		counts[grounded.Name]++
	}
	result.Symbols = result.Symbols[:0]
	for _, symbol := range symbols {
		switch counts[symbol] {
		case 0:
			missing = true
		case 1:
			for _, file := range files {
				content, ok := sourceFiles[file]
				if !ok {
					continue
				}
				declared, err := declaresSymbol(file, content, symbol)
				if err != nil {
					return FindingResult{}, fmt.Errorf("verified source %s could not be parsed", file)
				}
				if declared {
					result.Symbols = append(result.Symbols, FindingSymbol{Name: symbol, Path: file})
					break
				}
			}
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
	if missing {
		result.Warnings = append(result.Warnings, findingWarningExternal)
	}
	sort.Slice(result.Symbols, func(i, j int) bool {
		if result.Symbols[i].Name != result.Symbols[j].Name {
			return result.Symbols[i].Name < result.Symbols[j].Name
		}
		return result.Symbols[i].Path < result.Symbols[j].Path
	})
	return result, nil
}
