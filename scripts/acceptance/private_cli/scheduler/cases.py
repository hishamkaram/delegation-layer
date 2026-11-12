"""Bounded real-minute scheduler cases using the existing finite pi fixture."""
from dataclasses import dataclass, field
from pathlib import Path
import sqlite3
import time

from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.evidence.validation import require
from scripts.acceptance.private_cli.capacity.ownership import lease_candidate_specifications
from scripts.acceptance.private_cli.capacity.ownership import prove_lifetime_owner_cohort
from scripts.acceptance.private_cli.capacity.records import capacity_lease_specifications
from scripts.acceptance.private_cli.commands import run_json
from scripts.acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.acceptance.private_cli.fixture_records import FixtureInstallation
from scripts.acceptance.private_cli.leases import LiveFIFOLeaseCohort
from scripts.acceptance.private_cli.leases import LockedReleaseGate
from scripts.acceptance.private_cli.pair_evidence import assert_state_supervisor_pair
from scripts.acceptance.private_cli.receipt import AcceptanceReceipt
from scripts.acceptance.private_cli.scheduler.admission import not_created_failure
from scripts.acceptance.private_cli.scheduler.occurrences import MINUTE_NS
from scripts.acceptance.private_cli.scheduler.occurrences import Occurrence
from scripts.acceptance.private_cli.scheduler.occurrences import identity
from scripts.acceptance.private_cli.scheduler.occurrences import json_object
from scripts.acceptance.private_cli.scheduler.occurrences import occurrence_read_busy
from scripts.acceptance.private_cli.scheduler.occurrences import read_occurrences
from scripts.acceptance.private_cli.scheduler.occurrences import require_occurrences
from scripts.acceptance.private_cli.scheduler.occurrences import timestamp_ns
from scripts.acceptance.private_cli.scheduler.service import SchedulerService
from scripts.acceptance.private_cli.scheduler.host import SchedulerHost
from scripts.acceptance.private_cli.supervisor import await_managed_lifetime_closure
from scripts.acceptance.processes.observation import wait_until
from scripts.acceptance.tasks.publication import verify_collected_outcome


CASE_SECONDS = 240
SUCCESSIVE_SECONDS = 300
EXPECTED_PAYLOAD = b"private supervisor acceptance passed"


@dataclass
class SchedulerCase:
    """The fixture owns hosting and discovered task IDs, never task state."""

    fixture: FixtureInstallation
    lifetime: FixtureLifetime
    responses: Path
    receipt: AcceptanceReceipt
    service: SchedulerService | SchedulerHost
    deadline: float
    root_id: str = ""
    schedule_id: str = ""
    first_due_ns: int = 0
    command_count: int = 0
    previous_services: list[SchedulerService | SchedulerHost] = field(default_factory=list)
    default_root: bool = False

    @property
    def delegate(self) -> Path:
        if self.lifetime.old_install is None:
            raise RuntimeError("scheduler case lost its installed CLI")
        return self.lifetime.old_install / "delegate"

    def command(self, name: str, arguments: list[str], expected: tuple[int, ...] = (0,), *,
                deadline: float | None = None) -> dict[str, object]:
        command_deadline = self.deadline if deadline is None else deadline
        self.command_count += 1
        if self.default_root:
            if not isinstance(self.service, SchedulerHost):
                raise AcceptanceFailure("default-root case requires the managed host")
            remaining = max(0, command_deadline - time.monotonic())
            require(remaining > 0, "scheduler case command deadline expired")
            # Reuse the host's process registry so this caller has an explicit,
            # different cwd and every control child retains the same Wait owner.
            command = self.service.processes.run(
                f"case-{self.command_count}-{name}", [self.delegate, *arguments, "--json"],
                self.fixture.workspace, expected=expected, timeout=remaining,
            )
            response = json_object(command.json())
            if self.root_id:
                require(response.get("root_id") == self.root_id, "default-root command changed root identity")
            return response
        result = run_json(
            self.delegate, self.lifetime.env, ["--root", str(self.fixture.state), *arguments, "--json"],
            expected=expected, timeout=None, deadline=command_deadline, capture_root=self.lifetime.capture_token,
            receipt=self.responses / f"{self.command_count:03d}-{name}.json", capture_response=True,
        )
        return json_object(result)

    def remaining(self) -> float:
        return max(0, self.deadline - time.monotonic())

    def cold(self) -> None:
        require(not (self.fixture.state / ".supervisor").exists(), "schedule caller started the private supervisor")
        for marker in (self.fixture.old_daemon_marker, self.fixture.upgraded_daemon_marker):
            require(not marker.exists(), "private daemon ran before the scheduler service")

    def brief(self, *, held: bool, controlled_failure: bool) -> Path:
        brief = self.fixture.brief
        if held:
            if self.lifetime.gate is None or self.lifetime.base is None:
                raise RuntimeError("scheduler hold fixture lost its gate")
            self.lifetime.release_gate = LockedReleaseGate.acquire(self.lifetime.gate)
            brief = self.lifetime.base / "scheduler-held-brief.md"
            brief.write_text("DELEGATE_PRIVATE_UPGRADE_WAIT\n", encoding="utf-8")
            brief.chmod(0o600)
        elif controlled_failure:
            if self.lifetime.base is None:
                raise RuntimeError("scheduler fixture lost its owned directory")
            brief = self.lifetime.base / "scheduler-controlled-brief.md"
            brief.write_text("DELEGATE_SCHEDULER_CONTROLLED_FAILURE\n", encoding="utf-8")
            brief.chmod(0o600)
        return brief

    def require_start_required(self, response: dict[str, object]) -> None:
        require(not response.get("error") and not response.get("failure"), "saved schedule reported an operational failure")
        schedule = json_object(response.get("schedule"))
        require(response.get("root_id") == self.root_id and schedule.get("id") == self.schedule_id,
                "saved schedule identity changed before hosting")
        require(schedule.get("saved") is True, "no-host schedule was not reported saved")
        scheduler = json_object(response.get("scheduler"))
        require(scheduler.get("state") in ("no_host_observed", "stopped")
                and scheduler.get("next_action") == schedule.get("next_action") == "start_required"
                and json_object(response.get("pending")).get("kind") == "start_required",
                "no-host schedule did not require user start")
        require(scheduler.get("serve_command") == [str(self.delegate.resolve(strict=True)), "--root", str(self.fixture.state), "scheduler", "serve"],
                "saved schedule returned a different host start command")

    def create(self, *, held: bool, controlled_failure: bool = False, failure_hook: Path | None = None) -> None:
        # Leave a real startup window before the first due time. A slow host
        # that misses it fails; no timestamp, DB cursor, or wall clock is changed.
        wait_until(lambda: time.time_ns() % MINUTE_NS < 20_000_000_000,
                   timeout=self.remaining(), description="real schedule creation window")
        brief = self.brief(held=held, controlled_failure=controlled_failure)
        self.receipt.phase("schedule_create")
        managed = isinstance(self.service, SchedulerHost)
        hook_arguments = [] if failure_hook is None else ["--failure-hook", str(failure_hook), "--failure-threshold", "1"]
        response = self.command("create", [
            "schedule", "create", "--provider", "pi:json", "--cron", "* * * * *", "--timezone", "UTC",
            "--brief", str(brief), "--cwd", str(self.fixture.workspace), "--permission", "read-only", "--budget", "4m",
        ] + hook_arguments, (3,) if managed else (0, 3))
        self.root_id = identity(response.get("root_id"))
        self.schedule_id = identity(json_object(response.get("schedule")).get("id"))
        if managed:
            self.require_start_required(response)
            self.command("pause-before-host", ["schedule", "pause", self.schedule_id])
            resumed = self.command("resume-before-host", ["schedule", "resume", self.schedule_id], (3,))
            self.require_start_required(resumed)
        shown = json_object(self.command("show", ["schedule", "show", self.schedule_id]).get("schedule"))
        require(shown.get("id") == self.schedule_id, "saved schedule cannot be read by its original ID")
        created = timestamp_ns(shown.get("created_at"))
        self.first_due_ns = (created // MINUTE_NS + 1) * MINUTE_NS
        require(time.time_ns() < self.first_due_ns, "create caller did not exit before the first actual occurrence")
        self.cold()
        require(not self.discover(), "schedule creation invoked a task")
        self.receipt.add_details(scheduler={"schedule_id": self.schedule_id, "root_id": self.root_id,
                                           "create_caller_exited_before_first_due": True, "cold_supervisor": True})
        self.receipt.phase("scheduler_hosting")
        self.service.start()
        require(time.time_ns() < self.first_due_ns, "scheduler missed its first real-minute startup window")

    def discover(self) -> tuple[Occurrence, ...]:
        if not self.root_id:
            return ()
        rows = read_occurrences(self.fixture.state, self.root_id)
        # Register before inspecting count, schedule identity, task state, or
        # assertion results. prepare_fixture already registered this same root
        # and list with the canonical finalizer, including on a failed case.
        for row in rows:
            if row.task_id not in self.lifetime.attempted_task_ids:
                self.lifetime.attempted_task_ids.append(row.task_id)
        return rows

    def admission_failure(self, row: Occurrence, *, deadline: float | None = None,
                          in_flight: bool = False) -> dict[str, object] | None:
        require(row.schedule_id == self.schedule_id, "scheduler occurrence belongs to a different schedule")
        arguments = ["schedule", "execution", row.task_id]
        response = (self.command("admission-" + row.task_id, arguments, (0, 1)) if in_flight else
                    self.command("admission-" + row.task_id, arguments, deadline=deadline))
        return not_created_failure(response, self.root_id, row, allow_in_flight=in_flight)

    def require_not_refused(self, row: Occurrence) -> None:
        failure = self.admission_failure(row, in_flight=True)
        if failure is not None:
            self.receipt.add_details(scheduler_admission_failure=failure)
            raise AcceptanceFailure(f"scheduled task was not created: {failure['code']} "
                                    f"stage={failure['stage']} next_action={failure['next_action']}")

    def reconcile_cleanup_tasks(self, rows: tuple[Occurrence, ...], *, deadline: float) -> None:
        """After every host joins, separate reservations from ordinary task lifetimes."""
        self.receipt.add_details(scheduler_reserved_occurrences=[
            {"task_id": row.task_id, "schedule_id": row.schedule_id, "due_unix_nanos": row.due_ns} for row in rows])
        absent = []
        for row in rows:
            directory = self.fixture.state / "tasks" / row.task_id
            # Existing or unsafe entries remain subject to the canonical scan.
            if directory.exists() or directory.is_symlink():
                continue
            failure = self.admission_failure(row, deadline=deadline)
            if failure is not None:
                absent.append(failure)
        # Validate all observations before changing the shared finalizer list.
        proven = {failure["task_id"] for failure in absent}
        self.lifetime.attempted_task_ids[:] = [task_id for task_id in self.lifetime.attempted_task_ids
                                             if task_id not in proven]
        self.receipt.add_details(scheduler_not_created_tasks=absent)

    def occurrences(self, count: int) -> tuple[Occurrence, ...]:
        def ready():
            try:
                rows = self.discover()
            except sqlite3.OperationalError as error:
                if occurrence_read_busy(error):
                    return ()
                raise
            require(len(rows) <= count, "scheduler admitted an unexpected extra occurrence")
            require(all(row.schedule_id == self.schedule_id for row in rows), "scheduler used a different schedule")
            return rows if len(rows) == count else ()

        return wait_until(ready, timeout=self.remaining(), description=f"{count} real-minute occurrences")

    def status(self, row: Occurrence) -> dict[str, object]:
        response = self.command("status-" + row.task_id, ["status", row.task_id])
        require(response.get("root_id") == self.root_id and response.get("task_id") == row.task_id,
                "scheduler task status identity changed")
        require(not response.get("error") and not response.get("failure"), "scheduled task reported an operational failure")
        return response

    def await_admitted(self, row: Occurrence) -> dict[str, object]:
        def ready():
            directory = self.fixture.state / "tasks" / row.task_id
            if not all((directory / name).is_file() for name in ("meta.json", "supervisor.ref.json")):
                self.require_not_refused(row)
                return None
            response = self.status(row)
            return response if response.get("admission") == "admitted" else None

        response = wait_until(ready, timeout=self.remaining(), description="scheduled task admission")
        # Status owns the validated admission verdict for these exact IDs;
        # task_record belongs to dispatch and is absent from Status responses.
        require(self.lifetime.old_libexec is not None, "scheduler lost its bundled supervisor")
        assert_state_supervisor_pair(self.fixture.state, self.lifetime.old_libexec)
        require(self.fixture.old_daemon_marker.is_file(), "scheduler never started its cold private daemon")
        return response

    def collect(self, row: Occurrence) -> None:
        self.await_admitted(row)

        def collected():
            result = self.command("collect-" + row.task_id, ["collect", row.task_id, "--watch", "2s"], (0, 3))
            require(result.get("root_id") == self.root_id and result.get("task_id") == row.task_id,
                    "scheduler collect identity changed")
            require(not result.get("error") and not result.get("failure"), "scheduler collection failed")
            return result if result.get("publication") == "committed" else None

        result = wait_until(collected, timeout=self.remaining(), description="scheduled canonical publication")
        directory = self.fixture.state / "tasks" / row.task_id
        outcome, payload = verify_collected_outcome(result, directory, "committed")
        require(outcome.get("root_id") == self.root_id and outcome.get("task_id") == row.task_id,
                "canonical scheduled outcome identity changed")
        require(payload.strip() == EXPECTED_PAYLOAD, "scheduled fixture returned the wrong answer")
        closure = await_managed_lifetime_closure(
            self.fixture.state, (row.task_id,), timeout=min(30, self.remaining())
        )
        tasks = json_object(closure.get("tasks"))
        task_closure = json_object(tasks.get(row.task_id))
        require(json_object(task_closure.get("worker_wait")).get("result") == "success",
                "scheduled task lacks its canonical worker Wait")
        require(json_object(task_closure.get("termination")).get("scope_empty") is True,
                "scheduled task lacks canonical empty-scope proof")
        self.receipt.increment("collected_task_count")

    def held_lease(self, response: dict[str, object]) -> LiveFIFOLeaseCohort:
        def ready():
            specifications = capacity_lease_specifications(
                self.fixture.state, [response], self.fixture.waiter_directory, self.fixture.lease_nonce
            )
            candidates = lease_candidate_specifications(specifications)
            return candidates if len(candidates) == 1 else None

        specifications = wait_until(ready, timeout=self.remaining(), description="held scheduler fixture launch")
        cohort = LiveFIFOLeaseCohort.acquire(specifications, self.fixture.lease_nonce, timeout=min(10, self.remaining()))
        try:
            prove_lifetime_owner_cohort(specifications, cohort)
            return cohort
        except BaseException:
            cohort.close()
            raise

    def held(self, row: Occurrence, cohort: LiveFIFOLeaseCohort) -> None:
        cohort.assert_live()
        status = self.status(row)
        require(status.get("liveness") == "running" and not status.get("outcome"),
                "held scheduled worker stopped with its scheduler service")
        require(not (self.fixture.state / "tasks" / row.task_id / "outcome.json").exists(),
                "held scheduled worker published before gate release")

    def restart(self) -> None:
        """Keep both exact hosting generations until canonical fixture cleanup."""
        previous = self.service
        previous.stop()
        self.previous_services.append(previous)
        self.service = type(previous)(
            self.lifetime, self.fixture.state, previous.delegate, self.deadline,
            generation=len(self.previous_services),
        )
        self.service.start()


def run_overlap_stop(case: SchedulerCase) -> None:
    case.create(held=True)
    row, = case.occurrences(1)
    require_occurrences((row,), case.schedule_id, (case.first_due_ns,))
    response = case.await_admitted(row)
    with case.held_lease(response) as cohort:
        case.held(row, cohort)
        skipped_due = case.first_due_ns + MINUTE_NS

        def skipped():
            require_occurrences(case.discover(), case.schedule_id, (case.first_due_ns,))
            shown = json_object(case.command("overlap-cursor", ["schedule", "show", case.schedule_id]).get("schedule"))
            cursor = timestamp_ns(shown.get("cursor"))
            return cursor >= skipped_due

        # Poll the public cursor only near the actual second minute. Its
        # advance while the exact live worker remains held proves no backlog
        # row was armed for that tick, not restart/catch-up semantics.
        wait_until(lambda: time.time_ns() >= skipped_due, timeout=case.remaining(), description="second actual minute")
        wait_until(skipped, timeout=case.remaining(), description="overlap tick consumed without another task")
        case.held(row, cohort)
        case.receipt.phase("stop_service_while_worker_held")
        case.service.stop()
        require_occurrences(case.discover(), case.schedule_id, (case.first_due_ns,))
        case.held(row, cohort)
        if case.lifetime.release_gate is None:
            raise AcceptanceFailure("scheduler held gate owner disappeared")
        case.lifetime.release_gate.release()
        case.collect(row)
    case.receipt.add_details(actual_minute_ticks=2, overlap_tick_without_backlog=True,
                             worker_survived_service_stop=True, held_task_id=row.task_id)
