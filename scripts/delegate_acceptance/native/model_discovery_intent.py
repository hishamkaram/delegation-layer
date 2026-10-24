"""Pure values for the one model discovery request known before Delegate identifies it."""

from __future__ import annotations

from dataclasses import dataclass

from scripts.delegate_acceptance.evidence.validation import _hex_digest, require


# The fixed request Delegate's model discovery builds (internal/app/models.go).
MODEL_DISCOVERY_DEFINITION_REVISION = "model-discovery-v1"
MODEL_DISCOVERY_BRIEF = b"model discovery"


@dataclass(frozen=True)
class ModelDiscoveryIntent:
    """The one discovery request's inputs, known before Delegate generates its identity.

    It is observational only: a later canonical validator may correlate it with the
    exclusive root's journals, and an unmatched intent stays unresolved, never NoStart.
    """

    models_root: str
    initial_entries: tuple[str, ...]
    provider: str
    permission: str
    cwd: str
    definition_revision: str
    budget: str
    budget_nanos: int
    brief_sha256: str
    helper_executable: str
    helper_sha256: str

    def __post_init__(self) -> None:
        require(all(isinstance(value, str) and value for value in (
                    self.models_root, self.provider, self.permission, self.cwd,
                    self.definition_revision, self.budget, self.helper_executable)) and
                isinstance(self.initial_entries, tuple) and
                all(isinstance(name, str) and name for name in self.initial_entries) and
                type(self.budget_nanos) is int and self.budget_nanos > 0 and
                _hex_digest(self.brief_sha256) and _hex_digest(self.helper_sha256),
                "model discovery request intent is invalid")
