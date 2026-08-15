#!/usr/bin/env python3
"""Freeze blinded Agent Sandbox scores before runtime unblinding."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import sys


def canonical_hash(value: object) -> str:
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--blind-packets", required=True)
    parser.add_argument("--blind-scores", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    try:
        packets = json.loads(Path(args.blind_packets).read_text())
        scores = json.loads(Path(args.blind_scores).read_text())
        if packets.get("version") != 2 or scores.get("version") != 2:
            raise ValueError("packet and score versions must be 2")
        packet_rows = packets.get("packets")
        if not isinstance(packet_rows, list) or not packet_rows:
            raise ValueError("packet document is empty")
        packet_hash = canonical_hash({"version": 2, "packets": packet_rows})
        if packets.get("packet_set_sha256") != packet_hash:
            raise ValueError("packet_set_sha256 does not match the actual blinded packets")
        references = {}
        packet_keys_list = []
        for item in packet_rows:
            if not isinstance(item, dict) or item.get("arm") not in ("A", "B") or not isinstance(item.get("packet_id"), str) or not isinstance(item.get("case_id"), str) or not isinstance(item.get("causal_reference"), dict):
                raise ValueError("blinded packet identity is invalid")
            packet_keys_list.append((item["packet_id"], item["arm"]))
            existing = references.setdefault(item["case_id"], item["causal_reference"])
            if existing != item["causal_reference"]:
                raise ValueError("causal reference differs across arms")
        packet_keys = set(packet_keys_list)
        if len(packet_keys) != len(packet_keys_list) or any({arm for packet_id, arm in packet_keys if packet_id == current} != {"A", "B"} for current in {packet_id for packet_id, _ in packet_keys}):
            raise ValueError("blinded packets must contain exactly one A and B arm")
        reference_hash = canonical_hash({"version": 1, "cases": {case_id: references[case_id] for case_id in sorted(references)}})
        if packets.get("reference_set_sha256") != reference_hash:
            raise ValueError("reference_set_sha256 does not match the actual causal references")
        if scores.get("packet_set_sha256") != packet_hash or scores.get("reference_set_sha256") != reference_hash:
            raise ValueError("scores are not bound to the packet and reference sets")
        dimensions = ["diagnosis", "artifact_evidence", "claim_discipline", "remediation", "source_grounding"]
        if scores.get("rubric_version") != 2 or scores.get("score_max") != 10 or scores.get("dimensions") != dimensions:
            raise ValueError("score rubric identity is invalid")
        score_rows = scores.get("scores")
        if not isinstance(score_rows, list) or not all(isinstance(item, dict) for item in score_rows):
            raise ValueError("scores must contain only score objects")
        score_keys_list = [(item.get("packet_id"), item.get("arm")) for item in score_rows]
        score_keys = set(score_keys_list)
        if len(score_keys) != len(score_keys_list) or packet_keys != score_keys:
            raise ValueError("scores do not cover the exact blinded packet arms")
        reference_by_key = {(item["packet_id"], item["arm"]): item["causal_reference"] for item in packet_rows}
        for item in score_rows:
            values = item.get("scores")
            if not isinstance(values, dict) or set(values) != set(dimensions) or any(not isinstance(values[name], int) or isinstance(values[name], bool) or values[name] < 0 or values[name] > 2 for name in dimensions):
                raise ValueError("score dimensions are invalid")
            assessment = item.get("causal_assessment")
            if not isinstance(assessment, dict) or assessment.get("alignment") not in ("aligned", "partial", "missing", "contradicted") or not isinstance(assessment.get("initiating_cause_found"), bool) or not isinstance(assessment.get("downstream_treated_as_primary"), bool) or not isinstance(assessment.get("required_chain_coverage"), list) or not all(isinstance(value, str) for value in assessment["required_chain_coverage"]):
                raise ValueError("score causal assessment is incomplete")
            required_ids = {entry["id"] for entry in reference_by_key[(item["packet_id"], item["arm"])]["required_chain"]}
            coverage = set(assessment["required_chain_coverage"])
            if not coverage.issubset(required_ids):
                raise ValueError("score causal coverage contains an unknown reference id")
            if values["diagnosis"] == 2 and (assessment["alignment"] != "aligned" or not assessment["initiating_cause_found"] or assessment["downstream_treated_as_primary"] or coverage != required_ids):
                raise ValueError("full diagnosis credit requires complete reference-aligned causal coverage")
        freeze = {"version": 1, "packet_set_sha256": packet_hash, "reference_set_sha256": reference_hash, "score_set_sha256": canonical_hash(scores)}
        target = Path(args.output)
        target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        target.write_text(json.dumps(freeze, indent=2, sort_keys=True) + "\n")
        os.chmod(target, 0o600)
        return 0
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(f"blind score freeze error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
