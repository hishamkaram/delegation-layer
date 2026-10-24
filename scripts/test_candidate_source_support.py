"""Fixture Git sources and child-fault lifetime proofs shared by the candidate record command tests.

Nothing here owns state. ``FixtureTree`` owns every directory, the
classification of child failures and their disposition; the canonical process
runner owns each child's completion; ``test_candidate_build_support.git`` runs
each fixture Git command and forwards its unproven failures to the tree. These
helpers only build fixture histories and record the test's own observations
of which children ran.
"""

from __future__ import annotations

from collections.abc import Callable
import contextlib
import io
from pathlib import Path
import unittest

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, BoundedCommandResult, CommandFailure
from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.test_candidate_build_support import git


class SourceRepository:
    """One fixture Git repository, new or cloned, whose linear history the case commits."""

    def __init__(self, tree: FixtureTree, name: str, *, clone_of: SourceRepository | None = None):
        self.tree = tree
        self.path = tree.root / name
        if clone_of is None:
            self.path.mkdir()
            self.git("init", "--quiet", "--initial-branch", "main")
        else:
            git(tree.root, "clone", "--quiet", "--", str(clone_of.path), str(self.path), tree=tree)

    def git(self, *arguments: str) -> str:
        return git(self.path, *arguments, tree=self.tree)

    def commit(self, files: dict[str, bytes], message: str) -> str:
        for relative, data in files.items():
            target = self.path / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
        self.git("add", "--all")
        self.git("commit", "--quiet", "-m", message)
        return self.git("rev-parse", "HEAD")

    def tree_of(self, commit: str) -> str:
        return self.git("rev-parse", f"{commit}^{{tree}}")


class ChildFault:
    """Times out each command ``matches`` selects without launching it; every other command runs for real.

    A delegated command returns only after the canonical process owner proved
    its child's closure, so ``launched`` and ``unclosed`` are the test's own
    proof of which children ran and that none can still write. A child that
    raises stays unclosed.
    """

    def __init__(self, matches: Callable[[list[str]], bool]):
        self.matches = matches
        self.faulted: list[list[str]] = []
        self.launched = 0
        self.unclosed = 0

    def __call__(self, argv: list[str], environment: dict[str, str], **options: object) -> BoundedCommandResult:
        if self.matches(argv):
            self.faulted.append(argv)
            raise BoundedCommandError(CommandFailure.TIMEOUT)
        self.launched += 1
        self.unclosed += 1
        result = run_bounded_command(argv, environment, **options)
        self.unclosed -= 1
        return result


def settle_retained(case: unittest.TestCase, tree: FixtureTree, fault: ChildFault, resources: tuple[Path, ...]) -> None:
    """The tree keeps every resource after an unproven child; only the test's own proof then removes it."""
    with contextlib.redirect_stderr(io.StringIO()):
        case.assertFalse(tree.dispose())
    for path in resources:
        case.assertTrue(path.exists(), path)
    # The proof: the one faulted read never launched, and every launched child closed.
    case.assertEqual((len(fault.faulted), fault.unclosed), (1, 0))
    tree.remove_retained()
    case.assertFalse(tree.root.exists())
