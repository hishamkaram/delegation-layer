"""Pure modern cross-binary lease and immutable-record verdicts."""

from dataclasses import dataclass
from pathlib import Path

from scripts.acceptance.paths.policy import _raw_clean_absolute_path
from scripts.acceptance.evidence.validation import _hex_digest


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
DAEMON_IDENTITY_FIELDS = ("executable", "executable_identity", "sha256", *LOGICAL_IDENTITY_FIELDS)
INSTALLED_IDENTITY = "installed-path-v1"


def _require_executable_identity(record: dict[str, object], *, binding: bool) -> None:
    marker = record.get("executable_identity")
    hashes = ("client_sha256", "daemon_sha256") if binding else ("sha256",)
    if marker == INSTALLED_IDENTITY:
        if any(record.get(field) for field in hashes):
            raise RuntimeError("modern installed-path identity contains an executable digest")
    elif marker not in (None, "") or any(not _hex_digest(record.get(field)) for field in hashes):
        raise RuntimeError("modern executable identity marker or historical record shape is invalid")
    paths = (("client_executable", "pueue"), ("daemon_executable", "pueued")) if binding else (("executable", "pueued"),)
    for field, name in paths:
        value = record.get(field)
        if (not _raw_clean_absolute_path(value) or any(ord(char) < 32 or ord(char) == 127 for char in value)
                or Path(value).name != name):
            raise RuntimeError("modern executable identity does not name its installed role")


def _validated_migration_facts(
    facts: dict[str, object],
) -> tuple[dict[str, object], dict[str, object]]:
    """Validate installed selections and canonical records; grant no lifecycle mutation authority."""
    published, daemon, writer = (facts.get(name) for name in ("published", "daemon", "writer"))
    if not isinstance(published, dict) or not isinstance(daemon, dict) or not isinstance(writer, dict):
        raise RuntimeError("modern migration omitted published or resolved writer identity")
    _require_executable_identity(published, binding=True)
    _require_executable_identity(daemon, binding=False)
    _require_executable_identity(writer, binding=False)
    if writer.get("executable_identity") != INSTALLED_IDENTITY:
        raise RuntimeError("modern migration writer must select an installed-path identity")
    for field in LOGICAL_IDENTITY_FIELDS:
        if (not isinstance(published.get(field), str) or not published[field]
                or daemon.get(field) != published[field]
                or not isinstance(writer.get(field), str) or not writer[field]):
            raise RuntimeError("modern migration published logical identity is inconsistent")
    if (daemon.get("executable") != published.get("daemon_executable")
            or daemon.get("executable_identity") != published.get("executable_identity")
            or daemon.get("sha256") != published.get("daemon_sha256")):
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
        for field in ("executable_identity", "client_executable", "client_sha256", "daemon_executable", "daemon_sha256"):
            if saved.get(field) not in (None, "", published.get(field)):
                raise RuntimeError("modern migration recovery alias changed executable provenance")
        if alias.get("config_sha256") != facts.get("alias_config_sha256"):
            raise RuntimeError("modern migration recovery alias config digest changed")
    elif facts.get("alias_config_sha256") is not None:
        raise RuntimeError("modern migration has an orphaned recovery alias config")
    return daemon, writer


def require_migration_facts(facts: dict[str, object]) -> None:
    """Assert this scenario's inputs; this grants no lifecycle mutation authority."""
    _validated_migration_facts(facts)


def migration_expectation(facts: dict[str, object], *, active: bool) -> MigrationExpectation:
    daemon, writer = _validated_migration_facts(facts)
    same = all(daemon.get(field) == writer.get(field) for field in
               ("executable", "executable_identity", *LOGICAL_IDENTITY_FIELDS))
    pending = facts.get("migration")
    if pending is not None:
        if not isinstance(pending, dict) or type(pending.get("schema_version")) is not int or pending["schema_version"] != 1:
            raise RuntimeError("modern migration journal has an invalid schema")
        flags = (pending.get(field, False) for field in
                 ("legacy_shutdown_requested", "legacy_shutdown_dispatched", "legacy_stopped"))
        if any(type(flag) is not bool or flag for flag in flags) or pending.get("legacy_pid", 0) != 0 or pending.get("legacy_process_start", ""):
            raise RuntimeError("modern migration crossed its prepared-only phase")
        legacy, successor = pending.get("legacy"), pending.get("successor")
        if not isinstance(legacy, dict) or any(legacy.get(field) != daemon.get(field) for field in DAEMON_IDENTITY_FIELDS):
            raise RuntimeError("modern migration journal changed predecessor provenance")
        if not isinstance(successor, dict):
            raise RuntimeError("modern migration successor identity is invalid")
        _require_executable_identity(successor, binding=False)
        if any(successor.get(field) != writer.get(field) for field in ("executable", *LOGICAL_IDENTITY_FIELDS)):
            raise RuntimeError("modern migration writer differs from the recorded prepared successor; separate negative control required")
    if same and (pending is not None or facts.get("alias") is not None):
        raise RuntimeError("modern baseline prepared/alias writer needs a separate negative control")
    if same:
        return MigrationExpectation("healthy_same_generation", 0, "migrated", "present" if active else "none", "retry_dispatch", migration_phase="complete")
    if active:
        return MigrationExpectation("active_provenance_change", 1, "blocked", "present", "wait_for_legacy_work", reason_code="active_work")
    return MigrationExpectation("quiescent_provenance_change", 0, "migrated", "none", "retry_dispatch", migration_phase="complete")


def require_migration_response(response: object, root_id: str, expected: MigrationExpectation) -> None:
    if not isinstance(response, dict):
        raise RuntimeError("modern control changed root identity")
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
    shared_entered, exclusive_entered = shared["entered_ns"], exclusive["entered_ns"]
    if (type(shared_entered) is not int or type(exclusive_entered) is not int or
            not shared_entered < shared_release <= exclusive_entered < exclusive_release):
        raise RuntimeError("modern exclusive boundary crossed the shared release order")
