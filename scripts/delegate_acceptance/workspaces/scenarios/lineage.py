"""Managed-workspace lineage: a sibling child of a managed checkout, two turns, one cumulative capture."""

from __future__ import annotations

from collections.abc import Generator, Mapping, Sequence
from contextlib import contextmanager
from dataclasses import asdict, dataclass
import os
from pathlib import Path
import shlex

import scripts.delegate_acceptance.private_cli.leases as private_cli_leases
from scripts.delegate_acceptance.directories.policy import require
from scripts.delegate_acceptance.evidence.files import read_json
from .context import WorkspaceContext
from .lifecycle import SuccessfulTaskEvidence, _run_first_success
from .model import (
    CaptureEvidence, WorkspaceTask,
    CAPACITY_TASK_BUDGET, FIXTURE_FILE, OID_RE, SUCCESS_TASK_BUDGET, TASK_ID_RE, WAIT_SECONDS,
)
from .operations import (
    _adopt_workspace_container, _await_task, _collect_task, _control, _dispatch,
    _prove_lifetimes_closed, _provider_session, _wait_capture, _wait_workspace, _workspace_from,
)
from .policy import _validate_hex, classify_task_verdict
from .source import _git_executable, _git_ok
from .workspace_operations import (
    _capture_roundtrip, _forget_workspace, _release_and_forget, _verify_ref, _workspace_exec,
    verify_capture_receipt, verify_task_turn,
)

GATED_REUSE_BRIEF = "DELEGATE_WORKSPACE_REUSE_FIXTURE\nDELEGATE_PRIVATE_UPGRADE_WAIT\n"
REUSED_FILE = "managed-workspace-reused.txt"
TRACKED_FILE = "tracked.txt"
INTERMEDIATE_FILE = "lineage-intermediate.txt"
UNTRACKED_FILE = "lineage-final-untracked.txt"
FINAL_TREE = {
    FIXTURE_FILE: b"fixture workspace edit\n",
    REUSED_FILE: b"fixture workspace reused edit\n",
    TRACKED_FILE: b"managed workspace source\nlineage-final-dirty\n",
    INTERMEDIATE_FILE: b"lineage-intermediate\n",
    UNTRACKED_FILE: b"lineage-final-untracked\n",
}


@dataclass(frozen=True)
class CheckoutFacts:
    """Public workspace JSON that identifies one managed checkout and its base pin."""

    workspace_id: str
    path: str
    base_oid: str
    base_ref: str


@dataclass(frozen=True)
class TurnEvidence:
    task: WorkspaceTask
    workspace: dict[str, object]
    capture: CaptureEvidence
    session: dict[str, object]
    turn: dict[str, object]


def checkout_facts(workspace: dict[str, object]) -> CheckoutFacts:
    path = workspace.get("path")
    base_ref = workspace.get("base_ref")
    if not (isinstance(path, str) and os.path.isabs(path)):
        raise RuntimeError("workspace omitted its checkout path")
    if not (isinstance(base_ref, str) and base_ref.startswith("refs/delegate/")):
        raise RuntimeError("workspace omitted its owned base pin")
    return CheckoutFacts(
        workspace_id=_validate_hex(workspace.get("id"), TASK_ID_RE, "workspace ID"),
        path=path,
        base_oid=_validate_hex(workspace.get("base_oid"), OID_RE, "workspace base OID"),
        base_ref=base_ref,
    )


def require_sibling_checkouts(
    parent: CheckoutFacts, child: CheckoutFacts, container: Path,
) -> None:
    """The child is a distinct managed checkout with its own pin, never nested in the parent."""
    require(parent.workspace_id != child.workspace_id, "child reused the parent workspace identity")
    require(parent.base_ref != child.base_ref, "child shares the parent's base pin")
    require(parent.base_oid == child.base_oid, "child was pinned to a different base commit")
    for facts in (parent, child):
        require(Path(facts.path).is_relative_to(container),
                "managed checkout is outside the root container: " + facts.workspace_id)
    require(not Path(child.path).is_relative_to(parent.path)
            and not Path(parent.path).is_relative_to(child.path),
            "parent and child checkouts are nested")


def commit_and_dirty_script(git: str) -> str:
    """Commit one intermediate file, then leave tracked and untracked edits uncommitted."""
    executable = shlex.quote(git)
    identity = "-c user.name=Lineage -c user.email=lineage@example.invalid -c commit.gpgsign=false"
    return "\n".join((
        "set -eu",
        "printf '%s\\n' lineage-intermediate > " + INTERMEDIATE_FILE,
        executable + " add --all",
        executable + " " + identity + " commit --quiet -m 'lineage intermediate'",
        "printf '%s\\n' lineage-final-dirty >> " + TRACKED_FILE,
        "printf '%s\\n' lineage-final-untracked > " + UNTRACKED_FILE,
    ))


def verify_task_observation(
    state: Path, response: dict[str, object], expected_capture: Mapping[str, object],
    *, expected_state: str | None = None,
) -> dict[str, object]:
    """A task observation names its own turn's capture and never overstates current state."""
    workspace = _workspace_from(response)
    current = workspace.get("state")
    if expected_state is not None:
        require(current == expected_state,
                "task observation reports workspace " + repr(current) + ", not " + expected_state)
    continuation = response.get("continuation")
    if isinstance(continuation, dict) and continuation.get("resumable") is True:
        require(current == "retained",
                "task observation advertises continuation while workspace is " + repr(current))
    capture = workspace.get("capture")
    if not (isinstance(capture, dict)):
        raise RuntimeError("task observation omitted its workspace capture")
    observed = verify_capture_receipt(state, capture)
    require(observed == expected_capture,
            "task observation points to capture " + str(observed["operation_id"])
            + " instead of its own " + str(expected_capture["operation_id"]))
    return {"workspace_state": current, "operation_id": observed["operation_id"],
            "tree_oid": observed["tree_oid"], "snapshot_ref": observed["snapshot_ref"],
            "artifacts": observed["artifacts"]}


@contextmanager
def _stage(record: dict[str, object], name: str) -> Generator[None]:
    """Name the failing stage in the receipt; only a finished stage is recorded as completed."""
    record["current_stage"] = name
    yield
    completed = record["completed_stages"]
    if not (isinstance(completed, list)):
        raise RuntimeError("lineage record lost its stage list")
    completed.append(name)


def _settle_turn(
    context: WorkspaceContext, task: WorkspaceTask, *, label: str,
    expected_files: frozenset[str], forbidden_files: frozenset[str] = frozenset(),
) -> TurnEvidence:
    status = _await_task(context, task, WAIT_SECONDS)
    workspace, capture = _wait_capture(context, task)
    outcome = read_json(context.state / "tasks" / task["task_id"] / "outcome.json")
    closure = _prove_lifetimes_closed(context, [task["task_id"]])
    classification = classify_task_verdict(
        status, outcome, "success", lifetime_closed=bool(closure),
        capture_status=capture.get("status"),
    )
    require(classification["verdict"] == "passed", str(classification))
    session = _provider_session(context, task["task_id"])
    verified = verify_task_turn(
        context, capture, task_id=task["task_id"],
        expected_files=expected_files, forbidden_files=forbidden_files,
    )
    evidence = _capture_roundtrip(context, capture, label=label)
    return TurnEvidence(task=task, workspace=workspace, capture=evidence, session=session,
                        turn=verified["turn"])


def _observe_task(
    context: WorkspaceContext, task: WorkspaceTask, expected_capture: Mapping[str, object],
    *, label: str, expected_state: str | None = None,
) -> dict[str, object]:
    _code, status = _control(
        context, ["--root", str(context.state), "status", task["task_id"], "--json"],
        label=label + "-status", task_id=task["task_id"], expected=(0, 3),
        phase="task-observation",
    )
    collected = _collect_task(context, task, WAIT_SECONDS)
    return {
        "status": verify_task_observation(
            context.state, status, expected_capture, expected_state=expected_state),
        "collect": verify_task_observation(
            context.state, collected, expected_capture, expected_state=expected_state),
    }


def _capture_parent(
    context: WorkspaceContext, record: dict[str, object],
) -> tuple[SuccessfulTaskEvidence, CheckoutFacts]:
    parent = _run_first_success(context)
    facts = checkout_facts(_workspace_from(parent.workspace))
    require(facts.base_oid == context.source_baseline["head"],
            "parent was pinned to a different base commit than the source HEAD")
    require(Path(facts.path).is_dir(), "parent checkout directory is absent")
    record["parent"] = {"task_id": parent.task["task_id"], "checkout": asdict(facts),
                        "capture": parent.capture_evidence}
    return parent, facts


def _run_child_first_turn(
    context: WorkspaceContext, record: dict[str, object], parent: CheckoutFacts,
) -> tuple[TurnEvidence, CheckoutFacts]:
    task = _dispatch(
        context, context.brief_success, task_budget=SUCCESS_TASK_BUDGET, fresh=True,
        source=parent.path, base=parent.base_oid,
    )
    _adopt_workspace_container(context, task["root_id"], task["workspace_id"])
    turn = _settle_turn(context, task, label="lineage-child-first-capture",
                        expected_files=frozenset({FIXTURE_FILE}))
    facts = checkout_facts(_workspace_from(turn.workspace))
    container = context.container_token.directory if context.container_token else None
    if container is None:
        raise RuntimeError("managed root container was not adopted")
    require_sibling_checkouts(parent, facts, container)
    require(_verify_ref(context, facts.base_ref, "lineage-child-base-ref", (0,)) == 0,
            "child base pin does not resolve")
    record["child"] = {"checkout": asdict(facts), "first_turn": {**turn.capture, "turn": turn.turn},
                       "first_task_id": task["task_id"], "session": turn.session}
    return turn, facts


def _start_held_turn(
    context: WorkspaceContext, child: CheckoutFacts,
) -> WorkspaceTask:
    brief = context.base / "brief-lineage-gated-reuse.md"
    brief.write_text(GATED_REUSE_BRIEF, encoding="utf-8")
    brief.chmod(0o600)
    context.gates.append(private_cli_leases.LockedReleaseGate.acquire(context.gate_path))
    task = _dispatch(
        context, brief, task_budget=CAPACITY_TASK_BUDGET, fresh=False,
        workspace_id=child.workspace_id,
    )
    _adopt_workspace_container(context, task["root_id"], task["workspace_id"])
    busy = _wait_workspace(context, child.workspace_id, state="busy", task_id=task["task_id"])
    require(_workspace_from(busy).get("path") == child.path, "busy child changed its checkout path")
    return task


def _release_parent(
    context: WorkspaceContext, parent: SuccessfulTaskEvidence, parent_facts: CheckoutFacts,
    child: CheckoutFacts,
) -> dict[str, object]:
    """Release then forget the parent through the workspace API while the child stays busy."""
    snapshot = str(parent.capture_evidence["snapshot_ref"])
    release = _release_and_forget(context, parent.task)
    require(not os.path.lexists(parent_facts.path), "released parent checkout directory remains")
    require(Path(child.path).is_dir(), "releasing the parent removed the child checkout")
    for reference in (snapshot, parent_facts.base_ref, child.base_ref):
        _verify_ref(context, reference, "lineage-after-parent-release", (0,))
    forget = _forget_workspace(context, parent.task)
    for reference in (snapshot, parent_facts.base_ref):
        require(_verify_ref(context, reference, "lineage-after-parent-forget", (1,)) == 1,
                "parent forget left an owned ref")
    require(_verify_ref(context, child.base_ref, "lineage-child-base-after-parent", (0,)) == 0,
            "parent forget removed the child's base pin")
    return {"release": release, "forget": forget}


def _run_second_turn(
    context: WorkspaceContext, record: dict[str, object], parent: SuccessfulTaskEvidence,
    parent_facts: CheckoutFacts, first: TurnEvidence, child: CheckoutFacts,
) -> TurnEvidence:
    with _stage(record, "second-turn-held"):
        second_task = _start_held_turn(context, child)
    with _stage(record, "busy-window"):
        busy = _observe_task(
            context, first.task, first.capture, label="lineage-first-busy", expected_state="busy",
        )
        released = _release_parent(context, parent, parent_facts, child)
        context.release_gates()
        record["busy_window"] = {"first_task_observation": busy, "parent_release": released}
    with _stage(record, "second-turn-settled"):
        second = _settle_turn(
            context, second_task, label="lineage-child-second-capture",
            expected_files=frozenset({REUSED_FILE}), forbidden_files=frozenset({FIXTURE_FILE}),
        )
        private_cli_leases.require_empty_fixture_waiters(context.waiter_directory)
        require(second.capture["operation_id"] != first.capture["operation_id"]
                and second.capture["base_oid"] == child.base_oid,
                "second turn did not produce a distinct capture over the same base")
        latest = _observe_task(context, first.task, first.capture, label="lineage-first-after")
        record["second_turn"] = {"task_id": second_task["task_id"],
                                 "capture": {**second.capture, "turn": second.turn},
                                 "session": second.session, "first_task_observation": latest}
    return second


def _verify_snapshot_tree(context: WorkspaceContext, snapshot: str) -> None:
    for name, expected in FINAL_TREE.items():
        content = _git_ok(
            context, ["-C", str(context.source), "cat-file", "blob", snapshot + ":" + name],
            context.source, label="lineage-final-blob",
        )
        require(content == expected, "cumulative snapshot has wrong content for " + name)


def _commit_and_capture(
    context: WorkspaceContext, record: dict[str, object], first: TurnEvidence,
    second: TurnEvidence, child: CheckoutFacts,
) -> CaptureEvidence:
    script = commit_and_dirty_script(str(_git_executable()))
    executed = _workspace_exec(
        context, second.task, command=("/bin/sh", "-c", script), label="workspace-exec-lineage",
    )
    operation_id = str(executed["operation_id"])
    _workspace, capture = _wait_capture(context, second.task, subject_id=operation_id)
    final = _capture_roundtrip(context, capture, label="lineage-final-capture-roundtrip")
    head = _git_ok(context, ["-C", child.path, "rev-parse", "--verify", "HEAD^{commit}"],
                   context.source, label="lineage-worker-head").decode("ascii").strip()
    parent_head = _git_ok(context, ["-C", child.path, "rev-parse", "--verify", "HEAD^"],
                          context.source, label="lineage-worker-parent").decode("ascii").strip()
    require(final["base_oid"] == child.base_oid and parent_head == child.base_oid,
            "cumulative capture is not relative to the original base")
    require(final["observed_head"] == head and head != child.base_oid,
            "capture did not observe the worker commit")
    require(final["tree_oid"] != second.capture["tree_oid"], "final edits did not change the tree")
    _verify_snapshot_tree(context, str(final["snapshot_ref"]))
    record["final_capture"] = {"exec": executed, "capture": final,
                               "worker_head": head}
    for turn in (first, second):
        record["post_exec_" + str(turn.task["task_id"])] = _observe_task(
            context, turn.task, turn.capture, label="lineage-post-exec",
        )
    return final


def _finish_child(
    context: WorkspaceContext, record: dict[str, object], task_ids: list[str],
    child: CheckoutFacts, captures: Sequence[Mapping[str, object]],
) -> None:
    record["lifetime_closure"] = _prove_lifetimes_closed(context, task_ids)
    snapshots = [str(capture["snapshot_ref"]) for capture in captures]
    for reference in (*snapshots, child.base_ref):
        _verify_ref(context, reference, "lineage-before-child-release", (0,))
    handle = {"workspace_id": child.workspace_id, "task_id": task_ids[-1]}
    release = _release_and_forget(context, handle)
    require(not os.path.lexists(child.path), "released child checkout directory remains")
    for reference in (*snapshots, child.base_ref):
        _verify_ref(context, reference, "lineage-after-child-release", (0,))
    forget = _forget_workspace(context, handle)
    for reference in (*snapshots, child.base_ref):
        require(_verify_ref(context, reference, "lineage-after-child-forget", (1,)) == 1,
                "child forget left an owned ref")
    record["child_release"] = {"release": release, "forget": forget}


def _run_lineage(context: WorkspaceContext, record: dict[str, object]) -> None:
    """Fill `record` stage by stage so a failed run still names its last completed stage."""
    record.update({"scenario": "lineage", "completed_stages": [], "current_stage": None})
    with _stage(record, "parent-captured"):
        parent, parent_facts = _capture_parent(context, record)
    with _stage(record, "child-first-turn"):
        first, child = _run_child_first_turn(context, record, parent_facts)
    second = _run_second_turn(context, record, parent, parent_facts, first, child)
    with _stage(record, "commit-and-dirty-capture"):
        final = _commit_and_capture(context, record, first, second, child)
    with _stage(record, "child-released"):
        task_ids = [parent.task["task_id"], first.task["task_id"], second.task["task_id"]]
        _finish_child(context, record, task_ids, child,
                      [first.capture, second.capture, final])
    record["current_stage"] = None
