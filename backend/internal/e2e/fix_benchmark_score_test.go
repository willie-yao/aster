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

	verificationOK := false
	verificationDetail := "diff did not apply"
	if contractOK {
		if err := writeFixBenchmarkFiles(repoRoot, benchmarkCase.HiddenFiles); err != nil {
			verificationDetail = err.Error()
		} else {
			verificationDetail, err = runFixBenchmarkCommand(ctx, filepath.Join(repoRoot, filepath.FromSlash(benchmarkCase.Dir)), nil, "go", "test", "./...")
			verificationOK = err == nil
		}
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
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	return cmd.CombinedOutput()
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
		for _, path := range append(append([]string(nil), benchmarkCase.RequiredFiles...), sortedFixBenchmarkKeys(benchmarkCase.HiddenFiles)...) {
			if !strings.HasPrefix(path, benchmarkCase.Dir+"/") {
				t.Fatalf("%s path escapes fixture directory: %s", benchmarkCase.Name, path)
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
	baseCode, err := os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(benchmarkCase.RequiredFiles[0])))
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
		{name: "semantically wrong", make: func(t *testing.T) runtimepkg.GenerateResult {
			return makeFixBenchmarkResult(t, sourceRoot, benchmarkCase, map[string]string{
				benchmarkCase.RequiredFiles[0]: string(baseCode) + "\n// No behavioral fix.\n",
				benchmarkCase.RequiredFiles[1]: string(baseTest) + "\n// Claimed coverage without an assertion.\n",
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
