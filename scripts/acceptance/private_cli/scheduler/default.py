"""Fresh native-default acceptance; existing fixture owners retain all cleanup."""

from dataclasses import replace
from pathlib import Path
import time
import uuid

from scripts.acceptance.evidence.validation import require
from scripts.acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.acceptance.private_cli.fixture_records import FixtureInstallation
from scripts.acceptance.private_cli.packaged_defaults import isolated_default_state_path
from scripts.acceptance.private_cli.scheduler.cases import SchedulerCase
from scripts.acceptance.private_cli.scheduler.host import SchedulerHost
from scripts.acceptance.private_cli.scheduler.occurrences import MINUTE_NS
from scripts.acceptance.private_cli.scheduler.occurrences import identity
from scripts.acceptance.private_cli.scheduler.occurrences import json_object
from scripts.acceptance.private_cli.scheduler.occurrences import require_occurrences
from scripts.acceptance.private_cli.scheduler.occurrences import timestamp_ns
from scripts.acceptance.private_cli.scheduler.sequence import await_release
from scripts.acceptance.private_cli.scheduler.sequence import history_occurrences
from scripts.acceptance.private_cli.scheduler.sequence import require_public_result
from scripts.acceptance.processes.observation import wait_until


def bind_default_fixture(fixture: FixtureInstallation, lifetime: FixtureLifetime) -> FixtureInstallation:
    """Retarget primary cleanup before launch; keep every existing directory token."""
    if lifetime.base is None:
        raise RuntimeError("default-root fixture lost its owned profile")
    home = Path(lifetime.env["HOME"]).resolve(strict=True)
    require(home == lifetime.base / "home", "default-root HOME is outside the owned fixture")
    # os.UserConfigDir honors XDG_CONFIG_HOME on Linux. Never inherit a user's
    # config root; this is the same owned context configured by prepare_fixture.
    lifetime.env["XDG_CONFIG_HOME"] = str(home / ".config")
    state = isolated_default_state_path(home)
    require(not state.exists() and not state.is_symlink(), "native default state was not fresh")
    require(bool(lifetime.managed_state_roots), "default-root fixture lost its primary cleanup binding")
    primary, task_ids = lifetime.managed_state_roots[0]
    require(primary == fixture.state and task_ids is lifetime.attempted_task_ids,
            "default-root fixture has a different primary cleanup owner")
    lifetime.managed_state_roots[0] = (state, task_ids)
    lifetime.private_config = state / ".supervisor" / "pueue.yml"
    return replace(fixture, state=state)


def schedules(case: SchedulerCase) -> list[dict[str, object]]:
    response = case.command("default-list", ["schedule", "list"])
    rows = response.get("schedules")
    if not isinstance(rows, list):
        raise RuntimeError("default-root schedule list omitted its definitions")
    return [json_object(row) for row in rows]


def delete_default_schedule(case: SchedulerCase) -> None:
    """Disable the exact pre-registered definition, including uncertain create."""
    if not case.schedule_id:
        return
    rows = schedules(case)
    matching = [row for row in rows if row.get("id") == case.schedule_id]
    if not matching:
        return  # A refused create persisted no definition in this fresh root.
    require(len(matching) == 1, "default-root schedule identity was duplicated")
    if matching[0].get("state") != "deleted":
        response = case.command("default-delete", ["schedule", "delete", case.schedule_id])
        require(json_object(response.get("schedule")).get("id") == case.schedule_id,
                "default-root delete changed schedule identity")
    shown = json_object(case.command("default-deleted", ["schedule", "show", case.schedule_id]).get("schedule"))
    require(shown.get("id") == case.schedule_id and shown.get("state") == "deleted",
            "default-root schedule remains enabled after deletion")


def run_fresh_default(case: SchedulerCase) -> None:
    require(case.default_root and isinstance(case.service, SchedulerHost) and case.service.default_root,
            "fresh-default requires ordinary commands and a managed host")
    require(not case.fixture.state.exists(), "serve did not start with absent native default storage")
    case.cold()
    case.receipt.phase("fresh_default_serve")
    case.service.start()
    observed = case.command("default-ready", ["scheduler", "status"])
    case.root_id = identity(observed.get("root_id"))
    scheduler = json_object(observed.get("scheduler"))
    require(scheduler.get("state") == "ready" and scheduler.get("ready") is True,
            "empty native-default scheduler is not ready")
    require(not schedules(case) and not case.discover(), "fresh default already contains scheduler work")
    case.receipt.add_details(default_root={"state": str(case.fixture.state), "root_id": case.root_id,
                                           "host_cwd": str(case.service.directory),
                                           "caller_cwd": str(case.fixture.workspace),
                                           "ready_before_first_definition": True})
    wait_until(lambda: time.time_ns() % MINUTE_NS < 20_000_000_000,
               timeout=case.remaining(), description="default-root real schedule creation window")
    # Register before create: a lost response must not lose deletion authority.
    case.schedule_id = uuid.uuid4().hex
    case.receipt.phase("fresh_default_create")
    created = case.command("default-create", [
        "schedule", "create", "--id", case.schedule_id, "--provider", "pi:json",
        "--cron", "* * * * *", "--timezone", "UTC", "--brief", str(case.fixture.brief),
        "--cwd", str(case.fixture.workspace), "--permission", "read-only", "--budget", "4m",
    ])
    saved = json_object(created.get("schedule"))
    require(saved.get("id") == case.schedule_id and saved.get("saved") is True,
            "default-root schedule was not saved with its registered ID")
    require(json_object(created.get("scheduler")).get("ready") is True,
            "create did not reach the existing ready host")
    shown = json_object(case.command("default-show", ["schedule", "show", case.schedule_id]).get("schedule"))
    case.first_due_ns = (timestamp_ns(shown.get("created_at")) // MINUTE_NS + 1) * MINUTE_NS
    require(time.time_ns() < case.first_due_ns, "default-root create missed its real-minute window")
    require([row.get("id") for row in schedules(case)] == [case.schedule_id],
            "another cwd observed a different schedule inventory")
    row, = case.occurrences(1)
    delete_default_schedule(case)
    require(time.time_ns() < case.first_due_ns + MINUTE_NS,
            "default-root definition was not deleted before the second tick")
    require_occurrences((row,), case.schedule_id, (case.first_due_ns,))
    case.collect(row)
    await_release(case, row)
    require_public_result(case, row, "committed")
    history = history_occurrences(case.command("default-history", ["schedule", "history", case.schedule_id]))
    require(len(history) == 1 and history[0].get("task_id") == row.task_id
            and timestamp_ns(history[0].get("due")) == case.first_due_ns,
            "default-root public history changed the single occurrence")
    require_occurrences(case.discover(), case.schedule_id, (case.first_due_ns,))
    case.receipt.add_details(actual_minute_ticks=1, task_id=row.task_id,
                             schedule_id=case.schedule_id, definition_deleted=True)
