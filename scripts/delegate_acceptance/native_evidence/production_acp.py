"""Compact production OpenCode ACP proof for native acceptance cases."""

from __future__ import annotations

import json
import re

from ..evidence.digests import sha as _digest_bytes


OPENCODE_PROVIDER = "opencode:run"
ACP_PROFILE_REVISION = "opencode-acp-v1"
# The release proof accepts only the current predicate. Historical acp-v1 task
# replay is owned by the Go task contract and is never promotable evidence here.
ACP_PREDICATE_VERSION = "acp-v2"
PRODUCTION_ACP_KIND = "opencode-production-acp"
PRODUCTION_ACP_SCHEMA_VERSION = 1
PRODUCTION_ACP_STATES = frozenset({"complete", "missing", "invalid"})

_HEX_DIGEST = re.compile(r"^[0-9a-f]{64}$")
_TASK_ID = re.compile(r"^[0-9a-f]{32}$")
# Each continuation alias resumes exactly this predecessor alias.
_PREDECESSOR_ALIAS = {"resume": "fresh", "timeout_resume": "timeout_source"}
# The timeout source publishes nothing; every other turn commits a sealed outcome.
_UNPUBLISHED_ALIASES = frozenset({"timeout_source"})
_DIGEST_KEYS = ("manifest_sha256", "seal_sha256", "outcome_sha256")
_SOURCE_ENVELOPE_KEYS = frozenset({
    "schema_version", "kind", "profile_revision", "predicate_version", "tasks",
})
_SOURCE_TASK_KEYS = frozenset({
    "task_id", "session_id", "predecessor_task_id", "profile_revision", "predicate_version",
    "predicate_sha256", "spec_sha256", "meta_sha256", "noninteractive", "interaction_journal",
    "publication", *_DIGEST_KEYS,
})
_COMPACT_KEYS = frozenset({
    "state", "marker_sha256", "profile_revision", "predicate_version", "predicate_sha256", "tasks",
})
_COMPACT_TASK_KEYS = frozenset({
    "alias", "task_id", "predecessor_task_id", "publication", "spec_sha256", "meta_sha256",
    *_DIGEST_KEYS,
})


def requires_production_acp(provider: str) -> bool:
    """Return whether a case provider must prove the production ACP contract."""
    return provider == OPENCODE_PROVIDER


def _hex(value: object) -> bool:
    return isinstance(value, str) and _HEX_DIGEST.fullmatch(value) is not None


def _turn_is_bound(alias: str, entry: dict[str, object],
                   task_ids: dict[str, str]) -> bool:
    """Check the facts shared by the source marker and its compact projection."""
    predecessor_alias = _PREDECESSOR_ALIAS.get(alias)
    expected_predecessor = task_ids.get(predecessor_alias) if predecessor_alias else None
    if predecessor_alias is not None and expected_predecessor is None:
        return False
    if alias in _UNPUBLISHED_ALIASES:
        publication_bound = (entry.get("publication") == "unpublished"
                             and all(entry.get(key) is None for key in _DIGEST_KEYS))
    else:
        publication_bound = (entry.get("publication") == "committed"
                             and all(_hex(entry.get(key)) for key in _DIGEST_KEYS))
    return (entry.get("task_id") == task_ids.get(alias)
            and isinstance(entry.get("task_id"), str)
            and _TASK_ID.fullmatch(entry["task_id"]) is not None
            and entry.get("predecessor_task_id") == expected_predecessor
            and _hex(entry.get("spec_sha256")) and _hex(entry.get("meta_sha256"))
            and publication_bound)


def _source_turn(alias: str, entry: object, task_ids: dict[str, str],
                 sessions: dict[str, str]) -> dict[str, object] | None:
    if (not isinstance(entry, dict) or set(entry) != _SOURCE_TASK_KEYS
            or entry.get("session_id") != sessions.get(alias)
            or entry.get("profile_revision") != ACP_PROFILE_REVISION
            or entry.get("predicate_version") != ACP_PREDICATE_VERSION
            or not _hex(entry.get("predicate_sha256"))
            or entry.get("noninteractive") is not True
            or entry.get("interaction_journal") != "absent"
            or not _turn_is_bound(alias, entry, task_ids)):
        return None
    return {"alias": alias, **{key: entry[key] for key in sorted(_COMPACT_TASK_KEYS - {"alias"})}}


def compact_production_acp(
        source: dict[str, object], task_sessions: list[dict[str, str]],
        expected_aliases: tuple[str, ...]) -> dict[str, object]:
    """Validate the source ACP marker against the case's sealed task identities.

    A source without a marker, including every historical pre-ACP receipt, is
    explicitly reported as missing; it is never treated as production ACP.
    """
    marker = source.get("production_acp")
    if marker is None:
        return {"state": "missing"}
    task_ids = {binding["alias"]: binding["task_id"] for binding in task_sessions}
    sessions = {binding["alias"]: binding["session_id"] for binding in task_sessions}
    tasks = marker.get("tasks") if isinstance(marker, dict) else None
    if (not isinstance(marker, dict) or set(marker) != _SOURCE_ENVELOPE_KEYS
            or type(marker.get("schema_version")) is not int
            or marker.get("schema_version") != PRODUCTION_ACP_SCHEMA_VERSION
            or marker.get("kind") != PRODUCTION_ACP_KIND
            or marker.get("profile_revision") != ACP_PROFILE_REVISION
            or marker.get("predicate_version") != ACP_PREDICATE_VERSION
            or not isinstance(tasks, dict) or set(tasks) != set(expected_aliases)
            or set(task_ids) != set(expected_aliases)):
        return {"state": "invalid"}
    turns = [_source_turn(alias, tasks[alias], task_ids, sessions)
             for alias in sorted(expected_aliases)]
    if any(turn is None for turn in turns):
        return {"state": "invalid"}
    # Every turn is now validated, so each predicate digest is a hex string.
    predicates = {tasks[alias]["predicate_sha256"] for alias in expected_aliases}
    if len(predicates) != 1:
        return {"state": "invalid"}
    canonical = json.dumps(marker, sort_keys=True, separators=(",", ":"), allow_nan=False)
    return {
        "state": "complete", "marker_sha256": _digest_bytes((canonical + "\n").encode()),
        "profile_revision": ACP_PROFILE_REVISION, "predicate_version": ACP_PREDICATE_VERSION,
        "predicate_sha256": predicates.pop(), "tasks": turns,
    }


def _complete_proof_error(value: dict[str, object], task_ids: dict[str, str],
                          expected_aliases: tuple[str, ...]) -> str | None:
    tasks = value.get("tasks")
    if (set(value) != _COMPACT_KEYS or not _hex(value.get("marker_sha256"))
            or value.get("profile_revision") != ACP_PROFILE_REVISION
            or value.get("predicate_version") != ACP_PREDICATE_VERSION
            or not _hex(value.get("predicate_sha256"))
            or not isinstance(tasks, list) or set(task_ids) != set(expected_aliases)):
        return "aggregate_case_production_acp_invalid"
    aliases = [entry.get("alias") if isinstance(entry, dict) else None for entry in tasks]
    if aliases != sorted(expected_aliases) or any(
            set(entry) != _COMPACT_TASK_KEYS or not _turn_is_bound(entry["alias"], entry, task_ids)
            for entry in tasks):
        return "aggregate_case_production_acp_unbound"
    return None


def production_acp_proof_error(
        provider: str, proof: dict[str, object], *, resolved_pass: bool,
        expected_aliases: tuple[str, ...]) -> str | None:
    """Return the canonical case-proof error for production ACP evidence."""
    if not requires_production_acp(provider):
        return "aggregate_case_production_acp_unexpected" if "production_acp" in proof else None
    value = proof.get("production_acp")
    if value is None and not resolved_pass:
        return None
    state = value.get("state") if isinstance(value, dict) else None
    if not isinstance(state, str) or state not in PRODUCTION_ACP_STATES:
        return "aggregate_case_production_acp_invalid"
    if state != "complete":
        if resolved_pass or set(value) != {"state"}:
            return "aggregate_case_production_acp_incomplete"
        return None
    bindings = proof.get("task_session_bindings")
    if not isinstance(bindings, list) or any(
            not isinstance(binding, dict) or not isinstance(binding.get("alias"), str)
            or not isinstance(binding.get("task_id"), str) for binding in bindings):
        return "aggregate_case_production_acp_unbound"
    task_ids = {binding["alias"]: binding["task_id"] for binding in bindings}
    return _complete_proof_error(value, task_ids, expected_aliases)
