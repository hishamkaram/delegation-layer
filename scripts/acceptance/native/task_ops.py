"""Acceptance-specific task cleanup and queue ownership adapters."""

from __future__ import annotations

from pathlib import Path
from typing import Protocol

from scripts.acceptance.inspections.data import InspectionEvidence
from scripts.acceptance.native.constants import RUNNER_STATUS_POLL_SECONDS
from scripts.acceptance.native.constants import case_task_budget
from scripts.acceptance.supervisor.runner_layout import BUNDLE_RUNNER
from scripts.acceptance.tasks.contracts import TaskDaemon
from scripts.acceptance.tasks.contracts import TaskProcesses
from scripts.acceptance.tasks.operations import NativeTaskOps


class NativeTaskOwner(Protocol):
    """Acceptance-owned state read by task cleanup; mutations use the owner's callbacks."""

    @property
    def processes(self) -> TaskProcesses: ...
    @property
    def delegate(self) -> Path: ...
    @property
    def runner(self) -> Path: ...
    @property
    def pueue(self) -> Path: ...
    @property
    def models_state(self) -> Path: ...
    @property
    def output(self) -> Path: ...
    @property
    def base(self) -> Path: ...
    @property
    def state(self) -> Path: ...
    @property
    def provider(self) -> str: ...
    @property
    def root_id(self) -> str | None: ...
    @property
    def config(self) -> Path: ...
    @property
    def daemon(self) -> TaskDaemon | None: ...
    @property
    def ops(self) -> NativeTaskOps: ...

    def _sync_model_cleanup(self, ops: ModelDiscoveryCleanupOps) -> None: ...

    def _without_model_discovery_queue(self, status: dict[str, object]) -> dict[str, object]: ...

    def _prove_absent_resource_refusals(self, tasks: frozenset[str], deadline: float) -> frozenset[str]: ...


class ModelDiscoveryCleanupOps(NativeTaskOps):
    """Use the model discovery state root for failure cleanup.

    Model discovery is admitted with the acceptance's runner expectation, so
    the cleanup ledger reuses that one owner's managed mode and placement.
    """

    def __init__(self, owner: NativeTaskOwner) -> None:
        super().__init__(owner.processes, owner.delegate, owner.runner, owner.pueue,
                         owner.models_state.parent, owner.models_state, owner.output,
                         expected_inspection_binding_required=False,
                         managed_runner=owner.ops.managed_runner,
                         runner_placement=owner.ops.runner_placement)
        self.owner = owner

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        self.owner._sync_model_cleanup(self)
        return super().failure_queue_finished(status)


class NativeAcceptanceTaskOps(NativeTaskOps):
    """Keep ordinary task lifecycle validation scoped to the two owned roots.

    This ledger owns the acceptance's runner expectation. Every admission is
    managed, so Go selects the delegate's adjacent runner and the supplied
    runner is only its expected fingerprint. The acceptance supplies an
    external supervisor config, so Go queues that bundle runner itself. Only
    internal fixtures that exercise custom-runner history pass
    ``managed_runner=False``.

    The runner wait and failure cleanup observe the provider case's budget contract.
    """

    def __init__(self, owner: NativeTaskOwner, *, managed_runner: bool = True) -> None:
        super().__init__(owner.processes, owner.delegate, owner.runner, owner.pueue,
                         owner.base, owner.state, owner.output,
                         watch_seconds=case_task_budget(owner.provider).watch_seconds,
                         expected_inspection_binding_required=False,
                         managed_runner=managed_runner, runner_placement=BUNDLE_RUNNER,
                         runner_poll_seconds=RUNNER_STATUS_POLL_SECONDS)
        self.owner = owner

    def _validate_queue_membership(
            self, status: dict[str, object], allow_failure: bool
            ) -> dict[str, InspectionEvidence]:
        filtered = self.owner._without_model_discovery_queue(status)
        return super()._validate_queue_membership(filtered, allow_failure)

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        if self.owner.root_id is None and not self.tasks and not self.dispatch_attempts:
            model_ops = ModelDiscoveryCleanupOps(self.owner)
            model_ops.bind_supervisor(self.owner.config, self.owner.daemon)
            return model_ops.failure_queue_finished(status)
        filtered = self.owner._without_model_discovery_queue(status)
        return super().failure_queue_finished(filtered)

    def _prove_absent_refusals(self, tasks: frozenset[str], deadline: float) -> frozenset[str]:
        return self.owner._prove_absent_resource_refusals(tasks, deadline)
