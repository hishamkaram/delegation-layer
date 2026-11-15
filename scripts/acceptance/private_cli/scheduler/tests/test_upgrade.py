"""Bounded upgrade-driver contracts; no distribution download or provider launch."""

from dataclasses import replace
import hashlib
import io
import json
import os
from copy import deepcopy
from pathlib import Path
import sqlite3
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

from scripts.acceptance.private_cli import installation
from scripts.acceptance.private_cli.scheduler import releases
from scripts.acceptance.private_cli.scheduler import upgrade
from scripts.acceptance.private_cli.scheduler import upgrade_snapshot
from scripts.acceptance.private_cli.scheduler.upgrade_evidence import verify_upgrade_output
from scripts.acceptance.private_cli.scheduler.tests import test_default as default_tests


class UpgradeReleaseTests(unittest.TestCase):
    def test_release_selection_pins_both_schema_generations_and_all_native_archives(self):
        self.assertEqual([(r.version, r.schema) for r in releases.RELEASES], [("0.4.2", 1), ("0.5.1", 2)])
        for release in releases.RELEASES:
            for system in ("Linux", "Darwin"):
                for machine in ("x86_64", "arm64"):
                    name, digest = release.asset(system, machine)
                    self.assertIn(release.version, name)
                    self.assertEqual(len(digest), 64)
            with self.assertRaisesRegex(RuntimeError, "support"):
                release.asset("Windows", "x86_64")

    def test_archive_integrity_rejects_changed_download_before_extraction(self):
        with tempfile.TemporaryFile() as target:
            def download(_url, stream, _limit, **_options):
                stream.write(b"different archive bytes")
            with patch.object(releases.github_release_asset, "download_asset", side_effect=download):
                with self.assertRaisesRegex(RuntimeError, "integrity"):
                    releases.verified_download("https://example.invalid/archive", target, "0" * 64, 128)

    def test_transport_receives_only_explicit_download_token_without_recording_it(self):
        content = b"controlled archive bytes"
        secret = "PRIVATE_DOWNLOAD_TOKEN_CANARY"
        def download(_url, stream, _limit, *, token):
            self.assertEqual(token, secret)
            stream.write(content)
        with tempfile.TemporaryFile() as target, patch.dict(os.environ, {releases.github_release_asset.TOKEN_ENV: secret}), patch.object(releases.github_release_asset, "download_asset", side_effect=download):
            releases.verified_download("https://example.invalid/archive", target, hashlib.sha256(content).hexdigest(), 128)
            self.assertEqual(target.read(), content)

    def bundle(self, *, bad=""):
        stream = io.BytesIO()
        with tarfile.open(fileobj=stream, mode="w:gz") as bundle:
            for name in releases.MEMBERS:
                item = tarfile.TarInfo("../delegate" if bad == "path" and name == "delegate" else name)
                item.mode = 0o755
                data = b"fixture executable input"
                item.size = len(data)
                if bad == "symlink" and name == "delegate":
                    item.type = tarfile.SYMTYPE
                    item.linkname = "/private/unowned"
                    item.size = 0
                bundle.addfile(item, io.BytesIO(data))
            if bad == "duplicate":
                item = tarfile.TarInfo("delegate")
                item.mode, item.size = 0o755, 1
                bundle.addfile(item, io.BytesIO(b"x"))
        stream.seek(0)
        return stream

    def test_distribution_extraction_validates_entire_inventory_before_writing(self):
        for fault in ("path", "symlink", "duplicate"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as directory:
                path = Path(directory)
                with self.assertRaises(RuntimeError):
                    releases.extract_pair(self.bundle(bad=fault), path)
                self.assertEqual(list(path.iterdir()), [])

    def test_distribution_extraction_selects_pair_without_executable_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            with patch.object(releases.hashlib, "sha256", side_effect=AssertionError("no installed executable digest")):
                releases.extract_pair(self.bundle(), path)
            self.assertEqual({p.name for p in path.iterdir()}, {"delegate", "delegate-run"})
            self.assertEqual((path / "delegate").stat().st_mode & 0o777, 0o700)
            with self.assertRaisesRegex(RuntimeError, "occupied"):
                releases.extract_pair(self.bundle(), path)

    def test_release_receipt_is_published_only_after_verified_extraction(self):
        archive = self.bundle().getvalue()
        release = releases.RELEASES[0]
        asset, _ = release.asset("Linux", "x86_64")
        digest = hashlib.sha256(archive).hexdigest()
        checksums = (digest + "  " + asset + "\n").encode()
        selected = replace(release, checksums_sha256=hashlib.sha256(checksums).hexdigest(),
                           archives=(("linux", "amd64", digest),))
        evidence = {}
        with tempfile.TemporaryDirectory() as directory:
            def download(url, target, _limit, **_options):
                target.write(checksums if url.endswith("checksums.txt") else archive)
            with patch.object(releases.platform, "system", return_value="Linux"), patch.object(releases.platform, "machine", return_value="x86_64"), patch.object(releases.github_release_asset, "download_asset", side_effect=download):
                releases.ReleaseInstaller(selected, evidence)(Path(directory))
        self.assertEqual(evidence["archive_sha256"], digest)
        self.assertEqual(evidence["source_commit"], release.commit)
        self.assertNotIn("delegate_sha256", evidence)

    def test_candidate_version_is_explicit_and_invalid_linker_values_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaises(ValueError):
                installation.build_candidate_pair(root, root, version="0.6.1 -X injected")
            with patch.object(installation, "require_source_root", return_value=root), patch.object(installation, "_install_command") as command, patch.object(installation.os, "access", return_value=True), patch.object(Path, "is_file", return_value=True):
                installation.build_candidate_pair(root, root, version="0.6.1")
            self.assertEqual(command.call_count, 2)
            for call in command.call_args_list:
                self.assertIn("-X main.version=0.6.1", call.args[0])


class UpgradeSnapshotTests(unittest.TestCase):
    def test_migration_preserves_exact_rows_but_allows_schema_version_change(self):
        rows = (("schedules", ("schedule_id", "state", "request_json"), (("b" * 32, "paused", b'{"provider":"pi:json"}'),)),
                ("skip_spans", ("reason",), (("paused",),)))
        before = upgrade_snapshot.StoreSnapshot(1, "a" * 32, rows, (("c" * 32, {"outcome": "sealed"}),))
        empty = tuple((name, ("id",), ()) for name in upgrade_snapshot.RELIABILITY_TABLES)
        after = replace(before, version=2, tables=rows + empty)
        upgrade_snapshot.require_preserved(before, after)
        for changed in (replace(after, root_id="f" * 32), replace(after, tables=()), replace(after, tasks=()), replace(after, version=1)):
            with self.subTest(changed=changed), self.assertRaises(RuntimeError):
                upgrade_snapshot.require_preserved(before, changed)
        invented = replace(after, tables=rows + empty[:-1] + ((empty[-1][0], ("id",), (("fabricated",),)),))
        with self.assertRaisesRegex(RuntimeError, "invented"):
            upgrade_snapshot.require_preserved(before, invented)

    def test_schema2_snapshot_preserves_every_reliability_owner_table(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory)
            (state / ".scheduler").mkdir()
            database = state / ".scheduler/schedules.sqlite"
            with sqlite3.connect(database) as connection:
                connection.execute("PRAGMA user_version=2")
                for name, columns in (("scheduler_metadata", "singleton INTEGER, root_id TEXT"),
                                      ("schedules", "schedule_id TEXT"), ("occurrences", "task_id TEXT"),
                                      ("skip_spans", "schedule_id TEXT,to_unix_nanos INTEGER")):
                    connection.execute("CREATE TABLE " + name + "(" + columns + ")")
                connection.execute("INSERT INTO scheduler_metadata VALUES(1,?)", ("a" * 32,))
                for name in upgrade_snapshot.RELIABILITY_TABLES:
                    key = upgrade_snapshot.TABLE_KEYS[name]
                    connection.execute("CREATE TABLE " + name + "(" + key + " TEXT, value BLOB)")
                    connection.execute("INSERT INTO " + name + " VALUES(?,?)", ("c" * 32, b"owner evidence"))
            before = upgrade_snapshot.snapshot(state, "a" * 32)
            self.assertEqual({name for name, _columns, _rows in before.tables},
                             set(upgrade_snapshot.PRESERVED_TABLES + upgrade_snapshot.RELIABILITY_TABLES))
            for name in upgrade_snapshot.RELIABILITY_TABLES:
                with self.subTest(table=name), sqlite3.connect(database) as connection:
                    connection.execute("UPDATE " + name + " SET value=?", (b"corrupted evidence",))
                    connection.commit()
                    after = upgrade_snapshot.snapshot(state, "a" * 32)
                    with self.assertRaisesRegex(RuntimeError, "upgrade changed"):
                        upgrade_snapshot.require_preserved(before, after)
                    connection.execute("UPDATE " + name + " SET value=?", (b"owner evidence",))

    def test_runtime_oracle_preserves_old_evidence_but_allows_owner_observation_and_cursor(self):
        definition_columns = ("schedule_id", "created_at_unix_nanos", "cron", "timezone", "request_json", "brief",
                              "brief_sha256", "definition_sha256", "state", "cursor_unix_nanos")
        occurrence_columns = ("task_id", "schedule_id", "due_unix_nanos", "armed_at_unix_nanos", "observed_at_unix_nanos",
                              "released_at_unix_nanos", "release_kind", "release_ref")
        definition = ("b" * 32, 1, "* * * * *", "UTC", b"request", b"brief", "digest", "definition", "paused", 1)
        occurrence = ("c" * 32, "b" * 32, 60, 61, 62, None, None, None)
        # Populated controlled facts challenge the subset oracle; the healthy
        # real-release scenario deliberately has no failed-admission attempts.
        reliability = (("schedule_hooks", ("schedule_id", "hook_json"), (("b" * 32, b"hook"),)),
                       ("occurrence_attempts", ("task_id", "attempt_json"), (("c" * 32, b"attempt"),)),
                       ("schedule_holds", ("schedule_id",), ()), ("notification_attempts", ("event_id",), ()),
                       ("schedule_policy", ("schedule_id", "enabled", "max_hold", "source_floor"), (("b" * 32, 1, 120, 1),)),
                       ("occurrence_results", ("task_id", "verdict", "observed_at"),
                        (("c" * 32, "committed", 100), ("d" * 32, "unknown", 101))))
        before = upgrade_snapshot.StoreSnapshot(2, "a" * 32,
            (("schedules", definition_columns, (definition,)), ("occurrences", occurrence_columns, (occurrence,)),
             ("skip_spans", ("id",), (("old skip",),))) + reliability,
            (("c" * 32, {"raw": "original"}),), (("c" * 32, "closed"),))
        tables = (("schedules", definition_columns, (definition[:-1] + (120,),)),
                  ("occurrences", occurrence_columns, (occurrence[:4] + (130, 131, "closed", "evidence"),)), before.tables[2])
        tables += reliability[:-1] + ((reliability[-1][0], reliability[-1][1],
                                      (("c" * 32, "committed", 100), ("d" * 32, "committed", 130))),)
        after = replace(before, tables=tables)
        upgrade_snapshot.require_runtime_preserved(before, after, "b" * 32)
        for corrupted in (replace(after, tasks=(("c" * 32, {"raw": "changed"}),)), replace(after, closure=()),
                          replace(after, tables=(("schedules", definition_columns,
                            (definition[:4] + (b"different request",) + definition[5:],)),) + tables[1:]),
                          replace(after, tables=tables[:1] + (("occurrences", occurrence_columns,
                            (("d" * 32,) + occurrence[1:],)),) + tables[2:])):
            with self.subTest(corrupted=corrupted), self.assertRaises(RuntimeError):
                upgrade_snapshot.require_runtime_preserved(before, corrupted, "b" * 32)
        for name in ("schedule_hooks", "occurrence_attempts", "schedule_policy", "occurrence_results"):
            for drop in (False, True):
                changed = tuple((table, columns, () if drop else (rows[0][:-1] + ("corrupted",),))
                                if table == name else (table, columns, rows) for table, columns, rows in tables)
                with self.subTest(table=name, drop=drop), self.assertRaisesRegex(RuntimeError, "reliability"):
                    upgrade_snapshot.require_runtime_preserved(before, replace(after, tables=changed), "b" * 32)

    def test_snapshot_refuses_wrong_root_and_never_initializes_missing_store(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory)
            with self.assertRaises(FileNotFoundError):
                upgrade_snapshot.snapshot(state, "a" * 32)
            (state / ".scheduler").mkdir()
            database = state / ".scheduler/schedules.sqlite"
            with sqlite3.connect(database) as connection:
                connection.execute("PRAGMA user_version=1")
                connection.execute("CREATE TABLE scheduler_metadata(singleton INTEGER,root_id TEXT)")
                connection.execute("INSERT INTO scheduler_metadata VALUES(1,?)", ("b" * 32,))
            content = database.read_bytes()
            with self.assertRaisesRegex(RuntimeError, "root"):
                upgrade_snapshot.snapshot(state, "a" * 32)
            self.assertEqual(database.read_bytes(), content)


class UpgradeSelectionTests(unittest.TestCase):
    def setUp(self):
        self.fixture_case = default_tests.SchedulerDefaultTests()
        self.fixture_case.setUp()
        self.addCleanup(self.fixture_case.doCleanups)

    def test_handoff_selects_candidate_for_every_later_command_and_restart(self):
        case = self.fixture_case.case(use_default=False)
        case.upgraded = True
        self.assertEqual(case.delegate, self.fixture_case.fixture.upgraded_install / "delegate")
        self.fixture_case.lifetime.upgraded_libexec = self.fixture_case.base / "new-libexec"
        self.assertEqual(case.supervisor_directory, self.fixture_case.lifetime.upgraded_libexec)

    def test_transition_refuses_live_old_host_before_any_candidate_command(self):
        case = self.fixture_case.case(use_default=False)
        with patch.object(case, "command") as command:
            with self.assertRaisesRegex(RuntimeError, "old host Wait"):
                upgrade.transition(case, Mock(), releases.RELEASES[0], candidate_version="0.6.1")
        command.assert_not_called()
        self.assertFalse(case.upgraded)

    def test_doctor_must_preserve_database_and_sidecars_and_keep_advisory_separate(self):
        case = self.fixture_case.case(use_default=False)
        directory = case.fixture.state / ".scheduler"
        directory.mkdir()
        database = directory / "schedules.sqlite"
        database.write_bytes(b"controlled database evidence")
        response = {"issues": [], "scheduler": {"compatibility": "migration_required", "verification": "read_only",
                                              "issues": [{"code": "migration_required"}]}}
        with patch.object(case, "command", return_value=response):
            upgrade.observe_doctor(case, "before", "migration_required")
        def changed(*_args):
            (directory / "schedules.sqlite-journal").write_bytes(b"new sidecar")
            return response
        with patch.object(case, "command", side_effect=changed), self.assertRaisesRegex(RuntimeError, "sidecars"):
            upgrade.observe_doctor(case, "changed", "migration_required")

    def test_occurrence_poll_retries_only_native_contention(self):
        case = self.fixture_case.case(use_default=False)
        busy = sqlite3.OperationalError("controlled lock")
        busy.sqlite_errorcode = sqlite3.SQLITE_BUSY
        row = upgrade.Occurrence("a" * 32, "b" * 32, 60_000_000_000)
        with patch.object(case, "discover", side_effect=[busy, (row,)]):
            self.assertEqual(upgrade.rows_for(case, row.schedule_id, 1), (row,))
        with patch.object(case, "discover", side_effect=sqlite3.OperationalError("bad database")):
            with self.assertRaises(sqlite3.OperationalError):
                upgrade.rows_for(case, row.schedule_id, 1)

    def test_history_requires_every_skipped_minute_and_exact_occurrence_identity(self):
        from scripts.acceptance.private_cli.scheduler.occurrences import Occurrence
        rows = (Occurrence("a" * 32, "b" * 32, 60_000_000_000), Occurrence("c" * 32, "b" * 32, 180_000_000_000))
        entries = [{"kind": "occurrence", "occurrence": {"task_id": row.task_id,
                    "due": "1970-01-01T00:0" + minute + ":00Z"}} for row, minute in zip(rows, ("1", "3"))]
        response: dict[str, object] = {"schedule_history": {"entries": entries + [{"kind": "skip", "skip": {
            "from": "1970-01-01T00:01:00Z", "to": "1970-01-01T00:02:00Z", "reason": "paused"}}]}}
        upgrade.require_history_coverage(response, rows)
        with self.assertRaisesRegex(RuntimeError, "paused coverage"):
            upgrade.require_history_coverage({"schedule_history": {"entries": entries}}, rows)
        huge = deepcopy(response)
        huge_history = upgrade.json_object(huge["schedule_history"])
        huge_entries = huge_history["entries"]
        if not isinstance(huge_entries, list):
            self.fail("controlled history fixture omitted entries")
        huge_entries[1]["occurrence"]["due"] = "1970-01-01T05:00:00Z"
        with self.assertRaisesRegex(RuntimeError, "gap inventory"):
            upgrade.require_history_coverage(huge, (rows[0], replace(rows[1], due_ns=300 * 60_000_000_000)))


class UpgradeReceiptTests(unittest.TestCase):
    def receipt(self):
        release = releases.RELEASES[0]
        return {"result": "PASS", "status": "passed", "safe_stage": "scheduler-release-upgrade-proved",
                "cleanup": {"status": "passed", "private_root": {"status": "removed", "verified": True}, "pending_supervisor_count": 0},
                "historical_distribution": {"version": release.version, "source_commit": release.commit,
                    "checksums_sha256": release.checksums_sha256, "verification": "official-pinned-distribution"},
                "scheduler_upgrade": {"baseline": release.version, "candidate": "0.6.1", "from_schema": 1, "to_schema": 2,
                    "explicit_migration": True, "preservation_before_new_runtime": True, "old_host_joined": True, "old_tasks_closed": True,
                    "doctor_before": "migration_required", "doctor_after": "compatible", "doctor_read_only": True,
                    "reliability_state_preserved": True},
                "upgrade_acceptance": {"preserved_definitions": 3, "schema_verified": 2, "future_ticks": 2,
                    "pause_resume": True, "candidate_restart": True, "unobserved_closed_reservation_reconciled": True,
                    "no_replay_or_backfill": True, "provider_entries": 4,
                    "immutable_evidence_after_runtime": True, "history_skip_coverage": True,
                    "candidate_resume_bounds": [2, 60_000_000_002],
                    "future_due_times": [60_000_000_000, 120_000_000_000],
                    "independent_task_ids": [letter * 32 for letter in "abcd"]}}

    def test_exit_zero_requires_every_runtime_partition_and_canonical_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            baseline = output / "0.4.2"
            baseline.mkdir()
            receipt = baseline / "receipt.json"
            good = self.receipt()
            receipt.write_text(json.dumps(good))
            verify_upgrade_output(output, "0.4.2", "0.6.1")
            mutations = (
                ("cleanup", "status", "retained"),
                ("scheduler_upgrade", "old_tasks_closed", False),
                ("scheduler_upgrade", "preservation_before_new_runtime", False),
                ("scheduler_upgrade", "from_schema", 2),
                ("scheduler_upgrade", "doctor_before", "compatible"),
                ("scheduler_upgrade", "doctor_read_only", False),
                ("scheduler_upgrade", "reliability_state_preserved", False),
                ("upgrade_acceptance", "future_ticks", 1),
                ("upgrade_acceptance", "candidate_restart", False),
                ("upgrade_acceptance", "immutable_evidence_after_runtime", False),
                ("upgrade_acceptance", "history_skip_coverage", False),
                ("upgrade_acceptance", "future_due_times", [0, 120_000_000_000]),
                ("upgrade_acceptance", "provider_entries", 5),
                ("upgrade_acceptance", "independent_task_ids", ["a" * 32] * 4),
            )
            for section, key, value in mutations:
                changed = deepcopy(good)
                changed[section][key] = value
                receipt.write_text(json.dumps(changed))
                with self.subTest(key=key), self.assertRaises(RuntimeError):
                    verify_upgrade_output(output, "0.4.2", "0.6.1")
            receipt.write_text(json.dumps(good))
            with self.assertRaisesRegex(RuntimeError, "baseline"):
                verify_upgrade_output(output, "all", "0.6.1")


if __name__ == "__main__":
    unittest.main()
