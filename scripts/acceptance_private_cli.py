"""Exercise private supervisor reuse across two installed CLI layouts."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shlex
import shutil
import subprocess
import tempfile
import time
import uuid


# Leave time for the two-minute task budget and supervisor stop observation.
TASK_BUDGET = "2m"
COLLECT_WATCH = "150s"
COLLECT_TIMEOUT_SECONDS = 165


PI_FIXTURE = r'''#!/bin/sh
set -eu
case "${1:-}" in
  --list-models)
    printf 'provider model context max-out thinking images\nopenai gpt-4.1 128000 8192 enabled enabled\n'
    exit 0 ;;
  --version) printf 'pi fixture 1.0.0\n'; exit 0 ;;
  --help)
    cat <<'HELP'
Options: --mode --tools --model --thinking --session --list-models
HELP
    exit 0
    ;;
esac
prompt=$(cat)
case "$prompt" in
  *DELEGATE_PRIVATE_UPGRADE_WAIT*)
    while [ ! -e __GATE_PATH__ ]; do sleep 0.05; done
    ;;
esac
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


def install_wrapper(destination, executable, marker, identity):
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    destination.write_text(
        "#!/bin/sh\n"
        "set -eu\n"
        "printf '%s\\n' started >> " + shlex.quote(str(marker)) + "\n"
        "exec " + shlex.quote(str(executable)) + " \"$@\"\n"
        "# " + identity + "\n"
    )
    destination.chmod(0o700)


def install_pi_fixture(install_dir, gate_file):
    pi = install_dir / "pi"
    pi.write_text(PI_FIXTURE.replace("__GATE_PATH__", shlex.quote(str(gate_file))))
    pi.chmod(0o700)


def pueue_snapshot(client, config, env):
    result = subprocess.run(
        [str(client), "--config", str(config), "status", "--json"],
        env=env, capture_output=True, text=True, timeout=10, check=False,
    )
    if result.returncode != 0:
        raise RuntimeError("isolated pueue status failed: " + result.stderr[-1000:])
    return json.loads(result.stdout)


def wait_for_pueue_state(client, config, env, label, expected):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        snapshot = pueue_snapshot(client, config, env)
        for row in snapshot.get("tasks", {}).values():
            if row.get("label") == label:
                status = row.get("status", {})
                state = next(iter(status), "unknown")
                if state == expected:
                    return row
                if state == "Done":
                    raise RuntimeError("fixture task ended before reaching " + expected)
        time.sleep(0.05)
    raise RuntimeError("timed out waiting for isolated pueue task state " + expected)


def wait_for_queued_label_prefix(client, config, env, group, prefix, excluded_labels, process):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if process.poll() is not None:
            stdout, stderr = process.communicate()
            raise RuntimeError(
                "models command exited before its inspection worker queued: "
                "exit=%d stdout=%r stderr=%r"
                % (process.returncode, stdout[-2000:], stderr[-2000:])
            )
        snapshot = pueue_snapshot(client, config, env)
        for row in snapshot.get("tasks", {}).values():
            label = row.get("label") or ""
            if label not in excluded_labels and label.startswith(prefix) and row.get("group") == group:
                state = pueue_state(row)
                if state == "Queued":
                    return row
                if state == "Done":
                    raise RuntimeError("inspection worker completed despite its queue blocker")
        time.sleep(0.05)
    raise RuntimeError("timed out waiting for queued inspection worker")


def pueue_command(client, config, env, args, timeout=15):
    result = subprocess.run(
        [str(client), "--config", str(config), *args],
        env=env, capture_output=True, text=True, timeout=timeout, check=False,
    )
    if result.returncode != 0:
        raise RuntimeError("isolated pueue command failed: %r: %s" % (args, result.stderr[-1000:]))
    return result.stdout


def task_row_by_label(snapshot, label):
    for row in snapshot.get("tasks", {}).values():
        if row.get("label") == label:
            return row
    return None


def assert_state_supervisor_pair(state, bundled_directory):
    supervisor = state / ".supervisor"
    manifest_path = supervisor / "pueue-pair.json"
    if not manifest_path.is_file() or manifest_path.is_symlink():
        raise RuntimeError("state-root supervisor pair manifest is missing or unsafe")
    try:
        manifest = json.loads(manifest_path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        raise RuntimeError("state-root supervisor pair manifest is unreadable: " + str(error)) from error
    client_digest = manifest.get("client_sha256", "")
    daemon_digest = manifest.get("daemon_sha256", "")
    pair_digest = hashlib.sha256((client_digest + "\n" + daemon_digest).encode()).hexdigest()
    if (
        manifest.get("schema_version") != 1
        or manifest.get("pair_sha256") != pair_digest
        or len(client_digest) != 64
        or len(daemon_digest) != 64
    ):
        raise RuntimeError("state-root supervisor pair manifest has an invalid identity")
    pair_directory = supervisor / ("pueue-pair-" + pair_digest)
    for name, expected_digest in (("pueue", client_digest), ("pueued", daemon_digest)):
        installed = pair_directory / name
        bundled = bundled_directory / name
        if (
            installed.is_symlink()
            or not installed.is_file()
            or hashlib.sha256(installed.read_bytes()).hexdigest() != expected_digest
            or hashlib.sha256(bundled.read_bytes()).hexdigest() != expected_digest
        ):
            raise RuntimeError("state-root pair does not match its packaged supervisor member " + name)
    return pair_directory


def pueue_state(row):
    return next(iter(row.get("status", {})), "unknown")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--delegate", type=Path, required=True)
    parser.add_argument("--runner", type=Path, required=True)
    parser.add_argument("--upgraded-delegate", type=Path)
    parser.add_argument("--upgraded-runner", type=Path)
    parser.add_argument("--pueue", type=Path, required=True)
    parser.add_argument("--pueued", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    legacy_mode = args.upgraded_delegate is not None and args.upgraded_runner is not None
    queued_runner_check = (
        "the legacy CLI queued a task whose runner path pointed into its install"
        if legacy_mode else
        "the CLI queued a task whose runner path pointed to the durable state root"
    )
    inspection_runner_check = (
        "the upgraded CLI preserved an ambiguous legacy inspection runner and completed it before cleanup"
        if legacy_mode else
        "the upgraded CLI retained and completed an explicitly managed state-root inspection runner"
    )
    base = Path(tempfile.mkdtemp(prefix=".delegate-private-cli-", dir=Path.home())).resolve(strict=True)
    success = False
    inspection_process = None
    env = {}
    private_config = base / "state" / ".supervisor" / "pueue.yml"
    gate = base / "blocker-gate"
    try:
        old_install = base / "old" / "install"
        old_libexec = base / "old" / "libexec"
        upgraded_install = base / "upgraded" / "install"
        upgraded_libexec = base / "upgraded" / "libexec"
        provider_bin = base / "provider-bin"
        home = base / "home"
        workspace = base / "workspace"
        state = base / "state"
        output = args.output.resolve()
        for directory in (old_install, old_libexec, upgraded_install, upgraded_libexec, provider_bin, home, workspace, output.parent):
            directory.mkdir(mode=0o700, parents=True, exist_ok=True)

        delegate_binary = args.delegate.resolve(strict=True)
        runner_binary = args.runner.resolve(strict=True)
        upgraded_delegate_binary = (args.upgraded_delegate or args.delegate).resolve(strict=True)
        upgraded_runner_binary = (args.upgraded_runner or args.runner).resolve(strict=True)
        pueue_binary = args.pueue.resolve(strict=True)
        pueued_binary = args.pueued.resolve(strict=True)
        install_fixture(delegate_binary, old_install / "delegate")
        install_fixture(runner_binary, old_install / "delegate-run")
        install_fixture(upgraded_delegate_binary, upgraded_install / "delegate")
        install_fixture(upgraded_runner_binary, upgraded_install / "delegate-run")
        old_daemon_marker = base / "old-daemon-starts"
        upgraded_daemon_marker = base / "upgraded-daemon-starts"
        install_wrapper(old_libexec / "pueue", pueue_binary, base / "old-client-starts", "old-install")
        install_wrapper(old_libexec / "pueued", pueued_binary, old_daemon_marker, "old-install")
        install_wrapper(upgraded_libexec / "pueue", pueue_binary, base / "upgraded-client-starts", "upgraded-install")
        install_wrapper(upgraded_libexec / "pueued", pueued_binary, upgraded_daemon_marker, "upgraded-install")
        install_pi_fixture(provider_bin, gate)

        brief = base / "brief.md"
        brief.write_text("Reply with the requested acceptance sentence.\n")
        brief.chmod(0o600)
        env.update({
            "HOME": str(home),
            "PATH": str(provider_bin) + os.pathsep + str(old_install) + os.pathsep + str(old_libexec) + os.pathsep + "/usr/bin:/bin",
            "XDG_CONFIG_HOME": str(home / ".config"),
            "XDG_DATA_HOME": str(home / ".local" / "share"),
            "LANG": "C.UTF-8",
            "LC_ALL": "C.UTF-8",
        })

        task_id = uuid.uuid4().hex
        dispatch = run_json(old_install / "delegate", env, [
            "--root", str(state), "dispatch", "--provider", "pi:json",
            "--brief", str(brief), "--cwd", str(workspace),
            "--id", task_id, "--permission", "read-only", "--budget", TASK_BUDGET,
            "--json",
        ])
        if dispatch.get("task_id") != task_id or (
            "task_record" in dispatch and dispatch.get("task_record") != "created"
        ):
            raise RuntimeError(
                "dispatch did not durably create the requested task: response=%r expected_id=%s"
                % (dispatch, task_id)
            )
        if dispatch.get("admission") != "admitted":
            raise RuntimeError("dispatch did not admit the bounded task")

        first_status = run_json(old_install / "delegate", env, [
            "--root", str(state), "status", task_id, "--json",
        ])
        if first_status.get("task_id") != task_id:
            raise RuntimeError("status did not observe the dispatched task")

        collected = run_json(old_install / "delegate", env, [
            "--root", str(state), "collect", task_id, "--watch", COLLECT_WATCH, "--json",
        ], timeout=COLLECT_TIMEOUT_SECONDS)
        if collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError("delegate did not publish the successful fixture result")

        task_dir = state / "tasks" / task_id
        if not private_config.is_file():
            raise RuntimeError("delegate did not create its private supervisor config")
        first_meta = json.loads((task_dir / "meta.json").read_text())
        if legacy_mode:
            initial_daemon = old_libexec / "pueued"
            if str(old_libexec / "pueue") not in json.dumps(first_meta) or str(initial_daemon) not in json.dumps(first_meta):
                raise RuntimeError("legacy task did not bind the supervisor pair shipped in private libexec")
        else:
            initial_pair_directory = assert_state_supervisor_pair(state, old_libexec)
            initial_daemon = initial_pair_directory / "pueued"
            if str(initial_pair_directory / "pueue") not in json.dumps(first_meta) or str(initial_daemon) not in json.dumps(first_meta):
                raise RuntimeError("task did not bind the verified state-root supervisor pair")
        outcome = json.loads((task_dir / "outcome.json").read_text())
        if outcome.get("verdict") != "committed":
            raise RuntimeError("durable outcome does not confirm success")

        identity_path = state / ".supervisor" / "daemon.identity.json"
        old_identity_data = identity_path.read_bytes()
        old_identity = json.loads(old_identity_data)
        upgraded_daemon = upgraded_libexec / "pueued"
        upgraded_digest = hashlib.sha256(upgraded_daemon.read_bytes()).hexdigest()
        if old_identity.get("executable") != str(initial_daemon):
            raise RuntimeError("initial launch provenance does not name its selected private daemon")
        old_digest = hashlib.sha256((old_libexec / "pueued").read_bytes()).hexdigest()
        if old_identity.get("sha256") != old_digest:
            raise RuntimeError("initial launch provenance has the wrong daemon hash")
        if old_identity.get("executable") == str(upgraded_daemon) or old_identity.get("sha256") == upgraded_digest:
            raise RuntimeError("acceptance layouts do not reproduce daemon executable hash drift")
        if not old_daemon_marker.is_file() or old_daemon_marker.read_text().splitlines() != ["started"]:
            raise RuntimeError("initial CLI did not start exactly one old-layout daemon")

        inspection_group = "delegation-inspection-" + dispatch["root_id"]
        inspection_prefix = inspection_group + "-"
        models_args = [
            "--root", str(state), "models", "--provider", "pi:json",
            "--cwd", str(workspace), "--json",
        ]
        initial_models = run_json(old_install / "delegate", env, models_args)
        if initial_models.get("status") != "available" or [
            model.get("id") for model in initial_models.get("models", [])
        ] != ["openai/gpt-4.1"]:
            raise RuntimeError("legacy CLI could not complete its initial model inspection")
        # Keep the private group occupied while an old-release task is queued.
        # This reproduces a package upgrade with an existing task command that
        # still points into the old installation.
        blocker = base / "queue-blocker"
        blocker.write_text("#!/bin/sh\nset -eu\nwhile [ ! -e " + shlex.quote(str(gate)) + " ]; do sleep 0.05; done\n")
        blocker.chmod(0o700)
        old_pueue = old_libexec / "pueue"
        pueue_command(old_pueue, private_config, env, [
            "add", "--escape", "--label", "delegate-upgrade-blocker", "--print-task-id", "--", str(blocker),
        ])
        blocker_row = wait_for_pueue_state(old_pueue, private_config, env, "delegate-upgrade-blocker", "Running")
        if pueue_state(blocker_row) != "Running":
            raise RuntimeError("queue blocker did not occupy the private default group")

        queued_task_id = uuid.uuid4().hex
        queued_dispatch = run_json(old_install / "delegate", env, [
            "--root", str(state), "dispatch", "--provider", "pi:json",
            "--brief", str(brief), "--cwd", str(workspace),
            "--id", queued_task_id, "--permission", "read-only", "--budget", TASK_BUDGET,
            "--json",
        ])
        if queued_dispatch.get("task_id") != queued_task_id or queued_dispatch.get("admission") != "admitted":
            raise RuntimeError("legacy CLI did not admit the task that should remain queued")
        queued_label = "delegate:" + dispatch["root_id"] + ":" + queued_task_id
        queued_row = task_row_by_label(pueue_snapshot(old_pueue, private_config, env), queued_label)
        if queued_row is None or pueue_state(queued_row) != "Queued":
            raise RuntimeError("legacy task did not remain queued behind the blocker")
        old_queued_command = queued_row.get("original_command", "")
        old_runner_path = str(old_install / "delegate-run")
        initial_runner_digest = hashlib.sha256(runner_binary.read_bytes()).hexdigest()
        stable_runner_path = str(state / ".supervisor" / ("delegate-run-" + initial_runner_digest))
        if legacy_mode and old_runner_path not in old_queued_command:
            raise RuntimeError("legacy queued command does not reference the removable install path")
        if not legacy_mode and stable_runner_path not in old_queued_command:
            raise RuntimeError("current CLI did not submit the durable state-root runner path")
        previous_inspection_labels = {
            row.get("label")
            for row in pueue_snapshot(old_libexec / "pueue", private_config, env).get("tasks", {}).values()
            if row.get("group") == inspection_group and (row.get("label") or "").startswith(inspection_prefix)
        }

        # Queue an old-release inspection worker behind its private group
        # blocker. Start this after the ordinary dispatch has been admitted so
        # the two foreground CLI operations never race over supervisor calls.
        inspection_blocker = base / "inspection-queue-blocker"
        inspection_blocker.write_text(
            "#!/bin/sh\nset -eu\nwhile [ ! -e " + shlex.quote(str(gate)) + " ]; do sleep 0.05; done\n"
        )
        inspection_blocker.chmod(0o700)
        pueue_command(old_libexec / "pueue", private_config, env, [
            "add", "--escape", "--group", inspection_group, "--label",
            "inspection-upgrade-blocker", "--print-task-id", "--", str(inspection_blocker),
        ])
        wait_for_pueue_state(
            old_libexec / "pueue", private_config, env, "inspection-upgrade-blocker", "Running",
        )
        inspection_process = subprocess.Popen(
            [str(old_install / "delegate"), *models_args],
            env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        inspection_row = wait_for_queued_label_prefix(
            old_libexec / "pueue", private_config, env, inspection_group,
            inspection_prefix, previous_inspection_labels, inspection_process,
        )
        inspection_label = inspection_row["label"]
        inspection_task_id = inspection_label[len(inspection_prefix):]
        if len(inspection_task_id) != 32 or any(char not in "0123456789abcdef" for char in inspection_task_id):
            raise RuntimeError("queued inspection label does not contain a valid task identity")

        # Let the upgraded public CLI recover the queued task while the legacy
        # install is still present, so it can prove the saved runner is managed
        # and move the command to the new immutable state-root runner.
        env["PATH"] = (
            str(provider_bin) + os.pathsep + str(upgraded_install) + os.pathsep
            + str(upgraded_libexec) + os.pathsep + str(old_install) + os.pathsep
            + str(old_libexec) + os.pathsep + "/usr/bin:/bin"
        )
        repaired_status = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "status", queued_task_id, "--json",
        ])
        if repaired_status.get("task_id") != queued_task_id:
            raise RuntimeError("upgraded status did not observe the queued legacy task")
        migrated_row = task_row_by_label(pueue_snapshot(upgraded_libexec / "pueue", private_config, env), queued_label)
        if migrated_row is None or pueue_state(migrated_row) != "Queued":
            raise RuntimeError("upgraded CLI lost the legacy queued task during status recovery")
        migrated_command = migrated_row.get("original_command", "")
        upgraded_digest = hashlib.sha256(upgraded_runner_binary.read_bytes()).hexdigest()
        state_runner_path = str(state / ".supervisor" / ("delegate-run-" + upgraded_digest))
        if state_runner_path not in migrated_command:
            queued_meta = json.loads((state / "tasks" / queued_task_id / "meta.json").read_text())
            raise RuntimeError(
                "upgraded status did not migrate the queue to its immutable state-root runner: "
                "status=%r runner=%r ownership=%r expected=%r command=%r"
                % (
                    repaired_status,
                    queued_meta.get("runner_executable"),
                    queued_meta.get("runner_ownership"),
                    state_runner_path,
                    migrated_command,
                )
            )
        if old_runner_path in migrated_command:
            raise RuntimeError("migrated queue command still references the old installation")

        migrated_inspection = task_row_by_label(
            pueue_snapshot(upgraded_libexec / "pueue", private_config, env), inspection_label,
        )
        if migrated_inspection is None or pueue_state(migrated_inspection) != "Queued":
            raise RuntimeError("upgraded status lost the queued inspection worker")
        migrated_inspection_command = migrated_inspection.get("original_command", "")
        inspection_request_path = state / "inspections" / inspection_task_id / "request.json"
        inspection_request = json.loads(inspection_request_path.read_text())
        upgrade_record_path = state / "inspections" / inspection_task_id / "worker-upgrade.json"
        if legacy_mode:
            if old_runner_path not in migrated_inspection_command or state_runner_path in migrated_inspection_command:
                raise RuntimeError(
                    "upgraded status changed the runner for a legacy inspection with unknown ownership: "
                    + migrated_inspection_command
                )
            if inspection_request.get("binding", {}).get("runner_ownership"):
                raise RuntimeError("legacy inspection unexpectedly recorded runner ownership")
            if upgrade_record_path.exists():
                raise RuntimeError("ambiguous legacy inspection was incorrectly authorized for worker migration")
        else:
            if state_runner_path not in migrated_inspection_command or inspection_request.get("binding", {}).get("runner_ownership") != "managed":
                raise RuntimeError("managed inspection did not retain its recorded state-root runner")
        assert_state_supervisor_pair(state, upgraded_libexec)

        # Keep the old Pueue client layout until the running models command
        # finishes polling. For legacy records, retain only its pinned worker;
        # for new records, the state-root copy is sufficient.
        if legacy_mode:
            (old_install / "delegate").unlink()
            if (old_install / "delegate").exists():
                raise RuntimeError("old CLI remained after simulated package cleanup")
        else:
            shutil.rmtree(old_install)
            if old_install.exists():
                raise RuntimeError("old CLI and runner remained after simulated package cleanup")

        upgraded_pueue = upgraded_libexec / "pueue"
        migrated_row = task_row_by_label(pueue_snapshot(upgraded_pueue, private_config, env), queued_label)
        if migrated_row is None or pueue_state(migrated_row) != "Queued":
            raise RuntimeError("upgraded CLI lost the legacy queued task")
        migrated_command = migrated_row.get("original_command", "")
        if state_runner_path not in migrated_command:
            raise RuntimeError("upgraded CLI did not retain the migrated content-addressed runner")
        if old_runner_path in migrated_command:
            raise RuntimeError("migrated queue command still references the removed install")

        gate.touch(mode=0o600)
        queued_collected = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "collect", queued_task_id, "--watch", COLLECT_WATCH, "--json",
        ], timeout=COLLECT_TIMEOUT_SECONDS)
        if queued_collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError("upgraded CLI did not run and collect the migrated legacy queued task")
        upgraded_status = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "status", task_id, "--json",
        ])
        if upgraded_status.get("task_id") != task_id:
            raise RuntimeError("upgraded CLI could not recover the earlier task supervisor binding")
        if inspection_process is None:
            raise RuntimeError("queued inspection process was not started")
        inspection_stdout, inspection_stderr = inspection_process.communicate(timeout=45)
        if inspection_process.returncode != 0:
            raise RuntimeError(
                "legacy CLI did not finish the queued inspection using its preserved pinned worker: "
                "exit=%d stdout=%r stderr=%r"
                % (inspection_process.returncode, inspection_stdout[-2000:], inspection_stderr[-2000:])
            )
        try:
            migrated_models = json.loads(inspection_stdout)
        except json.JSONDecodeError as error:
            raise RuntimeError("migrated models command returned invalid JSON: " + str(error)) from error
        if migrated_models.get("status") != "available" or [
            model.get("id") for model in migrated_models.get("models", [])
        ] != ["openai/gpt-4.1"]:
            raise RuntimeError("legacy pinned worker did not complete the queued inspection")

        # Once the worker has finished, remove the remaining old package and
        # prove the upgraded CLI uses only its state-root supervisor pair.
        shutil.rmtree(base / "old")
        if old_install.exists() or old_libexec.exists():
            raise RuntimeError("old package paths remained after simulated package cleanup")
        state_root_models = run_json(upgraded_install / "delegate", env, models_args)
        if state_root_models.get("status") != "available" or [
            model.get("id") for model in state_root_models.get("models", [])
        ] != ["openai/gpt-4.1"]:
            raise RuntimeError("upgraded CLI could not inspect models after old package removal")

        upgraded_task_id = uuid.uuid4().hex
        env["PATH"] = str(provider_bin) + os.pathsep + str(upgraded_install) + os.pathsep + "/usr/bin:/bin"
        upgraded_dispatch = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "dispatch", "--provider", "pi:json",
            "--brief", str(brief), "--cwd", str(workspace),
            "--id", upgraded_task_id, "--permission", "read-only", "--budget", TASK_BUDGET,
            "--json",
        ])
        if upgraded_dispatch.get("task_id") != upgraded_task_id or upgraded_dispatch.get("admission") != "admitted":
            raise RuntimeError("upgraded CLI did not dispatch through the existing private supervisor")
        upgraded_meta = json.loads((state / "tasks" / upgraded_task_id / "meta.json").read_text())
        if upgraded_meta.get("runner_executable") != state_runner_path:
            raise RuntimeError("upgraded task metadata did not retain the state-root runner")
        upgraded_collected = run_json(upgraded_install / "delegate", env, [
            "--root", str(state), "collect", upgraded_task_id, "--watch", COLLECT_WATCH, "--json",
        ], timeout=COLLECT_TIMEOUT_SECONDS)
        if upgraded_collected.get("outcome", {}).get("verdict") != "committed":
            raise RuntimeError("upgraded CLI did not collect the successful second task")
        if upgraded_daemon_marker.exists():
            raise RuntimeError("upgraded daemon was launched while the old private endpoint was healthy")
        if old_daemon_marker.read_text().splitlines() != ["started"]:
            raise RuntimeError("adoption restarted the original private daemon")
        if identity_path.read_bytes() != old_identity_data:
            raise RuntimeError("adopting the existing daemon rewrote launch provenance")

        shutdown = subprocess.run(
            [str(upgraded_libexec / "pueue"), "--config", str(private_config), "shutdown"],
            env=env, capture_output=True, text=True, timeout=15, check=False,
        )
        if shutdown.returncode != 0:
            raise RuntimeError("private acceptance supervisor did not shut down cleanly")
        output.write_text(json.dumps({
            "schema_version": 1,
            "status": "passed",
            "platform": platform.system(),
            "task_ids": [task_id, queued_task_id, upgraded_task_id],
            "inspection_task_id": inspection_task_id,
            "checks": [
                "two installed CLI layouts resolved separate private Pueue pairs",
                "the initial CLI completed dispatch/status/collect against an isolated private supervisor",
                queued_runner_check,
                "the upgraded status repaired the queued runner before old package files were removed",
                "the upgraded CLI completed the migrated queued task after old package paths were removed",
                inspection_runner_check,
                "the upgraded CLI completed another bounded dispatch and collected its result",
                "adoption preserved old daemon launch provenance and did not start a second daemon",
            ],
        }, sort_keys=True) + "\n")
        success = True
        print("PASS private CLI supervisor acceptance: " + str(args.output))
    finally:
        gate.touch(mode=0o600, exist_ok=True)
        if not success and private_config.is_file():
            for client in (upgraded_libexec / "pueue", old_libexec / "pueue", args.pueue):
                if not client.is_file():
                    continue
                try:
                    shutdown = subprocess.run(
                        [str(client), "--config", str(private_config), "shutdown"],
                        env=env, capture_output=True, text=True, timeout=15, check=False,
                    )
                except (OSError, subprocess.TimeoutExpired):
                    continue
                if shutdown.returncode == 0:
                    break
        if success:
            shutil.rmtree(base)
        else:
            print("Retained private acceptance state for diagnosis: " + str(base))


if __name__ == "__main__":
    main()
