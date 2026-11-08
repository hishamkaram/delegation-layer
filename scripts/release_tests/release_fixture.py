"""The one test-owned containing tree for release fixtures and its disposition policy."""

from __future__ import annotations

import sys
from pathlib import Path

if __package__ in {None, ""}:
    _ENTRY_ROOT = str(Path(__file__).resolve().parents[2])
    sys.path[:] = [_ENTRY_ROOT, *(value for value in sys.path[1:] if value != _ENTRY_ROOT)]


import secrets
import tempfile
from collections.abc import Callable
from typing import TypeVar

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.directories.operations import OwnedDirectory
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.published_contract import UNRESOLVED_OWNERSHIP_CODES


# Path, parent, then the (device, inode) pins of the directory and of its parent.
DirectoryIdentity = tuple[Path, Path, tuple[int, int], tuple[int, int]]

T = TypeVar("T")


def directory_identity(token: OwnedDirectory) -> DirectoryIdentity:
    return (token.directory, token.parent, token.identity, token.parent_identity)


class FixtureTree:
    """The one test-owned containing tree and the single policy that disposes of it.

    Owner: this object (and its DirectoryOwner, which pins the root and every workspace
    created below it). Mutators: observe and observe_operation (children), dispose and
    remove_retained. Invariant:
    after dispose every pin is consumed; the tree is removed unless a child's closure was
    unresolved, in which case the whole tree is retained as evidence. Only remove_retained,
    called after the test's own proof that no child can still write, deletes a retained tree.

    Callers run each test-owned child operation through observe_operation, so the original
    error reaches the tree before any caller converts or sanitizes it. A bounded-command or
    verifier error is unresolved only for the canonical ownership codes; any other exception never. Where the caller requires a
    closure proof, any exception means that proof did not complete, so it is unresolved.
    The first unresolved original error is kept as is and never cleared or replaced, so a
    broad incomplete error may safely over-retain but never under-retains.
    """

    def __init__(self, owner: DirectoryOwner, root: Path, label: str,
                 identity: DirectoryIdentity):
        self.owner = owner
        self.root = root
        self.label = label
        self._identity = identity
        self.unresolved: BaseException | None = None
        self._removed: bool | None = None

    @classmethod
    def open(cls, parent: Path | None = None, *, prefix: str, label: str) -> FixtureTree:
        """Create ``<prefix>-<random>`` under ``parent`` (default: the system temp directory)."""
        base = (parent or Path(tempfile.gettempdir())).resolve(strict=True)
        root = base / f"{prefix}-{secrets.token_hex(6)}"
        owner = DirectoryOwner()
        token = owner.create(label, root, base)
        return cls(owner, root, label, directory_identity(token))

    def observe(self, error: BaseException, *, closure_required: bool = False) -> None:
        """Record the first failure whose process closure is not proven.

        With ``closure_required`` the failure interrupted a required closure proof, so any
        exception is unresolved; otherwise only an unproven child failure is.
        """
        _require_closure_flag(closure_required)
        if not isinstance(error, BaseException):
            raise TypeError("observe error must be the original exception")
        if self.unresolved is not None:
            return
        if closure_required:
            unproven = True
        elif isinstance(error, (BoundedCommandError, VerificationError)):
            unproven = _failure_code(error) in UNRESOLVED_OWNERSHIP_CODES
        else:
            unproven = False
        if unproven:
            self.unresolved = error

    def observe_operation(self, operation: Callable[[], T], *,
                          dependencies: tuple[FixtureTree, ...] = (),
                          closure_required: bool = False) -> T:
        """Run one child operation exactly once; observe its original failure, then re-raise it.

        ``dependencies`` are the other trees the child uses as input or is contained by; each
        observes the same original failure, under the same ``closure_required``, and keeps its
        own first unresolved fact. Success never clears a fact.
        """
        _require_closure_flag(closure_required)
        if not isinstance(dependencies, tuple) or not all(
                isinstance(tree, FixtureTree) for tree in dependencies):
            raise TypeError("observe_operation dependencies must be a tuple of FixtureTree owners")
        try:
            return operation()
        except BaseException as error:
            self.observe(error, closure_required=closure_required)
            for tree in dependencies:
                tree.observe(error, closure_required=closure_required)
            raise

    def dispose(self) -> bool:
        """Consume the pins, then remove the tree (True) or retain it as evidence (False)."""
        if self._removed is not None:
            return self._removed
        if self.unresolved is None:
            self.owner.rollback()
            self._removed = True
        else:
            self.owner.release()
            self._removed = False
            print(f"{self.label} retained ({_failure_code(self.unresolved)}): {self.root}",
                  file=sys.stderr)
        return self._removed

    def remove_retained(self) -> None:
        """Remove the retained tree by its created identity; never clears the unresolved fact.

        The caller must first prove, independently of the tree, that no child can still
        write to it. A replaced directory is refused and left untouched.
        """
        if self._removed is not False:
            raise RuntimeError(f"fixture tree is not retained: {self.root}")
        owner = DirectoryOwner()
        token = owner.adopt_created(self.label, self.root, self.root.parent)
        if directory_identity(token) != self._identity:
            owner.release()
            raise RuntimeError(f"retained fixture tree was replaced: {self.root}")
        owner.rollback()


def _require_closure_flag(closure_required: object) -> None:
    if type(closure_required) is not bool:
        raise TypeError("closure_required must be a bool")


def _failure_code(error: BaseException) -> str:
    """The stable code of one failure; any other exception is named by its type alone.

    The type name never carries the message, arguments, paths or traceback of the error.
    """
    if isinstance(error, BoundedCommandError):
        return error.reason.value
    if isinstance(error, VerificationError):
        return error.code
    return type(error).__name__
