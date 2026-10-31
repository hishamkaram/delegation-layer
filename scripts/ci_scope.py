"""Choose content-only CI only when every changed Git entry is recognized prose/artwork."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys


ROOT_PROSE = frozenset({"README.md", "CONTRIBUTING.md", "AGENTS.md"})
SKILL_DIRECTORIES = frozenset({
    "add-an-adapter", "agent-integration", "delegation-architect", "docs-updater",
    "go-concurrency", "go-implementer", "go-reviewer", "go-test-writer",
    "live-e2e", "supervisor-architect",
})
DIAGRAM_FILES = frozenset({
    "diagrams/SHA256SUMS", "diagrams/components.architecture.json",
    "diagrams/lead-interface.sequence.json",
    *(f"diagrams/{stem}.{suffix}" for stem in ("components", "lead-interface")
      for suffix in ("html", "light.svg", "dark.svg", "light.png", "dark.png")),
})
OBJECT_ID = re.compile(r"(?:[0-9a-f]{40}|[0-9a-f]{64})\Z")
RAW_HEADER = re.compile(
    rb":([0-7]{6}) ([0-7]{6}) (?:[0-9a-f]{40}|[0-9a-f]{64}) "
    rb"(?:[0-9a-f]{40}|[0-9a-f]{64}) ([A-Z])\Z")


class ScopeError(Exception):
    """Selection could not be established; required CI must refuse a safe skip."""


@dataclass(frozen=True)
class Change:
    path: str
    old_mode: str
    new_mode: str
    status: str


def content_path(path: str) -> bool:
    """One allowlist; unfamiliar files always retain the complete CI chain."""
    parts = PurePosixPath(path).parts
    if not parts or path.startswith("/") or any(part in ("", ".", "..") for part in path.split("/")):
        return False
    if path in ROOT_PROSE or path in DIAGRAM_FILES:
        return True
    if not path.endswith(".md"):
        return False
    if parts[0] == "docs":
        return len(parts) > 1 and parts[1] != "assets"
    if parts[0] == "agents":
        return len(parts) > 1
    if len(parts) > 2 and parts[0] == "skills":
        return parts[1] in SKILL_DIRECTORIES
    return len(parts) > 4 and parts[:4] == (
        "plugins", "delegation-layer", "skills", "agent-integration")


def requires_full(changes: tuple[Change, ...]) -> bool:
    if not changes:
        return True
    for change in changes:
        expected = {"A": ("000000", "100644"), "D": ("100644", "000000"),
                    "M": ("100644", "100644")}.get(change.status)
        if expected != (change.old_mode, change.new_mode) or not content_path(change.path):
            return True
    return False


def parse_changes(raw: bytes) -> tuple[Change, ...]:
    if not raw:
        return ()
    fields = raw.split(b"\0")
    if fields.pop() != b"" or len(fields) % 2:
        raise ScopeError("Git returned an incomplete change list")
    changes = []
    for index in range(0, len(fields), 2):
        match = RAW_HEADER.fullmatch(fields[index])
        if match is None or not fields[index + 1]:
            raise ScopeError("Git returned a malformed change entry")
        old_mode, new_mode, status = (field.decode("ascii") for field in match.groups())
        changes.append(Change(os.fsdecode(fields[index + 1]), old_mode, new_mode, status))
    return tuple(changes)


def git_output(repository: Path, *, arguments: tuple[str, ...]) -> bytes:
    environment = {**os.environ, "GIT_OPTIONAL_LOCKS": "0", "GIT_TERMINAL_PROMPT": "0"}
    try:
        result = subprocess.run(
            ["git", *arguments], cwd=repository, env=environment,
            stdin=subprocess.DEVNULL, capture_output=True, timeout=30, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ScopeError("Git change inspection could not complete") from error
    if result.returncode:
        raise ScopeError("Git change inspection failed; required history must be available")
    return result.stdout


def select_full(*, repository: Path, event: str, base: str, head: str) -> bool:
    if event == "workflow_dispatch":
        return True
    if event not in ("pull_request", "push"):
        return True
    for revision in (base, head):
        if OBJECT_ID.fullmatch(revision) is None or set(revision) == {"0"}:
            raise ScopeError("CI change range is missing a valid commit identity")
        git_output(repository, arguments=("rev-parse", "--verify", revision + "^{commit}"))
    start = base
    if event == "pull_request":
        start = git_output(repository, arguments=("merge-base", base, head)).decode("ascii").strip()
        if OBJECT_ID.fullmatch(start) is None:
            raise ScopeError("Git did not return a valid merge base")
    raw = git_output(repository, arguments=("diff", "--raw", "--no-abbrev", "-z", "--no-renames",
                                            "--no-ext-diff", "--no-textconv", start, head, "--"))
    return requires_full(parse_changes(raw))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--event", required=True)
    parser.add_argument("--base", default="")
    parser.add_argument("--head", default="")
    parser.add_argument("--repository", type=Path, default=Path.cwd())
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        full = select_full(repository=args.repository, event=args.event, base=args.base, head=args.head)
        with args.output.open("a", encoding="ascii") as output:
            output.write(f"full={str(full).lower()}\n")
    except (ScopeError, OSError, UnicodeError) as error:
        print(f"CI scope selection failed: {error}", file=sys.stderr)
        return 1
    print("CI scope: " + ("full" if full else "content-only"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
