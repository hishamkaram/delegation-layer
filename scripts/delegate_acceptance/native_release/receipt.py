"""The invocation lock holder is the sole writer of invocation stage facts.

Each intent is durable before its effect. Original inputs and completed outputs
are immutable; observation may complete an intent using independently read facts.
"""

from __future__ import annotations

from dataclasses import dataclass
from copy import deepcopy
import hashlib
from pathlib import Path

from scripts.delegate_acceptance.evidence.bounded_files import read_bounded_regular_file
from scripts.delegate_acceptance.evidence.files import write_json
from scripts.delegate_acceptance.native_evidence.public_contract import parse_json
from .contract import (
    CANDIDATE_RECEIPT_KIND, PUBLICATION_RECEIPT_KIND, CandidateRequest, Invocation, MAX_RECEIPT,
    PublicationRequest, ReleaseError, Request, invocation_name, object_value, receipt_kind, receipt_stages,
    request_parser, status_value, text_value,
)


@dataclass(frozen=True)
class Stage:
    state: str
    intent: dict[str, object]
    result: dict[str, object]

    def document(self) -> dict[str, object]:
        return {"state": self.state, "intent": self.intent, "result": self.result}

    @classmethod
    def parse(cls, value: object) -> Stage:
        record = object_value(value)
        if set(record) != {"state", "intent", "result"}:
            raise ReleaseError("release_stage_invalid")
        state = text_value(record["state"], "pending|intended|complete")
        intent, result = object_value(record["intent"]), object_value(record["result"])
        if state == "pending" and (intent or result) or state == "intended" and result:
            raise ReleaseError("release_stage_invalid")
        return cls(state, intent, result)


class Receipt:
    def __init__(self, invocation: Invocation, metadata: dict[str, object],
                 stages: dict[str, Stage]):
        self.invocation = invocation
        self.path = invocation.root / "receipt.json"
        self._metadata = deepcopy(metadata)
        self._stages = deepcopy(stages)
        self.status = "INCOMPLETE"
        self.reason = "invocation_initialized"

    @classmethod
    def create(cls, invocation: Invocation, argv: list[str]) -> Receipt:
        metadata: dict[str, object] = {
            "invocation_id": invocation.invocation_id, "request": invocation.request.document(),
            "checkout": str(invocation.checkout), "root": str(invocation.root),
            "platform_os": invocation.platform_os, "platform_arch": invocation.platform_arch,
            "directory_identity": invocation.directory_identity, "argv": argv,
        }
        stages = {name: Stage("pending", {}, {}) for name in receipt_stages(invocation.request)}
        receipt = cls(invocation, metadata, stages)
        receipt.save(create=True)
        return receipt

    @classmethod
    def load(cls, path: Path) -> Receipt:
        value = object_value(parse_json(read_bounded_regular_file(path, MAX_RECEIPT)))
        if (set(value) != {"schema_version", "kind", "metadata", "stages", "status", "reason"}
                or type(value["schema_version"]) is not int or value["schema_version"] != 1):
            raise ReleaseError("release_receipt_invalid")
        parse = request_parser(value["kind"])
        metadata = object_value(value["metadata"])
        request = parse(metadata.get("request"))
        invocation = Invocation(
            invocation_id=text_value(metadata.get("invocation_id"), "[0-9a-f]{32}"),
            request=request, checkout=Path(text_value(metadata.get("checkout"))),
            root=Path(text_value(metadata.get("root"))),
            platform_os=text_value(metadata.get("platform_os"), "Linux|Darwin"),
            platform_arch=text_value(metadata.get("platform_arch"), "amd64|arm64"),
            directory_identity=object_value(metadata.get("directory_identity")),
        )
        if (not invocation.checkout.is_absolute() or not invocation.root.is_absolute()
                or path != invocation.root / "receipt.json"
                or invocation.root.name != invocation_name(request, invocation.invocation_id)):
            raise ReleaseError("release_receipt_location_mismatch")
        rows = object_value(value["stages"])
        if set(rows) != set(receipt_stages(request)):
            raise ReleaseError("release_receipt_stages_invalid")
        receipt = cls(invocation, metadata, {name: Stage.parse(row) for name, row in rows.items()})
        receipt.status, receipt.reason = _outcome(request, value["status"], value["reason"])
        return receipt

    def stage(self, name: str) -> Stage:
        return deepcopy(self._stages[name])

    def intend(self, name: str, intent: dict[str, object]) -> bool:
        old = self.stage(name)
        if old.state != "pending":
            if old.intent != intent:
                raise ReleaseError("release_intent_conflict", incomplete=True)
            return False
        self._stages[name] = Stage("intended", deepcopy(intent), {})
        self.save()
        return True

    def complete(self, name: str, result: dict[str, object]) -> None:
        old = self.stage(name)
        if old.state == "pending" or old.state == "complete" and old.result != result:
            raise ReleaseError("release_completion_conflict", incomplete=True)
        self._stages[name] = Stage("complete", old.intent, deepcopy(result))
        self.save()

    def report(self, status: str, reason: str) -> None:
        """Validate before any assignment, so a refused outcome leaves this object and its file unchanged."""
        self.status, self.reason = _outcome(self.invocation.request, status, reason)
        self.save()

    def save(self, *, create: bool = False) -> None:
        value: dict[str, object] = {
            "schema_version": 1, "kind": receipt_kind(self.invocation.request), "metadata": self._metadata,
            "stages": {name: row.document() for name, row in self._stages.items()},
            "status": self.status, "reason": self.reason,
        }
        write_json(self.path, value, replace=not create)


def _outcome(request: Request, status: object, reason: object) -> tuple[str, str]:
    """The status and reason every load accepts; ``report`` writes nothing else."""
    return status_value(request, status), text_value(reason)


def lock_name(request: Request) -> str:
    """One lock per release tag, candidate attempt or publication reference, each in its own namespace.

    A repository always holds a slash, so no release identity starts with a
    candidate or publication kind's line. A publication lock binds the
    repository and exact frozen reference only; the selected readiness run and
    resume choice never select another lock. It is this host's owner lock, not
    a distributed publication lock: hosted release-wide concurrency serializes
    remote delivery.
    """
    if isinstance(request, CandidateRequest):
        identity = "\n".join((CANDIDATE_RECEIPT_KIND, request.repository, request.candidate_id, str(request.attempt)))
    elif isinstance(request, PublicationRequest):
        reference = request.reference
        identity = "\n".join((PUBLICATION_RECEIPT_KIND, request.repository, reference.candidate_id,
                              reference.freeze_commit, reference.merged_commit))
    else:
        identity = f"{request.repo}\n{request.tag}"
    return "." + hashlib.sha256(identity.encode("utf-8")).hexdigest() + ".lock"
