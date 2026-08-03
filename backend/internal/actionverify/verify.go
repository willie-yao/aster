// Package actionverify checks explicit remediation symbols against pinned source.
package actionverify

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	goscanner "go/scanner"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	StateUnresolved     = "unresolved"
	StateAlreadyPresent = "already_present"
	StateInconclusive   = "inconclusive"

	maxMatchingFiles = 200
	maxMatchingBytes = 8 << 20
)

// Archive is one bounded source snapshot at an immutable revision.
type Archive struct {
	Paths   map[string]bool
	GoFiles map[string]string
}

// Reader fetches one bounded archive for its pinned revision.
type Reader interface {
	ReadSourceArchive(context.Context) (Archive, error)
}

type Input struct {
	Proposal      string
	RelevantFiles []string
}

type Result struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

type symbolEvidence struct {
	occurred    bool
	uncertain   bool
	definitions map[string]bool
	directCalls map[string]bool
	selectors   []selectorCall
}

type selectorCall struct {
	symbol     string
	importPath string
}

type fileEvidence struct {
	path        string
	packageName string
	packageKey  string
	imports     map[string]string
	definitions map[string]bool
	directCalls map[string]bool
	selectors   []selectorCall
	uncertain   map[string]bool
}

var backtickPattern = regexp.MustCompile("`([^`\\n]+)`")
var bareSymbolPattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)(?:\(\))?$`)
var qualifiedCallPattern = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_]*\.)+([A-Za-z_][A-Za-z0-9_]*)\(\)$`)
var knownGOOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
	"hurd": true, "illumos": true, "ios": true, "js": true, "linux": true, "netbsd": true,
	"openbsd": true, "plan9": true, "solaris": true, "wasip1": true, "windows": true, "zos": true,
}
var knownGOARCH = map[string]bool{
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true, "mips": true,
	"mips64": true, "mips64le": true, "mipsle": true, "ppc64": true, "ppc64le": true,
	"riscv64": true, "s390x": true, "sparc64": true, "wasm": true,
}

// HasImplementationSymbols reports whether proposal has explicit backticked symbols.
func HasImplementationSymbols(proposal string) bool {
	return len(explicitSymbols(proposal)) > 0
}

func Verify(ctx context.Context, reader Reader, input Input) (Result, error) {
	if reader == nil {
		return inconclusive("source reader is unavailable"), nil
	}
	symbols := explicitSymbols(input.Proposal)
	if len(symbols) == 0 {
		return inconclusive("proposal does not name an explicit backticked remediation symbol"), nil
	}
	grounded := compact(input.RelevantFiles)
	if len(grounded) == 0 {
		return inconclusive("proposal has no verified source paths"), nil
	}
	archive, err := reader.ReadSourceArchive(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read pinned source archive: %w", err)
	}
	groundedSet := make(map[string]bool, len(grounded))
	groundedDirs := make(map[string]bool, len(grounded))
	groundedTests := make(map[string]bool)
	for _, file := range grounded {
		if !archive.Paths[file] {
			return inconclusive(fmt.Sprintf("verified source path %s was not found", file)), nil
		}
		groundedSet[file] = true
		if strings.HasSuffix(file, ".go") {
			groundedDirs[path.Dir(file)] = true
			if strings.HasSuffix(file, "_test.go") {
				groundedTests[file] = true
			}
		}
	}
	if len(groundedDirs) == 0 {
		return inconclusive("verified paths do not include Go source"), nil
	}

	symbolSet := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		symbolSet[symbol] = true
	}
	matching := make([]string, 0)
	matchingBytes := 0
	for file, content := range archive.GoFiles {
		if !groundedDirs[path.Dir(file)] {
			continue
		}
		if strings.HasSuffix(file, "_test.go") && !groundedTests[file] {
			continue
		}
		if !containsSymbolToken(content, symbolSet) {
			continue
		}
		matching = append(matching, file)
		matchingBytes += len(content)
		if len(matching) > maxMatchingFiles || matchingBytes > maxMatchingBytes {
			return inconclusive("matching source exceeds verification limits"), nil
		}
	}
	sort.Strings(matching)

	evidence := make(map[string]*symbolEvidence, len(symbols))
	for _, symbol := range symbols {
		evidence[symbol] = &symbolEvidence{
			definitions: map[string]bool{}, directCalls: map[string]bool{},
		}
	}
	parsed := make([]fileEvidence, 0, len(matching))
	for _, file := range matching {
		content := archive.GoFiles[file]
		present := symbolTokens(content, symbolSet)
		for symbol := range present {
			evidence[symbol].occurred = true
		}
		if isBuildConstrained(file, content) && !groundedTests[file] {
			for symbol := range present {
				evidence[symbol].uncertain = true
			}
			continue
		}
		item, parseErr := inspectFile(file, content, symbolSet)
		if parseErr != nil {
			return inconclusive(fmt.Sprintf("matching source %s could not be parsed", file)), nil
		}
		parsed = append(parsed, item)
		for symbol := range item.definitions {
			evidence[symbol].definitions[item.packageKey] = true
		}
		for symbol := range item.directCalls {
			evidence[symbol].directCalls[item.packageKey] = true
		}
		for symbol := range item.uncertain {
			evidence[symbol].uncertain = true
		}
		for _, call := range item.selectors {
			evidence[call.symbol].selectors = append(evidence[call.symbol].selectors, call)
		}
	}

	// A grounded selector call may prove use of a grounded package-level function.
	groundedDefs := map[string][]fileEvidence{}
	for _, item := range parsed {
		if !groundedSet[item.path] {
			continue
		}
		for symbol := range item.definitions {
			groundedDefs[symbol] = append(groundedDefs[symbol], item)
		}
	}
	for _, item := range parsed {
		if !groundedSet[item.path] {
			for _, call := range item.selectors {
				evidence[call.symbol].uncertain = true
			}
			continue
		}
		for _, call := range item.selectors {
			matches := 0
			matchedPackage := ""
			for _, def := range groundedDefs[call.symbol] {
				if def.packageName == path.Base(call.importPath) && path.Base(path.Dir(def.path)) == path.Base(call.importPath) {
					matches++
					matchedPackage = def.packageKey
				}
			}
			if matches == 1 {
				evidence[call.symbol].directCalls[matchedPackage] = true
			} else {
				evidence[call.symbol].uncertain = true
			}
		}
	}

	allPresent := true
	anyUncertain := false
	for _, symbol := range symbols {
		item := evidence[symbol]
		present := false
		for packageKey := range item.definitions {
			if item.directCalls[packageKey] {
				present = true
				break
			}
		}
		if present {
			continue
		}
		allPresent = false
		if item.occurred || item.uncertain {
			anyUncertain = true
		}
	}
	if allPresent {
		return alreadyPresent(symbols), nil
	}
	if anyUncertain {
		return inconclusive("matching source does not prove a complete definition and direct use"), nil
	}
	return Result{State: StateUnresolved, Reason: "explicit remediation symbols do not occur in applicable source"}, nil
}

type fileVisitor struct {
	item            *fileEvidence
	symbols         map[string]bool
	currentFunction string
}

func (v *fileVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil {
		return nil
	}
	switch value := node.(type) {
	case *ast.FuncDecl:
		if v.symbols[value.Name.Name] {
			if value.Recv == nil {
				v.item.definitions[value.Name.Name] = true
			} else {
				v.item.uncertain[value.Name.Name] = true
			}
		}
		child := *v
		child.currentFunction = ""
		if value.Recv == nil {
			child.currentFunction = value.Name.Name
		}
		return &child
	case *ast.CallExpr:
		fun := unwrapCall(value.Fun)
		switch target := fun.(type) {
		case *ast.Ident:
			if !v.symbols[target.Name] || target.Name == v.currentFunction {
				return v
			}
			if target.Obj == nil || target.Obj.Kind == ast.Fun {
				v.item.directCalls[target.Name] = true
			} else {
				v.item.uncertain[target.Name] = true
			}
		case *ast.SelectorExpr:
			if !v.symbols[target.Sel.Name] {
				return v
			}
			qualifier, ok := target.X.(*ast.Ident)
			if !ok || qualifier.Obj != nil || v.item.imports[qualifier.Name] == "" {
				v.item.uncertain[target.Sel.Name] = true
				return v
			}
			v.item.selectors = append(v.item.selectors, selectorCall{
				symbol: target.Sel.Name, importPath: v.item.imports[qualifier.Name],
			})
		}
	}
	return v
}

func inspectFile(filePath, content string, symbols map[string]bool) (fileEvidence, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filePath, content, 0)
	if err != nil {
		return fileEvidence{}, err
	}
	item := fileEvidence{
		path: filePath, packageName: file.Name.Name,
		packageKey: path.Dir(filePath) + "#" + file.Name.Name,
		imports:    map[string]string{}, definitions: map[string]bool{},
		directCalls: map[string]bool{}, uncertain: map[string]bool{},
	}
	for _, spec := range file.Imports {
		importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			continue
		}
		alias := path.Base(importPath)
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if alias != "" && alias != "." && alias != "_" {
			item.imports[alias] = importPath
		}
	}
	ast.Walk(&fileVisitor{item: &item, symbols: symbols}, file)
	return item, nil
}

func unwrapCall(expr ast.Expr) ast.Expr {
	for {
		switch value := expr.(type) {
		case *ast.IndexExpr:
			expr = value.X
		case *ast.IndexListExpr:
			expr = value.X
		case *ast.ParenExpr:
			expr = value.X
		default:
			return expr
		}
	}
}

func explicitSymbols(proposal string) []string {
	seen := map[string]bool{}
	var symbols []string
	for _, match := range backtickPattern.FindAllStringSubmatch(proposal, -1) {
		value := strings.TrimSpace(match[1])
		parsed := bareSymbolPattern.FindStringSubmatch(value)
		if len(parsed) != 2 {
			parsed = qualifiedCallPattern.FindStringSubmatch(value)
		}
		if len(parsed) != 2 {
			continue
		}
		symbol := parsed[1]
		if !seen[symbol] {
			seen[symbol] = true
			symbols = append(symbols, symbol)
		}
	}
	sort.Strings(symbols)
	return symbols
}

func containsSymbolToken(content string, symbols map[string]bool) bool {
	return len(symbolTokens(content, symbols)) > 0
}

func symbolTokens(content string, symbols map[string]bool) map[string]bool {
	found := map[string]bool{}
	file := token.NewFileSet().AddFile("source.go", -1, len(content))
	var scanner goscanner.Scanner
	scanner.Init(file, []byte(content), nil, 0)
	for {
		_, tok, literal := scanner.Scan()
		if tok == token.EOF {
			return found
		}
		if tok == token.IDENT && symbols[literal] {
			found[literal] = true
		}
	}
}

func isBuildConstrained(filePath, content string) bool {
	base := strings.TrimSuffix(path.Base(filePath), ".go")
	base = strings.TrimSuffix(base, "_test")
	parts := strings.Split(base, "_")
	if len(parts) > 1 {
		suffix := parts[len(parts)-1]
		if knownGOOS[suffix] || knownGOARCH[suffix] {
			return true
		}
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//go:build ") || strings.HasPrefix(line, "// +build ") {
			return true
		}
		if strings.HasPrefix(line, "package ") {
			break
		}
	}
	return false
}

func compact(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func inconclusive(reason string) Result {
	return Result{State: StateInconclusive, Reason: reason}
}

func alreadyPresent(symbols []string) Result {
	verb := "is"
	if len(symbols) > 1 {
		verb = "are"
	}
	return Result{
		State:  StateAlreadyPresent,
		Reason: fmt.Sprintf("%s %s already defined and directly used at the grounded commit", strings.Join(symbols, ", "), verb),
	}
}
