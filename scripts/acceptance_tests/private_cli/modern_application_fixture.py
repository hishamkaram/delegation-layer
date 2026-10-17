"""Finite fake CLI/owner/client protocol. This is never native application proof."""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import time


def wait_child(pid):
    deadline = time.monotonic() + 25
    while True:
        found, status = os.waitpid(pid, os.WNOHANG)
        if found:
            return os.waitstatus_to_exitcode(status)
        if time.monotonic() >= deadline:
            raise RuntimeError("finite fake control owner did not finish")
        time.sleep(0.01)


def execute_owned_client(client, config, descriptor, repeat):
    owner = os.fork()
    if owner == 0:
        try:
            code = 0
            for _ in range(repeat):
                child = os.posix_spawn(
                    str(client), [str(client), "-c", str(config), "status", "--json"], dict(os.environ)
                )
                code = wait_child(child)
                if code:
                    break
        finally:
            os.close(descriptor)
        os._exit(code)
    os.close(descriptor)
    return wait_child(owner)


def acquire_lock(path, operation):
    descriptor = os.open(path, os.O_RDWR | os.O_CREAT | os.O_CLOEXEC, 0o600)
    deadline = time.monotonic() + 20
    while True:
        try:
            if operation is not None:
                fcntl.flock(descriptor, operation | fcntl.LOCK_NB)
            return descriptor
        except BlockingIOError:
            if time.monotonic() >= deadline:
                os.close(descriptor)
                raise RuntimeError("finite fake control lock deadline expired")
            time.sleep(0.01)


def publish_pair(state, libexec, *, activate=True):
    supervisor = state / ".supervisor"
    client = hashlib.sha256((libexec / "pueue").read_bytes()).hexdigest()
    daemon = hashlib.sha256((libexec / "pueued").read_bytes()).hexdigest()
    pair_digest = hashlib.sha256((client + "\n" + daemon).encode()).hexdigest()
    pair = supervisor / ("pueue-pair-" + pair_digest)
    pair.mkdir(exist_ok=True)
    for member in ("pueue", "pueued"):
        destination = pair / member
        if not destination.exists():
            destination.write_bytes((libexec / member).read_bytes())
            destination.chmod(0o700)
    (supervisor / "pueue-pair.json").write_text(
        json.dumps({"schema_version": 1, "client_sha256": client, "daemon_sha256": daemon, "pair_sha256": pair_digest})
    )
    if activate:
        config = supervisor / "pueue.yml"
        logical = {
            "config_path": str(config),
            "config_digest": hashlib.sha256(config.read_bytes()).hexdigest(),
            "resolved_config_sha256": hashlib.sha256(config.read_bytes()).hexdigest(),
            "endpoint": "unix:" + str(supervisor / "run" / "pueue.sock"),
        }
        (supervisor / "daemon.identity.json").write_text(
            json.dumps({"executable": str(pair / "pueued"), "sha256": daemon, **logical})
        )
        (supervisor / "supervisor.identity.json").write_text(
            json.dumps({"schema_version": 1, "binding": {
                **logical, "client_executable": str(pair / "pueue"), "client_sha256": client,
                "daemon_executable": str(pair / "pueued"), "daemon_sha256": daemon,
            }})
        )
        runtime = supervisor / "run"
        runtime.mkdir(exist_ok=True)
        (runtime / "pueue.pid").write_text("424242\n")  # Fixture identity; no daemon is launched.
    return pair


def migration_response(state, options, role, root_id, pair):
    supervisor = state / ".supervisor"
    published = json.loads((supervisor / "supervisor.identity.json").read_text())["binding"]
    current = published["daemon_executable"] == str(pair / "pueued")
    active = options.get("active", True)
    control = {"action": "migrate", "state": "migrated", "migration_phase": "complete",
               "active_work": "present" if active else "none", "next_action": "retry_dispatch"}
    code = 0
    if not current and active:
        control = {"action": "migrate", "state": "blocked", "active_work": "present",
                   "reason_code": "active_work", "next_action": "wait_for_legacy_work"}
        code = 1
        config = supervisor / "legacy-pueue.yml"
        config.write_bytes((supervisor / "pueue.yml").read_bytes())
        (supervisor / "legacy-supervisor.json").write_text(json.dumps({
            "schema_version": 1, "supervisor": published, "config_path": str(config),
            "config_sha256": hashlib.sha256(config.read_bytes()).hexdigest(),
        }))
        if options.get("prepare_intent"):
            legacy = json.loads((supervisor / "daemon.identity.json").read_text())
            successor = {**legacy, "executable": str(pair / "pueued"),
                         "sha256": hashlib.sha256((pair / "pueued").read_bytes()).hexdigest()}
            (supervisor / "migration.json").write_text(
                json.dumps({"schema_version": 1, "legacy": legacy, "successor": successor})
            )
    elif not current:
        publish_pair(state, Path(options[role + "_libexec"]))
        for name in ("migration.json", "legacy-supervisor.json", "legacy-pueue.yml"):
            (supervisor / name).unlink(missing_ok=True)
    control.update(options.get("writer_control", {}))
    response = {"schema_version": 1, "command": "supervisor",
                "root_id": options.get("response_root", root_id), "supervisor_control": control}
    if code:
        response["error"] = "Legacy supervisor has active work. Wait for queued or running work before migrating."
    if "writer_error" in options:
        response["error"] = options["writer_error"]
    return response, options.get("writer_exit", code)


def dispatch(state, arguments, options, role, root_id):
    task_id = arguments[arguments.index("--id") + 1]
    directory = state / "tasks" / task_id
    directory.mkdir()
    lifetime = directory / "lifetime"
    lifetime.mkdir()
    for name in (
        "task.json",
        "meta.json",
        "brief.md",
        "supervisor.ref.json",
        "lifetime/intent.json",
        "lifetime/registration.json",
        "lifetime/worker.json",
        "lifetime/000001.launch.json",
    ):
        (directory / name).write_text(task_id + ":" + name)
    pair = publish_pair(state, Path(options[role + "_libexec"]), activate=False)
    if role == "candidate" and options.get("candidate_admission_pid"):
        (state / ".supervisor" / "run" / "pueue.pid").write_text(str(options["candidate_admission_pid"]) + "\n")
    return {
        "task_id": task_id,
        "root_id": root_id,
        "task_record": "created",
        "admission": "admitted",
        "supervisor": {"matched": True, "numeric_task_id": int(task_id[:4], 16)},
        "fixture_pair": str(pair),
    }


def run_control(state, arguments, options, role, root_id):
    writer = arguments[0] == "supervisor"
    lock_name = "wrong.lock" if options.get("wrong_lock") else "bootstrap.lock"
    mode = fcntl.LOCK_EX if writer else fcntl.LOCK_SH
    if options.get("no_writer_lock" if writer else "no_reader_lock"):
        mode = None
    descriptor = acquire_lock(state / ".supervisor" / lock_name, mode)
    pair = publish_pair(state, Path(options[role + "_libexec"]), activate=False) if writer else None
    identity = json.loads((state / ".supervisor" / "supervisor.identity.json").read_text())
    client = Path(identity["binding"]["client_executable"])
    if options.get("client_nonzero"):
        client = Path("/usr/bin/false")
    code = execute_owned_client(
        client, state / ".supervisor" / "pueue.yml", descriptor, options.get("reader_repeat", 1) if not writer else 1
    )
    if code:
        return {"root_id": root_id, "fixture_failure": True}, code
    if writer:
        if options.get("mutate_task"):
            (state / "tasks" / options["mutate_task"] / "meta.json").write_text("changed")
        if options.get("mutate_daemon"):
            (state / ".supervisor" / options["mutate_daemon"]).write_text("changed")
        return migration_response(state, options, role, root_id, pair)
    return {"root_id": root_id, "task_id": arguments[1]}, 0


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--role", required=True)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--pueue-config")
    parser.add_argument("--runner")
    args, arguments = parser.parse_known_args()
    options = json.loads((args.root / "fixture-options.json").read_text())
    root_id = json.loads((args.root / "root.json").read_text())["root_id"]
    supervisor = args.root / ".supervisor"
    old_prepared_read = (args.role == "baseline" and options.get("baseline_prepared_observation_error")
                         and any((supervisor / name).exists() for name in ("migration.json", "legacy-supervisor.json")))
    if arguments[0] in {"status", "collect"} and old_prepared_read:
        response = {"root_id": root_id, "task_id": arguments[1],
                    "error": "private supervisor migration is required: published private supervisor transition is incomplete"}
        code = 1
    elif arguments[0] == "dispatch":
        if options.get("fail_dispatch_role") == args.role:
            response, code = {"root_id": root_id, "admission": "not_admitted"}, 2
        else:
            response, code = dispatch(args.root, arguments, options, args.role, root_id), 0
    elif arguments[0] in {"status", "collect"} and (args.root / "tasks" / arguments[1] / "outcome.json").exists():
        response = {
            "root_id": root_id,
            "task_id": arguments[1],
            "outcome": json.loads((args.root / "tasks" / arguments[1] / "outcome.json").read_text()),
        }
        code = 0
    else:
        response, code = run_control(args.root, arguments, options, args.role, root_id)
    print(json.dumps(response), flush=True)
    return code


if __name__ == "__main__":
    raise SystemExit(main())
