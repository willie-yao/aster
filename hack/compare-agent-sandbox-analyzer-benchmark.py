#!/usr/bin/env python3
"""Compare paired in-process and Agent Sandbox analyzer benchmark records."""

from __future__ import annotations

import argparse
from decimal import Decimal, InvalidOperation
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import re
import secrets
import statistics
import subprocess
import sys
from typing import Any

PAIR_FIELDS = (
    "stable_id",
    "engine_commit",
    "benchmark_manifest_sha256",
    "fixture_sha256",
    "baseline_consumer_commit",
    "baseline_prompt_sha256",
    "project_sha256",
    "effective_prompt_sha256",
    "skill_set_hash",
    "effective_input_sha256",
    "comparison_input_sha256",
    "source_revision",
    "provider_path",
    "provider_config_sha256",
    "transport_id",
    "model_label",
    "api_mode",
    "reasoning_effort",
    "model_context_tokens",
    "model_output_tokens",
    "pricing",
    "evidence_condition",
    "evidence_mode",
    "source_expectation_sha256",
    "expected_source_ranges",
    "source_read_coverage_total",
    "source_signal_total",
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
MATRIX_FIELDS = (
    "engine_commit",
    "benchmark_manifest_sha256",
    "baseline_consumer_commit",
    "baseline_prompt_sha256",
    "project_sha256",
    "effective_prompt_sha256",
    "skill_set_hash",
    "provider_path",
    "provider_config_sha256",
    "transport_id",
    "model_label",
    "api_mode",
    "reasoning_effort",
    "model_context_tokens",
    "model_output_tokens",
    "pricing",
    "evidence_condition",
    "human_score_rubric_version",
    "human_score_max",
    "human_score_dimensions",
)
DIRECT_FILES = (
    "backend/internal/agentanalysis/workspace.go",
    "backend/internal/agentanalysis/workspace_analysis.go",
    "backend/internal/agentanalysis/workspace_prepare.go",
    "backend/internal/agentanalysis/workspace_publish.go",
    "backend/internal/agentanalysis/workspace_shadow.go",
    "backend/internal/agentanalysis/json_validation.go",
    "backend/internal/agentanalysis/result_bounds.go",
    "backend/internal/agentanalysis/sandbox_runtime.go",
    "backend/internal/fetcher/shadow_analysis.go",
)
DIRECT_DIRS = (
    "backend/internal/analysispublisher",
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
EVIDENCE_MODES = ("artifact_only", "artifact_and_source")
INPROCESS_STATUSES = ("valid_result", "no_result", "invalid_result", "contract_violation", "timeout", "runtime_failure")
SANDBOX_STATUSES = ("succeeded", "cleanup_pending", "no_result", "invalid_result", "timeout", "cancellation", "runtime_failure")
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
IMAGE_RE = re.compile(r"^[^\s@]+@sha256:[0-9a-f]{64}$")


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


def validate_pricing(record: dict[str, Any], runtime: str) -> dict[str, str]:
    pricing = record.get("pricing")
    required = ("currency", "input_per_million", "cached_input_per_million", "output_per_million", "sha256")
    if not isinstance(pricing, dict) or set(pricing) != set(required):
        raise ReportError(f"{runtime} line {record['_line']} pricing identity is incomplete")
    if any(not isinstance(pricing[field], str) or not pricing[field] for field in required):
        raise ReportError(f"{runtime} line {record['_line']} pricing identity is invalid")
    if not re.fullmatch(r"[A-Z]{3}", pricing["currency"]) or not SHA256_RE.fullmatch(pricing["sha256"]):
        raise ReportError(f"{runtime} line {record['_line']} pricing currency or hash is invalid")
    for field in ("input_per_million", "cached_input_per_million", "output_per_million"):
        try:
            value = Decimal(pricing[field])
        except InvalidOperation as exc:
            raise ReportError(f"{runtime} line {record['_line']} pricing rate is invalid") from exc
        if value < 0 or not value.is_finite():
            raise ReportError(f"{runtime} line {record['_line']} pricing rate must be finite and non-negative")
    canonical = {field: pricing[field] for field in required if field != "sha256"}
    if hashlib.sha256(json.dumps(canonical, sort_keys=True, separators=(",", ":")).encode()).hexdigest() != pricing["sha256"]:
        raise ReportError(f"{runtime} line {record['_line']} pricing hash does not match the rates")
    return pricing


def source_range_key(value: dict[str, Any]) -> tuple[Any, ...]:
    return (value["repository"], value["revision"], value["path"], value["line_start"], value["line_end"])


def validate_source_range(value: Any, runtime: str, line: int) -> None:
    if not isinstance(value, dict) or set(value) != {"repository", "revision", "path", "line_start", "line_end"}:
        raise ReportError(f"{runtime} line {line} source range is invalid")
    repository, revision, path = value["repository"], value["revision"], value["path"]
    start, end = value["line_start"], value["line_end"]
    if not isinstance(repository, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository) or not isinstance(revision, str) or not COMMIT_RE.fullmatch(revision):
        raise ReportError(f"{runtime} line {line} source range identity is invalid")
    if not isinstance(path, str) or not path or str(PurePosixPath(path)) != path or path.startswith("/") or path == "." or path.startswith("../") or "\\" in path or ".." in PurePosixPath(path).parts:
        raise ReportError(f"{runtime} line {line} source range path is invalid")
    if not isinstance(start, int) or isinstance(start, bool) or not isinstance(end, int) or isinstance(end, bool) or start < 1 or end < start or end-start+1 > 2000:
        raise ReportError(f"{runtime} line {line} source range lines are invalid")


def validate_source_evidence(record: dict[str, Any], runtime: str) -> None:
    expected = record.get("expected_source_ranges")
    reads = record.get("source_read_ranges")
    citations = record.get("source_citations")
    if not isinstance(expected, list) or not isinstance(reads, list) or not isinstance(citations, list):
        raise ReportError(f"{runtime} line {record['_line']} source evidence fields must be arrays")
    for name, values, limit in (("expected_source_ranges", expected, 8), ("source_read_ranges", reads, 512)):
        if len(values) > limit:
            raise ReportError(f"{runtime} line {record['_line']} {name} exceeds the bound")
        keys = []
        for value in values:
            payload = value if name == "expected_source_ranges" else {field: value.get(field) for field in ("repository", "revision", "path", "line_start", "line_end")} if isinstance(value, dict) else value
            validate_source_range(payload, runtime, record["_line"])
            keys.append(source_range_key(payload) + (() if name == "expected_source_ranges" else (value.get("tool"), value.get("outcome"))))
            if name == "source_read_ranges" and (set(value) != {"repository", "revision", "path", "line_start", "line_end", "tool", "outcome"} or value.get("tool") not in ("read_repo_file", "grep_repo", "read", "grep") or value.get("outcome") != "succeeded"):
                raise ReportError(f"{runtime} line {record['_line']} source read is invalid")
        if keys != sorted(keys) or len(keys) != len(set(keys)):
            raise ReportError(f"{runtime} line {record['_line']} {name} is not canonical")
    if record.get("source_read_count") != len(reads):
        raise ReportError(f"{runtime} line {record['_line']} source read count is inconsistent")
    hits = source_read_coverage(expected, reads)
    if record.get("source_read_coverage_hits") != hits or record.get("source_read_coverage_total") != len(expected):
        raise ReportError(f"{runtime} line {record['_line']} source read coverage is inconsistent")
    partial = source_read_partial_coverage(expected, reads)
    partial_fields = ("source_read_covered_lines", "source_read_expected_lines", "source_read_partial_coverage_ratio", "source_read_range_coverage")
    if any(field in record for field in partial_fields):
        if record.get("source_read_covered_lines") != partial[0] or record.get("source_read_expected_lines") != partial[1] or record.get("source_read_partial_coverage_ratio") != partial[2] or record.get("source_read_range_coverage") != partial[3]:
            raise ReportError(f"{runtime} line {record['_line']} diagnostic source coverage is inconsistent")
    citation_keys = []
    emitted = verified = 0
    for citation in citations:
        if not isinstance(citation, dict) or set(citation) != {"repository", "revision", "path", "line_start", "line_end", "emitted", "verified"}:
            raise ReportError(f"{runtime} line {record['_line']} source citation is invalid")
        validate_source_range({field: citation[field] for field in ("repository", "revision", "path", "line_start", "line_end")}, runtime, record["_line"])
        if not isinstance(citation["emitted"], bool) or not isinstance(citation["verified"], bool) or citation["verified"] and not citation["emitted"]:
            raise ReportError(f"{runtime} line {record['_line']} source citation flags are invalid")
        citation_keys.append(source_range_key(citation))
        emitted += int(citation["emitted"]); verified += int(citation["verified"])
    if citation_keys != sorted(citation_keys) or len(citation_keys) != len(set(citation_keys)) or len(citations) > 64:
        raise ReportError(f"{runtime} line {record['_line']} source citations are not canonical")
    if record.get("source_citation_emitted_count") != emitted or record.get("source_citation_verified_count") != verified:
        raise ReportError(f"{runtime} line {record['_line']} source citation counts are inconsistent")


def source_read_coverage(expected: list[dict[str, Any]], reads: list[dict[str, Any]]) -> int:
    hits = 0
    for wanted in expected:
        intervals = sorted((value["line_start"], value["line_end"]) for value in reads if all(value.get(field) == wanted[field] for field in ("repository", "revision", "path")) and value.get("outcome") == "succeeded")
        covered = wanted["line_start"] - 1
        for start, end in intervals:
            if end < wanted["line_start"] or start > covered + 1:
                continue
            covered = max(covered, end)
            if covered >= wanted["line_end"]:
                hits += 1
                break
    return hits


def source_read_partial_coverage(expected: list[dict[str, Any]], reads: list[dict[str, Any]]) -> tuple[int, int, float, list[dict[str, Any]]]:
    covered_total = expected_total = 0
    ranges = []
    for wanted in expected:
        intervals = sorted((max(value["line_start"], wanted["line_start"]), min(value["line_end"], wanted["line_end"])) for value in reads if all(value.get(field) == wanted[field] for field in ("repository", "revision", "path")) and value.get("outcome") == "succeeded" and max(value["line_start"], wanted["line_start"]) <= min(value["line_end"], wanted["line_end"]))
        covered = 0; through = wanted["line_start"] - 1
        for start, end in intervals:
            if end <= through: continue
            start = max(start, through + 1)
            if start <= end:
                covered += end - start + 1; through = end
        total = wanted["line_end"] - wanted["line_start"] + 1
        covered_total += covered; expected_total += total
        ranges.append({**wanted, "covered_lines": covered, "expected_lines": total, "coverage_ratio": covered / total})
    return covered_total, expected_total, covered_total / expected_total if expected_total else 0.0, ranges


def diagnostic_source_coverage(records: list[dict[str, Any]]) -> dict[str, Any]:
    selected = [record for record in records if "source_read_covered_lines" in record]
    covered = sum(record["source_read_covered_lines"] for record in selected)
    expected = sum(record["source_read_expected_lines"] for record in selected)
    return {"diagnostic_only": True, "trials": len(selected), "covered_lines": covered, "expected_lines": expected, "coverage_ratio": rate(covered, expected), "comparative_gate": False}


def validate_record(record: dict[str, Any], runtime: str) -> tuple[str, int]:
    case_id = require_string(record, "case_id", runtime)
    repetition = require_integer(record, "repetition", runtime, 1)
    for field in (
        "stable_id",
        "engine_commit",
        "benchmark_manifest_sha256",
        "fixture_sha256",
        "baseline_consumer_commit",
        "baseline_prompt_sha256",
        "project_sha256",
        "effective_prompt_sha256",
        "skill_set_hash",
        "effective_input_sha256",
        "comparison_input_sha256",
        "source_revision",
        "provider_path",
        "provider_config_sha256",
        "transport_id",
        "model_label",
        "api_mode",
        "evidence_condition",
        "source_expectation_sha256",
        "job_name",
        "build_id",
        "test_name",
    ):
        require_string(record, field, runtime)
    if not COMMIT_RE.fullmatch(record["engine_commit"]) or not COMMIT_RE.fullmatch(record["source_revision"]):
        raise ReportError(f"{runtime} line {record['_line']} engine or source revision is invalid")
    if not SHA256_RE.fullmatch(record["benchmark_manifest_sha256"]):
        raise ReportError(f"{runtime} line {record['_line']} benchmark manifest hash is invalid")
    if not SHA256_RE.fullmatch(record["provider_config_sha256"]):
        raise ReportError(f"{runtime} line {record['_line']} provider configuration hash is invalid")
    validate_pricing(record, runtime)
    source_expectation_sha256 = record["source_expectation_sha256"]
    if len(source_expectation_sha256) != 64 or any(char not in "0123456789abcdef" for char in source_expectation_sha256):
        raise ReportError(f"{runtime} line {record['_line']} source expectation SHA-256 is invalid")
    evidence_mode = record.get("evidence_mode")
    if evidence_mode not in EVIDENCE_MODES:
        raise ReportError(f"{runtime} line {record['_line']} field evidence_mode is invalid")
    if "test_source" not in record:
        record["test_source"] = ""
    if not isinstance(record.get("test_source"), str):
        raise ReportError(f"{runtime} line {record['_line']} field test_source must be a string")
    require_integer(record, "human_score_rubric_version", runtime, 1)
    require_integer(record, "human_score_max", runtime, 1)
    dimensions = record.get("human_score_dimensions")
    if not isinstance(dimensions, list) or not dimensions or not all(isinstance(value, str) and value for value in dimensions):
        raise ReportError(f"{runtime} line {record['_line']} field human_score_dimensions must be a non-empty string array")
    require_integer(record, "elapsed_ms", runtime)
    context_tokens = require_integer(record, "model_context_tokens", runtime, 8192)
    output_tokens = require_integer(record, "model_output_tokens", runtime, 1024)
    if output_tokens > context_tokens or output_tokens > 131072:
        raise ReportError(f"{runtime} line {record['_line']} model limits are invalid")
    require_integer(record, "signal_hits", runtime)
    require_integer(record, "signal_total", runtime)
    require_integer(record, "diagnosis_signal_hits", runtime)
    require_integer(record, "diagnosis_signal_total", runtime)
    require_integer(record, "forbidden_checks_passed", runtime)
    require_integer(record, "forbidden_checks_total", runtime)
    require_integer(record, "source_read_coverage_hits", runtime)
    require_integer(record, "source_read_coverage_total", runtime)
    require_integer(record, "source_read_count", runtime)
    require_integer(record, "source_citation_emitted_count", runtime)
    require_integer(record, "source_citation_verified_count", runtime)
    require_integer(record, "source_signal_hits", runtime)
    require_integer(record, "source_signal_total", runtime)
    require_integer(record, "source_evidence_tool_calls", runtime)
    validate_source_evidence(record, runtime)
    if record["source_read_coverage_hits"] > record["source_read_coverage_total"] or record["source_signal_hits"] > record["source_signal_total"]:
        raise ReportError(f"{runtime} line {record['_line']} source scoring numerators exceed denominators")
    if evidence_mode == "artifact_only" and (record["source_read_coverage_total"] != 0 or record["source_signal_total"] != 0):
        raise ReportError(f"{runtime} line {record['_line']} artifact-only case declares source expectations")
    if evidence_mode == "artifact_and_source" and (record["source_read_coverage_total"] < 1 or record["source_signal_total"] < 1):
        raise ReportError(f"{runtime} line {record['_line']} source-required case lacks source expectations")
    if record["signal_hits"] > record["signal_total"] or record["diagnosis_signal_hits"] > record["diagnosis_signal_total"] or record["forbidden_checks_passed"] > record["forbidden_checks_total"]:
        raise ReportError(f"{runtime} line {record['_line']} scoring numerators exceed denominators")

    legacy_structured = record.get("usable") is True if runtime == "inprocess" else record.get("analysis_valid") is True
    if "structured_valid" not in record:
        record["structured_valid"] = legacy_structured
    if "displayable" not in record:
        record["displayable"] = record["structured_valid"]
    if "analysis_disposition" not in record:
        has_artifact_grounding = bool(record.get("evidence_citations")) or record.get("artifact_citation_count", 0) > 0
        record["analysis_disposition"] = "grounded" if record["displayable"] and has_artifact_grounding else ("preliminary" if record["displayable"] else "")
    if "disposition_warnings" not in record:
        record["disposition_warnings"] = []
    if "grounded" not in record:
        record["grounded"] = record["analysis_disposition"] == "grounded"
    if runtime == "inprocess" and "contract_violation" not in record:
        record["contract_violation"] = record.get("trial_status") == "contract_violation"
    for field in ("structured_valid", "displayable", "grounded"):
        if not isinstance(record.get(field), bool):
            raise ReportError(f"{runtime} line {record['_line']} field {field} must be boolean")
    disposition = record.get("analysis_disposition")
    warnings = record.get("disposition_warnings")
    if disposition not in ("", "preliminary", "grounded") or not isinstance(warnings, list) or not all(isinstance(value, str) and value for value in warnings):
        raise ReportError(f"{runtime} line {record['_line']} analysis disposition is invalid")
    if record["structured_valid"] != record["displayable"] or record["displayable"] != (disposition != "") or record["grounded"] != (disposition == "grounded"):
        raise ReportError(f"{runtime} line {record['_line']} analysis disposition fields are inconsistent")
    if runtime == "inprocess" and not isinstance(record.get("contract_violation"), bool):
        raise ReportError(f"inprocess line {record['_line']} contract_violation must be boolean")
    if runtime == "inprocess":
        if record.get("api_mode") != "chat_completions":
            raise ReportError(f"inprocess line {record['_line']} must use chat_completions")
        if record.get("evidence_condition") != "fixture-v1":
            raise ReportError(f"inprocess line {record['_line']} must use fixture-v1 evidence")
        if not isinstance(record.get("usable"), bool):
            raise ReportError(f"inprocess line {record['_line']} field usable must be boolean")
        status = require_string(record, "trial_status", runtime)
        if status not in INPROCESS_STATUSES:
            raise ReportError(f"inprocess line {record['_line']} trial_status is invalid")
        links = record.get("file_links", {})
        relevant = record.get("relevant_files", [])
        if not isinstance(links, dict) or not isinstance(relevant, list) or not all(isinstance(path, str) and path for path in relevant): raise ReportError(f"inprocess line {record['_line']} source diagnostics are invalid")
        trace = record.get("trace")
        if not isinstance(trace, dict):
            raise ReportError(f"inprocess line {record['_line']} field trace must be an object")
        for field in ("model_requests", "reported_requests", "provider_attempts", "input_tokens", "cached_input_tokens", "output_tokens", "reasoning_tokens"):
            value = trace.get(field)
            if not isinstance(value, int) or isinstance(value, bool) or value < 0:
                raise ReportError(f"inprocess line {record['_line']} trace field {field} must be a non-negative integer")
        if not isinstance(trace.get("provider_attempts_known"), bool):
            raise ReportError(f"inprocess line {record['_line']} provider-attempt availability is invalid")
        if trace["reported_requests"] > trace["model_requests"] or trace["cached_input_tokens"] > trace["input_tokens"] or trace["reasoning_tokens"] > trace["output_tokens"]:
            raise ReportError(f"inprocess line {record['_line']} request or token telemetry is inconsistent")
    else:
        if record.get("api_mode") != "chat_completions" or record.get("evidence_condition") != "fixture-v1":
            raise ReportError(f"sandbox line {record['_line']} must use chat_completions and fixture-v1 evidence")
        if record.get("version") != 8 or record.get("runtime") != "agent-sandbox-opencode":
            raise ReportError(f"sandbox line {record['_line']} has an unsupported record contract")
        for field in ("runtime_identity_hash", "image_contract_sha256", "executor_image", "stager_image", "executor_aster_revision", "stager_aster_revision", "expected_opencode_version"):
            require_string(record, field, runtime)
        if not SHA256_RE.fullmatch(record["runtime_identity_hash"]) or not SHA256_RE.fullmatch(record["image_contract_sha256"]):
            raise ReportError(f"sandbox line {record['_line']} runtime identity hash is invalid")
        if not IMAGE_RE.fullmatch(record["executor_image"]) or not IMAGE_RE.fullmatch(record["stager_image"]):
            raise ReportError(f"sandbox line {record['_line']} runtime image is not immutable")
        if record["executor_aster_revision"] != record["engine_commit"] or record["stager_aster_revision"] != record["engine_commit"]:
            raise ReportError(f"sandbox line {record['_line']} embedded Aster revision differs from engine_commit")
        if record.get("request_shape_available") is True and record.get("opencode_version") != record["expected_opencode_version"]:
            raise ReportError(f"sandbox line {record['_line']} OpenCode version differs from the frozen image identity")
        if record.get("request_shape_available") is True and (record.get("request_context_limit") != context_tokens or record.get("request_output_token_limit") != output_tokens):
            raise ReportError(f"sandbox line {record['_line']} OpenCode request limits differ from the frozen benchmark limits")
        status = require_string(record, "status", runtime)
        if require_string(record, "trial_status", runtime) != status:
            raise ReportError(f"sandbox line {record['_line']} trial_status differs from status")
        if status not in SANDBOX_STATUSES:
            raise ReportError(f"sandbox line {record['_line']} status is invalid")
        record.setdefault("evidence_phase_allocated_steps", 0)
        record.setdefault("evidence_phase_bounded_exhaustion", False)
        record.setdefault("evidence_phase_exhaustion_steps", 0)
        record.setdefault("evidence_phase_exhaustion_requests", 0)
        record.setdefault("evidence_phase_exhaustion_classification", "")
        record.setdefault("successful_evidence_read_calls", 0)
        record.setdefault("duplicate_evidence_read_calls", 0)
        allocated_steps = require_integer(record, "evidence_phase_allocated_steps", runtime)
        exhaustion_steps = require_integer(record, "evidence_phase_exhaustion_steps", runtime)
        exhaustion_requests = require_integer(record, "evidence_phase_exhaustion_requests", runtime)
        successful_reads = require_integer(record, "successful_evidence_read_calls", runtime)
        duplicate_reads = require_integer(record, "duplicate_evidence_read_calls", runtime)
        if duplicate_reads > successful_reads:
            raise ReportError(f"sandbox line {record['_line']} duplicate evidence reads exceed successful reads")
        bounded_exhaustion = record.get("evidence_phase_bounded_exhaustion")
        if not isinstance(bounded_exhaustion, bool):
            raise ReportError(f"sandbox line {record['_line']} bounded evidence exhaustion must be boolean")
        exhaustion_classification = record.get("evidence_phase_exhaustion_classification", "")
        if not isinstance(exhaustion_classification, str):
            raise ReportError(f"sandbox line {record['_line']} bounded evidence exhaustion classification must be a string")
        if bounded_exhaustion:
            if allocated_steps < 2 or exhaustion_requests != allocated_steps or exhaustion_steps + 1 != allocated_steps or exhaustion_classification != "api_bad_request":
                raise ReportError(f"sandbox line {record['_line']} bounded evidence exhaustion telemetry is incomplete")
        elif allocated_steps != 0 or exhaustion_steps != 0 or exhaustion_requests != 0 or exhaustion_classification:
            raise ReportError(f"sandbox line {record['_line']} bounded evidence exhaustion telemetry is inconsistent")
        for field in ("analysis_valid", "finalization_valid", "cleanup_completed"):
            if not isinstance(record.get(field), bool):
                raise ReportError(f"sandbox line {record['_line']} field {field} must be boolean")
        require_integer(record, "artifact_citation_count", runtime)
        for field in ("model_requests", "provider_requests", "input_tokens", "cached_input_tokens", "output_tokens", "reasoning_tokens"):
            require_integer(record, field, runtime)
        require_integer(record, "max_steps", runtime, 1)
        for field in ("provider_requests_known", "token_usage_available", "cost_available"):
            if not isinstance(record.get(field), bool):
                raise ReportError(f"sandbox line {record['_line']} field {field} must be boolean")
        record["_token_usage_consistent"] = (
            record["cached_input_tokens"] <= record["input_tokens"]
            and record["reasoning_tokens"] <= record["output_tokens"]
        )
        if record["provider_requests_known"] and record["provider_requests"] < record["model_requests"]:
            raise ReportError(f"sandbox line {record['_line']} provider request telemetry is inconsistent")
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
        require_integer(record, "artifact_evidence_tool_calls", runtime)
        if not isinstance(record.get("evidence_contract_passed"), bool):
            raise ReportError(f"sandbox line {record['_line']} field evidence_contract_passed must be boolean")
        require_string(record, "evidence_contract_status", runtime)
        expected_passed, expected_status = evidence_contract_result(record, runtime)
        if record["evidence_contract_passed"] != expected_passed or record["evidence_contract_status"] != expected_status:
            raise ReportError(f"sandbox line {record['_line']} evidence contract is inconsistent")
        for field in ("task_finalized_ms", "result_available_ms", "cleanup_duration_ms", "runtime_duration_ms"):
            if field in record:
                require_integer(record, field, runtime)
        if record.get("result_available") is True and record.get("task_finalized_ms", 0) and record.get("result_available_ms", 0) < record["task_finalized_ms"]:
            raise ReportError(f"sandbox line {record['_line']} result availability precedes finalization")
    return case_id, repetition


def evidence_contract_result(record: dict[str, Any], runtime: str) -> tuple[bool, str]:
    if runtime == "inprocess":
        if record.get("usable") is not True: return False, "analysis_unavailable"
        if not record.get("evidence_citations"): return False, "artifact_citation_missing"
    else:
        if record.get("analysis_valid") is not True: return False, "analysis_unavailable"
        if record.get("artifact_evidence_tool_calls", 0) < 1: return False, "artifact_evidence_missing"
        if record.get("artifact_citation_count", 0) < 1: return False, "artifact_citation_missing"
    has_source_output = bool(record.get("source_citations")) or bool(record.get("relevant_files")) or record.get("source_signal_hits", 0) > 0
    if has_source_output and record.get("source_evidence_tool_calls", 0) < 1: return False, "unsupported_source_claim"
    if record.get("evidence_mode") == "artifact_and_source":
        if record.get("source_evidence_tool_calls", 0) < 1: return False, "source_evidence_missing"
        if record.get("source_read_count", 0) < 1: return False, "source_content_not_read"
        if record.get("source_read_coverage_hits") != record.get("source_read_coverage_total"): return False, "source_read_coverage_missing"
        if record.get("source_signal_hits") != record.get("source_signal_total"): return False, "source_diagnosis_missing"
    return True, "passed"


def evidence_mode_metrics(records: list[dict[str, Any]], runtime: str) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for mode in EVIDENCE_MODES:
        selected = [record for record in records if record.get("evidence_mode") == mode]
        passed = sum(evidence_contract_result(record, runtime)[0] for record in selected)
        result[mode] = {
            "trials": len(selected),
            "contract_passed_trials": passed,
            "contract_pass_rate": rate(passed, len(selected)),
        }
        if runtime == "sandbox":
            result[mode]["artifact_tool_trials"] = sum(record.get("artifact_evidence_tool_calls", 0) > 0 for record in selected)
            result[mode]["source_tool_trials"] = sum(record.get("source_evidence_tool_calls", 0) > 0 for record in selected)
    return result


def index_records(records: list[dict[str, Any]], runtime: str) -> dict[tuple[str, int], dict[str, Any]]:
    indexed: dict[tuple[str, int], dict[str, Any]] = {}
    case_modes: dict[str, str] = {}
    sandbox_runtime_identity: tuple[Any, ...] | None = None
    for record in records:
        key = validate_record(record, runtime)
        mode = record["evidence_mode"]
        if key[0] in case_modes and case_modes[key[0]] != mode:
            raise ReportError(f"{runtime} case {key[0]} changes evidence_mode across repetitions")
        case_modes[key[0]] = mode
        if runtime == "sandbox":
            current_identity = (
                record["executor_image"], record["stager_image"], record["executor_aster_revision"],
                record["stager_aster_revision"], record["expected_opencode_version"], record["image_contract_sha256"], record.get("runtime_identity_hash"), record["max_steps"],
            )
            if sandbox_runtime_identity is None:
                sandbox_runtime_identity = current_identity
            elif current_identity != sandbox_runtime_identity:
                raise ReportError("sandbox runtime or image identity changes across repetitions")
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


def validate_matrix_identity(keys: list[tuple[str, int]], inprocess: dict[tuple[str, int], dict[str, Any]], sandbox: dict[tuple[str, int], dict[str, Any]]) -> None:
    first = inprocess[keys[0]]
    for key in keys:
        for runtime, record in (("inprocess", inprocess[key]), ("sandbox", sandbox[key])):
            for field in MATRIX_FIELDS:
                if record.get(field) != first.get(field):
                    raise ReportError(f"benchmark matrix differs in {field} at {runtime} {key[0]}/rep-{key[1]:02d}")


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


def token_usage_available(record: dict[str, Any], runtime: str) -> bool:
    if runtime == "inprocess":
        trace = record["trace"]
        return (
            trace["model_requests"] > 0
            and trace["reported_requests"] == trace["model_requests"]
            and record.get("trace_truncated") is not True
        )
    return record.get("token_usage_available") is True and record.get("_token_usage_consistent") is True


def estimated_cost_usd(record: dict[str, Any], runtime: str) -> Decimal | None:
    if not token_usage_available(record, runtime):
        return None
    usage = record["trace"] if runtime == "inprocess" else record
    pricing = record["pricing"]
    input_tokens = Decimal(usage["input_tokens"] - usage["cached_input_tokens"])
    cached_tokens = Decimal(usage["cached_input_tokens"])
    output_tokens = Decimal(usage["output_tokens"])
    return (
        input_tokens * Decimal(pricing["input_per_million"])
        + cached_tokens * Decimal(pricing["cached_input_per_million"])
        + output_tokens * Decimal(pricing["output_per_million"])
    ) / Decimal(1_000_000)


def decimal_text(value: Decimal) -> str:
    return format(value.quantize(Decimal("0.00000001")), "f")


def decimal_metrics(values: list[Decimal]) -> dict[str, Any]:
    if not values:
        return {"min": None, "median": None, "max": None, "range": None}
    ordered = sorted(values)
    midpoint = len(ordered) // 2
    if len(ordered) % 2:
        median = ordered[midpoint]
    else:
        median = (ordered[midpoint - 1] + ordered[midpoint]) / Decimal(2)
    return {
        "min": decimal_text(ordered[0]),
        "median": decimal_text(median),
        "max": decimal_text(ordered[-1]),
        "range": [decimal_text(ordered[0]), decimal_text(ordered[-1])],
    }


def usage_metrics(records: list[dict[str, Any]], runtime: str) -> dict[str, Any]:
    usages = [record["trace"] if runtime == "inprocess" else record for record in records]
    token_records = [record for record in records if token_usage_available(record, runtime)]
    token_usages = [record["trace"] if runtime == "inprocess" else record for record in token_records]
    provider_known = [
        usage["provider_attempts" if runtime == "inprocess" else "provider_requests"]
        for record, usage in zip(records, usages)
        if usage["provider_attempts_known" if runtime == "inprocess" else "provider_requests_known"] is True
    ]
    costs = [value for value in (estimated_cost_usd(record, runtime) for record in records) if value is not None]
    provider_reported_costs: list[Decimal] = []
    if runtime == "sandbox":
        for record in records:
            value = record.get("cost_usd")
            if record.get("cost_available") is not True or not isinstance(value, str) or not value.strip():
                continue
            try:
                parsed = Decimal(value)
            except InvalidOperation as exc:
                raise ReportError(f"sandbox line {record['_line']} field cost_usd is invalid") from exc
            if parsed < 0 or not parsed.is_finite():
                raise ReportError(f"sandbox line {record['_line']} field cost_usd must be finite and non-negative")
            provider_reported_costs.append(parsed)
    return {
        "model_requests": sum(usage["model_requests"] for usage in usages),
        "model_requests_distribution": duration_metrics([usage["model_requests"] for usage in usages]),
        "provider_attempts_known_trials": len(provider_known),
        "provider_attempts_coverage": rate(len(provider_known), len(records)),
        "provider_attempts": sum(provider_known),
        "provider_attempts_distribution": duration_metrics(provider_known),
        "input_tokens": sum(usage["input_tokens"] for usage in token_usages) if token_usages else None,
        "input_tokens_distribution": duration_metrics([usage["input_tokens"] for usage in token_usages]),
        "cached_input_tokens": sum(usage["cached_input_tokens"] for usage in token_usages) if token_usages else None,
        "cached_input_tokens_distribution": duration_metrics([usage["cached_input_tokens"] for usage in token_usages]),
        "output_tokens": sum(usage["output_tokens"] for usage in token_usages) if token_usages else None,
        "output_tokens_distribution": duration_metrics([usage["output_tokens"] for usage in token_usages]),
        "reasoning_tokens": sum(usage["reasoning_tokens"] for usage in token_usages) if token_usages else None,
        "reasoning_tokens_distribution": duration_metrics([usage["reasoning_tokens"] for usage in token_usages]),
        "token_usage_trials": len(token_records),
        "token_usage_inconsistent_trials": sum(record.get("token_usage_available") is True and not token_usage_available(record, runtime) for record in records),
        "token_usage_coverage": rate(len(token_records), len(records)),
        "estimated_cost_available_trials": len(costs),
        "estimated_cost_coverage": rate(len(costs), len(records)),
        "estimated_cost_currency": records[0]["pricing"]["currency"] if records else None,
        "estimated_cost_usd_total": decimal_text(sum(costs, Decimal(0))) if costs else None,
        "estimated_cost_usd_distribution": decimal_metrics(costs),
        "provider_reported_cost_available_trials": len(provider_reported_costs),
        "provider_reported_cost_usd_total": decimal_text(sum(provider_reported_costs, Decimal(0))) if provider_reported_costs else None,
        "provider_reported_cost_usd_distribution": decimal_metrics(provider_reported_costs),
    }


def inprocess_metrics(records: list[dict[str, Any]]) -> dict[str, Any]:
    runtime_valid = [record for record in records if record["structured_valid"]]
    valid = [record for record in records if evidence_contract_result(record, "inprocess")[0]]
    statuses = [record["trial_status"] for record in records]
    citation_trials = sum(bool(record.get("evidence_citations")) for record in runtime_valid)
    source_evaluated = [record for record in runtime_valid if record.get("source_read_coverage_total", 0) > 0]
    source_trials = sum(record.get("source_read_coverage_hits") == record.get("source_read_coverage_total") for record in source_evaluated)
    citation_emitted_trials = sum(record.get("source_citation_emitted_count", 0) > 0 for record in runtime_valid)
    citation_verified_trials = sum(record.get("source_citation_verified_count", 0) > 0 for record in runtime_valid)
    metrics = {
        "trials": len(records),
        "runtime_valid_trials": len(runtime_valid),
        "runtime_valid_rate": rate(len(runtime_valid), len(records)),
        "structured_valid_trials": sum(record["structured_valid"] for record in records),
        "structured_valid_rate": rate(sum(record["structured_valid"] for record in records), len(records)),
        "displayable_trials": sum(record["displayable"] for record in records),
        "displayable_rate": rate(sum(record["displayable"] for record in records), len(records)),
        "preliminary_trials": sum(record["analysis_disposition"] == "preliminary" for record in records),
        "grounded_trials": sum(record["grounded"] for record in records),
        "grounded_rate": rate(sum(record["grounded"] for record in records), len(records)),
        "contract_warning_trials": sum(record["contract_violation"] for record in records),
        "valid_trials": len(valid),
        "valid_rate": rate(len(valid), len(records)),
        "invalid_trials": sum(status == "invalid_result" for status in statuses),
        "no_result_trials": sum(status == "no_result" for status in statuses),
        "runtime_failure_trials": sum(status in ("runtime_failure", "timeout") for status in statuses),
        "artifact_citation_trials": citation_trials,
        "artifact_citation_rate": rate(citation_trials, len(runtime_valid)),
        "complete_expected_source_coverage_trials": source_trials,
        "expected_source_range_coverage_rate": rate(source_trials, len(source_evaluated)),
        "partial_source_coverage": diagnostic_source_coverage(runtime_valid),
        "source_citation_emitted_trials": citation_emitted_trials,
        "source_citation_verified_trials": citation_verified_trials,
        "source_citation_emitted_count": sum(record.get("source_citation_emitted_count", 0) for record in records),
        "source_citation_verified_count": sum(record.get("source_citation_verified_count", 0) for record in records),
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
        "evidence_modes": evidence_mode_metrics(records, "inprocess"),
        "cleanup_status": "not_applicable_inprocess",
    }
    metrics.update(usage_metrics(records, "inprocess"))
    return metrics


def sandbox_metrics(records: list[dict[str, Any]]) -> dict[str, Any]:
    runtime_valid = [record for record in records if record["structured_valid"]]
    valid = [record for record in records if evidence_contract_result(record, "sandbox")[0]]
    statuses = [record["status"] for record in records]
    citation_trials = sum(record["artifact_citation_count"] > 0 for record in runtime_valid)
    source_evaluated = [record for record in runtime_valid if record.get("source_read_coverage_total", 0) > 0]
    source_trials = sum(record.get("source_read_coverage_hits") == record.get("source_read_coverage_total") for record in source_evaluated)
    citation_emitted_trials = sum(record.get("source_citation_emitted_count", 0) > 0 for record in runtime_valid)
    citation_verified_trials = sum(record.get("source_citation_verified_count", 0) > 0 for record in runtime_valid)
    metrics = {
        "trials": len(records),
        "runtime_valid_trials": len(runtime_valid),
        "runtime_valid_rate": rate(len(runtime_valid), len(records)),
        "structured_valid_trials": sum(record["structured_valid"] for record in records),
        "structured_valid_rate": rate(sum(record["structured_valid"] for record in records), len(records)),
        "displayable_trials": sum(record["displayable"] for record in records),
        "displayable_rate": rate(sum(record["displayable"] for record in records), len(records)),
        "preliminary_trials": sum(record["analysis_disposition"] == "preliminary" for record in records),
        "grounded_trials": sum(record["grounded"] for record in records),
        "grounded_rate": rate(sum(record["grounded"] for record in records), len(records)),
        "valid_trials": len(valid),
        "valid_rate": rate(len(valid), len(records)),
        "invalid_trials": sum(status == "invalid_result" for status in statuses),
        "no_result_trials": sum(status == "no_result" for status in statuses),
        "runtime_failure_trials": sum(status in ("runtime_failure", "timeout", "cancellation") for status in statuses),
        "cleanup_pending_trials": sum(status == "cleanup_pending" for status in statuses),
        "artifact_citation_trials": citation_trials,
        "artifact_citation_rate": rate(citation_trials, len(runtime_valid)),
        "complete_expected_source_coverage_trials": source_trials,
        "expected_source_range_coverage_rate": rate(source_trials, len(source_evaluated)),
        "partial_source_coverage": diagnostic_source_coverage(runtime_valid),
        "source_citation_emitted_trials": citation_emitted_trials,
        "source_citation_verified_trials": citation_verified_trials,
        "source_citation_emitted_count": sum(record.get("source_citation_emitted_count", 0) for record in records),
        "source_citation_verified_count": sum(record.get("source_citation_verified_count", 0) for record in records),
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
        "evidence_modes": evidence_mode_metrics(records, "sandbox"),
        "bounded_evidence_exhaustion_trials": sum(record.get("evidence_phase_bounded_exhaustion") is True for record in records),
        "successful_evidence_read_calls": sum(record.get("successful_evidence_read_calls", 0) for record in records),
        "duplicate_evidence_read_calls": sum(record.get("duplicate_evidence_read_calls", 0) for record in records),
        "duplicate_evidence_read_rate": rate(
            sum(record.get("duplicate_evidence_read_calls", 0) for record in records),
            sum(record.get("successful_evidence_read_calls", 0) for record in records),
        ),
        "runtime_identity": {
            "executor_image": records[0]["executor_image"] if records else None,
            "stager_image": records[0]["stager_image"] if records else None,
            "image_contract_sha256": records[0]["image_contract_sha256"] if records else None,
            "executor_aster_revision": records[0]["executor_aster_revision"] if records else None,
            "stager_aster_revision": records[0]["stager_aster_revision"] if records else None,
            "opencode_version": records[0]["expected_opencode_version"] if records else None,
        },
    }
    metrics.update(usage_metrics(records, "sandbox"))
    return metrics

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
        "range": [min(clean), max(clean)] if clean else None,
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


def ratio_decimal(numerator: str | None, denominator: str | None) -> float | None:
    if numerator is None or denominator is None:
        return None
    left, right = Decimal(numerator), Decimal(denominator)
    if right <= 0:
        return None
    return round(float(left / right), 4)


def evaluate_criteria(inprocess: dict[str, Any], sandbox: dict[str, Any], per_case: dict[str, Any], pairs: int, expected_pairs: int, holdouts_ok: bool, blind_quality: dict[str, Any], evidence_modes_complete: bool) -> dict[str, Any]:
    evidence_complete = pairs == expected_pairs and holdouts_ok and evidence_modes_complete
    lifecycle_non_regression = (
        (sandbox["runtime_valid_rate"] or 0) >= (inprocess["runtime_valid_rate"] or 0)
        and sandbox["invalid_trials"] <= inprocess["invalid_trials"]
        and sandbox["no_result_trials"] <= inprocess["no_result_trials"]
        and sandbox["runtime_failure_trials"] <= inprocess["runtime_failure_trials"]
        and (sandbox["finalization_valid_rate"] or 0) == 1.0
        and (sandbox["cleanup_completed_rate"] or 0) == 1.0
    )
    inprocess_source = inprocess["expected_source_range_coverage_rate"]
    sandbox_source = sandbox["expected_source_range_coverage_rate"]
    grounding_non_regression = (
        (sandbox["artifact_citation_rate"] or 0) >= (inprocess["artifact_citation_rate"] or 0)
        and inprocess_source is not None
        and sandbox_source is not None
        and sandbox_source >= inprocess_source
    )
    telemetry_complete = (
        (inprocess["token_usage_coverage"] or 0) == 1.0
        and (sandbox["token_usage_coverage"] or 0) == 1.0
        and (inprocess["provider_attempts_coverage"] or 0) == 1.0
        and (sandbox["provider_attempts_coverage"] or 0) == 1.0
        and (inprocess["estimated_cost_coverage"] or 0) == 1.0
        and (sandbox["estimated_cost_coverage"] or 0) == 1.0
    )
    latency_ratio = None
    inprocess_latency = inprocess["latency_ms"]["median"]
    sandbox_latency = sandbox["latency_ms"]["median"]
    if isinstance(inprocess_latency, int) and inprocess_latency > 0 and isinstance(sandbox_latency, int):
        latency_ratio = round(sandbox_latency / inprocess_latency, 4)
    cost_ratio = ratio_decimal(sandbox["estimated_cost_usd_total"], inprocess["estimated_cost_usd_total"])
    cost_latency_acceptable = (
        latency_ratio is not None and latency_ratio <= 2.0
        and cost_ratio is not None and cost_ratio <= 1.5
    )
    per_case_checks: dict[str, Any] = {}
    for case_id, item in per_case.items():
        left, right = item["inprocess"], item["agent_sandbox"]
        source_required = item["evidence_mode"] == "artifact_and_source"
        case_lifecycle = (
            (right["runtime_valid_rate"] or 0) >= (left["runtime_valid_rate"] or 0)
            and right["invalid_trials"] <= left["invalid_trials"]
            and right["no_result_trials"] <= left["no_result_trials"]
            and right["runtime_failure_trials"] <= left["runtime_failure_trials"]
            and (right["finalization_valid_rate"] or 0) == 1.0
            and (right["cleanup_completed_rate"] or 0) == 1.0
        )
        case_grounding = (right["artifact_citation_rate"] or 0) >= (left["artifact_citation_rate"] or 0)
        if source_required:
            left_source = left["expected_source_range_coverage_rate"]
            right_source = right["expected_source_range_coverage_rate"]
            case_grounding = case_grounding and left_source is not None and right_source is not None and right_source >= left_source
        case_latency_ratio = None
        if isinstance(left["latency_ms"]["median"], int) and left["latency_ms"]["median"] > 0 and isinstance(right["latency_ms"]["median"], int):
            case_latency_ratio = round(right["latency_ms"]["median"] / left["latency_ms"]["median"], 4)
        case_cost_ratio = ratio_decimal(right["estimated_cost_usd_total"], left["estimated_cost_usd_total"])
        case_cost_latency = (
            case_latency_ratio is not None and case_latency_ratio <= 2.0
            and case_cost_ratio is not None and case_cost_ratio <= 1.5
        )
        per_case_checks[case_id] = {
            "lifecycle_non_regression": case_lifecycle,
            "grounding_non_regression": case_grounding,
            "cost_latency_acceptable": case_cost_latency,
            "latency_median_ratio": case_latency_ratio,
            "estimated_cost_ratio": case_cost_ratio,
        }
    per_case_quality_non_regression = bool(per_case_checks) and all(
        item["lifecycle_non_regression"] and item["grounding_non_regression"]
        for item in per_case_checks.values()
    )
    per_case_cost_latency_acceptable = bool(per_case_checks) and all(
        item["cost_latency_acceptable"] for item in per_case_checks.values()
    )
    blind_complete = blind_quality.get("complete") is True
    blind_non_regression = blind_quality.get("non_regression") is True
    repeated_causal_improvement = blind_quality.get("material_causal_improvement") is True
    if not evidence_complete or not blind_complete or not telemetry_complete:
        classification = "insufficient_evidence"
    elif not lifecycle_non_regression or not grounding_non_regression or not per_case_quality_non_regression or not blind_non_regression:
        classification = "inprocess_preferred"
    elif repeated_causal_improvement and cost_latency_acceptable and per_case_cost_latency_acceptable:
        classification = "shadow_materially_better"
    else:
        classification = "shadow_promising_for_more_evaluation"
    return {
        "evidence_complete": evidence_complete,
        "evidence_modes_complete": evidence_modes_complete,
        "evidence_modes_required": True,
        "lifecycle_non_regression": lifecycle_non_regression,
        "grounding_non_regression": grounding_non_regression,
        "per_case_quality_non_regression": per_case_quality_non_regression,
        "per_case_cost_latency_acceptable": per_case_cost_latency_acceptable,
        "per_case_checks": per_case_checks,
        "blind_quality_complete": blind_complete,
        "blind_quality_non_regression": blind_non_regression,
        "repeated_causal_improvement_across_multiple_cases": repeated_causal_improvement,
        "telemetry_complete": telemetry_complete,
        "cost_latency_acceptable": cost_latency_acceptable,
        "latency_median_ratio": latency_ratio,
        "estimated_cost_ratio": cost_ratio,
        "cost_latency_thresholds": {"max_latency_median_ratio": 2.0, "max_estimated_cost_ratio": 1.5},
        "shadow_comparison": classification,
        "authoritative_analyzer": "inprocess_unchanged",
    }

def validate_holdouts(keys: list[tuple[str, int]], holdouts: list[str], required_repetitions: int) -> tuple[bool, dict[str, int]]:
    if len(holdouts) != len(set(holdouts)):
        raise ReportError("holdout cases must be unique")
    actual_cases = {case_id for case_id, _ in keys}
    requested_cases = set(holdouts)
    counts = {case_id: sum(key[0] == case_id for key in keys) for case_id in holdouts}
    if required_repetitions < 1:
        raise ReportError("required repetitions must be positive")
    return requested_cases == actual_cases and all(count == required_repetitions for count in counts.values()), counts


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
        relevant = []
    source = [
        {field: value.get(field) for field in ("repository", "revision", "path", "line_start", "line_end")}
        for value in record.get("source_read_ranges", []) if isinstance(value, dict)
    ]
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
        "source_content_reads": source,
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


def blind_case_runtime_summary(rows: list[dict[str, Any]], dimensions: list[str]) -> dict[str, Any]:
    totals = [row["total"] for row in rows]
    alignments = {value: 0 for value in ("aligned", "partial", "missing", "contradicted")}
    for row in rows:
        alignments[row["causal_assessment"]["alignment"]] += 1
    return {
        "trials": len(rows),
        "average_total": round(statistics.mean(totals), 4),
        "median_total": round(statistics.median(totals), 4),
        "total_range": [min(totals), max(totals)],
        "dimensions": {
            dimension: {
                "average": round(statistics.mean(row["scores"][dimension] for row in rows), 4),
                "median": round(statistics.median(row["scores"][dimension] for row in rows), 4),
                "range": [min(row["scores"][dimension] for row in rows), max(row["scores"][dimension] for row in rows)],
            }
            for dimension in dimensions
        },
        "causal_assessment": {
            "alignment_counts": alignments,
            "initiating_cause_found_trials": sum(row["causal_assessment"]["initiating_cause_found"] for row in rows),
            "downstream_treated_as_primary_trials": sum(row["causal_assessment"]["downstream_treated_as_primary"] for row in rows),
            "full_causal_resolution_trials": sum(row["scores"]["diagnosis"] == 2 for row in rows),
            "unresolved_trials": sum(row["scores"]["diagnosis"] < 2 for row in rows),
        },
    }


def load_blind_quality(map_path: str, scores_path: str, freeze_path: str, references: dict[str, dict[str, Any]], reference_hash: str, keys: list[tuple[str, int]], inprocess: dict[tuple[str, int], dict[str, Any]], sandbox: dict[tuple[str, int], dict[str, Any]], rubric_version: int, score_max: int, dimensions: list[str]) -> dict[str, Any]:
    scores_doc = json.loads(Path(scores_path).read_text())
    freeze_doc = json.loads(Path(freeze_path).read_text())
    score_hash = hashlib.sha256(json.dumps(scores_doc, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
    scoring_timestamp = scores_doc.get("scoring_timestamp")
    if not isinstance(scoring_timestamp, str) or not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z", scoring_timestamp):
        raise ReportError("blind scores require one immutable UTC scoring_timestamp")
    if freeze_doc.get("version") != 2 or freeze_doc.get("score_set_sha256") != score_hash or freeze_doc.get("packet_set_sha256") != scores_doc.get("packet_set_sha256") or freeze_doc.get("reference_set_sha256") != scores_doc.get("reference_set_sha256") or freeze_doc.get("scoring_timestamp") != scoring_timestamp:
        raise ReportError("blind scores do not match the pre-unblinding score freeze")
    mapping_doc = json.loads(Path(map_path).read_text())
    if mapping_doc.get("version") != 2 or scores_doc.get("version") != 2:
        raise ReportError("blind map and score versions must be 2")
    if mapping_doc.get("reference_set_sha256") != reference_hash or scores_doc.get("reference_set_sha256") != reference_hash:
        raise ReportError("blind scores are not bound to this causal reference set")
    packet_hash = mapping_doc.get("packet_set_sha256")
    if not isinstance(packet_hash, str) or not SHA256_RE.fullmatch(packet_hash) or scores_doc.get("packet_set_sha256") != packet_hash:
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
    repetition_by_packet = {f"{case_id}-rep-{repetition:02d}": repetition for case_id, repetition in keys}
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
    scored_packets: dict[tuple[str, str], dict[str, Any]] = {}
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
        scored_packets[(key[0], runtime)] = {"total": total, "scores": values, "causal_assessment": assessment}
        seen.add(key)
    if seen != set(runtime_by_key):
        raise ReportError("blind scores are incomplete")
    summary: dict[str, Any] = {}
    for runtime in totals:
        summary[runtime] = {
            "trials": len(totals[runtime]),
            "average_total": round(statistics.mean(totals[runtime]), 4),
            "median_total": round(statistics.median(totals[runtime]), 4),
            "total_range": [min(totals[runtime]), max(totals[runtime])],
            "dimension_averages": {
                dimension: round(statistics.mean(dimension_values[runtime][dimension]), 4)
                for dimension in dimensions
            },
        }
    cases: dict[str, Any] = {}
    causal_improvement_cases: list[str] = []
    causal_regression_cases: list[str] = []
    for case_id in sorted({key[0] for key in keys}):
        packet_ids = [f"{case_id}-rep-{repetition:02d}" for current_case, repetition in keys if current_case == case_id]
        runtime_rows = {
            runtime: [scored_packets[(packet_id, runtime)] for packet_id in packet_ids]
            for runtime in totals
        }
        diagnosis_wins = sum(
            scored_packets[(packet_id, "agent_sandbox")]["scores"]["diagnosis"]
            > scored_packets[(packet_id, "inprocess")]["scores"]["diagnosis"]
            for packet_id in packet_ids
        )
        diagnosis_losses = sum(
            scored_packets[(packet_id, "agent_sandbox")]["scores"]["diagnosis"]
            < scored_packets[(packet_id, "inprocess")]["scores"]["diagnosis"]
            for packet_id in packet_ids
        )
        sandbox_diagnosis = statistics.mean(row["scores"]["diagnosis"] for row in runtime_rows["agent_sandbox"])
        inprocess_diagnosis = statistics.mean(row["scores"]["diagnosis"] for row in runtime_rows["inprocess"])
        repeated_improvement = diagnosis_wins >= 2 and sandbox_diagnosis > inprocess_diagnosis
        if repeated_improvement:
            causal_improvement_cases.append(case_id)
        repeated_regression = diagnosis_losses >= 2 and sandbox_diagnosis < inprocess_diagnosis
        if repeated_regression:
            causal_regression_cases.append(case_id)
        cases[case_id] = {
            "repetitions": sorted(repetition_by_packet[packet_id] for packet_id in packet_ids),
            "inprocess": blind_case_runtime_summary(runtime_rows["inprocess"], dimensions),
            "agent_sandbox": blind_case_runtime_summary(runtime_rows["agent_sandbox"], dimensions),
            "sandbox_diagnosis_wins": diagnosis_wins,
            "sandbox_diagnosis_losses": diagnosis_losses,
            "repeated_causal_improvement": repeated_improvement,
            "repeated_causal_regression": repeated_regression,
        }
    non_regression = (
        summary["agent_sandbox"]["average_total"] >= summary["inprocess"]["average_total"]
        and summary["agent_sandbox"]["dimension_averages"]["diagnosis"] >= summary["inprocess"]["dimension_averages"]["diagnosis"]
        and not causal_regression_cases
    )
    return {
        "status": "scored", "complete": True, "non_regression": non_regression,
        "material_causal_improvement": len(causal_improvement_cases) > 1,
        "causal_improvement_cases": causal_improvement_cases,
        "causal_regression_cases": causal_regression_cases,
        "rubric_version": rubric_version, "score_max": score_max, "dimensions": dimensions,
        "packet_set_sha256": packet_hash, "reference_set_sha256": reference_hash,
        "score_set_sha256": score_hash, "scoring_timestamp": scoring_timestamp,
        "arms": summary, "cases": cases,
    }

def build_per_case_report(keys: list[tuple[str, int]], inprocess: dict[tuple[str, int], dict[str, Any]], sandbox: dict[tuple[str, int], dict[str, Any]], blind_quality: dict[str, Any]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    blind_cases = blind_quality.get("cases", {}) if isinstance(blind_quality.get("cases"), dict) else {}
    for case_id in sorted({key[0] for key in keys}):
        case_keys = [key for key in keys if key[0] == case_id]
        left = inprocess_metrics([inprocess[key] for key in case_keys])
        right = sandbox_metrics([sandbox[key] for key in case_keys])
        blind_case = blind_cases.get(case_id)
        model_unresolved = any(
            record.get("missing_must") or record.get("unresolved_details")
            for key in case_keys
            for record in (inprocess[key], sandbox[key])
        )
        blind_unresolved = not isinstance(blind_case, dict) or any(
            blind_case[runtime]["causal_assessment"]["unresolved_trials"] > 0
            for runtime in ("inprocess", "agent_sandbox")
        )
        unresolved = any(
            metrics["invalid_trials"] or metrics["no_result_trials"] or metrics["runtime_failure_trials"]
            for metrics in (left, right)
        ) or model_unresolved or blind_unresolved
        result[case_id] = {
            "evidence_mode": inprocess[case_keys[0]]["evidence_mode"],
            "repetitions": [key[1] for key in case_keys],
            "inprocess": left,
            "agent_sandbox": right,
            "blind_quality": blind_case,
            "model_unresolved": model_unresolved,
            "blind_unresolved": blind_unresolved,
            "quality_delta": {
                "signal_rate": round((right["signal_rate"] or 0) - (left["signal_rate"] or 0), 4),
                "diagnosis_signal_rate": round((right["diagnosis_signal_rate"] or 0) - (left["diagnosis_signal_rate"] or 0), 4),
                "valid_rate": round((right["valid_rate"] or 0) - (left["valid_rate"] or 0), 4),
            },
            "unresolved": unresolved,
        }
    return result


def build_report(args: argparse.Namespace) -> dict[str, Any]:
    inprocess_records = read_jsonl(args.inprocess, "inprocess")
    sandbox_records = read_jsonl(args.sandbox, "sandbox")
    inprocess = index_records(inprocess_records, "inprocess")
    sandbox = index_records(sandbox_records, "sandbox")
    keys = validate_pairs(inprocess, sandbox)
    validate_matrix_identity(keys, inprocess, sandbox)
    matrix_engine_commit = inprocess[keys[0]]["engine_commit"]
    try:
        final_branch_head = subprocess.check_output(["git", "-C", args.repo, "rev-parse", "HEAD"], text=True, stderr=subprocess.DEVNULL).strip()
    except subprocess.CalledProcessError as exc:
        raise ReportError("resolve final branch HEAD") from exc
    if not COMMIT_RE.fullmatch(final_branch_head) or matrix_engine_commit != final_branch_head:
        raise ReportError(f"matrix_engine_commit {matrix_engine_commit} differs from final_branch_head {final_branch_head}")
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
    blind_quality = {
        "status": "not_scored", "complete": False, "non_regression": False,
        "material_causal_improvement": False, "causal_improvement_cases": [],
        "rubric_version": rubric_version, "score_max": score_max, "dimensions": dimensions,
    }
    if args.blind_scores:
        if not args.blind_map_input or not args.score_freeze:
            raise ReportError("--blind-scores requires --blind-map-input and --score-freeze")
        if args.blind_map:
            raise ReportError("blind map generation and score unblinding must be separate operations")
        blind_quality = load_blind_quality(args.blind_map_input, args.blind_scores, args.score_freeze, references, reference_hash, keys, inprocess, sandbox, rubric_version, score_max, dimensions)
    evidence_mode_coverage = {
        mode: {
            "cases": sorted({key[0] for key in keys if inprocess[key]["evidence_mode"] == mode}),
            "trials": sum(inprocess[key]["evidence_mode"] == mode for key in keys),
        }
        for mode in EVIDENCE_MODES
    }
    evidence_modes_complete = all(item["trials"] > 0 for item in evidence_mode_coverage.values())
    per_case = build_per_case_report(keys, inprocess, sandbox, blind_quality)
    criteria = evaluate_criteria(
        inprocess_summary, sandbox_summary, per_case, len(keys), args.expected_pairs, holdouts_ok,
        blind_quality, evidence_modes_complete,
    )
    report = {
        "version": 4,
        "matrix_engine_commit": matrix_engine_commit,
        "final_branch_head": final_branch_head,
        "pair_count": len(keys),
        "planned_operations": args.expected_pairs * 2,
        "completed_operations": len(keys) * 2,
        "cases": sorted({key[0] for key in keys}),
        "repetitions": sorted({key[1] for key in keys}),
        "holdout_repetitions": holdout_counts,
        "holdouts_complete": holdouts_ok,
        "evidence_mode_coverage": evidence_mode_coverage,
        "evidence_modes_complete": evidence_modes_complete,
        "provider_config_sha256": inprocess[keys[0]]["provider_config_sha256"],
        "benchmark_manifest_sha256": inprocess[keys[0]]["benchmark_manifest_sha256"],
        "pricing": inprocess[keys[0]]["pricing"],
        "inprocess": inprocess_summary,
        "agent_sandbox": sandbox_summary,
        "per_case": per_case,
        "unresolved_cases": [case_id for case_id, item in per_case.items() if item["unresolved"]],
        "quality_delta": {
            "signal_rate": round((sandbox_summary["signal_rate"] or 0) - (inprocess_summary["signal_rate"] or 0), 4),
            "diagnosis_signal_rate": round((sandbox_summary["diagnosis_signal_rate"] or 0) - (inprocess_summary["diagnosis_signal_rate"] or 0), 4),
            "valid_rate": round((sandbox_summary["valid_rate"] or 0) - (inprocess_summary["valid_rate"] or 0), 4),
        },
        "disposition_follow_up": {
            "required": inprocess_summary["grounded_trials"] == 0 and sandbox_summary["grounded_trials"] == 0 and inprocess_summary["preliminary_trials"] == len(keys) and sandbox_summary["preliminary_trials"] == len(keys),
            "reason": "both analyzers completed only preliminary investigation_incomplete analyses; diagnose the shared stopping condition before another scored matrix",
        },
        "source_citation_capabilities": {
            "inprocess": {**{key: inprocess_summary[key] for key in ("source_citation_emitted_trials", "source_citation_verified_trials", "source_citation_emitted_count", "source_citation_verified_count")}, "verified_to_emitted_ratio": rate(inprocess_summary["source_citation_verified_count"], inprocess_summary["source_citation_emitted_count"])},
            "agent_sandbox": {**{key: sandbox_summary[key] for key in ("source_citation_emitted_trials", "source_citation_verified_trials", "source_citation_emitted_count", "source_citation_verified_count")}, "verified_to_emitted_ratio": rate(sandbox_summary["source_citation_verified_count"], sandbox_summary["source_citation_emitted_count"])},
            "comparative_gate": False,
        },
        "architecture_difference": {
            "comparison_type": "system_comparison_not_model_only_ab",
            "inprocess": "dashboard-owned agentic function-calling loop over artifact and pinned-source tools",
            "agent_sandbox": "frozen read-only workspace analyzed by OpenCode inside Agent Sandbox",
        },
        "simplicity": simplicity,
        "blind_quality": blind_quality,
        "criteria": criteria,
        "shadow_comparison": criteria["shadow_comparison"],
        "authoritative_analyzer": "inprocess_unchanged",
        "limitations": [
            "automatic regex signals remain separate from blinded causal-quality scoring",
            "the comparison evaluates two analyzer systems with different tool runtimes, not only a model response",
            "even shadow_materially_better does not authorize production replacement or publication",
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
