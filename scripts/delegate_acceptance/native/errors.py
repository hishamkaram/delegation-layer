"""Native acceptance error identity."""

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.native_evidence.contract import BlockReason


class BlockedFailure(AcceptanceFailure):
    """A required provider, supervisor, or authentication prerequisite is absent.

    `reason` is the canonical, typed cause. Detection sites pick it from an
    observed fact; anything not positively classified stays UNCLASSIFIED.
    """

    def __init__(self, message: str, *, reason: BlockReason = BlockReason.UNCLASSIFIED) -> None:
        if type(reason) is not BlockReason:
            raise TypeError("BlockedFailure reason must be a BlockReason")
        super().__init__(message)
        self.reason = reason
