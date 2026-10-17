"""Composed native acceptance behavior: scenarios."""

from __future__ import annotations


import stat
from scripts.delegate_acceptance.evidence.files import digest, write_json
from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.native.fixtures import require_replay_unchanged, require_same_session, verify_write_artifacts, verify_write_fixture
from scripts.delegate_acceptance.native.workspace import has_git_ancestor, task_digest_snapshot, workspace_snapshot
from scripts.delegate_acceptance.supervisor.queue_policy import done_result
from scripts.delegate_acceptance.native.constants import CANONICAL_TASK_BUDGET, CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET, MAX_RAW_BYTES, TASK_BUDGET, TIMEOUT_RESUME_SOURCE_BUDGET, TIMEOUT_SOURCE_RUNNER_RESULTS


def run_interactive(owner) -> int:
    acceptance = owner
    before = workspace_snapshot(acceptance.workspace)
    task_id = acceptance.dispatch("interactive", acceptance.interactive_brief("interactive"))
    pending_state, cursor, pending_events = acceptance.wait_for_interaction(task_id, pending=True)
    request_id, turn_id = acceptance.interactive_request(pending_state)
    message_id, queued_delivery = acceptance.reply_interaction(task_id, request_id, turn_id)
    final_state, final_cursor, final_events = acceptance.wait_for_interaction(task_id, cursor)
    deliveries = final_state.get("deliveries")
    require(isinstance(deliveries, list), "interactive final delivery state is malformed")
    delivery = next((item for item in deliveries
                     if isinstance(item, dict) and item.get("message_id") == message_id), None)
    require(isinstance(delivery, dict) and delivery.get("state") == "acknowledged",
            "interactive reply was not acknowledged by the provider")
    acceptance.wait_done("interactive", task_id)
    acceptance.collect("interactive-collect", task_id)
    acceptance.task_binding(task_id)
    provider_session = acceptance.session_ref(task_id)
    require(workspace_snapshot(acceptance.workspace) == before,
            "Claude interactive acceptance changed the workspace")

    interaction_path = acceptance.state / "tasks" / task_id / "raw" / "interaction"
    metadata = interaction_path.lstat()
    require(stat.S_ISREG(metadata.st_mode) and not stat.S_ISLNK(metadata.st_mode) and
            0 < metadata.st_size <= MAX_RAW_BYTES,
            "interactive task has no bounded sealed interaction receipt")
    interaction_receipt = {
        "schema_version": 1,
        "task_id": task_id,
        "request_id": request_id,
        "turn_id": turn_id,
        "message_id": message_id,
        "answer": "Blue",
        "queued_delivery": queued_delivery,
        "delivery": delivery,
        "initial_cursor": cursor,
        "final_cursor": final_cursor,
        "event_kinds": [event.get("kind") for event in pending_events + final_events
                        if isinstance(event.get("kind"), str)],
        "closed": final_state.get("closed") is True,
        "raw_interaction_sha256": digest(interaction_path),
    }
    require(interaction_receipt["closed"] is True,
            "interactive task did not publish a closed interaction state")
    write_json(acceptance.output / "interaction.json", interaction_receipt)
    acceptance.replay({"interactive": acceptance.records["interactive-collect"]}, names=("interactive",))
    acceptance.shutdown()
    acceptance.success_receipt = {
        "gate": "acceptance-native", "status": "passed", "provider": acceptance.provider,
        "mode": acceptance.mode, "scenario": acceptance.scenario,
        "task_budget": TASK_BUDGET,
        "canonical_task_budget": CANONICAL_TASK_BUDGET,
        "requested_model": getattr(acceptance.args, "model", None),
        "requested_effort": getattr(acceptance.args, "effort", None),
        "model_discovery": acceptance.model_discovery_summary,
        "native_mode": acceptance.native_mode_receipt(),
        **acceptance.provider_runtime_receipt(),
        "tasks": {"interactive": task_id},
        "provider_sessions": {"interactive": provider_session},
        "interaction": interaction_receipt,
        "provider_launches": 1, "replay_launches": 0,
        "credentials_in_receipt": False,
        "natural_daemon_shutdown": acceptance.closed,
    }
    return 0

def run_timeout_resume(owner) -> int:
    acceptance = owner
    source = acceptance.dispatch("timeout-source", acceptance.timeout_brief("timeout-source"),
                           budget=TIMEOUT_RESUME_SOURCE_BUDGET)
    acceptance.record_timeout_liveness(source)
    source_row = acceptance.wait_done(
        "timeout-source", source, expected_result=TIMEOUT_SOURCE_RUNNER_RESULTS)
    source_runner_result = done_result(source_row)
    require(isinstance(source_runner_result, str) and
            source_runner_result in TIMEOUT_SOURCE_RUNNER_RESULTS,
            "timeout source has no accepted runner result")
    acceptance.ops.expect_terminal_result("timeout-source", source_runner_result)
    timed_out = acceptance.wait_timed_out(source)
    acceptance.wait_timeout_leases_closed()
    source_collect = acceptance.collect_timed_out_source(source)
    source_directory = acceptance.timeout_source_binding(source)
    source_session = acceptance.session_ref(source)
    acceptance._timeout_workspace_snapshot(resumed=False)
    source_snapshot = task_digest_snapshot(source_directory)

    successor = acceptance.continue_timed_out_task(
        "timeout-resume", acceptance.timeout_brief("timeout-resume", resume=True), source)
    acceptance.wait_done("timeout-resume", successor)
    acceptance.collect("timeout-resume-collect", successor)
    acceptance.task_binding(successor, source, source_session)
    successor_session = acceptance.session_ref(successor)
    require_same_session(source_session, successor_session)
    require_replay_unchanged(source_snapshot, task_digest_snapshot(source_directory))
    require(not (source_directory / "result.txt").exists(),
            "continuation changed its timed-out predecessor into an answer")
    acceptance._timeout_workspace_snapshot(resumed=True)
    acceptance.replay({"timeout-resume": acceptance.records["timeout-resume-collect"]},
                names=("timeout-resume",))
    acceptance.shutdown()
    continuation = timed_out.get("continuation")
    require(isinstance(continuation, dict), "timed-out continuation receipt is unavailable")
    acceptance.success_receipt = {
        "gate": "acceptance-native", "status": "passed", "provider": acceptance.provider,
        "mode": acceptance.mode, "scenario": acceptance.scenario,
        "task_budget": TASK_BUDGET,
        "timeout_source_budget": TIMEOUT_RESUME_SOURCE_BUDGET,
        "canonical_timeout_source_budget": CANONICAL_TIMEOUT_RESUME_SOURCE_BUDGET,
        "requested_model": getattr(acceptance.args, "model", None),
        "requested_effort": getattr(acceptance.args, "effort", None),
        **acceptance.provider_runtime_receipt(),
        "tasks": {"timeout_source": source, "timeout_resume": successor},
        "provider_sessions": {
            "timeout_source": source_session,
            "timeout_resume": successor_session,
        },
        "source_runner_result": source_runner_result,
        "source_publication": source_collect.get("publication"),
        "continuation": continuation,
        "timeout_liveness": acceptance.timeout_state.liveness,
        "same_session": True,
        "source_immutable": True,
        "replay_launches": 0,
        "credentials_in_receipt": False,
        "natural_daemon_shutdown": acceptance.closed,
    }
    return 0

def run(owner) -> int:
    acceptance = owner
    acceptance.success_receipt = None
    acceptance.setup()
    if acceptance.discover_models_only:
        return acceptance._run_model_discovery()
    acceptance.require_default_model_prerequisite()
    if acceptance.scenario == "timeout-resume":
        return acceptance.run_timeout_resume()
    if acceptance.scenario == "interactive":
        return acceptance.run_interactive()
    if acceptance.provider == "codex:review":
        return acceptance._run_review()
    return acceptance._run_resumable()

def _run_model_discovery(owner) -> int:
    acceptance = owner
    acceptance.discover_models()
    acceptance.shutdown()
    acceptance.success_receipt = {
        "gate": "acceptance-native", "status":
        "passed" if acceptance.model_discovery_exit_code == 0 else
        "BLOCKED" if acceptance.model_discovery_exit_code == 2 else "failed",
        "provider": acceptance.provider, "mode": acceptance.mode,
        "task_budget": TASK_BUDGET,
        "canonical_task_budget": CANONICAL_TASK_BUDGET,
        "requested_model": getattr(acceptance.args, "model", None),
        "requested_effort": getattr(acceptance.args, "effort", None),
        "model_discovery": acceptance.model_discovery_summary,
        "discovery_exit_code": acceptance.model_discovery_exit_code,
        "native_mode": acceptance.native_mode_receipt(),
        "credentials_in_receipt": False,
        "natural_daemon_shutdown": acceptance.closed,
    }
    require(isinstance(acceptance.model_discovery_exit_code, int),
            "model discovery exit code is unavailable")
    return acceptance.model_discovery_exit_code

def _run_review(owner) -> int:
    acceptance = owner
    before = workspace_snapshot(acceptance.workspace)
    first = acceptance.dispatch("fresh", acceptance.brief("fresh"))
    acceptance.wait_done("fresh", first)
    acceptance.collect("fresh-collect", first)
    acceptance.task_binding(first)
    first_session = acceptance.session_ref(first)
    require(workspace_snapshot(acceptance.workspace) == before,
            "Codex review changed the workspace")
    acceptance.replay({"fresh": acceptance.records["fresh-collect"]}, names=("fresh",))
    acceptance.shutdown()
    acceptance.success_receipt = {
        "gate": "acceptance-native", "status": "passed", "provider": acceptance.provider,
        "mode": acceptance.mode, "task_budget": TASK_BUDGET,
        "canonical_task_budget": CANONICAL_TASK_BUDGET,
        "requested_model": getattr(acceptance.args, "model", None),
        "requested_effort": getattr(acceptance.args, "effort", None),
        "model_discovery": acceptance.model_discovery_summary,
        "native_mode": acceptance.native_mode_receipt(),
        **acceptance.provider_runtime_receipt(),
        "tasks": {"fresh": acceptance.tasks["fresh"]},
        "provider_sessions": {"fresh": first_session},
        "scenario": acceptance.scenario, "provider_launches": 1, "replay_launches": 0,
        "credentials_in_receipt": False,
        "natural_daemon_shutdown": acceptance.closed,
    }
    return 0

def _run_resumable(owner) -> int:
    acceptance = owner
    before = workspace_snapshot(acceptance.workspace)
    first = acceptance.dispatch("fresh", acceptance.brief("fresh"))
    acceptance.wait_done("fresh", first)
    acceptance.collect("fresh-collect", first)
    acceptance.task_binding(first)
    first_session = acceptance.session_ref(first)
    if acceptance.scenario == "non-git":
        require(not has_git_ancestor(acceptance.workspace),
                "fresh execution created a Git repository")
    first_record = task_digest_snapshot(acceptance.records["fresh-collect"]["directory"])
    first_check: bytes | None = None
    if acceptance.mode == "workspace-write":
        require(acceptance.nonce is not None, "write acceptance nonce is unavailable")
        verify_write_fixture(acceptance.nonce, acceptance.fixture_file)
        verify_write_artifacts(acceptance.nonce, acceptance.target_file, acceptance.check_file)
        acceptance._workspace_write_snapshot(continuation=False)
        first_check = acceptance.check_file.read_bytes()
    second = acceptance.continue_task("resume", acceptance.brief("resume", continuation=True), first)
    acceptance.wait_done("resume", second)
    acceptance.collect("resume-collect", second)
    acceptance.task_binding(second, first, first_session)
    second_session = acceptance.session_ref(second)
    require_same_session(first_session, second_session)
    if acceptance.scenario == "non-git":
        require(not has_git_ancestor(acceptance.workspace),
                "continuation created a Git repository")
    require_replay_unchanged(first_record,
                             task_digest_snapshot(acceptance.records["fresh-collect"]["directory"]))
    if acceptance.mode == "workspace-write":
        require(acceptance.nonce is not None and first_check is not None,
                "write acceptance evidence is unavailable")
        verify_write_artifacts(acceptance.nonce, acceptance.target_file, acceptance.continue_check_file,
                               continuation=True)
        acceptance._workspace_write_snapshot(continuation=True)
        require(acceptance.check_file.read_bytes() == first_check,
                "continuation changed the first shell digest artifact")
    else:
        require(workspace_snapshot(acceptance.workspace) == before,
                "read-only live acceptance changed the workspace")
    acceptance.replay()
    acceptance.shutdown()
    acceptance.success_receipt = {
        "gate": "acceptance-native", "status": "passed", "provider": acceptance.provider,
        "mode": acceptance.mode, "task_budget": TASK_BUDGET,
        "canonical_task_budget": CANONICAL_TASK_BUDGET,
        "requested_model": getattr(acceptance.args, "model", None),
        "requested_effort": getattr(acceptance.args, "effort", None),
        "model_discovery": acceptance.model_discovery_summary,
        "native_mode": acceptance.native_mode_receipt(),
        **acceptance.provider_runtime_receipt(),
        "tasks": {
            "fresh": acceptance.tasks["fresh"],
            "resume": acceptance.tasks["resume"],
        },
        "provider_sessions": {"fresh": first_session, "resume": second_session},
        "scenario": acceptance.scenario, "provider_launches": 2, "replay_launches": 0,
        "credentials_in_receipt": False,
        "natural_daemon_shutdown": acceptance.closed,
    }
    return 0
