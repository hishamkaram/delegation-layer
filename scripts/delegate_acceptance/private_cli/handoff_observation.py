"""Immutable external-task observations retained across a private handoff."""

from dataclasses import dataclass
from pathlib import Path
from typing import Mapping, Sequence


@dataclass(frozen=True)
class ObservedHandoffTask:
    task_id: str
    response: Mapping[str, object]
    specifications: Sequence[Mapping[str, object]]
    metadata_path: Path
    metadata: bytes
    metadata_sha256: str
    label: str
    row: Mapping[str, object]
    snapshot_name: str
    wrapper_calls: Sequence[Mapping[str, object]]
