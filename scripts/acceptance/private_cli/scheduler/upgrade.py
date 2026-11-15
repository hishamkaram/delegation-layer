"""Two released-writer to candidate scheduler upgrades using canonical task owners."""

import argparse
from dataclasses import dataclass, replace
import json
from pathlib import Path
import shlex
import sqlite3
import time
import uuid

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.acceptance.private_cli.fixture_setup import prepare_fixture
from scripts.acceptance.private_cli.fingerprints import REPOSITORY
from scripts.acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.acceptance.private_cli.scheduler.acceptance import SchedulerArguments, finish_case, output_directory
from scripts.acceptance.private_cli.scheduler.cases import SchedulerCase
from scripts.acceptance.private_cli.scheduler.host import SchedulerHost
from scripts.acceptance.private_cli.scheduler.occurrences import MINUTE_NS, Occurrence, identity, json_object, occurrence_read_busy, timestamp_ns
from scripts.acceptance.private_cli.scheduler.releases import RELEASES, Release, ReleaseInstaller
from scripts.acceptance.private_cli.scheduler.sequence import await_release, history_occurrences, prepare_failure_hook
from scripts.acceptance.private_cli.scheduler.upgrade_snapshot import snapshot, require_preserved, require_runtime_preserved, scheduler_files
from scripts.acceptance.private_cli.scheduler.upgrade_evidence import verify_upgrade_output
from scripts.acceptance.processes.observation import wait_until


UPGRADE_SECONDS = 720


@dataclass(frozen=True)
class UpgradeArguments(SchedulerArguments):
    baseline: str = "all"
    baseline_pair_installer: ReleaseInstaller | None = None
    candidate_version: str = "dev"


def parse_arguments(argv: list[str] | None = None) -> UpgradeArguments:
    parser = argparse.ArgumentParser(prog="acceptance-scheduler-upgrade")
    parser.add_argument("--source-root", type=Path, default=REPOSITORY)
    parser.add_argument("--pueue", type=Path, default=REPOSITORY / "bin/test-supervisor/pueue")
    parser.add_argument("--pueued", type=Path, default=REPOSITORY / "bin/test-supervisor/pueued")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--baseline", choices=("all", "0.4.2", "0.5.1"), default="all")
    args = parser.parse_args(argv)
    metadata = json_object(json.loads((args.source_root / "package.json").read_text()))
    version = metadata.get("version")
    if not isinstance(version, str):
        raise RuntimeError("candidate package version is absent")
    return UpgradeArguments(args.source_root, args.pueue, args.pueued, args.output,
                            baseline=args.baseline, candidate_version=version, scenario="scheduler-upgrade")


def install_entry_counter(case: SchedulerCase) -> Path:
    """Instrument only this newly generated finite provider fixture, not user tools."""
    if case.lifetime.base is None:
        raise RuntimeError("upgrade provider counter lost its fixture")
    directory = case.lifetime.ownership.create("scheduler provider entry counter", case.lifetime.base / "counted-provider",
                                               case.lifetime.base).directory
    entries = directory / "entries"
    entries.write_text("")
    entries.chmod(0o600)
    script = ("#!/bin/sh\nset -eu\n"
              "for arg do\n if [ \"$arg\" = --mode ]; then\n"
              " printf '%s\\n' entry >> " + shlex.quote(str(entries)) + "\n break\n fi\ndone\n"
              "exec " + shlex.quote(str(case.fixture.provider_bin / "pi")) + " \"$@\"\n")
    wrapper = directory / "pi"
    wrapper.write_text(script)
    wrapper.chmod(0o700)
    case.lifetime.env["PATH"] = str(directory) + ":" + case.lifetime.env["PATH"]
    # Host registries copy the environment at creation; before any Start, create
    # their owner with the final finite fixture environment.
    case.service = SchedulerHost(case.lifetime, case.fixture.state, case.delegate, case.deadline)
    return entries


def entry_count(path: Path) -> int:
    require(path.stat().st_size <= 4096, "provider entry counter exceeded bound")
    lines = path.read_text().splitlines()
    require(all(line == "entry" for line in lines), "provider entry counter contains unexpected data")
    return len(lines)


def create_definition(case: SchedulerCase, name: str, *, held: bool = False, paused: bool = False,
                      failure_hook: Path | None = None) -> str:
    registered = uuid.uuid4().hex
    # Register all possible definitions before a possibly-sent create. Cleanup
    # disables them through the same generation-selected CLI.
    case.upgrade_definitions.append(registered)
    brief = case.brief(held=held, controlled_failure=False) if held else case.fixture.brief
    hook_arguments = [] if failure_hook is None else ["--failure-hook", str(failure_hook), "--failure-threshold", "1"]
    response = case.command("create-" + name, [
        "schedule", "create", "--id", registered, "--provider", "pi:json", "--cron", "* * * * *",
        "--timezone", "UTC", "--brief", str(brief), "--cwd", str(case.fixture.workspace),
        "--permission", "read-only", "--budget", "4m",
    ] + hook_arguments, (0, 3))
    root_id = identity(response.get("root_id"))
    require(not case.root_id or case.root_id == root_id, "upgrade definitions changed root identity")
    case.root_id = root_id
    shown = json_object(response.get("schedule"))
    require(shown.get("id") == registered, "upgrade create changed registered definition ID")
    if paused:
        case.command("pause-" + name, ["schedule", "pause", registered])
    return registered


def rows_for(case: SchedulerCase, schedule: str, count: int):
    def ready():
        try:
            rows = tuple(row for row in case.discover() if row.schedule_id == schedule)
        except sqlite3.OperationalError as error:
            if occurrence_read_busy(error):
                return None
            raise
        require(len(rows) <= count, "upgrade produced an extra occurrence or replay")
        return rows if len(rows) == count else None
    return wait_until(ready, timeout=case.remaining(), description="upgrade real-minute occurrence inventory")


def collect_schedule(case: SchedulerCase, schedule: str, count: int):
    case.schedule_id = schedule
    rows = rows_for(case, schedule, count)
    case.collect(rows[-1])
    await_release(case, rows[-1])
    return rows


def delete_definitions(case: SchedulerCase) -> None:
    if not case.root_id:
        return
    response = case.command("cleanup-list", ["schedule", "list"])
    listed = response.get("schedules")
    if not isinstance(listed, list):
        raise RuntimeError("upgrade cleanup cannot inventory definitions")
    known = set(case.upgrade_definitions)
    for row in listed:
        definition = json_object(row)
        schedule_id = identity(definition.get("id"))
        require(schedule_id in known, "upgrade fixture contains an unowned definition")
        if definition.get("state") != "deleted":
            case.command("cleanup-delete", ["schedule", "delete", schedule_id])


def observe_doctor(case: SchedulerCase, label: str, expected: str) -> None:
    before = scheduler_files(case.fixture.state)
    # An intact owned installation has exit0. Scheduler compatibility remains
    # a separate passive advisory, including migration_required on schema1.
    response = case.command("doctor-" + label, ["doctor"])
    advisory = json_object(response.get("scheduler"))
    require(advisory.get("compatibility") == expected and advisory.get("verification") == "read_only",
            "candidate doctor returned the wrong scheduler advisory")
    issues = advisory.get("issues")
    require(issues == [] if expected == "compatible" else
            isinstance(issues, list) and len(issues) == 1 and json_object(issues[0]).get("code") == expected,
            "doctor scheduler advisory changed its issue policy")
    require(response.get("issues") == [] and scheduler_files(case.fixture.state) == before,
            "doctor changed scheduler evidence/sidecars or general exit policy")


def require_history_coverage(response: dict[str, object], rows: tuple[Occurrence, ...]) -> None:
    require(0 < len(rows) <= 256, "upgrade history occurrence inventory exceeded bound")
    observed = history_occurrences(response)
    expected = {row.task_id: row.due_ns for row in rows}
    require({identity(row.get("task_id")): timestamp_ns(row.get("due")) for row in observed} == expected
            and len(observed) == len(expected), "public history changed independent occurrence identities")
    history = json_object(response.get("schedule_history"))
    entries = history.get("entries")
    if not isinstance(entries, list):
        raise RuntimeError("public history omitted skip inventory")
    spans = [json_object(json_object(entry).get("skip")) for entry in entries
             if json_object(entry).get("kind") == "skip"]
    require(any(span.get("reason") == "paused" for span in spans), "public history omitted preserved paused coverage")
    dues = set(expected.values())
    require(max(dues) - min(dues) <= 256 * MINUTE_NS, "upgrade history gap inventory exceeded bound")
    for due in range(min(dues), max(dues) + 1, MINUTE_NS):
        if due not in dues:
            require(any(timestamp_ns(span.get("from")) < due <= timestamp_ns(span.get("to")) for span in spans),
                    "public history omitted a skipped historical minute")


def transition(case: SchedulerCase, before, release: Release, *, candidate_version: str) -> None:
    """The one generation authority changes only after old hosting and tasks join."""
    require(case.service.stopped, "upgrade cannot change generation before old host Wait")
    case.previous_services.append(case.service)
    case.upgraded = True
    case.lifetime.delegate = case.delegate
    # Installed selection is explicit for command, host, restart and finalizer.
    case.service = SchedulerHost(case.lifetime, case.fixture.state, case.delegate, case.deadline,
                                 generation=len(case.previous_services))
    doctor_before = "migration_required" if release.schema == 1 else "compatible"
    observe_doctor(case, "before-migration", doctor_before)
    case.receipt.phase("candidate_explicit_scheduler_migrate")
    migrated = case.command("scheduler-migrate", ["scheduler", "migrate"])
    require(not migrated.get("error") and not migrated.get("failure"), "candidate scheduler migration failed")
    require(json_object(migrated.get("scheduler")).get("state") == "migrated"
            and migrated.get("root_id") == case.root_id, "candidate did not acknowledge exact scheduler migration")
    observe_doctor(case, "after-migration", "compatible")
    after = snapshot(case.fixture.state, case.root_id)
    require(before.version == release.schema, "old release wrote an unexpected scheduler schema")
    require_preserved(before, after)
    case.receipt.add_details(scheduler_upgrade={"baseline": release.version, "candidate": candidate_version,
                            "from_schema": before.version, "to_schema": after.version,
                            "explicit_migration": True, "preservation_before_new_runtime": True,
                            "old_host_joined": True, "old_tasks_closed": True,
                            "doctor_before": doctor_before, "doctor_after": "compatible", "doctor_read_only": True,
                            "reliability_state_preserved": True})
    # Existing runtime binding migration owns the bundled supervisor retarget.
    migrated_supervisor = case.command("supervisor-migrate", ["supervisor", "migrate"])
    require(not migrated_supervisor.get("error") and not migrated_supervisor.get("failure"),
            "candidate supervisor migration failed")


def exercise_upgrade(case: SchedulerCase, release: Release, output: Path, *, candidate_version: str) -> None:
    entries = install_entry_counter(case)
    case.receipt.phase("released_baseline_definitions")
    wait_until(lambda: time.time_ns() % MINUTE_NS < 15_000_000_000,
               timeout=case.remaining(), description="old release creation before real UTC tick")
    completed = create_definition(case, "completed")
    hook = prepare_failure_hook(case) if release.schema == 2 else None
    paused = create_definition(case, "paused", paused=True, failure_hook=hook)
    if release.schema == 2:
        case.command("baseline-enabled-quota-policy", ["schedule", "policy", "set", paused,
                     "--quota-hold", "on", "--quota-max-hold", "2m"])
    uncertain = create_definition(case, "held-unobserved", held=True, paused=True)
    case.schedule_id = completed
    case.receipt.phase("released_baseline_completed_tick")
    case.service.start()
    completed_row, = collect_schedule(case, completed, 1)
    case.command("pause-completed-before-held", ["schedule", "pause", completed])
    case.command("resume-held-next-tick", ["schedule", "resume", uncertain])
    case.receipt.phase("released_baseline_held_tick")
    case.schedule_id = uncertain
    held_row, = rows_for(case, uncertain, 1)
    held_response = case.await_admitted(held_row)
    with case.held_lease(held_response) as lease:
        case.held(held_row, lease)
        # A second tick supplies historical paused/overlap skip spans, without
        # replacing real time, mutating a cursor, or fabricating a reservation.
        second_tick = held_row.due_ns + MINUTE_NS
        wait_until(lambda: time.time_ns() >= second_tick + 1_000_000_000,
                   timeout=case.remaining(), description="released baseline overlap/paused real tick")
        case.receipt.phase("released_baseline_stop_and_task_closure")
        case.service.stop()
        require(entry_count(entries) >= 2, "old release did not enter both finite providers")
        gate = case.lifetime.release_gate
        if gate is None:
            raise RuntimeError("held provider gate disappeared")
        gate.release()
        # Collect proves canonical outcome and closure, but the stopped old host
        # cannot observe/release its scheduler reservation.
        case.collect(held_row)
    old_rows = case.discover()
    require(all(row.schedule_id != paused for row in old_rows), "paused baseline definition launched")
    require({row.task_id for row in old_rows} == {held_row.task_id, completed_row.task_id},
            "baseline inventory differs from its two collected and canonically closed tasks")
    before = snapshot(case.fixture.state, case.root_id)
    if release.schema == 2:
        require(before.rows("schedule_hooks") and before.rows("schedule_policy") and before.rows("occurrence_results"),
                "released schema2 did not persist representative reliability state")
        require(all(not before.rows(name) for name in ("occurrence_attempts", "schedule_holds", "notification_attempts")),
                "successful released baseline unexpectedly persisted failure attempts, holds or notifications")
        columns, policies = next((columns, rows) for name, columns, rows in before.tables if name == "schedule_policy")
        require(any(row[columns.index("schedule_id")] == paused and row[columns.index("enabled")] == 1
                    and row[columns.index("max_hold")] == 120_000_000_000 for row in policies),
                "released schema2 did not retain the enabled nondefault quota policy")
    occurrence_table = next((columns, rows) for name, columns, rows in before.tables if name == "occurrences")
    columns, rows = occurrence_table
    held = next(row for row in rows if row[columns.index("task_id")] == held_row.task_id)
    require(held[columns.index("released_at_unix_nanos")] is None,
            "stopped baseline unexpectedly released the held/unobserved reservation")
    require(before.rows("skip_spans"), "baseline produced no historical skip spans")
    baseline_entries = entry_count(entries)
    require(baseline_entries == len(old_rows), "provider entries and independent old task IDs disagree")
    # The receipt stores counts/IDs; the private snapshot retains exact data.
    (output / "preservation-before.json").write_text(json.dumps({
        "schema": before.version, "root_id": before.root_id,
        "definitions": list(case.upgrade_definitions), "tasks": [row.task_id for row in old_rows],
        "skip_spans": len(before.rows("skip_spans")), "provider_entries": baseline_entries,
    }, indent=2) + "\n")
    completed_count = sum(row.schedule_id == completed for row in old_rows)
    transition(case, before, release, candidate_version=candidate_version)
    case.command("pause-unobserved-before-host", ["schedule", "pause", uncertain])
    case.receipt.phase("candidate_real_future_tick")
    wait_until(lambda: time.time_ns() % MINUTE_NS < 15_000_000_000,
               timeout=case.remaining(), description="candidate startup before future UTC tick")
    case.service.start()
    case.schedule_id = uncertain
    await_release(case, held_row)
    require(entry_count(entries) == baseline_entries, "candidate replayed a closed unobserved occurrence")
    # Pause the reconciled held schedule before enabling the completed stream.
    resume_bounds = [time.time_ns()]
    resumed = case.command("resume-candidate", ["schedule", "resume", completed])
    require(not resumed.get("failure"), "candidate could not resume preserved definition")
    future_rows = collect_schedule(case, completed, completed_count + 1)
    require(future_rows[-1].due_ns > resume_bounds[0], "candidate backfilled a tick at/before resume")
    case.command("pause-before-restart", ["schedule", "pause", completed])
    wait_until(lambda: time.time_ns() % MINUTE_NS < 15_000_000_000,
               timeout=case.remaining(), description="candidate restart before future UTC tick")
    case.restart()
    resume_bounds.append(time.time_ns())
    case.command("resume-after-restart", ["schedule", "resume", completed])
    final_rows = collect_schedule(case, completed, len(future_rows) + 1)
    require(final_rows[-1].due_ns > resume_bounds[1], "candidate restart backfilled a tick at/before resume")
    require_history_coverage(case.command("candidate-history-final", ["schedule", "history", completed]), final_rows)
    case.service.stop()
    require_runtime_preserved(before, snapshot(case.fixture.state, case.root_id), paused)
    delete_definitions(case)
    all_rows = case.discover()
    require(len({row.task_id for row in all_rows}) == len(all_rows), "upgrade/restart duplicated an occurrence identity")
    require(entry_count(entries) == len(all_rows), "provider entry count proves duplicate/missing execution")
    require(not any(row.schedule_id == paused for row in all_rows), "upgrade resumed a paused definition")
    require([row.task_id for row in all_rows if row.task_id == held_row.task_id] == [held_row.task_id],
            "upgrade replayed held occurrence")
    case.receipt.add_details(upgrade_acceptance={"preserved_definitions": len(case.upgrade_definitions),
                            "schema_verified": 2, "future_ticks": 2, "pause_resume": True,
                            "candidate_restart": True, "unobserved_closed_reservation_reconciled": True,
                            "no_replay_or_backfill": True, "provider_entries": entry_count(entries),
                            "immutable_evidence_after_runtime": True, "history_skip_coverage": True,
                            "candidate_resume_bounds": resume_bounds,
                            "future_due_times": [future_rows[-1].due_ns, final_rows[-1].due_ns],
                            "independent_task_ids": [row.task_id for row in all_rows]})
    require(final_rows[-1].task_id != future_rows[-1].task_id, "restart reused a task identity")


def run_release(args: UpgradeArguments, output: Path, release: Release) -> None:
    directory = output / release.version
    directory.mkdir(mode=0o700)
    evidence: dict[str, object] = {}
    selected = replace(args, output=directory / "receipt.json", baseline_pair_installer=ReleaseInstaller(release, evidence))
    receipt = AcceptanceReceipt(selected.output, 4, 1, {"pueue": args.pueue, "pueued": args.pueued})
    receipt.add_details(scenario="scheduler-upgrade", acceptance_scope="controlled provider fixture; no AI account")
    lifetime = FixtureLifetime()
    case = None
    failure = None
    try:
        responses = directory / "responses"
        fixture = prepare_fixture(selected, uuid.uuid4().hex, False, lifetime, responses, receipt)
        receipt.add_details(historical_distribution=evidence)
        if lifetime.old_install is None:
            raise RuntimeError("upgrade lost its released installation")
        deadline = time.monotonic() + UPGRADE_SECONDS
        host = SchedulerHost(lifetime, fixture.state, lifetime.old_install / "delegate", deadline)
        case = SchedulerCase(fixture, lifetime, responses, receipt, host, deadline)
        lifetime.delegate = case.delegate
        exercise_upgrade(case, release, directory, candidate_version=args.candidate_version)
        receipt.stage("scheduler-release-upgrade-proved")
    except BaseException as error:
        failure = error
        receipt.add_details(failure_phase=receipt.current_phase)
        (directory / "failure.txt").write_text(type(error).__name__ + ": " + str(error) + "\n")
    finally:
        if case is not None:
            case.deadline = time.monotonic() + 45
            try:
                delete_definitions(case)
            except BaseException as error:
                failure = error if failure is None else BaseExceptionGroup("upgrade and definition cleanup failed", [failure, error])
        finish_case(args=selected, case=case, lifetime=lifetime, receipt=receipt, failure=failure)


def main(argv: list[str] | None = None) -> int:
    args = parse_arguments(argv)
    owner = DirectoryOwner()
    output = output_directory(args.output, owner)
    print("Scheduler upgrade acceptance output: " + str(output), flush=True)
    try:
        for release in RELEASES:
            if args.baseline in ("all", release.version):
                run_release(args, output, release)
        verify_upgrade_output(output, args.baseline, args.candidate_version)
    except BaseException as error:
        print("FAIL scheduler upgrade acceptance: " + type(error).__name__ + "; inspect private output", flush=True)
        return 1
    finally:
        owner.release()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
