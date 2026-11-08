"""Private CLI errors boundary."""

from enum import Enum

from scripts.acceptance.processes.contracts import CommandFailure


class InstallationPhase(Enum):
    BUILD_DELEGATE = "build-delegate"
    BUILD_DELEGATE_RUN = "build-delegate-run"
    INSTALL_SUPERVISOR = "install-supervisor"


class InstallationFailure(RuntimeError):
    """Carry only fixed installation coordinates; child output stays private."""

    def __init__(self, phase: InstallationPhase, outcome: int | CommandFailure):
        if not isinstance(phase, InstallationPhase):
            raise ValueError("invalid installation failure phase")
        if isinstance(outcome, CommandFailure):
            message = f"candidate installation failed: {outcome.value} (phase={phase.value})"
        elif type(outcome) is int and outcome != 0 and outcome.bit_length() <= 32:
            message = f"candidate installation command failed with exit {outcome} (phase={phase.value})"
        else:
            raise ValueError("invalid installation failure outcome")
        super().__init__(message)
        self._phase, self._outcome = phase, outcome

    @property
    def phase(self) -> InstallationPhase:
        return self._phase

    @property
    def outcome(self) -> int | CommandFailure:
        return self._outcome


class AcceptanceBlocked(RuntimeError):
    """A required native prerequisite was unavailable or inconclusive."""

    def __init__(self, code):
        super().__init__(code)
        self.code = code
