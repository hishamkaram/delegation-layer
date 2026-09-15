#!/usr/bin/env python3
"""Exercise native inspection through the app and real isolated pueue, with no AI turns."""
from __future__ import annotations

import argparse
from pathlib import Path
import secrets
import shlex
import shutil
import time

from acceptance_provider_common import (
    AcceptanceFailure, NativeTaskOps, canonical_go_json, canonical_queue, done_result,
    ensure_private_directory, parse_json_output, require, row_state, snapshot,
    supervisor_binding,
    verify_collected_outcome, write_bytes, write_json,
)
from acceptance_supervisor_common import (
    Processes, config_for, digest, inherited_environment, read_json, sha,
)


def no_secret_bytes(paths: list[Path], sentinel: bytes) -> None:
    """Scan only owned output trees; fixture input is deliberately outside them."""
    require(0 < len(sentinel) <= 4096, "invalid private sentinel length")
    overlap = len(sentinel) - 1
    for root in paths:
        require(not root.is_symlink(), "unexpected symlink in inspection output")
        if not root.exists():
            continue
        require(root.is_dir(), "inspection output root is not a directory")
        for path in root.rglob("*"):
            require(not path.is_symlink(), "unexpected symlink in inspection output")
            if path.is_file():
                with path.open("rb") as stream:
                    carry = b""
                    while block := stream.read(65536):
                        data = carry + block
                        require(sentinel not in data, "private native sentinel escaped capture")
                        carry = data[-overlap:] if overlap else b""


BLOCKER_ARGV = ["/bin/sleep", "25"]


def fixture_inspection_binding(helper: Path, helper_config: Path, workspace: Path,
                               environment: dict[str, str], runner: Path,
                               supervisor: dict[str, object]) -> dict[str, object]:
    """Build the expected fixture inspection binding independently of journals."""
    helper = Path(helper).resolve()
    helper_config = Path(helper_config).resolve()
    workspace = Path(workspace).resolve()
    runner = Path(runner).resolve()
    definition = {
        "revision": "inspection-fixture-v1",
        "executable": str(helper),
        "executable_sha256": digest(helper),
        "arguments": [str(helper_config)],
        "directory": str(workspace),
        "environment": [key + "=" + environment[key] for key in sorted(environment)],
        "output_limit": 1 << 20,
    }
    return {
        "definition_revision": "inspection-fixture-v1",
        "definition_sha256": sha(canonical_go_json(definition)),
        "helper_executable": str(helper),
        "helper_sha256": digest(helper),
        "worker_executable": str(runner),
        "worker_sha256": digest(runner),
        "supervisor": supervisor,
    }


def validate_blocker_row(row: dict[str, object], numeric: int, group: str, label: str,
                        expected_state: str) -> None:
    """Bind the bounded blocker to its exact identity, argv, and state."""
    row_id = row.get("id")
    require(isinstance(row_id, int) and not isinstance(row_id, bool) and row_id == numeric,
            "bounded blocker numeric identity mismatch")
    require(row.get("group") == group and row.get("label") == label,
            "bounded blocker group or label mismatch")
    require(row_state(row) == expected_state, "bounded blocker state mismatch")
    if expected_state == "Running":
        require(done_result(row) is None, "running blocker already has a terminal result")
    for field in ("command", "original_command"):
        value = row.get(field)
        require(isinstance(value, str), "bounded blocker row is missing " + field)
        try:
            command = shlex.split(value)
        except ValueError as error:
            raise AcceptanceFailure("bounded blocker command is malformed") from error
        require(command == BLOCKER_ARGV, "bounded blocker argv changed")


def _read_record(path: Path, description: str) -> dict[str, object]:
    require(path.is_file() and not path.is_symlink(), description + " is not a regular record")
    value = read_json(path)
    require(isinstance(value, dict), description + " is not an object")
    return value


def _require_absent(path: Path, description: str) -> None:
    require(not path.exists() and not path.is_symlink(), description + " unexpectedly exists")


def validate_expired_inspection(state: Path, task_id: str, row: dict[str, object],
                                started: bool) -> None:
    """Validate deadline evidence without inventing worker stop acknowledgments.

    Queued expiry is represented by the immutable request/receipt and the
    dispatcher deadline error; the worker returns before its start guard and
    therefore has no result or stop records.  A running worker must persist
    the stop request.  Reply and ended-state records are independently checked
    when the self-stop path manages to persist them, but their absence remains
    an unknown supervisor reply rather than fabricated acknowledgment.
    """
    directory = state / "inspections" / task_id
    request_path = directory / "request.json"
    request = _read_record(request_path, "inspection request")
    request_digest = digest(request_path)
    require(request.get("schema_version") == 1 and request.get("task_id") == task_id,
            "inspection request identity is invalid")
    deadline = request.get("deadline")
    require(isinstance(deadline, str) and deadline, "inspection request deadline is absent")

    receipt_path = directory / "receipt.json"
    receipt = _read_record(receipt_path, "inspection receipt")
    numeric = row.get("id")
    require(isinstance(numeric, int) and not isinstance(numeric, bool) and numeric >= 0,
            "expired inspection row has no numeric identity")
    require(receipt.get("schema_version") == 1 and
            receipt.get("request_sha256") == request_digest and
            receipt.get("numeric_task_id") == numeric,
            "inspection receipt does not bind the expired worker")

    start_path = directory / "start.json"
    if started:
        start = _read_record(start_path, "inspection start")
        require(start.get("schema_version") == 1 and
                start.get("request_sha256") == request_digest,
                "inspection start does not bind the expired request")
    else:
        _require_absent(start_path, "queued inspection start record")

    result_path = directory / "result.json"
    completion_path = directory / "completion.json"
    result_present = result_path.exists() or result_path.is_symlink()
    completion_present = completion_path.exists() or completion_path.is_symlink()
    if started:
        if result_present:
            result = _read_record(result_path, "inspection result")
            require(result.get("schema_version") == 1 and
                    result.get("request_sha256") == request_digest and
                    result.get("reason") == "deadline-expired" and
                    result.get("facts") == {},
                    "running expiry result is not an empty deadline result")
            require(completion_present, "running expiry result has no completion record")
            completion = _read_record(completion_path, "inspection completion")
            require(completion.get("schema_version") == 1 and
                    completion.get("request_sha256") == request_digest and
                    completion.get("result_sha256") == digest(result_path) and
                    completion.get("native_exit") == "unavailable",
                    "running expiry completion is not bound to unavailable work")
        else:
            require(not completion_present, "running expiry completion has no result")
    else:
        _require_absent(result_path, "queued inspection result")
        _require_absent(completion_path, "queued inspection completion")

    stop_request_path = directory / "stop-request.json"
    stop_reply_path = directory / "stop-reply.json"
    stop_observation_path = directory / "stop-observation.json"
    if not started:
        for path, description in ((stop_request_path, "queued stop request"),
                                  (stop_reply_path, "queued stop reply"),
                                  (stop_observation_path, "queued stop observation")):
            _require_absent(path, description)
        return

    stop_request = _read_record(stop_request_path, "inspection stop request")
    binding = request.get("binding")
    require(isinstance(binding, dict) and isinstance(binding.get("supervisor"), dict),
            "inspection request supervisor binding is absent")
    require(stop_request.get("schema_version") == 1 and
            stop_request.get("request_sha256") == request_digest and
            stop_request.get("numeric_task_id") == numeric and
            stop_request.get("deadline") == deadline and
            stop_request.get("supervisor") == binding["supervisor"],
            "inspection stop request is not the exact expired target")
    stop_digest = digest(stop_request_path)

    if stop_reply_path.exists() or stop_reply_path.is_symlink():
        reply = _read_record(stop_reply_path, "inspection stop reply")
        acknowledged = reply.get("acknowledged")
        require(reply.get("schema_version") == 1 and
                reply.get("stop_sha256") == stop_digest and
                reply.get("numeric_task_id") == numeric and
                reply.get("action") == "kill" and
                isinstance(acknowledged, bool),
                "inspection stop reply is not an exact bounded kill result")
    if stop_observation_path.exists() or stop_observation_path.is_symlink():
        observation = _read_record(stop_observation_path, "inspection stop observation")
        require(observation.get("schema_version") == 1 and
                observation.get("stop_sha256") == stop_digest and
                observation.get("numeric_task_id") == numeric and
                observation.get("state") == "ended",
                "inspection stop observation is not an exact ended target")


def validate_concurrent_response(response: dict[str, object], exit_code: int, task_id: str) -> str:
    require(response.get("task_id") == task_id, "concurrent caller changed task identity")
    root = response.get("root_id")
    require(isinstance(root, str) and len(root) == 32 and
            all(character in "0123456789abcdef" for character in root),
            "concurrent caller omitted a canonical root identity")
    if exit_code == 0:
        require(response.get("admission") == "admitted", "successful caller did not prove admission")
        return root
    require(exit_code == 1 and response.get("admission") == "unknown" and
            isinstance(response.get("error"), str) and
            (response["error"].startswith("lock acquisition busy") or
             response["error"] == "task already submitted"),
            "concurrent caller failed for a reason other than held admission authority")
    return root


class InspectionAcceptance:
    def __init__(self, args: argparse.Namespace):
        self.tools = Path(args.tools).resolve(strict=True)
        self.pueue = Path(args.pueue).resolve(strict=True)
        self.pueued = Path(args.pueued).resolve(strict=True)
        self.output = Path(args.output).absolute()
        require(not self.output.exists(), "inspection evidence directory already exists")
        self.output = ensure_private_directory(self.output, "inspection evidence", create=True)
        self.base = ensure_private_directory(self.output / "fixture", "fixture base", create=True)
        self.state = self.base / "state"
        self.workspace = ensure_private_directory(self.base / "workspace", "fixture workspace", create=True)
        self.bindir = ensure_private_directory(self.base / "bin", "fixture binaries", create=True)
        self.inputs = ensure_private_directory(self.base / "inputs", "fixture inputs", create=True)
        self.events = ensure_private_directory(self.base / "events", "fixture events", create=True)
        self.cases = ensure_private_directory(self.output / "cases", "case evidence", create=True)
        for name in ("delegate", "delegate-run", "provider", "inspection-helper"):
            target = self.bindir / name
            shutil.copyfile(self.tools / name, target)
            target.chmod(0o700)
        self.delegate = self.bindir / "delegate"
        self.runner = self.bindir / "delegate-run"
        self.provider = self.bindir / "provider"
        self.helper = self.bindir / "inspection-helper"
        self.environment = inherited_environment()
        self.processes = Processes(self.output / "processes", self.environment)
        self.ops = NativeTaskOps(self.processes, self.delegate, self.runner, self.pueue,
                                 self.base, self.state, self.output, watch_seconds=60)
        self.daemon = None
        self.config = None
        self.pueue_base = None
        self.supervisor_config_digest = None
        self.sentinel = secrets.token_hex(32).encode()
        self.sentinel_file = self.inputs / "private-sentinel"
        write_bytes(self.sentinel_file, self.sentinel)
        self.case_records: dict[str, dict[str, object]] = {}
        self.serial = 0
        self.blocker: tuple[int, str] | None = None

    def setup(self) -> None:
        base = ensure_private_directory(Path("/Users/Shared") / ("dl-inspect-" + secrets.token_hex(6)),
                                        "private supervisor", create=True)
        self.pueue_base = base
        for name in ("state", "run"):
            ensure_private_directory(base / name, name, create=True)
        write_bytes(base / "aliases.yml", b"{}\n")
        self.config = base / "pueue.json"
        write_json(self.config, config_for(base))
        self.supervisor_config_digest = digest(self.config)
        for name, binary in (("pueue", self.pueue), ("pueued", self.pueued)):
            proc = self.ops.direct(name + "-version", [binary, "-c", self.config, "--version"])
            require((proc.directory / "stdout").read_text().strip() == name + " 4.0.4",
                    "unsupported supervisor fixture version")
        self.daemon = self.processes.start("daemon", [self.pueued, "-c", self.config], self.base)
        self.ops.bind_supervisor(self.config, self.daemon)
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            require(self.daemon.poll() is None, "fixture daemon exited during startup")
            proc = self.ops.client("ready", ["status", "--json"], expected={0, 1})
            if proc.result["exit_code"] == 0:
                require(not read_json(proc.directory / "stdout")["tasks"], "private queue was not empty")
                return
            time.sleep(0.1)
        raise AcceptanceFailure("private supervisor readiness expired")

    def configure(self, name: str, delay_ms: int) -> tuple[str, Path, Path, Path]:
        case = ensure_private_directory(self.cases / name, name, create=True)
        markers = ensure_private_directory(case / "helper", "helper markers", create=True)
        provider_records = ensure_private_directory(case / "provider", "provider markers", create=True)
        task_id = secrets.token_hex(16)
        provider_config = self.inputs / (name + "-provider.json")
        write_json(provider_config, {
            "scenario": "success", "artifact_dir": str(provider_records), "lifetime_ms": 1000,
            "task_id": task_id, "session_id": "inspection-" + task_id, "argv": [],
            "answer": "inspection-fixture-result",
        })
        write_json(self.bindir / "phase2-fixture.json", {
            "schema_version": 1, "provider_executable": str(self.provider),
            "provider_sha256": digest(self.provider), "provider_config": str(provider_config),
            "provider_config_sha256": digest(provider_config), "supervisor_executable": str(self.pueue),
            "events_directory": str(self.events),
            "environment": [key + "=" + value for key, value in sorted(self.environment.items())],
            "hooks": {"mode": "normal", "delay_ms": 0},
        }, replace=(self.bindir / "phase2-fixture.json").exists())
        helper_config = self.inputs / (name + "-helper.json")
        write_json(helper_config, {"schema_version": 1, "artifact_dir": str(markers),
                                   "sentinel_path": str(self.sentinel_file), "delay_ms": delay_ms,
                                   "eligible": True})
        write_json(self.bindir / "inspection-fixture.json", {
            "schema_version": 1, "helper_executable": str(self.helper),
            "helper_sha256": digest(self.helper), "helper_config": str(helper_config),
            "helper_config_sha256": digest(helper_config),
            "environment": [key + "=" + value for key, value in sorted(self.environment.items())],
        }, replace=(self.bindir / "inspection-fixture.json").exists())
        brief = self.inputs / (name + ".md")
        write_bytes(brief, b"inspection fixture finite input\n")
        return task_id, brief, case, helper_config

    def expected_inspection_binding(self, helper_config: Path) -> dict[str, object]:
        require(self.pueue_base is not None and self.config is not None and
                self.supervisor_config_digest is not None,
                "fixture supervisor binding is unavailable")
        supervisor = supervisor_binding(
            self.pueue, self.config, self.pueue_base, self.supervisor_config_digest)
        return fixture_inspection_binding(self.helper, helper_config, self.workspace,
                                          self.environment, self.runner, supervisor)

    def argv(self, task_id: str, brief: Path) -> list[object]:
        return [self.delegate, "--root", self.state, "--pueue-config", self.config,
                "--runner", self.runner, "dispatch", "--provider", "fixture:test",
                "--brief", brief, "--cwd", self.workspace, "--id", task_id,
                "--permission", "read-only", "--budget", "30s", "--json"]

    def queue(self) -> dict[str, object]:
        self.serial += 1
        return self.ops.queue_status("inspection-observation-" + str(self.serial))

    def wait_row(self, label: str, timeout: float = 45) -> dict[str, object]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            rows = [row for row in self.queue()["tasks"].values() if row.get("label") == label]
            require(len(rows) <= 1, "duplicate inspection fixture label")
            if rows and row_state(rows[0]) == "Done":
                require(done_result(rows[0]) is not None, "unknown fixture completion")
                return rows[0]
            time.sleep(0.2)
        raise AcceptanceFailure("inspection fixture completion observation expired")

    def wait_running_blocker(self, numeric: int, group: str, label: str,
                             timeout: float = 15) -> dict[str, object]:
        """Observe the exact blocker running before admitting queued expiry."""
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            rows = [row for row in self.queue()["tasks"].values()
                    if isinstance(row, dict) and row.get("label") == label]
            require(len(rows) <= 1, "duplicate bounded blocker label")
            if rows:
                row = rows[0]
                if row.get("id") == numeric and row.get("group") == group and row_state(row) == "Running":
                    validate_blocker_row(row, numeric, group, label, "Running")
                    return row
            time.sleep(0.2)
        raise AcceptanceFailure("bounded queue blocker did not reach Running")

    def success(self) -> None:
        task_id, brief, case, helper_config = self.configure("success", 1000)
        argv = self.argv(task_id, brief)
        self.ops.expect_inspection(task_id, self.expected_inspection_binding(helper_config))
        self.ops.dispatch_attempts.add(task_id)
        initial = self.processes.start("initial-dispatch", argv, self.base)
        deadline = time.monotonic() + 15
        while not list((case / "helper").rglob("started.json")):
            require(time.monotonic() < deadline, "initial helper did not start for reattachment")
            time.sleep(0.05)
        starts = list((case / "helper").rglob("started.json"))
        require(len(starts) == 1, "concurrent dispatch began after duplicate inspection work")
        require(initial.poll() is None, "initial dispatch completed before in-flight reattachment")
        # Reattach while the first caller still observes the same running
        # admission helper, not just after ordinary-task admission.
        # Multiple independent dispatchers exercise stable lock creation as
        # well as reattachment to the single already-running inspection.
        callers = [initial]
        for index in range(7):
            callers.append(self.processes.start("concurrent-dispatch-" + str(index), argv, self.base))
        for caller in callers:
            caller.wait(timeout=45, expected={0, 1})
        responses = [(caller, parse_json_output(caller, "concurrent dispatch"))
                     for caller in callers]
        # Retain every positive identity before evaluating a losing caller's
        # error, so failure cleanup can still verify the admitted ordinary job.
        for caller, response in responses:
            if caller.result["exit_code"] == 0:
                if "success" not in self.ops.tasks:
                    self.ops.record_admission("success", task_id, response)
                else:
                    supervisor = response.get("supervisor")
                    require(response.get("root_id") == self.ops.root_id and
                            isinstance(supervisor, dict) and supervisor.get("matched") is True and
                            type(supervisor.get("numeric_task_id")) is int and
                            supervisor["numeric_task_id"] == self.ops.numbers["success"],
                            "concurrent positive callers observed different supervisor identities")
        observed_roots = set()
        for caller, response in responses:
            observed_roots.add(validate_concurrent_response(response, caller.result["exit_code"], task_id))
        # Either contender may own admission. Its positive receipt plus the
        # exact queue and terminal evidence resolve the losing observation.
        require("success" in self.ops.tasks, "concurrent callers never proved ordinary admission")
        require(observed_roots == {self.ops.root_id}, "concurrent callers changed root identity")
        self.ops.wait_task("success", task_id)
        first = self.ops.collect("successful-task-collection", task_id)
        first_outcome, payload = verify_collected_outcome(first, self.state / "tasks" / task_id, "committed")
        require(payload == b"inspection-fixture-result", "fixture published the wrong payload")
        before = snapshot(self.state)
        provider_before = snapshot(case / "provider")
        helper_before = snapshot(case / "helper")
        queue_before = canonical_queue(self.queue())
        replay = self.ops.collect("success-replay", task_id)
        replay_outcome, replay_payload = verify_collected_outcome(
            replay, self.state / "tasks" / task_id, "committed")
        require(replay_outcome == first_outcome and replay_payload == payload and
                replay.get("payload") == first.get("payload") and
                replay.get("evidence_sha256") == first.get("evidence_sha256"),
                "collection replay changed outcome, payload or evidence hash")
        require(snapshot(self.state) == before, "collection replay changed inspection/task evidence")
        require(snapshot(case / "provider") == provider_before and
                snapshot(case / "helper") == helper_before,
                "collection replay changed fixture artifacts")
        require(canonical_queue(self.queue()) == queue_before,
                "collection replay changed the private supervisor queue")
        require(len(list((case / "helper").rglob("started.json"))) == 2,
                "success/reattachment did not preserve exactly two inspection launches")
        require(len(list((case / "helper").rglob("completed.json"))) == 2,
                "successful helpers did not naturally complete")
        self.case_records["success"] = {"task_id": task_id, "concurrent_callers": len(callers),
                                         "helper_starts": 2,
                                         "reattach_launches": 0, "collection_launches": 0}

    def failed_inspection(self, name: str, delay_ms: int) -> None:
        task_id, brief, case, helper_config = self.configure(name, delay_ms)
        self.ops.expect_inspection(task_id, self.expected_inspection_binding(helper_config))
        self.ops.dispatch_attempts.add(task_id)
        proc = self.ops.direct(name, self.argv(task_id, brief), expected={1, 2}, timeout=45)
        response = parse_json_output(proc, name)
        require(proc.result["exit_code"] == 1 and response.get("task_id") == task_id and
                response.get("root_id") == self.ops.root_id and
                response.get("admission") == "unknown" and
                response.get("error") == "inspection admission deadline expired",
                "expired inspection did not return the exact admission deadline error")
        label = "delegation-inspection-" + str(self.ops.root_id) + "-" + task_id
        row = self.wait_row(label)
        require(done_result(row) != "Success", "expired inspection worker reported success")
        require(not (self.state / "tasks" / task_id).exists(), "failed inspection created ordinary task")
        require(not list((case / "provider").iterdir()), "failed inspection started fixture provider")
        starts = list((case / "helper").rglob("started.json"))
        expected = 1 if delay_ms else 0
        require(len(starts) == expected, "expired inspection helper start count mismatch")
        validate_expired_inspection(self.state, task_id, row, bool(delay_ms))
        if delay_ms:
            require(len(starts) == 1 and not starts[0].is_symlink(),
                    "running expiry helper marker is not a regular file")
            helper_started_at = starts[0].stat().st_mtime_ns / 1_000_000_000
            # If the inherited helper survived, its bounded code would finish by
            # this point and write completed.json. No PID-based probe or signal.
            remaining = delay_ms / 1000 + 2 - (time.time() - helper_started_at)
            if remaining > 0:
                time.sleep(remaining)
            require(not list((case / "helper").rglob("completed.json")),
                    "helper survived the supervisor stop to natural completion")
        self.case_records[name] = {"task_id": task_id, "helper_starts": expected,
                                    "ordinary_task_created": False, "worker_result": done_result(row)}

    def queued_expiry(self) -> None:
        group = "delegation-inspection-" + str(self.ops.root_id)
        label = "inspection-fixture-bounded-blocker"
        proc = self.ops.client("queue-blocker", ["add", "--escape", "--group", group, "--label", label,
                                                "--print-task-id", "--", "/bin/sleep", "25"])
        numeric = int((proc.directory / "stdout").read_text().strip())
        require(numeric >= 0, "invalid blocker supervisor target")
        self.blocker = numeric, label
        self.wait_running_blocker(numeric, group, label)
        self.failed_inspection("queued-expiry", 0)
        row = self.wait_row(label)
        validate_blocker_row(row, numeric, group, label, "Done")
        require(done_result(row) == "Success", "bounded queue blocker did not complete successfully")
        self.ops.client("remove-completed-blocker", ["remove", str(numeric)])
        require(all(row.get("id") != numeric for row in self.queue()["tasks"].values()),
                "completed blocker removal was not observed")
        self.blocker = None

    def cleanup_blocker_after_failure(self) -> bool:
        """Retain the bounded blocker until its exact natural completion."""
        if self.blocker is None:
            return True
        numeric, label = self.blocker
        if self.ops.root_id is None:
            return False
        group = "delegation-inspection-" + self.ops.root_id
        while True:
            if self.daemon is None or self.daemon.poll() is not None:
                return False
            try:
                rows = [row for row in self.queue()["tasks"].values()
                        if isinstance(row, dict) and row.get("label") == label]
                require(len(rows) == 1, "bounded blocker disappeared during failure cleanup")
                row = rows[0]
                if row_state(row) != "Done":
                    time.sleep(0.25)
                    continue
                validate_blocker_row(row, numeric, group, label, "Done")
                require(done_result(row) == "Success",
                        "bounded blocker did not complete successfully during cleanup")
                self.ops.client("failure-remove-blocker", ["remove", str(numeric)], timeout=20)
                require(all(candidate.get("id") != numeric for candidate in self.queue()["tasks"].values()),
                        "failure cleanup did not remove the completed blocker")
                self.blocker = None
                return True
            except (AcceptanceFailure, OSError, RuntimeError, TypeError, ValueError):
                return False

    def run(self) -> None:
        try:
            self.setup()
            self.success()
            self.failed_inspection("running-expiry", 30000)
            self.queued_expiry()
            require(self.ops.safe_failure_shutdown(), "exact terminal fixture queue was not safely shut down")
            no_secret_bytes([self.workspace, self.state, self.events, self.cases, self.output / "processes",
                             self.pueue_base / "state"], self.sentinel)
            write_json(self.output / "success.json", {
                "status": "passed", "native_ai_turns": 0, "cases": self.case_records,
                "natural_daemon_shutdown": True, "signals_sent": 0,
                "driver_sha256": digest(__file__), "sentinel_escaped": False,
            })
            print("PASS native inspection acceptance", flush=True)
        except BaseException as error:
            blocker_cleaned = False
            try:
                write_json(self.output / "failure.json", {"status": "failed", "cases": self.case_records,
                            "error": str(error),
                            "natural_daemon_shutdown": self.ops.closed, "signals_sent": 0,
                            "state": str(self.state), "supervisor_config": str(self.config)})
            finally:
                # Diagnostic persistence cannot release ownership of live work.
                try:
                    blocker_cleaned = self.cleanup_blocker_after_failure()
                finally:
                    cleaned = self.ops.retain_failure_ownership()
                    write_json(self.output / "failure-cleanup.json", {
                        "blocker_cleaned": blocker_cleaned, "natural_daemon_shutdown": cleaned,
                        "signals_sent": 0, "owned_processes_joined": True,
                    })
            raise


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("tools", "pueue", "pueued", "output"):
        parser.add_argument("--" + name, required=True)
    try:
        InspectionAcceptance(parser.parse_args()).run()
    except (AcceptanceFailure, OSError, ValueError, RuntimeError) as error:
        print("FAIL native inspection acceptance: " + str(error), flush=True)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
