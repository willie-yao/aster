package main

import (
	"strings"
	"testing"
)

func TestParseOptionsRejectsRemovedAnalysisRuntime(t *testing.T) {
	_, _, _, err := parseOptions([]string{"-analysis-runtime=inprocess"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("removed analysis runtime flag error = %v", err)
	}
}

func TestParseOptionsRejectsRemovedPresubmitOverride(t *testing.T) {
	_, _, _, err := parseOptions([]string{"-include-presubmits"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("removed presubmit override error = %v", err)
	}
}
