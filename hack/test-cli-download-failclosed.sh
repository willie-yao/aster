#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
python3 - "$root" "$@" <<'PY'
from pathlib import Path
import argparse
import hashlib
import json
import os
import re
import shutil
import signal
import stat
import subprocess
import sys
import uuid


def download_block(text, label):
    headings = list(re.finditer(r"^## Download and verify the CLI$", text, re.M))
    if len(headings) != 1:
        raise AssertionError(f"{label}: expected exactly one CLI download section")
    section = re.split(r"^## ", text[headings[0].end():], maxsplit=1, flags=re.M)[0]
    blocks = re.findall(r"^```bash\n(.*?)^```[ \t]*$", section, re.M | re.S)
    if len(blocks) != 1 or len(re.findall(r"^```", section, re.M)) != 2:
        raise AssertionError(f"{label}: expected exactly one CLI download Bash fence")
    return blocks[0]


root = Path(sys.argv[1])
parser = argparse.ArgumentParser()
parser.add_argument("documents", nargs="*", type=Path)
parser.add_argument("--results", type=Path)
args = parser.parse_args(sys.argv[2:])
bash, awk = shutil.which("bash"), shutil.which("awk")
if not bash or not awk:
    raise SystemExit("Local bash and awk are required")
bash, awk = str(Path(bash).resolve()), str(Path(awk).resolve())
python = str(Path(sys.executable).resolve())
payload = b"fixture CLI payload\x00\xff\n"
old_payload = b"previous CLI\n"
digest = hashlib.sha256(payload).hexdigest()
version = "v0.0.0-fixture"

# Only exact fixture commands are accepted. Awk parses the real manifest, and
# checksum shims hash the real downloaded bytes without invoking host tools.
stub = f"#!{python} -I\n" + r'''
from pathlib import Path
import hashlib
import json
import os
import re
import subprocess
import sys

config = json.loads(Path(os.environ["CLI_FIXTURE"]).read_text())
tool, args = Path(sys.argv[0]).name, sys.argv[1:]
cwd = os.getcwd()
matches = [e for e in config["commands"] if e["tool"] == tool and e["args"] == args]
entry = matches[0] if len(matches) == 1 else {"name": "REJECTED", "status": 97}
status = entry["status"]
output = entry.get("output", "")
error = ""
if entry["name"] == "REJECTED":
    error = f"Rejected command: {tool} {args!r}\n"
elif tool == "uname":
    pass
elif tool == "install":
    if args[0] == "-d":
        if status == 0:
            Path(args[-1]).mkdir(parents=True, exist_ok=True)
            Path(args[-1]).chmod(0o755)
    elif status == 0:
        Path(args[-1]).write_bytes(Path(args[-2]).read_bytes())
        Path(args[-1]).chmod(0o755)
    elif entry.get("partial"):
        Path(args[-1]).write_bytes(b"partial install\n")
        Path(args[-1]).chmod(0o600)
elif tool == "mktemp":
    if status == 0:
        Path(config["download"]).mkdir()
        output = config["download"] + "\n"
elif tool == "curl":
    if entry.get("write", True):
        Path(args[-1]).write_bytes(bytes(entry["bytes"]))
elif tool == "awk":
    parsed = subprocess.run([config["awk"], *args], capture_output=True, text=True)
    output, error = parsed.stdout, parsed.stderr
    status = status or parsed.returncode
    if entry.get("remove_download"):
        Path(config["download"], config["asset"]).unlink()
        Path(config["download"], "SHA256SUMS").unlink()
        Path(config["download"]).rmdir()
elif tool in ("sha256sum", "shasum"):
    status = 0
    lines = sys.stdin.read().splitlines()
    if not lines:
        status, error = 1, "no checksum lines\n"
    for line in lines:
        match = re.fullmatch(r"([a-fA-F0-9]{64}) [ *](.+)", line)
        if not match or match[2] != config["asset"]:
            status, error = 1, "invalid checksum line\n"
            break
        if Path.cwd() != Path(config["download"]):
            status, error = 97, "checksum outside download directory\n"
            break
        actual = hashlib.sha256(Path(match[2]).read_bytes()).hexdigest()
        if actual != match[1].lower():
            status, error = 1, "checksum mismatch\n"
            break
elif tool == "rm":
    if status == 0:
        for value in args[2:]:
            Path(value).unlink(missing_ok=True)
elif tool == "rmdir":
    if status == 0:
        try:
            Path(args[-1]).rmdir()
        except OSError as exc:
            status, error = 1, str(exc) + "\n"
else:
    status, error = 97, f"Unhandled fixture command: {tool}\n"

with Path(os.environ["CLI_TRACE"]).open("a") as trace:
    trace.write(json.dumps({
        "name": entry["name"], "tool": tool, "args": args,
        "cwd": cwd, "status": status,
    }) + "\n")
sys.stdout.write(output)
sys.stderr.write(error)
sys.exit(status)
'''

reports = []


def run_case(directory, block, name, tools=("sha256sum",), platform=("Linux", "x86_64"),
             target="linux-amd64", changes=None, stop=None, status=0, diagnostic="",
             cli_version=version, installed="old", cleanup=True, caller_options=False):
    case = directory / name
    home, work, scratch, bin_dir = [case / p for p in ("home space", "work", "scratch space", "bin")]
    cli_dir = home / ".local" / "share" / "aster" / version
    download = scratch / "aster-cli-download.fixture"
    asset = f"aster-{version}-{target}"
    destination = cli_dir / asset
    manifest = f"{digest}  {asset}.sig\n{digest}  other-asset\n{digest}  {asset}\n"
    commands = {}

    def command(name, tool, argv, **values):
        commands[name] = {"name": name, "tool": tool, "args": argv, "status": 0, **values}

    command("os", "uname", ["-s"], output=platform[0] + "\n")
    command("arch", "uname", ["-m"], output=platform[1] + "\n")
    command("prepare", "install", ["-d", "-m", "755", str(cli_dir)])
    command("allocate", "mktemp", ["-d", str(scratch / "aster-cli-download.XXXXXX")])
    url = f"https://github.com/willie-yao/aster/releases/download/{version}"
    for step, filename, body in [("asset", asset, payload), ("manifest", "SHA256SUMS", manifest.encode())]:
        command(step, "curl", ["--fail", "--location", f"{url}/{filename}",
                              "--output", str(download / filename)], bytes=list(body))
    command("extract", "awk", ["-v", f"asset={asset}", "$2 == asset {print}", str(download / "SHA256SUMS")])
    for tool, argv in [("sha256sum", ["--check"]), ("shasum", ["-a", "256", "--check"])]:
        command(tool, tool, argv)
    command("install", "install", ["-m", "0755", str(download / asset), str(destination)])
    command("cleanup-files", "rm", ["-f", "--", str(download / asset), str(download / "SHA256SUMS")])
    command("cleanup-dir", "rmdir", ["--", str(download)])
    for key, values in (changes or {}).items():
        commands[key].update(values)

    directories = [case, home, work, scratch, bin_dir, home / ".local", home / ".local" / "share",
                   home / ".local" / "share" / "aster", cli_dir]
    files = [destination, download / asset, download / "SHA256SUMS"]
    for path in directories:
        path.mkdir()
    destination.write_bytes(old_payload)
    destination.chmod(0o640)
    config = case / "config.json"
    trace_path = case / "trace.jsonl"
    probe_names = ("before-options", "after-options", "before-trap", "after-trap", "caller-trap",
                   "state", "exports")
    probes = {key: case / key for key in probe_names}
    files.extend([config, trace_path, *probes.values()])
    try:
        for tool in ("uname", "curl", "install", "mktemp", "rm", "rmdir", "awk", *tools):
            path = bin_dir / tool
            files.append(path)
            path.write_text(stub)
            path.chmod(0o755)
        config.write_text(json.dumps({
            "commands": list(commands.values()), "awk": awk,
            "download": str(download), "asset": asset,
        }))
        env = {
            "PATH": str(bin_dir), "HOME": str(home), "TMPDIR": str(scratch),
            "LANG": "C", "LC_ALL": "C", "ASTER": "previous-aster",
            "CLI_FIXTURE": str(config), "CLI_TRACE": str(trace_path),
            **{"PROBE_" + key.upper().replace("-", "_"): str(path) for key, path in probes.items()},
        }
        if cli_version is not None:
            env["CLI_VERSION"] = cli_version
        before = r'''
trap 'printf "caller exit\n" >> "$PROBE_CALLER_TRAP"' EXIT
before_flags=$-
set +o > "$PROBE_BEFORE_OPTIONS"
trap -p EXIT > "$PROBE_BEFORE_TRAP"
'''
        after = r'''
snippet_status=$?
set +o > "$PROBE_AFTER_OPTIONS"
trap -p EXIT > "$PROBE_AFTER_TRAP"
if [ -e "$PROBE_CALLER_TRAP" ]; then trap_ran_early=yes; else trap_ran_early=no; fi
printf '%s\n' "$snippet_status" "${ASTER-unset}" "$before_flags" "$-" "$trap_ran_early" > "$PROBE_STATE"
export -p > "$PROBE_EXPORTS"
exit "$snippet_status"
'''
        # The body is not sourced in an if/function or given errexit/pipefail.
        script = ("set -f\nset -o noclobber\n" if caller_options else "") + before + block + after
        process = subprocess.Popen(
            [bash, "--noprofile", "--norc", "-c", script], cwd=work, env=env,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True,
        )
        try:
            stdout, stderr = process.communicate(timeout=30)
        except subprocess.TimeoutExpired as exc:
            trace = trace_path.read_text() if trace_path.exists() else ""
            raise AssertionError(f"exceeded 30 seconds; command trace:\n{trace}") from exc
        finally:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
        trace = [json.loads(line) for line in trace_path.read_text().splitlines()] if trace_path.exists() else []
        selected = "sha256sum" if "sha256sum" in tools else "shasum"
        steps = ["os", "arch", "prepare", "allocate", "asset", "manifest", "extract", selected, "install"]
        if stop == "version":
            steps = []
        elif stop == "platform":
            steps = steps[:2]
        elif stop in ("cd", "checksum-tool"):
            steps = steps[:steps.index("extract") + 1]
        elif stop:
            steps = steps[:steps.index(stop) + 1]
        if "allocate" in steps and commands["allocate"]["status"] == 0:
            steps.append("cleanup-files")
            if commands["cleanup-files"]["status"] == 0:
                steps.append("cleanup-dir")
        changed_dir = "extract" in steps and stop not in ("extract", "cd")
        expected = [{
            "name": step, "tool": commands[step]["tool"], "args": commands[step]["args"],
            "cwd": str(download if changed_dir and step in (selected, "install", "cleanup-files", "cleanup-dir") else work),
            "status": commands[step]["status"],
        } for step in steps]
        state = probes["state"].read_text().splitlines() if probes["state"].exists() else []
        expected_bytes, expected_mode = {
            "old": (old_payload, 0o640), "new": (payload, 0o755), "partial": (b"partial install\n", 0o600),
        }[installed]
        actual_bytes = destination.read_bytes()
        actual_mode = stat.S_IMODE(destination.stat().st_mode)
        report = {
            "name": name, "status": process.returncode, "expected_status": status,
            "trace": trace, "expected_trace": expected, "state": state,
            "installed_sha256": hashlib.sha256(actual_bytes).hexdigest(), "installed_mode": oct(actual_mode),
            "download_remaining": download.exists(), "stdout": stdout, "stderr": stderr,
        }
        reports.append(report)
        assert process.returncode == status, f"expected status {status}, got {process.returncode}"
        assert trace == expected, f"expected exact command prefix {steps}, got {trace}"
        assert diagnostic in stderr, f"missing causal diagnostic {diagnostic!r}: {stderr}"
        assert len(state) == 5 and state[0] == str(status), f"caller probe did not finish: {state}"
        expected_aster = str(destination) if status == 0 else "previous-aster"
        assert state[1] == expected_aster, f"unexpected caller ASTER: {state[1]}"
        assert f'declare -x ASTER="{expected_aster}"\n' in probes["exports"].read_text(), "ASTER not exported"
        assert state[2] == state[3], "caller shell flags changed"
        assert probes["before-options"].read_bytes() == probes["after-options"].read_bytes(), "caller options changed"
        assert probes["before-trap"].read_bytes() == probes["after-trap"].read_bytes(), "caller EXIT trap changed"
        assert state[4] == "no" and probes["caller-trap"].read_text() == "caller exit\n", "caller trap ran early or was lost"
        assert actual_bytes == expected_bytes and actual_mode == expected_mode, "unexpected destination bytes or mode"
        assert download.exists() != cleanup, "unexpected download cleanup state"
        if installed == "old":
            assert not any(e["name"] == "install" for e in trace), "payload installed after verification failure"
        report["outcome"] = "passed"
    except Exception as exc:
        raise AssertionError(f"{name}: {exc}") from exc
    finally:
        for path in files:
            path.unlink(missing_ok=True)
        if download.exists():
            download.rmdir()
        for path in reversed(directories):
            path.rmdir()


directory = root / (".cli-download-fixtures-" + uuid.uuid4().hex)
directory.mkdir()
extraction_controls = []
try:
    heading = "## Download and verify the CLI\n"
    fence = "```bash\nprintf fixture\n```\n"
    for name, text in [
        ("missing-section", ""), ("missing-fence", heading),
        ("ambiguous-fence", heading + fence + fence),
        ("ambiguous-section", heading + fence + heading + fence),
        ("unclosed-fence", heading + "```bash\nprintf fixture\n"),
    ]:
        try:
            download_block(text, name)
        except AssertionError:
            extraction_controls.append(name)
        else:
            raise AssertionError(f"{name}: extractor accepted invalid document")

    for document in args.documents or [root / "docs/kubernetes.md"]:
        block = download_block(document.read_text(), str(document))
        start = len(reports)
        for tool in ("sha256sum", "shasum"):
            for os_name, arch, target in [
                ("Linux", "x86_64", "linux-amd64"), ("Linux", "aarch64", "linux-arm64"),
                ("Linux", "arm64", "linux-arm64"), ("Darwin", "x86_64", "darwin-amd64"),
                ("Darwin", "arm64", "darwin-arm64"),
            ]:
                run_case(directory, block, f"{tool}-{os_name}-{arch}", tools=(tool,),
                         platform=(os_name, arch), target=target, installed="new")
            for name, changes, stop, status, diagnostic in [
                ("asset-transfer-with-bytes", {"asset": {"status": 7}}, "asset", 7, ""),
                ("asset-transfer", {"asset": {"status": 7, "write": False}}, "asset", 7, ""),
                ("manifest-transfer-with-valid-body", {"manifest": {"status": 8}}, "manifest", 8, ""),
                ("missing-exact-entry", {"manifest": {"bytes": list(
                    f"{digest}  aster-{version}-linux-amd64.sig\n".encode())}}, "extract", 1, "Missing CLI checksum entry"),
                ("corrupt-payload", {"asset": {"bytes": list(b"corrupt\n")}, tool: {"status": 1}},
                 tool, 1, "checksum mismatch"),
                ("malformed-checksum", {"manifest": {"bytes": list(
                    f"not-a-digest  aster-{version}-linux-amd64\n".encode())}, tool: {"status": 1}},
                 tool, 1, "invalid checksum line"),
                ("extraction-with-valid-output", {"extract": {"status": 9}}, "extract", 9, ""),
            ]:
                run_case(directory, block, f"{tool}-{name}", tools=(tool,), changes=changes,
                         stop=stop, status=status, diagnostic=diagnostic)
        for step, code in [("os", 41), ("arch", 42), ("prepare", 43), ("allocate", 44)]:
            run_case(directory, block, f"{step}-failed", changes={step: {"status": code}},
                     stop=step, status=code)
        run_case(directory, block, "change-directory-failed",
                 changes={"extract": {"remove_download": True}, "cleanup-dir": {"status": 1}},
                 stop="cd", status=1, diagnostic="cd:")
        for tools in (("sha256sum",), ("shasum",)):
            run_case(directory, block, f"{tools[0]}-install-failed",
                     tools=tools, changes={"install": {"status": 45, "partial": True}},
                     status=45, installed="partial")
        run_case(directory, block, "no-checksum-tool", tools=(), stop="checksum-tool", status=1,
                 diagnostic="Install sha256sum or shasum")
        run_case(directory, block, "both-tools-prefer-sha256sum", tools=("sha256sum", "shasum"), installed="new")
        run_case(directory, block, "both-tools-no-mismatch-fallback", tools=("sha256sum", "shasum"),
                 changes={"asset": {"bytes": list(b"corrupt\n")}, "sha256sum": {"status": 1}},
                 stop="sha256sum", status=1, diagnostic="checksum mismatch")
        for os_name, arch in [("FreeBSD", "x86_64"), ("Linux", "s390x"), ("Darwin", "aarch64"), ("", "x86_64")]:
            run_case(directory, block, f"unsupported-{os_name}-{arch}", platform=(os_name, arch),
                     stop="platform", status=1, diagnostic="Unsupported CLI platform")
        for value in ("", None):
            run_case(directory, block, f"version-{value!s}", cli_version=value, stop="version",
                     status=1, diagnostic="Set CLI_VERSION")
        for step, changes, cleanup_status in [
            ("files", {"cleanup-files": {"status": 57}}, 57),
            ("directory", {"cleanup-dir": {"status": 58}}, 58),
        ]:
            run_case(directory, block, f"cleanup-{step}-preserves-transfer-error",
                     changes={**changes, "asset": {"status": 7}}, stop="asset", status=7, cleanup=False)
            run_case(directory, block, f"cleanup-{step}-after-install",
                     changes=changes, status=cleanup_status, installed="new", cleanup=False)
        run_case(directory, block, "caller-options-success", caller_options=True, installed="new")
        run_case(directory, block, "caller-options-failure", caller_options=True,
                 changes={"asset": {"status": 7}}, stop="asset", status=7)
        print(f"{document}: {len(reports) - start} local CLI download controls passed")
    print(f"CLI download fail-closed checks passed ({len(reports)} execution cases, "
          f"{len(extraction_controls)} extraction controls).")
finally:
    directory.rmdir()
    if args.results:
        args.results.write_text(json.dumps({
            "cases": reports, "extraction_controls": extraction_controls,
            "bash": bash, "awk": awk, "python": python,
        }, indent=2) + "\n")
PY
