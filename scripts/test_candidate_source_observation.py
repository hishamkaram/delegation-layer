"""The source observer reads exact Git objects of a real local fixture repository.

Fixture constants (paths, file bytes and the reviewed rationale) are test
inputs only; nothing here is evidence about the real repository or a provider.
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import sys
import unittest
from unittest import mock

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.release_verification import candidate_source_observation as observation
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    ChangeScope, MatrixScope, ReviewedChangeScope,
)
from scripts.delegate_acceptance.release_verification.candidate_build_source import child_environment
from scripts.delegate_acceptance.release_verification.candidate_manifest import SourceIdentity
from scripts.delegate_acceptance.release_verification.candidate_source_inventory import (
    REGISTRY_PATH, reviewed_change_scope,
)
from scripts.delegate_acceptance.release_verification.contract import MAX_OUTPUT, VerificationError
from scripts.test_candidate_build_support import git


FRAGMENT_A = "agents/_data/subsystems/a.json"
FRAGMENT_B = "agents/_data/subsystems/b.json"
RATIONALE = "Fixture review: only shared files and the registry changed."
BASE_FILES = {
    "keep.txt": b"keep\n", "remove.txt": b"remove\n", "rename-me.txt": b"rename\n",
    FRAGMENT_A: b"[\"a\"]\n", FRAGMENT_B: b"[\"b\"]\n",
}
HEAD_FILES = {
    "keep.txt": b"kept and changed\n", "renamed.txt": b"rename\n", " lead": b"1\n", "trail ": b"2\n",
    "new\nline": b"3\n", "tab\there": b"4\n", "dir/added.txt": b"5\n",
    FRAGMENT_A: b"[\"a\", \"changed\"]\n", FRAGMENT_B: b"[\"b\"]\n",
}
# Byte order, written out: additions, deletions, edits and both sides of the rename.
HEAD_CHANGED = (" lead", FRAGMENT_A, "dir/added.txt", "keep.txt", "new\nline", "remove.txt", "rename-me.txt",
                "renamed.txt", "tab\there", "trail ")
# Enough added names that the complete inventory exceeds the default 64 KiB capture bound.
WIDE_FILES = tuple(f"wide/{index:04d}-{'n' * 120}" for index in range(600))


def master(*files: str) -> bytes:
    return json.dumps({"version": 1, "module": "fixture", "module_files": list(files)}).encode() + b"\n"


def registry_sha256(files: dict[str, bytes]) -> str:
    """The documented registry encoding, computed from the fixture's own bytes."""
    encoded = b"".join(path.encode() + b"\0" + hashlib.sha256(files[path]).hexdigest().encode() + b"\0"
                       for path in sorted(files, key=str.encode))
    return hashlib.sha256(encoded).hexdigest()


class SourceRepository:
    """One linear fixture history: base, head and registry variants, each a descendant of base."""

    def __init__(self, tree: FixtureTree):
        self.tree = tree
        self.path = tree.root / "repository"
        self.path.mkdir()
        self.git("init", "--quiet", "--initial-branch", "main")
        self.base = self.commit({**BASE_FILES, REGISTRY_PATH: master(FRAGMENT_A, FRAGMENT_B)}, "base")
        self.git("tag", "--annotate", "--message", "base tag", "base-tag", self.base)
        self.base_tag = self.git("rev-parse", "base-tag")
        self.head = self.commit({**HEAD_FILES, REGISTRY_PATH: master(FRAGMENT_A, FRAGMENT_B)}, "head")
        self.fragment_only = self.commit({**HEAD_FILES, FRAGMENT_B: b"[\"b\", \"edited\"]\n",
                                          REGISTRY_PATH: master(FRAGMENT_A, FRAGMENT_B)}, "fragment")
        self.missing = self.commit({**HEAD_FILES, REGISTRY_PATH: master(FRAGMENT_A, "agents/_data/missing.json")},
                                   "missing")
        self.symlink = self.commit({**HEAD_FILES, REGISTRY_PATH: master("agents/_data/link.json")}, "symlink",
                                   links={"agents/_data/link.json": "subsystems/a.json"})
        self.linked_directory = self.commit(
            {**HEAD_FILES, REGISTRY_PATH: master("agents/_data/linked/a.json")}, "linked directory",
            links={"agents/_data/linked": "subsystems"})
        self.directory = self.commit({**HEAD_FILES, REGISTRY_PATH: master("agents/_data/subsystems")},
                                     "directory")
        self.wide = self.commit({**HEAD_FILES, **{path: b"w\n" for path in WIDE_FILES},
                                 REGISTRY_PATH: master(FRAGMENT_A, FRAGMENT_B)}, "wide")
        self.unrelated = self.git("commit-tree", self.tree_of(self.base), "-m", "unrelated root")

    def git(self, *arguments: str) -> str:
        return git(self.path, *arguments, tree=self.tree)

    def tree_of(self, commit: str) -> str:
        return self.git("rev-parse", f"{commit}^{{tree}}")

    def commit(self, files: dict[str, bytes], message: str, *, links: dict[str, str] | None = None) -> str:
        """Replace the tracked content with exactly ``files`` and ``links``, then commit it."""
        self.git("rm", "-r", "--quiet", "--cached", "--ignore-unmatch", ".")
        for entry in self.path.iterdir():
            if entry.name != ".git":
                self._remove(entry)
        for relative, data in files.items():
            target = self.path / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
        for relative, destination in (links or {}).items():
            target = self.path / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.symlink_to(destination)
        self.git("add", "--all")
        self.git("commit", "--quiet", "--allow-empty", "-m", message)
        return self.git("rev-parse", "HEAD")

    def _remove(self, entry: Path) -> None:
        if entry.is_dir() and not entry.is_symlink():
            for child in entry.iterdir():
                self._remove(child)
            entry.rmdir()
        else:
            entry.unlink()

    def identity(self, commit: str, *, base: str | None = None) -> SourceIdentity:
        return SourceIdentity(commit=commit, tree=self.tree_of(commit), base_commit=base or self.base)

    def snapshot(self) -> tuple[bytes, bytes, str, dict[str, bytes]]:
        """HEAD, index and ref bytes plus every worktree file, read without running Git porcelain."""
        files = {}
        for directory, _, names in os.walk(self.path):
            if ".git" in Path(directory).relative_to(self.path).parts:
                continue
            for name in names:
                path = Path(directory, name)
                files[str(path.relative_to(self.path))] = (os.readlink(path).encode() if path.is_symlink()
                                                           else path.read_bytes())
        git_directory = self.path / ".git"
        return ((git_directory / "HEAD").read_bytes(), (git_directory / "index").read_bytes(),
                self.git("for-each-ref"), files)


class SourceObservationTests(unittest.TestCase):
    repository: SourceRepository

    @classmethod
    def setUpClass(cls):
        tree = FixtureTree.open(prefix="candidate-source-observation", label="candidate source observation")
        cls.addClassCleanup(tree.dispose)
        cls.repository = SourceRepository(tree)

    def observe(self, source: SourceIdentity, *, repository: Path | None = None, receipt: dict | None = None):
        try:
            return observation.observe_source_change(
                {"commands": []} if receipt is None else receipt, child_environment(os.environ),
                repository=repository or self.repository.path, source=source)
        except VerificationError as error:
            self.repository.tree.observe(error)
            raise

    def assertRefused(self, code: str, stage: str, source: SourceIdentity, **kwargs) -> None:
        with self.assertRaises(VerificationError) as caught:
            self.observe(source, **kwargs)
        self.assertEqual((caught.exception.code, caught.exception.stage), (code, stage))

    def test_observation_binds_complete_exact_diff_and_registry(self):
        repository = self.repository
        observed = self.observe(repository.identity(repository.head))
        self.assertEqual(observed.changed_paths.paths, HEAD_CHANGED)
        expected = b"".join(path.encode() + b"\0" for path in HEAD_CHANGED)
        self.assertEqual(observed.changed_paths.encoded, expected)
        self.assertEqual(observed.base_tree, repository.tree_of(repository.base))
        registry = registry_sha256({REGISTRY_PATH: master(FRAGMENT_A, FRAGMENT_B),
                                    FRAGMENT_A: HEAD_FILES[FRAGMENT_A], FRAGMENT_B: HEAD_FILES[FRAGMENT_B]})
        change = ChangeScope(matrix=MatrixScope.SHARED, changed_providers=(), provider_imports=())
        self.assertEqual(reviewed_change_scope(observed, scope=change, rationale=RATIONALE), ReviewedChangeScope(
            scope=change, base_commit=repository.base, base_tree=repository.tree_of(repository.base),
            changed_path_count=len(HEAD_CHANGED), changed_paths_sha256=hashlib.sha256(expected).hexdigest(),
            subsystem_registry_sha256=registry, rationale=RATIONALE))

    def test_complete_inventory_above_the_default_capture_bound(self):
        repository = self.repository
        receipt = {"commands": []}
        try:
            observed = self.observe(repository.identity(repository.wide), receipt=receipt)
        except VerificationError as error:
            self.fail(f"complete inventory was refused: {error.code} at {error.stage}")
        self.assertEqual(observed.changed_paths.paths, tuple(sorted((*HEAD_CHANGED, *WIDE_FILES), key=str.encode)))
        self.assertGreater(len(observed.changed_paths.encoded), MAX_OUTPUT)
        diff = [record for record in receipt["commands"] if record["name"] == "git-source-changed-paths"]
        self.assertEqual([record["exit_code"] for record in diff], [0])

    def test_empty_diff_has_the_empty_inventory(self):
        observed = self.observe(self.repository.identity(self.repository.base))
        self.assertEqual((observed.changed_paths.count, observed.changed_paths.encoded), (0, b""))

    def test_reads_leave_head_index_refs_and_worktree_unchanged(self):
        repository = self.repository
        decoy = repository.path / FRAGMENT_A
        original = decoy.read_bytes()
        self.addCleanup(decoy.write_bytes, original)
        decoy.write_bytes(b"[\"uncommitted\"]\n")
        before = repository.snapshot()
        observed = self.observe(repository.identity(repository.head))
        self.assertEqual(repository.snapshot(), before)
        self.assertEqual({item.path: item.sha256 for item in observed.registry.files}.get(FRAGMENT_A),
                         hashlib.sha256(HEAD_FILES[FRAGMENT_A]).hexdigest())

    def test_fragment_change_alone_changes_the_registry_fingerprint(self):
        repository = self.repository
        head = self.observe(repository.identity(repository.head)).registry
        edited = self.observe(repository.identity(repository.fragment_only)).registry
        self.assertNotEqual(edited.sha256, head.sha256)
        master_digest = {item.path: item.sha256 for item in head.files}[REGISTRY_PATH]
        self.assertEqual({item.path: item.sha256 for item in edited.files}[REGISTRY_PATH], master_digest)

    def test_source_identity_mismatches_refuse(self):
        repository = self.repository
        head = repository.identity(repository.head)
        cases = (
            ("source_tree_mismatch", "observe-candidate-source",
             SourceIdentity(commit=head.commit, tree=repository.tree_of(repository.base), base_commit=head.base_commit)),
            ("source_base_mismatch", "observe-candidate-source", repository.identity(repository.head,
                                                                                      base=repository.base_tag)),
            ("command_failed", "git-source-base", repository.identity(repository.head, base="0" * 40)),
            ("command_failed", "git-source-commit",
             SourceIdentity(commit=head.tree, tree=head.tree, base_commit=head.base_commit)),
            ("command_failed", "git-source-base-ancestor",
             repository.identity(repository.head, base=repository.unrelated)),
            ("command_failed", "git-source-base-ancestor",
             repository.identity(repository.base, base=repository.head)),
        )
        for code, stage, source in cases:
            with self.subTest(code=code, stage=stage):
                self.assertRefused(code, stage, source)

    def test_registry_reference_refusals_use_the_commit_not_the_checkout(self):
        repository = self.repository
        decoy = repository.path / "agents/_data/missing.json"
        decoy.write_bytes(b"[]\n")
        self.addCleanup(decoy.unlink)
        for commit, code in ((repository.missing, "registry_file_missing"),
                             (repository.symlink, "registry_file_not_regular"),
                             (repository.linked_directory, "registry_file_missing"),
                             (repository.directory, "registry_file_not_regular")):
            with self.subTest(code=code, commit=commit):
                self.assertRefused(code, "observe-candidate-source", repository.identity(commit))

    def test_git_failure_and_output_overflow_refuse_without_truncation(self):
        repository = self.repository
        not_git = repository.tree.root / "not-a-repository"
        not_git.mkdir(exist_ok=True)
        self.assertRefused("command_failed", "git-source-commit", repository.identity(repository.head),
                           repository=not_git)
        receipt = {"commands": []}
        with mock.patch.object(observation, "MAX_CHANGED_PATHS_OUTPUT", 16):
            self.assertRefused("command_output_exceeded", "git-source-changed-paths",
                               repository.identity(repository.head), receipt=receipt)
        self.assertEqual(receipt["commands"][-1]["name"], "git-source-changed-paths")
        with mock.patch.object(observation, "MAX_SOURCE_FILE", 8):
            self.assertRefused("command_output_exceeded", "git-registry-blob", repository.identity(repository.head))
        self.assertRefused("receipt_commands_invalid", "git-source-commit", repository.identity(repository.head),
                           receipt={})

    def read_blobs(self, receipt: dict, tree: str) -> dict[str, bytes]:
        try:
            return observation.read_regular_blobs(receipt, child_environment(os.environ),
                                                  repository=self.repository.path, tree=tree, paths=(FRAGMENT_A,))
        except VerificationError as error:
            self.repository.tree.observe(error)
            raise

    def test_blob_reads_accept_only_full_object_ids(self):
        repository = self.repository
        for object_id in (repository.head, repository.tree_of(repository.head)):
            with self.subTest(object_id=object_id):
                self.assertEqual(self.read_blobs({"commands": []}, object_id), {FRAGMENT_A: HEAD_FILES[FRAGMENT_A]})
        for expression in ("HEAD", "main", "base-tag", repository.head[:12], f"{repository.head}^{{tree}}"):
            receipt = {"commands": []}
            with self.subTest(expression=expression), self.assertRaises(VerificationError) as caught:
                self.read_blobs(receipt, expression)
            self.assertEqual((caught.exception.code, caught.exception.stage),
                             ("candidate_field_invalid", "observe-candidate-source"))
            self.assertEqual(receipt["commands"], [])

    def test_unsupported_source_is_refused_before_any_command(self):
        receipt = {"commands": []}
        with self.assertRaises(VerificationError) as caught:
            observation.observe_source_change(receipt, child_environment(os.environ),
                                              repository=self.repository.path, source=None)
        self.assertEqual(caught.exception.code, "source_observation_invalid")
        self.assertEqual(receipt["commands"], [])


class ReplacementObservationTests(unittest.TestCase):
    """Replace refs in the repository never change the facts of the exact object IDs observed.

    Each test adds one test-owned ``refs/replace`` ref to its own fixture
    repository, observes with that ref present, and deletes only that ref.
    """

    repository: SourceRepository

    @classmethod
    def setUpClass(cls):
        tree = FixtureTree.open(prefix="candidate-source-replacement", label="candidate source replacement")
        cls.addClassCleanup(tree.dispose)
        cls.repository = SourceRepository(tree)

    def setUp(self):
        repository = self.repository
        self.head = repository.identity(repository.head)
        # Read before any replace ref exists; fixture Git commands honor replace refs.
        self.base_tree = repository.tree_of(repository.base)
        self.ordinary = self.observe()
        self.expected_registry = registry_sha256({
            REGISTRY_PATH: master(FRAGMENT_A, FRAGMENT_B), FRAGMENT_A: HEAD_FILES[FRAGMENT_A],
            FRAGMENT_B: HEAD_FILES[FRAGMENT_B]})

    def observe(self):
        try:
            return observation.observe_source_change({"commands": []}, child_environment(os.environ),
                                                     repository=self.repository.path, source=self.head)
        except VerificationError as error:
            self.repository.tree.observe(error)
            self.fail(f"exact objects were refused under a replace ref: {error.code} at {error.stage}")

    def replace(self, original: str, replacement: str) -> None:
        self.repository.git("replace", original, replacement)
        self.addCleanup(self.repository.git, "replace", "--delete", original)

    def assertOriginalFacts(self) -> None:
        repository = self.repository
        self.assertEqual(self.ordinary.changed_paths.paths, HEAD_CHANGED)
        self.assertEqual(self.ordinary.registry.sha256, self.expected_registry)
        before = repository.snapshot()
        self.assertIn("refs/replace/", before[2])
        observed = self.observe()
        self.assertEqual(repository.snapshot(), before)
        self.assertEqual(observed, self.ordinary)
        self.assertEqual(observed.base_tree, self.base_tree)
        self.assertEqual(observed.changed_paths.paths, HEAD_CHANGED)
        self.assertEqual(observed.registry.sha256, self.expected_registry)

    def test_blob_replacement_cannot_change_the_registry_fingerprint(self):
        repository = self.repository
        decoy = repository.tree.root / "replacement-fragment.json"
        decoy.write_bytes(b"[\"replaced\"]\n")
        self.replace(repository.git("rev-parse", f"{repository.head}:{FRAGMENT_A}"),
                     repository.git("hash-object", "-w", str(decoy)))
        self.assertOriginalFacts()

    def test_tree_replacement_cannot_change_the_diff_or_registry(self):
        repository = self.repository
        self.replace(self.head.tree, repository.tree_of(repository.base))
        self.assertOriginalFacts()

    def test_commit_replacement_cannot_change_the_observed_base(self):
        repository = self.repository
        rootless_head = repository.git("commit-tree", self.head.tree, "-m", "replacement base")
        self.replace(repository.base, rootless_head)
        self.assertOriginalFacts()


if __name__ == "__main__":
    unittest.main()
