#!/usr/bin/env python3
"""Validate author-aster-diagnostics report contract v2."""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import re
import sys
import tempfile
from pathlib import Path
from typing import Any

CLASSIFICATIONS = {"recommended", "experimental", "rejected", "unresolved"}
PLANES = {"deterministic", "same_author_review", "fresh_holdout", "dashboard_provider"}
COMPLETED = {"passed", "failed", "partial"}
DENY_CATEGORIES = {
    "locked_benchmark_manifest",
    "answer_bearing_benchmark_test",
    "prior_diagnosis",
    "scoring_or_forbidden_file",
    "manual_recipe",
    "previous_evaluation_output",
}
STORAGE_DIMENSIONS = ("same_volume", "same_pvc", "same_pod", "same_node", "same_time_window")


def canonical(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def add(errors: list[str], path: str, message: str) -> None:
    errors.append(f"{path}: {message}")


def resolve(root: dict[str, Any], ref: str) -> Any:
    value: Any = root
    for part in ref.removeprefix("#/").split("/"):
        value = value[part.replace("~1", "/").replace("~0", "~")]
    return value


def type_ok(value: Any, expected: str) -> bool:
    return {
        "object": isinstance(value, dict),
        "array": isinstance(value, list),
        "string": isinstance(value, str),
        "integer": isinstance(value, int) and not isinstance(value, bool),
        "boolean": isinstance(value, bool),
        "null": value is None,
    }.get(expected, True)


def check_schema(value: Any, schema: Any, root: dict[str, Any], path: str, errors: list[str]) -> None:
    if schema is False:
        add(errors, path, "is not allowed")
        return
    if schema is True or not isinstance(schema, dict):
        return
    if "$ref" in schema:
        check_schema(value, resolve(root, schema["$ref"]), root, path, errors)
        return
    if "anyOf" in schema:
        if not any(not branch_errors(value, branch, root, path) for branch in schema["anyOf"]):
            add(errors, path, "does not match any allowed shape")
            return
    if "allOf" in schema:
        for branch in schema["allOf"]:
            check_schema(value, branch, root, path, errors)
    if "if" in schema and not branch_errors(value, schema["if"], root, path):
        check_schema(value, schema.get("then", True), root, path, errors)

    if "const" in schema and value != schema["const"]:
        add(errors, path, f"must equal {schema['const']!r}")
    if "enum" in schema and value not in schema["enum"]:
        add(errors, path, f"must be one of {schema['enum']!r}")

    expected = schema.get("type")
    expected_types = [expected] if isinstance(expected, str) else expected
    if isinstance(expected_types, list) and not any(type_ok(value, item) for item in expected_types):
        add(errors, path, f"must have type {expected_types!r}")
        return

    if isinstance(value, dict):
        required = set(schema.get("required", []))
        for key in sorted(required - value.keys()):
            add(errors, f"{path}.{key}", "is required")
        properties = schema.get("properties", {})
        for key, item in value.items():
            if key in properties:
                check_schema(item, properties[key], root, f"{path}.{key}", errors)
            elif schema.get("additionalProperties") is False:
                add(errors, f"{path}.{key}", "is not allowed")
    if isinstance(value, list):
        if len(value) < schema.get("minItems", 0):
            add(errors, path, f"requires at least {schema['minItems']} items")
        if schema.get("uniqueItems") and len({canonical(item) for item in value}) != len(value):
            add(errors, path, "must contain unique items")
        if "items" in schema:
            for index, item in enumerate(value):
                check_schema(item, schema["items"], root, f"{path}[{index}]", errors)
    if isinstance(value, str):
        if len(value) < schema.get("minLength", 0):
            add(errors, path, f"requires at least {schema['minLength']} characters")
        if "pattern" in schema and not re.fullmatch(schema["pattern"], value):
            add(errors, path, f"does not match {schema['pattern']!r}")
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        if "minimum" in schema and value < schema["minimum"]:
            add(errors, path, f"must be at least {schema['minimum']}")


def branch_errors(value: Any, schema: Any, root: dict[str, Any], path: str) -> list[str]:
    errors: list[str] = []
    check_schema(value, schema, root, path, errors)
    return errors


def unique(values: list[str], errors: list[str], path: str) -> None:
    if len(values) != len(set(values)):
        add(errors, path, "must contain unique values")


def aggregate_event_kind(kinds: list[str]) -> str:
    values = set(kinds)
    if values == {"not_applicable"}:
        return "not_applicable"
    values.discard("not_applicable")
    if "unresolved" in values or not values:
        return "unresolved"
    if values == {"recurrence"}:
        return "recurrence"
    if values == {"generalization"}:
        return "generalization"
    return "mixed"


def schema_contract(schema: dict[str, Any]) -> list[str]:
    errors: list[str] = []
    if not str(schema.get("$id", "")).endswith("report-schema-v2.json"):
        add(errors, "schema.$id", "must identify report schema v2")
    properties = schema.get("properties", {})
    if properties.get("schema_version", {}).get("const") != 2:
        add(errors, "schema.properties.schema_version.const", "must equal 2")
    if set(properties.get("document_type", {}).get("enum", [])) != {"failure_corpus", "benchmark_results"}:
        add(errors, "schema.properties.document_type.enum", "does not match validator")
    defs = schema.get("$defs", {})
    if set(defs.get("classification", {}).get("enum", [])) != CLASSIFICATIONS:
        add(errors, "schema.$defs.classification.enum", "does not match validator")
    if set(defs.get("evidence_plane", {}).get("enum", [])) != PLANES:
        add(errors, "schema.$defs.evidence_plane.enum", "does not match validator")
    for name in (
        "blind_access_control",
        "validation_file_manifest",
        "fresh_holdout_diagnosis",
        "benchmark_identity_manifest",
        "benchmark_scoring_overlay",
        "transient_assessment",
        "storage_identity_correlation",
        "post_reveal_event",
        "prompt_regression",
        "scoring_protocol",
        "evaluation_snapshot",
    ):
        if name not in defs:
            add(errors, f"schema.$defs.{name}", "is required by the validator")
    return errors


def semantic_common(doc: dict[str, Any], errors: list[str], label: str) -> None:
    consumer = doc["consumer"]
    if consumer["commit_status"] == "resolved" and consumer["commit"] is None:
        add(errors, f"{label}.consumer.commit", "resolved consumer requires a commit")
    if consumer["commit_status"] == "not_applicable" and consumer["commit"] is not None:
        add(errors, f"{label}.consumer.commit", "non-Git consumer must use null commit")

    planes = doc["evidence_planes"]
    for index, item in enumerate(doc["classifications"]):
        path = f"{label}.classifications[{index}]"
        classification = item["classification"]
        plane = item["evidence_plane"]
        scope = item["scope"]
        if scope == "behavior":
            if classification == "recommended":
                if plane != "dashboard_provider" or planes[plane]["status"] != "passed":
                    add(errors, path, "recommended behavior requires passed dashboard_provider evidence")
                if len(item["supporting_ids"]) < 3:
                    add(errors, f"{path}.supporting_ids", "recommended behavior requires at least three trials")
            if classification == "experimental":
                if plane not in {"fresh_holdout", "dashboard_provider"}:
                    add(errors, path, "experimental behavior requires fresh_holdout or dashboard_provider evidence")
                elif planes[plane]["status"] not in {"partial", "passed"}:
                    add(errors, path, f"experimental behavior requires partial or passed {plane} evidence")
        elif scope == "authoring_decision" and classification == "recommended" and not item["supporting_ids"]:
            add(errors, f"{path}.supporting_ids", "recommended authoring decision requires supporting evidence")
    unique([item["id"] for item in doc["classifications"]], errors, f"{label}.classifications.id")
    unique([item["id"] for item in doc["validation_commands"]], errors, f"{label}.validation_commands.id")

    clean = [item for item in doc["validation_commands"] if item["id"] == "clean-validation-engine"]
    if len(clean) != 1:
        add(errors, f"{label}.validation_commands", "requires exactly one clean-validation-engine command")
    elif clean[0]["status"] != "passed" or "write_validation_file_manifest.py" not in clean[0]["command"] or "--compare" not in clean[0]["command"]:
        add(errors, f"{label}.validation_commands.clean-validation-engine", "must pass deterministic baseline and final file-manifest comparison")
    for index, command in enumerate(doc["validation_commands"]):
        path = f"{label}.validation_commands[{index}]"
        if command["status"] in COMPLETED:
            if not command["output_path"] or not command["output_sha256"]:
                add(errors, path, "completed validation requires output path and SHA-256")
        elif command["output_path"] is not None or command["output_sha256"] is not None:
            add(errors, path, "not-run validation output fields must be null")

    blind = doc["blind_access_control"]
    categories = [item["category"] for item in blind["denylist"]]
    if set(categories) != DENY_CATEGORIES or len(categories) != len(DENY_CATEGORIES):
        add(errors, f"{label}.blind_access_control.denylist", "must contain each required pre-freeze deny category exactly once")
    unique([item["id"] for item in blind["access_log"]], errors, f"{label}.blind_access_control.access_log.id")
    for index, entry in enumerate(blind["access_log"]):
        path = f"{label}.blind_access_control.access_log[{index}]"
        if entry["phase"] == "pre_freeze" and entry["category"] in DENY_CATEGORIES and entry["decision"] != "blocked":
            add(errors, path, "pre-freeze access to denylisted answer-bearing material must be blocked")
        if entry["decision"] == "allowed" and entry["access_method"] == "wrapper":
            if not entry["content_sha256"] or entry["bytes_read"] is None:
                add(errors, path, "wrapper-mediated allowed access requires content hash and byte count")
        if entry["decision"] == "blocked" and (entry["content_sha256"] is not None or entry["bytes_read"] is not None):
            add(errors, path, "blocked access must not record content bytes")
        if blind["enforcement"] == "wrapper_enforced" and entry["phase"] == "pre_freeze" and entry["access_method"] == "self_reported":
            add(errors, path, "wrapper-enforced packages cannot use self-reported pre-freeze accesses")
    if blind["violations"]:
        add(errors, f"{label}.blind_access_control.violations", "must be empty for a valid blind package")
    if blind["enforcement"] == "wrapper_enforced":
        if not blind["wrapper_path"] or not blind["wrapper_sha256"]:
            add(errors, f"{label}.blind_access_control", "wrapper_enforced requires wrapper path and SHA-256")
    elif not blind["limitations"]:
        add(errors, f"{label}.blind_access_control.limitations", "self-reported access requires an explicit limitation")

    snapshot = doc["evaluation_snapshot"]
    if snapshot["mode"] == "committed_engine":
        if snapshot["companion_files"]:
            add(errors, f"{label}.evaluation_snapshot.companion_files", "committed engine snapshot must not list uncommitted companions")
    elif not snapshot["skill_manifest_path"] or not snapshot["skill_manifest_sha256"]:
        add(errors, f"{label}.evaluation_snapshot", "uncommitted skill snapshot requires a manifest path and SHA-256")
    if snapshot["mode"] == "uncommitted_skill_only" and not snapshot["limitations"]:
        add(errors, f"{label}.evaluation_snapshot.limitations", "skill-only snapshot must disclose omitted companion validation files")
    if snapshot["mode"] == "uncommitted_skill_with_companions" and not snapshot["companion_files"]:
        add(errors, f"{label}.evaluation_snapshot.companion_files", "companion snapshot mode requires at least one companion file")

    if doc["validation_file_manifests"]["comparison"] != "exact_match":
        add(errors, f"{label}.validation_file_manifests.comparison", "baseline and final file manifests must match exactly")
    if doc["recipe_exemplar_policy"]["existing_consumer_recipes_are_trusted_quality_exemplars"]:
        add(errors, f"{label}.recipe_exemplar_policy", "existing consumer recipes are not trusted quality exemplars")


def semantic_diagnosis(record: dict[str, Any], errors: list[str], path: str) -> None:
    evidence_list = [item["id"] for item in record.get("evidence", [])]
    unique(evidence_list, errors, f"{path}.evidence.id")
    evidence_ids = set(evidence_list)
    for evidence_index, item in enumerate(record.get("evidence", [])):
        start_line, end_line = item["line_start"], item["line_end"]
        if isinstance(start_line, int) and isinstance(end_line, int) and end_line < start_line:
            add(errors, f"{path}.evidence[{evidence_index}]", "line_end must be at least line_start")
    for step_index, step in enumerate(record.get("causal_chain", [])):
        unknown = sorted(set(step["evidence_ids"]) - evidence_ids)
        if unknown:
            add(errors, f"{path}.causal_chain[{step_index}].evidence_ids", f"unknown evidence {unknown}")
    for hypothesis_index, hypothesis in enumerate(record.get("competing_hypotheses", [])):
        unknown = sorted(set(hypothesis["evidence_ids"]) - evidence_ids)
        if unknown:
            add(errors, f"{path}.competing_hypotheses[{hypothesis_index}].evidence_ids", f"unknown evidence {unknown}")

    comparison = record.get("passing_comparison", {})
    if comparison.get("status") == "available":
        if not comparison.get("case_id") or not comparison.get("result") or not comparison.get("evidence_ids"):
            add(errors, f"{path}.passing_comparison", "available comparison requires case, result, and evidence")
    elif comparison.get("status") == "unavailable" and not comparison.get("unavailable_reason"):
        add(errors, f"{path}.passing_comparison.unavailable_reason", "is required")
    if set(comparison.get("evidence_ids", [])) - evidence_ids:
        add(errors, f"{path}.passing_comparison.evidence_ids", "references unknown evidence")

    ownership = record.get("ownership", {})
    assigned = ownership.get("assigned_component")
    strength = ownership.get("assignment_strength")
    positive = ownership.get("positive_owner_evidence_ids", [])
    exculpatory = ownership.get("exculpatory_evidence_ids", [])
    if strength == "none" and assigned is not None:
        add(errors, f"{path}.ownership", "assignment_strength none requires no assigned component")
    if assigned is not None and (strength == "none" or not positive):
        add(errors, f"{path}.ownership", "assigned owner requires positive component-specific evidence")
    if set(positive + exculpatory) - evidence_ids:
        add(errors, f"{path}.ownership", "ownership evidence references unknown evidence")
    if set(ownership.get("possible_components", [])) & set(ownership.get("excluded_components", [])):
        add(errors, f"{path}.ownership", "a component cannot be both possible and excluded")
    if assigned in ownership.get("excluded_components", []):
        add(errors, f"{path}.ownership", "assigned component cannot be excluded")

    storage = ownership.get("storage_identity_correlation", {})
    if set(storage.get("evidence_ids", [])) - evidence_ids:
        add(errors, f"{path}.ownership.storage_identity_correlation.evidence_ids", "references unknown evidence")
    values = [storage.get(field) for field in STORAGE_DIMENSIONS]
    if storage.get("applicable"):
        if "not_applicable" in values:
            add(errors, f"{path}.ownership.storage_identity_correlation", "applicable storage diagnosis must classify every identity dimension")
        if "missing" in values:
            if not storage.get("limitation"):
                add(errors, f"{path}.ownership.storage_identity_correlation.limitation", "is required when an identity dimension is missing")
            if not ownership.get("open_handoffs"):
                add(errors, f"{path}.ownership.open_handoffs", "requires an open handoff when a storage identity dimension is missing")
    elif any(value != "not_applicable" for value in values):
        add(errors, f"{path}.ownership.storage_identity_correlation", "non-storage diagnosis must mark every identity dimension not_applicable")

    transient = record.get("transient", {})
    same_run = transient.get("same_run_evidence_ids", [])
    cross_run = transient.get("cross_run_evidence_ids", [])
    if set(same_run + cross_run) - evidence_ids:
        add(errors, f"{path}.transient", "transient evidence references unknown evidence")
    if transient.get("status") == "true" and not same_run:
        add(errors, f"{path}.transient.same_run_evidence_ids", "transient true requires same-run recovery or forward-progress evidence")
    if transient.get("status") == "unresolved":
        if not transient.get("unresolved_reason"):
            add(errors, f"{path}.transient.unresolved_reason", "is required for unresolved transient status")
    elif transient.get("unresolved_reason") is not None:
        add(errors, f"{path}.transient.unresolved_reason", "must be null when transient status is true or false")


def semantic_corpus(corpus: dict[str, Any], errors: list[str]) -> dict[str, dict[str, Any]]:
    cases = {case["id"]: case for case in corpus["cases"]}
    unique([case["id"] for case in corpus["cases"]], errors, "failure_corpus.cases.id")
    unique([case["causal_event_id"] for case in corpus["cases"]], errors, "failure_corpus.cases.causal_event_id")
    diagnosis = {
        "failure_class", "phase_reached", "initiating_error", "terminal_wrapper",
        "causal_chain", "evidence", "competing_hypotheses", "passing_comparison",
        "ownership", "transient", "reusable_project_facts", "recurrence_signatures",
        "prompt_candidates", "recipe_candidate", "unresolved",
    }
    for index, case in enumerate(corpus["cases"]):
        path = f"failure_corpus.cases[{index}]"
        status, revision = case["source_revision_status"], case["source_revision"]
        if status in {"resolved_from_build", "resolved_from_artifact", "branch_tip_only"} and revision is None:
            add(errors, f"{path}.source_revision", f"{status} requires a revision")
        if status == "unavailable" and revision is not None:
            add(errors, f"{path}.source_revision", "unavailable revision must be null")
        kind = case["pre_freeze_holdout_kind"]
        scope = case["holdout_event_scope"]
        if case["split"] == "final_holdout":
            if kind not in {"recurrence", "generalization", "unresolved"}:
                add(errors, f"{path}.pre_freeze_holdout_kind", "final holdout must have a pre-freeze causal hypothesis")
            if scope not in {"single_event_identity", "build_level_unresolved"}:
                add(errors, f"{path}.holdout_event_scope", "final holdout must declare its event scope")
            if scope == "single_event_identity" and case["test_name"] == "Prow job execution":
                add(errors, f"{path}.test_name", "single-event holdout requires a specific analyzer or test identity")
        else:
            if kind != "not_applicable":
                add(errors, f"{path}.pre_freeze_holdout_kind", "non-holdout case must be not_applicable")
            if scope != "not_applicable":
                add(errors, f"{path}.holdout_event_scope", "non-holdout case must be not_applicable")
        if case["embargoed"]:
            if case["split"] != "final_holdout":
                add(errors, path, "only a final holdout may be embargoed")
            if diagnosis & case.keys():
                add(errors, path, "embargoed holdout contains diagnosis fields")
            continue
        for field in sorted(diagnosis - case.keys()):
            add(errors, f"{path}.{field}", "is required after reveal")
        if diagnosis <= case.keys():
            semantic_diagnosis(case, errors, path)
            overlap = set(case["reusable_project_facts"]) & set(case["recurrence_signatures"])
            if overlap:
                add(errors, path, "stable project facts and highly specific recurrence signatures must be separate")
    return cases


def semantic_benchmark(benchmark: dict[str, Any], cases: dict[str, dict[str, Any]], errors: list[str]) -> None:
    summary = benchmark["corpus"]
    for split, field in {
        "authoring": "authoring_case_ids",
        "validation": "validation_case_ids",
        "final_holdout": "final_holdout_case_ids",
    }.items():
        expected = sorted(case_id for case_id, case in cases.items() if case["split"] == split)
        if sorted(summary[field]) != expected:
            add(errors, f"benchmark_results.corpus.{field}", f"must equal {expected}")

    regression = benchmark["prompt_regression"]
    if regression["baseline_prompt_sha256"] != benchmark["consumer"]["existing_prompt_sha256"]:
        add(errors, "benchmark_results.prompt_regression.baseline_prompt_sha256", "must equal the consumer existing prompt hash")
    prompt_hashes = {item["sha256"] for item in benchmark["prompt_versions"]}
    if regression["proposed_prompt_sha256"] not in prompt_hashes:
        add(errors, "benchmark_results.prompt_regression.proposed_prompt_sha256", "must reference a recorded prompt version")
    unique([item["id"] for item in regression["items"]], errors, "benchmark_results.prompt_regression.items.id")
    provenance = regression["baseline_provenance"]
    unique([item["id"] for item in provenance], errors, "benchmark_results.prompt_regression.baseline_provenance.id")
    if regression["baseline_state"] == "existing_prompt" and not regression["items"]:
        add(errors, "benchmark_results.prompt_regression.items", "existing prompt requires a knowledge-retention inventory")
    if regression["baseline_state"] == "existing_prompt" and not provenance:
        add(errors, "benchmark_results.prompt_regression.baseline_provenance", "existing prompt requires provenance before final holdout selection")
    for index, item in enumerate(provenance):
        path = f"benchmark_results.prompt_regression.baseline_provenance[{index}]"
        if item["source_path"] is None and item["source_sha256"] is not None:
            add(errors, path, "source SHA-256 requires a source path")
        if item["source_path"] is not None and item["source_sha256"] is None:
            add(errors, path, "source path requires a SHA-256")
        if item["build_id"] is None and item["source_path"] is None:
            add(errors, path, "provenance requires a build identity or a hashed source path")
        if item["source_split"] in {"authoring", "validation", "final_holdout"} and (item["job_name"] is None or item["build_id"] is None):
            add(errors, path, "historical corpus provenance requires job and build identity")
        if item["source_split"] == "manual_source" and item["source_path"] is None:
            add(errors, path, "manual-source provenance requires a hashed source path")
    for case_id, case in cases.items():
        if case["split"] != "final_holdout":
            continue
        for item in provenance:
            if item["job_name"] == case["job_name"] and item["build_id"] == case["build_id"]:
                add(errors, f"benchmark_results.corpus.final_holdout_case_ids", f"final holdout {case_id} overlaps baseline prompt provenance build {case['build_id']}")
            if item["causal_event_id"] is not None and item["causal_event_id"] == case["causal_event_id"]:
                add(errors, f"benchmark_results.corpus.final_holdout_case_ids", f"final holdout {case_id} overlaps baseline prompt provenance event {case['causal_event_id']}")
    for index, item in enumerate(regression["items"]):
        path = f"benchmark_results.prompt_regression.items[{index}]"
        unknown = sorted(set(item["evidence_case_ids"]) - cases.keys())
        if unknown:
            add(errors, f"{path}.evidence_case_ids", f"unknown cases {unknown}")
        if item["category"] == "stable_fact" and item["disposition"] in {"removed", "deferred"} and not item["evidence_case_ids"]:
            add(errors, path, "removing or deferring a stable fact requires explicit case evidence")

    protocol = benchmark["scoring_protocol"]
    locked_allowed = (
        protocol["mode"] == "independent_pre_prompt"
        and protocol["overlay_frozen_before_prompt_access"]
        and protocol["scoring_author_id"]
        and protocol["prompt_scorer_id"]
        and protocol["scoring_author_id"] != protocol["prompt_scorer_id"]
    )
    if protocol["mode"] == "independent_pre_prompt" and not locked_allowed:
        add(errors, "benchmark_results.scoring_protocol", "independent scoring requires frozen overlay and distinct author and scorer IDs")
    if protocol["mode"] != "independent_pre_prompt" and protocol["overlay_frozen_before_prompt_access"]:
        add(errors, "benchmark_results.scoring_protocol", "only independent_pre_prompt may claim a pre-access frozen overlay")
    if protocol["mode"] != "independent_pre_prompt" and not protocol["limitations"]:
        add(errors, "benchmark_results.scoring_protocol.limitations", "non-independent scoring requires an explicit limitation")

    all_fresh_ids: list[str] = []
    holdout_ids: set[str] = set()
    scored_case_ids: set[str] = set()
    for group, split in (("authoring_validation", "validation"), ("fresh_holdout_trials", "final_holdout")):
        for index, trial in enumerate(benchmark[group]):
            path = f"benchmark_results.{group}[{index}]"
            all_fresh_ids.append(trial["id"])
            if group == "fresh_holdout_trials":
                holdout_ids.add(trial["id"])
            if trial["status"] in COMPLETED and (not trial["diagnosis_path"] or not trial["diagnosis_sha256"]):
                add(errors, path, "completed fresh trial requires diagnosis path and SHA-256")
            if group == "authoring_validation":
                if trial["review_mode"] == "same_author_review" and trial["classification"] not in {"unresolved", "rejected"}:
                    add(errors, f"{path}.classification", "same-author review is at most unresolved")
                if trial["pre_freeze_holdout_kind"] != "not_applicable" or trial["post_reveal_causal_kind"] != "not_applicable" or trial["post_reveal_event_kinds"] != ["not_applicable"] or trial["reclassified_after_reveal"]:
                    add(errors, path, "validation trials must use not_applicable holdout fields without reclassification")
            else:
                if trial["review_mode"] != "fresh_session":
                    add(errors, f"{path}.review_mode", "final holdout requires a fresh session")
                if trial["classification"] == "recommended":
                    add(errors, f"{path}.classification", "fresh holdout evidence is at most experimental")
                expected_kind = aggregate_event_kind(trial["post_reveal_event_kinds"])
                if trial["post_reveal_causal_kind"] != expected_kind:
                    add(errors, f"{path}.post_reveal_causal_kind", f"must aggregate event kinds to {expected_kind}")
            if group == "fresh_holdout_trials" and trial["status"] in {"passed", "partial"}:
                if trial["semantic_score"] is None:
                    add(errors, path, "scored fresh holdout requires a semantic score")
                if locked_allowed and trial["locked_score"] is None:
                    add(errors, path, "independently scored fresh holdout requires a locked score")
                if not locked_allowed and trial["locked_score"] is not None:
                    add(errors, path, "locked score requires an independently frozen scoring overlay")
                scored_case_ids.add(trial["case_id"])
            case = cases.get(trial["case_id"])
            if not case or case["split"] != split:
                add(errors, f"{path}.case_id", f"must reference {split} case")
                continue
            if split == "validation" and case["fresh_session_id"] != trial["session_id"]:
                add(errors, f"{path}.session_id", "must match corpus fresh_session_id")
            if split == "final_holdout":
                if case["fresh_session_id"] not in {None, trial["session_id"]}:
                    add(errors, f"{path}.session_id", "conflicts with corpus fresh_session_id")
                if trial["pre_freeze_holdout_kind"] != case["pre_freeze_holdout_kind"]:
                    add(errors, f"{path}.pre_freeze_holdout_kind", "must match the frozen corpus")
                changed = trial["pre_freeze_holdout_kind"] != trial["post_reveal_causal_kind"]
                if trial["reclassified_after_reveal"] != changed:
                    add(errors, f"{path}.reclassified_after_reveal", "must state whether the aggregate post-reveal kind changed")
    unique(all_fresh_ids, errors, "benchmark_results.fresh_trial.id")

    dashboard_ids: set[str] = set()
    for index, trial in enumerate(benchmark["dashboard_trials"]):
        path = f"benchmark_results.dashboard_trials[{index}]"
        dashboard_ids.add(trial["id"])
        if trial["status"] in COMPLETED and (not trial["result_path"] or not trial["result_sha256"]):
            add(errors, path, "completed dashboard trial requires result path and SHA-256")
        if trial["status"] in {"passed", "partial"}:
            if trial["semantic_score"] is None:
                add(errors, path, "scored dashboard trial requires a semantic score")
            if locked_allowed and trial["locked_score"] is None:
                add(errors, path, "independently scored dashboard trial requires a locked score")
            if not locked_allowed and trial["locked_score"] is not None:
                add(errors, path, "locked score requires an independently frozen scoring overlay")
            scored_case_ids.add(trial["case_id"])
        case = cases.get(trial["case_id"])
        if trial["condition"] in {"A", "B", "C"}:
            if not case or case["split"] != "final_holdout":
                add(errors, f"{path}.case_id", "A/B/C dashboard trials must reference a final holdout")
            expected_kind = aggregate_event_kind(trial["post_reveal_event_kinds"])
            if trial["post_reveal_causal_kind"] != expected_kind:
                add(errors, f"{path}.post_reveal_causal_kind", f"must aggregate event kinds to {expected_kind}")
        elif trial["post_reveal_causal_kind"] != "not_applicable" or trial["post_reveal_event_kinds"] != ["not_applicable"]:
            add(errors, path, "control trials must use not_applicable causal fields")
    unique(list(dashboard_ids), errors, "benchmark_results.dashboard_trials.id")

    conditions = benchmark["condition_manifests"]
    unique([item["id"] for item in conditions], errors, "benchmark_results.condition_manifests.id")
    for index, item in enumerate(conditions):
        path = f"benchmark_results.condition_manifests[{index}]"
        if item["consumer_commit"] != benchmark["consumer"]["commit"]:
            add(errors, f"{path}.consumer_commit", "must match consumer.commit")
        case = cases.get(item["case_id"])
        if item["condition"] in {"A", "B", "C"} and (not case or case["split"] != "final_holdout"):
            add(errors, f"{path}.case_id", "A/B/C condition must reference a final holdout")
        if item["scoring_overlay_status"] == "not_revealed":
            if item["scoring_overlay_path"] is not None or item["scoring_overlay_sha256"] is not None:
                add(errors, path, "not_revealed scoring overlay must have null path and hash")
        elif not item["scoring_overlay_path"] or not item["scoring_overlay_sha256"]:
            add(errors, path, "available scoring overlay requires path and SHA-256")
    for case_id, case in cases.items():
        if case["split"] != "final_holdout":
            continue
        counts = {name: 0 for name in ("A", "B", "C")}
        for item in conditions:
            if item["case_id"] == case_id and item["condition"] in counts:
                counts[item["condition"]] += 1
        if any(count != 1 for count in counts.values()):
            add(errors, "benchmark_results.condition_manifests", f"final holdout {case_id} requires exactly one A, B, and C condition command")
    if not benchmark["provider"]["available"] and any(item["status"] != "not_run" for item in conditions):
        add(errors, "benchmark_results.condition_manifests", "provider-unavailable manifests must be not_run")
    for case_id in scored_case_ids:
        case_conditions = [item for item in conditions if item["case_id"] == case_id and item["condition"] in {"A", "B", "C"}]
        if not case_conditions or any(item["scoring_overlay_status"] != "available" for item in case_conditions):
            add(errors, "benchmark_results.condition_manifests", f"scored case {case_id} requires post-reveal scoring overlays for A, B, and C")

    class_ids = {item["id"] for item in benchmark["classifications"]}
    validation_trials = {(trial["case_id"], trial["session_id"]): trial for trial in benchmark["authoring_validation"] if trial["review_mode"] == "fresh_session"}
    for index, proposal in enumerate(benchmark["proposals"]):
        path = f"benchmark_results.proposals[{index}]"
        if proposal["classification_id"] not in class_ids:
            add(errors, f"{path}.classification_id", "references unknown classification")
        events, sessions = [], []
        for miss_index, miss in enumerate(proposal["prompt_only_misses"]):
            mpath = f"{path}.prompt_only_misses[{miss_index}]"
            case = cases.get(miss["case_id"])
            if not case or case["split"] != "validation":
                add(errors, f"{mpath}.case_id", "must reference a validation case")
                continue
            if case["causal_event_id"] != miss["causal_event_id"] or case["fresh_session_id"] != miss["fresh_session_id"]:
                add(errors, mpath, "miss identity must match corpus")
            if (miss["case_id"], miss["fresh_session_id"]) not in validation_trials:
                add(errors, mpath, "requires a recorded fresh-session validation trial")
            events.append(miss["causal_event_id"]); sessions.append(miss["fresh_session_id"])
        unique(events, errors, f"{path}.causal_event_id"); unique(sessions, errors, f"{path}.fresh_session_id")

    for index, item in enumerate(benchmark["classifications"]):
        supporting = set(item["supporting_ids"])
        if item["evidence_plane"] == "fresh_holdout" and supporting - holdout_ids:
            add(errors, f"benchmark_results.classifications[{index}].supporting_ids", "references unknown fresh holdout trial")
        if item["evidence_plane"] == "dashboard_provider" and supporting - dashboard_ids:
            add(errors, f"benchmark_results.classifications[{index}].supporting_ids", "references unknown dashboard trial")
        if item["scope"] == "behavior" and item["classification"] == "recommended":
            supported = [trial for trial in benchmark["dashboard_trials"] if trial["id"] in supporting]
            if not any("generalization" in trial["post_reveal_event_kinds"] for trial in supported):
                add(errors, f"benchmark_results.classifications[{index}]", "recommended behavior requires a post-reveal generalization event")
    if any(item["scope"] == "behavior" and item["classification"] == "recommended" for item in benchmark["classifications"]):
        provider_controls = [control for control in benchmark["controls"] if control["evidence_plane"] == "dashboard_provider"]
        if not provider_controls or any(control["status"] != "passed" for control in provider_controls):
            add(errors, "benchmark_results.controls", "recommended behavior requires all dashboard-provider controls to pass")

    for group in ("authoring_validation", "fresh_holdout_trials", "dashboard_trials"):
        for index, trial in enumerate(benchmark[group]):
            if trial["status"] not in {"passed", "partial"} and (trial["locked_score"] is not None or trial["semantic_score"] is not None):
                add(errors, f"benchmark_results.{group}[{index}]", "unscored trial must have null locked and semantic scores")


def semantic(corpus: dict[str, Any], benchmark: dict[str, Any]) -> list[str]:
    errors: list[str] = []
    semantic_common(corpus, errors, "failure_corpus")
    semantic_common(benchmark, errors, "benchmark_results")
    if benchmark["engine_commit"] != corpus["engine_commit"]:
        add(errors, "benchmark_results.engine_commit", "must match failure corpus")
    for field in (
        "consumer", "freeze_manifest", "blind_access_control",
        "validation_file_manifests", "recipe_exemplar_policy", "evaluation_snapshot",
    ):
        if benchmark[field] != corpus[field]:
            add(errors, f"benchmark_results.{field}", "must match failure corpus")
    cases = semantic_corpus(corpus, errors)
    semantic_benchmark(benchmark, cases, errors)
    return errors


def load(path: Path) -> dict[str, Any]:
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError(f"{path}: root must be an object")
    return value


def bound_file(root: Path, value: Any, errors: list[str], path: str) -> Path | None:
    if not isinstance(value, str) or not value:
        add(errors, path, "must be a non-empty relative path")
        return None
    relative = Path(value)
    if relative.is_absolute() or ".." in relative.parts:
        add(errors, path, "must stay within the selected root")
        return None
    return root / relative


def verify_hash(root: Path, path_value: Any, hash_value: Any, errors: list[str], path: str) -> Path | None:
    target = bound_file(root, path_value, errors, f"{path}.path")
    if target is None:
        return None
    if not target.is_file():
        add(errors, f"{path}.path", "does not exist")
        return None
    if hashlib.sha256(target.read_bytes()).hexdigest() != hash_value:
        add(errors, f"{path}.sha256", "does not match file bytes")
        return None
    return target


def load_bound_json(target: Path | None, schema: dict[str, Any], definition: str, errors: list[str], path: str) -> dict[str, Any] | None:
    if target is None:
        return None
    try:
        value = load(target)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        add(errors, path, f"invalid JSON: {exc}")
        return None
    check_schema(value, schema["$defs"][definition], schema, path, errors)
    return value


def file_bindings(
    corpus_path: Path,
    benchmark_path: Path,
    corpus: dict[str, Any],
    benchmark: dict[str, Any],
    schema: dict[str, Any],
    evidence_root: Path | None = None,
) -> list[str]:
    errors: list[str] = []
    root = corpus_path.resolve().parent.parent
    private_root = evidence_root.resolve() if evidence_root else root
    relative_corpus = corpus_path.resolve().relative_to(root).as_posix()
    if benchmark["corpus"]["path"] != relative_corpus:
        add(errors, "benchmark_results.corpus.path", f"must equal {relative_corpus}")
    if benchmark["corpus"]["sha256"] != hashlib.sha256(corpus_path.read_bytes()).hexdigest():
        add(errors, "benchmark_results.corpus.sha256", "does not match corpus file")
    freeze = corpus["freeze_manifest"]
    if freeze["status"] == "frozen":
        verify_hash(root, freeze["path"], freeze["sha256"], errors, "freeze_manifest")

    blind = corpus["blind_access_control"]
    if blind["enforcement"] == "wrapper_enforced":
        verify_hash(private_root, blind["wrapper_path"], blind["wrapper_sha256"], errors, "blind_access_control.wrapper")
    snapshot = corpus["evaluation_snapshot"]
    if snapshot["skill_manifest_path"]:
        verify_hash(private_root, snapshot["skill_manifest_path"], snapshot["skill_manifest_sha256"], errors, "evaluation_snapshot.skill_manifest")
    for index, companion in enumerate(snapshot["companion_files"]):
        verify_hash(private_root, companion["path"], companion["sha256"], errors, f"evaluation_snapshot.companion_files[{index}]")

    for label, doc in (("failure_corpus", corpus), ("benchmark_results", benchmark)):
        for index, command in enumerate(doc["validation_commands"]):
            if command["status"] in COMPLETED:
                verify_hash(root, command["output_path"], command["output_sha256"], errors, f"{label}.validation_commands[{index}]")

    binding = corpus["validation_file_manifests"]
    baseline_path = verify_hash(root, binding["baseline_path"], binding["baseline_sha256"], errors, "validation_file_manifests.baseline")
    final_path = verify_hash(root, binding["final_path"], binding["final_sha256"], errors, "validation_file_manifests.final")
    baseline = load_bound_json(baseline_path, schema, "validation_file_manifest", errors, "validation_file_manifests.baseline")
    final = load_bound_json(final_path, schema, "validation_file_manifest", errors, "validation_file_manifests.final")
    if baseline is not None and final is not None:
        paths = [item["path"] for item in baseline.get("entries", [])]
        unique(paths, errors, "validation_file_manifests.baseline.entries.path")
        final_paths = [item["path"] for item in final.get("entries", [])]
        unique(final_paths, errors, "validation_file_manifests.final.entries.path")
        if baseline != final:
            add(errors, "validation_file_manifests", "baseline and final manifests must match exactly")

    cases = {case["id"]: case for case in corpus["cases"]}
    for group in ("authoring_validation", "fresh_holdout_trials"):
        for index, trial in enumerate(benchmark[group]):
            if trial["status"] not in COMPLETED:
                continue
            path = f"benchmark_results.{group}[{index}]"
            target = verify_hash(private_root, trial["diagnosis_path"], trial["diagnosis_sha256"], errors, path)
            if group == "authoring_validation":
                continue
            diagnosis = load_bound_json(target, schema, "fresh_holdout_diagnosis", errors, f"{path}.diagnosis")
            if diagnosis is None:
                continue
            semantic_diagnosis(diagnosis, errors, f"{path}.diagnosis")
            event_ids = [item["event_id"] for item in diagnosis["post_reveal_events"]]
            causal_ids = [item["causal_event_id"] for item in diagnosis["post_reveal_events"]]
            unique(event_ids, errors, f"{path}.diagnosis.post_reveal_events.event_id")
            unique(causal_ids, errors, f"{path}.diagnosis.post_reveal_events.causal_event_id")
            evidence_ids = {item["id"] for item in diagnosis["evidence"]}
            for event_index, event in enumerate(diagnosis["post_reveal_events"]):
                event_path = f"{path}.diagnosis.post_reveal_events[{event_index}]"
                unknown = sorted(set(event["evidence_ids"]) - evidence_ids)
                if unknown:
                    add(errors, f"{event_path}.evidence_ids", f"unknown evidence {unknown}")
                semantic_diagnosis({**event, "evidence": diagnosis["evidence"]}, errors, event_path)
            event_kinds = sorted({item["causal_kind"] for item in diagnosis["post_reveal_events"]})
            aggregate = aggregate_event_kind(event_kinds)
            if diagnosis["post_reveal_causal_kind"] != aggregate:
                add(errors, f"{path}.diagnosis.post_reveal_causal_kind", f"must aggregate event kinds to {aggregate}")
            if sorted(trial["post_reveal_event_kinds"]) != event_kinds:
                add(errors, f"{path}.diagnosis.post_reveal_events", "event kinds must match the trial record")
            case = cases.get(trial["case_id"])
            if case and case["holdout_event_scope"] == "single_event_identity" and len(diagnosis["post_reveal_events"]) != 1:
                add(errors, f"{path}.diagnosis.post_reveal_events", "single-event holdout must reveal exactly one causal event")
            for field in (
                "case_id", "prompt_sha256", "session_id", "pre_freeze_holdout_kind",
                "post_reveal_causal_kind", "reclassified_after_reveal", "classification",
            ):
                if diagnosis.get(field) != trial[field]:
                    add(errors, f"{path}.diagnosis.{field}", "must match the trial record")

    for index, trial in enumerate(benchmark["dashboard_trials"]):
        if trial["status"] in COMPLETED:
            verify_hash(private_root, trial["result_path"], trial["result_sha256"], errors, f"benchmark_results.dashboard_trials[{index}]")
    for index, proposal in enumerate(benchmark["proposals"]):
        verify_hash(root, proposal["path"], proposal["sha256"], errors, f"benchmark_results.proposals[{index}]")
    for index, item in enumerate(benchmark["prompt_regression"]["baseline_provenance"]):
        if item["source_path"] is not None:
            verify_hash(private_root, item["source_path"], item["source_sha256"], errors, f"benchmark_results.prompt_regression.baseline_provenance[{index}]")

    prompt_hashes = {benchmark["consumer"]["existing_prompt_sha256"]}
    prompt_hashes.update(item["sha256"] for item in benchmark["prompt_versions"])
    for index, condition in enumerate(benchmark["condition_manifests"]):
        path = f"benchmark_results.condition_manifests[{index}]"
        target = verify_hash(private_root, condition["identity_manifest_path"], condition["identity_manifest_sha256"], errors, f"{path}.identity_manifest")
        identity = load_bound_json(target, schema, "benchmark_identity_manifest", errors, f"{path}.identity_manifest")
        case = cases.get(condition["case_id"])
        if identity is not None:
            expected = {
                "case_id": condition["case_id"],
                "condition": condition["condition"],
                "consumer_commit": condition["consumer_commit"],
                "project_sha256": condition["project_sha256"],
                "prompt_sha256": condition["prompt_sha256"],
                "active_skill_set_hash": condition["active_skill_set_hash"],
            }
            if case:
                expected.update({"job_name": case["job_name"], "build_id": case["build_id"], "test_name": case["test_name"]})
            for field, value in expected.items():
                if identity.get(field) != value:
                    add(errors, f"{path}.identity_manifest.{field}", f"must equal {value!r}")
            if identity.get("prompt_sha256") not in prompt_hashes:
                add(errors, f"{path}.identity_manifest.prompt_sha256", "must reference a recorded prompt hash")
        if condition["scoring_overlay_status"] == "available":
            overlay_target = verify_hash(private_root, condition["scoring_overlay_path"], condition["scoring_overlay_sha256"], errors, f"{path}.scoring_overlay")
            overlay = load_bound_json(overlay_target, schema, "benchmark_scoring_overlay", errors, f"{path}.scoring_overlay")
            if overlay is not None:
                if overlay.get("case_id") != condition["case_id"]:
                    add(errors, f"{path}.scoring_overlay.case_id", "must match the condition case")
                for field in ("reference_diagnosis", "scoring", "forbidden"):
                    verify_hash(private_root, overlay[f"{field}_path"], overlay[f"{field}_sha256"], errors, f"{path}.scoring_overlay.{field}")
    return errors


def validate(corpus: dict[str, Any], benchmark: dict[str, Any], schema: dict[str, Any]) -> list[str]:
    errors = schema_contract(schema)
    check_schema(corpus, schema, schema, "failure_corpus", errors)
    check_schema(benchmark, schema, schema, "benchmark_results", errors)
    if not errors:
        errors.extend(semantic(corpus, benchmark))
    return errors


def fixtures() -> tuple[dict[str, Any], dict[str, Any]]:
    sha = lambda char: char * 64
    git = "a" * 40
    consumer = {
        "repository": "example/project",
        "commit": "b" * 40,
        "commit_status": "resolved",
        "project_sha256": sha("1"),
        "existing_prompt_sha256": sha("2"),
        "active_skill_set_hash": sha("3"),
    }
    freeze = {"status": "not_frozen", "path": None, "sha256": None, "frozen_at_utc": None}
    planes = {
        "deterministic": {"status": "passed", "evaluator": "validator", "limitations": []},
        "same_author_review": {"status": "passed", "evaluator": "author", "limitations": ["not independent"]},
        "fresh_holdout": {"status": "passed", "evaluator": "fresh-evaluator", "limitations": []},
        "dashboard_provider": {"status": "unavailable", "evaluator": None, "limitations": ["not run"]},
    }
    classification = {
        "id": "prompt",
        "item_type": "prompt",
        "scope": "behavior",
        "classification": "unresolved",
        "evidence_plane": "same_author_review",
        "reasons": ["provider unavailable"],
        "supporting_ids": ["json"],
    }
    commands = [
        {
            "id": "json",
            "command": "python3 -m json.tool",
            "status": "passed",
            "output_path": "reports/validation/json.log",
            "output_sha256": sha("4"),
        },
        {
            "id": "clean-validation-engine",
            "command": "python3 write_validation_file_manifest.py --compare baseline.json final.json",
            "status": "passed",
            "output_path": "reports/validation/clean.log",
            "output_sha256": sha("5"),
        },
    ]
    denylist = [
        {"category": category, "path_pattern": f"private/{category}/**", "reason": "answer-bearing material"}
        for category in sorted(DENY_CATEGORIES)
    ]
    blind = {
        "denylist": denylist,
        "access_log": [{
            "id": "schema-fixture",
            "phase": "pre_freeze",
            "category": "schema_only_fixture",
            "path": "references/benchmark-manifest.schema-only.json",
            "purpose": "construct identity-only manifests",
            "decision": "allowed",
            "access_method": "self_reported",
            "content_sha256": None,
            "bytes_read": None,
        }],
        "violations": [],
        "enforcement": "self_reported",
        "wrapper_path": None,
        "wrapper_sha256": None,
        "limitations": ["fixture access is self-reported"],
    }
    file_manifests = {
        "baseline_path": "reports/validation/validation-files-baseline.json",
        "baseline_sha256": sha("6"),
        "final_path": "reports/validation/validation-files-final.json",
        "final_sha256": sha("6"),
        "comparison": "exact_match",
    }
    exemplar = {
        "existing_consumer_recipes_are_trusted_quality_exemplars": False,
        "limitations": ["existing recipes require independent evidence and applicability review"],
    }
    storage_na = {
        "applicable": False,
        "same_volume": "not_applicable",
        "same_pvc": "not_applicable",
        "same_pod": "not_applicable",
        "same_node": "not_applicable",
        "same_time_window": "not_applicable",
        "evidence_ids": [],
        "limitation": None,
    }
    authoring = {
        "id": "case-a",
        "split": "authoring",
        "pre_freeze_holdout_kind": "not_applicable",
        "holdout_event_scope": "not_applicable",
        "embargoed": False,
        "causal_event_id": "event-a",
        "fresh_session_id": None,
        "job_name": "periodic-example",
        "build_id": "123",
        "test_name": "Prow job execution",
        "source_revision": git,
        "source_revision_status": "resolved_from_build",
        "source_revision_provenance": "prowjob.json spec.extra_refs",
        "failure_class": "api_compatibility",
        "phase_reached": "setup",
        "initiating_error": "request failed",
        "terminal_wrapper": "timeout",
        "causal_chain": [{
            "actor": "client",
            "operation": "GET resource",
            "response_or_state": "404",
            "consequence": "sync failed",
            "evidence_ids": ["E1"],
        }],
        "evidence": [{
            "id": "E1",
            "kind": "artifact",
            "path": "build-log.txt",
            "line_start": 1,
            "line_end": 2,
            "claim": "request returned 404",
        }],
        "competing_hypotheses": [{
            "hypothesis": "network timeout",
            "status": "rejected",
            "evidence_ids": ["E1"],
            "reason": "API returned a deterministic 404",
        }],
        "passing_comparison": {
            "status": "unavailable",
            "case_id": None,
            "evidence_ids": [],
            "result": None,
            "unavailable_reason": "no passing neighbor retained",
        },
        "ownership": {
            "assigned_component": None,
            "assignment_strength": "none",
            "possible_components": ["client", "server"],
            "excluded_components": [],
            "positive_owner_evidence_ids": [],
            "exculpatory_evidence_ids": [],
            "open_handoffs": ["client-to-server API boundary"],
            "unresolved": "owner open",
            "storage_identity_correlation": storage_na,
        },
        "transient": {
            "status": "false",
            "same_run_evidence_ids": [],
            "cross_run_evidence_ids": [],
            "non_transient_boundary": "no recovery",
            "unresolved_reason": None,
        },
        "reusable_project_facts": ["the client calls the API server"],
        "recurrence_signatures": ["this exact request returned 404"],
        "prompt_candidates": [],
        "recipe_candidate": None,
        "unresolved": [],
    }
    holdout = {
        "id": "holdout-a",
        "split": "final_holdout",
        "pre_freeze_holdout_kind": "recurrence",
        "holdout_event_scope": "build_level_unresolved",
        "embargoed": True,
        "causal_event_id": "event-h",
        "fresh_session_id": None,
        "job_name": "periodic-other",
        "build_id": "456",
        "test_name": "Prow job execution",
        "source_revision": None,
        "source_revision_status": "unavailable",
        "source_revision_provenance": "embargoed until holdout evaluation",
    }
    common = {
        "schema_version": 2,
        "engine_commit": git,
        "consumer": consumer,
        "freeze_manifest": freeze,
        "evidence_planes": planes,
        "classifications": [classification],
        "validation_commands": commands,
        "blind_access_control": blind,
        "validation_file_manifests": file_manifests,
        "recipe_exemplar_policy": exemplar,
        "evaluation_snapshot": {
            "mode": "committed_engine",
            "skill_manifest_path": None,
            "skill_manifest_sha256": None,
            "companion_files": [],
            "limitations": [],
        },
    }
    corpus = {
        **copy.deepcopy(common),
        "document_type": "failure_corpus",
        "selection": {"targets": {"cases": 6}, "achieved": {"cases": 2}, "limitations": ["small corpus"]},
        "cases": [authoring, holdout],
    }
    corpus_hash = hashlib.sha256((json.dumps(corpus, indent=2, sort_keys=True) + "\n").encode()).hexdigest()
    trial = {
        "id": "fresh-holdout-a",
        "case_id": "holdout-a",
        "prompt_sha256": sha("7"),
        "session_id": "fresh-session-a",
        "review_mode": "fresh_session",
        "status": "passed",
        "diagnosis_path": "fresh/holdout-a.json",
        "diagnosis_sha256": sha("8"),
        "classification": "experimental",
        "locked_score": {"hits": 4, "total": 4},
        "semantic_score": {"earned": 4, "max": 4, "rubric_version": 1},
        "pre_freeze_holdout_kind": "recurrence",
        "post_reveal_causal_kind": "generalization",
        "post_reveal_event_kinds": ["generalization"],
        "reclassified_after_reveal": True,
    }
    def condition(name: str) -> dict[str, Any]:
        return {
            "id": f"condition-{name.lower()}-holdout-a",
            "condition": name,
            "case_id": "holdout-a",
            "consumer_commit": consumer["commit"],
            "project_sha256": consumer["project_sha256"],
            "prompt_sha256": sha("7"),
            "active_skill_set_hash": consumer["active_skill_set_hash"],
            "identity_manifest_path": f"conditions/{name.lower()}-holdout-a.identity.json",
            "identity_manifest_sha256": sha(name.lower()[0]),
            "scoring_overlay_status": "available",
            "scoring_overlay_path": "overlays/holdout-a.scoring.json",
            "scoring_overlay_sha256": sha("9"),
            "command": f"run condition {name} for holdout-a",
            "status": "not_run",
        }
    benchmark = {
        **copy.deepcopy(common),
        "document_type": "benchmark_results",
        "corpus": {
            "path": "reports/failure-corpus.json",
            "sha256": corpus_hash,
            "authoring_case_ids": ["case-a"],
            "validation_case_ids": [],
            "final_holdout_case_ids": ["holdout-a"],
            "coverage_limitations": ["small corpus"],
        },
        "prompt_versions": [{
            "id": "baseline",
            "sha256": sha("7"),
            "source_split": "baseline",
            "parent_id": None,
            "changes": [],
        }],
        "authoring_validation": [],
        "fresh_holdout_trials": [trial],
        "dashboard_trials": [],
        "condition_manifests": [condition("A"), condition("B"), condition("C")],
        "proposals": [],
        "controls": [],
        "provider": {"available": False, "api_mode": "", "model_label": "", "limitations": ["unavailable"]},
        "generic_engine_gaps": [],
        "prompt_regression": {
            "baseline_state": "scaffold",
            "baseline_prompt_sha256": consumer["existing_prompt_sha256"],
            "proposed_prompt_sha256": sha("7"),
            "items": [],
            "limitations": ["baseline prompt is a scaffold"],
            "baseline_provenance": [],
            "provenance_limitations": ["scaffold has no historical authoring provenance"],
        },
        "scoring_protocol": {
            "mode": "independent_pre_prompt",
            "overlay_frozen_before_prompt_access": True,
            "scoring_author_id": "scoring-author",
            "prompt_scorer_id": "prompt-scorer",
            "limitations": [],
        },
    }
    return corpus, benchmark


def write_fixture_files(root: Path, corpus: dict[str, Any], benchmark: dict[str, Any]) -> tuple[Path, Path, dict[str, Any]]:
    (root / "reports/validation").mkdir(parents=True)
    logs = {"json.log": "validation output\n", "clean.log": "clean\n"}
    for name, content in logs.items():
        (root / "reports/validation" / name).write_text(content)
    for doc in (corpus, benchmark):
        for command in doc["validation_commands"]:
            command["output_sha256"] = hashlib.sha256((root / command["output_path"]).read_bytes()).hexdigest()

    file_manifest = {
        "schema_version": 1,
        "document_type": "validation_file_manifest",
        "root_commit": corpus["engine_commit"],
        "entries": [{"path": "SKILL.md", "mode": "100644", "git_blob_id": "c" * 40, "sha256": "d" * 64}],
    }
    for name in ("validation-files-baseline.json", "validation-files-final.json"):
        (root / "reports/validation" / name).write_text(json.dumps(file_manifest, indent=2, sort_keys=True) + "\n")
    for doc in (corpus, benchmark):
        binding = doc["validation_file_manifests"]
        binding["baseline_sha256"] = hashlib.sha256((root / binding["baseline_path"]).read_bytes()).hexdigest()
        binding["final_sha256"] = hashlib.sha256((root / binding["final_path"]).read_bytes()).hexdigest()

    (root / "reference").mkdir()
    supporting = {
        "diagnosis.json": "reference diagnosis\n",
        "scoring.json": "scoring rules\n",
        "forbidden.json": "forbidden rules\n",
    }
    for name, content in supporting.items():
        (root / "reference" / name).write_text(content)
    overlay = {
        "schema_version": 1,
        "document_type": "benchmark_scoring_overlay",
        "case_id": "holdout-a",
        "reference_diagnosis_path": "reference/diagnosis.json",
        "reference_diagnosis_sha256": hashlib.sha256((root / "reference/diagnosis.json").read_bytes()).hexdigest(),
        "scoring_path": "reference/scoring.json",
        "scoring_sha256": hashlib.sha256((root / "reference/scoring.json").read_bytes()).hexdigest(),
        "forbidden_path": "reference/forbidden.json",
        "forbidden_sha256": hashlib.sha256((root / "reference/forbidden.json").read_bytes()).hexdigest(),
    }
    (root / "overlays").mkdir()
    overlay_path = root / "overlays/holdout-a.scoring.json"
    overlay_path.write_text(json.dumps(overlay, indent=2, sort_keys=True) + "\n")
    overlay_hash = hashlib.sha256(overlay_path.read_bytes()).hexdigest()

    (root / "conditions").mkdir()
    for condition in benchmark["condition_manifests"]:
        identity = {
            "schema_version": 1,
            "document_type": "benchmark_identity_manifest",
            "identity_only": True,
            "case_id": "holdout-a",
            "job_name": "periodic-other",
            "build_id": "456",
            "test_name": "Prow job execution",
            "condition": condition["condition"],
            "consumer_commit": condition["consumer_commit"],
            "project_sha256": condition["project_sha256"],
            "prompt_sha256": condition["prompt_sha256"],
            "active_skill_set_hash": condition["active_skill_set_hash"],
        }
        target = root / condition["identity_manifest_path"]
        target.write_text(json.dumps(identity, indent=2, sort_keys=True) + "\n")
        condition["identity_manifest_sha256"] = hashlib.sha256(target.read_bytes()).hexdigest()
        condition["scoring_overlay_sha256"] = overlay_hash

    source = corpus["cases"][0]
    diagnosis = {
        "schema_version": 2,
        "document_type": "fresh_holdout_diagnosis",
        "case_id": "holdout-a",
        "prompt_sha256": benchmark["fresh_holdout_trials"][0]["prompt_sha256"],
        "session_id": benchmark["fresh_holdout_trials"][0]["session_id"],
        "pre_freeze_holdout_kind": "recurrence",
        "post_reveal_causal_kind": "generalization",
        "post_reveal_events": [{
            "event_id": "holdout-a-event-1",
            "causal_event_id": "event-h-revealed",
            "causal_kind": "generalization",
            "failure_class": "api_compatibility",
            "phase_reached": source["phase_reached"],
            "initiating_error": source["initiating_error"],
            "terminal_wrapper": source["terminal_wrapper"],
            "causal_chain": copy.deepcopy(source["causal_chain"]),
            "competing_hypotheses": copy.deepcopy(source["competing_hypotheses"]),
            "passing_comparison": copy.deepcopy(source["passing_comparison"]),
            "ownership": copy.deepcopy(source["ownership"]),
            "transient": copy.deepcopy(source["transient"]),
            "unresolved": [],
            "evidence_ids": ["E1"],
        }],
        "reclassified_after_reveal": True,
        "classification": "experimental",
        "initiating_error": source["initiating_error"],
        "terminal_wrapper": source["terminal_wrapper"],
        "causal_chain": copy.deepcopy(source["causal_chain"]),
        "evidence": copy.deepcopy(source["evidence"]),
        "competing_hypotheses": copy.deepcopy(source["competing_hypotheses"]),
        "passing_comparison": copy.deepcopy(source["passing_comparison"]),
        "ownership": copy.deepcopy(source["ownership"]),
        "transient": copy.deepcopy(source["transient"]),
        "unresolved": [],
    }
    (root / "fresh").mkdir()
    diagnosis_path = root / "fresh/holdout-a.json"
    diagnosis_path.write_text(json.dumps(diagnosis, indent=2, sort_keys=True) + "\n")
    benchmark["fresh_holdout_trials"][0]["diagnosis_sha256"] = hashlib.sha256(diagnosis_path.read_bytes()).hexdigest()

    corpus_path = root / "reports/failure-corpus.json"
    benchmark_path = root / "reports/benchmark-results.json"
    corpus_path.write_text(json.dumps(corpus, indent=2, sort_keys=True) + "\n")
    benchmark["corpus"]["sha256"] = hashlib.sha256(corpus_path.read_bytes()).hexdigest()
    benchmark_path.write_text(json.dumps(benchmark, indent=2, sort_keys=True) + "\n")
    return corpus_path, benchmark_path, diagnosis


def self_test(schema: dict[str, Any]) -> None:
    fixture_path = Path(__file__).resolve().parent.parent / "references/benchmark-manifest.schema-only.json"
    fixture = load(fixture_path)
    fixture_errors: list[str] = []
    check_schema(fixture, schema["$defs"]["benchmark_identity_manifest"], schema, "schema_only_fixture", fixture_errors)
    if fixture_errors:
        raise AssertionError(f"schema-only benchmark fixture failed: {fixture_errors}")

    corpus, benchmark = fixtures()
    if errors := validate(corpus, benchmark, schema):
        raise AssertionError(f"valid fixture failed: {errors}")

    nongit_corpus = copy.deepcopy(corpus)
    nongit_benchmark = copy.deepcopy(benchmark)
    for document in (nongit_corpus, nongit_benchmark):
        document["consumer"]["commit"] = None
        document["consumer"]["commit_status"] = "not_applicable"
    for condition in nongit_benchmark["condition_manifests"]:
        condition["consumer_commit"] = None
    if errors := validate(nongit_corpus, nongit_benchmark, schema):
        raise AssertionError(f"non-Git consumer fixture failed: {errors}")

    mutations = [
        ("holdout", lambda c, b: c["cases"][1].update(pre_freeze_holdout_kind="not_applicable"), "pre-freeze causal hypothesis"),
        ("reclassification", lambda c, b: b["fresh_holdout_trials"][0].update(reclassified_after_reveal=False), "aggregate post-reveal kind changed"),
        ("event-aggregate", lambda c, b: b["fresh_holdout_trials"][0].update(post_reveal_event_kinds=["recurrence", "generalization"]), "must aggregate event kinds to mixed"),
        ("event-scope", lambda c, b: c["cases"][1].update(holdout_event_scope="single_event_identity"), "single-event holdout requires a specific"),
        ("fresh-recommended", lambda c, b: b["fresh_holdout_trials"][0].update(classification="recommended"), "at most experimental"),
        ("access", lambda c, b: (c["blind_access_control"]["access_log"].append({"id": "leak", "phase": "pre_freeze", "category": "prior_diagnosis", "path": "locked/answer.json", "purpose": "authoring", "decision": "allowed", "access_method": "self_reported", "content_sha256": None, "bytes_read": None}), b.update(blind_access_control=copy.deepcopy(c["blind_access_control"]))), "must be blocked"),
        ("access-enforcement", lambda c, b: (c["blind_access_control"].update(enforcement="wrapper_enforced", wrapper_path="blind_access.py", wrapper_sha256="a" * 64), b.update(blind_access_control=copy.deepcopy(c["blind_access_control"]))), "cannot use self-reported"),
        ("snapshot", lambda c, b: (c["evaluation_snapshot"].update(mode="uncommitted_skill_with_companions", skill_manifest_path="skill.json", skill_manifest_sha256="a" * 64, companion_files=[]), b.update(evaluation_snapshot=copy.deepcopy(c["evaluation_snapshot"]))), "requires at least one companion"),
        ("revision", lambda c, b: c["cases"][1].update(source_revision="a" * 40), "unavailable revision must be null"),
        ("causal", lambda c, b: c["cases"][0]["causal_chain"][0].pop("actor"), "actor"),
        ("transient", lambda c, b: c["cases"][0]["transient"].update(status="maybe"), "must be one of"),
        ("storage", lambda c, b: (c["cases"][0]["ownership"]["storage_identity_correlation"].update(applicable=True, same_volume="matched", same_pvc="matched", same_pod="matched", same_node="missing", same_time_window="matched", limitation=None), c["cases"][0]["ownership"].update(open_handoffs=[])), "requires an open handoff"),
        ("ownership", lambda c, b: c["cases"][0]["ownership"].update(assigned_component="server", assignment_strength="probable"), "positive component-specific evidence"),
        ("facts", lambda c, b: c["cases"][0].update(recurrence_signatures=copy.deepcopy(c["cases"][0]["reusable_project_facts"])), "must be separate"),
        ("classification", lambda c, b: (c["classifications"][0].update(classification="experimental", evidence_plane="same_author_review"), b["classifications"][0].update(classification="experimental", evidence_plane="same_author_review")), "experimental behavior"),
        ("unknown", lambda c, b: c["cases"][0].update(unexpected=True), "not allowed"),
        ("prompt-regression", lambda c, b: b["prompt_regression"].update(baseline_state="existing_prompt", items=[]), "knowledge-retention inventory"),
        ("provenance-missing", lambda c, b: b["prompt_regression"].update(baseline_state="existing_prompt", items=[{"id": "retained", "category": "stable_fact", "baseline_rule": "read the build", "disposition": "retained", "evidence_case_ids": ["case-a"], "reason": "still valid"}], baseline_provenance=[], provenance_limitations=["unknown"]), "requires provenance before final holdout selection"),
        ("provenance-overlap", lambda c, b: b["prompt_regression"].update(baseline_state="existing_prompt", items=[{"id": "retained", "category": "stable_fact", "baseline_rule": "read the build", "disposition": "retained", "evidence_case_ids": ["case-a"], "reason": "still valid"}], baseline_provenance=[{"id": "prior-holdout", "job_name": "periodic-other", "build_id": "456", "test_name": "prior test", "causal_event_id": None, "source_split": "authoring", "source_path": None, "source_sha256": None}], provenance_limitations=[]), "overlaps baseline prompt provenance build"),
        ("stable-removal", lambda c, b: b["prompt_regression"].update(baseline_state="existing_prompt", items=[{"id": "stable", "category": "stable_fact", "baseline_rule": "read pod state", "disposition": "removed", "evidence_case_ids": [], "reason": "not seen"}]), "requires explicit case evidence"),
        ("scoring-protocol", lambda c, b: b["scoring_protocol"].update(mode="same_evaluator_post_hoc", overlay_frozen_before_prompt_access=False, limitations=["post hoc"]), "locked score requires"),
        ("consumer-commit", lambda c, b: (c["consumer"].update(commit=None), b["consumer"].update(commit=None)), "resolved consumer requires a commit"),
        ("condition-consumer", lambda c, b: b["condition_manifests"][0].update(consumer_commit="c" * 40), "must match consumer.commit"),
        ("conditions", lambda c, b: b["condition_manifests"].pop(), "exactly one A, B, and C"),
        ("overlay", lambda c, b: [item.update(scoring_overlay_status="not_revealed", scoring_overlay_path=None, scoring_overlay_sha256=None) for item in b["condition_manifests"]], "requires post-reveal scoring overlays"),
        ("exemplar", lambda c, b: (c["recipe_exemplar_policy"].update(existing_consumer_recipes_are_trusted_quality_exemplars=True), b["recipe_exemplar_policy"].update(existing_consumer_recipes_are_trusted_quality_exemplars=True)), "must equal False"),
    ]
    for name, mutate, expected in mutations:
        bad_c, bad_b = copy.deepcopy(corpus), copy.deepcopy(benchmark)
        mutate(bad_c, bad_b)
        if not any(expected in error for error in validate(bad_c, bad_b, schema)):
            raise AssertionError(f"{name} mutation was not rejected")

    with tempfile.TemporaryDirectory() as temp:
        root = Path(temp)
        file_corpus, file_benchmark = fixtures()
        corpus_path, benchmark_path, diagnosis = write_fixture_files(root, file_corpus, file_benchmark)
        if errors := file_bindings(corpus_path, benchmark_path, file_corpus, file_benchmark, schema, root):
            raise AssertionError(f"file binding fixture failed: {errors}")

        final_path = root / file_corpus["validation_file_manifests"]["final_path"]
        altered = load(final_path)
        altered["entries"][0]["sha256"] = "e" * 64
        final_path.write_text(json.dumps(altered, indent=2, sort_keys=True) + "\n")
        final_hash = hashlib.sha256(final_path.read_bytes()).hexdigest()
        for doc in (file_corpus, file_benchmark):
            doc["validation_file_manifests"]["final_sha256"] = final_hash
        if not any("must match exactly" in error for error in file_bindings(corpus_path, benchmark_path, file_corpus, file_benchmark, schema, root)):
            raise AssertionError("validation file manifest mismatch was not rejected")

        baseline_path = root / file_corpus["validation_file_manifests"]["baseline_path"]
        final_path.write_bytes(baseline_path.read_bytes())
        final_hash = hashlib.sha256(final_path.read_bytes()).hexdigest()
        for doc in (file_corpus, file_benchmark):
            doc["validation_file_manifests"]["final_sha256"] = final_hash

        diagnosis_path = root / file_benchmark["fresh_holdout_trials"][0]["diagnosis_path"]
        bad_diagnosis = copy.deepcopy(diagnosis)
        bad_diagnosis["causal_chain"][0].pop("actor")
        diagnosis_path.write_text(json.dumps(bad_diagnosis, indent=2, sort_keys=True) + "\n")
        file_benchmark["fresh_holdout_trials"][0]["diagnosis_sha256"] = hashlib.sha256(diagnosis_path.read_bytes()).hexdigest()
        if not any("actor" in error for error in file_bindings(corpus_path, benchmark_path, file_corpus, file_benchmark, schema, root)):
            raise AssertionError("fresh diagnosis schema mutation was not rejected")

        diagnosis_path.write_text(json.dumps(diagnosis, indent=2, sort_keys=True) + "\n")
        file_benchmark["fresh_holdout_trials"][0]["diagnosis_sha256"] = hashlib.sha256(diagnosis_path.read_bytes()).hexdigest()
        identity_path = root / file_benchmark["condition_manifests"][0]["identity_manifest_path"]
        identity = load(identity_path)
        identity["locked_score"] = 4
        identity_path.write_text(json.dumps(identity, indent=2, sort_keys=True) + "\n")
        file_benchmark["condition_manifests"][0]["identity_manifest_sha256"] = hashlib.sha256(identity_path.read_bytes()).hexdigest()
        if not any("locked_score" in error and "not allowed" in error for error in file_bindings(corpus_path, benchmark_path, file_corpus, file_benchmark, schema, root)):
            raise AssertionError("answer-bearing identity manifest mutation was not rejected")

        (root / "reports/validation/json.log").write_text("changed output\n")
        if not any("does not match file bytes" in error for error in file_bindings(corpus_path, benchmark_path, file_corpus, file_benchmark, schema, root)):
            raise AssertionError("validation log mutation was not rejected")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("corpus", nargs="?", type=Path)
    parser.add_argument("benchmark", nargs="?", type=Path)
    parser.add_argument("--schema", type=Path)
    parser.add_argument("--evidence-root", type=Path, help="private root for fresh-agent and dashboard result paths")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    schema_path = args.schema or Path(__file__).resolve().parent.parent / "references/report-schema.json"
    try:
        schema = load(schema_path)
        if args.self_test:
            self_test(schema)
            print("report validator self-test passed")
            return 0
        if not args.corpus or not args.benchmark:
            parser.error("corpus and benchmark paths are required")
        corpus, benchmark = load(args.corpus), load(args.benchmark)
        errors = validate(corpus, benchmark, schema)
        errors.extend(file_bindings(args.corpus, args.benchmark, corpus, benchmark, schema, args.evidence_root))
        if errors:
            print("report validation failed:", file=sys.stderr)
            for error in errors:
                print(f"- {error}", file=sys.stderr)
            return 1
        print("report validation passed")
        return 0
    except (OSError, ValueError, json.JSONDecodeError, AssertionError) as exc:
        print(f"report validation failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
