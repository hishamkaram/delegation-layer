"""Composed public-proof projection and portable validation tests."""

from __future__ import annotations

from argparse import Namespace
from dataclasses import replace
from collections.abc import Callable
from io import BytesIO
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import struct
import tempfile
import unittest
from unittest.mock import patch
import zipfile

from scripts import native_acceptance_evidence as evidence
from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.native_evidence.public_contract import (
    CASE_DEFINITIONS,
    MAX_PUBLIC_MEMBER_BYTES,
    MAX_PUBLIC_ZIP_BYTES,
    PUBLIC_MEMBER_NAMES,
    ProofExpectation,
    PublicProofError,
    canonical_public_zip,
    canonical_json,
    sha256,
)
from scripts.delegate_acceptance.native_evidence.public_projection import (
    _fingerprint as private_fingerprint,
    _project_case,
    build_public_proof,
)
from scripts.delegate_acceptance.native_evidence.public_stage import _read_stage_file, load_stage
from scripts.delegate_acceptance.native_evidence.public_validation import (
    inspect_public_proof,
    validate_public_identity,
    validate_public_proof,
)


def _object(value: object) -> dict[str, object]:
    if not isinstance(value, dict):
        raise AssertionError("fixture object expected")
    return value


def _read_members(data: bytes) -> dict[str, bytes]:
    with zipfile.ZipFile(BytesIO(data), "r") as archive:
        return {info.filename: archive.read(info) for info in archive.infolist()}


def _pack_rows(rows: list[tuple[str, bytes]]) -> bytes:
    return canonical_public_zip(dict(rows))


def _pack_unchecked_rows(rows: list[tuple[str, bytes]]) -> bytes:
    buffer = BytesIO()
    with zipfile.ZipFile(buffer, "w", compression=zipfile.ZIP_STORED) as archive:
        for name, data in rows:
            info = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_STORED
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | 0o644) << 16
            archive.writestr(info, data)
    return buffer.getvalue()


class _PublicNativeFixture:
    """Produces a real private stage under ``root`` through the canonical archive fixture.

    The archive fixture is created below ``release_parent`` (default: the system temporary
    directory); each ``containing`` tree also observes the fixture's verifier child failures.
    """

    def __init__(self, root: Path, *, failed_index: int | None = None,
                 release_parent: Path | None = None, containing: tuple[FixtureTree, ...] = ()) -> None:
        self.root = root
        self.failed_index = failed_index
        self.release_parent = release_parent
        self.containing = containing
        self.expectation: ProofExpectation | None = None
        self.release_root: Path | None = None

    def build_stage(self) -> Path:
        from scripts import verify_release_archive as verifier
        from scripts.test_verify_release_archive import ReleaseArchiveVerificationTests, REVISION, TAG

        release_fixture = ReleaseArchiveVerificationTests(
            parent=self.release_parent, containing=self.containing)
        with release_fixture.prepared():
            self.release_root = release_fixture.root
            if release_fixture.verify() != 0:
                raise AssertionError("canonical archive verifier did not pass")
            archive_receipt_path = release_fixture.receipt_path
            self._inject_private_canaries(archive_receipt_path)
            archive_receipt = _object(json.loads(archive_receipt_path.read_bytes()))
            binary_hashes = self._bind_actual_archive_binaries(
                release_fixture.extract, archive_receipt,
            )
            aggregate_path = self._build_aggregate(binary_hashes, TAG, REVISION)
            exit_file = self.root / "acceptance.exit"
            expected_exit = 0 if self.failed_index is None else 1
            exit_file.write_text(f"{expected_exit}\n", encoding="ascii")
            archive_bytes = archive_receipt_path.read_bytes()
            native_receipt_path = release_fixture.root / "native-acceptance-receipt.json"
            result = verifier.finalize_native(Namespace(
                receipt=str(native_receipt_path), archive_receipt=str(archive_receipt_path),
                aggregate_receipt=str(aggregate_path), assets_dir=str(release_fixture.assets),
                extract_dir=str(release_fixture.extract),
                acceptance_outcome="success" if expected_exit == 0 else "failure",
                exit_code_file=str(exit_file), tag=TAG, source_revision=REVISION,
            ))
            if result != expected_exit:
                raise AssertionError("canonical native finalizer changed the native verdict")
            if archive_receipt_path.read_bytes() != archive_bytes:
                raise AssertionError("native finalizer changed the retained archive receipt")
            if release_fixture.assets.exists() != (self.failed_index is not None):
                raise AssertionError("native finalizer changed the expected asset retention")
            native_receipt = _object(json.loads(native_receipt_path.read_bytes()))
            if native_receipt.get("archive_receipt_sha256") != sha256(archive_bytes):
                raise AssertionError("native finalizer did not bind the retained archive receipt")
            stage = self.root / "stage"
            evidence.stage_evidence(
                aggregate_path, stage, archive_receipt=archive_receipt_path,
                native_receipt=native_receipt_path, expected_tag=TAG,
                expected_source_revision=REVISION,
            )
            self.expectation = self._expectation_from_receipts(
                archive_receipt, native_receipt,
            )
            return stage

    def _inject_private_canaries(self, receipt_path: Path) -> None:
        receipt = _object(json.loads(receipt_path.read_bytes()))
        commands = receipt.get("commands")
        if not isinstance(commands, list):
            raise AssertionError("archive verifier command records missing")
        provider = next((row for row in commands if isinstance(row, dict)
                         and row.get("name") == "delegate-providers"), None)
        command = _object(provider)
        command.update({
            "argv": [str(self.root / "PUBLIC_HOST_PATH_CANARY" / "private-provider")],
            "env": {"PRIVATE_TOKEN": "PRIVATE_ENV_CANARY"},
            "stdout": "PRIVATE_PROVIDER_OUTPUT_CANARY",
        })
        receipt_path.write_text(json.dumps(receipt, sort_keys=True, separators=(",", ":")),
                                encoding="utf-8")

    def _bind_actual_archive_binaries(
            self, extracted: Path, archive_receipt: dict[str, object]) -> dict[str, str]:
        receipt_binaries = _object(archive_receipt.get("binaries"))
        mapping = {
            "delegate": "delegate_sha256", "delegate-run": "runner_sha256",
            "pueue": "pueue_sha256", "pueued": "pueued_sha256",
        }
        hashes: dict[str, str] = {}
        for archive_name, binding_name in mapping.items():
            extracted_hash = hashlib.sha256((extracted / archive_name).read_bytes()).hexdigest()
            recorded = _object(receipt_binaries.get(archive_name)).get("sha256")
            if recorded != extracted_hash:
                raise AssertionError("archive receipt binary hash differs from extracted binary")
            hashes[binding_name] = extracted_hash
        return hashes

    def _build_aggregate(self, binary_hashes: dict[str, str], tag: str, revision: str) -> Path:
        from scripts.test_native_acceptance_evidence import NativeAcceptanceEvidenceTests

        source_root = self.root / "aggregate"
        fixture = object.__new__(NativeAcceptanceEvidenceTests)
        matrix = fixture.make_matrix(
            source_root, status="PASS", failed_index=self.failed_index,
            requested_model="fixture-model",
        )
        for index, definition in enumerate(evidence.EXPECTED_CASES):
            if index == self.failed_index:
                continue
            binding_path = source_root / "cases" / definition.case_id / "binding.json"
            binding = _object(json.loads(binding_path.read_bytes()))
            binding.update(binary_hashes)
            binding_path.write_text(
                json.dumps(binding, sort_keys=True, separators=(",", ":")), encoding="utf-8",
            )
        aggregate_path = source_root / evidence.AGGREGATE_RECEIPT_NAME
        aggregate, code = evidence.build_aggregate(
            source_root, matrix, aggregate_path, tag=tag,
            source_revision=revision, require_pass=True,
        )
        expected = (0, "PASS") if self.failed_index is None else (1, "FAIL")
        if (code, aggregate.get("status")) != expected:
            raise AssertionError("native acceptance fixture changed the expected verdict")
        return aggregate_path

    def _expectation_from_receipts(
            self, archive: dict[str, object], native: dict[str, object]) -> ProofExpectation:
        release = _object(archive.get("release_identity"))
        platform = _object(archive.get("platform"))
        artifact = _object(archive.get("artifact"))
        binaries = _object(archive.get("binaries"))
        source = _object(native.get("source_harness"))

        def text_value(value: object) -> str:
            if not isinstance(value, str):
                raise AssertionError("release receipt text field missing")
            return value

        def binary_hash(name: str) -> str:
            return text_value(_object(binaries.get(name)).get("sha256"))

        return ProofExpectation(
            repo=text_value(release.get("repository")), tag=text_value(archive.get("tag")),
            release_revision=text_value(release.get("commit_sha")),
            harness_revision=text_value(archive.get("harness_revision")),
            platform_os=text_value(platform.get("os")), platform_arch=text_value(platform.get("arch")),
            opencode_model="fixture-model", ccr_model="fixture-model",
            source_harness_sha256=text_value(source.get("sha256")),
            archive_sha256=text_value(artifact.get("archive_sha256")),
            delegate_sha256=binary_hash("delegate"), runner_sha256=binary_hash("delegate-run"),
            pueue_sha256=binary_hash("pueue"), pueued_sha256=binary_hash("pueued"),
        )


class PublicNativeProofTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temporary = tempfile.TemporaryDirectory(prefix="PUBLIC_HOST_PATH_CANARY-")
        cls.root = Path(cls.temporary.name).resolve(strict=True)
        fixture = _PublicNativeFixture(cls.root)
        cls.stage = fixture.build_stage()
        if fixture.expectation is None:
            raise AssertionError("public proof expectation was not derived from producer receipts")
        cls.expectation = fixture.expectation
        cls.public_path = cls.root / "public-proof.zip"
        cls.private_snapshot = {
            path.relative_to(cls.stage).as_posix(): sha256(path.read_bytes())
            for path in cls.stage.rglob("*") if path.is_file()
        }
        result = build_public_proof(cls.stage, cls.public_path, cls.expectation)
        if result.get("status") != "PASS":
            raise AssertionError("composed public proof did not pass")
        cls.public_zip = cls.public_path.read_bytes()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temporary.cleanup()

    def _finalize_members(self, entries: dict[str, bytes]) -> bytes:
        manifest = json.loads(entries["manifest.json"])
        manifest_object = _object(manifest)
        manifest_object["projected_members"] = [
            {"path": name, "sha256": sha256(entries[name])}
            for name in PUBLIC_MEMBER_NAMES[1:]
        ]
        entries["manifest.json"] = canonical_json(manifest_object)
        return _pack_rows([(name, entries[name]) for name in PUBLIC_MEMBER_NAMES])

    def _change_case(self, name: str, change: Callable[[dict[str, object]], None]) -> bytes:
        entries = _read_members(self.public_zip)
        case = _object(json.loads(entries[name]))
        change(case)
        entries[name] = canonical_json(case)
        return self._finalize_members(entries)

    def _change_identity(self, change: Callable[[dict[str, object]], None]) -> bytes:
        entries = _read_members(self.public_zip)
        proof = _object(json.loads(entries["proof.json"]))
        identity = _object(proof["identity"])
        change(identity)
        entries["proof.json"] = canonical_json(proof)
        return self._finalize_members(entries)

    def test_real_stage_projects_and_validates_without_mutating_private_sources(self) -> None:
        envelope = inspect_public_proof(self.public_zip)
        self.assertEqual(canonical_public_zip(_read_members(self.public_zip)), self.public_zip)
        identity = validate_public_identity(self.public_zip, self.expectation)
        result = validate_public_proof(self.public_zip, self.expectation)
        self.assertEqual(envelope["status"], "ENVELOPE_VALID")
        self.assertIn("identity", envelope)
        self.assertEqual(identity["status"], "IDENTITY_VALID")
        self.assertEqual(result["status"], "PASS")
        self.assertEqual(result["case_results_verdict"], "PASS")
        self.assertEqual(result["expected_case_count"], len(CASE_DEFINITIONS))
        source_manifest_sha = sha256((self.stage / "manifest.json").read_bytes())
        public_zip_sha = sha256(self.public_zip)
        self.assertEqual(envelope["source_stage_manifest_sha256"], source_manifest_sha)
        self.assertEqual(identity["source_stage_manifest_sha256"], source_manifest_sha)
        self.assertEqual(result["source_stage_manifest_sha256"], source_manifest_sha)
        self.assertEqual(envelope["public_zip_sha256"], public_zip_sha)
        self.assertEqual(identity["public_zip_sha256"], public_zip_sha)
        self.assertEqual(result["public_zip_sha256"], public_zip_sha)
        self.assertNotEqual(source_manifest_sha, public_zip_sha)
        archive_receipt = _object(json.loads((self.stage / "archive-receipt.json").read_bytes()))
        native_receipt = _object(json.loads(
            (self.stage / "native-acceptance-receipt.json").read_bytes(),
        ))
        archive_cleanup = _object(archive_receipt.get("cleanup"))
        native_cleanup = _object(native_receipt.get("cleanup"))
        self.assertEqual(archive_cleanup.get("status"), "retained-for-native-acceptance")
        self.assertIsNone(archive_cleanup.get("transaction"))
        self.assertIsNone(archive_receipt.get("completion"))
        self.assertEqual(native_cleanup.get("status"), "complete")
        self.assertEqual(_object(native_receipt.get("completion")).get("state"), "complete")
        self.assertEqual(native_receipt.get("archive_receipt_sha256"),
                         sha256((self.stage / "archive-receipt.json").read_bytes()))
        artifact = _object(archive_receipt.get("artifact"))
        archive_path = artifact.get("archive_path")
        self.assertIsInstance(archive_path, str)
        assets_path = str(Path(archive_path).parent)
        archive_identity = _object(
            _object(archive_cleanup.get("directory_identities")).get(assets_path),
        )
        native_identity = _object(
            _object(native_cleanup.get("directory_identities")).get(assets_path),
        )
        self.assertEqual(archive_identity, native_identity)
        public_identity = _object(envelope["identity"])
        cleanup_identity = _object(public_identity.get("release_cleanup"))
        archive_public_cleanup = _object(cleanup_identity.get("archive"))
        native_public_cleanup = _object(cleanup_identity.get("native"))
        resource_id = sha256(canonical_json(archive_identity))
        self.assertEqual(archive_public_cleanup.get("resource_id"), resource_id)
        self.assertEqual(native_public_cleanup.get("resource_id"), resource_id)
        current = {
            path.relative_to(self.stage).as_posix(): sha256(path.read_bytes())
            for path in self.stage.rglob("*") if path.is_file()
        }
        self.assertEqual(current, self.private_snapshot)

    def test_projection_drops_private_host_paths_argv_environment_and_provider_output(self) -> None:
        archive_receipt = (self.stage / "archive-receipt.json").read_bytes()
        self.assertIn(b"PUBLIC_HOST_PATH_CANARY", archive_receipt)
        self.assertIn(b"PRIVATE_ENV_CANARY", archive_receipt)
        self.assertIn(b"PRIVATE_PROVIDER_OUTPUT_CANARY", archive_receipt)
        for canary in (
            b"PUBLIC_HOST_PATH_CANARY", b"/fixture/provider-", b'"argv"', b'"env"',
            b"PRIVATE_ENV_CANARY", b"PRIVATE_PROVIDER_OUTPUT_CANARY", b"checksums_path",
        ):
            self.assertNotIn(canary, self.public_zip)

    def test_real_failed_stage_preserves_verdict_and_retained_sources(self) -> None:
        root = self.root / "failed-stage"
        fixture = _PublicNativeFixture(root, failed_index=1)
        stage = fixture.build_stage()
        if fixture.expectation is None:
            raise AssertionError("failed fixture did not produce an expectation")
        snapshot = {path.relative_to(stage).as_posix(): sha256(path.read_bytes())
                    for path in stage.rglob("*") if path.is_file()}
        output = root / "public-failed.zip"
        build_public_proof(stage, output, fixture.expectation)
        data = output.read_bytes()
        self.assertEqual(inspect_public_proof(data)["status"], "ENVELOPE_VALID")
        result = validate_public_proof(data, fixture.expectation)
        self.assertEqual((result["status"], result["case_results_verdict"]), ("BLOCKED", "FAIL"))
        self.assertNotIn("blocking_reason", result)
        self.assertEqual((result["pass_count"], result["failed_count"],
                          result["unresolved_case_count"]), (len(CASE_DEFINITIONS) - 1, 1, 1))
        failed = _object(json.loads(_read_members(data)[f"cases/{CASE_DEFINITIONS[1].case_id}.json"]))
        cleanup = _object(failed["cleanup"])
        self.assertEqual(cleanup["terminal_state"], "published-unresolved")
        self.assertIs(cleanup["raw_deleted"], False)
        self.assertIsNone(cleanup["journal_sha256"])
        self.assertIsNone(cleanup["authorization_sha256"])
        raw = root / "aggregate" / CASE_DEFINITIONS[1].case_output / "raw-provider-output"
        self.assertTrue(raw.is_file())
        self.assertNotIn(raw.read_bytes(), data)
        self.assertEqual({path.relative_to(stage).as_posix(): sha256(path.read_bytes())
                          for path in stage.rglob("*") if path.is_file()}, snapshot)
        case_id = CASE_DEFINITIONS[1].case_id
        name = f"cases/{case_id}.json"
        for key, value in (("terminal_state", "published-after-cleanup"),
                           ("raw_deleted", True), ("journal_sha256", "0" * 64),
                           ("authorization_sha256", "0" * 64)):
            with self.subTest(key=key):
                entries = _read_members(data)
                changed = _object(json.loads(entries[name]))
                _object(changed["cleanup"])[key] = value
                entries[name] = canonical_json(changed)
                rejected = validate_public_proof(self._finalize_members(entries), fixture.expectation)
                self.assertEqual(rejected.get("blocking_reason"), "case_schema_or_predicate_invalid")
        entries = _read_members(data)
        manifest = _object(json.loads(entries["manifest.json"]))
        _object(_object(manifest["source_sha256"])["cleanup_journals"])[case_id] = "0" * 64
        entries["manifest.json"] = canonical_json(manifest)
        rejected = validate_public_proof(self._finalize_members(entries), fixture.expectation)
        self.assertEqual(rejected.get("blocking_reason"), "case_schema_or_predicate_invalid")
        staged = load_stage(stage)
        reference = _object(json.loads(staged.data["aggregate-receipt.json"])["cases"][1])
        source_path = f"case-receipts/{case_id}.json"
        for field, value in (("terminal_state", "unknown"), ("raw_deleted", True),
                             ("journal_path", ""), ("journal_sha256", ""),
                             ("authorization_sha256", "")):
            with self.subTest(source_field=field):
                changed = _object(json.loads(staged.data[source_path]))
                target = changed if field == "terminal_state" else _object(changed[
                    "cleanup" if field == "raw_deleted" else "raw_deletion"])
                target[field] = value
                encoded = canonical_json(changed)
                changed_snapshot = replace(staged, digests={**staged.digests, source_path: sha256(encoded)})
                with self.assertRaisesRegex(PublicProofError, "public_case_source_invalid"):
                    _project_case(CASE_DEFINITIONS[1], {**reference, "receipt_sha256": sha256(encoded)},
                                  encoded, changed_snapshot)

    def test_unresolved_or_unknown_cleanup_cannot_upgrade_resolved_evidence(self) -> None:
        for state in ("published-unresolved", "unknown", None):
            with self.subTest(state=state):
                modified = self._change_case(
                    f"cases/{CASE_DEFINITIONS[0].case_id}.json",
                    lambda case: _object(case["cleanup"]).__setitem__("terminal_state", state),
                )
                result = validate_public_proof(modified, self.expectation)
                self.assertEqual((result["status"], result["case_results_verdict"]),
                                 ("BLOCKED", "FAIL"))

    def test_outer_source_binding_mismatch_is_rejected_after_envelope_inspection(self) -> None:
        for key in ("source_harness", "archive_source_harness"):
            with self.subTest(key=key):
                def change(identity: dict[str, object]) -> None:
                    _object(identity[key])["sha256"] = "f" * 64

                modified = self._change_identity(change)
                self.assertEqual(inspect_public_proof(modified)["status"], "ENVELOPE_VALID")
                with self.assertRaisesRegex(PublicProofError, "public_identity_binding_mismatch"):
                    validate_public_identity(modified, self.expectation)

    def test_private_lifetime_closure_is_bound_by_digest_and_redacted_publicly(self) -> None:
        entries = _read_members(self.public_zip)
        for definition in CASE_DEFINITIONS:
            with self.subTest(case=definition.case_id):
                public_case = _object(json.loads(entries[f"cases/{definition.case_id}.json"]))
                public_lifecycle = _object(_object(public_case["source_proof"])["lifecycle_proof"])
                self.assertNotIn("closure", public_lifecycle)
                self.assertEqual(public_lifecycle["state"], "complete")
                compact_bytes = (self.stage / "case-receipts" / f"{definition.case_id}.json").read_bytes()
                self.assertEqual(public_case["source_compact_sha256"], sha256(compact_bytes))
                private_proof = _object(_object(json.loads(compact_bytes))["source_proof"])
                closure = _object(_object(private_proof["lifecycle_proof"])["closure"])
                self.assertEqual(set(_object(_object(closure["ordinary"])["tasks"])),
                                 set(private_proof["task_ids"]))

    def test_public_source_bindings_contain_only_the_trusted_digest(self) -> None:
        envelope = inspect_public_proof(self.public_zip)
        identity = _object(envelope["identity"])
        for key in ("source_harness", "archive_source_harness"):
            self.assertEqual(identity[key], {"sha256": self.expectation.source_harness_sha256})

    def test_private_source_manifest_is_bounded_unique_and_canonical(self) -> None:
        def fingerprint(rows: list[dict[str, str]]) -> dict[str, object]:
            encoded = json.dumps(rows, sort_keys=True, separators=(",", ":")).encode()
            return {"sha256": sha256(encoded), "files": rows}

        rows = [{"path": f"scripts/data/module-{index:03d}.py", "sha256": "a" * 64}
                for index in range(300)]
        value = fingerprint(rows)
        self.assertEqual(private_fingerprint(value)["files"], rows)
        for invalid in ([rows[0], rows[0]], list(reversed(rows[:2]))):
            with self.subTest(invalid=invalid):
                with self.assertRaisesRegex(PublicProofError, "public_fingerprint_order_invalid"):
                    private_fingerprint(fingerprint(invalid))

    def test_native_release_cleanup_resource_mismatch_blocks_after_admission(self) -> None:
        def change(identity: dict[str, object]) -> None:
            cleanup = _object(identity["release_cleanup"])
            native = _object(cleanup["native"])
            native["resource_id"] = "f" * 64

        modified = self._change_identity(change)
        self.assertEqual(validate_public_identity(modified, self.expectation)["status"],
                         "IDENTITY_VALID")
        result = validate_public_proof(modified, self.expectation)
        self.assertEqual((result["status"], result["case_results_verdict"]), ("BLOCKED", "FAIL"))

    def test_unfinished_native_release_cleanup_blocks_after_admission(self) -> None:
        def change(identity: dict[str, object]) -> None:
            cleanup = _object(identity["release_cleanup"])
            native = _object(cleanup["native"])
            native.update({"status": "pending", "completion_state": "prepared"})

        modified = self._change_identity(change)
        self.assertEqual(validate_public_identity(modified, self.expectation)["status"],
                         "IDENTITY_VALID")
        result = validate_public_proof(modified, self.expectation)
        self.assertEqual((result["status"], result["case_results_verdict"]), ("BLOCKED", "FAIL"))

    def test_bad_options_and_cleanup_are_admitted_blocking_case_failures(self) -> None:
        opencode = next(case for case in CASE_DEFINITIONS if case.provider == "opencode:run")

        def bad_options(case: dict[str, object]) -> None:
            _object(case["source_proof"])["requested_model"] = "wrong-model"

        bad_option_zip = self._change_case(f"cases/{opencode.case_id}.json", bad_options)
        validate_public_identity(bad_option_zip, self.expectation)
        option_result = validate_public_proof(bad_option_zip, self.expectation)
        self.assertEqual((option_result["status"], option_result["case_results_verdict"]),
                         ("BLOCKED", "FAIL"))

        first = CASE_DEFINITIONS[0]
        bad_cleanup_zip = self._change_case(
            f"cases/{first.case_id}.json",
            lambda case: _object(case["cleanup"]).__setitem__("journal_sha256", "0" * 64),
        )
        validate_public_identity(bad_cleanup_zip, self.expectation)
        cleanup_result = validate_public_proof(bad_cleanup_zip, self.expectation)
        self.assertEqual((cleanup_result["status"], cleanup_result["case_results_verdict"]),
                         ("BLOCKED", "FAIL"))

    def test_outer_requested_models_must_match_before_admission(self) -> None:
        for field in ("opencode_model", "ccr_model"):
            with self.subTest(field=field):
                expectation = replace(self.expectation, **{field: "wrong-model"})
                with self.assertRaisesRegex(PublicProofError, "public_identity_binding_mismatch"):
                    validate_public_identity(self.public_zip, expectation)

    def test_missing_ccr_alias_is_neutral_when_all_ccr_cases_are_blocked(self) -> None:
        entries = _read_members(self.public_zip)
        blocked_count = 0
        for definition in CASE_DEFINITIONS:
            if definition.provider != "ccr:print":
                continue
            blocked_count += 1
            case = _object(json.loads(entries[f"cases/{definition.case_id}.json"]))
            case["status"], case["exit_code"] = "BLOCKED", 2
            proof = _object(case["source_proof"])
            proof.update({
                "state": "blocked-before-runtime", "final_status": "BLOCKED",
                "source_status": "blocked", "requested_model": None,
                "task_ids": [], "task_session_bindings": [], "diagnostic_task_ids": [],
                "session_ids": {"state": "not-recorded", "values": [], "by_alias": {}},
                "binary_identities": {"state": "unavailable", "sha256": {}, "task_ids": []},
            })
            _object(proof["lifecycle_proof"])["state"] = "unresolved"
            entries[f"cases/{definition.case_id}.json"] = canonical_json(case)
        self.assertGreater(blocked_count, 0)
        proof_value = _object(json.loads(entries["proof.json"]))
        _object(proof_value["identity"])["ccr_model"] = None
        aggregate = _object(proof_value["aggregate"])
        aggregate.update({"pass_count": len(CASE_DEFINITIONS) - blocked_count,
                          "blocked_count": blocked_count})
        entries["proof.json"] = canonical_json(proof_value)
        expectation = replace(self.expectation, ccr_model=None)
        result = validate_public_proof(self._finalize_members(entries), expectation)
        self.assertEqual(result["status"], "PASS")
        self.assertEqual(result["case_results_verdict"], "PASS")
        self.assertEqual(result["blocked_count"], blocked_count)

    def test_submitted_fail_is_blocking_and_reports_fail_verdict(self) -> None:
        case_name = f"cases/{CASE_DEFINITIONS[0].case_id}.json"

        def submitted_fail(case: dict[str, object]) -> None:
            case["status"], case["exit_code"] = "FAIL", 1
            proof = _object(case["source_proof"])
            proof.update({"state": "unresolved", "final_status": "FAIL", "source_status": "fail"})

        entries = _read_members(self.public_zip)
        failed_case = _object(json.loads(entries[case_name]))
        submitted_fail(failed_case)
        entries[case_name] = canonical_json(failed_case)
        proof_value = _object(json.loads(entries["proof.json"]))
        aggregate = _object(proof_value["aggregate"])
        aggregate.update({"status": "FAIL", "exit_code": 1, "case_results_verdict": "FAIL",
                          "pass_count": len(CASE_DEFINITIONS) - 1, "failed_count": 1})
        entries["proof.json"] = canonical_json(proof_value)
        result = validate_public_proof(self._finalize_members(entries), self.expectation)
        self.assertEqual((result["status"], result["case_results_verdict"]), ("BLOCKED", "FAIL"))
        self.assertEqual(result.get("blocking_reason"), "case_schema_or_predicate_invalid")

    def test_zip_rejects_extra_duplicate_traversal_digest_and_size_violations(self) -> None:
        rows = [(name, data) for name, data in _read_members(self.public_zip).items()]
        with self.assertRaises(PublicProofError):
            inspect_public_proof(_pack_unchecked_rows([*rows, ("extra.json", b"{}")]))
        with self.assertWarns(UserWarning):
            duplicate = _pack_unchecked_rows([*rows, ("manifest.json", rows[0][1])])
        with self.assertRaises(PublicProofError):
            inspect_public_proof(duplicate)
        traversed = [("../outside", data) if name == PUBLIC_MEMBER_NAMES[-1] else (name, data)
                     for name, data in rows]
        with self.assertRaises(PublicProofError):
            inspect_public_proof(_pack_unchecked_rows(traversed))
        entries = _read_members(self.public_zip)
        manifest = _object(json.loads(entries["manifest.json"]))
        _object(manifest["projected_members"][-1])["sha256"] = "0" * 64
        entries["manifest.json"] = canonical_json(manifest)
        with self.assertRaisesRegex(PublicProofError, "public_projected_digest_mismatch"):
            inspect_public_proof(_pack_rows([(name, entries[name]) for name in PUBLIC_MEMBER_NAMES]))
        with self.assertRaises(PublicProofError):
            inspect_public_proof(b"x" * (MAX_PUBLIC_ZIP_BYTES + 1))
        oversized = _read_members(self.public_zip)
        oversized[PUBLIC_MEMBER_NAMES[-1]] = b"x" * (MAX_PUBLIC_MEMBER_BYTES + 1)
        manifest = _object(json.loads(oversized["manifest.json"]))
        _object(manifest["projected_members"][-1])["sha256"] = sha256(
            oversized[PUBLIC_MEMBER_NAMES[-1]],
        )
        oversized["manifest.json"] = canonical_json(manifest)
        with self.assertRaisesRegex(PublicProofError, "public_zip_member_invalid"):
            inspect_public_proof(_pack_unchecked_rows([
                (name, oversized[name]) for name in PUBLIC_MEMBER_NAMES
            ]))

    def test_zip_rejects_private_padding_before_central_directory(self) -> None:
        padding = b"private-provider-path-canary"
        data = self.public_zip
        eocd = data.rfind(b"PK\x05\x06")
        self.assertGreaterEqual(eocd, 0)
        central_offset = struct.unpack_from("<I", data, eocd + 16)[0]
        padded = bytearray(data[:central_offset] + padding + data[central_offset:])
        shifted_eocd = eocd + len(padding)
        struct.pack_into("<I", padded, shifted_eocd + 16, central_offset + len(padding))
        padded_bytes = bytes(padded)
        with zipfile.ZipFile(BytesIO(padded_bytes), "r") as archive:
            self.assertEqual(tuple(row.filename for row in archive.infolist()), PUBLIC_MEMBER_NAMES)
        with self.assertRaisesRegex(PublicProofError, "public_zip_noncanonical"):
            inspect_public_proof(padded_bytes)

    def test_boolean_is_not_a_schema_version(self) -> None:
        for name in ("manifest.json", "proof.json"):
            with self.subTest(name=name):
                members = _read_members(self.public_zip)
                value = _object(json.loads(members[name]))
                value["schema_version"] = True
                members[name] = canonical_json(value)
                with self.assertRaises(PublicProofError):
                    inspect_public_proof(self._finalize_members(members))

    def test_stage_rejects_unexpected_files_and_oversized_sources(self) -> None:
        unexpected = self.root / "stage-with-extra"
        shutil.copytree(self.stage, unexpected)
        (unexpected / "unexpected.json").write_text("{}", encoding="utf-8")
        with self.assertRaisesRegex(PublicProofError, "public_stage_unexpected_file"):
            build_public_proof(unexpected, self.root / "unexpected-output.zip", self.expectation)
        oversized = self.root / "stage-oversized"
        shutil.copytree(self.stage, oversized)
        (oversized / "retention" / "plan.json").write_bytes(
            b"x" * (1024 * 1024 + 1),
        )
        with self.assertRaisesRegex(PublicProofError, "public_stage_file_too_large"):
            build_public_proof(oversized, self.root / "oversized-output.zip", self.expectation)

    def test_stage_inventory_stops_at_bounded_entry_count(self) -> None:
        crowded = self.root / "stage-crowded"
        shutil.copytree(self.stage, crowded)
        for index in range(256):
            (crowded / f"extra-{index:03}.json").write_bytes(b"{}")
        with self.assertRaisesRegex(PublicProofError, "public_stage_entry_limit"):
            build_public_proof(crowded, self.root / "crowded-output.zip", self.expectation)

    def test_stage_reader_rejects_replacement_after_file_identity_check(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve(strict=True)
            target = root / "receipt.json"
            replacement = root / "replacement.json"
            target.write_bytes(b"original")
            replacement.write_bytes(b"replacement")
            original_open = os.open
            replaced = False

            def replace_before_open(path: str | os.PathLike[str], flags: int) -> int:
                nonlocal replaced
                if Path(path) == target and not replaced:
                    os.replace(replacement, target)
                    replaced = True
                return original_open(path, flags)

            with patch("scripts.delegate_acceptance.evidence.bounded_files.os.open",
                       side_effect=replace_before_open):
                with self.assertRaisesRegex(PublicProofError, "public_stage_file_changed"):
                    _read_stage_file(root, "receipt.json", 64)
            self.assertTrue(replaced)

    def test_stage_reader_rejects_growth_after_bounded_read_starts(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve(strict=True)
            target = root / "receipt.json"
            target.write_bytes(b"four")
            original_read = os.read
            grew = False

            def grow_after_read(descriptor: int, size: int) -> bytes:
                nonlocal grew
                data = original_read(descriptor, size)
                if data and not grew:
                    with target.open("ab") as stream:
                        stream.write(b"!")
                    grew = True
                return data

            with patch("scripts.delegate_acceptance.evidence.bounded_files.os.read",
                       side_effect=grow_after_read):
                with self.assertRaisesRegex(PublicProofError, "public_stage_file_too_large"):
                    _read_stage_file(root, "receipt.json", 4)
            self.assertTrue(grew)


class PublicNativeFixtureOwnershipTests(unittest.TestCase):
    def test_failed_release_setup_still_disposes_the_release_fixture(self) -> None:
        from scripts import verify_release_archive as verifier

        before = open_descriptors()
        tree = FixtureTree.open(prefix="public-native-fixture", label="public native fixture")
        self.addCleanup(tree.dispose)
        producer = _PublicNativeFixture(tree.root, release_parent=tree.root, containing=(tree,))
        fault = RuntimeError("release fixture setUp fault")
        with patch.object(verifier, "expected_archive", side_effect=fault), \
                self.assertRaises(RuntimeError) as raised:
            producer.build_stage()
        self.assertIs(raised.exception, fault)
        self.assertEqual(list(tree.root.iterdir()), [], "a failed setUp must still dispose the release fixture")
        self.assertIsNone(tree.unresolved)
        self.assertTrue(tree.dispose())
        self.assertEqual(open_descriptors(), before)

