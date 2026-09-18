#!/usr/bin/env python3
"""Run a bounded live acceptance gate for Pi or OpenCode.

The gate uses the shipped dispatcher and runner for every provider turn.  It
does not copy credentials, call the provider directly, retry paid work, or
turn an authentication refusal into a passing result.
"""

from __future__ import annotations

import argparse
from copy import deepcopy
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import sys
import time
import traceback

from acceptance_provider_common import (
    AcceptanceFailure,
    INSPECTION_GROUP_PREFIX,
    NativeTaskOps,
    clean_absolute,
    done_result,
    parse_json_output,
    pueue_command,
    reject_tmp,
    require,
    snapshot,
    status_jobs,
    validate_queue_row,
    verify_collected_outcome,
    write_bytes,
)
from acceptance_supervisor_common import (
    Processes,
    config_for,
    digest,
    inherited_environment,
    read_json,
    write_json,
)


PROFILES = {
    "pi:json": {"executable": "pi", "mode": "read-only"},
    "opencode:run": {"executable": "opencode", "mode": "read-only"},
}
PUEUE_VERSION = "4.0.4"
TASK_BUDGET = "120s"
CANONICAL_TASK_BUDGET = "2m0s"
TASK_BUDGET_NANOS = 120_000_000_000
WATCH_SECONDS = 180
MAX_RAW_BYTES = 8 * 1024 * 1024
AUTHENTICATION_MARKERS = (
    "authentication required", "not authenticated", "please log in", "sign in",
    "unauthorized", "api key", "credentials", "login required", "no provider",
)
AUTHENTICATION_STATUS_CODES = frozenset({401, 403})


class BlockedFailure(AcceptanceFailure):
    """A required provider, supervisor, or authentication prerequisite is absent."""


def resolve_executable(value: str | None, default: Path, label: str) -> Path:
    candidate = Path(value) if value else default
    if not candidate.is_absolute():
        discovered = shutil.which(str(candidate))
        if discovered is None:
            raise BlockedFailure(f"{label} is not discoverable")
        candidate = Path(discovered)
    if not candidate.is_file() or not os.access(candidate, os.X_OK):
        raise BlockedFailure(f"{label} is unavailable or not executable")
    resolved = candidate.resolve(strict=True)
    if not resolved.is_file() or not os.access(resolved, os.X_OK):
        raise BlockedFailure(f"{label} resolved target is not executable")
    return resolved


def require_discovery(name: str, selected: Path, environment: dict[str, str]) -> None:
    discovered = shutil.which(name, path=environment.get("PATH"))
    if discovered is None or Path(discovered).resolve() != selected:
        raise BlockedFailure(f"{name} discovery does not match the selected executable")


def choose_output(provider: str, explicit: str | None) -> Path:
    if explicit:
        path = clean_absolute(explicit, "live acceptance output")
    else:
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        path = Path.home() / ".local" / "state" / "delegation-layer-evidence" / f"{provider.replace(':', '-')}-{stamp}"
    reject_tmp(path, "live acceptance output")
    if path.exists():
        raise AcceptanceFailure(f"live acceptance output already exists: {path}")
    path.mkdir(mode=0o700, parents=True, exist_ok=False)
    return path.resolve()


def authentication_unavailable(directory: Path) -> bool:
    outcome_path = directory / "outcome.json"
    if not outcome_path.is_file() or outcome_path.is_symlink():
        return False
    try:
        outcome = read_json(outcome_path)
    except (OSError, ValueError, TypeError, RecursionError, RuntimeError):
        return False
    if not isinstance(outcome, dict) or outcome.get("verdict") != "rejected":
        return False
    for relative in ("raw/stderr", "raw/stdout", "publish.reject"):
        path = directory / relative
        if not path.is_file() or path.is_symlink():
            continue
        try:
            with path.open("rb") as stream:
                data = stream.read(MAX_RAW_BYTES).decode("utf-8", errors="replace").lower()
        except OSError:
            continue
        if any(marker in data for marker in AUTHENTICATION_MARKERS):
            return True
    for relative in ("raw/stdout", "raw/stderr", "publish.reject"):
        path = directory / relative
        if not path.is_file() or path.is_symlink():
            continue
        try:
            with path.open("rb") as stream:
                data = stream.read(MAX_RAW_BYTES).decode("utf-8", errors="replace")
        except OSError:
            continue
        for line in data.splitlines():
            try:
                event = json.loads(line)
            except (TypeError, ValueError, RecursionError):
                continue
            if not isinstance(event, dict):
                continue
            error = event.get("error")
            details = error.get("data") if isinstance(error, dict) else None
            status_code = details.get("statusCode") if isinstance(details, dict) else None
            if type(status_code) is int and status_code in AUTHENTICATION_STATUS_CODES:
                return True
    return False


def task_digest_snapshot(directory: Path) -> dict[str, dict[str, object]]:
    return snapshot(directory, ignored_prefixes=(".ack",))


def bounded_text(path: Path, label: str) -> str:
    with path.open("rb") as stream:
        data = stream.read(MAX_RAW_BYTES + 1)
    require(len(data) <= MAX_RAW_BYTES, label + " exceeds the bounded output limit")
    return data.decode("utf-8", errors="replace").strip()


def dispatch_binding(response: dict[str, object], task_id: str, label: str) -> tuple[str, int]:
    require(response.get("task_id") == task_id, f"{label} changed task identity")
    root_id = response.get("root_id")
    require(isinstance(root_id, str) and len(root_id) == 32 and
            all(character in "0123456789abcdef" for character in root_id),
            f"{label} returned no root identity")
    supervisor = response.get("supervisor")
    require(isinstance(supervisor, dict) and supervisor.get("matched") is True,
            f"{label} has no matched supervisor identity")
    numeric_id = supervisor.get("numeric_task_id")
    require(type(numeric_id) is int and numeric_id >= 0,
            f"{label} has no numeric supervisor identity")
    return root_id, numeric_id


class NativeAcceptance:
    def __init__(self, args: argparse.Namespace):
        if args.provider not in PROFILES:
            raise AcceptanceFailure(f"unsupported live provider: {args.provider}")
        profile = PROFILES[args.provider]
        if args.mode != profile["mode"]:
            raise AcceptanceFailure(f"{args.provider} supports only {profile['mode']} in this gate")
        self.args = args
        self.provider = args.provider
        self.mode = args.mode
        self.environment = inherited_environment()
        self.output = clean_absolute(args.output, "live acceptance output")
        require(self.output.is_dir() and not self.output.is_symlink(),
                "live acceptance output is not a regular directory")
        self.delegate = resolve_executable(args.delegate, Path(__file__).resolve().parent.parent / "bin/delegate", "delegate")
        self.runner = resolve_executable(args.runner, Path(__file__).resolve().parent.parent / "bin/delegate-run", "delegate-run")
        self.pueue = resolve_executable(args.pueue, Path(shutil.which("pueue", path=self.environment.get("PATH")) or "pueue"), "pueue")
        self.pueued = resolve_executable(args.pueued, Path(shutil.which("pueued", path=self.environment.get("PATH")) or "pueued"), "pueued")
        discovered_provider = shutil.which(profile["executable"], path=self.environment.get("PATH"))
        self.provider_executable = resolve_executable(
            args.provider_executable, Path(discovered_provider or profile["executable"]), profile["executable"])
        require_discovery(profile["executable"], self.provider_executable, self.environment)

        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        self.base = Path.home() / "delegation-layer-live" / f"{self.provider.replace(':', '-')}-{stamp}"
        reject_tmp(self.base, "live acceptance base")
        self.base.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.state = self.base / "state"
        self.workspace = self.base / "workspace"
        self.briefs = self.base / "briefs"
        self.pueue_base = self.base / "pueue"
        for path in (self.state, self.workspace, self.briefs, self.pueue_base,
                     self.pueue_base / "state", self.pueue_base / "run"):
            path.mkdir(mode=0o700, parents=False)
        self.config = self.pueue_base / "pueue.yml"
        write_json(self.config, config_for(self.pueue_base))
        self.processes = Processes(self.output / "processes", self.environment)
        self.daemon = None
        self.closed = False
        self.root_id: str | None = None
        self.provider_version: str | None = None
        self.provider_sha256: str | None = None
        self.tasks: dict[str, str] = {}
        self.numeric_ids: dict[str, int] = {}
        self.records: dict[str, dict[str, object]] = {}
        self.workspace_marker = self.workspace / "acceptance-marker.txt"
        write_bytes(self.workspace_marker, b"DELEGATION-LIVE-MARKER\n")

    def direct(self, name: str, argv: list[object], expected=0, timeout=30):
        return self.processes.run(name, argv, self.base, expected=expected, timeout=timeout)

    def setup(self) -> None:
        version = self.direct("provider-version", [self.provider_executable, "--version"], timeout=20)
        provider_version = bounded_text(version.directory / "stdout", "provider version output")
        require(provider_version, "provider returned an empty version")
        self.provider_version = provider_version
        self.provider_sha256 = digest(self.provider_executable)
        pueue_version = self.direct("pueue-version", pueue_command(self.pueue, self.config, "--version"), timeout=20)
        pueued_version = self.direct("pueued-version", [self.pueued, "-c", self.config, "--version"], timeout=20)
        if bounded_text(pueue_version.directory / "stdout", "pueue version output") != f"pueue {PUEUE_VERSION}":
            raise BlockedFailure(f"pueue {PUEUE_VERSION} is unavailable")
        if bounded_text(pueued_version.directory / "stdout", "pueued version output") != f"pueued {PUEUE_VERSION}":
            raise BlockedFailure(f"pueued {PUEUE_VERSION} is unavailable")
        discovery = parse_json_output(
            self.direct("discovery", [self.delegate, "providers", "--json"], timeout=30), "provider discovery")
        providers = discovery.get("providers")
        require(isinstance(providers, list) and any(isinstance(item, dict) and item.get("id") == self.provider for item in providers),
                "selected provider is absent from compiled discovery")
        write_json(self.output / "binding.json", {
            "provider": self.provider,
            "mode": self.mode,
            "provider_executable": str(self.provider_executable),
            "provider_sha256": self.provider_sha256,
            "provider_version": provider_version,
            "delegate_sha256": digest(self.delegate),
            "runner_sha256": digest(self.runner),
            "pueue_sha256": digest(self.pueue),
            "pueued_sha256": digest(self.pueued),
            "pueue_version": PUEUE_VERSION,
            "credentials_in_receipt": False,
        })
        self.daemon = self.processes.start("pueued", [self.pueued, "-c", self.config], self.base)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            process = self.direct("daemon-status", pueue_command(self.pueue, self.config, "status", "--json"), expected={0, 1}, timeout=20)
            if process.result.get("exit_code") == 0:
                status = status_jobs(process)
                if not status.get("tasks"):
                    return
            time.sleep(0.2)
        raise AcceptanceFailure("private pueue readiness observation expired")

    def brief(self, name: str, continuation: bool = False) -> Path:
        path = self.briefs / f"{name}.txt"
        text = ("Continue the exact previous conversation and answer with a short confirmation. "
                "Do not modify files, use shell, use network, or access paths outside the current workspace.\n"
                if continuation else
                "Read acceptance-marker.txt in the current workspace and reply with DELEGATION_LIVE_OK. "
                "Do not modify files, use shell, use network, or access paths outside the current workspace.\n")
        write_bytes(path, text.encode())
        return path

    def dispatch(self, name: str, brief: Path, predecessor: str | None = None) -> str:
        task_id = secrets.token_hex(16)
        argv: list[object] = [self.delegate, "--root", self.state, "--pueue-config", self.config,
                              "--runner", self.runner, "dispatch", "--provider", self.provider,
                              "--brief", brief, "--cwd", self.workspace, "--id", task_id,
                              "--permission", self.mode, "--budget", TASK_BUDGET, "--json"]
        if self.args.model:
            argv.extend(["--model", self.args.model])
        if self.args.effort:
            argv.extend(["--effort", self.args.effort])
        if predecessor:
            argv.extend(["--resume-task", predecessor])
        process = self.direct(name, argv, expected={0, 1, 2}, timeout=60)
        response = parse_json_output(process, name)
        write_json(self.output / f"{name}.json", response)
        if response.get("admission") != "admitted":
            message = str(response.get("error", ""))
            if any(marker in message.lower() for marker in AUTHENTICATION_MARKERS):
                raise BlockedFailure(f"{self.provider} authentication is unavailable")
            raise AcceptanceFailure(f"{self.provider} dispatch was not admitted: {message}")
        root_id, numeric_id = dispatch_binding(response, task_id, name)
        if self.root_id is None:
            self.root_id = root_id
        require(root_id == self.root_id, "dispatch changed root identity")
        self.numeric_ids[task_id] = numeric_id
        self.tasks[name] = task_id
        return task_id

    def wait_done(self, name: str, task_id: str) -> None:
        require(self.root_id is not None, "root identity is unavailable")
        label = f"delegate:{self.root_id}:{task_id}"
        deadline = time.monotonic() + WATCH_SECONDS
        while time.monotonic() < deadline:
            process = self.direct(f"{name}-status", pueue_command(self.pueue, self.config, "status", "--json"), timeout=20)
            status = status_jobs(process)
            rows = status.get("tasks")
            require(isinstance(rows, dict), "pueue status has no tasks object")
            matches = [row for row in rows.values() if isinstance(row, dict) and row.get("label") == label]
            require(len(matches) == 1, f"{name} has no unique supervisor row")
            row = matches[0]
            require(task_id in self.numeric_ids, f"{name} has no numeric supervisor identity")
            validate_queue_row(row, label, self.runner, self.state, task_id,
                               self.numeric_ids[task_id], "default")
            result = done_result(row)
            if result is not None:
                if result != "Success":
                    directory = self.state / "tasks" / task_id
                    if authentication_unavailable(directory):
                        raise BlockedFailure(f"{self.provider} authentication is unavailable")
                    raise AcceptanceFailure(f"{name} runner ended with {result!r}")
                return
            time.sleep(0.25)
        raise AcceptanceFailure(f"{name} runner completion was not observed")

    def terminal_private_queue(self, status: dict[str, object]) -> bool:
        try:
            if self.root_id is None:
                return self._empty_private_queue(status)
            return self._terminal_private_queue(status)
        except (AcceptanceFailure, KeyError, OSError, RuntimeError, TypeError,
                UnicodeError, ValueError, RecursionError):
            return False

    @staticmethod
    def _empty_private_queue(status: dict[str, object]) -> bool:
        rows = status.get("tasks")
        groups = status.get("groups")
        if not isinstance(rows, dict) or not isinstance(groups, dict) or rows:
            return False
        if not groups:
            return True
        if set(groups) != {"default"}:
            return False
        default = groups["default"]
        return (isinstance(default, dict) and default.get("status") == "Running" and
                type(default.get("parallel_tasks")) is int and
                default.get("parallel_tasks") == 1)

    def _inspection_queue_records(self) -> dict[str, dict[str, object]]:
        """Return complete inspection journals owned by every admitted task."""
        require(self.root_id is not None, "root identity is unavailable")
        task_ids = set(self.tasks.values())
        validator = NativeTaskOps(
            self.processes, self.delegate, self.runner, self.pueue,
            self.state.parent, self.state, self.output)
        validator.bind_supervisor(self.config, self.daemon)
        validator.root_id = self.root_id
        validator.dispatch_attempts = set(task_ids)
        validator.inspection_attempts = set(task_ids)
        records = validator.validate_inspection_journals(
            allow_failure=False, expected_binding_required=False)
        require(set(records) == task_ids,
                "inspection journal coverage is incomplete for admitted tasks")
        require(isinstance(self.provider_sha256, str) and self.provider_sha256,
                "provider executable digest is unavailable")
        for task_id, record in records.items():
            request = record.get("request")
            require(isinstance(request, dict), f"inspection request is absent for {task_id}")
            binding = request.get("binding")
            require(isinstance(binding, dict) and
                    binding.get("helper_executable") == str(self.provider_executable) and
                    binding.get("helper_sha256") == self.provider_sha256,
                    f"inspection helper binding is not the selected provider for {task_id}")
        return records

    def _terminal_private_queue(self, status: dict[str, object]) -> bool:
        rows = status.get("tasks")
        groups = status.get("groups")
        require(isinstance(rows, dict) and isinstance(groups, dict),
                "private queue status is malformed")
        require(self.root_id is not None, "root identity is unavailable")
        inspection_records = self._inspection_queue_records()
        inspection_group = INSPECTION_GROUP_PREFIX + self.root_id
        expected_groups = {"default"} | ({inspection_group} if inspection_records else set())
        require(set(groups) == expected_groups, "private queue contains an unknown supervisor group")
        for name in expected_groups:
            group = groups[name]
            require(isinstance(group, dict) and group.get("status") == "Running" and
                    type(group.get("parallel_tasks")) is int and
                    group.get("parallel_tasks") == 1,
                    f"private queue group is not running at parallelism one: {name}")

        expected: dict[str, tuple[str, str]] = {}
        for task_id in self.tasks.values():
            require(task_id in self.numeric_ids, f"ordinary task has no numeric supervisor identity: {task_id}")
            label = f"delegate:{self.root_id}:{task_id}"
            require(label not in expected, "ordinary task labels are not unique")
            expected[label] = ("ordinary", task_id)
        for task_id, record in inspection_records.items():
            label = record["label"]
            require(isinstance(label, str) and label not in expected,
                    "inspection task labels are not unique")
            expected[label] = ("inspection", task_id)
        require(len(rows) == len(expected), "private queue membership is not exact")

        seen: set[str] = set()
        for row in rows.values():
            require(isinstance(row, dict), "private queue contains a malformed row")
            label = row.get("label")
            require(isinstance(label, str) and label in expected and label not in seen,
                    "private queue labels are not exact")
            seen.add(label)
            kind, task_id = expected[label]
            if kind == "ordinary":
                validate_queue_row(row, label, self.runner, self.state, task_id,
                                   self.numeric_ids[task_id], "default")
            else:
                record = inspection_records[task_id]
                require(row.get("group") == record["group"] and
                        row.get("id") == record["numeric_task_id"],
                        f"inspection queue identity is invalid for {task_id}")
                expected_command = [str(self.runner), "--inspection", "--root",
                                    str(self.state), task_id]
                commands = []
                for key in ("original_command", "command"):
                    value = row.get(key)
                    require(isinstance(value, str), f"inspection queue row missing {key}")
                    commands.append(shlex.split(value))
                require(commands == [expected_command, expected_command],
                        f"inspection queue argv is invalid for {task_id}")
            require(done_result(row) == "Success", f"private queue task is not successful: {task_id}")
        return seen == set(expected)

    def collect(self, name: str, task_id: str) -> dict[str, object]:
        process = self.direct(name, [self.delegate, "--root", self.state, "--pueue-config", self.config,
                                     "--runner", self.runner, "collect", task_id, "--watch", "0s", "--json"],
                             expected={0, 4}, timeout=60)
        response = parse_json_output(process, name)
        write_json(self.output / f"{name}.json", response)
        require(response.get("task_id") == task_id, f"{name} collected the wrong task")
        directory = self.state / "tasks" / task_id
        if authentication_unavailable(directory):
            raise BlockedFailure(f"{self.provider} authentication is unavailable")
        outcome, payload = verify_collected_outcome(response, directory, "committed")
        require(payload, f"{name} committed an empty response")
        provider_exit = read_json(directory / "provider.exit")
        require(provider_exit.get("root_id") == self.root_id and
                provider_exit.get("task_id") == task_id and
                provider_exit.get("invocation_state") == "started" and
                provider_exit.get("exit_code") == 0 and provider_exit.get("error") == "" and
                isinstance(provider_exit.get("raw_manifest"), list) and
                isinstance(provider_exit.get("manifest_sha256"), str) and
                outcome.get("evidence_sha256") == provider_exit.get("manifest_sha256"),
                f"{name} lacks sealed provider evidence")
        self.records[name] = {"task_id": task_id, "directory": directory,
                              "outcome": deepcopy(outcome), "payload": payload,
                              "snapshot": task_digest_snapshot(directory)}
        return response

    def task_binding(self, task_id: str, predecessor: str | None = None,
                     predecessor_session: str | None = None) -> None:
        directory = self.state / "tasks" / task_id
        require(directory.is_dir() and not directory.is_symlink(),
                f"task {task_id} directory is absent")
        record = read_json(directory / "task.json")
        require(record.get("root_id") == self.root_id and record.get("task_id") == task_id and
                record.get("provider") == self.provider and record.get("mode") == self.mode and
                record.get("canonical_cwd") == str(self.workspace),
                f"task {task_id} changed its immutable binding")
        requested = record.get("requested_config")
        brief = directory / "brief.md"
        require(isinstance(requested, dict) and requested.get("permission") == self.mode and
                requested.get("budget") == CANONICAL_TASK_BUDGET and
                record.get("budget_nanos") == TASK_BUDGET_NANOS and
                brief.is_file() and not brief.is_symlink() and
                record.get("brief_length") == brief.stat().st_size and
                record.get("brief_sha256") == digest(brief),
                f"task {task_id} request binding is invalid")
        require(self.provider_version is not None and self.provider_sha256 is not None,
                "provider prerequisite evidence is unavailable")
        meta = read_json(directory / "meta.json")
        meta_digest = digest(directory / "meta.json")
        spec_digest = meta.get("spec_sha256")
        require(meta.get("root_id") == self.root_id and meta.get("task_id") == task_id and
                meta.get("provider_executable") == str(self.provider_executable) and
                meta.get("provider_version") == self.provider_version and
                meta.get("containment") == self.mode and
                isinstance(spec_digest, str) and len(spec_digest) == 64 and
                all(character in "0123456789abcdef" for character in spec_digest),
                f"task {task_id} metadata binding is invalid")
        effective = meta.get("effective_config")
        policy = effective.get("policy") if isinstance(effective, dict) else None
        require(isinstance(effective, dict) and effective.get("containment") == self.mode and
                isinstance(effective.get("digest"), str) and
                isinstance(policy, dict) and policy.get("workspace") == str(self.workspace) and
                policy.get("runtime_sha256") == self.provider_sha256,
                f"task {task_id} effective policy binding is invalid")
        predicate = meta.get("predicate")
        require(isinstance(predicate, dict) and predicate.get("adapter") == self.provider and
                predicate.get("mode") == self.mode and isinstance(predicate.get("version"), str) and
                bool(predicate["version"]) and isinstance(predicate.get("sha256"), str) and
                len(predicate["sha256"]) == 64 and
                all(character in "0123456789abcdef" for character in predicate["sha256"]),
                f"task {task_id} predicate binding is invalid")
        submit = read_json(directory / "submit.json")
        label = f"delegate:{self.root_id}:{task_id}"
        supervisor = submit.get("supervisor")
        require(submit.get("root_id") == self.root_id and submit.get("task_id") == task_id and
                submit.get("spec_sha256") == spec_digest and submit.get("meta_sha256") == meta_digest and
                submit.get("label") == label and isinstance(supervisor, dict) and
                supervisor.get("client_executable") == str(self.pueue) and
                supervisor.get("config_path") == str(self.config) and
                supervisor.get("observed_version") == PUEUE_VERSION,
                f"task {task_id} supervisor submission binding is invalid")
        for name in ("provider.start", "provider.started.json"):
            evidence = read_json(directory / name)
            require(evidence.get("root_id") == self.root_id and evidence.get("task_id") == task_id and
                    evidence.get("spec_sha256") == spec_digest and
                    evidence.get("meta_sha256") == meta_digest,
                    f"task {task_id} {name} binding is invalid")
        provider_exit = read_json(directory / "provider.exit")
        require(provider_exit.get("root_id") == self.root_id and
                provider_exit.get("task_id") == task_id and
                provider_exit.get("spec_sha256") == spec_digest and
                provider_exit.get("meta_sha256") == meta_digest and
                provider_exit.get("invocation_state") == "started" and
                provider_exit.get("exit_code") == 0 and provider_exit.get("error") == "" and
                provider_exit.get("predicate") == predicate and
                isinstance(provider_exit.get("raw_manifest"), list) and
                isinstance(provider_exit.get("manifest_sha256"), str) and
                len(provider_exit["manifest_sha256"]) == 64 and
                all(character in "0123456789abcdef" for character in provider_exit["manifest_sha256"]),
                f"task {task_id} provider seal binding is invalid")
        reference = read_json(directory / "provider.ref.json")
        session = reference.get("conversation_id")
        require(reference.get("root_id") == self.root_id and reference.get("task_id") == task_id and
                reference.get("spec_sha256") == spec_digest and
                reference.get("meta_sha256") == meta_digest and reference.get("provider") == self.provider and
                isinstance(session, str) and bool(session),
                f"task {task_id} provider session binding is invalid")
        outcome = read_json(directory / "outcome.json")
        require(outcome.get("root_id") == self.root_id and outcome.get("task_id") == task_id and
                outcome.get("spec_sha256") == spec_digest and outcome.get("meta_sha256") == meta_digest and
                outcome.get("verdict") == "committed" and outcome.get("predicate") == predicate and
                outcome.get("evidence_sha256") == provider_exit.get("manifest_sha256"),
                f"task {task_id} outcome authority binding is invalid")
        prior = record.get("prior_session")
        if predecessor is None:
            require(prior is None, f"task {task_id} unexpectedly has a predecessor")
            return
        require(isinstance(prior, dict) and prior.get("provider") == self.provider and
                prior.get("predecessor_task_id") == predecessor and
                prior.get("conversation_id") == predecessor_session,
                f"task {task_id} lacks the exact predecessor session binding")

    def session_ref(self, task_id: str) -> str:
        reference = read_json(self.state / "tasks" / task_id / "provider.ref.json")
        require(reference.get("root_id") == self.root_id and reference.get("task_id") == task_id and
                reference.get("provider") == self.provider,
                f"task {task_id} provider reference identity changed")
        session = reference.get("conversation_id")
        require(isinstance(session, str) and bool(session),
                f"task {task_id} has no provider session identity")
        return session

    def replay(self) -> None:
        for name, record_name in (("fresh", "fresh-collect"), ("resume", "resume-collect")):
            record = self.records[record_name]
            response = self.collect(f"replay-{name}", record["task_id"])
            require(response.get("outcome") == record["outcome"], f"{name} replay changed outcome")
            require(task_digest_snapshot(record["directory"]) == record["snapshot"],
                    f"{name} replay changed immutable evidence")
        write_json(self.output / "replay-control.json", {"provider_launches": 2, "replay_launches": 0,
                                                            "outcomes_unchanged": True})

    def shutdown(self) -> None:
        require(self.daemon is not None, "private daemon was not started")
        for _ in range(30):
            process = self.direct("final-status", pueue_command(self.pueue, self.config, "status", "--json"), timeout=20)
            status = status_jobs(process)
            if self.terminal_private_queue(status):
                break
            time.sleep(0.2)
        else:
            raise AcceptanceFailure("private queue did not reach a terminal state")
        self.direct("private-shutdown", pueue_command(self.pueue, self.config, "shutdown"), timeout=30)
        self.daemon.wait(30, expected=0)
        self.closed = True

    def safe_shutdown(self) -> bool:
        if self.daemon is None or self.daemon.poll() is not None or self.closed:
            return self.closed
        try:
            if self.processes.drain(timeout=0, exclude=(self.daemon,)):
                return False
            process = self.direct("failure-status", pueue_command(self.pueue, self.config, "status", "--json"), timeout=20)
            status = status_jobs(process)
            if not self.terminal_private_queue(status):
                return False
            self.direct("failure-shutdown", pueue_command(self.pueue, self.config, "shutdown"), timeout=30)
            self.daemon.wait(30, expected=0)
            self.closed = True
            return True
        except BaseException:
            return False

    def run(self) -> None:
        self.setup()
        before = snapshot(self.workspace)
        first = self.dispatch("fresh-dispatch", self.brief("fresh"))
        self.wait_done("fresh", first)
        self.collect("fresh-collect", first)
        self.task_binding(first)
        first_session = self.session_ref(first)
        first_record = task_digest_snapshot(self.records["fresh-collect"]["directory"])
        second = self.dispatch("resume-dispatch", self.brief("resume", continuation=True), first)
        self.wait_done("resume", second)
        self.collect("resume-collect", second)
        self.task_binding(second, first, first_session)
        second_session = self.session_ref(second)
        require(second_session == first_session,
                "continuation changed the provider session")
        require(task_digest_snapshot(self.records["fresh-collect"]["directory"]) == first_record,
                "continuation changed its predecessor")
        require(snapshot(self.workspace) == before, "read-only live acceptance changed the workspace")
        self.replay()
        self.shutdown()
        write_json(self.output / "success.json", {
            "gate": "acceptance-native", "status": "passed", "provider": self.provider,
            "mode": self.mode, "tasks": {
                "fresh": self.tasks["fresh-dispatch"],
                "resume": self.tasks["resume-dispatch"],
            },
            "provider_launches": 2, "replay_launches": 0, "credentials_in_receipt": False,
            "natural_daemon_shutdown": self.closed,
        })


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--provider", required=True, choices=sorted(PROFILES))
    parser.add_argument("--mode", default="read-only")
    parser.add_argument("--output")
    parser.add_argument("--delegate")
    parser.add_argument("--runner")
    parser.add_argument("--pueue")
    parser.add_argument("--pueued")
    parser.add_argument("--provider-executable")
    parser.add_argument("--model")
    parser.add_argument("--effort")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    run: NativeAcceptance | None = None
    output: Path | None = None
    try:
        output = choose_output(args.provider, args.output)
        run = NativeAcceptance(argparse.Namespace(**{**vars(args), "output": str(output)}))
        run.run()
        print(json.dumps({"gate": "acceptance-native", "status": "passed", "provider": args.provider,
                          "evidence": str(output)}, sort_keys=True))
        return 0
    except BlockedFailure as error:
        if output is not None:
            write_json(output / "failure.json", {"gate": "acceptance-native", "status": "BLOCKED",
                                                  "provider": args.provider, "error": str(error),
                                                  "no_retry": True, "credentials_in_receipt": False})
        if run is not None:
            run.safe_shutdown()
        print(f"BLOCKED acceptance-native: {error}", file=sys.stderr, flush=True)
        return 2
    except (AcceptanceFailure, OSError, ValueError, json.JSONDecodeError) as error:
        if output is not None:
            write_json(output / "failure.json", {"gate": "acceptance-native", "status": "failed",
                                                  "provider": args.provider, "error": str(error),
                                                  "no_retry": True, "credentials_in_receipt": False})
        if run is not None:
            run.safe_shutdown()
        print(f"FAIL acceptance-native: {error}", file=sys.stderr, flush=True)
        return 1
    except BaseException as error:
        if output is not None:
            write_json(output / "failure.json", {"gate": "acceptance-native", "status": "failed",
                                                  "provider": args.provider, "error": str(error),
                                                  "traceback": traceback.format_exc(), "no_retry": True,
                                                  "credentials_in_receipt": False})
        if run is not None:
            run.safe_shutdown()
        print(f"FAIL acceptance-native: unexpected {error}", file=sys.stderr, flush=True)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
