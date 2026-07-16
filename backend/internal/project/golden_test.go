package project

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestLoadExampleGolden verifies the minimal configs/example/project.yaml parses
// and validates with only the required fields.
func TestLoadExampleGolden(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// backend/internal/project/golden_test.go maps to ../../../configs/example/project.yaml.
	dir := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "configs", "example")
	cfg, prompt, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir(%s): %v", dir, err)
	}

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"id", cfg.ID, "example"},
		{"testgrid.dashboard", cfg.TestGrid.Dashboard, "sig-foo-your-project"},
		{"storage.provider", cfg.Storage.Provider, "gcs"},
		{"storage.bucket", cfg.Storage.Bucket, "kubernetes-ci-logs"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if prompt == "" {
		t.Error("LoadDir returned empty prompt; expected example/prompts/system.md content")
	}
}

// TestLoadReferenceGolden verifies the full project.reference.yaml parses and
// validates, including the optional fields the minimal file omits.
func TestLoadReferenceGolden(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	ref := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "configs", "example", "project.reference.yaml")
	cfg, err := Load(ref)
	if err != nil {
		t.Fatalf("Load(%s): %v", ref, err)
	}
	if cfg.ShortName != "EXAMPLE" {
		t.Errorf("short_name = %q, want EXAMPLE", cfg.ShortName)
	}
	if cfg.AI == nil {
		t.Errorf("reference should carry an active ai: block: %+v", cfg)
	}
}
