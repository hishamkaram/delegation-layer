"""Composed native acceptance behavior: operations."""

from __future__ import annotations


import time
from pathlib import Path
from scripts.delegate_acceptance.evidence.files import digest, write_json
from scripts.delegate_acceptance.evidence.validation import _hex_digest, _task_identity, require
from scripts.delegate_acceptance.native.fixtures import shell_append_continuation_command, shell_digest_command
from scripts.delegate_acceptance.paths.operations import write_bytes
from scripts.delegate_acceptance.native.constants import CANONICAL_TASK_BUDGET, TASK_BUDGET, TIMEOUT_LEASE_SETUP_SECONDS


def direct(owner, name: str, argv: list[object], expected=0, timeout=30):
    acceptance = owner
    return acceptance.ops.direct(name, argv, expected=expected, timeout=timeout)

def _sync_ops_owned_tasks(owner) -> None:
    """Copy only complete, positively admitted task identities for cleanup."""
    acceptance = owner
    ops = acceptance.ops
    ops_root = ops.root_id
    valid_root = (isinstance(ops_root, str) and len(ops_root) == 32 and
                  all(character in "0123456789abcdef" for character in ops_root))
    if valid_root:
        if acceptance.root_id is None:
            acceptance.root_id = ops_root
        else:
            require(acceptance.root_id == ops_root, "dispatch changed root identity")
    for name, task_id in ops.tasks.items():
        numeric_id = ops.numbers.get(name)
        label = ops.labels.get(name)
        if not (valid_root and isinstance(name, str) and isinstance(task_id, str) and
                task_id in ops.dispatch_attempts and
                label == f"delegate:{ops_root}:{task_id}" and
                type(numeric_id) is int and numeric_id >= 0):
            continue
        existing_task = acceptance.tasks.get(name)
        if existing_task is not None:
            require(existing_task == task_id, f"cleanup task identity changed for {name}")
        existing_numeric = acceptance.numeric_ids.get(task_id)
        if existing_numeric is not None:
            require(existing_numeric == numeric_id,
                    f"cleanup supervisor identity changed for {task_id}")
        acceptance.tasks[name] = task_id
        acceptance.numeric_ids[task_id] = numeric_id

def write_binding(owner, task_id: str, provider_identity: dict[str, str]) -> None:
    acceptance = owner
    binding = acceptance.output / "binding.json"
    require(not binding.exists() and not binding.is_symlink(),
            "runtime binding was already persisted")
    recorded_identity = acceptance._provider_bindings.identity_for(task_id)
    require(_task_identity(task_id) and
            set(provider_identity) == {"executable", "version", "sha256"} and
            recorded_identity is not None and
            dict(recorded_identity) == provider_identity and
            provider_identity.get("version") == acceptance.provider_version and
            _hex_digest(provider_identity.get("sha256")),
            "first admitted provider identity is unavailable")
    args = getattr(acceptance, "args", None)
    write_json(binding, {
        "provider": acceptance.provider,
        "mode": acceptance.mode,
        "scenario": acceptance.scenario,
        "task_budget": TASK_BUDGET,
        "canonical_task_budget": CANONICAL_TASK_BUDGET,
        "requested_model": getattr(args, "model", None),
        "requested_effort": getattr(args, "effort", None),
        "model_discovery": getattr(acceptance, "model_discovery_summary", None),
        "native_mode": acceptance.native_mode_receipt(),
        "provider_identity_scope": "first-admitted-task",
        "provider_task_id": task_id,
        "provider_executable": provider_identity["executable"],
        "provider_sha256": provider_identity["sha256"],
        "provider_version": provider_identity["version"],
        "delegate_sha256": digest(acceptance.delegate),
        "runner_sha256": digest(acceptance.runner),
        "pueue_sha256": digest(acceptance.pueue),
        "pueued_sha256": digest(acceptance.pueued),
        "pueue_version": acceptance.pueue_version,
        "pueued_version": acceptance.pueued_version,
        "credentials_in_receipt": False,
    })

def _record_provider_runtime_identity(
        owner, task_id: str, meta: dict[str, object],
        launch_evidence: tuple[dict[str, object], dict[str, object]] | None = None,
        started_evidence: dict[str, object] | None = None) -> dict[str, str]:
    acceptance = owner
    return acceptance._provider_bindings.record(
        task_id, meta, acceptance.provider_executable, launch_evidence, started_evidence)

def provider_runtime_receipt(owner) -> dict[str, object]:
    acceptance = owner
    return acceptance._provider_bindings.receipt(set(acceptance.tasks.values()))

def native_mode_receipt(owner) -> dict[str, object] | None:
    acceptance = owner
    if acceptance.provider != "antigravity:print":
        return None
    if acceptance.mode == "read-only":
        return {"sandbox": "--sandbox", "mode": "plan", "bypass": False}
    return {"sandbox": "--sandbox", "mode": "accept-edits", "bypass": True}

def brief(owner, name: str, continuation: bool = False) -> Path:
    acceptance = owner
    path = acceptance.briefs / f"{name}.txt"
    if acceptance.mode == "workspace-write":
        if continuation:
            text = (
                "Continue the exact previous conversation and complete this workspace integration test. "
                "Use native file and shell tools in the current workspace. Verify the existing target, "
                f"then execute `{shell_append_continuation_command()}` to append exactly `continued` "
                "as a second line to acceptance-write-target.txt without replacing its existing content. "
                f"Execute `{shell_digest_command('acceptance-write-continue-check.txt')}` "
                "after the edit. Reply exactly `DELEGATION_LIVE_WRITE_CONTINUED` after completing the test. "
                "Do not modify any other file, implement source code, change configuration, "
                "use network, access paths outside the workspace, or claim results from prose.\n"
            )
        else:
            text = (
                "Perform this workspace write integration test with native file and shell tools. This is an "
                "integration test, not a request to implement code. Read acceptance-write-nonce.txt and edit "
                "the pre-existing acceptance-write-fixture.txt so it contains exactly "
                "`edited-fixture:<nonce>`. Create "
                "acceptance-write-target.txt containing exactly `created:<nonce>`, edit that same file so it "
                "contains exactly `edited:<nonce>`, then execute "
                f"`{shell_digest_command('acceptance-write-check.txt')}` after the edit. "
                "Reply exactly `DELEGATION_LIVE_WRITE_OK` after completing the test. "
                "Do not modify any other file, implement source "
                "code, change configuration, use network, access paths outside the current workspace, or claim "
                "results from prose.\n"
            )
    else:
        if getattr(acceptance, "provider", None) == "codex:review":
            text = (
                "Review the current uncommitted changes in this Git workspace. Return a concise review "
                "of the change and do not modify files, use network, or access paths outside the workspace.\n"
            )
            write_bytes(path, text.encode())
            return path
        prefix = "DELEGATION_LIVE_CONTINUED:" if continuation else "DELEGATION_LIVE_OK:"
        text = "Continue the exact previous conversation. " if continuation else ""
        provider = getattr(acceptance, "provider", None)
        read_instruction = ("using the provider's file-read tool only"
                            if provider == "antigravity:print"
                            else "using a file-read tool or a read-only shell command")
        no_command = (" Do not use a command or shell tool."
                      if provider == "antigravity:print" else "")
        text += (f"Read the actual file `{acceptance.workspace_marker}` {read_instruction}.{no_command} "
                 "Do not infer its contents from examples or other repositories. "
                 f"Reply with exactly the literal prefix `{prefix}` followed by one space and the exact "
                 "contents read from that file, with no other text. "
                 "Do not modify files, use network, or access paths outside the current workspace.\n")
    write_bytes(path, text.encode())
    return path

def interactive_brief(owner, name: str) -> Path:
    acceptance = owner
    path = acceptance.briefs / f"{name}.txt"
    text = (
        "You MUST call the root AskUserQuestion tool before producing any final answer. "
        "Ask exactly one question with the text `Choose a color` and exactly two options, "
        "`Blue` and `Green`; make it single-select, then wait for my answer. "
        "After I answer Blue, return exactly `DELEGATION_LIVE_INTERACTIVE_OK: Blue` as your final answer. "
        "Do not modify files, use shell, use network, or access paths outside the current workspace.\n"
    )
    write_bytes(path, text.encode())
    return path

def timeout_brief(owner, name: str, resume: bool = False) -> Path:
    acceptance = owner
    return acceptance.timeout_state.write_brief(acceptance.briefs, name, resume)

def _observe_timeout_leases(owner) -> None:
    acceptance = owner
    acceptance.timeout_state.observe()

def _timeout_liveness_ready(owner) -> bool:
    acceptance = owner
    return acceptance.timeout_state.ready()

def _timeout_liveness_receipt(owner, closed: bool) -> dict[str, object]:
    acceptance = owner
    return acceptance.timeout_state.receipt(closed)

def _capture_timeout_liveness(owner) -> bool:
    acceptance = owner
    return acceptance.timeout_state.capture(acceptance.output)

def record_timeout_liveness(
        owner, task_id: str, timeout: float = TIMEOUT_LEASE_SETUP_SECONDS,
        sleep_fn=time.sleep, monotonic_fn=time.monotonic) -> None:
    acceptance = owner
    acceptance.timeout_state.record_liveness(
        task_id=task_id, root_id=acceptance.root_id, operations=acceptance.ops, output=acceptance.output,
        timeout=timeout, sleep_fn=sleep_fn, monotonic_fn=monotonic_fn)

def _timeout_liveness_closed(owner) -> bool:
    acceptance = owner
    return acceptance.timeout_state.closed()

def wait_timeout_leases_closed(owner) -> None:
    acceptance = owner
    acceptance.timeout_state.wait_closed(acceptance.output)
