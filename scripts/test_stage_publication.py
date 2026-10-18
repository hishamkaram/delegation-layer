"""Real interrupted staging publications consumed by the public-proof boundary."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

from scripts import test_public_native_proof as fixtures
from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import (
    DIRECTORY_CREATION_PREFIX, DirectoryOwner, is_creation_temporary_name,
)
from scripts.delegate_acceptance.evidence import files as atomic_files
from scripts.delegate_acceptance.native_evidence.contract import (
    EvidenceError, MAX_AGGREGATE_BYTES, MAX_RECEIPT_BYTES,
    EXPECTED_CASES,
)
from scripts.delegate_acceptance.native_evidence.public_projection import build_public_proof
from scripts.delegate_acceptance.native_evidence.public_validation import validate_public_proof
from scripts.delegate_acceptance.native_evidence.stage_publication import StagePlan, publish_stage
from scripts.delegate_acceptance.native_evidence.staging import _check_source_digests, stage_evidence
from scripts.delegate_acceptance.processes.operations import run_bounded_command


SUMMARIES = {"aggregate-receipt.json", "archive-receipt.json", "native-acceptance-receipt.json"}


def _load_plan(source: Path, output: Path) -> StagePlan:
    manifest = json.loads((source / "manifest.json").read_bytes())
    plan = StagePlan(output)
    for row in manifest["files"]:
        name = row["path"]
        bound = MAX_AGGREGATE_BYTES if name in SUMMARIES else MAX_RECEIPT_BYTES
        plan.put(output / name, (source / name).read_bytes(), bound)
    plan.put(output / "manifest.json", (source / "manifest.json").read_bytes(), MAX_RECEIPT_BYTES)
    return plan


def _snapshot(root: Path) -> dict[str, str]:
    return {path.relative_to(root).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in root.rglob("*") if path.is_file()}


class StagePublicationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source_temporary = tempfile.TemporaryDirectory(prefix="stage-source-")
        cls.addClassCleanup(cls.source_temporary.cleanup)
        cls.source_root = Path(cls.source_temporary.name).resolve(strict=True)
        producer = fixtures._PublicNativeFixture(cls.source_root)
        cls.source = producer.build_stage()
        cls.expectation = producer.expectation
        cls.original = _snapshot(cls.source)

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="stage-replay-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.parent = self.root / "owned-parent"
        self.parent.mkdir()
        self.output = self.parent / "stage"
        owner = DirectoryOwner()
        try:
            token = owner.create("test stage", self.output, self.parent)
            self.identity = directory_identity_record(token)
        finally:
            owner.release()
        self.plan = _load_plan(self.source, self.output)

    def tearDown(self):
        self.assertEqual(_snapshot(self.source), self.original)

    def prove_public_consumer(self):
        if self.expectation is None:
            raise AssertionError("fixture expectation missing")
        result = self.root / "proof.zip"
        build_public_proof(self.output, result, self.expectation)
        verdict = validate_public_proof(result.read_bytes(), self.expectation)
        self.assertEqual(verdict["status"], "PASS")

    def interrupt_after_first_publication(self):
        original = atomic_files.write_bytes
        interrupted = False

        def publish_then_interrupt(path, data, **options):
            nonlocal interrupted
            original(path, data, **options)
            if not interrupted:
                interrupted = True
                raise KeyboardInterrupt

        with patch("scripts.delegate_acceptance.native_evidence.stage_publication.write_bytes",
                   side_effect=publish_then_interrupt):
            with self.assertRaises(KeyboardInterrupt):
                publish_stage(self.plan, self.identity)
        self.assertTrue(interrupted)
        self.assertFalse((self.output / "manifest.json").exists())

    def test_replay_preserves_identical_bytes_and_the_public_consumer(self):
        publish_stage(self.plan, self.identity)
        before = {path: path.stat().st_mtime_ns for path in self.output.rglob("*") if path.is_file()}
        publish_stage(self.plan, self.identity)
        self.assertEqual(before, {path: path.stat().st_mtime_ns for path in before})
        self.prove_public_consumer()

    def test_interrupted_copy_resumes_without_rewriting_originals(self):
        self.interrupt_after_first_publication()
        publish_stage(self.plan, self.identity)
        self.prove_public_consumer()

    def test_canonical_staging_reenters_owned_partial_output(self):
        aggregate = self.source_root / "aggregate" / "aggregate-receipt.json"
        before = _snapshot(aggregate.parent)
        original = atomic_files.write_bytes
        interrupted = False

        def publish_then_interrupt(path, data, **options):
            nonlocal interrupted
            original(path, data, **options)
            if not interrupted:
                interrupted = True
                raise KeyboardInterrupt

        with patch("scripts.delegate_acceptance.native_evidence.stage_publication.write_bytes",
                   side_effect=publish_then_interrupt):
            with self.assertRaises(KeyboardInterrupt):
                stage_evidence(aggregate, self.output, directory_identity=self.identity)
        self.assertTrue(interrupted)
        manifest = stage_evidence(aggregate, self.output, directory_identity=self.identity)
        self.assertEqual(manifest["case_count"], len(EXPECTED_CASES))
        self.assertEqual(_snapshot(aggregate.parent), before)
        self.assertTrue((self.output / "manifest.json").is_file())

    def test_real_crash_recovers_partial_temporary_file_and_link_publication(self):
        for phase in ("partial", "linked", "directory"):
            with self.subTest(phase=phase):
                if phase != "partial":
                    self.setUp()
                identity_file = self.root / "identity.json"
                identity_file.write_text(json.dumps(self.identity))
                script = self.crashing_child_source(phase)
                result = run_bounded_command(
                    [sys.executable, "-c", script, str(self.source), str(self.output), str(identity_file)],
                    dict(os.environ), max_output=4096, timeout=15, capture=True,
                )
                self.assertEqual(result.exit_code, 17)
                remnants = [path for path in self.output.rglob("*")
                            if path.name.endswith(".staging") or is_creation_temporary_name(path.name)]
                self.assertTrue(remnants)
                publish_stage(self.plan, self.identity)
                self.assertFalse(list(self.output.rglob("*.staging")))
                self.assertFalse([path for path in self.output.rglob("*")
                                  if is_creation_temporary_name(path.name)])
                self.prove_public_consumer()

    def crashing_child_source(self, phase):
        common = (
            "import json,os,sys\nfrom pathlib import Path\n"
            "from scripts.test_stage_publication import _load_plan\n"
            "from scripts.delegate_acceptance.evidence import files\n"
            "from scripts.delegate_acceptance.native_evidence.stage_publication import publish_stage\n"
        )
        if phase == "partial":
            hook = (
                "original=files.tempfile.mkstemp\n"
                "def crash(*args,**kwargs):\n"
                " fd,name=original(*args,**kwargs);os.write(fd,b'partial');os.fsync(fd);os._exit(17)\n"
                "files.tempfile.mkstemp=crash\n"
            )
        elif phase == "linked":
            hook = (
                "def crash(source_fd,source_name,destination_fd,destination_name):\n"
                " os.link(source_name,destination_name,src_dir_fd=source_fd,dst_dir_fd=destination_fd,follow_symlinks=False)\n"
                " os._exit(17)\n"
                "files._rename_noreplace=crash\n"
            )
        else:
            hook = (
                "from scripts.delegate_acceptance.directories import operations\n"
                "def crash(*args):\n os._exit(17)\n"
                "operations._rename_noreplace=crash\n"
            )
        return common + hook + (
            "source,output=Path(sys.argv[1]),Path(sys.argv[2])\n"
            "identity=json.loads(Path(sys.argv[3]).read_bytes())\n"
            "publish_stage(_load_plan(source,output),identity)\n"
        )

    def test_creation_temporary_recovery_rejects_malformed_nonempty_and_symlinks(self):
        for kind in ("malformed", "nonempty", "symlink"):
            with self.subTest(kind=kind):
                if kind != "malformed":
                    self.setUp()
                name = DIRECTORY_CREATION_PREFIX + ("invalid" if kind == "malformed" else "0" * 32)
                path = self.output / name
                if kind == "symlink":
                    path.symlink_to(self.source, target_is_directory=True)
                else:
                    path.mkdir()
                    if kind == "nonempty":
                        (path / "foreign").write_bytes(b"preserve")
                with self.assertRaises(EvidenceError):
                    publish_stage(self.plan, self.identity)
                self.assertTrue(path.exists())
                if kind == "nonempty":
                    self.assertEqual((path / "foreign").read_bytes(), b"preserve")

    def test_manifest_publication_interruption_reconciles_committed_bytes(self):
        original = atomic_files._rename_noreplace

        def commit_then_interrupt(source_fd, source_name, destination_fd, destination_name):
            original(source_fd, source_name, destination_fd, destination_name)
            if destination_name == "manifest.json":
                raise KeyboardInterrupt

        with patch.object(atomic_files, "_rename_noreplace", side_effect=commit_then_interrupt):
            with self.assertRaises(KeyboardInterrupt):
                publish_stage(self.plan, self.identity)
        self.assertTrue((self.output / "manifest.json").exists())
        publish_stage(self.plan, self.identity)
        self.prove_public_consumer()

    def test_ambiguous_publish_error_rechecks_matching_bytes_before_progress(self):
        original = atomic_files.write_bytes

        def publish_then_report_failure(path, data, **options):
            original(path, data, **options)
            raise OSError("lost publication acknowledgement")

        with patch("scripts.delegate_acceptance.native_evidence.stage_publication.write_bytes",
                   side_effect=publish_then_report_failure):
            publish_stage(self.plan, self.identity)
        self.prove_public_consumer()

    def test_nested_extras_and_symlinks_are_retained_without_more_writes(self):
        for kind in ("extra", "symlink"):
            with self.subTest(kind=kind):
                if kind == "symlink":
                    self.setUp()
                self.interrupt_after_first_publication()
                extra = self.output / "retention" / "unexpected"
                if kind == "extra":
                    extra.write_bytes(b"foreign")
                else:
                    extra.symlink_to(self.source / "manifest.json")
                before = (self.output / "aggregate-receipt.json").stat().st_mtime_ns
                with self.assertRaisesRegex(EvidenceError, "stage_inventory"):
                    publish_stage(self.plan, self.identity)
                self.assertTrue(extra.exists())
                self.assertEqual((self.output / "aggregate-receipt.json").stat().st_mtime_ns, before)

    def test_replaced_stage_or_parent_identity_refuses_publication(self):
        for parent in (False, True):
            with self.subTest(parent=parent):
                if parent:
                    self.setUp()
                    old = self.root / "original-parent"
                    self.parent.rename(old)
                    self.parent.mkdir()
                    (old / "stage").rename(self.output)
                else:
                    self.output.rename(self.parent / "original-stage")
                    self.output.mkdir()
                with self.assertRaisesRegex(EvidenceError, "stage_directory_identity_changed"):
                    publish_stage(self.plan, self.identity)
                self.assertEqual(list(self.output.iterdir()), [])

    def test_committed_missing_file_and_conflicting_partial_file_fail_closed(self):
        publish_stage(self.plan, self.identity)
        (self.output / "aggregate-receipt.json").unlink()
        with self.assertRaisesRegex(EvidenceError, "stage_committed_inventory_incomplete"):
            publish_stage(self.plan, self.identity)
        self.assertFalse((self.output / "aggregate-receipt.json").exists())
        self.setUp()
        (self.output / "aggregate-receipt.json").write_bytes(b"foreign")
        with self.assertRaisesRegex(EvidenceError, "stage_file_conflict"):
            publish_stage(self.plan, self.identity)
        self.assertEqual((self.output / "aggregate-receipt.json").read_bytes(), b"foreign")

    def test_source_digest_change_refuses_the_reconstructed_plan(self):
        expected = {item.name: hashlib.sha256(item.data).hexdigest()
                    for item in self.plan.files() if item.name in SUMMARIES}
        _check_source_digests(self.plan, expected)
        expected["native-acceptance-receipt.json"] = "0" * 64
        with self.assertRaisesRegex(EvidenceError, "stage_source_digest_changed"):
            _check_source_digests(self.plan, expected)
        self.assertEqual(list(self.output.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
