"""Crashes at each candidate boundary, then an observe-only resume that never launches again.

A crash is a raw interrupt, a lost reply or a lost durable result at one
boundary of the composed fixture in ``test_native_candidate_support``. Every
test asserts the native Start count across the run and its resume.
"""

from __future__ import annotations

from collections.abc import Callable, Iterator
import contextlib
from contextlib import contextmanager
from dataclasses import dataclass, replace
import errno
import hashlib
import io
import json
from pathlib import Path
import stat
import subprocess
import sys
import unittest
import zipfile
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import open_descriptors
from scripts.delegate_acceptance.native_release import candidate, cleanup, cli, execution, projection, storage
from scripts.delegate_acceptance.native_release.contract import ReleaseError
from scripts.delegate_acceptance.native_evidence.contract import COMPACT_RECEIPT_ROOT_NAME, EXPECTED_CASES
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.delegate_acceptance.release_verification.candidate_attempts import BlockReason, CellStatus, Closure
from scripts.delegate_acceptance.release_verification.candidate_manifest import encode_locator
from scripts.delegate_acceptance.release_verification.candidate_readiness import evaluate_attempt
from scripts.delegate_acceptance.release_verification.candidate_store_contract import TransitionKind
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError, BoundedCommandResult, CommandFailure,
)
from scripts.test_candidate_store_support import TIMEOUT, Fault
from scripts.test_native_candidate_support import MODEL, CandidateCase


PATCH_REF = "/git/refs/heads/"
LOST = BoundedCommandResult(1, b'{"message":"Server Error","status":"502"}')
READBACK = ("INCOMPLETE", "candidate_proof_readback_transport_closure_unproven")


class ClaimCrashTests(CandidateCase):
    """Nothing after a claim intent without this call's exact acknowledgement may Start."""

    def assertNeverLaunched(self, receipt: Receipt, reason: str) -> Receipt:
        self.assertEqual((receipt.status, receipt.reason), ("INCOMPLETE", reason))
        self.assertEqual((receipt.stage("claim").state, receipt.stage("native").state), ("intended", "pending"))
        self.assertEqual(self.starts, 0)
        resumed = self.resume()
        self.assertEqual((resumed.status, resumed.reason), ("INCOMPLETE", "candidate_claim_unresolved_no_launch"))
        self.assertEqual((self.starts, self.github.downloads), (0, 1))
        return resumed

    def test_an_interrupt_before_the_claim_ref_update_stays_intended(self) -> None:
        with self.fault_after_intent("claim", Fault("PATCH", PATCH_REF, error=KeyboardInterrupt())):
            receipt = self.execute()
        self.assertNeverLaunched(receipt, "invocation_interrupted_native_ownership_retained")
        self.assertEqual(self.snapshot().history.claims, ())

    def test_an_interrupt_after_the_claim_ref_moved_stays_intended(self) -> None:
        with self.fault_after_intent("claim", Fault("PATCH", PATCH_REF, error=KeyboardInterrupt(), apply=True)):
            receipt = self.execute()
        self.assertNeverLaunched(receipt, "invocation_interrupted_native_ownership_retained")
        self.assertEqual(len(self.snapshot().history.claims), 1)

    def test_a_lost_claim_acknowledgement_is_ambiguous_with_its_original_failure(self) -> None:
        with self.fault_after_intent("claim", Fault("PATCH", PATCH_REF, error=TIMEOUT, apply=True)):
            receipt = self.execute()
        self.assertNeverLaunched(receipt, "candidate_claim_append_ambiguous:command_timeout")
        self.assertEqual(len(self.snapshot().history.claims), 1)

    def test_a_lost_claim_result_after_the_acknowledgement_is_not_a_refusal(self) -> None:
        with self.failing_completion("claim", OSError("fixture lost the durable claim result")):
            receipt = self.execute()
        self.assertNeverLaunched(receipt, "release_evidence_or_io_invalid")
        self.assertEqual(receipt.stage("claim").result, {})
        self.assertEqual(len(self.snapshot().history.claims), 1)

    def foreign_acknowledgement(self, **change: object) -> Receipt:
        append = candidate.append

        def foreign(*arguments, **options):
            return replace(append(*arguments, **options), **change)

        with patch.object(candidate, "append", side_effect=foreign):
            receipt = self.execute()
        self.assertEqual((receipt.status, receipt.reason), ("INCOMPLETE", "candidate_claim_ack_mismatch"))
        self.assertEqual((receipt.stage("claim").state, self.starts), ("intended", 0))
        return receipt

    def test_an_acknowledgement_of_another_record_never_launches(self) -> None:
        self.foreign_acknowledgement(record_sha256s=("0" * 64,))

    def test_an_acknowledgement_of_another_transition_kind_never_launches(self) -> None:
        self.foreign_acknowledgement(kind=TransitionKind.PROOF)

    def test_a_saved_claim_intent_never_launches_even_through_the_launch_function(self) -> None:
        with self.failing_completion("claim", OSError("fixture lost the durable claim result")):
            receipt = self.execute()
        saved = Receipt.load(receipt.path)
        with self.owners(), self.assertRaises(ReleaseError) as caught:
            candidate._claim_and_launch(saved, self.client, saved.invocation.request)
        self.assertEqual((caught.exception.code, caught.exception.incomplete),
                         ("candidate_claim_intent_exists_no_launch", True))
        self.assertEqual((self.starts, len(self.github.writes("PATCH"))), (0, 1))

    def test_a_crash_before_the_claim_intent_resumes_without_any_store_write(self) -> None:
        with patch("scripts.delegate_acceptance.native_release.candidate._claim", side_effect=KeyboardInterrupt):
            receipt = self.execute()
        self.assertEqual((receipt.status, receipt.stage("claim").state), ("INCOMPLETE", "pending"))
        calls = len(self.github.calls)
        resumed = self.resume()
        self.assertEqual((resumed.status, resumed.reason), ("INCOMPLETE", "candidate_claim_unresolved_no_launch"))
        self.assertEqual((self.starts, len(self.github.calls)), (0, calls))


class ResumeObservesOnlyTests(CandidateCase):
    """Resume never downloads, extracts, claims or launches, whatever stage was left pending."""

    def assertObservedOnly(self, receipt: Receipt, status: str, reason: str) -> None:
        calls = len(self.github.calls)
        resumed = self.resume()
        self.assertEqual((resumed.status, resumed.reason), (status, reason))
        self.assertEqual((self.starts, len(self.github.calls)), (0, calls))
        self.assertEqual([resumed.stage(name).state for name in ("claim", "native")],
                         [receipt.stage(name).state for name in ("claim", "native")])

    def test_an_interrupted_download_is_never_downloaded_again(self) -> None:
        with patch.object(candidate, "load_candidate_artifact", side_effect=KeyboardInterrupt):
            receipt = self.execute()
        self.assertEqual(receipt.stage("download").state, "intended")
        self.assertObservedOnly(receipt, "INCOMPLETE", "candidate_pre_claim_incomplete_no_rerun")

    def test_a_confirmed_claim_without_a_native_intent_never_launches(self) -> None:
        with patch.object(candidate, "candidate_environment", side_effect=KeyboardInterrupt):
            receipt = self.execute()
        self.assertEqual((receipt.stage("claim").result["disposition"], receipt.stage("native").state),
                         ("confirmed", "pending"))
        self.assertObservedOnly(receipt, "INCOMPLETE", "candidate_native_not_launched_no_rerun")

    def test_a_refused_claim_stays_refused(self) -> None:
        self.put_claim(self.foreign_claim())
        receipt = self.execute()
        self.assertEqual(receipt.stage("claim").result["disposition"], "refused")
        self.assertObservedOnly(receipt, "BLOCKED", "candidate_claim_refused")


class NativeCrashTests(CandidateCase):
    def test_a_crash_after_native_intent_before_start_never_launches(self) -> None:
        with patch.object(execution, "Processes", side_effect=KeyboardInterrupt):
            receipt = self.execute()
        self.assertEqual((receipt.status, receipt.stage("native").state), ("INCOMPLETE", "intended"))
        self.assertEqual(receipt.stage("claim").result["disposition"], "confirmed")
        resumed = self.resume()
        self.assertEqual((resumed.status, resumed.reason), ("INCOMPLETE", "child_natural_completion_unproven"))
        self.assertEqual((self.starts, resumed.stage("native").state), (0, "intended"))
        self.assertEqual(len(self.snapshot().history.proofs), 0)

    def test_a_crash_after_the_natural_wait_resumes_from_the_observed_completion(self) -> None:
        with self.failing_completion("native", KeyboardInterrupt()):
            receipt = self.execute()
        self.assertEqual((receipt.status, receipt.stage("native").state), ("INCOMPLETE", "intended"))
        resumed = self.resume()
        self.assertEqual(resumed.status, "LOCAL_ONLY")
        self.assertEqual((self.starts, len(self.github.writes("PATCH"))), (1, 2))

    def test_a_crash_inside_projection_resumes_the_same_projection(self) -> None:
        with patch.object(projection, "build_candidate_proof", side_effect=KeyboardInterrupt):
            receipt = self.execute()
        self.assertEqual((receipt.status, receipt.stage("project").state), ("INCOMPLETE", "intended"))
        resumed = self.resume()
        self.assertEqual(resumed.status, "LOCAL_ONLY")
        self.assertEqual(resumed.stage("project").intent, receipt.stage("project").intent)
        self.assertEqual(self.starts, 1)


class RecordCrashTests(CandidateCase):
    """An intended record is read back only; an absent record is never permission to append again.

    A readback proves that the proof landed, not that this invocation's store
    command closed, so only a durable exact confirmation authorizes cleanup.
    """

    def resumed_patches(self, receipt: Receipt) -> tuple[Receipt, int]:
        self.assertEqual((receipt.status, receipt.stage("record").state), ("INCOMPLETE", "intended"))
        patches = len(self.github.writes("PATCH"))
        resumed = self.resume()
        self.assertEqual(self.starts, 1)
        return resumed, len(self.github.writes("PATCH")) - patches

    def assertReadBackRetained(self, receipt: Receipt) -> Receipt:
        resumed, patches = self.resumed_patches(receipt)
        self.assertEqual((resumed.status, resumed.reason), READBACK)
        record = resumed.stage("record")
        self.assertEqual((record.state, record.result["disposition"], patches), ("complete", "readback", 0))
        self.assertEqual([proof.proof_sha256 for proof in self.snapshot().history.proofs],
                         [resumed.stage("project").result["proof_sha256"]])
        self.assertEqual(resumed.stage("cleanup").state, "pending")
        self.assertTrue((resumed.invocation.root / "runner").is_dir())
        self.assertFalse((resumed.invocation.root / "runner.cleanup.json").exists())
        again = self.resume()
        self.assertEqual((again.status, again.reason, again.stage("record").result), (*READBACK, record.result))
        self.assertEqual((self.starts, len(self.github.writes("PATCH"))), (1, 2))
        self.assertTrue((again.invocation.root / "runner").is_dir())
        return resumed

    def test_an_interrupted_record_that_never_landed_stays_unresolved_without_another_append(self) -> None:
        with self.fault_after_intent("record", Fault("PATCH", PATCH_REF, error=KeyboardInterrupt())):
            receipt = self.execute()
        resumed, patches = self.resumed_patches(receipt)
        self.assertEqual((resumed.status, resumed.reason), ("INCOMPLETE", "candidate_proof_record_unresolved"))
        self.assertEqual((patches, resumed.stage("record").state, self.snapshot().history.proofs), (0, "intended", ()))
        self.assertTrue((resumed.invocation.root / "runner").is_dir())

    def test_an_interrupted_record_that_landed_is_read_back_and_retained(self) -> None:
        with self.fault_after_intent("record", Fault("PATCH", PATCH_REF, error=KeyboardInterrupt(), apply=True)):
            receipt = self.execute()
        self.assertReadBackRetained(receipt)

    def failed_transport(self, *, applied: bool, result: BoundedCommandResult | None = None,
                         error: BaseException | None = None) -> Receipt:
        with self.fault_after_intent("record", Fault("PATCH", PATCH_REF, result=result, error=error,
                                                     apply=applied)):
            return self.execute()

    def test_a_closed_http_failure_that_landed_is_read_back_and_retained(self) -> None:
        receipt = self.failed_transport(applied=True, result=LOST)
        self.assertEqual(receipt.reason, "candidate_record_append_ambiguous:github_request_failed")
        self.assertReadBackRetained(receipt)

    def test_a_closed_http_failure_that_did_not_land_stays_unresolved(self) -> None:
        resumed, patches = self.resumed_patches(self.failed_transport(applied=False, result=LOST))
        self.assertEqual((resumed.status, resumed.reason, patches),
                         ("INCOMPLETE", "candidate_proof_record_unresolved", 0))

    def assertUnwaitedReadBackRetained(self, failure: CommandFailure) -> None:
        receipt = self.failed_transport(applied=True, error=BoundedCommandError(failure))
        self.assertEqual(receipt.reason, f"candidate_record_append_ambiguous:{failure.value}")
        self.assertReadBackRetained(receipt)

    def test_a_landed_record_whose_store_command_termination_failed_is_retained(self) -> None:
        self.assertUnwaitedReadBackRetained(CommandFailure.TERMINATION_FAILED)

    def test_a_landed_record_whose_store_command_termination_timed_out_is_retained(self) -> None:
        self.assertUnwaitedReadBackRetained(CommandFailure.TERMINATION_TIMEOUT)

    def test_an_unwaited_store_command_that_did_not_land_stays_unresolved_and_retained(self) -> None:
        receipt = self.failed_transport(applied=False, error=BoundedCommandError(CommandFailure.TERMINATION_FAILED))
        resumed, patches = self.resumed_patches(receipt)
        self.assertEqual((resumed.status, resumed.reason, patches),
                         ("INCOMPLETE", "candidate_proof_record_unresolved", 0))
        self.assertEqual((resumed.stage("cleanup").state, self.snapshot().history.proofs), ("pending", ()))
        self.assertTrue((resumed.invocation.root / "runner").is_dir())

    def lost_diagnostic(self, error: BaseException) -> Receipt:
        """The ambiguity callback dies or cannot persist; only the durable record intent remains."""
        report = Receipt.report

        def failing(receipt: Receipt, status: str, reason: str) -> None:
            if reason.startswith("store_append_ambiguous:"):
                raise error
            report(receipt, status, reason)

        with patch.object(Receipt, "report", failing):
            return self.failed_transport(applied=True, error=BoundedCommandError(CommandFailure.TERMINATION_FAILED))

    def test_an_interrupted_ambiguity_callback_is_read_back_and_retained(self) -> None:
        receipt = self.lost_diagnostic(KeyboardInterrupt())
        self.assertEqual(receipt.reason, "invocation_interrupted_native_ownership_retained")
        self.assertReadBackRetained(receipt)

    def test_an_unpersisted_ambiguity_diagnostic_is_read_back_and_retained(self) -> None:
        receipt = self.lost_diagnostic(OSError("fixture could not persist the store diagnostic"))
        self.assertEqual(receipt.reason, "release_evidence_or_io_invalid")
        self.assertReadBackRetained(receipt)

    def test_a_lost_record_result_after_the_acknowledgement_is_read_back_and_retained(self) -> None:
        with self.failing_completion("record", OSError("fixture lost the durable record result")):
            receipt = self.execute()
        self.assertReadBackRetained(receipt)

    def test_a_pending_record_at_resume_makes_the_first_append(self) -> None:
        with patch("scripts.delegate_acceptance.native_release.candidate._record", side_effect=KeyboardInterrupt):
            receipt = self.execute()
        self.assertEqual((receipt.stage("project").state, receipt.stage("record").state), ("complete", "pending"))
        resumed = self.resume()
        self.assertEqual((resumed.status, resumed.stage("record").result["disposition"]), ("LOCAL_ONLY", "confirmed"))
        self.assertEqual((self.starts, len(self.github.writes("PATCH"))), (1, 2))
        self.assertFalse((resumed.invocation.root / "runner").exists())


class StaleCleanupTests(CandidateCase):
    """A readback receipt never replays or reports cleanup, whatever an earlier build left in that stage."""

    def read_back(self) -> Receipt:
        fault = Fault("PATCH", PATCH_REF, error=BoundedCommandError(CommandFailure.TERMINATION_FAILED), apply=True)
        with self.fault_after_intent("record", fault):
            self.execute()
        return Receipt.load(self.receipt.path)

    def resume_without_cleanup(self) -> Receipt:
        refused = AssertionError("a readback receipt reached the cleanup journal")
        with patch.object(cleanup, "prepare_directory_cleanup", side_effect=refused), \
                patch.object(cleanup, "resume_directory_cleanup", side_effect=refused):
            resumed = self.resume()
        self.assertEqual((resumed.status, resumed.reason), READBACK)
        self.assertEqual(resumed.stage("record").result["disposition"], "readback")
        self.assertEqual((self.starts, len(self.github.writes("PATCH"))), (1, 2))
        return resumed

    def test_a_stale_cleanup_intent_is_never_replayed(self) -> None:
        receipt = self.read_back()
        stale = {"target": receipt.stage("download").result["runner_identity"], "authorization_sha256": "0" * 64}
        receipt.intend("cleanup", stale)
        resumed = self.resume_without_cleanup()
        self.assertEqual((resumed.stage("cleanup").state, resumed.stage("cleanup").intent), ("intended", stale))
        self.assertTrue((resumed.invocation.root / "runner").is_dir())

    def test_a_cleanup_completed_by_an_earlier_readback_build_never_reports_success(self) -> None:
        self.read_back()

        def earlier(receipt: Receipt) -> None:
            cleanup._authorized_cleanup(receipt, children=("native",),
                                        authorization={"consumer": receipt.stage("record").result})

        with patch.object(candidate, "cleanup_candidate", side_effect=earlier):
            self.resume()
        self.assertEqual(Receipt.load(self.receipt.path).stage("cleanup").state, "complete")
        resumed = self.resume_without_cleanup()
        self.assertEqual(resumed.stage("cleanup").state, "complete")
        self.assertFalse((resumed.invocation.root / "runner").exists())


class CleanupRecoveryTests(CandidateCase):
    def test_a_crash_after_the_durable_confirmation_recovers_cleanup(self) -> None:
        with patch.object(candidate, "cleanup_candidate", side_effect=KeyboardInterrupt):
            receipt = self.execute()
        self.assertEqual((receipt.stage("record").result["disposition"], receipt.stage("cleanup").state),
                         ("confirmed", "pending"))
        resumed = self.resume()
        self.assertEqual((resumed.status, resumed.stage("cleanup").state), ("LOCAL_ONLY", "complete"))
        self.assertFalse((resumed.invocation.root / "runner").exists())
        self.assertEqual((self.starts, len(self.github.writes("PATCH"))), (1, 2))

    def test_an_interrupted_cleanup_replays_its_journal(self) -> None:
        with patch.object(cleanup, "resume_directory_cleanup", side_effect=KeyboardInterrupt):
            receipt = self.execute()
        self.assertEqual((receipt.status, receipt.stage("cleanup").state), ("INCOMPLETE", "intended"))
        resumed = self.resume()
        self.assertEqual((resumed.status, resumed.stage("cleanup").state), ("LOCAL_ONLY", "complete"))
        self.assertFalse((resumed.invocation.root / "runner").exists())
        self.assertEqual(self.starts, 1)

    def test_a_resume_after_cleanup_validates_the_proof_from_persisted_inputs_only(self) -> None:
        receipt = self.execute()
        self.assertFalse((receipt.invocation.root / "runner").exists())
        calls = len(self.github.calls)
        resumed = self.resume()
        self.assertEqual((resumed.status, resumed.reason), (receipt.status, receipt.reason))
        self.assertEqual((self.starts, len(self.github.calls)), (1, calls))
        self.assertEqual(resumed.stage("project").result, receipt.stage("project").result)

    def test_a_changed_retained_proof_is_refused_after_cleanup(self) -> None:
        receipt = self.execute()
        receipt.invocation.proof.write_bytes(receipt.invocation.proof.read_bytes() + b"\0")
        resumed = self.resume()
        self.assertEqual(resumed.status, "INCOMPLETE")
        self.assertEqual(self.starts, 1)


class NativeDependencyClosureTests(unittest.TestCase):
    """An unproven native completion retains the case and the class inputs its child consumed.

    One real finite native child runs in a dedicated class fixture, so this control never
    retains a shared owner. After its natural Wait the child's completion record is moved
    aside, so the inner cleanup's observation is unproven. The trees are removed by their
    created identity only after the record is restored and the canonical observer proves
    that natural Wait.
    """

    ASIDE = "completed.json.aside"

    @staticmethod
    def completion_record(inner: CandidateCase) -> Path:
        root = Receipt.load(inner.receipt.path).invocation.root
        return root / "runner" / "native-processes" / "001-native" / "completed.json"

    def test_an_unproven_native_completion_retains_the_case_and_its_class_inputs(self) -> None:
        record_of = self.completion_record

        class Inner(CandidateCase):
            def test_unproven_native_completion(inner) -> None:
                inner.mode = "fail"  # cleanup stays pending, so the runner keeps the native records
                self.assertEqual(inner.execute().stage("cleanup").state, "pending")
                record = record_of(inner)
                record.rename(record.with_name(self.ASIDE))

        inner = Inner("test_unproven_native_completion")
        self.addCleanup(self.remove_after_proven_completion, inner)
        self.addCleanup(Inner.doClassCleanups)
        result, stderr = unittest.TestResult(), io.StringIO()
        with contextlib.redirect_stderr(stderr):
            Inner.setUpClass()
            inner.run(result)
            Inner.doClassCleanups()
        self.assertEqual((result.testsRun, result.failures, len(result.errors), inner.starts), (1, [], 1, 1))
        self.assertIn("child_natural_completion_unproven", result.errors[0][1])
        unresolved = inner.case.unresolved
        self.assertIsInstance(unresolved, ReleaseError)
        self.assertIs(inner.tree.unresolved, unresolved)
        for tree in (inner.case, inner.tree):
            self.assertIn(f"{tree.label} retained (child_natural_completion_unproven)", stderr.getvalue())
            self.assertTrue(tree.root.is_dir())

    def remove_after_proven_completion(self, inner: CandidateCase) -> None:
        """Restore the record; only the canonical observer's proven natural Wait authorizes removal."""
        if getattr(inner, "receipt", None) is None:
            return
        record = self.completion_record(inner)
        aside = record.with_name(self.ASIDE)
        if aside.exists() and not record.exists():
            aside.rename(record)
        execution.observe_child_completion(Receipt.load(inner.receipt.path), "native")
        for tree in (inner.case, inner.tree):
            if tree.root.exists() and not tree.dispose():
                tree.remove_retained()


@dataclass
class IsolatedFixture:
    """One dedicated case class and its test; ``setup_closed`` holds only once its setup children are closed.

    It is set only after every real setup Git child returned normally, or after a
    non-spawning fake was proven to be the only setup launcher reached.
    """

    case: type[CandidateCase]
    inner: CandidateCase
    setup_closed: bool = False


class ClosureObservationOwnerTests(unittest.TestCase):
    """An observation the case owner could not complete retains its dedicated case and class trees.

    Each control defines a fresh CandidateCase subclass, so it never retains a shared
    owner. A retained tree is removed by its created identity only once its setup is
    proven closed and this test's own count of native Start calls is zero. Zero native
    Starts is only a further guard: it never proves that a setup Git child closed.
    """

    RECEIPT_FAULTS: dict[str, tuple[Callable[[Path], object], str]] = {
        "missing": (lambda path: path.unlink(), "BoundedReadError"),
        "not_a_file": (lambda path: (path.unlink(), path.mkdir()), "BoundedReadError"),
        "malformed": (lambda path: path.write_bytes(b'{"schema_version": '), "PublicProofError"),
        "definite": (lambda path: path.write_bytes(b"{}"), "release_receipt_invalid"),
    }

    def setUp(self) -> None:
        self.fixtures: list[IsolatedFixture] = []
        self.native_starts: list[int] = []
        start = execution.Processes.start

        def counted(processes: execution.Processes, *arguments: object, **options: object) -> object:
            self.native_starts.append(1)
            return start(processes, *arguments, **options)

        patcher = patch.object(execution.Processes, "start", counted)
        patcher.start()
        self.addCleanup(patcher.stop)

    def remove_without_child(self, fixture: IsolatedFixture) -> None:
        """Dispose of each tree; remove a retained one only once no child can still write to it.

        Without proven closed setup a retained tree stays in place with its fact; the
        canonical disposal has already consumed its pins and reported it.
        """
        trees = (getattr(fixture.inner, "case", None), getattr(fixture.case, "tree", None))
        retained = [tree for tree in trees if tree is not None and not tree.dispose()]
        if not fixture.setup_closed:
            return
        self.assertEqual(self.native_starts, [], "a native child started; its trees stay retained")
        for tree in retained:
            tree.remove_retained()

    def isolated(self, case: type[CandidateCase]) -> IsolatedFixture:
        """One test of ``case``; its class cleanups, then the guarded removal, always run."""
        fixture = IsolatedFixture(case, case("test_body"))
        self.fixtures.append(fixture)
        self.addCleanup(self.remove_without_child, fixture)
        self.addCleanup(case.doClassCleanups)
        return fixture

    @staticmethod
    def run_inner(inner: CandidateCase) -> unittest.TestResult:
        result = unittest.TestResult()
        inner.run(result)
        return result

    def test_a_started_child_whose_receipt_cannot_be_loaded_retains_both_owners(self) -> None:
        for fault, (corrupt, code) in self.RECEIPT_FAULTS.items():
            with self.subTest(fault=fault):
                self.assert_receipt_fault_retains(corrupt, code)

    def assert_receipt_fault_retains(self, corrupt: Callable[[Path], object], code: str) -> None:
        loads: list[BaseException] = []
        load = Receipt.load

        def observed(path: Path) -> Receipt:
            try:
                return load(path)
            except BaseException as error:
                loads.append(error)
                raise

        class Inner(CandidateCase):
            def test_body(inner) -> None:
                inner.durable = inner.receipt.path.read_bytes()
                inner.starts = 1  # a synthetic started intent; no native child exists
                corrupt(inner.receipt.path)

        fixture, stderr = self.isolated(Inner), io.StringIO()
        inner = fixture.inner
        with contextlib.redirect_stderr(stderr), patch.object(Receipt, "load", observed):
            Inner.setUpClass()
            fixture.setup_closed = True
            result = self.run_inner(inner)
            Inner.doClassCleanups()
        (original,) = loads
        self.assertEqual((result.testsRun, result.failures, len(result.errors)), (1, [], 1))
        self.assertIn(type(original).__name__, result.errors[0][1])
        self.assertIs(inner.case.unresolved, original)
        self.assertIs(Inner.tree.unresolved, original)
        for tree in (inner.case, Inner.tree):
            self.assertIn(f"{tree.label} retained ({code}): {tree.root}\n", stderr.getvalue())
            self.assertTrue(tree.root.is_dir())
            self.assertFalse(tree.dispose())
        if inner.receipt.path.is_dir():
            inner.receipt.path.rmdir()
        inner.receipt.path.write_bytes(inner.durable)
        refused = Inner("test_body")
        self.assertIn("refused", self.run_inner(refused).errors[0][1])
        self.assertIsNone(getattr(refused, "case", None))
        self.assertIs(Inner.tree.unresolved, original)
        self.assertFalse((inner.receipt.invocation.root / "runner").exists())

    def test_a_case_that_started_nothing_never_loads_its_receipt(self) -> None:
        loads: list[Path] = []
        load = Receipt.load

        def observed(path: Path) -> Receipt:
            loads.append(path)
            return load(path)

        class Inner(CandidateCase):
            def test_body(inner) -> None:
                inner.receipt.path.unlink()

        fixture = self.isolated(Inner)
        inner = fixture.inner
        with patch.object(Receipt, "load", observed):
            Inner.setUpClass()
            fixture.setup_closed = True
            result = self.run_inner(inner)
            Inner.doClassCleanups()
        self.assertTrue(result.wasSuccessful(), result.errors)
        self.assertEqual((loads, inner.starts, inner.case.unresolved, Inner.tree.unresolved), ([], 0, None, None))
        self.assertFalse(inner.case.root.exists())
        self.assertFalse(Inner.tree.root.exists())

    def test_a_lost_harness_git_child_retains_the_class_tree_with_its_raw_original(self) -> None:
        raised: list[BoundedCommandError] = []

        def lost(argv: list[str], environment: dict[str, str], **options: object) -> BoundedCommandResult:
            raised.append(BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT))
            raise raised[-1]

        class Inner(CandidateCase):
            def test_body(inner) -> None:
                raise AssertionError("a test ran over an unresolved class harness")

        fixture, stderr = self.isolated(Inner), io.StringIO()
        refused = fixture.inner
        with contextlib.redirect_stderr(stderr), \
                patch("scripts.delegate_acceptance.release_verification.commands.run_bounded_command", lost):
            with self.assertRaises(VerificationError) as caught:
                Inner.setUpClass()
            # The fake, which never spawns, was the only setup launcher reached.
            fixture.setup_closed = len(raised) == 1 and caught.exception.__context__ is raised[0]
            result = self.run_inner(refused)
            Inner.doClassCleanups()
        (original,) = raised
        self.assertEqual(caught.exception.code, "command_termination_timeout")
        self.assertIs(Inner.tree.unresolved, original)
        self.assertIn("refused", result.errors[0][1])
        self.assertIsNone(getattr(refused, "case", None))
        self.assertIn(f"{Inner.tree.label} retained (command_termination_timeout): {Inner.tree.root}\n",
                      stderr.getvalue())
        self.assertTrue(Inner.tree.root.is_dir())

    def test_zero_native_starts_never_authorize_removing_a_tree_whose_real_setup_git_is_unproven(self) -> None:
        refusals: list[subprocess.TimeoutExpired] = []

        def refuse(argv: list[str], *arguments: object, **options: object) -> subprocess.Popen[bytes]:
            refusals.append(subprocess.TimeoutExpired(argv, 0))
            raise refusals[-1]

        target = ClosureObservationOwnerTests("test_a_case_that_started_nothing_never_loads_its_receipt")
        self.addCleanup(self.remove_refused_setup, target)
        result, stderr, descriptors = unittest.TestResult(), io.StringIO(), open_descriptors()
        with contextlib.redirect_stderr(stderr), patch.object(subprocess, "Popen", refuse):
            target.run(result)
        self.assertEqual(open_descriptors(), descriptors)
        self.assertEqual((result.testsRun, result.failures, len(result.errors)), (1, [], 1))
        self.assertIn("command_timeout", result.errors[0][1])
        ((argv, *_),) = [error.args for error in refusals]
        self.assertEqual(argv[0], "git")
        (fixture,) = target.fixtures
        tree = fixture.case.tree
        self.assertIs(type(tree.unresolved), BoundedCommandError)
        self.assertIs(tree.unresolved.__context__, refusals[0], "the real boundary's raw original is kept")
        self.assertEqual((target.native_starts, fixture.setup_closed, getattr(fixture.inner, "case", None)),
                         ([], False, None))
        self.assertEqual(stderr.getvalue().count(f"{tree.label} retained (command_timeout): {tree.root}\n"), 1)
        self.assertTrue(tree.root.is_dir())
        self.assertFalse(tree.dispose())

    def remove_refused_setup(self, target: ClosureObservationOwnerTests) -> None:
        """This test refused every Popen before any process existed; only that authorizes removal."""
        for fixture in getattr(target, "fixtures", ()):
            tree = getattr(fixture.case, "tree", None)
            if tree is not None and tree.root.exists() and not tree.dispose():
                tree.remove_retained()


class PublicCandidateCase(CandidateCase):
    """The public commands over this case's admitted receipt; defines no tests of its own.

    ``disk`` makes every save raise ENOSPC from the start, or once ``arm`` holds
    after a durable one; ``durable`` keeps the file's last durable bytes.
    """

    def setUp(self) -> None:
        super().setUp()
        self.durable = self.receipt.path.read_bytes()

    @contextmanager
    def disk(self, arm: Callable[[Receipt], bool] | None = None) -> Iterator[None]:
        save = Receipt.save
        self.full = arm is None

        def guarded(receipt: Receipt, *, create: bool = False) -> None:
            if self.full:
                raise OSError(errno.ENOSPC, "fixture receipt volume is full")
            save(receipt, create=create)
            self.durable = receipt.path.read_bytes()
            self.full = arm is None or arm(receipt)

        with patch.object(Receipt, "save", guarded):
            yield

    def admitted(self, _checkout: Path, request: object, _argv: list[str]) -> Receipt:
        self.assertEqual(request, self.receipt.invocation.request)
        return self.receipt

    def public(self, argv: list[str]) -> tuple[int, dict[str, object]]:
        """``cli.main`` with this case's tree as the state base and its receipt as the admitted one."""
        output = io.StringIO()
        with self.owners(), patch.object(cli, "state_base", return_value=self.case.root), \
                patch.object(storage, "state_base", return_value=self.case.root), \
                patch.object(cli, "create_receipt", side_effect=self.admitted), \
                patch("sys.stdout", output), patch("sys.stderr", io.StringIO()):
            code = cli.main(argv)
        return code, json.loads(output.getvalue())

    def run_candidate(self) -> tuple[int, dict[str, object]]:
        locator = self.case.root / "locator.json"
        locator.write_bytes(encode_locator(self.locator))
        store = self.location()
        return self.public(["run-candidate", "--locator", str(locator), "--store-branch", store.branch,
                            "--store-genesis-commit", store.genesis_commit, "--store-marker-blob", store.marker_blob,
                            "--attempt", "1", "--opencode-model", MODEL, "--ccr-model", MODEL, "--agy-model", MODEL])

    def resume_public(self) -> tuple[int, dict[str, object]]:
        return self.public(["resume", "--receipt", str(self.receipt.path)])

    def assertUnsaved(self, outcome: tuple[int, dict[str, object]], status: str, fault: str) -> None:
        self.assertEqual(outcome, ({"BLOCKED": 2, "INCOMPLETE": 3}[status], {
            "status": status, "reason": "release_receipt_unavailable", "fault": fault,
            "receipt": str(self.receipt.path), "receipt_persisted": False,
            "invocation_id": self.receipt.invocation.invocation_id, "candidate_id": self.locator.candidate_id}))
        self.assertEqual(self.receipt.path.read_bytes(), self.durable)


class UnpersistedOutcomeTests(PublicCandidateCase):
    """The public commands while every receipt save fails: the selected outcome, never a plain block.

    Every save raises ENOSPC from the start, or once ``arm`` holds after a
    durable one. The report names the fault and identity, the file keeps its
    last durable bytes, and nothing launches or appends again.
    """

    @staticmethod
    def native_intended(receipt: Receipt) -> bool:
        return receipt.stage("native").state == "intended"

    @staticmethod
    def ambiguity_saved(receipt: Receipt) -> bool:
        return receipt.reason.startswith("store_append_ambiguous:")

    def test_a_lost_native_completion_is_incomplete_and_resume_never_launches_again(self) -> None:
        with self.disk(self.native_intended):
            self.assertUnsaved(self.run_candidate(), "INCOMPLETE", "release_evidence_or_io_invalid")
            self.assertEqual(Receipt.load(self.receipt.path).stage("native").state, "intended")
            self.assertEqual((self.starts, len(self.github.writes("PATCH"))), (1, 1))
            self.assertUnsaved(self.resume_public(), "INCOMPLETE", "release_evidence_or_io_invalid")
        self.assertEqual((self.starts, len(self.github.writes("PATCH"))), (1, 1))
        self.assertTrue((self.receipt.invocation.root / "runner").is_dir())

    def test_an_interrupted_native_completion_keeps_its_interruption_reason(self) -> None:
        with self.disk(self.native_intended), self.failing_completion("native", KeyboardInterrupt()):
            outcome = self.run_candidate()
        self.assertUnsaved(outcome, "INCOMPLETE", "invocation_interrupted_native_ownership_retained")
        self.assertEqual(self.starts, 1)
        self.assertTrue((self.receipt.invocation.root / "runner").is_dir())

    def test_an_ambiguous_claim_append_keeps_its_typed_fault_and_resume_never_launches(self) -> None:
        with self.disk(self.ambiguity_saved), \
                self.fault_after_intent("claim", Fault("PATCH", PATCH_REF, error=TIMEOUT, apply=True)):
            outcome = self.run_candidate()
        self.assertUnsaved(outcome, "INCOMPLETE", "candidate_claim_append_ambiguous:command_timeout")
        self.assertEqual(Receipt.load(self.receipt.path).stage("claim").state, "intended")
        with self.disk():
            self.assertUnsaved(self.resume_public(), "INCOMPLETE", "candidate_claim_unresolved_no_launch")
        self.assertEqual((self.starts, len(self.github.writes("PATCH"))), (0, 1))
        self.assertEqual(len(self.snapshot().history.claims), 1)

    def test_an_ambiguous_proof_append_keeps_its_typed_fault_and_resume_never_appends(self) -> None:
        with self.disk(self.ambiguity_saved), \
                self.fault_after_intent("record", Fault("PATCH", PATCH_REF, error=TIMEOUT, apply=True)):
            outcome = self.run_candidate()
        self.assertUnsaved(outcome, "INCOMPLETE", "candidate_record_append_ambiguous:command_timeout")
        self.assertEqual(Receipt.load(self.receipt.path).stage("record").state, "intended")
        with self.disk():
            self.assertUnsaved(self.resume_public(), "INCOMPLETE", "release_evidence_or_io_invalid")
        self.assertEqual((self.starts, len(self.github.writes("PATCH"))), (1, 2))
        self.assertEqual(len(self.snapshot().history.proofs), 1)
        self.assertTrue((self.receipt.invocation.root / "runner").is_dir())

    def test_a_save_failure_before_any_paid_effect_stays_blocked_with_its_fault(self) -> None:
        calls = len(self.github.calls)
        with self.disk():
            outcome = self.run_candidate()
        self.assertUnsaved(outcome, "BLOCKED", "release_evidence_or_io_invalid")
        self.assertEqual((self.starts, len(self.github.calls)), (0, calls))



class UnknownControlOutcomeTests(PublicCandidateCase):
    """A control child whose closure the canonical runner could not prove, before any claim.

    The first store read fails inside the fake GitHub owner, so no real child exists.
    Only the canonical unproven-ownership codes are INCOMPLETE; a closed start or an
    HTTP refusal stays BLOCKED. Nothing starts or writes, and resume never reruns.
    """

    CLAIMED = ("claim", "native", "project", "record", "cleanup")
    NOT_FOUND = BoundedCommandResult(1, b'{"message":"Not Found","status":"404"}')

    def fail_first_read(self, *, error: BaseException | None = None,
                        result: BoundedCommandResult | None = None) -> None:
        self.github.faults.append(Fault("GET", "", error=error, result=result))

    def assertBeforeClaim(self, outcome: tuple[int, dict[str, object]], status: str, reason: str) -> None:
        code, report = outcome
        self.assertEqual((code, report["status"], report["reason"]),
                         ({"BLOCKED": 2, "INCOMPLETE": 3}[status], status, reason))
        stored = Receipt.load(self.receipt.path)
        self.assertEqual((stored.status, stored.reason), (status, reason))
        self.assertEqual(stored.stage("preflight").state, "intended")
        self.assertEqual([stored.stage(name).state for name in self.CLAIMED], ["pending"] * 5)
        self.assertEqual((self.starts, self.github.writes("POST"), self.github.writes("PATCH")), (0, [], []))

    def assert_unknown_control_is_incomplete(self, failure: CommandFailure) -> None:
        self.fail_first_read(error=BoundedCommandError(failure))
        self.assertBeforeClaim(self.run_candidate(), "INCOMPLETE", failure.value)
        calls = len(self.github.calls)
        code, report = self.resume_public()
        self.assertEqual((code, report["status"], report["reason"]),
                         (3, "INCOMPLETE", "candidate_pre_claim_incomplete_no_rerun"))
        self.assertEqual((self.starts, len(self.github.calls)), (0, calls))

    def test_a_timed_out_store_read_is_incomplete_and_never_reruns(self) -> None:
        self.assert_unknown_control_is_incomplete(CommandFailure.TIMEOUT)

    def test_a_store_read_whose_termination_timed_out_is_incomplete(self) -> None:
        self.assert_unknown_control_is_incomplete(CommandFailure.TERMINATION_TIMEOUT)

    def test_a_store_read_whose_termination_failed_is_incomplete(self) -> None:
        self.assert_unknown_control_is_incomplete(CommandFailure.TERMINATION_FAILED)

    def test_a_store_read_without_its_capture_is_incomplete(self) -> None:
        self.assert_unknown_control_is_incomplete(CommandFailure.CAPTURE_UNAVAILABLE)

    def test_a_store_read_that_never_started_stays_blocked(self) -> None:
        self.fail_first_read(error=BoundedCommandError(CommandFailure.START_FAILED))
        self.assertBeforeClaim(self.run_candidate(), "BLOCKED", "command_start_failed")

    def test_a_refused_store_read_stays_blocked(self) -> None:
        self.fail_first_read(result=self.NOT_FOUND)
        self.assertBeforeClaim(self.run_candidate(), "BLOCKED", "github_request_failed")

    def test_an_unsaved_unknown_control_outcome_is_incomplete_with_its_fault(self) -> None:
        self.fail_first_read(error=TIMEOUT)
        with self.disk(lambda receipt: receipt.stage("preflight").state == "intended"):
            self.assertUnsaved(self.run_candidate(), "INCOMPLETE", "command_timeout")
            self.assertEqual(Receipt.load(self.receipt.path).stage("preflight").state, "intended")
            self.assertUnsaved(self.resume_public(), "INCOMPLETE", "candidate_pre_claim_incomplete_no_rerun")
        self.assertEqual((self.starts, self.github.writes("POST"), self.github.writes("PATCH")), (0, [], []))


class BlockedFailureCopyCleanupTests(CandidateCase):
    """Outer candidate cleanup removes runner scratch but never the private blocked failure copies."""

    SENTINEL = b"do not copy this detail"

    def test_candidate_cleanup_keeps_each_private_failure_copy_out_of_the_public_proof(self) -> None:
        self.mode = "blocked"
        receipt = self.execute()
        self.assertEqual((receipt.stage("cleanup").state, (receipt.invocation.root / "runner").exists()),
                         ("complete", False))
        receipts = receipt.invocation.aggregate.parent / COMPACT_RECEIPT_ROOT_NAME
        for case in EXPECTED_CASES:
            copy = receipts / f"{case.case_id}.failure.json"
            compact = json.loads((receipts / f"{case.case_id}.json").read_bytes())
            self.assertEqual(hashlib.sha256(copy.read_bytes()).hexdigest(), compact["source_receipt_sha256"])
            self.assertEqual(stat.S_IMODE(copy.lstat().st_mode), 0o600)
            self.assertIn(self.SENTINEL, copy.read_bytes())
        with zipfile.ZipFile(receipt.invocation.proof) as proof:
            self.assertEqual([name for name in proof.namelist() if "failure" in name], [])
            self.assertFalse(any(self.SENTINEL in proof.read(name) for name in proof.namelist()))
        snapshot = self.snapshot()
        platform_proof = snapshot.history.proofs[0]
        self.assertEqual((platform_proof.closure, platform_proof.unresolved_resources), (Closure.COMPLETE, ()))
        self.assertEqual({(cell.status, cell.block_reason) for cell in platform_proof.cells},
                         {(CellStatus.BLOCKED, BlockReason.UNCLASSIFIED)})
        verdict = evaluate_attempt(snapshot.candidate.manifest, snapshot.history, 1)
        prefix = f"{platform_proof.os}/{platform_proof.arch}/"
        self.assertEqual({finding.subject.removeprefix(prefix): finding.code for finding in verdict.findings
                          if finding.subject.startswith(prefix)},
                         {case.case_id: "cell_blocked" for case in EXPECTED_CASES})


if __name__ == "__main__":
    unittest.main()
