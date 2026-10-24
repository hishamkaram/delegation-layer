"""Trusted-main readiness observation over a real frozen store history behind the real ``GitHubClient``.

The store is the in-memory ``FakeGitHub``; a finite front answers only the pull
request read and the status POSTs and forwards every other request to it. The
ready history is appended through the public writer from canonical records, so
success comes from canonical replay and readiness, never from a mocked verdict.
The executing harness is a stated test input; its committed-source binding is
the canonical observer's own tested contract.
"""

from __future__ import annotations

import contextlib
from dataclasses import dataclass, field, replace
import errno
import io
import json
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, BoundedCommandResult, CommandFailure
from scripts.delegate_acceptance.release_verification import candidate_readiness_check as check
from scripts.delegate_acceptance.release_verification import commands, filesystem, github
from scripts.delegate_acceptance.release_verification.candidate_history import encode_publication
from scripts.delegate_acceptance.release_verification.candidate_install_contract import InstallChannel
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    BoundCandidate, SourceObservation, authorize_publication,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import EntryFamily
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.source import fingerprint_sources
from scripts.test_candidate_store_replay import DARWIN, LINUX, Scenario
from scripts.test_candidate_store_semantic import SemanticCase, name
from scripts.test_candidate_source_support import SourceRepository
from scripts.test_candidate_store_support import NOT_FOUND, FakeGitHub, Fault


def _source_tree() -> str:
    """The tree ``SemanticCase.put_source`` stores for the fixture installer: the real tree of B."""
    store = FakeGitHub()
    return store.build({"install.sh": ("100755", store.blob(fx.INSTALL_SH)),
                        "README.md": ("100644", store.blob(b"r"))})


def _candidate() -> BoundCandidate:
    """The canonical fixture candidate whose manifest names the tree its source commit actually has."""
    manifest = fx.manifest()
    manifest = replace(manifest, source=replace(manifest.source, tree=_source_tree()))
    return BoundCandidate(locator=fx.locator_for(manifest), manifest=manifest)


PULL_REQUEST = 41
RUN_ID = "5550001"
HARNESS = "4" * 40  # the trusted main commit; the candidate build harness is fx.HARNESS
HARNESS_SHA256 = "e" * 64
TARGET_URL = f"https://github.com/{fx.REPOSITORY}/actions/runs/{RUN_ID}"
STATUS_ROUTE = f"repos/{fx.REPOSITORY}/statuses/"
STORE_KEYS = ("store-repository", "store-branch", "store-genesis-commit", "store-marker-blob")
# The reviewed production store pins, stated here independently of the default under test.
PRODUCTION_PINS = {"store-repository": "hishamkaram/delegation-layer", "store-branch": "development-proof/evidence",
                   "store-genesis-commit": "d3582a5613e0194899170b5034833d1b88ddaa09",
                   "store-marker-blob": "8b5bcb36f5acbed53dcef51220d7056643ed70b9"}
ENVIRONMENT = {
    "GITHUB_REPOSITORY": fx.REPOSITORY, "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main",
    "GITHUB_WORKFLOW_REF": f"{fx.REPOSITORY}/.github/workflows/development-readiness.yml@refs/heads/main",
    "GITHUB_SHA": HARNESS, "GITHUB_RUN_ID": RUN_ID, "GITHUB_RUN_ATTEMPT": "1",
    "GITHUB_SERVER_URL": "https://github.com", "GITHUB_API_URL": "https://api.github.com",
    "GH_TOKEN": "test-token-not-a-secret", "PATH": "/usr/bin",
}


@dataclass
class PullRequestFront:
    """Answer the pull request read and status POSTs; forward every other request to the store."""

    store: FakeGitHub
    pull: dict[str, object] | None = None
    posted: list[tuple[str, dict[str, object]]] = field(default_factory=list)
    replies: list[BoundedCommandResult | dict[str, object] | None] = field(default_factory=list)
    next_id: int = 57089257227

    def __call__(self, argv: list[str], environment: dict[str, str], *, max_output: int,
                 **options: object) -> BoundedCommandResult:
        method, endpoint = argv[3], argv[8]
        if endpoint == f"repos/{fx.REPOSITORY}/pulls/{PULL_REQUEST}" and method == "GET":
            self.store.calls.append((method, endpoint))
            return NOT_FOUND if self.pull is None else BoundedCommandResult(0, json.dumps(self.pull).encode())
        if endpoint.startswith(STATUS_ROUTE) and method == "POST":
            self.store.calls.append((method, endpoint))
            body = json.loads(Path(argv[argv.index("--input") + 1]).read_bytes())
            self.posted.append((endpoint.rsplit("/", 1)[1], body))
            override = self.replies.pop(0) if self.replies else None
            if isinstance(override, BoundedCommandResult):
                return override
            self.next_id += 1
            ack = {"id": self.next_id, "url": f"https://api.github.com/{endpoint}", **body, **(override or {})}
            return BoundedCommandResult(0, json.dumps(ack).encode())
        return self.store(argv, environment, max_output=max_output, **options)


def pull_request(state: object = "open", merged: object = False, *, head: object = fx.COMMIT,
                 head_repo: object = {"full_name": fx.REPOSITORY}, **changes: object) -> dict[str, object]:
    value: dict[str, object] = {
        "number": PULL_REQUEST, "state": state, "merged": merged,
        "base": {"ref": "main", "repo": {"full_name": fx.REPOSITORY}},
        "head": {"sha": head, "ref": "task", "repo": head_repo},
    }
    value.update(changes)
    return value


def scoped_fault(owner: object, attribute: str, error: BaseException | type[BaseException]):
    """Run the canonical receipt writer with one OS step failing only inside it, never in the transport."""
    def write(*args: object, **options: object) -> None:
        with patch.object(owner, attribute, side_effect=error):
            filesystem.write_receipt(*args, **options)
    return write


class CloseFaultFile:
    """The real file ``fdopen`` returned; ``close`` really flushes and closes it, then reports a fault."""

    def __init__(self, file: io.BufferedWriter):
        self.file = file
        self.released = False

    def __enter__(self) -> CloseFaultFile:
        return self

    def __exit__(self, *_exception: object) -> None:
        self.close()

    def write(self, data: bytes) -> int:
        return self.file.write(data)

    def flush(self) -> None:
        self.file.flush()

    def fileno(self) -> int:
        return self.file.fileno()

    def close(self) -> None:
        descriptor = self.file.fileno()
        self.file.close()
        try:
            os.fstat(descriptor)
        except OSError as error:
            self.released = error.errno == errno.EBADF
        raise OSError(errno.EIO, "injected close fault after a real close")


class CloseFault:
    """Wrap only the descriptor the canonical receipt writer opens, inside that writer only."""

    def __init__(self) -> None:
        self.files: list[CloseFaultFile] = []

    def write_receipt(self, *args: object, **options: object) -> None:
        real = filesystem.os.fdopen

        def fdopen(descriptor: int, *mode: object, **settings: object) -> CloseFaultFile:
            self.files.append(CloseFaultFile(real(descriptor, *mode, **settings)))
            return self.files[-1]

        with patch.object(filesystem.os, "fdopen", fdopen):
            filesystem.write_receipt(*args, **options)


class ReadinessCase(SemanticCase):
    candidate = _candidate()

    @classmethod
    def setUpClass(cls) -> None:
        super().setUpClass()
        s = cls.scenario = Scenario(cls.candidate)
        darwin, linux = s.claim(DARWIN), s.claim(LINUX)
        cls.ready = [s.register(), s.intent(), s.claim_file(darwin), s.claim_file(linux),
                     s.proof_file(darwin, s.zip(darwin)), s.proof_file(linux, s.zip(linux)),
                     *(s.install(platform, channel)[0] for platform in (DARWIN, LINUX) for channel in InstallChannel),
                     s.digest_file(EntryFamily.REVIEW, s.review(tree=cls.candidate.manifest.source.tree))]

    def setUp(self):
        super().setUp()
        self.front = PullRequestFront(self.github, pull=pull_request())
        transport = patch.object(github, "run_bounded_command", self.front)
        transport.start()
        self.addCleanup(transport.stop)
        harness = patch.object(check, "observe_harness", return_value=(HARNESS, HARNESS_SHA256, True))
        self.harness = harness.start()
        self.addCleanup(harness.stop)
        self.runs = 0

    def append_ready(self) -> str:
        """Append the ready history; return the head before any freeze."""
        return [self.confirmed(files) for files in self.ready][-1].commit

    def freeze(self) -> str:
        """Freeze the ready history through the canonical writer and policy; return F."""
        parent = self.append_ready()
        snapshot = self.loaded()
        source = self.candidate.manifest.source
        record = authorize_publication(
            candidate=self.candidate, observed=SourceObservation(head_commit=source.commit, tree=source.tree),
            scope=snapshot.history.intents[-1].scope, history=snapshot.history, review=snapshot.reviews[0],
            evidence_parent=parent)
        return self.confirmed({name(EntryFamily.PUBLICATION): encode_publication(record)}).commit

    def arguments(self, freeze: str, output: Path, *, omit: tuple[str, ...] = (), **changes: str) -> list[str]:
        values = {"pull-request": str(PULL_REQUEST), "candidate-id": self.candidate.candidate_id,
                  "freeze-commit": freeze, "store-repository": self.store.location.repository,
                  "store-branch": self.store.location.branch,
                  "store-genesis-commit": self.store.location.genesis_commit,
                  "store-marker-blob": self.store.location.marker_blob, **changes}
        return [part for key, value in values.items() if key not in omit
                for part in (f"--{key}", value)] + ["--output-dir", str(output)]

    def output(self) -> Path:
        self.runs += 1
        directory = self.work / f"readiness-{self.runs}"
        directory.mkdir()
        return directory

    def run_check(self, freeze: str, *, environment: dict[str, str] | None = None, output: Path | None = None,
                  omit: tuple[str, ...] = (), **changes: str) -> tuple[int, dict, dict, Path]:
        """Run the CLI once from a cleared call log; return exit code, printed summary, receipt and output."""
        output = output or self.output()
        self.github.calls.clear()
        self.front.posted.clear()
        stdout = io.StringIO()
        with patch.dict(check.os.environ, environment or ENVIRONMENT, clear=True), \
                contextlib.redirect_stdout(stdout):
            code = check.main(self.arguments(freeze, output, omit=omit, **changes))
        data = (output / "receipt.json").read_bytes()
        return code, json.loads(stdout.getvalue()), json.loads(data) if data else {}, output

    def assertRefusedBeforeRequests(self, code: int, receipt: dict, failure: str) -> None:
        self.assertEqual(code, check.EXIT_INCOMPLETE)
        self.assertEqual(receipt["setup_failure"]["code"], failure)
        self.assertEqual(self.github.calls, [])

    def assertNoStoreRead(self) -> None:
        self.assertEqual(self.requests("GET", "/git/ref/heads/"), [])

    def remote_methods(self) -> set[str]:
        return {method for method, _ in self.github.calls}


class ReadyTests(ReadinessCase):
    def test_open_and_merged_pull_requests_post_success_on_the_same_head_with_one_store_read(self):
        freeze = self.freeze()
        receipts = {}
        for state, merged in (("open", False), ("closed", True)):
            with self.subTest(state=state):
                self.front.pull = pull_request(state, merged)
                code, summary, receipt, _ = self.run_check(freeze)
                self.assertEqual((code, summary["result"], summary["receipt_finalized"]),
                                 (check.EXIT_READY, "ready", True))
                self.assertEqual(receipt["decision"], {"state": "success", "reason": "ready"})
                self.assertTrue(receipt["readiness"]["ready"])
                self.assertEqual(receipt["readiness"]["review_sha256"], receipt["selected_review_sha256"])
                self.assertEqual(receipt["target"], {"commit": fx.COMMIT, "tree": self.candidate.manifest.source.tree})
                self.assertEqual((receipt["store"]["head"], receipt["store"]["freeze_commit"]), (freeze, freeze))
                self.assertEqual(receipt["harness"], {"commit": HARNESS, "sha256": HARNESS_SHA256, "committed": True})
                self.assertEqual(receipt["store"]["candidate_build_harness_commit"], fx.HARNESS)
                self.assertEqual((receipt["pull_request"]["state"], receipt["pull_request"]["merged"]),
                                 (state, merged))
                self.assertIs(receipt["protection_unverified"], True)
                self.assertEqual([(commit, body["state"]) for commit, body in self.front.posted],
                                 [(fx.COMMIT, "pending"), (fx.COMMIT, "success")])
                for _, body in self.front.posted:
                    self.assertEqual(body, {"state": body["state"], "context": "Development readiness",
                                            "target_url": TARGET_URL})
                self.assertEqual([row["delivery"] for row in summary["status_deliveries"].values()],
                                 ["acknowledged", "acknowledged"])
                self.assertEqual(len(self.requests("GET", f"/pulls/{PULL_REQUEST}")), 1)
                self.assertEqual(len(self.requests("GET", "/git/ref/heads/")), 1, "H is read once")
                self.assertEqual(self.requests("GET", "/rules"), [])
                self.assertEqual(self.remote_methods(), {"GET", "POST"})
                self.assertEqual(len(self.requests("POST")), 2)
                self.assertTrue(all(STATUS_ROUTE in endpoint for endpoint in self.requests("POST")))
                self.assertEqual(self.requests("GET", "merge"), [])
                receipts[state] = receipt
        for receipt in receipts.values():
            for key in ("pull_request", "statuses", "commands"):
                receipt.pop(key)
        self.assertEqual(receipts["open"], receipts["closed"])

    def test_a_merged_pull_request_from_a_deleted_fork_is_valid(self):
        freeze = self.freeze()
        self.front.pull = pull_request("closed", True, head_repo=None)
        code, _, receipt, _ = self.run_check(freeze)
        self.assertEqual(code, check.EXIT_READY)
        self.assertIsNone(receipt["pull_request"]["head_repository"])

    def test_an_unrelated_head_is_a_canonical_source_changed_denial_on_that_head(self):
        freeze = self.freeze()
        other = "9" * 40
        self.put_source(other, fx.INSTALL_SH)
        self.front.pull = pull_request(head=other)
        code, _, receipt, _ = self.run_check(freeze)
        self.assertEqual(code, check.EXIT_DENIED)
        self.assertEqual(receipt["decision"], {"state": "failure", "reason": "readiness_denied"})
        self.assertIn({"code": "source_changed", "subject": other}, receipt["readiness"]["findings"])
        self.assertEqual([commit for commit, _ in self.front.posted], [other, other])


class SelectionTests(ReadinessCase):
    def assertDenied(self, freeze: str, reason: str) -> dict:
        code, _, receipt, _ = self.run_check(freeze)
        self.assertEqual((code, receipt["decision"]), (check.EXIT_DENIED, {"state": "failure", "reason": reason}))
        self.assertEqual([body["state"] for _, body in self.front.posted], ["pending", "failure"])
        self.assertNotIn("readiness", receipt)
        return receipt

    def test_an_absent_candidate_is_denied_without_inventing_a_head(self):
        receipt = self.assertDenied("1" * 40, "candidate_absent")
        self.assertEqual(receipt["store"], {"absent": True})

    def test_an_unfrozen_candidate_is_denied(self):
        head = self.append_ready()
        receipt = self.assertDenied(head, "candidate_unfrozen")
        self.assertEqual((receipt["store"]["head"], receipt["store"]["freeze_commit"]), (head, None))

    def test_only_the_exact_freeze_commit_selects_the_candidate(self):
        freeze = self.freeze()
        parent = self.github.commits[freeze].parents[0]
        for wrong in (parent, self.store.genesis, "7" * 40):
            with self.subTest(freeze=wrong):
                receipt = self.assertDenied(wrong, "freeze_commit_mismatch")
                self.assertEqual(receipt["store"]["freeze_commit"], freeze)

    def test_the_selected_review_is_the_one_the_freeze_record_names(self):
        freeze = self.freeze()
        snapshot = self.loaded()
        self.assertEqual(snapshot.freeze_commit, freeze)
        self.assertEqual(check.selected_review(snapshot).sha256, snapshot.history.publication.review_sha256)
        for reviews in ((), snapshot.reviews * 2):
            with self.subTest(reviews=len(reviews)), self.assertRaises(VerificationError) as caught:
                check.selected_review(replace(snapshot, reviews=reviews))
            self.assertEqual(caught.exception.code, "selected_review_invalid")

    def test_a_store_integrity_or_transport_failure_is_an_error_status(self):
        freeze = self.freeze()
        for needle in ("/git/blobs/", "/git/ref/heads/"):
            with self.subTest(needle=needle):
                self.github.faults.append(Fault("GET", needle, NOT_FOUND))
                code, _, receipt, _ = self.run_check(freeze)
                self.assertEqual((code, receipt["decision"]["state"]), (check.EXIT_INCOMPLETE, "error"))
                self.assertEqual(receipt["store_failure"]["code"], "github_request_failed")
                self.assertEqual([body["state"] for _, body in self.front.posted], ["pending", "error"])
                self.assertNotIn("store", receipt)


class PullRequestTests(ReadinessCase):
    def test_a_closed_unmerged_pull_request_is_a_definite_failure_before_any_store_read(self):
        self.front.pull = pull_request("closed", False)
        code, _, receipt, _ = self.run_check("1" * 40)
        self.assertEqual((code, receipt["decision"]["reason"]), (check.EXIT_DENIED, "pull_request_closed_unmerged"))
        self.assertEqual([body["state"] for _, body in self.front.posted], ["pending", "failure"])
        self.assertNoStoreRead()

    def test_a_malformed_or_contradictory_state_is_an_error_before_any_store_read(self):
        for state, merged in (("open", True), ("closed", "true"), ("open", None), ("merged", True), (None, False),
                              ("open", 0)):
            with self.subTest(state=state, merged=merged):
                self.front.pull = pull_request(state, merged)
                code, _, receipt, _ = self.run_check("1" * 40)
                self.assertEqual((code, receipt["decision"]), (check.EXIT_INCOMPLETE,
                                                                {"state": "error",
                                                                 "reason": "pull_request_state_invalid"}))
                self.assertEqual([body["state"] for _, body in self.front.posted], ["pending", "error"])
                self.assertNoStoreRead()
        missing = pull_request()
        del missing["merged"]
        self.front.pull = missing
        self.assertEqual(self.run_check("1" * 40)[2]["decision"]["state"], "error")

    def test_a_foreign_or_unverified_target_posts_nothing(self):
        foreign = {"ref": "main", "repo": {"full_name": "other/delegation-layer"}}
        cases = {
            "pull_request_identity_mismatch": [pull_request(number=PULL_REQUEST + 1), pull_request(base=foreign),
                                               pull_request(base={"ref": "release", "repo": foreign["repo"]}),
                                               pull_request(number=str(PULL_REQUEST))],
            "pull_request_head_invalid": [pull_request(head="A" * 40), pull_request(head=None)],
            "github_request_failed": [None, pull_request(head="8" * 40)],
        }
        for expected, replies in cases.items():
            for reply in replies:
                with self.subTest(expected=expected, reply=reply):
                    self.front.pull = reply
                    code, _, receipt, _ = self.run_check("1" * 40)
                    self.assertEqual((code, receipt["remote_failure"]["code"]), (check.EXIT_INCOMPLETE, expected))
                    self.assertEqual(self.front.posted, [])
                    self.assertNotIn("POST", self.remote_methods())
        self.front.pull = pull_request()
        self.github.commits[fx.COMMIT] = replace(self.github.commits[fx.COMMIT], tree="not-a-tree")
        code, _, receipt, _ = self.run_check("1" * 40)
        self.assertEqual((receipt["remote_failure"]["code"], self.front.posted), ("status_target_mismatch", []))


class AcknowledgmentTests(ReadinessCase):
    MISMATCHES = (
        ({"id": 0}, ["id"]), ({"id": "57089257227"}, ["id"]), ({"id": True}, ["id"]),
        ({"state": "success"}, ["state"]), ({"context": "Development readiness (other)"}, ["context"]),
        ({"target_url": "https://github.com/other/run"}, ["target_url"]),
        ({"url": f"https://api.github.invalid/repos/{fx.REPOSITORY}/statuses/{fx.COMMIT}"}, ["url"]),
        ({"url": f"https://api.github.com/repos/other/{fx.REPOSITORY}/statuses/{fx.COMMIT}"}, ["url"]),
        ({"url": f"https://api.github.com/repos/{fx.REPOSITORY}/statuses/{'9' * 40}"}, ["url"]),
    )

    def test_an_unacknowledged_pending_status_stops_with_no_further_post_or_store_read(self):
        for override, mismatches in (*self.MISMATCHES, (BoundedCommandResult(1, b"{}"), None)):
            with self.subTest(override=override):
                self.front.replies = [override]
                code, summary, receipt, _ = self.run_check("1" * 40)
                self.assertEqual(code, check.EXIT_INCOMPLETE)
                self.assertEqual(len(self.front.posted), 1)
                pending = receipt["statuses"]["pending"]
                self.assertEqual(pending["delivery"], "unknown")
                self.assertEqual(pending.get("ack_mismatches"), mismatches)
                self.assertNotIn("terminal", receipt["statuses"])
                self.assertNotIn("decision", receipt)
                self.assertEqual(summary["status_deliveries"]["pending"]["delivery"], "unknown")
                self.assertNoStoreRead()

    def test_an_unacknowledged_terminal_status_is_nonzero_and_keeps_the_pending_acknowledgment(self):
        freeze = self.freeze()
        for override in ({"id": -1}, BoundedCommandResult(1, b"{}")):
            with self.subTest(override=override):
                self.front.replies = [None, override]
                code, summary, receipt, _ = self.run_check(freeze)
                self.assertEqual((code, summary["observed_result"]), (check.EXIT_INCOMPLETE, "incomplete"))
                self.assertEqual(receipt["decision"]["state"], "success")
                self.assertEqual(receipt["statuses"]["pending"]["delivery"], "acknowledged")
                self.assertEqual(receipt["statuses"]["terminal"]["delivery"], "unknown")
                self.assertEqual(len(self.front.posted), 2, "no retry after an unknown terminal delivery")


class ReceiptTests(ReadinessCase):
    def test_a_finalization_fault_is_nonzero_and_keeps_the_acknowledged_remote_facts(self):
        freeze = self.freeze()
        directory_fault = VerificationError("receipt_directory_sync_failed", "write-receipt")
        faults = {"readiness_receipt_write_failed": [(filesystem.os, "fsync", OSError),
                                                     (filesystem.os, "replace", OSError)],
                  "receipt_directory_sync_failed": [(filesystem, "_fsync_directory", directory_fault)]}
        for expected, injections in faults.items():
            for owner, attribute, error in injections:
                with self.subTest(expected=expected, fault=attribute), \
                        patch.object(check, "write_receipt", scoped_fault(owner, attribute, error)):
                    code, summary, receipt, output = self.run_check(freeze)
                    self.assertEqual(code, check.EXIT_INCOMPLETE)
                    self.assertEqual((summary["observed_result"], summary["result"]), ("ready", "incomplete"))
                    self.assertEqual((summary["receipt_finalized"], summary["receipt_failure"]["code"]),
                                     (False, expected))
                    self.assertEqual([(row["state"], row["delivery"], type(row["id"]))
                                      for row in summary["status_deliveries"].values()],
                                     [("pending", "acknowledged", int), ("success", "acknowledged", int)])
                    if expected == "readiness_receipt_write_failed":
                        self.assertEqual(receipt, {}, "the reservation is never promoted to a record")
                    self.assertEqual(sorted(path.name for path in output.iterdir() if path.name != "receipt.json"),
                                     ["status-pending-body.json", "status-terminal-body.json"])

    def test_a_real_close_fault_of_the_final_receipt_is_nonzero_and_keeps_the_acknowledged_ids(self):
        freeze = self.freeze()
        fault = CloseFault()
        with patch.object(check, "write_receipt", fault.write_receipt):
            code, summary, receipt, output = self.run_check(freeze)
        self.assertEqual([file.released for file in fault.files], [True], "one descriptor, really closed")
        self.assertEqual(code, check.EXIT_INCOMPLETE)
        self.assertEqual((summary["observed_result"], summary["result"]), ("ready", "incomplete"))
        self.assertEqual((summary["receipt_finalized"], summary["receipt_failure"]),
                         (False, {"code": "readiness_receipt_write_failed", "stage": check.STAGE}))
        deliveries = list(summary["status_deliveries"].values())
        self.assertEqual([(row["state"], row["delivery"]) for row in deliveries],
                         [("pending", "acknowledged"), ("success", "acknowledged")])
        self.assertEqual([row["id"] for row in deliveries], [self.front.next_id - 1, self.front.next_id])
        self.assertEqual(len(self.requests("POST")), 2, "no retry or further status after the fault")
        self.assertEqual(receipt, {}, "the reservation is never promoted to a record")
        self.assertEqual(sorted(path.name for path in output.iterdir()),
                         ["receipt.json", "status-pending-body.json", "status-terminal-body.json"])

    def test_a_reservation_close_fault_refuses_before_any_request(self):
        reserved: list[int] = []
        real_create, real_close = check.create_new_file, os.close

        def create(*args: object, **options: object) -> int:
            reserved.append(real_create(*args, **options))
            return reserved[-1]

        def close(descriptor: int) -> None:
            real_close(descriptor)
            if descriptor in reserved:
                raise OSError(errno.EIO, "injected close fault after a real close")

        output = self.output()
        stdout = io.StringIO()
        with patch.dict(check.os.environ, ENVIRONMENT, clear=True), contextlib.redirect_stdout(stdout), \
                patch.object(check, "create_new_file", create), patch.object(check.os, "close", close):
            code = check.main(self.arguments("1" * 40, output))
        self.assertEqual(len(reserved), 1)
        self.assertEqual((code, json.loads(stdout.getvalue())),
                         (check.EXIT_INCOMPLETE, {"result": "receipt_unavailable", "exit_code": check.EXIT_INCOMPLETE,
                                                  "code": "readiness_receipt_reservation_failed",
                                                  "receipt_finalized": False}))
        self.assertEqual((output / "receipt.json").read_bytes(), b"")
        self.assertEqual(self.github.calls, [])
        self.harness.assert_not_called()

    def test_an_existing_receipt_is_refused_before_any_request_and_left_untouched(self):
        output = self.output()
        (output / "receipt.json").write_bytes(b"prior")
        stdout = io.StringIO()
        with patch.dict(check.os.environ, ENVIRONMENT, clear=True), contextlib.redirect_stdout(stdout):
            code = check.main(self.arguments("1" * 40, output))
        self.assertEqual((code, json.loads(stdout.getvalue())["result"]), (check.EXIT_INCOMPLETE, "receipt_unavailable"))
        self.assertEqual((output / "receipt.json").read_bytes(), b"prior")
        self.assertEqual(self.github.calls, [])
        self.harness.assert_not_called()


class TrustTests(ReadinessCase):
    def test_every_run_context_fact_must_match_a_main_dispatch_of_this_workflow(self):
        changes = {"GITHUB_REPOSITORY": "other/delegation-layer", "GITHUB_EVENT_NAME": "push",
                   "GITHUB_REF": "refs/heads/task", "GITHUB_RUN_ID": "0123", "GITHUB_RUN_ATTEMPT": "0",
                   "GITHUB_SHA": "D" * 40, "GITHUB_SERVER_URL": "https://github.invalid",
                   "GITHUB_API_URL": "https://api.github.invalid",
                   "GITHUB_WORKFLOW_REF": f"{fx.REPOSITORY}/.github/workflows/ci.yml@refs/heads/main"}
        for key, value in changes.items():
            with self.subTest(key=key):
                code, _, receipt, _ = self.run_check("1" * 40, environment={**ENVIRONMENT, key: value})
                self.assertRefusedBeforeRequests(code, receipt, "run_context_mismatch")
                self.assertFalse(all(receipt["context_checks"].values()))
        self.harness.assert_not_called()

    def test_invalid_inputs_refuse_before_any_request(self):
        cases = {"pull-request": ("0", "pull_request_number_invalid"),
                 "candidate-id": ("A" * 64, "store_candidate_id_invalid"),
                 "freeze-commit": ("abc", "freeze_commit_invalid"),
                 "store-repository": ("not a repository", "store_location_invalid"),
                 "store-branch": ("", "store_location_invalid"),
                 "store-genesis-commit": ("g" * 40, "store_location_invalid"),
                 "store-marker-blob": ("", "store_location_invalid")}
        for key, (value, expected) in cases.items():
            with self.subTest(key=key):
                code, _, receipt, _ = self.run_check("1" * 40, **{key: value})
                self.assertRefusedBeforeRequests(code, receipt, expected)

    def test_only_the_maintainer_store_option_names_parse(self):
        output = self.output()
        for short, full in (("--store-genesis", "--store-genesis-commit"), ("--store-marker", "--store-marker-blob")):
            with self.subTest(option=short), contextlib.redirect_stderr(io.StringIO()), \
                    self.assertRaises(SystemExit) as caught:
                check.main([short if part == full else part for part in self.arguments("1" * 40, output)])
            self.assertEqual(caught.exception.code, 2)
        self.assertEqual(list(output.iterdir()), [])
        self.assertEqual(self.github.calls, [])

    def test_the_executing_harness_must_be_the_committed_run_commit(self):
        for observed, expected in (((HARNESS, HARNESS_SHA256, False), "harness_uncommitted"),
                                   (("c" * 40, HARNESS_SHA256, True), "harness_commit_mismatch")):
            with self.subTest(expected=expected):
                self.harness.return_value = observed
                code, _, receipt, _ = self.run_check("1" * 40)
                self.assertRefusedBeforeRequests(code, receipt, expected)
                self.assertEqual(receipt["harness"]["commit"], observed[0])

    def test_a_missing_workflow_token_refuses_before_any_request(self):
        environment = {key: value for key, value in ENVIRONMENT.items() if key != "GH_TOKEN"}
        code, _, receipt, _ = self.run_check("1" * 40, environment=environment)
        self.assertRefusedBeforeRequests(code, receipt, "github_auth_unavailable")

    def test_the_harness_is_this_checkout_and_its_closure_includes_the_entry_point(self):
        self.run_check("1" * 40)
        options = self.harness.call_args.kwargs
        root = Path(__file__).resolve().parent.parent
        self.assertEqual((options["checkout"], options["seeds"]), (root, check.HARNESS_SEEDS))
        paths = {row["path"] for row in fingerprint_sources(root, check.HARNESS_SEEDS)["files"]}
        self.assertLessEqual({*check.HARNESS_SEEDS, "scripts/delegate_acceptance/native_release/contract.py",
                              "scripts/delegate_acceptance/release_verification/candidate_readiness.py",
                              "scripts/delegate_acceptance/release_verification/candidate_store.py"}, paths)


class StoreSelectionTests(ReadinessCase):
    def test_omitting_every_store_option_selects_the_reviewed_production_store(self):
        _, _, omitted, _ = self.run_check("1" * 40, omit=STORE_KEYS)
        self.assertEqual(omitted["request"]["store"], {key.removeprefix("store-").replace("-", "_"): value
                                                       for key, value in PRODUCTION_PINS.items()})
        self.assertEqual(self.requests("GET", "/git/ref/heads/"),
                         [f"repos/{fx.REPOSITORY}/git/ref/heads/development-proof/evidence"])
        _, _, explicit, _ = self.run_check("1" * 40, **PRODUCTION_PINS)
        self.assertEqual(json.dumps(explicit["request"], sort_keys=True).encode(),
                         json.dumps(omitted["request"], sort_keys=True).encode())

    def test_a_complete_override_is_used_unchanged(self):
        _, _, receipt, _ = self.run_check("1" * 40)
        location = self.store.location
        self.assertEqual(receipt["request"]["store"], {
            "repository": location.repository, "branch": location.branch,
            "genesis_commit": location.genesis_commit, "marker_blob": location.marker_blob})
        self.assertNotEqual(location.branch, PRODUCTION_PINS["store-branch"])

    def test_every_partial_override_is_a_usage_error_before_the_receipt_or_any_request(self):
        for mask in range(1, 2 ** len(STORE_KEYS) - 1):
            absent = tuple(key for index, key in enumerate(STORE_KEYS) if not mask >> index & 1)
            supplied = {key: "" for key in STORE_KEYS if key not in absent}
            for changes in ({}, supplied):
                output = self.output()
                stderr = io.StringIO()
                with self.subTest(absent=absent, empty=bool(changes)), contextlib.redirect_stderr(stderr), \
                        self.assertRaises(SystemExit) as caught:
                    check.main(self.arguments("1" * 40, output, omit=absent, **changes))
                self.assertEqual(caught.exception.code, 2)
                self.assertIn(f"missing: {' '.join(f'--{key}' for key in absent)}\n", stderr.getvalue())
                self.assertEqual(list(output.iterdir()), [])
                self.assertEqual(self.github.calls, [])
        self.harness.assert_not_called()

    def test_a_direct_partial_request_refuses_instead_of_ignoring_supplied_values(self):
        args = check._parser().parse_args(["--pull-request", "1", "--candidate-id", "a" * 64,
                                           "--freeze-commit", "1" * 40, "--store-branch", "other",
                                           "--output-dir", str(self.output())])
        with self.assertRaises(VerificationError) as caught:
            check.parse_request(args)
        self.assertEqual((caught.exception.code, caught.exception.stage), ("store_options_incomplete", check.STAGE))


class CommittedHarnessTests(unittest.TestCase):
    """The CLI's actual harness binding over an owned Git fixture holding this module's real closure.

    The fixture bytes are test inputs, never evidence about the real checkout. Only the
    pull request read can follow the binding; it answers 404 so no status is posted. Every
    harness Git child runs through the tree's observer at the canonical command port, so its
    original failure reaches the tree before the command layer or the CLI converts it.
    """

    def setUp(self) -> None:
        self.tree = FixtureTree.open(prefix="readiness-harness", label="readiness harness binding")
        self.addCleanup(self.tree.dispose)
        self.repository = SourceRepository(self.tree, "checkout")
        root = Path(__file__).resolve().parent.parent
        files = fingerprint_sources(root, check.HARNESS_SEEDS)["files"]
        self.head = self.repository.commit({row["path"]: (root / row["path"]).read_bytes() for row in files},
                                           "trusted main harness")
        self.requests: list[list[str]] = []
        self.fault: BoundedCommandError | None = None
        self.launches = 0
        self.launch = commands.run_bounded_command
        for target in (patch.object(check, "checkout_root", return_value=self.repository.path),
                       patch.object(commands, "run_bounded_command", self.observed_child),
                       patch.object(github, "run_bounded_command", self.transport)):
            target.start()
            self.addCleanup(target.stop)

    def observed_child(self, *args: object, **options: object) -> BoundedCommandResult:
        """The real canonical launch, or the injected fault in its place, observed by the tree."""
        return self.tree.observe_operation(lambda: self.child(*args, **options))

    def child(self, *args: object, **options: object) -> BoundedCommandResult:
        if self.fault is not None:
            raise self.fault
        self.launches += 1
        return self.launch(*args, **options)

    def transport(self, argv: list[str], _environment: dict[str, str], **_options: object) -> BoundedCommandResult:
        self.requests.append(argv)
        return NOT_FOUND

    def run_check(self, commit: str) -> tuple[int, dict]:
        output = self.tree.root / f"output-{len(list(self.tree.root.iterdir()))}"
        output.mkdir()
        environment = {**ENVIRONMENT, "GITHUB_SHA": commit, "PATH": os.environ["PATH"],
                       **({"HOME": os.environ["HOME"]} if "HOME" in os.environ else {})}
        arguments = ["--pull-request", str(PULL_REQUEST), "--candidate-id", "a" * 64, "--freeze-commit", "1" * 40,
                     "--store-repository", fx.REPOSITORY, "--store-branch", "development-evidence",
                     "--store-genesis-commit", "2" * 40, "--store-marker-blob", "3" * 40, "--output-dir", str(output)]
        with patch.dict(check.os.environ, environment, clear=True), contextlib.redirect_stdout(io.StringIO()):
            code = check.main(arguments)
        return code, json.loads((output / "receipt.json").read_bytes())

    def test_the_committed_run_commit_binds_and_only_then_permits_a_request(self):
        code, receipt = self.run_check(self.head)
        fingerprint = fingerprint_sources(self.repository.path, check.HARNESS_SEEDS)["sha256"]
        self.assertEqual(receipt["harness"], {"commit": self.head, "sha256": fingerprint, "committed": True})
        self.assertEqual((code, receipt["remote_failure"]["code"]), (check.EXIT_INCOMPLETE, "github_request_failed"))
        self.assertEqual([argv[8] for argv in self.requests], [f"repos/{fx.REPOSITORY}/pulls/{PULL_REQUEST}"])

    def test_substituted_bytes_or_another_claimed_commit_refuse_before_any_request(self):
        module = self.repository.path / check.HARNESS_SEEDS[1]
        committed = module.read_bytes()
        module.write_bytes(committed + b"# substituted after commit\n")
        code, receipt = self.run_check(self.head)
        self.assertEqual((code, receipt["setup_failure"]["code"]), (check.EXIT_INCOMPLETE, "harness_uncommitted"))
        self.assertEqual(receipt["harness"]["committed"], False)
        module.write_bytes(committed)
        code, receipt = self.run_check("4" * 40)
        self.assertEqual((code, receipt["setup_failure"]["code"]), (check.EXIT_INCOMPLETE, "harness_commit_mismatch"))
        self.assertEqual(receipt["harness"]["commit"], self.head)
        self.assertEqual(self.requests, [])

    def test_the_production_store_pin_bytes_are_part_of_the_harness_fingerprint(self):
        contract = "scripts/delegate_acceptance/native_release/contract.py"
        original = fingerprint_sources(self.repository.path, check.HARNESS_SEEDS)
        self.assertIn(contract, {row["path"] for row in original["files"]})
        module = self.repository.path / contract
        committed = module.read_bytes()
        pin = PRODUCTION_PINS["store-marker-blob"].encode()
        self.assertEqual(committed.count(pin), 1)
        module.write_bytes(committed.replace(pin, b"9" * 40))
        self.assertNotEqual(fingerprint_sources(self.repository.path, check.HARNESS_SEEDS)["sha256"],
                            original["sha256"])
        code, receipt = self.run_check(self.head)
        self.assertEqual((code, receipt["setup_failure"]["code"]), (check.EXIT_INCOMPLETE, "harness_uncommitted"))
        self.assertEqual(self.requests, [])

    def test_an_unresolved_harness_child_retains_the_tree_until_independent_no_child_proof(self):
        self.fault = BoundedCommandError(CommandFailure.TIMEOUT)
        code, receipt = self.run_check(self.head)
        self.assertIs(self.tree.unresolved, self.fault, "the original error reached the tree before conversion")
        self.assertEqual((code, receipt["setup_failure"]["code"]), (check.EXIT_INCOMPLETE, "command_timeout"))
        self.assertNotIn("harness", receipt)
        self.assertEqual(self.requests, [])
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertFalse(self.tree.dispose())
        self.assertTrue((self.repository.path / ".git").is_dir(), "retained as evidence")
        # Independent proof: the fault replaced the only harness launch, so no child exists to write.
        self.assertEqual(self.launches, 0)
        self.tree.remove_retained()
        self.assertFalse(self.tree.root.exists())


if __name__ == "__main__":
    unittest.main()
