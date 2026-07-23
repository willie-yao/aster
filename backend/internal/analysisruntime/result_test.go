package analysisruntime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func testResult() ai.FailureAnalysisResult {
	return ai.FailureAnalysisResult{
		Summary:  &models.AISummary{GeneratedAt: "2026-07-23T00:00:00Z", Summary: "summary"},
		Analysis: &models.AIAnalysis{RootCause: "cause", Severity: "High", Mode: "agentic"},
	}
}

func framedResult(t *testing.T, result ai.FailureAnalysisResult) string {
	t.Helper()
	var out bytes.Buffer
	if err := WriteFailureAnalysisResult(&out, result); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestFailureAnalysisResultRoundTripFromMixedLogs(t *testing.T) {
	want := testResult()
	marker := framedResult(t, want)
	raw := "runtime log before\n" + marker + "runtime log after\n"
	got, err := ParseFailureAnalysisResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %+v, want %+v", got, want)
	}
	if !strings.HasPrefix(marker, FailureAnalysisResultMarker) {
		t.Fatalf("marker = %q", marker)
	}
	if strings.Contains(marker, `"ai_summary"`) {
		t.Fatalf("result JSON was not encoded: %q", marker)
	}
}

func TestParseFailureAnalysisResultUsesLastValidMarker(t *testing.T) {
	marker := framedResult(t, testResult())
	got, err := ParseFailureAnalysisResult("PROW_AI_RESULT_B64:not-base64\n" + marker + marker + "PROW_AI_RESULT_B64:still-not-base64\ntrailing log\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, testResult()) {
		t.Fatalf("result = %+v", got)
	}
}

func TestParseFailureAnalysisResultRejectsConflictingMarkers(t *testing.T) {
	first := testResult()
	second := testResult()
	second.Summary = &models.AISummary{Summary: "different"}
	if _, err := ParseFailureAnalysisResult(framedResult(t, first) + framedResult(t, second)); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("ParseFailureAnalysisResult error = %v", err)
	}
}

func TestParseFailureAnalysisResultRejectsInvalidMarkers(t *testing.T) {
	unknown, err := json.Marshal(map[string]any{"ai_summary": map[string]any{"summary": "ok"}, "unknown": true})
	if err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxFailureAnalysisResultBytes)+1)
	for name, raw := range map[string]string{
		"missing":         "runtime log only\n",
		"empty":           FailureAnalysisResultMarker + "\n",
		"malformed":       FailureAnalysisResultMarker + "not-base64\n",
		"oversized":       FailureAnalysisResultMarker + oversized + "\n",
		"malformed JSON":  FailureAnalysisResultMarker + base64.StdEncoding.EncodeToString([]byte("not json")) + "\n",
		"unknown field":   FailureAnalysisResultMarker + base64.StdEncoding.EncodeToString(unknown) + "\n",
		"missing summary": FailureAnalysisResultMarker + base64.StdEncoding.EncodeToString([]byte(`{"ai_analysis":{}}`)) + "\n",
		"blank summary":   FailureAnalysisResultMarker + base64.StdEncoding.EncodeToString([]byte(`{"ai_summary":{"summary":"   "}}`)) + "\n",
		"multiple JSON":   FailureAnalysisResultMarker + base64.StdEncoding.EncodeToString([]byte(`{"ai_summary":{"summary":"ok"}} {}`)) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFailureAnalysisResult(raw); err == nil {
				t.Fatalf("ParseFailureAnalysisResult(%q) succeeded", raw)
			}
		})
	}
}

func TestWriteFailureAnalysisResultRejectsInvalidResult(t *testing.T) {
	for _, result := range []ai.FailureAnalysisResult{
		{},
		{Summary: &models.AISummary{}},
		{Summary: &models.AISummary{Summary: "   "}},
		{Summary: &models.AISummary{Summary: strings.Repeat("x", MaxFailureAnalysisResultBytes)}},
	} {
		if err := WriteFailureAnalysisResult(&bytes.Buffer{}, result); err == nil {
			t.Fatalf("WriteFailureAnalysisResult(%+v) succeeded", result)
		}
	}
}
