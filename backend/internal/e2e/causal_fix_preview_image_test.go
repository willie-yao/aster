package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCausalFixPreviewServerImageIncludesValidationToolchain(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "FROM golang:1.25.12-alpine AS remote-fixer-runtime") || !strings.Contains(text, "&& go version") {
		t.Fatal("remote fixer server image lacks the pinned Go validation toolchain")
	}
}
