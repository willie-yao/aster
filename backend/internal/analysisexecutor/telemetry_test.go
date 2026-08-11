package analysisexecutor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
)

func TestParseOpenCodeTelemetrySanitizesSuccessfulUsage(t *testing.T) {
	raw := []byte(`[{
		"info":{"role":"assistant","cost":0.125,"tokens":{"input":100,"output":25,"cache":{"read":40}},"private":"secret prompt"},
		"parts":[
			{"type":"step-start","snapshot":"private source"},
			{"type":"step-finish","cost":0.125,"tokens":{"input":100,"output":25,"cache":{"read":40}}},
			{"type":"tool","tool":"read","state":{"status":"completed","output":"artifact secret"}},
			{"type":"tool","tool":"bash","state":{"status":"error","error":"Permission denied by policy","input":{"command":"secret"}}}
		]
	}]`)
	usage, telemetry, err := parseOpenCodeTelemetry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || usage.ModelRequests != 1 || usage.InputTokens != 100 || usage.CachedInputTokens != 40 || usage.OutputTokens != 25 || !usage.CostAvailable || usage.CostUSD != "0.12500000" {
		t.Fatalf("usage=%+v", usage)
	}
	if !telemetry.Available || telemetry.StepsUsed != 1 || telemetry.ToolFailureCount != 1 || telemetry.DeniedToolCount != 1 || len(telemetry.Tools) != 2 {
		t.Fatalf("telemetry=%+v", telemetry)
	}
	encoded, err := json.Marshal(struct {
		Usage     agentanalysis.WorkspaceUsage
		Telemetry agentanalysis.WorkspaceOpenCodeTelemetry
	}{usage, telemetry})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret prompt", "private source", "artifact secret", "command"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("sanitized telemetry leaked %q: %s", secret, encoded)
		}
	}
}

func TestParseOpenCodeTelemetryRecordsStructuredAndContextErrors(t *testing.T) {
	raw := []byte(`[
		{"info":{"role":"assistant","cost":0,"tokens":{"input":10,"output":2,"cache":{"read":0}},"error":{"name":"StructuredOutputError"}},"parts":[{"type":"step-start"},{"type":"step-finish","cost":0,"tokens":{"input":10,"output":2,"cache":{"read":0}}}]},
		{"info":{"role":"assistant","cost":0,"tokens":{"input":12,"output":1,"cache":{"read":3}},"error":{"name":"ContextOverflowError"}},"parts":[{"type":"step-start"},{"type":"step-finish","cost":0,"tokens":{"input":12,"output":1,"cache":{"read":3}}}]}
	]`)
	usage, telemetry, err := parseOpenCodeTelemetry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if usage.CostAvailable || usage.CostUSD != "" {
		t.Fatalf("zero unpriced cost was treated as available: %+v", usage)
	}
	if telemetry.StructuredOutputErrors != 1 || !telemetry.ContextLimit || !telemetry.StructuredOutputRetriesKnown || telemetry.StructuredOutputRetries != 0 || telemetry.StepsUsed != 2 {
		t.Fatalf("telemetry=%+v", telemetry)
	}
}

func TestParseOpenCodeTelemetryRejectsMalformedAndOversizedFields(t *testing.T) {
	for name, raw := range map[string][]byte{
		"malformed":       []byte(`[{`),
		"trailing":        []byte(`[] {}`),
		"oversized error": []byte(`[{"info":{"role":"assistant","cost":0,"tokens":{"input":1,"output":1,"cache":{"read":0}}},"parts":[{"type":"step-start"},{"type":"step-finish","cost":0.1,"tokens":{"input":1,"output":1,"cache":{"read":0}}},{"type":"tool","tool":"read","state":{"status":"error","error":"` + strings.Repeat("x", maxOpenCodeFieldBytes+1) + `"}}]}]`),
	} {
		t.Run(name, func(t *testing.T) {
			usage, telemetry, err := parseOpenCodeTelemetry(raw)
			if err == nil || usage.Available || telemetry.Available || usage.Status != agentanalysis.WorkspaceTelemetryMalformed {
				t.Fatalf("usage=%+v telemetry=%+v err=%v", usage, telemetry, err)
			}
		})
	}
}

func TestBoundedCaptureReportsTruncationWithoutRetainingData(t *testing.T) {
	capture := newBoundedCapture(4)
	if n, err := capture.Write([]byte("sensitive")); err != nil || n != len("sensitive") || !capture.Truncated() {
		t.Fatalf("n=%d truncated=%v err=%v", n, capture.Truncated(), err)
	}
}

func TestTelemetryStatusDistinguishesUnavailableMalformedAndTruncated(t *testing.T) {
	if got := telemetryStatusForError(assertTelemetryError("network unavailable")); got != agentanalysis.WorkspaceTelemetryUnavailable {
		t.Fatalf("unavailable status = %q", got)
	}
	if got := telemetryStatusForError(assertTelemetryError("decode telemetry")); got != agentanalysis.WorkspaceTelemetryMalformed {
		t.Fatalf("malformed status = %q", got)
	}
	if got := telemetryStatusForError(assertTelemetryError("response exceeded the bound")); got != agentanalysis.WorkspaceTelemetryTruncated {
		t.Fatalf("truncated status = %q", got)
	}
}

type assertTelemetryError string

func (e assertTelemetryError) Error() string { return string(e) }

func TestParseOpenCodeTelemetryAggregatesMultipleSteps(t *testing.T) {
	raw := []byte(`[{"info":{"role":"assistant"},"parts":[
		{"type":"step-start"},
		{"type":"step-finish","cost":0.1,"tokens":{"input":30,"output":4,"cache":{"read":2}}},
		{"type":"step-start"},
		{"type":"step-finish","cost":0.2,"tokens":{"input":70,"output":8,"cache":{"read":10}}}
	]}]`)
	usage, telemetry, err := parseOpenCodeTelemetry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if usage.ModelRequests != 2 || usage.InputTokens != 100 || usage.CachedInputTokens != 12 || usage.OutputTokens != 12 || usage.CostUSD != "0.30000000" || telemetry.StepsUsed != 2 {
		t.Fatalf("usage=%+v telemetry=%+v", usage, telemetry)
	}
}

func TestParseOpenCodeTelemetryRejectsMissingStepUsageFields(t *testing.T) {
	parts := []string{
		`{"type":"step-finish","cost":0.1}`,
		`{"type":"step-finish","cost":0.1,"tokens":{"output":1,"cache":{"read":0}}}`,
		`{"type":"step-finish","cost":0.1,"tokens":{"input":1,"cache":{"read":0}}}`,
		`{"type":"step-finish","cost":0.1,"tokens":{"input":1,"output":1}}`,
		`{"type":"step-finish","cost":0.1,"tokens":{"input":1,"output":1,"cache":{}}}`,
	}
	for _, part := range parts {
		raw := []byte(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},` + part + `]}]`)
		usage, telemetry, err := parseOpenCodeTelemetry(raw)
		if err == nil || usage.Available || telemetry.Available {
			t.Fatalf("part=%s usage=%+v telemetry=%+v err=%v", part, usage, telemetry, err)
		}
	}
}

func TestParseOpenCodeTelemetryPreservesRecognizedIncompleteFailures(t *testing.T) {
	for _, test := range []struct {
		name         string
		errorName    string
		contextLimit bool
	}{
		{name: "context overflow", errorName: "ContextOverflowError", contextLimit: true},
		{name: "provider failure", errorName: "APIError"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(`[{"info":{"role":"assistant","error":{"name":"` + test.errorName + `"}},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed"}}]}]`)
			usage, telemetry, err := parseOpenCodeTelemetry(raw)
			if err != nil {
				t.Fatal(err)
			}
			if usage.Available || usage.Status != agentanalysis.WorkspaceTelemetryUnavailable || !telemetry.Available || telemetry.Status != agentanalysis.WorkspaceTelemetryAvailable || telemetry.StepsUsed != 1 || telemetry.ContextLimit != test.contextLimit || len(telemetry.Tools) != 1 {
				t.Fatalf("usage=%+v telemetry=%+v", usage, telemetry)
			}
		})
	}
}

func TestParseOpenCodeTelemetryPreservesRecoveryAfterIncompleteStep(t *testing.T) {
	raw := []byte(`[
		{"info":{"role":"assistant"},"parts":[{"type":"step-start"}]},
		{"info":{"role":"user"},"parts":[{"type":"compaction","auto":true,"overflow":true}]},
		{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"step-finish","cost":0.2,"tokens":{"input":20,"output":5,"cache":{"read":3}}}]}
	]`)
	usage, telemetry, err := parseOpenCodeTelemetry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Available || usage.ModelRequests != 0 || !telemetry.Available || telemetry.StepsUsed != 2 || !telemetry.ContextLimit {
		t.Fatalf("usage=%+v telemetry=%+v", usage, telemetry)
	}
}

func TestParseOpenCodeTelemetryRejectsUnexplainedIncompleteStep(t *testing.T) {
	raw := []byte(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"}]}]`)
	if _, _, err := parseOpenCodeTelemetry(raw); err == nil {
		t.Fatal("unexplained incomplete step was accepted")
	}
}

func TestParseOpenCodeTelemetryDoesNotAuthorizeUnrelatedIncompleteSteps(t *testing.T) {
	tests := []string{
		`[
			{"info":{"role":"assistant","error":{"name":"StructuredOutputError"}},"parts":[{"type":"step-start"},{"type":"step-finish","cost":0.1,"tokens":{"input":1,"output":1,"cache":{"read":0}}}]},
			{"info":{"role":"assistant"},"parts":[{"type":"step-start"}]}
		]`,
		`[
			{"info":{"role":"assistant"},"parts":[{"type":"step-start"}]},
			{"info":{"role":"assistant","error":{"name":"APIError"}},"parts":[{"type":"step-start"}]}
		]`,
		`[
			{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"step-start"}]},
			{"info":{"role":"user"},"parts":[{"type":"compaction","auto":true,"overflow":true}]}
		]`,
	}
	for _, raw := range tests {
		if _, _, err := parseOpenCodeTelemetry([]byte(raw)); err == nil {
			t.Fatalf("unrelated incomplete step was accepted: %s", raw)
		}
	}
}

func TestParseOpenCodeTelemetryRejectsInvalidMessagePartOwnership(t *testing.T) {
	tests := []string{
		`[{"info":{"role":"user"},"parts":[{"type":"step-start"},{"type":"step-finish","cost":0.1,"tokens":{"input":10,"output":2,"cache":{"read":0}}}]}]`,
		`[{"info":{"role":"user"},"parts":[{"type":"tool","tool":"read","state":{"status":"completed"}}]}]`,
		`[{"info":{"role":"assistant"},"parts":[{"type":"compaction","auto":true,"overflow":true}]}]`,
		`[{"info":{"role":"unknown"},"parts":[]}]`,
		`[{"info":{"role":"user","error":{"name":"APIError"}},"parts":[]}]`,
	}
	for _, raw := range tests {
		if _, _, err := parseOpenCodeTelemetry([]byte(raw)); err == nil {
			t.Fatalf("invalid message ownership was accepted: %s", raw)
		}
	}
}

func TestParseOpenCodeTelemetryValidatesToolStates(t *testing.T) {
	base := func(state string) []byte {
		return []byte(`[{"info":{"role":"assistant"},"parts":[
			{"type":"step-start"},
			{"type":"step-finish","cost":0.1,"tokens":{"input":1,"output":1,"cache":{"read":0}}},
			{"type":"tool","tool":"read","state":` + state + `}
		]}]`)
	}
	for _, state := range []string{`{}`, `{"status":"mystery"}`} {
		if _, _, err := parseOpenCodeTelemetry(base(state)); err == nil {
			t.Fatalf("invalid tool state was accepted: %s", state)
		}
	}
	for _, test := range []struct {
		state    string
		failures int
		denied   int
	}{
		{state: `{"status":"completed"}`},
		{state: `{"status":"error","error":"tool failed"}`, failures: 1},
		{state: `{"status":"error","error":"Permission denied by policy"}`, failures: 1, denied: 1},
		{state: `{"status":"pending"}`, failures: 1},
		{state: `{"status":"running"}`, failures: 1},
	} {
		_, telemetry, err := parseOpenCodeTelemetry(base(test.state))
		if err != nil {
			t.Fatal(err)
		}
		if len(telemetry.Tools) != 1 || telemetry.Tools[0].Failures != test.failures || telemetry.Tools[0].Denied != test.denied || telemetry.ToolFailureCount != test.failures || telemetry.DeniedToolCount != test.denied {
			t.Fatalf("state=%s telemetry=%+v", test.state, telemetry)
		}
	}
}
