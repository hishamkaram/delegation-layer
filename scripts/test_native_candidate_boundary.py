"""The request type alone selects the release or candidate path; legacy entry points refuse candidates.

These tests use a stored candidate receipt in a test-owned tree. A process
owner that fails on construction proves no child starts; the static check
supplements those behavior tests and claims no sandbox.
"""

from __future__ import annotations

import ast
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock, patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.native_release import (
    cleanup, coordinator, execution, preflight, projection,
)
from scripts.delegate_acceptance.native_release.contract import (
    Invocation, ReleaseError, ReleaseRequest, candidate_request, invocation_name, release_request,
)
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.test_native_candidate_request import candidate


PACKAGE = Path(execution.__file__).resolve().parent


class CandidateReceiptCase(unittest.TestCase):
    def setUp(self) -> None:
        self.tree = FixtureTree.open(prefix="native-candidate-boundary", label="native candidate boundary")
        self.addCleanup(self.tree.dispose)
        self.receipt = self.create(candidate())

    def create(self, request) -> Receipt:
        invocation_id = "9" * 32
        root = self.tree.root / invocation_name(request, invocation_id)
        owner = DirectoryOwner()
        try:
            token = owner.create("boundary invocation", root, self.tree.root)
            return Receipt.create(Invocation(
                invocation_id=invocation_id, request=request, checkout=self.tree.root, root=root,
                platform_os="Linux", platform_arch="amd64", directory_identity=directory_identity_record(token)),
                ["boundary"])
        finally:
            owner.release()

    def no_start(self):
        return patch.object(execution, "Processes", Mock(side_effect=AssertionError("a child was started")))


class PublicRunChildTests(CandidateReceiptCase):
    def test_every_candidate_child_is_refused_before_any_intent(self) -> None:
        before = self.receipt.path.read_bytes()
        for name in ("archive", "native", "finalize"):
            with self.subTest(child=name), self.no_start() as processes:
                with self.assertRaises(ReleaseError) as caught:
                    execution.run_child(self.receipt, name, ["/bin/true"], {}, timeout=1)
                self.assertEqual((caught.exception.code, caught.exception.incomplete),
                                 ("release_request_required", False))
                processes.assert_not_called()
        self.assertEqual(self.receipt.path.read_bytes(), before)

    def test_the_private_transaction_runs_only_the_candidate_native_child(self) -> None:
        before = self.receipt.path.read_bytes()
        with self.no_start() as processes, self.assertRaises(ReleaseError) as caught:
            execution._child_transaction(self.receipt, "archive", ["/bin/true"], {}, timeout=1)
        self.assertEqual(caught.exception.code, "candidate_child_invalid")
        processes.assert_not_called()
        self.assertEqual(self.receipt.path.read_bytes(), before)

    def test_an_existing_candidate_native_intent_never_starts_or_observes(self) -> None:
        argv = ["/bin/true"]
        self.receipt.intend("native", execution.child_intent(self.receipt, "native", argv))
        with self.no_start() as processes, patch.object(execution, "observe_child") as observe:
            with self.assertRaises(ReleaseError) as caught:
                execution._child_transaction(self.receipt, "native", argv, {}, timeout=1)
        self.assertEqual((caught.exception.code, caught.exception.incomplete),
                         ("candidate_native_intent_exists_no_launch", True))
        processes.assert_not_called()
        observe.assert_not_called()


class LegacyBoundaryTests(CandidateReceiptCase):
    def test_release_only_reads_refuse_a_candidate_before_any_effect(self) -> None:
        before = self.receipt.path.read_bytes()
        invocation = self.receipt.invocation
        calls = {
            "release_request": lambda: release_request(invocation),
            "child_environment": lambda: execution.child_environment(invocation),
            "preflight": lambda: preflight.preflight(invocation, Mock(side_effect=AssertionError)),
            "download": lambda: execution.download(self.receipt, Mock(side_effect=AssertionError)),
            "archive": lambda: execution.archive(self.receipt, {}),
            "finalize": lambda: execution.finalize(self.receipt, {}),
            "expectation": lambda: projection.expectation(self.receipt),
            "cleanup": lambda: cleanup.cleanup(self.receipt),
            "submit": lambda: coordinator.submit(self.receipt, None),
            "promotable": lambda: projection.locally_promotable(self.receipt),
        }
        for name, call in calls.items():
            with self.subTest(entry=name), self.no_start():
                with self.assertRaises(ReleaseError) as caught:
                    call()
                self.assertEqual(caught.exception.code, "release_request_required")
        self.assertEqual(self.receipt.path.read_bytes(), before)

    def test_a_release_invocation_never_reaches_candidate_reads(self) -> None:
        release = self.create(ReleaseRequest(repo="hishamkaram/delegation-layer", tag="v1.2.3",
                                             opencode_model="vendor/model", ccr_model=None, no_submit=True))
        with self.assertRaises(ReleaseError) as caught:
            candidate_request(release.invocation)
        self.assertEqual(caught.exception.code, "candidate_request_required")
        self.assertEqual(release_request(release.invocation), release.invocation.request)


class EnvironmentTests(CandidateReceiptCase):
    def test_a_candidate_keeps_user_settings_but_never_the_native_tag_input(self) -> None:
        inherited = {"TAG": "user-own-tag", "DELEGATE_ACCEPTANCE_TAG": "v9.9.9", "PROVIDER_SETTING": "kept"}
        with patch.dict(os.environ, inherited):
            env = execution.candidate_environment(self.receipt.invocation, "a" * 40)
            self.assertEqual(os.environ["DELEGATE_ACCEPTANCE_TAG"], "v9.9.9")
        self.assertNotIn("DELEGATE_ACCEPTANCE_TAG", env)
        self.assertEqual((env["TAG"], env["PROVIDER_SETTING"]), ("user-own-tag", "kept"))
        self.assertEqual(env["DELEGATE_ACCEPTANCE_SOURCE_REVISION"], "a" * 40)
        self.assertEqual(env["OPENCODE_LIVE_MODEL"], self.receipt.invocation.request.opencode_model)

    def test_a_release_still_forwards_its_tag(self) -> None:
        release = self.create(ReleaseRequest(repo="hishamkaram/delegation-layer", tag="v1.2.3",
                                             opencode_model="vendor/model", ccr_model=None, no_submit=True))
        with patch.dict(os.environ, {"TAG": "user-own-tag", "DELEGATE_ACCEPTANCE_TAG": "v9.9.9"}):
            env = execution.child_environment(release.invocation)
        self.assertEqual((env["DELEGATE_ACCEPTANCE_TAG"], env["TAG"]), ("v1.2.3", "user-own-tag"))


class StaticLaunchBoundaryTests(unittest.TestCase):
    """Supplements the behavior tests: Python privacy is a convention, not a sandbox."""

    def callers(self, name: str) -> set[tuple[str, str]]:
        found = set()
        for path in sorted(PACKAGE.glob("*.py")):
            module = ast.parse(path.read_text(encoding="utf-8"))
            for function in ast.walk(module):
                if isinstance(function, ast.FunctionDef) and function.name != name and any(
                        isinstance(node, ast.Name) and node.id == name for node in ast.walk(function)):
                    found.add((path.stem, function.name))
        return found

    def test_only_the_release_entry_and_the_fresh_claim_reach_the_child_transaction(self) -> None:
        self.assertEqual(self.callers("_child_transaction"),
                         {("execution", "run_child"), ("candidate", "_claim_and_launch")})

    def test_recovery_never_reaches_a_launch(self) -> None:
        self.assertEqual(self.callers("_claim_and_launch"), {("candidate", "execute_candidate")})
        self.assertNotIn(("candidate", "resume_candidate"), self.callers("native_argv"))


if __name__ == "__main__":
    unittest.main()
