"""Private CLI handoff journal boundary."""

from pathlib import Path
from scripts.delegate_acceptance.directories.policy import require
import json


def read_external_inspection_handoff_journal(state, private_config, *, supervisor_status):
    """Retain edge evidence after the CLI validates the current journal head."""
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
        and supervisor_status.get("root_id") == legacy.get("root_id")
        and isinstance(control, dict)
        and control.get("action") == "status"
        and (control.get("state"), control.get("migration_phase")) in (("migrated", "complete"), ("ready", "adopted"))
        and control.get("next_action") == "none",
        "CLI did not validate the completed private inspection handoff",
    )
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
