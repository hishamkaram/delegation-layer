"""Single owner for compact production OpenCode ACP evidence of admitted tasks."""

from __future__ import annotations

import posixpath
from pathlib import Path
from typing import Mapping

from scripts.delegate_acceptance.evidence.files import digest, path_present, read_json
from scripts.delegate_acceptance.evidence.validation import (
    AcceptanceFailure,
    _hex_digest,
    _task_identity,
    require,
)

# The pure release contract owns every ACP identity pin; this producer only
# projects admitted records into that contract.
from scripts.delegate_acceptance.native_evidence.production_acp import (
    ACP_PREDICATE_VERSION,
    ACP_PROFILE_REVISION,
    OPENCODE_PROVIDER,
    PRODUCTION_ACP_KIND,
    PRODUCTION_ACP_SCHEMA_VERSION,
)

# Task-directory entries the store creates only for interactive tasks.
INTERACTION_JOURNAL_ENTRIES = ("interaction", ".interaction.lock", "raw/interaction")


def _absent(path: Path, label: str) -> bool:
    try:
        return not path_present(path)
    except OSError as error:
        raise AcceptanceFailure(f"{label} cannot be inspected: {error}") from error


def _admitted_acp_predicate(task_id: str, meta: dict[str, object]) -> dict[str, object]:
    effective = meta.get("effective_config")
    policy = effective.get("policy") if isinstance(effective, dict) else None
    require(isinstance(policy, dict) and policy.get("profile_revision") == ACP_PROFILE_REVISION,
            f"task {task_id} was not admitted with the production OpenCode ACP profile")
    predicate = meta.get("predicate")
    require(isinstance(predicate, dict) and predicate.get("adapter") == OPENCODE_PROVIDER and
            predicate.get("version") == ACP_PREDICATE_VERSION and
            _hex_digest(predicate.get("sha256")),
            f"task {task_id} does not bind the production ACP predicate")
    return predicate


def _require_noninteractive(task_id: str, directory: Path,
                            record: dict[str, object], meta: dict[str, object]) -> None:
    for requested in (record.get("requested_config"), meta.get("requested_config")):
        require(isinstance(requested, dict) and requested.get("interactive") in (None, False),
                f"task {task_id} requested interactive execution")
    for name in INTERACTION_JOURNAL_ENTRIES:
        require(_absent(directory / name, f"task {task_id} interaction entry {name}"),
                f"task {task_id} has a user interaction journal entry: {name}")


def _session_binding(task_id: str, directory: Path, record: dict[str, object],
                     meta_digest: str, spec_digest: str) -> tuple[str, str | None]:
    reference = read_json(directory / "provider.ref.json")
    session = reference.get("conversation_id")
    require(reference.get("task_id") == task_id and reference.get("provider") == OPENCODE_PROVIDER and
            reference.get("spec_sha256") == spec_digest and
            reference.get("meta_sha256") == meta_digest and
            isinstance(session, str) and bool(session),
            f"task {task_id} has no exact ACP session binding")
    prior = record.get("prior_session")
    if prior is None:
        return session, None
    predecessor = prior.get("predecessor_task_id") if isinstance(prior, dict) else None
    require(isinstance(prior, dict) and prior.get("provider") == OPENCODE_PROVIDER and
            isinstance(predecessor, str) and _task_identity(predecessor) and
            prior.get("conversation_id") == session,
            f"task {task_id} did not resume its exact predecessor ACP session")
    return session, predecessor


def _committed_seal(task_id: str, directory: Path,
                    predicate: dict[str, object]) -> dict[str, str]:
    seal = read_json(directory / "provider.exit")
    outcome = read_json(directory / "outcome.json")
    manifest = seal.get("raw_manifest")
    manifest_digest = seal.get("manifest_sha256")
    require(seal.get("predicate") == predicate and outcome.get("predicate") == predicate and
            outcome.get("verdict") == "committed" and isinstance(manifest, list) and
            all(isinstance(entry, dict) and isinstance(entry.get("path"), str) and
                posixpath.basename(entry["path"]) != "interaction" for entry in manifest) and
            _hex_digest(manifest_digest) and outcome.get("evidence_sha256") == manifest_digest,
            f"task {task_id} lacks a committed production ACP core seal")
    return {
        "manifest_sha256": manifest_digest,
        "seal_sha256": digest(directory / "provider.exit"),
        "outcome_sha256": digest(directory / "outcome.json"),
    }


def production_acp_marker(
        *, task_id: str, directory: Path, record: dict[str, object], meta: dict[str, object],
        meta_digest: str, spec_digest: str, committed: bool) -> dict[str, object]:
    """Project one admitted task's production ACP facts without raw output.

    A committed task binds its core seal and outcome. An uncommitted timeout
    source records only that it published nothing; its disposition is owned
    by the timeout collector and is deliberately not reinterpreted here.
    """
    require(_task_identity(task_id) and _hex_digest(meta_digest) and _hex_digest(spec_digest),
            f"task {task_id} production ACP identity is invalid")
    predicate = _admitted_acp_predicate(task_id, meta)
    _require_noninteractive(task_id, directory, record, meta)
    session, predecessor = _session_binding(task_id, directory, record, meta_digest, spec_digest)
    if committed:
        seal = _committed_seal(task_id, directory, predicate)
        publication = "committed"
    else:
        require(_absent(directory / "outcome.json", f"task {task_id} outcome"),
                f"task {task_id} uncommitted ACP source published an outcome")
        seal = {"manifest_sha256": None, "seal_sha256": None, "outcome_sha256": None}
        publication = "unpublished"
    return {
        "task_id": task_id, "session_id": session, "predecessor_task_id": predecessor,
        "profile_revision": ACP_PROFILE_REVISION, "predicate_version": ACP_PREDICATE_VERSION,
        "predicate_sha256": predicate["sha256"],
        "spec_sha256": spec_digest, "meta_sha256": meta_digest,
        "noninteractive": True, "interaction_journal": "absent",
        "publication": publication, **seal,
    }


class ProductionAcpEvidence:
    """Record each admitted task's ACP marker once and project the receipt."""

    def __init__(self) -> None:
        self._by_task_id: dict[str, dict[str, object]] = {}

    def record(self, task_id: str, marker: dict[str, object]) -> None:
        require(_task_identity(task_id) and marker.get("task_id") == task_id,
                f"task {task_id} production ACP marker identity is invalid")
        existing = self._by_task_id.get(task_id)
        require(existing is None or existing == marker,
                f"task {task_id} changed its production ACP marker")
        self._by_task_id[task_id] = dict(marker)

    def receipt(self, tasks: Mapping[str, str]) -> dict[str, object]:
        require(bool(tasks) and len(set(tasks.values())) == len(tasks) and
                set(tasks.values()) == set(self._by_task_id),
                "production ACP evidence coverage is incomplete")
        return {
            "schema_version": PRODUCTION_ACP_SCHEMA_VERSION, "kind": PRODUCTION_ACP_KIND,
            "profile_revision": ACP_PROFILE_REVISION, "predicate_version": ACP_PREDICATE_VERSION,
            "tasks": {alias: dict(self._by_task_id[task_id])
                      for alias, task_id in sorted(tasks.items())},
        }
