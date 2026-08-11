#!/usr/bin/env python3
"""Compare paired in-process and Agent Sandbox analyzer benchmark records."""

from __future__ import annotations

import argparse
from decimal import Decimal, InvalidOperation
import hashlib
import json
import math
import os
from pathlib import Path
import secrets
import statistics
import sys
from typing import Any

PAIR_FIELDS = (
    "stable_id",
    "engine_commit",
    "fixture_sha256",
    "baseline_consumer_commit",
    "baseline_prompt_sha256",
    "project_sha256",
    "source_revision",
    "provider_path",
    "transport_id",
    "model_label",
    "api_mode",
    "evidence_condition",
    "job_name",
    "build_id",
    "test_name",
    "test_source",
    "human_score_rubric_version",
    "human_score_max",
    "human_score_dimensions",
    "signal_total",
    "diagnosis_signal_total",
    "forbidden_checks_total",
)
DIRECT_FILES = (
    "backend/internal/agentanalysis/workspace.go",
    "backend/internal/agentanalysis/workspace_analysis.go",
    "backend/internal/agentanalysis/sandbox_runtime.go",
)
DIRECT_DIRS = (
    "backend/internal/analysisexecutor",
    "backend/internal/analysisstager",
    "backend/cmd/analysisexecutor",
    "backend/cmd/analysisstager",
)
FORBIDDEN_IMPORTS = (
    "backend/internal/causalcritic",
    "backend/internal/ai/evidenceplan",
)
FORBIDDEN_PHASE_SYMBOLS = (
    "EvaluateExternalDraftCritique",
    "SemanticJudge",
    "RevisionReview",
)


class ReportError(ValueError):
    pass


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--inprocess", required=True)
    parser.add_argument("--sandbox", required=True)
    parser.add_argument("--repo", required=True)
    parser.add_argument("--holdout-case", action="append", default=[])
    parser.add_argument("--expected-pairs", type=int, default=9)
    parser.add_argument("--required-repetitions", type=int, default=3)
    parser.add_argument("--output-json")
    parser.add_argument("--blind-packets")
    parser.add_argument("--blind-map")
    parser.add_argument("--blind-map-input")
    parser.add_argument("--blind-scores")
    parser.add_argument("--score-freeze")
    parser.add_argument("--reference-manifest")
    return parser.parse_args()


def read_jsonl(path: str, runtime: str) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    for line_number, line in enumerate(Path(path).read_text().splitlines(), 1):
        if not line.strip():
            continue
        try:
            value = json.loads(line)
        except json.JSONDecodeError as exc:
            raise ReportError(f"{runtime} line {line_number} is invalid JSON: {exc}") from exc
        if not isinstance(value, dict):
            raise ReportError(f"{runtime} line {line_number} must contain one JSON object")
        value["_line"] = line_number
        records.append(value)
    if not records:
        raise ReportError(f"{runtime} results are empty")
    return records


def require_string(record: dict[str, Any], field: str, runtime: str) -> str:
    value = record.get(field)
    if not isinstance(value, str) or not value.strip():
        raise ReportError(f"{runtime} line {record['_line']} field {field} must be a non-empty string")
    return value


def require_integer(record: dict[str, Any], field: str, runtime: str, minimum: int = 0) -> int:
    value = record.get(field)
    if not isinstance(value, int) or isinstance(value, bool) or value < minimum:
        raise ReportError(f"{runtime} line {record['_line']} field {field} must be an integer at least {minimum}")
    return value


def validate_record(record: dict[str, Any], runtime: str) -> tuple[str, int]:
    case_id = require_string(record, "case_id", runtime)
    repetition = require_integer(record, "repetition", runtime, 1)
    for field in (
        "stable_id",
        "engine_commit",
        "fixture_sha256",
        "baseline_consumer_commit",
        "baseline_prompt_sha256",
        "project_sha256",
        "source_revision",
        "provider_path",
        "transport_id",
        "model_label",
        "api_mode",
        "evidence_condition",
        "job_name",
        "build_id",
        "test_name",
    ):
        require_string(record, field, runtime)
    if not isinstance(record.get("test_source"), str):
        raise ReportError(f"{runtime} line {record['_line']} field test_source must be a string")
    require_integer(record, "human_score_rubric_version", runtime, 1)
    require_integer(record, "human_score_max", runtime, 1)
    dimensions = record.get("human_score_dimensions")
    if not isinstance(dimensions, list) or not dimensions or not all(isinstance(value, str) and value for value in dimensions):
        raise ReportError(f"{runtime} line {record['_line']} field human_score_dimensions must be a non-empty string array")
    require_integer(record, "elapsed_ms", runtime)
    require_integer(record, "signal_hits", runtime)
    require_integer(record, "signal_total", runtime)
    require_integer(record, "diagnosis_signal_hits", runtime)
    require_integer(record, "diagnosis_signal_total", runtime)
    require_integer(record, "forbidden_checks_passed", runtime)
    require_integer(record, "forbidden_checks_total", runtime)
    if record["signal_hits"] > record["signal_total"] or record["diagnosis_signal_hits"] > record["diagnosis_signal_total"] or record["forbidden_checks_passed"] > record["forbidden_checks_total"]:
        raise ReportError(f"{runtime} line {record['_line']} scoring numerators exceed denominators")
    if runtime == "inprocess":
        if record.get("api_mode") != "chat_completions":
            raise ReportError(f"inprocess line {record['_line']} must use chat_completions")
        if record.get("evidence_condition") != "fixture-v1":
            raise ReportError(f"inprocess line {record['_line']} must use fixture-v1 evidence")
        if not isinstance(record.get("usable"), bool):
            raise ReportError(f"inprocess line {record['_line']} field usable must be boolean")
        require_string(record, "trial_status", runtime)
        trace = record.get("trace")
        if not isinstance(trace, dict):
            raise ReportError(f"inprocess line {record['_line']} field trace must be an object")
    else:
        if record.get("api_mode") != "chat_completions" or record.get("evidence_condition") != "fixture-v1":
            raise ReportError(f"sandbox line {record['_line']} must use chat_completions and fixture-v1 evidence")
        if record.get("version") != 1 or record.get("runtime") != "agent-sandbox-opencode":
            raise ReportError(f"sandbox line {record['_line']} has an unsupported record contract")
        require_string(record, "status", runtime)
        for field in ("analysis_valid", "finalization_valid", "cleanup_completed", "source_verified"):
            if not isinstance(record.get(field), bool):
                raise ReportError(f"sandbox line {record['_line']} field {field} must be boolean")
        require_integer(record, "artifact_citation_count", runtime)
        require_integer(record, "source_citation_count", runtime)
        status = record["status"]
        if record["analysis_valid"] and not record["finalization_valid"]:
            raise ReportError(f"sandbox line {record['_line']} valid analysis lacks valid finalization")
        if status == "succeeded" and (not record["analysis_valid"] or not record["cleanup_completed"]):
            raise ReportError(f"sandbox line {record['_line']} succeeded without valid analysis and cleanup")
        if status == "cleanup_pending" and (not record["analysis_valid"] or record["cleanup_completed"]):
            raise ReportError(f"sandbox line {record['_line']} cleanup_pending lifecycle is inconsistent")
        evidence = record.get("evidence_citations", [])
        source = record.get("source_citations", [])
        if not isinstance(evidence, list) or len(evidence) != record["artifact_citation_count"]:
            raise ReportError(f"sandbox line {record['_line']} artifact citation count is inconsistent")
        if not isinstance(source, list) or len(source) != record["source_citation_count"]:
            raise ReportError(f"sandbox line {record['_line']} source citation count is inconsistent")
        if record["source_verified"] != (bool(source) and all(isinstance(citation, dict) and citation.get("verified") is True for citation in source)):
            raise ReportError(f"sandbox line {record['_line']} source verification is inconsistent")
        for field in ("task_finalized_ms", "result_available_ms", "cleanup_duration_ms", "runtime_duration_ms"):
            if field in record:
                require_integer(record, field, runtime)
        if record.get("task_finalized_ms", 0) and record.get("result_available_ms", 0) < record["task_finalized_ms"]:
            raise ReportError(f"sandbox line {record['_line']} result availability precedes finalization")
    return case_id, repetition


def index_records(records: list[dict[str, Any]], runtime: str) -> dict[tuple[str, int], dict[str, Any]]:
    indexed: dict[tuple[str, int], dict[str, Any]] = {}
    for record in records:
        key = validate_record(record, runtime)
        if key in indexed:
            first = indexed[key]["_line"]
            raise ReportError(f"duplicate {runtime} record {key[0]}/rep-{key[1]:02d}; first seen on line {first}")
        indexed[key] = record
    return indexed


def validate_pairs(inprocess: dict[tuple[str, int], dict[str, Any]], sandbox: dict[tuple[str, int], dict[str, Any]]) -> list[tuple[str, int]]:
    if set(inprocess) != set(sandbox):
        missing_sandbox = sorted(set(inprocess) - set(sandbox))
        missing_inprocess = sorted(set(sandbox) - set(inprocess))
        raise ReportError(f"unpaired benchmark records: missing_sandbox={missing_sandbox} missing_inprocess={missing_inprocess}")
    keys = sorted(inprocess)
    for key in keys:
        left, right = inprocess[key], sandbox[key]
        for field in PAIR_FIELDS:
            if left.get(field) != right.get(field):
                raise ReportError(f"paired record {key[0]}/rep-{key[1]:02d} differs in {field}")
    return keys


def percentile(values: list[int], quantile: float) -> int | None:
    if not values:
        return None
    ordered = sorted(values)
    index = max(math.ceil(quantile * len(ordered)) - 1, 0)
    return ordered[index]


def rate(numerator: int, denominator: int) -> float | None:
    if denominator == 0:
        return None
    return round(numerator / denominator, 4)


def inprocess_metrics(records: list[dict[str, Any]]) -> dict[str, Any]:
    valid = [record for record in records if record["usable"]]
    statuses = [record["trial_status"] for record in records]
    traces = [record["trace"] for record in records]
    citation_trials = sum(bool(record.get("evidence_citations")) for record in valid)
    source_trials = sum(bool(record.get("file_links")) for record in valid)
    token_trials = sum(
        isinstance(trace.get("input_tokens"), int)
        and isinstance(trace.get("output_tokens"), int)
        and (trace.get("input_tokens", 0) > 0 or trace.get("output_tokens", 0) > 0)
        for trace in traces
    )
    return {
        "trials": len(records),
        "valid_trials": len(valid),
        "valid_rate": rate(len(valid), len(records)),
        "invalid_trials": sum(status == "contract_violation" for status in statuses),
        "no_result_trials": sum(status == "no_result" for status in statuses),
        "runtime_failure_trials": sum(status in ("runtime_failure", "timeout") for status in statuses),
        "artifact_citation_trials": citation_trials,
        "artifact_citation_rate": rate(citation_trials, len(valid)),
        "source_grounded_trials": source_trials,
        "source_grounded_rate": rate(source_trials, len(valid)),
        "signal_hits": sum(record["signal_hits"] for record in records),
        "signal_total": sum(record["signal_total"] for record in records),
        "signal_rate": rate(sum(record["signal_hits"] for record in records), sum(record["signal_total"] for record in records)),
        "diagnosis_signal_hits": sum(record["diagnosis_signal_hits"] for record in records),
        "diagnosis_signal_total": sum(record["diagnosis_signal_total"] for record in records),
        "diagnosis_signal_rate": rate(sum(record["diagnosis_signal_hits"] for record in records), sum(record["diagnosis_signal_total"] for record in records)),
        "transient_correct_trials": sum(record.get("transient_classification_correct") is True for record in records),
        "transient_evaluated_trials": sum(isinstance(record.get("transient_classification_correct"), bool) for record in records),
        "transient_correct_rate": rate(
            sum(record.get("transient_classification_correct") is True for record in records),
            sum(isinstance(record.get("transient_classification_correct"), bool) for record in records),
        ),
        "forbidden_checks_passed": sum(max(int(record.get("forbidden_checks_passed", 0)), 0) for record in records),
        "forbidden_checks_total": sum(max(int(record.get("forbidden_checks_total", 0)), 0) for record in records),
        "forbidden_checks_pass_rate": rate(
            sum(max(int(record.get("forbidden_checks_passed", 0)), 0) for record in records),
            sum(max(int(record.get("forbidden_checks_total", 0)), 0) for record in records),
        ),
        "latency_ms": latency_metrics(records),
        "model_requests": sum(max(int(trace.get("model_requests", 0)), 0) for trace in traces),
        "provider_attempts": sum(max(int(trace.get("provider_attempts", 0)), 0) for trace in traces),
        "input_tokens": sum(max(int(trace.get("input_tokens", 0)), 0) for trace in traces),
        "cached_input_tokens": sum(max(int(trace.get("cached_input_tokens", 0)), 0) for trace in traces),
        "output_tokens": sum(max(int(trace.get("output_tokens", 0)), 0) for trace in traces),
        "token_usage_trials": token_trials,
        "token_usage_coverage": rate(token_trials, len(records)),
        "cost_available_trials": 0,
        "cost_coverage": 0.0,
        "cost_status": "unavailable_from_inprocess_benchmark_record",
        "cleanup_status": "not_applicable_inprocess",
    }


def sandbox_metrics(records: list[dict[str, Any]]) -> dict[str, Any]:
    valid = [record for record in records if record["analysis_valid"]]
    statuses = [record["status"] for record in records]
    citation_trials = sum(record["artifact_citation_count"] > 0 for record in valid)
    source_trials = sum(record["source_verified"] for record in valid)
    token_trials = sum(record.get("token_usage_available") is True for record in records)
    cost_trials = sum(record.get("cost_available") is True for record in records)
    return {
        "trials": len(records),
        "valid_trials": len(valid),
        "valid_rate": rate(len(valid), len(records)),
        "invalid_trials": sum(status == "invalid_result" for status in statuses),
        "no_result_trials": sum(status == "no_result" for status in statuses),
        "runtime_failure_trials": sum(status in ("runtime_failure", "timeout", "cancellation") for status in statuses),
        "cleanup_pending_trials": sum(status == "cleanup_pending" for status in statuses),
        "artifact_citation_trials": citation_trials,
        "artifact_citation_rate": rate(citation_trials, len(valid)),
        "source_grounded_trials": source_trials,
        "source_grounded_rate": rate(source_trials, len(valid)),
        "signal_hits": sum(record["signal_hits"] for record in records),
        "signal_total": sum(record["signal_total"] for record in records),
        "signal_rate": rate(sum(record["signal_hits"] for record in records), sum(record["signal_total"] for record in records)),
        "diagnosis_signal_hits": sum(record["diagnosis_signal_hits"] for record in records),
        "diagnosis_signal_total": sum(record["diagnosis_signal_total"] for record in records),
        "diagnosis_signal_rate": rate(sum(record["diagnosis_signal_hits"] for record in records), sum(record["diagnosis_signal_total"] for record in records)),
        "transient_correct_trials": sum(record.get("transient_classification_correct") is True for record in records),
        "transient_evaluated_trials": sum(isinstance(record.get("transient_classification_correct"), bool) for record in records),
        "transient_correct_rate": rate(
            sum(record.get("transient_classification_correct") is True for record in records),
            sum(isinstance(record.get("transient_classification_correct"), bool) for record in records),
        ),
        "forbidden_checks_passed": sum(max(int(record.get("forbidden_checks_passed", 0)), 0) for record in records),
        "forbidden_checks_total": sum(max(int(record.get("forbidden_checks_total", 0)), 0) for record in records),
        "forbidden_checks_pass_rate": rate(
            sum(max(int(record.get("forbidden_checks_passed", 0)), 0) for record in records),
            sum(max(int(record.get("forbidden_checks_total", 0)), 0) for record in records),
        ),
        "latency_ms": latency_metrics(records),
        "runtime_duration_ms": duration_metrics([record.get("runtime_duration_ms", 0) for record in records]),
        "finalization_valid_trials": sum(record["finalization_valid"] for record in records),
        "finalization_valid_rate": rate(sum(record["finalization_valid"] for record in records), len(records)),
        "cleanup_completed_trials": sum(record["cleanup_completed"] for record in records),
        "cleanup_completed_rate": rate(sum(record["cleanup_completed"] for record in records), len(records)),
        "model_requests": sum(record.get("model_requests", 0) for record in records),
        "input_tokens": sum(record.get("input_tokens", 0) for record in records),
        "cached_input_tokens": sum(record.get("cached_input_tokens", 0) for record in records),
        "output_tokens": sum(record.get("output_tokens", 0) for record in records),
        "token_usage_trials": token_trials,
        "token_usage_coverage": rate(token_trials, len(records)),
        "cost_available_trials": cost_trials,
        "cost_coverage": rate(cost_trials, len(records)),
        "cost_usd_total": sandbox_cost_total(records),
        "cost_statuses": sorted({record.get("usage_status", "unknown") for record in records}),
    }



def sandbox_cost_total(records: list[dict[str, Any]]) -> str | None:
    total = Decimal("0")
    found = False
    for record in records:
        value = record.get("cost_usd")
        if not isinstance(value, str) or not value.strip():
            continue
        try:
            parsed = Decimal(value)
        except InvalidOperation as exc:
            raise ReportError(f"sandbox line {record['_line']} field cost_usd is invalid") from exc
        if parsed < 0:
            raise ReportError(f"sandbox line {record['_line']} field cost_usd must be non-negative")
        total += parsed
        found = True
    return format(total, "f") if found else None

def latency_metrics(records: list[dict[str, Any]]) -> dict[str, int | None]:
    return duration_metrics([record["elapsed_ms"] for record in records])


def duration_metrics(values: list[int]) -> dict[str, int | None]:
    clean = [max(int(value), 0) for value in values]
    return {
        "min": min(clean) if clean else None,
        "median": int(statistics.median(clean)) if clean else None,
        "p95": percentile(clean, 0.95),
        "max": max(clean) if clean else None,
    }


def production_files(repo: Path, explicit: tuple[str, ...], directories: tuple[str, ...]) -> list[Path]:
    files = [repo / path for path in explicit]
    for directory in directories:
        files.extend(sorted(path for path in (repo / directory).rglob("*.go") if not path.name.endswith("_test.go")))
    missing = [str(path.relative_to(repo)) for path in files if not path.is_file()]
    if missing:
        raise ReportError(f"simplicity files are missing: {missing}")
    return sorted(set(files))


def code_lines(path: Path) -> int:
    count = 0
    for line in path.read_text().splitlines():
        stripped = line.strip()
        if stripped and not stripped.startswith("//"):
            count += 1
    return count


def simplicity_metrics(repo_path: str) -> dict[str, Any]:
    repo = Path(repo_path).resolve()
    direct = production_files(repo, DIRECT_FILES, DIRECT_DIRS)
    inprocess = sorted(path for path in (repo / "backend/internal/ai").rglob("*.go") if not path.name.endswith("_test.go"))
    if not inprocess:
        raise ReportError("in-process analyzer production files are missing")
    direct_loc = sum(code_lines(path) for path in direct)
    inprocess_loc = sum(code_lines(path) for path in inprocess)
    direct_text = "\n".join(path.read_text() for path in direct)
    forbidden = [value for value in FORBIDDEN_IMPORTS + FORBIDDEN_PHASE_SYMBOLS if value in direct_text]
    ratio = round(direct_loc / inprocess_loc, 4) if inprocess_loc else None
    passed = ratio is not None and ratio <= 0.5 and not forbidden
    return {
        "criterion": {
            "max_direct_to_inprocess_loc_ratio": 0.5,
            "required_model_sessions": 1,
            "forbidden_dashboard_owned_phases": ["critic", "evidence_digest", "revision", "model_evidence_planner", "case_specific_rules"],
        },
        "direct_analyzer": {
            "production_files": len(direct),
            "production_packages": len({str(path.parent.relative_to(repo)) for path in direct}),
            "production_loc": direct_loc,
            "model_sessions": 1,
            "forbidden_symbols_found": forbidden,
        },
        "inprocess_analyzer": {
            "production_files": len(inprocess),
            "production_packages": len({str(path.parent.relative_to(repo)) for path in inprocess}),
            "production_loc": inprocess_loc,
        },
        "direct_to_inprocess_loc_ratio": ratio,
        "passed": passed,
    }


def evaluate_criteria(inprocess: dict[str, Any], sandbox: dict[str, Any], simplicity: dict[str, Any], pairs: int, expected_pairs: int, holdouts_ok: bool, blind_quality: dict[str, Any]) -> dict[str, Any]:
    automatic_quality = (
        sandbox["signal_rate"] is not None
        and inprocess["signal_rate"] is not None
        and sandbox["diagnosis_signal_rate"] is not None
        and inprocess["diagnosis_signal_rate"] is not None
        and sandbox["signal_rate"] + 0.05 >= inprocess["signal_rate"]
        and sandbox["diagnosis_signal_rate"] + 0.05 >= inprocess["diagnosis_signal_rate"]
        and (sandbox["valid_rate"] or 0) >= 0.95
        and (sandbox["artifact_citation_rate"] or 0) >= 0.95
        and (sandbox["source_grounded_rate"] or 0) >= 0.8
        and (sandbox["transient_correct_rate"] or 0) + 0.05 >= (inprocess["transient_correct_rate"] or 0)
        and (sandbox["forbidden_checks_pass_rate"] or 0) + 0.05 >= (inprocess["forbidden_checks_pass_rate"] or 0)
        and (sandbox["cleanup_completed_rate"] or 0) == 1.0
    )
    telemetry = (
        (inprocess["token_usage_coverage"] or 0) >= 0.95
        and (sandbox["token_usage_coverage"] or 0) >= 0.95
        and (inprocess["cost_coverage"] or 0) >= 0.95
        and (sandbox["cost_coverage"] or 0) >= 0.95
    )
    evidence = pairs == expected_pairs and holdouts_ok
    blind_complete = blind_quality.get("complete") is True
    blind_passed = blind_quality.get("passed") is True
    quality = automatic_quality and blind_complete and blind_passed
    replacement_ready = evidence and quality and telemetry and simplicity["passed"]
    if not evidence:
        recommendation = "insufficient_evidence"
    elif not automatic_quality or not simplicity["passed"]:
        recommendation = "do_not_replace"
    elif not blind_complete:
        recommendation = "continue_experiment_not_replacement"
    elif not blind_passed:
        recommendation = "do_not_replace"
    elif not telemetry:
        recommendation = "continue_experiment_not_replacement"
    else:
        recommendation = "replacement_candidate"
    return {
        "evidence_complete": evidence,
        "automatic_quality_passed": automatic_quality,
        "blind_quality_complete": blind_complete,
        "blind_quality_passed": blind_passed,
        "quality_passed": quality,
        "telemetry_passed": telemetry,
        "simplicity_passed": simplicity["passed"],
        "replacement_ready": replacement_ready,
        "recommendation": recommendation,
    }


def validate_holdouts(keys: list[tuple[str, int]], holdouts: list[str], required_repetitions: int) -> tuple[bool, dict[str, int]]:
    counts = {case_id: sum(key[0] == case_id for key in keys) for case_id in holdouts}
    if required_repetitions < 1:
        raise ReportError("required repetitions must be positive")
    return bool(holdouts) and all(count == required_repetitions for count in counts.values()), counts


def write_private_json(path: str, value: Any) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    target.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    os.chmod(target, 0o600)



def nonnegative_int(value: Any) -> int:
    if not isinstance(value, int) or isinstance(value, bool):
        return 0
    return max(value, 0)

def normalized_citations(record: dict[str, Any], field: str) -> list[dict[str, Any]]:
    citations = record.get(field, [])
    if not isinstance(citations, list):
        return []
    normalized = []
    for citation in citations:
        if not isinstance(citation, dict) or not isinstance(citation.get("path"), str):
            continue
        normalized.append({
            "path": citation["path"],
            "line_start": nonnegative_int(citation.get("line_start", 0)),
            "line_end": nonnegative_int(citation.get("line_end", 0)),
        })
    return normalized


def normalized_blind_analysis(record: dict[str, Any]) -> dict[str, Any]:
    relevant = record.get("relevant_files")
    if not isinstance(relevant, list):
        links = record.get("file_links", {})
        relevant = sorted(links) if isinstance(links, dict) else []
    source = [citation["path"] for citation in normalized_citations(record, "source_citations")]
    if not source:
        links = record.get("file_links", {})
        if isinstance(links, dict):
            source = sorted(links)
    unresolved = record.get("unresolved_details", [])
    if not isinstance(unresolved, list):
        unresolved = []
    return {
        "summary": record.get("summary", ""),
        "root_cause": record.get("root_cause", ""),
        "suggested_fix": record.get("suggested_fix", ""),
        "severity": record.get("severity", ""),
        "is_transient": record.get("is_transient"),
        "relevant_files": [value for value in relevant if isinstance(value, str)],
        "evidence_citations": normalized_citations(record, "evidence_citations"),
        "source_references": source,
        "unresolved_details": [value for value in unresolved if isinstance(value, str)],
    }


def blind_analysis_sha256(record: dict[str, Any]) -> str:
    data = json.dumps(normalized_blind_analysis(record), sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(data).hexdigest()


def load_case_references(path: str | None, case_ids: set[str]) -> tuple[dict[str, dict[str, Any]], str]:
    if not path:
        raise ReportError("--reference-manifest is required for blinded packets and scores")
    document = json.loads(Path(path).read_text())
    if document.get("version") != 1 or not isinstance(document.get("cases"), dict):
        raise ReportError("causal reference manifest is invalid")
    references: dict[str, dict[str, Any]] = {}
    for case_id in sorted(case_ids):
        value = document["cases"].get(case_id)
        if not isinstance(value, dict) or not isinstance(value.get("reference_diagnosis"), str):
            raise ReportError(f"causal reference is missing for {case_id}")
        chain = value.get("required_chain")
        if not isinstance(chain, list) or not chain:
            raise ReportError(f"causal reference required_chain is missing for {case_id}")
        ids = set()
        for item in chain:
            if not isinstance(item, dict) or not isinstance(item.get("id"), str) or not isinstance(item.get("text"), str) or item["id"] in ids:
                raise ReportError(f"causal reference required_chain is invalid for {case_id}")
            ids.add(item["id"])
        noise = value.get("downstream_noise", [])
        if not isinstance(noise, list) or not all(isinstance(item, str) for item in noise):
            raise ReportError(f"causal reference downstream_noise is invalid for {case_id}")
        references[case_id] = {"reference_diagnosis": value["reference_diagnosis"], "required_chain": chain, "downstream_noise": noise}
    encoded = json.dumps({"version": 1, "cases": references}, sort_keys=True, separators=(",", ":")).encode()
    return references, hashlib.sha256(encoded).hexdigest()


def causal_scoring_rubric(reference: dict[str, Any]) -> dict[str, Any]:
    return {
        "diagnosis_full_credit_requires": [item["id"] for item in reference["required_chain"]],
        "full_credit_rule": "Diagnosis score 2 requires the initiating cause, every required causal link, and no downstream-only symptom presented as primary.",
        "alignment_values": ["aligned", "partial", "missing", "contradicted"],
    }


def packet_set_sha256(packets: list[dict[str, Any]]) -> str:
    data = json.dumps({"version": 2, "packets": packets}, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(data).hexdigest()


def blind_packet(packet_id: str, case_id: str, repetition: int, arm: str, record: dict[str, Any], reference: dict[str, Any]) -> dict[str, Any]:
    packet = {"packet_id": packet_id, "case_id": case_id, "repetition": repetition, "arm": arm, "causal_reference": reference, "scoring_rubric": causal_scoring_rubric(reference)}
    packet.update(normalized_blind_analysis(record))
    return packet


def write_blind_packets(path: str, map_path: str, references: dict[str, dict[str, Any]], reference_hash: str, keys: list[tuple[str, int]], inprocess: dict[tuple[str, int], dict[str, Any]], sandbox: dict[tuple[str, int], dict[str, Any]]) -> None:
    packets: list[dict[str, Any]] = []
    mapping: list[dict[str, Any]] = []
    chooser = secrets.SystemRandom()
    for case_id, repetition in keys:
        rows = [("inprocess", inprocess[(case_id, repetition)]), ("agent_sandbox", sandbox[(case_id, repetition)])]
        chooser.shuffle(rows)
        packet_id = f"{case_id}-rep-{repetition:02d}"
        for index, (runtime, record) in enumerate(rows):
            blind_arm = chr(ord("A") + index)
            packets.append(blind_packet(packet_id, case_id, repetition, blind_arm, record, references[case_id]))
            mapping.append({"packet_id": packet_id, "arm": blind_arm, "runtime": runtime, "analysis_sha256": blind_analysis_sha256(record)})
    packet_hash = packet_set_sha256(packets)
    write_private_json(path, {"version": 2, "packet_set_sha256": packet_hash, "reference_set_sha256": reference_hash, "packets": packets})
    write_private_json(map_path, {"version": 2, "packet_set_sha256": packet_hash, "reference_set_sha256": reference_hash, "mapping": mapping})


def load_blind_quality(map_path: str, scores_path: str, freeze_path: str, references: dict[str, dict[str, Any]], reference_hash: str, keys: list[tuple[str, int]], inprocess: dict[tuple[str, int], dict[str, Any]], sandbox: dict[tuple[str, int], dict[str, Any]], rubric_version: int, score_max: int, dimensions: list[str]) -> dict[str, Any]:
    scores_doc = json.loads(Path(scores_path).read_text())
    freeze_doc = json.loads(Path(freeze_path).read_text())
    score_hash = hashlib.sha256(json.dumps(scores_doc, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    if freeze_doc.get("version") != 1 or freeze_doc.get("score_set_sha256") != score_hash or freeze_doc.get("packet_set_sha256") != scores_doc.get("packet_set_sha256") or freeze_doc.get("reference_set_sha256") != scores_doc.get("reference_set_sha256"):
        raise ReportError("blind scores do not match the pre-unblinding score freeze")
    mapping_doc = json.loads(Path(map_path).read_text())
    if mapping_doc.get("version") != 2 or scores_doc.get("version") != 2:
        raise ReportError("blind map and score versions must be 2")
    if mapping_doc.get("reference_set_sha256") != reference_hash or scores_doc.get("reference_set_sha256") != reference_hash:
        raise ReportError("blind scores are not bound to this causal reference set")
    packet_hash = mapping_doc.get("packet_set_sha256")
    if not isinstance(packet_hash, str) or len(packet_hash) != 64 or scores_doc.get("packet_set_sha256") != packet_hash:
        raise ReportError("blind scores are not bound to this packet set")
    if scores_doc.get("rubric_version") != rubric_version or scores_doc.get("score_max") != score_max or scores_doc.get("dimensions") != dimensions:
        raise ReportError("blind score rubric identity does not match benchmark records")
    if "diagnosis" not in dimensions:
        raise ReportError("blind score rubric must include diagnosis")
    mapping = mapping_doc.get("mapping")
    scores = scores_doc.get("scores")
    if not isinstance(mapping, list) or not isinstance(scores, list):
        raise ReportError("blind map and scores must contain arrays")
    expected_packets = {f"{case_id}-rep-{repetition:02d}" for case_id, repetition in keys}
    case_by_packet = {f"{case_id}-rep-{repetition:02d}": case_id for case_id, repetition in keys}
    record_hashes: dict[tuple[str, str], str] = {}
    for case_id, repetition in keys:
        packet_id = f"{case_id}-rep-{repetition:02d}"
        record_hashes[(packet_id, "inprocess")] = blind_analysis_sha256(inprocess[(case_id, repetition)])
        record_hashes[(packet_id, "agent_sandbox")] = blind_analysis_sha256(sandbox[(case_id, repetition)])
    runtime_by_key: dict[tuple[str, str], str] = {}
    for item in mapping:
        if not isinstance(item, dict):
            raise ReportError("blind map entry must be an object")
        key = (item.get("packet_id"), item.get("arm"))
        runtime = item.get("runtime")
        analysis_hash = item.get("analysis_sha256")
        if key[0] not in expected_packets or key[1] not in ("A", "B") or runtime not in ("inprocess", "agent_sandbox") or key in runtime_by_key:
            raise ReportError("blind map entry is invalid or duplicated")
        if analysis_hash != record_hashes.get((key[0], runtime)):
            raise ReportError("blind map analysis hash does not match benchmark results")
        runtime_by_key[key] = runtime
    if len(runtime_by_key) != len(keys) * 2:
        raise ReportError("blind map is incomplete")
    reconstructed_packets = []
    for case_id, repetition in keys:
        packet_id = f"{case_id}-rep-{repetition:02d}"
        runtimes = {runtime_by_key.get((packet_id, arm)) for arm in ("A", "B")}
        if runtimes != {"inprocess", "agent_sandbox"}:
            raise ReportError("blind map must assign one arm to each runtime")
        for arm in ("A", "B"):
            runtime = runtime_by_key[(packet_id, arm)]
            record = inprocess[(case_id, repetition)] if runtime == "inprocess" else sandbox[(case_id, repetition)]
            reconstructed_packets.append(blind_packet(packet_id, case_id, repetition, arm, record, references[case_id]))
    if packet_set_sha256(reconstructed_packets) != packet_hash:
        raise ReportError("blind map packet_set_sha256 does not match its randomized assignment")
    totals = {"inprocess": [], "agent_sandbox": []}
    dimension_values = {runtime: {dimension: [] for dimension in dimensions} for runtime in totals}
    seen: set[tuple[str, str]] = set()
    for item in scores:
        if not isinstance(item, dict):
            raise ReportError("blind score entry must be an object")
        key = (item.get("packet_id"), item.get("arm"))
        if key not in runtime_by_key or key in seen:
            raise ReportError("blind score entry is invalid or duplicated")
        values = item.get("scores")
        if not isinstance(values, dict) or set(values) != set(dimensions):
            raise ReportError("blind score dimensions are incomplete")
        assessment = item.get("causal_assessment")
        reference = references[case_by_packet[key[0]]]
        required_ids = {entry["id"] for entry in reference["required_chain"]}
        if not isinstance(assessment, dict) or assessment.get("alignment") not in ("aligned", "partial", "missing", "contradicted") or not isinstance(assessment.get("initiating_cause_found"), bool) or not isinstance(assessment.get("downstream_treated_as_primary"), bool) or not isinstance(assessment.get("required_chain_coverage"), list) or not all(isinstance(value, str) for value in assessment["required_chain_coverage"]):
            raise ReportError("blind score causal assessment is incomplete")
        coverage = set(assessment["required_chain_coverage"])
        if not coverage.issubset(required_ids):
            raise ReportError("blind score causal coverage contains an unknown reference id")
        diagnosis_value = values.get("diagnosis")
        if diagnosis_value == 2 and (assessment["alignment"] != "aligned" or not assessment["initiating_cause_found"] or assessment["downstream_treated_as_primary"] or coverage != required_ids):
            raise ReportError("full diagnosis credit requires complete reference-aligned causal coverage")
        total = 0
        runtime = runtime_by_key[key]
        for dimension in dimensions:
            value = values[dimension]
            if not isinstance(value, int) or isinstance(value, bool) or value < 0 or value > 2:
                raise ReportError("blind score dimensions must be integers from 0 to 2")
            total += value
            dimension_values[runtime][dimension].append(value)
        if total > score_max:
            raise ReportError("blind score exceeds score_max")
        totals[runtime].append(total)
        seen.add(key)
    if seen != set(runtime_by_key):
        raise ReportError("blind scores are incomplete")
    summary = {}
    for runtime in totals:
        summary[runtime] = {
            "trials": len(totals[runtime]),
            "average_total": round(statistics.mean(totals[runtime]), 4),
            "median_total": round(statistics.median(totals[runtime]), 4),
            "dimension_averages": {
                dimension: round(statistics.mean(dimension_values[runtime][dimension]), 4)
                for dimension in dimensions
            },
        }
    passed = (
        summary["agent_sandbox"]["average_total"] + 0.5 >= summary["inprocess"]["average_total"]
        and summary["agent_sandbox"]["dimension_averages"]["diagnosis"] + 0.2
        >= summary["inprocess"]["dimension_averages"]["diagnosis"]
    )
    return {"status": "scored", "complete": True, "passed": passed, "rubric_version": rubric_version, "score_max": score_max, "dimensions": dimensions, "packet_set_sha256": packet_hash, "reference_set_sha256": reference_hash, "score_set_sha256": score_hash, "arms": summary}


def build_report(args: argparse.Namespace) -> dict[str, Any]:
    inprocess_records = read_jsonl(args.inprocess, "inprocess")
    sandbox_records = read_jsonl(args.sandbox, "sandbox")
    inprocess = index_records(inprocess_records, "inprocess")
    sandbox = index_records(sandbox_records, "sandbox")
    keys = validate_pairs(inprocess, sandbox)
    holdouts_ok, holdout_counts = validate_holdouts(keys, args.holdout_case, args.required_repetitions)
    inprocess_summary = inprocess_metrics([inprocess[key] for key in keys])
    sandbox_summary = sandbox_metrics([sandbox[key] for key in keys])
    simplicity = simplicity_metrics(args.repo)
    rubric_version = inprocess[keys[0]]["human_score_rubric_version"]
    score_max = inprocess[keys[0]]["human_score_max"]
    dimensions = inprocess[keys[0]]["human_score_dimensions"]
    references: dict[str, dict[str, Any]] = {}
    reference_hash = ""
    if args.blind_packets or args.blind_map or args.blind_scores:
        references, reference_hash = load_case_references(args.reference_manifest, {key[0] for key in keys})
    blind_quality = {"status": "not_scored", "complete": False, "passed": False, "rubric_version": rubric_version, "score_max": score_max, "dimensions": dimensions}
    if args.blind_scores:
        if not args.blind_map_input or not args.score_freeze:
            raise ReportError("--blind-scores requires --blind-map-input and --score-freeze")
        if args.blind_map:
            raise ReportError("blind map generation and score unblinding must be separate operations")
        blind_quality = load_blind_quality(args.blind_map_input, args.blind_scores, args.score_freeze, references, reference_hash, keys, inprocess, sandbox, rubric_version, score_max, dimensions)
    criteria = evaluate_criteria(inprocess_summary, sandbox_summary, simplicity, len(keys), args.expected_pairs, holdouts_ok, blind_quality)
    report = {
        "version": 1,
        "pair_count": len(keys),
        "cases": sorted({key[0] for key in keys}),
        "repetitions": sorted({key[1] for key in keys}),
        "holdout_repetitions": holdout_counts,
        "holdouts_complete": holdouts_ok,
        "inprocess": inprocess_summary,
        "agent_sandbox": sandbox_summary,
        "quality_delta": {
            "signal_rate": round((sandbox_summary["signal_rate"] or 0) - (inprocess_summary["signal_rate"] or 0), 4),
            "diagnosis_signal_rate": round((sandbox_summary["diagnosis_signal_rate"] or 0) - (inprocess_summary["diagnosis_signal_rate"] or 0), 4),
            "valid_rate": round((sandbox_summary["valid_rate"] or 0) - (inprocess_summary["valid_rate"] or 0), 4),
        },
        "simplicity": simplicity,
        "blind_quality": blind_quality,
        "criteria": criteria,
        "limitations": [
            "cost comparison remains unavailable until both runtimes report per-trial cost",
            "replacement quality remains incomplete until independent blind scores are supplied",
            "automatic regex signals do not replace causal-quality review",
        ],
    }
    if args.blind_packets or args.blind_map:
        if not args.blind_packets or not args.blind_map:
            raise ReportError("--blind-packets and --blind-map must be provided together")
        write_blind_packets(args.blind_packets, args.blind_map, references, reference_hash, keys, inprocess, sandbox)
        report["blind_packets_written"] = True
    return report


def main() -> int:
    args = parse_args()
    try:
        report = build_report(args)
    except (OSError, ReportError) as exc:
        print(f"benchmark report error: {exc}", file=sys.stderr)
        return 2
    encoded = json.dumps(report, indent=2, sort_keys=True) + "\n"
    if args.output_json:
        write_private_json(args.output_json, report)
    sys.stdout.write(encoded)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
