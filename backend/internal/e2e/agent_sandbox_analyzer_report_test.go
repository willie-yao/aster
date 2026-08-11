package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentSandboxAnalyzerReport(t *testing.T) {
	inprocess, sandbox := validAgentSandboxAnalyzerReportRecords(t)
	dir := t.TempDir()
	inprocessPath := filepath.Join(dir, "inprocess.jsonl")
	sandboxPath := filepath.Join(dir, "sandbox.jsonl")
	blindPackets := filepath.Join(dir, "blind-packets.json")
	blindMap := filepath.Join(dir, "blind-map.json")
	writeReportJSONL(t, inprocessPath, inprocess)
	writeReportJSONL(t, sandboxPath, sandbox)
	command := exec.Command(
		"python3", filepath.Join("..", "..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath,
		"--sandbox", sandboxPath,
		"--repo", filepath.Join("..", "..", ".."),
		"--holdout-case", "case",
		"--expected-pairs", "3",
		"--blind-packets", blindPackets,
		"--blind-map", blindMap,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("report: %v: %s", err, output)
	}
	var report map[string]any
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	criteria := report["criteria"].(map[string]any)
	if criteria["recommendation"] != "continue_experiment_not_replacement" || criteria["automatic_quality_passed"] != true || criteria["quality_passed"] != false || criteria["simplicity_passed"] != true {
		t.Fatalf("criteria = %+v", criteria)
	}
	for _, sensitive := range []string{"INPROCESS_PRIVATE_ROOT_CAUSE", "SANDBOX_PRIVATE_ROOT_CAUSE", "PRIVATE_ARTIFACT_QUOTE"} {
		if strings.Contains(string(output), sensitive) {
			t.Fatalf("content-free report contains %q", sensitive)
		}
	}
	packets, err := os.ReadFile(blindPackets)
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := os.ReadFile(blindMap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(packets), "INPROCESS_PRIVATE_ROOT_CAUSE") || !strings.Contains(string(packets), "SANDBOX_PRIVATE_ROOT_CAUSE") {
		t.Fatalf("blind packets omit analysis content: %s", packets)
	}
	if strings.Contains(string(packets), "agent_sandbox") || strings.Contains(string(packets), "inprocess") {
		t.Fatalf("blind packets reveal runtime mapping: %s", packets)
	}
	if !strings.Contains(string(mapping), "agent_sandbox") || !strings.Contains(string(mapping), "inprocess") {
		t.Fatalf("blind map omits runtime mapping: %s", mapping)
	}
	assertBlindPacketSchemasMatch(t, packets)

	var mapDoc struct {
		PacketSetSHA256 string `json:"packet_set_sha256"`
		Mapping         []struct {
			PacketID string `json:"packet_id"`
			Arm      string `json:"arm"`
		} `json:"mapping"`
	}
	if err := json.Unmarshal(mapping, &mapDoc); err != nil {
		t.Fatal(err)
	}
	scoresPath := filepath.Join(dir, "blind-scores.json")
	scores := map[string]any{
		"version": 1, "packet_set_sha256": mapDoc.PacketSetSHA256, "rubric_version": 1, "score_max": 10,
		"dimensions": benchmarkHumanScoreDimensions,
		"scores": func() []map[string]any {
			out := make([]map[string]any, 0, len(mapDoc.Mapping))
			for _, item := range mapDoc.Mapping {
				values := map[string]int{}
				for _, dimension := range benchmarkHumanScoreDimensions {
					values[dimension] = 2
				}
				out = append(out, map[string]any{"packet_id": item.PacketID, "arm": item.Arm, "scores": values})
			}
			return out
		}(),
	}
	data, err := json.Marshal(scores)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scoresPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	scoredCommand := exec.Command(
		"python3", filepath.Join("..", "..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath,
		"--sandbox", sandboxPath,
		"--repo", filepath.Join("..", "..", ".."),
		"--holdout-case", "case",
		"--expected-pairs", "3",
		"--blind-map-input", blindMap,
		"--blind-scores", scoresPath,
	)
	scoredOutput, err := scoredCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("scored report: %v: %s", err, scoredOutput)
	}
	if err := json.Unmarshal(scoredOutput, &report); err != nil {
		t.Fatal(err)
	}
	criteria = report["criteria"].(map[string]any)
	if criteria["blind_quality_complete"] != true || criteria["blind_quality_passed"] != true || criteria["quality_passed"] != true {
		t.Fatalf("scored criteria = %+v", criteria)
	}

	var tampered map[string]any
	if err := json.Unmarshal(mapping, &tampered); err != nil {
		t.Fatal(err)
	}
	entries := tampered["mapping"].([]any)
	first := entries[0].(map[string]any)
	second := entries[1].(map[string]any)
	first["runtime"], second["runtime"] = second["runtime"], first["runtime"]
	first["analysis_sha256"], second["analysis_sha256"] = second["analysis_sha256"], first["analysis_sha256"]
	tamperedPath := filepath.Join(dir, "tampered-map.json")
	tamperedData, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tamperedPath, tamperedData, 0o600); err != nil {
		t.Fatal(err)
	}
	tamperedCommand := exec.Command(
		"python3", filepath.Join("..", "..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath,
		"--sandbox", sandboxPath,
		"--repo", filepath.Join("..", "..", ".."),
		"--blind-map-input", tamperedPath,
		"--blind-scores", scoresPath,
	)
	tamperedOutput, err := tamperedCommand.CombinedOutput()
	if err == nil || !strings.Contains(string(tamperedOutput), "packet_set_sha256 does not match") {
		t.Fatalf("tampered map error = %v, output = %s", err, tamperedOutput)
	}
}

func TestAgentSandboxAnalyzerReportRejectsIdentityMismatch(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func([]map[string]any, []map[string]any)
		expected string
	}{
		{name: "fixture", mutate: func(_ []map[string]any, sandbox []map[string]any) {
			sandbox[0]["fixture_sha256"] = strings.Repeat("0", 64)
		}, expected: "differs in fixture_sha256"},
		{name: "api mode", mutate: func(_ []map[string]any, sandbox []map[string]any) { sandbox[0]["api_mode"] = "responses" }, expected: "sandbox line 1 must use chat_completions"},
		{name: "evidence", mutate: func(inprocess []map[string]any, _ []map[string]any) {
			inprocess[0]["evidence_condition"] = "kueue-oracle-v1"
		}, expected: "inprocess line 1 must use fixture-v1 evidence"},
		{name: "model label", mutate: func(_ []map[string]any, sandbox []map[string]any) { sandbox[0]["model_label"] = "model-b" }, expected: "differs in model_label"},
		{name: "rubric version", mutate: func(_ []map[string]any, sandbox []map[string]any) { sandbox[0]["human_score_rubric_version"] = 2 }, expected: "differs in human_score_rubric_version"},
		{name: "rubric max", mutate: func(_ []map[string]any, sandbox []map[string]any) { sandbox[0]["human_score_max"] = 8 }, expected: "differs in human_score_max"},
		{name: "rubric dimensions", mutate: func(_ []map[string]any, sandbox []map[string]any) {
			sandbox[0]["human_score_dimensions"] = []string{"diagnosis"}
		}, expected: "differs in human_score_dimensions"},
		{name: "signals", mutate: func(_ []map[string]any, sandbox []map[string]any) { sandbox[0]["signal_total"] = 6 }, expected: "differs in signal_total"},
		{name: "diagnosis signals", mutate: func(_ []map[string]any, sandbox []map[string]any) { sandbox[0]["diagnosis_signal_total"] = 4 }, expected: "differs in diagnosis_signal_total"},
		{name: "forbidden checks", mutate: func(_ []map[string]any, sandbox []map[string]any) { sandbox[0]["forbidden_checks_total"] = 1 }, expected: "differs in forbidden_checks_total"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inprocess, sandbox := validAgentSandboxAnalyzerReportRecords(t)
			test.mutate(inprocess, sandbox)
			dir := t.TempDir()
			inprocessPath := filepath.Join(dir, "inprocess.jsonl")
			sandboxPath := filepath.Join(dir, "sandbox.jsonl")
			writeReportJSONL(t, inprocessPath, inprocess)
			writeReportJSONL(t, sandboxPath, sandbox)
			command := exec.Command(
				"python3", filepath.Join("..", "..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
				"--inprocess", inprocessPath,
				"--sandbox", sandboxPath,
				"--repo", filepath.Join("..", "..", ".."),
			)
			output, err := command.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.expected) {
				t.Fatalf("report error = %v, output = %s", err, output)
			}
		})
	}
}

func validAgentSandboxAnalyzerReportRecords(t *testing.T) ([]map[string]any, []map[string]any) {
	t.Helper()
	var inprocess []map[string]any
	var sandbox []map[string]any
	for repetition := 1; repetition <= 3; repetition++ {
		common := map[string]any{
			"case_id": "case", "stable_id": "0123456789abcdef0123", "repetition": repetition,
			"model_label": "model-a", "engine_commit": strings.Repeat("a", 40),
			"fixture_sha256": strings.Repeat("b", 64), "baseline_consumer_commit": strings.Repeat("c", 40),
			"baseline_prompt_sha256": strings.Repeat("d", 64), "project_sha256": strings.Repeat("e", 64),
			"source_revision": strings.Repeat("f", 40), "provider_path": "provider/model", "transport_id": "transport-v1",
			"api_mode": "chat_completions", "evidence_condition": "fixture-v1",
			"job_name": "job", "build_id": "1", "test_name": "test", "test_source": "build",
			"human_score_rubric_version": 1, "human_score_max": 10, "human_score_dimensions": benchmarkHumanScoreDimensions,
			"elapsed_ms": 1000 + repetition, "signal_hits": 4, "signal_total": 5,
			"diagnosis_signal_hits": 3, "diagnosis_signal_total": 3,
			"transient_classification_correct": true, "forbidden_checks_passed": 0, "forbidden_checks_total": 0,
		}
		left := cloneReportRecord(common)
		left["arm"] = "baseline"
		left["trial_status"] = "usable"
		left["usable"] = true
		left["summary"] = "private summary"
		left["root_cause"] = "INPROCESS_PRIVATE_ROOT_CAUSE"
		left["suggested_fix"] = "private fix"
		left["severity"] = "High"
		left["is_transient"] = false
		left["evidence_citations"] = []map[string]any{{"path": "artifact.log", "quote": "PRIVATE_ARTIFACT_QUOTE"}}
		left["file_links"] = map[string]string{"pkg/file.go": "private"}
		left["trace"] = map[string]any{"model_requests": 2, "provider_attempts": 2, "input_tokens": 100, "cached_input_tokens": 0, "output_tokens": 20}
		inprocess = append(inprocess, left)

		right := cloneReportRecord(common)
		right["version"] = 1
		right["runtime"] = "agent-sandbox-opencode"
		right["arm"] = "arm-b"
		right["status"] = "succeeded"
		right["analysis_valid"] = true
		right["finalization_valid"] = true
		right["cleanup_completed"] = true
		right["source_verified"] = true
		right["artifact_citation_count"] = 1
		right["source_citation_count"] = 1
		right["summary"] = "private summary"
		right["root_cause"] = "SANDBOX_PRIVATE_ROOT_CAUSE"
		right["suggested_fix"] = "private fix"
		right["severity"] = "High"
		right["is_transient"] = false
		right["evidence_citations"] = []map[string]any{{"path": "artifact.log", "line_start": 1, "line_end": 1}}
		right["source_citations"] = []map[string]any{{"path": "pkg/file.go", "line_start": 1, "line_end": 1, "verified": true}}
		right["unresolved_details"] = []string{"private unknown"}
		right["token_usage_available"] = true
		right["cost_available"] = false
		right["usage_status"] = "tokens_reported_cost_unavailable"
		right["model_requests"] = 1
		right["input_tokens"] = 50
		right["cached_input_tokens"] = 0
		right["output_tokens"] = 10
		sandbox = append(sandbox, right)
	}
	return inprocess, sandbox
}

func assertBlindPacketSchemasMatch(t *testing.T, data []byte) {
	t.Helper()
	var document struct {
		Packets []map[string]any `json:"packets"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	byPacket := map[string][]map[string]any{}
	for _, packet := range document.Packets {
		packetID, _ := packet["packet_id"].(string)
		byPacket[packetID] = append(byPacket[packetID], packet)
		citations, ok := packet["evidence_citations"].([]any)
		if !ok || len(citations) == 0 {
			t.Fatalf("packet %s evidence citations are not populated", packetID)
		}
		citation, ok := citations[0].(map[string]any)
		if !ok || len(citation) != 3 {
			t.Fatalf("packet %s citation schema = %+v", packetID, citations[0])
		}
		for _, key := range []string{"path", "line_start", "line_end"} {
			if _, ok := citation[key]; !ok {
				t.Fatalf("packet %s citation omits %s", packetID, key)
			}
		}
		source, ok := packet["source_references"].([]any)
		if !ok || len(source) == 0 {
			t.Fatalf("packet %s source references are not populated", packetID)
		}
	}
	for packetID, packets := range byPacket {
		if len(packets) != 2 {
			t.Fatalf("packet %s has %d arms", packetID, len(packets))
		}
		left, err := json.Marshal(schemaShape(packets[0]))
		if err != nil {
			t.Fatal(err)
		}
		right, err := json.Marshal(schemaShape(packets[1]))
		if err != nil {
			t.Fatal(err)
		}
		if string(left) != string(right) {
			t.Fatalf("packet %s schemas differ:\n%s\n%s", packetID, left, right)
		}
	}
}

func schemaShape(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range typed {
			out[key] = schemaShape(item)
		}
		return out
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

func writeReportJSONL(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
}
