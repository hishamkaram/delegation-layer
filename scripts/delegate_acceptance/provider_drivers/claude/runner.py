"""Own the stateful claude native acceptance run."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import secrets
import shutil
import time
import traceback
from datetime import datetime, timezone
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps
from scripts.delegate_acceptance.evidence.records import bounded_text
from scripts.delegate_acceptance.paths.policy import clean_absolute
from scripts.delegate_acceptance.paths.operations import ensure_private_directory, no_prompt_argv
from scripts.delegate_acceptance.supervisor.binding import observed_pueue_version
from scripts.delegate_acceptance.paths.operations import path_is_within, reject_tmp
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.tasks.publication import snapshot, verify_collected_outcome
from scripts.delegate_acceptance.paths.operations import write_bytes
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.supervisor.config_policy import config_for
from scripts.delegate_acceptance.evidence.files import digest
from scripts.delegate_acceptance.directories.finalization import finalize_acceptance_artifacts
from scripts.delegate_acceptance.evidence.files import read_json
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.provider_drivers.claude.constants import APPROVAL
from scripts.delegate_acceptance.provider_drivers.claude.evidence import provider_record_snapshot, validate_task_meta as validate_claude_task_meta, validate_task_outcome as validate_claude_task_outcome, validate_task_publication as validate_claude_task_publication, validate_task_request as validate_claude_task_request, validate_task_contract as validate_claude_task_contract
from scripts.delegate_acceptance.provider_drivers.claude.protocol import parse_claude_events, validate_fresh_controls
from scripts.delegate_acceptance.provider_drivers.claude.runtime import (BlockedFailure, expected_claude_inspection_binding, fresh_session_id, host_home_directory, native_account_acceptance_environment, path_validation_environment, reject_runtime_roots, require_discovery, resolve_executable, task_id)
from scripts.delegate_acceptance.provider_drivers.claude.identity import is_session_id, is_task_id

from scripts.delegate_acceptance.provider_drivers.claude.constants import (ACCEPTANCE_STATUS, CANONICAL_TASK_BUDGET, MODE, NATIVE_PROFILE_REVISION, PLANNED_NATIVE_AI_TURNS, PREDICATE_SHA256, PREDICATE_VERSION, PRELAUNCH_STATUS, PROVIDER, TASK_BUDGET, WATCH_SECONDS)
from scripts.delegate_acceptance.provider_drivers.claude.runtime import DEFAULT_STATE_PARENT

from scripts.delegate_acceptance.provider_drivers.claude.runtime import (
    private_mkdtemp
)


class ClaudeAcceptance:
    def __init__(self, args: argparse.Namespace):
        self._constructor_ownership = DirectoryOwner()
        self._pueue_ownership = DirectoryOwner()
        try:
            self._initialize(args)
        except BaseException:
            self._constructor_ownership.rollback()
            self._pueue_ownership.rollback()
            raise
        self.owned_directories = self._constructor_ownership.transfer()
        self.pueue_owned_directories = ()

    def _initialize(self, args: argparse.Namespace):
        self.args = args
        self._initialize_tools(args)
        self._initialize_output(args)
        self._initialize_state()
        self._initialize_workspace()
        self._initialize_task_ops()

    def _initialize_tools(self, args: argparse.Namespace) -> None:
        self.tools = Path(args.tools).resolve(strict=True)
        require(self.tools.is_dir() and not self.tools.is_symlink(), "--tools must be a directory")
        self.delegate = resolve_executable(args.delegate, self.tools / "delegate", "delegate")
        self.runner = resolve_executable(args.runner, self.tools / "delegate-run", "delegate-run")
        self.pueue = resolve_executable(args.pueue, Path("/opt/homebrew/bin/pueue"), "pueue")
        self.pueued = resolve_executable(args.pueued, Path("/opt/homebrew/bin/pueued"), "pueued")
        self.claude = resolve_executable(args.claude, Path(shutil.which("claude") or "/opt/homebrew/bin/claude"), "claude")
        self.provider_version: str | None = None
        self.pueue_version: str | None = None
        self.pueued_version: str | None = None
        self.provider_sha256 = digest(self.claude)
        self.profile_revision: str | None = None
        require_discovery("claude", self.claude)
        require_discovery("pueue", self.pueue)
        require_discovery("pueued", self.pueued)

    def _initialize_output(self, args: argparse.Namespace) -> None:
        self.output = clean_absolute(args.output, "evidence output")
        reject_tmp(self.output, "evidence output")
        reject_runtime_roots(self.output, "evidence output", path_validation_environment(os.environ))
        require(not self.output.exists(), f"evidence output already exists: {self.output}")
        self.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        os.chmod(self.output, 0o700)

    def _initialize_state(self) -> None:
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        if not DEFAULT_STATE_PARENT.exists() and not DEFAULT_STATE_PARENT.is_symlink():
            state_root = self._constructor_ownership.create(
                "acceptance state root", DEFAULT_STATE_PARENT,
                DEFAULT_STATE_PARENT.parent, parents=True).directory
        else:
            state_root = ensure_private_directory(DEFAULT_STATE_PARENT, "acceptance state root")
        self.state_parent = self._constructor_ownership.create(
            "acceptance state", state_root / ("claude-" + stamp), state_root).directory
        self.test_tmp = self._constructor_ownership.create(
            "isolated Claude temporary root", self.state_parent / "tmp", self.state_parent).directory
        self.host_home = host_home_directory()
        self.environment = native_account_acceptance_environment(os.environ, self.host_home, self.test_tmp)
        self.state = self._constructor_ownership.create(
            "acceptance state data", self.state_parent / "state", self.state_parent).directory

    def _initialize_workspace(self) -> None:
        # Keep the scratch checkout outside the caller's home tree. Claude's
        # project-policy walk includes every ancestor up to the filesystem
        # root, so a home-contained checkout would still discover the real
        # ~/.claude project settings even with an isolated HOME.
        workspace_parent = private_mkdtemp(
            "delegation-layer-claude-workspace-", "acceptance workspace parent",
            self._constructor_ownership).directory
        self.workspace_parent = workspace_parent
        self.workspace = self._constructor_ownership.create(
            "acceptance workspace", workspace_parent / "fresh", workspace_parent).directory
        self.sibling = self._constructor_ownership.create(
            "acceptance sibling", workspace_parent / "sibling", workspace_parent).directory
        self.briefs = self._constructor_ownership.create(
            "acceptance briefs", self.state_parent / "briefs", self.state_parent).directory
        for path, label in ((self.state, "acceptance state"), (self.workspace, "acceptance workspace"),
                            (self.sibling, "acceptance sibling"), (self.output, "evidence output")):
            reject_runtime_roots(path, label, self.environment)
            if path.exists() and path.is_dir():
                require(path.resolve(strict=True) == path, f"{label} must be canonical")
        require(not path_is_within(self.output, self.state_parent) and
                not path_is_within(self.state_parent, self.output) and
                not path_is_within(self.output, self.workspace) and
                not path_is_within(self.workspace, self.output) and
                not path_is_within(self.output, self.sibling) and
                not path_is_within(self.sibling, self.output),
                "evidence output overlaps acceptance state or workspace")
        self.nonce_file = self.workspace / "nonce.txt"
        self.workspace_sentinel = self.workspace / "workspace-sentinel.txt"
        self.sibling_sentinel = self.sibling / "sibling-sentinel.txt"
        self.nonce = secrets.token_hex(24).encode("ascii")
        self.inside_bytes = b"WORKSPACE-SENTINEL-ORIGINAL\n"
        self.sibling_bytes = b"SIBLING-SENTINEL-ORIGINAL\n"
        write_bytes(self.nonce_file, self.nonce + b"\n")
        write_bytes(self.workspace_sentinel, self.inside_bytes)
        write_bytes(self.sibling_sentinel, self.sibling_bytes)
        self.workspace_before = None
        self.sibling_before = None

    def _initialize_task_ops(self) -> None:
        self.processes = Processes(self.output / "processes", self.environment,
                                   owner=self._constructor_ownership)
        self.ops = NativeTaskOps(self.processes, self.delegate, self.runner, self.pueue,
                                 self.state_parent, self.state, self.output,
                                 watch_seconds=WATCH_SECONDS)
        self.pueue_base: Path | None = None
        self.pueue_config: Path | None = None
        self.daemon = None
        self.closed = False
        self.root_id: str | None = None
        self.inspection_binding: dict[str, object] | None = None
        self.tasks = self.ops.tasks
        self.labels = self.ops.labels
        self.numbers = self.ops.numbers
        self.dispatch_attempts = self.ops.dispatch_attempts
        self.records: dict[str, dict[str, object]] = {}
        self.queue_before_replay = None

    def provider_argv(self, session_id: str, resume: bool = False) -> list[str]:
        args = [str(self.claude), "--print", "--input-format", "text", "--output-format", "stream-json",
                "--verbose", "--permission-mode", "plan", "--permission-prompts", "none"]
        args.extend(["--resume", session_id] if resume else ["--session-id", session_id])
        return args

    def setup(self) -> None:
        try:
            pueue_token = private_mkdtemp(
                "delegation-layer-claude-", "private pueue base", self._pueue_ownership)
            self.pueue_base = pueue_token.directory
            self._pueue_ownership.create(
                "private pueue state", self.pueue_base / "state", self.pueue_base)
            self._pueue_ownership.create(
                "private pueue runtime", self.pueue_base / "run", self.pueue_base)
            write_bytes(self.pueue_base / "aliases.yml", b"{}\n")
            self.pueue_config = self.pueue_base / "pueue.yml"
            write_json(self.pueue_config, config_for(self.pueue_base))
            self.pueue_owned_directories = self._pueue_ownership.transfer()
        except BaseException:
            self._pueue_ownership.rollback()
            raise
        config_digest = digest(self.pueue_config)
        pueue_version = self.ops.direct("pueue-version", [self.pueue, "--version"], timeout=15)
        pueued_version = self.ops.direct("pueued-version", [self.pueued, "-c", self.pueue_config, "--version"],
            timeout=15)
        self.pueue_version = observed_pueue_version(
            bounded_text(pueue_version.directory / "stdout", "pueue version output"), "pueue")
        self.pueued_version = observed_pueue_version(
            bounded_text(pueued_version.directory / "stdout", "pueued version output"), "pueued")
        claude_version = self.ops.direct("claude-version", [self.claude, "--version"], timeout=15)
        observed = (claude_version.directory / "stdout").read_text().strip()
        require(observed, "Claude returned an empty version")
        self.provider_version = observed
        self.inspection_binding = expected_claude_inspection_binding(
            pueue=self.pueue, config=self.pueue_config, base=self.pueue_base, config_digest=config_digest,
            workspace=self.workspace, executable=self.claude, runner=self.runner, environment=self.environment,
            pueue_version=self.pueue_version)
        self.daemon = self.processes.start("private-daemon", [self.pueued, "-c", self.pueue_config], self.state_parent)
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        for _ in range(200):
            require(self.daemon.poll() is None, "private pueued exited before readiness")
            process = self.ops.client("ready", ["status", "--json"], expected={0, 1}, timeout=15)
            if int(process.result["exit_code"]) == 0:
                status = self.ops.queue_status("ready-status")
                require(status["tasks"] == {}, "new private pueue queue was not empty")
                break
            time.sleep(0.1)
        else:
            raise AcceptanceFailure("private pueue readiness was not established")
        write_json(self.output / "binding.json", {
            "provider": PROVIDER, "mode": MODE, "approval": APPROVAL,
            "claude": str(self.claude), "claude_version": self.provider_version,
            "claude_sha256": self.provider_sha256, "delegate": str(self.delegate),
            "delegate_sha256": digest(self.delegate), "runner": str(self.runner),
            "runner_sha256": digest(self.runner), "pueue": str(self.pueue),
            "pueue_sha256": digest(self.pueue), "pueued": str(self.pueued),
            "pueued_sha256": digest(self.pueued), "pueue_version": self.pueue_version,
            "pueued_version": self.pueued_version, "pueue_base": str(self.pueue_base),
            "pueue_config": str(self.pueue_config), "config_sha256": digest(self.pueue_config),
            "state": str(self.state), "workspace": str(self.workspace), "sibling": str(self.sibling),
            "host_home": str(self.host_home), "isolated_temporary_root": str(self.test_tmp),
            "environment_isolated": True, "host_home_inherited": True,
            "native_auth_context": "host-login-keychain",
            "nonce_sha256": sha(self.nonce + b"\n"), "environment_keys": sorted(self.environment),
            "environment_values_in_record": False,
            "acceptance_status": PRELAUNCH_STATUS,
            "planned_native_ai_turns": PLANNED_NATIVE_AI_TURNS, "driver_sha256": digest(__file__),
        })
        self.workspace_before = snapshot(self.workspace, ignored_prefixes=())
        self.sibling_before = snapshot(self.sibling, ignored_prefixes=())

    def fresh_brief(self) -> Path:
        path = self.briefs / "fresh.md"
        text = (
            "Use any available native read-only tool to read exactly this file: " + str(self.nonce_file) +
            ". Do not modify the workspace or its sibling. Return exactly the 48-character nonce read from "
            "that file as the result, with no prose or explanation."
        )
        write_bytes(path, text.encode())
        require(self.nonce.decode() not in path.read_text(), "fresh Claude brief contains the nonce")
        return path

    def resume_brief(self) -> Path:
        path = self.briefs / "resume.md"
        text = ("Use no tools. For each of the 48 hexadecimal characters in the answer from the previous turn, "
                "write two bits: the first bit is 1 for a-f and 0 for 0-9; the second bit is 1 for odd and 0 "
                "for even. Concatenate the 48 pairs and reply only with that 96-bit string.")
        write_bytes(path, text.encode())
        require(self.nonce.decode() not in path.read_text(), "resume Claude brief contains the nonce")
        return path

    def resume_challenge(self) -> bytes:
        """Require a high-entropy, nonce-dependent continuation transform."""
        return "".join(
            ("1" if character in "abcdef" else "0") +
            ("1" if int(character, 16) % 2 else "0")
            for character in self.nonce.decode("ascii")
        ).encode("ascii")

    def dispatch_arguments(self, task: str, brief: Path, predecessor: str | None = None) -> list[object]:
        require(self.pueue_config is not None, "pueue is not configured")
        args: list[object] = [self.delegate, "--root", self.state, "--pueue-config", self.pueue_config,
                              "--runner", self.runner, "dispatch", "--provider", PROVIDER,
                              "--brief", brief, "--cwd", self.workspace, "--id", task,
                              "--permission", MODE, "--budget", TASK_BUDGET, "--json"]
        if predecessor is not None:
            args.extend(["--resume-task", predecessor])
        no_prompt_argv(args, [brief])
        return args

    def dispatch(self, name: str, task: str, brief: Path, predecessor: str | None = None) -> dict[str, object]:
        self.ops.root_id = self.root_id
        require(self.inspection_binding is not None,
                "expected Claude inspection binding is unavailable")
        self.ops.expect_inspection(task, self.inspection_binding)
        response = self.ops.dispatch(name, task, self.dispatch_arguments(task, brief, predecessor))
        root = response.get("root_id")
        require(is_task_id(root), f"{name} omitted a valid root identity")
        self.root_id = root
        self.ops.root_id = root
        return response

    def wait_task(self, name: str, task: str) -> dict[str, object]:
        require(self.root_id is not None, "root identity is unavailable")
        self.ops.root_id = self.root_id
        return self.ops.wait_task(name, task)

    def collect(self, name: str, task: str) -> dict[str, object]:
        return self.ops.collect(name, task)

    def read_record(self, task: str, name: str) -> dict[str, object]:
        value = read_json(self.state / "tasks" / task / name)
        require(isinstance(value, dict), f"{task}/{name} must be an object")
        return value

    def validate_task(self, name: str, task: str, expected_answer: bytes,
                      predecessor: str | None = None,
                      expected_session: str | None = None) -> dict[str, object]:
        directory = self.state / "tasks" / task
        require(directory.is_dir() and not directory.is_symlink(), f"{name} task directory is absent")
        self._require_task_records(directory, name)
        request = self._validate_task_request(name, task, directory, predecessor)
        meta = self._validate_task_meta(name, task, directory)
        seal = self._validate_task_publication(name, task, directory, meta)
        return self._validate_task_outcome(
            name=name, task=task, directory=directory, expected_answer=expected_answer, predecessor=predecessor,
            expected_session=expected_session, request=request, meta=meta, seal=seal)

    @staticmethod
    def _require_task_records(directory: Path, name: str) -> None:
        for record in ("brief.md", "task.json", "meta.json", "submit.json", "provider.start",
                       "provider.started.json", "provider.exit", "provider.ref.json", "outcome.json"):
            require((directory / record).is_file(), f"{name} missing evidence {record}")

    def _validate_task_request(self, name: str, task: str, directory: Path,
                               predecessor: str | None) -> dict[str, object]:
        return validate_claude_task_request(
            name=name, task=task, directory=directory, root_id=self.root_id, workspace=self.workspace,
            predecessor=predecessor,
            provider=PROVIDER, mode=MODE, budget=CANONICAL_TASK_BUDGET)

    def _validate_task_meta(self, name: str, task: str,
                            directory: Path) -> dict[str, object]:
        meta, profile_revision = validate_claude_task_meta(
            name=name, task=task, directory=directory, root_id=self.root_id, claude=self.claude, provider_version=self.provider_version,
            workspace=self.workspace, provider_sha256=self.provider_sha256, provider=PROVIDER, mode=MODE,
            approval=APPROVAL,
            revision=NATIVE_PROFILE_REVISION)
        if self.profile_revision is None:
            self.profile_revision = profile_revision
        require(profile_revision == self.profile_revision,
                f"{name} effective policy revision drifted")
        validate_claude_task_contract(
            name, meta, PROVIDER, MODE, PREDICATE_VERSION, PREDICATE_SHA256)
        return meta

    def _validate_task_publication(self, name: str, task: str, directory: Path,
                                   meta: dict[str, object]) -> dict[str, object]:
        return validate_claude_task_publication(
            name=name, task=task, directory=directory, root_id=self.root_id, label=self.labels[name],
            pueue_config=self.pueue_config,
            pueue_version=self.pueue_version, pueue=self.pueue, meta=meta)

    def _validate_task_outcome(self, *, name: str, task: str, directory: Path,
                               expected_answer: bytes, predecessor: str | None,
                               expected_session: str | None, request: dict[str, object],
                               meta: dict[str, object], seal: dict[str, object]) -> dict[str, object]:
        return validate_claude_task_outcome(
            name=name, task=task, directory=directory, expected_answer=expected_answer, predecessor=predecessor,
            expected_session=expected_session,
            request=request, meta=meta, seal=seal, root_id=self.root_id, workspace=self.workspace, provider=PROVIDER)

    def run_fresh(self) -> dict[str, object]:
        brief = self.fresh_brief()
        task = task_id()
        response = self.dispatch("fresh", task, brief)
        self.wait_task("fresh", task)
        collected = self.collect("fresh-collect", task)
        outcome, data = verify_collected_outcome(collected, self.state / "tasks" / task, "committed")
        require(data == self.nonce, "fresh Claude result was not exactly the nonce")
        require(self.root_id is not None, "fresh Claude root identity is unavailable")
        expected_session = fresh_session_id(self.root_id, task)
        parsed = parse_claude_events((self.state / "tasks" / task / "raw/stdout").read_bytes())
        controls = validate_fresh_controls(parsed, self.nonce_file, self.workspace,
                                           self.sibling, self.nonce, expected_session)
        require(self.workspace_before == snapshot(self.workspace, ignored_prefixes=()),
                "Claude changed the scratch workspace or workspace sentinel")
        require(self.sibling_before == snapshot(self.sibling, ignored_prefixes=()),
                "Claude changed the sibling sentinel")
        record = self.validate_task("fresh", task, data, expected_session=expected_session)
        record.update({"dispatch": response, "collect": collected, "controls": controls})
        self.records["fresh"] = record
        write_json(self.output / "fresh-control.json", {
            "task_id": task, "conversation_id": record["reference"]["conversation_id"],
            "nonce_sha256": sha(self.nonce), "tool_names": controls["tool_names"],
            "api_key_source": controls["api_key_source"],
            "mcp_server_count": controls["mcp_server_count"],
            "nonce_read": True, "nonce_tool_name": controls["nonce_tool_name"],
            "nonce_read_count": controls["nonce_read_count"],
            "tool_use_count": controls["tool_use_count"],
            "permission_denial_count": controls["permission_denial_count"],
            "workspace_unchanged": True,
            "sibling_unchanged": True, "provider_exit_manifest_sha256": record["seal"]["manifest_sha256"],
            "outcome_sha256": digest(record["directory"] / "outcome.json"), "outcome": outcome,
        })
        return record

    def run_resume(self, predecessor: dict[str, object]) -> dict[str, object]:
        predecessor_task = self.tasks["fresh"]
        original_snapshot = predecessor["snapshot"]
        require(isinstance(original_snapshot, dict), "fresh task snapshot is unavailable")
        self.nonce_file.unlink(missing_ok=False)
        require(not self.nonce_file.exists(), "resume nonce file still exists")
        workspace_before = snapshot(self.workspace, ignored_prefixes=())
        task = task_id()
        response = self.dispatch("resume", task, self.resume_brief(), predecessor_task)
        self.wait_task("resume", task)
        collected = self.collect("resume-collect", task)
        outcome, data = verify_collected_outcome(collected, self.state / "tasks" / task, "committed")
        require(data == self.resume_challenge(), "Claude resume did not recover the prior answer context")
        predecessor_reference = predecessor.get("reference")
        require(isinstance(predecessor_reference, dict) and
                is_session_id(predecessor_reference.get("conversation_id")),
                "fresh Claude predecessor session identity is unavailable")
        expected_session = str(predecessor_reference["conversation_id"])
        record = self.validate_task("resume", task, data, predecessor_task, expected_session)
        require(record["reference"]["conversation_id"] == predecessor["reference"]["conversation_id"],
                "Claude resume changed the conversation identity")
        require(original_snapshot == provider_record_snapshot(predecessor["directory"]),
                "Claude resume changed immutable predecessor task records")
        require(workspace_before == snapshot(self.workspace, ignored_prefixes=()),
                "Claude resume changed workspace or sentinel")
        require(self.sibling_before == snapshot(self.sibling, ignored_prefixes=()),
                "Claude resume changed sibling sentinel")
        controls = record.get("resume_controls")
        require(isinstance(controls, dict) and controls.get("tool_free") is True,
                "Claude resume tool-free oracle result is absent")
        record.update({"dispatch": response, "collect": collected})
        self.records["resume"] = record
        write_json(self.output / "resume-control.json", {
            "task_id": task, "predecessor_task_id": predecessor_task,
            "conversation_id": record["reference"]["conversation_id"], "nonce_sha256": sha(self.nonce),
            "resume_challenge_sha256": sha(data), "resume_brief_contains_nonce": False, "tool_free": True,
            "resume_item_count": controls["item_count"], "resume_item_types": controls["item_types"],
            "original_records_unchanged": True, "provider_exit_manifest_sha256": record["seal"]["manifest_sha256"],
            "outcome_sha256": digest(record["directory"] / "outcome.json"), "outcome": outcome,
        })
        return record

    def replay(self) -> None:
        self.ops.root_id = self.root_id
        self.ops.replay(self.records, names=("fresh", "resume"))
        self.queue_before_replay = self.ops.queue_before_replay

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        return self.ops.failure_queue_finished(status)

    def shutdown(self) -> None:
        self.ops.root_id = self.root_id
        self.ops.bind_supervisor(self.pueue_config, self.daemon)
        self.ops.shutdown()
        self.closed = self.ops.closed

    def safe_failure_shutdown(self) -> bool:
        self.ops.root_id = self.root_id
        if self.pueue_config is not None:
            self.ops.bind_supervisor(self.pueue_config, self.daemon)
        result = self.ops.safe_failure_shutdown()
        self.closed = self.ops.closed
        return result

    def retain_failure_ownership(self) -> bool:
        """Retain active acceptance processes until exact natural cleanup."""
        self.ops.root_id = self.root_id
        if self.pueue_config is not None:
            self.ops.bind_supervisor(self.pueue_config, self.daemon)
        result = self.ops.retain_failure_ownership()
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
            success_receipt = {
                "status": ACCEPTANCE_STATUS, "provider": PROVIDER,
                "native_ai_turns": len(self.tasks),
                "planned_native_ai_turns": PLANNED_NATIVE_AI_TURNS,
                "tasks": {name: {"task_id": self.tasks[name],
                                  "conversation_id": self.records[name]["reference"]["conversation_id"],
                                  "outcome_sha256": digest(self.records[name]["directory"] / "outcome.json"),
                                  "evidence_sha256": self.records[name]["outcome"]["evidence_sha256"]}
                           for name in ("fresh", "resume")},
                "replay_launches": 0, "natural_daemon_shutdown": self.closed, "signals_sent": 0,
            }
            if not self.finalize(True):
                raise AcceptanceFailure("completed acceptance artifacts could not be cleaned")
            write_json(self.output / "success.json", success_receipt, replace=True)
            print("PASS Claude acceptance", flush=True)
        except BaseException as error:
            traceback_text = traceback.format_exc()
            diagnostics_written = False
            naturally_shutdown = False
            artifact_finalized = False
            finalization_error: str | None = None
            try:
                (self.output / "success.json").unlink(missing_ok=True)
            except BaseException as receipt_error:
                finalization_error = "passed receipt removal failed: " + str(receipt_error)
            try:
                try:
                    write_json(self.output / "failure.json", {
                        "status": "BLOCKED" if isinstance(error, BlockedFailure) else "failed",
                        "error": str(error), "traceback": traceback_text,
                        "natural_daemon_shutdown": self.closed, "signals_sent": 0,
                        "unknown_termination_preserved": True, "state": str(self.state),
                        "workspace": str(self.workspace), "tasks": dict(self.tasks),
                        "dispatch_attempts": sorted(self.dispatch_attempts),
                        "daemon_pid": None if self.daemon is None else self.daemon.pid,
                    })
                    diagnostics_written = True
                except BaseException:
                    # Failure diagnostics are best effort; ownership cleanup
                    # must still run if serialization or the write fails.
                    pass
            finally:
                try:
                    naturally_shutdown = self.retain_failure_ownership()
                except BaseException:
                    naturally_shutdown = False
                try:
                    artifact_finalized = self.finalize(False)
                    if not artifact_finalized:
                        finalization_error = finalization_error or "acceptance artifact finalization was not completed"
                except BaseException as finalize_error:
                    finalization_error = finalization_error or str(finalize_error)
                try:
                    write_json(self.output / "failure-cleanup.json", {
                        "natural_daemon_shutdown": naturally_shutdown,
                        "signals_sent": 0,
                        "owned_processes_joined": naturally_shutdown,
                        "failure_diagnostics_written": diagnostics_written,
                        "artifact_finalization_completed": artifact_finalized,
                        "artifact_finalization_error": finalization_error,
                        "passed_receipt_absent": not (self.output / "success.json").exists(),
                    }, replace=True)
                except BaseException:
                    pass
            raise
