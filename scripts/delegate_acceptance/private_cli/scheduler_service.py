"""Invocation-owned native scheduler service; every stop enters the service host."""

from dataclasses import dataclass
import fcntl
import os
from pathlib import Path
import platform
import plistlib
import shutil
import stat
import subprocess
import time
import uuid

from scripts.delegate_acceptance.private_cli.capture import run_captured_text_command
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.private_cli.fixture_lifetime import FixtureLifetime
from scripts.delegate_acceptance.processes.observation import wait_until


def manager_environment() -> dict[str, str]:
    """Only transport coordinates reach the host control CLI, never a global dump."""
    selected = {"HOME": str(Path.home()), "PATH": "/usr/bin:/bin", "LC_ALL": "C"}
    for key in ("XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"):
        if key in os.environ:
            selected[key] = os.environ[key]
    return selected


def scheduler_command(delegate: Path, state: Path, environment: dict[str, str]) -> list[str]:
    # Both service hosts inherit manager settings. env -i gives the scheduler
    # exactly the finite fixture scope, including only its private HOME/PATH.
    return ["/usr/bin/env", "-i", *(f"{key}={value}" for key, value in sorted(environment.items())),
            str(delegate), "--root", str(state), "scheduler", "run", "--json"]


def linux_start_command(unit: str, command: list[str], directory: Path) -> list[str]:
    # systemd.kill(5): process leaves descendants alive; no forced stop follows
    # a scheduler shutdown timeout. Only the service host sends the stop signal.
    return ["systemd-run", "--user", "--quiet", "--expand-environment=no", f"--unit={unit}",
            f"--description={unit}", "--property=Type=exec", "--property=KillMode=process", "--property=Delegate=yes",
            "--property=ExitType=cgroup", "--property=SendSIGKILL=no",
            "--property=TimeoutStopSec=infinity", "--property=Restart=no",
            f"--property=StandardOutput=append:{directory / 'stdout'}",
            f"--property=StandardError=append:{directory / 'stderr'}", "--", *command]


def launchd_definition(label: str, command: list[str], directory: Path) -> dict[str, object]:
    # Apple's launchd.plist(5): AbandonProcessGroup disables the implicit
    # process-group cleanup when this job exits. bootout still stops the job.
    return {"Label": label, "ProgramArguments": command, "RunAtLoad": True, "KeepAlive": False,
            "AbandonProcessGroup": True, "ExitTimeOut": 30,
            "StandardOutPath": str(directory / "stdout"), "StandardErrorPath": str(directory / "stderr")}


def owner_released(state: Path) -> bool:
    """The production scheduler lease stays held through joined shutdown."""
    descriptor = os.open(state / ".scheduler" / "owner.lock", os.O_RDWR | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            raise RuntimeError("scheduler owner lease is unsafe")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return False
        fcntl.flock(descriptor, fcntl.LOCK_UN)
        return True
    finally:
        os.close(descriptor)


def linux_properties(output: str) -> dict[str, str]:
    properties = {}
    for line in output.splitlines():
        key, separator, value = line.partition("=")
        if not separator or key in properties:
            raise RuntimeError("systemd returned malformed service properties")
        properties[key] = value
    return properties


def require_linux_owner(properties: dict[str, str], unit: str, invocation: str = "") -> str:
    if (properties.get("Id") != unit or properties.get("Description") != unit
            or properties.get("Transient") != "yes" or properties.get("KillMode") != "process"
            or properties.get("Delegate") != "yes" or properties.get("ExitType") != "cgroup"):
        raise RuntimeError("scheduler transient unit identity or stop policy changed")
    observed = properties.get("InvocationID", "")
    if len(observed) != 32 or any(char not in "0123456789abcdef" for char in observed):
        raise RuntimeError("scheduler transient unit lacks its invocation identity")
    if invocation and observed != invocation:
        raise RuntimeError("scheduler transient unit was replaced")
    return observed


@dataclass
class SchedulerService:
    """One writer owns registration, native stop, and retirement of one random job.

    A failed/ambiguous start or stop retains the fixture. No generic service
    framework, global service settings, process signals, or provider control.
    """

    lifetime: FixtureLifetime
    state: Path
    delegate: Path
    deadline: float
    attempted: bool = False
    started: bool = False
    stopped: bool = False
    invocation: str = ""

    def __post_init__(self) -> None:
        self.system = platform.system()
        if self.system not in ("Linux", "Darwin"):
            raise AcceptanceBlocked("unsupported-scheduler-service-host")
        if self.lifetime.base is None:
            raise RuntimeError("scheduler service requires an owned fixture")
        self.directory = self.lifetime.ownership.create(
            "scheduler service files", self.lifetime.base / "scheduler-service", self.lifetime.base
        ).directory
        self.label = "org.delegate.scheduler-acceptance." + uuid.uuid4().hex
        self.unit = self.label + ".service"
        # This is a per-user LaunchAgent. Its logged-in session owns the GUI
        # domain, even when this test caller connects over SSH.
        self.domain = f"gui/{os.getuid()}"
        self.target = self.domain + "/" + self.label
        self.control_environment = manager_environment()

    def control(self, arguments: list[str]) -> subprocess.CompletedProcess[str]:
        if self.lifetime.capture_token is None:
            raise RuntimeError("scheduler service lost its capture owner")
        executable = shutil.which(arguments[0], path="/usr/bin:/bin")
        if executable is None:
            raise AcceptanceBlocked("scheduler-service-control-unavailable")
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("scheduler service observation deadline expired")
        return run_captured_text_command(
            [executable, *arguments[1:]], self.control_environment, min(35, remaining),
            capture_root=self.lifetime.capture_token,
        )

    def check(self, arguments: list[str]) -> subprocess.CompletedProcess[str]:
        result = self.control(arguments)
        if result.returncode != 0:
            raise RuntimeError(f"scheduler service host command failed with exit {result.returncode}")
        return result

    def properties(self) -> dict[str, str]:
        result = self.control(["systemctl", "--user", "show", self.unit,
                               "--property=Id,Description,Transient,InvocationID,KillMode,Delegate,ExitType,LoadState,ActiveState,MainPID,Result,ExecMainStatus"])
        properties = linux_properties(result.stdout)
        if result.returncode not in (0, 1) or not properties.get("LoadState"):
            raise RuntimeError("scheduler service host could not observe its exact unit")
        return properties

    def start(self) -> None:
        command = scheduler_command(self.delegate, self.state, self.lifetime.env)
        if self.system == "Linux":
            result = self.control(["systemctl", "--user", "is-system-running"])
            if (result.returncode, result.stdout.strip()) not in ((0, "running"), (1, "degraded")):
                raise AcceptanceBlocked("systemd-user-manager-unavailable")
            if self.properties().get("LoadState") != "not-found":
                raise RuntimeError("scheduler transient unit already exists")
            self.attempted = True
            self.check(linux_start_command(self.unit, command, self.directory))
            properties = self.properties()
            self.invocation = require_linux_owner(properties, self.unit)
            if properties.get("ActiveState") != "active" or int(properties.get("MainPID", "0")) <= 0:
                raise RuntimeError("scheduler transient service did not start")
        else:
            definition = launchd_definition(self.label, command, self.directory)
            path = self.directory / (self.label + ".plist")
            with path.open("xb") as stream:
                plistlib.dump(definition, stream)
            path.chmod(0o600)
            self.check(["launchctl", "help", "bootstrap"])
            self.check(["launchctl", "help", "bootout"])
            self.attempted = True
            self.check(["launchctl", "bootstrap", self.domain, str(path)])
            self.check(["launchctl", "print", self.target])
        self.started = True
        # Probe only after the service has acquired its lease. Trying an
        # exclusive lock during nonblocking acquisition could make it fail busy.
        wait_until(self.ready, timeout=self.remaining(), description="scheduler ready diagnostic")
        wait_until(lambda: not owner_released(self.state), timeout=self.remaining(), description="scheduler owner lease")

    def ready(self) -> bool:
        try:
            with (self.directory / "stderr").open("r", encoding="utf-8") as stream:
                return stream.readline(256) == "scheduler phase=runtime schedule_id= task_id= code=ready\n"
        except FileNotFoundError:
            return False

    def remaining(self) -> float:
        return max(0, self.deadline - time.monotonic())

    def stop(self) -> None:
        if self.stopped or not self.attempted:
            return
        if not self.started:
            raise RuntimeError("scheduler service registration is uncertain; retain its fixture")
        if self.system == "Linux":
            properties = self.properties()
            require_linux_owner(properties, self.unit, self.invocation)
            if properties.get("ActiveState") != "active" or int(properties.get("MainPID", "0")) <= 0:
                raise RuntimeError("scheduler main process is not live before orderly stop; retain its fixture")
            # Keep the delegated subtree until independent children close. The
            # scheduler itself must exit and join its owner, while its unit may
            # still be draining. Waiting for the whole cgroup here would wait
            # for (or kill) the deliberately held independent worker.
            self.check(["systemctl", "--user", "--no-block", "stop", self.unit])
            wait_until(self.linux_scheduler_stopped, timeout=self.remaining(), description="scheduler main process stop")
        else:
            self.check(["launchctl", "bootout", self.target])
            wait_until(self.launchd_absent, timeout=self.remaining(), description="scheduler job bootout")
        wait_until(lambda: owner_released(self.state), timeout=self.remaining(), description="scheduler joined shutdown")
        self.stopped = True

    def linux_scheduler_stopped(self) -> bool:
        properties = self.properties()
        if properties.get("LoadState") == "not-found":
            return True
        require_linux_owner(properties, self.unit, self.invocation)
        if properties.get("ActiveState") == "failed":
            raise RuntimeError("scheduler service did not stop cleanly")
        if properties.get("MainPID") != "0" or properties.get("ActiveState") == "active":
            return False
        if (properties.get("ActiveState") not in ("inactive", "deactivating")
                or properties.get("Result") != "success" or properties.get("ExecMainStatus") != "0"):
            raise RuntimeError("scheduler service did not stop cleanly")
        return True

    def launchd_absent(self) -> bool:
        result = self.control(["launchctl", "print", self.target])
        if result.returncode == 0:
            return False
        if "Could not find service" not in result.stderr or self.label not in result.stderr:
            raise RuntimeError("launchd job absence could not be verified")
        return True

    def retire(self) -> None:
        """Called only after all task closures and private daemon shutdown."""
        if not self.stopped:
            raise RuntimeError("cannot retire a scheduler service with unknown closure")
        if self.system == "Darwin":
            if not self.launchd_absent():
                raise RuntimeError("scheduler launchd job remained registered")
            return
        properties = self.properties()
        if properties.get("LoadState") == "not-found":
            return
        self.control(["systemctl", "--user", "reset-failed", self.unit])
        wait_until(lambda: self.properties().get("LoadState") == "not-found",
                   timeout=self.remaining(), description="owned transient unit retirement")
