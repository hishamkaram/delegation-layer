"""Owned launch scopes that separate a native continuation from its predecessor.

``NativeAcceptance`` owns one immutable ``LaunchScopes`` per case. Each scope
is a full copy of the ambient snapshot that changes only an owned marker: the
fresh turn is admitted under the ``predecessor`` TMPDIR and its successor under
``successor``. Every other command (setup, supervision, collection and replay)
runs under the unchanged ambient snapshot the process owner captured. HOME,
authentication and provider configuration stay the user's own entries.

Each admitted task's ``meta.json`` remains the authority for its launch. These
helpers only read it to prove which scope a launch recorded, and they return
bounded facts that never carry an environment value.
"""

from __future__ import annotations

from dataclasses import dataclass
import os
from pathlib import Path
from types import MappingProxyType
from typing import Mapping

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.files import digest
from scripts.acceptance.evidence.files import read_json
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.native.failure_diagnostics import authentication_unavailable
from scripts.acceptance.native.constants import MAX_RAW_BYTES
from scripts.acceptance.native.errors import BlockedFailure
from scripts.acceptance.native.errors import BlockReason
from scripts.acceptance.supervisor.queue_policy import done_result
from scripts.acceptance.tasks.publication import verify_collected_outcome


CONTINUATION_COMMANDS = ("dispatch", "continue")
TMPDIR_MARKERS = MappingProxyType({"predecessor": "tp", "successor": "ts"})
PI_SESSION_DIRECTORY_KEY = "PI_CODING_AGENT_SESSION_DIR"
# Pi 1.4.0 main.ts: an unmatched --session logs this line to stderr and exits 1
# before any session manager exists, so it never creates a fresh session.
PI_SESSION_MISS_EXIT_CODE = 1
PI_SESSION_MISS_TEXT = "No session found matching '{session}'"
# A refused lookup leaves the runner either completed or failed; collection decides.
PI_SESSION_MISS_RUNNER_RESULTS = frozenset({"Success", "Failed"})


@dataclass(frozen=True)
class LaunchScopes:
    """Immutable per-case launch markers; only their owner creates the directories."""
    ambient: Mapping[str, str]
    tmpdirs: Mapping[str, Path]
    # (control, empty) Pi session directories, present only for the Pi control.
    pi_sessions: tuple[Path, Path] | None

    def environment(self, scope: str, *, pi_session: Path | None = None) -> dict[str, str]:
        """Return a new full replacement that changes only owned markers."""
        require(scope in self.tmpdirs, f"unknown native launch scope: {scope}")
        require(pi_session is None or (self.pi_sessions is not None and pi_session in self.pi_sessions),
                "Pi session directory is not owned by this case")
        environment = dict(self.ambient)
        environment["TMPDIR"] = str(self.tmpdirs[scope])
        if pi_session is not None:
            environment[PI_SESSION_DIRECTORY_KEY] = str(pi_session)
        return environment


def create_launch_scopes(ownership: DirectoryOwner, base: Path, ambient: Mapping[str, str], *,
                         pi_sessions: bool) -> LaunchScopes:
    """Create every owned marker under the case base, outside workspace and state."""
    tmpdirs = {scope: ownership.create(f"{scope} launch TMPDIR", base / name, base).directory
               for scope, name in TMPDIR_MARKERS.items()}
    sessions = None
    if pi_sessions:
        sessions = (ownership.create("Pi control session directory", base / "s1", base).directory,
                    ownership.create("Pi empty session directory", base / "s2", base).directory)
    return LaunchScopes(MappingProxyType(dict(ambient)), MappingProxyType(tmpdirs), sessions)


def _marker_forms(marker: Path) -> frozenset[str]:
    return frozenset({str(marker), os.path.realpath(marker)})


def _mentions(environment: Mapping[str, str], marker: Path) -> bool:
    return any(form in value for value in environment.values() for form in _marker_forms(marker))


def _under(root: str, marker: Path) -> bool:
    return any(root == form or root.startswith(form + os.sep) for form in _marker_forms(marker))


def _recorded_launch(state: Path, task_id: str) -> tuple[dict[str, str], tuple[str, ...], dict[str, object]]:
    """Read one task's recorded launch environment and writable roots."""
    meta = read_json(state / "tasks" / task_id / "meta.json")
    require(isinstance(meta, dict) and meta.get("environment_recorded") is True,
            f"task {task_id} has no recorded launch environment")
    entries = meta.get("environment")
    require(isinstance(entries, list) and all(isinstance(entry, str) and "=" in entry for entry in entries),
            f"task {task_id} recorded launch environment is malformed")
    environment: dict[str, str] = {}
    for entry in entries:
        key, value = entry.split("=", 1)
        require(key not in environment, f"task {task_id} recorded a duplicate launch entry")
        environment[key] = value
    effective = meta.get("effective_config")
    policy = effective.get("policy") if isinstance(effective, dict) else None
    roots = policy.get("writable_roots", []) if isinstance(policy, dict) else []
    require(isinstance(roots, list) and all(isinstance(root, str) for root in roots),
            f"task {task_id} recorded writable roots are malformed")
    return environment, tuple(roots), meta


def _require_inherited_selection(state: Path, predecessor: str, successor: str,
                                 first_meta: dict[str, object], second_meta: dict[str, object]) -> None:
    first = read_json(state / "tasks" / predecessor / "task.json")
    second = read_json(state / "tasks" / successor / "task.json")
    first_request = first.get("requested_config") if isinstance(first, dict) else None
    second_request = second.get("requested_config") if isinstance(second, dict) else None
    require(isinstance(first_request, dict) and isinstance(second_request, dict) and
            all(first_request.get(key) == second_request.get(key) for key in ("model", "effort")),
            "continuation changed its predecessor's model or effort")
    executable = first_meta.get("provider_executable")
    require(isinstance(executable, str) and bool(executable) and
            second_meta.get("provider_executable") == executable,
            "continuation changed its predecessor's provider executable")


def require_continuation_scope(owner, predecessor: str, successor: str) -> dict[str, object]:
    """Prove each turn recorded its own admission TMPDIR and the successor inherited selection."""
    markers = owner.launch_scopes.tmpdirs
    first_environment, first_roots, first_meta = _recorded_launch(owner.state, predecessor)
    second_environment, second_roots, second_meta = _recorded_launch(owner.state, successor)
    require(first_environment.get("TMPDIR") == str(markers["predecessor"]),
            "predecessor did not record its own launch TMPDIR")
    require(second_environment.get("TMPDIR") == str(markers["successor"]),
            "continuation did not record its caller's launch TMPDIR")
    for environment, roots, own, foreign in (
            (first_environment, first_roots, "predecessor", "successor"),
            (second_environment, second_roots, "successor", "predecessor")):
        require(not _mentions(environment, markers[foreign]),
                "a launch environment recorded another scope's TMPDIR")
        require(not any(_under(root, markers[foreign]) for root in roots),
                "writable roots derive from another scope's TMPDIR")
        # Every supported native profile grants its caller's TMPDIR as a writable root.
        require(any(_under(root, markers[own]) for root in roots),
                "a launch did not derive its writable roots from its own TMPDIR")
    _require_inherited_selection(owner.state, predecessor, successor, first_meta, second_meta)
    return {
        "tmpdir_scopes": {"fresh": "predecessor", "resume": "successor", "replay": "ambient"},
        "writable_roots": "own-tmpdir",
        "selection_inherited": True,
        "meta_sha256": {"fresh": digest(owner.state / "tasks" / predecessor / "meta.json"),
                        "resume": digest(owner.state / "tasks" / successor / "meta.json")},
    }


def admit_successor(owner, name: str, brief: Path, predecessor: str,
                    environment: Mapping[str, str]) -> str:
    """Admit the successor through the case's one selected public command."""
    if owner.continuation_command == "continue":
        return owner.continue_public(name, brief, predecessor, environment=environment)
    return owner.continue_task(name, brief, predecessor, environment=environment)


def require_session_directory(owner, task_id: str, directory: Path) -> None:
    """Prove one Pi launch recorded and was granted the exact owned session-directory selector."""
    environment, roots, _ = _recorded_launch(owner.state, task_id)
    forms = _marker_forms(directory)
    require(environment.get(PI_SESSION_DIRECTORY_KEY) in forms,
            "Pi launch did not record its owned session directory")
    require(any(root in forms for root in roots),
            "Pi launch was not granted its owned session directory")


def _sealed_stderr(directory: Path, seal: dict[str, object]) -> str:
    entries = seal.get("raw_manifest")
    entry = next((item for item in entries if isinstance(item, dict) and item.get("path") == "raw/stderr"),
                 None) if isinstance(entries, list) else None
    path = directory / "raw" / "stderr"
    require(isinstance(entry, dict) and type(entry.get("size")) is int and
            0 <= entry["size"] <= MAX_RAW_BYTES and path.is_file() and not path.is_symlink() and
            path.stat().st_size == entry["size"] and digest(path) == entry.get("sha256"),
            "Pi session miss has no sealed bounded stderr")
    return path.read_bytes().decode("utf-8", errors="replace")


def require_empty_session_directory(directory: Path) -> None:
    """Observe the owned empty Pi session directory as a real directory with no entries.

    Any entry, including a child symlink, fails without following it or reporting its name.
    """
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    try:
        descriptor = os.open(directory, flags)
        try:
            with os.scandir(descriptor) as entries:
                empty = next(entries, None) is None
        finally:
            os.close(descriptor)
    except OSError as error:
        raise AcceptanceFailure("Pi empty session directory could not be observed as an owned "
                                "directory; recreate the case") from error
    require(empty, "Pi session miss left an entry in its empty session directory")


def require_session_miss(owner, task_id: str, predecessor: str, session: str,
                         empty_directory: Path) -> dict[str, object]:
    """Accept only Pi's own sealed session-not-found refusal for the exact predecessor session."""
    row = owner.wait_done("session-miss", task_id, expected_result=PI_SESSION_MISS_RUNNER_RESULTS)
    runner_result = done_result(row)
    require(runner_result in PI_SESSION_MISS_RUNNER_RESULTS, "Pi session miss has no accepted runner result")
    owner.ops.expect_terminal_result("session-miss", runner_result)
    directory = owner.state / "tasks" / task_id
    owner.ops.root_id = owner.root_id
    response = owner.ops.collect("session-miss-collect", task_id, expected={0, 4})
    if authentication_unavailable(directory):
        raise BlockedFailure("pi:json authentication is unavailable", reason=BlockReason.AUTH_UNAVAILABLE)
    try:
        outcome, _ = verify_collected_outcome(response, directory, "rejected")
    except AcceptanceFailure as error:
        raise AcceptanceFailure("Pi continuation under an empty session directory was not rejected") from error
    # Binds the request and records the launched runtime the success receipt must cover.
    _, record, _, _, _, _ = owner._request_binding(task_id, require_launch_evidence=True)
    prior = record.get("prior_session")
    require(isinstance(prior, dict) and prior.get("predecessor_task_id") == predecessor and
            prior.get("conversation_id") == session,
            "Pi session miss did not target the exact predecessor session")
    seal = read_json(directory / "provider.exit")
    require(isinstance(seal, dict) and seal.get("root_id") == owner.root_id and
            seal.get("task_id") == task_id and seal.get("invocation_state") == "started" and
            seal.get("exit_code") == PI_SESSION_MISS_EXIT_CODE and
            outcome.get("evidence_sha256") == seal.get("manifest_sha256"),
            "Pi session miss is not a sealed native exit")
    require(PI_SESSION_MISS_TEXT.format(session=session) in _sealed_stderr(directory, seal),
            "Pi continuation failed without its native session-not-found refusal")
    reference = directory / "provider.ref.json"
    if reference.exists() or reference.is_symlink():
        value = read_json(reference)
        require(isinstance(value, dict) and value.get("conversation_id") == session,
                "Pi session miss substituted another provider session")
    require_session_directory(owner, task_id, empty_directory)
    require_empty_session_directory(empty_directory)
    return {
        "task_id": task_id, "runner_result": runner_result, "publication": "rejected",
        "provider_exit_code": PI_SESSION_MISS_EXIT_CODE, "session_lookup": "not-found",
        "fresh_session_created": False, "outcome_sha256": digest(directory / "outcome.json"),
    }
