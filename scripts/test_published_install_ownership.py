"""Fixture-tree ownership of the published-consumer harness and of the archive verifier's private HOME.

An injected fault times out one real child before it launches, below the tree's
observation and above the verifier's conversion, so the tree and the verifier
must both keep everything that child could still write to. Only the test's own
proof, that the fault launched nothing and every launched child closed, then
removes the retained tree.
"""

from __future__ import annotations

import contextlib
import io
import json
import os
from pathlib import Path
import sys
import unittest
from unittest import mock

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import test_published_install as base
from scripts.acceptance.testing.resources import open_descriptors
from scripts.acceptance.directories import operations as directories
from scripts.acceptance.processes import operations as operations
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.contracts import CommandFailure
from scripts.acceptance.evidence import source as commands
from scripts.test_candidate_source_support import ChildFault, settle_retained


class ArchiveVerifierHomeTests(base.PublishedBase):
    """The real archive verifier every published case drives in setUp, faulted inside its private HOME."""

    def setUp(self):
        self.open_fixture()
        self.prepare_archive()

    def test_an_unproven_exercise_child_retains_its_home_and_the_tree(self):
        homes: list[Path] = []

        def marker_then_fault(argv: list[str]) -> bool:
            if argv[:1] != ["delegate"] or homes:
                return False
            homes.extend(self.assets.glob("delegate-release-home-*"))
            (homes[0] / "marker").write_bytes(b"written before the unknown Wait\n")
            return True

        self.children = ChildFault(marker_then_fault)
        self.assertEqual(self.verify_archive(), 1)
        self.assertIs(type(self.tree.unresolved), BoundedCommandError)
        assert isinstance(self.tree.unresolved, BoundedCommandError)
        self.assertIs(self.tree.unresolved.reason, CommandFailure.TIMEOUT)
        receipt = json.loads(self.archive_receipt.read_text(encoding="utf-8"))
        self.assertEqual((receipt["failure"]["code"], receipt["failure"]["stage"]),
                         ("command_timeout", "delegate-version"))
        self.assertEqual(len(homes), 1)
        self.assertIn(str(homes[0]), receipt["cleanup"]["retained_paths"])
        self.assertNotIn(str(homes[0]), receipt["cleanup"]["removed_paths"])
        with contextlib.redirect_stderr(io.StringIO()):
            self.doCleanups()
        settle_retained(self, self.tree, self.children, (homes[0] / "marker", self.assets))

    def test_a_home_that_cannot_be_removed_after_a_closed_exercise_is_a_reported_failure(self):
        self.doCleanups()
        baseline = open_descriptors()
        self.setUp()
        real_remove = directories._remove_entry_at
        held_assets = self.assets.stat()
        refused: list[str] = []

        def remove(parent_fd: int, name: str, expected_identity: tuple[int, int], *args, **options):
            # Refuse only the private HOME below the assets directory; the real rollback still consumes its pins.
            parent = os.fstat(parent_fd)
            if ((parent.st_dev, parent.st_ino) == (held_assets.st_dev, held_assets.st_ino)
                    and name.startswith("delegate-release-home-")):
                home = (self.assets / name).stat()
                if expected_identity == (home.st_dev, home.st_ino):
                    refused.append(name)
                    raise PermissionError("injected HOME removal refusal")
            return real_remove(parent_fd, name, expected_identity, *args, **options)

        before = open_descriptors()
        with mock.patch.object(directories, "_remove_entry_at", remove):
            self.assertEqual(self.verify_archive(), 1)
        # Native acceptance releases the outer assets owner, so only the private HOME was removed.
        self.assertEqual(open_descriptors(), before)
        receipt = json.loads(self.archive_receipt.read_text(encoding="utf-8"))
        self.assertEqual((receipt["failure"]["code"], receipt["failure"]["stage"]),
                         ("release_cleanup_failed", "cleanup-isolated-home"))
        home = receipt["default_path_observation"]["isolated_home"]
        self.assertEqual(refused, [Path(home).name])
        self.assertTrue(Path(home).is_dir())
        self.assertIn(home, receipt["cleanup"]["retained_paths"])
        self.assertEqual((self.children.faulted, self.children.unclosed), ([], 0))
        self.assertIsNone(self.tree.unresolved)
        root = self.tree.root
        self.doCleanups()
        self.assertFalse(root.exists())
        self.assertEqual(open_descriptors(), baseline)

    def test_a_replaced_home_keeps_the_primary_fault_and_reports_one_identity_error(self):
        moved: list[Path] = []

        def replace_then_fault(argv: list[str]) -> bool:
            if argv[:1] != ["delegate"] or moved:
                return False
            [home] = self.assets.glob("delegate-release-home-*")
            (home / "marker").write_bytes(b"original HOME\n")
            original = home.with_name(home.name + "-original")
            home.rename(original)
            home.mkdir()
            (home / "replacement").write_bytes(b"replacement HOME\n")
            moved.extend((original, home))
            return True

        self.children = ChildFault(replace_then_fault)
        before = open_descriptors()
        self.assertEqual(self.verify_archive(), 1)
        self.assertEqual(open_descriptors(), before)
        self.assertIs(type(self.tree.unresolved), BoundedCommandError)
        assert isinstance(self.tree.unresolved, BoundedCommandError)
        self.assertIs(self.tree.unresolved.reason, CommandFailure.TIMEOUT)
        receipt = json.loads(self.archive_receipt.read_text(encoding="utf-8"))
        self.assertEqual((receipt["failure"]["code"], receipt["failure"]["stage"]),
                         ("command_timeout", "delegate-version"))
        self.assertEqual(receipt["cleanup"]["errors"], ["owned_directory_identity_check_failed"])
        original, home = moved
        self.assertIn(str(home), receipt["cleanup"]["retained_paths"])
        self.assertIn(str(self.assets), receipt["cleanup"]["retained_paths"])
        with contextlib.redirect_stderr(io.StringIO()):
            self.doCleanups()
        self.assertEqual((original / "marker").read_bytes(), b"original HOME\n")
        self.assertEqual((home / "replacement").read_bytes(), b"replacement HOME\n")
        settle_retained(self, self.tree, self.children, (original / "marker", home / "replacement", self.assets))


class PublishedChildOwnershipTests(base.PublishedBase):
    def test_an_unproven_command_child_retains_the_workspace_and_the_tree(self):
        self.children = ChildFault(lambda argv: argv[:1] == ["delegate"])
        self.assertEqual(self.publish(), 1)
        self.assertIs(type(self.tree.unresolved), BoundedCommandError)
        assert isinstance(self.tree.unresolved, BoundedCommandError)
        self.assertIs(self.tree.unresolved.reason, CommandFailure.TIMEOUT)
        receipt = self.receipt()
        self.assertEqual((receipt["failure"]["code"], receipt["failure"]["stage"]),
                         ("command_timeout", "delegate-version"))
        workspaces = self.workspaces()
        self.assertEqual(len(workspaces), 1)
        self.assertEqual(receipt["published"]["unresolved_ownership"]["workspace"], str(workspaces[0]))
        self.assertIn(str(workspaces[0]), receipt["cleanup"]["retained_paths"])
        with contextlib.redirect_stderr(io.StringIO()):
            self.doCleanups()
        settle_retained(self, self.tree, self.children, (workspaces[0], self.receipt_path))

    def test_a_closed_run_removes_its_tree_and_restores_every_patch(self):
        self.doCleanups()
        environment, descriptors = dict(os.environ), open_descriptors()
        self.assertIs(commands.run_bounded_command, operations.run_bounded_command)
        self.setUp()
        first = self.tree.root
        self.reset()
        self.assertFalse(first.exists())
        self.assertEqual(os.environ["HOME"], str(self.tree.root))
        self.assertEqual(self.publish(), 0)
        self.assertGreater(self.children.launched, 0)
        self.assertEqual((self.children.faulted, self.children.unclosed), ([], 0))
        root = self.tree.root
        self.doCleanups()
        self.assertFalse(root.exists())
        self.assertEqual(dict(os.environ), environment)
        self.assertIs(commands.run_bounded_command, operations.run_bounded_command)
        self.assertEqual(open_descriptors(), descriptors)


if __name__ == "__main__":
    unittest.main()
