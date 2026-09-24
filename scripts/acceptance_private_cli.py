"""Exercise private supervisor bootstrap through the installed CLI layout."""

import argparse
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tempfile
import uuid


PI_FIXTURE = r'''#!/bin/sh
set -eu
case "${1:-}" in
  --version) printf 'pi fixture 1.0.0\n'; exit 0 ;;
  --help)
    cat <<'HELP'
Options: --mode --tools --model --thinking --session
HELP
    exit 0
    ;;
esac
cat >/dev/null
cat <<'EVENTS'
{"type":"session","id":"00000000-0000-4000-8000-000000000001"}
{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_start","message":{"role":"assistant","content":[]}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"}}
{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"},"toolResults":[]}
{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"private supervisor acceptance passed"}],"stopReason":"stop"}]}
EVENTS
'''
REPOSITORY = Path(__file__).resolve().parent.parent


def run_json(delegate, env, args, expected=(0,), timeout=45):
    result = subprocess.run(
        [str(delegate), *args], env=env, capture_output=True, text=True,
        timeout=timeout, check=False,
    )
    if result.returncode not in expected:
        raise RuntimeError(
            "delegate command failed: %r exit=%d stdout=%r stderr=%r"
            % (args, result.returncode, result.stdout[-2000:], result.stderr[-2000:])
        )
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise RuntimeError("delegate returned invalid JSON for %r: %s" % (args, error)) from error


def install_fixture(source, destination):
    if not source.is_file() or not os.access(source, os.X_OK):
        raise RuntimeError("required executable is missing: " + str(source))
    shutil.copyfile(source, destination)
    destination.chmod(0o700)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--delegate", type=Path, required=True)
    parser.add_argument("--runner", type=Path, required=True)
    parser.add_argument("--pueue", type=Path, required=True)
    parser.add_argument("--pueued", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    base = Path(tempfile.mkdtemp(prefix=".delegate-private-cli-", dir=Path.home())).resolve(strict=True)
    success = False
    env = dict(os.environ)
    private_config = base / "state" / ".supervisor" / "pueue.yml"
    private_pueue = base / "libexec" / "pueue"
    try:
        install_dir = base / "install"
        libexec = base / "libexec"
        home = base / "home"
        workspace = base / "workspace"
        state = base / "state"
        output = args.output.resolve()
        for directory in (install_dir, libexec, home, workspace, output.parent):
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)

        install_fixture(args.delegate.resolve(strict=True), install_dir / "delegate")
        install_fixture(args.runner.resolve(strict=True), install_dir / "delegate-run")
        install_fixture(args.pueue.resolve(strict=True), libexec / "pueue")
        install_fixture(args.pueued.resolve(strict=True), libexec / "pueued")
        pi = install_dir / "pi"
        pi.write_text(PI_FIXTURE)
        pi.chmod(0o700)

        brief = base / "brief.md"
        brief.write_text("Reply with the requested acceptance sentence.\n")
        brief.chmod(0o600)
        task_id = uuid.uuid4().hex
        env["HOME"] = str(home)
        env["PATH"] = str(install_dir) + os.pathsep + "/usr/bin:/bin"
        env["XDG_CONFIG_HOME"] = str(home / ".config")
        env["XDG_DATA_HOME"] = str(home / ".local" / "share")
        for key in ("DELEGATE_PUEUE_CONFIG", "PUEUE_CONFIG"):
            env.pop(key, None)

        dispatch = run_json(install_dir / "delegate", env, [
            "--root", str(state), "dispatch", "--provider", "pi:json",
            "--brief", str(brief), "--cwd", str(workspace),
            "--id", task_id, "--permission", "read-only", "--budget", "2m",
            "--json",
        ])
        if dispatch.get("task_id") != task_id or dispatch.get("task_record") != "created":
            raise RuntimeError("dispatch did not durably create the requested task")
        if dispatch.get("admission") != "admitted":
            raise RuntimeError("dispatch did not admit the bounded task")

        status = run_json(install_dir / "delegate", env, [
            "--root", str(state), "status", task_id, "--json",
        ])
        if status.get("task_id") != task_id:
            raise RuntimeError("status did not observe the dispatched task")

        collected = run_json(install_dir / "delegate", env, [
            "--root", str(state), "collect", task_id, "--watch", "30s", "--json",
        ], timeout=45)
        if collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError("delegate did not publish the successful fixture result")

        task_dir = state / "tasks" / task_id
        if not private_config.is_file():
            raise RuntimeError("delegate did not create its private supervisor config")
        meta = (task_dir / "meta.json").read_text()
        if str(private_pueue) not in meta or str(libexec / "pueued") not in meta:
            raise RuntimeError("task did not bind the supervisor pair shipped in private libexec")
        outcome = json.loads((task_dir / "outcome.json").read_text())
        if outcome.get("verdict") != "committed":
            raise RuntimeError("durable outcome does not confirm success")

        shutdown = subprocess.run(
            [str(private_pueue), "--config", str(private_config), "shutdown"],
            env=env, capture_output=True, text=True, timeout=15, check=False,
        )
        if shutdown.returncode != 0:
            raise RuntimeError("private acceptance supervisor did not shut down cleanly")
        output.write_text(json.dumps({
            "schema_version": 1,
            "status": "passed",
            "platform": platform.system(),
            "task_id": task_id,
            "checks": [
                "delegate found its private libexec supervisor pair without PATH discovery",
                "delegate created and started a private supervisor without global config",
                "delegate dispatch, status, and collect completed a bounded task",
                "the committed outcome is present in durable task state",
            ],
        }, sort_keys=True) + "\n")
        success = True
        print("PASS private CLI supervisor acceptance: " + str(args.output))
    finally:
        if not success and private_config.is_file():
            try:
                subprocess.run(
                    [str(private_pueue), "--config", str(private_config), "shutdown"],
                    env=env, capture_output=True, text=True, timeout=15, check=False,
                )
            except (OSError, subprocess.TimeoutExpired):
                pass
        if success:
            shutil.rmtree(base)
        else:
            print("Retained private acceptance state for diagnosis: " + str(base))


if __name__ == "__main__":
    main()
