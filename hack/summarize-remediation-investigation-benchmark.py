#!/usr/bin/env python3
"""Summarize private remediation-investigation JSONL without reading model content."""

import argparse
import collections
import json
import math
import statistics
from pathlib import Path


def percentile(values, fraction):
    if not values:
        return None
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(fraction * len(ordered)) - 1))
    return ordered[index]


def distribution(values):
    if not values:
        return {"count": 0}
    return {
        "count": len(values),
        "min": min(values),
        "median": statistics.median(values),
        "p95": percentile(values, 0.95),
        "max": max(values),
        "total": sum(values),
    }


def repair_count(row):
    metrics = row.get("metrics", {})
    if "repair_count" in metrics:
        return int(metrics.get("repair_count", 0))
    if row.get("trial_status") == "invalid_result":
        return 1
    return 0


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("results", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()

    rows = []
    with args.results.open(encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, 1):
            if not line.strip():
                continue
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError as exc:
                raise SystemExit(f"invalid JSONL row {line_number}: {exc}") from exc

    statuses = collections.Counter(row.get("trial_status", "missing") for row in rows)
    confusion = collections.defaultdict(collections.Counter)
    valid = [row for row in rows if row.get("trial_status") == "valid_result"]
    for row in valid:
        confusion[row.get("expected_classification", "missing")][row.get("actual_classification", "missing")] += 1

    expected_positive = sum(bool(row.get("expected_actionable")) for row in valid)
    model_positive = sum(bool(row.get("actual_actionable")) for row in valid)
    verified_positive = sum(bool(row.get("verified_actionable")) for row in valid)
    model_true_positive = sum(bool(row.get("expected_actionable")) and bool(row.get("actual_actionable")) and bool(row.get("exact_target")) for row in valid)
    true_positive = sum(bool(row.get("expected_actionable")) and bool(row.get("verified_actionable")) and bool(row.get("exact_target")) for row in valid)
    precision = true_positive / verified_positive if verified_positive else None
    recall = true_positive / expected_positive if expected_positive else None
    model_precision = model_true_positive / model_positive if model_positive else None
    model_recall = model_true_positive / expected_positive if expected_positive else None

    total_requests = sum(int(row.get("metrics", {}).get("model_requests", 0)) for row in rows)
    total_reported = sum(int(row.get("metrics", {}).get("reported_requests", 0)) for row in rows)
    cost_eligible = [row for row in rows if int(row.get("metrics", {}).get("model_requests", 0)) > 0]
    fully_cost_covered = [row for row in cost_eligible if
        bool(row.get("metrics", {}).get("coverage_counts_known")) and
        not bool(row.get("metrics", {}).get("usage_invalid")) and
        int(row.get("metrics", {}).get("unreported_requests", 0)) == 0 and
        int(row.get("metrics", {}).get("reported_requests", 0)) == int(row.get("metrics", {}).get("model_requests", 0)) and
        bool(row.get("metrics", {}).get("pricing_hash")) and
        bool(row.get("metrics", {}).get("currency"))]

    summary = {
        "trials": len(rows),
        "trial_statuses": dict(sorted(statuses.items())),
        "structurally_valid": sum(bool(row.get("structurally_valid")) for row in rows),
        "classification_correct": sum(bool(row.get("classification_correct")) for row in valid),
        "classification_accuracy": (sum(bool(row.get("classification_correct")) for row in valid) / len(valid)) if valid else None,
        "classification_confusion": {expected: dict(sorted(actual.items())) for expected, actual in sorted(confusion.items())},
        "actionable_true_positives": true_positive,
        "actionable_precision": precision,
        "actionable_recall": recall,
        "model_actionable_true_positives": model_true_positive,
        "model_actionable_precision": model_precision,
        "model_actionable_recall": model_recall,
        "unverified_unsafe_proposals": sum(bool(row.get("unverified_unsafe_proposal")) for row in rows),
        "exact_target_accuracy": (sum(bool(row.get("exact_target")) for row in valid if row.get("expected_actionable")) / expected_positive) if expected_positive else None,
        "unsafe_false_acceptances": sum(bool(row.get("unsafe_false_acceptance")) for row in rows),
        "already_fixed_blocked": sum(bool(row.get("already_fixed_blocked")) for row in valid if row.get("expected_classification") == "already_fixed"),
        "verification_statuses": dict(sorted(collections.Counter(row.get("verification_status", "missing") for row in rows).items())),
        "requests": distribution([int(row.get("metrics", {}).get("model_requests", 0)) for row in rows]),
        "reported_requests": distribution([int(row.get("metrics", {}).get("reported_requests", 0)) for row in rows]),
        "usage_request_coverage": (total_reported / total_requests) if total_requests else None,
        "usage_invalid_trials": sum(bool(row.get("metrics", {}).get("usage_invalid")) for row in rows),
        "cost_coverage_eligible_trials": len(cost_eligible),
        "fully_cost_covered_trials": len(fully_cost_covered),
        "cost_coverage": (len(fully_cost_covered) / len(cost_eligible)) if cost_eligible else None,
        "input_tokens": distribution([int(row.get("metrics", {}).get("input_tokens", 0)) for row in rows]),
        "cached_input_tokens": distribution([int(row.get("metrics", {}).get("cached_input_tokens", 0)) for row in rows]),
        "output_tokens": distribution([int(row.get("metrics", {}).get("output_tokens", 0)) for row in rows]),
        "reasoning_tokens": distribution([int(row.get("metrics", {}).get("reasoning_tokens", 0)) for row in rows]),
        "estimated_cost_nanos": distribution([int(row.get("metrics", {}).get("estimated_cost_nanos", 0)) for row in rows]),
        "latency_ms": distribution([int(row.get("metrics", {}).get("elapsed_ms", 0)) for row in rows]),
        "repair_counts": distribution([repair_count(row) for row in rows]),
        "repair_count_inferred_trials": sum("repair_count" not in row.get("metrics", {}) and row.get("trial_status") == "invalid_result" for row in rows),
        "error_codes": dict(sorted(collections.Counter(row.get("error_code") for row in rows if row.get("error_code")).items())),
    }
    encoded = json.dumps(summary, indent=2, sort_keys=True) + "\n"
    if args.output:
        args.output.write_text(encoded, encoding="utf-8")
    else:
        print(encoded, end="")


if __name__ == "__main__":
    main()
