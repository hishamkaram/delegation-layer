#!/usr/bin/env python3
"""Build and run the native editor's surviving-child counterfactual pair."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.processes.operations import run_bounded_command


CLOSE_LINE = "\tdefer func() { resultErr = errors.Join(resultErr, file.Close()) }()"
ENTRY = "\tif len(args) != 2 {"
LAUNCHER = r'''
// Counterfactual only: deliberately orphan one finite child, whose native
// adopter owns Wait. The native proof owns its authenticated release and exit.
// The bad mutant pins this file object so its finalizer cannot hide the leak.
var repairEditorCounterfactualLease *os.File

func repairEditorCounterfactualChild() error {
    base := filepath.Join(os.Getenv(repairRootEnv), privateSupervisorDirectory)
    if _, err := os.Lstat(filepath.Join(base, "abort")); !errors.Is(err, os.ErrNotExist) {
        return errors.Join(ErrConfiguration, err)
    }
    deadline, err := os.ReadFile(filepath.Join(base, "survivor-deadline"))
    if err != nil { return err }
    command := exec.Command(HELPER, "-test.run=^TestRepairEditorSurvivorFixture$", "-test.timeout=45s")
    command.Dir = base
    command.Env = []string{"GORACE=atexit_sleep_ms=0", "DELEGATE_REPAIR_SURVIVOR_CHILD=1",
        "DELEGATE_REPAIR_SURVIVOR_BASE="+base, "DELEGATE_REPAIR_SURVIVOR_DEADLINE="+string(deadline)}
    if _, descriptorErr := unix.FcntlInt(repairEditorFD, unix.F_GETFD, 0); descriptorErr == nil {
        command.ExtraFiles = []*os.File{os.NewFile(repairEditorFD, "counterfactual-inherited-admission")}
    } else if !errors.Is(descriptorErr, unix.EBADF) { return descriptorErr }
    if err = command.Start(); err != nil { return err }
    child, err := lifetime.ObserveStartedProcess(command.Process.Pid)
    if err != nil { return err }
    editor, err := lifetime.ObserveProcess(os.Getpid())
    if err != nil { return err }
    record, err := task.MarshalCanonical(struct {
        PID int `json:"pid"`
        Token string `json:"token"`
        EditorPID int `json:"editor_pid"`
        EditorToken string `json:"editor_token"`
    }{PID: child.PID(), Token: child.StartToken(), EditorPID: editor.PID(), EditorToken: editor.StartToken()})
    if err != nil { return err }
    if err = os.WriteFile(filepath.Join(base, "survivor.json"), record, 0o600); err != nil { return err }
    return command.Process.Release()
}
'''


def overlay_source(source: str, *, helper: Path, retain: bool) -> str:
    if source.count(CLOSE_LINE) != 1 or source.count(ENTRY) != 1:
        raise RuntimeError("editor boundary changed; counterfactual needs explicit review")
    if retain:
        source = source.replace(CLOSE_LINE, "\trepairEditorCounterfactualLease = file // Counterfactual: retain the validated inherited descriptor.")
    source = source.replace('"os"', '"os"\n "os/exec"\n'
                            '"github.com/hishamkaram/delegation-layer/internal/lifetime"', 1)
    source = source.replace(ENTRY, "\tif err := repairEditorCounterfactualChild(); err != nil { return 1,true }\n" + ENTRY)
    return source + LAUNCHER.replace("HELPER", json.dumps(str(helper)))


def command(argv: list[str], *, root: Path, environment: dict[str, str], timeout: int = 180):
    return run_bounded_command(argv, environment, cwd=root, capture=True, capture_stderr=True,
                               max_output=8 * 1024 * 1024, timeout=timeout)


def build(root: Path, output: Path, environment: dict[str, str]) -> dict[str, str]:
    source_path = root / "internal/pueue/internal/control/repair_editor.go"
    source = source_path.read_text()
    helper, control = output / "lifetime.fixture", output / "control.fixture"
    for target, package in ((helper, "./internal/lifetime"), (control, "./internal/pueue/internal/control")):
        result = command(["go", "test", "-race", "-c", "-o", str(target), package], root=root, environment=environment)
        if result.exit_code != 0:
            raise RuntimeError(f"fixture build failed: {result.stderr.decode(errors='replace')}")
    paths = {"helper": str(helper), "control": str(control)}
    for name, retain in (("keep", True), ("close", False)):
        alternate = output / f"{name}-editor.go"
        alternate.write_text(overlay_source(source, helper=helper, retain=retain))
        overlay = output / f"{name}-overlay.json"
        overlay.write_text(json.dumps({"Replace": {str(source_path): str(alternate)}}))
        binary = output / f"{name}-delegate-run"
        result = command(["go", "build", "-overlay=" + str(overlay), "-o", str(binary), "./cmd/delegate-run"],
                         root=root, environment=environment)
        if result.exit_code != 0:
            raise RuntimeError(f"{name} editor build failed: {result.stderr.decode(errors='replace')}")
        paths[name] = str(binary)
    paths["editor_source_sha256"] = hashlib.sha256(source.encode()).hexdigest()
    return paths


def require_counterfactual_result(name: str, exit_code: int, diagnostic: bytes) -> None:
    expected_code = {"keep": 1, "close": 0}[name]
    cleanup_faults = (b"retained native admission diagnostics", b"native admission directory finalization")
    if exit_code != expected_code or any(marker in diagnostic for marker in cleanup_faults):
        raise RuntimeError(f"{name} counterfactual or actor/path cleanup failed")
    if name == "keep" and b"fresh actual RootAdmission EX=busy, want acquired" not in diagnostic:
        raise RuntimeError("keep counterfactual did not fail the intended root EX closure oracle")
    if name == "close" and b"surviving native child is live after CLI exit" not in diagnostic:
        raise RuntimeError("close counterfactual did not prove live child plus root EX acquisition")


def prove(root: Path, output: Path, paths: dict[str, str], *, pueue: Path, environment: dict[str, str]) -> None:
    for name, inherited in (("keep", "true"), ("close", "false")):
        selected = dict(environment)
        selected.update({"DELEGATE_REPAIR_SURVIVOR_PROOF": "1", "DELEGATE_REPAIR_SURVIVOR_EXPECT_FD": inherited,
                         "DELEGATE_SHUTDOWN_LEASE_CONTROL_OWNER": paths[name],
                         "DELEGATE_SHUTDOWN_LEASE_CONTROL_FIXTURE": paths["control"],
                         "DELEGATE_SHUTDOWN_LEASE_PUEUE": str(pueue)})
        result = command([paths["helper"], "-test.run=^TestRepairEditorSurvivorNativeProof$", "-test.v", "-test.timeout=60s"],
                         root=root / "internal/lifetime", environment=selected, timeout=75)
        diagnostic = result.output + result.stderr
        (output / f"{name}-native.log").write_bytes(diagnostic)
        try:
            require_counterfactual_result(name, result.exit_code, diagnostic)
        except RuntimeError as error:
            raise RuntimeError(f"{error}; see {output / (name + '-native.log')}") from error


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--pueue", type=Path, required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    environment = dict(os.environ)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    try:
        paths = build(root, output, environment)
        (output / "builds.json").write_text(json.dumps(paths, indent=2) + "\n")
        prove(root, output, paths, pueue=args.pueue.resolve(), environment=environment)
    except (OSError, RuntimeError) as error:
        print(f"repair editor counterfactual: {error}", file=sys.stderr)
        return 1
    print(f"repair editor counterfactual pair and actor closure passed: {output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
