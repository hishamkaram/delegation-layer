"""The read-only native closure observation of one retained case, on owned temporary records.

Each test owns a temporary invocation tree shaped like a retained failed
case: its bound private records, its retained owned directories with their
real identities, and finished process captures. No provider, queue or signal
is involved; the real C2 retained state is Root's native proof.
"""

from __future__ import annotations

from contextlib import redirect_stdout
import hashlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.native_evidence.lifetime_closure import LIFETIME_CLOSURE_KIND
from scripts.delegate_acceptance.native_evidence.public_contract import CaseClosureAnchor
from scripts.delegate_acceptance.native_release import closure_observation
from scripts.delegate_acceptance.native_release.closure_observation import observe_case_closure
from scripts.delegate_acceptance.native_release.contract import ReleaseError


CASE = "case-07-codex-exec-workspace-write-timeout-resume"
LABELS = ("raw process captures", "model discovery state", "acceptance state")


def write(path: Path, value: object) -> bytes:
    path.parent.mkdir(parents=True, exist_ok=True)
    data = json.dumps(value, sort_keys=True).encode()
    path.write_bytes(data)
    return data


def identity(path: Path) -> dict[str, object]:
    current, parent = path.lstat(), path.parent.lstat()
    return {"path": str(path), "parent": str(path.parent), "device": current.st_dev, "inode": current.st_ino,
            "parent_device": parent.st_dev, "parent_inode": parent.st_ino}


class CaseObservationTests(unittest.TestCase):
    def setUp(self) -> None:
        owned = tempfile.TemporaryDirectory()
        self.addCleanup(owned.cleanup)
        self.root = Path(owned.name).resolve()
        self.native = self.root / "native"
        self.case = self.native / "cases" / CASE
        self.captures = self.case / "processes"
        self.state, self.models = self.root / "live" / "state", self.root / "live" / "models-state"
        for directory in (self.state, self.models):
            directory.mkdir(parents=True)
        self.capture("001-pueued")
        self.diagnostics = {"failure": {"discover_models": False}, "tasks": [], "task_ids": []}

    def capture(self, name: str, *, natural: bool = True) -> None:
        directory = self.captures / name
        write(directory / "invocation.json", {"argv": ["x"], "cwd": "/"})
        write(directory / "started.json", {"pid": 7, "started_ns": 1})
        write(directory / "completed.json", {"pid": 7, "ended_ns": 2, "exit_code": 0, "natural_wait": natural})

    def anchor(self, **changes: object) -> CaseClosureAnchor:
        """Write the bound records and return the anchor whose digests bind them."""
        digests = {}
        records = {"source_compact_sha256": (self.native / "receipts" / f"{CASE}.json", {"compact": True}),
                   "source_receipt_sha256": (self.case / "failure.json", {"status": "failed"}),
                   "failure_diagnostics_sha256": (self.case / "failure-diagnostics.json", self.diagnostics)}
        for field, (path, value) in records.items():
            digests[field] = hashlib.sha256(write(path, value)).hexdigest()
        directories = {"raw process captures": self.captures, "model discovery state": self.models,
                       "acceptance state": self.state}
        # The actual failed-run shape the acceptance finalizer writes: its plan
        # finished without failures and explicitly retained every failed source.
        cleanup = {"status": "removed", "completion": "failed", "supervisor_shutdown_observed": True,
                   "raw_process_captures": "retained", "removed": [], "retained": list(LABELS),
                   "retained_owned_directories": [identity(directories[label]) for label in LABELS]}
        cleanup.update(changes.pop("cleanup", {}))
        digests["cleanup_receipt_sha256"] = hashlib.sha256(write(self.case / "cleanup.json", cleanup)).hexdigest()
        values = {"case_id": CASE, "task_ids": (), "task_session_bindings": (), "diagnostic_task_ids": (),
                  **digests, **changes}
        return CaseClosureAnchor(**values)

    def assertRefused(self, code: str, anchor: CaseClosureAnchor) -> None:
        with self.assertRaises(ReleaseError) as caught:
            observe_case_closure(self.native, anchor)
        self.assertEqual(caught.exception.code, code)

    def test_a_no_launch_case_observes_its_retained_closure(self):
        case, identities, captures = observe_case_closure(self.native, self.anchor())
        self.assertEqual((case.ordinary_tasks, case.discovery_inspection_id), ((), None))
        self.assertEqual(case.lifetime_closure, {
            "schema_version": 1, "kind": LIFETIME_CLOSURE_KIND, "model_discovery": None,
            "ordinary": {"root_id": None, "launch": "none", "tasks": {}, "inspections": {}}})
        self.assertEqual(len(identities), 3)
        self.assertEqual([item["name"] for item in captures], ["001-pueued"])

    def test_only_a_failed_run_finalized_after_observed_shutdown_retains_resources(self):
        for label, change in {"shutdown unobserved": {"status": "retained", "supervisor_shutdown_observed": False},
                              "partial plan": {"status": "partial", "failures": ["acceptance state"]},
                              "passed run": {"completion": "passed"},
                              "captures removed": {"raw_process_captures": "removed"}}.items():
            with self.subTest(label):
                self.assertRefused("closure_retention_invalid", self.anchor(cleanup=change))

    def test_a_changed_or_unbound_private_record_refuses(self):
        anchor = self.anchor()
        write(self.case / "failure-diagnostics.json", {"changed": True})
        self.assertRefused("closure_record_binding_mismatch", anchor)
        self.assertRefused("closure_record_binding_mismatch", self.anchor(cleanup_receipt_sha256=None))

    def test_a_changed_owned_directory_identity_refuses(self):
        anchor = self.anchor()
        # The original directory stays alive elsewhere, so its replacement has another identity.
        self.models.rename(self.root / "moved-models-state")
        self.models.mkdir()
        self.assertRefused("release_directory_identity_changed", anchor)

    def test_a_negative_wait_or_ambiguous_capture_refuses(self):
        self.capture("002-provider", natural=False)
        self.assertRefused("child_natural_completion_unproven", self.anchor())
        self.capture("002-provider")
        (self.captures / "stray").write_bytes(b"")
        self.assertRefused("closure_capture_ambiguous", self.anchor())

    def test_model_discovery_admission_must_be_classified(self):
        self.diagnostics["failure"]["discover_models"] = True
        self.assertRefused("closure_discovery_unresolved", self.anchor())
        self.diagnostics["failure"]["discover_models"] = False
        (self.models / "inspections").mkdir()
        self.assertRefused("closure_discovery_unresolved", self.anchor())
        self.diagnostics["model_discovery_inspection"] = {"journal_task_id": "0" * 32}
        self.assertRefused("closure_discovery_admission_invalid", self.anchor())


class OutputPlacementTests(unittest.TestCase):
    """The command writes only to a clean path outside the invocation root; it never writes after refusing."""

    def setUp(self) -> None:
        owned = tempfile.TemporaryDirectory()
        self.addCleanup(owned.cleanup)
        self.base = Path(owned.name).resolve()
        self.invocation = self.base / "invocation"
        self.invocation.mkdir()
        self.receipt = self.invocation / "receipt.json"
        self.external = self.base / "external"
        self.external.mkdir()

    def run_main(self, output: Path) -> tuple[int, dict[str, object], mock.MagicMock, mock.MagicMock]:
        stdout = io.StringIO()
        with (mock.patch.object(closure_observation, "observe_proof_closure", return_value=b"{}") as observe,
              mock.patch.object(closure_observation, "write_bytes") as writer, redirect_stdout(stdout)):
            code = closure_observation.main(["--receipt", str(self.receipt), "--proof-sha256", "0" * 64,
                                             "--output", str(output)])
        return code, json.loads(stdout.getvalue()), observe, writer

    def assertRefusedBeforeWriting(self, output: Path, reason: str) -> None:
        code, result, observe, writer = self.run_main(output)
        self.assertEqual((code, result), (1, {"status": "refused", "code": reason}))
        observe.assert_not_called()
        writer.assert_not_called()

    def test_an_external_clean_output_is_written_once(self):
        output = self.external / "supplement.json"
        code, result, observe, writer = self.run_main(output)
        self.assertEqual((code, result["status"]), (0, "written"))
        observe.assert_called_once_with(self.receipt, "0" * 64)
        writer.assert_called_once_with(output, b"{}")

    def test_a_traversal_into_the_invocation_root_is_refused(self):
        self.assertRefusedBeforeWriting(self.external / ".." / "invocation" / "supplement.json",
                                        "release_path_not_absolute_or_clean")

    def test_a_symlinked_parent_into_the_invocation_root_is_refused(self):
        alias = self.base / "alias"
        alias.symlink_to(self.invocation, target_is_directory=True)
        self.assertRefusedBeforeWriting(alias / "supplement.json", "release_path_symlink")

    def test_an_output_inside_the_invocation_root_or_existing_is_refused(self):
        self.assertRefusedBeforeWriting(self.invocation / "supplement.json", "closure_output_unsafe")
        existing = self.external / "existing.json"
        existing.write_bytes(b"")
        self.assertRefusedBeforeWriting(existing, "closure_output_unsafe")


if __name__ == "__main__":
    unittest.main()
