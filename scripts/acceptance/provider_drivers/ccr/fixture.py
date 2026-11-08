#!/usr/bin/env python3
"""Prove real Delegate -> CCR -> Claude lifecycle against a local HTTP fixture.

No account or paid model is used. Provider setup exists only in this run's XDG
root. The shared native gate owns the private supervisor and failure cleanup.
"""

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[4])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]

import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
import os
import select
import threading
import time


from scripts.acceptance.native.cli import NativeAcceptance
from scripts.acceptance.native.cli import build_parser
from scripts.acceptance.native.cli import choose_output
from scripts.acceptance.evidence.records import bounded_text
from scripts.acceptance.evidence.records import status_jobs
from scripts.acceptance.evidence.validation import _task_identity
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.native import lifecycle as acceptance_lifecycle
from scripts.acceptance.tasks.operations import AdmissionFailure
from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.directories.operations import rollback_owned_directories
from scripts.acceptance.evidence.files import write_json


class Fixture:
    def __init__(self):
        self.seen = {name: threading.Event() for name in ("cancel", "budget")}
        self.disconnected = {name: threading.Event() for name in self.seen}
        self.parallel = threading.Barrier(2, timeout=45)
        self.requests = 0
        self.model_list_requests = 0
        self.parallel_requests = 0
        self.blocking_requests = {name: 0 for name in self.seen}
        self.lock = threading.Lock()
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, format: str, *args: object) -> None:
                pass

            def do_GET(self):
                with fixture.lock:
                    fixture.model_list_requests += 1
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"object": "list", "data": [
                    {"id": name, "object": "model"} for name in ("ok", "parallel", "write", "cancel", "budget")
                ]}).encode())

            def do_POST(self):
                length = int(self.headers.get("Content-Length", "0"))
                if length > 8 * 1024 * 1024:
                    self.send_error(413)
                    return
                request = json.loads(self.rfile.read(length))
                with fixture.lock:
                    fixture.requests += 1
                    model = request.get("model")
                    if model == "parallel":
                        fixture.parallel_requests += 1
                        parallel_request = fixture.parallel_requests
                    else:
                        parallel_request = 0
                    if model in fixture.seen:
                        fixture.blocking_requests[model] += 1
                        blocking_request = fixture.blocking_requests[model]
                    else:
                        blocking_request = 0
                if model == "parallel" and parallel_request <= 2:
                    fixture.parallel.wait()
                if model in fixture.seen and blocking_request == 1:
                    fixture.seen[model].set()
                    deadline = time.monotonic() + 150
                    while time.monotonic() < deadline:
                        ready, _, _ = select.select([self.connection], [], [], 0.2)
                        if ready and not self.connection.recv(1):
                            fixture.disconnected[model].set()
                            return
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                first_delta: dict[str, object] = {"role": "assistant", "content": "CCR_FIXTURE_OK"}
                finish_reason = "stop"
                messages = request.get("messages", [])
                if model == "write" and not any(m.get("role") == "tool" for m in messages):
                    command = "printf created > fixture-new.txt; printf edited > fixture-new.txt; printf changed > fixture-existing.txt; cat fixture-new.txt > fixture-shell.txt"
                    first_delta = {"tool_calls": [{"index": 0,
                        "id": "call-fixture-write", "type": "function", "function": {
                            "name": "Bash", "arguments": json.dumps({"command": command})}}]}
                    finish_reason = "tool_calls"
                frames: list[dict[str, object]] = [
                    {"choices": [{"index": 0, "delta": first_delta}]},
                    {"choices": [{"index": 0, "delta": {}, "finish_reason": finish_reason}]},
                    {"choices": [], "usage": {"prompt_tokens": 8, "completion_tokens": 4}},
                ]
                for frame in frames:
                    frame["id"] = "chatcmpl-ccr-fixture"
                    self.wfile.write(("data: " + json.dumps(frame) + "\n\n").encode())
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_args):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(5)


def require_user_cancellation(response, root_id, task_id):
    """Require Delegate's closed user-cancellation collect envelope for this exact task."""
    require((response.get("command"), response.get("root_id"), response.get("task_id")) ==
            ("collect", root_id, task_id), "cancel collect changed task identity")
    require(response.get("status") == "interrupted" and response.get("publication") == "unknown" and
            "outcome" not in response, "cancelled task was not reported as an unpublished interruption")
    require(response.get("failure") == {"code": "task_interrupted", "stage": "collect",
                                        "next_action": "stop_and_report"},
            "cancelled task did not report task_interrupted/stop_and_report")
    continuation = response.get("continuation")
    require(isinstance(continuation, dict) and continuation.get("resumable") is False and
            continuation.get("mode") == "native" and continuation.get("predecessor_task_id") == task_id and
            continuation.get("reason") == "task was cancelled by the user" and
            "continue_command" not in continuation,
            "cancelled task offered automatic continuation")


# This fixture's own gate; the shared native lifecycle commits and publishes its receipt.
FIXTURE_GATE = "acceptance-ccr-fixture"
REFUSAL_CLEANUP_GATE = "acceptance-ccr-refusal-cleanup"
CPU_SNAPSHOT_NAME = "failure-host-cpu-snapshot.json"
CPU_SNAPSHOT_BYTES = 256 * 1024
CPU_SNAPSHOT_ROWS = 4096
CPU_SNAPSHOT_ARGV = ("/bin/ps", "-A", "-o", "pcpu=,comm=")
CPU_SNAPSHOT_ROLES = frozenset((
    "delegate", "delegate-run", "pueue", "pueued", "ccr", "claude", "node",
    "python", "python3", "python3.12", "ps", "mds", "mds_stores", "mdworker_shared",
    "syspolicyd", "xprotectservice", "amfid", "kernel_task",
    "runner.worker", "runner.listener", "trustd", "xprotect",
))


def confirmed_host_cpu_refusal(observation: object, root: object) -> bool:
    if not isinstance(observation, dict):
        return False
    response = observation.get("response")
    task = observation.get("task_id")
    if not isinstance(response, dict) or not _task_identity(task):
        return False
    resource = response.get("resource_admission")
    return (observation.get("natural_wait") is True and type(observation.get("exit_code")) is int and
            observation.get("exit_code") == 1 and type(response.get("schema_version")) is int and
            response.get("schema_version") == 1 and response.get("command") == "dispatch" and
            _task_identity(response.get("root_id")) and
            (root is None or (_task_identity(root) and response.get("root_id") == root)) and
            response.get("task_id") == task and
            response.get("task_record") == "unknown" and response.get("admission") == "unknown" and
            response.get("liveness") == "undetermined" and response.get("publication") == "unknown" and
            response.get("failure") == {"code": "resource_pressure", "stage": "check-resources",
                                        "reason_code": "host_cpu_sustained", "next_action": "stop_and_report"} and
            isinstance(resource, dict) and resource.get("status") == "pressure")


def cpu_snapshot_rows(text: str) -> dict[str, object]:
    """Project one native historical-percent listing, never identity or closure."""
    if len(text.encode("utf-8")) > CPU_SNAPSHOT_BYTES:
        raise ValueError("cpu_snapshot_byte_limit")
    lines = text.splitlines()
    if not lines or len(lines) > CPU_SNAPSHOT_ROWS:
        raise ValueError("cpu_snapshot_row_limit_or_empty")
    rows: list[tuple[float, str]] = []
    counts: dict[str, int] = {}
    for line in lines:
        fields = line.split(maxsplit=1)
        if len(fields) != 2:
            raise ValueError("cpu_snapshot_malformed_row")
        percent = float(fields[0])
        if not math.isfinite(percent) or percent < 0:
            raise ValueError("cpu_snapshot_invalid_percent")
        basename = Path(fields[1]).name.lower()
        if not basename:
            raise ValueError("cpu_snapshot_missing_command")
        role = basename if basename in CPU_SNAPSHOT_ROLES else "other"
        rows.append((percent, role))
        counts[role] = counts.get(role, 0) + 1
    rows.sort(key=lambda row: row[0], reverse=True)
    return {"listed_process_count": counts,
            "top_cpu": [{"role": role, "native_cpu_percent": percent} for percent, role in rows[:8]]}


class FixtureAcceptance(NativeAcceptance):
    # Unset until the exercise completes, so a premature success commit is refused, not an AttributeError.
    success_receipt: dict[str, object] | None = None

    def capture_cpu_snapshot(self, failure_observed_at: float) -> None:
        observations = self.ops.dispatch_observations
        if not observations or not confirmed_host_cpu_refusal(observations[-1], self.root_id):
            return
        receipt: dict[str, object] = {
            "method": "native_ps", "argv": ["ps", "-A", "-o", "pcpu=,comm="],
            "timing": "after_resource_refusal", "basis": "native_platform_historical_percent",
            "platform": os.uname().sysname,
            "origin": "driver_failure_observed",
            "capture_started_offset_ms": int((time.monotonic() - failure_observed_at) * 1000),
            "admission_intervals_matched": False, "causal_attribution": False,
        }
        reason = "capture_unavailable"
        try:
            process = self.processes.run("failure-host-cpu", list(CPU_SNAPSHOT_ARGV), self.output,
                expected=None, timeout=5, environment={**self.processes.environment, "LC_ALL": "C"})
            reason = "native_completion_unavailable"
            require(isinstance(process.result, dict) and process.result.get("natural_wait") is True and
                    type(process.result.get("exit_code")) is int and process.result.get("exit_code") == 0,
                    "native CPU listing did not complete")
            reason = "output_unavailable"
            text = bounded_text(process.directory / "stdout", "native CPU listing", CPU_SNAPSHOT_BYTES)
            receipt["capture_completed_offset_ms"] = int((time.monotonic() - failure_observed_at) * 1000)
            reason = "invalid_output"
            receipt.update(cpu_snapshot_rows(text), status="observed")
        except BaseException:
            # A secondary diagnostic interruption must not bypass the existing sequential cleanup.
            receipt.update(status="unavailable", reason=reason)
        write_json(self.output / CPU_SNAPSHOT_NAME, receipt)

    def __init__(self, args, fixture_directories=()):
        self.fixture_directories = tuple(fixture_directories)
        try:
            super().__init__(args)
        except BaseException as error:
            try:
                rollback_owned_directories(self.fixture_directories)
            except BaseException as rollback_error:
                raise RuntimeError(
                    "CCR fixture constructor rollback failed: " + str(rollback_error)) from error
            self.fixture_directories = ()
            raise

    def finalize(self, success: bool, *, blocked: bool = False,
                 shutdown_attempted: bool = False) -> bool:
        if not super().finalize(success, blocked=blocked, shutdown_attempted=shutdown_attempted):
            return False
        try:
            rollback_owned_directories(self.fixture_directories)
        except BaseException:
            return False
        self.fixture_directories = ()
        return True

    def dispatch_argv(self, task_id, brief, predecessor=None, budget=None):
        argv = super().dispatch_argv(task_id, brief, predecessor,
                                    budget if budget is not None else "5m")
        if self.args.model == "budget":
            argv[argv.index("--budget") + 1] = "20s"
        if not self.args.cpu_guard:
            argv.append("--cpu-guard=false")
        return argv

    def command(self, name, *args, expected=0):
        return self.direct(name, [self.delegate, "--root", self.state,
            "--pueue-config", self.config, "--runner", self.runner, *args], expected=expected, timeout=45)

    def published(self, name, task_id):
        self.wait_done(name, task_id)
        response = self.command(name + "-collect", "collect", task_id, "--json").json()
        require(response.get("publication") == "committed", "fixture result was not committed")
        directory = self.state / "tasks" / task_id
        require((directory / "result.txt").read_text().strip() == "CCR_FIXTURE_OK", "fixture result changed")
        return self.session_ref(task_id)

    def _wait_stopped(self, model: str, task_id: str) -> None:
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            tasks = status_jobs(self.ops.client(model + "-status", ["status", "--json"]))["tasks"]
            if not isinstance(tasks, dict):
                raise RuntimeError("fixture queue status has no task map")
            row = tasks.get(str(self.numeric_ids[task_id]))
            if not isinstance(row, dict):
                raise RuntimeError("fixture task is absent from queue status")
            if isinstance(row["status"], dict) and "Done" in row["status"]:
                break
            time.sleep(0.2)
        else:
            raise RuntimeError(model + " task did not stop")

    def exercise(self, fixture):
        self.setup()
        self.ops.client("fixture-concurrency", ["parallel", "0"])
        brief = self.briefs / "fixture.txt"
        brief.write_text("Reply with CCR_FIXTURE_OK. Do not call tools.")
        self.args.model = "parallel"
        first = self.dispatch("first", brief)
        second = self.dispatch("parallel", brief)
        first_session = self.published("first", first)
        second_session = self.published("parallel", second)
        require(first_session != second_session, "parallel tasks shared a session")
        with fixture.lock:
            model_lists_before_resume = fixture.model_list_requests
        resumed = self.dispatch("resumed", brief, first)
        require(self.published("resumed", resumed) == first_session, "continuation changed session")
        with fixture.lock:
            require(fixture.model_list_requests == model_lists_before_resume,
                    "CCR continuation repeated remote model discovery")
        self.args.model = "write"
        self.mode = "workspace-write"
        (self.workspace / "fixture-existing.txt").write_text("original")
        write_brief = self.briefs / "write.txt"
        write_brief.write_text("Create and edit fixture-new.txt, edit fixture-existing.txt, and execute a shell command to copy fixture-new.txt to fixture-shell.txt. Reply CCR_FIXTURE_OK.")
        written = self.dispatch("written", write_brief)
        self.published("written", written)
        for name, expected in (("fixture-new.txt", "edited"), ("fixture-existing.txt", "changed"), ("fixture-shell.txt", "edited")):
            require((self.workspace / name).read_text() == expected, "native write/shell evidence missing")
        self.mode = "read-only"
        with fixture.lock:
            requests_before = fixture.requests
        directory = self.state / "tasks" / first
        before = {name: (directory / name).read_bytes() for name in ("outcome.json", "result.txt", "provider.exit")}
        self.command("replay", "collect", first, "--json")
        require(all((directory / name).read_bytes() == data for name, data in before.items()), "replay mutated evidence")
        with fixture.lock:
            require(fixture.requests == requests_before, "replay called the provider")
        for model in ("cancel", "budget"):
            self.args.model = model
            task_id = self.dispatch(model, brief)
            require(fixture.seen[model].wait(40), model + " never reached the fixture")
            if model == "cancel":
                result = self.command("cancel-request", "cancel", task_id, "--json")
                require(result.result["exit_code"] == 0, "cancel failed")
            self._wait_stopped(model, task_id)
            # A closed user cancellation is a terminal interruption (exit 1); a budget stop stays pending (exit 3).
            stopped = self.command(model + "-collect", "collect", task_id, "--json",
                                   expected=1 if model == "cancel" else 3).json()
            if model == "cancel":
                require_user_cancellation(stopped, self.root_id, task_id)
            require(stopped.get("publication") != "committed", "stopped task committed")
            directory = self.state / "tasks" / task_id
            require((directory / "stop").is_dir(), "stopped task lacks durable stop evidence")
            require(not (directory / "result.txt").exists(), "stopped task published a result")
            require(fixture.disconnected[model].wait(15), "CCR gateway retained its upstream request after stop")
        stopped_session = self.session_ref(task_id)
        with fixture.lock:
            model_lists_before_budget_resume = fixture.model_list_requests
        recovered = self.dispatch("budget-resumed", brief, task_id)
        require(self.published("budget-resumed", recovered) == stopped_session, "budget continuation lost its session")
        with fixture.lock:
            require(fixture.model_list_requests == model_lists_before_budget_resume,
                    "budget continuation repeated remote model discovery")
        self.args.model = "missing-alias"
        rejected = self.dispatch("missing", brief)
        self.wait_done("missing", rejected)
        response = self.command("missing-collect", "collect", rejected, "--json", expected=4).json()
        require(response.get("publication") == "rejected", "unknown alias did not produce a rejected outcome")
        self.success_receipt = {"gate": FIXTURE_GATE, "status": "passed", "platform": os.uname().sysname,
            "tasks": self.tasks, "sessions_isolated": True, "continuation": True, "budget_continuation": True, "continuation_reuses_route": True, "native_write_and_shell": True,
            "replay_observational": True, "invalid_alias_rejected": True,
            "cancel_and_budget_disconnected": True, "real_clis": ["delegate", "ccr", "claude"]}
        return self.success_receipt


class RefusalCleanupAcceptance(FixtureAcceptance):
    """Only the second admission is synthetic; warm work and cleanup are real."""

    def __init__(self, args, fixture_directories=()):
        super().__init__(args, fixture_directories)
        self.inject_refusal = False
        self.refused_task_id: str | None = None
        self.observed_absence: frozenset[str] = frozenset()

    def dispatch_argv(self, task_id, brief, predecessor=None, budget=None):
        if not self.inject_refusal:
            return super().dispatch_argv(task_id, brief, predecessor, budget)
        require(self.refused_task_id is None and predecessor is None and _task_identity(self.root_id),
                "refusal fixture may replace only one dispatch into its warmed root")
        self.refused_task_id = task_id
        self.inject_refusal = False
        fixture = Path(__file__).resolve().parents[2] / "private_cli/refusal_fixture.py"
        return [sys.executable, fixture, self.root_id, task_id]

    def exercise(self, fixture):
        self.setup()
        brief = self.briefs / "refusal-cleanup.txt"
        brief.write_text("Reply with CCR_FIXTURE_OK. Do not call tools.")
        self.args.model = "ok"
        warm = self.dispatch("warm", brief)
        self.published("warm", warm)
        self.inject_refusal = True
        try:
            self.dispatch("refused", brief)
        except AdmissionFailure as error:
            observations = self.ops.dispatch_observations
            require(self.refused_task_id is not None and len(observations) == 2
                    and observations[-1].get("task_id") == self.refused_task_id
                    and observations[-1].get("response") == error.response
                    and confirmed_host_cpu_refusal(observations[-1], self.root_id),
                    "expected only the injected explicit-ID resource refusal")
            response = dict(error.response)
        else:
            raise RuntimeError("refusal fixture unexpectedly admitted work")
        self.success_receipt = {
            "gate": REFUSAL_CLEANUP_GATE, "status": "passed", "platform": os.uname().sysname,
            "warm_task_id": warm, "controlled_refusal": True,
            "original_failure": {"status": "failed", "response": response},
        }
        return self.success_receipt

    def _prove_absent_resource_refusals(self, tasks: frozenset[str], deadline: float) -> frozenset[str]:
        self.observed_absence = frozenset()
        proof = super()._prove_absent_resource_refusals(tasks, deadline)
        self.observed_absence = proof
        return proof

    def safe_shutdown(self) -> bool:
        if not super().safe_shutdown():
            return False
        if self.success_receipt is None:
            return True
        require(self.closed and self.refused_task_id is not None
                and self.observed_absence == frozenset((self.refused_task_id,)),
                "shared cleanup did not prove the injected refusal absent")
        for process in self.processes.entries:
            require(not (process.argv[0] == str(self.delegate) and self.refused_task_id in process.argv),
                    "refused task reached another Delegate dispatch, observation or cancellation")
        result = getattr(self.daemon, "result", None)
        require(isinstance(result, dict) and result.get("natural_wait") is True
                and result.get("exit_code") == 0, "private daemon was not naturally joined")
        acceptance_lifecycle.record_lifetime_closure(self)
        self.success_receipt.update(acceptance_lifecycle.lifecycle_receipt(self))
        self.success_receipt.update(refusal_absence_proven=True, refused_task_controls=0,
                                    daemon_wait={"natural_wait": True, "exit_code": 0})
        return True


def cleanup_fixture(run, success):
    cleanup_errors = []
    shutdown_completed = False
    artifact_finalized = False
    try:
        if not run.safe_shutdown():
            cleanup_errors.append(RuntimeError("private supervisor cleanup was not proven"))
        else:
            shutdown_completed = True
    except BaseException as error:
        cleanup_errors.append(error)
    try:
        if not run.finalize(success and not cleanup_errors, shutdown_attempted=True):
            cleanup_errors.append(RuntimeError("fixture artifact finalization was not completed"))
        else:
            artifact_finalized = True
    except BaseException as error:
        cleanup_errors.append(error)
    if cleanup_errors:
        output = getattr(run, "output", None)
        if isinstance(output, Path):
            try:
                (output / "success.json").unlink(missing_ok=True)
                write_json(output / "failure-cleanup.json", {
                    "status": "failed",
                    "natural_daemon_shutdown": shutdown_completed,
                    "artifact_finalization_completed": artifact_finalized,
                    "passed_receipt_absent": not (output / "success.json").exists(),
                    "signals_sent": 0,
                }, replace=True)
            except BaseException:
                pass
        raise RuntimeError("fixture cleanup failed; inspect retained evidence: "
                           + str(cleanup_errors[0])) from cleanup_errors[0]


def prepare_fixture_acceptance(output, *, refusal_cleanup=False, cpu_guard=True):
    ownership = DirectoryOwner()
    try:
        isolated = ownership.create("CCR fixture profile", output / "profile", output).directory
        data = ownership.create("CCR fixture data", isolated / "data", isolated).directory
        claude = ownership.create("CCR fixture Claude config", isolated / "claude", isolated).directory
        os.environ["XDG_DATA_HOME"] = str(data)
        os.environ["CLAUDE_CONFIG_DIR"] = str(claude)
        args = build_parser().parse_args([
            "--provider", "ccr:print", "--model", "ok", "--output", str(output),
        ])
        args.cpu_guard = cpu_guard
        directories = ownership.transfer()
    except BaseException:
        ownership.rollback()
        raise
    acceptance = RefusalCleanupAcceptance if refusal_cleanup else FixtureAcceptance
    return acceptance(args, directories)


def main():
    parser = argparse.ArgumentParser(prog='acceptance-ccr', description=__doc__)
    parser.add_argument("--output")
    parser.add_argument("--cpu-guard", choices=("true", "false"), default="true",
                        help="control CPU admission checks for positive fixture tasks (default: true)")
    parser.add_argument("--refusal-cleanup", action="store_true",
                        help="prove cleanup after one controlled refusal following real warm work")
    options = parser.parse_args()
    output = choose_output("ccr-fixture", options.output)
    run = None
    success_receipt = None
    primary_error = None
    try:
        run = prepare_fixture_acceptance(output, refusal_cleanup=options.refusal_cleanup,
                                         cpu_guard=options.cpu_guard == "true")
        with Fixture() as fixture:
            url = f"http://127.0.0.1:{fixture.server.server_port}"
            run.direct("provider-setup", [run.provider_executable, "provider", "add", "fixture",
                "--type", "local", "--base-url", url, "--no-api-key"])
            for model in ("ok", "parallel", "write", "cancel", "budget"):
                run.direct("model-setup-" + model, [run.provider_executable, "model", "add", model,
                    "--provider", "fixture", "--model", model, "--compat", "full"])
            success_receipt = run.exercise(fixture)
    except BaseException as error:
        primary_error = error
        failure_observed_at = time.monotonic()
        if run is not None and isinstance(error, Exception):
            try:
                run.capture_cpu_snapshot(failure_observed_at)
            except BaseException:
                error.add_note("host_cpu_snapshot_receipt_unavailable")
    cleanup_error = None
    if run is not None:
        try:
            cleanup_fixture(run, success_receipt is not None and primary_error is None)
        except BaseException as error:
            cleanup_error = error
    if primary_error is None and cleanup_error is None:
        try:
            # Publish only the confirmed candidate bytes, after every owned cleanup completed.
            if run is None:
                raise RuntimeError("CCR fixture acceptance was not prepared")
            run.publish_success(expected_gate=REFUSAL_CLEANUP_GATE if options.refusal_cleanup else FIXTURE_GATE)
        except BaseException as error:
            primary_error = error
    if primary_error is not None or cleanup_error is not None:
        error = cleanup_error or primary_error
        (output / "success.json").unlink(missing_ok=True)
        try:
            write_json(output / "failure.json", {
                "status": "failed",
                "error": str(error),
                "behavior_completed": success_receipt is not None,
                "passed_receipt_absent": not (output / "success.json").exists(),
                "signals_sent": 0,
            }, replace=True)
        except BaseException:
            pass
        if cleanup_error is not None:
            raise cleanup_error from primary_error
        if primary_error is not None:
            raise primary_error
    print(json.dumps({"status": "passed", "evidence": str(output)}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
