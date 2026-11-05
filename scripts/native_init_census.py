#!/usr/bin/env python3
"""Inventory native delegate-run initialization sources and trace Linux startup."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.processes.operations import run_bounded_command


def bounded(argv: list[str], *, root: Path, environment: dict[str, str], timeout: int = 180):
    return run_bounded_command(argv, environment, cwd=root, capture=True, capture_stderr=True,
                               max_output=32 * 1024 * 1024, timeout=timeout)


def checked(argv: list[str], *, root: Path, environment: dict[str, str]) -> bytes:
    result = bounded(argv, root=root, environment=environment)
    if result.exit_code != 0:
        raise RuntimeError(f"native census command failed ({argv[0]}): {result.stderr.decode(errors='replace')}")
    return result.output


def packages(data: bytes) -> list[dict[str, object]]:
    decoder = json.JSONDecoder()
    remaining = data.decode()
    selected = []
    while remaining.strip():
        record, end = decoder.raw_decode(remaining.lstrip())
        if not isinstance(record, dict) or not isinstance(record.get("ImportPath"), str):
            raise ValueError("native go-list package record is malformed")
        selected.append(record)
        if len(selected) > 4096:
            raise ValueError("native go-list package count exceeds census bound")
        remaining = remaining.lstrip()[end:]
    return selected


def process_events(trace: str) -> list[str]:
    """Keep native process/exec attempts; Go's CLONE_THREAD births are threads."""
    pending = {}
    events = []
    for line in trace.splitlines():
        match = re.fullmatch(r"(\d+)\s+(.*)", line)
        if match is None:
            continue
        pid, body = match.groups()
        if "<unfinished ...>" in body:
            pending[pid] = body.replace("<unfinished ...>", "")
            continue
        resumed = re.match(r"<\.\.\. \w+ resumed>(.*)", body)
        if resumed is not None:
            if pid not in pending:
                raise ValueError("native process trace has unmatched resumed syscall")
            body = pending.pop(pid) + resumed.group(1)
        syscall = re.match(r"(execveat|execve|fork|vfork|clone3|clone)\(", body)
        if syscall is None:
            continue
        if syscall.group(1) in {"clone", "clone3"} and "CLONE_THREAD" in body:
            continue
        events.append(pid + " " + body)
    if pending:
        raise ValueError("native process trace has incomplete syscalls")
    return events


def trace_linux(root: Path, output: Path, binary: Path, environment: dict[str, str]) -> dict[str, object]:
    executable = shutil.which("strace", path=environment.get("PATH"))
    if executable is None:
        return {"status": "unverified", "reason": "native strace unavailable"}
    trace = output / "startup.strace"
    selected = dict(environment)
    selected["GODEBUG"] = "inittrace=1"
    result = bounded([executable, "-f", "-qq", "-e", "trace=process", "-o", str(trace), "--",
                      str(binary), "--supervisor-repair-editor"], root=root, environment=selected, timeout=30)
    (output / "startup.inittrace").write_bytes(result.stderr)
    if result.exit_code != 1:
        raise RuntimeError("native malformed editor did not exit at its first admission check")
    events = process_events(trace.read_text())
    if len(events) != 1 or " execve(" not in events[0]:
        raise RuntimeError("native initialization attempted an additional process/exec; inspect startup.strace")
    if b"init " not in result.stderr:
        raise RuntimeError("native inittrace did not report executed dependency initializers")
    return {"status": "passed", "process_events": events,
            "scope": "Observed native malformed-editor startup only; no all-branch effects claim."}


def verify_sources(report: dict[str, object]) -> None:
    sources = report.get("sources")
    if not isinstance(sources, list):
        raise ValueError("native source census omitted its source inventory")
    for source in sources:
        if not isinstance(source, dict) or not isinstance(source.get("path"), str) or not isinstance(source.get("sha256"), str):
            raise ValueError("native source inventory is malformed")
        if hashlib.sha256(Path(source["path"]).read_bytes()).hexdigest() != source["sha256"]:
            raise RuntimeError("selected source changed during native census; rerun on stable source")


def census(root: Path, output: Path, environment: dict[str, str]) -> dict[str, object]:
    host = checked(["go", "env", "GOHOSTOS", "GOHOSTARCH", "GOOS", "GOARCH", "GOVERSION", "CGO_ENABLED"], root=root, environment=environment).decode().splitlines()
    expected_os = {"Linux": "linux", "Darwin": "darwin"}.get(platform.system())
    if len(host) != 6 or host[0] != expected_os or host[:2] != host[2:4]:
        raise RuntimeError("native init census refuses cross-host metadata")
    metadata = checked(["go", "list", "-deps", "-compiled", "-export", "-json", "./cmd/delegate-run"], root=root, environment=environment)
    metadata_path = output / "native-packages.json"
    metadata_path.write_text(json.dumps({"host_os": host[0], "host_arch": host[1], "packages": packages(metadata)}))
    facts = checked(["go", "run", "./cmd/maintainability", "--native-init-census", str(metadata_path)], root=root, environment=environment)
    report = json.loads(facts)
    if not isinstance(report, dict):
        raise ValueError("native source census did not return an object")
    binary = output / "delegate-run"
    checked(["go", "build", "-o", str(binary), "./cmd/delegate-run"], root=root, environment=environment)
    report["toolchain"] = {"version": host[4], "cgo_enabled": host[5]}
    report["binary_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
    report["dynamic_startup"] = trace_linux(root, output, binary, environment) if host[0] == "linux" else {
        "status": "unverified", "reason": "Darwin syscall tracing not implemented; no privileged/SIP changes attempted."}
    verify_sources(report)
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    root = Path(__file__).resolve().parent.parent
    try:
        report = census(root, output, dict(os.environ))
        (output / "census.json").write_text(json.dumps(report, indent=2) + "\n")
        if report.get("type_errors"):
            raise RuntimeError("native source type resolution is incomplete; inspect census.json")
    except (OSError, RuntimeError, ValueError) as error:
        print(f"native init census: {error}", file=sys.stderr)
        return 1
    print(f"native dependency initialization SOURCE census: {output / 'census.json'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
