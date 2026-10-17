"""Canonical task/admission ledger and acceptance command orchestration."""

from __future__ import annotations

if __name__ != "scripts.delegate_acceptance.tasks.operations":
    raise ImportError("Import shared owners through scripts.delegate_acceptance")

import os
import time
from copy import deepcopy
from datetime import datetime
from scripts.delegate_acceptance.evidence.files import digest, write_json
from scripts.delegate_acceptance.evidence.records import (
    _read_control_record,
    _state_root_identity,
    parse_json_output,
    status_jobs,
)
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _task_identity,
    require,
)
from scripts.delegate_acceptance.inspections.binding import _validate_inspection_binding
from scripts.delegate_acceptance.inspections.journal import validate_inspection_evidence
from scripts.delegate_acceptance.lifetime.closure import _ordinary_lifetime_control_started
from scripts.delegate_acceptance.processes.observation import (
    failure_cleanup_deadline,
    wait_for_failure_cleanup,
)
from scripts.delegate_acceptance.supervisor.configuration import pueue_command
from scripts.delegate_acceptance.supervisor.membership import (
    validate_group_snapshot,
    validate_inspection_queue_row,
    validate_queue_membership,
)
from scripts.delegate_acceptance.supervisor.observation import wait_runner_terminal
from scripts.delegate_acceptance.supervisor.queue_policy import (
    canonical_queue,
    done_result,
    validate_queue_row,
)
from scripts.delegate_acceptance.tasks.admission import durable_ordinary_admission
from scripts.delegate_acceptance.tasks.publication import snapshot, verify_collected_outcome
from scripts.delegate_acceptance.tasks.refusal_policy import (
    empty_private_queue,
    is_static_refusal_observation,
)
from scripts.delegate_acceptance.workspaces.admission_evidence import (
    _inspection_request_identity,
    _validate_historical_workspace_attempt,
)
from pathlib import Path


class NativeTaskOps:
    """Small pueue/task lifecycle used by the Codex and Claude gates.

    The provider drivers still own their profiles, event parsers, and positive
    controls.  This object only owns the repeated supervisor observations and
    immutable collection/replay checks.  In particular it never starts a
    provider process itself: the shipped delegate remains the only launcher.
    """

    def __init__(self, processes: object, delegate: Path, runner: Path,
                 pueue: Path, state_parent: Path, state: Path, output: Path,
                 watch_seconds: float = 150,
                 expected_inspection_binding_required: bool = True,
                 managed_runner: bool = False) -> None:
        self.processes = processes
        self.delegate = Path(delegate)
        self.runner = Path(runner)
        self.pueue = Path(pueue)
        self.state_parent = Path(state_parent)
        self.state = Path(state)
        self.output = Path(output)
        self.watch_seconds = watch_seconds
        require(type(expected_inspection_binding_required) is bool,
                "expected inspection binding mode is invalid")
        require(type(managed_runner) is bool, "managed runner mode is invalid")
        self.expected_inspection_binding_required = expected_inspection_binding_required
        self.managed_runner = managed_runner
        self.pueue_config: Path | None = None
        self.daemon: object | None = None
        self.root_id: str | None = None
        self.root_created_at: tuple[datetime, int] | None = None
        self.tasks: dict[str, str] = {}
        self.labels: dict[str, str] = {}
        self.numbers: dict[str, int] = {}
        self.expected_results: dict[str, str] = {}
        self.dispatch_attempts: set[str] = set()
        self.dispatch_observations: list[dict[str, object]] = []
        self.inspection_attempts: set[str] = set()
        self.inspection_bindings: dict[str, dict[str, object]] = {}
        self.queue_before_replay: dict[str, object] | None = None
        self.closed = False

    def _ordinary_runner_executable(self, task: str) -> Path:
        if not self.managed_runner:
            return self.runner
        meta = _read_control_record(
            self.state / "tasks" / task / "meta.json",
            f"ordinary task metadata for {task}")
        executable = meta.get("runner_executable")
        require(meta.get("runner_ownership") == "managed" and
                isinstance(executable, str) and Path(executable).is_absolute() and
                Path(executable) == Path(os.path.normpath(executable)),
                f"ordinary task managed runner binding is invalid for {task}")
        runner = Path(executable)
        require(runner.is_file() and not runner.is_symlink() and
                digest(runner) == digest(self.runner),
                f"ordinary task managed runner executable changed for {task}")
        return runner

    def _ordinary_lifetime_validation_required(self, task: str) -> bool:
        """Detect managed ordinary work without changing markerless history."""
        directory = self.state / "tasks" / task
        if self.managed_runner or _ordinary_lifetime_control_started(directory):
            return True
        meta_path = directory / "meta.json"
        if not meta_path.exists() and not meta_path.is_symlink():
            return False
        meta = _read_control_record(meta_path, f"ordinary task metadata for {task}")
        ownership = meta.get("runner_ownership")
        require(ownership is None or ownership in {"managed", "custom"},
                f"ordinary task runner ownership is invalid for {task}")
        return ownership == "managed"

    def validate_ordinary_queue_row(
            self, row: dict[str, object], label: str, task: str,
            numeric: int | None = None, group: str | None = None) -> None:
        validate_queue_row(
            row, label, self._ordinary_runner_executable(task), self.state, task,
            numeric, group, supervised=self.managed_runner)
        if done_result(row) is None or not self._ordinary_lifetime_validation_required(task):
            return
        durable = self._durable_ordinary_admission(task, None)
        require(durable is not None and durable.get("task_id") == task and
                durable.get("label") == label and
                (numeric is None or durable.get("numeric_task_id") == numeric) and
                isinstance(durable.get("managed_lifetime"), dict),
                f"ordinary task lacks authoritative lifetime closure for {task}")

    def bind_supervisor(self, config: Path, daemon: object | None = None) -> None:
        self.pueue_config = Path(config)
        if daemon is not None:
            self.daemon = daemon

    def expect_inspection(self, task_id: str,
                          binding: dict[str, object] | None = None) -> None:
        """Declare an inspection expected for a task before dispatch."""
        require(_task_identity(task_id), "inspection task identity is invalid")
        require(not self.closed, "acceptance supervisor is closed")
        if task_id in self.dispatch_attempts:
            raise AcceptanceFailure("inspection expectation must precede dispatch")
        existing = self.inspection_bindings.get(task_id)
        if binding is not None:
            _validate_inspection_binding(binding, "expected inspection binding")
            if existing is not None:
                require(binding == existing,
                        "expected inspection binding was re-registered with different source")
            else:
                self.inspection_bindings[task_id] = deepcopy(binding)
        else:
            require(existing is not None, "expected inspection binding is required")
        self.inspection_attempts.add(task_id)

    def direct(self, name: str, argv: list[object], expected: int | set[int] = 0,
               timeout: float = 30) -> object:
        require(not self.closed, "acceptance supervisor is closed")
        return self.processes.run(name, argv, self.state_parent,
                                  expected=expected, timeout=timeout)

    def client(self, name: str, operation: list[str], expected: int | set[int] = 0,
               timeout: float = 30) -> object:
        require(self.pueue_config is not None, "pueue is not configured")
        return self.direct(name, pueue_command(self.pueue, self.pueue_config, *operation),
                           expected, timeout)

    def dispatch(self, name: str, task: str, argv: list[object]) -> dict[str, object]:
        """Admit one exact delegate task and bind its supervisor identity."""
        require(not self.closed, "acceptance supervisor is closed")
        require(task not in self.dispatch_attempts, "duplicate dispatch attempt")
        self.dispatch_attempts.add(task)
        process = self.processes.run(name, argv, self.state_parent,
                                     expected=None, timeout=45)
        try:
            response = parse_json_output(process, name)
        except BaseException:
            self._record_dispatch_observation(name, task, process, None)
            raise
        if response.get("admission") == "admitted":
            # Retain a valid positive identity before writing the auxiliary
            # dispatch-attempt receipt.  A receipt failure must not discard
            # the queue authority needed for natural failure cleanup.
            try:
                admitted = self.record_admission(name, task, response)
            except BaseException:
                self._record_dispatch_observation(name, task, process, response)
                raise
            self._record_dispatch_observation(name, task, process, response)
            result = getattr(process, "result", None)
            require(isinstance(result, dict) and result.get("natural_wait") is True and
                    type(result.get("exit_code")) is int and result.get("exit_code") == 0,
                    f"{name} dispatch did not exit successfully")
            return admitted
        self._record_dispatch_observation(name, task, process, response)
        return self.record_admission(name, task, response)

    def _record_dispatch_observation(self, name: str, task: str, process: object,
                                     response: dict[str, object] | None,
                                     command: str = "dispatch") -> None:
        result = getattr(process, "result", None)
        observation = {
            "name": name,
            "task_id": task,
            "exit_code": result.get("exit_code") if isinstance(result, dict) else None,
            "natural_wait": result.get("natural_wait") if isinstance(result, dict) else None,
            "response": response,
            "receipt_written": False,
        }
        self.dispatch_observations.append(observation)
        write_json(self.output / (name + "-" + command + "-attempt.json"),
                   {**observation, "receipt_written": True})
        observation["receipt_written"] = True

    def record_admission(self, name: str, task: str, response: dict[str, object],
                         command: str = "dispatch") -> dict[str, object]:
        """Retain a positively observed admission, including concurrent callers."""
        require(task in self.dispatch_attempts, "admission has no declared dispatch attempt")
        require(response.get("admission") == "admitted", f"{name} was not admitted")
        require(response.get("task_id") == task, f"{name} task identity mismatch")
        root = response.get("root_id")
        require(isinstance(root, str) and root, f"{name} omitted a root identity")
        if self.root_id is None:
            self.root_id = root
        require(self.root_id == root, f"{name} changed root identity")
        supervisor = response.get("supervisor")
        require(isinstance(supervisor, dict) and supervisor.get("matched") is True,
                f"{name} has no positive supervisor admission")
        numeric = supervisor.get("numeric_task_id")
        require(isinstance(numeric, int) and not isinstance(numeric, bool) and numeric >= 0,
                f"{name} has no numeric pueue identity")
        require(name not in self.tasks and task not in self.tasks.values(),
                f"{name} duplicated an admitted task")
        self.tasks[name] = task
        self.labels[name] = f"delegate:{root}:{task}"
        self.numbers[name] = numeric
        self.expected_results[name] = "Success"
        write_json(self.output / (name + "-" + command + ".json"), response)
        return response

    def continue_task(self, name: str, argv: list[object]) -> tuple[str, dict[str, object]]:
        """Admit one public continuation after Delegate assigns its successor ID."""
        require(not self.closed, "acceptance supervisor is closed")
        process = self.processes.run(name, argv, self.state_parent,
                                     expected=None, timeout=45)
        response = parse_json_output(process, name)
        task = response.get("task_id")
        require(isinstance(task, str) and _task_identity(task),
                f"{name} continuation has no valid successor task identity")
        require(task not in self.dispatch_attempts,
                "continuation reused an existing task identity")
        self.dispatch_attempts.add(task)
        if response.get("admission") == "admitted":
            try:
                admitted = self.record_admission(name, task, response, command="continue")
            except BaseException:
                self._record_dispatch_observation(name, task, process, response, command="continue")
                raise
            self._record_dispatch_observation(name, task, process, response, command="continue")
            result = getattr(process, "result", None)
            require(isinstance(result, dict) and result.get("natural_wait") is True and
                    type(result.get("exit_code")) is int and result.get("exit_code") == 0,
                    f"{name} continuation did not exit successfully")
            return task, admitted
        self._record_dispatch_observation(name, task, process, response, command="continue")
        return task, self.record_admission(name, task, response, command="continue")

    def expect_terminal_result(self, name: str, result: str) -> None:
        require(name in self.tasks, f"terminal result has no admitted task: {name}")
        require(result in {"Success", "Killed", "Errored", "DependencyFailed", "Failed", "FailedToSpawn"},
                "expected terminal result is invalid")
        self.expected_results[name] = result

    def wait_task(self, name: str, task: str,
                  expected_result: str | frozenset[str] | None = None) -> dict[str, object]:
        require(self.root_id is not None, "root identity is unavailable")
        if expected_result is None:
            expected_result = self.expected_results.get(name, "Success")
        row = wait_runner_terminal(self.client, self.root_id, task, expected_result,
                                   timeout=self.watch_seconds,
                                   sleep_fn=time.sleep, monotonic_fn=time.monotonic)
        require(isinstance(row, dict), "runner completion row is malformed")
        require(name in self.numbers, f"runner task {task} has no recorded numeric identity")
        self.validate_ordinary_queue_row(
            row, self.labels[name], task, self.numbers[name], "default")
        write_json(self.output / (name + "-done.json"), row)
        return row

    def collect(self, name: str, task: str, expected: int | set[int] = 0) -> dict[str, object]:
        require(self.pueue_config is not None, "pueue is not configured")
        process = self.direct(name, [self.delegate, "--root", self.state,
                                     "--pueue-config", self.pueue_config,
                                     "--runner", self.runner, "collect", task,
                                     "--watch", "0s", "--json"], expected=expected, timeout=45)
        response = parse_json_output(process, name)
        require(response.get("task_id") == task, f"{name} collected the wrong task")
        write_json(self.output / (name + ".json"), response)
        return response

    def status(self, name: str, task: str,
               expected: int | set[int] = 0, timeout: float = 45) -> dict[str, object]:
        require(self.pueue_config is not None, "pueue is not configured")
        require(timeout > 0, "status observation timeout must be positive")
        process = self.direct(name, [self.delegate, "--root", self.state,
                                     "--pueue-config", self.pueue_config,
                                     "--runner", self.runner, "status", task,
                                     "--json"], expected=expected, timeout=timeout)
        response = parse_json_output(process, name)
        require(response.get("task_id") == task, f"{name} observed the wrong task")
        write_json(self.output / (name + ".json"), response)
        return response

    def queue_status(self, name: str) -> dict[str, object]:
        status = status_jobs(self.client(name, ["status", "--json"], timeout=20))
        write_json(self.output / (name + ".json"), status)
        return status

    def _known_root_id(self) -> str:
        value, root_created = _state_root_identity(self.state, "inspection root record")
        if self.root_id is None:
            self.root_id = value
        require(self.root_id == value, "inspection root identity changed")
        if self.root_created_at is None:
            self.root_created_at = root_created
        require(self.root_created_at == root_created, "inspection root creation timestamp changed")
        return value

    def _inspection_journal_paths(self) -> dict[str, list[Path]]:
        parent = self.state / "inspections"
        try:
            parent.lstat()
        except FileNotFoundError:
            return {}
        except OSError as error:
            raise AcceptanceFailure(f"cannot inspect inspection journal parent: {error}") from error
        require(parent.is_dir() and not parent.is_symlink(),
                "inspection journal parent is not a regular directory")
        root = self._known_root_id()
        paths: dict[str, list[Path]] = {}
        for path in parent.iterdir():
            require(_task_identity(path.name), "inspection journal has an invalid task identity")
            require(path.is_dir() and not path.is_symlink(),
                    "inspection journal task entry is not a regular directory")
            request = _read_control_record(
                path / "request.json", f"inspection request for {path.name}")
            identity = _inspection_request_identity(
                self.state, root, request, path.name,
                f"inspection request for {path.name}")
            task = identity["task_id"]
            require(task in self.dispatch_attempts,
                    "inspection journal exists for an unattempted task")
            paths.setdefault(task, []).append(path)
        self.inspection_attempts.update(paths)
        return paths

    def validate_inspection_journal(self, task: str, directory: Path,
                                    allow_failure: bool = False,
                                    expected_binding_required: bool = True) -> dict[str, object]:
        """Validate one complete inspection journal for an acceptance gate.

        Provider-specific gates normally compare the journal binding with a
        definition they constructed before dispatch.  A native provider gate
        cannot reconstruct that definition without duplicating provider setup,
        so it may disable only that equality check while retaining every
        structural, timing, executable, supervisor, and result predicate.
        """
        return self._validate_inspection_journal(
            task, directory, allow_failure, expected_binding_required)

    def _validate_inspection_journal(self, task: str, directory: Path,
                                     allow_failure: bool,
                                     expected_binding_required: bool = True) -> dict[str, object]:
        root = self._known_root_id()
        return validate_inspection_evidence(
            state=self.state, root=root, root_created_at=self.root_created_at, runner=self.runner, pueue=self.pueue,
            pueue_config=self.pueue_config, inspection_bindings=self.inspection_bindings, task=task,
            directory=directory,
            allow_failure=allow_failure, expected_binding_required=expected_binding_required)

    def validate_inspection_journals(self, allow_failure: bool = False,
                                     expected_binding_required: bool | None = None) -> dict[str, dict[str, object]]:
        """Validate every inspection journal and require exact expected coverage."""
        if expected_binding_required is None:
            expected_binding_required = self.expected_inspection_binding_required
        return self._inspection_journals(allow_failure, expected_binding_required)

    def _inspection_journals(self, allow_failure: bool,
                             expected_binding_required: bool | None = None) -> dict[str, dict[str, object]]:
        if expected_binding_required is None:
            expected_binding_required = self.expected_inspection_binding_required
        journals: dict[str, dict[str, object]] = {}
        for task, directories in self._inspection_journal_paths().items():
            candidates = [
                self._validate_inspection_journal(
                    task, directory, allow_failure, expected_binding_required)
                for directory in directories
            ]
            exact = [candidate for candidate in candidates
                     if candidate["ordinary_task_matches"] is True]
            current = [candidate for candidate in candidates
                       if candidate["historical_aborted"] is False]
            require(len(exact) <= 1,
                    f"multiple inspection journals bind ordinary task {task}")
            require(len(current) <= 1,
                    f"multiple current inspection journals exist for {task}")
            if exact:
                selected = exact[0]
            elif current:
                selected = current[0]
            else:
                require(candidates, f"inspection journal candidates are absent for {task}")
                require(all(candidate["historical_aborted"] is True
                            for candidate in candidates),
                        f"inspection journal history is not positively aborted for {task}")
                selected = max(
                    candidates,
                    key=lambda candidate: (
                        candidate["attempt"].get("sequence", 0)
                        if isinstance(candidate.get("attempt"), dict) else 0,
                        candidate["operation_id"],
                    ),
                )
            selected["historical_attempts"] = [
                candidate for candidate in candidates if candidate is not selected
            ]
            for historical in selected["historical_attempts"]:
                _validate_historical_workspace_attempt(
                    self.state, historical["attempt"], selected["request"]["task"],
                    selected["root_id"], f"inspection task history for {task}")
            journals[task] = selected
        require(self.inspection_attempts <= set(journals),
                "an expected inspection journal is missing")
        return journals

    @staticmethod
    def _validate_group_snapshot(groups: object, expected: set[str]) -> None:
        return validate_group_snapshot(groups, expected)

    def _validate_inspection_queue_row(self, row: dict[str, object], journal: dict[str, object],
                                       allow_failure: bool,
                                       terminal_required: bool = True) -> None:
        return validate_inspection_queue_row(
            row=row, journal=journal, allow_failure=allow_failure, state_parent=self.state_parent, pueue_config=self.pueue_config,
            runner=self.runner, state_root=self.state, terminal_required=terminal_required)

    def _durable_ordinary_admission(
            self, task: str,
            journal: dict[str, object] | None) -> dict[str, object] | None:
        return durable_ordinary_admission(
            self.state, self._known_root_id, self.managed_runner, task, journal)

    def _validate_queue_membership(
            self, status: dict[str, object], allow_failure: bool
            ) -> dict[str, dict[str, object]]:
        return validate_queue_membership(
            status=status, allow_failure=allow_failure, tasks_by_name=self.tasks, labels_by_name=self.labels,
            numbers_by_name=self.numbers,
            expected_results=self.expected_results, dispatch_attempts=self.dispatch_attempts,
            expected_binding_required=self.expected_inspection_binding_required, inspection_journals=self._inspection_journals,
            durable_admission=self._durable_ordinary_admission, validate_groups=self._validate_group_snapshot,
            validate_ordinary_row=self.validate_ordinary_queue_row, validate_inspection_row=self._validate_inspection_queue_row)

    def _inspection_snapshot(self) -> dict[str, dict[str, object]]:
        parent = self.state / "inspections"
        try:
            parent.lstat()
        except FileNotFoundError:
            return {}
        except OSError as error:
            raise AcceptanceFailure(f"cannot snapshot inspection journals: {error}") from error
        return snapshot(parent)

    def _inspection_control_started(self) -> bool:
        if self.inspection_attempts:
            return True
        for name in ("inspection-group", "inspections"):
            try:
                (self.state / name).lstat()
            except FileNotFoundError:
                continue
            except OSError:
                return True
            return True
        return False

    @staticmethod
    def _empty_private_queue(status: dict[str, object]) -> bool:
        # Shared native-provider acceptance uses an explicit external
        # supervisor config; it must not impose private-supervisor policy.
        return empty_private_queue(status)

    def _static_refusal_state_is_clean(self, initialized: bool = True) -> bool:
        """Reject every durable task, inspection, and group authorization record."""
        try:
            if not self.state.is_dir() or self.state.is_symlink():
                return False
            entries = list(self.state.iterdir())
        except OSError:
            return False
        names = {entry.name for entry in entries}
        if initialized and not {"root.json", ".maintenance.lock"} <= names:
            return False
        if not names <= {"root.json", ".maintenance.lock", ".probe"}:
            return False
        for entry in entries:
            if entry.name in {"root.json", ".maintenance.lock"}:
                if not entry.is_file() or entry.is_symlink():
                    return False
            elif entry.is_symlink() or not entry.is_dir():
                return False
        return True

    def _unattempted_queue_finished(self, status: dict[str, object]) -> bool:
        """Allow setup cleanup only with no attempted or durable task work."""
        if (self.tasks or self.labels or self.numbers or self.dispatch_attempts or
                self.dispatch_observations or self.inspection_attempts or self.inspection_bindings):
            return False
        if not self._empty_private_queue(status) or not self._static_refusal_state_is_clean(False):
            return False
        try:
            if (self.state / "root.json").exists():
                self._known_root_id()
            elif self.root_id is not None or self.root_created_at is not None:
                return False
        except (AcceptanceFailure, OSError, TypeError, ValueError, RecursionError):
            return False
        return True

    @staticmethod
    def _is_static_refusal_observation(observation: dict[str, object], root: str,
                                       task: str) -> bool:
        return is_static_refusal_observation(observation, root, task)

    def _static_refusal_queue_finished(self, status: dict[str, object]) -> bool:
        """Allow cleanup for an exact pre-admission static refusal only."""
        if not self.dispatch_attempts or not self.dispatch_observations:
            return False
        if len(self.dispatch_observations) != len(self.dispatch_attempts):
            return False
        if not self.inspection_attempts <= self.dispatch_attempts:
            return False
        tasks = [observation.get("task_id") for observation in self.dispatch_observations]
        if any(not _task_identity(task) for task in tasks):
            return False
        if len(set(tasks)) != len(tasks) or set(tasks) != self.dispatch_attempts:
            return False
        if not self._empty_private_queue(status):
            return False
        try:
            root = self._known_root_id()
        except (AcceptanceFailure, OSError, TypeError, ValueError, RecursionError):
            return False
        if not self._static_refusal_state_is_clean():
            return False
        return all(self._is_static_refusal_observation(observation, root, task)
                   for observation, task in zip(self.dispatch_observations, tasks))

    def replay(self, records: dict[str, dict[str, object]],
               names: tuple[str, ...] = ("fresh", "resume"),
               expected: str = "committed") -> None:
        """Re-collect exact records and prove replay created no queue work."""
        before_queue = self.queue_status("replay-queue-before")
        self._validate_queue_membership(before_queue, allow_failure=False)
        self.queue_before_replay = canonical_queue(before_queue)
        before_inspections = self._inspection_snapshot()
        for name in names:
            require(name in self.tasks and name in records, f"replay lacks {name} task")
            task = self.tasks[name]
            record = records[name]
            directory = record.get("directory")
            require(isinstance(directory, Path), f"replay {name} record has no directory")
            before = snapshot(directory)
            outcome = record.get("outcome")
            payload = record.get("payload")
            require(isinstance(outcome, dict) and isinstance(payload, dict),
                    f"replay {name} record is incomplete")
            original = {"outcome": deepcopy(outcome), "payload": deepcopy(payload),
                        "evidence_sha256": outcome.get("evidence_sha256")}
            collected = self.collect("replay-" + name, task)
            actual_outcome, _ = verify_collected_outcome(collected, directory, expected)
            actual = {"outcome": deepcopy(actual_outcome),
                      "payload": deepcopy(collected.get("payload")),
                      "evidence_sha256": collected.get("evidence_sha256")}
            require(actual == original, f"{name} replay changed outcome, payload or evidence hash")
            require(before == snapshot(directory), f"{name} replay changed immutable task records")
        after_queue = self.queue_status("replay-queue-after")
        self._validate_queue_membership(after_queue, allow_failure=False)
        require(canonical_queue(after_queue) == self.queue_before_replay,
                "collection replay changed the private supervisor queue")
        require(self._inspection_snapshot() == before_inspections,
                "collection replay changed inspection journal records")
        write_json(self.output / "replay-control.json", {
            "tasks": dict(self.tasks),
            "provider_launches": len(self.tasks),
            "replay_launches": 0,
            "queue_unchanged": True,
            "outcomes_unchanged": True,
        })

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        if not self.dispatch_attempts:
            return self._unattempted_queue_finished(status)
        if self._static_refusal_queue_finished(status):
            return True
        try:
            self._validate_queue_membership(status, allow_failure=True)
        except (AcceptanceFailure, KeyError, OSError, TypeError, ValueError, RecursionError):
            return False
        return True

    def shutdown(self) -> None:
        """Naturally shut down only after every owned task is Done.Success."""
        status = self.queue_status("final-queue")
        self._validate_queue_membership(status, allow_failure=False)
        excluded = () if self.daemon is None else (self.daemon,)
        pending = self.processes.drain(timeout=1, exclude=excluded)
        require(not pending, "owned acceptance process remains active")
        self.client("private-shutdown", ["shutdown"], timeout=20)
        require(self.daemon is not None, "private daemon was not started")
        self.daemon.wait(timeout=30, expected=0)
        self.closed = True

    def safe_failure_shutdown(self) -> bool:
        """Observe and close only a positively finished private queue."""
        if self.daemon is None or self.daemon.poll() is not None or self.pueue_config is None:
            return False
        try:
            pending = self.processes.drain(timeout=0, exclude=(self.daemon,))
            if pending:
                return False
            status = status_jobs(self.client("failure-queue", ["status", "--json"],
                                             expected=0, timeout=20))
            if not self.failure_queue_finished(status):
                return False
            self.client("failure-shutdown", ["shutdown"], timeout=20)
            self.daemon.wait(timeout=30, expected=0)
            self.closed = True
            return True
        except BaseException:
            return False

    def retain_failure_ownership(self) -> bool:
        """Retain owned callers until natural completion and exact queue cleanup.

        Failure cleanup is an observation loop.  It never sends a signal or
        attempts PID-based recovery: an active caller or daemon remains owned
        until it ends naturally, and an exact terminal queue observation is
        still required before reporting a successful shutdown.
        """
        reported = False
        queue_retry_at = 0.0
        queue_retry_delay = 0.25
        observation_retry_delay = 0.25
        deadline = failure_cleanup_deadline(self.watch_seconds)

        def wait(delay: float, reason: str) -> bool:
            if wait_for_failure_cleanup(deadline, delay):
                return True
            write_json(self.output / "failure-cleanup-expired.json", {
                "reason": reason,
                "timeout_seconds": self.watch_seconds,
                "supervisor_closed": self.closed,
                "signals_sent": 0,
            }, replace=(self.output / "failure-cleanup-expired.json").exists())
            return False

        while True:
            excluded = () if self.daemon is None else (self.daemon,)
            try:
                pending = self.processes.drain(timeout=0, exclude=excluded)
            except BaseException:
                # A completion receipt can fail after a child has exited.
                # Keep the owner and retry observation rather than assuming
                # that the caller or daemon is naturally finished.
                pending = None
                if not reported:
                    print("Acceptance assertion failed; retaining owned processes until exact cleanup is observed.",
                          flush=True)
                    reported = True
                if not wait(observation_retry_delay,
                            "owned process completion could not be observed"):
                    return False
                observation_retry_delay = min(observation_retry_delay * 2, 5.0)
                continue
            observation_retry_delay = 0.25
            if not pending and self.closed:
                return True
            if not pending and self.daemon is not None:
                now = time.monotonic()
                if now >= queue_retry_at:
                    try:
                        cleaned = self.safe_failure_shutdown()
                    except BaseException:
                        cleaned = False
                    if cleaned:
                        return True
                    queue_retry_at = now + queue_retry_delay
                    queue_retry_delay = min(queue_retry_delay * 2, 5.0)
            if not pending:
                if self.daemon is None:
                    if self.dispatch_attempts:
                        return False
                    self.closed = True
                    return True
                try:
                    daemon_finished = self.daemon.poll() is not None
                except BaseException:
                    daemon_finished = False
                if daemon_finished:
                    return False
            if not reported:
                print("Acceptance assertion failed; retaining owned processes until exact cleanup is observed.",
                      flush=True)
                reported = True
            delay = 0.25
            if not pending and self.daemon is not None:
                delay = min(delay, max(0.0, queue_retry_at - time.monotonic()))
            if not wait(delay, "owned processes or private queue did not finish naturally"):
                return False
