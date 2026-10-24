"""Merge observation through the real command owner, GitHub client and finite fake ``gh`` children.

The fake ``gh`` is a real child found through PATH. It answers the exact merge
query argv and the exact GitHub API endpoints from a test-owned JSON state file,
logs one line per invocation, and answers anything else as not found. Nothing
touches the network or a provider.
"""

from __future__ import annotations

import contextlib
import io
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock, patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.processes import operations
from scripts.delegate_acceptance.release_verification import commands, github
from scripts.delegate_acceptance.release_verification.candidate_manifest import parse_manifest
from scripts.delegate_acceptance.release_verification.candidate_merge_observation import (
    MAX_COMPARE_BYTES, observe_publication_source,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    BoundCandidate, PublicationSource,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_artifact_support import (
    COMMIT, REPOSITORY, TREE, locator, manifest_bytes, payload_files,
)
from scripts.test_candidate_source_support import ChildFault, settle_retained
from scripts.test_release_github_transport import ObservedGitHubClient


NUMBER = 32
MERGED = "e5" * 20
MAIN = "d4" * 20
OTHER = "c7" * 20
MERGE_ARGV = ["pr", "view", "32", "--repo", REPOSITORY, "--json",
              "number,state,headRefOid,baseRefName,mergeCommit,mergedAt"]
HEADERS = ["-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2026-03-10"]
COMMIT_ENDPOINT = f"repos/{REPOSITORY}/git/commits/{MERGED}"
MAIN_ENDPOINT = f"repos/{REPOSITORY}/git/ref/heads/main"
COMPARE_ENDPOINT = f"repos/{REPOSITORY}/compare/{MERGED}...{MAIN}?per_page=1&page=2"
MERGE_KEY = " ".join(MERGE_ARGV)
API_KEYS = ["GET " + COMMIT_ENDPOINT, "GET " + MAIN_ENDPOINT, "GET " + COMPARE_ENDPOINT]

_FAKE_GH = r'''#!{python}
import json, os, sys
argv = sys.argv[1:]
with open(os.environ["FIXTURE_STATE"]) as source:
    replies = json.load(source)
key = " ".join(argv) if argv[0] == "pr" else "GET " + argv[7]
with open(os.environ["FIXTURE_LOG"], "a") as sink:
    sink.write(json.dumps({"argv": argv, "key": key}) + "\n")
reply = replies.get(key)
if reply is None:
    sys.stdout.write('{"message":"Not Found","status":"404"}')
    sys.exit(1)
sys.stdout.write(reply["output"] if isinstance(reply["output"], str) else json.dumps(reply["output"]))
sys.exit(reply["exit"])
'''


def merge_view(**changes: object) -> dict[str, object]:
    """The observed PR 32 reply shape, bound to the synthetic B and M."""
    return {"baseRefName": "main", "headRefOid": COMMIT, "mergeCommit": {"oid": MERGED},
            "mergedAt": "2026-09-24T17:00:17Z", "number": NUMBER, "state": "MERGED", **changes}


def comparison(**changes: object) -> dict[str, object]:
    """The proven page-2 comparison metadata: one commit row and no file list."""
    return {"status": "ahead", "ahead_by": 55, "behind_by": 0, "total_commits": 55,
            "base_commit": {"sha": MERGED}, "merge_base_commit": {"sha": MERGED},
            "commits": [{"sha": OTHER}], **changes}


class MergeObservationCase(unittest.TestCase):
    def setUp(self) -> None:
        self.descriptors = open_descriptors()
        self.addCleanup(lambda: self.assertEqual(open_descriptors(), self.descriptors))
        self.tree = FixtureTree.open(prefix="candidate-merge-observation-test",
                                     label="candidate merge observation fixture")
        self.addCleanup(self.tree.dispose)
        root = self.tree.root
        (root / "bin").mkdir()
        gh = root / "bin" / "gh"
        gh.write_text(_FAKE_GH.replace("{python}", sys.executable, 1))
        gh.chmod(0o755)
        self.state_path, self.log_path = root / "state.json", root / "invocations.jsonl"
        files = payload_files()
        manifest = manifest_bytes(files)
        self.candidate = BoundCandidate(locator=locator(b"container", manifest),
                                        manifest=parse_manifest(manifest))
        self.receipt: dict = {"commands": []}
        self.replies: dict[str, dict[str, object]] = {}
        self.reply(MERGE_KEY, merge_view())
        self.reply(API_KEYS[0], {"sha": MERGED, "tree": {"sha": TREE}})
        self.reply(API_KEYS[1], {"ref": "refs/heads/main", "object": {"type": "commit", "sha": MAIN}})
        self.reply(API_KEYS[2], comparison())

    def reply(self, key: str, output: object, exit_code: int = 0) -> None:
        self.replies[key] = {"output": output, "exit": exit_code}
        self.state_path.write_text(json.dumps(self.replies))

    def observe(self, *, pull_request: object = NUMBER, merged_commit: object = MERGED) -> PublicationSource:
        environment = {"PATH": str(self.tree.root / "bin"), "FIXTURE_STATE": str(self.state_path),
                       "FIXTURE_LOG": str(self.log_path), "GH_TOKEN": "private-canary"}
        client = ObservedGitHubClient(self.receipt, environment, self.tree)
        # The merge query runs through the command owner, outside the observed client.
        return self.tree.observe_operation(lambda: observe_publication_source(
            client, candidate=self.candidate, pull_request=pull_request, merged_commit=merged_commit))

    def refused(self, code: str, **kwargs: object) -> VerificationError:
        with self.assertRaises(VerificationError) as caught:
            self.observe(**kwargs)
        self.assertEqual(caught.exception.code, code)
        return caught.exception

    def log(self) -> list[dict]:
        if not self.log_path.exists():
            return []
        return [json.loads(line) for line in self.log_path.read_text().splitlines()]

    def keys(self) -> list[str]:
        return [entry["key"] for entry in self.log()]


class HappyPathTests(MergeObservationCase):
    def test_observed_pull_request_shape_returns_the_canonical_publication_source(self):
        source = self.observe()
        self.assertEqual(source, PublicationSource(build_commit=COMMIT, merged_commit=MERGED, tree=TREE))
        self.assertEqual(self.keys(), [MERGE_KEY, *API_KEYS])
        merge, *api = self.log()
        self.assertEqual(merge["argv"], MERGE_ARGV)
        for entry, endpoint in zip(api, (COMMIT_ENDPOINT, MAIN_ENDPOINT, COMPARE_ENDPOINT)):
            self.assertEqual(entry["argv"], ["api", "--method", "GET", *HEADERS, endpoint])
        self.assertEqual(self.receipt["commands"], [
            {"name": "github-pull-request-merge", "argv": ["gh", *MERGE_ARGV], "exit_code": 0},
            *({"name": "github-api", "endpoint": endpoint, "method": "GET", "exit_code": 0}
              for endpoint in (COMMIT_ENDPOINT, MAIN_ENDPOINT, COMPARE_ENDPOINT)),
        ])
        self.assertNotIn("private-canary", repr(self.receipt))
        self.assertTrue(self.tree.dispose())

    def test_comparison_names_the_pinned_main_commit_never_the_symbolic_branch(self):
        self.observe()
        compare = self.log()[-1]["argv"][-1]
        self.assertEqual(compare, COMPARE_ENDPOINT)
        self.assertNotIn("...main", compare)

    def test_main_at_the_merge_commit_is_identical_and_accepted(self):
        identical = f"repos/{REPOSITORY}/compare/{MERGED}...{MERGED}?per_page=1&page=2"
        self.reply(API_KEYS[1], {"ref": "refs/heads/main", "object": {"type": "commit", "sha": MERGED}})
        self.reply("GET " + identical, comparison(status="identical", ahead_by=0, total_commits=0, commits=[]))
        self.assertEqual(self.observe().merged_commit, MERGED)
        self.assertEqual(self.keys()[-1], "GET " + identical)


class InputTests(MergeObservationCase):
    def test_invalid_inputs_refuse_before_any_command(self):
        for pull_request in (True, 0, -1, "32", 32.0):
            with self.subTest(pull_request=pull_request):
                self.refused("publication_pull_request_invalid", pull_request=pull_request)
        for merged_commit in ("e5" * 10, MERGED.upper(), None):
            with self.subTest(merged_commit=merged_commit):
                self.refused("publication_merged_commit_invalid", merged_commit=merged_commit)
        self.assertEqual(self.log(), [])
        self.assertEqual(self.receipt["commands"], [])


class MergeReplyTests(MergeObservationCase):
    """Every refused merge reply stops before the commit, main or comparison read."""

    def assert_merge_refused(self, code: str, output: object) -> None:
        self.reply(MERGE_KEY, output)
        self.refused(code)
        self.assertEqual(self.keys(), [MERGE_KEY])

    def test_unmerged_states_are_refused(self):
        for state in ("OPEN", "CLOSED", "merged"):
            with self.subTest(state=state):
                self.log_path.unlink(missing_ok=True)
                self.assert_merge_refused("pull_request_not_merged", merge_view(state=state))

    def test_missing_or_potential_merge_commit_is_refused(self):
        for merge in (None, {"oid": OTHER}, {"oid": MERGED, "url": "x"}, MERGED, {}):
            with self.subTest(merge=merge):
                self.log_path.unlink(missing_ok=True)
                self.assert_merge_refused("pull_request_merge_commit_mismatch", merge_view(mergeCommit=merge))

    def test_wrong_identity_base_or_head_is_refused(self):
        cases = (("pull_request_identity_mismatch", {"number": 33}),
                 ("pull_request_identity_mismatch", {"number": True}),
                 ("pull_request_identity_mismatch", {"number": "32"}),
                 ("pull_request_base_mismatch", {"baseRefName": "release"}),
                 ("pull_request_head_mismatch", {"headRefOid": OTHER}),
                 ("pull_request_merge_invalid", {"mergedAt": ""}),
                 ("pull_request_merge_invalid", {"mergedAt": None}))
        for code, change in cases:
            with self.subTest(change=change):
                self.log_path.unlink(missing_ok=True)
                self.assert_merge_refused(code, merge_view(**change))

    def test_malformed_or_unexpected_fields_are_refused(self):
        extra = {**merge_view(), "potentialMergeCommit": {"oid": MERGED}}
        missing = {key: value for key, value in merge_view().items() if key != "mergedAt"}
        for output in ("not json", "[]", json.dumps(extra), json.dumps(missing)):
            with self.subTest(output=output[:20]):
                self.log_path.unlink(missing_ok=True)
                self.assert_merge_refused(
                    "github_object_invalid" if output == "[]" else "pull_request_merge_invalid", output)

    def test_failed_merge_query_is_refused(self):
        self.reply(MERGE_KEY, "", exit_code=1)
        self.refused("command_failed")
        self.assertEqual(self.keys(), [MERGE_KEY])
        self.assertEqual(self.receipt["commands"][-1]["exit_code"], 1)


class CommitTests(MergeObservationCase):
    def test_commit_identity_mismatch_stops_before_main(self):
        for reply in ({"sha": OTHER, "tree": {"sha": TREE}}, {"sha": MERGED, "tree": None},
                      {"sha": MERGED, "tree": {"sha": 7}}):
            with self.subTest(reply=reply):
                self.log_path.unlink(missing_ok=True)
                self.reply(API_KEYS[0], reply)
                self.refused("publication_commit_mismatch")
                self.assertEqual(self.keys(), [MERGE_KEY, API_KEYS[0]])

    def test_tree_mismatch_is_refused_by_the_canonical_policy_before_main(self):
        self.reply(API_KEYS[0], {"sha": MERGED, "tree": {"sha": OTHER}})
        error = self.refused("publication_tree_mismatch")
        self.assertEqual(error.stage, "decide-candidate-readiness")
        self.assertEqual(self.keys(), [MERGE_KEY, API_KEYS[0]])

    def test_malformed_tree_is_refused_by_the_canonical_policy(self):
        self.reply(API_KEYS[0], {"sha": MERGED, "tree": {"sha": "main"}})
        self.refused("publication_source_invalid")
        self.assertEqual(self.keys(), [MERGE_KEY, API_KEYS[0]])

    def test_failed_commit_read_stops_before_main(self):
        self.reply(API_KEYS[0], "", exit_code=1)
        self.refused("github_request_failed")
        self.assertEqual(self.keys(), [MERGE_KEY, API_KEYS[0]])


class MainTests(MergeObservationCase):
    def test_bad_main_ref_stops_before_comparison(self):
        for reply in ({"ref": "refs/heads/other", "object": {"type": "commit", "sha": MAIN}},
                      {"ref": "refs/heads/main", "object": {"type": "tag", "sha": MAIN}},
                      {"ref": "refs/heads/main", "object": {"type": "commit", "sha": "main"}},
                      {"ref": "refs/heads/main"}):
            with self.subTest(reply=reply):
                self.log_path.unlink(missing_ok=True)
                self.reply(API_KEYS[1], reply)
                self.refused("publication_main_ref_invalid")
                self.assertEqual(self.keys(), [MERGE_KEY, *API_KEYS[:2]])

    def test_merge_commit_not_on_main_is_refused(self):
        for change in ({"status": "behind", "ahead_by": 0, "behind_by": 3},
                       {"status": "diverged", "ahead_by": 2, "behind_by": 1},
                       {"behind_by": 1}):
            with self.subTest(change=change):
                self.reply(API_KEYS[2], comparison(**change))
                self.refused("publication_merge_not_on_main")

    def test_wrong_comparison_base_is_refused(self):
        for change in ({"merge_base_commit": {"sha": OTHER}}, {"base_commit": {"sha": OTHER}},
                       {"merge_base_commit": None}):
            with self.subTest(change=change):
                self.reply(API_KEYS[2], comparison(**change))
                self.refused("publication_main_ancestry_mismatch")

    def test_malformed_or_incoherent_comparison_is_refused(self):
        for change in ({"behind_by": False}, {"ahead_by": True}, {"ahead_by": -1}, {"status": None},
                       {"behind_by": "0"}, {"status": "identical", "ahead_by": 0}, {"ahead_by": 0}):
            with self.subTest(change=change):
                self.reply(API_KEYS[2], comparison(**change))
                self.refused("publication_main_comparison_invalid")

    def test_failed_comparison_is_refused(self):
        self.reply(API_KEYS[2], "", exit_code=1)
        self.refused("github_request_failed")
        self.assertEqual(self.keys(), [MERGE_KEY, *API_KEYS])

    def test_comparison_overflow_keeps_its_identity(self):
        self.reply(API_KEYS[2], "x" * (MAX_COMPARE_BYTES + 1))
        self.refused("command_output_exceeded")
        self.assertEqual(self.receipt["commands"][-1]["endpoint"], COMPARE_ENDPOINT)
        self.assertTrue(self.tree.dispose())


class UnprovenClosureTests(MergeObservationCase):
    """An unknown Wait at the canonical process port retains the tree and stops every later read.

    Every child of the observation runs through one ``ChildFault`` at both canonical ports, so
    the test itself proves which one request faulted and that every launched child closed.
    """

    def observe_with(self, fault: ChildFault, code_stage: tuple[str, str]) -> VerificationError:
        with (patch.object(commands, "run_bounded_command", fault),
              patch.object(github, "run_bounded_command", fault)):
            error = self.refused(code_stage[0])
        self.assertEqual((error.code, error.stage), code_stage)
        self.assertIs(self.tree.unresolved, error)
        self.assertIsNone(self.receipt["commands"][-1]["exit_code"])
        self.assertNotIn("private-canary", repr(self.receipt))
        return error

    def test_unknown_merge_query_wait_retains_and_stops(self):
        fault = ChildFault(lambda argv: argv[:3] == ["gh", "pr", "view"])
        self.observe_with(fault, ("command_timeout", "github-pull-request-merge"))
        self.assertEqual((fault.faulted, fault.launched, self.keys()), ([["gh", *MERGE_ARGV]], 0, []))
        settle_retained(self, self.tree, fault, (self.state_path,))

    def test_unknown_comparison_wait_retains_and_stops(self):
        fault = ChildFault(lambda argv: argv[-1] == COMPARE_ENDPOINT)
        self.observe_with(fault, ("command_timeout", "github-api"))
        self.assertEqual(fault.faulted, [["gh", "api", "--method", "GET", *HEADERS, COMPARE_ENDPOINT]])
        self.assertEqual((fault.launched, self.keys()), (3, [MERGE_KEY, *API_KEYS[:2]]))
        settle_retained(self, self.tree, fault, (self.state_path, self.log_path))

    def test_unexpected_earlier_failure_without_a_child_keeps_the_tree_retained(self):
        refusal = AssertionError("no child may launch after an earlier synthetic failure")
        earlier = VerificationError("command_timeout", "synthetic-earlier-operation")
        def fail() -> None:
            raise earlier

        with patch.object(operations.subprocess, "Popen", Mock(side_effect=refusal)) as launches:
            with self.assertRaises(VerificationError) as caught:
                self.tree.observe_operation(fail)
        self.assertIs(caught.exception, earlier)
        self.assertIs(self.tree.unresolved, earlier)
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertFalse(self.tree.dispose())
        self.assertTrue(self.state_path.exists())
        # The test's own proof that no child started is the only licence to remove the tree.
        self.assertEqual((launches.call_count, self.log()), (0, []))
        self.tree.remove_retained()
        self.assertFalse(self.tree.root.exists())

if __name__ == "__main__":
    unittest.main()
