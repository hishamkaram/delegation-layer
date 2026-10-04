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
from acceptance_supervisor_common import write_json


class Fixture:
    def __init__(self):
        self.seen = {name: threading.Event() for name in ("cancel", "budget")}
        self.disconnected = {name: threading.Event() for name in self.seen}
        self.parallel = threading.Barrier(2, timeout=45)
        self.requests = 0
        self.lock = threading.Lock()
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_GET(self):
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
                    fixture.parallel.wait()
                if model in fixture.seen:
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
    def dispatch_argv(self, task_id, brief, predecessor=None):
        argv = super().dispatch_argv(task_id, brief, predecessor)
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
        self.args.model = "ok"
        resumed = self.dispatch("resumed", brief, first)
        require(self.published("resumed", resumed) == first_session, "continuation changed session")
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
        self.args.model = "ok"
        recovered = self.dispatch("budget-resumed", brief, task_id)
        require(self.published("budget-resumed", recovered) == stopped_session, "budget continuation lost its session")
        self.args.model = "missing-alias"
        rejected = self.dispatch("missing", brief)
        self.wait_done("missing", rejected)
        response = self.command("missing-collect", "collect", rejected, "--json", expected=4).json()
        require(response.get("publication") == "rejected", "unknown alias did not produce a rejected outcome")
        require(self.safe_shutdown(), "private supervisor cleanup was not proven")
        write_json(self.output / "success.json", {"status": "passed", "platform": os.uname().sysname,
            "tasks": self.tasks, "sessions_isolated": True, "continuation": True, "budget_continuation": True, "native_write_and_shell": True,
            "replay_observational": True, "invalid_alias_rejected": True,
            "cancel_and_budget_disconnected": True, "real_clis": ["delegate", "ccr", "claude"]})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output")
    options = parser.parse_args()
    output = choose_output("ccr-fixture", options.output)
    isolated = output / "profile"
    isolated.mkdir()
    os.environ["XDG_DATA_HOME"] = str(isolated / "data")
    os.environ["CLAUDE_CONFIG_DIR"] = str(isolated / "claude")
    Path(os.environ["XDG_DATA_HOME"]).mkdir()
    Path(os.environ["CLAUDE_CONFIG_DIR"]).mkdir()
    args = build_parser().parse_args(["--provider", "ccr:print", "--model", "ok", "--output", str(output)])
    run = FixtureAcceptance(args)
    try:
        with Fixture() as fixture:
            url = f"http://127.0.0.1:{fixture.server.server_port}"
            run.direct("provider-setup", [run.provider_executable, "provider", "add", "fixture",
                "--type", "local", "--base-url", url, "--no-api-key"])
            for model in ("ok", "parallel", "write", "cancel", "budget"):
                run.direct("model-setup-" + model, [run.provider_executable, "model", "add", model,
                    "--provider", "fixture", "--model", model, "--compat", "full"])
            run.exercise(fixture)
        print(json.dumps({"status": "passed", "evidence": str(output)}))
    finally:
        require(run.safe_shutdown(), "fixture cleanup failed; inspect retained evidence")


if __name__ == "__main__":
    main()
