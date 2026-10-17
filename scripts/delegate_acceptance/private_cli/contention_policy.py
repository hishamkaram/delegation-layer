"""Pure modern cross-binary lease and immutable-record verdicts."""

from dataclasses import dataclass
from pathlib import Path


MODERN_CONTENTION_SCENARIO = "modern-two-binary-contention"
MODERN_CONTENTION_TASK_COUNT = 2
MODERN_CONTENTION_TASK_BUDGET = "5m"


@dataclass(frozen=True)
class ModernInstallation:
    delegate: Path
    runner: Path
    libexec: Path


@dataclass(frozen=True)
class MigrationExpectation:
    case: str
    exit_code: int
    state: str
    active_work: str
    next_action: str
    reason_code: str | None = None
    migration_phase: str | None = None


LOGICAL_IDENTITY_FIELDS = ("config_path", "config_digest", "resolved_config_sha256", "endpoint")
DAEMON_IDENTITY_FIELDS = ("executable", "sha256", *LOGICAL_IDENTITY_FIELDS)


def require_migration_facts(facts: dict[str, object]) -> None:
    """Assert this scenario's inputs; this grants no lifecycle mutation authority."""
    published, daemon, writer = (facts.get(name) for name in ("published", "daemon", "writer"))
    if not all(isinstance(value, dict) for value in (published, daemon, writer)):
        raise RuntimeError("modern migration omitted published or resolved writer identity")
    for field in LOGICAL_IDENTITY_FIELDS:
        if not isinstance(published.get(field), str) or not published[field] or daemon.get(field) != published[field]:
            raise RuntimeError("modern migration published logical identity is inconsistent")
    for field in ("executable", "sha256"):
        if daemon.get(field) != published.get("daemon_" + field) or not isinstance(writer.get(field), str):
            raise RuntimeError("modern migration published daemon provenance is inconsistent")
    if facts.get("unsettled_records"):
        raise RuntimeError("modern migration has an unsupported pending publication or bootstrap")
    alias = facts.get("alias")
    if alias is not None:
        if not isinstance(alias, dict) or type(alias.get("schema_version")) is not int or alias["schema_version"] != 1:
            raise RuntimeError("modern migration recovery alias has an invalid schema")
        saved = alias.get("supervisor")
        if not isinstance(saved, dict) or any(saved.get(field) != published[field] for field in LOGICAL_IDENTITY_FIELDS):
            raise RuntimeError("modern migration recovery alias does not name the published predecessor")
        for field in ("client_executable", "client_sha256", "daemon_executable", "daemon_sha256"):
            if saved.get(field) not in (None, "", published.get(field)):
                raise RuntimeError("modern migration recovery alias changed executable provenance")
        if alias.get("config_sha256") != facts.get("alias_config_sha256"):
            raise RuntimeError("modern migration recovery alias config digest changed")
    elif facts.get("alias_config_sha256") is not None:
        raise RuntimeError("modern migration has an orphaned recovery alias config")


def migration_expectation(facts: dict[str, object], *, active: bool) -> MigrationExpectation:
    require_migration_facts(facts)
    daemon, writer = facts["daemon"], facts["writer"]
    same = all(daemon[field] == writer[field] for field in ("executable", "sha256"))
    pending = facts.get("migration")
    if pending is not None:
        if not isinstance(pending, dict) or type(pending.get("schema_version")) is not int or pending["schema_version"] != 1:
            raise RuntimeError("modern migration journal has an invalid schema")
        flags = (pending.get(field, False) for field in
                 ("legacy_shutdown_requested", "legacy_shutdown_dispatched", "legacy_stopped"))
        if any(type(flag) is not bool or flag for flag in flags) or pending.get("legacy_pid", 0) != 0 or pending.get("legacy_process_start", ""):
            raise RuntimeError("modern migration crossed its prepared-only phase")
        legacy, successor = pending.get("legacy"), pending.get("successor")
        if not isinstance(legacy, dict) or any(legacy.get(field) != daemon[field] for field in DAEMON_IDENTITY_FIELDS):
            raise RuntimeError("modern migration journal changed predecessor provenance")
        if not isinstance(successor, dict) or any(successor.get(field) != writer[field] for field in DAEMON_IDENTITY_FIELDS):
            raise RuntimeError("modern migration writer differs from the recorded prepared successor; separate negative control required")
    if same and (pending is not None or facts.get("alias") is not None):
        raise RuntimeError("modern baseline prepared/alias writer needs a separate negative control")
    if same:
        return MigrationExpectation("healthy_same_generation", 0, "migrated", "present" if active else "none", "retry_dispatch", migration_phase="complete")
    if active:
        return MigrationExpectation("active_provenance_change", 1, "blocked", "present", "wait_for_legacy_work", reason_code="active_work")
    return MigrationExpectation("quiescent_provenance_change", 0, "migrated", "none", "retry_dispatch", migration_phase="complete")


def require_migration_response(response: object, root_id: str, expected: MigrationExpectation) -> None:
    require_control_response(response, root_id, migration_state=expected.state)
    if type(response.get("schema_version")) is not int or response["schema_version"] != 1 or response.get("command") != "supervisor":
        raise RuntimeError("modern migration returned an invalid command envelope")
    control = response["supervisor_control"]
    fields = {"action": "migrate", "active_work": expected.active_work, "next_action": expected.next_action,
              "reason_code": expected.reason_code, "migration_phase": expected.migration_phase}
    if any(control.get(field) != value for field, value in fields.items()):
        raise RuntimeError("modern migration response differs from the observed " + expected.case + " contract")
    error = response.get("error")
    busy = "Legacy supervisor has active work. Wait for queued or running work before migrating."
    if (expected.exit_code == 0 and error) or (expected.exit_code == 1 and error != busy):
        raise RuntimeError("modern migration returned an unexpected error for " + expected.case)


def require_lease_mode(mode: str, shared_acquired: bool, exclusive_acquired: bool) -> None:
    expected = {"shared": (True, False), "exclusive": (False, False), "unlocked": (True, True)}
    if (
        type(shared_acquired) is not bool
        or type(exclusive_acquired) is not bool
        or mode not in expected
        or (shared_acquired, exclusive_acquired) != expected[mode]
    ):
        raise RuntimeError("modern control did not retain its exact " + mode + " bootstrap lease")


def require_content_identity(before: object, after: object, phase: str) -> None:
    if before != after:
        raise RuntimeError("modern contention changed immutable root/task content during " + phase)


def require_boundary_identity(receipt: object, nonce: str, cli_pid: int, config: str) -> dict[str, object]:
    if (
        not isinstance(receipt, dict)
        or (type(receipt.get("schema_version")) is not int or receipt["schema_version"] != 1)
        or receipt.get("nonce") != nonce
    ):
        raise RuntimeError("modern client boundary belongs to another fixture invocation")
    pids = [receipt.get(key) for key in ("cli_pid", "control_owner_pid", "client_pid")]
    if any(type(pid) is not int or pid <= 0 for pid in pids) or pids[0] != cli_pid or len(set(pids)) != 3:
        raise RuntimeError("modern client boundary omitted its distinct CLI/owner/client identities")
    if type(receipt.get("entered_ns")) is not int or receipt["entered_ns"] <= 0:
        raise RuntimeError("modern client boundary omitted its monotonic entry observation")
    if receipt.get("command") != "status-json":
        raise RuntimeError("modern client boundary did not enter the leased status command")
    arguments = receipt.get("arguments")
    if (
        not isinstance(arguments, list)
        or not all(isinstance(arg, str) for arg in arguments)
        or len(arguments) < 2
        or arguments[0] != "status"
        or "--json" not in arguments[1:]
        or receipt.get("config") != config
    ):
        raise RuntimeError("modern client boundary did not enter the exact root JSON status command")
    return receipt


def require_control_response(
    response: object, root_id: str, *, task_id: str | None = None, migration_state: str | None = None
) -> None:
    if not isinstance(response, dict) or response.get("root_id") != root_id:
        raise RuntimeError("modern control changed root identity")
    if task_id is not None and response.get("task_id") != task_id:
        raise RuntimeError("modern status changed task identity")
    if task_id is not None and response.get("error"):
        raise RuntimeError("modern status returned an operational error")
    if migration_state is not None:
        control = response.get("supervisor_control")
        if not isinstance(control, dict) or control.get("state") != migration_state:
            raise RuntimeError("modern migration did not report " + migration_state)


def require_same_lock(*observations: dict[str, object]) -> None:
    identities = {(value["path"], value["device"], value["inode"], value["file_mode"]) for value in observations}
    if len(identities) != 1:
        raise RuntimeError("modern contention changed its bootstrap lock identity or file mode")


def require_boundary_order(
    shared: dict[str, object], shared_release: int, exclusive: dict[str, object], exclusive_release: int
) -> None:
    if not shared["entered_ns"] < shared_release <= exclusive["entered_ns"] < exclusive_release:
        raise RuntimeError("modern exclusive boundary crossed the shared release order")
