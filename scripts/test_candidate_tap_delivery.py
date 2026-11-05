"""Tap delivery reads, compares and writes the exact candidate formula through the real client.

The candidate is loaded by the real artifact loader from a fixture container. A
second finite fake ``gh`` answers the tap requests from a queue of fixture
replies; nothing reaches the network, a provider or a real tap.
"""

from __future__ import annotations

import base64
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.release_verification import github
from scripts.delegate_acceptance.release_verification import candidate_tap_delivery as tap
from scripts.delegate_acceptance.release_verification.candidate_manifest import HOMEBREW_FORMULA_PATH
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.published_contract import git_blob_sha
from scripts.test_candidate_artifact_support import CandidateArtifactCase, payload_files
from scripts.test_candidate_source_support import ChildFault, settle_retained
from scripts.test_release_github_transport import ObservedGitHubClient


FORMULA = b'class DelegationLayer < Formula\n  desc "Delegation layer"\n  version "0.1.37"\nend\n'
OLD = b'class DelegationLayer < Formula\n  version "0.1.36"\nend\n'
REPO = tap.TAP_REPOSITORY
TREE = f"GET repos/{REPO}/git/trees/main?recursive=1"
PUT = f"PUT repos/{REPO}/contents/{tap.FORMULA_PATH}"


def blob(content: bytes) -> str:
    return f"GET repos/{REPO}/git/blobs/{git_blob_sha(content)}"


# Replies are consumed in order per "METHOD endpoint"; an unrouted request fails like a refused API call.
_FAKE_TAP_GH = """#!{python}
import json, os, sys
argv = sys.argv[1:]
key = argv[2] + " " + argv[7]
body = None
if "--input" in argv:
    with open(argv[argv.index("--input") + 1]) as source:
        body = json.load(source)
with open(os.environ["TAP_LOG"], "a") as log:
    log.write(json.dumps({{"key": key, "body": body}}) + "\\n")
with open(os.environ["TAP_ROUTES"]) as source:
    routes = json.load(source)
queue = routes.get(key) or []
if not queue:
    sys.stderr.write("fixture: no route\\n")
    sys.exit(1)
reply = queue.pop(0)
with open(os.environ["TAP_ROUTES"], "w") as sink:
    json.dump(routes, sink)
if "write" in reply:
    with open(reply["write"][0], "w") as target:
        target.write(reply["write"][1])
if "exit" in reply:
    sys.stderr.write("fixture: HTTP failure\\n")
    sys.exit(reply["exit"])
sys.stdout.buffer.write(bytes.fromhex(reply["raw"]) if "raw" in reply else json.dumps(reply["json"]).encode())
"""


def listing(*entries: tuple[str, str, str, str], truncated: object = False) -> dict:
    """A tree reply of (path, mode, type, sha) rows."""
    return {"json": {"sha": "9" * 40, "truncated": truncated, "tree": [
        {"path": path, "mode": mode, "type": kind, "sha": sha} for path, mode, kind, sha in entries]}}


def formula_row(content: bytes, *, mode: str = "100644", kind: str = "blob") -> tuple[str, str, str, str]:
    return (tap.FORMULA_PATH, mode, kind, git_blob_sha(content))


README = ("README.md", "100644", "blob", "1" * 40)


def raw(content: bytes) -> dict:
    return {"raw": content.hex()}


def ack(data: bytes, **changes: object) -> dict:
    reply = {"content": {"path": tap.FORMULA_PATH, "type": "file", "sha": git_blob_sha(data)},
             "commit": {"sha": "8" * 40}}
    reply.update(changes)
    return {"json": reply}


class TapDeliveryTests(CandidateArtifactCase):
    def setUp(self) -> None:
        super().setUp()
        self.artifact = self.load(self.serve(files=payload_files(**{HOMEBREW_FORMULA_PATH: FORMULA})).locator)
        self.formula = self.artifact.payload / HOMEBREW_FORMULA_PATH
        bin_dir = self.root / "tap-bin"
        bin_dir.mkdir()
        (bin_dir / "gh").write_text(_FAKE_TAP_GH.format(python=sys.executable))
        (bin_dir / "gh").chmod(0o755)
        self.work = self.root / "tap-work"
        self.work.mkdir()
        self.tap_log, self.tap_routes = self.root / "tap-log.jsonl", self.root / "tap-routes.json"
        self.receipt: dict = {"commands": []}

    def deliver(self, routes: dict[str, list[dict]], **options) -> str:
        self.tap_routes.write_text(json.dumps(routes))
        environment = {"PATH": str(self.root / "tap-bin"), "TAP_LOG": str(self.tap_log),
                       "TAP_ROUTES": str(self.tap_routes), "GH_TOKEN": "private-canary"}
        client = ObservedGitHubClient(self.receipt, environment, self.tree)
        return tap.deliver_tap(self.artifact, client=client, work_directory=self.work.resolve(), **options)

    def assert_delivery_refused(self, code: str, routes: dict[str, list[dict]], **options) -> VerificationError:
        with self.assertRaises(VerificationError) as caught:
            self.deliver(routes, **options)
        self.assertEqual(caught.exception.code, code)
        self.assertNotIn("private-canary", repr(caught.exception) + json.dumps(self.receipt))
        return caught.exception

    def calls(self) -> list[dict]:
        if not self.tap_log.exists():
            return []
        return [json.loads(line) for line in self.tap_log.read_text().splitlines()]

    def keys(self) -> list[str]:
        return [call["key"] for call in self.calls()]

    def put_body(self) -> dict:
        (body,) = (call["body"] for call in self.calls() if call["key"] == PUT)
        return body

    def test_an_absent_formula_is_created_with_the_exact_bytes_and_read_back(self) -> None:
        confirmed = self.deliver({TREE: [listing(README), listing(README, formula_row(FORMULA))],
                                  PUT: [ack(FORMULA)], blob(FORMULA): [raw(FORMULA)]})
        self.assertEqual(confirmed, git_blob_sha(FORMULA))
        self.assertEqual(self.keys(), [TREE, PUT, TREE, blob(FORMULA)])
        body = self.put_body()
        self.assertEqual(base64.b64decode(body["content"], validate=True), FORMULA)
        self.assertEqual(body["content"], base64.b64encode(FORMULA).decode("ascii"))
        self.assertEqual((body["branch"], "sha" in body), ("main", False))
        self.assertEqual(json.loads((self.work / "tap-formula.json").read_text()), body)
        self.assertNotIn("private-canary", json.dumps(self.receipt))

    def test_an_existing_different_formula_is_replaced_only_against_its_observed_blob(self) -> None:
        confirmed = self.deliver({TREE: [listing(formula_row(OLD)), listing(formula_row(FORMULA))],
                                  blob(OLD): [raw(OLD)], PUT: [ack(FORMULA)], blob(FORMULA): [raw(FORMULA)]})
        self.assertEqual(confirmed, git_blob_sha(FORMULA))
        self.assertEqual(self.keys(), [TREE, blob(OLD), PUT, TREE, blob(FORMULA)])
        self.assertEqual(self.put_body()["sha"], git_blob_sha(OLD))
        self.assertEqual(base64.b64decode(self.put_body()["content"]), FORMULA)

    def test_identical_bytes_skip_without_a_put(self) -> None:
        self.assertEqual(self.deliver({TREE: [listing(formula_row(FORMULA))], blob(FORMULA): [raw(FORMULA)]}),
                         git_blob_sha(FORMULA))
        self.assertEqual(self.keys(), [TREE, blob(FORMULA)])

    def test_raw_bytes_that_do_not_hash_to_the_listed_blob_refuse_before_any_put(self) -> None:
        self.assert_delivery_refused("candidate_tap_observation_invalid",
                                    {TREE: [listing(formula_row(OLD))], blob(OLD): [raw(OLD + b"#")]})
        self.assertNotIn(PUT, self.keys())

    def test_a_formula_not_matching_the_manifest_refuses_before_any_request(self) -> None:
        self.formula.write_bytes(FORMULA.replace(b"0.1.37", b"0.1.38"))
        self.assert_delivery_refused("candidate_tap_formula_mismatch", {})
        self.assertEqual(self.keys(), [])

    def test_a_formula_changed_after_observation_is_refused_before_the_put(self) -> None:
        changed = FORMULA.decode().replace("0.1.37", "0.1.99")
        self.assert_delivery_refused("candidate_tap_formula_mismatch",
                                    {TREE: [{**listing(README), "write": [str(self.formula), changed]}], PUT: [ack(FORMULA)]})
        self.assertEqual(self.keys(), [TREE])

    def test_only_a_complete_well_formed_listing_without_the_path_means_absence(self) -> None:
        cases = {
            "candidate_tap_tree_truncated": [listing(README, truncated=True)],
            "candidate_tap_tree_invalid": [listing(formula_row(OLD), formula_row(OLD))],
            "candidate_tap_formula_not_regular": [listing(formula_row(OLD, mode="120000"))],
            "github_request_failed": [{"exit": 1}],
        }
        for code, replies in cases.items():
            with self.subTest(code=code):
                self.tap_log.unlink(missing_ok=True)
                self.assert_delivery_refused(code, {TREE: replies, PUT: [ack(FORMULA)]})
                self.assertEqual(self.keys(), [TREE])
        for kind in ("tree", "commit"):
            with self.subTest(kind=kind):
                self.tap_log.unlink(missing_ok=True)
                self.assert_delivery_refused("candidate_tap_formula_not_regular",
                                            {TREE: [listing(formula_row(OLD, kind=kind))], PUT: [ack(FORMULA)]})

    def test_a_refused_put_stops_without_retry_or_later_read(self) -> None:
        self.assert_delivery_refused(
            "github_request_failed", {TREE: [listing(formula_row(OLD)), listing(formula_row(FORMULA))],
                                      blob(OLD): [raw(OLD)], PUT: [{"exit": 1}, ack(FORMULA)]})
        self.assertEqual(self.keys(), [TREE, blob(OLD), PUT])

    def test_a_malformed_acknowledgement_stops_without_retry_or_later_read(self) -> None:
        for name, reply in {"blob": ack(OLD), "path": ack(FORMULA, content={"path": "x.rb", "type": "file",
                                                                            "sha": git_blob_sha(FORMULA)}),
                            "commit": ack(FORMULA, commit={"sha": "HEAD"})}.items():
            with self.subTest(ack=name):
                # The caller owns each request body file; the next attempt needs a new one.
                for stale in (self.tap_log, self.work / "tap-formula.json"):
                    stale.unlink(missing_ok=True)
                self.assert_delivery_refused("candidate_tap_ack_invalid",
                                            {TREE: [listing(README), listing(formula_row(FORMULA))], PUT: [reply, ack(FORMULA)]})
                self.assertEqual(self.keys(), [TREE, PUT])

    def test_a_known_acknowledgement_whose_branch_differs_on_readback_stops(self) -> None:
        self.assert_delivery_refused("candidate_tap_readback_mismatch",
                                    {TREE: [listing(README), listing(formula_row(OLD))], PUT: [ack(FORMULA)], blob(OLD): [raw(OLD)]})
        self.assertEqual(self.keys(), [TREE, PUT, TREE, blob(OLD)])

    def test_an_invalid_target_refuses_before_any_request(self) -> None:
        for options in ({"tap_repository": "owner/../x"}, {"tap_repository": "owner"},
                        {"branch": "refs/heads/main"}, {"branch": "main?recursive=1"}, {"branch": "a..b"},
                        {"branch": "-main"}):
            with self.subTest(options=options):
                self.assert_delivery_refused("candidate_tap_target_invalid", {}, **options)
        self.assertEqual(self.keys(), [])

    def test_an_unproven_put_wait_is_retained_and_never_followed(self) -> None:
        fault = ChildFault(lambda argv: "PUT" in argv)
        with patch.object(github, "run_bounded_command", fault):
            self.assert_delivery_refused("command_timeout", {TREE: [listing(README), listing(formula_row(FORMULA))],
                                                             PUT: [ack(FORMULA)]})
        self.assertEqual(self.keys(), [TREE])
        self.assertIsInstance(self.tree.unresolved, VerificationError)
        assert isinstance(self.tree.unresolved, VerificationError)
        self.assertEqual(self.tree.unresolved.code, "command_timeout")
        settle_retained(self, self.tree, fault, (self.work / "tap-formula.json",))


if __name__ == "__main__":
    unittest.main()
