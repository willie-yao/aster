#!/usr/bin/env python3
"""Validate the setup-to-diagnostic-authoring handoff without external packages."""

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

SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
MODES = {"pages", "k8s"}
ARTIFACT_ACCESS = {"public", "authenticated", "private", "unknown"}
PROMPT_STATUS = {
    "preserved-existing",
    "replaced-with-source-only-baseline",
    "created-source-only-baseline",
}


def add(errors: list[str], path: str, message: str) -> None:
    errors.append(f"{path}: {message}")


def require_dict(value: Any, path: str, errors: list[str]) -> dict[str, Any]:
    if not isinstance(value, dict):
        add(errors, path, "must be an object")
        return {}
    return value


def require_list(value: Any, path: str, errors: list[str]) -> list[Any]:
    if not isinstance(value, list):
        add(errors, path, "must be an array")
        return []
    return value


def require_text(obj: dict[str, Any], key: str, path: str, errors: list[str]) -> str:
    value = obj.get(key)
    if not isinstance(value, str) or not value.strip():
        add(errors, f"{path}.{key}", "must be a non-empty string")
        return ""
    return value


def require_sha(obj: dict[str, Any], key: str, path: str, errors: list[str], optional: bool = False) -> str:
    value = obj.get(key)
    if optional and value is None:
        return ""
    if not isinstance(value, str) or not SHA256_RE.fullmatch(value):
        add(errors, f"{path}.{key}", "must be sha256:<64 lowercase hex>")
        return ""
    return value


def validate(data: Any) -> list[str]:
    errors: list[str] = []
    root = require_dict(data, "$", errors)
    if root.get("schema_version") != 1:
        add(errors, "$.schema_version", "must equal 1")
    plan_digest = require_sha(root, "plan_digest", "$", errors)

    engine = require_dict(root.get("engine"), "$.engine", errors)
    require_text(engine, "path", "$.engine", errors)
    require_text(engine, "version", "$.engine", errors)
    if "revision" in engine and not isinstance(engine["revision"], str):
        add(errors, "$.engine.revision", "must be a string")

    consumer = require_dict(root.get("consumer"), "$.consumer", errors)
    consumer_path = require_text(consumer, "path", "$.consumer", errors)
    require_text(consumer, "project_id", "$.consumer", errors)
    require_text(consumer, "name", "$.consumer", errors)
    validate_repository(consumer.get("repository"), "$.consumer.repository", errors)

    source = require_dict(root.get("source"), "$.source", errors)
    validate_repository(source.get("repository"), "$.source.repository", errors)
    revision = require_dict(source.get("revision"), "$.source.revision", errors)
    status = revision.get("status")
    if status not in {"resolved", "unresolved"}:
        add(errors, "$.source.revision.status", "must be resolved or unresolved")
    if status == "resolved":
        value = require_text(revision, "revision", "$.source.revision", errors)
        if value and not re.fullmatch(r"[0-9a-fA-F]{40}", value):
            add(errors, "$.source.revision.revision", "must be a 40-character Git commit")
        require_text(revision, "ref", "$.source.revision", errors)
    elif revision.get("revision"):
        add(errors, "$.source.revision.revision", "must be empty when unresolved")

    discovery = require_dict(root.get("discovery"), "$.discovery", errors)
    if discovery.get("selector") not in {"testgrid", "bucket", "bucket-exact-jobs"}:
        add(errors, "$.discovery.selector", "is invalid")
    require_sha(discovery, "digest", "$.discovery", errors)
    jobs = require_list(discovery.get("jobs"), "$.discovery.jobs", errors)
    if not jobs:
        add(errors, "$.discovery.jobs", "must contain at least one selected job")
    identities: set[tuple[str, str, str]] = set()
    for index, raw_job in enumerate(jobs):
        job = require_dict(raw_job, f"$.discovery.jobs[{index}]", errors)
        name = require_text(job, "name", f"$.discovery.jobs[{index}]", errors)
        job_type = job.get("job_type")
        if job_type not in {"periodic", "presubmit"}:
            add(errors, f"$.discovery.jobs[{index}].job_type", "must be periodic or presubmit")
        repo = job.get("repo", "")
        if not isinstance(repo, str):
            add(errors, f"$.discovery.jobs[{index}].repo", "must be a string")
            repo = ""
        identity = (name, str(job_type), repo)
        if identity in identities:
            add(errors, f"$.discovery.jobs[{index}]", "duplicates a selected job identity")
        identities.add(identity)

    artifact_location = require_dict(root.get("artifact_location"), "$.artifact_location", errors)
    provider = artifact_location.get("provider")
    if provider not in {"gcs", "gcsweb", "local"}:
        add(errors, "$.artifact_location.provider", "must be gcs, gcsweb, or local")
    if provider in {"gcs", "gcsweb"}:
        require_text(artifact_location, "bucket", "$.artifact_location", errors)
    if provider in {"gcsweb", "local"}:
        require_text(artifact_location, "base", "$.artifact_location", errors)
    if discovery.get("selector") in {"bucket", "bucket-exact-jobs"}:
        if provider in {"gcs", "gcsweb"} and artifact_location.get("bucket") != discovery.get("bucket"):
            add(errors, "$.artifact_location.bucket", "must match discovery.bucket")
        if provider == "gcsweb" and artifact_location.get("base") != discovery.get("gcsweb_base"):
            add(errors, "$.artifact_location.base", "must match discovery.gcsweb_base")

    test_infra = require_dict(root.get("test_infra"), "$.test_infra", errors)
    test_infra_status = test_infra.get("status")
    if test_infra_status not in {"resolved", "unresolved", "not_applicable"}:
        add(errors, "$.test_infra.status", "is invalid")
    if discovery.get("selector") == "testgrid":
        test_infra_repo = require_dict(test_infra.get("repository"), "$.test_infra.repository", errors)
        validate_repository(test_infra_repo, "$.test_infra.repository", errors)
        if test_infra_repo.get("full_name") != "kubernetes/test-infra":
            add(errors, "$.test_infra.repository.full_name", "must be kubernetes/test-infra for TestGrid discovery")
        if test_infra_status == "not_applicable":
            add(errors, "$.test_infra.status", "cannot be not_applicable for TestGrid discovery")
        if test_infra_status == "resolved":
            revision_value = require_text(test_infra, "revision", "$.test_infra", errors)
            if revision_value and not re.fullmatch(r"[0-9a-f]{40}", revision_value):
                add(errors, "$.test_infra.revision", "must be a lowercase 40-character Git commit")
            if revision_value != discovery.get("catalog_revision"):
                add(errors, "$.test_infra.revision", "must match discovery.catalog_revision")
    elif test_infra_status != "not_applicable":
        add(errors, "$.test_infra.status", "must be not_applicable for bucket discovery")
    config_files = test_infra.get("config_files", [])
    if not isinstance(config_files, list) or any(not isinstance(item, str) or not item for item in config_files):
        add(errors, "$.test_infra.config_files", "must contain non-empty strings")
    elif config_files != sorted(set(config_files)):
        add(errors, "$.test_infra.config_files", "must be sorted and unique")

    deployment = require_dict(root.get("deployment"), "$.deployment", errors)
    if deployment.get("mode") not in MODES:
        add(errors, "$.deployment.mode", "must be pages or k8s")
    reasons = require_list(deployment.get("reasons"), "$.deployment.reasons", errors)
    if not reasons or any(not isinstance(reason, str) or not reason.strip() for reason in reasons):
        add(errors, "$.deployment.reasons", "must contain non-empty reviewed reasons")
    if deployment.get("artifact_access") not in ARTIFACT_ACCESS:
        add(errors, "$.deployment.artifact_access", "is invalid")

    prompt = require_dict(root.get("prompt"), "$.prompt", errors)
    original = require_sha(prompt, "original_sha256", "$.prompt", errors, optional=True)
    candidate = require_sha(prompt, "candidate_sha256", "$.prompt", errors)
    active = require_sha(prompt, "active_sha256", "$.prompt", errors)
    if prompt.get("status") not in PROMPT_STATUS:
        add(errors, "$.prompt.status", "is invalid")
    if prompt.get("baseline_status") != "source-only-unvalidated":
        add(errors, "$.prompt.baseline_status", "must be source-only-unvalidated")
    require_text(prompt, "active_path", "$.prompt", errors)
    candidate_text = require_text(prompt, "source_only_candidate", "$.prompt", errors)
    if candidate_text and candidate != "sha256:" + hashlib.sha256(candidate_text.encode()).hexdigest():
        add(errors, "$.prompt.candidate_sha256", "does not match source_only_candidate")
    if prompt.get("status") == "preserved-existing" and not original:
        add(errors, "$.prompt.original_sha256", "is required for a preserved prompt")
    if prompt.get("status") != "preserved-existing" and active != candidate:
        add(errors, "$.prompt.active_sha256", "must match the candidate when the candidate was applied")

    apply_result = require_dict(root.get("apply_result"), "$.apply_result", errors)
    if apply_result.get("schema_version") != 1:
        add(errors, "$.apply_result.schema_version", "must equal 1")
    if apply_result.get("plan_digest") != plan_digest:
        add(errors, "$.apply_result.plan_digest", "must match the top-level plan digest")
    if apply_result.get("destination") != consumer_path:
        add(errors, "$.apply_result.destination", "must match consumer.path")
    if apply_result.get("matches_reviewed_plan") is not True:
        add(errors, "$.apply_result.matches_reviewed_plan", "must be true")
    files = require_list(apply_result.get("files"), "$.apply_result.files", errors)
    if not files:
        add(errors, "$.apply_result.files", "must not be empty")
    seen_paths: set[str] = set()
    for index, raw_file in enumerate(files):
        item = require_dict(raw_file, f"$.apply_result.files[{index}]", errors)
        path = require_text(item, "path", f"$.apply_result.files[{index}]", errors)
        if path in seen_paths:
            add(errors, f"$.apply_result.files[{index}].path", "duplicates another manifest path")
        seen_paths.add(path)
        require_sha(item, "sha256", f"$.apply_result.files[{index}]", errors)
        if item.get("status") not in {"create", "replace", "preserve"}:
            add(errors, f"$.apply_result.files[{index}].status", "is invalid")
        if item.get("ownership") not in {"engine_generated", "consumer_owned"}:
            add(errors, f"$.apply_result.files[{index}].ownership", "is invalid")
        if item.get("matches_reviewed_plan") is not True:
            add(errors, f"$.apply_result.files[{index}].matches_reviewed_plan", "must be true")
    if "prompts/system.md" not in seen_paths:
        add(errors, "$.apply_result.files", "must include prompts/system.md")

    smoke = require_dict(root.get("artifact_smoke"), "$.artifact_smoke", errors)
    if smoke.get("read_only") is not True:
        add(errors, "$.artifact_smoke.read_only", "must be true")
    builds_per_job = smoke.get("builds_per_job")
    if not isinstance(builds_per_job, int) or not 0 <= builds_per_job <= 5:
        add(errors, "$.artifact_smoke.builds_per_job", "must be an integer from 0 to 5")
    smoke_jobs = require_list(smoke.get("jobs"), "$.artifact_smoke.jobs", errors)
    if builds_per_job and len(smoke_jobs) != len(jobs):
        add(errors, "$.artifact_smoke.jobs", "must cover every selected job when enabled")

    doctor = require_dict(root.get("doctor"), "$.doctor", errors)
    if doctor.get("project_dir") != consumer_path:
        add(errors, "$.doctor.project_dir", "must match consumer.path")
    checks = require_list(doctor.get("checks"), "$.doctor.checks", errors)
    if not checks:
        add(errors, "$.doctor.checks", "must contain doctor results")

    warnings = root.get("unresolved_warnings", [])
    if not isinstance(warnings, list) or any(not isinstance(item, str) or not item.strip() for item in warnings):
        add(errors, "$.unresolved_warnings", "must contain non-empty strings")
    elif warnings != sorted(set(warnings)):
        add(errors, "$.unresolved_warnings", "must be sorted and unique")
    next_phase = require_text(root, "next_phase", "$", errors)
    if next_phase and "$author-prow-ai-diagnostics" not in next_phase:
        add(errors, "$.next_phase", "must name $author-prow-ai-diagnostics")
    return errors


def validate_repository(value: Any, path: str, errors: list[str]) -> None:
    repo = require_dict(value, path, errors)
    owner = require_text(repo, "owner", path, errors)
    name = require_text(repo, "name", path, errors)
    full_name = require_text(repo, "full_name", path, errors)
    if owner and name and full_name != f"{owner}/{name}":
        add(errors, f"{path}.full_name", "must equal owner/name")


def valid_fixture() -> dict[str, Any]:
    candidate_text = "# Source-only prompt\n"
    candidate = "sha256:" + hashlib.sha256(candidate_text.encode()).hexdigest()
    plan_digest = "sha256:" + "1" * 64
    prompt_file = {
        "path": "prompts/system.md",
        "mode": "0644",
        "sha256": candidate,
        "status": "create",
        "ownership": "consumer_owned",
        "matches_reviewed_plan": True,
    }
    apply_result = {
        "schema_version": 1,
        "plan_digest": plan_digest,
        "destination": "/tmp/consumer",
        "files": [prompt_file],
        "matches_reviewed_plan": True,
        "prompt": {"candidate_sha256": candidate, "active_sha256": candidate, "status": "created-source-only-baseline"},
    }
    return {
        "schema_version": 1,
        "plan_digest": plan_digest,
        "engine": {"path": "/tmp/engine", "version": "v1.2.3", "revision": "a" * 40},
        "consumer": {"repository": {"owner": "example", "name": "dashboard", "full_name": "example/dashboard"}, "path": "/tmp/consumer", "project_id": "project", "name": "Project"},
        "source": {"repository": {"owner": "example", "name": "project", "full_name": "example/project"}, "revision": {"status": "resolved", "ref": "main", "revision": "b" * 40}},
        "discovery": {"selector": "testgrid", "testgrid": "sig-project", "catalog_revision": "c" * 40, "digest": "sha256:" + "2" * 64, "jobs": [{"name": "periodic-project", "job_type": "periodic"}]},
        "artifact_location": {"provider": "gcs", "bucket": "kubernetes-ci-logs"},
        "test_infra": {"repository": {"owner": "kubernetes", "name": "test-infra", "full_name": "kubernetes/test-infra"}, "revision": "c" * 40, "status": "resolved", "config_files": ["config/jobs/project.yaml"]},
        "deployment": {"mode": "pages", "reasons": ["Public artifacts and provider are reachable from GitHub Actions."], "artifact_access": "public", "ai_enabled": False},
        "prompt": {"candidate_sha256": candidate, "active_sha256": candidate, "status": "created-source-only-baseline", "baseline_status": "source-only-unvalidated", "active_path": "/tmp/consumer/prompts/system.md", "source_only_candidate": candidate_text, "requested_mode": "handoff", "source": "Agent handoff bundle with TODO template"},
        "apply_result": apply_result,
        "artifact_smoke": {"read_only": True, "builds_per_job": 1, "jobs": [{}]},
        "doctor": {"project_dir": "/tmp/consumer", "checks": [{"name": "project.yaml", "status": "pass", "detail": "ok"}]},
        "unresolved_warnings": [],
        "next_phase": "Run $author-prow-ai-diagnostics with this handoff.",
    }


def self_test() -> None:
    fixture = valid_fixture()
    errors = validate(fixture)
    if errors:
        raise AssertionError(errors)
    broken = copy.deepcopy(fixture)
    broken["prompt"]["candidate_sha256"] = "sha256:" + "0" * 64
    if not any("source_only_candidate" in item for item in validate(broken)):
        raise AssertionError("candidate hash mismatch was accepted")
    broken = copy.deepcopy(fixture)
    broken["apply_result"]["files"][0]["matches_reviewed_plan"] = False
    if not any("matches_reviewed_plan" in item for item in validate(broken)):
        raise AssertionError("manifest mismatch was accepted")
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / "handoff.json"
        path.write_text(json.dumps(fixture))
        loaded = json.loads(path.read_text())
        if validate(loaded):
            raise AssertionError("round-trip fixture failed")
    print("setup handoff validator self-test passed")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("handoff", nargs="?", type=Path)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        return 0
    if args.handoff is None:
        parser.error("handoff path is required unless --self-test is used")
    try:
        data = json.loads(args.handoff.read_text())
    except (OSError, json.JSONDecodeError) as exc:
        print(f"handoff: {exc}", file=sys.stderr)
        return 1
    errors = validate(data)
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 1
    print(f"valid setup handoff: {args.handoff}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
