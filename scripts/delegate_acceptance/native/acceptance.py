#!/usr/bin/env python3
"""Run a bounded live acceptance gate for the native provider matrix.

The gate uses the shipped dispatcher and runner for every provider turn.  It
does not copy credentials, invoke a provider task directly, retry paid work,
or turn an authentication refusal into a passing result. A provider's native,
non-task authentication-status command may gate the turn when advertised here.
"""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
from functools import cached_property
from pathlib import Path
import select as select
import secrets
import shutil
import sys as sys
import time

from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.evidence.files import (
    digest as digest,
    read_json as read_json,
    write_json,
)
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure, _task_identity, require,
)
from scripts.delegate_acceptance.inspections.binding import (
    INSPECTION_GROUP_PREFIX as INSPECTION_GROUP_PREFIX,
)
from scripts.delegate_acceptance.native.bindings import ProviderRuntimeBindings
from scripts.delegate_acceptance.native.errors import BlockedFailure
from scripts.delegate_acceptance.native.interactions import InteractionObserver
from scripts.delegate_acceptance.native.model_discovery import ModelDiscoveryState
import scripts.delegate_acceptance.native.acceptance_collection as acceptance_collection
import scripts.delegate_acceptance.native.acceptance_diagnostics as acceptance_diagnostics
import scripts.delegate_acceptance.native.acceptance_discovery as acceptance_discovery
import scripts.delegate_acceptance.native.acceptance_dispatch as acceptance_dispatch
import scripts.delegate_acceptance.native.acceptance_lifecycle as acceptance_lifecycle
import scripts.delegate_acceptance.native.acceptance_operations as acceptance_operations
import scripts.delegate_acceptance.native.acceptance_scenarios as acceptance_scenarios
from scripts.delegate_acceptance.native.acceptance_diagnostics import (
    authentication_unavailable as authentication_unavailable,
)
from scripts.delegate_acceptance.native.acceptance_discovery import (
    native_environment,
    require_discovery,
    resolve_executable,
)
from scripts.delegate_acceptance.native.task_ops import (
    ModelDiscoveryCleanupOps,
    NativeAcceptanceTaskOps,
)
from scripts.delegate_acceptance.native.timeouts import (
    TimeoutFifoLease as TimeoutFifoLease,
    TimeoutLeaseOwner,
)
from scripts.delegate_acceptance.native.constants import (
    PROFILES,
    TASK_BUDGET,
    CANONICAL_TASK_BUDGET,
    TASK_BUDGET_NANOS,
    WATCH_SECONDS,
    TIMEOUT_RESUME_SOURCE_BUDGET as TIMEOUT_RESUME_SOURCE_BUDGET,
    CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET as CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET,
    TIMEOUT_RESUME_SOURCE_BUDGET_NANOS as TIMEOUT_RESUME_SOURCE_BUDGET_NANOS,
    CODEX_TIMEOUT_BASELINE_DIRECTORIES as CODEX_TIMEOUT_BASELINE_DIRECTORIES,
    TIMEOUT_LEASE_SETUP_SECONDS,
    MODEL_DISCOVERY_TASK_BUDGET as MODEL_DISCOVERY_TASK_BUDGET,
    MODEL_DISCOVERY_TASK_BUDGET_NANOS as MODEL_DISCOVERY_TASK_BUDGET_NANOS,
    MODEL_DISCOVERY_TIMEOUT_SECONDS as MODEL_DISCOVERY_TIMEOUT_SECONDS,
    FAILURE_DIAGNOSTICS_BYTES as FAILURE_DIAGNOSTICS_BYTES,
)
from scripts.delegate_acceptance.native.provider_policy import (
    authentication_blocked as authentication_blocked,
    model_discovery_exit_code as model_discovery_exit_code,
    validate_model_discovery_response as validate_model_discovery_response,
)
from scripts.delegate_acceptance.native.workspace import (
    task_digest_snapshot as task_digest_snapshot,
    has_git_ancestor,
    workspace_snapshot as workspace_snapshot,
)
from scripts.delegate_acceptance.native.diagnostics import (
    _diagnostic_task,
    _fit_failure_diagnostics,
)
from scripts.delegate_acceptance.native.fixtures import (
    bounded_text,
    marker_answer as marker_answer,
    random_marker,
    verify_read_only_answer as verify_read_only_answer,
    verify_write_artifacts as verify_write_artifacts,
    verify_write_fixture as verify_write_fixture,
    shell_digest_command as shell_digest_command,
    shell_append_continuation_command as shell_append_continuation_command,
    require_same_session as require_same_session,
    require_replay_unchanged as require_replay_unchanged,
)
from scripts.delegate_acceptance.native.binding_policy import (
    dispatch_binding as dispatch_binding,
)
from scripts.delegate_acceptance.paths.operations import reject_tmp, write_bytes
from scripts.delegate_acceptance.paths.policy import clean_absolute
from scripts.delegate_acceptance.processes.observation import (
    inherited_environment as inherited_environment,
)
from scripts.delegate_acceptance.processes.operations import Processes
from scripts.delegate_acceptance.supervisor.config_policy import config_for
from scripts.delegate_acceptance.tasks.operations import (
    NativeTaskOps as NativeTaskOps,
)


class NativeAcceptance:
    def __init__(self, args: argparse.Namespace):
        if args.provider not in PROFILES:
            raise AcceptanceFailure(f"unsupported live provider: {args.provider}")
        profile = PROFILES[args.provider]
        requested_mode = args.mode or profile["default_mode"]
        if requested_mode not in profile["modes"]:
            supported = ", ".join(profile["modes"])
            raise AcceptanceFailure(f"{args.provider} does not support {requested_mode}; supports {supported}")
        scenario = getattr(args, "scenario", None)
        if scenario is None:
            scenario = "write" if requested_mode == "workspace-write" else "read-only"
        if scenario not in {"read-only", "write", "timeout-resume", "interactive", "non-git"}:
            raise AcceptanceFailure(f"unsupported native acceptance scenario: {scenario}")
        if scenario == "read-only" and requested_mode != "read-only":
            raise AcceptanceFailure("read-only scenario requires --permission read-only")
        if scenario == "write" and requested_mode != "workspace-write":
            raise AcceptanceFailure("write scenario requires --permission workspace-write")
        if scenario == "non-git" and args.provider != "codex:exec":
            raise AcceptanceFailure("non-git scenario requires codex:exec")
        if scenario == "timeout-resume" and (args.provider != "codex:exec" or
                                                requested_mode != "workspace-write"):
            raise AcceptanceFailure("timeout-resume scenario requires codex:exec workspace-write")
        if scenario == "interactive" and (args.provider != "claude:print" or
                                            requested_mode != "read-only"):
            raise AcceptanceFailure("interactive scenario requires claude:print read-only")
        discover_models = bool(getattr(args, "discover_models", False))
        self.args = args
        self.provider = args.provider
        self.profile = profile
        self.mode = requested_mode
        self.scenario = scenario
        self.discover_models_only = discover_models
        self.environment = native_environment()
        self.output = clean_absolute(args.output, "live acceptance output")
        require(self.output.is_dir() and not self.output.is_symlink(),
                "live acceptance output is not a regular directory")
        repository = Path(__file__).resolve().parents[3]
        self.delegate = resolve_executable(args.delegate, repository / "bin" / "delegate", "delegate")
        self.runner = resolve_executable(args.runner, repository / "bin" / "delegate-run", "delegate-run")
        self.pueue = resolve_executable(args.pueue, Path(shutil.which("pueue", path=self.environment.get("PATH")) or "pueue"), "pueue")
        require_discovery("pueue", self.pueue, self.environment)
        self.pueued = resolve_executable(args.pueued, Path(shutil.which("pueued", path=self.environment.get("PATH")) or "pueued"), "pueued")
        discovered_provider = shutil.which(profile["executable"], path=self.environment.get("PATH"))
        self.provider_executable = resolve_executable(
            args.provider_executable, Path(discovered_provider or profile["executable"]), profile["executable"])
        require_discovery(profile["executable"], self.provider_executable, self.environment)
        self.git: Path | None = None
        if self.provider in {"codex:exec", "codex:review"}:
            discovered_git = shutil.which("git", path=self.environment.get("PATH"))
            try:
                self.git = resolve_executable(discovered_git, Path("/usr/bin/git"), "git")
            except BlockedFailure as error:
                raise BlockedFailure("Codex git prerequisite is unavailable") from error

        self._constructor_ownership = DirectoryOwner()
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + secrets.token_hex(6)
        # Provider identity is recorded in the receipt; keep the disposable path
        # short enough for Darwin's Unix socket address limit.
        self.base_parent = Path.home() / "delegation-layer-live"
        self.base = self.base_parent / stamp
        if self.scenario == "non-git" and has_git_ancestor(self.base):
            raise BlockedFailure("non-Git acceptance requires a base outside Git repositories")
        try:
            reject_tmp(self.base, "live acceptance base")
            # This parent is shared by concurrent invocations, even when this
            # invocation creates it. Only the unique child is disposable.
            self.base_parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            require(self.base_parent.is_dir() and not self.base_parent.is_symlink(),
                    "live acceptance base parent is unsafe")
            self.base = self._constructor_ownership.create(
                "acceptance base", self.base, self.base_parent).directory
            self._initialize_runtime_roots()
        except BaseException as error:
            try:
                self._constructor_ownership.rollback()
            except BaseException as rollback_error:
                raise RuntimeError(
                    "native acceptance constructor rollback failed: " + str(rollback_error)) from error
            raise
        self.owned_directories = self._constructor_ownership.transfer()

    def _initialize_runtime_roots(self) -> None:
        self.state = self.base / "state"
        self.workspace = self.base / "workspace"
        self.briefs = self.base / "briefs"
        self.pueue_base = self.base / "pueue"
        self.models_state = self.base / "models-state"
        for label, path, parent in (
            ("acceptance state", self.state, self.base),
            ("acceptance workspace", self.workspace, self.base),
            ("acceptance briefs", self.briefs, self.base),
            ("private pueue", self.pueue_base, self.base),
            ("private pueue state", self.pueue_base / "state", self.pueue_base),
            ("private pueue runtime", self.pueue_base / "run", self.pueue_base),
            ("model discovery state", self.models_state, self.base),
        ):
            self._constructor_ownership.create(label, path, parent)
        # The timeout owner holds FIFO identity and readiness until closure.
        self.timeout_state = TimeoutLeaseOwner(
            self.workspace, self.scenario == "timeout-resume", self._constructor_ownership)
        self.config = self.pueue_base / "pueue.yml"
        write_json(self.config, config_for(self.pueue_base))
        self.processes = Processes(
            self.output / "processes", self.environment,
            owner=self._constructor_ownership)
        self.ops = NativeAcceptanceTaskOps(self)
        self.daemon = None
        self.closed = False
        self.model_discovery_state = ModelDiscoveryState()
        self.root_id: str | None = None
        self.provider_version: str | None = None
        # Immutable task metadata remains authoritative; this owner keeps the
        # acceptance receipt projection bound to those task identities.
        self.pueue_version: str | None = None
        self.pueued_version: str | None = None
        self.tasks: dict[str, str] = {}
        self.numeric_ids: dict[str, int] = {}
        self.records: dict[str, dict[str, object]] = {}
        self.workspace_baseline: dict[str, dict[str, object]] | None = None
        self.workspace_marker = self.workspace / "acceptance-marker.txt"
        self.marker_bytes = random_marker()
        write_bytes(self.workspace_marker, self.marker_bytes)
        self.marker_text = bounded_text(self.workspace_marker, "acceptance marker").removesuffix("\n")
        require(self.marker_bytes == (self.marker_text + "\n").encode(),
                "acceptance marker has an unexpected value")
        self.nonce: str | None = None
        self.nonce_file = self.workspace / "acceptance-write-nonce.txt"
        self.fixture_file = self.workspace / "acceptance-write-fixture.txt"
        self.target_file = self.workspace / "acceptance-write-target.txt"
        self.check_file = self.workspace / "acceptance-write-check.txt"
        self.continue_check_file = self.workspace / "acceptance-write-continue-check.txt"
        if self.mode == "workspace-write":
            self.nonce = secrets.token_hex(24)
            write_bytes(self.nonce_file, (self.nonce + "\n").encode())
            write_bytes(self.fixture_file, (f"preexisting:{self.nonce}\n").encode())

    def _discovery_state(self) -> ModelDiscoveryState:
        state = getattr(self, "model_discovery_state", None)
        if not isinstance(state, ModelDiscoveryState):
            state = ModelDiscoveryState()
            self.model_discovery_state = state
        return state

    @cached_property
    def _provider_bindings(self) -> ProviderRuntimeBindings:
        return ProviderRuntimeBindings()

    @cached_property
    def _interaction_observer(self) -> InteractionObserver:
        return InteractionObserver()

    @property
    def model_discovery(self) -> dict[str, object] | None:
        return self._discovery_state().response

    @model_discovery.setter
    def model_discovery(self, value: dict[str, object] | None) -> None:
        self._discovery_state().response = value

    @property
    def model_discovery_summary(self) -> dict[str, object] | None:
        return self._discovery_state().summary

    @model_discovery_summary.setter
    def model_discovery_summary(self, value: dict[str, object] | None) -> None:
        self._discovery_state().summary = value

    @property
    def model_discovery_exit_code(self) -> int | None:
        return self._discovery_state().exit_code

    @model_discovery_exit_code.setter
    def model_discovery_exit_code(self, value: int | None) -> None:
        self._discovery_state().exit_code = value

    @property
    def models_root_id(self) -> str | None:
        return self._discovery_state().root_id

    @models_root_id.setter
    def models_root_id(self, value: str | None) -> None:
        self._discovery_state().root_id = value

    @property
    def model_discovery_task_id(self) -> str | None:
        return self._discovery_state().task_id

    @model_discovery_task_id.setter
    def model_discovery_task_id(self, value: str | None) -> None:
        if value is None:
            self._discovery_state().task_id = None
            return
        self._discovery_state().record_task(value)


    @staticmethod
    def _fit_failure_diagnostics(value: dict[str, object]) -> dict[str, object]:
        return _fit_failure_diagnostics(value)

    @staticmethod
    def _diagnostic_task(state: Path, task_id: str, omitted: list[str]) -> dict[str, object]:
        return _diagnostic_task(state, task_id, omitted)

    def _failure_diagnostics(self) -> dict[str, object]:
        return acceptance_diagnostics._failure_diagnostics(self)

    def _failure_diagnostic_record(
            self, omitted: list[str]) -> tuple[dict[str, object], bool]:
        return acceptance_diagnostics._failure_diagnostic_record(self, omitted)

    def _failure_diagnostic_task_specs(
            self, omitted: list[str]) -> list[tuple[str, str, Path]]:
        return acceptance_diagnostics._failure_diagnostic_task_specs(self, omitted)

    def _write_failure_diagnostics(self) -> bool:
        return acceptance_diagnostics._write_failure_diagnostics(self)

    def _cleanup_timeout_leases(self) -> bool:
        return acceptance_lifecycle._cleanup_timeout_leases(self)

    def finalize(self, success: bool, *, blocked: bool = False) -> bool:
        return acceptance_lifecycle.finalize(self, success, blocked=blocked)

    def publish_success(self) -> None:
        return acceptance_lifecycle.publish_success(self)

    def direct(self, name: str, argv: list[object], expected=0, timeout=30):
        return acceptance_operations.direct(self, name, argv, expected, timeout)

    def _sync_ops_owned_tasks(self) -> None:
        return acceptance_operations._sync_ops_owned_tasks(self)

    def write_binding(self, task_id: str, provider_identity: dict[str, str]) -> None:
        return acceptance_operations.write_binding(self, task_id, provider_identity)

    def _sync_model_cleanup(self, ops: ModelDiscoveryCleanupOps) -> None:
        return acceptance_discovery._sync_model_cleanup(self, ops)

    def _record_provider_runtime_identity(
            self, task_id: str, meta: dict[str, object],
            launch_evidence: tuple[dict[str, object], dict[str, object]] | None = None,
            started_evidence: dict[str, object] | None = None) -> dict[str, str]:
        return acceptance_operations._record_provider_runtime_identity(self,
            task_id, meta, launch_evidence, started_evidence)

    def provider_runtime_receipt(self) -> dict[str, object]:
        return acceptance_operations.provider_runtime_receipt(self)

    def native_mode_receipt(self) -> dict[str, object] | None:
        return acceptance_operations.native_mode_receipt(self)

    def _prepare_codex_workspace(self) -> None:
        return acceptance_discovery._prepare_codex_workspace(self)

    def discover_models(self) -> None:
        return acceptance_discovery.discover_models(self)

    def model_discovery_argv(self) -> list[object]:
        return acceptance_discovery.model_discovery_argv(self)

    def _validate_model_discovery_evidence(self, status: dict[str, object],
                                           model_group: str) -> None:
        return acceptance_discovery._validate_model_discovery_evidence(self, status, model_group)

    def setup(self) -> None:
        return acceptance_discovery.setup(self)

    def require_authentication_prerequisite(self) -> None:
        return acceptance_discovery.require_authentication_prerequisite(self)

    def require_default_model_prerequisite(self) -> None:
        return acceptance_discovery.require_default_model_prerequisite(self)

    def _adopt_daemon(self, process: object) -> None:
        return acceptance_discovery._adopt_daemon(self, process)

    def brief(self, name: str, continuation: bool = False) -> Path:
        return acceptance_operations.brief(self, name, continuation)

    def interactive_brief(self, name: str) -> Path:
        return acceptance_operations.interactive_brief(self, name)

    def timeout_brief(self, name: str, resume: bool = False) -> Path:
        return acceptance_operations.timeout_brief(self, name, resume)

    def _observe_timeout_leases(self) -> None:
        return acceptance_operations._observe_timeout_leases(self)

    def _timeout_liveness_ready(self) -> bool:
        return acceptance_operations._timeout_liveness_ready(self)

    def _timeout_liveness_receipt(self, closed: bool) -> dict[str, object]:
        return acceptance_operations._timeout_liveness_receipt(self, closed)

    def _capture_timeout_liveness(self) -> bool:
        return acceptance_operations._capture_timeout_liveness(self)

    def record_timeout_liveness(
            self, task_id: str, timeout: float = TIMEOUT_LEASE_SETUP_SECONDS,
            sleep_fn=time.sleep, monotonic_fn=time.monotonic) -> None:
        return acceptance_operations.record_timeout_liveness(self, task_id, timeout, sleep_fn, monotonic_fn)

    def _timeout_liveness_closed(self) -> bool:
        return acceptance_operations._timeout_liveness_closed(self)

    def wait_timeout_leases_closed(self) -> None:
        return acceptance_operations.wait_timeout_leases_closed(self)

    def task_command_prefix(self) -> list[object]:
        return acceptance_dispatch.task_command_prefix(self)

    def dispatch_argv(self, task_id: str, brief: Path,
                      predecessor: str | None = None,
                      budget: str = TASK_BUDGET) -> list[object]:
        return acceptance_dispatch.dispatch_argv(self, task_id, brief, predecessor, budget)

    def dispatch(self, name: str, brief: Path, predecessor: str | None = None,
                 budget: str = TASK_BUDGET) -> str:
        return acceptance_dispatch.dispatch(self, name, brief, predecessor, budget)

    def continue_task(self, name: str, brief: Path, predecessor: str) -> str:
        return acceptance_dispatch.continue_task(self, name, brief, predecessor)

    def _interaction_events(self, name: str, task_id: str, after: int,
                            watch_seconds: int) -> dict[str, object]:
        return acceptance_dispatch._interaction_events(self, name, task_id, after, watch_seconds)

    def wait_for_interaction(self, task_id: str, after: int = 0,
                             pending: bool = False) -> tuple[dict[str, object], int, list[dict[str, object]]]:
        return acceptance_dispatch.wait_for_interaction(self, task_id, after, pending)

    @staticmethod
    def interactive_request(state: dict[str, object]) -> tuple[str, str]:
        requests = state.get("pending_requests")
        require(isinstance(requests, list) and len(requests) == 1,
                "interactive acceptance did not expose one pending question")
        request = requests[0]
        require(isinstance(request, dict), "interactive pending question is malformed")
        request_id = request.get("id")
        turn_id = request.get("turn_id")
        require(_task_identity(request_id) and isinstance(turn_id, str) and bool(turn_id) and
                request.get("blocking") is True,
                "interactive pending question identity is invalid")
        questions = request.get("questions")
        require(isinstance(questions, list) and len(questions) == 1,
                "interactive acceptance did not expose one question")
        question = questions[0]
        require(isinstance(question, dict) and question.get("id") == "1" and
                question.get("text") == "Choose a color" and
                question.get("options") == ["Blue", "Green"] and
                question.get("multiple") is False,
                "interactive question contract changed")
        return request_id, turn_id

    def reply_interaction(self, task_id: str, request_id: str, turn_id: str) -> tuple[str, dict[str, object]]:
        return acceptance_dispatch.reply_interaction(self, task_id, request_id, turn_id)

    def continue_timed_out_task(self, name: str, brief: Path, predecessor: str) -> str:
        return acceptance_dispatch.continue_timed_out_task(self, name, brief, predecessor)

    def wait_done(self, name: str, task_id: str,
                  expected_result: str | frozenset[str] | None = None) -> dict[str, object]:
        return acceptance_dispatch.wait_done(self, name, task_id, expected_result)

    def wait_timed_out(self, task_id: str, timeout: float = WATCH_SECONDS,
                       sleep_fn=time.sleep, monotonic_fn=time.monotonic) -> dict[str, object]:
        return acceptance_dispatch.wait_timed_out(self, task_id, timeout, sleep_fn, monotonic_fn)

    def terminal_private_queue(self, status: dict[str, object]) -> bool:
        return acceptance_collection.terminal_private_queue(self, status)

    def _without_model_discovery_queue(self, status: dict[str, object]) -> dict[str, object]:
        return acceptance_collection._without_model_discovery_queue(self, status)

    @staticmethod
    def _empty_private_queue(status: dict[str, object]) -> bool:
        # This provider harness passes an explicit external supervisor config;
        # external supervisors retain their owner's parallelism policy.
        rows = status.get("tasks")
        groups = status.get("groups")
        if not isinstance(rows, dict) or not isinstance(groups, dict) or rows:
            return False
        if not groups:
            return True
        if set(groups) != {"default"}:
            return False
        default = groups["default"]
        return (isinstance(default, dict) and default.get("status") == "Running" and
                type(default.get("parallel_tasks")) is int and
                default.get("parallel_tasks") == 1)

    def _expected_inspection_attempts(self) -> set[str]:
        return acceptance_collection._expected_inspection_attempts(self)

    @staticmethod
    def _validate_terminal_default_group(status: dict[str, object]) -> None:
        groups = status.get("groups")
        require(isinstance(groups, dict), "private queue groups object is absent")
        default = groups.get("default")
        require(isinstance(default, dict) and default.get("status") == "Running" and
                type(default.get("parallel_tasks")) is int and
                default.get("parallel_tasks") == 1,
                "default supervisor group is not running at parallelism one")

    def _validate_inspection_runtime_bindings(
            self, journals: dict[str, dict[str, object]]) -> None:
        return acceptance_collection._validate_inspection_runtime_bindings(self, journals)

    def _terminal_private_queue(self, status: dict[str, object]) -> bool:
        return acceptance_collection._terminal_private_queue(self, status)

    def collect(self, name: str, task_id: str) -> dict[str, object]:
        return acceptance_collection.collect(self, name, task_id)

    def collect_timed_out_source(self, task_id: str) -> dict[str, object]:
        return acceptance_collection.collect_timed_out_source(self, task_id)

    def _read_bound_provider_evidence(
            self, task_id: str, directory: Path, name: str,
            meta_digest: str, spec_digest: object) -> dict[str, object]:
        return acceptance_collection._read_bound_provider_evidence(self, task_id, directory, name, meta_digest, spec_digest)

    def _read_bound_provider_started_evidence(
            self, task_id: str, directory: Path,
            meta_digest: str, spec_digest: object) -> dict[str, object]:
        return acceptance_collection._read_bound_provider_started_evidence(self, task_id, directory, meta_digest, spec_digest)

    def _validate_provider_metadata_binding(
            self, task_id: str, directory: Path,
            meta: dict[str, object], spec_digest: object) -> None:
        return acceptance_collection._validate_provider_metadata_binding(self, task_id, directory, meta, spec_digest)

    def _request_binding(self, task_id: str, require_launch_evidence: bool = False,
                         require_started_evidence: bool = False,
                         budget: str = CANONICAL_TASK_BUDGET,
                         budget_nanos: int = TASK_BUDGET_NANOS) -> tuple[
            Path, dict[str, object], dict[str, object], str, str, Path]:
        return acceptance_collection._request_binding(self, task_id, require_launch_evidence, require_started_evidence, budget, budget_nanos)

    def timeout_source_binding(self, task_id: str) -> Path:
        return acceptance_collection.timeout_source_binding(self, task_id)

    def _timeout_workspace_snapshot(self, resumed: bool) -> dict[str, dict[str, object]]:
        return acceptance_collection._timeout_workspace_snapshot(self, resumed)

    def _workspace_write_snapshot(self, continuation: bool) -> dict[str, dict[str, object]]:
        return acceptance_collection._workspace_write_snapshot(self, continuation)

    def _is_pi_runtime_entry(self, relative: str) -> bool:
        return acceptance_collection._is_pi_runtime_entry(self, relative)

    def _validate_pi_runtime_roots(self) -> None:
        return acceptance_collection._validate_pi_runtime_roots(self)

    def task_binding(self, task_id: str, predecessor: str | None = None,
                     predecessor_session: str | None = None) -> None:
        return acceptance_collection.task_binding(self, task_id, predecessor, predecessor_session)

    def _validate_task_selection(self, task_id: str, record: dict[str, object]) -> None:
        return acceptance_collection._validate_task_selection(self, task_id, record)

    def _validate_task_policy(self, task_id: str, meta: dict[str, object]) -> dict[str, object]:
        return acceptance_collection._validate_task_policy(self, task_id, meta)

    def _validate_task_provider_receipts(
            self, task_id: str, directory: Path, meta_digest: str,
            spec_digest: str, predicate: dict[str, object]) -> str:
        return acceptance_collection._validate_task_provider_receipts(self, task_id, directory, meta_digest, spec_digest, predicate)

    def _validate_task_outcome(
            self, task_id: str, directory: Path, meta_digest: str, spec_digest: str,
            predicate: dict[str, object], record: dict[str, object],
            predecessor: str | None, predecessor_session: str | None, manifest_digest: str) -> None:
        return acceptance_collection._validate_task_outcome(self, task_id, directory, meta_digest, spec_digest, predicate, record, predecessor, predecessor_session, manifest_digest)

    def session_ref(self, task_id: str) -> str:
        return acceptance_collection.session_ref(self, task_id)

    def replay(self, records: dict[str, dict[str, object]] | None = None,
               names: tuple[str, ...] = ("fresh", "resume")) -> None:
        return acceptance_collection.replay(self, records, names)

    def shutdown(self) -> None:
        return acceptance_lifecycle.shutdown(self)

    def safe_shutdown(self) -> bool:
        return acceptance_lifecycle.safe_shutdown(self)

    def run_interactive(self) -> int:
        return acceptance_scenarios.run_interactive(self)

    def run_timeout_resume(self) -> int:
        return acceptance_scenarios.run_timeout_resume(self)

    def run(self) -> int:
        return acceptance_scenarios.run(self)

    def _run_model_discovery(self) -> int:
        return acceptance_scenarios._run_model_discovery(self)

    def _run_review(self) -> int:
        return acceptance_scenarios._run_review(self)

    def _run_resumable(self) -> int:
        return acceptance_scenarios._run_resumable(self)
