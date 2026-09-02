package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAIAnalysisPublicJSONOmitsModelAndRetainsConsumedFields(t *testing.T) {
	analysis := AIAnalysis{
		GeneratedAt: "2026-09-01T00:00:00Z", Model: "private-model",
		RootCause: "controller failed", Severity: "High", SuggestedFix: "fix the controller",
		RelevantFiles: []string{"controllers/example.go"}, Mode: "agentic",
	}
	data, err := json.Marshal(analysis)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "private-model") || strings.Contains(text, `"model"`) {
		t.Fatalf("public analysis leaked model: %s", text)
	}
	for _, want := range []string{
		`"generated_at":"2026-09-01T00:00:00Z"`, `"root_cause":"controller failed"`,
		`"severity":"High"`, `"suggested_fix":"fix the controller"`, `"relevant_files":["controllers/example.go"]`, `"mode":"agentic"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("public analysis missing %s: %s", want, text)
		}
	}
}
