#!/usr/bin/env python3
"""Prove real Delegate -> CCR -> Claude lifecycle against a local HTTP fixture.

No account or paid model is used. Provider setup exists only in this run's XDG
root. The shared native gate owns the private supervisor and failure cleanup.
"""
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import select
import threading
import time

from acceptance_native import NativeAcceptance, build_parser, choose_output
from acceptance_provider_common import require
from acceptance_supervisor_common import (
    DirectoryOwner,
    rollback_owned_directories,
    write_json,
)


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
            def log_message(self, *_args):
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
                frames = [
                    {"choices": [{"index": 0, "delta": {"role": "assistant", "content": "CCR_FIXTURE_OK"}}]},
                    {"choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]},
                    {"choices": [], "usage": {"prompt_tokens": 8, "completion_tokens": 4}},
                ]
                messages = request.get("messages", [])
                if model == "write" and not any(m.get("role") == "tool" for m in messages):
                    command = "printf created > fixture-new.txt; printf edited > fixture-new.txt; printf changed > fixture-existing.txt; cat fixture-new.txt > fixture-shell.txt"
                    frames[0]["choices"][0]["delta"] = {"tool_calls": [{"index": 0,
                        "id": "call-fixture-write", "type": "function", "function": {
                            "name": "Bash", "arguments": json.dumps({"command": command})}}]}
                    frames[1]["choices"][0]["finish_reason"] = "tool_calls"
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


class FixtureAcceptance(NativeAcceptance):
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

    def finalize(self, success):
        if not super().finalize(success):
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
            deadline = time.monotonic() + 90
            while time.monotonic() < deadline:
                row = self.ops.client(model + "-status", ["status", "--json"]).json()["tasks"][str(self.numeric_ids[task_id])]
                if isinstance(row["status"], dict) and "Done" in row["status"]:
                    break
                time.sleep(0.2)
            else:
                raise RuntimeError(model + " task did not stop")
            stopped = self.command(model + "-collect", "collect", task_id, "--json", expected=3).json()
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
        return {"status": "passed", "platform": os.uname().sysname,
            "tasks": self.tasks, "sessions_isolated": True, "continuation": True, "budget_continuation": True, "continuation_reuses_route": True, "native_write_and_shell": True,
            "replay_observational": True, "invalid_alias_rejected": True,
            "cancel_and_budget_disconnected": True, "real_clis": ["delegate", "ccr", "claude"]}


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
        if not run.finalize(success and not cleanup_errors):
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


def prepare_fixture_acceptance(output):
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
        directories = ownership.transfer()
    except BaseException:
        ownership.rollback()
        raise
    return FixtureAcceptance(args, directories)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output")
    options = parser.parse_args()
    output = choose_output("ccr-fixture", options.output)
    run = None
    success_receipt = None
    primary_error = None
    try:
        run = prepare_fixture_acceptance(output)
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
    cleanup_error = None
    if run is not None:
        try:
            cleanup_fixture(run, success_receipt is not None and primary_error is None)
        except BaseException as error:
            cleanup_error = error
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
        raise primary_error
    require(isinstance(success_receipt, dict), "fixture success receipt is unavailable")
    write_json(output / "success.json", success_receipt, replace=True)
    print(json.dumps({"status": "passed", "evidence": str(output)}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
