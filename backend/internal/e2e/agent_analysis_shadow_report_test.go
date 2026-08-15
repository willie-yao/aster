package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
	agentruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

func TestAgentAnalysisShadowReport(t *testing.T) {
	inprocess, shadow := validShadowReportRecords()
	output, err := runShadowReport(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow))
	if err != nil {
		t.Fatalf("report: %v: %s", err, output)
	}
	text := string(output)
	for _, want := range []string{"case", "2/3", "10/5", "unavailable_from_agent_runtime"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report missing %q:\n%s", want, text)
		}
	}
	for _, sensitive := range []string{
		"summary content", "root cause content", "artifact contents", "unresolved content", "missing signal content",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde0",
	} {
		if strings.Contains(text, sensitive) {
			t.Fatalf("content-free report contains %q:\n%s", sensitive, text)
		}
	}
}

func TestAgentAnalysisShadowReportAcceptsGroundedPolicyUnavailable(t *testing.T) {
	inprocess, shadow := validShadowReportRecords()
	inprocess["outcome"] = "grounded_policy_unavailable"
	inprocess["usable"] = false
	inprocess["trial_status"] = "invalid_result"
	output, err := runShadowReport(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow))
	if err != nil {
		t.Fatalf("report: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "invalid_result") {
		t.Fatalf("report missing invalid trial status:\n%s", output)
	}
}

func TestAgentAnalysisShadowReportAcceptsEveryShadowStatus(t *testing.T) {
	tests := []struct {
		status  string
		success bool
	}{
		{status: "succeeded", success: true},
		{status: "cleanup_pending", success: true},
		{status: "no_result"},
		{status: "malformed_result"},
		{status: "extra_file"},
		{status: "deletion"},
		{status: "rename"},
		{status: "contract_violation"},
		{status: "runtime_failure"},
		{status: "timeout"},
		{status: "cancellation"},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			shadow["status"] = test.status
			if test.status != "succeeded" {
				shadow["error_code"] = test.status
			}
			if test.status == "cleanup_pending" {
				shadow["cleanup_completed"] = false
			}
			if !test.success {
				shadow["attempts"] = 0
				shadow["artifact_citation_count"] = 0
				shadow["deterministic_status"] = "not_run"
				shadow["deterministic_passed"] = false
			}
			output, err := runShadowReport(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow))
			if err != nil {
				t.Fatalf("report: %v: %s", err, output)
			}
		})
	}
}

func TestShadowRecordWriterMatchesReportSchema(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantStatus   string
		resultStatus agentanalysis.ShadowStatus
		withAnalysis bool
	}{
		{name: "success", wantStatus: "succeeded", withAnalysis: true},
		{name: "runtime failure", err: errors.New("runtime failed"), wantStatus: "runtime_failure"},
		{name: "contract violation", resultStatus: agentanalysis.ShadowStatusContractViolation, err: agentanalysis.ErrInvalidResult, wantStatus: "contract_violation"},
		{name: "timeout", err: context.DeadlineExceeded, wantStatus: "timeout"},
		{name: "cleanup pending", err: agentruntime.ErrCleanupPending, wantStatus: "cleanup_pending", withAnalysis: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := shadowBenchmarkConfig{
				EngineCommit: "0123456789abcdef0123456789abcdef01234567", OrkaCommit: "6789abcdef0123456789abcdef0123456789abcd",
				Namespace: "orka-system", AgentRef: "analysis-agent-v1", AgentVersion: "v1",
				ProviderPath: "github-copilot/claude-sonnet-4.6", TransportID: "copilot-structural-proxy-v1", ModelLabel: "model", MaxTurns: 12, Timeout: 5 * time.Minute,
			}
			bc := benchCase{
				name: "case", stableID: "0123456789abcdef0123", fixtureSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				consumerCommit: "123456789abcdef0123456789abcdef012345678", promptSHA256: "123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0",
				projectSHA256:  "23456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef01",
				evidenceGroups: []benchmarkEvidenceGroup{{id: "evidence-group"}, {id: "secondary-group"}},
			}
			bundle := agentanalysis.EvidenceBundle{
				SkillSetHash: "3456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef012",
				Excerpts:     []agentanalysis.EvidenceExcerpt{{ID: "evidence-id", Path: "build-log.txt"}},
			}
			result := agentanalysis.Result{
				Status:    test.resultStatus,
				SourceSHA: "456789abcdef0123456789abcdef0123456789ab", EvidenceHash: "89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567",
				SkillHash:    "789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456",
				IdentityHash: "9abcdef0123456789abcdef0123456789abcdef0123456789abcdef012345678",
				ExecutionID:  "agent-analysis-0123456789abcdef", Attempts: 1,
			}
			if test.withAnalysis {
				result.Telemetry = agentruntime.GenerateTelemetry{TaskFinalized: true, ResultAvailable: true, FinalizationChecked: true, FinalizationValid: true, CleanupCompleted: test.err == nil, UsageStatus: "unavailable_from_agent_runtime"}
				result.Quality = agentanalysis.ShadowQuality{DeterministicStatus: "passed", DeterministicPassed: true, SemanticStatus: "unavailable", SemanticReason: "evidence_aware_semantic_judge_not_exposed"}
				result.Analysis = agentanalysis.Analysis{
					Summary: "summary", EvidenceCitations: []agentanalysis.EvidenceCitation{{ExcerptID: "evidence-id", LineStart: 1, LineEnd: 1, Quote: "private quote"}},
					UnresolvedDetails: []string{},
				}
			}
			record := shadowRecordForResult(cfg, bc, 1, bundle, result, time.Second, test.err, "56789abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234")
			record.SignalTotal = 3
			if record.Status != test.wantStatus {
				t.Fatalf("status = %q", record.Status)
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			var shadow map[string]any
			if err := json.Unmarshal(data, &shadow); err != nil {
				t.Fatal(err)
			}
			inprocess, _ := validShadowReportRecords()
			for _, field := range []string{"arm", "engine_commit", "fixture_sha256", "baseline_consumer_commit", "baseline_prompt_sha256", "project_sha256", "skill_set_hash", "provider_path", "transport_id", "api_mode", "model_label", "stable_id", "evidence_condition", "evidence_stage_sha256", "evidence_stage_ids", "source_revision", "human_score_rubric_version", "human_score_max", "human_score_dimensions", "signal_total"} {
				inprocess[field] = shadow[field]
			}
			output, err := runShadowReport(t, marshalJSONL(t, inprocess), string(data)+"\n")
			if err != nil {
				t.Fatalf("writer record rejected by report: %v: %s", err, output)
			}
		})
	}
}

func TestAgentAnalysisShadowReportRejectsDuplicateRecords(t *testing.T) {
	inprocess, shadow := validShadowReportRecords()
	tests := []struct {
		name      string
		inprocess string
		shadow    string
	}{
		{name: "in-process", inprocess: marshalJSONL(t, inprocess) + marshalJSONL(t, inprocess), shadow: marshalJSONL(t, shadow)},
		{name: "shadow", inprocess: marshalJSONL(t, inprocess), shadow: marshalJSONL(t, shadow) + marshalJSONL(t, shadow)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertShadowReportError(t, test.inprocess, test.shadow, "duplicate benchmark record case/fixture-v1/rep-01")
		})
	}
}

func TestAgentAnalysisShadowReportRejectsEmptyInputs(t *testing.T) {
	inprocess, shadow := validShadowReportRecords()
	tests := []struct {
		name      string
		inprocess string
		shadow    string
		want      string
	}{
		{name: "in-process", inprocess: "\n", shadow: marshalJSONL(t, shadow), want: "empty in-process JSONL input"},
		{name: "shadow", inprocess: marshalJSONL(t, inprocess), shadow: "\n", want: "empty shadow JSONL input"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertShadowReportError(t, test.inprocess, test.shadow, test.want)
		})
	}
}

func TestAgentAnalysisShadowReportRejectsUnpairedRecords(t *testing.T) {
	inprocess, shadow := validShadowReportRecords()
	shadow["case_id"] = "other-case"
	assertShadowReportError(
		t,
		marshalJSONL(t, inprocess),
		marshalJSONL(t, shadow),
		"unpaired benchmark records: case/fixture-v1/rep-01, other-case/fixture-v1/rep-01",
	)
}

func TestAgentAnalysisShadowReportRejectsPairMismatches(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "engine commit", field: "engine_commit", value: strings.Repeat("f", 40)},
		{name: "fixture hash", field: "fixture_sha256", value: strings.Repeat("f", 64)},
		{name: "consumer commit", field: "baseline_consumer_commit", value: strings.Repeat("e", 40)},
		{name: "prompt hash", field: "baseline_prompt_sha256", value: strings.Repeat("e", 64)},
		{name: "project hash", field: "project_sha256", value: strings.Repeat("d", 64)},
		{name: "skill set hash", field: "skill_set_hash", value: strings.Repeat("c", 64)},
		{name: "provider path", field: "provider_path", value: "other-provider"},
		{name: "model label", field: "model_label", value: "other-model"},
		{name: "stable id", field: "stable_id", value: strings.Repeat("b", 20)},
		{name: "evidence stage hash", field: "evidence_stage_sha256", value: strings.Repeat("c", 64)},
		{name: "source revision", field: "source_revision", value: strings.Repeat("a", 40)},
		{name: "rubric version", field: "human_score_rubric_version", value: benchmarkHumanScoreRubricVersion + 1},
		{name: "rubric maximum", field: "human_score_max", value: 20},
		{name: "rubric dimensions", field: "human_score_dimensions", value: []string{"diagnosis", "remediation"}},
		{name: "signal total", field: "signal_total", value: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			shadow[test.field] = test.value
			assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), test.field+" mismatch for case/fixture-v1/rep-01")
		})
	}
}

func TestAgentAnalysisShadowReportRejectsMissingRequiredFields(t *testing.T) {
	commonFields := []string{
		"case_id", "stable_id", "repetition", "arm", "engine_commit", "fixture_sha256",
		"baseline_consumer_commit", "baseline_prompt_sha256", "project_sha256", "skill_set_hash",
		"provider_path", "api_mode", "model_label", "evidence_condition", "evidence_stage_sha256", "evidence_stage_ids", "source_revision", "signal_hits", "signal_total",
		"elapsed_ms", "human_score_rubric_version", "human_score_max", "human_score_dimensions",
	}
	inprocessFields := append(append([]string(nil), commonFields...), "outcome", "usable", "trace")
	for _, field := range inprocessFields {
		t.Run("in-process "+field, func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			delete(inprocess, field)
			assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), "in-process record missing required field "+field)
		})
	}

	for _, field := range []string{"input_tokens", "output_tokens"} {
		t.Run("in-process trace "+field, func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			delete(inprocess["trace"].(map[string]any), field)
			assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), "in-process trace missing required field "+field)
		})
	}

	shadowFields := append(append([]string(nil), commonFields...),
		"version", "runtime", "status", "attempts", "artifact_citation_count", "source_citation_count",
		"source_verified", "unresolved_details", "contract_version", "tool_policy_version", "agent_namespace",
		"agent_ref", "agent_version", "agent_config_sha256", "orka_commit", "agent_skill_hash", "evidence_hash",
		"runtime_identity_hash", "execution_id", "max_turns", "timeout", "retries", "token_usage_available", "cost_status",
	)
	for _, field := range shadowFields {
		t.Run("shadow "+field, func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			delete(shadow, field)
			assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), "shadow record missing required field "+field)
		})
	}
}

func TestAgentAnalysisShadowReportRejectsCommonContractViolations(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value any
		want  string
	}{
		{name: "case id", field: "case_id", value: " ", want: "in-process field case_id must be a non-empty string"},
		{name: "stable id length", field: "stable_id", value: strings.Repeat("a", 19), want: "in-process field stable_id must be 20 lowercase hexadecimal characters"},
		{name: "stable id case", field: "stable_id", value: strings.Repeat("A", 20), want: "in-process field stable_id must be 20 lowercase hexadecimal characters"},
		{name: "repetition", field: "repetition", value: 0, want: "in-process field repetition must be a positive integer"},
		{name: "arm", field: "arm", value: "candidate", want: "in-process arm must be baseline"},
		{name: "engine commit", field: "engine_commit", value: strings.Repeat("a", 39), want: "in-process field engine_commit must be 40 lowercase hexadecimal characters"},
		{name: "fixture hash", field: "fixture_sha256", value: strings.Repeat("A", 64), want: "in-process field fixture_sha256 must be 64 lowercase hexadecimal characters"},
		{name: "consumer commit", field: "baseline_consumer_commit", value: strings.Repeat("b", 39), want: "in-process field baseline_consumer_commit must be 40 lowercase hexadecimal characters"},
		{name: "prompt hash", field: "baseline_prompt_sha256", value: strings.Repeat("b", 63), want: "in-process field baseline_prompt_sha256 must be 64 lowercase hexadecimal characters"},
		{name: "project hash", field: "project_sha256", value: strings.Repeat("c", 63), want: "in-process field project_sha256 must be 64 lowercase hexadecimal characters"},
		{name: "skill set hash", field: "skill_set_hash", value: strings.Repeat("d", 63), want: "in-process field skill_set_hash must be 64 lowercase hexadecimal characters"},
		{name: "provider path empty", field: "provider_path", value: "", want: "in-process field provider_path must be a non-empty string without whitespace"},
		{name: "provider path whitespace", field: "provider_path", value: "copilot proxy", want: "in-process field provider_path must be a non-empty string without whitespace"},
		{name: "api mode", field: "api_mode", value: "responses", want: "in-process api_mode must be chat_completions"},
		{name: "model label", field: "model_label", value: " ", want: "in-process field model_label must be a non-empty string"},
		{name: "source revision", field: "source_revision", value: strings.Repeat("e", 41), want: "in-process field source_revision must be 40 lowercase hexadecimal characters"},
		{name: "signal hits", field: "signal_hits", value: -1, want: "in-process field signal_hits must be a non-negative integer"},
		{name: "signal total", field: "signal_total", value: 0, want: "in-process field signal_total must be a positive integer"},
		{name: "elapsed", field: "elapsed_ms", value: -1, want: "in-process field elapsed_ms must be a non-negative integer"},
		{name: "rubric version", field: "human_score_rubric_version", value: 0, want: "in-process field human_score_rubric_version must be a positive integer"},
		{name: "rubric maximum", field: "human_score_max", value: 0, want: "in-process field human_score_max must be a positive integer"},
		{name: "rubric dimensions", field: "human_score_dimensions", value: []string{}, want: "in-process field human_score_dimensions must be a non-empty list of non-empty strings"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			inprocess[test.field] = test.value
			assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), test.want)
		})
	}

	for _, kind := range []string{"in-process", "shadow"} {
		t.Run(kind+" signal hits exceed total", func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			record := inprocess
			if kind == "shadow" {
				record = shadow
			}
			record["signal_hits"] = 4
			record["signal_total"] = 3
			assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), kind+" signal_hits exceeds signal_total")
		})
	}
}

func TestAgentAnalysisShadowReportRejectsInprocessOutcomeViolations(t *testing.T) {
	tests := []struct {
		name    string
		outcome string
		usable  any
		want    string
	}{
		{name: "invalid outcome", outcome: "other", usable: false, want: "in-process outcome is invalid"},
		{name: "unknown valid result", outcome: "unknown", usable: false, want: "in-process valid_result requires outcome=usable, usable=true, and a model request"},
		{name: "usable marked false", outcome: "usable", usable: false, want: "in-process valid_result requires outcome=usable, usable=true, and a model request"},
		{name: "unavailable marked true", outcome: "grounded_policy_unavailable", usable: true, want: "in-process valid_result requires outcome=usable, usable=true, and a model request"},
		{name: "usable wrong type", outcome: "usable", usable: "true", want: "in-process field usable must be a boolean"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			inprocess["outcome"] = test.outcome
			inprocess["usable"] = test.usable
			assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), test.want)
		})
	}

	for _, field := range []string{"input_tokens", "output_tokens"} {
		t.Run("negative "+field, func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			inprocess["trace"].(map[string]any)[field] = -1
			assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), "in-process trace field "+field+" must be a non-negative integer")
		})
	}
}

func TestAgentAnalysisShadowReportAcceptsFailedInprocessTrials(t *testing.T) {
	tests := []struct {
		status  string
		outcome string
		usable  bool
	}{
		{status: "no_result", outcome: "unknown"},
		{status: "timeout", outcome: "unknown"},
		{status: "runtime_failure", outcome: "unknown"},
		{status: "contract_violation", outcome: "usable", usable: true},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			inprocess["trial_status"] = test.status
			inprocess["outcome"] = test.outcome
			inprocess["usable"] = test.usable
			if test.status != "contract_violation" {
				inprocess["model_request_made"] = test.status != "no_result"
			}
			if test.status == "no_result" {
				for _, stage := range inprocess["evidence_stages"].([]map[string]any) {
					stage["model_received_evidence"] = false
				}
			}
			output, err := runShadowReport(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow))
			if err != nil {
				t.Fatalf("report: %v: %s", err, output)
			}
			if !strings.Contains(string(output), test.status) {
				t.Fatalf("report omitted trial status %q:\n%s", test.status, output)
			}
		})
	}
}

func TestAgentAnalysisShadowReportRejectsEvidenceTelemetryViolations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{name: "telemetry version", mutate: func(record map[string]any) { record["evidence_telemetry_version"] = 1 }, want: "in-process evidence_telemetry_version must be 2"},
		{name: "trial status", mutate: func(record map[string]any) { record["trial_status"] = "unknown" }, want: "in-process trial_status is invalid"},
		{name: "missing stages", mutate: func(record map[string]any) { record["evidence_stages"] = []map[string]any{} }, want: "in-process evidence_stages must not be empty"},
		{name: "partial stages", mutate: func(record map[string]any) {
			record["evidence_stages"] = record["evidence_stages"].([]map[string]any)[:1]
		}, want: "in-process evidence stage IDs do not match stages"},
		{name: "missing model request", mutate: func(record map[string]any) {
			record["model_request_made"] = false
		}, want: "in-process evidence stage reports receipt without a model request"},
		{name: "duplicate stage", mutate: func(record map[string]any) {
			stages := record["evidence_stages"].([]map[string]any)
			record["evidence_stages"] = append(stages, cloneReportRecord(stages[0]))
		}, want: "in-process evidence stage group_id is duplicated"},
		{name: "malformed stage boolean", mutate: func(record map[string]any) {
			record["evidence_stages"].([]map[string]any)[0]["model_received_evidence"] = "yes"
		}, want: "in-process evidence stage model_received_evidence must be a boolean"},
		{name: "oracle missing hash", mutate: func(record map[string]any) {
			record["evidence_condition"] = benchmarkEvidenceConditionOracle
		}, want: "oracle in-process record requires frozen_evidence_sha256"},
		{name: "valid result without usable", mutate: func(record map[string]any) {
			record["usable"] = false
			record["outcome"] = "grounded_policy_unavailable"
		}, want: "in-process valid_result requires outcome=usable, usable=true, and a model request"},
		{name: "malformed semantic outcomes", mutate: func(record map[string]any) {
			record["semantic_judge_outcomes"] = "draft:passed"
		}, want: "in-process field semantic_judge_outcomes must be a string list"},
		{name: "unknown semantic finding", mutate: func(record map[string]any) {
			record["semantic_finding_classes"] = []string{"invented"}
		}, want: "in-process semantic_finding_classes contains an invalid value"},
		{name: "negative supported facts", mutate: func(record map[string]any) {
			record["supported_facts_retained"] = -1
		}, want: "in-process field supported_facts_retained must be a non-negative integer"},
		{name: "revision selected without outcome", mutate: func(record map[string]any) {
			record["semantic_revision_selected"] = true
		}, want: "in-process semantic_revision_selected does not match revision outcomes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inprocess, shadow := validShadowReportRecords()
			test.mutate(inprocess)
			assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), test.want)
		})
	}
}

func validShadowReportRecords() (map[string]any, map[string]any) {
	dimensions := []string{"diagnosis", "artifact_evidence", "claim_discipline", "remediation", "source_grounding"}
	stageGroups := []benchmarkEvidenceGroup{{id: "evidence-group"}, {id: "secondary-group"}}
	common := map[string]any{
		"case_id":                    "case",
		"stable_id":                  "0123456789abcdef0123",
		"repetition":                 1,
		"arm":                        "baseline",
		"engine_commit":              "0123456789abcdef0123456789abcdef01234567",
		"fixture_sha256":             "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"baseline_consumer_commit":   "123456789abcdef0123456789abcdef012345678",
		"baseline_prompt_sha256":     "123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0",
		"project_sha256":             "23456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef01",
		"skill_set_hash":             "3456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef012",
		"provider_path":              "github-copilot/claude-sonnet-4.6",
		"transport_id":               "copilot-structural-proxy-v1",
		"api_mode":                   "chat_completions",
		"model_label":                "model",
		"evidence_condition":         benchmarkEvidenceConditionFixture,
		"evidence_stage_sha256":      benchmarkEvidenceStageSHA256(stageGroups),
		"evidence_stage_ids":         benchmarkEvidenceStageIDs(stageGroups),
		"source_revision":            "456789abcdef0123456789abcdef0123456789ab",
		"signal_total":               3,
		"human_score_rubric_version": benchmarkHumanScoreRubricVersion,
		"human_score_max":            10,
		"human_score_dimensions":     dimensions,
	}
	inprocess := cloneReportRecord(common)
	inprocess["outcome"] = "usable"
	inprocess["usable"] = true
	inprocess["signal_hits"] = 2
	inprocess["elapsed_ms"] = 100
	inprocess["trace"] = map[string]any{"input_tokens": 10, "output_tokens": 5}
	inprocess["evidence_telemetry_version"] = 2
	inprocess["trial_status"] = "valid_result"
	inprocess["model_request_made"] = true
	inprocess["evidence_stages"] = []map[string]any{
		{
			"group_id": "evidence-group", "required_signal_in_fixture": true, "candidate_path_selected": true,
			"frozen_excerpt_contains_signal": true, "model_received_evidence": true, "evidence_cited": true,
			"causally_used_in_root_cause": true, "causal_signal_configured": true,
		},
		{
			"group_id": "secondary-group", "required_signal_in_fixture": true, "candidate_path_selected": false,
			"frozen_excerpt_contains_signal": false, "model_received_evidence": false, "evidence_cited": false,
			"causally_used_in_root_cause": false, "causal_signal_configured": false,
		},
	}
	inprocess["evidence_revisions"] = []map[string]any{}
	inprocess["semantic_judge_outcomes"] = []string{"draft:passed"}
	inprocess["semantic_finding_classes"] = []string{}
	inprocess["semantic_revision_attempted"] = false
	inprocess["semantic_revision_selected"] = false
	inprocess["semantic_revision_rejected"] = false
	inprocess["supported_facts_retained"] = 0
	inprocess["supported_facts_added"] = 0
	inprocess["supported_facts_dropped"] = 0
	inprocess["summary"] = "summary content"
	inprocess["root_cause"] = "root cause content"

	shadow := cloneReportRecord(common)
	shadow["version"] = 3
	shadow["runtime"] = "orka-opencode-shadow"
	shadow["status"] = "succeeded"
	shadow["attempts"] = 1
	shadow["artifact_citation_count"] = 2
	shadow["source_citation_count"] = 1
	shadow["source_verified"] = true
	shadow["unresolved_details"] = []string{"unresolved content"}
	shadow["contract_version"] = "agent-analysis-v1"
	shadow["tool_policy_version"] = "agent-analysis-tools-v2"
	shadow["agent_namespace"] = "orka-system"
	shadow["agent_ref"] = "analysis-agent-v1"
	shadow["agent_version"] = "v1"
	shadow["agent_config_sha256"] = "56789abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234"
	shadow["orka_commit"] = "6789abcdef0123456789abcdef0123456789abcd"
	shadow["agent_skill_hash"] = "789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456"
	shadow["evidence_hash"] = "89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	shadow["runtime_identity_hash"] = "9abcdef0123456789abcdef0123456789abcdef0123456789abcdef012345678"
	shadow["execution_id"] = "agent-analysis-0123456789abcdef"
	shadow["max_turns"] = 15
	shadow["timeout"] = "5m0s"
	shadow["retries"] = 1
	shadow["token_usage_available"] = false
	shadow["cost_status"] = "unavailable_from_agent_runtime"
	shadow["task_finalized"] = true
	shadow["result_available"] = true
	shadow["finalization_checked"] = true
	shadow["finalization_valid"] = true
	shadow["cleanup_completed"] = true
	shadow["model_identity_available"] = false
	shadow["provider_identity_available"] = false
	shadow["identity_status"] = "agent_owned_identity_unavailable"
	shadow["deterministic_status"] = "passed"
	shadow["deterministic_passed"] = true
	shadow["semantic_status"] = "unavailable"
	shadow["semantic_valid"] = false
	shadow["signal_hits"] = 3
	shadow["elapsed_ms"] = 80
	shadow["summary"] = "artifact contents"
	shadow["missing_must"] = []string{"missing signal content"}
	shadow["evidence_citations"] = []map[string]any{{
		"quote": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde0",
	}}
	return inprocess, shadow
}

func cloneReportRecord(record map[string]any) map[string]any {
	clone := make(map[string]any, len(record))
	for key, value := range record {
		if strings, ok := value.([]string); ok {
			clone[key] = append([]string(nil), strings...)
			continue
		}
		clone[key] = value
	}
	return clone
}

func marshalJSONL(t *testing.T, record map[string]any) string {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func assertShadowReportError(t *testing.T, inprocess, shadow, want string) {
	t.Helper()
	output, err := runShadowReport(t, inprocess, shadow)
	if err == nil {
		t.Fatalf("report unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), want) {
		t.Fatalf("report error missing %q:\n%s", want, output)
	}
}

func runShadowReport(t *testing.T, inprocessData, shadowData string) ([]byte, error) {
	t.Helper()
	dir := t.TempDir()
	inprocess := filepath.Join(dir, "inprocess.jsonl")
	shadow := filepath.Join(dir, "shadow.jsonl")
	if err := os.WriteFile(inprocess, []byte(inprocessData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shadow, []byte(shadowData), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("python3", filepath.Join("..", "..", "..", "hack", "compare-agent-analysis-shadow-benchmark.py"), "--inprocess", inprocess, "--shadow", shadow)
	return command.CombinedOutput()
}

func TestAgentAnalysisShadowReportRejectsInvalidLifecycleTiming(t *testing.T) {
	t.Run("negative", func(t *testing.T) {
		inprocess, shadow := validShadowReportRecords()
		shadow["runtime_duration_ms"] = -1
		assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), "shadow field runtime_duration_ms must be a non-negative integer")
	})
	t.Run("non-monotonic", func(t *testing.T) {
		inprocess, shadow := validShadowReportRecords()
		shadow["task_finalized_ms"] = 20
		shadow["result_available_ms"] = 10
		assertShadowReportError(t, marshalJSONL(t, inprocess), marshalJSONL(t, shadow), "shadow result_available_ms must be at least task_finalized_ms")
	})
}
