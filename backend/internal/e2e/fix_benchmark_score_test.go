package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	runtimepkg "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

type fixBenchmarkCheck struct {
	Name   string
	Must   bool
	Passed bool
	Detail string
}

type fixBenchmarkScore struct {
	Hit        int
	Total      int
	MissedMust []string
	Checks     []fixBenchmarkCheck
}

func (s *fixBenchmarkScore) add(name string, must, passed bool, detail string) {
	s.Total++
	if passed {
		s.Hit++
	}
	if must && !passed {
		s.MissedMust = append(s.MissedMust, name)
	}
	s.Checks = append(s.Checks, fixBenchmarkCheck{Name: name, Must: must, Passed: passed, Detail: boundedFixBenchmarkDetail(detail)})
}

func scoreFixBenchmarkResult(ctx context.Context, sourceRoot string, benchmarkCase fixBenchmarkCase, result runtimepkg.GenerateResult) fixBenchmarkScore {
	var score fixBenchmarkScore
	resultPresent := strings.TrimSpace(result.Diff) != "" && len(result.Files) > 0
	score.add("result", true, resultPresent, "runtime returned a non-empty diff and file set")

	gotFiles := sortedFixBenchmarkKeys(result.Files)
	wantFiles := append([]string(nil), benchmarkCase.RequiredFiles...)
	sort.Strings(wantFiles)
	scopeOK := slices.Equal(gotFiles, wantFiles)
	score.add("file_scope", true, scopeOK, fmt.Sprintf("got=%v want=%v", gotFiles, wantFiles))

	repoRoot, cleanup, err := prepareFixBenchmarkRepo(sourceRoot, benchmarkCase)
	if err != nil {
		score.add("diff_contract", true, false, err.Error())
		score.add("protected_files", true, false, "fixture setup failed")
		score.add("verification", true, false, "fixture setup failed")
		return score
	}
	defer cleanup()

	protectedBefore, err := fixBenchmarkProtectedHashes(repoRoot, benchmarkCase)
	if err != nil {
		score.add("diff_contract", true, false, err.Error())
		score.add("protected_files", true, false, err.Error())
		score.add("verification", true, false, "protected-file snapshot failed")
		return score
	}

	applyOutput, applyErr := runFixBenchmarkCommand(ctx, repoRoot, []byte(result.Diff), "git", "apply", "--check", "-")
	contractOK := applyErr == nil
	contractDetail := applyOutput
	if contractOK {
		applyOutput, applyErr = runFixBenchmarkCommand(ctx, repoRoot, []byte(result.Diff), "git", "apply", "-")
		contractOK = applyErr == nil
		contractDetail = applyOutput
	}
	if contractOK {
		changedOutput, changedErr := runFixBenchmarkCommandRaw(ctx, repoRoot, nil, "git", "status", "--porcelain", "--untracked-files=all")
		changedFiles := fixBenchmarkStatusPaths(string(changedOutput))
		sort.Strings(changedFiles)
		if changedErr != nil || !slices.Equal(changedFiles, gotFiles) {
			contractOK = false
			contractDetail = fmt.Sprintf("diff paths=%v reported files=%v: %v", changedFiles, gotFiles, changedErr)
		}
	}
	if contractOK {
		for path, want := range result.Files {
			got, readErr := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
			if readErr != nil || string(got) != want {
				contractOK = false
				contractDetail = fmt.Sprintf("result file %s does not match applied diff: %v", path, readErr)
				break
			}
		}
	}
	score.add("diff_contract", true, contractOK, contractDetail)

	protectedAfter, hashErr := fixBenchmarkProtectedHashes(repoRoot, benchmarkCase)
	protectedOK := hashErr == nil && fixBenchmarkHashesEqual(protectedBefore, protectedAfter)
	protectedDetail := "protected files unchanged"
	if hashErr != nil {
		protectedDetail = hashErr.Error()
	} else if !protectedOK {
		protectedDetail = fmt.Sprintf("before=%v after=%v", protectedBefore, protectedAfter)
	}
	score.add("protected_files", true, protectedOK, protectedDetail)

	if len(benchmarkCase.RegressionTestFiles) > 0 {
		regressionOK, regressionDetail := runFixBenchmarkRegressionTests(ctx, sourceRoot, benchmarkCase, result)
		score.add("regression_test", true, regressionOK, regressionDetail)
	}

	verificationOK := false
	verificationDetail := "diff did not apply"
	if contractOK {
		verificationDetail, err = runFixBenchmarkVerifier(ctx, repoRoot, benchmarkCase)
		verificationOK = err == nil
	}
	score.add("verification", true, verificationOK, verificationDetail)
	return score
}

func fixBenchmarkStatusPaths(status string) []string {
	var paths []string
	for _, line := range strings.Split(status, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if _, renamed, ok := strings.Cut(path, " -> "); ok {
			path = renamed
		}
		paths = append(paths, filepath.ToSlash(path))
	}
	sort.Strings(paths)
	return paths
}

func prepareFixBenchmarkRepo(sourceRoot string, benchmarkCase fixBenchmarkCase) (string, func(), error) {
	repoRoot, err := os.MkdirTemp("", "fix-benchmark-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(repoRoot) }
	if err := copyFixBenchmarkTree(filepath.Join(sourceRoot, filepath.FromSlash(benchmarkCase.Dir)), filepath.Join(repoRoot, filepath.FromSlash(benchmarkCase.Dir))); err != nil {
		cleanup()
		return "", func() {}, err
	}
	commands := [][]string{
		{"git", "init", "-q"},
		{"git", "add", "."},
		{"git", "-c", "commit.gpgsign=false", "-c", "user.name=Fix Benchmark", "-c", "user.email=benchmark@example.invalid", "commit", "-qm", "fixture"},
	}
	for _, command := range commands {
		if output, err := runFixBenchmarkCommand(context.Background(), repoRoot, nil, command[0], command[1:]...); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("%s: %w: %s", strings.Join(command, " "), err, output)
		}
	}
	return repoRoot, cleanup, nil
}

func makeFixBenchmarkResult(t *testing.T, sourceRoot string, benchmarkCase fixBenchmarkCase, files map[string]string) runtimepkg.GenerateResult {
	t.Helper()
	repoRoot, cleanup, err := prepareFixBenchmarkRepo(sourceRoot, benchmarkCase)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := writeFixBenchmarkFiles(repoRoot, files); err != nil {
		t.Fatal(err)
	}
	if output, err := runFixBenchmarkCommand(context.Background(), repoRoot, nil, "git", "add", "-N", "."); err != nil {
		t.Fatalf("git add -N: %v: %s", err, output)
	}
	diff, err := runFixBenchmarkCommandRaw(context.Background(), repoRoot, nil, "git", "diff", "--binary", "--full-index")
	if err != nil {
		t.Fatal(err)
	}
	out := runtimepkg.GenerateResult{Files: make(map[string]string, len(files)), Diff: string(diff)}
	for path := range files {
		contents, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		out.Files[path] = string(contents)
	}
	return out
}

func fixBenchmarkProtectedHashes(repoRoot string, benchmarkCase fixBenchmarkCase) (map[string][sha256.Size]byte, error) {
	required := map[string]bool{}
	for _, path := range benchmarkCase.RequiredFiles {
		required[filepath.ToSlash(path)] = true
	}
	hashes := map[string][sha256.Size]byte{}
	base := filepath.Join(repoRoot, filepath.FromSlash(benchmarkCase.Dir))
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if required[rel] {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hashes[rel] = sha256.Sum256(contents)
		return nil
	})
	return hashes, err
}

func fixBenchmarkHashesEqual(a, b map[string][sha256.Size]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for path, want := range a {
		if got, ok := b[path]; !ok || got != want {
			return false
		}
	}
	return true
}

func writeFixBenchmarkFiles(repoRoot string, files map[string]string) error {
	for path, contents := range files {
		target := filepath.Join(repoRoot, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(contents), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func copyFixBenchmarkTree(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture contains symlink %s", path)
		}
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, contents, 0o644)
	})
}

func runFixBenchmarkCommand(ctx context.Context, dir string, stdin []byte, name string, args ...string) (string, error) {
	output, err := runFixBenchmarkCommandRaw(ctx, dir, stdin, name, args...)
	return boundedFixBenchmarkDetail(string(output)), err
}

func runFixBenchmarkCommandRaw(ctx context.Context, dir string, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = fixBenchmarkCommandEnv()
	return cmd.CombinedOutput()
}

func fixBenchmarkCommandEnv() []string {
	blocked := map[string]bool{
		"GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_SYSTEM": true, "GIT_TERMINAL_PROMPT": true,
		"GO111MODULE": true, "GOENV": true, "GOFLAGS": true, "GOWORK": true,
	}
	env := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || blocked[name] {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GOENV=off",
		"GOFLAGS=",
		"GO111MODULE=on",
		"GOWORK=off",
	)
}

func runFixBenchmarkVerifier(ctx context.Context, repoRoot string, benchmarkCase fixBenchmarkCase) (string, error) {
	// Generated fixes may be incorrect, but the scorer does not treat them as
	// active attempts to forge benchmark telemetry. Runtime isolation owns that boundary.
	fixtureDir := filepath.Join(repoRoot, filepath.FromSlash(benchmarkCase.Dir))
	verifierDir := filepath.Join(repoRoot, ".fix-benchmark-verifier", benchmarkCase.Name)
	if err := os.MkdirAll(verifierDir, 0o755); err != nil {
		return "", err
	}
	goMod := fmt.Sprintf("module fixbench/verifier\n\ngo 1.25\n\nrequire %s v0.0.0\n\nreplace %s => %q\n", benchmarkCase.Module, benchmarkCase.Module, filepath.ToSlash(fixtureDir))
	if err := os.WriteFile(filepath.Join(verifierDir, "go.mod"), []byte(goMod), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(verifierDir, "verifier_test.go"), []byte(benchmarkCase.VerifierSource), 0o644); err != nil {
		return "", err
	}
	verifierOutput, err := runFixBenchmarkCommand(ctx, verifierDir, nil, "go", "test", "-count=1", "-run", "^TestBenchmarkVerifier$", ".")
	if err != nil {
		return verifierOutput, fmt.Errorf("benchmark verifier: %w", err)
	}

	publicDir, err := os.MkdirTemp("", "fix-benchmark-public-tests-")
	if err != nil {
		return verifierOutput, err
	}
	defer os.RemoveAll(publicDir) //nolint:errcheck
	if err := copyFixBenchmarkTree(fixtureDir, publicDir); err != nil {
		return verifierOutput, err
	}
	publicOutput, err := runFixBenchmarkCommand(ctx, publicDir, nil, "go", "test", "-count=1", "./...")
	if err != nil {
		return strings.TrimSpace(verifierOutput + "\n" + publicOutput), fmt.Errorf("candidate tests: %w", err)
	}
	return strings.TrimSpace(publicOutput + "\n" + verifierOutput), nil
}

func runFixBenchmarkRegressionTests(ctx context.Context, sourceRoot string, benchmarkCase fixBenchmarkCase, result runtimepkg.GenerateResult) (bool, string) {
	repoRoot, cleanup, err := prepareFixBenchmarkRepo(sourceRoot, benchmarkCase)
	if err != nil {
		return false, err.Error()
	}
	defer cleanup()
	testFiles := make(map[string]string, len(benchmarkCase.RegressionTestFiles))
	for _, path := range benchmarkCase.RegressionTestFiles {
		contents, ok := result.Files[path]
		if !ok {
			return false, fmt.Sprintf("candidate did not report regression test %s", path)
		}
		testFiles[path] = contents
	}
	if err := writeFixBenchmarkFiles(repoRoot, testFiles); err != nil {
		return false, err.Error()
	}
	output, testErr := runFixBenchmarkCommand(ctx, filepath.Join(repoRoot, filepath.FromSlash(benchmarkCase.Dir)), nil, "go", "test", "-count=1", "./...")
	if testErr == nil {
		return false, "candidate regression tests passed against the original broken implementation"
	}
	return true, "candidate regression tests rejected the original broken implementation: " + output
}

func boundedFixBenchmarkDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	const limit = 512
	if len(detail) <= limit {
		return detail
	}
	return "..." + detail[len(detail)-limit:]
}

func sortedFixBenchmarkKeys(files map[string]string) []string {
	keys := make([]string, 0, len(files))
	for path := range files {
		keys = append(keys, filepath.ToSlash(path))
	}
	sort.Strings(keys)
	return keys
}

func fixBenchmarkSourceRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFixBenchmarkCaseCatalog(t *testing.T) {
	seen := map[string]bool{}
	for _, benchmarkCase := range fixBenchmarkCases() {
		if benchmarkCase.Name == "" || seen[benchmarkCase.Name] {
			t.Fatalf("invalid benchmark case name %q", benchmarkCase.Name)
		}
		seen[benchmarkCase.Name] = true
		if strings.TrimSpace(benchmarkCase.Instruction) == "" {
			t.Fatalf("%s has no instruction", benchmarkCase.Name)
		}
		want := append([]string(nil), benchmarkCase.RequiredFiles...)
		got := sortedFixBenchmarkKeys(benchmarkCase.ReferenceFiles)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Fatalf("%s reference files = %v, want %v", benchmarkCase.Name, got, want)
		}
		if strings.TrimSpace(benchmarkCase.Module) == "" || strings.TrimSpace(benchmarkCase.VerifierSource) == "" {
			t.Fatalf("%s has no benchmark-owned verifier", benchmarkCase.Name)
		}
		for _, path := range benchmarkCase.RequiredFiles {
			if !strings.HasPrefix(path, benchmarkCase.Dir+"/") {
				t.Fatalf("%s path escapes fixture directory: %s", benchmarkCase.Name, path)
			}
		}
		for _, path := range benchmarkCase.RegressionTestFiles {
			if !slices.Contains(benchmarkCase.RequiredFiles, path) || !strings.HasSuffix(path, "_test.go") {
				t.Fatalf("%s invalid regression test path: %s", benchmarkCase.Name, path)
			}
		}
	}
	if _, ok := fixBenchmarkCaseByName("retry-validation"); !ok {
		t.Fatal("retry benchmark case lookup failed")
	}
}

func TestFixBenchmarkReferenceResultsPass(t *testing.T) {
	sourceRoot := fixBenchmarkSourceRoot(t)
	for _, benchmarkCase := range fixBenchmarkCases() {
		t.Run(benchmarkCase.Name, func(t *testing.T) {
			result := makeFixBenchmarkResult(t, sourceRoot, benchmarkCase, benchmarkCase.ReferenceFiles)
			score := scoreFixBenchmarkResult(context.Background(), sourceRoot, benchmarkCase, result)
			if len(score.MissedMust) != 0 || score.Hit != score.Total {
				t.Fatalf("score = %+v", score)
			}
		})
	}
}

func TestFixBenchmarkRejectsIncompleteOrUnsafeResults(t *testing.T) {
	sourceRoot := fixBenchmarkSourceRoot(t)
	benchmarkCase, _ := fixBenchmarkCaseByName("route-table-defaulting")
	baseTest, err := os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(benchmarkCase.RequiredFiles[1])))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		make func(t *testing.T) runtimepkg.GenerateResult
		miss string
	}{
		{name: "empty", make: func(*testing.T) runtimepkg.GenerateResult { return runtimepkg.GenerateResult{} }, miss: "result"},
		{name: "test only", make: func(t *testing.T) runtimepkg.GenerateResult {
			return makeFixBenchmarkResult(t, sourceRoot, benchmarkCase, map[string]string{
				benchmarkCase.RequiredFiles[1]: string(baseTest) + "\n// Test-only change.\n",
			})
		}, miss: "file_scope"},
		{name: "unexpected file", make: func(t *testing.T) runtimepkg.GenerateResult {
			files := map[string]string{}
			for path, contents := range benchmarkCase.ReferenceFiles {
				files[path] = contents
			}
			files[benchmarkCase.Dir+"/README.md"] = "unrelated\n"
			return makeFixBenchmarkResult(t, sourceRoot, benchmarkCase, files)
		}, miss: "file_scope"},
		{name: "unreported diff file", make: func(t *testing.T) runtimepkg.GenerateResult {
			files := map[string]string{}
			for path, contents := range benchmarkCase.ReferenceFiles {
				files[path] = contents
			}
			extra := benchmarkCase.Dir + "/README.md"
			files[extra] = "unreported\n"
			result := makeFixBenchmarkResult(t, sourceRoot, benchmarkCase, files)
			delete(result.Files, extra)
			return result
		}, miss: "diff_contract"},
		{name: "unapplicable diff", make: func(t *testing.T) runtimepkg.GenerateResult {
			result := makeFixBenchmarkResult(t, sourceRoot, benchmarkCase, benchmarkCase.ReferenceFiles)
			result.Diff = "not a diff"
			return result
		}, miss: "diff_contract"},
		{name: "nondiscriminating regression test", make: func(t *testing.T) runtimepkg.GenerateResult {
			return makeFixBenchmarkResult(t, sourceRoot, benchmarkCase, map[string]string{
				benchmarkCase.RequiredFiles[0]: benchmarkCase.ReferenceFiles[benchmarkCase.RequiredFiles[0]],
				benchmarkCase.RequiredFiles[1]: string(baseTest) + "\n// Comment-only test change.\n",
			})
		}, miss: "regression_test"},
		{name: "semantically wrong but well formed", make: func(t *testing.T) runtimepkg.GenerateResult {
			return makeFixBenchmarkResult(t, sourceRoot, benchmarkCase, map[string]string{
				benchmarkCase.RequiredFiles[0]: `package routetable

// NetworkSpec contains the route table names used by the control-plane and node subnets.
type NetworkSpec struct {
	ControlPlaneRouteTable string
	NodeRouteTable         string
}

// DefaultControlPlaneRouteTable applies network defaults without replacing explicit values.
func DefaultControlPlaneRouteTable(spec *NetworkSpec) {
	if spec == nil || spec.ControlPlaneRouteTable != "" {
		return
	}
	spec.ControlPlaneRouteTable = "some-default"
}
`,
				benchmarkCase.RequiredFiles[1]: `package routetable

import "testing"

func TestDefaultControlPlaneRouteTableSetsAValue(t *testing.T) {
	spec := &NetworkSpec{NodeRouteTable: "node"}
	DefaultControlPlaneRouteTable(spec)
	if spec.ControlPlaneRouteTable == "" {
		t.Fatal("control-plane route table was not defaulted")
	}
}
`,
			})
		}, miss: "verification"},
		{name: "protected file", make: func(t *testing.T) runtimepkg.GenerateResult {
			files := map[string]string{}
			for path, contents := range benchmarkCase.ReferenceFiles {
				files[path] = contents
			}
			files[benchmarkCase.Dir+"/go.mod"] = "module changed.example/unsafe\n\ngo 1.25\n"
			return makeFixBenchmarkResult(t, sourceRoot, benchmarkCase, files)
		}, miss: "protected_files"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			score := scoreFixBenchmarkResult(context.Background(), sourceRoot, benchmarkCase, tc.make(t))
			if !slices.Contains(score.MissedMust, tc.miss) {
				t.Fatalf("missed = %v, want %s; checks=%+v", score.MissedMust, tc.miss, score.Checks)
			}
		})
	}
}

func TestFixBenchmarkCommandEnvForcesModuleMode(t *testing.T) {
	t.Setenv("GO111MODULE", "off")
	env := map[string]string{}
	for _, entry := range fixBenchmarkCommandEnv() {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			env[name] = value
		}
	}
	if env["GO111MODULE"] != "on" {
		t.Fatalf("GO111MODULE = %q, want on", env["GO111MODULE"])
	}
}
