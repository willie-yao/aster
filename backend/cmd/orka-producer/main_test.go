package main

import (
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
