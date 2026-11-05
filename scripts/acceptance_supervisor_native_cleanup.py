"""Native-suite proof strategies around the canonical task cleanup owner."""

from collections.abc import Callable
from pathlib import Path
from typing import Final

from scripts.delegate_acceptance.tasks.contracts import TaskProcesses
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps


type QueueCheck = Callable[[dict[str, object]], bool]
type QueueProof = Callable[[QueueCheck, dict[str, object]], bool]


class ProofGatedTaskOps(NativeTaskOps):
    """The suite supplies proof policy; this ledger retains admission and process authority."""

    def __init__(self, *, processes: TaskProcesses, delegate: Path, runner: Path,
                 pueue: Path, state_parent: Path, state: Path, output: Path,
                 queue_proof: QueueProof) -> None:
        super().__init__(processes, delegate, runner, pueue, state_parent, state, output)
        self._queue_proof: Final[QueueProof] = queue_proof

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        return self._queue_proof(super().failure_queue_finished, status)


def no_shutdown_authority(_canonical: QueueCheck, _status: dict[str, object]) -> bool:
    return False
