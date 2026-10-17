"""Bounded native provider fixture text and artifact checks."""

from __future__ import annotations

import hashlib
from pathlib import Path
import secrets
import shlex

from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure, require
from scripts.delegate_acceptance.native.constants import MAX_RAW_BYTES


def bounded_text(path: Path, label: str, bound: int = MAX_RAW_BYTES) -> str:
    with path.open("rb") as stream:
        data = stream.read(bound + 1)
    require(len(data) <= bound, label + " exceeds the bounded output limit")
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure(label + " is not valid UTF-8") from error


def marker_answer(marker: str, continuation: bool) -> str:
    return f"DELEGATION_LIVE_{'CONTINUED' if continuation else 'OK'}: {marker}"


def random_marker() -> bytes:
    return ("DELEGATION-LIVE-" + secrets.token_hex(24) + "\n").encode()


def verify_read_only_answer(payload: bytes, expected: str, label: str) -> None:
    try:
        answer = payload.decode("utf-8")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure(f"{label} answer is not valid UTF-8") from error
    require(answer.strip() == expected,
            f"{label} did not return the exact marker-derived answer")


def verify_interactive_answer(payload: bytes, label: str) -> None:
    verify_read_only_answer(payload, "DELEGATION_LIVE_INTERACTIVE_OK: Blue", label)


def verify_review_answer(payload: bytes, label: str) -> None:
    try:
        answer = payload.decode("utf-8")
    except UnicodeDecodeError as error:
        raise AcceptanceFailure(f"{label} review answer is not valid UTF-8") from error
    require(bool(answer.strip()), f"{label} returned an empty review")


def verify_shell_digest_artifact(target: Path, artifact: Path, label: str) -> None:
    """Require an actual shell-produced digest bound to the final target bytes."""
    require(target.is_file() and not target.is_symlink(), f"{label} target is not a regular file")
    require(artifact.is_file() and not artifact.is_symlink(), f"{label} artifact is not a regular file")
    expected = hashlib.sha256(target.read_bytes()).hexdigest()
    fields = bounded_text(artifact, f"{label} artifact").split()
    require(len(fields) == 2 and fields[0] == expected and
            Path(fields[1].lstrip("*")).name == target.name,
            f"{label} artifact is not a digest of the final target")


def verify_write_artifacts(nonce: str, target: Path, artifact: Path,
                           continuation: bool = False) -> None:
    """Validate filesystem evidence, independent of provider response prose."""
    suffix = "\ncontinued" if continuation else ""
    require(target.read_bytes().removesuffix(b"\n") == f"edited:{nonce}{suffix}".encode(),
            "write task did not leave the exact edited target")
    verify_shell_digest_artifact(target, artifact, "write task")


def verify_write_fixture(nonce: str, fixture: Path) -> None:
    """Require the pre-existing fixture to have been edited to its final value."""
    require(fixture.is_file() and not fixture.is_symlink(),
            "write task fixture is not a regular file")
    require(fixture.read_bytes().removesuffix(b"\n") ==
            f"edited-fixture:{nonce}".encode(),
            "write task did not edit the pre-existing fixture")


def shell_digest_command(artifact: str) -> str:
    program = ('import hashlib; from pathlib import Path; '
               'p = Path("acceptance-write-target.txt"); '
               'print(hashlib.sha256(p.read_bytes()).hexdigest(), p.name)')
    return f"python3 -c {shlex.quote(program)} > {shlex.quote(artifact)}"


def shell_append_continuation_command() -> str:
    program = ('from pathlib import Path; p = Path("acceptance-write-target.txt"); '
               'data = p.read_bytes(); '
               'p.write_bytes(data + (b"" if data.endswith(b"\\n") else b"\\n") + b"continued\\n")')
    return f"python3 -c {shlex.quote(program)}"


def require_same_session(first: str, second: str) -> None:
    require(first == second, "continuation changed the provider session")


def require_replay_unchanged(before: dict[str, dict[str, object]],
                             after: dict[str, dict[str, object]]) -> None:
    require(before == after, "collection replay changed immutable evidence")
