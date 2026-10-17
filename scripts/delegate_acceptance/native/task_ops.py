"""Acceptance-specific task cleanup and queue ownership adapters."""

from __future__ import annotations


from scripts.delegate_acceptance.native.constants import WATCH_SECONDS
from scripts.delegate_acceptance.tasks.operations import NativeTaskOps


class ModelDiscoveryCleanupOps(NativeTaskOps):
    """Use the model discovery state root for failure cleanup."""

    def __init__(self, owner) -> None:
        super().__init__(owner.processes, owner.delegate, owner.runner, owner.pueue,
                         owner.models_state.parent, owner.models_state, owner.output,
                         expected_inspection_binding_required=False)
        self.owner = owner

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        self.owner._sync_model_cleanup(self)
        return super().failure_queue_finished(status)


class NativeAcceptanceTaskOps(NativeTaskOps):
    """Keep ordinary task lifecycle validation scoped to the two owned roots."""

    def __init__(self, owner) -> None:
        super().__init__(owner.processes, owner.delegate, owner.runner, owner.pueue,
                         owner.base, owner.state, owner.output,
                         watch_seconds=WATCH_SECONDS,
                         expected_inspection_binding_required=False,
                         managed_runner=owner.scenario == "timeout-resume")
        self.owner = owner

    def _validate_queue_membership(
            self, status: dict[str, object], allow_failure: bool
            ) -> dict[str, dict[str, object]]:
        filtered = self.owner._without_model_discovery_queue(status)
        return super()._validate_queue_membership(filtered, allow_failure)

    def failure_queue_finished(self, status: dict[str, object]) -> bool:
        if self.owner.root_id is None and not self.tasks and not self.dispatch_attempts:
            model_ops = ModelDiscoveryCleanupOps(self.owner)
            model_ops.bind_supervisor(self.owner.config, self.owner.daemon)
            return model_ops.failure_queue_finished(status)
        filtered = self.owner._without_model_discovery_queue(status)
        return super().failure_queue_finished(filtered)
