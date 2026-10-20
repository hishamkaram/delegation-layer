"""Managed-workspace capture, mutation, and exact reference operations."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import re
import time

import scripts.delegate_acceptance.private_cli.leases as private_cli_leases
from scripts.delegate_acceptance.workspaces.closure import (
    validate_managed_workspace_operation_closure,
)
from scripts.delegate_acceptance.evidence.files import digest, read_json
from scripts.delegate_acceptance.directories.policy import require
from .context import WorkspaceContext
from .evidence import _compact
from .model import (
    FIXTURE_FILE, MAX_RESPONSE_BYTES, OID_RE, SHA256_RE, TASK_ID_RE, WAIT_SECONDS,
    WORKSPACE_OPERATION_BUDGET,
)
from .operations import (
    _control, _operation_id, _task_record, _wait_workspace, _workspace_from,
)
from .policy import (
    _validate_hex, classify_control_response,
)
from .source import _git, _git_ok, _git_process


FIXTURE_EXEC_COMMAND = (
    "/bin/sh", "-c", "test -f " + FIXTURE_FILE + " && printf '%s\\n' workspace-exec-ok",
)


@dataclass(frozen=True)
class CaptureBinding:
    workspace_id: str
    operation_id: str
    base_oid: str
    observed_head: str
    tree_oid: str
    snapshot_ref: str
    root_id: str
    subject: dict[str, object]


def _verify_previous_boot_capture(
    state: Path, operation_id: str, root_id: str, termination_sha256: str,
) -> None:
    operation_path = Path(state) / "workspace-operations" / operation_id
    closure = validate_managed_workspace_operation_closure(operation_path, root_id)
    native = closure.get("managed_lifetime")
    require(isinstance(native, dict), "previous-boot capture has no native closure")
    termination = native.get("termination")
    require(isinstance(termination, dict) and termination.get("cause") == "previous-boot" and
            digest(operation_path / "lifetime-termination.json") == termination_sha256,
            "previous-boot capture termination does not match retained closure")


def _capture_binding(state: Path, capture: dict[str, object]) -> CaptureBinding:
    require(capture.get("status") == "captured", "workspace capture is not captured")
    workspace_id = _validate_hex(capture.get("workspace_id"), TASK_ID_RE, "capture workspace ID")
    operation_id = _validate_hex(capture.get("operation_id"), TASK_ID_RE, "capture operation ID")
    termination_sha256 = _validate_hex(
        capture.get("termination_sha256"), SHA256_RE, "capture termination_sha256",
    )
    previous_boot = capture.get("lifetime_closure") == "legacy-previous-boot"
    if previous_boot:
        require(not capture.get("worker_wait_sha256") and not capture.get("unlaunched_sha256"),
                "previous-boot capture has mixed closure facts")
    else:
        _validate_hex(capture.get("worker_wait_sha256"), SHA256_RE,
                      "capture worker_wait_sha256")
    candidate = capture.get("candidate")
    require(isinstance(candidate, dict), "captured workspace omitted its candidate")
    require(candidate.get("workspace_id") == workspace_id, "capture candidate workspace mismatch")
    require(candidate.get("operation_id") == operation_id, "capture candidate operation mismatch")
    subject = candidate.get("subject")
    require(isinstance(subject, dict), "capture candidate omitted subject")
    _validate_hex(subject.get("id"), TASK_ID_RE, "capture subject ID")
    _validate_hex(subject.get("sha256"), SHA256_RE, "capture subject SHA256")
    base_oid = _validate_hex(candidate.get("base_oid"), OID_RE, "capture base OID")
    observed_head = _validate_hex(candidate.get("observed_head"), OID_RE, "capture observed HEAD")
    tree_oid = _validate_hex(candidate.get("tree_oid"), OID_RE, "capture tree OID")
    snapshot_ref = candidate.get("snapshot_ref")
    expected_ref = re.compile(
        r"^refs/delegate/([0-9a-f]{32})/workspaces/"
        + re.escape(workspace_id) + r"/captures/" + re.escape(operation_id) + r"$"
    )
    ref_match = expected_ref.fullmatch(snapshot_ref) if isinstance(snapshot_ref, str) else None
    require(
        isinstance(snapshot_ref, str)
        and ref_match is not None
        and "\x00" not in snapshot_ref
        and len(snapshot_ref) <= 256,
        "capture snapshot ref is not the exact owned workspace ref",
    )
    root_record_path = Path(state) / "root.json"
    if root_record_path.is_file() and not root_record_path.is_symlink():
        root_record = read_json(root_record_path)
        require(root_record.get("root_id") == ref_match.group(1),
                "capture snapshot ref root identity changed")
    if previous_boot:
        _verify_previous_boot_capture(state, operation_id, ref_match.group(1), termination_sha256)
    return CaptureBinding(
        workspace_id=workspace_id,
        operation_id=operation_id,
        base_oid=base_oid,
        observed_head=observed_head,
        tree_oid=tree_oid,
        snapshot_ref=snapshot_ref,
        root_id=ref_match.group(1),
        subject=subject,
    )


CUMULATIVE_CAPTURE_ARTIFACTS = frozenset({"diff.patch", "name-status.z"})
TURN_CAPTURE_ARTIFACTS = frozenset({"turn-diff.patch", "turn-name-status.z"})


def _capture_observed_turn(capture: dict[str, object]) -> bool:
    """Report whether the capture compares against its own prelaunch baseline."""
    candidate = capture.get("candidate")
    turn = candidate.get("turn") if isinstance(candidate, dict) else None
    if turn is None:
        return False
    require(isinstance(turn, dict) and turn.get("status") in {"observed", "unavailable"},
            "capture turn observation is invalid")
    return turn["status"] == "observed"


def _verify_artifact_descriptor(artifact_root: Path, artifact: object) -> dict[str, object]:
    """Match the Go descriptor contract, then bind complete bytes to the retained file."""
    require(isinstance(artifact, dict), "capture artifact is not an object")
    name = artifact.get("name")
    require(isinstance(name, str) and name in CUMULATIVE_CAPTURE_ARTIFACTS | TURN_CAPTURE_ARTIFACTS,
            "capture artifact name is not allowlisted")
    omitted = artifact.get("omitted", "")
    require(isinstance(omitted, str), "capture omitted reason is invalid")
    require(artifact.get("truncated", False) is False, "capture artifact is truncated")
    length = artifact.get("length")
    require(type(length) is int and length >= 0, "capture artifact length is invalid")
    if omitted:
        require(omitted == "limit" and length == 0 and artifact.get("sha256", "") == "",
                "capture omitted artifact is not an empty limit descriptor")
        return {"name": name, "omitted": omitted}
    sha256 = _validate_hex(artifact.get("sha256"), SHA256_RE, "capture artifact SHA256")
    path = artifact_root / name
    require(path.parent == artifact_root and path.is_file() and not path.is_symlink(),
            "capture artifact path is not a retained regular file")
    require(len(path.read_bytes()) == length and digest(path) == sha256,
            "capture artifact digest does not match retained bytes")
    return {"name": name, "length": length, "sha256": sha256}


def _verify_capture_artifacts(
    state: Path, operation_id: str, artifacts_value: object, observed_turn: bool,
) -> list[dict[str, object]]:
    require(isinstance(artifacts_value, list), "capture artifact inventory is not a list")
    artifact_root = Path(state) / "workspace-operations" / operation_id / "artifacts"
    verified: list[dict[str, object]] = []
    for artifact in artifacts_value:
        item = _verify_artifact_descriptor(artifact_root, artifact)
        require(all(prior["name"] != item["name"] for prior in verified),
                "capture artifact name is duplicated")
        verified.append(item)
    names = {item["name"] for item in verified}
    require(CUMULATIVE_CAPTURE_ARTIFACTS <= names, "capture omitted a cumulative artifact")
    require(any(item["name"] == "diff.patch" and item.get("length", 0) > 0 for item in verified),
            "capture did not retain a nonempty patch")
    require(names & TURN_CAPTURE_ARTIFACTS == (TURN_CAPTURE_ARTIFACTS if observed_turn else frozenset()),
            "capture turn artifacts do not match its turn observation")
    return verified


def verify_capture_receipt(state: Path, capture: dict[str, object]) -> dict[str, object]:
    """Verify capture digests and artifact paths against retained state evidence."""
    binding = _capture_binding(state, capture)
    artifacts = _verify_capture_artifacts(
        state, binding.operation_id, capture.get("artifacts"), _capture_observed_turn(capture),
    )
    subject = binding.subject
    return {
        "workspace_id": binding.workspace_id,
        "operation_id": binding.operation_id,
        "base_oid": binding.base_oid,
        "observed_head": binding.observed_head,
        "tree_oid": binding.tree_oid,
        "subject": {
            "kind": subject.get("kind"),
            "id": subject.get("id"),
            "sha256": subject.get("sha256"),
        },
        "root_id": binding.root_id,
        "snapshot_ref": binding.snapshot_ref,
        "artifacts": artifacts,
    }


def _task_baseline_ref(
    context: WorkspaceContext, evidence: dict[str, object], *, task_id: str,
) -> str:
    record = _task_record(context, task_id)
    require(record.get("root_id") == evidence["root_id"], "turn task root identity changed")
    binding = record.get("workspace")
    require(isinstance(binding, dict) and binding.get("id") == evidence["workspace_id"],
            "turn task workspace binding changed")
    sequence = binding.get("reservation")
    require(type(sequence) is int and 0 < sequence < 2**64,
            "turn task reservation is invalid")
    return ("refs/delegate/" + str(evidence["root_id"]) + "/workspaces/"
            + str(evidence["workspace_id"]) + f"/baselines/{sequence:020d}")


def _turn_files(data: bytes) -> frozenset[bytes]:
    """Decode Git's --no-renames --name-status -z pairs without splitting path bytes."""
    if not data:
        return frozenset()
    require(data.endswith(b"\x00"), "turn name-status is not NUL terminated")
    fields = data[:-1].split(b"\x00")
    require(len(fields) % 2 == 0, "turn name-status has an incomplete status/path pair")
    files: set[bytes] = set()
    for status, path in zip(fields[::2], fields[1::2], strict=True):
        require(status in {b"A", b"D", b"M", b"T", b"U", b"X", b"B"} and bool(path),
                "turn name-status has an invalid status/path pair")
        require(path not in files, "turn name-status has a duplicate path")
        files.add(path)
    return frozenset(files)


def verify_task_turn(
    context: WorkspaceContext, capture: dict[str, object], *, task_id: str,
    expected_files: frozenset[str], forbidden_files: frozenset[str] = frozenset(),
) -> dict[str, object]:
    """Require a fresh task's own baseline and retained, attributable turn artifacts."""
    _validate_hex(task_id, TASK_ID_RE, "turn task ID")
    evidence = verify_capture_receipt(context.state, capture)
    subject = evidence["subject"]
    require(isinstance(subject, dict) and subject.get("kind") == "task"
            and subject.get("id") == task_id and capture.get("subject") == subject,
            "turn capture is not bound to the exact task subject")
    candidate = capture["candidate"]
    turn = candidate.get("turn")
    require(isinstance(turn, dict) and turn.get("status") == "observed"
            and turn.get("reason_code", "") == "", "fresh task turn was not observed")
    for field in ("content", "head", "index"):
        require(turn.get(field) in {"changed", "unchanged"},
                "fresh task turn has invalid " + field + " observation")
    reference = _task_baseline_ref(context, evidence, task_id=task_id)
    require(turn.get("baseline_ref") == reference, "turn baseline ref does not match task reservation")
    tree = _validate_hex(turn.get("start_tree_oid"), OID_RE, "turn start tree OID")
    observed = _git_ok(
        context, ["-C", str(context.source), "rev-parse", "--verify", reference + "^{tree}"],
        context.source, label="turn-baseline-tree",
    ).decode("ascii").strip()
    require(observed == tree, "turn baseline ref does not resolve to its start tree")
    require(all(not item.get("omitted") for item in evidence["artifacts"]
                if item["name"] in TURN_CAPTURE_ARTIFACTS), "fresh task turn artifacts were omitted")
    path = (context.state / "workspace-operations" / evidence["operation_id"]
            / "artifacts" / "turn-name-status.z")
    files = _turn_files(path.read_bytes())
    require({os_name.encode("utf-8") for os_name in expected_files} <= files,
            "turn attribution omitted expected files")
    require(not {os_name.encode("utf-8") for os_name in forbidden_files} & files,
            "turn attribution includes a forbidden previous-turn file")
    require(not files or turn["content"] == "changed", "turn files require changed content")
    return {**evidence, "turn": dict(turn)}


def _capture_roundtrip(
    context: WorkspaceContext,
    capture: dict[str, object],
    *,
    label: str = "capture-roundtrip",
) -> dict[str, object]:
    evidence = verify_capture_receipt(context.state, capture)
    ref = evidence["snapshot_ref"]
    code, _ = _git(
        context, ["-C", str(context.source), "cat-file", "-e", str(ref) + "^{tree}"],
        context.source, label="capture-ref-before-release",
    )
    require(code == 0, "capture snapshot ref does not resolve before release")
    patch_path = context.state / "workspace-operations" / evidence["operation_id"] / "artifacts" / "diff.patch"
    roundtrip = context.owner.create(
        "capture patch roundtrip", context.base / (label + "-" + evidence["operation_id"]), context.base,
    ).directory
    _git_ok(
        context, ["clone", "--no-local", str(context.source), str(roundtrip)],
        context.base, label="capture-roundtrip-clone",
    )
    _git_ok(
        context, ["-C", str(roundtrip), "apply", "--check", str(patch_path)],
        context.base, label="capture-roundtrip-check",
    )
    _git_ok(
        context, ["-C", str(roundtrip), "apply", str(patch_path)],
        context.base, label="capture-roundtrip-apply",
    )
    _git_ok(
        context, ["-C", str(roundtrip), "add", "--all"],
        context.base, label="capture-roundtrip-add",
    )
    roundtrip_tree = _git_ok(
        context, ["-C", str(roundtrip), "write-tree"],
        context.base, label="capture-roundtrip-tree",
    ).decode("ascii").strip()
    require(roundtrip_tree == evidence["tree_oid"],
            "capture patch roundtrip produced a different tree OID")
    edited = roundtrip / FIXTURE_FILE
    require(edited.is_file() and edited.read_text(encoding="utf-8") == "fixture workspace edit\n",
            "capture patch did not reproduce fixture edit")
    return evidence


def _workspace_exec(
    context: WorkspaceContext,
    task: dict[str, object],
    *,
    command: tuple[str, ...] = FIXTURE_EXEC_COMMAND,
    label: str = "workspace-exec",
) -> dict[str, object]:
    """Run one literal argv through supervised workspace verification and wait for its result."""
    workspace_id = task["workspace_id"]
    code, response = _workspace_mutation(
        context,
        [
            "--root", str(context.state), "workspace", "exec", workspace_id,
            "--budget", WORKSPACE_OPERATION_BUDGET, "--json", "--", *command,
        ],
        action="exec", workspace_id=workspace_id,
        label=label, task_id=task["task_id"],
    )
    operation_id = _operation_id(response)
    require(operation_id is not None, "workspace exec omitted operation identity")
    observed = _wait_workspace(
        context, workspace_id, operation_id=operation_id, state="retained", timeout=WAIT_SECONDS,
        task_id=task["task_id"],
    )
    operation = _workspace_from(observed).get("operation")
    require(isinstance(operation, dict) and operation.get("status") == "succeeded",
            "workspace exec operation did not succeed")
    require(operation.get("command_exit") == 0, "workspace exec command did not exit zero")
    return {
        "exit_code": code,
        "operation_id": operation_id,
        "command_exit": operation.get("command_exit"),
    }


def _workspace_mutation(
    context: WorkspaceContext,
    arguments: list[str],
    *,
    action: str,
    workspace_id: str,
    label: str,
    task_id: str,
) -> tuple[int, dict[str, object]]:
    """Retry only a negative action acknowledgement, preserving exact argv."""
    deadline = time.monotonic() + WAIT_SECONDS
    while True:
        remaining = deadline - time.monotonic()
        require(remaining > 0, "workspace action admission expired: " + action)
        code, response = _control(
            context, arguments, label=label, task_id=task_id,
            expected=(0, 3), timeout=min(90, remaining), phase=label,
        )
        classification = classify_control_response(code, response)
        if classification != "not-authorized":
            require(classification != "failed", "workspace action failed: " + repr(_compact(response)))
            return code, response
        require(_workspace_from(response).get("next_action") == "retry_" + action,
                "workspace action returned a different retry instruction")
        remaining = deadline - time.monotonic()
        require(remaining > 0, "workspace action admission expired: " + action)
        _wait_workspace(
            context, workspace_id, state="released" if action == "forget" else "retained",
            timeout=remaining, task_id=task_id,
        )


def _verify_ref(context: WorkspaceContext, reference: str, label: str, expected: tuple[int, ...]) -> int:
    process = _git_process(
        context,
        ["-C", str(context.source), "for-each-ref", "--format=%(refname)", "--", reference],
        context.source, label=label,
    )
    references = process.output(MAX_RESPONSE_BYTES).decode("utf-8").splitlines()
    present = reference in references
    if not present:
        require(not process.stderr_path.read_bytes(),
                "ref lookup diagnostics prevent proving exact absence")
    code = 0 if present else 1
    require(code in expected, "ref presence did not match expected state: " + label)
    return code


def _release_and_forget(context: WorkspaceContext, task: dict[str, object]) -> dict[str, object]:
    workspace_id = task["workspace_id"]
    _code, released = _workspace_mutation(
        context,
        [
            "--root", str(context.state), "workspace", "release", workspace_id,
            "--budget", WORKSPACE_OPERATION_BUDGET, "--json",
        ],
        action="release", workspace_id=workspace_id,
        label="workspace-release", task_id=task["task_id"],
    )
    release_operation = _operation_id(released)
    released_observed = _wait_workspace(
        context, workspace_id, operation_id=release_operation,
        state="released", timeout=WAIT_SECONDS, task_id=task["task_id"],
    )
    return {
        "release_state": _workspace_from(released_observed).get("state"),
        "release_operation": release_operation,
    }


def _forget_workspace(context: WorkspaceContext, task: dict[str, object]) -> dict[str, object]:
    workspace_id = task["workspace_id"]
    _code, forgotten = _workspace_mutation(
        context,
        [
            "--root", str(context.state), "workspace", "forget", workspace_id,
            "--budget", WORKSPACE_OPERATION_BUDGET, "--json",
        ],
        action="forget", workspace_id=workspace_id,
        label="workspace-forget", task_id=task["task_id"],
    )
    forget_operation = _operation_id(forgotten)
    if _workspace_from(forgotten).get("state") == "forgotten":
        final = forgotten
    else:
        final = _wait_workspace(
            context, workspace_id, operation_id=forget_operation,
            state="forgotten", timeout=WAIT_SECONDS, task_id=task["task_id"],
        )
    require(_workspace_from(final).get("state") == "forgotten",
            "workspace forget did not reach forgotten state")
    return {
        "forget_state": _workspace_from(final).get("state"),
        "forget_operation": forget_operation,
    }


def _finish_cohort(context: WorkspaceContext, cohort: private_cli_leases.LiveFIFOLeaseCohort) -> list[dict[str, object]]:
    evidence = cohort.remove_closed_fifos()
    cohort.close()
    if cohort in context.cohorts:
        context.cohorts.remove(cohort)
    private_cli_leases.require_empty_fixture_waiters(context.waiter_directory)
    return evidence
