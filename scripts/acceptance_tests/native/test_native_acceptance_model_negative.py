"""A negative model catalog takes the entry's failure path, never the success shutdown."""

import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts import acceptance_native as entry
from scripts.acceptance_tests.native.support import fixture_list, fixture_record, fixture_text
from scripts.delegate_acceptance.native import acceptance as gate


# Root's live `delegate models --json` responses for an invalid `--cwd`; both exit 2.
INVALID_CWD = '{"schema_version":1,"command":"models","provider":"ccr:print","observed_at":"2026-10-19T23:07:55.582689477Z","status":"unavailable","reason_code":"discovery_unavailable","complete":false,"source":"none","models":[],"efforts":null,"failure":{"code":"invalid_request","stage":"validate-request","next_action":"correct_request"},"error":"Delegate could not accept the dispatch request."}'
NONDIRECTORY_CWD = '{"schema_version":1,"command":"models","provider":"ccr:print","observed_at":"2026-10-19T23:09:03.661814061Z","status":"failed","reason_code":"discovery_failed","complete":false,"source":"none","models":[],"efforts":null,"failure":{"code":"invalid_request","stage":"validate-request","next_action":"correct_request"},"error":"Delegate could not accept the dispatch request."}'
CLI_EXIT = 2
JOURNAL, ROOT = "4" * 32, "5a" * 16
CANARY = "CANARY-RAW-INSPECTION-TEXT"
# A typed inspection failure with Go's fixed projection (internal/app/dispatch_failure.go).
INSPECTION_FAILURE = json.dumps({
    "schema_version": 1, "command": "models", "provider": "ccr:print",
    "observed_at": "2026-10-19T23:09:03.661814061Z", "status": "failed", "reason_code": "discovery_failed",
    "complete": False, "source": "none", "models": [], "efforts": None,
    "failure": {"code": "provider_preflight_failed", "stage": "inspect-provider", "next_action": "stop_and_report",
                "inspection": {"class": "inspection_failed", "deadline_state": "unknown",
                               "journal_task_id": JOURNAL}},
    "error": "Delegate could not inspect the provider " + CANARY,
})
INSPECTION_RECORDS = {
    "request.json": json.dumps({"brief": CANARY, "task": {"provider": "ccr:print"}}),
    "submission.json": json.dumps({"numeric_task_id": 3}),
    "result.json": json.dumps({"reason": "failed", "facts": {"error": CANARY}}),
}
SANITIZED_KEYS = {"status", "reason_code", "complete", "model_count", "effort_count"}


class RecordingAcceptance(gate.NativeAcceptance):
    """The real native owner, recorded so a test can inspect its owned resources."""

    instances: list["RecordingAcceptance"] = []

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        RecordingAcceptance.instances.append(self)


class NegativeModelCatalogTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-model-negative-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.output = self.root / "evidence"
        self.output.mkdir()
        self.response = self.root / "response.json"
        self.calls = self.root / "calls"
        self.exit_file = self.root / "exit"
        # Records the finite CLI writes under its --root, as Delegate's state root would hold.
        self.state_fixture = self.root / "state-fixture"
        bundle = self.root / "bundle"
        bundle.mkdir()
        # Finite delegate CLI fixture: one recorded call, the recorded response, its exit.
        self.delegate, self.runner = bundle / "delegate", bundle / "delegate-run"
        self.delegate.write_text(
            "#!/bin/sh\n"
            f"printf '%s\\n' \"$*\" >> '{self.calls}'\n"
            f"[ -d '{self.state_fixture}' ] && /bin/cp -R '{self.state_fixture}/.' \"$2/\"\n"
            "for argument in \"$@\"; do\n"
            f"  [ \"$argument\" = models ] && {{ /bin/cat '{self.response}'; exit \"$(/bin/cat '{self.exit_file}')\"; }}\n"
            "done\n"
            "exit 64\n")
        self.runner.write_text("#!/bin/sh\nexit 0\n")
        for path in (self.delegate, self.runner):
            path.chmod(0o700)
        RecordingAcceptance.instances = []

    def write_state(self, root: object = ROOT, records: dict[str, str] | None = None) -> None:
        self.state_fixture.mkdir()
        (self.state_fixture / "root.json").write_text(json.dumps({"root_id": root}) + "\n")
        if records is not None:
            journal = self.state_fixture / "inspections" / JOURNAL
            journal.mkdir(parents=True)
            for name, text in records.items():
                (journal / name).write_text(text + "\n")

    def main(self, response: str, *patches, cli_exit: int = CLI_EXIT) -> tuple[int, RecordingAcceptance]:
        self.response.write_text(response + "\n")
        self.exit_file.write_text(f"{cli_exit}\n")
        # Only the private supervisor bootstrap is replaced; discovery, shutdown and cleanup are real.
        with patch.object(gate.Path, "home", return_value=self.root / "home"), \
                patch.object(entry, "choose_output", return_value=self.output), \
                patch.object(entry, "NativeAcceptance", RecordingAcceptance), \
                patch.object(gate, "reject_tmp"), \
                patch.object(gate, "require_discovery"), \
                patch.object(gate.NativeAcceptance, "setup"), \
                patch.object(gate.NativeAcceptance, "dispatch",
                             side_effect=AssertionError("an ordinary task must not start")):
            for active in patches:
                active.start()
            try:
                code = entry.main([
                    "--provider", "ccr:print", "--permission", "read-only", "--discover-models",
                    "--output", str(self.output), "--delegate", str(self.delegate),
                    "--runner", str(self.runner), "--pueue", "/bin/sh", "--pueued", "/bin/sh",
                    "--provider-executable", "/bin/sh"])
            finally:
                for active in reversed(patches):
                    active.stop()
        calls = self.calls.read_text().splitlines()
        self.assertEqual(len(calls), 1)
        self.assertIn(" models --provider ccr:print ", f" {calls[0]} ")
        return code, RecordingAcceptance.instances[-1]

    def assert_models_failure_receipt(self, status: str, catalog_status: str) -> None:
        data = (self.output / "failure.json").read_text()
        receipt = json.loads(data)
        self.assertEqual((receipt["status"], receipt["discovery_exit_code"]), (status, CLI_EXIT))
        self.assertTrue(receipt["discover_models"])
        self.assertEqual(set(receipt["model_discovery"]), SANITIZED_KEYS)
        self.assertEqual(receipt["model_discovery"]["status"], catalog_status)
        self.assertNotIn("error", receipt)
        self.assertNotIn("Delegate could not accept", data)
        self.assertFalse((self.output / "success.json").exists())
        self.assertFalse((self.output / "success-candidate.json").exists())

    def test_unavailable_invalid_request_before_any_inspection_is_neutral_blocked(self):
        code, run = self.main(INVALID_CWD)
        self.assertEqual(code, 2)
        self.assertEqual(run.model_discovery_exit_code, CLI_EXIT)
        self.assert_models_failure_receipt("BLOCKED", "unavailable")
        cleanup = json.loads((self.output / "cleanup.json").read_text())
        self.assertEqual((cleanup["completion"], cleanup["retained"]), ("failed", []))
        self.assertFalse(run.base.exists())

    def test_typed_failed_invalid_request_is_failed_with_its_observed_exit(self):
        code, run = self.main(NONDIRECTORY_CWD)
        self.assertEqual(code, 1)
        self.assertEqual(run.model_discovery_exit_code, CLI_EXIT)
        self.assert_models_failure_receipt("failed", "failed")
        cleanup = json.loads((self.output / "cleanup.json").read_text())
        self.assertEqual(cleanup["completion"], "failed")
        for path in (run.base, run.state, run.workspace):
            self.assertTrue(path.is_dir(), path)


    def assert_all_sources_retained(self, run: RecordingAcceptance) -> None:
        cleanup = json.loads((self.output / "cleanup.json").read_text())
        self.assertEqual(cleanup["completion"], "failed")
        self.assertEqual(sorted(cleanup["retained"]), sorted(owned.label for owned in run.owned_directories))
        self.assertEqual(cleanup["raw_process_captures"], "retained")
        for owned in run.owned_directories:
            self.assertTrue(owned.directory.is_dir(), owned.label)
        journal = run.models_state / "inspections" / JOURNAL
        for name, text in INSPECTION_RECORDS.items():
            self.assertEqual((journal / name).read_text(), text + "\n", name)
        self.assertEqual(json.loads((run.models_state / "root.json").read_text()), {"root_id": ROOT})
        captures = [path for path in (self.output / "processes").rglob("stdout") if path.is_file()]
        self.assertTrue(any(path.read_text().strip() == INSPECTION_FAILURE for path in captures))

    def test_genuine_inspection_failure_retains_every_owned_source_and_raw_capture(self):
        self.write_state(records=INSPECTION_RECORDS)
        code, run = self.main(INSPECTION_FAILURE, cli_exit=1)
        self.assertEqual((code, run.models_root_id), (1, ROOT))
        self.assertTrue((self.output / "failure-diagnostics.json").is_file())
        self.assert_all_sources_retained(run)

    def test_genuine_failure_retention_does_not_depend_on_diagnostics(self):
        self.write_state(records=INSPECTION_RECORDS)
        code, run = self.main(INSPECTION_FAILURE, patch.object(
            gate.NativeAcceptance, "_failure_diagnostics", side_effect=OSError("diagnostics unavailable")),
            cli_exit=1)
        self.assertEqual(code, 1)
        self.assertFalse((self.output / "failure-diagnostics.json").exists())
        self.assert_all_sources_retained(run)

    def diagnostics(self) -> tuple[dict[str, object], str]:
        data = (self.output / "failure-diagnostics.json").read_text()
        for name in ("failure-diagnostics.json", "failure.json", "cleanup.json"):
            self.assertNotIn(CANARY, (self.output / name).read_text(), name)
        return json.loads(data), data

    def test_inspection_journal_is_projected_as_bounded_descriptors_not_an_ordinary_task(self):
        for label, records in (("intact", INSPECTION_RECORDS),
                               ("partial", {"request.json": INSPECTION_RECORDS["request.json"]}),
                               ("missing", None)):
            with self.subTest(journal=label):
                self.setUp()
                self.write_state(records=records)
                code, run = self.main(INSPECTION_FAILURE, cli_exit=1)
                self.assertEqual(code, 1)
                value, _data = self.diagnostics()
                self.assertEqual((value["tasks"], value["task_ids"]), ([], [JOURNAL]))
                omitted = fixture_list(fixture_record(value["status"])["omitted"])
                self.assertFalse([item for item in omitted if JOURNAL in fixture_text(item)])
                inspection = fixture_record(value["model_discovery_inspection"])
                self.assertEqual(inspection["journal_task_id"], JOURNAL)
                self.assertEqual(inspection["context"], {"class": "inspection_failed",
                                                         "deadline_state": "unknown",
                                                         "journal_task_id": JOURNAL})
                journal = run.models_state / "inspections" / JOURNAL
                self.assertEqual(inspection["journal"], "missing" if records is None else "present")
                expected = {} if records is None else {
                    name: {"type": "file", "mode": (journal / name).stat().st_mode & 0o7777,
                           "bytes": (journal / name).stat().st_size,
                           "sha256": hashlib.sha256((journal / name).read_bytes()).hexdigest()}
                    for name in records}
                self.assertEqual(inspection["files"], expected)

    def test_unsafe_inspection_entries_are_reported_without_following_them(self):
        self.write_state(records=INSPECTION_RECORDS)
        outside = self.root / "outside.json"
        outside.write_text(CANARY + "\n")
        journal = self.state_fixture / "inspections" / JOURNAL
        (journal / "receipt.json").symlink_to(outside)
        (journal / "raw").mkdir()
        code, _run = self.main(INSPECTION_FAILURE, cli_exit=1)
        self.assertEqual(code, 1)
        value, _data = self.diagnostics()
        inspection = fixture_record(value["model_discovery_inspection"])
        self.assertEqual(set(fixture_record(inspection["files"])), set(INSPECTION_RECORDS))
        omitted = fixture_list(fixture_record(value["status"])["omitted"])
        self.assertIn(f"model-inspection:{JOURNAL}:receipt.json:symlink", omitted)
        self.assertIn(f"model-inspection:{JOURNAL}:raw:special", omitted)

    def test_model_root_identity_uses_the_canonical_task_identity(self):
        for label, root in (("valid", ROOT), ("uppercase", ROOT.upper()), ("short", ROOT[:31]),
                            ("integer", 5), ("null", None)):
            with self.subTest(root=label):
                self.setUp()
                self.write_state(root=root)
                code, run = self.main(NONDIRECTORY_CWD)
                self.assertEqual(code, 1)
                if label == "valid":
                    self.assertEqual(run.models_root_id, ROOT)
                    self.assertEqual(json.loads((self.output / "failure.json").read_text())["discovery_exit_code"],
                                     CLI_EXIT)
                else:
                    self.assertIsNone(run.models_root_id)
                    receipt = json.loads((self.output / "failure.json").read_text())
                    self.assertEqual(receipt["status"], "failed")
                    self.assertEqual(receipt["error"], "model discovery root identity is invalid")


if __name__ == "__main__":
    unittest.main()
