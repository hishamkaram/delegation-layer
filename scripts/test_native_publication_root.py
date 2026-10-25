"""Root publish-candidate and resume-publication through the public CLI, real receipts and canonical validators.

Receipts are created, locked and loaded by the real storage owners inside one
test-owned ``FixtureTree``. GitHub is a recording fake behind the canonical
client port; the shared preflight, merge, required-check and harness observers
are replaced by fixed observations, since their own suites own those rules. No
child process, network or destination write runs.
"""

from __future__ import annotations

from collections.abc import Callable
import copy
import errno
import hashlib
import io
import json
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
import zipfile

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.native_release import cli, publication, publication_observation, storage
from scripts.delegate_acceptance.native_release.contract import (
    MAX_RECEIPT, PRODUCTION_STORE, PUBLICATION_STAGES, PublicationRequest, ReleaseRequest,
)
from scripts.delegate_acceptance.native_release.publication_contract import (
    ReportBinding, parse_dispatch_inputs, publication_report,
)
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_publication_contract import BUILD, TREE
from scripts.test_native_publication_contract import PROMOTION, delivery, failed, gate


REPOSITORY = PRODUCTION_STORE.repository
HARNESS = "9" * 40
RUN_ID = 7001
REFERENCE = PROMOTION.reference
ARGV = ["publish-candidate", "--candidate-id", REFERENCE.candidate_id, "--freeze-commit", REFERENCE.freeze_commit,
        "--merged-commit", REFERENCE.merged_commit, "--readiness-run-id", "901", "--readiness-run-attempt", "2",
        "--resume-published", "false"]
DISPATCHES = f"repos/{REPOSITORY}/actions/workflows/release.yml/dispatches"
RUNS = f"repos/{REPOSITORY}/actions/workflows/release.yml/runs?"
ARTIFACTS = f"repos/{REPOSITORY}/actions/runs/{RUN_ID}/artifacts?"
RUN = f"repos/{REPOSITORY}/actions/runs/{RUN_ID}"
POLL_SECONDS, OBSERVE_TIMEOUT = publication_observation.POLL_SECONDS, publication_observation.OBSERVE_TIMEOUT
# The acknowledged run before its title is exact: same ID and every other binding, not completed.
PENDING_TITLE: dict[str, object] = {"display_title": "Release", "status": "queued", "conclusion": None}
# Every attribute ``PublicationCase`` patches, with its unpatched value.
PATCHED = ((storage, "state_base"), (cli, "state_base"), (cli, "github_client"), (publication, "preflight"),
           (publication, "observe_publication_source"), (publication, "observe_required_checks"),
           (publication, "observe_harness"), (publication, "git"), (operations.subprocess, "Popen"))
ORIGINALS = tuple(getattr(owner, name) for owner, name in PATCHED)
# Every fixture archive member's timestamp: the earliest the ZIP format represents.
ARCHIVE_TIME = (1980, 1, 1, 0, 0, 0)


def context(*, private: object = False) -> SimpleNamespace:
    snapshot = SimpleNamespace(candidate=PROMOTION.candidate, history=SimpleNamespace(publication=PROMOTION.freeze),
                               head="e" * 40)
    return SimpleNamespace(snapshot=snapshot, pull_request=17, target=SimpleNamespace(commit=BUILD, tree=TREE),
                           harness_commit="8" * 40, artifact=SimpleNamespace(sha256="c" * 64),
                           repository_private=private)


def archive(members: dict[str, bytes], compression: int = zipfile.ZIP_DEFLATED) -> bytes:
    """Equal members always give equal bytes: a fake listing and its later download must hash the same."""
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w", compression) as bundle:
        for name, data in members.items():
            # A name alone would stamp the current time; keep its regular-file mode and the requested compression.
            member = zipfile.ZipInfo(name, date_time=ARCHIVE_TIME)
            member.external_attr = 0o600 << 16
            bundle.writestr(member, data, compress_type=compression)
    return buffer.getvalue()


class FakeGitHub:
    """Records every request; the hosted report is built from the dispatch fields the POST carried."""

    def __init__(self, case: PublicationCase):
        self.case = case
        self.receipt: dict[str, object] = {"commands": []}
        # Only a fixed search path; the inherited environment is never copied.
        self.environment: dict[str, str] = {"PATH": "/usr/bin:/bin"}
        self.calls: list[tuple[str, str]] = []
        self.ack: object = "exact"
        self.run: dict[str, object] = {"run_attempt": 1, "status": "completed", "conclusion": "success"}
        # Per-read changes to the acknowledged run's row, consumed in order before ``run`` alone applies.
        self.run_reads: list[dict[str, object] | Exception] = []
        self.listed: int = 1
        self.report: str = "success"
        self.members: dict[str, bytes] | None = None
        # Built only when requested, after the dispatch that binds the report exists.
        self.archive_override: Callable[[], bytes] | None = None
        self.binding_changes: dict[str, object] = {}
        self.first_mutation_started = True
        self.artifact_pages: Callable[[], list[dict[str, object]]] | None = None

    def dispatch(self):
        return parse_dispatch_inputs({key[7:-1]: value for key, value in self.case.fields.items()
                                      if key.startswith("inputs[")})

    def run_row(self, run_id: int = RUN_ID) -> dict[str, object]:
        return {"id": run_id, "display_title": self.dispatch().title, "head_sha": HARNESS, "head_branch": "main",
                "event": "workflow_dispatch", "path": ".github/workflows/release.yml",
                "html_url": f"https://github.com/{REPOSITORY}/actions/runs/{run_id}", **self.run}

    def gate(self) -> dict[str, object]:
        binding = ReportBinding.of(self.dispatch(), run_id=RUN_ID, run_attempt=1)
        if self.binding_changes:
            binding = ReportBinding(**{**binding.document(), **self.binding_changes})
        if self.report == "success":
            return gate(publication_report(binding, first_mutation_started=True, promotion=PROMOTION,
                                           delivery=delivery()))
        return failed(publication_report(binding, first_mutation_started=self.first_mutation_started))

    def archive(self) -> bytes:
        if self.archive_override is not None:
            return self.archive_override()
        members = self.members or {"receipt.json": json.dumps(self.gate()).encode("utf-8")}
        return archive(members)

    def json(self, endpoint: str, *, method: str = "GET", fields: dict[str, str] | None = None,
             limit: int | None = None) -> object:
        self.calls.append((method, endpoint))
        if endpoint == f"repos/{REPOSITORY}/branches/main":
            return {"protected": True, "commit": {"sha": HARNESS}}
        if endpoint == DISPATCHES:
            self.case.assert_intent_durable()
            self.case.fields = dict(fields or {})
            if isinstance(self.ack, Exception):
                raise self.ack
            if self.ack == "malformed":
                return {"workflow_run_id": RUN_ID, "html_url": "https://example.invalid/run", "run_url": "x"}
            return {"workflow_run_id": RUN_ID, "html_url": f"https://github.com/{REPOSITORY}/actions/runs/{RUN_ID}",
                    "run_url": f"https://api.github.com/repos/{REPOSITORY}/actions/runs/{RUN_ID}"}
        if endpoint.startswith(RUNS):
            rows = [self.run_row(RUN_ID + offset) for offset in range(self.listed)]
            return {"total_count": len(rows), "workflow_runs": rows}
        if endpoint == f"repos/{REPOSITORY}/actions/runs/{RUN_ID}":
            change = self.run_reads.pop(0) if self.run_reads else {}
            if isinstance(change, Exception):
                raise change
            return {**self.run_row(), **change}
        if endpoint.startswith(ARTIFACTS):
            if self.artifact_pages is not None:
                return self.artifact_pages()[int(endpoint.rsplit("&page=", 1)[1]) - 1]
            if self.report == "missing":
                return {"total_count": 0, "artifacts": []}
            return {"total_count": 1, "artifacts": [self.artifact_row()]}
        raise AssertionError(f"unexpected request {method} {endpoint}")

    def artifact_row(self, identity: int = 99, name: str = f"candidate-publication-{RUN_ID}-1") -> dict[str, object]:
        data = self.archive()
        return {"id": identity, "name": name, "size_in_bytes": len(data),
                "digest": "sha256:" + hashlib.sha256(data).hexdigest(), "expired": False,
                "workflow_run": {"id": RUN_ID, "head_sha": HARNESS}}

    def artifact_archive(self, repo: str, artifact_id: int, *, limit: int) -> bytes:
        self.calls.append(("GET", f"artifact:{artifact_id}"))
        return self.archive()

    def posts(self) -> int:
        return sum(1 for method, _ in self.calls if method == "POST")


class FakeClock:
    """The observer's ``time`` alias only: ``sleep`` advances ``monotonic`` and is recorded, so nothing waits."""

    def __init__(self) -> None:
        self.now = 0.0
        self.sleeps: list[float] = []

    def monotonic(self) -> float:
        return self.now

    def sleep(self, seconds: float) -> None:
        self.sleeps.append(seconds)
        self.now += seconds


class TickingZipClock:
    """``zipfile``'s ``time`` alias only: each read moves past the format's two-second resolution, so nothing waits."""

    def __init__(self) -> None:
        self.reads = 0

    def time(self) -> float:
        self.reads += 1
        return float(2 * self.reads)

    def localtime(self, seconds: float) -> tuple[int, ...]:
        return (2026, 1, 1, 0, 0, int(seconds) % 60, 0, 0, 0)


class PublicationCase(unittest.TestCase):
    def setUp(self):
        self.tree = FixtureTree.open(prefix="native-publication-root", label="publication root fixture")
        self.addCleanup(self.tree.dispose)
        self.state = self.tree.root / "state"
        self.fields: dict[str, str] = {}
        self.client = FakeGitHub(self)
        self.context = context()
        self.harness = (HARNESS, "f" * 64, True)
        self.status_output = b""
        self.preflight = Mock(side_effect=lambda *args, **kwargs: self.context)
        patches = (
            patch.object(storage, "state_base", side_effect=lambda checkout: self.state),
            patch.object(cli, "state_base", side_effect=lambda checkout: self.state),
            patch.object(cli, "github_client", side_effect=lambda: self.client),
            patch.object(publication, "preflight", self.preflight),
            patch.object(publication, "observe_publication_source", side_effect=lambda *a, **k: PROMOTION.source),
            patch.object(publication, "observe_required_checks",
                         side_effect=lambda *a, **k: SimpleNamespace(checks=(SimpleNamespace(context="ci"),))),
            patch.object(publication, "observe_harness", side_effect=lambda *a, **k: self.harness),
            patch.object(publication, "git", side_effect=lambda *a, **k: self.status_output),
            patch.object(operations.subprocess, "Popen", Mock(side_effect=AssertionError("child launched"))),
        )
        for item in patches:
            item.start()
            self.addCleanup(item.stop)

    def reset(self) -> None:
        """Dispose this case's tree and stop its patches through the registered cleanups, then set up afresh."""
        self.assertTrue(self.doCleanups())
        self.setUp()

    def receipt_path(self) -> Path:
        (path,) = self.state.glob("publication-*/receipt.json")
        return path

    def assert_intent_durable(self) -> None:
        stage = storage.load_receipt(self.receipt_path()).stage("dispatch")
        self.assertEqual((stage.state, stage.intent["workflow"], stage.intent["ref"]),
                         ("intended", "release.yml", "main"))

    def run_cli(self, argv: list[str]) -> tuple[int, dict[str, object]]:
        output = io.StringIO()
        with patch("sys.stdout", output), patch("sys.stderr", io.StringIO()):
            code = cli.main(argv)
        return code, json.loads(output.getvalue())

    def publish(self) -> tuple[int, dict[str, object]]:
        return self.run_cli(ARGV)

    def resume(self) -> tuple[int, dict[str, object]]:
        return self.run_cli(["resume-publication", "--receipt", str(self.receipt_path())])

    def assertOutcome(self, result: tuple[int, dict[str, object]], code: int, status: str, reason: str) -> None:
        self.assertEqual((result[0], result[1]["status"], result[1]["reason"]), (code, status, reason))
        stored = storage.load_receipt(self.receipt_path())
        self.assertEqual((stored.status, stored.reason), (status, reason))


class PublishTests(PublicationCase):
    def test_a_verified_hosted_delivery_and_finalized_receipt_publish_with_one_dispatch(self):
        code, report = self.publish()
        self.assertOutcome((code, report), 0, "PUBLISHED", "hosted_delivery_verified_and_receipt_finalized")
        self.assertEqual(set(report["stages"]), set(PUBLICATION_STAGES))
        self.assertTrue(all(row["state"] == "complete" for row in report["stages"].values()))
        self.assertEqual(self.client.posts(), 1)
        self.preflight.assert_called_once()
        self.assertIs(self.preflight.call_args.kwargs["verify_store_protection"], True)
        request = self.preflight.call_args.args[1]
        self.assertEqual(request, PublicationRequest(store=PRODUCTION_STORE, reference=REFERENCE,
                                                     readiness_run_id=901, readiness_run_attempt=2,
                                                     resume_published=False))
        dispatch = self.client.dispatch()
        stored = storage.load_receipt(self.receipt_path())
        self.assertEqual(self.fields, {"ref": "main", **{f"inputs[{k}]": v for k, v in dispatch.inputs().items()}})
        self.assertEqual(stored.stage("dispatch").intent, {"workflow": "release.yml", "ref": "main",
                                                           "title": dispatch.title, "inputs": dispatch.inputs()})
        self.assertEqual((dispatch.harness, dispatch.invocation_id), (HARNESS, stored.invocation.invocation_id))
        self.assertEqual(stored.stage("preflight").result["readiness_harness"], "8" * 40)
        self.assertEqual(stored.stage("verify").result["delivery"], gate(publication_report(
            ReportBinding.of(dispatch, run_id=RUN_ID, run_attempt=1), first_mutation_started=True,
            promotion=PROMOTION, delivery=delivery()))["publication"]["delivery"])
        self.assertEqual(stored.stage("cleanup").result, {"root_scratch": []})

    def test_definite_refusals_before_any_dispatch_are_blocked_and_send_nothing(self):
        cases = {
            "private": (lambda: setattr(self, "context", context(private=True)), "publication_repository_private"),
            "unknown visibility": (lambda: setattr(self, "context", context(private=None)),
                                   "repository_visibility_unknown"),
            "uncommitted": (lambda: setattr(self, "harness", (HARNESS, "f" * 64, False)),
                            "publication_requires_clean_current_main_harness"),
            "dirty": (lambda: setattr(self, "status_output", b"?? x\n"),
                      "publication_requires_clean_current_main_harness"),
            "not main": (lambda: setattr(self, "harness", ("7" * 40, "f" * 64, True)),
                         "publication_requires_clean_current_main_harness"),
            "not ready": (lambda: setattr(self.preflight, "side_effect",
                                          VerificationError("publication_readiness_denied", "x")),
                          "publication_readiness_denied"),
        }
        for label, (arrange, reason) in cases.items():
            with self.subTest(label):
                self.reset()
                arrange()
                self.assertOutcome(self.publish(), 2, "BLOCKED", reason)
                self.assertEqual(self.client.posts(), 0)
                self.assertEqual(storage.load_receipt(self.receipt_path()).stage("dispatch").state, "pending")

    def test_explicit_fields_are_validated_before_any_receipt(self):
        for index, value in ((8, "01"), (8, "+5"), (10, "0"), (8, "1" * 19), (2, "XYZ")):
            argv = list(ARGV)
            argv[index] = value
            with self.subTest(argv=argv):
                code, report = self.run_cli(argv)
                self.assertEqual((code, report["status"]), (2, "BLOCKED"))
                self.assertEqual(list(self.state.glob("publication-*")), [])

    def test_an_unknown_acknowledgement_stops_incomplete_without_retry_or_observation(self):
        for ack in (VerificationError("github_api_failed", "github-api"), "malformed"):
            with self.subTest(ack=ack):
                self.reset()
                self.client.ack = ack
                self.assertOutcome(self.publish(), 3, "INCOMPLETE",
                                   "publication_dispatch_unacknowledged_resume_observes_only")
                self.assertEqual(self.client.posts(), 1)
                self.assertFalse([call for call in self.client.calls if "/runs" in call[1]])

    def test_a_rerun_attempt_is_never_substituted(self):
        self.client.run["run_attempt"] = 2
        self.assertOutcome(self.publish(), 3, "INCOMPLETE", "publication_run_attempt_substituted")

    def test_a_missing_report_is_incomplete_and_never_falls_back_to_the_run_conclusion(self):
        self.client.report = "missing"
        self.assertOutcome(self.publish(), 3, "INCOMPLETE", "publication_report_missing")
        self.assertEqual(storage.load_receipt(self.receipt_path()).stage("cleanup").state, "pending")

    def test_a_hosted_failure_is_never_success_and_keeps_its_fields(self):
        for started, code, status in ((False, 2, "BLOCKED"), (True, 3, "INCOMPLETE")):
            with self.subTest(started=started):
                self.reset()
                self.client.report, self.client.first_mutation_started = "failed", started
                self.client.run["conclusion"] = "failure"
                self.assertOutcome(self.publish(), code, status,
                                   "hosted_publication_failed:deliver-candidate-npm:candidate_npm_conflict")
                verdict = storage.load_receipt(self.receipt_path()).stage("verify").result
                self.assertEqual(verdict["failure"], {"code": "candidate_npm_conflict",
                                                      "stage": "deliver-candidate-npm",
                                                      "first_mutation_started": started,
                                                      "delivery_confirmed": False})

    def test_a_success_report_needs_exact_bindings_a_successful_run_and_a_strict_archive(self):
        cases = {
            "other attempt": (lambda: self.client.binding_changes.update(run_attempt=2),
                              "publication_report_binding_mismatch:mismatch=run_attempt"),
            "other harness": (lambda: self.client.binding_changes.update(harness="7" * 40),
                              "publication_report_binding_mismatch:mismatch=harness"),
            "failed run": (lambda: self.client.run.update(conclusion="failure"),
                           "publication_run_conclusion_conflict"),
            "two members": (lambda: setattr(self.client, "members", {"receipt.json": b"{}", "extra.json": b"{}"}),
                            "publication_report_zip_inventory_mismatch"),
            "nested": (lambda: setattr(self.client, "members", {"out/receipt.json": b"{}"}),
                       "publication_report_zip_inventory_mismatch"),
            "not zip": (lambda: setattr(self.client, "archive_override", lambda: b"not a zip"),
                        "publication_report_zip_invalid"),
            "not json": (lambda: setattr(self.client, "members", {"receipt.json": b"{"}),
                         "publication_report_json_invalid"),
            "crc": (lambda: setattr(self.client, "archive_override", self.corrupted),
                    "publication_report_zip_invalid"),
            "encrypted": (lambda: setattr(self.client, "archive_override", self.encrypted),
                          "publication_report_zip_member_invalid"),
            "oversized plaintext": (lambda: setattr(self.client, "members",
                                                    {"receipt.json": b" " * (MAX_RECEIPT + 1)}),
                                    "publication_report_too_large"),
        }
        for label, (arrange, reason) in cases.items():
            with self.subTest(label):
                self.reset()
                arrange()
                self.assertOutcome(self.publish(), 3, "INCOMPLETE", reason)
                self.assertEqual(self.client.posts(), 1)

    def corrupted(self) -> bytes:
        receipt = json.dumps(self.client.gate()).encode("utf-8")
        return archive({"receipt.json": receipt}, zipfile.ZIP_STORED).replace(receipt[:64], bytes(64), 1)

    def encrypted(self) -> bytes:
        data = bytearray(archive({"receipt.json": json.dumps(self.client.gate()).encode("utf-8")},
                                 zipfile.ZIP_STORED))
        data[data.index(b"PK\x01\x02") + 8] |= 0x01  # the central directory's general-purpose flags
        return bytes(data)

    def test_fixture_archives_ignore_the_clock_and_keep_each_compression(self):
        members = {"receipt.json": b'{"report": true}' * 64}
        for compression in (zipfile.ZIP_DEFLATED, zipfile.ZIP_STORED):
            with self.subTest(compression=compression):
                clock = TickingZipClock()
                with patch.object(zipfile, "time", clock):
                    first, second = archive(members, compression), archive(members, compression)
                self.assertEqual(first, second)
                with zipfile.ZipFile(io.BytesIO(first)) as bundle:
                    self.assertIsNone(bundle.testzip())
                    self.assertEqual([info.compress_type for info in bundle.infolist()], [compression])
                    self.assertEqual({name: bundle.read(name) for name in bundle.namelist()}, members)

    def test_an_oversized_report_is_refused_for_its_size_when_the_clock_ticks_between_listing_and_download(self):
        self.client.members = {"receipt.json": b" " * (MAX_RECEIPT + 1)}
        with patch.object(zipfile, "time", TickingZipClock()):
            self.assertOutcome(self.publish(), 3, "INCOMPLETE", "publication_report_too_large")
        self.assertEqual(self.client.posts(), 1)

    def test_the_report_listing_must_be_complete_across_pages_with_unique_ids(self):
        def other(identity: int) -> dict[str, object]:
            return {"id": identity, "name": "other", "size_in_bytes": 1, "digest": "sha256:" + "0" * 64,
                    "expired": False, "workflow_run": {"id": RUN_ID, "head_sha": HARNESS}}

        full = [other(1000 + index) for index in range(100)]
        cases = {
            "publication_report_listing_incomplete": [{"total_count": 102, "artifacts": full},
                                                      {"total_count": 102, "artifacts": [other(5)]}],
            "publication_report_listing_invalid": [{"total_count": 2, "artifacts": [other(5), other(5)]}],
        }
        for reason, pages in cases.items():
            with self.subTest(reason):
                self.reset()
                self.client.artifact_pages = lambda pages=pages: pages
                self.assertOutcome(self.publish(), 3, "INCOMPLETE", reason)
                self.assertFalse([call for call in self.client.calls if call[1].startswith("artifact:")])
        self.reset()
        self.client.artifact_pages = lambda: [{"total_count": 101, "artifacts": full},
                                              {"total_count": 101, "artifacts": [self.client.artifact_row()]}]
        self.assertOutcome(self.publish(), 0, "PUBLISHED", "hosted_delivery_verified_and_receipt_finalized")

    def test_published_needs_the_local_receipt_finalization(self):
        save = Receipt.save

        def refuse_published(receipt: Receipt, *, create: bool = False) -> None:
            if receipt.status == "PUBLISHED":
                raise OSError("disk full")
            save(receipt, create=create)

        with patch.object(Receipt, "save", refuse_published):
            self.assertOutcome(self.publish(), 3, "INCOMPLETE", "release_evidence_or_io_invalid")

    def persistently_failing(self, failed: Callable[[Receipt], bool]) -> tuple[int, dict[str, object], bytes]:
        """Publish while every save from the first ``failed`` state on raises; return the last durable bytes."""
        save, durable = Receipt.save, {}

        def guarded(receipt: Receipt, *, create: bool = False) -> None:
            if failed(receipt):
                raise OSError(errno.EIO, "persistent receipt write failure")
            save(receipt, create=create)
            durable["bytes"] = receipt.path.read_bytes()

        with patch.object(Receipt, "save", guarded):
            code, report = self.publish()
        return code, report, durable["bytes"]

    def assertUnpersisted(self, report: dict[str, object], status: str) -> None:
        stored = storage.load_receipt(self.receipt_path())
        self.assertEqual(report, {"status": status, "reason": "publication_receipt_not_persisted",
                                  "fault": "release_evidence_or_io_invalid", "receipt": str(self.receipt_path()),
                                  "receipt_persisted": False, "invocation_id": stored.invocation.invocation_id,
                                  "candidate_id": REFERENCE.candidate_id})

    def test_a_persistent_save_failure_after_the_post_is_incomplete_and_leaves_the_last_durable_receipt(self):
        code, report, durable = self.persistently_failing(lambda receipt: receipt.stage("dispatch").state == "complete")
        self.assertEqual(code, 3)
        self.assertUnpersisted(report, "INCOMPLETE")
        self.assertEqual(self.receipt_path().read_bytes(), durable)
        stored = storage.load_receipt(self.receipt_path())
        self.assertEqual((stored.stage("dispatch").state, stored.status), ("intended", "INCOMPLETE"))
        self.assertEqual(self.client.posts(), 1)
        self.assertFalse([call for call in self.client.calls if "/runs/" in call[1] or "/runs?" in call[1]])

    def test_a_persistent_save_failure_after_verified_delivery_is_never_published(self):
        code, report, durable = self.persistently_failing(lambda receipt: receipt.stage("cleanup").state == "complete")
        self.assertEqual(code, 3)
        self.assertUnpersisted(report, "INCOMPLETE")
        self.assertEqual(self.receipt_path().read_bytes(), durable)
        stored = storage.load_receipt(self.receipt_path())
        self.assertEqual((stored.stage("verify").result["outcome"], stored.stage("cleanup").state, stored.status),
                         ("published", "intended", "INCOMPLETE"))
        self.assertEqual(self.client.posts(), 1)

    def test_a_persistent_save_failure_before_any_dispatch_stays_blocked(self):
        code, report, durable = self.persistently_failing(
            lambda receipt: receipt.stage("preflight").state == "complete")
        self.assertEqual(code, 2)
        self.assertUnpersisted(report, "BLOCKED")
        self.assertEqual(self.receipt_path().read_bytes(), durable)
        self.assertEqual(storage.load_receipt(self.receipt_path()).stage("dispatch").state, "pending")
        self.assertEqual(self.client.posts(), 0)

    def test_reset_disposes_the_prior_tree_and_restores_every_patch(self):
        first = self.tree.root
        self.reset()
        self.assertFalse(first.exists())
        self.assertTrue(self.tree.root.is_dir())
        current = self.tree.root
        self.assertTrue(self.doCleanups())
        self.assertFalse(current.exists())
        self.assertEqual(tuple(getattr(owner, name) for owner, name in PATCHED), ORIGINALS)

    def test_an_expired_observation_is_incomplete_and_a_later_resume_only_observes(self):
        self.client.run.update(status="in_progress", conclusion=None)
        clock = iter((0.0, 10_000.0))
        with patch.object(publication_observation.time, "monotonic", side_effect=lambda: next(clock)):
            self.assertOutcome(self.publish(), 3, "INCOMPLETE", "publication_observation_expired")
        self.client.run.update(status="completed", conclusion="success")
        self.assertOutcome(self.resume(), 0, "PUBLISHED", "hosted_delivery_verified_and_receipt_finalized")
        self.assertEqual(self.client.posts(), 1)
        self.assertEqual(self.preflight.call_count, 2)


class PendingTitleTests(PublicationCase):
    """Only a nonterminal attempt 1 whose title alone is unbound is read again; nothing else waits or retries."""

    def observe(self) -> tuple[tuple[int, dict[str, object]], FakeClock]:
        clock = FakeClock()
        with patch.object(publication_observation, "time", clock):
            result = self.publish()
        return result, clock

    def run_requests(self) -> list[tuple[str, str]]:
        """Every request for a run other than its artifacts, including any inventory listing."""
        return [call for call in self.client.calls
                if "/actions/runs" in call[1] and not call[1].startswith(ARTIFACTS)]

    def test_a_pending_title_rereads_the_same_run_until_it_binds_and_completes(self):
        self.client.run_reads = [PENDING_TITLE, {**PENDING_TITLE, "status": "in_progress"}]
        result, clock = self.observe()
        self.assertOutcome(result, 0, "PUBLISHED", "hosted_delivery_verified_and_receipt_finalized")
        self.assertEqual((self.run_requests(), clock.sleeps), ([("GET", RUN)] * 3, [POLL_SECONDS] * 2))
        self.assertEqual(self.client.posts(), 1)
        observed = storage.load_receipt(self.receipt_path()).stage("observe")
        self.assertEqual((observed.state, observed.result),
                         ("complete", {"run_id": RUN_ID, "run_attempt": 1, "conclusion": "success"}))

    def test_a_title_still_unbound_at_the_deadline_refuses_with_it_and_never_redispatches(self):
        self.client.run.update(display_title="Release", status="in_progress", conclusion=None)
        result, clock = self.observe()
        self.assertOutcome(result, 3, "INCOMPLETE", "publication_run_unbound:mismatch=display_title")
        requests = self.run_requests()
        self.assertEqual((len(requests), set(requests)), (OBSERVE_TIMEOUT // POLL_SECONDS + 1, {("GET", RUN)}))
        self.assertEqual((sum(clock.sleeps), set(clock.sleeps)), (OBSERVE_TIMEOUT, {POLL_SECONDS}))
        self.assertEqual(self.client.posts(), 1)
        self.assertEqual(storage.load_receipt(self.receipt_path()).stage("observe").state, "intended")
        self.assertFalse([call for call in self.client.calls if call[1].startswith(ARTIFACTS)])

    def test_every_other_unbound_or_substituted_read_refuses_at_once(self):
        cases = {
            "completed wrong title": ({"display_title": "Release"}, "publication_run_unbound:mismatch=display_title"),
            "pending title on attempt 2": ({**PENDING_TITLE, "run_attempt": 2}, "publication_run_attempt_substituted"),
            "pending title, malformed attempt": ({**PENDING_TITLE, "run_attempt": "1"},
                                                 "release_record_integer_invalid"),
            "pending title and head": ({**PENDING_TITLE, "head_sha": "7" * 40},
                                       "publication_run_unbound:mismatch=display_title,head_sha"),
            "pending title and event": ({**PENDING_TITLE, "event": "push"},
                                        "publication_run_unbound:mismatch=display_title,event"),
            "pending title and other ID": ({**PENDING_TITLE, "id": RUN_ID + 1},
                                           "publication_run_unbound:mismatch=id,display_title,html_url"),
        }
        for label, (change, reason) in cases.items():
            with self.subTest(label):
                self.reset()
                # A later read would bind and complete, so any wait would publish instead of refusing.
                self.client.run_reads = [change]
                result, clock = self.observe()
                self.assertOutcome(result, 3, "INCOMPLETE", reason)
                self.assertEqual((self.run_requests(), clock.sleeps, self.client.posts()), ([("GET", RUN)], [], 1))

    def test_a_read_fault_after_a_pending_title_is_reported_not_masked(self):
        self.client.run_reads = [PENDING_TITLE, VerificationError("github_api_failed", "github-api")]
        result, clock = self.observe()
        self.assertOutcome(result, 3, "INCOMPLETE", "github_api_failed")
        self.assertEqual((self.run_requests(), clock.sleeps, self.client.posts()),
                         ([("GET", RUN)] * 2, [POLL_SECONDS], 1))


class ResumeTests(PublicationCase):
    def unknown_ack(self) -> None:
        self.client.ack = VerificationError("github_api_failed", "github-api")
        self.assertOutcome(self.publish(), 3, "INCOMPLETE", "publication_dispatch_unacknowledged_resume_observes_only")

    def test_resume_binds_the_unique_exact_run_and_never_redispatches(self):
        self.unknown_ack()
        self.assertOutcome(self.resume(), 0, "PUBLISHED", "hosted_delivery_verified_and_receipt_finalized")
        self.assertEqual(self.client.posts(), 1)
        dispatch = storage.load_receipt(self.receipt_path()).stage("dispatch")
        self.assertEqual((dispatch.result["run_id"], dispatch.result["bound_by"]), (RUN_ID, "inventory"))

    def test_resume_refuses_zero_or_several_matching_runs_without_dispatching(self):
        for listed, reason in ((0, "publication_dispatched_run_not_found_no_redispatch"),
                               (2, "publication_run_ambiguous")):
            with self.subTest(listed=listed):
                self.reset()
                self.unknown_ack()
                self.client.listed = listed
                self.assertOutcome(self.resume(), 3, "INCOMPLETE", reason)
                self.assertEqual(self.client.posts(), 1)

    def test_resume_refuses_a_title_match_that_is_not_the_bound_dispatch(self):
        self.unknown_ack()
        self.client.run["head_branch"] = "feature"
        self.assertOutcome(self.resume(), 3, "INCOMPLETE", "publication_run_unbound:mismatch=head_branch")

    def test_resume_of_a_never_dispatched_receipt_is_blocked_without_any_request(self):
        self.preflight.side_effect = VerificationError("publication_readiness_denied", "x")
        self.publish()
        self.client.calls.clear()
        self.assertOutcome(self.resume(), 2, "BLOCKED", "publication_never_dispatched_start_a_new_invocation")
        self.assertEqual(self.client.calls, [])

    def test_resume_refuses_a_changed_promotion_or_tampered_dispatch(self):
        self.unknown_ack()
        path = self.receipt_path()
        original = json.loads(path.read_text(encoding="utf-8"))
        tampered = copy.deepcopy(original)
        tampered["stages"]["dispatch"]["intent"]["title"] = "Candidate publication other"
        path.write_text(json.dumps(tampered), encoding="utf-8")
        self.assertOutcome(self.resume(), 3, "INCOMPLETE", "publication_prepared_dispatch_mismatch")
        path.write_text(json.dumps(original), encoding="utf-8")
        with patch.object(publication, "sha", side_effect=lambda data: "0" * 64):
            self.assertOutcome(self.resume(), 3, "INCOMPLETE", "publication_promotion_changed")
        self.assertEqual(self.client.posts(), 1)

    def test_a_finalized_receipt_resumes_to_the_same_outcome_without_requests(self):
        self.publish()
        self.client.calls.clear()
        self.assertOutcome(self.resume(), 0, "PUBLISHED", "hosted_delivery_verified_and_receipt_finalized")
        self.assertEqual(self.client.calls, [])

    def test_resume_publication_refuses_a_native_receipt_before_any_lock_or_effect(self):
        native = ReleaseRequest(repo=REPOSITORY, tag="v1.2.3", opencode_model="vendor/model", ccr_model=None,
                                no_submit=True)
        receipt = storage.create_receipt(Path(cli.__file__).resolve().parents[3], native, ["internal-test"])
        original = receipt.path.read_bytes()
        with patch.object(cli, "acquire_exclusive_file_lock", Mock(side_effect=AssertionError("locked"))), \
                patch.object(cli, "github_client", Mock(side_effect=AssertionError("client"))):
            code, report = self.run_cli(["resume-publication", "--receipt", str(receipt.path)])
        self.assertEqual((code, report), (2, {"status": "BLOCKED", "reason": "publication_request_required"}))
        self.assertEqual(receipt.path.read_bytes(), original)


if __name__ == "__main__":
    unittest.main()
