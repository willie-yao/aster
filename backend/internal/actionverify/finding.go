package actionverify

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationpolicy"
)

// FindingSymbol identifies one uniquely grounded source declaration.
type FindingSymbol struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// FindingResult is deterministic source grounding for one selected finding.
type FindingResult struct {
	Files   []string        `json:"files"`
	Symbols []FindingSymbol `json:"symbols"`
}

// VerifyFindingSource requires every explicit finding symbol in one selected source file.
func VerifyFindingSource(ctx context.Context, reader Reader, proposal string, relevantFiles []string) (FindingResult, error) {
	if reader == nil {
		return FindingResult{}, fmt.Errorf("source reader is unavailable")
	}
	if reason := remediationpolicy.Reason(proposal, nil); reason != "" {
		return FindingResult{}, fmt.Errorf("remediation policy: %s", reason)
	}
	symbols := explicitSymbols(proposal)
	if len(symbols) == 0 {
		return FindingResult{}, fmt.Errorf("finding does not name an explicit backticked remediation symbol")
	}
	files := compact(relevantFiles)
	if len(files) == 0 {
		return FindingResult{}, fmt.Errorf("finding has no verified source paths")
	}
	archive, err := reader.ReadSourceArchive(ctx)
	if err != nil {
		return FindingResult{}, fmt.Errorf("read pinned source archive: %w", err)
	}
	slices.Sort(files)
	files = slices.Compact(files)
	result := FindingResult{Files: files, Symbols: make([]FindingSymbol, 0, len(symbols))}
	for _, file := range files {
		if !archive.Paths[file] {
			return FindingResult{}, fmt.Errorf("verified source path %s was not found", file)
		}
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		content, ok := archive.GoFiles[file]
		if !ok {
			return FindingResult{}, fmt.Errorf("verified Go source %s was not loaded", file)
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
	for _, symbol := range symbols {
		matches := 0
		for _, grounded := range result.Symbols {
			if grounded.Name == symbol {
				matches++
			}
		}
		if matches == 0 {
			return FindingResult{}, fmt.Errorf("finding symbol %s is not declared in verified source paths", symbol)
		}
		if matches > 1 {
			return FindingResult{}, fmt.Errorf("finding symbol %s is ambiguous across verified source paths", symbol)
		}
	}
	sort.Slice(result.Symbols, func(i, j int) bool {
		if result.Symbols[i].Name != result.Symbols[j].Name {
			return result.Symbols[i].Name < result.Symbols[j].Name
		}
		return result.Symbols[i].Path < result.Symbols[j].Path
	})
	return result, nil
}
