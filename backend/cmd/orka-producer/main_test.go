package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
)

func TestResolveToolsIncludesQualityTools(t *testing.T) {
	names, k8sEnabled := resolveTools([]string{"filesystem"})
	if k8sEnabled {
		t.Fatal("filesystem-only config enabled k8s tools")
	}
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
	}
	for _, want := range qualityTools {
		if !seen[want] {
			t.Fatalf("missing quality tool %q", want)
		}
	}
}

func TestResolveToolsKeepsQualityToolsForExplicitNames(t *testing.T) {
	names, _ := resolveTools([]string{"read-artifact"})
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
	}
	if !seen["read-artifact"] || !seen["validate-analysis"] || !seen["verify-timeline"] {
		t.Fatalf("resolved tools = %v, want explicit and mandatory quality tools", names)
	}
}

func TestBuildToolNameSeparatesConsumerScopes(t *testing.T) {
	projectA := orka.ProjectScopeID("a", "gcs", "bucket", "", "", "")
	projectB := orka.ProjectScopeID("b", "gcs", "bucket", "", "", "")
	scopeA := orka.BuildScopeID(projectA, "job", "1", "logs/job/1/")
	scopeB := orka.BuildScopeID(projectB, "job", "1", "logs/job/1/")
	nameA := buildToolName("read-artifact", orka.ToolScopeID(scopeA, "contract"))
	nameB := buildToolName("read-artifact", orka.ToolScopeID(scopeB, "contract"))
	if nameA == nameB {
		t.Fatalf("consumer-scoped Tool names collided: %q", nameA)
	}
}

func TestCloneSkillAwareToolsCarrySkillContract(t *testing.T) {
	for _, tool := range []string{"required-evidence", "validate-analysis"} {
		t.Run(tool, func(t *testing.T) {
			base := map[string]any{
				"metadata": map[string]any{"name": tool},
				"spec":     map[string]any{"http": map[string]any{"url": "http://artifact-tool/tool/" + tool}},
			}
			got := cloneToolForBuild(base, tool, "scope", "logs/job/1/", "bucket", "orka-system", nil, "encoded-skills", "validation-key", 123, "artifact-tool-auth", "token")
			spec := got["spec"].(map[string]any)
			headers := spec["http"].(map[string]any)["headers"].(map[string]any)
			if headers[orka.ToolScopeHeader] != "scope" {
				t.Fatalf("scope header = %+v", headers)
			}
			if headers["X-Prow-AI-Skills"] != "encoded-skills" {
				t.Fatalf("headers = %+v", headers)
			}
			if tool == "validate-analysis" && headers[orka.ValidationKeyHeader] != "validation-key" {
				t.Fatalf("validation key header = %+v", headers)
			}
			if tool == "validate-analysis" && headers[orka.MinGCSBytesHeader] != "123" {
				t.Fatalf("minimum GCS byte header = %+v", headers)
			}
			auth := spec["http"].(map[string]any)["authSecretRef"].(map[string]any)
			if auth["name"] != "artifact-tool-auth" || auth["key"] != "token" {
				t.Fatalf("authSecretRef = %+v", auth)
			}
		})
	}
}

func TestQualityToolsIncludeDiffLastPassing(t *testing.T) {
	names, _ := resolveTools([]string{"filesystem"})
	for _, name := range names {
		if name == "diff-last-passing" {
			return
		}
	}
	t.Fatalf("resolved tools = %v, want diff-last-passing", names)
}

func TestLoadOrCreateValidationKeyReusesManifestKey(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreateValidationKey(dir)
	if err != nil || first == "" {
		t.Fatalf("first key = %q, err = %v", first, err)
	}
	if err := os.WriteFile(filepath.Join(dir, orka.AnalysisManifestFile), []byte(`{"validation_key":"persisted-key"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateValidationKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != "persisted-key" {
		t.Fatalf("reused key = %q, want persisted-key", second)
	}
}

func TestTaskToolNamesUseTaskSpecificValidator(t *testing.T) {
	got := taskToolNames([]string{"read-artifact", "validate-analysis", "recurrence"}, "build-scope", "az-analysis-task")
	if got[0] != buildToolName("read-artifact", "build-scope") || got[2] != buildToolName("recurrence", "build-scope") {
		t.Fatalf("build-scoped tools = %v", got)
	}
	if got[1] != validationToolName("az-analysis-task") {
		t.Fatalf("validation tool = %q, want task-specific name", got[1])
	}
}
