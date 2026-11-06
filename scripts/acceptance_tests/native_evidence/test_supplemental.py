"""Supplemental public-continue receipts bind to one canonical candidate, never beside it.

Validation only observes native evidence: every test that validates also proves
the inputs' bytes and inventory are unchanged, so any outcome can be retried.
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

from scripts import native_acceptance_evidence as evidence
from scripts import test_native_acceptance_evidence as evidence_tests
from scripts.acceptance_tests.native.support import lifetime_closure_source, production_acp_source
from scripts.delegate_acceptance.native.constants import PI_SESSION_MISS_GATE, PI_SESSION_MISS_SCENARIO
from scripts.delegate_acceptance.native_evidence import supplemental
from scripts.delegate_acceptance.native_evidence.contract import EXPECTED_CASES, EvidenceError
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.source import source_seeds


REVISION = "a" * 40
OTHER_REVISION = "b" * 40
BINARIES = {"runner_sha256": "3" * 64, "pueue_sha256": "4" * 64, "pueued_sha256": "5" * 64}
FINGERPRINT = "7" * 64
HARNESS = {"harness_revision": REVISION, "harness_sha256": FINGERPRINT, "committed": True}
SCOPE = {"tmpdir_scopes": dict(supplemental.CONTINUATION_TMPDIR_SCOPES), "writable_roots": "own-tmpdir",
         "selection_inherited": True, "meta_sha256": {"fresh": "8" * 64, "resume": "9" * 64}}
# Observed native Pi guardian commands per task: version, help and the one primary command.
PI_GUARDIAN_LAUNCHES = 3


def _write(path: Path, value: object) -> None:
    path.write_text(json.dumps(value), encoding="utf-8")


def _snapshot(root: Path) -> dict[str, str]:
    """Every entry under ``root``: directories by kind, files by content digest."""
    entries: dict[str, str] = {}
    for current, directories, files in os.walk(root):
        for name in directories:
            entries[os.path.relpath(Path(current) / name, root)] = "directory"
        for name in files:
            path = Path(current) / name
            entries[os.path.relpath(path, root)] = hashlib.sha256(path.read_bytes()).hexdigest()
    return entries


def _rewrite(path: Path, **changes) -> None:
    _write(path, {**json.loads(path.read_text()), **changes})


class SupplementalFixture:
    """One frozen candidate: canonical aggregate, twelve continue cases and the Pi control."""

    def __init__(self, root: Path):
        self.root = root
        self.delegate = root / "delegate"
        self.delegate.write_bytes(b"candidate delegate\n")
        self.binaries = {"delegate_sha256": hashlib.sha256(self.delegate.read_bytes()).hexdigest(), **BINARIES}
        self.serial = 0
        self.canonical = root / "canonical"
        self.supplemental = root / "supplemental"
        self.pi_control = root / "pi-control"

    def task_ids(self, aliases) -> dict[str, str]:
        tasks = {}
        for alias in aliases:
            self.serial += 1
            tasks[alias] = f"{self.serial:032x}"
        return tasks

    def write_case(self, directory: Path, case, tasks: dict[str, str], **extra) -> None:
        session = "session-" + case.case_id
        runtimes = {task: {"executable": "/fixture/provider", "version": "1.0", "sha256": "1" * 64}
                    for task in tasks.values()}
        sessions = {alias: session for alias in tasks}
        _write(directory / "success.json", {
            "gate": "acceptance-native", "status": "passed", "provider": case.provider,
            "mode": case.permission, "scenario": case.scenario,
            "requested_model": None, "requested_effort": None, "tasks": tasks,
            "provider_sessions": sessions,
            "provider_versions": {task: "1.0" for task in tasks.values()},
            "provider_runtime_identities": runtimes, "natural_daemon_shutdown": True,
            "lifetime_closure": lifetime_closure_source(tasks),
            **({"production_acp": production_acp_source(tasks, sessions)}
               if case.provider == "opencode:run" else {}),
            **extra,
        })
        self.write_binding(directory, case.provider, case.permission, case.scenario, next(iter(tasks.values())))
        _write(directory / "cleanup.json", {"status": "removed", "supervisor_shutdown_observed": True,
                                            "raw_process_captures": "removed"})

    def write_binding(self, directory: Path, provider: str, mode: str, scenario: str, first: str,
                      **overrides) -> None:
        _write(directory / "binding.json", {
            "provider": provider, "mode": mode, "scenario": scenario,
            "provider_identity_scope": "first-admitted-task", "provider_task_id": first,
            "requested_model": None, "requested_effort": None,
            "provider_executable": "/fixture/provider", "provider_version": "1.0",
            "provider_sha256": "1" * 64, **self.binaries, **overrides,
        })

    def build_canonical(self, revision: str = REVISION) -> Path:
        (self.canonical / evidence.CASE_ROOT_NAME).mkdir(parents=True)
        lines = [evidence.MATRIX_HEADER]
        for case in EXPECTED_CASES:
            directory = evidence.prepare_case_directory(self.canonical, case.case_id)
            aliases = evidence._expected_task_aliases(case.provider, case.scenario)
            self.write_case(directory, case, self.task_ids(aliases))
            lines.append("\t".join((case.case_id, case.provider, case.permission, case.scenario,
                                    "0", "PASS", case.case_output)) + "\n")
        matrix = self.canonical / evidence.CASE_MATRIX_NAME
        matrix.write_text("".join(lines), encoding="ascii")
        receipt = self.canonical / evidence.AGGREGATE_RECEIPT_NAME
        evidence.build_aggregate(self.canonical, matrix, receipt, source_revision=revision)
        # The producer records the digest of the canonical aggregate it validated.
        self.aggregate_sha256 = hashlib.sha256(receipt.read_bytes()).hexdigest()
        return receipt

    def build_supplemental(self) -> dict[str, dict[str, str]]:
        (self.supplemental / evidence.CASE_ROOT_NAME).mkdir(parents=True)
        written = {}
        for case in supplemental.supplemental_cases():
            directory = evidence.prepare_case_directory(self.supplemental, case.case_id)
            tasks = self.task_ids(("fresh", "resume"))
            self.write_continue_case(directory, case, tasks)
            written[case.case_id] = tasks
        return written

    def write_continue_case(self, directory: Path, case, tasks: dict[str, str]) -> None:
        self.write_case(directory, case, tasks, continuation_command="continue",
                        continuation_scope=SCOPE, harness=HARNESS,
                        canonical_aggregate_sha256=self.aggregate_sha256)
        _write(directory / supplemental.CONTINUE_ATTEMPT_NAME, {
            "name": "resume", "task_id": tasks["resume"], "receipt_written": True,
            "response": {"command": "continue", "admission": "admitted", "task_id": tasks["resume"],
                         "parent_task_id": tasks["fresh"]},
        })

    def build_pi_control(self, **overrides) -> dict[str, str]:
        self.pi_control.mkdir()
        tasks = self.task_ids(("fresh", "resume", "session_miss"))
        closure = lifetime_closure_source(tasks)
        ordinary = closure["ordinary"]
        assert isinstance(ordinary, dict)
        for fact in ordinary["tasks"].values():
            fact["launch_count"] = PI_GUARDIAN_LAUNCHES
        _write(self.pi_control / "success.json", {
            "gate": PI_SESSION_MISS_GATE, "status": "passed", "provider": "pi:json",
            "mode": "read-only", "scenario": PI_SESSION_MISS_SCENARIO,
            "continuation_command": "continue", "tasks": tasks,
            "provider_sessions": {"fresh": "pi-session", "resume": "pi-session"},
            "session_miss": {"task_id": tasks["session_miss"], "runner_result": "Success",
                             "publication": "rejected", "provider_exit_code": 1,
                             "session_lookup": "not-found", "fresh_session_created": False,
                             "outcome_sha256": "6" * 64},
            "session_directories": dict(supplemental.PI_SESSION_DIRECTORIES), "harness": HARNESS,
            "canonical_aggregate_sha256": self.aggregate_sha256,
            "natural_daemon_shutdown": True, "lifetime_closure": closure, **overrides,
        })
        self.write_binding(self.pi_control, "pi:json", "read-only", PI_SESSION_MISS_SCENARIO,
                           tasks["fresh"])
        _write(self.pi_control / "cleanup.json", {"status": "removed", "supervisor_shutdown_observed": True,
                                                  "raw_process_captures": "removed"})
        return tasks

    def validate(self, receipt: Path, revision: str = REVISION, *,
                 observed: tuple[str, str, bool] = (REVISION, FINGERPRINT, True)) -> supplemental.SupplementalSummary:
        # The Git observation is the I/O owner; the fixture fixes what it reports.
        with mock.patch.object(supplemental, "observe_harness", return_value=observed) as observe:
            summary = supplemental.validate_supplemental(
                supplemental_root=self.supplemental, canonical_aggregate=receipt, source_revision=revision,
                candidate_delegate=self.delegate, pi_control=self.pi_control,
                environment={}, checkout=self.root)
        observe.assert_called_once()
        self.observed_seeds = observe.call_args.kwargs["seeds"]
        return summary


class SupplementalValidationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-supplemental-")
        self.addCleanup(temporary.cleanup)
        self.fixture = SupplementalFixture(Path(temporary.name).resolve(strict=True))

    def built(self) -> tuple[Path, dict[str, dict[str, str]]]:
        receipt = self.fixture.build_canonical()
        cases = self.fixture.build_supplemental()
        self.fixture.build_pi_control()
        return receipt, cases

    def case_directory(self, index: int = 0) -> Path:
        return self.fixture.supplemental / supplemental.supplemental_cases()[index].case_output

    def refused_unchanged(self, receipt: Path, code: str, **options) -> None:
        """A refusal names its cause and leaves every input byte and entry in place."""
        before = _snapshot(self.fixture.root)
        with self.assertRaisesRegex(EvidenceError, code):
            self.fixture.validate(receipt, **options)
        self.assertEqual(_snapshot(self.fixture.root), before)

    def test_supplemental_group_repeats_twelve_canonical_rows_without_changing_the_matrix(self):
        rows = supplemental.supplemental_cases()
        self.assertEqual(len(rows), 12)
        self.assertEqual(len(EXPECTED_CASES), 17)
        self.assertTrue(set(rows) <= set(EXPECTED_CASES))
        self.assertEqual({(case.permission, case.scenario) for case in rows},
                         {("workspace-write", "write"), ("read-only", "read-only")})
        self.assertNotIn("codex:review", {case.provider for case in rows})

    def test_valid_validation_repeats_with_unchanged_bytes_and_inventory(self):
        receipt, cases = self.built()
        before = _snapshot(self.fixture.root)
        first = self.fixture.validate(receipt)
        self.assertEqual(_snapshot(self.fixture.root), before)
        second = self.fixture.validate(receipt)
        self.assertEqual(_snapshot(self.fixture.root), before)
        self.assertEqual(first, second)
        self.assertEqual((first["source_revision"], first["harness_revision"], first["harness_sha256"]),
                         (REVISION, REVISION, FINGERPRINT))
        self.assertEqual(first["binaries"], self.fixture.binaries)
        self.assertEqual([case["case_id"] for case in first["cases"]],
                         [case.case_id for case in supplemental.supplemental_cases()])
        self.assertTrue(all(case["command"] == "continue" and case["tasks"] == cases[case["case_id"]]
                            for case in first["cases"]))
        self.assertEqual(first["pi_session_miss"]["gate"], PI_SESSION_MISS_GATE)

    def test_summary_retains_the_sanitized_source_proof_and_bindings(self):
        receipt, _ = self.built()
        bound = self.fixture.validate(receipt)["cases"][0]
        directory = self.case_directory()
        source_sha256 = hashlib.sha256((directory / "success.json").read_bytes()).hexdigest()
        attempt_sha256 = hashlib.sha256(
            (directory / supplemental.CONTINUE_ATTEMPT_NAME).read_bytes()).hexdigest()
        self.assertEqual(bound["source_receipt_sha256"], source_sha256)
        self.assertEqual(bound["source_proof"]["source_receipt_sha256"], source_sha256)
        self.assertEqual(bound["continue_attempt_sha256"], attempt_sha256)
        binaries = bound["source_proof"]["binary_identities"]
        assert isinstance(binaries, dict)
        self.assertEqual(binaries["task_ids"], sorted(bound["tasks"].values()))
        self.assertNotIn("receipt_path", bound)

    def test_late_bad_pi_control_leaves_all_cases_and_a_pi_fix_succeeds(self):
        receipt, _ = self.built()
        path = self.fixture.pi_control / "success.json"
        good = json.loads(path.read_text())
        _rewrite(path, session_miss={**good["session_miss"], "session_lookup": "found"})
        self.refused_unchanged(receipt, "pi_control_session_miss_invalid")
        self.assertTrue(all((self.case_directory(index) / "success.json").is_file()
                            for index in range(len(supplemental.supplemental_cases()))))
        _write(path, good)
        self.assertEqual(self.fixture.validate(receipt)["pi_session_miss"]["gate"], PI_SESSION_MISS_GATE)

    def test_bad_middle_case_leaves_all_inputs_and_its_correction_succeeds(self):
        receipt, cases = self.built()
        index = 6
        case = supplemental.supplemental_cases()[index]
        self.assertNotEqual(case.provider, "opencode:run")
        directory = self.case_directory(index)
        source = directory / "success.json"
        tasks = cases[case.case_id]
        corruptions = {
            "supplemental_binary_mismatch": lambda: self.fixture.write_binding(
                directory, case.provider, case.permission, case.scenario,
                cases[case.case_id]["fresh"], runner_sha256="9" * 64),
            "supplemental_command_not_continue": lambda: _rewrite(source, continuation_command="dispatch"),
            "supplemental_task_overlap": lambda: self.fixture.write_continue_case(
                directory, case, {"fresh": f"{1:032x}", "resume": tasks["resume"]}),
        }
        for code, corrupt in corruptions.items():
            with self.subTest(code=code):
                corrupt()
                self.refused_unchanged(receipt, case.case_id + ": " + code)
                self.fixture.write_continue_case(directory, case, tasks)
                self.assertEqual(len(self.fixture.validate(receipt)["cases"]), 12)

    def test_missing_inputs_are_refused_without_creating_directories(self):
        receipt, _ = self.built()
        shutil_target = self.case_directory(11)
        (shutil_target / "success.json").unlink()
        self.refused_unchanged(receipt, supplemental.supplemental_cases()[11].case_id)
        for path in sorted(shutil_target.rglob("*"), reverse=True):
            path.rmdir() if path.is_dir() else path.unlink()
        shutil_target.rmdir()
        self.refused_unchanged(receipt, supplemental.supplemental_cases()[11].case_id)
        self.assertFalse(shutil_target.exists())

    def test_absent_supplemental_root_is_refused_without_creating_it(self):
        receipt = self.fixture.build_canonical()
        self.fixture.build_pi_control()
        self.refused_unchanged(receipt, supplemental.supplemental_cases()[0].case_id)
        self.assertFalse(self.fixture.supplemental.exists())

    def test_stale_source_revision_is_refused(self):
        receipt = self.fixture.build_canonical(revision=OTHER_REVISION)
        self.fixture.build_supplemental()
        self.fixture.build_pi_control()
        self.refused_unchanged(receipt, "aggregate_source_revision_mismatch")
        self.refused_unchanged(receipt, "supplemental_source_revision_mismatch", revision="")
        self.refused_unchanged(receipt, "supplemental_source_revision_mismatch", revision=OTHER_REVISION)

    def test_stale_or_uncommitted_harness_is_refused(self):
        receipt, _ = self.built()
        self.refused_unchanged(receipt, "supplemental_harness_mismatch",
                               observed=(REVISION, "6" * 64, True))
        self.refused_unchanged(receipt, "supplemental_harness_not_committed",
                               observed=(REVISION, FINGERPRINT, False))
        source = self.case_directory(3) / "success.json"
        _rewrite(source, harness={**HARNESS, "harness_revision": OTHER_REVISION})
        self.refused_unchanged(receipt, supplemental.supplemental_cases()[3].case_id +
                               ": supplemental_harness_mismatch")

    def test_missing_or_other_producer_aggregate_reference_is_refused_and_correctable(self):
        receipt, cases = self.built()
        index = 7
        case = supplemental.supplemental_cases()[index]
        source = self.case_directory(index) / "success.json"
        good = json.loads(source.read_text())
        missing = {key: value for key, value in good.items() if key != "canonical_aggregate_sha256"}
        for label, value in (("missing", missing), ("other", {**good, "canonical_aggregate_sha256": "6" * 64})):
            with self.subTest(label):
                _write(source, value)
                self.refused_unchanged(receipt, case.case_id + ": supplemental_canonical_aggregate_mismatch")
        _write(source, good)
        summary = self.fixture.validate(receipt)
        self.assertEqual({item["case_id"]: item["tasks"] for item in summary["cases"]}, cases)

    def test_aggregate_reference_from_another_canonical_run_is_refused(self):
        receipt, _ = self.built()
        other = self.fixture.root / "other-canonical"
        other.mkdir()
        fixture = SupplementalFixture(other)
        fixture.delegate.write_bytes(self.fixture.delegate.read_bytes())
        fixture.serial = 1000
        _write(self.case_directory(0) / "success.json", {
            **json.loads((self.case_directory(0) / "success.json").read_text()),
            "canonical_aggregate_sha256": hashlib.sha256(fixture.build_canonical().read_bytes()).hexdigest()})
        self.refused_unchanged(receipt, "supplemental_canonical_aggregate_mismatch")

    def test_harness_observation_failure_is_refused(self):
        receipt, _ = self.built()
        before = _snapshot(self.fixture.root)
        failure = VerificationError("source_changed_during_observation", "harness")
        with mock.patch.object(supplemental, "observe_harness", side_effect=failure):
            with self.assertRaisesRegex(EvidenceError, "harness_observation_source_changed_during_observation"):
                supplemental.validate_supplemental(
                    supplemental_root=self.fixture.supplemental, canonical_aggregate=receipt,
                    source_revision=REVISION, candidate_delegate=self.fixture.delegate,
                    pi_control=self.fixture.pi_control, environment={}, checkout=self.fixture.root)
        self.assertEqual(_snapshot(self.fixture.root), before)

    def test_producer_and_validator_observe_one_seed_declaration(self):
        receipt, _ = self.built()
        self.fixture.validate(receipt)
        self.assertEqual(self.fixture.observed_seeds, supplemental.CONTINUATION_HARNESS_SEEDS)
        self.assertEqual(supplemental.CONTINUATION_HARNESS_SEEDS[:-1], source_seeds("native-acceptance"))
        self.assertEqual(supplemental.CONTINUATION_HARNESS_SEEDS[-1], "scripts/verify_native_continuation.py")
        checkout = Path(supplemental.__file__).resolve().parents[3]
        self.assertTrue((checkout / supplemental.CONTINUATION_HARNESS_SEEDS[-1]).is_file())

    def test_scope_without_own_tmpdir_roots_is_refused(self):
        receipt, _ = self.built()
        _rewrite(self.case_directory(2) / "success.json",
                 continuation_scope={**SCOPE, "writable_roots": "tmpdir-not-derived"})
        self.refused_unchanged(receipt, "supplemental_scope_unproven")

    def test_candidate_delegate_must_be_the_canonical_binary(self):
        receipt, _ = self.built()
        self.fixture.delegate.write_bytes(b"another build\n")
        self.refused_unchanged(receipt, "candidate_delegate_mismatch")

    def test_dispatch_substitution_is_refused(self):
        receipt, _ = self.built()
        directory = self.case_directory()
        attempt = json.loads((directory / supplemental.CONTINUE_ATTEMPT_NAME).read_text())
        attempt["response"]["command"] = "dispatch"
        _write(directory / supplemental.CONTINUE_ATTEMPT_NAME, attempt)
        self.refused_unchanged(receipt, "supplemental_continue_binding_invalid")

    def test_resume_dispatch_attempt_beside_the_continue_record_is_refused(self):
        receipt, _ = self.built()
        _write(self.case_directory() / supplemental.DISPATCH_ATTEMPT_NAME, {"response": {}})
        self.refused_unchanged(receipt, "supplemental_dispatch_substituted")

    def test_substituted_task_id_or_canonical_digest_is_refused(self):
        receipt, cases = self.built()
        case = supplemental.supplemental_cases()[0]
        source = self.case_directory() / "success.json"
        good = source.read_text()
        _rewrite(source, tasks={"fresh": cases[case.case_id]["fresh"], "resume": f"{0xfff:032x}"})
        self.refused_unchanged(receipt, case.case_id + ": source_proof_incomplete")
        source.write_text(good, encoding="utf-8")
        aggregate = json.loads(receipt.read_text())
        aggregate["cases"][0]["binary_identities"]["sha256"]["runner_sha256"] = "9" * 64
        _write(receipt, aggregate)
        self.refused_unchanged(receipt, "aggregate_")


class PiControlValidationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-pi-control-")
        self.addCleanup(temporary.cleanup)
        self.fixture = SupplementalFixture(Path(temporary.name).resolve(strict=True))
        self.receipt = self.fixture.build_canonical()
        self.candidate = supplemental.candidate_from_aggregate(self.receipt, HARNESS, self.fixture.delegate)

    def reset_pi_control(self) -> None:
        """Remove the flat Pi control files a previous build wrote under the owned root."""
        if self.fixture.pi_control.exists():
            for path in self.fixture.pi_control.iterdir():
                path.unlink()
            self.fixture.pi_control.rmdir()

    def refused(self, code: str, *, excluded: set[str] | None = None, **overrides) -> None:
        self.reset_pi_control()
        self.fixture.build_pi_control(**overrides)
        before = _snapshot(self.fixture.root)
        with self.assertRaisesRegex(EvidenceError, code):
            supplemental.validate_pi_control(self.fixture.pi_control, self.candidate,
                                             set(self.candidate.task_ids) if excluded is None else excluded)
        self.assertEqual(_snapshot(self.fixture.root), before)

    def test_positive_control_stays_outside_the_aggregate_gate(self):
        tasks = self.fixture.build_pi_control()
        bound = supplemental.validate_pi_control(self.fixture.pi_control, self.candidate, set())
        self.assertEqual(bound["tasks"], tasks)
        self.assertNotEqual(PI_SESSION_MISS_GATE, "acceptance-native")

    def test_canonical_gate_receipt_is_not_a_pi_control(self):
        self.refused("pi_control_identity_invalid", gate="acceptance-native")

    def test_resumed_session_is_not_a_miss(self):
        self.refused("pi_control_session_miss_invalid", session_miss={"publication": "committed"})

    def test_miss_under_the_control_directory_is_refused(self):
        self.refused("pi_control_session_miss_invalid",
                     session_directories={"fresh": "control", "resume": "control", "session_miss": "control"})

    def test_control_without_the_producer_aggregate_reference_is_refused(self):
        self.refused("supplemental_canonical_aggregate_mismatch", canonical_aggregate_sha256=None)
        self.refused("supplemental_canonical_aggregate_mismatch", canonical_aggregate_sha256="6" * 64)

    def test_control_from_another_harness_is_refused(self):
        self.refused("supplemental_harness_mismatch", harness={**HARNESS, "harness_sha256": "6" * 64})

    def rewrite_closure(self, change) -> dict[str, str]:
        tasks = self.fixture.build_pi_control()
        path = self.fixture.pi_control / "success.json"
        source = json.loads(path.read_text())
        change(source, tasks)
        _write(path, source)
        return tasks

    def test_guardian_launch_count_is_not_a_provider_turn_count(self):
        for launches in (1, 3, 4):
            with self.subTest(launches=launches):
                self.reset_pi_control()
                tasks = self.rewrite_closure(lambda source, tasks: source["lifetime_closure"]["ordinary"][
                    "tasks"][tasks["session_miss"]].update(launch_count=launches))
                bound = supplemental.validate_pi_control(self.fixture.pi_control, self.candidate, set())
                self.assertEqual(bound["tasks"], tasks)

    def test_unproven_lifetime_closure_is_refused(self):
        def ordinary(source):
            return source["lifetime_closure"]["ordinary"]

        def extra_task(source, tasks):
            ordinary(source)["tasks"]["f" * 32] = dict(ordinary(source)["tasks"][tasks["session_miss"]])

        changes = {
            "no launch": lambda source, tasks: ordinary(source)["tasks"][tasks["session_miss"]].update(
                launch_count=0),
            "missing task": lambda source, tasks: ordinary(source)["tasks"].pop(tasks["session_miss"]),
            "extra task": extra_task,
            "unfinished daemon": lambda source, tasks: source.update(natural_daemon_shutdown=False),
            "no closure": lambda source, tasks: source.update(lifetime_closure=None),
        }
        for label, change in changes.items():
            with self.subTest(label):
                self.reset_pi_control()
                self.rewrite_closure(change)
                before = _snapshot(self.fixture.root)
                with self.assertRaisesRegex(EvidenceError, "pi_control_lifetime_unproven"):
                    supplemental.validate_pi_control(self.fixture.pi_control, self.candidate, set())
                self.assertEqual(_snapshot(self.fixture.root), before)

    def test_control_task_overlapping_another_receipt_is_refused(self):
        self.refused("supplemental_task_overlap", excluded={f"{self.fixture.serial + 1:032x}"})

    def test_control_binaries_must_match_the_candidate(self):
        tasks = self.fixture.build_pi_control()
        self.fixture.write_binding(self.fixture.pi_control, "pi:json", "read-only",
                                   PI_SESSION_MISS_SCENARIO, tasks["fresh"],
                                   pueued_sha256="8" * 64)
        with self.assertRaisesRegex(EvidenceError, "pi_control_binary_mismatch"):
            supplemental.validate_pi_control(self.fixture.pi_control, self.candidate, set())


class CanonicalCandidateTests(unittest.TestCase):
    """Only a complete all-PASS canonical aggregate names a candidate a producer may bind."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-canonical-candidate-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve(strict=True)
        self.fixture = SupplementalFixture(self.root)

    def test_blocked_or_failed_canonical_aggregate_is_not_a_candidate(self):
        builder = object.__new__(evidence_tests.NativeAcceptanceEvidenceTests)
        builder.root = self.root
        for label, options in (("blocked", {}), ("failed", {"status": "PASS", "failed_index": 2})):
            with self.subTest(label):
                target = self.root / label
                value, _ = builder.build(root=target, **options)
                self.assertNotEqual(value["status"], "PASS")
                with self.assertRaisesRegex(EvidenceError, "canonical_aggregate_not_passed"):
                    supplemental.candidate_from_aggregate(
                        target / evidence.AGGREGATE_RECEIPT_NAME, HARNESS, self.fixture.delegate)

    def test_selected_runtime_binaries_must_be_the_candidates(self):
        candidate = supplemental.candidate_from_aggregate(
            self.fixture.build_canonical(), HARNESS, self.fixture.delegate)
        self.assertEqual(candidate.aggregate_sha256, self.fixture.aggregate_sha256)
        runner = self.root / "runner"
        runner.write_bytes(b"runner\n")
        matching = {**candidate.binaries, "runner_sha256": hashlib.sha256(b"runner\n").hexdigest()}
        supplemental.require_selected_binaries(
            supplemental.Candidate(candidate.harness, candidate.aggregate_sha256, matching, candidate.task_ids),
            {"runner_sha256": runner})
        with self.assertRaisesRegex(EvidenceError, "candidate_runner_mismatch"):
            supplemental.require_selected_binaries(candidate, {"runner_sha256": runner})
        with self.assertRaisesRegex(EvidenceError, "candidate_binary_selection_invalid"):
            supplemental.require_selected_binaries(candidate, {"provider_sha256": runner})


class HarnessObservationTests(unittest.TestCase):
    def test_observation_reports_this_checkout_through_the_source_owner(self):
        checkout = Path(supplemental.__file__).resolve().parents[3]
        observed = supplemental.observe_continuation_harness(os.environ, checkout)
        self.assertRegex(observed["harness_revision"], r"^[0-9a-f]{40,64}$")
        self.assertRegex(observed["harness_sha256"], r"^[0-9a-f]{64}$")
        self.assertIsInstance(observed["committed"], bool)


if __name__ == "__main__":
    unittest.main()
