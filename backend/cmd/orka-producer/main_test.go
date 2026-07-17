package main

import "testing"

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
