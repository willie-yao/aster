package benchmarks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCausalFixPreviewServerImageIncludesValidationToolchain(t *testing.T) {
	text := dockerfile(t)
	if !strings.Contains(text, "FROM golang:1.25.12-alpine AS remote-fixer-runtime") {
		t.Fatal("remote fixer server image lacks the pinned Go validation toolchain")
	}
}

func TestAgentSandboxFixExecutorImageIncludesValidationToolchain(t *testing.T) {
	text := dockerfile(t)
	if !strings.Contains(text, "FROM golang:1.25.12-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS agent-sandbox-fix-go") {
		t.Fatal("Agent Sandbox Fix executor lacks the pinned Go toolchain source")
	}
	block := dockerfileStage(t, text, "agent-sandbox-fix-executor", "agent-sandbox-analysis-executor")
	for _, required := range []string{
		"COPY --from=agent-sandbox-fix-go /usr/local/go /usr/local/go",
		"GOTOOLCHAIN=local",
		`test "$(go env GOVERSION)" = "go1.25.12"`,
		`test "$(opencode --version)" = "1.18.2"`,
	} {
		if !strings.Contains(block, required) {
			t.Fatalf("Agent Sandbox Fix executor stage lacks %q", required)
		}
	}
}

func dockerfile(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func dockerfileStage(t *testing.T, text, stage, nextStage string) string {
	t.Helper()
	start := strings.Index(text, " AS "+stage)
	if start < 0 {
		t.Fatalf("Dockerfile stage %q is missing", stage)
	}
	end := strings.Index(text[start:], " AS "+nextStage)
	if end < 0 {
		t.Fatalf("Dockerfile stage %q is not followed by %q", stage, nextStage)
	}
	return text[start : start+end]
}
