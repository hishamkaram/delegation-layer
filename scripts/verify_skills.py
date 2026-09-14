#!/usr/bin/env python3
"""Validate the repository's deliberately small skill format.

This checker is intentionally narrower than YAML or Markdown.  It validates
the two-line metadata contract, required repository skills, local links,
explicit scaffold markers, and command-shaped ``make`` references.  It does
not attempt to render Markdown or infer the quality of a skill's prose.
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import re
from pathlib import Path
import sys
from typing import Iterable, Iterator, Sequence


REQUIRED_SKILLS: tuple[str, ...] = (
    "delegation-architect",
    "go-implementer",
    "go-reviewer",
    "go-test-writer",
    "live-e2e",
    "go-concurrency",
    "docs-updater",
    "add-an-adapter",
)

NAME_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
FRONTMATTER_FIELD_RE = re.compile(r"^(name|description):[ \t]+(.*)$")
MARKDOWN_LINK_RE = re.compile(r"(?<!!)\[[^\]\n]*\]\(([^)\n]*)\)")
MAKE_COMMAND_RE = re.compile(
    r"(?:^|[^A-Za-z0-9_./-])(?:\$[ \t]*)?make[ \t]+"
    r"(?P<target>[A-Za-z0-9][A-Za-z0-9_.-]*)"
    r"(?=[ \t]*(?:&&|\|\||[;,]|[`'\"\)\]\}.,:;!?#]|$))"
)
COMMAND_LINE_RE = re.compile(r"^(?:[-*+]\s+)?(?:\$[ \t]*)?make[ \t]+", re.IGNORECASE)
PLANNED_COMMAND_RE = re.compile(
    r"(?:\[\s*planned\s*\]|\(\s*planned\s*\))", re.IGNORECASE
)
PLACEHOLDER_RE = re.compile(
    r"(?<![A-Za-z0-9_])(?:TODO|FIXME|TBD|PLACEHOLDER)(?![A-Za-z0-9_])"
    r"|<YOUR_[A-Z0-9_]+>"
    r"|\{\{[A-Z0-9_.-]+\}\}"
)
FRONTMATTER_END = "---"


@dataclass(frozen=True)
class SkillDocument:
    path: Path
    name: str
    description: str
    body: str
    body_start_line: int


def _unquote_scalar(raw: str) -> tuple[str | None, str | None]:
    """Decode the small scalar subset accepted by the metadata contract."""

    value = raw.strip()
    if not value:
        return None, "must be non-blank"
    if value.startswith('"'):
        if len(value) < 2 or not value.endswith('"'):
            return None, "has an unterminated double-quoted value"
        # The metadata contract does not need escape processing.  Reject it
        # rather than silently accepting a value unlike the documented format.
        inner = value[1:-1]
        if '"' in inner or "\\" in inner:
            return None, "uses unsupported escapes or embedded double quotes"
        return inner, None
    if value.startswith("'"):
        if len(value) < 2 or not value.endswith("'"):
            return None, "has an unterminated single-quoted value"
        inner = value[1:-1]
        if "'" in inner:
            return None, "uses an embedded single quote unsupported by this format"
        return inner, None
    # A colon followed by whitespace starts another YAML mapping entry.  The
    # simple unquoted form therefore rejects it and asks authors to quote it.
    if re.search(r":[ \t]", value):
        return None, "must be quoted when it contains ': '"
    # Keep plain values strings instead of accepting YAML's collection,
    # boolean, null, numeric, date, tag, or comment forms without a YAML
    # parser. Plain metadata is deliberately required to begin with a letter.
    if re.match(r"^[^A-Za-z]", value):
        return None, "must begin with an ASCII letter or be explicitly quoted"
    if re.search(r"(?:^|[ \t])#", value):
        return None, "must be quoted when it contains a YAML comment marker"
    if value.lower() in {"null", "true", "false", "yes", "no", "on", "off", "~"}:
        return None, "must be quoted when it resembles a YAML non-string scalar"
    return value, None


def parse_skill(path: Path) -> tuple[SkillDocument | None, list[str]]:
    """Parse one skill and return diagnostics suitable for a CLI user."""

    errors: list[str] = []
    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        return None, [f"{path}: cannot read SKILL.md: {error}"]

    lines = text.splitlines()
    if not lines or lines[0] != "---":
        errors.append(f"{path}: frontmatter must start with --- on line 1")
        return None, errors

    closing: int | None = None
    for index in range(1, len(lines)):
        if lines[index] == FRONTMATTER_END:
            closing = index
            break
    if closing is None:
        errors.append(f"{path}: frontmatter is missing its closing ---")
        return None, errors

    fields: dict[str, str] = {}
    for index, line in enumerate(lines[1:closing], start=2):
        match = FRONTMATTER_FIELD_RE.fullmatch(line)
        if match is None:
            errors.append(
                f"{path}:{index}: metadata must be a single name: value or description: value line"
            )
            continue
        key, raw_value = match.groups()
        if key in fields:
            errors.append(f"{path}:{index}: duplicate metadata field {key!r}")
            continue
        value, value_error = _unquote_scalar(raw_value)
        if value is None or not value.strip():
            reason = value_error or "must be non-blank"
            errors.append(f"{path}:{index}: metadata field {key!r} {reason}")
            continue
        fields[key] = value

    for key in ("name", "description"):
        if key not in fields:
            errors.append(f"{path}: frontmatter is missing {key!r}")

    name = fields.get("name", "")
    description = fields.get("description", "")
    if name and (len(name) > 64 or NAME_RE.fullmatch(name) is None):
        errors.append(
            f"{path}: name {name!r} must be lowercase hyphenated text of at most 64 characters"
        )
    if name and name != path.parent.name:
        errors.append(
            f"{path}: name {name!r} must match its skill directory {path.parent.name!r}"
        )

    if errors:
        return None, errors
    return SkillDocument(
        path,
        name,
        description,
        "\n".join(lines[closing + 1 :]) + "\n",
        closing + 2,
    ), errors


def _skill_files(root: Path) -> list[Path]:
    skills_root = root / "skills"
    if not skills_root.is_dir():
        return []
    return sorted(path for path in skills_root.rglob("SKILL.md") if path.is_file())


def _relative_to_root(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
    except ValueError:
        return False
    return True


def _validate_links(document: SkillDocument, root: Path) -> Iterator[str]:
    text = document.body
    for match in MARKDOWN_LINK_RE.finditer(text):
        raw_target = match.group(1).strip()
        line = document.body_start_line + _line_number(text, match.start()) - 1

        if raw_target.startswith("<") and raw_target.endswith(">"):
            target = raw_target[1:-1]
        else:
            pieces = raw_target.split(None, 1)
            if not pieces:
                yield f"{document.path}:{line}: local link has an empty destination"
                continue
            target = pieces[0]
        if not target or target.startswith("#"):
            continue
        if target.startswith("//") or re.match(r"^[A-Za-z][A-Za-z0-9+.-]*:", target):
            continue
        if target.startswith("/") or re.match(r"^[A-Za-z]:[\\/]", target):
            yield f"{document.path}:{line}: link {target!r} must be relative"
            continue

        local_target = target.split("#", 1)[0]
        if not local_target:
            continue
        candidate = (document.path.parent / local_target).resolve(strict=False)
        root_resolved = root.resolve(strict=False)
        if not _relative_to_root(candidate, root_resolved):
            yield f"{document.path}:{line}: link {target!r} escapes the repository root"
        elif not candidate.is_file():
            yield f"{document.path}:{line}: local link {target!r} does not exist"


def _make_targets(makefile: Path) -> tuple[set[str], str | None]:
    try:
        lines = makefile.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError) as error:
        return set(), str(error)

    targets: set[str] = set()
    target_definition = re.compile(r"^\s*([A-Za-z0-9][A-Za-z0-9_.-]*):(?:\s|$)")
    for line in lines:
        match = target_definition.match(line)
        if match:
            targets.add(match.group(1))
    return targets, None


def _line_number(text: str, offset: int) -> int:
    return text.count("\n", 0, offset) + 1


def _command_matches(text: str) -> Iterator[tuple[int, str, str]]:
    """Yield line, target, and line text for command-shaped make references.

    Inline code and fenced code are always inspected.  Outside code, only a
    line beginning with ``make`` (optionally after a list marker or ``$``) is
    considered a command; this avoids treating ordinary prose such as "make a
    plan" as a build invocation.
    """

    seen: set[tuple[int, str]] = set()

    def emit(segment: str, base_offset: int, line_text: str) -> Iterator[tuple[int, str, str]]:
        for match in MAKE_COMMAND_RE.finditer(segment):
            target = match.group("target")
            line = _line_number(text, base_offset + match.start("target"))
            key = (line, target)
            if key not in seen:
                seen.add(key)
                yield line, target, line_text

    in_fence = False
    for line_match in re.finditer(r"^.*(?:\n|$)", text, re.MULTILINE):
        line_text = line_match.group(0).rstrip("\n")
        stripped = line_text.lstrip()
        if stripped.startswith("```") or stripped.startswith("~~~"):
            in_fence = not in_fence
            continue
        if in_fence:
            yield from emit(line_text, line_match.start(), line_text)
        elif COMMAND_LINE_RE.match(stripped):
            yield from emit(line_text, line_match.start(), line_text)

    for inline in re.finditer(r"`([^`\n]+)`", text):
        line_start = text.rfind("\n", 0, inline.start()) + 1
        line_end = text.find("\n", inline.end())
        if line_end < 0:
            line_end = len(text)
        yield from emit(inline.group(1), inline.start(1), text[line_start:line_end])


def _validate_body(document: SkillDocument, make_targets: set[str]) -> Iterator[str]:
    for line_number, line in enumerate(
        document.body.splitlines(), start=document.body_start_line
    ):
        for marker in PLACEHOLDER_RE.finditer(line):
            yield (
                f"{document.path}:{line_number}: unfinished scaffold marker "
                f"{marker.group(0)!r}; replace it or remove it"
            )

    for line, target, source in _command_matches(document.body):
        if PLANNED_COMMAND_RE.search(source):
            continue
        actual_line = document.body_start_line + line - 1
        if target not in make_targets:
            yield (
                f"{document.path}:{actual_line}: make target {target!r} is not defined in Makefile; "
                "mark intentionally future work with [planned] or (planned)"
            )


def verify(root: Path, required_skills: Sequence[str] = REQUIRED_SKILLS) -> list[str]:
    """Return deterministic diagnostics for a repository skill tree."""

    root = root.resolve(strict=False)
    errors: list[str] = []
    skills_root = root / "skills"
    if not skills_root.is_dir():
        return [f"{skills_root}: required skills directory does not exist"]

    files = _skill_files(root)
    documents: list[SkillDocument] = []
    names: dict[str, Path] = {}
    for path in files:
        document, parse_errors = parse_skill(path)
        errors.extend(parse_errors)
        if document is None:
            continue
        documents.append(document)
        previous = names.get(document.name)
        if previous is not None:
            errors.append(
                f"{path}: duplicate skill name {document.name!r}; already declared by {previous}"
            )
        else:
            names[document.name] = path

    for required in required_skills:
        required_path = root / "skills" / required / "SKILL.md"
        if not required_path.is_file():
            errors.append(f"{required_path}: required skill is missing")

    make_targets, make_error = _make_targets(root / "Makefile")
    if make_error is not None:
        if any(next(_command_matches(document.body), None) is not None for document in documents):
            errors.append(f"{root / 'Makefile'}: cannot read Makefile: {make_error}")
    for document in documents:
        errors.extend(_validate_links(document, root))
        errors.extend(_validate_body(document, make_targets))
    return sorted(set(errors))


def main(argv: Iterable[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "root",
        nargs="?",
        type=Path,
        help="repository root (default: the parent of scripts/)",
    )
    args = parser.parse_args(list(argv) if argv is not None else None)
    root = args.root or Path(__file__).resolve().parent.parent
    errors = verify(root)
    if errors:
        for error in errors:
            print(f"verify-skills: {error}", file=sys.stderr)
        return 1
    count = len(_skill_files(root.resolve(strict=False)))
    print(f"verify-skills: checked {count} skill(s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
