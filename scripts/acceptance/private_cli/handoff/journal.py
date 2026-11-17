"""Private CLI handoff journal boundary."""

from pathlib import Path
from scripts.acceptance.directories.policy import require
from scripts.acceptance.evidence.validation import _task_identity
import json


def read_external_inspection_handoff_journal(
    state, private_config, *, supervisor_status, expected_external_task_id: str,
    expected_private_task_id: str | None = None,
):
    """Retain edge evidence after the CLI validates the current journal head."""
    require(_task_identity(expected_external_task_id), "expected external task identity is invalid")
    require(
        expected_private_task_id is None
        or (_task_identity(expected_private_task_id) and expected_private_task_id != expected_external_task_id),
        "expected private task identity is invalid",
    )
    root = Path(state)
    journal = root / "inspection-group"
    request_path = journal / "request.json"
    migration_path = journal / "migration-complete.json"
    require(request_path.is_file() and not request_path.is_symlink(), "external inspection journal request is missing")
    legacy = json.loads(request_path.read_text(encoding="utf-8"))
    require(isinstance(legacy, dict), "external inspection journal request is invalid")
    require(
        migration_path.is_file() and not migration_path.is_symlink(), "inspection handoff completion marker is missing"
    )
    migration = json.loads(migration_path.read_text(encoding="utf-8"))
    require(isinstance(migration, dict), "inspection handoff completion marker is invalid")
    require(isinstance(supervisor_status, dict), "canonical supervisor status is missing")
    control = supervisor_status.get("supervisor_control")
    require(
        supervisor_status.get("command") == "supervisor"
        and supervisor_status.get("failure") is None
        and supervisor_status.get("error") is None
        and supervisor_status.get("root_id") == legacy.get("root_id")
        and isinstance(control, dict)
        and control.get("action") == "status"
        and (control.get("state"), control.get("migration_phase")) in (("migrated", "complete"), ("ready", "adopted")),
        "CLI did not validate the completed private inspection handoff",
    )
    _require_expected_maintenance(control, expected_external_task_id, expected_private_task_id)
    successor = migration.get("successor")
    require(isinstance(successor, dict), "inspection handoff omitted its successor")
    legacy_supervisor = legacy.get("supervisor")
    successor_supervisor = successor.get("supervisor")
    require(
        isinstance(legacy_supervisor, dict) and isinstance(successor_supervisor, dict),
        "inspection handoff omitted supervisor bindings",
    )
    require(
        legacy_supervisor.get("config_path") != str(private_config), "external inspection journal was already private"
    )
    require(
        successor_supervisor.get("config_path") == str(private_config),
        "inspection handoff successor is not the private state-root supervisor",
    )
    return {"legacy": legacy, "migration": migration, "successor": successor, "canonical_status": control}


def _require_expected_maintenance(control, external_task_id: str, private_task_id: str | None):
    """Completed routing can coexist with the fixture's exact still-live task."""
    blocker = control.get("first_blocker")
    require(isinstance(blocker, dict), "handoff maintenance blocker is missing")
    expected_blocker = {
        "kind": "task", "task_id": external_task_id,
        "reason_code": "owner_busy", "next_action": "wait_for_legacy_work",
    }
    activity = "none"
    if private_task_id is not None:
        queue_state = blocker.get("queue_state")
        require(queue_state in ("queued", "stashed", "running"), "handoff maintenance private queue state is not active")
        expected_blocker = {
            "kind": "queue", "task_id": private_task_id, "queue_state": queue_state,
            "reason_code": "queue_work_present", "next_action": "wait_for_legacy_work",
        }
        activity = "present"
    require(
        control.get("migration_readiness") == "blocked"
        and control.get("active_work") == activity
        and blocker == expected_blocker
        and control.get("next_action") == expected_blocker["next_action"],
        "handoff maintenance does not match the expected live task",
    )
