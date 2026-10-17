"""Own one stateful Antigravity native acceptance run."""

from __future__ import annotations

import argparse
import os
import shutil
import tempfile
import time
from pathlib import Path

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.paths.operations import ensure_private_directory
from scripts.delegate_acceptance.supervisor.binding import observed_pueue_version
from scripts.delegate_acceptance.paths.operations import path_is_within, reject_tmp
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.lifetime.closure import validate_managed_lifetime_closure
from scripts.delegate_acceptance.paths.operations import write_bytes
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.supervisor.config_policy import config_for
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.directories.finalization import finalize_acceptance_artifacts
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.provider_drivers.agy.constants import (FINALIZATION_SECONDS, L3_NATIVE_TIMEOUT, MODE, PROVIDER, TASK_BUDGET, TIMEOUT_MARKER, WATCH_SECONDS)
from scripts.delegate_acceptance.provider_drivers.agy.runtime import PUEUE_PARENT
from scripts.delegate_acceptance.provider_drivers.agy.evidence import (
    directory_snapshot,
    immutable_task_snapshot,
    read_inspection_records,
    read_task_record,
    validate_effective_policy,
    validate_inspection_queue_row,
    validate_meta_supervisor,
    validate_outcome,
    validate_policy_sources,
    validate_provider_exit,
    validate_start_records,
    validate_submit_record,
    validate_task_request,
)
from scripts.delegate_acceptance.provider_drivers.agy.prepared import Prepared
from scripts.delegate_acceptance.provider_drivers.agy.process_capture import OwnedProcess, ProcessBook
from scripts.delegate_acceptance.provider_drivers.agy.protocol import validate_l1_write_denial
from scripts.delegate_acceptance.provider_drivers.agy.queue import (
    parse_json_output,
    pueue_command,
    row_state,
    status_jobs,
    status_row,
    validate_queue_row,
)
from scripts.delegate_acceptance.provider_drivers.agy.runtime import (
    BlockedFailure,
    authentication_unavailable,
    copy_bytes,
    no_prompt_argv,
    reject_runtime_roots,
    require_discovery,
    resolve_executable,
    selected_pueue,
    selected_pueued,
    selected_tools,
    task_id,
)
from scripts.delegate_acceptance.provider_drivers.agy.receipts import success_receipt
from scripts.delegate_acceptance.provider_drivers.agy.shutdown import failure_shutdown as shutdown_after_failure

REPO_ROOT = Path(__file__).resolve().parents[4]

class NativeRun:
    def __init__(self, args: argparse.Namespace, prepared: Prepared, output: Path):
        self.args = args
        self.prepared = prepared
        self.output = output
        self.environment = os.environ.copy()
        for path, label in ((prepared.workspace, "workspace"), (prepared.scratch, "scratch"),
                            (prepared.state, "state"), (output, "evidence")):
            reject_tmp(path, label)
            reject_runtime_roots(path, label, self.environment)
        require(not path_is_within(output, prepared.workspace) and not path_is_within(output, prepared.scratch),
                "evidence must be outside workspace and scratch")
        require(not path_is_within(prepared.workspace, output) and not path_is_within(prepared.state, output) and
                not path_is_within(output, prepared.state),
                "evidence path overlaps task state or workspace")
        self.pueue_base: Path | None = None
        self.pueue_owned_directories = ()
        self.pueue_config: Path | None = None
        self.daemon: OwnedProcess | None = None
        self.daemon_done = False
        self.final_queue: dict[str, object] | None = None
        self.root_id: str | None = None
        self.task_numbers: dict[str, int] = {}
        self.labels: dict[str, str] = {}
        self.done_tasks: set[str] = set()
        self.last_queue: dict[str, object] | None = None
        self.daemon_ready_empty = False
        self.processed_turns = 0
        self.dispatch_attempts: set[str] = set()
        # Every admitted candidate creates one supervised runtime inspection
        # row.  Keep those rows in the final queue proof, including an
        # inspection created by a continuation that is later refused as busy.
        self.inspection_attempts: set[str] = set()
        self.l1_snapshot: dict[str, dict[str, object]] | None = None
        self.l1_conversation: str | None = None
        self.l1_nonce: str | None = None
        self.l1_nonce_bytes: bytes | None = None
        self.l1_workspace_before: dict[str, dict[str, object]] | None = None
        self.task_results: dict[str, dict[str, object]] = {}
        self.nonprovider_tasks: dict[str, dict[str, object]] = {}
        self.lifetime_closure: dict[str, object] | None = None
        self.delegate = resolve_executable(args.delegate, REPO_ROOT / "bin" / "delegate", "delegate")
        self.runner = resolve_executable(args.runner, REPO_ROOT / "bin" / "delegate-run", "delegate-run")
        self.agy = resolve_executable(args.agy, Path(shutil.which("agy") or "/opt/homebrew/bin/agy"), "agy")
        self.pueue = selected_pueue(resolve_executable(args.pueue, Path(shutil.which("pueue") or "/opt/homebrew/bin/pueue"), "pueue"))
        self.pueued = selected_pueued(resolve_executable(args.pueued, Path(shutil.which("pueued") or "/opt/homebrew/bin/pueued"), "pueued"))
        self.probe = selected_tools(REPO_ROOT)
        require_discovery("agy", self.agy)
        require_discovery("pueue", self.pueue)
        self.provider_version: str | None = None
        self.pueue_version: str | None = None
        self.pueued_version: str | None = None
        self.provider_sha256 = digest(self.agy)
        self.profile_revision: str | None = None
        self.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.process_owner = DirectoryOwner()
        try:
            self.processes = ProcessBook(
                output / "processes", self.environment, owner=self.process_owner)
        except BaseException:
            self.process_owner.rollback()
            raise
        self.process_owned_directories = self.process_owner.transfer()

    def direct(self, name: str, argv: list[str], expected: int | set[int] | tuple[int, ...] | None = 0,
               timeout: float = 30) -> OwnedProcess:
        return self.processes.run(name, argv, REPO_ROOT, expected=expected, timeout=timeout)

    def setup(self) -> None:
        require(self.prepared.state.resolve() == self.prepared.state, "state path must be canonical")
        write_json(self.output / "prepared-input.json", self.prepared.data)
        if PUEUE_PARENT.exists() or PUEUE_PARENT.is_symlink():
            ensure_private_directory(PUEUE_PARENT, "private supervisor parent")
        else:
            ensure_private_directory(PUEUE_PARENT, "private supervisor parent", create=True)
        ownership = DirectoryOwner()
        try:
            base = Path(tempfile.mkdtemp(dir=str(PUEUE_PARENT), prefix="dl-agy-"))
            self.pueue_base = ownership.adopt_created("private pueue", base, PUEUE_PARENT).directory
            os.chmod(self.pueue_base, 0o700)
            ownership.create("private pueue state", self.pueue_base / "state",
                             self.pueue_base)
            ownership.create("private pueue runtime", self.pueue_base / "run",
                             self.pueue_base)
            self.pueue_config = self.pueue_base / "pueue.yml"
            write_json(self.pueue_config, config_for(self.pueue_base))
        except BaseException:
            try:
                ownership.rollback()
            finally:
                self.pueue_base = None
            raise
        self.pueue_owned_directories = ownership.transfer()
        self.processes.run("agy-version", [str(self.agy), "--version"], REPO_ROOT, timeout=15)
        version = self.processes.entries[-1].output().decode().strip()
        require(version, "agy returned an empty version")
        self.provider_version = version
        self.processes.run("pueue-version", pueue_command(self.pueue, self.pueue_config, "--version"), REPO_ROOT,
            timeout=15)
        self.processes.run("pueued-version", [str(self.pueued), "-c", str(self.pueue_config), "--version"], REPO_ROOT,
            timeout=15)
        pueue_version = self.processes.entries[-2].output()
        pueued_version = self.processes.entries[-1].output()
        self.pueue_version = observed_pueue_version(pueue_version, "pueue")
        self.pueued_version = observed_pueue_version(pueued_version, "pueued")
        self.processes.run("isolation-probe", [str(self.probe), "isolate", str(self.pueue_config), str(self.pueue_base)], REPO_ROOT,
            timeout=30)
        write_json(self.output / "binding.json", {
            "agy": str(self.agy), "agy_version": self.provider_version, "agy_sha256": self.provider_sha256,
            "pueue": str(self.pueue), "pueue_sha256": digest(self.pueue),
            "pueued": str(self.pueued), "pueued_sha256": digest(self.pueued),
            "pueue_version": self.pueue_version, "pueued_version": self.pueued_version,
            "pueue_base": str(self.pueue_base), "pueue_config": str(self.pueue_config),
            "environment_values_in_record": False,
        })
        self.daemon = self.processes.start("pueued", [str(self.pueued), "-c", str(self.pueue_config)], REPO_ROOT)
        self.wait_daemon_ready()

    def status(self, name: str) -> dict[str, object]:
        require(self.pueue_config is not None, "pueue is not configured")
        process = self.direct(name, pueue_command(self.pueue, self.pueue_config, "status", "--json"), timeout=20)
        value = status_jobs(process)
        self.last_queue = value
        write_json(self.output / f"{name}.json", value)
        return value

    def wait_daemon_ready(self) -> None:
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            require(self.pueue_config is not None, "pueue is not configured")
            process = self.direct(
                f"daemon-status-{len(self.processes.entries):03d}",
                pueue_command(self.pueue, self.pueue_config, "status", "--json"),
                expected={0, 1}, timeout=20)
            if int(process.result["exit_code"]) == 0:
                value = status_jobs(process)
                tasks = value.get("tasks")
                if isinstance(tasks, dict) and not tasks:
                    self.daemon_ready_empty = True
                    return
            time.sleep(0.2)
        raise AcceptanceFailure("pueued readiness observation expired")

    def dispatch(self, name: str, task: str, brief: Path, resume: str | None, native_timeout: str | None) -> dict[str, object]:
        require(self.pueue_config is not None, "pueue is not configured")
        argv = [str(self.delegate), "--root", str(self.prepared.state), "--pueue-config", str(self.pueue_config),
                "--runner", str(self.runner), "dispatch", "--json", "--id", task, "--provider", PROVIDER,
                "--brief", str(brief), "--cwd", str(self.prepared.workspace), "--permission", MODE,
                "--budget", TASK_BUDGET]
        if native_timeout is not None:
            argv.extend(["--native-timeout", native_timeout])
        if resume is not None:
            argv.extend(["--resume-task", resume])
        no_prompt_argv(argv, [brief])
        self.dispatch_attempts.add(task)
        self.inspection_attempts.add(task)
        process = self.direct(name, argv, timeout=45)
        response = parse_json_output(process, name)
        require(response.get("admission") == "admitted", f"{name} was not admitted: {response}")
        require(response.get("task_id") == task, f"{name} task identity mismatch")
        root = response.get("root_id")
        require(isinstance(root, str) and root, f"{name} omitted root identity")
        if self.root_id is None:
            self.root_id = root
        require(root == self.root_id, f"{name} changed the immutable root identity")
        supervisor = response.get("supervisor")
        require(isinstance(supervisor, dict) and supervisor.get("matched") is True,
                f"{name} lacks positive supervisor admission")
        numeric = supervisor.get("numeric_task_id")
        require(isinstance(numeric, int) and numeric >= 0, f"{name} lacks numeric pueue identity")
        self.task_numbers[task] = numeric
        self.labels[task] = f"delegate:{root}:{task}"
        write_json(self.output / f"{name}-dispatch.json", response)
        self.processed_turns += 1
        return response

    def wait_task(self, name: str, task: str) -> dict[str, object]:
        deadline = time.monotonic() + WATCH_SECONDS
        while time.monotonic() < deadline:
            value = self.status(f"{name}-status-{int(time.monotonic() * 1000)}")
            row = status_row(value, self.task_numbers[task])
            require(row is not None, f"{name} task disappeared from private pueue queue")
            validate_queue_row(row, self.labels[task], self.runner, self.prepared.state, task)
            state = row_state(row)
            if state == "Done":
                self.done_tasks.add(task)
                return row
            require(state in {"Queued", "Running", "Paused", "Stashed", "Locked"},
                    f"{name} reached unknown pueue state {state}")
            time.sleep(0.5)
        marker = self.output / f"{name}-observation-expired.json"
        write_json(marker, {"task_id": task, "watch_seconds": WATCH_SECONDS, "termination": "unknown", "signals_sent": 0})
        raise AcceptanceFailure(f"{name} pueue observation expired; task handle retained")

    def collect(self, name: str, task: str, expected_code: set[int]) -> tuple[OwnedProcess, dict[str, object]]:
        argv = [str(self.delegate), "--root", str(self.prepared.state), "--pueue-config", str(self.pueue_config),
                "--runner", str(self.runner), "collect", task, "--watch", "0s", "--json"]
        no_prompt_argv(argv, [])
        process = self.direct(name, argv, expected=expected_code, timeout=45)
        response = parse_json_output(process, name)
        write_json(self.output / f"{name}.json", response)
        require(response.get("task_id") == task, f"{name} collected wrong task")
        return process, response

    def immutable_snapshot(self, task: str) -> dict[str, dict[str, object]]:
        return immutable_task_snapshot(self.prepared.state, task)

    def read_record(self, task: str, name: str) -> dict[str, object]:
        return read_task_record(self.prepared.state, task, name)

    def validate_evidence(self, name: str, task: str, expected_verdict: str) -> dict[str, object]:
        directory = self.prepared.state / "tasks" / task
        for record in ("brief.md", "task.json", "meta.json", "submit.json", "provider.start",
                       "provider.started.json", "provider.exit"):
            require((directory / record).is_file(), f"{name} missing one-use evidence {record}")
        request = self.read_record(task, "task.json")
        validate_task_request(name, task, request, self.root_id)
        submit = self.read_record(task, "submit.json")
        supervisor = validate_submit_record(
            name=name, task=task, submit=submit, root_id=self.root_id, label=self.labels[task],
            pueue_config=self.pueue_config, pueue_version=self.pueue_version, pueue=self.pueue)
        start = self.read_record(task, "provider.start")
        started = self.read_record(task, "provider.started.json")
        validate_start_records(name, task, start, started, self.root_id)
        meta = self.read_record(task, "meta.json")
        predicate = validate_meta_supervisor(name, meta, supervisor)
        policy, pending_runtime, profile_revision = validate_effective_policy(
            name, meta, self.prepared.workspace, self.provider_sha256)
        if self.profile_revision is None:
            self.profile_revision = profile_revision
        require(profile_revision == self.profile_revision,
                f"{name} effective policy revision drifted")
        validate_policy_sources(name, policy, self.environment, profile_revision)
        provider_exit = self.read_record(task, "provider.exit")
        validate_provider_exit(
            name=name, task=task, meta=meta, started=started, provider_exit=provider_exit, pending_runtime=pending_runtime,
            agy=self.agy,
            root_id=self.root_id, predicate=predicate, expected_verdict=expected_verdict)
        outcome = self.read_record(task, "outcome.json")
        payload_bytes = validate_outcome(
            name, task, expected_verdict, directory, outcome, provider_exit)
        return {
            "meta": meta, "provider_exit": provider_exit, "outcome": outcome, "payload": payload_bytes,
            "events": {"A": int((directory / "submit.json").is_file()),
                       "S": int((directory / "provider.start").is_file()),
                       "E": int((directory / "provider.started.json").is_file()),
                       "seal": int((directory / "provider.exit").is_file())},
        }

    def replay(self, name: str, task: str, before: dict[str, dict[str, object]]) -> dict[str, object]:
        _, response = self.collect(name, task, {0, 4})
        after = self.immutable_snapshot(task)
        require(before == after, f"{name} replay changed immutable task evidence")
        return response

    def set_queue_running(self, running: bool, label: str) -> None:
        args = ("start", "--group", "default") if running else ("pause", "--wait")
        self.direct(label, pueue_command(self.pueue, self.pueue_config, *args))

    def assert_unstarted_queued(self, task: str, label: str) -> None:
        status = self.status(label)
        row = status_row(status, self.task_numbers[task])
        require(row is not None and row_state(row) == "Queued", "paused task was not Queued")
        directory = self.prepared.state / "tasks" / task
        require(not any((directory / name).exists() for name in
                        ("provider.start", "provider.started.json", "provider.exit")),
                "provider crossed Start while its queue was paused")

    def run_policy_drift(self) -> None:
        """Historical profile oracle; not part of native-permission acceptance."""
        task = task_id()
        policy = self.prepared.workspace / "AGENTS.md"
        require(not os.path.lexists(policy), "policy drift fixture requires absent workspace AGENTS.md")
        write_json(self.output / "policy-drift-plan.json", {"task_id": task, "policy": str(policy)})
        initial = b"Acceptance context revision one.\n"
        write_bytes(policy, initial)
        brief = self.output / "briefs" / "policy-drift.txt"
        write_bytes(brief, b"This acceptance task must be refused before the provider starts.\n")
        self.set_queue_running(False, "drift-pause")
        self.dispatch("policy-drift", task, brief, None, None)
        self.assert_unstarted_queued(task, "drift-queued")
        admitted = self.read_record(task, "meta.json")["effective_config"]["policy"]["sources"]
        require(any(source["path"] == str(policy) and source["sha256"] == sha(initial)
                    for source in admitted), "drift fixture was not bound at admission")
        with policy.open("wb") as stream:
            stream.write(b"Acceptance context revision two.\n")
            stream.flush()
            os.fsync(stream.fileno())
        self.set_queue_running(True, "drift-resume")
        row = self.wait_task("policy-drift", task)
        directory = self.prepared.state / "tasks" / task
        require(not any((directory / name).exists() for name in
                        ("provider.start", "provider.started.json", "provider.exit")),
                "drifted policy crossed provider Start")
        logs = self.direct("drift-runner-log", pueue_command(
            self.pueue, self.pueue_config, "log", "--json", "--full", str(self.task_numbers[task])))
        require(b"record identity does not match expected task or root" in logs.output(),
                "runner did not report the expected policy identity mismatch")
        require(row["status"]["Done"].get("result") != "Success", "drifted runner unexpectedly succeeded")
        self.nonprovider_tasks[task] = {"case": "policy-drift", "provider_start_records": 0, "row": row}
        write_json(self.output / "policy-drift-verified.json", self.nonprovider_tasks[task])
        policy.unlink()

    def assert_busy_continuation(self, brief: Path, queued_task: str) -> None:
        self.assert_unstarted_queued(queued_task, "L2-queued")
        refused_task = task_id()
        write_json(self.output / "busy-plan.json", {"task_id": refused_task, "queued_owner": queued_task})
        argv = [str(self.delegate), "--root", str(self.prepared.state), "--pueue-config", str(self.pueue_config),
                "--runner", str(self.runner), "dispatch", "--json", "--id", refused_task, "--provider", PROVIDER,
                "--brief", str(brief), "--cwd", str(self.prepared.workspace), "--permission", MODE,
                "--budget", TASK_BUDGET, "--resume-task", self.prepared.ids["L1"]]
        self.dispatch_attempts.add(refused_task)
        self.inspection_attempts.add(refused_task)
        process = self.direct("busy-refusal", argv, expected=1, timeout=45)
        response = parse_json_output(process, "busy-refusal")
        require("session continuation busy" in response.get("error", ""), "overlapping continuation was not refused as busy")
        directory = self.prepared.state / "tasks" / refused_task
        require(not any((directory / name).exists() for name in
                        ("submit.json", "provider.start", "provider.started.json", "provider.exit")),
                "busy continuation crossed submission or Start")
        self.assert_unstarted_queued(queued_task, "L2-still-queued")
        queue = self.last_queue["tasks"]
        require(not any(row.get("label", "").endswith(":" + refused_task) for row in queue.values()),
                "refused continuation acquired a queue row")
        # Positive CLI refusal plus absent create-once submission guard proves
        # this attempted continuation never entered the supervisor.
        self.dispatch_attempts.remove(refused_task)
        write_json(self.output / "busy-verified.json", {"task_id": refused_task, "response": response,
                                                       "submission_records": 0, "provider_start_records": 0})
        self.set_queue_running(True, "L2-resume")

    def run_l1(self) -> None:
        brief = self.copy_brief("L1")
        task = self.prepared.ids["L1"]
        nonce_bytes = self.prepared.nonce_file.read_bytes()
        require(nonce_bytes and b"\x00" not in nonce_bytes, "L1 nonce fixture is invalid")
        nonce = nonce_bytes[:-1] if nonce_bytes.endswith(b"\n") else nonce_bytes
        require(nonce, "L1 nonce fixture is empty")
        self.l1_nonce_bytes = nonce
        self.l1_nonce = nonce.decode(errors="strict")
        self.l1_workspace_before = directory_snapshot(self.prepared.workspace)
        self.dispatch("L1", task, brief, None, None)
        self.wait_task("L1", task)
        _, response = self.collect("L1-collect", task, {0, 4})
        if authentication_unavailable(self.prepared.state / "tasks" / task):
            raise BlockedFailure("Antigravity authentication is unavailable")
        verdict = self.read_record(task, "outcome.json").get("verdict")
        require(verdict in {"committed", "rejected"}, "L1 has no terminal outcome")
        evidence = self.validate_evidence("L1", task, verdict)
        payload = evidence["payload"]
        require(isinstance(payload, bytes), "L1 payload is not bytes")
        if verdict == "committed":
            require(self.l1_nonce in payload.decode(errors="replace"), "L1 response did not report the nonce")
        require(self.prepared.inside.is_file() and not self.prepared.inside.is_symlink(), "L1 inside sentinel is absent")
        inside_bytes = self.prepared.inside.read_bytes()
        allowed_inside = {nonce}
        if nonce_bytes.endswith(b"\n"):
            allowed_inside.add(nonce_bytes)
        require(inside_bytes in allowed_inside, "L1 inside sentinel content mismatch")
        require(self.prepared.outside.is_file() and not self.prepared.outside.is_symlink(),
                "L1 outside sentinel was not written by native workspace-write")
        outside_bytes = self.prepared.outside.read_bytes()
        require(outside_bytes in {b"OUTSIDE-WRITE-PROBE", b"OUTSIDE-WRITE-PROBE\n"},
                "L1 outside sentinel content mismatch")
        write_json(self.output / "L1-filesystem.json", {
            "expected_nonce_sha256": sha(nonce),
            "inside_sha256": sha(inside_bytes),
            "inside_matches": True, "outside_entry_present": True,
            "outside_sha256": sha(outside_bytes),
        })
        if verdict == "committed":
            text = payload.decode(errors="replace").lower()
            attempted = any(item in text for item in ("outside-sentinel", "outside file", "scratch file", "outside"))
            completed = any(item in text for item in ("successfully created", "successfully", "created with content", "created"))
            require(attempted and completed, "L1 native outside-write result was not reported (inconclusive)")
        else:
            directory = self.prepared.state / "tasks" / task
            seal = evidence["provider_exit"]
            require(seal.get("exit_code") == 0 and seal.get("error") == "", "L1 failed for a reason other than a controlled denial")
            require(evidence["outcome"]["payload"]["basename"] == "publish.reject" and
                    not (directory / "publish.commit").exists(), "L1 denial has a successful publication")
            reference = self.read_record(task, "provider.ref.json")
            validate_l1_write_denial((directory / "raw/stdout").read_bytes(),
                                    (directory / "raw/stderr").read_bytes(), reference.get("conversation_id"))
        write_json(self.output / "L1-control-outcome.json", {"verdict": verdict,
                   "inside_matches": True, "outside_entry_present": True,
                   "continuation_requires_committed_nonce_recall": True})
        before = self.immutable_snapshot(task)
        self.replay("L1-replay", task, before)
        after_replay = self.immutable_snapshot(task)
        require(before == after_replay, "L1 replay changed immutable evidence")
        self.l1_snapshot = before
        provider_ref = self.read_record(task, "provider.ref.json")
        conversation = provider_ref.get("conversation_id")
        require(provider_ref.get("provider") == PROVIDER and isinstance(conversation, str) and conversation,
                "L1 provider identity is absent")
        self.l1_conversation = conversation
        self.task_results["L1"] = {"task_id": task, "conversation_id": conversation, "response": response,
                                    "snapshot": before, "events": evidence["events"], "provider_exit": evidence["provider_exit"]}
        self.prepared.nonce_file.unlink()
        self.prepared.inside.unlink()
        require(not self.prepared.nonce_file.exists(), "nonce fixture was not removed before L2")
        require(not self.prepared.inside.exists(), "inside sentinel was not removed before L2")
        expected_workspace = dict(self.l1_workspace_before)
        expected_workspace.pop(str(self.prepared.nonce_file.relative_to(self.prepared.workspace)))
        require(directory_snapshot(self.prepared.workspace) == expected_workspace,
                "L1 left an unexpected workspace file before L2")

    def copy_brief(self, name: str) -> Path:
        destination = self.output / "briefs" / f"{name}.brief.txt"
        copy_bytes(self.prepared.briefs[name], destination, f"prepared {name} brief")
        return destination

    def run_l2(self) -> None:
        require(self.l1_conversation is not None and self.l1_snapshot is not None, "L2 requires L1 identity and snapshot")
        require(self.l1_nonce is not None and self.l1_nonce_bytes is not None, "L2 requires the pre-dispatch nonce")
        require(not self.prepared.nonce_file.exists(), "L2 nonce fixture was not removed")
        task = self.prepared.ids["L2"]
        brief = self.copy_brief("L2")
        self.set_queue_running(False, "L2-pause")
        response = self.dispatch("L2", task, brief, self.prepared.ids["L1"], None)
        self.assert_busy_continuation(brief, task)
        self.wait_task("L2", task)
        _, collect_response = self.collect("L2-collect", task, {0})
        evidence = self.validate_evidence("L2", task, "committed")
        payload = evidence["payload"]
        require(isinstance(payload, bytes) and self.l1_nonce in payload.decode(errors="replace"),
                "L2 response did not recall the L1 nonce")
        provider_ref = self.read_record(task, "provider.ref.json")
        require(provider_ref.get("provider") == PROVIDER and provider_ref.get("conversation_id") == self.l1_conversation,
                "L2 used a different provider conversation")
        task_record = self.read_record(task, "task.json")
        prior = task_record.get("prior_session")
        require(isinstance(prior, dict) and prior.get("provider") == PROVIDER and
                prior.get("conversation_id") == self.l1_conversation and
                prior.get("predecessor_task_id") == self.prepared.ids["L1"], "L2 prior session is not exact")
        require(self.immutable_snapshot(self.prepared.ids["L1"]) == self.l1_snapshot,
                "L2 changed the immutable L1 task evidence")
        before = self.immutable_snapshot(task)
        self.replay("L2-replay", task, before)
        require(self.immutable_snapshot(self.prepared.ids["L1"]) == self.l1_snapshot,
                "L2 replay changed the immutable L1 evidence")
        self.task_results["L2"] = {"task_id": task, "conversation_id": self.l1_conversation,
                                   "dispatch": response, "collect": collect_response, "snapshot": before,
                                   "events": evidence["events"], "provider_exit": evidence["provider_exit"]}

    def run_l3(self) -> None:
        task = self.prepared.ids["L3"]
        brief = self.copy_brief("L3")
        self.dispatch("L3", task, brief, None, L3_NATIVE_TIMEOUT)
        self.wait_task("L3", task)
        _, response = self.collect("L3-collect", task, {0, 4})
        evidence = self.validate_evidence("L3", task, "rejected")
        require(response.get("publication") == "rejected", "L3 unexpectedly published a successful result")
        require(len(evidence["payload"]) > 0, "L3 rejection evidence is empty")
        raw_stdout = self.prepared.state / "tasks" / task / "raw" / "stdout"
        raw_stderr = self.prepared.state / "tasks" / task / "raw" / "stderr"
        require(raw_stdout.stat().st_size > 0 or raw_stderr.stat().st_size > 0, "L3 raw evidence was not retained")
        require(TIMEOUT_MARKER in raw_stderr.read_bytes(), "L3 did not record the exact native agy timeout marker")
        before = self.immutable_snapshot(task)
        self.replay("L3-replay", task, before)
        self.task_results["L3"] = {"task_id": task, "response": response, "snapshot": before,
                                   "events": evidence["events"], "provider_exit": evidence["provider_exit"]}

    def authorize_lifetime_closure(self) -> dict[str, object]:
        """Use the shared root-level lifetime authority for this acceptance state."""
        self.lifetime_closure = validate_managed_lifetime_closure(
            self.prepared.state, self.dispatch_attempts)
        return self.lifetime_closure

    def shutdown(self) -> None:
        require(self.pueue_config is not None, "pueue is not configured")
        final = self.status("final-status")
        tasks = final.get("tasks")
        require(isinstance(tasks, dict), "private pueue queue has no tasks object")
        ordinary = {key: row for key, row in tasks.items()
                    if isinstance(row, dict) and row.get("group") == "default"}
        inspection = {key: row for key, row in tasks.items()
                      if isinstance(row, dict) and row.get("group") != "default"}
        require(len(ordinary) == len(self.task_numbers),
                "private pueue queue has an unexpected ordinary job count")
        expected_inspections = read_inspection_records(
            self.prepared.state, getattr(self, "inspection_attempts", set()))
        require(len(inspection) == len(expected_inspections),
                "private pueue queue has an unexpected inspection job count")
        for task, number in self.task_numbers.items():
            row = status_row(final, number)
            require(row is not None, f"final private pueue status lost {task}")
            validate_queue_row(row, self.labels[task], self.runner, self.prepared.state, task)
            require(row_state(row) == "Done", f"private pueue job {task} was not positively observed Done")
        for task, record in expected_inspections.items():
            number = record["receipt"]["numeric_task_id"]
            row = status_row(final, number)
            require(row is not None, f"final private pueue status lost inspection {task}")
            validate_inspection_queue_row(row, record, self.runner, self.prepared.state)
        self.authorize_lifetime_closure()
        self.final_queue = final
        pending = self.processes.pending(exclude=(self.daemon,) if self.daemon is not None else ())
        require(not pending, f"owned command remains pending at supervisor shutdown: {pending}")
        require(self.daemon is not None, "pueued was not started")
        process = self.direct("pueue-shutdown", pueue_command(self.pueue, self.pueue_config, "shutdown"), timeout=30)
        require(process.result is not None and process.result["exit_code"] == 0, "pueue shutdown was not acknowledged")
        self.daemon.wait(30, 0)
        self.daemon_done = True

    def failure_shutdown(self) -> bool:
        return shutdown_after_failure(self, REPO_ROOT)



    def finalize(self, success: bool) -> bool:
        """Remove only the acceptance directories created by this invocation."""
        if success:
            self.authorize_lifetime_closure()
        directories = []
        directories.extend(self.prepared.owned_directories)
        process_directories = getattr(self, "process_owned_directories",
                                      (self.processes.capture_root,))
        directories.extend(process_directories)
        directories.extend(getattr(self, "pueue_owned_directories", ()))
        return finalize_acceptance_artifacts(
            self.output, self.daemon_done, tuple(directories), success,
            raw_process_captures_token=self.processes.capture_root,
            timeout=FINALIZATION_SECONDS)

    def failure_queue_finished(self, value: dict[str, object]) -> bool:
        tasks = value["tasks"]
        if not isinstance(tasks, dict):
            return False
        if not self.dispatch_attempts and not getattr(self, "inspection_attempts", set()):
            if tasks:
                return False
            state = getattr(getattr(self, "prepared", None), "state", None)
            if state is None:
                return True
            state = Path(state)
            root_record = state / "root.json"
            if not root_record.exists() and not root_record.is_symlink():
                if not state.exists():
                    return True
                if state.is_symlink() or not state.is_dir():
                    return False
                try:
                    return not any(state.iterdir())
                except OSError:
                    return False
            try:
                self.authorize_lifetime_closure()
            except (AcceptanceFailure, OSError, ValueError, TypeError, KeyError):
                return False
            return True
        try:
            expected_inspections = read_inspection_records(
                self.prepared.state, getattr(self, "inspection_attempts", set())) \
                if hasattr(self, "prepared") else {}
        except (AcceptanceFailure, OSError, ValueError, TypeError, KeyError):
            return False
        ordinary_rows = [row for row in tasks.values()
                         if isinstance(row, dict) and row.get("group") == "default"]
        inspection_rows = [row for row in tasks.values()
                           if isinstance(row, dict) and row.get("group") != "default"]
        if expected_inspections:
            if len(inspection_rows) != len(expected_inspections):
                return False
            by_label = {row.get("label"): row for row in inspection_rows}
            for record in expected_inspections.values():
                row = by_label.get(record["request"].get("label"))
                if not isinstance(row, dict):
                    return False
                try:
                    validate_inspection_queue_row(row, record, self.runner, self.prepared.state)
                except (AcceptanceFailure, KeyError, TypeError, ValueError):
                    return False
        elif inspection_rows:
            return False
        seen = set()
        for row in ordinary_rows:
            if not isinstance(row, dict) or row_state(row) != "Done":
                return False
            label = row.get("label", "")
            matches = [task for task in self.dispatch_attempts
                       if isinstance(label, str) and label.endswith(":" + task)]
            if len(matches) != 1:
                return False
            task = matches[0]
            if self.root_id is None or label != f"delegate:{self.root_id}:{task}":
                return False
            validate_queue_row(row, f"delegate:{self.root_id}:{task}", self.runner, self.prepared.state, task)
            if task in seen:
                return False
            seen.add(task)
        if seen != self.dispatch_attempts:
            return False
        try:
            self.authorize_lifetime_closure()
        except (AcceptanceFailure, OSError, ValueError, TypeError, KeyError):
            return False
        return True

    def success_receipt(self) -> dict[str, object]:
        return success_receipt(
            prepared=self.prepared, daemon_done=self.daemon_done, final_queue=self.final_queue, task_numbers=self.task_numbers,
            labels=self.labels, task_results=self.task_results, nonprovider_tasks=self.nonprovider_tasks,
            pueue_base=self.pueue_base)

    def run(self) -> dict[str, object]:
        self.setup()
        self.run_l1()
        self.run_l2()
        self.run_l3()
        self.shutdown()
        return self.success_receipt()
