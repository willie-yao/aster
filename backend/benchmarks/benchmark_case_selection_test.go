package benchmarks

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/project"
)

// shadowBenchmarkCases selects the one pinned external failure a private
// comparison benchmark runs against.
func shadowBenchmarkCases(t *testing.T) []benchCase {
	t.Helper()
	cases := benchCases
	var err error
	if manifest := strings.TrimSpace(os.Getenv("BENCH_MANIFEST")); manifest != "" {
		cases, err = loadBenchmarkManifest(manifest)
		if err != nil {
			t.Fatal(err)
		}
	}
	selected := strings.TrimSpace(os.Getenv("BENCH_CASE"))
	if selected == "" {
		t.Fatal("BENCH_CASE must select exactly one pinned failure")
	}
	cases, err = selectBenchmarkCases(cases, selected)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].stableID == "" || cases[0].consumerCommit == "" {
		t.Fatal("shadow comparison requires one pinned external benchmark case")
	}
	return cases
}

// shadowBenchmarkSkills loads the consumer skill set the benchmark analyses under.
func shadowBenchmarkSkills(t *testing.T, cases []benchCase) *skills.Set {
	t.Helper()
	dir := t.TempDir()
	agentic := defaultBenchAgentic()
	if projectDir := strings.TrimSpace(os.Getenv("BENCH_PROJECT_DIR")); projectDir != "" {
		if len(cases) == 1 && cases[0].consumerCommit != "" {
			if err := validateBenchmarkProjectDir(projectDir, cases[0]); err != nil {
				t.Fatal(err)
			}
		}
		cfg, _, err := project.LoadDir(projectDir)
		if err != nil {
			t.Fatal(err)
		}
		agentic = cfg.AI.EffectiveAgentic()
		dir = projectDir
	} else if cases[0].consumerCommit != "" {
		t.Fatal("pinned external benchmark cases require BENCH_PROJECT_DIR")
	}
	set, _, err := skills.LoadForTools(dir, agentic.Tools)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// shadowBenchmarkDuration reads one optional Go duration override.
func shadowBenchmarkDuration(t *testing.T, name string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("%s must be a Go duration", name)
	}
	return value
}

// shadowBenchmarkInt reads one bounded optional integer override.
func shadowBenchmarkInt(t *testing.T, name string, fallback, minValue, maxValue int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		t.Fatalf("%s must be between %d and %d", name, minValue, maxValue)
	}
	return value
}
