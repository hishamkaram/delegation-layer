"""Native acceptance error identity."""

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure


class BlockedFailure(AcceptanceFailure):
    """A required provider, supervisor, or authentication prerequisite is absent."""
