#!/usr/bin/env python3
"""Compare private in-process and Orka OpenCode benchmark JSONL records."""

import argparse
import json
import pathlib
import re
import sys


def nonempty_string(value):
    return isinstance(value, str) and bool(value.strip())


def no_whitespace_string(value):
    return isinstance(value, str) and bool(value) and not re.search(r"\s", value)


def exact_lower_hex(length):
    pattern = re.compile(rf"[0-9a-f]{{{length}}}\Z")
    return lambda value: isinstance(value, str) and bool(pattern.fullmatch(value))


def integer(value):
    return type(value) is int


def nonnegative_integer(value):
    return integer(value) and value >= 0


def positive_integer(value):
    return integer(value) and value > 0


def string_list(value):
    return isinstance(value, list) and all(nonempty_string(item) for item in value)


LOWER_HEX_20 = exact_lower_hex(20)
LOWER_HEX_40 = exact_lower_hex(40)
LOWER_HEX_64 = exact_lower_hex(64)

COMMON_SCHEMA = {
    "case_id": (nonempty_string, "a non-empty string"),
    "stable_id": (LOWER_HEX_20, "20 lowercase hexadecimal characters"),
    "repetition": (positive_integer, "a positive integer"),
    "arm": (nonempty_string, "a non-empty string"),
    "engine_commit": (LOWER_HEX_40, "40 lowercase hexadecimal characters"),
    "fixture_sha256": (LOWER_HEX_64, "64 lowercase hexadecimal characters"),
    "baseline_consumer_commit": (LOWER_HEX_40, "40 lowercase hexadecimal characters"),
    "baseline_prompt_sha256": (LOWER_HEX_64, "64 lowercase hexadecimal characters"),
    "project_sha256": (LOWER_HEX_64, "64 lowercase hexadecimal characters"),
    "skill_set_hash": (LOWER_HEX_64, "64 lowercase hexadecimal characters"),
    "provider_path": (no_whitespace_string, "a non-empty string without whitespace"),
    "transport_id": (no_whitespace_string, "a non-empty string without whitespace"),
    "api_mode": (nonempty_string, "a non-empty string"),
    "model_label": (nonempty_string, "a non-empty string"),
    "source_revision": (LOWER_HEX_40, "40 lowercase hexadecimal characters"),
    "signal_hits": (nonnegative_integer, "a non-negative integer"),
    "signal_total": (positive_integer, "a positive integer"),
    "elapsed_ms": (nonnegative_integer, "a non-negative integer"),
    "human_score_rubric_version": (positive_integer, "a positive integer"),
    "human_score_max": (positive_integer, "a positive integer"),
    "human_score_dimensions": (
        lambda value: string_list(value) and bool(value),
        "a non-empty list of non-empty strings",
    ),
}

INPROCESS_SCHEMA = {
    **COMMON_SCHEMA,
    "outcome": (nonempty_string, "a non-empty string"),
    "usable": (lambda value: type(value) is bool, "a boolean"),
    "trace": (lambda value: isinstance(value, dict), "an object"),
}

SHADOW_SCHEMA = {
    **COMMON_SCHEMA,
    "version": (integer, "an integer"),
    "runtime": (nonempty_string, "a non-empty string"),
    "status": (nonempty_string, "a non-empty string"),
    "attempts": (nonnegative_integer, "a non-negative integer"),
    "artifact_citation_count": (nonnegative_integer, "a non-negative integer"),
    "source_citation_count": (nonnegative_integer, "a non-negative integer"),
    "source_verified": (lambda value: type(value) is bool, "a boolean"),
    "unresolved_details": (string_list, "a list of non-empty strings"),
    "contract_version": (nonempty_string, "a non-empty string"),
    "tool_policy_version": (nonempty_string, "a non-empty string"),
    "agent_namespace": (nonempty_string, "a non-empty string"),
    "agent_ref": (nonempty_string, "a non-empty string"),
    "agent_version": (nonempty_string, "a non-empty string"),
    "agent_config_sha256": (LOWER_HEX_64, "64 lowercase hexadecimal characters"),
    "orka_commit": (LOWER_HEX_40, "40 lowercase hexadecimal characters"),
    "agent_skill_hash": (LOWER_HEX_64, "64 lowercase hexadecimal characters"),
    "evidence_hash": (LOWER_HEX_64, "64 lowercase hexadecimal characters"),
    "runtime_identity_hash": (LOWER_HEX_64, "64 lowercase hexadecimal characters"),
    "execution_id": (lambda value: isinstance(value, str) and bool(re.fullmatch(r"agent-analysis-[0-9a-f]{16}", value)), "agent-analysis- followed by 16 lowercase hexadecimal characters"),
    "max_turns": (positive_integer, "a positive integer"),
    "timeout": (nonempty_string, "a non-empty string"),
    "retries": (nonnegative_integer, "a non-negative integer"),
    "token_usage_available": (lambda value: type(value) is bool, "a boolean"),
    "cost_status": (nonempty_string, "a non-empty string"),
}

PAIR_FIELDS = (
    "arm",
    "engine_commit",
    "fixture_sha256",
    "baseline_consumer_commit",
    "baseline_prompt_sha256",
    "project_sha256",
    "skill_set_hash",
    "provider_path",
    "transport_id",
    "api_mode",
    "model_label",
    "stable_id",
    "source_revision",
    "human_score_rubric_version",
    "human_score_max",
    "human_score_dimensions",
    "signal_total",
)

INPROCESS_OUTCOMES = {
    "usable": True,
    "grounded_policy_unavailable": False,
}

SHADOW_ERROR_CODES = {
    "succeeded": None,
    "cleanup_pending": "cleanup_pending",
    "invalid_result": "invalid_result",
    "cancelled": "cancelled",
    "runtime_failed": "runtime",
}


def invalid(path, line_no, message):
    raise SystemExit(f"{path}:{line_no}: {message}")


def validate_common_contract(path, line_no, record, kind):
    if record["arm"] != "baseline":
        invalid(path, line_no, f"{kind} arm must be baseline")
    if record["api_mode"] != "chat_completions":
        invalid(path, line_no, f"{kind} api_mode must be chat_completions")
    if record["signal_hits"] > record["signal_total"]:
        invalid(path, line_no, f"{kind} signal_hits exceeds signal_total")


def validate_inprocess(path, line_no, record):
    outcome = record["outcome"]
    if outcome not in INPROCESS_OUTCOMES:
        allowed = ", ".join(sorted(INPROCESS_OUTCOMES))
        invalid(path, line_no, f"in-process outcome must be one of: {allowed}")
    expected_usable = INPROCESS_OUTCOMES[outcome]
    if record["usable"] is not expected_usable:
        invalid(path, line_no, f"in-process usable must be {str(expected_usable).lower()} for outcome {outcome}")

    trace = record["trace"]
    for field in ("input_tokens", "output_tokens"):
        if field not in trace:
            invalid(path, line_no, f"in-process trace missing required field {field}")
        if not nonnegative_integer(trace[field]):
            invalid(path, line_no, f"in-process trace field {field} must be a non-negative integer")


def validate_shadow(path, line_no, record):
    if record["version"] != 1:
        invalid(path, line_no, "shadow record version must be 1")
    if record["runtime"] != "orka-opencode-shadow":
        invalid(path, line_no, "shadow runtime must be orka-opencode-shadow")
    if record["contract_version"] != "agent-analysis-v1":
        invalid(path, line_no, "shadow contract_version must be agent-analysis-v1")
    if record["tool_policy_version"] != "agent-analysis-tools-v2":
        invalid(path, line_no, "shadow tool_policy_version must be agent-analysis-tools-v2")
    if record["token_usage_available"]:
        invalid(path, line_no, "shadow token_usage_available must be false")
    if record["cost_status"] != "external_runtime_usage_unavailable":
        invalid(path, line_no, "shadow cost_status must be external_runtime_usage_unavailable")

    status = record["status"]
    if status not in SHADOW_ERROR_CODES:
        allowed = ", ".join(sorted(SHADOW_ERROR_CODES))
        invalid(path, line_no, f"shadow status must be one of: {allowed}")
    expected_error = SHADOW_ERROR_CODES[status]
    if expected_error is None:
        if "error_code" in record:
            invalid(path, line_no, "shadow status succeeded requires error_code to be absent")
    elif record.get("error_code") != expected_error:
        invalid(path, line_no, f"shadow status {status} requires error_code {expected_error}")

    if status in ("succeeded", "cleanup_pending"):
        if record["attempts"] < 1:
            invalid(path, line_no, f"shadow status {status} requires attempts of at least 1")
        if record["artifact_citation_count"] < 1:
            invalid(path, line_no, f"shadow status {status} requires at least 1 artifact citation")

    expected_source_verified = record["source_citation_count"] > 0
    if record["source_verified"] is not expected_source_verified:
        invalid(
            path,
            line_no,
            "shadow source_verified must equal whether source_citation_count is greater than 0",
        )


def validate_schema(path, line_no, record, kind):
    schema = INPROCESS_SCHEMA if kind == "in-process" else SHADOW_SCHEMA
    for field, (validator, description) in schema.items():
        if field not in record:
            invalid(path, line_no, f"{kind} record missing required field {field}")
        if not validator(record[field]):
            invalid(path, line_no, f"{kind} field {field} must be {description}")

    validate_common_contract(path, line_no, record, kind)
    if kind == "in-process":
        validate_inprocess(path, line_no, record)
    else:
        validate_shadow(path, line_no, record)


def load(path, kind):
    records = {}
    record_lines = {}
    with pathlib.Path(path).open(encoding="utf-8") as stream:
        for line_no, line in enumerate(stream, 1):
            line = line.strip()
            if not line:
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError as exc:
                invalid(path, line_no, f"invalid JSON: {exc}")
            if not isinstance(record, dict):
                invalid(path, line_no, f"{kind} record must be an object")
            validate_schema(path, line_no, record, kind)
            key = (record["case_id"], record["repetition"])
            if key in records:
                case_id, repetition = key
                invalid(
                    path,
                    line_no,
                    f"duplicate benchmark record {case_id}/rep-{repetition:02d}; first seen on line {record_lines[key]}",
                )
            records[key] = record
            record_lines[key] = line_no
    if not records:
        raise SystemExit(f"{path}: empty {kind} JSONL input")
    return records


def value(record, *path, default=0):
    current = record
    for key in path:
        if not isinstance(current, dict) or key not in current:
            return default
        current = current[key]
    return current


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--inprocess", required=True)
    parser.add_argument("--shadow", required=True)
    parser.add_argument("--output")
    args = parser.parse_args()

    inprocess = load(args.inprocess, "in-process")
    shadow = load(args.shadow, "shadow")
    keys = sorted(set(inprocess) | set(shadow))
    missing = [key for key in keys if key not in inprocess or key not in shadow]
    if missing:
        raise SystemExit(
            "unpaired benchmark records: "
            + ", ".join(f"{case}/rep-{rep:02d}" for case, rep in missing)
        )

    lines = [
        "# Agent analysis shadow comparison",
        "",
        "| Case | Rep | Model | In-process | Signals | Latency ms | Tokens in/out | Shadow | Signals | Latency ms | Attempts | Artifact/source citations | Source verified | Unresolved | Cost |",
        "| --- | ---: | --- | --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | ---: | --- | ---: | --- |",
    ]
    for key in keys:
        direct = inprocess[key]
        agent = shadow[key]
        for field in PAIR_FIELDS:
            if direct[field] != agent[field]:
                raise SystemExit(f"{field} mismatch for {key[0]}/rep-{key[1]:02d}")
        direct_tokens = f"{value(direct, 'trace', 'input_tokens')}/{value(direct, 'trace', 'output_tokens')}"
        citations = f"{agent['artifact_citation_count']}/{agent['source_citation_count']}"
        lines.append(
            "| {case} | {rep} | {model} | {direct_status} | {direct_hits}/{direct_total} | {direct_ms} | {tokens} | "
            "{shadow_status} | {shadow_hits}/{shadow_total} | {shadow_ms} | {attempts} | {citations} | {verified} | {unresolved} | {cost} |".format(
                case=key[0],
                rep=key[1],
                model=direct["model_label"],
                direct_status=direct["outcome"],
                direct_hits=direct["signal_hits"],
                direct_total=direct["signal_total"],
                direct_ms=direct["elapsed_ms"],
                tokens=direct_tokens,
                shadow_status=agent["status"],
                shadow_hits=agent["signal_hits"],
                shadow_total=agent["signal_total"],
                shadow_ms=agent["elapsed_ms"],
                attempts=agent["attempts"],
                citations=citations,
                verified="yes" if agent["source_verified"] else "no",
                unresolved=len(agent["unresolved_details"]),
                cost=agent["cost_status"],
            )
        )
    lines.extend(
        [
            "",
            "Human review uses the shared five-dimension rubric: diagnosis, artifact evidence, claim discipline, remediation, and source grounding.",
            "Orka OpenCode cost remains unavailable until its result contract reports provider usage; do not infer cost from bytes or latency.",
        ]
    )
    output = "\n".join(lines) + "\n"
    if args.output:
        path = pathlib.Path(args.output)
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        path.write_text(output, encoding="utf-8")
        path.chmod(0o600)
    else:
        sys.stdout.write(output)


if __name__ == "__main__":
    main()
