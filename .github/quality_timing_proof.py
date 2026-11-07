"""Draft isolated CI timing proof. Run from the final checkout root, once."""

from __future__ import annotations

import argparse
from collections.abc import Mapping, Sequence
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import platform
import re
import shutil
import statistics
import sys
import time


BASE = "fe8448ede0b80348559293d6f7349ee71635c024"
OVERLAY_PATHS = (
    "internal/app/control_environment_test.go",
    "internal/app/recorded_environment_runner_fixture_test.go",
)
TESTS = (
    "TestContinuationMissingNativeSessionFailsClosedAtLaunch",
    "TestCollectWatchReevaluatesOfferAfterUserStopInSameWatch",
    "TestCollectWatchPendingPublicationReturnsOnlyAfterItsBound",
)
TEST_FLAGS = ("-test.run=^(" + "|".join(TESTS) + ")$", "-test.count=1", "-test.timeout=30m", "-test.v")
PAIR_ORDER = (("before", "after"), ("after", "before"), ("before", "after"))
GO_ENV_FACTS = ("GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOEXPERIMENT", "GOAMD64")


def validate_base(base: str) -> None:
    if re.fullmatch(r"[0-9a-f]{40}", base) is None or base != BASE:
        raise ValueError("base must be the exact approved 40-hex commit")


def validate_source_difference(before: Mapping[str, str], after: Mapping[str, str]) -> None:
    if before.keys() != after.keys():
        raise ValueError("effective source inventories differ")
    changed = {path for path in before if before[path] != after[path]}
    if changed != set(OVERLAY_PATHS):
        raise ValueError("effective sources must differ at exactly the two approved test paths")


def validate_top_level_passes(output: bytes) -> dict[str, float]:
    text = output.decode("utf-8")
    started = re.findall(r"^=== RUN\s+(\w+)$", text, re.MULTILINE)
    passed = re.findall(r"^--- PASS: (\w+) \(([0-9]+(?:\.[0-9]+)?)s\)$", text, re.MULTILINE)
    if sorted(started) != sorted(TESTS) or sorted(name for name, _ in passed) != sorted(TESTS):
        raise ValueError("measurement must start and pass exactly the fixed three top-level tests")
    if re.search(r"^--- (?:FAIL|SKIP):", text, re.MULTILINE) or text.splitlines().count("PASS") != 1:
        raise ValueError("measurement contains a failed/skipped top-level test or lacks final PASS")
    return {name: float(seconds) for name, seconds in passed}


def require_paired_improvement(before: Sequence[float], after: Sequence[float]) -> dict[str, object]:
    if len(before) != 3 or len(after) != 3:
        raise ValueError("exactly three complete pairs are required")
    if not all(math.isfinite(value) and value > 0 for value in (*before, *after)):
        raise ValueError("elapsed times must be finite positive values")
    wins = [new < old for old, new in zip(before, after, strict=True)]
    before_median, after_median = statistics.median(before), statistics.median(after)
    if not all(wins) or after_median >= before_median:
        raise ValueError("timing proof requires every paired combined-wall win AND a lower median")
    return {"paired_combined_wall_wins": wins, "before_median_seconds": before_median,
            "after_median_seconds": after_median, "passed": True}


def source_paths(raw: bytes) -> tuple[str, ...]:
    records = raw.decode("utf-8").split("\0")
    if not records or records.pop() != "" or not records or len(records) != len(set(records)):
        raise ValueError("source inventory must be nonempty, unique and NUL-terminated")
    selected = []
    for value in records:
        path = PurePosixPath(value)
        if path.is_absolute() or ".." in path.parts or not (
                value.endswith(".go") or value in ("go.mod", "go.sum", ".go-version")):
            raise ValueError("unexpected source inventory path")
        if path.parts[0] not in ("bin", ".local"):
            selected.append(value)
    if not {"go.mod", "go.sum", ".go-version", *OVERLAY_PATHS}.issubset(selected):
        raise ValueError("required source inputs are missing")
    return tuple(sorted(selected))


def source_manifest(root: Path, paths: Sequence[str], replacements: Mapping[str, Path]) -> dict[str, str]:
    manifest: dict[str, str] = {}
    for relative in paths:
        path = replacements.get(relative, root / relative)
        if path.is_symlink():
            raise ValueError("source manifest refuses symlink inputs")
        # Only validated Go source, module metadata and compiler-pin DATA are
        # hashed. Candidate/installed executable contents are never read.
        manifest[relative] = hashlib.sha256(path.read_bytes()).hexdigest()
    return manifest


def write_record(path: Path, value: object) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def host_context() -> dict[str, object]:
    context: dict[str, object] = {"cpu_count": os.cpu_count(), "load_average": None}
    if sys.platform.startswith("linux"):
        try:
            context["load_average"] = list(os.getloadavg())
        except (AttributeError, OSError):
            context["load_average_reason"] = "unavailable"
    return context


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default=BASE)
    parser.add_argument("--output", type=Path, default=Path(".local/archive/quality-followups/ci-timing-proof/result"))
    args = parser.parse_args()
    validate_base(args.base)
    root = Path.cwd().resolve()
    if (root / ".go-version").read_text().strip() != "1.27.3":
        raise ValueError("proof requires the final checkout's Go 1.27.3 pin")
    sys.path.insert(0, str(root))
    from scripts.delegate_acceptance.directories.operations import DirectoryOwner
    from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, command_closure_unproven
    from scripts.delegate_acceptance.processes.operations import run_bounded_command

    environment = dict(os.environ)
    if environment.get("GOFLAGS"):
        raise ValueError("proof requires empty GOFLAGS; its value is not recorded")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    runtime, data = output / "runtime", output / "source-data"
    scratch_owner = DirectoryOwner()
    environment.update(GOTOOLCHAIN="local", GOMAXPROCS="2", GOCACHE=str(runtime / "cache"), TMPDIR=str(runtime / "tmp"))
    commands: list[dict[str, object]] = []
    warmups: list[dict[str, object]] = []
    measurements: list[dict[str, object]] = []
    all_test_invocations_passed = False
    state: dict[str, object] = {"status": "running", "base": args.base, "commands": commands,
                               "warmups": warmups, "measurements": measurements, "runtime_ancestor": str(runtime)}

    def execute(name: str, argv: list[str], cwd: Path, timeout: float) -> tuple[bytes, float]:
        record: dict[str, object] = {"name": name, "argv": argv, "cwd": str(cwd),
                                     "timeout_seconds": timeout, "actual_wait": False,
                                     "host_before": host_context()}
        commands.append(record)
        write_record(output / "result.json", state)
        started = time.perf_counter()
        try:
            result = run_bounded_command(argv, environment, max_output=8 * 1024 * 1024,
                                         capture=True, capture_stderr=True, timeout=timeout, cwd=cwd)
        except BaseException as error:
            record.update(elapsed_seconds=time.perf_counter() - started,
                          error_type=type(error).__name__, closure_unproven=command_closure_unproven(error),
                          host_after=host_context())
            if isinstance(error, BoundedCommandError):
                record.update(reason=error.reason.value, exit_code=error.exit_code,
                              actual_wait=error.exit_code is not None)
            write_record(output / "result.json", state)
            raise
        elapsed = time.perf_counter() - started
        record["host_after"] = host_context()
        (output / f"{name}.stdout").write_bytes(result.output)
        (output / f"{name}.stderr").write_bytes(result.stderr)
        record.update(exit_code=result.exit_code, elapsed_seconds=elapsed, actual_wait=True)
        write_record(output / "result.json", state)
        if result.exit_code != 0:
            raise RuntimeError(f"{name} failed; see captured command result")
        return result.output, elapsed

    try:
        scratch_owner.create("timing proof scratch", runtime, output)
        data.mkdir()
        for path in (runtime / "cache", runtime / "tmp"):
            path.mkdir()
        repository_root, _ = execute("repository-root", ["git", "rev-parse", "--show-toplevel"], root, 30)
        if Path(repository_root.decode("utf-8").strip()).resolve() != root:
            raise ValueError("invoke the proof from the final checkout root")
        head_output, _ = execute("checkout-head", ["git", "rev-parse", "HEAD"], root, 30)
        checkout_head = head_output.decode("ascii").strip()
        if re.fullmatch(r"[0-9a-f]{40}", checkout_head) is None:
            raise ValueError("checkout HEAD must be one full 40-hex commit")
        state["checkout_head"] = checkout_head
        kind, _ = execute("base-kind", ["git", "cat-file", "-t", args.base], root, 30)
        if kind.strip() != b"commit":
            raise ValueError("approved base is not an available commit; checkout must contain it")
        version, _ = execute("compiler", ["go", "version"], root, 30)
        if not version.decode("utf-8").startswith("go version go1.27.3 "):
            raise ValueError("selected compiler is not the exact Go 1.27.3 pin")
        facts_output, _ = execute("go-environment", ["go", "env", "-json", *GO_ENV_FACTS], root, 30)
        facts: object = json.loads(facts_output)
        if not isinstance(facts, dict) or set(facts) != set(GO_ENV_FACTS):
            raise ValueError("selected Go tool facts must contain exactly the requested keys")
        go_facts: dict[str, str] = {}
        for key in GO_ENV_FACTS:
            value = facts[key]
            if not isinstance(value, str):
                raise ValueError("selected Go tool facts must be strings")
            go_facts[key] = value
        state["metadata"] = {"compiler": version.decode("utf-8").strip(), "compiler_path": shutil.which("go"),
                             "go_facts": go_facts,
                             "system": platform.system(), "machine": platform.machine(), "gomaxprocs": 2,
                             "build_flags": ["-race", "-buildvcs=false", "-c"], "test_flags": TEST_FLAGS,
                             "cache_policy": "one fresh owned Go cache shared by both builds, two warmups and six measurements"}
        inventory, _ = execute("source-inventory", ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z",
                                                    "--", "*.go", "go.mod", "go.sum", ".go-version"], root, 30)
        paths = source_paths(inventory)
        after = source_manifest(root, paths, {})
        replacements: dict[str, Path] = {}
        for index, relative in enumerate(OVERLAY_PATHS):
            preimage, _ = execute(f"preimage-{index}", ["git", "show", f"{args.base}:{relative}"], root, 30)
            target = data / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(preimage)
            replacements[relative] = target
        before = source_manifest(root, paths, replacements)
        validate_source_difference(before, after)
        write_record(output / "source-inputs-before.json", before)
        write_record(output / "source-inputs-after.json", after)
        overlay = output / "before-overlay.json"
        write_record(overlay, {"Replace": {str(root / relative): str(path) for relative, path in replacements.items()}})
        binaries = {variant: runtime / f"{variant}-app.test" for variant in ("before", "after")}
        for variant in ("before", "after"):
            if source_manifest(root, paths, {}) != after:
                raise ValueError("current source changed before a build")
            argv = ["go", "test", "-race", "-buildvcs=false", "-c", "-o", str(binaries[variant])]
            if variant == "before":
                argv.append(f"-overlay={overlay}")
            execute(f"build-{variant}", [*argv, "./internal/app"], root, 900)
            if source_manifest(root, paths, {}) != after:
                raise ValueError("current source changed during a build")
        for variant in ("before", "after"):
            if source_manifest(root, paths, {}) != after:
                raise ValueError("current source changed before a warmup")
            stdout, wall = execute(f"warmup-{variant}", [str(binaries[variant]), *TEST_FLAGS], root / "internal/app", 1860)
            passed = validate_top_level_passes(stdout)
            warmups.append({"variant": variant, "elapsed_seconds": wall, "top_level_pass_seconds": passed})
            if source_manifest(root, paths, {}) != after:
                raise ValueError("current source changed during a warmup")
        elapsed: dict[str, list[float]] = {"before": [], "after": []}
        for pair, order in enumerate(PAIR_ORDER, start=1):
            pair_values: dict[str, float] = {}
            for variant in order:
                if source_manifest(root, paths, {}) != after:
                    raise ValueError("current source changed before a measurement")
                name = f"pair-{pair}-{variant}"
                stdout, wall = execute(name, [str(binaries[variant]), *TEST_FLAGS], root / "internal/app", 1860)
                passed = validate_top_level_passes(stdout)
                measurements.append({"pair": pair, "variant": variant, "elapsed_seconds": wall,
                                     "top_level_pass_seconds": passed})
                pair_values[variant] = wall
                if source_manifest(root, paths, {}) != after:
                    raise ValueError("current source changed during a measurement")
            for variant in elapsed:
                elapsed[variant].append(pair_values[variant])
        all_test_invocations_passed = True
        state["comparison"] = require_paired_improvement(elapsed["before"], elapsed["after"])
        state["status"] = "passed"
        return 0
    except BaseException as error:
        state.update(status="failed", error_type=type(error).__name__)
        if isinstance(error, ValueError):
            state["reason"] = str(error)
        raise
    finally:
        # A failed/interrupted test may leave descendants despite a direct Wait.
        # Retain its exact runtime ancestor. Only two warmups plus six passing
        # measured invocations
        # establish fixture cleanup for removal of all this job's scratch,
        # including when the timing criterion itself failed.
        try:
            if all_test_invocations_passed:
                scratch_owner.rollback()
            else:
                scratch_owner.release()
        except (OSError, RuntimeError):
            state.update(status="failed", cleanup_failure="owned scratch removal failed")
            raise
        finally:
            state["scratch_removed"] = not runtime.exists()
            state["runtime_retained"] = str(runtime) if runtime.exists() else None
            write_record(output / "result.json", state)


if __name__ == "__main__":
    raise SystemExit(main())
