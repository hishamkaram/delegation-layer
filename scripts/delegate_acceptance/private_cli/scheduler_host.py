"""Foreground host consumer; the shared process owner retains Start/Wait authority."""

from dataclasses import dataclass, field
from pathlib import Path
import platform
import time

from scripts.delegate_acceptance.evidence.validation import require
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.private_cli.scheduler_occurrences import json_object
from scripts.delegate_acceptance.processes.observation import wait_until
from scripts.delegate_acceptance.processes.operations import Process, Processes


@dataclass
class SchedulerHost:
    """One fixture controls its own serve process through the public CLI.

    Processes owns every child and stream; this consumer only requests stop
    and observes those same handles. Unknown Start/Wait never permits deletion.
    """

    lifetime: FixtureLifetime
    state: Path
    delegate: Path
    deadline: float
    generation: int = 0
    default_root: bool = False
    attempted: bool = field(default=False, init=False)
    stopped: bool = field(default=False, init=False)
    process: Process | None = field(default=None, init=False, repr=False)
    processes: Processes = field(init=False, repr=False)

    def __post_init__(self) -> None:
        if platform.system() not in ("Linux", "Darwin"):
            raise AcceptanceBlocked("unsupported-scheduler-host")
        capture = self.lifetime.capture_token
        if self.lifetime.base is None or capture is None:
            raise RuntimeError("scheduler host requires an owned fixture and capture root")
        self.processes = Processes(
            capture.directory, self.lifetime.env, capture_root=capture,
            environment_keys_only=True,
        )

    def remaining(self) -> float:
        return max(0, self.deadline - time.monotonic())

    @property
    def prefix(self) -> list[str]:
        return [] if self.default_root else ["--root", str(self.state)]

    @property
    def directory(self) -> Path:
        if self.default_root:
            if self.lifetime.base is None:
                raise RuntimeError("scheduler host lost its owned fixture directory")
            return self.lifetime.base
        return self.state

    def command(self, action: str, expected: tuple[int, ...] = (0,)) -> dict[str, object]:
        require(self.remaining() > 0, "scheduler host command deadline expired")
        command = self.processes.run(
            f"scheduler-{self.generation}-{action}",
            [self.delegate, *self.prefix, "scheduler", action, "--json"],
            self.directory, timeout=min(30, self.remaining()), expected=expected,
        )
        response = json_object(command.json())
        require(not response.get("error") and not response.get("failure"),
                "scheduler host control reported an operational failure")
        return json_object(response.get("scheduler"))

    def _observe_started(self, process: Process) -> None:
        # The registry already owns this exact handle before this callback.
        self.process = process

    def start(self) -> None:
        require(not self.attempted, "scheduler host generation was already started")
        require(self.remaining() > 0, "scheduler host start deadline expired")
        self.attempted = True
        self.processes.start(
            f"scheduler-host-{self.generation}",
            [self.delegate, *self.prefix, "scheduler", "serve"],
            self.directory, on_spawned=self._observe_started, start_new_session=True,
        )
        wait_until(self.ready, timeout=self.remaining(), description="public scheduler host readiness")

    def ready(self) -> bool:
        process = self.process
        if process is None:
            raise RuntimeError("scheduler host has no exact started process")
        require(process.poll() is None, "scheduler host exited before readiness")
        response = self.command("status")
        require(process.poll() is None, "scheduler host exited during readiness observation")
        return response.get("state") == "ready"

    def stop(self) -> None:
        if self.stopped or not self.attempted:
            return
        errors: list[BaseException] = []
        process = self.process
        try:
            if process is not None and process.poll() is None:
                # Exit 3 may acknowledge accepted-but-pending stop. Only Wait
                # below proves that this invocation's host actually ended.
                self.command("stop", (0, 3))
        except BaseException as error:
            errors.append(error)
        try:
            if process is not None:
                process.wait(timeout=self.remaining(), expected=0)
            require(self.processes.finished_naturally(), "scheduler host or control closure remains unknown")
        except BaseException as error:
            errors.append(error)
        if errors:
            raise BaseExceptionGroup("scheduler host stop or exact Wait failed", errors)
        self.stopped = True

    def retire(self) -> None:
        require(not self.attempted or self.stopped, "scheduler host retirement requires exact Wait")
