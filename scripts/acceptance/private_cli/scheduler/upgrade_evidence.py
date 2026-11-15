"""Require every selected real upgrade scenario's completed receipt before exit0."""

import json
from pathlib import Path

from scripts.acceptance.evidence.validation import require
from scripts.acceptance.private_cli.scheduler.occurrences import identity, json_object
from scripts.acceptance.private_cli.scheduler.releases import RELEASES


MAX_RECEIPT = 1 << 20


def verify_upgrade_output(output: Path, baseline: str, candidate: str) -> None:
    selected = tuple(release for release in RELEASES if baseline in ("all", release.version))
    require(bool(selected), "upgrade verification selected no baseline")
    require({path.name for path in output.iterdir()} == {release.version for release in selected},
            "upgrade output omits or adds a baseline scenario")
    for release in selected:
        path = output / release.version / "receipt.json"
        require(path.is_file() and not path.is_symlink() and path.stat().st_size <= MAX_RECEIPT,
                "upgrade scenario receipt is unavailable or oversized")
        receipt = json_object(json.loads(path.read_bytes()))
        require(receipt.get("result") == "PASS" and receipt.get("status") == "passed"
                and receipt.get("safe_stage") == "scheduler-release-upgrade-proved"
                and not receipt.get("failure"), "upgrade scenario did not prove its required cases")
        cleanup = json_object(receipt.get("cleanup"))
        root = json_object(cleanup.get("private_root"))
        require(cleanup.get("status") == "passed" and root.get("status") == "removed"
                and root.get("verified") is True and cleanup.get("pending_supervisor_count") == 0,
                "upgrade scenario cleanup is incomplete")
        distribution = json_object(receipt.get("historical_distribution"))
        require(distribution.get("version") == release.version and distribution.get("source_commit") == release.commit
                and distribution.get("checksums_sha256") == release.checksums_sha256
                and distribution.get("verification") == "official-pinned-distribution",
                "upgrade scenario lost official baseline integrity evidence")
        migration = json_object(receipt.get("scheduler_upgrade"))
        require(migration.get("baseline") == release.version and migration.get("candidate") == candidate
                and migration.get("from_schema") == release.schema and migration.get("to_schema") == 2
                and all(migration.get(key) is True for key in (
                    "explicit_migration", "preservation_before_new_runtime", "old_host_joined", "old_tasks_closed",
                    "doctor_read_only", "reliability_state_preserved"))
                and migration.get("doctor_before") == ("migration_required" if release.schema == 1 else "compatible")
                and migration.get("doctor_after") == "compatible",
                "upgrade scenario omitted its stopped-writer preservation proof")
        proof = json_object(receipt.get("upgrade_acceptance"))
        require(proof.get("preserved_definitions") == 3 and proof.get("schema_verified") == 2
                and proof.get("future_ticks") == 2 and all(proof.get(key) is True for key in (
                    "pause_resume", "candidate_restart", "unobserved_closed_reservation_reconciled", "no_replay_or_backfill",
                    "immutable_evidence_after_runtime", "history_skip_coverage")),
                "upgrade scenario omitted a required runtime partition")
        bounds, dues = proof.get("candidate_resume_bounds"), proof.get("future_due_times")
        if not isinstance(bounds, list) or not isinstance(dues, list) or len(bounds) != 2 or len(dues) != 2:
            raise RuntimeError("upgrade scenario omitted candidate startup bounds")
        require(all(type(bound) is int and type(due) is int and 0 < bound < due and due % 60_000_000_000 == 0
                    for bound, due in zip(bounds, dues)), "upgrade candidate dispatched a historical startup tick")
        ids = proof.get("independent_task_ids")
        if not isinstance(ids, list):
            raise RuntimeError("upgrade scenario omitted independent task identities")
        parsed = tuple(identity(item) for item in ids)
        require(len(parsed) >= 4 and len(set(parsed)) == len(parsed)
                and proof.get("provider_entries") == len(parsed), "upgrade provider entries and occurrence IDs disagree")
