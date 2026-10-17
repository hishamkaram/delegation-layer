import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
import sys

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import native_lifetime_proof as proof
from scripts.native_lifetime_proof import (
    MAX_RECEIPT_BYTES,
    native_go_architecture,
    read_receipt,
    validate_receipt,
)


def expected_source() -> tuple[str, str, str]:
    return ("3" * 40, "4" * 64, "5" * 64)


def fixture_receipt(host_os: str, backend: str, assurance_name: str) -> dict[str, object]:
    primitive_name = {
        ("linux", "cgroup-v2"): "cgroup-v2 cgroup.kill with setsid descendant",
        ("linux", "process-group"): "Linux private session with owner-bound process termination",
        ("darwin", "process-group"): "Darwin private session with process identity-checked termination",
    }[(host_os, backend)]
    observation = (
        "setsid descendant terminated; command Wait consumed once; scope empty and destroyed"
        if backend == "cgroup-v2"
        else "prepared child terminated; command Wait consumed once; scope empty and destroyed"
    )
    primitive: dict[str, object] = {
        "schema_version": 1,
        "host_os": host_os,
        "host_kernel": "Linux 6.8.0" if host_os == "linux" else "Darwin 25.0.0",
        "architecture": "amd64" if host_os == "linux" else "arm64",
        "backend": backend,
        "assurance": assurance_name,
        "test": "TestNativeBackendProof",
        "primitive": primitive_name,
        "observed": observation,
        "plan_sha256": "1" * 64,
        "scope_identity_sha256": "2" * 64,
        "source": {
            "revision": "3" * 40,
            "package_sha256": "4" * 64,
            "gate_sha256": "5" * 64,
        },
        "binary": {
            "name": "lifetime.test",
            "sha256": "6" * 64,
            "size_bytes": 1024,
            "go_version": "go1.27.2",
        },
        "verdict": "passed",
    }
    primitive_bytes = json.dumps(primitive, separators=(",", ":"), ensure_ascii=True).encode("ascii")
    limits = [
        "fixture_only_no_live_provider_acceptance",
        "application_lifetime_owner_lease_not_exercised",
        "cooperative_scope_owner_loss_remains_unverifiable"
        if backend == "process-group"
        else "cgroup_proof_limited_to_recorded_kernel_and_exercised_primitive",
    ]
    return {
        "schema_version": 1,
        "claim_class": "native-lifetime",
        "primitive_receipt": primitive,
        "assurance_receipt": {
            "schema_version": 1,
            "primitive_receipt_sha256": hashlib.sha256(primitive_bytes).hexdigest(),
            "assurance": assurance_name,
            "owner": "native proof test process",
            "lease": "test invocation owns the created scope until cleanup",
            "proof": "production Prepare selected the backend; CreatePrepared created it; the observed child was waited and the scope was proven empty before destruction",
            "unresolved_limits": limits,
        },
        "last_stage": "scope-released",
        "cleanup_status": "released",
        "retained_evidence": [],
        "unresolved_evidence": [],
        "failure": {"code": "none", "stage": "", "message": ""},
        "verdict": "passed",
    }


class NativeLifetimeProofTests(unittest.TestCase):
    def test_json_containers_in_primitive_verdict_return_validation_errors(self) -> None:
        for verdict in ([], {}):
            with self.subTest(verdict=verdict):
                receipt = fixture_receipt("linux", "process-group", "cooperative")
                receipt["primitive_receipt"]["verdict"] = verdict
                errors = validate_receipt(receipt, "linux", "amd64", expected_source())
                self.assertIn("primitive proof verdict is invalid", errors)

    def test_json_containers_in_failure_code_return_validation_errors(self) -> None:
        for code in ([], {}):
            with self.subTest(code=code):
                receipt = fixture_receipt("linux", "process-group", "cooperative")
                receipt["verdict"] = "failed"
                receipt["failure"] = {"code": code, "stage": "scope-released", "message": "failure"}
                errors = validate_receipt(receipt, "linux", "amd64", expected_source())
                self.assertIn("receipt failure details are invalid", errors)

    def test_json_containers_in_cleanup_status_return_validation_errors(self) -> None:
        for status in ([], {}):
            with self.subTest(status=status):
                receipt = fixture_receipt("linux", "process-group", "cooperative")
                receipt["cleanup_status"] = status
                errors = validate_receipt(receipt, "linux", "amd64", expected_source())
                self.assertIn("receipt cleanup status is missing or unresolved", errors)

    def test_source_fingerprint_includes_extracted_contract_and_native_files(self) -> None:
        with tempfile.TemporaryDirectory(prefix="native-fingerprint-") as temporary:
            root = Path(temporary)
            sources = (
                "internal/lifetime/first.go", "internal/lifetime/process_marker_darwin.go",
                "scripts/native_lifetime_proof.py", "scripts/test_native_lifetime_proof.py",
                "scripts/delegate_acceptance/native_lifetime/__init__.py",
                "scripts/delegate_acceptance/native_lifetime/contract.py",
            )
            for name in sources:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(name, encoding="utf-8")
            with patch.object(proof.subprocess, "run", return_value=SimpleNamespace(stdout="a" * 40)):
                first = proof.source_fingerprint(root)
                contract = root / sources[-1]
                contract.write_text("changed pure receipt validation", encoding="utf-8")
                changed_gate = proof.source_fingerprint(root)
                self.assertEqual(first[:2], changed_gate[:2])
                self.assertNotEqual(first[2], changed_gate[2])
                (root / sources[1]).write_text("changed Darwin backend", encoding="utf-8")
                changed_package = proof.source_fingerprint(root)
                self.assertEqual(changed_gate[0], changed_package[0])
                self.assertNotEqual(changed_gate[1], changed_package[1])
                self.assertEqual(changed_gate[2], changed_package[2])

    def test_maps_uname_architecture_to_go_architecture(self) -> None:
        self.assertEqual(native_go_architecture("x86_64"), "amd64")
        self.assertEqual(native_go_architecture("aarch64"), "arm64")
        self.assertEqual(native_go_architecture("arm64"), "arm64")

    def test_accepts_each_native_backend_row(self) -> None:
        cases = (
            ("linux", "amd64", "cgroup-v2", "exact"),
            ("linux", "amd64", "process-group", "cooperative"),
            ("darwin", "arm64", "process-group", "cooperative"),
        )
        for host_os, arch, backend, assurance_name in cases:
            with self.subTest(host_os=host_os, backend=backend):
                receipt = fixture_receipt(host_os, backend, assurance_name)
                self.assertEqual(validate_receipt(receipt, host_os, arch, expected_source()), [])

    def test_rejects_inconclusive_or_missing_receipt_fields(self) -> None:
        receipt = fixture_receipt("linux", "process-group", "cooperative")
        receipt["verdict"] = "inconclusive"
        del receipt["assurance_receipt"]
        errors = validate_receipt(receipt, "linux", "amd64", expected_source())
        self.assertIn("native lifetime proof verdict is not passed", errors)
        self.assertIn("primitive and assurance receipts are required", errors)

    def test_rejects_backend_that_disagrees_with_native_matrix(self) -> None:
        receipt = fixture_receipt("darwin", "process-group", "cooperative")
        primitive = receipt["primitive_receipt"]
        self.assertIsInstance(primitive, dict)
        primitive["backend"] = "cgroup-v2"
        errors = validate_receipt(receipt, "darwin", "arm64", expected_source())
        self.assertIn("receipt backend and assurance do not match the native backend matrix", errors)

    def test_rejects_changed_primitive_without_assurance_rebinding(self) -> None:
        receipt = fixture_receipt("linux", "cgroup-v2", "exact")
        primitive = receipt["primitive_receipt"]
        self.assertIsInstance(primitive, dict)
        primitive["observed"] = "different observation"
        errors = validate_receipt(receipt, "linux", "amd64", expected_source())
        self.assertIn("assurance receipt does not bind the primitive receipt", errors)

    def test_rejects_different_host_architecture(self) -> None:
        errors = validate_receipt(
            fixture_receipt("linux", "process-group", "cooperative"), "linux", "arm64", expected_source()
        )
        self.assertIn("receipt host does not match the native runner", errors)

    def test_binds_each_prelaunch_source_identity_field(self) -> None:
        field_errors = {
            "revision": "receipt source revision does not match the pre-launch fingerprint",
            "package_sha256": "receipt source package hash does not match the pre-launch fingerprint",
            "gate_sha256": "receipt source gate hash does not match the pre-launch fingerprint",
        }
        for field, expected_error in field_errors.items():
            with self.subTest(field=field):
                receipt = fixture_receipt("linux", "process-group", "cooperative")
                source = receipt["primitive_receipt"]["source"]
                self.assertIsInstance(source, dict)
                source[field] = "f" * (40 if field == "revision" else 64)
                errors = validate_receipt(receipt, "linux", "amd64", expected_source())
                self.assertIn(expected_error, errors)

    def test_success_binds_cleanup_and_resource_evidence(self) -> None:
        receipt = fixture_receipt("linux", "process-group", "cooperative")
        receipt["cleanup_status"] = "scope-retained"
        receipt["retained_evidence"] = ["native-scope:scope_identity_sha256=2" * 32]
        receipt["unresolved_evidence"] = ["destroy-empty-failed"]
        errors = validate_receipt(receipt, "linux", "amd64", expected_source())
        self.assertIn("passed receipt cleanup is not released", errors)
        self.assertIn("passed receipt retains unresolved resource evidence", errors)

    def test_failed_receipt_requires_structured_failure_shape(self) -> None:
        receipt = fixture_receipt("linux", "process-group", "cooperative")
        receipt["verdict"] = "failed"
        primitive = receipt["primitive_receipt"]
        self.assertIsInstance(primitive, dict)
        primitive["verdict"] = "failed"
        receipt["assurance_receipt"]["primitive_receipt_sha256"] = hashlib.sha256(
            json.dumps(primitive, separators=(",", ":"), ensure_ascii=True).encode("ascii")
        ).hexdigest()
        receipt["cleanup_status"] = "scope-retained"
        receipt["retained_evidence"] = ["native-scope:scope_identity_sha256=" + "2" * 64]
        receipt["unresolved_evidence"] = ["destroy-empty-failed"]
        receipt["failure"] = {"code": "destroy-empty-failed", "stage": "scope-released"}
        errors = validate_receipt(receipt, "linux", "amd64", expected_source())
        self.assertIn("receipt failure details are incomplete or unexpected", errors)

    def test_receipt_reader_rejects_missing_and_oversized_files(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "receipt.json"
            value, errors = read_receipt(path)
            self.assertIsNone(value)
            self.assertTrue(errors)
            path.write_bytes(b"x" * (MAX_RECEIPT_BYTES + 1))
            value, errors = read_receipt(path)
            self.assertIsNone(value)
            self.assertTrue(errors)

    def test_receipt_reader_rejects_duplicate_json_fields(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "receipt.json"
            path.write_text('{"verdict":"passed","verdict":"failed"}', encoding="utf-8")
            value, errors = read_receipt(path)
            self.assertIsNone(value)
            self.assertTrue(errors)


if __name__ == "__main__":
    unittest.main()
