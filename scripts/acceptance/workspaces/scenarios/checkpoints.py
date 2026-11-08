"""Checkpoint scenario: ordinary and managed checkpoints, forks, paged metadata and exact removal.

Every command runs through the shipped CLI and the shared control, Git process,
dispatch and closure owners. Creation and removal results are acknowledgements;
a qualified status observation decides usability and removal. Accepted
mutations are never repeated; only a refusal, which acknowledges nothing, is
retried within its bound.
"""

from __future__ import annotations

from dataclasses import asdict
import hashlib
import os
from pathlib import Path
import time
from typing import Mapping, Sequence

import scripts.acceptance.private_cli.leases as private_cli_leases
from scripts.acceptance.directories.policy import require
from scripts.acceptance.evidence.files import read_json
from scripts.acceptance.workspaces.checkpoint_records import CHECKPOINT_CANDIDATE_RECORD
from scripts.acceptance.workspaces.closure import validate_managed_workspace_operation_closure
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import Acknowledgement
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import CheckpointFacts
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import classify_creation
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import classify_removal
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import classify_removal_status
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import classify_status
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import closure_facts
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import list_page
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import qualified_checkpoint
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import ref_observation
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import removed_history
from scripts.acceptance.workspaces.scenarios.checkpoint_policy import unchanged_checkout
from scripts.acceptance.workspaces.scenarios.context import WorkspaceContext
from scripts.acceptance.workspaces.scenarios.lifecycle import SuccessfulTaskEvidence
from scripts.acceptance.workspaces.scenarios.lifecycle import _run_first_success
from scripts.acceptance.workspaces.scenarios.lineage import REUSED_FILE
from scripts.acceptance.workspaces.scenarios.lineage import CheckoutFacts
from scripts.acceptance.workspaces.scenarios.lineage import _settle_turn
from scripts.acceptance.workspaces.scenarios.lineage import _stage
from scripts.acceptance.workspaces.scenarios.lineage import _start_held_turn
from scripts.acceptance.workspaces.scenarios.lineage import checkout_facts
from scripts.acceptance.workspaces.scenarios.model import ADMISSION_COMMAND_TIMEOUT_SECONDS
from scripts.acceptance.workspaces.scenarios.model import CaptureEvidence
from scripts.acceptance.workspaces.scenarios.model import CheckoutSnapshot
from scripts.acceptance.workspaces.scenarios.model import WorkspaceTask
from scripts.acceptance.workspaces.scenarios.model import FIXTURE_FILE
from scripts.acceptance.workspaces.scenarios.model import MAX_RESPONSE_BYTES
from scripts.acceptance.workspaces.scenarios.model import SUCCESS_TASK_BUDGET
from scripts.acceptance.workspaces.scenarios.model import TASK_ID_RE
from scripts.acceptance.workspaces.scenarios.model import WAIT_SECONDS
from scripts.acceptance.workspaces.scenarios.model import WORKSPACE_OPERATION_BUDGET
from scripts.acceptance.workspaces.scenarios.operations import _adopt_workspace_container
from scripts.acceptance.workspaces.scenarios.operations import _assert_immutable_task_snapshot
from scripts.acceptance.workspaces.scenarios.operations import _await_task
from scripts.acceptance.workspaces.scenarios.operations import _collect_task
from scripts.acceptance.workspaces.scenarios.operations import _control
from scripts.acceptance.workspaces.scenarios.operations import _dispatch
from scripts.acceptance.workspaces.scenarios.operations import _immutable_task_snapshot
from scripts.acceptance.workspaces.scenarios.operations import _prove_lifetimes_closed
from scripts.acceptance.workspaces.scenarios.operations import _wait_capture
from scripts.acceptance.workspaces.scenarios.operations import _workspace_from
from scripts.acceptance.workspaces.scenarios.policy import _validate_hex
from scripts.acceptance.workspaces.scenarios.policy import classify_task_verdict
from scripts.acceptance.workspaces.scenarios.source import _git_executable
from scripts.acceptance.workspaces.scenarios.source import _git_ok
from scripts.acceptance.workspaces.scenarios.source import _git_process
from scripts.acceptance.workspaces.scenarios.workspace_operations import _forget_workspace
from scripts.acceptance.workspaces.scenarios.workspace_operations import _release_and_forget
from scripts.acceptance.workspaces.scenarios.workspace_operations import verify_capture_receipt

STAGED_FILE = "checkpoint-staged.txt"
UNTRACKED_FILE = "checkpoint-untracked.txt"
# The timeout-resume fixture marker writes the only fixture file absent from the managed checkpoint.
RESUMED_FILE = "managed-workspace-timeout-resumed.txt"
BASE_README = b"managed workspace acceptance source\n"
BASE_TRACKED = b"managed workspace source\n"
DIRTY_TRACKED = BASE_TRACKED + b"checkpoint-dirty-tracked\n"
FIXTURE_EDIT = b"fixture workspace edit\n"
# Full content trees: the checkpoint commit holds the whole worktree, not a diff.
ORDINARY_TREE = {
    "tracked.txt": DIRTY_TRACKED,
    STAGED_FILE: b"checkpoint-staged\n",
    UNTRACKED_FILE: b"checkpoint-untracked\n",
}
MANAGED_TREE = {
    "README.md": BASE_README,
    "tracked.txt": BASE_TRACKED,
    FIXTURE_FILE: FIXTURE_EDIT,
    REUSED_FILE: b"fixture workspace reused edit\n",
}
OBSERVED_FILES = ("README.md", *ORDINARY_TREE, FIXTURE_FILE, REUSED_FILE)
LIST_PAGE_LIMIT = 1
MAX_LIST_PAGES = 8
REFUSAL_RETRY_SECONDS = 0.5
COLLECT_WATCH_SECONDS = 5


def _root_id(context: WorkspaceContext) -> str:
    """The state root's authoritative identity; ordinary creation may be what initialized it."""
    return _validate_hex(read_json(context.state / "root.json").get("root_id"), TASK_ID_RE, "state root ID")


def _checkpoint_command(
    context: WorkspaceContext, record: dict[str, object], arguments: list[str], *, label: str,
) -> tuple[int, dict[str, object]]:
    """Run one checkpoint command and keep its exact argv and exit in the compact record."""
    args = ["--root", str(context.state), "checkpoint", *arguments, "--json"]
    # Exit 1 is decoded too so the oracle reports the whole fault and retained IDs.
    code, response = _control(
        context, args, label=label, expected=(0, 1, 3),
        timeout=ADMISSION_COMMAND_TIMEOUT_SECONDS, phase=label,
    )
    commands = record["commands"]
    if not (isinstance(commands, list)):
        raise RuntimeError("checkpoint record lost its command log")
    commands.append({"label": label, "arguments": args, "exit_code": code})
    return code, response


def _watch(remaining: float) -> str:
    return str(min(5.0, remaining)) + "s"


def _create_once(
    context: WorkspaceContext, record: dict[str, object], target: list[str], *, label: str,
) -> Acknowledgement:
    code, response = _checkpoint_command(
        context, record, ["create", *target, "--budget", WORKSPACE_OPERATION_BUDGET], label=label)
    return classify_creation(code, response, _root_id(context))


def _accepted_creation(
    context: WorkspaceContext, record: dict[str, object], target: list[str], *, label: str,
) -> Acknowledgement:
    """Retry only an unacknowledged refusal; an accepted creation is returned and never repeated."""
    deadline = time.monotonic() + WAIT_SECONDS
    while True:
        acknowledgement = _create_once(context, record, target, label=label)
        if acknowledgement.accepted:
            return acknowledgement
        require(time.monotonic() + REFUSAL_RETRY_SECONDS < deadline, "checkpoint creation stayed refused")
        time.sleep(REFUSAL_RETRY_SECONDS)


def _accepted_removal(
    context: WorkspaceContext, record: dict[str, object], checkpoint_id: str, *, label: str,
) -> Acknowledgement:
    deadline = time.monotonic() + WAIT_SECONDS
    while True:
        code, response = _checkpoint_command(
            context, record, ["remove", checkpoint_id, "--budget", WORKSPACE_OPERATION_BUDGET], label=label)
        acknowledgement = classify_removal(code, response, _root_id(context), checkpoint_id)
        if acknowledgement.accepted:
            return acknowledgement
        require(time.monotonic() + REFUSAL_RETRY_SECONDS < deadline, "checkpoint removal stayed refused")
        time.sleep(REFUSAL_RETRY_SECONDS)


def _await_status(
    context: WorkspaceContext, record: dict[str, object], checkpoint_id: str, *, label: str,
) -> dict[str, object]:
    """Repeat bounded plain status observations until creation and latest removal settle."""
    deadline = time.monotonic() + WAIT_SECONDS
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("checkpoint observation expired: " + checkpoint_id)
        code, response = _checkpoint_command(
            context, record, ["status", checkpoint_id, "--watch", _watch(remaining)], label=label)
        current = classify_status(code, response, _root_id(context), checkpoint_id)
        if current is not None:
            return current


def _await_removal(
    context: WorkspaceContext, record: dict[str, object], acknowledgement: Acknowledgement, *, label: str,
) -> dict[str, object]:
    """Observe exactly the acknowledged attempt R; a newer head never answers for it."""
    deadline = time.monotonic() + WAIT_SECONDS
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("checkpoint removal observation expired: " + acknowledgement.operation_id)
        code, response = _checkpoint_command(
            context, record,
            ["status", acknowledgement.checkpoint_id, "--operation", acknowledgement.operation_id,
             "--watch", _watch(remaining)],
            label=label,
        )
        operation = classify_removal_status(
            code, response, _root_id(context), checkpoint_id=acknowledgement.checkpoint_id,
            operation_id=acknowledgement.operation_id)
        if operation is not None:
            return operation


def _list_all(context: WorkspaceContext, record: dict[str, object], *, label: str) -> list[dict[str, object]]:
    """Traverse every metadata page with the smallest page size so the cursor is exercised."""
    entries: list[dict[str, object]] = []
    after = ""
    for _page in range(MAX_LIST_PAGES):
        arguments = ["list", "--limit", str(LIST_PAGE_LIMIT), *(["--after", after] if after else [])]
        code, response = _checkpoint_command(context, record, arguments, label=label)
        require(code == 0, "checkpoint list did not succeed: exit " + str(code))
        page, after = list_page(response, _root_id(context), limit=LIST_PAGE_LIMIT, after=after)
        entries.extend(page)
        if not after:
            return entries
    raise RuntimeError("checkpoint list exceeded its page bound")


def _owned_ref(context: WorkspaceContext, repository: Path, reference: str, *, label: str) -> str | None:
    """Return the commit an owned ref names, or None after inventory, symbolic and resolution all agree."""
    def git(arguments: list[str], suffix: str, expected: tuple[int, ...]):
        return _git_process(context, ["-C", str(repository), *arguments], repository,
                            label=label + "-" + suffix, expected=expected)

    inventory = git(["for-each-ref", "--format=%(refname) %(objectname)", "--", reference], "inventory", (0,))
    symbolic = git(["symbolic-ref", "--quiet", reference], "symbolic", (0, 1))
    resolved = git(["rev-parse", "--verify", "--quiet", reference + "^{commit}"], "resolve", (0, 1))
    return ref_observation(
        reference=reference,
        inventory=inventory.output(MAX_RESPONSE_BYTES),
        inventory_stderr=inventory.stderr_path.read_bytes(),
        symbolic_exit=symbolic.result["exit_code"],
        symbolic_stdout=symbolic.output(MAX_RESPONSE_BYTES),
        resolved_exit=resolved.result["exit_code"],
        resolved_stdout=resolved.output(MAX_RESPONSE_BYTES),
    )


def _observe_checkout(context: WorkspaceContext, checkout: Path, *, label: str) -> CheckoutSnapshot:
    """Read HEAD, the real index and worktree bytes without letting the observer write the index."""
    def git(arguments: list[str], suffix: str) -> bytes:
        return _git_ok(context, ["-C", str(checkout), *arguments], checkout, label=label + "-" + suffix)

    symbolic = _git_process(context, ["-C", str(checkout), "symbolic-ref", "--quiet", "HEAD"], checkout,
                            label=label + "-head-ref", expected=(0, 1))
    index = Path(git(["rev-parse", "--path-format=absolute", "--git-path", "index"], "index-path")
                 .decode("utf-8").strip())
    info = index.lstat()
    data = index.read_bytes()
    status = git(["--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=all"], "status")
    files = {}
    for name in OBSERVED_FILES:
        path = checkout / name
        files[name] = hashlib.sha256(path.read_bytes()).hexdigest() if path.is_file() else None
    return {
        "head": git(["rev-parse", "--verify", "HEAD^{commit}"], "head").decode("ascii").strip(),
        "head_ref": symbolic.output(MAX_RESPONSE_BYTES).decode("utf-8").strip() or None,
        "index": {"bytes": len(data), "sha256": hashlib.sha256(data).hexdigest(),
                  "device": info.st_dev, "inode": info.st_ino},
        "status": {"bytes": len(status), "sha256": hashlib.sha256(status).hexdigest()},
        "files": files,
    }


def _require_commit_content(
    context: WorkspaceContext, repository: Path, checkpoint: CheckpointFacts, *, head: str,
    tree: dict[str, bytes],
) -> dict[str, object]:
    """The published ref names a commit whose sole parent is the observed HEAD and whose tree is the worktree."""
    def git(arguments: list[str], suffix: str) -> bytes:
        return _git_ok(context, ["-C", str(repository), *arguments], repository,
                       label="checkpoint-commit-" + suffix)

    require(_owned_ref(context, repository, checkpoint.snapshot_ref, label="checkpoint-ref-present")
            == checkpoint.commit_oid, "checkpoint ref does not name the qualified commit")
    payload = git(["cat-file", "commit", checkpoint.commit_oid], "payload")
    header = payload.split(b"\n\n", 1)[0].split(b"\n")
    require([line for line in header if line.startswith(b"parent ")] == [b"parent " + head.encode()],
            "checkpoint commit parent is not the observed HEAD")
    require(b"\nDelegate-Checkpoint: " + checkpoint.checkpoint_id.encode() + b"\n" in payload,
            "checkpoint commit does not carry its checkpoint trailer")
    names = git(["ls-tree", "-r", "-z", "--name-only", checkpoint.commit_oid], "names").split(b"\x00")
    require({name.decode("utf-8") for name in names if name} == set(tree),
            "checkpoint tree does not contain exactly the worktree files")
    for name, expected in tree.items():
        require(git(["cat-file", "blob", checkpoint.commit_oid + ":" + name], "blob") == expected,
                "checkpoint tree has wrong content for " + name)
    return {"commit_sha256": hashlib.sha256(payload).hexdigest(), "files": sorted(tree)}


def _creation_journal(
    context: WorkspaceContext, checkpoint: CheckpointFacts, *, checkout: Path,
) -> dict[str, object]:
    """Bind the public description to the canonically validated creation journal and its closure."""
    directory = context.state / "workspace-operations" / checkpoint.checkpoint_id
    closure = validate_managed_workspace_operation_closure(directory, _root_id(context))
    require(closure.get("checkpoint") == {"candidate": True, "status": "checkpointed", "removals": []},
            "checkpoint journal is not one qualified creation without removals")
    candidate = read_json(directory / CHECKPOINT_CANDIDATE_RECORD)
    target = candidate.get("target")
    if not (str(candidate.get("commit_oid", "")).lower() == checkpoint.commit_oid and
            candidate.get("snapshot_ref") == checkpoint.snapshot_ref and
            isinstance(target, dict) and target.get("checkout_root") == str(checkout)):
        raise RuntimeError("checkpoint journal candidate differs from the public description")
    return {"closure": closure_facts(closure), "tree_oid": candidate.get("tree_oid")}


def _prepare_dirty_source(context: WorkspaceContext) -> Path:
    """Clone the clean fixture into an owned ordinary checkout and leave staged, unstaged and untracked edits."""
    checkout = context.owner.create(
        "checkpoint dirty ordinary source", context.base / "checkpoint-source", context.base).directory
    _git_ok(context, ["clone", "--quiet", "--no-local", str(context.source), str(checkout)],
            context.base, label="checkpoint-source-clone")
    (checkout / "tracked.txt").write_bytes(DIRTY_TRACKED)
    (checkout / "README.md").unlink()
    (checkout / STAGED_FILE).write_bytes(ORDINARY_TREE[STAGED_FILE])
    _git_ok(context, ["-C", str(checkout), "add", "--", STAGED_FILE], checkout, label="checkpoint-source-stage")
    (checkout / UNTRACKED_FILE).write_bytes(ORDINARY_TREE[UNTRACKED_FILE])
    return checkout


def _create_ordinary(
    context: WorkspaceContext, record: dict[str, object],
) -> tuple[Path, CheckpointFacts, CheckoutSnapshot]:
    checkout = _prepare_dirty_source(context)
    before = _observe_checkout(context, checkout, label="checkpoint-ordinary-before")
    require(before["head"] == context.source_baseline["head"] and before["status"]["bytes"] > 0,
            "ordinary checkpoint input is not the deliberately dirty clone of the fixture")
    acknowledgement = _accepted_creation(context, record, ["--cwd", str(checkout)],
                                         label="checkpoint-create-ordinary")
    current = _await_status(context, record, acknowledgement.checkpoint_id, label="checkpoint-status-ordinary")
    checkpoint = qualified_checkpoint(current, root_id=_root_id(context), selection="ordinary", cwd=str(checkout))
    require(checkpoint.checkpoint_id == acknowledgement.checkpoint_id, "status described another checkpoint")
    after = _observe_checkout(context, checkout, label="checkpoint-ordinary-after")
    unchanged_checkout(before, after, "ordinary checkpoint creation")
    record["ordinary"] = {
        "acknowledgement": {"checkpoint_id": acknowledgement.checkpoint_id, "settled": acknowledgement.settled},
        "checkpoint": asdict(checkpoint), "input_before": before, "input_after_creation": after,
        "commit": _require_commit_content(context, checkout, checkpoint, head=str(before["head"]),
                                          tree=ORDINARY_TREE),
        "journal": _creation_journal(context, checkpoint, checkout=checkout),
    }
    return checkout, checkpoint, before


def _settle_fork(
    context: WorkspaceContext, task: WorkspaceTask, checkpoint: CheckpointFacts,
) -> tuple[CheckoutFacts, CaptureEvidence]:
    status = _await_task(context, task, WAIT_SECONDS)
    response, capture = _wait_capture(context, task)
    outcome = read_json(context.state / "tasks" / str(task["task_id"]) / "outcome.json")
    closure = _prove_lifetimes_closed(context, [str(task["task_id"])])
    classification = classify_task_verdict(
        status, outcome, "success", lifetime_closed=bool(closure), capture_status=capture.get("status"))
    require(classification["verdict"] == "passed", str(classification))
    evidence = verify_capture_receipt(context.state, capture)
    subject = evidence["subject"]
    if not (isinstance(subject, dict) and subject.get("id") == task["task_id"] and
            evidence["base_oid"] == checkpoint.commit_oid):
        raise RuntimeError("fork capture is not its own task's capture over the checkpoint commit")
    return checkout_facts(_workspace_from(response)), evidence


def require_checkpoint_fork(
    child: CheckoutFacts, checkpoint: CheckpointFacts, *, origin_base: str, container: Path,
    parent: CheckoutFacts | None,
) -> None:
    """A fork is pinned to the checkpoint commit: its base intentionally differs from its origin's."""
    require(child.base_oid == checkpoint.commit_oid, "fork was not pinned to the checkpoint commit")
    require(child.base_oid != origin_base, "fork base did not move away from its origin base")
    require(Path(child.path).is_relative_to(container), "fork checkout is outside the root container")
    if parent is None:
        return
    require(parent.workspace_id != child.workspace_id and parent.base_ref != child.base_ref,
            "fork reused its parent workspace identity or base pin")
    require(not Path(child.path).is_relative_to(parent.path) and
            not Path(parent.path).is_relative_to(child.path), "parent and fork checkouts are nested")


def _fork(
    context: WorkspaceContext, checkpoint: CheckpointFacts, *, source: Path, brief: Path,
    present: dict[str, bytes], absent: frozenset[str], origin_base: str, parent: CheckoutFacts | None,
) -> tuple[WorkspaceTask, CheckoutFacts, dict[str, object]]:
    task = _dispatch(context, brief, task_budget=SUCCESS_TASK_BUDGET, fresh=True,
                     source=source, base=checkpoint.commit_oid)
    _adopt_workspace_container(context, task["root_id"], task["workspace_id"])
    child, capture = _settle_fork(context, task, checkpoint)
    container = context.container_token.directory if context.container_token else None
    if container is None:
        raise RuntimeError("managed root container was not adopted")
    require_checkpoint_fork(child, checkpoint, origin_base=origin_base, container=container, parent=parent)
    head = _git_ok(context, ["-C", child.path, "rev-parse", "--verify", "HEAD^{commit}"],
                   Path(child.path), label="checkpoint-fork-head").decode("ascii").strip()
    require(head == checkpoint.commit_oid, "fork checkout HEAD is not the checkpoint commit")
    for name, expected in present.items():
        require((Path(child.path) / name).read_bytes() == expected, "fork checkout lacks content of " + name)
    require(not any(os.path.lexists(Path(child.path) / name) for name in absent),
            "fork checkout contains a file absent from the checkpoint")
    return task, child, {"task_id": task["task_id"], "checkout": asdict(child), "capture": capture}


def _refuse_busy_managed(
    context: WorkspaceContext, record: dict[str, object], *, known: list[str],
) -> tuple[SuccessfulTaskEvidence, CheckoutFacts, WorkspaceTask]:
    """A busy managed target refuses creation without an ID; it is never retargeted through --cwd."""
    parent = _run_first_success(context)
    facts = checkout_facts(_workspace_from(parent.workspace))
    held = _start_held_turn(context, facts)
    refusal = _create_once(context, record, ["--workspace", facts.workspace_id], label="checkpoint-create-busy")
    require(not refusal.accepted, "busy managed workspace acknowledged a checkpoint creation")
    listed = [entry["id"] for entry in _list_all(context, record, label="checkpoint-list-after-refusal")]
    require(listed == known, "refused creation left a checkpoint in the metadata")
    context.release_gates()
    settled = _settle_turn(context, held, label="checkpoint-parent-second-capture",
                           expected_files=frozenset({REUSED_FILE}), forbidden_files=frozenset({FIXTURE_FILE}))
    private_cli_leases.require_empty_fixture_waiters(context.waiter_directory)
    record["managed_parent"] = {"checkout": asdict(facts), "first_task_id": parent.task["task_id"],
                                "held_task_id": held["task_id"], "refused_listing": listed,
                                "held_capture": settled.capture}
    return parent, facts, held


def _create_managed(
    context: WorkspaceContext, record: dict[str, object], parent: SuccessfulTaskEvidence,
    facts: CheckoutFacts, held: Mapping[str, object],
) -> CheckpointFacts:
    checkout = Path(facts.path)
    task_ids = (str(parent.task["task_id"]), str(held["task_id"]))
    snapshots = {task_id: _immutable_task_snapshot(context, task_id) for task_id in task_ids}
    before = _observe_checkout(context, checkout, label="checkpoint-managed-before")
    acknowledgement = _accepted_creation(context, record, ["--workspace", facts.workspace_id],
                                         label="checkpoint-create-managed")
    current = _await_status(context, record, acknowledgement.checkpoint_id, label="checkpoint-status-managed")
    checkpoint = qualified_checkpoint(current, root_id=_root_id(context), selection="managed",
                                      workspace_id=facts.workspace_id)
    require(checkpoint.checkpoint_id == acknowledgement.checkpoint_id, "status described another checkpoint")
    after = _observe_checkout(context, checkout, label="checkpoint-managed-after")
    unchanged_checkout(before, after, "managed checkpoint creation")
    # A committed outcome and its retained workspace are untouched by the checkpoint use.
    collected = _collect_task(context, held, COLLECT_WATCH_SECONDS)
    for task_id, snapshot in snapshots.items():
        _assert_immutable_task_snapshot(context, task_id, snapshot)
    collected_outcome = collected.get("outcome")
    if not isinstance(collected_outcome, dict):
        raise RuntimeError("collection omitted provider outcome")
    record["managed"] = {
        "acknowledgement": {"checkpoint_id": acknowledgement.checkpoint_id, "settled": acknowledgement.settled},
        "checkpoint": asdict(checkpoint), "checkout_before": before, "checkout_after_creation": after,
        "commit": _require_commit_content(context, context.source, checkpoint, head=str(before["head"]),
                                          tree=MANAGED_TREE),
        "journal": _creation_journal(context, checkpoint, checkout=checkout),
        "collect_after_checkpoint": {"publication": collected.get("publication"),
                                     "verdict": collected_outcome.get("verdict")},
    }
    return checkpoint


def _require_listing(
    context: WorkspaceContext, record: dict[str, object], *, ordinary: CheckpointFacts, ordinary_cwd: Path,
    managed: CheckpointFacts, managed_workspace: str,
) -> None:
    entries = _list_all(context, record, label="checkpoint-list-paged")
    by_id = {str(entry["id"]): entry for entry in entries}
    require([entry["id"] for entry in entries] == sorted((ordinary.checkpoint_id, managed.checkpoint_id)),
            "paged checkpoint list does not contain exactly both checkpoints")
    root_id = _root_id(context)
    require(qualified_checkpoint(by_id[ordinary.checkpoint_id], root_id=root_id, selection="ordinary",
                                 cwd=str(ordinary_cwd)) == ordinary and
            qualified_checkpoint(by_id[managed.checkpoint_id], root_id=root_id, selection="managed",
                                 workspace_id=managed_workspace) == managed,
            "listed metadata differs from the qualified status")
    record["paged_list"] = {"limit": LIST_PAGE_LIMIT, "ids": [entry["id"] for entry in entries]}


def _release_origin(
    context: WorkspaceContext, record: dict[str, object], held: Mapping[str, object], facts: CheckoutFacts,
    managed: CheckpointFacts,
) -> None:
    """The managed checkpoint's ref and usability survive its workspace's release and forget."""
    released = _release_and_forget(context, held)
    require(not os.path.lexists(facts.path), "released parent checkout directory remains")
    forgotten = _forget_workspace(context, held)
    require(_owned_ref(context, context.source, facts.base_ref, label="checkpoint-parent-base-forgotten") is None,
            "parent forget left its base pin")
    require(_owned_ref(context, context.source, managed.snapshot_ref, label="checkpoint-ref-after-forget")
            == managed.commit_oid, "checkpoint ref did not survive its workspace forget")
    current = _await_status(context, record, managed.checkpoint_id, label="checkpoint-status-after-forget")
    require(qualified_checkpoint(current, root_id=_root_id(context), selection="managed",
                                 workspace_id=facts.workspace_id) == managed,
            "checkpoint stopped being usable after its workspace forget")
    record["parent_release"] = {"release": released, "forget": forgotten}


def _remove(
    context: WorkspaceContext, record: dict[str, object], checkpoint: CheckpointFacts, *,
    repository: Path, label: str,
) -> dict[str, object]:
    """Remove explicitly, then observe that exact attempt, the history and the ref's proven absence."""
    acknowledgement = _accepted_removal(context, record, checkpoint.checkpoint_id, label=label + "-remove")
    operation = _await_removal(context, record, acknowledgement, label=label + "-removal-status")
    require(operation == {"operation_id": acknowledgement.operation_id, "attempts": 1, "state": "removed"},
            "exact removal attempt did not remove the checkpoint ref")
    current = _await_status(context, record, checkpoint.checkpoint_id, label=label + "-history")
    removed_history(current, checkpoint, acknowledgement.operation_id)
    require(_owned_ref(context, repository, checkpoint.snapshot_ref, label=label + "-ref-absent") is None,
            "removed checkpoint ref remains")
    creation = context.state / "workspace-operations" / checkpoint.checkpoint_id
    root_id = _root_id(context)
    checkpoint_closure = validate_managed_workspace_operation_closure(creation, root_id).get("checkpoint", {})
    require(isinstance(checkpoint_closure, dict) and checkpoint_closure.get("removals")
            == [{"sequence": 1, "operation_id": acknowledgement.operation_id, "status": "removed"}],
            "checkpoint journal does not close its exact removal")
    removal = validate_managed_workspace_operation_closure(
        context.state / "workspace-operations" / acknowledgement.operation_id, root_id)
    require(removal.get("checkpoint_removal") == {"checkpoint_id": checkpoint.checkpoint_id, "sequence": 1},
            "removal journal is not the registered attempt")
    return {"operation_id": acknowledgement.operation_id, "settled_on_admission": acknowledgement.settled,
            "operation": operation, "closure": closure_facts(removal)}


def _release_forks(
    context: WorkspaceContext, record: dict[str, object],
    forks: Sequence[tuple[Mapping[str, object], CheckoutFacts, Path]],
) -> None:
    record["lifetime_closure"] = _prove_lifetimes_closed(context, list(context.task_ids))
    released = []
    for task, child, repository in forks:
        release = _release_and_forget(context, task)
        require(not os.path.lexists(child.path), "released fork checkout directory remains")
        forget = _forget_workspace(context, task)
        require(_owned_ref(context, repository, child.base_ref, label="checkpoint-fork-base-forgotten") is None,
                "fork forget left its base pin")
        released.append({"workspace_id": child.workspace_id, "release": release, "forget": forget})
    record["fork_release"] = released


def _tool_identity(context: WorkspaceContext) -> dict[str, object]:
    """The exact Git and installed Delegate the scenario executes, as each reports itself."""
    version = context.processes.run(
        "delegate-version", [str(context.delegate), "version"], context.base, expected=(0,), timeout=15)
    return {
        "git": {"path": str(_git_executable()),
                "version": _git_ok(context, ["--version"], context.base, label="version").decode().strip()},
        "delegate_version": version.output(4096).decode("utf-8").strip(),
    }


def _run_checkpoints(context: WorkspaceContext, record: dict[str, object]) -> None:
    """Fill `record` stage by stage so a failed run still names its last completed stage."""
    record.update({"scenario": "checkpoint", "completed_stages": [], "current_stage": None, "commands": []})
    with _stage(record, "tooling"):
        record["tooling"] = _tool_identity(context)
    with _stage(record, "ordinary-checkpoint"):
        dirty, ordinary, before = _create_ordinary(context, record)
    with _stage(record, "ordinary-fork"):
        ordinary_task, ordinary_child, ordinary_fork = _fork(
            context, ordinary, source=dirty, brief=context.brief_success,
            present={**ORDINARY_TREE, FIXTURE_FILE: FIXTURE_EDIT}, absent=frozenset({"README.md"}),
            origin_base=str(before["head"]), parent=None)
        record["ordinary_fork"] = ordinary_fork
    with _stage(record, "managed-busy-refused"):
        parent, facts, held = _refuse_busy_managed(context, record, known=[ordinary.checkpoint_id])
    with _stage(record, "managed-checkpoint"):
        managed = _create_managed(context, record, parent, facts, held)
    with _stage(record, "managed-fork"):
        managed_task, managed_child, managed_fork = _fork(
            context, managed, source=Path(facts.path), brief=context.brief_timeout_resume,
            present={**MANAGED_TREE, RESUMED_FILE: b"fixture workspace timeout continuation\n"},
            absent=frozenset(), origin_base=facts.base_oid, parent=facts)
        record["managed_fork"] = managed_fork
    with _stage(record, "paged-list"):
        _require_listing(context, record, ordinary=ordinary, ordinary_cwd=dirty, managed=managed,
                         managed_workspace=facts.workspace_id)
    with _stage(record, "parent-released"):
        _release_origin(context, record, held, facts, managed)
    with _stage(record, "checkpoints-removed"):
        record["removals"] = {
            ordinary.checkpoint_id: _remove(context, record, ordinary, repository=dirty, label="checkpoint-ordinary"),
            managed.checkpoint_id: _remove(context, record, managed, repository=context.source,
                                           label="checkpoint-managed"),
        }
        final = _observe_checkout(context, dirty, label="checkpoint-ordinary-final")
        unchanged_checkout(before, final, "ordinary checkpoint input through removal")
    with _stage(record, "forks-released"):
        _release_forks(context, record, [(ordinary_task, ordinary_child, dirty),
                                         (managed_task, managed_child, context.source)])
    record["current_stage"] = None
