package benchmarks

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	references := filepath.Join(dir, "causal-references.json")
	writeTestCausalReferences(t, references, []string{"case"})
	writeReportJSONL(t, inprocessPath, inprocess)
	writeReportJSONL(t, sandboxPath, sandbox)
	command := exec.Command(
		"python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath,
		"--sandbox", sandboxPath,
		"--repo", filepath.Join("..", ".."),
		"--holdout-case", "case",
		"--expected-pairs", "3",
		"--blind-packets", blindPackets,
		"--blind-map", blindMap,
		"--reference-manifest", references,
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
	if criteria["shadow_comparison"] != "insufficient_evidence" || criteria["authoritative_analyzer"] != "inprocess_unchanged" || report["shadow_comparison"] != "insufficient_evidence" {
		t.Fatalf("criteria = %+v", criteria)
	}
	evidenceModes := report["evidence_mode_coverage"].(map[string]any)
	artifactAndSource := evidenceModes[benchmarkEvidenceModeArtifactAndSource].(map[string]any)
	if artifactAndSource["trials"] != float64(3) || report["evidence_modes_complete"] != false {
		t.Fatalf("evidence mode coverage = %+v", evidenceModes)
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
	var packetDoc struct {
		PacketSetSHA256    string `json:"packet_set_sha256"`
		ReferenceSetSHA256 string `json:"reference_set_sha256"`
		Packets            []struct {
			PacketID string `json:"packet_id"`
			Arm      string `json:"arm"`
		} `json:"packets"`
	}
	if err := json.Unmarshal(packets, &packetDoc); err != nil {
		t.Fatal(err)
	}
	assertBlindPacketSchemasMatch(t, packets)

	scoresPath := filepath.Join(dir, "blind-scores.json")
	scores := map[string]any{
		"version": 2, "packet_set_sha256": packetDoc.PacketSetSHA256, "reference_set_sha256": packetDoc.ReferenceSetSHA256, "rubric_version": benchmarkHumanScoreRubricVersion, "score_max": 10,
		"dimensions": benchmarkHumanScoreDimensions, "scoring_timestamp": "2026-08-18T00:00:00Z",
		"scores": func() []map[string]any {
			out := make([]map[string]any, 0, len(packetDoc.Packets))
			for _, item := range packetDoc.Packets {
				values := map[string]int{}
				for _, dimension := range benchmarkHumanScoreDimensions {
					values[dimension] = 2
				}
				out = append(out, map[string]any{
					"packet_id": item.PacketID, "arm": item.Arm, "scores": values,
					"causal_assessment": map[string]any{"alignment": "aligned", "initiating_cause_found": true, "downstream_treated_as_primary": false, "required_chain_coverage": []string{"initiating-cause", "causal-link"}},
				})
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
	freezePath := filepath.Join(dir, "score-freeze.json")
	freezeCommand := exec.Command("python3", filepath.Join("..", "..", "hack", "freeze-agent-sandbox-blind-scores.py"), "--blind-packets", blindPackets, "--blind-scores", scoresPath, "--output", freezePath)
	if output, err := freezeCommand.CombinedOutput(); err != nil {
		t.Fatalf("freeze: %v: %s", err, output)
	}
	for name, mutate := range map[string]func(map[string]any){
		"extra row": func(doc map[string]any) { doc["scores"] = append(doc["scores"].([]any), "bad") },
		"invalid dimension": func(doc map[string]any) {
			doc["scores"].([]any)[0].(map[string]any)["scores"].(map[string]any)["diagnosis"] = float64(3)
		},
		"incomplete assessment": func(doc map[string]any) { delete(doc["scores"].([]any)[0].(map[string]any), "causal_assessment") },
		"unknown chain": func(doc map[string]any) {
			doc["scores"].([]any)[0].(map[string]any)["causal_assessment"].(map[string]any)["required_chain_coverage"] = []any{"unknown"}
		},
		"invalid full credit": func(doc map[string]any) {
			doc["scores"].([]any)[0].(map[string]any)["causal_assessment"].(map[string]any)["alignment"] = "missing"
		},
		"missing scoring timestamp": func(doc map[string]any) { delete(doc, "scoring_timestamp") },
	} {
		t.Run("freeze rejects malformed "+name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			mutate(doc)
			malformedPath := filepath.Join(dir, "malformed-scores-"+strings.ReplaceAll(name, " ", "-")+".json")
			encoded, _ := json.Marshal(doc)
			if err := os.WriteFile(malformedPath, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("python3", filepath.Join("..", "..", "hack", "freeze-agent-sandbox-blind-scores.py"), "--blind-packets", blindPackets, "--blind-scores", malformedPath, "--output", filepath.Join(dir, "bad-score-freeze.json"))
			if output, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("malformed score freeze succeeded: %s", output)
			}
		})
	}
	for name, mutate := range map[string]func(map[string]any){
		"analysis": func(doc map[string]any) { doc["packets"].([]any)[0].(map[string]any)["root_cause"] = "tampered" },
		"reference": func(doc map[string]any) {
			doc["packets"].([]any)[0].(map[string]any)["causal_reference"].(map[string]any)["reference_diagnosis"] = "tampered"
		},
		"missing arm":   func(doc map[string]any) { doc["packets"] = doc["packets"].([]any)[1:] },
		"duplicate arm": func(doc map[string]any) { rows := doc["packets"].([]any); doc["packets"] = append(rows, rows[0]) },
	} {
		t.Run("freeze rejects "+name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(packets, &doc); err != nil {
				t.Fatal(err)
			}
			mutate(doc)
			tamperedPacketsPath := filepath.Join(dir, "tampered-packets-"+strings.ReplaceAll(name, " ", "-")+".json")
			data, _ := json.Marshal(doc)
			if err := os.WriteFile(tamperedPacketsPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("python3", filepath.Join("..", "..", "hack", "freeze-agent-sandbox-blind-scores.py"), "--blind-packets", tamperedPacketsPath, "--blind-scores", scoresPath, "--output", filepath.Join(dir, "bad-freeze.json"))
			if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "packet_set_sha256") {
				t.Fatalf("error=%v output=%s", err, output)
			}
		})
	}
	mapping, err := os.ReadFile(blindMap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mapping), "agent_sandbox") || !strings.Contains(string(mapping), "inprocess") {
		t.Fatalf("blind map omits runtime mapping: %s", mapping)
	}
	var mapDoc struct {
		Mapping []struct {
			PacketID string `json:"packet_id"`
			Arm      string `json:"arm"`
		} `json:"mapping"`
	}
	if err := json.Unmarshal(mapping, &mapDoc); err != nil {
		t.Fatal(err)
	}
	scoredCommand := exec.Command(
		"python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath,
		"--sandbox", sandboxPath,
		"--repo", filepath.Join("..", ".."),
		"--holdout-case", "case",
		"--expected-pairs", "3",
		"--blind-map-input", blindMap,
		"--blind-scores", scoresPath,
		"--score-freeze", freezePath,
		"--reference-manifest", references,
	)
	scoredOutput, err := scoredCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("scored report: %v: %s", err, scoredOutput)
	}
	if err := json.Unmarshal(scoredOutput, &report); err != nil {
		t.Fatal(err)
	}
	criteria = report["criteria"].(map[string]any)
	blindQuality := report["blind_quality"].(map[string]any)
	if blindQuality["packet_set_sha256"] != packetDoc.PacketSetSHA256 || blindQuality["reference_set_sha256"] != packetDoc.ReferenceSetSHA256 || blindQuality["score_set_sha256"] == "" {
		t.Fatalf("blind quality identities = %+v", blindQuality)
	}
	if criteria["blind_quality_complete"] != true || criteria["blind_quality_non_regression"] != true || criteria["shadow_comparison"] != "insufficient_evidence" || criteria["evidence_complete"] != false {
		t.Fatalf("scored criteria = %+v", criteria)
	}
	unresolved := report["unresolved_cases"].([]any)
	caseScores := blindQuality["cases"].(map[string]any)["case"].(map[string]any)["agent_sandbox"].(map[string]any)
	if len(unresolved) != 1 || unresolved[0] != "case" || caseScores["total_range"] == nil || caseScores["dimensions"].(map[string]any)["source_grounding"] == nil {
		t.Fatalf("unresolved=%v case_scores=%+v", unresolved, caseScores)
	}

	var mutatedScores map[string]any
	if err := json.Unmarshal(data, &mutatedScores); err != nil {
		t.Fatal(err)
	}
	mutatedRows := mutatedScores["scores"].([]any)
	mutatedRows[0].(map[string]any)["scores"].(map[string]any)["remediation"] = float64(0)
	mutatedScoreData, _ := json.Marshal(mutatedScores)
	mutatedScoresPath := filepath.Join(dir, "mutated-scores.json")
	if err := os.WriteFile(mutatedScoresPath, mutatedScoreData, 0o600); err != nil {
		t.Fatal(err)
	}
	mutatedScoreCommand := exec.Command("python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"), "--inprocess", inprocessPath, "--sandbox", sandboxPath, "--repo", filepath.Join("..", ".."), "--holdout-case", "case", "--expected-pairs", "3", "--blind-map-input", blindMap, "--blind-scores", mutatedScoresPath, "--score-freeze", freezePath, "--reference-manifest", references)
	mutatedScoreOutput, err := mutatedScoreCommand.CombinedOutput()
	if err == nil || !strings.Contains(string(mutatedScoreOutput), "pre-unblinding score freeze") {
		t.Fatalf("mutated score error=%v output=%s", err, mutatedScoreOutput)
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
		"python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath,
		"--sandbox", sandboxPath,
		"--repo", filepath.Join("..", ".."),
		"--blind-map-input", tamperedPath,
		"--blind-scores", scoresPath,
		"--score-freeze", freezePath,
		"--reference-manifest", references,
	)
	tamperedOutput, err := tamperedCommand.CombinedOutput()
	if err == nil || !strings.Contains(string(tamperedOutput), "packet_set_sha256 does not match") {
		t.Fatalf("tampered map error = %v, output = %s", err, tamperedOutput)
	}
}

func TestAgentSandboxAnalyzerReportDefaultsOmittedJUnitTestSource(t *testing.T) {
	inprocess, sandbox := validAgentSandboxAnalyzerReportRecords(t)
	for index := range inprocess {
		delete(inprocess[index], "test_source")
		sandbox[index]["test_source"] = ""
	}
	dir := t.TempDir()
	inprocessPath := filepath.Join(dir, "inprocess.jsonl")
	sandboxPath := filepath.Join(dir, "sandbox.jsonl")
	writeReportJSONL(t, inprocessPath, inprocess)
	writeReportJSONL(t, sandboxPath, sandbox)
	command := exec.Command(
		"python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath,
		"--sandbox", sandboxPath,
		"--repo", filepath.Join("..", ".."),
		"--holdout-case", "case",
		"--expected-pairs", "3",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("report: %v: %s", err, output)
	}
}

func TestAgentSandboxAnalyzerReportMarksInconsistentSandboxTokenTelemetryUnavailable(t *testing.T) {
	inprocess, sandbox := validAgentSandboxAnalyzerReportRecords(t)
	for index := range sandbox {
		sandbox[index]["input_tokens"] = 10
		sandbox[index]["cached_input_tokens"] = 20
		sandbox[index]["token_usage_available"] = true
	}
	dir := t.TempDir()
	inprocessPath := filepath.Join(dir, "inprocess.jsonl")
	sandboxPath := filepath.Join(dir, "sandbox.jsonl")
	writeReportJSONL(t, inprocessPath, inprocess)
	writeReportJSONL(t, sandboxPath, sandbox)
	command := exec.Command(
		"python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath,
		"--sandbox", sandboxPath,
		"--repo", filepath.Join("..", ".."),
		"--holdout-case", "case",
		"--expected-pairs", "3",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("report: %v: %s", err, output)
	}
	var report map[string]any
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	usage := report["agent_sandbox"].(map[string]any)
	if usage["token_usage_trials"] != float64(0) || usage["token_usage_inconsistent_trials"] != float64(3) || usage["input_tokens"] != nil || usage["estimated_cost_usd_total"] != nil {
		t.Fatalf("usage = %+v", usage)
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
		{name: "evidence mode", mutate: func(_ []map[string]any, sandbox []map[string]any) {
			for index := range sandbox {
				sandbox[index]["evidence_mode"] = benchmarkEvidenceModeArtifactOnly
				sandbox[index]["source_expectation_paths"] = []string{}
				sandbox[index]["source_expectation_hits"] = 0
				sandbox[index]["source_expectation_total"] = 0
				sandbox[index]["source_signal_hits"] = 0
				sandbox[index]["source_signal_total"] = 0
				sandbox[index]["evidence_contract_passed"] = true
				sandbox[index]["evidence_contract_status"] = "passed"
			}
		}, expected: "differs in evidence_mode"},
		{name: "case evidence mode drift", mutate: func(inprocess []map[string]any, sandbox []map[string]any) {
			inprocess[1]["evidence_mode"] = benchmarkEvidenceModeArtifactOnly
			sandbox[1]["evidence_mode"] = benchmarkEvidenceModeArtifactOnly
			for _, record := range []map[string]any{inprocess[1], sandbox[1]} {
				record["source_expectation_paths"] = []string{}
				record["source_expectation_hits"] = 0
				record["source_expectation_total"] = 0
				record["source_signal_hits"] = 0
				record["source_signal_total"] = 0
			}
			sandbox[1]["evidence_contract_passed"] = true
			sandbox[1]["evidence_contract_status"] = "passed"
		}, expected: "inprocess case case changes evidence_mode across repetitions"},
		{name: "model label", mutate: func(_ []map[string]any, sandbox []map[string]any) { sandbox[0]["model_label"] = "model-b" }, expected: "differs in model_label"},
		{name: "provider config", mutate: func(_ []map[string]any, sandbox []map[string]any) {
			sandbox[0]["provider_config_sha256"] = strings.Repeat("0", 64)
		}, expected: "differs in provider_config_sha256"},
		{name: "matrix provider drift", mutate: func(inprocess []map[string]any, sandbox []map[string]any) {
			inprocess[1]["provider_config_sha256"] = strings.Repeat("0", 64)
			sandbox[1]["provider_config_sha256"] = strings.Repeat("0", 64)
		}, expected: "benchmark matrix differs in provider_config_sha256"},
		{name: "executor image drift", mutate: func(_ []map[string]any, sandbox []map[string]any) {
			sandbox[1]["executor_image"] = "registry.example.test/executor@sha256:" + strings.Repeat("c", 64)
		}, expected: "sandbox runtime or image identity changes"},
		{name: "Sandbox max steps drift", mutate: func(_ []map[string]any, sandbox []map[string]any) {
			sandbox[1]["max_steps"] = 40
		}, expected: "sandbox runtime or image identity changes"},
		{name: "embedded revision", mutate: func(_ []map[string]any, sandbox []map[string]any) {
			sandbox[0]["executor_aster_revision"] = strings.Repeat("0", 40)
		}, expected: "embedded Aster revision differs"},
		{name: "OpenCode version", mutate: func(_ []map[string]any, sandbox []map[string]any) { sandbox[0]["opencode_version"] = "1.18.1" }, expected: "OpenCode version differs"},
		{name: "OpenCode limits", mutate: func(_ []map[string]any, sandbox []map[string]any) { sandbox[0]["request_context_limit"] = 100000 }, expected: "request limits differ"},
		{name: "rubric version", mutate: func(_ []map[string]any, sandbox []map[string]any) {
			sandbox[0]["human_score_rubric_version"] = benchmarkHumanScoreRubricVersion + 1
		}, expected: "differs in human_score_rubric_version"},
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
				"python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
				"--inprocess", inprocessPath,
				"--sandbox", sandboxPath,
				"--repo", filepath.Join("..", ".."),
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
	pricing := map[string]any{
		"currency": "USD", "input_per_million": "3", "cached_input_per_million": "0.30", "output_per_million": "15",
	}
	pricingData, err := json.Marshal(pricing)
	if err != nil {
		t.Fatal(err)
	}
	pricing["sha256"] = sha256Hex(pricingData)
	var inprocess []map[string]any
	var sandbox []map[string]any
	for repetition := 1; repetition <= 3; repetition++ {
		common := map[string]any{
			"case_id": "case", "stable_id": "0123456789abcdef0123", "repetition": repetition,
			"model_label": "model-a", "engine_commit": strings.Repeat("a", 40),
			"benchmark_manifest_sha256": strings.Repeat("6", 64),
			"fixture_sha256":            strings.Repeat("b", 64), "baseline_consumer_commit": strings.Repeat("c", 40),
			"baseline_prompt_sha256": strings.Repeat("d", 64), "project_sha256": strings.Repeat("e", 64),
			"effective_prompt_sha256": strings.Repeat("1", 64), "skill_set_hash": strings.Repeat("2", 64), "effective_input_sha256": strings.Repeat("3", 64),
			"comparison_input_sha256": strings.Repeat("4", 64),
			"source_revision":         strings.Repeat("f", 40), "provider_path": "provider/model", "provider_config_sha256": strings.Repeat("8", 64), "transport_id": "transport-v1",
			"api_mode": "chat_completions", "evidence_condition": "fixture-v1", "evidence_mode": benchmarkEvidenceModeArtifactAndSource,
			"model_context_tokens": 200000, "model_output_tokens": 8192, "pricing": pricing,
			"source_expectation_sha256": strings.Repeat("9", 64), "source_expectation_paths": []string{"pkg/file.go"}, "source_expectation_hits": 1, "source_expectation_total": 1,
			"source_signal_hits": 1, "source_signal_total": 1, "source_evidence_tool_calls": 1,
			"job_name": "job", "build_id": "1", "test_name": "test", "test_source": "build",
			"human_score_rubric_version": benchmarkHumanScoreRubricVersion, "human_score_max": 10, "human_score_dimensions": benchmarkHumanScoreDimensions,
			"elapsed_ms": 1000 + repetition, "signal_hits": 4, "signal_total": 5,
			"diagnosis_signal_hits": 3, "diagnosis_signal_total": 3,
			"transient_classification_correct": true, "forbidden_checks_passed": 0, "forbidden_checks_total": 0,
		}
		if repetition == 1 {
			common["test_source"] = ""
		}
		left := cloneReportRecord(common)
		left["arm"] = "baseline"
		left["trial_status"] = "valid_result"
		left["usable"] = true
		left["summary"] = "private summary"
		left["root_cause"] = "INPROCESS_PRIVATE_ROOT_CAUSE"
		left["suggested_fix"] = "private fix"
		left["severity"] = "High"
		left["is_transient"] = false
		left["evidence_citations"] = []map[string]any{{"path": "artifact.log", "quote": "PRIVATE_ARTIFACT_QUOTE"}}
		left["relevant_files"] = []string{"pkg/file.go"}
		left["file_links"] = map[string]string{"pkg/file.go": "private"}
		left["tool_names"] = []string{"read_artifact", "read_repo_file"}
		left["trace"] = map[string]any{"model_requests": 2, "reported_requests": 2, "provider_attempts": 2, "provider_attempts_known": true, "input_tokens": 100, "cached_input_tokens": 0, "output_tokens": 20, "reasoning_tokens": 4}
		inprocess = append(inprocess, left)

		right := cloneReportRecord(common)
		right["version"] = agentSandboxAnalyzerBenchmarkRecordVersion
		right["runtime"] = "agent-sandbox-opencode"
		right["runtime_identity_hash"] = strings.Repeat("7", 64)
		right["image_contract_sha256"] = strings.Repeat("5", 64)
		right["executor_image"] = "registry.example.test/executor@sha256:" + strings.Repeat("a", 64)
		right["stager_image"] = "registry.example.test/stager@sha256:" + strings.Repeat("b", 64)
		right["executor_aster_revision"] = strings.Repeat("a", 40)
		right["stager_aster_revision"] = strings.Repeat("a", 40)
		right["expected_opencode_version"] = "1.18.2"
		right["request_shape_available"] = true
		right["opencode_version"] = "1.18.2"
		right["request_context_limit"] = 200000
		right["request_output_token_limit"] = 8192
		right["arm"] = "arm-b"
		right["status"] = "succeeded"
		right["analysis_valid"] = true
		right["finalization_valid"] = true
		right["cleanup_completed"] = true
		right["source_verified"] = true
		right["artifact_citation_count"] = 1
		right["source_citation_count"] = 1
		right["artifact_evidence_tool_calls"] = 1
		right["source_evidence_tool_calls"] = 1
		right["evidence_contract_passed"] = true
		right["evidence_contract_status"] = "passed"
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
		right["max_steps"] = 20
		right["provider_requests"] = 1
		right["provider_requests_known"] = true
		right["input_tokens"] = 50
		right["cached_input_tokens"] = 0
		right["output_tokens"] = 10
		right["reasoning_tokens"] = 2
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

func writeTestCausalReferences(t *testing.T, path string, caseIDs []string) {
	t.Helper()
	cases := map[string]any{}
	for _, caseID := range caseIDs {
		cases[caseID] = map[string]any{
			"reference_diagnosis": "The initiating cause leads through the required causal link to a downstream timeout.",
			"required_chain":      []map[string]string{{"id": "initiating-cause", "text": "Find the initiating cause."}, {"id": "causal-link", "text": "Connect the causal link."}},
			"downstream_noise":    []string{"Do not treat the terminal timeout as primary."},
		}
	}
	data, err := json.Marshal(map[string]any{"version": 1, "cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAgentSandboxAnalyzerReportRejectsFullDiagnosisCreditForKueueReadinessNarrative(t *testing.T) {
	inprocess, sandbox := validAgentSandboxAnalyzerReportRecords(t)
	for _, records := range [][]map[string]any{inprocess, sandbox} {
		for _, record := range records {
			record["case_id"] = "kueue-was-podgroup-api-mismatch"
			record["root_cause"] = "Kind nodes were not ready and the dependency deployment timed out."
		}
	}
	dir := t.TempDir()
	inprocessPath, sandboxPath := filepath.Join(dir, "inprocess.jsonl"), filepath.Join(dir, "sandbox.jsonl")
	packetsPath, mapPath := filepath.Join(dir, "packets.json"), filepath.Join(dir, "map.json")
	references := filepath.Join("testdata", "benchmarks", "agent-sandbox-causal-references.json")
	writeReportJSONL(t, inprocessPath, inprocess)
	writeReportJSONL(t, sandboxPath, sandbox)
	command := exec.Command("python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"), "--inprocess", inprocessPath, "--sandbox", sandboxPath, "--repo", filepath.Join("..", ".."), "--expected-pairs", "3", "--blind-packets", packetsPath, "--blind-map", mapPath, "--reference-manifest", references)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("packets: %v: %s", err, output)
	}
	var packetDoc struct {
		PacketSetSHA256    string `json:"packet_set_sha256"`
		ReferenceSetSHA256 string `json:"reference_set_sha256"`
		Packets            []struct {
			PacketID string `json:"packet_id"`
			Arm      string `json:"arm"`
		} `json:"packets"`
	}
	data, _ := os.ReadFile(packetsPath)
	if err := json.Unmarshal(data, &packetDoc); err != nil {
		t.Fatal(err)
	}
	scoreRows := []map[string]any{}
	for _, item := range packetDoc.Packets {
		values := map[string]int{}
		for _, dimension := range benchmarkHumanScoreDimensions {
			values[dimension] = 2
		}
		scoreRows = append(scoreRows, map[string]any{"packet_id": item.PacketID, "arm": item.Arm, "scores": values, "causal_assessment": map[string]any{"alignment": "missing", "initiating_cause_found": false, "downstream_treated_as_primary": true, "required_chain_coverage": []string{}}})
	}
	scoresPath := filepath.Join(dir, "scores.json")
	scoreData, _ := json.Marshal(map[string]any{"version": 2, "packet_set_sha256": packetDoc.PacketSetSHA256, "reference_set_sha256": packetDoc.ReferenceSetSHA256, "rubric_version": benchmarkHumanScoreRubricVersion, "score_max": 10, "dimensions": benchmarkHumanScoreDimensions, "scoring_timestamp": "2026-08-18T00:00:00Z", "scores": scoreRows})
	if err := os.WriteFile(scoresPath, scoreData, 0o600); err != nil {
		t.Fatal(err)
	}
	freezePath := filepath.Join(dir, "score-freeze.json")
	freeze := exec.Command("python3", filepath.Join("..", "..", "hack", "freeze-agent-sandbox-blind-scores.py"), "--blind-packets", packetsPath, "--blind-scores", scoresPath, "--output", freezePath)
	freezeOutput, err := freeze.CombinedOutput()
	if err == nil || !strings.Contains(string(freezeOutput), "full diagnosis credit requires complete reference-aligned causal coverage") {
		t.Fatalf("freeze error=%v output=%s", err, freezeOutput)
	}
}

func TestAgentSandboxAnalyzerReportRejectsIncompleteTwoRepetitionMatrix(t *testing.T) {
	allInprocess, allSandbox := validAgentSandboxAnalyzerReportRecords(t)
	inprocess, sandbox := allInprocess[:2], allSandbox[:2]
	dir := t.TempDir()
	inprocessPath, sandboxPath := filepath.Join(dir, "inprocess.jsonl"), filepath.Join(dir, "sandbox.jsonl")
	writeReportJSONL(t, inprocessPath, inprocess)
	writeReportJSONL(t, sandboxPath, sandbox)
	command := exec.Command("python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"), "--inprocess", inprocessPath, "--sandbox", sandboxPath, "--repo", filepath.Join("..", ".."), "--expected-pairs", "2", "--required-repetitions", "2", "--holdout-case", "case")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("report: %v: %s", err, output)
	}
	var report map[string]any
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	criteria := report["criteria"].(map[string]any)
	if criteria["evidence_complete"] != false || report["holdouts_complete"] != true || criteria["evidence_modes_complete"] != false {
		t.Fatalf("report=%s", output)
	}
	writeReportJSONL(t, inprocessPath, allInprocess)
	writeReportJSONL(t, sandboxPath, allSandbox)
	extra := exec.Command("python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"), "--inprocess", inprocessPath, "--sandbox", sandboxPath, "--repo", filepath.Join("..", ".."), "--expected-pairs", "6", "--required-repetitions", "2", "--holdout-case", "case")
	extraOutput, err := extra.CombinedOutput()
	if err != nil {
		t.Fatalf("extra report: %v: %s", err, extraOutput)
	}
	if err := json.Unmarshal(extraOutput, &report); err != nil {
		t.Fatal(err)
	}
	criteria = report["criteria"].(map[string]any)
	if criteria["evidence_complete"] != false || report["holdouts_complete"] != false {
		t.Fatalf("extra trials were accepted: %s", extraOutput)
	}
}

func TestAgentSandboxAnalyzerReportArtifactOnlyDoesNotRequireSource(t *testing.T) {
	inprocess, sandbox := validAgentSandboxAnalyzerReportRecords(t)
	for index := range inprocess {
		inprocess[index]["evidence_mode"] = benchmarkEvidenceModeArtifactOnly
		inprocess[index]["file_links"] = map[string]string{}
		inprocess[index]["relevant_files"] = []string{}
		inprocess[index]["source_expectation_paths"] = []string{}
		inprocess[index]["source_expectation_hits"] = 0
		inprocess[index]["source_expectation_total"] = 0
		inprocess[index]["source_signal_hits"] = 0
		inprocess[index]["source_signal_total"] = 0
		inprocess[index]["source_evidence_tool_calls"] = 0
		sandbox[index]["evidence_mode"] = benchmarkEvidenceModeArtifactOnly
		sandbox[index]["source_expectation_paths"] = []string{}
		sandbox[index]["source_expectation_hits"] = 0
		sandbox[index]["source_expectation_total"] = 0
		sandbox[index]["source_signal_hits"] = 0
		sandbox[index]["source_signal_total"] = 0
		sandbox[index]["source_verified"] = false
		sandbox[index]["source_citation_count"] = 0
		sandbox[index]["source_citations"] = []map[string]any{}
		sandbox[index]["source_evidence_tool_calls"] = 0
		sandbox[index]["evidence_contract_passed"] = true
		sandbox[index]["evidence_contract_status"] = "passed"
	}
	report := runAgentSandboxAnalyzerReport(t, inprocess, sandbox)
	sandboxSummary := report["agent_sandbox"].(map[string]any)
	modes := sandboxSummary["evidence_modes"].(map[string]any)
	artifactOnly := modes[benchmarkEvidenceModeArtifactOnly].(map[string]any)
	if sandboxSummary["runtime_valid_trials"] != float64(3) || sandboxSummary["valid_trials"] != float64(3) || sandboxSummary["source_grounded_trials"] != float64(0) || artifactOnly["contract_pass_rate"] != float64(1) {
		t.Fatalf("sandbox summary = %+v", sandboxSummary)
	}
	criteria := report["criteria"].(map[string]any)
	if criteria["shadow_comparison"] != "insufficient_evidence" || criteria["evidence_modes_complete"] != false {
		t.Fatalf("criteria = %+v", criteria)
	}
}

func TestAgentSandboxAnalyzerReportSourceRequiredRejectsUnreadFileLink(t *testing.T) {
	inprocess, sandbox := validAgentSandboxAnalyzerReportRecords(t)
	inprocess[0]["relevant_files"] = []string{}
	inprocess[0]["source_expectation_hits"] = 0
	report := runAgentSandboxAnalyzerReport(t, inprocess, sandbox)
	inprocessSummary := report["inprocess"].(map[string]any)
	modes := inprocessSummary["evidence_modes"].(map[string]any)
	sourceRequired := modes[benchmarkEvidenceModeArtifactAndSource].(map[string]any)
	if inprocessSummary["runtime_valid_trials"] != float64(3) || inprocessSummary["valid_trials"] != float64(2) || sourceRequired["contract_passed_trials"] != float64(2) {
		t.Fatalf("in-process summary = %+v", inprocessSummary)
	}
}

func TestAgentSandboxAnalyzerReportSourceRequiredNeedsToolAndCitation(t *testing.T) {
	inprocess, sandbox := validAgentSandboxAnalyzerReportRecords(t)
	sandbox[0]["source_evidence_tool_calls"] = 0
	sandbox[0]["evidence_contract_passed"] = false
	sandbox[0]["evidence_contract_status"] = "unsupported_source_claim"
	report := runAgentSandboxAnalyzerReport(t, inprocess, sandbox)
	sandboxSummary := report["agent_sandbox"].(map[string]any)
	modes := sandboxSummary["evidence_modes"].(map[string]any)
	sourceRequired := modes[benchmarkEvidenceModeArtifactAndSource].(map[string]any)
	if sandboxSummary["runtime_valid_trials"] != float64(3) || sandboxSummary["valid_trials"] != float64(2) || sourceRequired["contract_passed_trials"] != float64(2) {
		t.Fatalf("sandbox summary = %+v", sandboxSummary)
	}
	criteria := report["criteria"].(map[string]any)
	if criteria["lifecycle_non_regression"] != true || criteria["grounding_non_regression"] != false {
		t.Fatalf("criteria = %+v", criteria)
	}
}

func TestAgentSandboxAnalyzerReportArtifactOnlyRejectsUnsupportedSourceOutput(t *testing.T) {
	inprocess, sandbox := validAgentSandboxAnalyzerReportRecords(t)
	for index := range inprocess {
		for _, record := range []map[string]any{inprocess[index], sandbox[index]} {
			record["evidence_mode"] = benchmarkEvidenceModeArtifactOnly
			record["source_expectation_paths"] = []string{}
			record["source_expectation_hits"] = 0
			record["source_expectation_total"] = 0
			record["source_signal_hits"] = 0
			record["source_signal_total"] = 0
			record["source_evidence_tool_calls"] = 0
		}
		sandbox[index]["evidence_contract_passed"] = false
		sandbox[index]["evidence_contract_status"] = "unsupported_source_claim"
	}
	report := runAgentSandboxAnalyzerReport(t, inprocess, sandbox)
	sandboxSummary := report["agent_sandbox"].(map[string]any)
	if sandboxSummary["runtime_valid_trials"] != float64(3) || sandboxSummary["valid_trials"] != float64(0) {
		t.Fatalf("sandbox summary = %+v", sandboxSummary)
	}
}

func TestAgentSandboxAnalyzerSixTrialReportRequiresBothEvidenceModes(t *testing.T) {
	inprocess, sandbox := validAgentSandboxAnalyzerReportRecords(t)
	for index := range inprocess {
		for _, record := range []map[string]any{inprocess[index], sandbox[index]} {
			record["evidence_mode"] = benchmarkEvidenceModeArtifactOnly
			record["source_expectation_paths"] = []string{}
			record["source_expectation_hits"] = 0
			record["source_expectation_total"] = 0
			record["source_signal_hits"] = 0
			record["source_signal_total"] = 0
			record["source_evidence_tool_calls"] = 0
		}
		inprocess[index]["file_links"] = map[string]string{}
		sandbox[index]["source_citations"] = []map[string]any{}
		sandbox[index]["source_citation_count"] = 0
		sandbox[index]["source_verified"] = false
		sandbox[index]["evidence_contract_passed"] = true
		sandbox[index]["evidence_contract_status"] = "passed"
	}
	for repetition := 4; repetition <= 6; repetition++ {
		left := cloneReportRecord(inprocess[0])
		right := cloneReportRecord(sandbox[0])
		left["source_expectation_paths"], right["source_expectation_paths"] = []string{}, []string{}
		left["repetition"], right["repetition"] = repetition, repetition
		inprocess, sandbox = append(inprocess, left), append(sandbox, right)
	}
	report := runAgentSandboxAnalyzerReportWithExpected(t, inprocess, sandbox, 6)
	criteria := report["criteria"].(map[string]any)
	if report["evidence_modes_complete"] != false || criteria["evidence_complete"] != false || criteria["evidence_modes_required"] != true {
		t.Fatalf("report = %+v", report)
	}
}

func TestAgentSandboxAnalyzerReportMateriallyBetterRequiresRepeatedMultiCaseImprovement(t *testing.T) {
	baseInprocess, baseSandbox := validAgentSandboxAnalyzerReportRecords(t)
	caseIDs := []string{"case-source", "case-artifact-a", "case-artifact-b"}
	var inprocess, sandbox []map[string]any
	for caseIndex, caseID := range caseIDs {
		for repetition := 1; repetition <= 3; repetition++ {
			left := cloneReportRecord(baseInprocess[repetition-1])
			right := cloneReportRecord(baseSandbox[repetition-1])
			for _, record := range []map[string]any{left, right} {
				record["case_id"] = caseID
				record["stable_id"] = fmt.Sprintf("%020x", caseIndex+1)
				record["comparison_input_sha256"] = strings.Repeat(fmt.Sprintf("%x", caseIndex+5), 64)
			}
			if caseID != "case-source" {
				left["evidence_mode"] = benchmarkEvidenceModeArtifactOnly
				left["file_links"] = map[string]string{}
				left["relevant_files"] = []string{}
				right["evidence_mode"] = benchmarkEvidenceModeArtifactOnly
				right["source_verified"] = false
				right["source_citation_count"] = 0
				right["source_citations"] = []map[string]any{}
				for _, record := range []map[string]any{left, right} {
					record["source_expectation_sha256"] = strings.Repeat("0", 64)
					record["source_expectation_paths"] = []string{}
					record["source_expectation_hits"] = 0
					record["source_expectation_total"] = 0
					record["source_signal_hits"] = 0
					record["source_signal_total"] = 0
					record["source_evidence_tool_calls"] = 0
				}
				right["evidence_contract_passed"] = true
				right["evidence_contract_status"] = "passed"
			}
			inprocess = append(inprocess, left)
			sandbox = append(sandbox, right)
		}
	}
	dir := t.TempDir()
	inprocessPath, sandboxPath := filepath.Join(dir, "inprocess.jsonl"), filepath.Join(dir, "sandbox.jsonl")
	packetsPath, mapPath := filepath.Join(dir, "packets.json"), filepath.Join(dir, "map.json")
	referencesPath := filepath.Join(dir, "references.json")
	writeReportJSONL(t, inprocessPath, inprocess)
	writeReportJSONL(t, sandboxPath, sandbox)
	writeTestCausalReferences(t, referencesPath, caseIDs)
	packetCommand := exec.Command(
		"python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath, "--sandbox", sandboxPath, "--repo", filepath.Join("..", ".."),
		"--expected-pairs", "9", "--required-repetitions", "3",
		"--holdout-case", caseIDs[0], "--holdout-case", caseIDs[1], "--holdout-case", caseIDs[2],
		"--blind-packets", packetsPath, "--blind-map", mapPath, "--reference-manifest", referencesPath,
	)
	if output, err := packetCommand.CombinedOutput(); err != nil {
		t.Fatalf("packets: %v: %s", err, output)
	}
	var packetDoc struct {
		PacketSetSHA256    string `json:"packet_set_sha256"`
		ReferenceSetSHA256 string `json:"reference_set_sha256"`
		Packets            []struct {
			PacketID string `json:"packet_id"`
			CaseID   string `json:"case_id"`
			Arm      string `json:"arm"`
		} `json:"packets"`
	}
	packetData, _ := os.ReadFile(packetsPath)
	if err := json.Unmarshal(packetData, &packetDoc); err != nil {
		t.Fatal(err)
	}
	var mapDoc struct {
		Mapping []struct {
			PacketID string `json:"packet_id"`
			Arm      string `json:"arm"`
			Runtime  string `json:"runtime"`
		} `json:"mapping"`
	}
	mapData, _ := os.ReadFile(mapPath)
	if err := json.Unmarshal(mapData, &mapDoc); err != nil {
		t.Fatal(err)
	}
	caseByPacket := map[string]string{}
	for _, packet := range packetDoc.Packets {
		caseByPacket[packet.PacketID] = packet.CaseID
	}
	var scoreRows []map[string]any
	for _, item := range mapDoc.Mapping {
		values := map[string]int{}
		for _, dimension := range benchmarkHumanScoreDimensions {
			values[dimension] = 2
		}
		if (caseByPacket[item.PacketID] == "case-source" || caseByPacket[item.PacketID] == "case-artifact-a") && item.Runtime == "inprocess" {
			values["diagnosis"] = 1
		}
		scoreRows = append(scoreRows, map[string]any{
			"packet_id": item.PacketID, "arm": item.Arm, "scores": values,
			"causal_assessment": map[string]any{"alignment": "aligned", "initiating_cause_found": true, "downstream_treated_as_primary": false, "required_chain_coverage": []string{"initiating-cause", "causal-link"}},
		})
	}
	scoresPath := filepath.Join(dir, "scores.json")
	scores := map[string]any{
		"version": 2, "packet_set_sha256": packetDoc.PacketSetSHA256, "reference_set_sha256": packetDoc.ReferenceSetSHA256,
		"rubric_version": benchmarkHumanScoreRubricVersion, "score_max": 10, "dimensions": benchmarkHumanScoreDimensions,
		"scoring_timestamp": "2026-08-18T00:00:00Z", "scores": scoreRows,
	}
	scoreData, _ := json.Marshal(scores)
	if err := os.WriteFile(scoresPath, scoreData, 0o600); err != nil {
		t.Fatal(err)
	}
	freezePath := filepath.Join(dir, "freeze.json")
	freeze := exec.Command("python3", filepath.Join("..", "..", "hack", "freeze-agent-sandbox-blind-scores.py"), "--blind-packets", packetsPath, "--blind-scores", scoresPath, "--output", freezePath)
	if output, err := freeze.CombinedOutput(); err != nil {
		t.Fatalf("freeze: %v: %s", err, output)
	}
	scored := exec.Command(
		"python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath, "--sandbox", sandboxPath, "--repo", filepath.Join("..", ".."),
		"--expected-pairs", "9", "--required-repetitions", "3",
		"--holdout-case", caseIDs[0], "--holdout-case", caseIDs[1], "--holdout-case", caseIDs[2],
		"--blind-map-input", mapPath, "--blind-scores", scoresPath, "--score-freeze", freezePath, "--reference-manifest", referencesPath,
	)
	output, err := scored.CombinedOutput()
	if err != nil {
		t.Fatalf("scored report: %v: %s", err, output)
	}
	var report map[string]any
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	criteria := report["criteria"].(map[string]any)
	if report["shadow_comparison"] != "shadow_materially_better" || criteria["repeated_causal_improvement_across_multiple_cases"] != true || criteria["authoritative_analyzer"] != "inprocess_unchanged" {
		t.Fatalf("criteria=%+v", criteria)
	}
	// Keep whole-matrix invalid counts equal while moving the Sandbox failure to
	// the source-required case. Per-case gating must still prefer in-process.
	sandbox[0]["status"] = "invalid_result"
	sandbox[0]["analysis_valid"] = false
	sandbox[0]["finalization_valid"] = false
	sandbox[0]["evidence_contract_passed"] = false
	sandbox[0]["evidence_contract_status"] = "analysis_unavailable"
	inprocess[3]["usable"] = false
	inprocess[3]["trial_status"] = "invalid_result"
	writeReportJSONL(t, inprocessPath, inprocess)
	writeReportJSONL(t, sandboxPath, sandbox)
	regressed := exec.Command(
		"python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath, "--sandbox", sandboxPath, "--repo", filepath.Join("..", ".."),
		"--expected-pairs", "9", "--required-repetitions", "3",
		"--holdout-case", caseIDs[0], "--holdout-case", caseIDs[1], "--holdout-case", caseIDs[2],
		"--blind-map-input", mapPath, "--blind-scores", scoresPath, "--score-freeze", freezePath, "--reference-manifest", referencesPath,
	)
	regressedOutput, err := regressed.CombinedOutput()
	if err != nil {
		t.Fatalf("regressed report: %v: %s", err, regressedOutput)
	}
	if err := json.Unmarshal(regressedOutput, &report); err != nil {
		t.Fatal(err)
	}
	criteria = report["criteria"].(map[string]any)
	if report["shadow_comparison"] != "inprocess_preferred" || criteria["per_case_quality_non_regression"] != false {
		t.Fatalf("regressed criteria=%+v", criteria)
	}
}

func runAgentSandboxAnalyzerReport(t *testing.T, inprocess, sandbox []map[string]any) map[string]any {
	t.Helper()
	return runAgentSandboxAnalyzerReportWithExpected(t, inprocess, sandbox, 3)
}

func runAgentSandboxAnalyzerReportWithExpected(t *testing.T, inprocess, sandbox []map[string]any, expected int) map[string]any {
	t.Helper()
	dir := t.TempDir()
	inprocessPath := filepath.Join(dir, "inprocess.jsonl")
	sandboxPath := filepath.Join(dir, "sandbox.jsonl")
	writeReportJSONL(t, inprocessPath, inprocess)
	writeReportJSONL(t, sandboxPath, sandbox)
	command := exec.Command(
		"python3", filepath.Join("..", "..", "hack", "compare-agent-sandbox-analyzer-benchmark.py"),
		"--inprocess", inprocessPath,
		"--sandbox", sandboxPath,
		"--repo", filepath.Join("..", ".."),
		"--holdout-case", "case",
		"--expected-pairs", strconv.Itoa(expected),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("report: %v: %s", err, output)
	}
	var report map[string]any
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func cloneReportRecord(record map[string]any) map[string]any {
	clone := make(map[string]any, len(record))
	for key, value := range record {
		if values, ok := value.([]string); ok {
			clone[key] = append([]string(nil), values...)
			continue
		}
		clone[key] = value
	}
	return clone
}
