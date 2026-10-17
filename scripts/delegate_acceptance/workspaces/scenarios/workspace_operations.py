"""Managed-workspace capture, mutation, and exact reference operations."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
import re
import time

import scripts.delegate_acceptance.private_cli.leases as private_cli_leases
import scripts.delegate_acceptance.private_cli.stop as private_cli_stop
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
    _control, _operation_id, _wait_workspace, _workspace_from,
)
from .policy import (
    _validate_hex, classify_control_response,
)
from .source import _git, _git_ok, _git_process



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
        workspace_id, operation_id, base_oid, observed_head, tree_oid, snapshot_ref,
        ref_match.group(1), subject,
    )


def _verify_capture_artifacts(
    state: Path, operation_id: str, artifacts_value: object,
) -> list[dict[str, object]]:
    require(isinstance(artifacts_value, list), "capture artifact inventory is not a list")
    verified: list[dict[str, object]] = []
    artifact_root = Path(state) / "workspace-operations" / operation_id / "artifacts"
    for artifact in artifacts_value:
        require(isinstance(artifact, dict), "capture artifact is not an object")
        name = artifact.get("name")
        require(
            isinstance(name, str) and name in {"diff.patch", "name-status.z"},
            "capture artifact name is not allowlisted",
        )
        omitted = artifact.get("omitted", "")
        if omitted:
            require(isinstance(omitted, str), "capture omitted reason is invalid")
            verified.append({"name": name, "omitted": omitted})
            continue
        length = artifact.get("length")
        require(type(length) is int and length >= 0, "capture artifact length is invalid")
        sha256 = _validate_hex(artifact.get("sha256"), SHA256_RE, "capture artifact SHA256")
        path = artifact_root / name
        require(path.parent == artifact_root and path.is_file() and not path.is_symlink(),
                "capture artifact path is not a retained regular file")
        data = path.read_bytes()
        require(len(data) == length and digest(path) == sha256,
                "capture artifact digest does not match retained bytes")
        verified.append({"name": name, "length": length, "sha256": sha256})
    require(any(item.get("name") == "diff.patch" and item.get("length", 0) > 0
                for item in verified),
            "capture did not retain a nonempty patch")
    return verified


def verify_capture_receipt(state: Path, capture: dict[str, object]) -> dict[str, object]:
    """Verify capture digests and artifact paths against retained state evidence."""
    binding = _capture_binding(state, capture)
    artifacts = _verify_capture_artifacts(state, binding.operation_id, capture.get("artifacts"))
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


def _workspace_exec(context: WorkspaceContext, task: dict[str, object]) -> dict[str, object]:
    workspace_id = task["workspace_id"]
    code, response = _workspace_mutation(
        context,
        [
            "--root", str(context.state), "workspace", "exec", workspace_id,
            "--budget", WORKSPACE_OPERATION_BUDGET, "--json", "--",
            "/bin/sh", "-c",
            "test -f managed-workspace-fixture.txt && "
            "printf '%s\\n' workspace-exec-ok",
        ],
        action="exec", workspace_id=workspace_id,
        label="workspace-exec", task_id=task["task_id"],
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
    private_cli_stop.require_empty_fixture_waiters(context.waiter_directory)
    return evidence
