#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
python3 - "$root/docs/kubernetes.md" "$@" <<'PY'
from pathlib import Path
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile


def verification_blocks(path):
    text = path.read_text()
    sections = list(re.finditer(r"^## (?:Verify the first deployment|Verify)$", text, re.M))
    if len(sections) != 1:
        raise AssertionError(f"{path}: expected exactly one verification section")
    section = re.split(r"^## ", text[sections[0].end():], maxsplit=1, flags=re.M)[0]
    blocks = re.findall(r"^```bash\n(.*?)^```[ \t]*$", section, re.M | re.S)
    if len(blocks) != 3 or len(re.findall(r"^```", section, re.M)) != 6:
        raise AssertionError(f"{path}: expected readiness, port-forward, and data Bash fences")
    for block, anchors in zip(blocks, [
        ["SERVER=$(kubectl", "WRITER=$(kubectl", "SERVICE=$(kubectl", "rollout status"],
        ["port-forward", "service/$SERVICE", "18080:80"],
        ["/data/manifest.json", "/data/dashboard.json", "/data/ai_cache.json",
         "python3 -m json.tool", 'grep -F "$EXPECTED_JOB"', "sandboxes.agents.x-k8s.io"],
    ]):
        if any(anchor not in block for anchor in anchors):
            raise AssertionError(f"{path}: verification fence is missing its expected anchors")
    if any("port-forward" in block for block in (blocks[0], blocks[2])):
        raise AssertionError(f"{path}: port-forward must remain a separate, unexecuted fence")
    return blocks[0], blocks[2]


bash = shutil.which("bash")
grep = shutil.which("grep")
if not bash or not grep:
    raise SystemExit("Local bash and grep are required")
bash = str(Path(bash).resolve())
grep = str(Path(grep).resolve())
python = str(Path(sys.executable).resolve())

context = "verification-context"
namespace = "verification-app"
release = "verification-release"
execution_namespace = "verification-execution"
public_url = "https://dashboard.example.test/"
job = "fixture-e2e-job"
local_url = "http://127.0.0.1:18080/data/"
kubectl = ["--context", context, "-n", namespace]
commands = {}


def command(name, tool, args, output=""):
    commands[name] = {"name": name, "tool": tool, "args": args, "output": output, "status": 0}


for name, resource, component, resolved in [
    ("server-get", "deployment", "server", "fixture-server"),
    ("writer-get", "deployment", "worker", "fixture-worker"),
    ("service-get", "service", "server", "fixture-service"),
]:
    command(name, "kubectl", kubectl + [
        "get", resource, "-l",
        f"app.kubernetes.io/instance={release},app.kubernetes.io/component={component}",
        "-o", "jsonpath={.items[0].metadata.name}",
    ], resolved)
for name, resolved, timeout in [
    ("server-rollout", "fixture-server", "5m"), ("writer-rollout", "fixture-worker", "10m"),
]:
    command(name, "kubectl", kubectl + [
        "rollout", "status", f"deployment/{resolved}", f"--timeout={timeout}",
    ])
command("manifest", "curl", [
    "--fail", "--retry", "60", "--retry-delay", "10", "--retry-connrefused",
    local_url + "manifest.json",
], '{"version": 1}\n')
command("dashboard", "curl", ["--fail", local_url + "dashboard.json"],
        json.dumps({"jobs": [job]}) + "\n")
command("private", "curl", [
    "--silent", "--show-error", "--output", "/dev/null", "--write-out", "%{http_code}",
    local_url + "ai_cache.json",
], "404")
command("public", "curl", ["--fail", public_url.rstrip("/") + "/data/manifest.json"],
        '{"version": 1}\n')
command("sandboxes", "kubectl", [
    "--context", context, "-n", execution_namespace, "get", "sandboxes.agents.x-k8s.io",
    "-o", "name",
])

# Only exact read-only fixture requests are accepted. These controls isolate the
# reviewed examples; they are not a security sandbox for arbitrary shell code.
stub = f"#!{python} -I\n" + r'''
from pathlib import Path
import json
import os
import sys

tool = Path(sys.argv[0]).name
args = sys.argv[1:]
entries = json.loads(Path(os.environ["VERIFICATION_RESPONSES"]).read_text())
matches = [entry for entry in entries if entry["tool"] == tool and entry["args"] == args]
entry = matches[0] if len(matches) == 1 else {
    "name": "REJECTED", "status": 97, "output": "",
}
with Path(os.environ["VERIFICATION_TRACE"]).open("a") as trace:
    trace.write(json.dumps({
        "name": entry["name"], "tool": tool, "args": args, "status": entry["status"],
    }) + "\n")
if entry["name"] == "REJECTED":
    print(f"Rejected command: {tool} {args!r}", file=sys.stderr)
sys.stdout.write(entry["output"])
sys.exit(entry["status"])
'''

readiness_trace = ["server-get", "writer-get", "service-get", "server-rollout", "writer-rollout"]
data_trace = ["manifest", "dashboard", "private", "public", "sandboxes"]
base_env = {
    "CONTEXT": context, "NAMESPACE": namespace, "RELEASE": release,
    "EXPECTED_JOB": job, "PUBLIC_URL": public_url, "EXECUTION_NAMESPACE": execution_namespace,
}


def run_case(directory, name, block, expected_trace, success, responses=None, variables=None):
    case = directory / name
    case.mkdir()
    for child in ("bin", "home", "tmp", "work"):
        (case / child).mkdir()
    for tool in ("curl", "kubectl"):
        path = case / "bin" / tool
        path.write_text(stub)
        path.chmod(0o755)
    (case / "bin" / "python3").symlink_to(python)
    (case / "bin" / "grep").symlink_to(grep)
    entries = {key: dict(value) for key, value in commands.items()}
    for key, response in (responses or {}).items():
        entries[key].update(response)
    response_path = case / "responses.json"
    response_path.write_text(json.dumps(list(entries.values())))
    trace_path = case / "trace.jsonl"
    env = {
        "PATH": str(case / "bin"), "HOME": str(case / "home"), "TMPDIR": str(case / "tmp"),
        "LANG": "C", "LC_ALL": "C", "VERIFICATION_RESPONSES": str(response_path),
        "VERIFICATION_TRACE": str(trace_path), **base_env,
    }
    for key, value in (variables or {}).items():
        if value is None:
            env.pop(key, None)
        else:
            env[key] = value

    # No harness-supplied errexit/pipefail or inherited shell startup environment.
    process = subprocess.Popen(
        [bash, "--noprofile", "--norc", "-c", block],
        cwd=case / "work", env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        text=True, start_new_session=True,
    )
    try:
        stdout, stderr = process.communicate(timeout=10)
    except subprocess.TimeoutExpired:
        raise AssertionError(f"{directory.name}/{name}: exceeded 10 seconds")
    finally:
        # Also reap any descendants if a changed example starts background work.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()
    actual = [json.loads(line) for line in trace_path.read_text().splitlines()] if trace_path.exists() else []
    expected = [
        {key: entries[step][key] for key in ("name", "tool", "args", "status")}
        for step in expected_trace
    ]
    if (process.returncode == 0) != success or actual != expected:
        raise AssertionError(
            f"{directory.name}/{name}: expected success={success}, trace={expected_trace}; "
            f"got status={process.returncode}, trace={actual}\nstdout={stdout}\nstderr={stderr}"
        )


paths = [Path(value).resolve() for value in (sys.argv[2:] or [sys.argv[1]])]
total = 0
with tempfile.TemporaryDirectory(prefix="aster-kubernetes-verification-") as scratch:
    for document_index, path in enumerate(paths):
        readiness, data = verification_blocks(path)
        directory = Path(scratch) / f"document-{document_index}"
        directory.mkdir()
        cases = [
            ("readiness-success", readiness, readiness_trace, True, {}, {}),
            ("data-success", data, data_trace, True, {}, {}),
        ]
        for index, step in enumerate(readiness_trace):
            cases.append((f"{step}-failed", readiness, readiness_trace[:index + 1], False,
                          {step: {"status": 7}}, {}))
            if step.endswith("-get"):
                cases.append((f"{step}-empty", readiness, readiness_trace[:index + 1], False,
                              {step: {"output": ""}}, {}))
        for variable in ("CONTEXT", "NAMESPACE", "RELEASE"):
            for value in ("", None):
                cases.append((f"{variable}-{value!s}", readiness, [], False, {}, {variable: value}))
        for name, step, response in [
            ("manifest-producer-failed", "manifest", {"status": 7}),
            ("manifest-invalid-json", "manifest", {"output": "not json\n"}),
            ("dashboard-producer-failed", "dashboard", {"status": 7}),
            ("dashboard-missing-job", "dashboard", {"output": '{"jobs": []}\n'}),
            ("private-200", "private", {"output": "200"}),
            ("private-failed-with-404", "private", {"status": 7}),
            ("public-producer-failed", "public", {"status": 7}),
            ("public-invalid-json", "public", {"output": "not json\n"}),
            ("sandboxes-failed-empty", "sandboxes", {"status": 7}),
            ("sandboxes-not-empty", "sandboxes", {"output": "sandbox/leftover\n"}),
        ]:
            cases.append((name, data, data_trace[:data_trace.index(step) + 1], False,
                          {step: response}, {}))
        for value in ("", None):
            cases.append((f"EXPECTED_JOB-{value!s}", data, [], False, {}, {"EXPECTED_JOB": value}))
            cases.append((f"execution-without-context-{value!s}", data, data_trace[:-1], False,
                          {}, {"CONTEXT": value}))
        for public_index, public in enumerate(("", None, public_url)):
            for execution_index, execution in enumerate(("", None, execution_namespace)):
                trace = data_trace[:3]
                if public:
                    trace += ["public"]
                if execution:
                    trace += ["sandboxes"]
                cases.append((f"optional-{public_index}-{execution_index}",
                              data, trace, True, {}, {
                                  "PUBLIC_URL": public, "EXECUTION_NAMESPACE": execution,
                                  "CONTEXT": context if execution else None,
                              }))

        # Separate success probes keep the extracted body intact and verify its
        # interactive-shell contract without supplying missing failure handling.
        before = 'before_flags=$-\nbefore_options=$(set +o)\n'
        after = '\nstatus=$?\ntest "$status" = 0 && test "$-" = "$before_flags" && test "$(set +o)" = "$before_options"'
        cases.append(("readiness-caller-state", before + readiness + after +
                      '\nresult=$?\ntest "$result" = 0 && test "$SERVER" = fixture-server && '
                      'test "$WRITER" = fixture-worker && test "$SERVICE" = fixture-service',
                      readiness_trace, True, {}, {}))
        cases.append(("data-caller-options", before + data + after, data_trace, True, {}, {}))
        for args in cases:
            run_case(directory, *args)
        total += len(cases)
        print(f"{path}: {len(cases)} local verification controls passed")
print(f"Kubernetes verification fail-closed checks passed ({total} cases).")
PY
