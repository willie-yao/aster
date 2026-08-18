package analysisexecutor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
)

func TestParseOpenCodeTelemetrySanitizesSuccessfulUsage(t *testing.T) {
	raw := []byte(`[{
		"info":{"role":"assistant","cost":0.125,"tokens":{"input":100,"output":25,"cache":{"read":40}},"private":"secret prompt"},
		"parts":[
			{"type":"step-start","snapshot":"private source"},
			{"type":"step-finish","cost":0.125,"tokens":{"input":100,"output":25,"reasoning":7,"cache":{"read":40}}},
			{"type":"tool","tool":"read","state":{"status":"completed","output":"artifact secret"}},
			{"type":"tool","tool":"bash","state":{"status":"error","error":"Permission denied by policy","input":{"command":"secret"}}}
		]
	}]`)
	usage, telemetry, err := parseOpenCodeTelemetry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || usage.ModelRequests != 1 || usage.InputTokens != 140 || usage.CachedInputTokens != 40 || usage.OutputTokens != 25 || usage.ReasoningTokens != 7 || !usage.CostAvailable || usage.CostUSD != "0.12500000" {
		t.Fatalf("usage=%+v", usage)
	}
	if !telemetry.Available || telemetry.ProviderRequests != 1 || !telemetry.ProviderRequestsKnown || telemetry.StepsUsed != 1 || telemetry.ToolFailureCount != 1 || telemetry.DeniedToolCount != 1 || len(telemetry.Tools) != 2 {
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
		{"info":{"role":"assistant","cost":0,"tokens":{"input":12,"output":1,"cache":{"read":3}},"error":{"name":"ContextOverflowError","data":{"message":"context full"}}},"parts":[{"type":"step-start"},{"type":"step-finish","cost":0,"tokens":{"input":12,"output":1,"cache":{"read":3}}}]}
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

func TestParseOpenCodeTelemetryRejectsMalformedPayloads(t *testing.T) {
	for name, raw := range map[string][]byte{
		"malformed": []byte(`[{`),
		"trailing":  []byte(`[] {}`),
	} {
		t.Run(name, func(t *testing.T) {
			usage, telemetry, err := parseOpenCodeTelemetry(raw)
			if err == nil || usage.Available || telemetry.Available || usage.Status != agentanalysis.WorkspaceTelemetryMalformed {
				t.Fatalf("usage=%+v telemetry=%+v err=%v", usage, telemetry, err)
			}
		})
	}
}

func TestParseOpenCodeTelemetryAcceptsLongToolErrorsWithoutRetainingThem(t *testing.T) {
	errorText := strings.Repeat("private provider detail ", 40) + "permission denied"
	raw := []byte(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"step-finish","cost":0.1,"tokens":{"input":1,"output":1,"cache":{"read":0}}},{"type":"tool","tool":"read","state":{"status":"error","error":` + strconv.Quote(errorText) + `}}]}]`)
	usage, telemetry, err := parseOpenCodeTelemetry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || !telemetry.Available || telemetry.ToolFailureCount != 1 || telemetry.DeniedToolCount != 1 {
		t.Fatalf("usage=%+v telemetry=%+v", usage, telemetry)
	}
	encoded, err := json.Marshal(telemetry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), errorText) || strings.Contains(string(encoded), "private provider detail") {
		t.Fatal("sanitized telemetry retained the tool error")
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
	if usage.ModelRequests != 2 || usage.InputTokens != 112 || usage.CachedInputTokens != 12 || usage.OutputTokens != 12 || usage.CostUSD != "0.30000000" || telemetry.StepsUsed != 2 {
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
		errorJSON    string
		contextLimit bool
	}{
		{name: "context overflow", errorJSON: `{"name":"ContextOverflowError","data":{"message":"context full"}}`, contextLimit: true},
		{name: "provider failure", errorJSON: `{"name":"APIError","data":{"message":"provider failed","isRetryable":true}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte(`[{"info":{"role":"assistant","error":` + test.errorJSON + `},"parts":[{"type":"step-start"},{"type":"tool","tool":"read","state":{"status":"completed"}}]}]`)
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

func TestParseOpenCodeTelemetryClassifiesBoundedEvidenceTools(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	sourceDir := filepath.Join(workDir, agentanalysis.WorkspaceSourceDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(artifactDir, "failure.log")
	sourcePath := filepath.Join(sourceDir, "main.go")
	if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`[{
		"info":{"role":"assistant"},"parts":[
		{"type":"step-start"},
		{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},
		{"type":"tool","tool":"grep","state":{"status":"completed","input":{"path":%[2]q},"output":%[3]q,"metadata":{"matches":1}}},
		{"type":"tool","tool":"write","state":{"status":"error","error":"Permission denied by policy","input":{"filePath":"result/out"}}},
		{"type":"tool","tool":"StructuredOutput","state":{"status":"completed","input":{}}},
		{"type":"step-finish","cost":0.1,"tokens":{"input":1,"output":1,"cache":{"read":0}}}
	]}]`, artifactPath, sourcePath, sourcePath+":\n  Line 1: package main"))
	_, _, facts, err := parseOpenCodeTelemetryForWorkspace(raw, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if facts.ArtifactToolCalls != 1 || facts.SourceToolCalls != 1 || facts.NonStructuredToolCalls != 3 || facts.StructuredOutputCalls != 1 {
		t.Fatalf("facts=%+v", facts)
	}
	wantHandles := []agentanalysis.WorkspaceEvidenceHandle{
		{ID: "artifact-001", Root: agentanalysis.WorkspaceArtifactsDir, Path: "failure.log", LineStart: 1, LineEnd: 1},
		{ID: "source-001", Root: agentanalysis.WorkspaceSourceDir, Path: "main.go", LineStart: 1, LineEnd: 1},
	}
	if !slices.Equal(facts.EvidenceHandles, wantHandles) {
		t.Fatalf("handles=%+v want=%+v", facts.EvidenceHandles, wantHandles)
	}
}

func TestParseOpenCodeTelemetryPreservesUsageWithInvalidOptionalRange(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	sourceDir := filepath.Join(workDir, agentanalysis.WorkspaceSourceDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(artifactDir, "failure.log")
	sourcePath := filepath.Join(sourceDir, "main.go")
	if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`[{
		"info":{"role":"assistant"},"parts":[
		{"type":"step-start"},
		{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},
		{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[2]q},"metadata":{"display":{"type":"file","path":%[2]q,"lineStart":2,"lineEnd":2}}}},
		{"type":"step-finish","cost":0.1,"tokens":{"input":3,"output":2,"cache":{"read":1}}}
	]}]`, artifactPath, sourcePath))
	usage, telemetry, facts, err := parseOpenCodeTelemetryForWorkspace(raw, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || usage.InputTokens != 4 || usage.CachedInputTokens != 1 || usage.OutputTokens != 2 || !telemetry.Available || facts.ArtifactToolCalls != 1 || facts.SourceToolCalls != 1 || len(facts.EvidenceHandles) != 1 || facts.EvidenceHandles[0].Root != agentanalysis.WorkspaceArtifactsDir {
		t.Fatalf("usage=%+v telemetry=%+v facts=%+v", usage, telemetry, facts)
	}
	if telemetry.EvidenceHandles.Status != agentanalysis.WorkspaceEvidenceHandlesAcceptedWithWarnings || telemetry.EvidenceHandles.AcceptedArtifactHandleCount != 1 || telemetry.EvidenceHandles.AcceptedSourceHandleCount != 0 || telemetry.EvidenceHandles.DroppedRangeCount != 1 || !slices.Contains(telemetry.EvidenceHandles.Codes, agentanalysis.WorkspaceEvidenceRangeLineInvalid) {
		t.Fatalf("diagnostics=%+v", telemetry.EvidenceHandles)
	}
}

func TestParseOpenCodeTelemetryRejectsUnsafeEvidenceWithoutDiscardingUsage(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(artifactDir, "failure.log")
	if err := os.WriteFile(artifactPath, []byte("failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`[{
		"info":{"role":"assistant"},"parts":[
		{"type":"step-start"},
		{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":%[1]q},"metadata":{"display":{"type":"file","path":%[1]q,"lineStart":1,"lineEnd":1}}}},
		{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":"/etc/passwd"},"metadata":{"display":{"type":"file","path":"/etc/passwd","lineStart":1,"lineEnd":1}}}},
		{"type":"step-finish","cost":0.1,"tokens":{"input":3,"output":2,"cache":{"read":1}}}
	]}]`, artifactPath))
	usage, telemetry, facts, err := parseOpenCodeTelemetryForWorkspace(raw, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || !telemetry.Available || telemetry.EvidenceHandles.Status != agentanalysis.WorkspaceEvidenceHandlesRejected || !slices.Equal(telemetry.EvidenceHandles.Codes, []string{agentanalysis.WorkspaceEvidenceRangePathInvalid}) || len(facts.EvidenceHandles) != 0 {
		t.Fatalf("usage=%+v telemetry=%+v facts=%+v", usage, telemetry, facts)
	}
	if err := validateOpenCodeEvidencePhase(facts, nil); err == nil {
		t.Fatal("unsafe evidence was allowed to reach finalization")
	}
}

func TestParseOpenCodeTelemetryCapsHighCardinalityDirectoryGrep(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(artifactDir, "many.log")
	var content, output strings.Builder
	fmt.Fprintf(&output, "%s:\n", path)
	for line := 1; line <= 513; line++ {
		fmt.Fprintf(&content, "match-%03d\n", line)
		fmt.Fprintf(&output, "  Line %d: match-%03d\n", line, line)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`[{
		"info":{"role":"assistant"},"parts":[
		{"type":"step-start"},
		{"type":"tool","tool":"grep","state":{"status":"completed","input":{"path":%q},"output":%q,"metadata":{"matches":%d}}},
		{"type":"step-finish","cost":0.1,"tokens":{"input":3,"output":2,"cache":{"read":1}}}
	]}]`, artifactDir, output.String(), 513))
	usage, telemetry, facts, err := parseOpenCodeTelemetryForWorkspace(raw, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || facts.ArtifactToolCalls != 1 || len(facts.EvidenceHandles) != 64 || telemetry.EvidenceHandles.Status != agentanalysis.WorkspaceEvidenceHandlesAcceptedWithWarnings || !telemetry.EvidenceHandles.Truncated || !slices.Contains(telemetry.EvidenceHandles.Codes, agentanalysis.WorkspaceEvidenceRangeOverflow) || !slices.Contains(telemetry.EvidenceHandles.Codes, agentanalysis.WorkspaceEvidenceHandleTruncated) {
		t.Fatalf("usage=%+v telemetry=%+v facts=%+v", usage, telemetry, facts)
	}
}

func TestParseOpenCodeTelemetryBoundsCompactHighCardinalityGrep(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(artifactDir, "compact.log")
	const matches = 20_000
	var content, output strings.Builder
	fmt.Fprintf(&output, "%s:\n", path)
	for line := 1; line <= matches; line++ {
		fmt.Fprintln(&content, "x")
		fmt.Fprintf(&output, "  Line %d: x\n", line)
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"grep","state":{"status":"completed","input":{"path":%q},"output":%q,"metadata":{"matches":%d}}},{"type":"step-finish","cost":0.1,"tokens":{"input":3,"output":2,"cache":{"read":1}}}]}]`, artifactDir, output.String(), matches))
	usage, telemetry, facts, err := parseOpenCodeTelemetryForWorkspace(raw, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || len(facts.EvidenceRanges) != 512 || facts.EvidenceRetainedByRoot[agentanalysis.WorkspaceArtifactsDir] != 512 || len(facts.EvidenceHandles) != 64 || telemetry.EvidenceHandles.ObservedRangeCount != 513 || telemetry.EvidenceHandles.DroppedRangeCount != 513 || !telemetry.EvidenceHandles.Truncated || !slices.Contains(telemetry.EvidenceHandles.Codes, agentanalysis.WorkspaceEvidenceRangeOverflow) || !slices.Contains(telemetry.EvidenceHandles.Codes, agentanalysis.WorkspaceEvidenceHandleTruncated) {
		t.Fatalf("usage=%+v telemetry=%+v facts=%+v", usage, telemetry, facts)
	}
}

func TestParseOpenCodeTelemetryRejectsUnsafeGrepPathAfterRetainedCap(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(artifactDir, "many.log")
	var content, output strings.Builder
	fmt.Fprintf(&output, "%s:\n", path)
	for line := 1; line <= 513; line++ {
		fmt.Fprintln(&content, "match")
		fmt.Fprintf(&output, "  Line %d: match\n", line)
	}
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(artifactDir, "escape.log")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&output, "%s:\n", escape)
	fmt.Fprintln(&output, "  Line 1: root")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"grep","state":{"status":"completed","input":{"path":%q},"output":%q,"metadata":{"matches":514}}},{"type":"step-finish","cost":0.1,"tokens":{"input":3,"output":2,"cache":{"read":1}}}]}]`, artifactDir, output.String()))
	usage, telemetry, facts, err := parseOpenCodeTelemetryForWorkspace(raw, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || !telemetry.Available || len(facts.EvidenceRanges) != 512 || telemetry.EvidenceHandles.Status != agentanalysis.WorkspaceEvidenceHandlesRejected || !slices.Contains(telemetry.EvidenceHandles.Codes, agentanalysis.WorkspaceEvidenceRangePathInvalid) {
		t.Fatalf("usage=%+v telemetry=%+v facts=%+v", usage, telemetry, facts)
	}
	if err := validateOpenCodeEvidencePhase(facts, nil); err == nil {
		t.Fatal("unsafe grep path was allowed to reach finalization")
	}
}

func TestParseOpenCodeTelemetryDoesNotCountZeroMatchGrep(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"grep","state":{"status":"completed","input":{"path":%q},"metadata":{"matches":0}}},{"type":"step-finish","cost":0.1,"tokens":{"input":1,"output":1,"cache":{"read":0}}}]}]`, artifactDir))
	_, _, facts, err := parseOpenCodeTelemetryForWorkspace(raw, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if facts.ArtifactToolCalls != 0 {
		t.Fatalf("facts=%+v", facts)
	}
}

func TestParseOpenCodeTelemetryDoesNotCountGrepWithoutRanges(t *testing.T) {
	workDir := t.TempDir()
	artifactDir := filepath.Join(workDir, agentanalysis.WorkspaceArtifactsDir)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{agentanalysis.WorkspaceArtifactsDir, artifactDir} {
		raw := []byte(fmt.Sprintf(`[{"info":{"role":"assistant"},"parts":[{"type":"step-start"},{"type":"tool","tool":"grep","state":{"status":"completed","input":{"path":%q},"metadata":{"matches":1}}},{"type":"step-finish","cost":0.1,"tokens":{"input":1,"output":1,"cache":{"read":0}}}]}]`, candidate))
		_, _, facts, err := parseOpenCodeTelemetryForWorkspace(raw, workDir)
		if err != nil {
			t.Fatal(err)
		}
		if facts.ArtifactToolCalls != 0 || facts.EvidenceDiagnostics.Status != agentanalysis.WorkspaceEvidenceHandlesRejected {
			t.Fatalf("candidate=%q facts=%+v", candidate, facts)
		}
	}
}

func TestParseOpenCodeTelemetryTreatsZeroTokenUsageAsUnavailable(t *testing.T) {
	raw := []byte(`[{
		"info":{"role":"assistant","cost":0,"tokens":{"input":0,"output":0,"cache":{"read":0}}},
		"parts":[{"type":"step-start"},{"type":"step-finish","cost":0,"tokens":{"input":0,"output":0,"cache":{"read":0}}}]
	}]`)
	usage, telemetry, err := parseOpenCodeTelemetry(raw)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Available || usage.Status != agentanalysis.WorkspaceTelemetryUnavailable || usage.ModelRequests != 0 || telemetry.ProviderRequests != 1 || !telemetry.ProviderRequestsKnown {
		t.Fatalf("usage=%+v telemetry=%+v", usage, telemetry)
	}
}
