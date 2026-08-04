// Package actionverify checks explicit remediation symbols against pinned source.
package actionverify

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	goscanner "go/scanner"
	"go/token"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"gopkg.in/yaml.v3"
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
	Files   map[string]string
}

// Reader fetches one bounded archive for its pinned revision.
type Reader interface {
	ReadSourceArchive(context.Context) (Archive, error)
}

type Input struct {
	Proposal      string
	RelevantFiles []string
	Targets       []models.RemediationTarget
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

type declarationKind int

const (
	declarationMissing declarationKind = iota
	declarationPackage
	declarationMethod
)

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

// UnexpectedImplementationSymbols returns explicit symbols outside the allowed set.
func UnexpectedImplementationSymbols(proposal string, allowed []string) []string {
	allowedSet := make(map[string]bool, len(allowed))
	for _, symbol := range allowed {
		allowedSet[strings.TrimSpace(symbol)] = true
	}
	var unexpected []string
	for _, symbol := range explicitSymbols(proposal) {
		if !allowedSet[symbol] {
			unexpected = append(unexpected, symbol)
		}
	}
	return unexpected
}

func Verify(ctx context.Context, reader Reader, input Input) (Result, error) {
	if len(input.Targets) > 0 {
		return verifyTargets(ctx, reader, input)
	}
	return verifyLegacyProposal(ctx, reader, input)
}

func verifyTargets(ctx context.Context, reader Reader, input Input) (Result, error) {
	if reader == nil {
		return inconclusive("source reader is unavailable"), nil
	}
	archive, err := reader.ReadSourceArchive(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read pinned source archive: %w", err)
	}
	results := make([]Result, 0, len(input.Targets))
	for _, target := range input.Targets {
		if reason := InvalidTargetReason(target); reason != "" {
			return inconclusive(reason), nil
		}
		result, err := verifyTarget(ctx, reader, archive, target)
		if err != nil {
			return Result{}, err
		}
		results = append(results, result)
	}
	return combineTargetResults(results), nil
}

func verifyTarget(ctx context.Context, reader Reader, archive Archive, target models.RemediationTarget) (Result, error) {
	switch target.Intent {
	case models.RemediationIntentAddSymbol:
		return verifyAddSymbol(ctx, reader, archive, target)
	case models.RemediationIntentModifySymbol:
		return verifyModifySymbol(ctx, reader, archive, target)
	case models.RemediationIntentSetConfiguration, models.RemediationIntentRemoveConfiguration:
		return verifyConfiguration(ctx, reader, archive, target)
	case models.RemediationIntentInvestigate:
		return inconclusive("proposal identifies investigation work but no implementation-ready source target"), nil
	default:
		return inconclusive(fmt.Sprintf("remediation intent %q is unsupported", target.Intent)), nil
	}
}

func verifyAddSymbol(ctx context.Context, reader Reader, archive Archive, target models.RemediationTarget) (Result, error) {
	content, ok := archive.GoFiles[target.Path]
	if !archive.Paths[target.Path] || !ok {
		return inconclusive(fmt.Sprintf("remediation path %s is not verified Go source", target.Path)), nil
	}
	if isBuildConstrained(target.Path, content) && !strings.HasSuffix(target.Path, "_test.go") {
		return inconclusive(fmt.Sprintf("remediation source %s is build-constrained", target.Path)), nil
	}
	kind, err := symbolDeclarationKind(target.Path, content, target.Symbol)
	if err != nil {
		return inconclusive(fmt.Sprintf("remediation source %s could not be parsed", target.Path)), nil
	}
	if kind == declarationMethod {
		return inconclusive(fmt.Sprintf("%s is a method in %s, but add_symbol requires a package-level symbol", target.Symbol, target.Path)), nil
	}
	declared := kind == declarationPackage
	used, occurred, uncertain, err := symbolUseAtTarget(ctx, reader, archive, target)
	if err != nil {
		return inconclusive(err.Error()), nil
	}
	if declared && used {
		return Result{State: StateAlreadyPresent, Reason: fmt.Sprintf("%s is already defined in %s and directly used at the grounded commit", target.Symbol, target.Path)}, nil
	}
	if !declared && !occurred && !uncertain {
		return Result{State: StateUnresolved, Reason: fmt.Sprintf("%s is absent from the verified source and remains to be added in %s", target.Symbol, target.Path)}, nil
	}
	if !declared && occurred {
		return inconclusive(fmt.Sprintf("%s occurs elsewhere but is not defined in the proposed path %s", target.Symbol, target.Path)), nil
	}
	return inconclusive(fmt.Sprintf("source does not prove that %s is both defined in %s and directly used", target.Symbol, target.Path)), nil
}

func symbolUseAtTarget(ctx context.Context, reader Reader, archive Archive, target models.RemediationTarget) (used, occurred, uncertain bool, err error) {
	targetContent := archive.GoFiles[target.Path]
	targetFile, err := parser.ParseFile(token.NewFileSet(), target.Path, targetContent, 0)
	if err != nil {
		return false, false, false, fmt.Errorf("remediation source %s could not be parsed", target.Path)
	}
	targetDir := path.Dir(target.Path)
	targetPackageKey := targetDir + "#" + targetFile.Name.Name
	modulePath := ""
	if goMod, found, readErr := readSourceFile(ctx, reader, archive, "go.mod"); readErr != nil {
		return false, false, false, fmt.Errorf("repository module identity could not be read")
	} else if found {
		modulePath = modulePathFromGoMod(goMod)
	}
	expectedImportPath := modulePath
	if modulePath != "" && targetDir != "." {
		expectedImportPath += "/" + targetDir
	}
	symbols := map[string]bool{target.Symbol: true}
	matchingFiles, matchingBytes := 0, 0
	for file, content := range archive.GoFiles {
		if !containsSymbolToken(content, symbols) {
			continue
		}
		occurred = true
		matchingFiles++
		matchingBytes += len(content)
		if matchingFiles > maxMatchingFiles || matchingBytes > maxMatchingBytes {
			return false, occurred, true, fmt.Errorf("matching source exceeds verification limits")
		}
		if isBuildConstrained(file, content) && !strings.HasSuffix(file, "_test.go") {
			uncertain = true
			continue
		}
		item, parseErr := inspectStructuredReferences(file, content, target.Symbol, targetFile.Name.Name, expectedImportPath)
		if parseErr != nil {
			uncertain = true
			continue
		}
		if path.Dir(file)+"#"+item.packageName == targetPackageKey && item.packageReference {
			used = true
		}
		if item.importedReference {
			used = true
		}
		if item.ambiguousImport {
			uncertain = true
		}
	}
	return used, occurred, uncertain, nil
}

type structuredReferenceEvidence struct {
	packageName       string
	packageReference  bool
	importedReference bool
	ambiguousImport   bool
}

func inspectStructuredReferences(filePath, content, symbol, targetPackageName, expectedImportPath string) (structuredReferenceEvidence, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filePath, content, 0)
	if err != nil {
		return structuredReferenceEvidence{}, err
	}
	evidence := structuredReferenceEvidence{packageName: file.Name.Name}
	imports := map[string]string{}
	for _, spec := range file.Imports {
		importPath, unquoteErr := strconv.Unquote(spec.Path.Value)
		if unquoteErr != nil {
			continue
		}
		alias := path.Base(importPath)
		if importPath == expectedImportPath && targetPackageName != "" {
			alias = targetPackageName
		}
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if alias != "" && alias != "." && alias != "_" {
			imports[alias] = importPath
		}
	}

	topLevel := file.Scope.Lookup(symbol)
	var selfStart, selfEnd token.Pos
	for _, declaration := range file.Decls {
		switch value := declaration.(type) {
		case *ast.FuncDecl:
			if value.Recv == nil && value.Name.Name == symbol {
				selfStart, selfEnd = value.Pos(), value.End()
			}
		case *ast.GenDecl:
			for _, spec := range value.Specs {
				switch item := spec.(type) {
				case *ast.TypeSpec:
					if item.Name.Name == symbol {
						selfStart, selfEnd = item.Pos(), item.End()
					}
				case *ast.ValueSpec:
					for _, name := range item.Names {
						if name.Name == symbol {
							selfStart, selfEnd = item.Pos(), item.End()
						}
					}
				}
			}
		}
	}

	excluded := map[*ast.Ident]bool{file.Name: true}
	ast.Inspect(file, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.FuncDecl:
			excluded[value.Name] = true
		case *ast.TypeSpec:
			excluded[value.Name] = true
		case *ast.ValueSpec:
			for _, name := range value.Names {
				excluded[name] = true
			}
		case *ast.Field:
			for _, name := range value.Names {
				excluded[name] = true
			}
		case *ast.ImportSpec:
			if value.Name != nil {
				excluded[value.Name] = true
			}
		case *ast.LabeledStmt:
			excluded[value.Label] = true
		case *ast.BranchStmt:
			if value.Label != nil {
				excluded[value.Label] = true
			}
		case *ast.CompositeLit:
			if compositeLiteralIsMap(file, value) {
				break
			}
			for _, element := range value.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := pair.Key.(*ast.Ident); ok && (key.Obj == nil || key.Obj != topLevel) {
					excluded[key] = true
				}
			}
		case *ast.SelectorExpr:
			excluded[value.Sel] = true
			if value.Sel.Name != symbol {
				break
			}
			qualifier, ok := value.X.(*ast.Ident)
			if !ok || qualifier.Obj != nil {
				break
			}
			importPath := imports[qualifier.Name]
			if importPath == "" {
				break
			}
			if expectedImportPath == "" {
				evidence.ambiguousImport = true
			} else if importPath == expectedImportPath {
				evidence.importedReference = true
			}
		}
		return true
	})

	ast.Inspect(file, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if !ok || identifier.Name != symbol || excluded[identifier] {
			return true
		}
		if selfStart.IsValid() && identifier.Pos() >= selfStart && identifier.End() <= selfEnd {
			return true
		}
		if identifier.Obj != nil && identifier.Obj != topLevel {
			return true
		}
		evidence.packageReference = true
		return true
	})
	return evidence, nil
}

func compositeLiteralIsMap(file *ast.File, literal *ast.CompositeLit) bool {
	switch value := literal.Type.(type) {
	case *ast.MapType:
		return true
	case *ast.Ident:
		object := file.Scope.Lookup(value.Name)
		if object == nil || object.Kind != ast.Typ {
			return false
		}
		spec, ok := object.Decl.(*ast.TypeSpec)
		if !ok {
			return false
		}
		_, ok = spec.Type.(*ast.MapType)
		return ok
	default:
		return false
	}
}

func modulePathFromGoMod(content string) string {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return ""
}

func verifyModifySymbol(ctx context.Context, reader Reader, archive Archive, target models.RemediationTarget) (Result, error) {
	content, ok, err := readSourceFile(ctx, reader, archive, target.Path)
	if err != nil {
		return Result{}, fmt.Errorf("read pinned source file %s: %w", target.Path, err)
	}
	if !ok || !strings.HasSuffix(target.Path, ".go") {
		return inconclusive(fmt.Sprintf("remediation path %s is not verified Go source", target.Path)), nil
	}
	declared, err := declaresSymbol(target.Path, content, target.Symbol)
	if err != nil {
		return inconclusive(fmt.Sprintf("remediation source %s could not be parsed", target.Path)), nil
	}
	if !declared {
		return inconclusive(fmt.Sprintf("%s is not defined in the proposed path %s", target.Symbol, target.Path)), nil
	}
	return Result{State: StateUnresolved, Reason: fmt.Sprintf("verified %s in %s requires the proposed behavior change", target.Symbol, target.Path)}, nil
}

func verifyConfiguration(ctx context.Context, reader Reader, archive Archive, target models.RemediationTarget) (Result, error) {
	content, ok, err := readSourceFile(ctx, reader, archive, target.Path)
	if err != nil {
		return Result{}, fmt.Errorf("read pinned source file %s: %w", target.Path, err)
	}
	if !ok {
		return inconclusive(fmt.Sprintf("remediation path %s was not found", target.Path)), nil
	}
	present, supported := configurationValuePresent(target.Path, content, target.Value)
	if !supported {
		return inconclusive(fmt.Sprintf("configuration format for %s is unsupported", target.Path)), nil
	}
	switch target.Intent {
	case models.RemediationIntentSetConfiguration:
		if present {
			return Result{State: StateAlreadyPresent, Reason: fmt.Sprintf("configuration %s is already applied in %s", target.Value, target.Path)}, nil
		}
		return Result{State: StateUnresolved, Reason: fmt.Sprintf("configuration %s is missing from %s", target.Value, target.Path)}, nil
	case models.RemediationIntentRemoveConfiguration:
		if !present {
			return Result{State: StateAlreadyPresent, Reason: fmt.Sprintf("configuration %s is already absent from %s", target.Value, target.Path)}, nil
		}
		return Result{State: StateUnresolved, Reason: fmt.Sprintf("configuration %s remains in %s", target.Value, target.Path)}, nil
	default:
		return inconclusive("configuration remediation intent is unsupported"), nil
	}
}

// InvalidTargetReason returns why a structured remediation target is unusable.
func InvalidTargetReason(target models.RemediationTarget) string {
	validPath := func(value string) bool {
		return value != "" && len(value) <= 512 && !strings.HasPrefix(value, "/") && !strings.Contains(value, "\\") &&
			path.Clean(value) == value && value != "." && value != ".." && !strings.HasPrefix(value, "../")
	}
	switch target.Intent {
	case models.RemediationIntentAddSymbol, models.RemediationIntentModifySymbol:
		if !token.IsIdentifier(target.Symbol) || !validPath(target.Path) || !strings.HasSuffix(target.Path, ".go") || target.Value != "" {
			return "symbol remediation metadata is invalid"
		}
	case models.RemediationIntentSetConfiguration, models.RemediationIntentRemoveConfiguration:
		key, expected, assignment := strings.Cut(target.Value, "=")
		if target.Symbol != "" || !validPath(target.Path) || !assignment || strings.TrimSpace(key) == "" || strings.TrimSpace(expected) == "" ||
			len(target.Value) > 256 || strings.ContainsAny(target.Value, "\r\n\x00") {
			return "configuration remediation metadata is invalid"
		}
	case models.RemediationIntentInvestigate:
		if target.Symbol != "" || target.Path != "" || target.Value != "" {
			return "investigation remediation metadata must not claim a source target"
		}
	default:
		return fmt.Sprintf("remediation intent %q is unsupported", target.Intent)
	}
	return ""
}

func combineTargetResults(results []Result) Result {
	if len(results) == 0 {
		return inconclusive("proposal has no remediation targets")
	}
	for _, result := range results {
		if result.State == StateInconclusive {
			return result
		}
	}
	for _, result := range results {
		if result.State == StateUnresolved {
			return result
		}
	}
	reasons := make([]string, 0, len(results))
	for _, result := range results {
		reasons = append(reasons, result.Reason)
	}
	return Result{State: StateAlreadyPresent, Reason: strings.Join(reasons, "; ")}
}

type sourceFileReader interface {
	ReadFile(context.Context, string) (string, bool, error)
}

func readSourceFile(ctx context.Context, reader Reader, archive Archive, file string) (string, bool, error) {
	if content, ok := archive.GoFiles[file]; ok {
		return content, true, nil
	}
	if content, ok := archive.Files[file]; ok {
		return content, true, nil
	}
	if source, ok := reader.(sourceFileReader); ok {
		return source.ReadFile(ctx, file)
	}
	return "", false, nil
}

func declaresSymbol(filePath, content, symbol string) (bool, error) {
	kind, err := symbolDeclarationKind(filePath, content, symbol)
	return kind != declarationMissing, err
}

func symbolDeclarationKind(filePath, content, symbol string) (declarationKind, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filePath, content, 0)
	if err != nil {
		return declarationMissing, err
	}
	method := false
	for _, declaration := range file.Decls {
		switch value := declaration.(type) {
		case *ast.FuncDecl:
			if value.Name.Name == symbol {
				if value.Recv == nil {
					return declarationPackage, nil
				}
				method = true
			}
		case *ast.GenDecl:
			for _, spec := range value.Specs {
				switch item := spec.(type) {
				case *ast.TypeSpec:
					if item.Name.Name == symbol {
						return declarationPackage, nil
					}
				case *ast.ValueSpec:
					for _, name := range item.Names {
						if name.Name == symbol {
							return declarationPackage, nil
						}
					}
				}
			}
		}
	}
	if method {
		return declarationMethod, nil
	}
	return declarationMissing, nil
}

func configurationValuePresent(filePath, content, value string) (bool, bool) {
	value = strings.TrimSpace(value)
	key, expected, assignment := strings.Cut(value, "=")
	if !assignment {
		return false, true
	}
	key, expected = strings.TrimSpace(key), strings.TrimSpace(expected)
	switch strings.ToLower(path.Ext(filePath)) {
	case ".yaml", ".yml":
		if present, parsed := yamlConfigurationValuePresent(content, key, expected); parsed {
			return present, true
		}
		return false, false
	case ".json":
		if present, parsed := jsonConfigurationValuePresent(content, key, expected); parsed {
			return present, true
		}
		return false, false
	}
	markers, supported := configCommentMarkers(filePath)
	if !supported {
		return false, false
	}
	pattern := regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(key) + `\s*=\s*` + regexp.QuoteMeta(expected) + `(?:\s|[,\]}"']|$)`)
	mappingPattern := regexp.MustCompile(`(^|[\s{,\-])["']?` + regexp.QuoteMeta(key) + `["']?\s*:\s*["']?` + regexp.QuoteMeta(expected) + `["']?(?:\s|[,\]}]|$)`)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(stripLineComment(line, markers))
		if line == "" {
			continue
		}
		if pattern.MatchString(line) || mappingPattern.MatchString(line) {
			return true, true
		}
	}
	return false, true
}

func yamlConfigurationValuePresent(content, key, expected string) (bool, bool) {
	decoder := yaml.NewDecoder(strings.NewReader(content))
	parsed := false
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if err == io.EOF {
			return false, parsed
		}
		if err != nil {
			return false, false
		}
		if len(document.Content) == 0 {
			continue
		}
		parsed = true
		if yamlNodeHasConfiguration(&document, key, expected) {
			return true, true
		}
	}
}

func yamlNodeHasConfiguration(node *yaml.Node, key, expected string) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			name, value := node.Content[i], node.Content[i+1]
			if name.Kind == yaml.ScalarNode && strings.TrimSpace(name.Value) == key && configScalarMatches(value, expected) {
				return true
			}
			if yamlNodeHasConfiguration(value, key, expected) {
				return true
			}
		}
		return false
	}
	if node.Kind == yaml.ScalarNode && assignmentTokenPresent(node.Value, key, expected) {
		return true
	}
	for _, child := range node.Content {
		if yamlNodeHasConfiguration(child, key, expected) {
			return true
		}
	}
	return false
}

func jsonConfigurationValuePresent(content, key, expected string) (bool, bool) {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return false, false
	}
	return jsonValueHasConfiguration(value, key, expected), true
}

func jsonValueHasConfiguration(value any, key, expected string) bool {
	switch item := value.(type) {
	case map[string]any:
		for name, child := range item {
			if name == key && configValueMatches(child, expected) {
				return true
			}
			if jsonValueHasConfiguration(child, key, expected) {
				return true
			}
		}
	case []any:
		for _, child := range item {
			if jsonValueHasConfiguration(child, key, expected) {
				return true
			}
		}
	case string:
		return assignmentTokenPresent(item, key, expected)
	}
	return false
}

func configScalarMatches(node *yaml.Node, expected string) bool {
	return node != nil && node.Kind == yaml.ScalarNode && strings.TrimSpace(node.Value) == expected
}

func configValueMatches(value any, expected string) bool {
	switch item := value.(type) {
	case string:
		return strings.TrimSpace(item) == expected
	case bool:
		return strconv.FormatBool(item) == expected
	case json.Number:
		return item.String() == expected
	case nil:
		return expected == "null"
	default:
		return false
	}
}

func assignmentTokenPresent(content, key, expected string) bool {
	pattern := regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(key) + `\s*=\s*` + regexp.QuoteMeta(expected) + `(?:\s|[,\]}"']|$)`)
	return pattern.MatchString(content)
}

func configCommentMarkers(filePath string) ([]string, bool) {
	switch strings.ToLower(path.Ext(filePath)) {
	case ".yaml", ".yml", ".toml", ".cfg", ".conf", ".sh", ".py", ".star", ".bzl", ".env":
		return []string{"#"}, true
	case ".ini":
		return []string{"#", ";"}, true
	case ".properties":
		return []string{"#", "!"}, true
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".java", ".rs", ".c", ".cc", ".cpp", ".h", ".hpp", ".proto":
		return []string{"//"}, true
	case ".json":
		return nil, true
	case ".tpl":
		return []string{"#", "//"}, true
	default:
		base := strings.ToLower(path.Base(filePath))
		if base == "dockerfile" || base == "makefile" {
			return []string{"#"}, true
		}
		return nil, false
	}
}

func stripLineComment(line string, markers []string) string {
	var quote byte
	escaped := false
	for i := 0; i < len(line); i++ {
		char := line[i]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' && quote != '\'' {
				escaped = true
				continue
			}
			if char == quote {
				if quote == '\'' && i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' || char == '`' {
			quote = char
			continue
		}
		for _, marker := range markers {
			if strings.HasPrefix(line[i:], marker) {
				return line[:i]
			}
		}
	}
	return line
}

func verifyLegacyProposal(ctx context.Context, reader Reader, input Input) (Result, error) {
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
