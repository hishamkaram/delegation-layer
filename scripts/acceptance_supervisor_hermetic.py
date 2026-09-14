"""Hermetic H acceptance for the Phase 2 supervisor and finite fixture.

The harness deliberately starts the compiled acceptance command and the
compiled finite supervisor/provider as separate OS processes.  The fake
supervisor records invocations but never starts a runner or sends a signal;
the harness starts delegate-run itself after it has learned the real task
label from the add receipt.

This module is intentionally independent of the Go implementation.  It is an
acceptance oracle, not a second task store or a provider implementation.
"""

import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import time
import traceback
import uuid

from acceptance_supervisor_common import (
    Processes,
    digest,
    inherited_environment,
    read_json,
    require,
    sha,
    wait_until,
    write_json,
)


SCHEMA_VERSION = 1
SUPERVISOR_VERSION = "4.0.4"
PREDICATE = "fixture:test"
MODE = "read-only"
FIXTURE_VERSION = "2"
FIXTURE_TIME = "2026-09-13T20:00:00.123456789Z"
PREDICATE_SHA256 = "8f42ad8ce7677acab2f694c1e014a918edaf8ae8fac7a4bbbdc51603c6adbe93"
OBSERVATION_BOUND = 5.0
CASE_TIMEOUT = 20
MAX_CONTROL_BYTES = 1 << 20


class HarnessFailure(RuntimeError):
    """A required acceptance observation was absent or incorrect."""


def require_true(condition, message):
    if not condition:
        raise HarnessFailure(message)


def write_bytes(path, data, replace=False):
    """Write one bounded evidence/input file with a durable close."""
    path = Path(path)
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    if replace:
        stage = path.with_name(path.name + ".staging")
        with stage.open("xb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(stage, path)
    else:
        with path.open("xb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
    path.chmod(0o600)


def canonical_executable(path):
    path = Path(path)
    require_true(path.is_absolute(), "acceptance executable must be absolute")
    resolved = path.resolve(strict=True)
    require_true(resolved == path, "acceptance executable must be canonical: " + str(path))
    require_true(path.is_file() and os.access(path, os.X_OK),
                 "acceptance executable is not executable: " + str(path))
    return path


def copy_executable(source, destination):
    source = canonical_executable(source)
    destination = Path(destination)
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    shutil.copyfile(source, destination)
    destination.chmod(0o700)
    require_true(digest(source) == digest(destination),
                 "copied executable digest changed: " + str(destination))
    require_true(destination.resolve(strict=True) == destination,
                 "copied executable is not canonical: " + str(destination))
    return destination


def yaml_string(value):
    return json.dumps(str(value), ensure_ascii=False)


def private_pueue_yaml(base):
    """Return ordinary YAML accepted by the strict pueue parser.

    The fake supervisor has a sibling JSON control file.  This document is a
    separate production-style -c input, so its path and digest are part of the
    saved binding.
    """
    base = Path(base)
    values = {
        "pueue_directory": base / "supervisor-state",
        "runtime_directory": base / "supervisor-run",
        "unix_socket_path": base / "supervisor-run" / "pueue.sock",
        "alias_file": base / "aliases.yml",
        "pid_path": base / "supervisor-run" / "pueue.pid",
        "shared_secret_path": base / "supervisor-run" / "secret",
        "daemon_cert": base / "supervisor-run" / "cert.pem",
        "daemon_key": base / "supervisor-run" / "key.pem",
    }
    lines = [
        "shared:",
        "  pueue_directory: " + yaml_string(values["pueue_directory"]),
        "  runtime_directory: " + yaml_string(values["runtime_directory"]),
        "  unix_socket_path: " + yaml_string(values["unix_socket_path"]),
        "  alias_file: " + yaml_string(values["alias_file"]),
        "  use_unix_socket: true",
        "  unix_socket_permissions: 448",
        "  host: " + yaml_string("127.0.0.1"),
        "  port: " + yaml_string("6924"),
        "  pid_path: " + yaml_string(values["pid_path"]),
        "  shared_secret_path: " + yaml_string(values["shared_secret_path"]),
        "  daemon_cert: " + yaml_string(values["daemon_cert"]),
        "  daemon_key: " + yaml_string(values["daemon_key"]),
        "client:",
        "  show_confirmation_questions: false",
        "daemon:",
        "  callback: null",
        "  env_vars: {}",
        "  shell_command:",
        "    - " + yaml_string("/bin/sh"),
        "    - " + yaml_string("-c"),
        "    - " + yaml_string("{{ pueue_command_string }}"),
    ]
    return ("\n".join(lines) + "\n").encode()


def status_job(numeric_id, label, state):
    if state == "queued":
        status = {"Queued": {"enqueued_at": FIXTURE_TIME}}
    elif state == "running":
        status = {"Running": {"enqueued_at": FIXTURE_TIME, "start": FIXTURE_TIME}}
    elif state == "ended":
        status = {
            "Done": {
                "enqueued_at": FIXTURE_TIME,
                "start": FIXTURE_TIME,
                "end": FIXTURE_TIME,
                "result": "Success",
            }
        }
    else:
        raise HarnessFailure("unsupported fake status state: " + state)
    return {
        "id": numeric_id,
        "created_at": FIXTURE_TIME,
        "original_command": "delegate-run --root /tmp/fixture",
        "command": "delegate-run --root /tmp/fixture",
        "path": "/tmp",
        "envs": {},
        "group": "default",
        "dependencies": [],
        "priority": 0,
        "label": label,
        "status": status,
    }


def status_document(rows):
    return {"tasks": {str(row["id"]): row for row in rows}, "groups": {}}


def canonical_json_bytes(value):
    """Match task.MarshalCanonical for the ASCII manifest values below."""
    return (json.dumps(value, separators=(",", ":"), ensure_ascii=False) + "\n").encode()


def label_parts(label):
    prefix = "delegate:"
    require_true(label.startswith(prefix), "fake add label lacks delegate prefix")
    fields = label[len(prefix):].split(":")
    require_true(len(fields) == 2, "fake add label has unexpected identity")
    root_id, task_id = fields
    require_true(len(root_id) == 32 and len(task_id) == 32,
                 "fake add label has malformed IDs")
    return root_id, task_id


def task_id():
    return uuid.uuid4().hex


class Case:
    """One fresh task/root/supervisor fixture and its owned processes."""

    def __init__(self, suite, case_id, scenario="success", hook="normal",
                 budget="1m0s", add_release=True, answer=None,
                 provider_kwargs=None):
        self.suite = suite
        self.case_id = case_id
        self.base = Path(tempfile.mkdtemp(prefix="dl-hermetic-" + case_id + "-", dir="/tmp")).resolve(strict=True)
        self.base.chmod(0o700)
        self.outcome = None
        self.root_id = None
        self.helper_completion_observed = None
        # A supervisor completion receipt is authored by the finite helper;
        # only its matching observer event proves that this process also
        # completed its natural Wait.  Keep that fact separate from the
        # helper's own receipt because the dispatcher may exit first.
        self.external_wait_observed = None
        self.external_wait_required = False
        self.external_wait_unknown = {}
        self.task_id = task_id()
        # The ordinary cases own one task.  The focused continuation control
        # deliberately reuses one root and records every chained task ID so
        # its immutable provider receipts can be counted together.
        self.chain_task_ids = [self.task_id]
        self.chain_records = []
        self.numeric_id = 7
        self.budget = budget
        self.answer = answer if answer is not None else (
            "Exact fixture answer: 'quoted' \"double\"\n"
            "literal slash-n token: \\n\n"
            "$(touch DO_NOT_CREATE) ; <&> | literal\n"
        )
        self.provider_kwargs = dict(provider_kwargs or {})
        self.add_release_enabled = add_release
        self._create_layout()
        self._copy_tools()
        self._write_configs(scenario, hook)
        self.processes = Processes(self.base / "processes")

    def _create_layout(self):
        self.bin_dir = self.base / "cli space ' ; literal"
        self.supervisor_dir = self.base / "supervisor space ' ; literal"
        self.work = self.base / "work space ' ; literal"
        self.root = self.base / "task state ' ; literal"
        self.events_dir = self.base / "events"
        self.provider_records = self.base / "provider records"
        self.supervisor_records = self.base / "supervisor records"
        for path in (
            self.bin_dir,
            self.supervisor_dir,
            self.work,
            self.events_dir,
            self.provider_records,
            self.supervisor_records,
        ):
            path.mkdir(mode=0o700, parents=True)
        self.aliases = self.base / "aliases.yml"
        write_bytes(self.aliases, b"{}\n")
        self.production_config = self.base / "pueue config.yml"
        write_bytes(self.production_config, private_pueue_yaml(self.base))
        self.status_path = self.base / "status.json"
        write_json(self.status_path, status_document([]))
        self.fake_config = self.supervisor_dir / "phase2-supervisor.json"
        self.add_release = self.base / "add.release"
        self.kill_release = self.base / "kill.release"
        self.remove_release = self.base / "remove.release"
        self.provider_release = self.base / "provider.release"
        self.brief = self.base / "brief ' ; literal.md"
        write_bytes(
            self.brief,
            (
                "Exact brief: 'quoted' \"double\"\n"
                "literal slash-n token: \\n\n"
                "$(touch DO_NOT_CREATE) ; <&> | literal\n"
            ).encode(),
        )

    def _copy_tools(self):
        tools = self.suite.tools
        self.delegate = copy_executable(tools / "delegate", self.bin_dir / "delegate")
        self.runner = copy_executable(tools / "delegate-run", self.bin_dir / "delegate-run")
        self.provider = copy_executable(tools / "provider", self.bin_dir / "provider")
        self.pueue = copy_executable(tools / "pueue-fake", self.supervisor_dir / "pueue-fake")
        self.probe = canonical_executable(tools / "phase2probe")

    def _write_configs(self, scenario, hook):
        provider = {
            "scenario": scenario,
            "artifact_dir": str(self.provider_records),
            "lifetime_ms": int(self.provider_kwargs.pop("lifetime_ms", 250)),
            "task_id": self.task_id,
            "session_id": "phase2-" + self.task_id,
            "argv": self.provider_kwargs.pop(
                "argv",
                ["literal argument", "$(touch DO_NOT_CREATE); 'quoted' \"double\""],
            ),
            "answer": self.provider_kwargs.pop("answer", self.answer),
            "stdout_bytes": int(self.provider_kwargs.pop("stdout_bytes", 0)),
            "stderr_bytes": int(self.provider_kwargs.pop("stderr_bytes", 0)),
            "rendezvous_path": str(self.provider_release)
            if self.provider_kwargs.pop("provider_hold", False)
            else "",
            "child_lifetime_ms": int(self.provider_kwargs.pop("child_lifetime_ms", 0)),
        }
        provider.update(self.provider_kwargs)
        self.provider_config = self.base / "provider config.json"
        write_json(self.provider_config, provider)
        self.provider_settings = provider
        fake = {
            "schema_version": 1,
            "config_path": str(self.fake_config),
            "expected_config_path": str(self.production_config),
            "artifact_dir": str(self.supervisor_records),
            "status_path": str(self.status_path),
            "version": SUPERVISOR_VERSION,
            "add_id": self.numeric_id,
            "status": {"delay": 0, "exit_code": 0},
            "add": {
                "delay": 0,
                "release_path": str(self.add_release) if self.add_release_enabled else "",
                "exit_code": 0,
            },
            "kill": {"delay": 0, "exit_code": 0},
            "remove": {"delay": 0, "exit_code": 0},
        }
        write_json(self.fake_config, fake)
        hook_config = {
            "mode": hook,
            "delay_ms": 0,
            "release_path": "",
        }
        if hook == "start-delayed":
            hook_config["delay_ms"] = 10000
            hook_config["release_path"] = str(self.base / "start.release")
            self.start_release = Path(hook_config["release_path"])
        else:
            self.start_release = None
        if hook == "publication-delayed":
            hook_config["release_path"] = str(self.base / "publication.release")
            self.publication_release = Path(hook_config["release_path"])
        else:
            self.publication_release = None
        profile = {
            "schema_version": SCHEMA_VERSION,
            "provider_executable": str(self.provider),
            "provider_sha256": digest(self.provider),
            "provider_config": str(self.provider_config),
            "provider_config_sha256": digest(self.provider_config),
            "supervisor_executable": str(self.pueue),
            "events_directory": str(self.events_dir),
            "environment": [
                key + "=" + value for key, value in sorted(inherited_environment().items())
            ],
            "hooks": hook_config,
        }
        self.profile_config = self.bin_dir / "phase2-fixture.json"
        write_json(self.profile_config, profile)
        self.profile_settings = profile

    def fake_config_data(self):
        return read_json(self.fake_config)

    def update_fake(self, verb, **changes):
        data = self.fake_config_data()
        data[verb] = dict(data[verb])
        data[verb].update(changes)
        write_json(self.fake_config, data, replace=True)

    def set_status(self, state, label=None, numeric_id=None, duplicate=False):
        if label is None:
            require_true(self.root_id is not None, "status label requested before add identity")
            label = "delegate:" + self.root_id + ":" + self.task_id
        if numeric_id is None:
            numeric_id = self.numeric_id
        rows = [status_job(numeric_id, label, state)]
        if duplicate:
            rows.append(status_job(numeric_id + 1, label, state))
        write_json(self.status_path, status_document(rows), replace=True)

    def set_empty_status(self):
        write_json(self.status_path, status_document([]), replace=True)

    def set_malformed_status(self, value=None):
        if value is None:
            value = b'{"tasks":{"7":{"label":"incomplete"}},"groups":{}}\n'
        write_bytes(self.status_path, value, replace=True)

    def release(self, path):
        if not Path(path).exists():
            write_bytes(path, b"release\n")

    def release_add(self):
        self.release(self.add_release)

    def release_provider(self):
        # The finite provider treats rendezvous_path as a prefix and waits for
        # the corresponding ".release" marker. Keep this distinct from the
        # direct release files used by the supervisor and Start hooks. Parent
        # exit and its child intentionally share this configured rendezvous.
        self.release(str(self.provider_release) + ".release")

    def dispatch_argv(self, brief=None, cwd=None, budget=None, task=None,
                      pueue_config=None, provider=PREDICATE, extra=(),
                      omit_budget=False):
        argv = [
            str(self.delegate),
            "--root",
            str(self.root),
            "--pueue-config",
            str(self.production_config if pueue_config is None else pueue_config),
            "--runner",
            str(self.runner),
            "dispatch",
            "--provider",
            provider,
            "--brief",
            str(self.brief if brief is None else brief),
            "--cwd",
            str(self.work if cwd is None else cwd),
            "--id",
            self.task_id if task is None else task,
            "--permission",
            MODE,
        ]
        if not omit_budget:
            argv.extend(["--budget", self.budget if budget is None else budget])
        argv.extend(["--json", *extra])
        return argv

    def start_dispatch(self, **kwargs):
        return self.processes.start(
            "dispatch",
            self.dispatch_argv(**kwargs),
            self.base,
        )

    def dispatch_dynamic(self, expected=(0,), status="queued"):
        process = self.start_dispatch()
        entry = self.wait_entry("add")
        self.root_id, observed_task = label_parts(entry["argv"][5])
        require_true(observed_task == self.task_id, "add receipt changed task identity")
        self.set_status(status)
        self.release_add()
        process.wait(CASE_TIMEOUT, expected=expected)
        response = process.json()
        require_true(response.get("task_id") == self.task_id, "dispatch response task mismatch")
        self.numeric_id = int(read_json(self.fake_config)["add_id"])
        self.require_bound_records()
        return process, response

    def configure_chain_task(self, label, answer, session_id, scenario="success",
                             lifetime_ms=250, provider_hold=False,
                             task_value=None, brief=None):
        """Retarget the acceptance-only provider config for one chain task.

        The profile/config path stays fixed, while the task ID and session
        identity are changed before that task is dispatched.  Provider
        invocation receipts remain in the shared immutable invocation tree;
        this is why the chain can prove three distinct natural launches while
        the public CLI continues to use one root and one supervisor binding.
        """
        if task_value is None:
            task_value = task_id()
        if brief is None:
            brief = self.base / ("chain brief " + label + " ' ; literal.md")
            write_bytes(
                brief,
                (
                    "Continuation chain brief " + label + "\n"
                    "literal slash-n token: \\n\n"
                    "$(touch DO_NOT_CREATE) ; <&> | literal\n"
                ).encode(),
            )
        settings = dict(self.provider_settings)
        settings.update(
            {
                "scenario": scenario,
                "task_id": task_value,
                "session_id": session_id,
                "answer": answer,
                "lifetime_ms": lifetime_ms,
                "rendezvous_path": str(self.provider_release) if provider_hold else "",
                "child_lifetime_ms": 0,
            }
        )
        write_json(self.provider_config, settings, replace=True)
        profile = read_json(self.profile_config)
        profile["provider_config_sha256"] = digest(self.provider_config)
        write_json(self.profile_config, profile, replace=True)
        self.provider_settings = settings
        if task_value not in self.chain_task_ids:
            self.chain_task_ids.append(task_value)
        record = {
            "label": label,
            "task_id": task_value,
            "brief": Path(brief),
            "answer": answer,
            "session_id": session_id,
            "scenario": scenario,
            "provider_settings": copy.deepcopy(settings),
        }
        self.chain_records.append(record)
        return record

    def wait_entry_for_task(self, task_value, timeout=CASE_TIMEOUT):
        """Return the one add receipt whose label names task_value."""
        require_true(self.root_id is not None, "chain root identity is not known")
        label = "delegate:" + self.root_id + ":" + task_value

        def find():
            for path in sorted(self.supervisor_records.glob("*.entry.json")):
                try:
                    entry = read_json(path, MAX_CONTROL_BYTES)
                except (FileNotFoundError, json.JSONDecodeError):
                    continue
                if entry.get("verb") == "add" and len(entry.get("argv", [])) > 5 and entry["argv"][5] == label:
                    return entry
            return None

        return wait_until(
            find,
            timeout=timeout,
            description="fake supervisor add entry for " + task_value,
        )

    def dispatch_chain_task(self, record, resume_task=None, expected=(0,)):
        """Submit one chain task through the public dispatch executable."""
        # Each fake add has an explicit release rendezvous.  Remove only this
        # harness-owned marker so the next add has the same controlled
        # entry/status/release observation as the first one.
        if self.add_release.exists():
            self.add_release.unlink()
        extra = () if resume_task is None else ("--resume-task", resume_task)
        process = self.processes.start(
            "dispatch-chain-" + record["label"],
            self.dispatch_argv(
                brief=record["brief"],
                task=record["task_id"],
                extra=extra,
            ),
            self.base,
        )
        entry = self.wait_entry_for_task(record["task_id"])
        self.set_status(
            "queued",
            label="delegate:" + self.root_id + ":" + record["task_id"],
        )
        self.release_add()
        process.wait(CASE_TIMEOUT, expected=expected)
        response = process.json()
        require_true(response.get("task_id") == record["task_id"],
                     "chain dispatch response task mismatch")
        require_true(response.get("admission") == "admitted",
                     "chain dispatch was not admitted")
        record["dispatch_entry"] = entry
        record["dispatch_response"] = response
        self.require_chain_bound_records(record)
        return process, response

    def require_chain_bound_records(self, record):
        task_dir = self.root / "tasks" / record["task_id"]
        task_record = read_json(task_dir / "task.json", MAX_CONTROL_BYTES)
        submit = read_json(task_dir / "submit.json", MAX_CONTROL_BYTES)
        meta = read_json(task_dir / "meta.json", MAX_CONTROL_BYTES)
        require_true(task_record["root_id"] == self.root_id and
                     task_record["task_id"] == record["task_id"] and
                     task_record["provider"] == PREDICATE,
                     "chain task request identity changed")
        require_true(submit["label"] == "delegate:" + self.root_id + ":" + record["task_id"] and
                     submit["supervisor"]["config_path"] == str(self.production_config) and
                     submit["supervisor"]["client_executable"] == str(self.pueue),
                     "chain supervisor binding changed")
        require_true(meta["provider_executable"] == str(self.provider) and
                     meta["provider_version"] == "fixture-v2",
                     "chain provider binding changed")
        return task_dir

    def start_runner_for(self, record):
        return self.processes.start(
            "runner-chain-" + record["label"],
            [str(self.runner), "--root", str(self.root), record["task_id"], "--json"],
            self.base,
        )

    def finish_runner_for(self, record, process, expected=(0,)):
        process.wait(CASE_TIMEOUT, expected=expected)
        allowed = {expected} if isinstance(expected, int) else set(expected)
        require_true(process.result["natural_wait"] is True and
                     process.result["exit_code"] in allowed,
                     "chain runner did not complete with a natural wait")
        response = process.json()
        record["runner_receipt"] = copy.deepcopy(process.result)
        record["runner_response"] = response
        return response

    def run_command_for(self, record, name, command, expected=(0,), extra=()):
        """Run a public read/control command against one chained task ID."""
        argv = [
            str(self.delegate),
            "--root",
            str(self.root),
            command,
            record["task_id"],
            "--json",
            *extra,
        ]
        process = self.processes.run(
            name,
            argv,
            self.base,
            expected=expected,
            timeout=CASE_TIMEOUT,
        )
        response = process.json()
        record.setdefault("control_receipts", []).append({
            "name": name,
            "command": command,
            "response": response,
            "process": copy.deepcopy(process.result),
        })
        return process, response

    def wait_provider_entry_for_task(self, record, timeout=CASE_TIMEOUT):
        invocations = self.provider_records / "invocations"

        def find():
            if not invocations.is_dir():
                return None
            for directory in sorted(invocations.iterdir()):
                path = directory / "provider.entry.json"
                if not path.exists():
                    continue
                try:
                    entry = read_json(path, MAX_CONTROL_BYTES)
                except (FileNotFoundError, json.JSONDecodeError):
                    continue
                if entry.get("task_id") == record["task_id"]:
                    return entry
            return None

        entry = wait_until(
            find,
            timeout=timeout,
            description="provider invocation entry for " + record["task_id"],
        )
        record["provider_entry"] = entry
        return entry

    def wait_chain_provider_hold(self, record, entry, timeout=CASE_TIMEOUT):
        waiting = Path(str(self.provider_release) + ".waiting")
        self.wait_path(waiting, timeout=timeout)
        expected = (str(entry["pid"]) + ":waiting\n").encode()
        require_true(waiting.read_bytes() == expected,
                     "chain provider rendezvous PID changed")
        return entry["pid"]

    def snapshot_chain_provider(self, record, expected_exit=0):
        """Freeze independent provider artifacts before the next task rewrites them."""
        entry = record["provider_entry"]
        invocation_dir = Path(entry["invocation_dir"])
        completion_path = invocation_dir / "provider.complete.json"
        completion = wait_until(
            lambda: read_json(completion_path, MAX_CONTROL_BYTES)
            if completion_path.exists() else None,
            timeout=CASE_TIMEOUT,
            description="provider completion for " + record["task_id"],
        )
        settings = record["provider_settings"]
        expected_argv = [str(self.provider), str(self.provider_config), *settings["argv"]]
        brief_bytes = (self.provider_records / "brief.input").read_bytes()
        require_true(brief_bytes == record["brief"].read_bytes(),
                     "chain provider stdin bytes changed for " + record["label"])
        require_true(entry["task_id"] == record["task_id"] and
                     entry["session_id"] == record["session_id"] and
                     entry["process_argv"] == expected_argv and
                     entry["pid"] > 0 and completion["pid"] == entry["pid"] and
                     completion["natural"] is True and
                     completion["exit_code"] == expected_exit,
                     "chain provider completion identity changed")
        for name in ("stdout", "stderr"):
            source = self.provider_records / ("expected." + name)
            target = self.base / ("chain-" + record["label"] + ".expected." + name)
            write_bytes(target, source.read_bytes())
            record["expected_" + name] = target
        brief_target = self.base / ("chain-" + record["label"] + ".brief.input")
        write_bytes(brief_target, brief_bytes)
        record["expected_brief"] = brief_target
        record["provider_completion"] = completion
        return entry, completion

    def chain_execution_sequence(self, record):
        candidates = [
            values for values in self.execution_sequences()
            if any(record["task_id"] in value.get("argv", []) for value in values)
        ]
        require_true(len(candidates) == 1,
                     "expected one runner event sequence for " + record["task_id"])
        return candidates[0]

    def require_chain_terminal(self, record, response):
        task_dir = self.root / "tasks" / record["task_id"]
        seal = read_json(task_dir / "provider.exit", MAX_CONTROL_BYTES)
        outcome = read_json(task_dir / "outcome.json", MAX_CONTROL_BYTES)
        require_true(response.get("publication") == "committed" and
                     outcome["verdict"] == "committed" and
                     response.get("outcome") == outcome,
                     "chain task did not publish one committed winner")
        for value in (seal, outcome):
            require_true(value["schema_version"] == 1 and
                         value["root_id"] == self.root_id and
                         value["task_id"] == record["task_id"],
                         "chain terminal identity changed")
            require_true(value["predicate"] == {
                "adapter": "fixture:test",
                "mode": "read-only",
                "version": "2",
                "sha256": PREDICATE_SHA256,
            }, "chain predicate identity changed")
        require_true(seal["invocation_state"] == "started" and
                     seal["exit_code"] == 0 and seal["error"] == "",
                     "chain provider seal was not a clean natural exit")
        require_true(outcome["spec_sha256"] == seal["spec_sha256"] and
                     outcome["meta_sha256"] == seal["meta_sha256"] and
                     outcome["evidence_sha256"] == seal["manifest_sha256"] and
                     outcome["predicate"] == seal["predicate"],
                     "chain outcome changed sealed identity")
        for name in ("stderr", "stdout"):
            expected = record["expected_" + name].read_bytes()
            actual = (task_dir / "raw" / name).read_bytes()
            require_true(actual == expected,
                         "chain raw " + name + " bytes changed")
        payload_descriptor = outcome["payload"]
        require_true(payload_descriptor["basename"] == "result.txt" and
                     (task_dir / "result.txt").read_bytes() == record["answer"].encode() and
                     payload_descriptor["length"] == len(record["answer"].encode()) and
                     payload_descriptor["sha256"] == sha(record["answer"].encode()),
                     "chain payload bytes or descriptor changed")
        require_true(response.get("payload") == payload_descriptor and
                     response.get("evidence_sha256") == seal["manifest_sha256"],
                     "chain response lost terminal identity")
        provider_ref = read_json(task_dir / "provider.ref.json", MAX_CONTROL_BYTES)
        require_true(provider_ref["provider"] == PREDICATE and
                     provider_ref["root_id"] == self.root_id and
                     provider_ref["task_id"] == record["task_id"] and
                     provider_ref["conversation_id"] == record["session_id"] and
                     provider_ref["meta_sha256"] == sha((task_dir / "meta.json").read_bytes()) and
                     provider_ref["spec_sha256"] == sha((task_dir / "task.json").read_bytes()),
                     "chain provider session identity was not durably recorded")
        record["seal"] = seal
        record["outcome"] = outcome
        record["provider_ref"] = provider_ref
        values = self.chain_execution_sequence(record)
        names = [value.get("name") for value in values]
        wait_events = [value for value in values if value.get("name") == "wait-completed"]
        require_true(len(wait_events) == 1,
                     "chain runner did not record exactly one Wait event")
        runner_receipt = record.get("runner_receipt") or {}
        require_true(
            wait_events[0].get("pid") == runner_receipt.get("pid") and
            wait_events[0].get("argv") == values[0].get("argv") and
            runner_receipt.get("natural_wait") is True and
            runner_receipt.get("exit_code") == 0,
            "chain Wait event is not bound to its owned runner process",
        )
        provider_entry = record.get("provider_entry")
        provider_completion = record.get("provider_completion")
        require_true(
            isinstance(provider_entry, dict) and isinstance(provider_completion, dict) and
            provider_entry.get("task_id") == record["task_id"] and
            provider_entry.get("session_id") == record["session_id"] and
            provider_entry.get("pid", 0) > 0 and
            provider_completion.get("pid") == provider_entry.get("pid") and
            provider_completion.get("natural") is True and
            provider_completion.get("exit_code") == 0,
            "chain Wait lacks a unique natural provider receipt binding",
        )
        for name in (
            "start-entry", "started", "parent-fds-closed", "started-receipt",
            "wait-completed", "stdout-eof", "stderr-eof", "stdout-raw-closed",
            "stderr-raw-closed", "completion-observed", "timer-disarmed",
            "sealed", "published",
        ):
            require_true(name in names,
                         "chain runner event missing: " + record["task_id"] + ":" + name)
        position = {name: names.index(name) for name in names}
        require_true(position["start-entry"] < position["started"] and
                     position["wait-completed"] < position["completion-observed"] <
                     position["timer-disarmed"] < position["sealed"] <
                     position["published"],
                     "chain runner Wait/seal events are out of order")
        require_true(position["stdout-eof"] < position["stdout-raw-closed"] <
                     position["completion-observed"] and
                     position["stderr-eof"] < position["stderr-raw-closed"] <
                     position["completion-observed"],
                     "chain capture close events are out of order")

    def dispatch_without_admission(self, expected=(2,), **kwargs):
        process = self.start_dispatch(**kwargs)
        process.wait(CASE_TIMEOUT, expected=expected)
        if (process.directory / "stdout").stat().st_size == 0:
            return process, {"error": (process.directory / "stderr").read_text()}
        return process, process.json()

    def retry_dispatch(self, expected=(0,), **kwargs):
        process = self.start_dispatch(**kwargs)
        process.wait(CASE_TIMEOUT, expected=expected)
        return process, process.json()

    def command_argv(self, command, extra=()):
        return [
            str(self.delegate),
            "--root",
            str(self.root),
            command,
            self.task_id,
            "--json",
            *extra,
        ]

    def run_command(self, name, command, expected=(0,), extra=()):
        process = self.processes.run(
            name,
            self.command_argv(command, extra),
            self.base,
            expected=expected,
            timeout=CASE_TIMEOUT,
        )
        return process, process.json()

    def run_command_with_environment(self, name, command, environment,
                                     expected=(0,), extra=()):
        """Run one fresh CLI with an explicit ambient environment overlay."""
        previous = self.processes.environment
        self.processes.environment = dict(environment)
        try:
            return self.run_command(name, command, expected, extra)
        finally:
            self.processes.environment = previous

    def start_runner(self, expected=(0, 1), name="delegate-run"):
        process = self.processes.start(
            name,
            [str(self.runner), "--root", str(self.root), self.task_id, "--json"],
            self.base,
        )
        return process

    def finish_runner(self, process, expected=(0, 1)):
        process.wait(CASE_TIMEOUT, expected=expected)
        return process.json()

    def wait_entry(self, verb, timeout=CASE_TIMEOUT):
        def find():
            for path in sorted(self.supervisor_records.glob("*.entry.json")):
                try:
                    receipt = read_json(path, MAX_CONTROL_BYTES)
                except (FileNotFoundError, json.JSONDecodeError):
                    continue
                if receipt.get("verb") == verb:
                    return receipt
            return None

        return wait_until(find, timeout=timeout, description="fake supervisor " + verb + " entry")

    def entries(self):
        entries = []
        for path in sorted(self.supervisor_records.glob("*.entry.json")):
            entries.append(read_json(path, MAX_CONTROL_BYTES))
        return entries

    def wait_completion(self, entry, timeout=CASE_TIMEOUT):
        path = self.supervisor_records / (entry["invocation_id"] + ".completion.json")
        return wait_until(
            lambda: read_json(path, MAX_CONTROL_BYTES) if path.exists() else None,
            timeout=timeout,
            description="fake supervisor command completion",
        )

    def observe_helper_completion_if_present(self, entry, timeout=1.0):
        self.external_wait_required = True
        # Register the exact helper invocation before waiting.  If the
        # dispatcher exits first, a completion may become visible later while
        # its observer Wait remains unknowable; only this invocation may be
        # exempted from the paired observer-completion check.
        self.external_wait_unknown[entry["invocation_id"]] = entry["pid"]
        path = self.supervisor_records / (entry["invocation_id"] + ".completion.json")
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if path.exists():
                self.helper_completion_observed = True
                completion = read_json(path, MAX_CONTROL_BYTES)
                self.external_wait_observed = bool(
                    self.supervisor_observer_completion_for_entry(entry)
                )
                if self.external_wait_observed:
                    self.external_wait_unknown.pop(entry["invocation_id"], None)
                return completion
            time.sleep(0.025)
        self.helper_completion_observed = False
        # The caller's process has already returned, so absence of the helper
        # receipt cannot prove a negative.  Leave external Wait unknown rather
        # than converting the observation bound into a lifecycle fact.
        self.external_wait_observed = None
        return None

    def supervisor_observer_events_for_entry(self, entry):
        """Match observer events by executable, PID, and literal argv.

        The finite helper chooses its own invocation ID, while the application
        observer chooses a separate command ID.  The stable join key is the
        helper PID and the complete command argv, including the executable.
        """
        expected_argv = [entry["executable"], *entry["argv"]]
        by_command = {}
        for value in self.events():
            if value.get("kind") != "supervisor":
                continue
            command = value.get("command") or {}
            if command.get("argv") != expected_argv:
                continue
            command_id = command.get("command_id")
            if not command_id:
                continue
            by_command.setdefault(command_id, []).append(value)
        candidates = []
        for values in by_command.values():
            if any(
                (value.get("command") or {}).get("stage") == "started"
                and (value.get("command") or {}).get("pid") == entry["pid"]
                for value in values
            ):
                candidates.append(values)
        require_true(len(candidates) <= 1,
                     "supervisor recorder matched multiple command IDs for invocation " + entry["invocation_id"])
        return candidates[0] if candidates else []

    def supervisor_observer_completion_for_entry(self, entry):
        return next(
            (
                value
                for value in self.supervisor_observer_events_for_entry(entry)
                if (value.get("command") or {}).get("stage") == "completed"
            ),
            None,
        )

    def wait_supervisor_completion(self, entry, timeout=CASE_TIMEOUT):
        return wait_until(
            lambda: self.supervisor_observer_completion_for_entry(entry),
            timeout=timeout,
            description="supervisor observer completion for " + entry["invocation_id"],
        )

    def require_supervisor_recorder(self, entries):
        """Prove each recorded helper invocation has a matching event log.

        This prevents an absent observer log from being silently interpreted as
        S=0/K=0/M=0.  An in-flight helper legitimately has entry/started only;
        a helper completion must also have a matching observer completion,
        except for the exact invocation explicitly retained as external-Wait
        unknown after its caller exited.
        """
        for entry in entries:
            events = self.supervisor_observer_events_for_entry(entry)
            stages = {
                (value.get("command") or {}).get("stage") for value in events
            }
            require_true("entry" in stages and "started" in stages,
                         "supervisor recorder missed Start for invocation " + entry["invocation_id"])
            completion_path = self.supervisor_records / (entry["invocation_id"] + ".completion.json")
            if completion_path.exists():
                helper_completion = read_json(completion_path, MAX_CONTROL_BYTES)
                require_true(
                    helper_completion.get("invocation_id") == entry["invocation_id"] and
                    helper_completion.get("pid") == entry["pid"] and
                    helper_completion.get("executable") == entry["executable"] and
                    helper_completion.get("config_path") == entry["config_path"] and
                    helper_completion.get("argv") == entry["argv"],
                    "supervisor helper completion identity changed for invocation " + entry["invocation_id"],
                )
                completion = self.supervisor_observer_completion_for_entry(entry)
                if completion is None:
                    require_true(
                        self.external_wait_unknown.get(entry["invocation_id"]) == entry["pid"],
                        "supervisor recorder missed completion for invocation " + entry["invocation_id"],
                    )
                elif self.external_wait_unknown.get(entry["invocation_id"]) == entry["pid"]:
                    self.external_wait_observed = True
                    self.external_wait_unknown.pop(entry["invocation_id"], None)
        return entries

    def observed_supervisor_completions(self):
        """Return command IDs whose wrapper observer wrote a completed event."""
        result = []
        for value in self.events():
            if value.get("kind") != "supervisor":
                continue
            command = value.get("command") or {}
            if command.get("stage") == "completed" and command.get("command_id"):
                result.append(command["command_id"])
        return sorted(set(result))

    def provider_entry(self):
        path = self.provider_records / "provider.entry.json"
        return read_json(path, MAX_CONTROL_BYTES)

    def provider_completion(self):
        path = self.provider_records / "provider.complete.json"
        return read_json(path, MAX_CONTROL_BYTES)

    def events(self):
        found = []
        for path in sorted(self.events_dir_files()):
            try:
                value = read_json(path, MAX_CONTROL_BYTES)
            except (FileNotFoundError, json.JSONDecodeError):
                continue
            if isinstance(value, dict):
                found.append(value)
        return found

    def events_dir_files(self):
        return self.events_dir.rglob("*.json")

    def wait_event(self, name, timeout=CASE_TIMEOUT):
        def find():
            return name in self.event_names()

        return wait_until(find, timeout=timeout, description="execution event " + name)

    def event_names(self):
        return [
            value.get("name", value.get("event"))
            for value in self.events()
            if value.get("name", value.get("event")) is not None
        ]

    def execution_sequences(self):
        grouped = {}
        for value in self.events():
            if value.get("kind") != "execution":
                continue
            grouped.setdefault(value.get("invocation_id"), []).append(value)
        return [
            sorted(values, key=lambda value: value.get("sequence", 0))
            for values in grouped.values()
        ]

    def runner_execution_sequence(self):
        candidates = [
            values for values in self.execution_sequences()
            if any(value.get("name") == "start-entry" for value in values)
        ]
        require_true(len(candidates) == 1,
                     "expected exactly one runner start-entry, found %d" % len(candidates))
        return candidates[0]

    def counts(self):
        entries = self.entries()
        self.require_supervisor_recorder(entries)
        names = self.event_names()
        # The root convenience receipts are first-writer summaries. Count
        # immutable per-invocation receipts instead, so a duplicate provider
        # launch cannot hide behind the same root filename.
        provider_entries = []
        child_entries = []
        allowed_task_ids = set(self.chain_task_ids)
        for path in sorted((self.provider_records / "invocations").glob("*/provider.entry.json")):
            value = read_json(path, MAX_CONTROL_BYTES)
            require_true(value.get("invocation_id") == path.parent.name,
                         "provider invocation receipt ID changed")
            require_true(value.get("task_id") in allowed_task_ids and value.get("pid", 0) > 0,
                         "provider invocation receipt identity changed")
            if value.get("scenario") == "child" or "--child" in value.get("process_argv", []):
                child_entries.append(path)
            else:
                provider_entries.append(path)
        return {
            "A": sum(1 for item in entries if item.get("verb") == "add"),
            "K": sum(1 for item in entries if item.get("verb") == "kill"),
            "M": sum(1 for item in entries if item.get("verb") == "remove"),
            "S": names.count("start-entry"),
            "E": len(provider_entries),
            "child_E": len(child_entries),
            "supervisor_entries": len(entries),
            "event_names": names,
        }

    def require_bound_records(self):
        task_path = self.root / "tasks" / self.task_id
        submit = read_json(task_path / "submit.json")
        meta = read_json(task_path / "meta.json")
        require_true(submit["label"] == "delegate:" + self.root_id + ":" + self.task_id,
                     "saved submit label changed")
        require_true(submit["supervisor"]["config_path"] == str(self.production_config),
                     "saved supervisor config path changed")
        require_true(submit["supervisor"]["client_executable"] == str(self.pueue),
                     "saved supervisor executable changed")
        require_true(meta["provider_executable"] == str(self.provider),
                     "saved provider executable changed")
        require_true(meta["provider_version"] == "fixture-v2",
                     "saved provider version changed")
        return task_path

    def require_provider_evidence(self, expect_exit=0):
        entry = self.provider_entry()
        completion = self.provider_completion()
        expected_argv = [str(self.provider), str(self.provider_config), *self.provider_settings["argv"]]
        require_true(entry["pid"] == completion["pid"] and entry["pid"] > 0,
                     "provider entry/completion PID mismatch")
        require_true(completion["natural"] is True and completion["exit_code"] == expect_exit,
                     "provider did not complete with expected natural exit")
        require_true(entry["task_id"] == self.task_id and entry["cwd"] == str(self.work),
                     "provider identity/cwd changed")
        require_true(entry["process_argv"] == expected_argv,
                     "provider literal argv changed")
        require_true(
            (self.provider_records / "brief.input").read_bytes() == self.brief.read_bytes(),
            "provider stdin bytes changed",
        )
        return entry, completion

    def expected_raw_files(self):
        prefix = "child." if self.provider_settings["scenario"] == "parent_exit" else ""
        return {
            name: self.provider_records / (prefix + "expected." + name)
            for name in ("stderr", "stdout")
        }

    def expected_payload(self):
        scenario = self.provider_settings["scenario"]
        if scenario == "large":
            count = self.provider_settings["stdout_bytes"] or 2 * 1024 * 1024
            return b"S" * count
        if scenario == "parent_exit":
            return b"child fixture answer"
        if scenario == "empty":
            return b"empty_answer"
        if scenario == "diagnostic":
            return b"stderr_error"
        if scenario in ("malformed", "truncated"):
            return b"malformed_output"
        if scenario == "nonzero":
            return b"provider_exit: 7"
        answer = self.provider_settings["answer"]
        return answer.encode()

    def require_terminal_evidence(self, response, expect_exit=0):
        """Validate independent raw, seal, payload, and event identities."""
        task_dir = self.task_dir()
        seal = read_json(task_dir / "provider.exit", MAX_CONTROL_BYTES)
        outcome = read_json(task_dir / "outcome.json", MAX_CONTROL_BYTES)
        require_true(seal["schema_version"] == 1 and outcome["schema_version"] == 1,
                     "terminal records are unversioned")
        for record in (seal, outcome):
            require_true(record["root_id"] == self.root_id and record["task_id"] == self.task_id,
                         "terminal record identity changed")
            require_true(record["predicate"] == {
                "adapter": "fixture:test",
                "mode": "read-only",
                "version": "2",
                "sha256": PREDICATE_SHA256,
            }, "terminal predicate identity changed")
        require_true(seal["invocation_state"] == "started" and
                     seal["exit_code"] == expect_exit and seal["error"] == "",
                     "provider seal has unexpected invocation result")
        require_true(
            outcome["spec_sha256"] == seal["spec_sha256"] and
            outcome["meta_sha256"] == seal["meta_sha256"] and
            outcome["evidence_sha256"] == seal["manifest_sha256"] and
            outcome["predicate"] == seal["predicate"],
            "outcome does not preserve seal identity",
        )
        expected_manifest = []
        for name in ("stderr", "stdout"):
            expected_path = self.expected_raw_files()[name]
            expected = expected_path.read_bytes()
            actual_path = task_dir / "raw" / name
            actual = actual_path.read_bytes()
            require_true(actual == expected, "raw " + name + " bytes changed")
            expected_manifest.append({
                "path": "raw/" + name,
                "size": len(expected),
                "sha256": sha(expected),
            })
        require_true(seal["raw_manifest"] == expected_manifest,
                     "raw manifest does not match independent fixture bytes")
        require_true(
            seal["manifest_sha256"] == sha(canonical_json_bytes(expected_manifest)),
            "raw manifest digest changed",
        )
        payload_descriptor = outcome["payload"]
        payload_name = payload_descriptor["basename"]
        require_true(payload_name in ("result.txt", "publish.reject"),
                     "unexpected terminal payload name")
        payload = (task_dir / payload_name).read_bytes()
        expected_payload = self.expected_payload()
        require_true(payload == expected_payload, "terminal payload bytes changed")
        require_true(
            payload_descriptor == {
                "basename": payload_name,
                "length": len(expected_payload),
                "sha256": sha(expected_payload),
            },
            "terminal payload descriptor changed",
        )
        require_true(response.get("outcome") == outcome and
                     response.get("payload") == payload_descriptor and
                     response.get("evidence_sha256") == seal["manifest_sha256"],
                     "runner response changed terminal identity")
        expected_verdict = (
            "committed"
            if self.provider_settings["scenario"]
            in ("success", "large", "hold", "parent_exit", "late_session")
            else "rejected"
        )
        require_true(outcome["verdict"] == expected_verdict,
                     "terminal verdict changed")
        self.require_terminal_event_sequence()

    def require_terminal_event_sequence(self):
        values = self.runner_execution_sequence()
        names = [value.get("name") for value in values]
        required = (
            "start-entry", "started", "parent-fds-closed", "started-receipt",
            "wait-completed", "stdout-eof", "stderr-eof",
            "stdout-raw-closed", "stderr-raw-closed", "completion-observed",
            "timer-disarmed", "sealed", "published",
        )
        for name in required:
            require_true(name in names, "runner event missing: " + name)
        position = {name: names.index(name) for name in names}
        require_true(position["start-entry"] < position["started"] <
                     position["parent-fds-closed"] < position["started-receipt"],
                     "runner Start events are out of order")
        require_true(position["stdout-eof"] < position["stdout-raw-closed"] <
                     position["completion-observed"],
                     "stdout close events are out of order")
        require_true(position["stderr-eof"] < position["stderr-raw-closed"] <
                     position["completion-observed"],
                     "stderr close events are out of order")
        require_true(position["wait-completed"] < position["completion-observed"] <
                     position["timer-disarmed"] < position["sealed"] <
                     position["published"],
                     "runner Wait/seal events are out of order")

    def task_dir(self):
        return self.root / "tasks" / self.task_id

    def wait_path(self, path, timeout=CASE_TIMEOUT):
        return wait_until(
            lambda: Path(path).exists(),
            timeout=timeout,
            description="path " + str(path),
        )

    def wait_provider_hold(self, timeout=CASE_TIMEOUT):
        """Observe the finite provider's actual held-pipe rendezvous."""
        waiting = Path(str(self.provider_release) + ".waiting")
        self.wait_path(waiting, timeout=timeout)
        content = waiting.read_text()
        fields = content.rstrip("\n").split(":")
        require_true(len(fields) == 2 and fields[1] == "waiting",
                     "provider rendezvous receipt is malformed")
        try:
            pid = int(fields[0])
        except ValueError as error:
            raise HarnessFailure("provider rendezvous PID is malformed") from error
        known = []
        for path in (
            self.provider_records / "provider.entry.json",
            self.provider_records / "child.provider.entry.json",
        ):
            if path.exists():
                known.append(read_json(path, MAX_CONTROL_BYTES).get("pid"))
        require_true(pid > 0 and pid in known,
                     "provider rendezvous PID is not a recorded fixture process")
        return pid

    def stop_record(self, request_id):
        path = self.task_dir() / "stop" / (request_id + ".request.json")
        return read_json(path, MAX_CONTROL_BYTES)

    def receipt(self):
        return {
            "case": self.case_id,
            "task_id": self.task_id,
            "chain_task_ids": list(self.chain_task_ids),
            "chain": [
                {
                    key: value
                    for key, value in record.items()
                    if key in ("label", "task_id", "answer", "session_id", "scenario")
                }
                for record in self.chain_records
            ],
            "chain_summary": getattr(self, "chain_summary", None),
            "root_id": self.root_id,
            "base": str(self.base),
            "helper_completion_observed": self.helper_completion_observed,
            "external_wait_observed": self.external_wait_observed,
            "external_wait_required": self.external_wait_required,
            "external_wait_unknown": [
                {"invocation_id": invocation_id, "pid": pid}
                for invocation_id, pid in sorted(self.external_wait_unknown.items())
            ],
            "supervisor_observer_completions": self.observed_supervisor_completions(),
            "counts": self.counts(),
            "supervisor_binding": {
                "config_path": str(self.production_config),
                "config_sha256": digest(self.production_config),
                "fake_executable": str(self.pueue),
            },
        }

    def copy_evidence(self):
        destination = self.suite.output / "cases" / self.case_id
        destination.mkdir(mode=0o700, parents=True, exist_ok=True)
        for source_name, target_name in (
            ("task state ' ; literal", "task-state"),
            ("events", "events"),
            ("provider records", "provider-records"),
            ("supervisor records", "supervisor-records"),
            ("processes", "processes"),
        ):
            source = self.base / source_name
            if source.exists():
                shutil.copytree(source, destination / target_name)
        for source in (
            self.production_config,
            self.fake_config,
            self.profile_config,
            self.provider_config,
            self.status_path,
            self.brief,
        ):
            if source.exists():
                shutil.copyfile(source, destination / source.name)
        write_json(destination / "receipt.json", self.receipt())
        return destination

    def finish(self, status="pass", error=None):
        active = self.processes.drain(timeout=0, exclude=())
        require_true(not active, "owned process remained active at case completion")
        self.outcome = {"status": status}
        if error is not None:
            self.outcome["error"] = str(error)
        self.copy_evidence()
        if status == "pass" and not (
            self.external_wait_required and self.external_wait_observed is None
        ):
            shutil.rmtree(self.base)

    def retain_failure(self, error):
        active = self.processes.drain(timeout=10, exclude=())
        destination = self.suite.output / "cases" / self.case_id
        destination.mkdir(mode=0o700, parents=True, exist_ok=True)
        write_json(
            destination / "failure.json",
            {
                "case": self.case_id,
                "error": str(error),
                "traceback": traceback.format_exc(),
                "private_base": str(self.base),
                "unresolved_owned_processes": active,
                "signals_sent": 0,
                "termination_unknown_preserved": True,
            },
        )


class HermeticSuite:
    def __init__(self, tools, output):
        self.tools = Path(tools).resolve(strict=True)
        self.output = Path(output).resolve()
        self.output.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.results = []
        for name in ("delegate", "delegate-run", "provider", "pueue-fake", "phase2probe"):
            canonical_executable(self.tools / name)

    def run_case(self, case_id, fn, **kwargs):
        case = None
        try:
            case = Case(self, case_id, **kwargs)
            fn(case)
            result = case.receipt()
            case.finish("pass")
            result["status"] = "pass"
            self.results.append(result)
            return
        except BaseException as error:
            if case is not None:
                try:
                    case.retain_failure(error)
                except BaseException:
                    pass
            result = {
                "case": case_id,
                "status": "fail",
                "error": str(error),
                "traceback": traceback.format_exc(),
            }
            if case is not None:
                result["private_base"] = str(case.base)
            self.results.append(result)

    def write_summary(self):
        summary = {
            "schema_version": 1,
            "suite": "phase2-supervisor-hermetic",
            "tools": {
                name: {
                    "path": str(self.tools / name),
                    "sha256": digest(self.tools / name),
                }
                for name in ("delegate", "delegate-run", "provider", "pueue-fake", "phase2probe")
            },
            "cases": self.results,
            "counts": {
                "pass": sum(item["status"] == "pass" for item in self.results),
                "fail": sum(item["status"] == "fail" for item in self.results),
            },
            "forbidden_operations": {
                "signals_sent": 0,
                "shell_true": 0,
                "command_context": 0,
                "wait_delay": 0,
                "process_group_operations": 0,
                "real_native_supervisor": 0,
            },
        }
        write_json(self.output / "summary.json", summary)
        return summary

    def run(self):
        self.run_case("A01", run_a01)
        self.run_case("A02-empty", run_a02_empty)
        self.run_case("A02-duplicate", run_a02_duplicate)
        self.run_case("A02-malformed", run_a02_malformed)
        self.run_case("A03-config", run_a03_config)
        self.run_case("A03-label", run_a03_label)
        self.run_case("A04-invalid-config", run_a04_invalid_config)
        self.run_case("A04-symlink-config", run_a04_symlink_config)
        self.run_case("A04-valid", run_a04_valid)
        self.run_case("A04-saved-binding", run_a04_saved_binding)
        self.run_case("A05-concurrent", run_a05_concurrent)
        self.run_case("A06", run_a06)
        self.run_case("P01", run_p01)
        self.run_case("P02", run_p02, provider_kwargs={"scenario": "large"})
        self.run_case("P03-success", run_p03, provider_kwargs={"scenario": "success"})
        self.run_case("P03-empty", run_p03, provider_kwargs={"scenario": "empty"})
        self.run_case("P03-diagnostic", run_p03, provider_kwargs={"scenario": "diagnostic"})
        self.run_case("P03-malformed", run_p03, provider_kwargs={"scenario": "malformed"})
        self.run_case("P03-truncated", run_p03, provider_kwargs={"scenario": "truncated"})
        self.run_case("P03-nonzero", run_p03, provider_kwargs={"scenario": "nonzero"})
        self.run_case(
            "P04",
            run_p04,
            provider_kwargs={"scenario": "parent_exit", "provider_hold": True, "child_lifetime_ms": 5000},
        )
        self.run_case("P05-start", run_p05_start, hook="start-failed")
        self.run_case("P05-wait", run_p05_fault, hook="wait-failed")
        self.run_case("P05-capture", run_p05_fault, hook="capture-failed")
        self.run_case(
            "P06-overlap",
            run_p06_overlap,
            provider_kwargs={"scenario": "hold", "provider_hold": True, "lifetime_ms": 5000},
        )
        self.run_case("P06-abandon", run_p06_abandon, hook="abandon-before-seal")
        self.run_case(
            "P07",
            run_p07,
            provider_kwargs={"scenario": "late_session", "provider_hold": True, "lifetime_ms": 5000},
        )
        self.run_case("B01-invalid-budget", run_b01_invalid_budget)
        self.run_case("B01-raw-argv", run_b01_raw_argv)
        self.run_case("B01-valid", run_b01_valid)
        self.run_case("B02", run_b02, budget="1s")
        self.run_case("B03", run_b03, provider_kwargs={"scenario": "hold", "provider_hold": True, "lifetime_ms": 5000})
        self.run_case(
            "B04",
            run_b04,
            provider_kwargs={"scenario": "parent_exit", "provider_hold": True, "child_lifetime_ms": 5000},
            budget="1s",
        )
        self.run_case("B05", run_b05, hook="start-delayed", budget="100ms")
        self.run_case("B06-complete-first", run_b06_complete_first)
        self.run_case(
            "B06-expire-first",
            run_b06_expire_first,
            provider_kwargs={"scenario": "parent_exit", "provider_hold": True, "child_lifetime_ms": 5000},
            budget="1s",
        )
        self.run_case(
            "B07",
            run_b07,
            hook="publication-delayed",
            provider_kwargs={"scenario": "hold", "provider_hold": True, "lifetime_ms": 5000},
            budget="100ms",
        )
        self.run_case("C01-mismatch", run_c01_mismatch)
        self.run_case("C02-valid", run_c02_valid)
        self.run_case("C02-mismatch", run_c02_mismatch)
        self.run_case("C03-queued", run_c03_queued)
        self.run_case("C03-transition", run_c03_transition)
        self.run_case("C04", run_c04)
        self.run_case("C05", run_c05)
        self.run_case("C06", run_c06)
        self.run_case("SESSION-CHAIN", run_session_chain)
        summary = self.write_summary()
        if summary["counts"]["fail"]:
            raise SystemExit(1)
        print(
            "PASS hermetic H: %d cases; A/P/B/C counts and independently started "
            "delegate-run evidence recorded" % summary["counts"]["pass"],
            flush=True,
        )

    def run_targeted(self, case_ids):
        controls = {
            "SESSION-CHAIN": run_session_chain,
            "SESSION-CHAIN-BASELINE": run_session_chain_baseline,
        }
        for case_id in case_ids:
            fn = controls.get(case_id)
            if fn is None:
                raise SystemExit("unknown targeted H case: " + case_id)
            self.run_case(case_id, fn)
        summary = self.write_summary()
        if summary["counts"]["fail"]:
            raise SystemExit(1)
        print(
            "PASS hermetic H targeted: %d cases; terminal continuation chain "
            "and natural provider/runner receipts recorded" % summary["counts"]["pass"],
            flush=True,
        )


def complete_success_case(case, provider_expected=0):
    case.dispatch_dynamic(status="queued")
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    response = case.finish_runner(runner, expected=(0,))
    case.set_status("ended")
    case.require_provider_evidence(provider_expected)
    case.require_terminal_evidence(response, provider_expected)
    return response


def assert_counts(case, **expected):
    counts = case.counts()
    for key, value in expected.items():
        require_true(counts.get(key) == value,
                     "%s count %s=%s, want %s" % (case.case_id, key, counts.get(key), value))
    return counts


def require_chain_session_release(case, record):
    """Prove a terminal continuation task released its predecessor claim."""
    matches = []
    for path in case.root.rglob("*.release.json"):
        try:
            value = read_json(path, MAX_CONTROL_BYTES)
        except (FileNotFoundError, json.JSONDecodeError):
            continue
        if value.get("task_id") == record["task_id"]:
            matches.append((path, value))
    require_true(len(matches) == 1,
                 "continuation release receipt count changed for " + record["label"])
    path, release = matches[0]
    require_true(
        release.get("schema_version") == 1 and
        release.get("root_id") == case.root_id and
        release.get("task_id") == record["task_id"] and
        release.get("provider") == PREDICATE and
        release.get("conversation_id") == record["session_id"] and
        isinstance(release.get("predecessor_evidence_sha256"), str) and
        len(release["predecessor_evidence_sha256"]) == 64 and
        release.get("predecessor_evidence_sha256") == record.get("outcome", {}).get("evidence_sha256") and
        release.get("released_at"),
        "continuation release receipt identity changed for " + record["label"],
    )
    record["session_release"] = {"path": str(path), **release}
    return release


def require_chain_task_links(case, records):
    require_true(len(records) == 3, "continuation chain did not create A/B/C records")
    expected_ids = [record["task_id"] for record in records]
    require_true(len(set(expected_ids)) == 3, "continuation chain reused a task ID")
    for index, record in enumerate(records):
        task_record = read_json(
            case.root / "tasks" / record["task_id"] / "task.json",
            MAX_CONTROL_BYTES,
        )
        require_true(
            task_record.get("root_id") == case.root_id and
            task_record.get("task_id") == record["task_id"] and
            task_record.get("provider") == PREDICATE and
            task_record.get("mode") == MODE,
            "continuation task request identity changed for " + record["label"],
        )
        prior = task_record.get("prior_session")
        if index == 0:
            require_true(prior is None, "initial chain task unexpectedly has a predecessor")
            continue
        previous = records[index - 1]
        require_true(
            prior == {
                "provider": PREDICATE,
                "conversation_id": record["session_id"],
                "predecessor_task_id": previous["task_id"],
            },
            "continuation predecessor link changed for " + record["label"],
        )


def require_chain_provider_receipts(case, records):
    invocations = case.provider_records / "invocations"
    paths = sorted(invocations.glob("*/provider.entry.json"))
    require_true(len(paths) == len(records),
                 "immutable provider invocation count changed for continuation chain")
    by_task = {}
    for path in paths:
        entry = read_json(path, MAX_CONTROL_BYTES)
        completion = read_json(path.parent / "provider.complete.json", MAX_CONTROL_BYTES)
        task_value = entry.get("task_id")
        require_true(task_value not in by_task,
                     "continuation chain launched a duplicate provider task")
        by_task[task_value] = (entry, completion)
        require_true(
            entry.get("invocation_id") == path.parent.name and
            entry.get("pid", 0) > 0 and
            completion.get("invocation_id") == entry.get("invocation_id") and
            completion.get("pid") == entry.get("pid") and
            completion.get("natural") is True and
            completion.get("exit_code") == 0,
            "provider natural completion receipt changed",
        )
    require_true(set(by_task) == {record["task_id"] for record in records},
                 "provider receipts do not cover exactly A/B/C")
    pids = {entry["pid"] for entry, _ in by_task.values()}
    require_true(len(pids) == len(records), "continuation chain reused a provider PID")
    for record in records:
        entry, completion = by_task[record["task_id"]]
        require_true(
            entry.get("session_id") == record["session_id"] and
            entry.get("cwd") == str(case.work) and
            completion.get("natural") is True,
            "provider session or natural completion identity changed for " + record["label"],
        )
        record["provider_entry"] = entry
        record["provider_completion"] = completion
    return by_task


def run_session_chain(case, expect_session_block=False):
    """Exercise public A -> B -> C terminal continuation ownership."""
    conversation = "phase2-chain-" + case.task_id
    records = []

    record_a = case.configure_chain_task(
        "A",
        "chain A answer: α\nliteral \\n Ω\n",
        conversation,
        scenario="success",
        lifetime_ms=250,
        task_value=case.task_id,
        brief=case.brief,
    )
    records.append(record_a)
    case.dispatch_dynamic(status="queued")
    case.set_status("running", label="delegate:" + case.root_id + ":" + record_a["task_id"])
    runner_a = case.start_runner_for(record_a)
    response_a = case.finish_runner_for(record_a, runner_a)
    require_publication(response_a, "committed")
    case.set_status("ended", label="delegate:" + case.root_id + ":" + record_a["task_id"])
    case.wait_provider_entry_for_task(record_a)
    case.snapshot_chain_provider(record_a)
    case.require_chain_terminal(record_a, response_a)

    record_b = case.configure_chain_task(
        "B",
        "chain B answer: β\nheld until release\n",
        conversation,
        scenario="hold",
        lifetime_ms=5000,
        provider_hold=True,
        task_value=task_id(),
    )
    records.append(record_b)
    case.dispatch_chain_task(record_b, resume_task=record_a["task_id"])
    case.set_status("running", label="delegate:" + case.root_id + ":" + record_b["task_id"])
    runner_b = case.start_runner_for(record_b)
    entry_b = case.wait_provider_entry_for_task(record_b)
    case.wait_chain_provider_hold(record_b, entry_b)
    require_true(
        not (case.root / "tasks" / record_b["task_id"] / "provider.exit").exists() and
        not (case.root / "tasks" / record_b["task_id"] / "outcome.json").exists(),
        "held continuation published before its provider natural exit",
    )
    case.release_provider()
    response_b = case.finish_runner_for(record_b, runner_b)
    require_publication(response_b, "committed")
    case.set_status("ended", label="delegate:" + case.root_id + ":" + record_b["task_id"])
    case.snapshot_chain_provider(record_b)
    case.require_chain_terminal(record_b, response_b)
    if expect_session_block:
        release_paths = [
            path for path in case.root.rglob("*.release.json")
            if read_json(path, MAX_CONTROL_BYTES).get("task_id") == record_b["task_id"]
        ]
        require_true(not release_paths,
                     "baseline unexpectedly released the B continuation claim")
    else:
        require_chain_session_release(case, record_b)

    record_c = case.configure_chain_task(
        "C",
        "chain C answer: γ\nthird terminal successor\n",
        conversation,
        scenario="success",
        lifetime_ms=250,
        provider_hold=False,
        task_value=task_id(),
    )
    records.append(record_c)
    if expect_session_block:
        if case.add_release.exists():
            case.add_release.unlink()
        blocked = case.processes.start(
            "dispatch-chain-C-session-busy",
            case.dispatch_argv(
                brief=record_c["brief"],
                task=record_c["task_id"],
                extra=("--resume-task", record_b["task_id"]),
            ),
            case.base,
        )
        blocked.wait(CASE_TIMEOUT, expected=(1,))
        blocked_response = blocked.json()
        require_true(
            "session" in blocked_response.get("error", "").lower() and
            "busy" in blocked_response.get("error", "").lower(),
            "baseline continuation refusal did not identify a busy session",
        )
        require_true(
            not any(
                entry.get("verb") == "add" and
                len(entry.get("argv", [])) > 5 and
                entry["argv"][5].endswith(":" + record_c["task_id"])
                for entry in case.entries()
            ),
            "baseline session refusal reached supervisor admission",
        )
        require_chain_task_links(case, records)
        require_chain_provider_receipts(case, records[:2])
        counts = assert_counts(case, A=2, S=2, E=2, child_E=0, K=0, M=0)
        verbs = [entry.get("verb") for entry in case.entries()]
        require_true(
            set(verbs).issubset({"add", "status", "--version"}) and
            verbs.count("add") == 2,
            "baseline session refusal made an unexpected supervisor call",
        )
        case.chain_summary = {
            "conversation_id": conversation,
            "task_ids": [record["task_id"] for record in records],
            "blocked_successor": record_c["task_id"],
            "blocked_error": blocked_response["error"],
            "natural_runner_waits": [record["runner_receipt"]["natural_wait"] for record in records[:2]],
            "natural_provider_completions": [record["provider_completion"]["natural"] for record in records[:2]],
        }
        return
    case.dispatch_chain_task(record_c, resume_task=record_b["task_id"])
    case.set_status("running", label="delegate:" + case.root_id + ":" + record_c["task_id"])
    runner_c = case.start_runner_for(record_c)
    response_c = case.finish_runner_for(record_c, runner_c)
    require_publication(response_c, "committed")
    case.set_status("ended", label="delegate:" + case.root_id + ":" + record_c["task_id"])
    case.wait_provider_entry_for_task(record_c)
    case.snapshot_chain_provider(record_c)
    case.require_chain_terminal(record_c, response_c)
    require_chain_session_release(case, record_c)

    require_chain_task_links(case, records)
    require_chain_provider_receipts(case, records)
    expected_outcomes = {
        record["task_id"]: record["outcome"] for record in records
    }
    for record in records:
        provider_ref_before = (case.root / "tasks" / record["task_id"] / "provider.ref.json").read_bytes()
        _, collected = case.run_command_for(
            record,
            "collect-chain-" + record["label"],
            "collect",
            expected=(0,),
        )
        _, collected_again = case.run_command_for(
            record,
            "collect-chain-repeat-" + record["label"],
            "collect",
            expected=(0,),
        )
        for value in (collected, collected_again):
            require_publication(value, "committed")
            require_true(
                value.get("task_id") == record["task_id"] and
                value.get("outcome") == expected_outcomes[record["task_id"]] and
                value.get("payload") == record["outcome"]["payload"],
                "terminal collection changed chain identity for " + record["label"],
            )
        provider_ref_after = (case.root / "tasks" / record["task_id"] / "provider.ref.json").read_bytes()
        require_true(provider_ref_after == provider_ref_before,
                     "terminal collection changed provider session evidence")

    counts = assert_counts(case, A=3, S=3, E=3, child_E=0, K=0, M=0)
    verbs = [entry.get("verb") for entry in case.entries()]
    require_true(
        set(verbs).issubset({"add", "status", "--version"}) and
        verbs.count("add") == 3,
        "continuation chain made an unexpected supervisor call",
    )
    case.chain_summary = {
        "conversation_id": conversation,
        "task_ids": [record["task_id"] for record in records],
        "labels": [record["label"] for record in records],
        "natural_runner_waits": [record["runner_receipt"]["natural_wait"] for record in records],
        "natural_provider_completions": [record["provider_completion"]["natural"] for record in records],
    }


def run_session_chain_baseline(case):
    """Record the pre-fix negative control against an old helper binary set."""
    run_session_chain(case, expect_session_block=True)


def require_supervisor_argv(case, verb, expected):
    matches = [entry for entry in case.entries() if entry.get("verb") == verb]
    require_true(len(matches) == 1,
                 "expected one fake supervisor %s entry, found %d" % (verb, len(matches)))
    require_true(matches[0].get("argv") == expected,
                 "fake supervisor %s argv changed: %s" % (verb, matches[0].get("argv")))
    return matches[0]


def require_publication(response, publication, code=None):
    require_true(response.get("publication") == publication,
                 "unexpected publication: " + json.dumps(response, sort_keys=True))
    if code is not None:
        require_true(response.get("schema_version") == 1, "unversioned response")


def require_live_raw_descriptors(case, response):
    """Check F11's bounded, observation-only live stream descriptors."""
    raw = response.get("raw")
    require_true(isinstance(raw, list) and len(raw) == 2,
                 "pending response omitted live raw descriptors")
    for descriptor, name in zip(raw, ("stderr", "stdout")):
        require_true(isinstance(descriptor, dict), "raw descriptor is not an object")
        expected_path = str(case.task_dir() / "raw" / name)
        require_true(descriptor.get("path") == expected_path,
                     "raw descriptor path escaped task root")
        require_true(Path(descriptor["path"]).is_absolute(),
                     "raw descriptor path is not absolute")
        require_true(descriptor.get("available") is True,
                     "existing live raw stream was marked unavailable")
        require_true(descriptor.get("sealed") is False,
                     "pending response invented a sealed stream")
        require_true("size" not in descriptor and "sha256" not in descriptor,
                     "pending response invented final raw identity")


def run_a01(case):
    process = case.start_dispatch()
    entry = case.wait_entry("add")
    case.root_id, _ = label_parts(entry["argv"][5])
    require_true(not (case.supervisor_records / (entry["invocation_id"] + ".completion.json")).exists(),
                 "withheld add completed before observation bound")
    process.wait(OBSERVATION_BOUND + 1.0, expected=(1,))
    pending = process.json()
    require_true(pending.get("admission") in ("unknown", "admitted"),
                 "ambiguous add was converted to a different admission")
    require_true(pending.get("pending") is not None or pending.get("error"),
                 "ambiguous add returned no uncertainty evidence")
    require_true((case.root / "tasks" / case.task_id / "submit.json").exists(),
                 "submit intent was not durable")
    case.set_status("queued")
    case.release_add()
    # The add waiter belongs to the dispatcher process.  Once the bounded
    # caller observation returns, the harness cannot claim ownership of that
    # internal Wait; record whether a completion receipt is eventually visible
    # and base admission only on the durable retry observation.
    case.observe_helper_completion_if_present(entry)
    _, retry = case.retry_dispatch()
    require_publication(retry, "unknown")
    require_true(retry.get("admission") == "admitted", "retry did not attach to one add")
    case.set_status("running")
    runner = case.start_runner()
    run_response = case.finish_runner(runner, expected=(0,))
    case.set_status("ended")
    case.require_provider_evidence()
    require_true(run_response.get("publication") == "committed", "A01 runner did not commit")
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_a02_fixture(case, fixture):
    process = case.start_dispatch()
    entry = case.wait_entry("add")
    case.root_id, _ = label_parts(entry["argv"][5])
    if fixture == "empty":
        case.set_empty_status()
    elif fixture == "duplicate":
        case.set_status("queued", duplicate=True)
    else:
        case.set_malformed_status()
    case.release_add()
    process.wait(CASE_TIMEOUT, expected=(1,))
    first = process.json()
    require_true(first.get("admission") == "unknown", "ambiguous status established admission")
    require_true((case.root / "tasks" / case.task_id / "submit.json").exists(),
                 "ambiguous status lost submit intent")
    if fixture == "empty":
        case.set_empty_status()
    elif fixture == "duplicate":
        case.set_status("queued", duplicate=True)
    else:
        case.set_malformed_status()
    _, second = case.retry_dispatch(expected=(1,))
    require_true(second.get("admission") == "unknown", "repeated ambiguous status changed fact")
    case.set_status("queued")
    _, attached = case.retry_dispatch(expected=(0,))
    require_true(attached.get("admission") == "admitted", "unique observation did not attach")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_a02_empty(case):
    run_a02_fixture(case, "empty")


def run_a02_duplicate(case):
    run_a02_fixture(case, "duplicate")


def run_a02_malformed(case):
    run_a02_fixture(case, "malformed")


def run_a03_config(case):
    case.dispatch_dynamic()
    original = case.production_config.read_bytes()
    write_bytes(case.production_config, original + b"# changed after binding\n", replace=True)
    _, status = case.run_command("status-changed-config", "status", expected=(1,))
    require_true(status.get("admission") != "admitted" or status.get("error"),
                 "changed config established admission")
    _, cancel = case.run_command("cancel-changed-config", "cancel", expected=(1,))
    require_true((case.task_dir() / "stop").exists(), "stop request was not durable")
    require_true(cancel.get("stop") is None or cancel.get("error"),
                 "changed config authorized stop")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_a03_label(case):
    case.dispatch_dynamic()
    case.set_status("running", label="delegate:" + case.root_id + ":" + uuid.uuid4().hex)
    _, status = case.run_command("status-wrong-label", "status", expected=(0,))
    require_true(status.get("admission") == "unknown", "wrong label was admitted")
    _, cancel = case.run_command("cancel-wrong-label", "cancel", expected=(1,))
    require_true(cancel.get("error"), "wrong label cancellation lacked uncertainty")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_a04_invalid_config(case):
    missing = case.base / "missing pueue.yml"
    _, response = case.dispatch_without_admission(
        pueue_config=missing,
        expected=(2,),
    )
    require_true(response.get("error"), "missing config did not produce diagnostic")
    assert_counts(case, A=0, S=0, E=0, K=0, M=0)


def run_a04_symlink_config(case):
    link = case.base / "config symlink.yml"
    link.symlink_to(case.production_config)
    _, response = case.dispatch_without_admission(pueue_config=link, expected=(2,))
    require_true(response.get("error"), "symlink config did not refuse")
    assert_counts(case, A=0, S=0, E=0, K=0, M=0)


def run_a04_valid(case):
    case.dispatch_dynamic()
    case.require_bound_records()
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_a04_saved_binding(case):
    """Fresh commands must ignore an unrelated ambient config path."""
    case.dispatch_dynamic()
    case.set_status("queued")
    ambient_root = case.base / "ambient"
    ambient_root.mkdir(mode=0o700)
    ambient_config = ambient_root / "unrelated pueue.yml"
    write_bytes(ambient_config, private_pueue_yaml(ambient_root))
    environment = inherited_environment()
    environment["DELEGATE_PUEUE_CONFIG"] = str(ambient_config)
    _, status = case.run_command_with_environment(
        "status-ambient-config", "status", environment, expected=(0,)
    )
    require_true(status.get("admission") == "admitted",
                 "saved binding status did not remain admitted")
    _, collected = case.run_command_with_environment(
        "collect-ambient-config", "collect", environment, expected=(3,)
    )
    require_true(collected.get("publication") in ("pending", "unknown"),
                 "saved binding collect changed pending state")
    _, cancel = case.run_command_with_environment(
        "cancel-ambient-config", "cancel", environment, expected=(0,)
    )
    require_true(cancel.get("stop", {}).get("action") == "remove",
                 "saved binding cancel did not target queued task")
    for entry in case.entries():
        require_true(entry.get("config_path") == str(case.production_config),
                     "fresh command consulted ambient supervisor config")
    assert_counts(case, A=1, S=0, E=0, K=0, M=1)


def run_a05_concurrent(case):
    first = case.start_dispatch()
    entry = case.wait_entry("add")
    case.root_id, _ = label_parts(entry["argv"][5])
    case.set_status("queued")
    # Keep the first add in flight while the second caller reaches the same
    # durable intent.  This proves independent callers do not serialize on a
    # process-local lock, while the fake add still admits only one request.
    second = case.start_dispatch()
    second.wait(CASE_TIMEOUT, expected=(0, 1, 2))
    second.json()
    case.release_add()
    first.wait(CASE_TIMEOUT, expected=(0,))
    first.json()
    brief = case.base / "changed.md"
    write_bytes(brief, b"changed immutable request\n")
    _, conflict = case.retry_dispatch(brief=brief, expected=(2,))
    require_true("conflict" in conflict.get("error", "").lower(),
                 "different same-ID request was not rejected")
    require_true(first.pid != second.pid, "concurrent controls did not use distinct PIDs")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_a06(case):
    case.update_fake("add", release_path=str(case.add_release))
    process = case.start_dispatch()
    entry = case.wait_entry("add")
    case.root_id, _ = label_parts(entry["argv"][5])
    process.wait(OBSERVATION_BOUND + 1.0, expected=(1,))
    response = process.json()
    require_true(response.get("pending") is not None or response.get("error"),
                 "bounded add observation had no in-flight evidence")
    require_true(not (case.supervisor_records / (entry["invocation_id"] + ".completion.json")).exists(),
                 "delayed add completed before caller returned")
    case.set_status("queued")
    case.release_add()
    case.observe_helper_completion_if_present(entry)
    _, retry = case.retry_dispatch(expected=(0,))
    require_true(retry.get("admission") == "admitted", "late add was discarded or replayed")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_p01(case):
    response = complete_success_case(case)
    require_publication(response, "committed")
    case.require_provider_evidence()
    task_dir = case.task_dir()
    require_true((task_dir / "raw" / "stdout").read_bytes() ==
                 (case.provider_records / "expected.stdout").read_bytes(),
                 "stdout bytes changed in transport")
    require_true((task_dir / "raw" / "stderr").read_bytes() ==
                 (case.provider_records / "expected.stderr").read_bytes(),
                 "stderr bytes changed in transport")
    require_true(not (case.work / "DO_NOT_CREATE").exists(),
                 "shell-looking literal was executed")
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p02(case):
    case.provider_settings["stdout_bytes"] = 2 * 1024 * 1024 + 17
    case.provider_settings["stderr_bytes"] = 2 * 1024 * 1024 + 29
    write_json(case.provider_config, case.provider_settings, replace=True)
    # Profile digest must follow the exact provider-config bytes.
    profile = read_json(case.profile_config)
    profile["provider_config_sha256"] = digest(case.provider_config)
    write_json(case.profile_config, profile, replace=True)
    response = complete_success_case(case)
    require_publication(response, "committed")
    raw_stdout = case.task_dir() / "raw" / "stdout"
    raw_stderr = case.task_dir() / "raw" / "stderr"
    require_true(raw_stdout.stat().st_size > 2 * 1024 * 1024, "stdout did not exercise backpressure")
    require_true(raw_stderr.stat().st_size > 2 * 1024 * 1024, "stderr did not exercise backpressure")
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p03(case):
    scenario = case.provider_settings["scenario"]
    expected_provider_exit = 7 if scenario == "nonzero" else 0
    response = complete_success_case(case, provider_expected=expected_provider_exit)
    if scenario == "success":
        require_publication(response, "committed")
        expected_code = 0
    else:
        require_publication(response, "rejected")
        expected_code = 4
        _, collected = case.run_command("collect-" + scenario, "collect", expected=(4,))
        require_true(collected.get("publication") == "rejected",
                     "semantic refusal was not preserved by collect")
        outcome = collected.get("outcome") or {}
        refusal = outcome.get("refusal", "")
        prefixes = {
            "empty": "empty_answer",
            "diagnostic": "stderr_error",
            "malformed": "malformed_output",
            "truncated": "malformed_output",
            "nonzero": "provider_exit",
        }
        payload = response.get("payload") or {}
        basename = payload.get("basename")
        require_true(isinstance(basename, str) and basename,
                     "semantic refusal omitted payload descriptor")
        refusal_text = (case.task_dir() / basename).read_text()
        require_true(refusal_text.startswith(prefixes[scenario]),
                     "wrong refusal reason: " + refusal_text)
        case.require_provider_evidence(expected_provider_exit)
    require_true(expected_code in (0, 4), "invalid semantic code mapping")
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p04(case):
    case.dispatch_dynamic(status="queued")
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    case.wait_event("wait-completed")
    require_true(not (case.task_dir() / "provider.exit").exists(),
                 "parent Wait fabricated a seal while child held raw pipe")
    require_true(not (case.task_dir() / "outcome.json").exists(),
                 "parent Wait fabricated an outcome while child held raw pipe")
    child_entry = case.provider_records / "child.provider.entry.json"
    child_complete = case.provider_records / "child.provider.complete.json"
    require_true(child_entry.exists() and not child_complete.exists(),
                 "held-pipe observation missed the child completion boundary")
    case.release_provider()
    response = case.finish_runner(runner, expected=(0,))
    require_publication(response, "committed")
    case.require_provider_evidence()
    child_entry = case.provider_records / "child.provider.entry.json"
    require_true(child_entry.exists() and child_complete.exists(),
                 "finite pipe-owning child receipt is missing")
    require_true(case.counts()["child_E"] == 1, "unexpected child provider count")
    case.require_terminal_evidence(response)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p05_start(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0,))
    response = case.finish_runner(runner, expected=(0,))
    require_publication(response, "rejected")
    task_dir = case.task_dir()
    seal = read_json(task_dir / "provider.exit", MAX_CONTROL_BYTES)
    outcome = read_json(task_dir / "outcome.json", MAX_CONTROL_BYTES)
    require_true(seal["invocation_state"] == "start_failed" and seal["exit_code"] == 1,
                 "definite Start failure did not produce a start_failed seal")
    require_true(not (task_dir / "raw" / "stdout").read_bytes() and
                 (task_dir / "raw" / "stderr").read_bytes(),
                 "Start failure diagnostics were not durably captured")
    manifest = []
    for name in ("stderr", "stdout"):
        data = (task_dir / "raw" / name).read_bytes()
        manifest.append({"path": "raw/" + name, "size": len(data), "sha256": sha(data)})
    require_true(seal["raw_manifest"] == manifest and
                 seal["manifest_sha256"] == sha(canonical_json_bytes(manifest)),
                 "Start failure raw seal changed")
    payload_name = outcome["payload"]["basename"]
    payload = (task_dir / payload_name).read_text()
    require_true(payload.startswith("start_failed: "),
                 "Start failure predicate refusal reason changed")
    require_true(response.get("outcome") == outcome and
                 response.get("payload") == outcome["payload"],
                 "Start failure response lost terminal identity")
    require_true(not (case.provider_records / "provider.entry.json").exists(),
                 "provider entered after definite Start failure")
    retry_runner = case.start_runner(expected=(0, 1), name="retry-start-failed")
    retry = case.finish_runner(retry_runner, expected=(0, 1))
    require_true(retry.get("task_id") == case.task_id, "start failure task lost")
    values = case.runner_execution_sequence()
    names = [value.get("name") for value in values]
    require_true("start-entry" in names and "start-failed" in names and
                 "sealed" in names and "published" in names,
                 "Start failure event sequence was incomplete")
    require_true(names.index("start-entry") < names.index("start-failed") <
                 names.index("sealed") < names.index("published"),
                 "Start failure events are out of order")
    assert_counts(case, A=1, S=1, E=0, K=0, M=0)


def run_p05_fault(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(1,))
    response = case.finish_runner(runner, expected=(1,))
    require_true(response.get("error"), "infrastructure failure was swallowed")
    require_true(not (case.task_dir() / "provider.exit").exists(),
                 "infrastructure failure fabricated a seal")
    case.require_provider_evidence()
    for name in ("stderr", "stdout"):
        require_true(
            (case.task_dir() / "raw" / name).read_bytes()
            == case.expected_raw_files()[name].read_bytes(),
            "infrastructure fault changed raw " + name,
        )
    event_names = [
        value.get("name") for value in case.runner_execution_sequence()
    ]
    for name in ("start-entry", "started", "parent-fds-closed",
                 "started-receipt", "wait-completed",
                 "stdout-raw-closed", "stderr-raw-closed",
                 "completion-observed", "timer-disarmed"):
        require_true(name in event_names, "fault event missing: " + name)
    require_true(
        ("stdout-eof" in event_names or "stdout-drained-after-error" in event_names) and
        ("stderr-eof" in event_names or "stderr-drained-after-error" in event_names),
        "fault path did not observe both capture drains",
    )
    require_true("sealed" not in event_names and "published" not in event_names,
                 "fault path fabricated terminal events")
    retry = case.start_runner(expected=(1,), name="retry-runner")
    retry_response = case.finish_runner(retry, expected=(1,))
    require_true(retry_response.get("error"), "retry bypassed existing start guard")
    counts = case.counts()
    require_true(counts["S"] == 1 and counts["E"] == 1, "fault retry launched provider twice")
    require_true(counts["A"] == 1 and counts["K"] == 0 and counts["M"] == 0,
                 "fault path mutated supervisor")


def run_p06_overlap(case):
    case.dispatch_dynamic()
    case.set_status("running")
    first = case.start_runner(expected=(0, 1), name="runner-a")
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    second = case.start_runner(expected=(1,), name="runner-b")
    second_response = case.finish_runner(second, expected=(1,))
    require_true(second_response.get("error"), "overlapping runner acquired start")
    require_true(first.pid != second.pid and
                 not (case.task_dir() / "provider.exit").exists() and
                 not (case.task_dir() / "outcome.json").exists(),
                 "overlapping runner changed terminal authority")
    case.release_provider()
    first_response = case.finish_runner(first, expected=(0,))
    require_publication(first_response, "committed")
    case.set_status("ended")
    case.require_provider_evidence()
    case.require_terminal_evidence(first_response)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p06_abandon(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(73,))
    case.wait_path(case.provider_records / "provider.entry.json")
    # The abandon hook exits immediately after observing completion, before
    # the runner can publish a JSON response.  Observe its natural exit
    # directly; an empty stdout stream is part of this fault's evidence.
    runner.wait(CASE_TIMEOUT, expected=(73,))
    require_true(not (case.task_dir() / "provider.exit").exists(),
                 "abandon-before-seal fabricated a seal")
    retry = case.start_runner(expected=(1,), name="retry-after-abandon")
    retry_response = case.finish_runner(retry, expected=(1,))
    require_true(retry_response.get("error"), "abandoned task relaunched")
    case.require_provider_evidence()
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_p07(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    metadata_path = case.task_dir() / "meta.json"
    metadata_before = metadata_path.read_bytes()
    provider_ref_path = case.task_dir() / "provider.ref.json"
    require_true(not provider_ref_path.exists(),
                 "late session identity appeared before provider evidence")
    _, pending = case.run_command("collect-late-pending", "collect", expected=(3,))
    require_true(pending.get("publication") in ("pending", "unknown"),
                 "early collect fabricated publication")
    require_live_raw_descriptors(case, pending)
    _, status = case.run_command("status-late", "status", expected=(0,))
    require_true(not (case.task_dir() / "provider.ref.json").exists(),
                 "late session identity was recorded before provider session evidence")
    require_true(status.get("publication") in ("unknown", "pending"),
                 "late session status fabricated publication")
    _, logs = case.run_command("logs-late", "logs", expected=(0,))
    require_live_raw_descriptors(case, logs)
    case.release_provider()
    final = case.finish_runner(runner, expected=(0,))
    require_publication(final, "committed")
    case.set_status("ended")
    require_true(provider_ref_path.exists(),
                 "late session identity was not recorded")
    metadata_after = metadata_path.read_bytes()
    require_true(metadata_after == metadata_before,
                 "late session identity overwrote immutable metadata")
    provider_ref = read_json(provider_ref_path, MAX_CONTROL_BYTES)
    provider_config = read_json(case.provider_config, MAX_CONTROL_BYTES)
    require_true(
        provider_ref == {
            "meta_sha256": sha(metadata_before),
            "provider": PREDICATE,
            "observed_at": provider_ref["observed_at"],
            "schema_version": 1,
            "root_id": case.root_id,
            "task_id": case.task_id,
            # task.json is the immutable request; TaskRecord deliberately has
            # no self-referential spec_sha256 field.  The production binding
            # uses the digest of its exact canonical bytes.
            "spec_sha256": sha((case.task_dir() / "task.json").read_bytes()),
            "conversation_id": provider_config["session_id"],
        },
        "late provider identity changed session or immutable binding",
    )
    _, collected = case.run_command("collect-late-final", "collect", expected=(0,))
    _, collected_again = case.run_command("collect-late-repeat", "collect", expected=(0,))
    require_publication(collected, "committed")
    require_publication(collected_again, "committed")
    for value in (collected, collected_again):
        require_true(
            value.get("outcome") == final.get("outcome") and
            value.get("payload") == final.get("payload") and
            value.get("evidence_sha256") == final.get("evidence_sha256"),
            "repeated collection changed the sealed winner",
        )
    case.require_provider_evidence()
    case.require_terminal_evidence(final)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_b01_invalid_budget(case):
    for invalid in ("0s", "-1s", "999999999999999999999999h"):
        _, response = case.dispatch_without_admission(budget=invalid, expected=(2,))
        require_true(response.get("error"), "invalid budget lacked diagnostic")
    for extra in (("--token-budget", "10"), ("--dollar-budget", "10")):
        _, response = case.dispatch_without_admission(extra=extra, expected=(2,))
        require_true(response.get("error"), "unsupported ceiling lacked diagnostic")
    process = case.processes.start(
        "invalid-watch",
        case.command_argv("collect", extra=("--watch", "-1ns")),
        case.base,
    )
    process.wait(CASE_TIMEOUT, expected=(2,))
    require_true((process.directory / "stdout").stat().st_size == 0 and
                 "watch" in (process.directory / "stderr").read_text().lower(),
                 "invalid watch lacked parse diagnostic")
    oversized = case.base / "oversized brief.md"
    write_bytes(oversized, b"x" * (8 * 1024 * 1024 + 1))
    _, response = case.dispatch_without_admission(brief=oversized, expected=(2,))
    require_true(response.get("error"), "oversized brief lacked diagnostic")
    _, response = case.dispatch_without_admission(
        provider="codex:exec", expected=(2,)
    )
    require_true(response.get("error"), "native profile was admitted by fixture CLI")
    assert_counts(case, A=0, S=0, E=0, K=0, M=0)


def run_b01_raw_argv(case):
    process = case.processes.start(
        "raw-argv",
        case.dispatch_argv(extra=("--", "inert-provider-argv")),
        case.base,
    )
    process.wait(CASE_TIMEOUT, expected=(2,))
    require_true((process.directory / "stdout").stat().st_size == 0,
                 "raw argv refusal unexpectedly published a response")
    require_true("raw argv" in (process.directory / "stderr").read_text().lower(),
                 "raw argv refusal lacked diagnostic")
    assert_counts(case, A=0, S=0, E=0, K=0, M=0)


def run_b01_valid(case):
    process = case.start_dispatch(omit_budget=True)
    entry = case.wait_entry("add")
    case.root_id, _ = label_parts(entry["argv"][5])
    case.set_status("queued")
    case.release_add()
    process.wait(CASE_TIMEOUT, expected=(0,))
    response = process.json()
    require_true(response.get("admission") == "admitted",
                 "omitted budget dispatch was not admitted")
    task = read_json(case.task_dir() / "task.json", MAX_CONTROL_BYTES)
    require_true(task.get("budget_nanos") == 30 * 60 * 1_000_000_000 and
                 task.get("requested_config", {}).get("budget") == "30m0s",
                 "omitted budget was not persisted as the 30m default")
    started = time.monotonic()
    _, pending = case.run_command("default-watch", "collect", expected=(3,))
    require_true(time.monotonic() - started < 1.0 and pending.get("pending") is not None,
                 "default watch did not remain nonblocking")
    case.require_bound_records()
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_b02(case):
    case.update_fake("add", delay=1_500_000_000)
    process = case.start_dispatch()
    entry = case.wait_entry("add")
    case.root_id, _ = label_parts(entry["argv"][5])
    case.set_status("queued")
    queued_at = time.monotonic()
    time.sleep(1.1)
    require_true(not (case.task_dir() / "stop" / "budget.request.json").exists(),
                 "queued time armed a budget stop")
    require_true("start-entry" not in case.event_names() and
                 not (case.provider_records / "provider.entry.json").exists(),
                 "queued time started provider execution")
    case.release_add()
    process.wait(CASE_TIMEOUT, expected=(0,))
    require_true(time.monotonic() - queued_at > 1.0,
                 "queued observation did not exceed the recorded budget")
    response = process.json()
    require_true(response.get("admission") == "admitted" and
                 response.get("publication") == "unknown",
                 "queued dispatch returned an unexpected publication")
    case.require_bound_records()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    response = case.finish_runner(runner, expected=(0,))
    require_publication(response, "committed")
    case.set_status("ended")
    case.require_provider_evidence()
    case.require_terminal_evidence(response)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_b03(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    _, pending = case.run_command("short-watch", "collect", expected=(3,), extra=("--watch", "10ms"))
    require_true(pending.get("publication") in ("pending", "unknown"),
                 "watch expiry changed task state")
    require_live_raw_descriptors(case, pending)
    case.release_provider()
    final = case.finish_runner(runner, expected=(0,))
    require_publication(final, "committed")
    case.require_provider_evidence()
    case.require_terminal_evidence(final)
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_b04(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    case.wait_path(case.provider_records / "child.provider.entry.json")
    child_complete = case.provider_records / "child.provider.complete.json"
    case.wait_event("wait-completed")
    case.wait_event("deadline-observed")
    names = case.event_names()
    require_true(names.index("wait-completed") < names.index("deadline-observed"),
                 "budget expiry did not observe parent Wait first")
    require_true(not child_complete.exists(),
                 "budget case lost held-child boundary before stop request")
    require_true(not (case.task_dir() / "provider.exit").exists(),
                 "budget case sealed before held child released")
    case.wait_path(case.task_dir() / "stop" / "budget.request.json", timeout=5)
    kill = case.wait_entry("kill")
    require_true(not child_complete.exists(),
                 "budget stop was observed after child pipe had already closed")
    case.release_provider()
    for event_name in ("stdout-eof", "stderr-eof", "stdout-raw-closed", "stderr-raw-closed"):
        case.wait_event(event_name)
    names = case.event_names()
    require_true(
        names.index("deadline-observed") < names.index("stdout-eof") <
        names.index("stdout-raw-closed") and
        names.index("deadline-observed") < names.index("stderr-eof") <
        names.index("stderr-raw-closed"),
        "budget expiry did not precede both capture drains",
    )
    response = case.finish_runner(runner, expected=(0, 1))
    require_true((case.task_dir() / "stop" / "budget.request.json").exists(),
                 "budget request was not persisted before stop")
    require_true(response.get("publication") in ("committed", "rejected", "unknown"),
                 "budget path returned an invalid publication")
    case.require_provider_evidence()
    require_true(child_complete.exists(), "held child did not complete naturally")
    assert_counts(case, child_E=1)
    require_true(kill["argv"] == ["-c", str(case.production_config), "kill", str(case.numeric_id)],
                 "budget stop used an unexpected supervisor argv")
    case.require_terminal_evidence(response)
    assert_counts(case, A=1, S=1, E=1, K=1, M=0)


def run_b05(case):
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_event("start-entry")
    case.wait_path(case.task_dir() / "stop" / "budget.request.json", timeout=5)
    require_true(case.start_release is not None, "start delayed case lacks release")
    case.release(case.start_release)
    response = case.finish_runner(runner, expected=(0, 1))
    require_true(response.get("task_id") == case.task_id, "delayed Start lost task")
    case.require_provider_evidence()
    assert_counts(case, A=1, S=1, E=1, K=1, M=0)


def run_b06_complete_first(case):
    response = complete_success_case(case)
    require_publication(response, "committed")
    assert_counts(case, A=1, S=1, E=1, K=0, M=0)


def run_b06_expire_first(case):
    run_b04(case)


def run_b07(case):
    case.update_fake("kill", release_path=str(case.kill_release))
    case.dispatch_dynamic()
    case.set_status("running")
    runner = case.start_runner(expected=(0, 1))
    case.wait_path(case.provider_records / "provider.entry.json")
    case.wait_provider_hold()
    case.wait_path(case.task_dir() / "stop" / "budget.request.json", timeout=5)
    kill = case.wait_entry("kill")
    require_true(not (case.supervisor_records / (kill["invocation_id"] + ".completion.json")).exists(),
                 "delayed stop reply completed before publication")
    # The provider owns its natural completion.  Release its finite hold so
    # the winner can publish while the stop acknowledgement remains delayed.
    case.release_provider()
    case.wait_path(case.task_dir() / "outcome.json", timeout=5)
    require_true(not (case.supervisor_records / (kill["invocation_id"] + ".completion.json")).exists(),
                 "delayed stop reply completed before natural publication")
    case.release(case.kill_release)
    case.wait_supervisor_completion(kill, timeout=5)
    require_true(case.publication_release is not None,
                 "publication-delayed case lacks release rendezvous")
    case.release(case.publication_release)
    response = case.finish_runner(runner, expected=(0, 1))
    require_true((case.task_dir() / "outcome.json").exists(),
                 "natural provider publication was blocked by stop reply")
    case.require_provider_evidence()
    case.require_terminal_evidence(response)
    require_true(response.get("publication") in ("committed", "rejected", "unknown"),
                 "late stop returned malformed response")
    assert_counts(case, A=1, S=1, E=1, K=1, M=0)


def run_c01_mismatch(case):
    case.dispatch_dynamic()
    case.set_empty_status()
    _, response = case.run_command("cancel-unmatched", "cancel", expected=(1,))
    require_true((case.task_dir() / "stop").exists(), "unmatched stop request disappeared")
    require_true(response.get("error"), "unmatched stop lacked unknown result")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_c02_valid(case):
    case.dispatch_dynamic()
    case.set_status("running")
    _, response = case.run_command("cancel-running", "cancel", expected=(0,))
    stop = response.get("stop")
    require_true(isinstance(stop, dict), "valid running cancel lacked stop response")
    require_true(
        stop.get("requested") is True and stop.get("matched") is True and
        stop.get("attempted") is True and stop.get("action") == "kill" and
        stop.get("numeric_task_id") == case.numeric_id and
        stop.get("acknowledged") is True and stop.get("terminated") is False,
        "valid running stop facts were not distinct",
    )
    require_supervisor_argv(
        case, "kill",
        ["-c", str(case.production_config), "kill", str(case.numeric_id)],
    )
    assert_counts(case, A=1, S=0, E=0, K=1, M=0)


def run_c02_mismatch(case):
    case.dispatch_dynamic()
    case.set_status("running", numeric_id=case.numeric_id + 1)
    _, response = case.run_command("cancel-wrong-id", "cancel", expected=(1,))
    require_true(response.get("error"), "mismatched numeric stop did not remain unknown")
    assert_counts(case, A=1, S=0, E=0, K=0, M=0)


def run_c03_queued(case):
    case.dispatch_dynamic()
    case.set_status("queued")
    _, response = case.run_command("cancel-queued", "cancel", expected=(0,))
    stop = response.get("stop")
    require_true(isinstance(stop, dict), "queued cancellation lacked stop response")
    require_true(
        stop.get("requested") is True and stop.get("matched") is True and
        stop.get("attempted") is True and stop.get("action") == "remove" and
        stop.get("numeric_task_id") == case.numeric_id and
        stop.get("acknowledged") is True and stop.get("terminated") is False,
        "queued removal facts were not distinct",
    )
    require_supervisor_argv(
        case, "remove",
        ["-c", str(case.production_config), "remove", str(case.numeric_id)],
    )
    assert_counts(case, A=1, S=0, E=0, K=0, M=1)


def run_c03_transition(case):
    case.dispatch_dynamic()
    case.set_status("queued")
    case.update_fake("remove", release_path=str(case.remove_release), exit_code=7)
    process = case.processes.start(
        "cancel-transition",
        case.command_argv("cancel"),
        case.base,
    )
    remove = case.wait_entry("remove")
    case.set_status("running")
    case.release(case.remove_release)
    process.wait(CASE_TIMEOUT, expected=(1,))
    response = process.json()
    require_true(response.get("error"), "refused queue removal was reported as success")
    stop = response.get("stop")
    require_true(
        isinstance(stop, dict) and stop.get("action") == "remove" and
        stop.get("acknowledged") is False and stop.get("terminated") is False,
        "refused queue removal changed stop facts",
    )
    require_true(remove["argv"] == [
        "-c", str(case.production_config), "remove", str(case.numeric_id)
    ], "queue transition used an unexpected remove argv")
    assert_counts(case, A=1, S=0, E=0, K=0, M=1)


def run_c04(case):
    case.dispatch_dynamic()
    case.set_status("running")
    _, cancel = case.run_command("cancel-ack", "cancel", expected=(0,))
    require_true((case.task_dir() / "stop").exists(), "acknowledged stop request absent")
    stop = cancel.get("stop")
    require_true(
        isinstance(stop, dict) and stop.get("requested") is True and
        stop.get("matched") is True and stop.get("acknowledged") is True and
        stop.get("terminated") is False and stop.get("observed_state") == "running" and
        stop.get("numeric_task_id") == case.numeric_id and stop.get("action") == "kill",
        "acknowledgment was conflated with termination",
    )
    request_id = stop["request_id"]
    request = case.stop_record(request_id)
    reply = read_json(case.task_dir() / "stop" / (request_id + ".reply.json"), MAX_CONTROL_BYTES)
    require_true(request["request_id"] == request_id and request["cause"] == "user" and
                 request["numeric_task_id"] == case.numeric_id,
                 "stop request identity was not durable")
    require_true(reply["request_id"] == request_id and reply["acknowledged"] is True,
                 "stop acknowledgment reply was not durable")
    require_true(not (case.task_dir() / "stop" / (request_id + ".observed.json")).exists(),
                 "termination observation was fabricated at acknowledgment")
    case.set_status("ended")
    _, status = case.run_command("status-ended-stop", "status", expected=(0,))
    status_stops = status.get("stops") or []
    observed = next((item for item in status_stops if item.get("request_id") == request_id), None)
    require_true(
        observed is not None and observed.get("acknowledged") is True and
        observed.get("terminated") is True and observed.get("observed_state") == "ended",
        "positive ended observation did not update the same stop request",
    )
    observed_record = read_json(
        case.task_dir() / "stop" / (request_id + ".observed.json"), MAX_CONTROL_BYTES
    )
    require_true(
        observed_record["request_id"] == request_id and
        observed_record["task_id"] == case.task_id and
        observed_record["state"] == "ended" and
        observed_record["terminated"] is True,
        "ended observation record changed scope",
    )
    _, collected = case.run_command("collect-without-seal", "collect", expected=(3,))
    require_true(collected.get("outcome") is None, "stop evidence fabricated outcome")
    assert_counts(case, A=1, S=0, E=0, K=1, M=0)


def run_c05(case):
    case.update_fake("kill", exit_code=7)
    case.dispatch_dynamic()
    case.set_status("running")
    _, cancel = case.run_command("cancel-refused", "cancel", expected=(1,))
    require_true(cancel.get("error"), "failed stop lacked operational error")
    case.set_status("running")
    runner = case.start_runner(expected=(0,))
    response = case.finish_runner(runner, expected=(0,))
    require_publication(response, "committed")
    case.require_terminal_evidence(response)
    task_dir = case.task_dir()
    outcome_before = (task_dir / "outcome.json").read_bytes()
    payload_descriptor = read_json(task_dir / "outcome.json", MAX_CONTROL_BYTES)["payload"]
    payload_path = task_dir / payload_descriptor["basename"]
    payload_before = payload_path.read_bytes()
    case.set_status("ended")
    _, terminal = case.run_command("cancel-terminal", "cancel", expected=(0,))
    require_publication(terminal, "committed")
    require_true((task_dir / "outcome.json").read_bytes() == outcome_before,
                 "terminal cancellation changed the sealed outcome")
    require_true(payload_path.read_bytes() == payload_before,
                 "terminal cancellation changed the sealed payload")
    case.require_terminal_evidence(terminal)
    case.require_provider_evidence()
    assert_counts(case, A=1, S=1, E=1, K=1, M=0)


def run_c06(case):
    case.dispatch_dynamic()
    case.set_empty_status()
    _, first = case.run_command("cancel-unknown", "cancel", expected=(1,))
    require_true(first.get("error"), "first unresolved cancel lacked uncertainty")
    case.run_command("status-poll-one", "status", expected=(0,))
    case.run_command("status-poll-two", "status", expected=(0,))
    case.set_status("running")
    _, second = case.run_command("cancel-explicit-later", "cancel", expected=(0, 1))
    require_true(second.get("stop") is not None or second.get("stops"),
                 "later explicit cancel lacked a new response")
    requests = list((case.task_dir() / "stop").glob("*.request.json"))
    require_true(len(requests) == 2, "explicit later cancel did not receive new request ID")
    assert_counts(case, A=1, S=0, E=0, K=1, M=0)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--tools", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument(
        "--case",
        action="append",
        choices=("SESSION-CHAIN", "SESSION-CHAIN-BASELINE"),
        dest="cases",
        help="run one focused compiled H control instead of the complete suite",
    )
    args = parser.parse_args()
    suite = HermeticSuite(args.tools, args.output)
    if args.cases:
        suite.run_targeted(args.cases)
    else:
        suite.run()


if __name__ == "__main__":
    main()
