"""The harness observer compares a real fixture checkout with one exact commit's own objects.

Every repository here is a test-owned fixture whose bytes are test inputs only;
nothing is evidence about the real repository, a provider or a release.
"""

from __future__ import annotations

import os
from pathlib import Path
import sys
import unittest
from unittest import mock

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.release_verification import candidate_source_observation, source
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_source_support import SourceRepository


ENTRY = "scripts/fixture_entry.py"
HELPER = "scripts/fixture_helper.py"
DATA = "fixture.txt"
EXTRA = "scripts/fixture_extra.py"
SEEDS = (ENTRY, DATA)
FILES = {
    source.PARSER_SOURCE: (Path(__file__).resolve().parent.parent / source.PARSER_SOURCE).read_bytes(),
    ENTRY: b"from scripts.fixture_helper import VALUE\n",
    HELPER: b"VALUE = 'executing'\n",
    DATA: b"fixture data\n",
}
OTHER_HELPER = b"VALUE = 'other committed bytes'\n"


class HarnessCase(unittest.TestCase):
    """One fixture tree and one repository per test, so no test inherits another's refs or files."""

    def setUp(self) -> None:
        self.tree = FixtureTree.open(prefix="committed-source", label="committed source observation")
        self.addCleanup(self.tree.dispose)
        self.repository = SourceRepository(self.tree, "harness")
        self.head = self.repository.commit(FILES, "harness")

    def observe(self, *, receipt: dict | None = None, seeds: tuple[str, ...] = SEEDS,
                environment: dict[str, str] | None = None, checkout: Path | None = None) -> tuple[str, str, bool]:
        try:
            return source.observe_harness({"commands": []} if receipt is None else receipt,
                                          os.environ if environment is None else environment,
                                          checkout=checkout or self.repository.path, seeds=seeds)
        except VerificationError as error:
            self.tree.observe(error)
            raise

    def fingerprint(self, seeds: tuple[str, ...] = SEEDS) -> str:
        return source.fingerprint_sources(self.repository.path, seeds)["sha256"]

    def assertRefused(self, code: str, stage: str, **kwargs) -> None:
        with self.assertRaises(VerificationError) as caught:
            self.observe(**kwargs)
        self.assertEqual((caught.exception.code, caught.exception.stage), (code, stage))

    def git_state(self) -> tuple[bytes, bytes, str]:
        """HEAD and index bytes plus every ref, including replace refs."""
        directory = self.repository.path / ".git"
        return ((directory / "HEAD").read_bytes(), (directory / "index").read_bytes(),
                self.repository.git("for-each-ref"))


class CommittedMatchTests(HarnessCase):
    def test_a_committed_harness_matches_the_one_head_it_resolved(self) -> None:
        receipt = {"commands": []}
        self.assertEqual(self.observe(receipt=receipt), (self.head, self.fingerprint(), True))
        names = [record["name"] for record in receipt["commands"]]
        self.assertEqual(names[0], "git-harness-head")
        self.assertEqual(names.count("git-harness-head"), 1)
        # Every later read names the resolved commit or its blobs, never a ref.
        self.assertTrue(all("HEAD" not in argument for record in receipt["commands"][1:]
                            for argument in record["argv"]))
        self.assertIn(self.head, receipt["commands"][1]["argv"])
        self.assertEqual({record["exit_code"] for record in receipt["commands"]}, {0})
        self.assertEqual(sorted(item["path"] for item in source.fingerprint_sources(
            self.repository.path, SEEDS)["files"]), sorted(FILES))

    def assertNotCommitted(self, *, head: str, seeds: tuple[str, ...] = SEEDS) -> None:
        receipt = {"commands": []}
        self.assertEqual(self.observe(receipt=receipt, seeds=seeds), (head, self.fingerprint(seeds), False))
        # A mismatch is decided from the listing and blobs; no Git read fails along the way.
        self.assertEqual({record["exit_code"] for record in receipt["commands"]}, {0})

    def test_a_changed_tracked_file_is_not_committed(self) -> None:
        (self.repository.path / HELPER).write_bytes(OTHER_HELPER)
        self.assertNotCommitted(head=self.head)

    def test_an_untracked_fingerprinted_file_is_not_committed(self) -> None:
        (self.repository.path / EXTRA).write_bytes(b"EXTRA = 1\n")
        self.assertNotCommitted(head=self.head, seeds=(*SEEDS, EXTRA))

    def test_an_ignored_fingerprinted_file_is_not_committed(self) -> None:
        head = self.repository.commit({".gitignore": EXTRA.encode() + b"\n"}, "ignore")
        (self.repository.path / EXTRA).write_bytes(b"EXTRA = 1\n")
        self.assertEqual(self.repository.git("status", "--porcelain=v1", "--untracked-files=all"), "")
        self.assertNotCommitted(head=head, seeds=(*SEEDS, EXTRA))

    def test_a_file_committed_as_a_symlink_is_not_committed(self) -> None:
        path = self.repository.path / DATA
        path.unlink()
        path.symlink_to(HELPER)
        self.repository.git("add", "--all")
        self.repository.git("commit", "--quiet", "-m", "symlink")
        path.unlink()
        path.write_bytes(FILES[DATA])
        self.assertNotCommitted(head=self.repository.git("rev-parse", "HEAD"))


class RefusalTests(HarnessCase):
    def test_a_malformed_fingerprint_refuses_instead_of_mismatching(self) -> None:
        digest = "a" * 64
        cases = (
            ("source_fingerprint_invalid", {"sha256": "short", "files": [{"path": DATA, "sha256": digest}]}),
            ("source_fingerprint_invalid", {"sha256": digest, "files": []}),
            ("source_fingerprint_invalid", {"sha256": digest, "files": [DATA]}),
            ("source_fingerprint_invalid", {"sha256": digest, "files": [{"path": DATA, "sha256": "A" * 64}]}),
            ("source_fingerprint_invalid", {"sha256": digest, "files": [{"path": DATA, "sha256": digest}] * 2}),
            ("source_path_invalid", {"sha256": digest, "files": [{"path": "/etc/passwd", "sha256": digest}]}),
            ("source_path_invalid", {"sha256": digest, "files": [{"path": "scripts/../x", "sha256": digest}]}),
            ("source_path_invalid", {"sha256": digest, "files": [{"path": "", "sha256": digest}]}),
            ("source_path_invalid", {"sha256": digest, "files": [{"path": 7, "sha256": digest}]}),
        )
        for code, fingerprint in cases:
            with self.subTest(code=code, fingerprint=fingerprint), \
                    mock.patch.object(source, "fingerprint_sources", return_value=fingerprint):
                self.assertRefused(code, source.HARNESS_STAGE)

    def test_git_and_receipt_failures_refuse_instead_of_mismatching(self) -> None:
        not_git = self.tree.root / "not-a-repository"
        not_git.mkdir()
        self.assertRefused("command_failed", "git-harness-head", checkout=not_git)
        unborn = SourceRepository(self.tree, "unborn")
        self.assertRefused("command_failed", "git-harness-head", checkout=unborn.path)
        self.assertRefused("receipt_commands_invalid", "git-harness-head", receipt={})
        self.assertRefused("build_path_unavailable", "build-candidate", environment={"HOME": "/"})
        with mock.patch.object(candidate_source_observation, "MAX_SOURCE_FILE", 8):
            self.assertRefused("command_output_exceeded", "git-registry-blob")

    def test_a_file_changed_or_removed_during_the_observation_refuses(self) -> None:
        fingerprint = source.fingerprint_sources
        helper = self.repository.path / HELPER
        for code, stage, change in (
                ("source_changed_during_observation", source.HARNESS_STAGE, lambda: helper.write_bytes(OTHER_HELPER)),
                ("source_file_invalid", "bounded-read", helper.unlink)):
            def fingerprint_then_change(root: Path, seeds: tuple[str, ...]) -> dict[str, object]:
                observed = fingerprint(root, seeds)
                change()
                return observed

            with self.subTest(code=code), \
                    mock.patch.object(source, "fingerprint_sources", side_effect=fingerprint_then_change):
                self.assertRefused(code, stage)
            helper.write_bytes(FILES[HELPER])


class ReplacementTests(HarnessCase):
    """HEAD commits other helper bytes; one test-owned replace ref presents the executing ones.

    The fixture Git commands honor replace refs, so each test first proves its
    ref presents the executing bytes at HEAD, the condition that made the
    unsanitized native observer report a committed match.
    """

    def setUp(self) -> None:
        super().setUp()
        self.executing_head = self.head
        self.head = self.repository.commit({HELPER: OTHER_HELPER}, "other helper bytes")
        (self.repository.path / HELPER).write_bytes(FILES[HELPER])

    def assertOriginalObjectsCompared(self) -> None:
        repository = self.repository
        self.assertEqual(repository.git("show", f"{self.head}:{HELPER}"), FILES[HELPER].decode().strip())
        before = self.git_state()
        self.assertIn("refs/replace/", before[2])
        self.assertEqual(self.observe(), (self.head, self.fingerprint(), False))
        self.assertEqual(self.git_state(), before)

    def test_a_blob_replacement_cannot_make_other_bytes_committed(self) -> None:
        repository = self.repository
        repository.git("replace", repository.git("rev-parse", f"{self.head}:{HELPER}"),
                       repository.git("hash-object", "-w", "--", HELPER))
        self.assertOriginalObjectsCompared()

    def test_a_tree_replacement_cannot_make_other_bytes_committed(self) -> None:
        repository = self.repository
        repository.git("replace", repository.tree_of(self.head), repository.tree_of(self.executing_head))
        self.assertOriginalObjectsCompared()

    def test_a_commit_replacement_cannot_make_other_bytes_committed(self) -> None:
        self.repository.git("replace", self.head, self.executing_head)
        self.assertOriginalObjectsCompared()


if __name__ == "__main__":
    unittest.main()
