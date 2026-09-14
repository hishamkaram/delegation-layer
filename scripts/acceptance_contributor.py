"""A new compiled provider through the normal CLI and an isolated real pueue.

Provider-specific scenarios are fixtures; process ownership and supervisor
configuration reuse the existing acceptance harness. This does not certify a
native AI provider or add a provider to production discovery.
"""
import argparse
import json
import os
from pathlib import Path
import secrets
import shutil
import tempfile
import time
import traceback
import uuid

from acceptance_supervisor_common import (
    Processes, config_for, digest, inherited_environment, read_json,
    require, sha, write_json,
)


PROVIDER = "synthetic:contributor-proof"


class ContributorAcceptance:
    def __init__(self, tools, pueue, pueued, output):
        self.tools = Path(tools).resolve(strict=True)
        self.pueue = Path(pueue).resolve(strict=True)
        self.pueued = Path(pueued).resolve(strict=True)
        self.output = Path(output).resolve()
        self.output.mkdir(mode=0o700, parents=True)
        self.base = Path(tempfile.mkdtemp(prefix="dc-", dir="/tmp")).resolve()
        self.root = self.base / "task-state"
        self.work = self.base / "work space"
        self.runtime = self.base / "provider-runtime"
        self.queue = self.base / "queue"
        self.bindir = self.base / "binaries"
        for directory in (self.work, self.runtime, self.queue, self.bindir):
            directory.mkdir(mode=0o700)
        environment = inherited_environment()
        environment["DELEGATE_TEST_PUEUE"] = str(self.pueue)
        environment["DELEGATE_CONTRIBUTOR_RUNTIME"] = str(self.runtime)
        self.processes = Processes(self.output / "processes", environment)
        self.daemon = None
        self.tasks = {}
        self.rows = {}
        self.closed = False

    def setup(self):
        for name in ("state", "run"):
            (self.queue / name).mkdir(mode=0o700)
        (self.queue / "aliases.yml").write_text("{}\n")
        (self.queue / "aliases.yml").chmod(0o600)
        self.config = self.queue / "p.yml"
        write_json(self.config, config_for(self.queue))
        for name in ("delegate", "delegate-run", "provider"):
            shutil.copyfile(self.tools / name, self.bindir / name)
            (self.bindir / name).chmod(0o700)
            require(digest(self.tools / name) == digest(self.bindir / name), "binary copy changed")
        self.delegate = self.bindir / "delegate"
        self.runner = self.bindir / "delegate-run"
        self.provider = self.bindir / "provider"
        for name, binary in (("pueue", self.pueue), ("pueued", self.pueued)):
            process = self.processes.run(name + "-version", [binary, "--version"], self.base)
            require((process.directory / "stdout").read_text().strip() == name + " 4.0.4", "wrong supervisor version")
        discovery = self.processes.run("discovery", [self.delegate, "providers", "--json"], self.base).json()
        require([p["id"] for p in discovery["providers"]] == [PROVIDER], "test catalog differs from declared contributor")
        write_json(self.output / "binding.json", {
            "base": str(self.base), "provider": PROVIDER, "discovery": discovery,
            "binaries": {str(p): digest(p) for p in (self.delegate, self.runner, self.provider, self.pueue, self.pueued)},
            "driver_sha256": digest(__file__), "config_sha256": digest(self.config),
            "native_ai_turns": 0,
        })
        self.daemon = self.processes.start("private-daemon", [self.pueued, "-c", self.config], self.base)
        for _ in range(30):
            require(self.daemon.poll() is None, "private daemon exited before readiness")
            process = self.client("ready", ["status", "--json"], expected={0, 1})
            if process.result["exit_code"] == 0:
                require(process.json().get("tasks") == {}, "new private queue was not empty")
                return
            time.sleep(0.1)
        raise RuntimeError("private supervisor readiness not established")

    def client(self, name, operation, expected=0):
        return self.processes.run(name, [self.pueue, "-c", self.config, *operation], self.base, expected)

    def collect(self, name, task_id):
        return self.processes.run(name, [self.delegate, "--root", self.root, "collect", task_id,
                                        "--watch", "150s", "--json"], self.base, expected={0, 4}, timeout=160).json()

    def wait_runner_done(self, root_id, task_id):
        label = "delegate:" + root_id + ":" + task_id
        deadline = time.monotonic() + 150
        while time.monotonic() < deadline:
            rows = self.client("runner-status", ["status", "--json"]).json().get("tasks", {})
            matching = [row for row in rows.values() if row.get("label") == label]
            require(len(matching) == 1, "submitted task has no unique supervisor row")
            status = matching[0].get("status")
            if isinstance(status, dict) and "Done" in status:
                require(status["Done"].get("result") == "Success", "supervisor reports a failed runner")
                return
            time.sleep(0.1)
        raise RuntimeError("runner completion was not observed within acceptance deadline")

    def run_case(self, case, expected, answer="", nonce="", predecessor=None):
        task_id = uuid.uuid4().hex
        brief = self.base / (case + ".brief.json")
        value = {"case": case}
        if answer and case != "resume":
            value["answer"] = answer
        if nonce:
            value["nonce"] = nonce
        write_json(brief, value)
        args = [self.delegate, "--root", self.root, "--pueue-config", self.config,
                "--runner", self.runner, "dispatch", "--provider", PROVIDER,
                "--brief", brief, "--cwd", self.work, "--id", task_id,
                "--permission", "read-only", "--budget", "120s", "--json"]
        if predecessor:
            args.extend(["--resume-task", predecessor])
        dispatched = self.processes.run("dispatch-" + case, args, self.base).json()
        require(dispatched.get("task_id") == task_id, "dispatch returned a different task")
        self.tasks[case] = task_id
        # Outcome publication can precede session-release cleanup. Inspect the
        # completed runner; do not mistake a transient cleanup lock for a
        # provider failure or accept an error-bearing collection as success.
        self.wait_runner_done(dispatched["root_id"], task_id)
        collected = self.collect("collect-" + case, task_id)
        directory = self.root / "tasks" / task_id
        outcome = read_json(directory / "outcome.json")
        require(collected.get("outcome") == outcome, "CLI and sole outcome authority disagree")
        require(outcome["verdict"] == expected, case + " outcome mismatch")
        payload = outcome["payload"]
        data = (directory / payload["basename"]).read_bytes()
        require(len(data) == payload["length"] and sha(data) == payload["sha256"], "payload digest mismatch")
        if expected == "committed":
            require(data == answer.encode(), case + " changed final answer bytes")
        self.verify_files(case, directory)
        seal = read_json(directory / "provider.exit")
        require(seal["invocation_state"] == "started", "case did not enter real provider")
        require(outcome["evidence_sha256"] == seal["manifest_sha256"], "outcome is not bound to seal")
        self.rows[case] = {"task_id": task_id, "verdict": expected, "manifest_sha256": seal["manifest_sha256"],
                           "outcome_sha256": digest(directory / "outcome.json")}
        print("PASS contributor " + case, flush=True)
        return task_id

    def verify_files(self, case, directory):
        meta = read_json(directory / "meta.json")
        require(len(meta["input_files"]) == 2, "two task-owned config files were not exercised")
        for declaration in meta["input_files"]:
            path = directory / "provider-input" / declaration["name"]
            require(path.read_bytes() == declaration["content"].encode(), "input bytes differ from admitted metadata")
            require(path.stat().st_mode & 0o777 == 0o600, "input file is not private")
        require(len(meta["output_artifacts"]) == 1, "optional output declaration missing")
        name = meta["output_artifacts"][0]["name"]
        seal = read_json(directory / "provider.exit")
        entries = {entry["path"]: entry for entry in seal["raw_manifest"]}
        absent = case in ("absent", "invalid-utf8")
        if absent:
            require("raw/" + name not in entries, "absent output gained a manifest entry")
        else:
            require("raw/" + name in entries, "present output omitted from seal")
        for relative, entry in entries.items():
            path = directory / relative
            require(path.stat().st_size == entry["size"] and digest(path) == entry["sha256"], "raw manifest digest mismatch")
        if not absent:
            raw = directory / "raw" / name
            staged = directory / "provider-output" / name
            require(raw.stat().st_ino != staged.stat().st_ino, "native output inode became sealed raw evidence")
            if case == "empty":
                require(raw.stat().st_size == 0, "present empty output was not preserved")

    def snapshot(self, task_id):
        directory = self.root / "tasks" / task_id
        return {name: digest(directory / name) for name in
                ("task.json", "meta.json", "provider.start", "provider.exit", "outcome.json", "provider.ref.json")
                if (directory / name).exists()}

    def receipt_snapshot(self):
        return {str(path.relative_to(self.runtime)): digest(path)
                for kind in ("launches", "completions") for path in (self.runtime / kind).glob("*.json")}

    def verify_replay(self):
        before = self.receipt_snapshot()
        expected = {kind + "/" + task_id + ".json"
                    for kind in ("launches", "completions") for task_id in self.tasks.values()}
        require(set(before) == expected, "actual launch/completion receipts differ from dispatched tasks")
        for task_id in self.tasks.values():
            directory = self.root / "tasks" / task_id
            (directory / "provider-output").rename(directory / "provider-output.archived")
            (directory / "provider-input").rename(directory / "provider-input.archived")
        self.provider.rename(self.provider.with_name("provider.archived"))
        for case, task_id in self.tasks.items():
            snapshot = self.snapshot(task_id)
            self.collect("replay-" + case, task_id)
            require(snapshot == self.snapshot(task_id), "replay changed immutable task records")
        require(self.receipt_snapshot() == before, "collection relaunched the provider")

    def shutdown(self):
        for _ in range(30):
            final = self.client("final-status", ["status", "--json"]).json()
            rows = final.get("tasks")
            require(isinstance(rows, dict) and len(rows) == len(self.tasks), "unexpected private queue membership")
            if all(isinstance(row.get("status"), dict) and "Done" in row["status"] for row in rows.values()):
                break
            time.sleep(0.1)
        else:
            raise RuntimeError("unknown or active task prevents shutdown")
        pending = self.processes.drain(timeout=1, exclude=(self.daemon,))
        require(not pending, "owned client not finished")
        self.client("private-shutdown", ["shutdown"])
        self.daemon.wait(timeout=30, expected=0)
        self.closed = True

    def run(self):
        try:
            self.setup()
            nonce = secrets.token_hex(16)
            first = self.run_case("present", "committed", nonce, nonce)
            original = self.snapshot(first)
            self.run_case("resume", "committed", nonce, predecessor=first)
            require(self.snapshot(first) == original, "continuation changed its predecessor")
            original_ref = read_json(self.root / "tasks" / first / "provider.ref.json")
            resumed_ref = read_json(self.root / "tasks" / self.tasks["resume"] / "provider.ref.json")
            require(original_ref["conversation_id"] == resumed_ref["conversation_id"], "continuation changed session")
            self.run_case("absent", "committed", " exact Ω answer\nsecond line ")
            for case in ("empty", "conflict", "rejected", "malformed", "wrong-task", "wrong-session", "nonzero",
                         "oversized-envelope", "oversized-output", "invalid-utf8"):
                self.run_case(case, "rejected", "fixture answer")
            self.verify_replay()
            self.shutdown()
            write_json(self.output / "success.json", {"cases": self.rows, "base": str(self.base),
                       "actual_provider_launches": len(self.tasks), "replay_launches": 0,
                       "natural_daemon_shutdown": self.closed, "signals_sent": 0})
        except BaseException as error:
            active = self.processes.drain(timeout=5, exclude=(() if self.daemon is None else (self.daemon,)))
            cleanup_error = None
            if self.daemon is not None and self.daemon.poll() is None and not active:
                try:
                    self.shutdown()
                except Exception as shutdown_error:
                    cleanup_error = str(shutdown_error)
            write_json(self.output / "failure.json", {"error": str(error), "traceback": traceback.format_exc(),
                       "cleanup_error": cleanup_error, "natural_daemon_shutdown": self.closed,
                       "base": str(self.base), "tasks": self.tasks, "owned_clients": active,
                       "daemon_pid": None if self.daemon is None else self.daemon.pid,
                       "unknown_termination_preserved": True, "signals_sent": 0})
            raise


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--tools", required=True)
    parser.add_argument("--pueue", required=True)
    parser.add_argument("--pueued", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    ContributorAcceptance(args.tools, args.pueue, args.pueued, args.output).run()


if __name__ == "__main__":
    main()
