"""I01-I05: one foreground private pueued lifecycle and the shared compiled CLIs.

No native stop/remove/kill is permitted. Shutdown requires the sole finite job's
positive completion plus the provider's actual Wait and complete capture.
"""
import argparse
import json
import os
from pathlib import Path
import platform
import re
import shutil
import tempfile
import time
import traceback
import uuid

from acceptance_supervisor_common import (
    Processes, config_for, digest, inherited_environment, read_json,
    require, sha, wait_until, write_json,
)
from acceptance_provider_common import AcceptanceFailure, NativeTaskOps
from acceptance_supervisor_yaml import YAMLChecks


COMMAND_ID_RE = re.compile(r"^[0-9a-f]{32}$")
EVENT_FILE_RE = re.compile(r"^event-(\d{6})\.json$")


# These events are emitted by execution.Run for one ordinary finite provider
# task. The stdout/stderr capture pairs may interleave, so the validator below
# checks their per-stream order while requiring every event exactly once.
EXPECTED_EXECUTION_EVENTS = (
    "start-permit-consumed", "timer-armed", "start-entry",
    "preflight-authorized", "start-authorized", "started",
    "parent-fds-closed", "started-receipt", "wait-completed",
    "stdout-eof", "stderr-eof", "stdout-raw-closed", "stderr-raw-closed",
    "completion-observed", "timer-disarmed", "sealed", "published",
)


def validate_execution_event_sequence(names):
    """Validate the exact runner ownership events and their causal order.

    The two capture goroutines are independent. Their eof/close events may
    therefore interleave, while each stream's eof must precede its close and
    both closes must precede completion. Requiring the closed set and these
    source-backed edges rejects missing, duplicate, extra, and reordered
    events without pretending the concurrent pair has a total order.
    """
    require(isinstance(names, list), "runner event names are not a list")
    require(all(isinstance(name, str) for name in names), "runner event name is not a string")
    expected = list(EXPECTED_EXECUTION_EVENTS)
    require(len(names) == len(expected), "runner emitted an unexpected event count")
    require(set(names) == set(expected), "runner emitted an unexpected or incomplete ownership event sequence")
    for name in expected:
        require(names.count(name) == 1, "missing or duplicate provider ownership event: " + name)
    positions = {name: names.index(name) for name in expected}

    def before(left, right):
        require(positions[left] < positions[right],
                "runner event order changed: %s must precede %s" % (left, right))

    for left, right in (
        ("start-permit-consumed", "timer-armed"),
        ("timer-armed", "start-entry"),
        ("start-entry", "preflight-authorized"),
        ("preflight-authorized", "start-authorized"),
        ("start-authorized", "started"),
        ("started", "parent-fds-closed"),
        ("parent-fds-closed", "started-receipt"),
        ("started", "wait-completed"),
        ("stdout-eof", "stdout-raw-closed"),
        ("stderr-eof", "stderr-raw-closed"),
        ("wait-completed", "completion-observed"),
        ("stdout-raw-closed", "completion-observed"),
        ("stderr-raw-closed", "completion-observed"),
        ("completion-observed", "timer-disarmed"),
        ("timer-disarmed", "sealed"),
        ("sealed", "published"),
    ):
        before(left, right)

    require("deadline-observed" not in names, "native acceptance budget expired")


class NativeSuite:
    def __init__(self, tools, pueue, pueued, output):
        self.tools = Path(tools).resolve(strict=True)
        self.pueue, self.pueued = Path(pueue).resolve(strict=True), Path(pueued).resolve(strict=True)
        self.output = Path(output).resolve()
        self.output.mkdir(mode=0o700, parents=True)
        self.base = Path(tempfile.mkdtemp(prefix="dl-", dir="/tmp")).resolve(strict=True)
        self.base.chmod(0o700)
        self.processes = Processes(self.output / "processes")
        self.daemon = None
        self.native_job = None
        self.shutdown_gate = False
        self.native_terminal_proven = False
        self.dispatch_attempted = False
        self.admitted = False
        self.task_id = uuid.uuid4().hex
        self.case_ids = []

    def setup(self):
        for name in ("state", "run", "events", "provider-records"):
            (self.base / name).mkdir(mode=0o700)
        (self.base / "aliases.yml").write_text("{}\n")
        (self.base / "aliases.yml").chmod(0o600)
        self.config = config_for(self.base)
        self.config_path = self.base / "p.yml"
        write_json(self.config_path, self.config)
        self.config_hash = digest(self.config_path)
        self.work = self.base / "work space ' ; literal"
        self.work.mkdir(mode=0o700)
        self.root = self.base / "task state ' ; literal"
        self.bindir = self.base / "cli space ' ; literal"
        self.bindir.mkdir(mode=0o700)
        self.delegate = self.copy_binary("delegate", "delegate")
        self.runner = self.copy_binary("delegate-run", "delegate-run")
        self.provider = self.copy_binary("provider", "provider ' ; literal")
        self.brief = self.base / "brief.md"
        self.answer = "Exact brief: 'quoted' \"double\"\n$(touch DO_NOT_CREATE) ; <&> | literal\n"
        self.brief.write_text(self.answer)
        self.brief.chmod(0o600)
        self.provider_config = self.base / "provider.json"
        self.release = self.base / "provider-release"
        provider_cfg = {
            "scenario": "hold", "artifact_dir": str(self.base / "provider-records"),
            "lifetime_ms": 15000, "task_id": self.task_id, "session_id": "phase2-" + self.task_id,
            "argv": ["literal argument", "$(touch DO_NOT_CREATE); 'quoted' \"double\""],
            "answer": self.answer, "rendezvous_path": str(self.release),
        }
        write_json(self.provider_config, provider_cfg)
        self.profile = self.bindir / "phase2-fixture.json"
        write_json(self.profile, {
            "schema_version": 1, "provider_executable": str(self.provider),
            "provider_sha256": digest(self.provider), "provider_config": str(self.provider_config),
            "provider_config_sha256": digest(self.provider_config),
            "supervisor_executable": str(self.pueue), "events_directory": str(self.base / "events"),
            "environment": [key + "=" + value for key, value in sorted(inherited_environment().items())],
            "hooks": {"mode": "normal", "delay_ms": 0},
        })
        write_json(self.output / "binding.json", {
            "private_base": str(self.base), "task_id": self.task_id,
            "config_path": str(self.config_path), "config_sha256": self.config_hash,
            "binaries": {str(p): digest(p) for p in (self.pueue, self.pueued, self.delegate, self.runner, self.provider, self.tools / "phase2probe")},
            "platform": platform.platform(), "architecture": platform.machine(),
            "driver_sha256": digest(__file__), "fixtures_sha256": digest(self.profile),
            "supervisor": "4.0.4", "K": 0, "M": 0,
        })
        probe = self.processes.run("private-isolation", [self.tools / "phase2probe", "isolate", self.config_path, self.base], self.base)
        write_json(self.output / "resolved-isolation.json", probe.json())
        self.parity = YAMLChecks(self.output / "yaml", self.processes, self.tools / "phase2probe", self.base, self.config).run()
        for name, binary in (("pueue", self.pueue), ("pueued", self.pueued)):
            proc = self.processes.run(name + "-version", [binary, "-c", self.config_path, "--version"], self.base)
            require((proc.directory / "stdout").read_bytes() == (name + " 4.0.4\n").encode(), "unsupported native " + name)
            self.processes.run(name + "-help", [binary, "-c", self.config_path, "--help"], self.base)
        self.case_ids.append("I01")

    def copy_binary(self, source, target):
        original, copied = self.tools / source, self.bindir / target
        require(original.is_file(), "missing compiled acceptance executable: " + str(original))
        shutil.copyfile(original, copied)
        copied.chmod(0o700)
        require(digest(original) == digest(copied), "copied executable mismatch")
        return copied

    def client(self, name, operation, expected=0, config=None):
        require(operation in (["status", "--json"], ["shutdown"]), "native client operation is not observational")
        if operation == ["shutdown"]:
            require(self.shutdown_gate, "shutdown requires positive finite-job completion")
        require(digest(self.config_path) == self.config_hash, "private supervisor config changed")
        path = self.config_path if config is None else Path(config)
        return self.processes.run(name, [self.pueue, "-c", path, *operation], self.base, expected)

    def start_daemon(self):
        self.daemon = self.processes.start("foreground-daemon", [self.pueued, "-c", self.config_path], self.base)
        for attempt in range(12):
            require(self.daemon.poll() is None, "private daemon exited before readiness")
            proc = self.client("ready-%02d" % attempt, ["status", "--json"], expected={0, 1})
            if proc.result["exit_code"] == 0:
                require(proc.json().get("tasks") == {}, "fresh private queue is not empty")
                break
            time.sleep(min(0.1 * (attempt + 1), 1))
        else:
            raise RuntimeError("private supervisor did not become ready")
        # These exercise the native client's 4.0.4 YAML loader against the same
        # private endpoint. No alternate daemon or workload is created.
        for index, path in enumerate(self.parity):
            proc = self.client("yaml-native-%02d" % index, ["status", "--json"], config=path)
            require(proc.json().get("tasks") == {}, "native YAML variant selected another queue")
        write_json(self.output / "yaml-native-parity.json", {
            "configurations": [str(path) for path in self.parity],
            "verified": "native 4.0.4 client YAML loading and explicit private endpoint",
            "daemon_configuration": str(self.config_path), "all_empty": True,
        })

    def cli(self, name, command, expected=0, extra=(), timeout=15):
        return self.processes.run(name, [self.delegate, "--root", self.root, command, self.task_id, "--json", *extra], self.base, expected, timeout)

    def dispatch_and_observe(self):
        self.dispatch_attempted = True
        dispatched = self.processes.run("dispatch", [
            self.delegate, "--root", self.root, "--pueue-config", self.config_path,
            "--runner", self.runner, "dispatch", "--provider", "fixture:test",
            "--brief", self.brief, "--cwd", self.work, "--id", self.task_id,
            "--permission", "read-only", "--budget", "1m", "--json",
        ], self.base)
        reply = dispatched.json()
        require(reply["task_id"] == self.task_id, "dispatcher returned another task")
        self.admitted = True
        self.root_id = read_json(self.root / "root.json")["root_id"]
        self.task_dir = self.root / "tasks" / self.task_id
        wait_until(lambda: Path(str(self.release) + ".waiting").exists(), timeout=12, description="provider finite hold")
        waiting_entry = self.validate_provider_waiting()
        require(not (self.task_dir / "provider.exit").exists(), "provider sealed while held before natural exit")
        early = self.cli("collect-pending", "collect", expected=3).json()
        require(early.get("outcome") is None, "pending collection fabricated outcome")
        self.validate_logs(early, sealed=False)
        self.validate_logs(self.cli("logs-pending", "logs").json(), sealed=False)
        status = self.cli("status-running", "status").json()
        require(status.get("supervisor", {}).get("matched") is True, "running task did not reconcile exact binding")
        self.validate_provider_waiting(waiting_entry)
        require(not (self.task_dir / "provider.exit").exists(), "provider sealed before cooperative release")
        self.case_ids.extend(["I02", "I03"])
        Path(str(self.release) + ".release").write_text("cooperative release\n")
        completed = self.cli("collect-watch", "collect", extra=["--watch", "20s"], timeout=25).json()
        require(completed["outcome"]["verdict"] == "committed", "finite worker did not commit")
        self.validate_terminal(completed)
        repeated = self.cli("collect-repeat", "collect").json()
        require(repeated["outcome"] == completed["outcome"], "repeated collect changed immutable winner")
        outcome_path = self.task_dir / "outcome.json"
        saved_outcome_bytes = outcome_path.read_bytes()
        saved_outcome = read_json(outcome_path)
        terminal_status = self.cli("status-terminal", "status").json()
        require(terminal_status.get("outcome") == saved_outcome, "terminal status changed the saved outcome")
        require(outcome_path.read_bytes() == saved_outcome_bytes, "terminal status changed outcome bytes")
        self.validate_logs(self.cli("logs-terminal", "logs").json(), sealed=True)
        terminal_cancel = self.cli("cancel-terminal-noop", "cancel").json()
        require(terminal_cancel.get("outcome") == saved_outcome, "terminal cancel changed the saved outcome")
        require(outcome_path.read_bytes() == saved_outcome_bytes, "terminal cancel changed outcome bytes")
        self.case_ids.append("I04")

    def validate_logs(self, reply, sealed):
        descriptors = reply.get("raw", [])
        require(len(descriptors) == 2, "logs must identify both raw streams")
        expected = {str(self.task_dir / "raw" / name) for name in ("stdout", "stderr")}
        require({item["path"] for item in descriptors} == expected, "logs returned another task's raw paths")
        for item in descriptors:
            require(item["available"] and item["sealed"] is sealed, "log descriptor liveness mismatch")
            if sealed:
                path = Path(item["path"])
                require(item["size"] == path.stat().st_size and item["sha256"] == digest(path), "sealed log descriptor changed")
            else:
                require("size" not in item and "sha256" not in item, "live log descriptor claimed final bytes")

    def validate_terminal(self, reply=None):
        outcome, seal = read_json(self.task_dir / "outcome.json"), read_json(self.task_dir / "provider.exit")
        if reply is not None:
            require(reply["outcome"] == outcome, "CLI outcome differs from durable bytes")
        for record in (outcome, seal):
            require(record["task_id"] == self.task_id and record["root_id"] == self.root_id, "terminal identity mismatch")
        descriptor = outcome["payload"]
        payload = self.task_dir / descriptor["basename"]
        require(payload.read_bytes() == self.answer.encode(), "literal answer bytes changed")
        require(payload.stat().st_size == descriptor["length"] and digest(payload) == descriptor["sha256"], "payload descriptor mismatch")
        require(seal["invocation_state"] == "started" and seal["exit_code"] == 0, "provider did not exit normally")
        for entry in seal["raw_manifest"]:
            path = self.task_dir / entry["path"]
            require(path.stat().st_size == entry["size"] and digest(path) == entry["sha256"], "raw manifest mismatch")
        records = self.base / "provider-records"
        require((records / "brief.input").read_bytes() == self.brief.read_bytes(), "stdin brief changed")
        for stream in ("stdout", "stderr"):
            require(digest(self.task_dir / "raw" / stream) == digest(records / ("expected." + stream)), "raw stream differs from provider expectation")
        entry, end = self.provider_receipts()
        provider_config = read_json(self.provider_config)
        expected_argv = provider_config["argv"]
        require(entry["pid"] > 0 and entry["pid"] == end["pid"] and end["natural"] and end["exit_code"] == 0, "provider completion identity mismatch")
        require(entry["task_id"] == self.task_id and entry["cwd"] == str(self.work), "provider cwd/task changed")
        require(entry["session_id"] == provider_config["session_id"], "provider session changed")
        require(entry["argv"] == expected_argv and entry["declared_argv"] == expected_argv, "provider literal argv declaration changed")
        require(entry["process_argv"] == [str(self.provider), str(self.provider_config), *expected_argv], "literal provider argv changed")
        require(not end.get("child_pid"), "unexpected child PID in native lifecycle")
        require(not (self.work / "DO_NOT_CREATE").exists(), "literal shell-looking argument was executed")
        provider_ref = read_json(self.task_dir / "provider.ref.json")
        require(provider_ref["conversation_id"] == "phase2-" + self.task_id, "recorded provider session changed")

    def validate_provider_waiting(self, expected_entry=None):
        records = self.base / "provider-records"
        invocations = records / "invocations"
        require(invocations.is_dir(), "missing provider invocation directory while held")
        directories = sorted(invocations.iterdir())
        require(all(path.is_dir() for path in directories), "unexpected provider invocation artifact while held")
        require(len(directories) == 1, "provider waiting receipt is not uniquely identified")
        directory = directories[0]
        entry_path = directory / "provider.entry.json"
        completion_path = directory / "provider.complete.json"
        require(entry_path.is_file(), "provider waiting receipt entry is missing")
        entry = read_json(entry_path)
        if expected_entry is not None:
            require(entry == expected_entry, "provider waiting receipt changed before release")
        provider_config = read_json(self.provider_config)
        require(entry.get("invocation_id") == directory.name, "provider waiting invocation ID mismatch")
        require(isinstance(entry.get("pid"), int) and entry["pid"] > 0, "provider waiting PID is not positive")
        require(entry.get("task_id") == self.task_id == provider_config.get("task_id"), "provider waiting task changed")
        require(entry.get("session_id") == provider_config.get("session_id"), "provider waiting session changed")
        require(entry.get("scenario") == provider_config.get("scenario") == "hold", "provider waiting scenario changed")
        expected_process_argv = [str(self.provider), str(self.provider_config), *provider_config["argv"]]
        require(entry.get("process_argv") == expected_process_argv, "provider waiting process argv changed")
        waiting_path = Path(str(self.release) + ".waiting")
        require(waiting_path.read_bytes() == (f'{entry["pid"]}:waiting\n').encode(), "provider waiting rendezvous changed")
        require(not completion_path.exists() and not (records / "provider.complete.json").exists(), "provider completed before release")
        return entry

    def provider_receipts(self):
        records = self.base / "provider-records"
        invocations = records / "invocations"
        require(invocations.is_dir(), "missing provider invocation receipt directory")
        entries = []
        children = []
        invocation_paths = sorted(invocations.iterdir())
        require(all(path.is_dir() for path in invocation_paths), "unexpected provider invocation artifact")
        for directory in invocation_paths:
            entry_path = directory / "provider.entry.json"
            completion_path = directory / "provider.complete.json"
            require(entry_path.is_file() and completion_path.is_file(), "provider invocation lacks paired receipts: " + str(directory))
            entry, completion = read_json(entry_path), read_json(completion_path)
            require(entry.get("invocation_id") == directory.name and completion.get("invocation_id") == directory.name, "provider receipt invocation ID mismatch")
            require(entry.get("invocation_dir") == str(directory) and completion.get("invocation_dir") == str(directory), "provider receipt directory mismatch")
            require(entry.get("artifact_dir") == str(records) and completion.get("artifact_dir") == str(records), "provider receipt artifact root mismatch")
            if entry.get("scenario") == "child" or "--child" in entry.get("process_argv", []):
                children.append((entry, completion))
            else:
                entries.append((entry, completion))
        require(len(entries) == 1, "provider invocation count is not exactly one")
        require(not children, "unexpected child provider invocation in native lifecycle")
        entry, completion = entries[0]
        require(completion.get("scenario") == entry.get("scenario") == "hold", "provider scenario changed")
        convenience_entry = read_json(records / "provider.entry.json")
        convenience_completion = read_json(records / "provider.complete.json")
        require(convenience_entry.get("invocation_id") == entry["invocation_id"], "provider convenience entry is not the invocation receipt")
        require(convenience_completion.get("invocation_id") == completion["invocation_id"], "provider convenience completion is not the invocation receipt")
        require(convenience_entry == entry and convenience_completion == completion, "provider convenience receipt changed immutable invocation evidence")
        return entry, completion

    def finish(self):
        job = None
        supervisor_ref = read_json(self.task_dir / "supervisor.ref.json")
        for attempt in range(20):
            proc = self.client("finished-%02d" % attempt, ["status", "--json"])
            tasks = proc.json()["tasks"]
            require(len(tasks) == 1, "unexpected native job count")
            job = next(iter(tasks.values()))
            require(job.get("id") == supervisor_ref["numeric_task_id"], "native job ID is not the saved positive target")
            require(job["label"] == "delegate:" + self.root_id + ":" + self.task_id, "native job label mismatch")
            if job["status"].get("Done", {}).get("result") == "Success":
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("supervised job did not finish naturally")
        self.native_job = job
        self.validate_events()
        self.native_terminal_proven = True
        # Historical reads use only saved task evidence after all work is done.
        for path in (self.profile, self.provider_config, self.provider, self.work):
            path.rename(path.with_name(path.name + ".offline"))
        saved = self.cli("collect-without-runtime-files", "collect").json()
        require(saved["outcome"] == read_json(self.task_dir / "outcome.json"), "runtime files required for historical winner")
        require(self.daemon.poll() is None, "daemon exited before authorized shutdown")
        self.shutdown_gate = True
        self.client("private-shutdown", ["shutdown"])
        self.daemon.wait(15, expected=0)
        require(not self.processes.drain(timeout=1), "owned process left unresolved")
        self.case_ids.append("I05")
        # Copy only task evidence/config paths, never generated secret/key data.
        shutil.copytree(self.root, self.output / "task-state")
        shutil.copytree(self.base / "events", self.output / "events")
        shutil.copytree(self.base / "provider-records", self.output / "provider-records")
        write_json(self.output / "success.json", {
            "cases": self.case_ids, "task_id": self.task_id, "root_id": self.root_id,
            "A": 1, "S": 1, "E": 1, "K": 0, "M": 0,
            "native_shutdown": 1, "daemon_pid": self.daemon.pid,
            "daemon_natural_exit": self.daemon.result["exit_code"],
            "limitations": "native client YAML parity; no real signal enforcement, escaped-child containment, or hardware power-loss claim",
        })
        shutil.rmtree(self.base)

    def validate_events(self):
        # The acceptance-only constructor supplies these immutable records;
        # every supervisor command still uses the configured native pueue.
        events_root = self.base / "events"
        directories = sorted(path for path in events_root.iterdir() if path.is_dir())
        require(directories, "missing acceptance event records")
        wrappers, supervisors = [], []
        for directory in directories:
            entry_path = directory / "entry.json"
            require(entry_path.is_file(), "event directory lacks entry receipt: " + str(directory))
            entry = read_json(entry_path)
            if entry.get("kind") == "supervisor":
                supervisors.append((directory, entry))
            else:
                wrappers.append((directory, entry))

        execution_wrappers = []
        for directory, entry in wrappers:
            events, completion = self.validate_wrapper_receipts(directory, entry)
            if events:
                require(entry["argv"][0] == str(self.runner), "provider execution came from another wrapper")
                execution_wrappers.append((directory, entry, completion, events))
            else:
                require(entry["argv"][0] != str(self.runner), "queue runner emitted no execution events")
                self.validate_owned_wrapper_receipt(entry, completion)
        require(len(execution_wrappers) == 1, "runner execution recorder count is not exactly one")
        runner_directory, _, runner_completion, runner_events = execution_wrappers[0]
        require(self.native_job is not None, "queue runner has no saved native status evidence")
        require(self.native_job["status"].get("Done", {}).get("result") == "Success", "queue runner lacks positive Done.Success evidence")
        require(runner_completion["exit_code"] == 0 and runner_completion["failed"] is False, "runner wrapper did not complete successfully")

        command_ids, verbs = set(), []
        for directory, entry in supervisors:
            command_id, argv = self.validate_supervisor_receipts(directory, entry)
            require(command_id not in command_ids, "duplicate supervisor command ID: " + command_id)
            command_ids.add(command_id)
            verbs.append(argv[3])
        require(verbs, "missing positive supervisor command recorder")
        require(verbs.count("add") == 1 and "kill" not in verbs and "remove" not in verbs, "native mutation counts violated")

        names = [event["name"] for event in runner_events]
        validate_execution_event_sequence(names)
        positions = {name: names.index(name) for name in names}
        for name in ("wait-completed", "stdout-eof", "stderr-eof", "stdout-raw-closed", "stderr-raw-closed", "completion-observed", "timer-disarmed"):
            require(positions[name] < positions["sealed"], "provider sealed before " + name)
        provider_entry, _ = self.provider_receipts()
        provider_entries = [self.base / "provider-records" / "invocations" / provider_entry["invocation_id"] / "provider.entry.json"]
        child_entries = []
        write_json(self.output / "counts.json", {
            "supervisor_command_ids": sorted(command_ids), "supervisor_verbs": verbs,
            "execution_wrapper": str(runner_directory), "execution_events": names,
            "provider_entries": [str(path) for path in provider_entries],
            "provider_entry_count": len(provider_entries), "child_provider_entries": [str(path) for path in child_entries],
        })

    def validate_wrapper_receipts(self, directory, entry):
        invocation_id = entry.get("invocation_id")
        require(isinstance(invocation_id, str) and COMMAND_ID_RE.fullmatch(invocation_id) and directory.name == invocation_id, "wrapper invocation ID mismatch")
        require(isinstance(entry.get("pid"), int) and entry["pid"] > 0, "wrapper entry PID is not positive")
        require(isinstance(entry.get("argv"), list) and entry["argv"], "wrapper entry argv is missing")
        require(entry.get("exit_code") == -1 and entry.get("failed") is False and entry.get("entry"), "wrapper entry receipt is not open")
        completion_path = directory / "completion.json"
        require(completion_path.is_file(), "wrapper completion receipt is missing: " + str(directory))
        completion = read_json(completion_path)
        require(completion.get("invocation_id") == invocation_id and completion.get("pid") == entry["pid"], "wrapper completion identity mismatch")
        require(completion.get("argv") == entry["argv"] and completion.get("completed"), "wrapper completion argv/timestamp mismatch")
        require(isinstance(completion.get("exit_code"), int) and isinstance(completion.get("failed"), bool), "wrapper completion status malformed")
        event_paths = self.event_paths(directory)
        events = []
        for expected_sequence, path in enumerate(event_paths, 1):
            event = read_json(path)
            require(event.get("kind") == "execution" and event.get("invocation_id") == invocation_id, "execution event identity mismatch")
            require(event.get("sequence") == expected_sequence and event.get("name"), "execution event sequence is not contiguous")
            require(event.get("pid") == entry["pid"] and event.get("argv") == entry["argv"] and event.get("at"), "execution event process identity changed")
            events.append(event)
        return events, completion

    def validate_owned_wrapper_receipt(self, entry, completion):
        matches = [process for process in self.processes.entries
                   if process.pid == entry["pid"] and process.argv == entry["argv"]]
        require(len(matches) == 1, "delegate wrapper is not paired with one owned Process")
        process = matches[0]
        require(process.result is not None and process.result["natural_wait"], "delegate wrapper lacks an owned natural Wait result")
        require(process.result["pid"] == entry["pid"] and process.result["exit_code"] == completion["exit_code"], "delegate wrapper exit does not match owned Wait")

    def validate_supervisor_receipts(self, directory, entry):
        command = entry.get("command")
        require(entry.get("kind") == "supervisor" and isinstance(command, dict), "supervisor entry envelope is malformed")
        command_id = command.get("command_id")
        require(isinstance(command_id, str) and COMMAND_ID_RE.fullmatch(command_id) and directory.name == command_id, "supervisor command ID mismatch")
        require(entry.get("invocation_id") == command_id and entry.get("sequence") == 0, "supervisor entry envelope identity mismatch")
        require(entry.get("pid") == command.get("pid") == 0 and entry.get("argv") == command.get("argv") and entry.get("at"), "supervisor entry envelope fields changed")
        self.validate_command_event(command, "entry", command_id, pid=0, exit_code=-1)
        argv = command["argv"]
        require(len(argv) >= 4 and argv[:3] == [str(self.pueue), "-c", str(self.config_path)], "native client used an unbound endpoint")
        require(argv[3] in ("--version", "status", "add"), "unexpected native supervisor operation")
        prefix = [str(self.pueue), "-c", str(self.config_path)]
        if argv[3] == "--version":
            require(argv == [*prefix, "--version"], "native --version argv changed")
        elif argv[3] == "status":
            require(argv == [*prefix, "status", "--json"], "native status argv changed")
        else:
            require(argv == [
                *prefix, "add", "--escape", "--label",
                "delegate:" + self.root_id + ":" + self.task_id,
                "--print-task-id", "--", str(self.runner), "--root", str(self.root), self.task_id,
            ], "native add argv changed")

        event_paths = self.event_paths(directory)
        require(len(event_paths) == 1 and event_paths[0].name == "event-000001.json", "supervisor started event pairing is incomplete")
        started_envelope = read_json(event_paths[0])
        started = started_envelope.get("command")
        require(started_envelope.get("kind") == "supervisor" and started_envelope.get("invocation_id") == command_id and started_envelope.get("sequence") == 1, "supervisor started envelope is malformed")
        require(isinstance(started, dict), "supervisor started command is missing")
        self.validate_command_event(started, "started", command_id, pid=None, exit_code=-1)
        require(started["pid"] > 0 and started["argv"] == argv, "supervisor Start identity mismatch")
        require(started_envelope.get("pid") == started["pid"] and started_envelope.get("argv") == argv and started_envelope.get("at"), "supervisor started envelope identity mismatch")

        completion_path = directory / "completion.json"
        require(completion_path.is_file(), "supervisor completion receipt is missing: " + str(directory))
        completion = read_json(completion_path)
        completed = completion.get("command")
        require(completion.get("kind") == "supervisor" and completion.get("invocation_id") == command_id and completion.get("command_id") == command_id, "supervisor completion identity mismatch")
        require(isinstance(completed, dict), "supervisor completion command is missing")
        self.validate_command_event(completed, "completed", command_id, pid=None, exit_code=None)
        require(completed["pid"] == started["pid"] and completed["argv"] == argv and completed["exit_code"] == 0, "native client Wait identity or result mismatch")
        require(completion.get("pid") == completed["pid"] and completion.get("argv") == argv and completion.get("exit_code") == 0 and completion.get("failed") is False and completion.get("completed"), "unresolved native client ownership")
        return command_id, argv

    def validate_command_event(self, command, stage, command_id, pid, exit_code):
        require(command.get("command_id") == command_id and command.get("stage") == stage, "supervisor command stage pairing mismatch")
        require(isinstance(command.get("argv"), list) and command["argv"], "supervisor command argv is missing")
        if pid is not None:
            require(command.get("pid") == pid, "supervisor command entry PID is not zero")
        if exit_code is not None:
            require(command.get("exit_code") == exit_code, "supervisor command exit code mismatch")

    @staticmethod
    def event_paths(directory):
        paths = []
        for path in directory.glob("event-*.json"):
            match = EVENT_FILE_RE.fullmatch(path.name)
            require(match is not None, "malformed event filename: " + str(path))
            paths.append((int(match.group(1)), path))
        paths.sort(key=lambda item: item[0])
        require([number for number, _ in paths] == list(range(1, len(paths) + 1)), "event file sequence is not contiguous")
        return [path for _, path in paths]

    def failure_terminal_proof(self):
        """Require durable task/capture evidence before failure shutdown.

        NativeTaskOps proves that every owned caller has naturally Waited and
        that the exact queue row is terminal. This additional gate proves the
        dispatched provider task has its sealed raw streams and paired native
        completion receipts before the daemon shutdown request is allowed.
        Unknown dispatch or incomplete evidence keeps ownership retained.
        """
        if not self.dispatch_attempted:
            return True
        if not self.admitted or not hasattr(self, "root_id") or not hasattr(self, "task_dir"):
            return False
        try:
            native_job = getattr(self, "native_job", None)
            if not isinstance(native_job, dict):
                return False
            if not self.task_dir.is_dir() or not (self.task_dir / "provider.exit").is_file():
                return False
            supervisor_ref = read_json(self.task_dir / "supervisor.ref.json")
            require(isinstance(supervisor_ref, dict), "supervisor reference is not an object")
            numeric_task_id = supervisor_ref.get("numeric_task_id")
            require(isinstance(numeric_task_id, int) and not isinstance(numeric_task_id, bool) and numeric_task_id >= 0,
                    "supervisor reference has no numeric task identity")
            require(native_job.get("id") == numeric_task_id and
                    native_job.get("label") == "delegate:" + self.root_id + ":" + self.task_id,
                    "fresh supervisor row identity mismatch")
            status = native_job.get("status")
            require(isinstance(status, dict) and
                    isinstance(status.get("Done"), dict) and
                    status["Done"].get("result") == "Success",
                    "fresh supervisor row is not Done.Success")
            # A failure in finish() can occur before it records this proof.
            # Re-run the complete event/receipt oracle then; once it passed,
            # preserve that proof across the later offline replay checks.
            if not getattr(self, "native_terminal_proven", False):
                self.validate_events()
                self.native_terminal_proven = True
            self.validate_terminal()
        except (AcceptanceFailure, AttributeError, KeyError, OSError, RuntimeError, TypeError, ValueError, RecursionError):
            return False
        return True

    def retained_failure_cleanup(self):
        """Use shared observation-only cleanup with a native proof gate.

        NativeTaskOps obtains a fresh private queue snapshot immediately before
        its shutdown request.  Install the gate at that point, rather than
        checking self.native_job first: finish() may have failed before it had
        a chance to retain the terminal row.
        """
        config_path = getattr(self, "config_path", None)
        if self.daemon is None or config_path is None:
            return False
        owner = NativeTaskOps(self.processes, self.delegate, self.runner, self.pueue,
                              self.base, getattr(self, "root", self.base), self.output)
        owner.bind_supervisor(config_path, self.daemon)
        if self.admitted and hasattr(self, "root_id"):
            owner.root_id = self.root_id
            owner.tasks = {"native": self.task_id}
            owner.labels = {"native": "delegate:" + self.root_id + ":" + self.task_id}
            owner.dispatch_attempts = {self.task_id}
        elif self.dispatch_attempted:
            # A dispatch response that did not establish an exact admission
            # identity is unknown. Keep the owner from treating an empty
            # snapshot as evidence that no native task was admitted.
            owner.dispatch_attempts = {self.task_id}

        queue_finished = owner.failure_queue_finished

        def guarded_queue_finished(status):
            if not queue_finished(status):
                return False
            if not self.dispatch_attempted:
                return True
            if not self.admitted or not hasattr(self, "root_id"):
                return False
            rows = status.get("tasks") if isinstance(status, dict) else None
            label = "delegate:" + self.root_id + ":" + self.task_id
            matches = [row for row in (rows.values() if isinstance(rows, dict) else ())
                       if isinstance(row, dict) and row.get("label") == label]
            if len(matches) != 1:
                return False
            # The shared queue oracle has just positively checked this fresh
            # row as Done.Success and bound its command to runner/root/task.
            self.native_job = matches[0]
            return self.failure_terminal_proof()

        # This is an instance callback, so it intentionally accepts the one
        # status argument supplied by NativeTaskOps.safe_failure_shutdown.
        owner.failure_queue_finished = guarded_queue_finished
        return owner.retain_failure_ownership()

    def retain_unknown_failure_ownership(self):
        """Retain handles when setting up the normal proof path itself faults.

        The fallback deliberately has no shutdown authority.  The shared
        retainer still observes owned callers and the daemon until they end
        naturally, but an accessor, receipt, or setup fault cannot be turned
        into evidence that the private queue is safe to close.
        """
        config_path = getattr(self, "config_path", None)
        if self.daemon is None or config_path is None:
            return False
        owner = NativeTaskOps(self.processes, self.delegate, self.runner, self.pueue,
                              self.base, getattr(self, "root", self.base), self.output)
        owner.bind_supervisor(config_path, self.daemon)
        if self.dispatch_attempted:
            owner.dispatch_attempts = {self.task_id}

        def no_shutdown_authority(_status):
            return False

        owner.failure_queue_finished = no_shutdown_authority
        return owner.retain_failure_ownership()

    def run(self):
        try:
            self.setup()
            self.start_daemon()
            self.dispatch_and_observe()
            self.finish()
        except BaseException as error:
            cleanup = False
            try:
                cleanup = self.retained_failure_cleanup()
            except BaseException:
                # The common retainer handles operational faults during its
                # observation loop. If constructing the gated owner itself
                # fails, keep the same handles under a no-authority owner
                # instead of exiting while a caller or daemon is active.
                cleanup = self.retain_unknown_failure_ownership()
            active = self.processes.drain(timeout=0, exclude=(() if self.daemon is None else (self.daemon,)))
            if self.daemon is not None:
                self.daemon.poll()
            write_json(self.output / "failure.json", {
                "error": str(error), "traceback": traceback.format_exc(), "private_base": str(self.base),
                "daemon_pid": None if self.daemon is None else self.daemon.pid,
                "daemon_result": None if self.daemon is None else self.daemon.result,
                "unresolved_owned_clients": active, "shutdown_gate": self.shutdown_gate,
                "natural_failure_cleanup": cleanup,
                "unknown_termination_preserved": True, "signals_sent": 0,
            })
            raise


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--tools", required=True)
    parser.add_argument("--pueue", required=True)
    parser.add_argument("--pueued", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    NativeSuite(args.tools, args.pueue, args.pueued, args.output).run()
    print("PASS I01 I02 I03 I04 I05: actual private pueue/pueued 4.0.4; A=S=E=1, K=M=0; natural shutdown", flush=True)


if __name__ == "__main__":
    main()
