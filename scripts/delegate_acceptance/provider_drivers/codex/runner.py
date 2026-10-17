"""Own the stateful codex native acceptance run."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import secrets
import shutil
import sys
import tempfile
import time
import traceback
from datetime import datetime, timezone
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps
from scripts.delegate_acceptance.evidence.records import bounded_text
from scripts.delegate_acceptance.paths.policy import clean_absolute
from scripts.delegate_acceptance.paths.operations import ensure_private_directory
from scripts.delegate_acceptance.supervisor.queue_policy import failure_queue_finished
from scripts.delegate_acceptance.paths.operations import path_is_within
from scripts.delegate_acceptance.supervisor.binding import observed_pueue_version
from scripts.delegate_acceptance.paths.operations import reject_tmp
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.tasks.publication import snapshot
from scripts.delegate_acceptance.evidence.records import status_jobs
from scripts.delegate_acceptance.tasks.publication import verify_collected_outcome
from scripts.delegate_acceptance.paths.operations import write_bytes
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.supervisor.config_policy import config_for
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.directories.finalization import finalize_acceptance_artifacts
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.provider_drivers.codex.constants import CANONICAL_TASK_BUDGET, OUTPUT_ARGUMENT_INDEX, OUTPUT_NAME
from scripts.delegate_acceptance.provider_drivers.codex.evidence import binding_record, provider_record_snapshot, success_record, validate_task_meta as validate_codex_task_meta, validate_task_policy as validate_codex_task_policy, validate_task_request as validate_codex_task_request, validate_task_result as validate_codex_task_result, validate_task_seal as validate_codex_task_seal
from scripts.delegate_acceptance.provider_drivers.codex.probe import probe_script_bytes, require_probe_file_unchanged, validate_command_execution_controls
from scripts.delegate_acceptance.provider_drivers.codex.protocol import parse_codex_events
from scripts.delegate_acceptance.provider_drivers.codex.requests import dispatch_arguments as codex_dispatch_arguments, write_fresh_brief, write_resume_brief
from scripts.delegate_acceptance.provider_drivers.codex.runtime import (BlockedFailure, expected_codex_inspection_binding, reject_runtime_roots, require_discovery, resolve_executable, task_id)
from scripts.delegate_acceptance.provider_drivers.codex.identity import is_task_id

from scripts.delegate_acceptance.provider_drivers.codex.constants import (ACCEPTANCE_STATUS, APPROVAL, MODE, NATIVE_PROFILE_REVISION, OUTPUT_WRITER_CONTRACT, PLANNED_NATIVE_AI_TURNS, PREDICATE_SHA256, PREDICATE_VERSION, PRELAUNCH_STATUS, PROVIDER, WATCH_SECONDS)
from scripts.delegate_acceptance.provider_drivers.codex.runtime import DEFAULT_STATE_PARENT, DEFAULT_WORKSPACE_PARENT, PUEUE_PARENT


class CodexAcceptance:
    def __init__(self, args: argparse.Namespace):
        self._constructor_ownership = DirectoryOwner()
        self._pueue_ownership = DirectoryOwner()
        try:
            self._initialize(args)
        except BaseException as error:
            try:
                self._constructor_ownership.rollback()
                self._pueue_ownership.rollback()
            except BaseException as rollback_error:
                raise AcceptanceFailure("Codex constructor cleanup failed: "
                                         + str(rollback_error)) from error
            raise
        self.owned_directories = self._constructor_ownership.transfer()
        self.pueue_owned_directories = ()

    def _initialize(self, args: argparse.Namespace) -> None:
        self.args = args
        self.environment = os.environ.copy()
        self._initialize_tools(args)
        self._initialize_output(args)
        self._initialize_roots()
        self._initialize_probe()
        self._initialize_task_ops()

    def _initialize_tools(self, args: argparse.Namespace) -> None:
        self.tools = Path(args.tools).resolve(strict=True)
        require(self.tools.is_dir() and not self.tools.is_symlink(), "--tools must be a directory")
        self.delegate = resolve_executable(args.delegate, self.tools / "delegate", "delegate")
        self.runner = resolve_executable(args.runner, self.tools / "delegate-run", "delegate-run")
        self.pueue = resolve_executable(args.pueue, Path("/opt/homebrew/bin/pueue"), "pueue")
        self.pueued = resolve_executable(args.pueued, Path("/opt/homebrew/bin/pueued"), "pueued")
        self.codex = resolve_executable(args.codex, Path(shutil.which("codex") or "/opt/homebrew/bin/codex"), "codex")
        self.provider_version: str | None = None
        self.pueue_version: str | None = None
        self.pueued_version: str | None = None
        self.provider_sha256 = digest(self.codex)
        self.profile_revision: str | None = None
        self.inspection_binding: dict[str, object] | None = None
        require_discovery("codex", self.codex)
        require_discovery("pueue", self.pueue)
        require_discovery("pueued", self.pueued)

    def _initialize_output(self, args: argparse.Namespace) -> None:
        self.output = clean_absolute(args.output, "evidence output")
        reject_tmp(self.output, "evidence output")
        reject_runtime_roots(self.output, "evidence output", self.environment)
        require(not self.output.exists(), f"evidence output already exists: {self.output}")
        self.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        os.chmod(self.output, 0o700)

    def _initialize_roots(self) -> None:
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        if not DEFAULT_STATE_PARENT.exists() and not DEFAULT_STATE_PARENT.is_symlink():
            state_root = self._constructor_ownership.create(
                "acceptance state root", DEFAULT_STATE_PARENT,
                DEFAULT_STATE_PARENT.parent, parents=True).directory
        else:
            state_root = ensure_private_directory(DEFAULT_STATE_PARENT, "acceptance state root")
        if not DEFAULT_WORKSPACE_PARENT.exists() and not DEFAULT_WORKSPACE_PARENT.is_symlink():
            workspace_root = self._constructor_ownership.create(
                "acceptance workspace root", DEFAULT_WORKSPACE_PARENT,
                DEFAULT_WORKSPACE_PARENT.parent, parents=True).directory
        else:
            workspace_root = ensure_private_directory(DEFAULT_WORKSPACE_PARENT,
                                                       "acceptance workspace root")
        self.state_parent = self._constructor_ownership.create(
            "acceptance state parent", state_root / ("codex-" + stamp), state_root).directory
        self.state = self._constructor_ownership.create(
            "acceptance state", self.state_parent / "state", self.state_parent).directory
        self.workspace = self._constructor_ownership.create(
            "acceptance workspace", workspace_root / ("codex-" + stamp), workspace_root).directory
        self.sibling = self._constructor_ownership.create(
            "acceptance sibling", workspace_root / ("codex-" + stamp + "-sibling"), workspace_root).directory
        self.briefs = self._constructor_ownership.create(
            "acceptance briefs", self.state_parent / "briefs", self.state_parent).directory
        self.probe = self.state_parent / "probe.sh"
        reject_runtime_roots(self.probe, "acceptance probe", self.environment)
        for path, label in ((self.state, "acceptance state"), (self.workspace, "acceptance workspace"),
                            (self.sibling, "acceptance sibling"), (self.output, "evidence output")):
            reject_runtime_roots(path, label, self.environment)
            require(path.resolve(strict=True) == path, f"{label} must be canonical")
        require(not path_is_within(self.output, self.state_parent) and
                not path_is_within(self.state_parent, self.output) and
                not path_is_within(self.output, self.workspace) and
                not path_is_within(self.workspace, self.output) and
                not path_is_within(self.output, self.sibling) and
                not path_is_within(self.sibling, self.output),
                "evidence output overlaps acceptance state or workspace")

    def _initialize_probe(self) -> None:
        self.nonce_file = self.workspace / "nonce.txt"
        self.inside_sentinel = self.workspace / "workspace-sentinel.txt"
        self.sibling_sentinel = self.sibling / "sibling-sentinel.txt"
        self.nonce = secrets.token_hex(24).encode("ascii")
        self.nonce_bytes = self.nonce + b"\n"
        self.inside_bytes = b"WORKSPACE-SENTINEL-ORIGINAL\n"
        self.sibling_bytes = b"SIBLING-SENTINEL-ORIGINAL\n"
        write_bytes(self.nonce_file, self.nonce_bytes)
        write_bytes(self.inside_sentinel, self.inside_bytes)
        write_bytes(self.sibling_sentinel, self.sibling_bytes)
        self.probe_source = probe_script_bytes(self.nonce_file, self.inside_sentinel,
                                                self.sibling_sentinel)
        require(self.nonce not in self.probe_source, "probe script contains the nonce value")
        self.probe_size = len(self.probe_source)
        self.probe_sha256 = sha(self.probe_source)
        write_bytes(self.probe, self.probe_source)
        os.chmod(self.probe, 0o400)
        self.require_probe_unchanged("probe creation")
        self.workspace_before: dict[str, dict[str, object]] | None = None
        self.sibling_before: dict[str, dict[str, object]] | None = None

    def _initialize_task_ops(self) -> None:
        git = resolve_executable(shutil.which("git"), Path("/usr/bin/git"), "git")
        self.processes = Processes(self.output / "processes", self.environment,
                                   owner=self._constructor_ownership)
        self.ops = NativeTaskOps(self.processes, self.delegate, self.runner, self.pueue,
                                 self.state_parent, self.state, self.output,
                                 watch_seconds=WATCH_SECONDS)
        self.git = git
        self.pueue_base: Path | None = None
        self.pueue_config: Path | None = None
        self.daemon = None
        self.closed = False
        self.root_id: str | None = None
        self.tasks = self.ops.tasks
        self.labels = self.ops.labels
        self.numbers = self.ops.numbers
        self.records: dict[str, dict[str, object]] = {}
        self.dispatch_attempts = self.ops.dispatch_attempts
        self.queue_before_replay: dict[str, object] | None = None

    def require_probe_unchanged(self, phase: str) -> None:
        """Bind the probe as one regular, immutable, exact-byte fixture."""
        require_probe_file_unchanged(
            self.probe, self.state_parent, self.state, self.workspace, self.sibling,
            phase, self.probe_size, self.probe_sha256, self.nonce)

    def direct(self, name: str, argv: list[object], expected: int | set[int] = 0,
               timeout: float = 30):
        return self.ops.direct(name, argv, expected=expected, timeout=timeout)

    def client(self, name: str, operation: list[str], expected: int | set[int] = 0,
               timeout: float = 30):
        require(self.pueue_config is not None, "pueue is not configured")
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        return self.ops.client(name, operation, expected=expected, timeout=timeout)

    def setup(self) -> None:
        self.require_probe_unchanged("before setup")
        self._prepare_pueue()
        self._observe_provider_versions()
        self._start_private_daemon()
        self._write_binding_record()
        self._prepare_workspace_git()

    def _prepare_pueue(self) -> None:
        if PUEUE_PARENT.exists() or PUEUE_PARENT.is_symlink():
            ensure_private_directory(PUEUE_PARENT, "private supervisor parent")
        else:
            ensure_private_directory(PUEUE_PARENT, "private supervisor parent", create=True)
        try:
            pueue_base = Path(tempfile.mkdtemp(
                prefix="delegation-layer-codex-", dir=str(PUEUE_PARENT)))
            pueue_token = self._pueue_ownership.adopt_created(
                "private pueue", pueue_base, PUEUE_PARENT)
            self.pueue_base = pueue_token.directory
            self._pueue_ownership.create(
                "private pueue state", self.pueue_base / "state", self.pueue_base)
            self._pueue_ownership.create(
                "private pueue runtime", self.pueue_base / "run", self.pueue_base)
            aliases = self.pueue_base / "aliases.yml"
            write_bytes(aliases, b"{}\n")
            self.pueue_config = self.pueue_base / "pueue.yml"
            write_json(self.pueue_config, config_for(self.pueue_base))
            self.pueue_owned_directories = self._pueue_ownership.transfer()
        except BaseException:
            self._pueue_ownership.rollback()
            raise

    def _observe_provider_versions(self) -> None:
        pueue_version = self.direct("pueue-version", [self.pueue, "--version"], timeout=15)
        pueued_version = self.direct("pueued-version", [self.pueued, "-c", self.pueue_config, "--version"], timeout=15)
        self.pueue_version = observed_pueue_version(
            bounded_text(pueue_version.directory / "stdout", "pueue version output"), "pueue")
        self.pueued_version = observed_pueue_version(
            bounded_text(pueued_version.directory / "stdout", "pueued version output"), "pueued")
        codex_version = self.direct("codex-version", [self.codex, "--version"], timeout=15)
        observed = (codex_version.directory / "stdout").read_text().strip()
        require(observed, "Codex returned an empty version")
        self.provider_version = observed
        self.inspection_binding = expected_codex_inspection_binding(
            self.pueue, self.pueue_config, self.pueue_base, digest(self.pueue_config),
            self.runner, self.workspace, self.environment, self.codex, self.provider_sha256,
            self.pueue_version)

    def _start_private_daemon(self) -> None:
        self.daemon = self.processes.start("private-daemon", [self.pueued, "-c", self.pueue_config], self.state_parent)
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        for _ in range(200):
            require(self.daemon.poll() is None, "private pueued exited before readiness")
            process = self.client("ready", ["status", "--json"], expected={0, 1}, timeout=15)
            if int(process.result["exit_code"]) == 0:
                status = status_jobs(process)
                require(status["tasks"] == {}, "new private pueue queue was not empty")
                break
            time.sleep(0.1)
        else:
            raise AcceptanceFailure("private pueue readiness was not established")

    def _write_binding_record(self) -> None:
        binding = binding_record(
            provider=PROVIDER,
            mode=MODE,
            approval=APPROVAL,
            codex=self.codex,
            codex_version=self.provider_version,
            codex_digest=self.provider_sha256,
            delegate=self.delegate,
            delegate_digest=digest(self.delegate),
            runner=self.runner,
            runner_digest=digest(self.runner),
            pueue=self.pueue,
            pueue_digest=digest(self.pueue),
            pueued=self.pueued,
            pueued_digest=digest(self.pueued),
            pueue_version=self.pueue_version,
            pueued_version=self.pueued_version,
            pueue_base=self.pueue_base,
            pueue_config=self.pueue_config,
            config_digest=digest(self.pueue_config),
            state=self.state,
            workspace=self.workspace,
            sibling=self.sibling,
            probe=self.probe,
            probe_digest=self.probe_sha256,
            probe_bytes=self.probe_size,
            nonce_digest=sha(self.nonce_bytes),
            environment_keys=sorted(self.environment),
            acceptance_status=PRELAUNCH_STATUS,
            planned_native_ai_turns=PLANNED_NATIVE_AI_TURNS,
            driver_digest=digest(__file__),
        )
        write_json(self.output / "binding.json", binding)

    def _prepare_workspace_git(self) -> None:
        git_result = self.processes.run("git-init", [self.git, "init", "--quiet", "--initial-branch=main", self.workspace],
                                       self.workspace.parent, timeout=15)
        require(git_result.result["exit_code"] == 0 and (self.workspace / ".git").is_dir(),
                "scratch workspace is not a git repository")
        self.workspace_before = snapshot(self.workspace, ignored_prefixes=())
        self.sibling_before = snapshot(self.sibling, ignored_prefixes=())

    def fresh_brief(self) -> Path:
        return write_fresh_brief(self.briefs / "fresh.md", self.probe, self.nonce)

    def resume_brief(self) -> Path:
        return write_resume_brief(self.briefs / "resume.md", self.nonce)

    def dispatch_arguments(self, task: str, brief: Path, predecessor: str | None = None) -> list[object]:
        require(self.pueue_config is not None, "pueue is not configured")
        return codex_dispatch_arguments(
            self.delegate, self.state, self.pueue_config, self.runner, task, brief,
            self.workspace, PROVIDER, MODE, predecessor)

    def dispatch(self, name: str, task: str, brief: Path, predecessor: str | None = None) -> dict[str, object]:
        self.ops.root_id = self.root_id
        require(self.inspection_binding is not None,
                "expected Codex inspection binding is unavailable")
        self.ops.expect_inspection(task, self.inspection_binding)
        response = self.ops.dispatch(name, task, self.dispatch_arguments(task, brief, predecessor))
        root = response.get("root_id")
        require(is_task_id(root), f"{name} omitted a valid root identity")
        self.root_id = root
        self.ops.root_id = root
        require(self.root_id == root, f"{name} changed root identity")
        return response

    def wait_task(self, name: str, task: str) -> dict[str, object]:
        require(self.root_id is not None, "root identity is unavailable")
        self.ops.root_id = self.root_id
        return self.ops.wait_task(name, task)

    def collect(self, name: str, task: str) -> dict[str, object]:
        self.ops.root_id = self.root_id
        return self.ops.collect(name, task)

    def read_record(self, task: str, name: str) -> dict[str, object]:
        value = read_json(self.state / "tasks" / task / name)
        require(isinstance(value, dict), f"{task}/{name} must be an object")
        return value

    def validate_task(self, name: str, task: str, expected_answer: bytes,
                      predecessor: str | None = None) -> dict[str, object]:
        directory = self.state / "tasks" / task
        require(directory.is_dir() and not directory.is_symlink(), f"{name} task directory is absent")
        request = self._validate_task_request(name, task, directory, predecessor)
        meta = self._validate_task_meta(name, task, directory)
        seal = self._validate_task_seal(name, task, directory, meta)
        return self._validate_task_result(
            name, task, directory, expected_answer, predecessor, request, meta, seal)

    def _validate_task_request(self, name: str, task: str, directory: Path,
                               predecessor: str | None) -> dict[str, object]:
        return validate_codex_task_request(
            name, task, directory, self.root_id, self.workspace, predecessor,
            PROVIDER, MODE, CANONICAL_TASK_BUDGET)

    def _validate_task_meta(self, name: str, task: str,
                            directory: Path) -> dict[str, object]:
        meta, policy, profile_revision = validate_codex_task_meta(
            name, task, directory, self.root_id, self.codex, self.provider_version,
            self.workspace, self.provider_sha256, MODE, APPROVAL, NATIVE_PROFILE_REVISION)
        if self.profile_revision is None:
            self.profile_revision = profile_revision
        require(profile_revision == self.profile_revision,
                f"{name} effective policy revision drifted")
        validate_codex_task_policy(
            name, meta, policy, self.environment, PROVIDER, MODE, PREDICATE_VERSION,
            PREDICATE_SHA256, OUTPUT_WRITER_CONTRACT, OUTPUT_NAME, OUTPUT_ARGUMENT_INDEX)
        return meta

    def _validate_task_seal(self, name: str, task: str, directory: Path,
                            meta: dict[str, object]) -> dict[str, object]:
        return validate_codex_task_seal(
            name, task, directory, self.root_id, self.labels[name], self.pueue_config,
            self.pueue_version, self.pueue, meta)

    def _validate_task_result(self, name: str, task: str, directory: Path,
                              expected_answer: bytes, predecessor: str | None,
                              request: dict[str, object], meta: dict[str, object],
                              seal: dict[str, object]) -> dict[str, object]:
        return validate_codex_task_result(
            name, task, directory, expected_answer, predecessor, request, meta, seal,
            self.root_id, PROVIDER)

    def run_fresh(self) -> dict[str, object]:
        self.require_probe_unchanged("before fresh dispatch")
        brief = self.fresh_brief()
        task = task_id()
        response = self.dispatch("fresh", task, brief)
        self.wait_task("fresh", task)
        collected = self.collect("fresh-collect", task)
        outcome, data = verify_collected_outcome(collected, self.state / "tasks" / task, "committed")
        require(data and self.nonce in data, "fresh final answer did not report the nonce")
        self.require_probe_unchanged("after fresh collection")
        parsed = parse_codex_events((self.state / "tasks" / task / "raw/stdout").read_bytes())
        controls = validate_command_execution_controls(parsed, self.probe, self.inside_sentinel,
                                                        self.sibling_sentinel, self.nonce)
        require(self.workspace_before == snapshot(self.workspace, ignored_prefixes=()),
                "Codex changed the scratch workspace or workspace sentinel")
        require(self.sibling_before == snapshot(self.sibling, ignored_prefixes=()),
                "Codex changed the sibling sentinel")
        record = self.validate_task("fresh", task, data)
        record.update({"dispatch": response, "collect": collected, "controls": controls})
        self.records["fresh"] = record
        write_json(self.output / "fresh-control.json", {
            "task_id": task,
            "conversation_id": record["reference"]["conversation_id"],
            "nonce_sha256": sha(self.nonce),
            "probe_command_id": controls["probe_command_id"],
            "probe_command": controls["probe_command"],
            "probe_sha256": self.probe_sha256,
            "probe_bytes": self.probe_size,
            "read_rc": controls["read_rc"],
            "workspace_write_rc": controls["workspace_rc"],
            "sibling_write_rc": controls["sibling_rc"],
            "workspace_denial": controls["workspace_stderr"],
            "sibling_denial": controls["sibling_stderr"],
            "probe_unchanged": True,
            "workspace_unchanged": True,
            "sibling_unchanged": True,
            "provider_exit_manifest_sha256": record["seal"]["manifest_sha256"],
            "outcome_sha256": digest(record["directory"] / "outcome.json"),
            "outcome": outcome,
        })
        return record

    def run_resume(self, predecessor: dict[str, object]) -> dict[str, object]:
        self.require_probe_unchanged("before resume dispatch")
        predecessor_task = self.tasks["fresh"]
        original_snapshot = predecessor["snapshot"]
        require(isinstance(original_snapshot, dict), "fresh task snapshot is unavailable")
        self.nonce_file.unlink(missing_ok=False)
        require(not self.nonce_file.exists(), "resume nonce file still exists")
        workspace_before_resume = snapshot(self.workspace, ignored_prefixes=())
        brief = self.resume_brief()
        task = task_id()
        require(task != predecessor_task, "resume must be admitted as a new task")
        response = self.dispatch("resume", task, brief, predecessor_task)
        self.wait_task("resume", task)
        collected = self.collect("resume-collect", task)
        outcome, data = verify_collected_outcome(collected, self.state / "tasks" / task, "committed")
        self.require_probe_unchanged("after resume collection")
        require(data == self.nonce, "resume did not recover exactly the prior nonce")
        record = self.validate_task("resume", task, data, predecessor_task)
        require(record["reference"]["conversation_id"] == predecessor["reference"]["conversation_id"],
                "resume changed the Codex conversation identity")
        require(original_snapshot == provider_record_snapshot(predecessor["directory"]),
                "resume changed the immutable predecessor task records")
        require(workspace_before_resume == snapshot(self.workspace, ignored_prefixes=()),
                "resume changed the workspace or workspace sentinel")
        require(self.sibling_before == snapshot(self.sibling, ignored_prefixes=()),
                "resume changed the sibling sentinel")
        resume_controls = record.get("resume_controls")
        require(isinstance(resume_controls, dict) and resume_controls.get("tool_free") is True,
                "resume tool-free oracle result is absent")
        record.update({"dispatch": response, "collect": collected})
        self.records["resume"] = record
        write_json(self.output / "resume-control.json", {
            "task_id": task,
            "predecessor_task_id": predecessor_task,
            "conversation_id": record["reference"]["conversation_id"],
            "nonce_sha256": sha(data),
            "resume_brief_contains_nonce": False,
            "tool_free": True,
            "resume_item_count": resume_controls["item_count"],
            "resume_item_types": resume_controls["item_types"],
            "original_records_unchanged": True,
            "provider_exit_manifest_sha256": record["seal"]["manifest_sha256"],
            "outcome_sha256": digest(record["directory"] / "outcome.json"),
            "outcome": outcome,
        })
        return record

    def queue_status(self, name: str) -> dict[str, object]:
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        return self.ops.queue_status(name)

    def replay(self) -> None:
        self.require_probe_unchanged("before replay")
        self.ops.root_id = self.root_id
        self.ops.replay(self.records, names=("fresh", "resume"))
        self.queue_before_replay = self.ops.queue_before_replay
        self.require_probe_unchanged("after replay")

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        return failure_queue_finished(status, self.dispatch_attempts, self.tasks,
                                      self.labels, self.runner, self.state,
                                      self.numbers)

    def shutdown(self) -> None:
        self.ops.root_id = self.root_id
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        self.ops.shutdown()
        self.closed = self.ops.closed

    def safe_failure_shutdown(self) -> bool:
        """Shutdown only after an all-Done observation; never signals unknown work."""
        self.ops.root_id = self.root_id
        if self.pueue_config is not None:
            self.ops.bind_supervisor(self.pueue_config, self.daemon)
        result = self.ops.safe_failure_shutdown()
        self.closed = self.ops.closed
        return result

    def finalize(self, success: bool) -> bool:
        """Remove this completed run's private state and raw process captures."""
        directories = list(self.owned_directories)
        directories.extend(self.pueue_owned_directories)
        return finalize_acceptance_artifacts(
            self.output, self.closed, tuple(directories), success,
            raw_process_captures_token=self.processes.capture_root)

    def run(self) -> None:
        try:
            self.setup()
            fresh = self.run_fresh()
            self.run_resume(fresh)
            self.replay()
            self.shutdown()
            self._publish_success()
        except BaseException as error:
            self._handle_failure(error)
            raise

    def _publish_success(self) -> None:
        success_receipt = success_record(
            ACCEPTANCE_STATUS, PROVIDER, PLANNED_NATIVE_AI_TURNS,
            self.tasks, self.records, self.closed)
        if not self.finalize(True):
            raise AcceptanceFailure("completed acceptance artifacts could not be cleaned")
        write_json(self.output / "success.json", success_receipt, replace=True)
        print("PASS Codex acceptance", flush=True)

    def _handle_failure(self, error: BaseException) -> None:
        receipt_removal_error: BaseException | None = None
        try:
            (self.output / "success.json").unlink(missing_ok=True)
        except BaseException as removal_error:
            receipt_removal_error = removal_error
        try:
            traceback_text = traceback.format_exc()
            write_json(self.output / "failure.json", {
                "status": "BLOCKED" if isinstance(error, BlockedFailure) else "failed",
                "error": str(error),
                "traceback": traceback_text,
                "signals_sent": 0,
                "unknown_termination_preserved": True,
                "state": str(self.state),
                "workspace": str(self.workspace),
                "tasks": dict(self.tasks),
                "dispatch_attempts": sorted(self.dispatch_attempts),
                "daemon_pid": None if self.daemon is None else self.daemon.pid,
            })
        except BaseException as diagnostic_error:
            print("WARNING Codex acceptance diagnostics were not written: "
                  + str(diagnostic_error), file=sys.stderr, flush=True)

        cleanup_errors: list[BaseException] = []
        naturally_shutdown = False
        artifact_finalized = False
        if receipt_removal_error is not None:
            cleanup_errors.append(receipt_removal_error)
        try:
            if not self.safe_failure_shutdown():
                cleanup_errors.append(
                    AcceptanceFailure("private supervisor failure shutdown was not proven"))
            else:
                naturally_shutdown = True
        except BaseException as cleanup_error:
            cleanup_errors.append(cleanup_error)
        finally:
            try:
                if not self.finalize(False):
                    cleanup_errors.append(
                        AcceptanceFailure("acceptance artifact finalization was not completed"))
                else:
                    artifact_finalized = True
            except BaseException as finalize_error:
                cleanup_errors.append(finalize_error)
        try:
            write_json(self.output / "failure-cleanup.json", {
                "status": "completed" if not cleanup_errors else "failed",
                "natural_daemon_shutdown": naturally_shutdown,
                "artifact_finalization_completed": artifact_finalized,
                "passed_receipt_absent": not (self.output / "success.json").exists(),
                "signals_sent": 0,
            }, replace=True)
        except BaseException as evidence_error:
            print("WARNING Codex cleanup evidence was not written: "
                  + str(evidence_error), file=sys.stderr, flush=True)

        if cleanup_errors:
            raise AcceptanceFailure("Codex acceptance cleanup failed: "
                                     + str(cleanup_errors[0])) from error
