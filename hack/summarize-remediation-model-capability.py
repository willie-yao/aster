#!/usr/bin/env python3
"""Summarize private remediation model-capability rows without model content."""

import argparse
import collections
import json
import math
import statistics
from pathlib import Path

EXPECTED_MODELS = {"gpt-5.4", "gpt-5.6-sol"}
EXPECTED_CASES = {
    "pre_fix": "capg-kubernetes-version-pre-fix",
    "post_fix": "capg-kubernetes-version-post-fix",
}
EXPECTED_REPETITIONS = {1, 2, 3}
EXPECTED_API_MODE = "responses"
EXPECTED_PROVIDER = "github_copilot"
EXPECTED_TRANSPORT_FINGERPRINT = "f040744e3082f5cb72da45d764f9975e81569acc43db4e2d18012a3b878214ca"


def distribution(values):
    if not values:
        return {"count": 0}
    values = sorted(values)
    return {
        "count": len(values),
        "min": values[0],
        "median": statistics.median(values),
        "p95": values[max(0, min(len(values) - 1, math.ceil(0.95 * len(values)) - 1))],
        "max": values[-1],
        "total": sum(values),
    }


def read_rows(paths):
    rows = []
    for path in paths:
        with path.open(encoding="utf-8") as handle:
            for line_number, line in enumerate(handle, 1):
                if not line.strip():
                    continue
                try:
                    row = json.loads(line)
                except json.JSONDecodeError as exc:
                    raise SystemExit(f"{path}: invalid JSONL row {line_number}: {exc}") from exc
                row["_source"] = f"{path}:{line_number}"
                rows.append(row)
    return rows


def require_one(rows, field, scope):
    values = {row.get(field) for row in rows}
    if len(values) != 1 or None in values or "" in values:
        raise SystemExit(f"{scope}: inconsistent or missing {field}: {sorted(repr(value) for value in values)}")
    return next(iter(values))


def validate_rows(rows, expected_models):
    expected_trials = 6 * len(expected_models)
    if len(rows) != expected_trials:
        raise SystemExit(f"expected exactly {expected_trials} trials, found {len(rows)}")
    models = {row.get("model") for row in rows}
    if models != expected_models:
        raise SystemExit(f"models must be exactly {sorted(expected_models)}, found {sorted(repr(model) for model in models)}")

    require_one(rows, "engine_commit", "all trials")
    require_one(rows, "manifest_sha256", "all trials")
    if require_one(rows, "api_mode", "all trials") != EXPECTED_API_MODE:
        raise SystemExit(f"api_mode must be {EXPECTED_API_MODE}")
    if require_one(rows, "provider_identity", "all trials") != EXPECTED_PROVIDER:
        raise SystemExit(f"provider_identity must be {EXPECTED_PROVIDER}")
    if require_one(rows, "transport_fingerprint", "all trials") != EXPECTED_TRANSPORT_FINGERPRINT:
        raise SystemExit("transport fingerprint does not match the frozen Copilot Responses transport")

    seen = set()
    effective_inputs = collections.defaultdict(set)
    for row in rows:
        model = row.get("model")
        state = row.get("temporal_state")
        repetition = row.get("repetition")
        key = (model, row.get("case_id"), repetition)
        if key in seen:
            raise SystemExit(f"duplicate trial {key} at {row['_source']}")
        seen.add(key)
        if state not in EXPECTED_CASES or row.get("case_id") != EXPECTED_CASES[state]:
            raise SystemExit(f"{row['_source']}: invalid case/state identity")
        if repetition not in EXPECTED_REPETITIONS:
            raise SystemExit(f"{row['_source']}: repetition must be one of {sorted(EXPECTED_REPETITIONS)}")
        effective_hash = row.get("effective_input_sha256")
        if not effective_hash:
            raise SystemExit(f"{row['_source']}: missing effective_input_sha256")
        for field in (
            "memo_mentions_target_job",
            "memo_mentions_target_container",
            "memo_mentions_target_name",
            "memo_mentions_target_value",
            "final_result_contains_candidate",
            "target_hypotheses_emitted",
            "exact_hypothesis_identity_present",
        ):
            if not isinstance(row.get(field), bool):
                raise SystemExit(f"{row['_source']}: missing boolean diagnostic {field}")
        if not isinstance(row.get("deterministic_verification_outcome"), str) or not row.get("deterministic_verification_outcome"):
            raise SystemExit(f"{row['_source']}: missing deterministic_verification_outcome")
        effective_inputs[state].add(effective_hash)

    for state, values in effective_inputs.items():
        if len(values) != 1:
            raise SystemExit(f"{state}: inconsistent effective input hashes")
    if set(effective_inputs) != set(EXPECTED_CASES):
        raise SystemExit("both temporal states are required")

    for model in expected_models:
        model_rows = [row for row in rows if row.get("model") == model]
        if len(model_rows) != 6:
            raise SystemExit(f"{model}: expected exactly six rows, found {len(model_rows)}")
        require_one(model_rows, "provider_fingerprint", model)
        for state in EXPECTED_CASES:
            repetitions = {
                row.get("repetition")
                for row in model_rows
                if row.get("temporal_state") == state
            }
            if repetitions != EXPECTED_REPETITIONS:
                raise SystemExit(f"{model} {state}: repetitions must be exactly 1, 2, 3")


def summarize_model(rows):
    valid = [
        row
        for row in rows
        if row.get("trial_status") == "valid_result" and row.get("structurally_valid") is True
    ]
    pre = [row for row in rows if row.get("temporal_state") == "pre_fix"]
    post = [row for row in rows if row.get("temporal_state") == "post_fix"]
    accepted = [row for row in rows if row.get("verified_actionable") is True]
    correct_accepted = [
        row
        for row in accepted
        if row.get("temporal_state") == "pre_fix"
        and row.get("actual_classification") == "actionable"
        and row.get("exact_identity") is True
    ]
    unsafe = [row for row in accepted if row not in correct_accepted]
    pre_exact = sum(
        row.get("verified_actionable") is True
        and row.get("actual_classification") == "actionable"
        and row.get("exact_identity") is True
        for row in pre
    )
    post_exact = sum(
        row.get("verified_actionable") is not True
        and row.get("actual_classification") == "already_fixed"
        and row.get("exact_identity") is True
        for row in post
    )
    precision = len(correct_accepted) / len(accepted) if accepted else None
    evidence_kinds = collections.Counter()
    for row in valid:
        for evidence_id in row.get("selected_evidence_ids", []):
            evidence_kinds[evidence_id.split(":", 1)[0]] += 1
    summary = {
        "trials": len(rows),
        "trial_statuses": dict(sorted(collections.Counter(row.get("trial_status", "missing") for row in rows).items())),
        "structurally_valid": len(valid),
        "pre_fix_exact_actionable": pre_exact,
        "post_fix_exact_already_fixed": post_exact,
        "verified_actionable": len(accepted),
        "actionable_precision": precision,
        "unsafe_acceptances": len(unsafe),
        "unsafe_trials": [
            {"case_id": row.get("case_id"), "repetition": row.get("repetition")}
            for row in unsafe
        ],
        "candidate_kinds": dict(sorted(collections.Counter(row.get("candidate_kind", "none") or "none" for row in valid).items())),
        "actual_classifications": dict(sorted(collections.Counter(row.get("actual_classification", "missing") or "missing" for row in valid).items())),
        "source_read_trials": sum(
            int(row.get("evidence", {}).get("source_reads", 0)) > 0
            and int(row.get("evidence", {}).get("source_read_bytes", 0)) > 0
            for row in rows
        ),
        "memo_mentions_target_job": sum(row["memo_mentions_target_job"] for row in rows),
        "memo_mentions_target_container": sum(row["memo_mentions_target_container"] for row in rows),
        "memo_mentions_target_name": sum(row["memo_mentions_target_name"] for row in rows),
        "memo_mentions_target_value": sum(row["memo_mentions_target_value"] for row in rows),
        "memo_contains_complete_target_identity": sum(
            row["memo_mentions_target_job"]
            and row["memo_mentions_target_container"]
            and row["memo_mentions_target_name"]
            and row["memo_mentions_target_value"]
            for row in rows
        ),
        "final_result_contains_candidate": sum(row["final_result_contains_candidate"] for row in rows),
        "target_hypotheses_emitted": sum(row["target_hypotheses_emitted"] for row in rows),
        "exact_hypothesis_identity_present": sum(row["exact_hypothesis_identity_present"] for row in rows),
        "deterministic_verification_outcomes": dict(sorted(collections.Counter(row["deterministic_verification_outcome"] for row in rows).items())),
        "diagnostics_by_state": {
            state: {
                "trials": len([row for row in rows if row.get("temporal_state") == state]),
                "memo_contains_complete_target_identity": sum(
                    row["memo_mentions_target_job"]
                    and row["memo_mentions_target_container"]
                    and row["memo_mentions_target_name"]
                    and row["memo_mentions_target_value"]
                    for row in rows
                    if row.get("temporal_state") == state
                ),
                "final_result_contains_candidate": sum(
                    row["final_result_contains_candidate"]
                    for row in rows
                    if row.get("temporal_state") == state
                ),
            }
            for state in EXPECTED_CASES
        },
        "selected_evidence_kinds": dict(sorted(evidence_kinds.items())),
        "requests": distribution([int(row.get("metrics", {}).get("model_requests", 0)) for row in rows]),
        "input_tokens": distribution([int(row.get("metrics", {}).get("input_tokens", 0)) for row in rows]),
        "cached_input_tokens": distribution([int(row.get("metrics", {}).get("cached_input_tokens", 0)) for row in rows]),
        "output_tokens": distribution([int(row.get("metrics", {}).get("output_tokens", 0)) for row in rows]),
        "reasoning_tokens": distribution([int(row.get("metrics", {}).get("reasoning_tokens", 0)) for row in rows]),
        "latency_ms": distribution([int(row.get("metrics", {}).get("elapsed_ms", 0)) for row in rows]),
        "repair_counts": distribution([int(row.get("metrics", {}).get("repair_count", 0)) for row in rows]),
        "cost_available_trials": sum(bool(row.get("cost_available")) for row in rows),
        "error_codes": dict(sorted(collections.Counter(row.get("error_code") for row in rows if row.get("error_code")).items())),
    }
    summary["gate_passed"] = (
        len(valid) >= 5
        and pre_exact >= 2
        and post_exact >= 2
        and precision == 1.0
        and not unsafe
    )
    return summary


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("results", nargs="+", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--model", choices=sorted(EXPECTED_MODELS))
    args = parser.parse_args()
    rows = read_rows(args.results)
    expected_models = {args.model} if args.model else EXPECTED_MODELS
    validate_rows(rows, expected_models)
    by_model = collections.defaultdict(list)
    for row in rows:
        by_model[row["model"]].append(row)
    output = {
        "complete": True,
        "models": {model: summarize_model(by_model[model]) for model in sorted(expected_models)},
    }
    encoded = json.dumps(output, indent=2, sort_keys=True) + "\n"
    if args.output:
        args.output.write_text(encoded, encoding="utf-8")
    else:
        print(encoded, end="")


if __name__ == "__main__":
    main()
