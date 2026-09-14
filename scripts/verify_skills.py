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
from collections import Counter
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
MAKE_WORD_RE = re.compile(r"(?<![A-Za-z0-9_./-])make(?![A-Za-z0-9_.-])", re.IGNORECASE)
COMMAND_LINE_RE = re.compile(r"^(?:[-*+]\s+)?(?:\$[ \t]*)?make(?:[ \t]+|$)", re.IGNORECASE)
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


@dataclass(frozen=True)
class MakeOccurrence:
    line: int
    target: str | None
    source: str
    syntax_error: str | None


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

    if path.is_symlink():
        return None, [f"{path}: SKILL.md must not be a symlink"]
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
    # Keep symlink entries in the candidate list so verification can report
    # them instead of silently following or dropping them.
    candidates: set[Path] = set()
    for path in skills_root.rglob("*"):
        if path.name == "SKILL.md":
            candidates.add(path)
        elif path.is_symlink() and path.is_dir():
            candidates.add(path / "SKILL.md")
    return sorted(candidates)


def _relative_to_root(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
    except ValueError:
        return False
    return True


def _skill_path_error(path: Path, root: Path, required: bool = False) -> str | None:
    if path.is_symlink():
        return f"{path}: SKILL.md must not be a symlink"
    try:
        relative = path.relative_to(root)
        current = root
        for part in relative.parts[:-1]:
            current /= part
            if current.is_symlink():
                return f"{path}: SKILL.md uses a symlinked directory ({current})"
        resolved = path.resolve(strict=False)
    except (OSError, RuntimeError) as error:
        return f"{path}: cannot resolve SKILL.md path: {error}"
    if not _relative_to_root(resolved, root.resolve(strict=False)):
        return f"{path}: SKILL.md resolves outside the repository root ({resolved})"
    if not path.is_file():
        message = "required skill is missing" if required else "SKILL.md is missing or not a regular file"
        return f"{path}: {message}"
    return None


def _markdown_links(text: str) -> Iterator[tuple[int, str | None, str | None]]:
    """Yield line, destination, and parse error for narrow inline links.

    Reference links are intentionally not parsed by this small checker; their
    syntax is reported separately so a broken reference cannot pass silently.
    Parentheses in a destination are balanced, while nested Markdown labels
    and full title parsing remain outside the supported syntax.
    """

    index = 0
    while index < len(text):
        if text[index] != "[" or (index > 0 and text[index - 1] == "!"):
            index += 1
            continue
        label_end = text.find("]", index + 1)
        if label_end < 0:
            index += 1
            continue
        next_index = label_end + 1
        if next_index < len(text) and text[next_index] == "[":
            reference_end = text.find("]", next_index + 1)
            if reference_end < 0:
                reference_end = next_index
            line = _line_number(text, index)
            yield line, None, "reference-style Markdown links are unsupported; use an inline destination"
            index = reference_end + 1
            continue
        if next_index >= len(text) or text[next_index] != "(":
            index = label_end + 1
            continue

        cursor = next_index + 1
        if cursor < len(text) and text[cursor] == "<":
            closing = text.find(">", cursor + 1)
            if closing < 0 or "\n" in text[cursor:closing]:
                yield _line_number(text, index), None, "unterminated angle-bracket link destination"
                index = cursor + 1
                continue
            if closing + 1 >= len(text) or text[closing + 1] != ")":
                yield _line_number(text, index), None, "unsupported link destination; omit titles"
                index = closing + 1
                continue
            yield _line_number(text, index), text[cursor:closing + 1], None
            index = closing + 2
            continue
        depth = 1
        while cursor < len(text) and depth:
            if text[cursor] == "\\" and cursor + 1 < len(text):
                cursor += 2
                continue
            if text[cursor] == "(":
                depth += 1
            elif text[cursor] == ")":
                depth -= 1
            cursor += 1
        line = _line_number(text, index)
        if depth:
            yield line, None, "unterminated parenthesized Markdown link"
            index = cursor
            continue
        yield line, text[next_index + 1 : cursor - 1], None
        index = cursor


def _link_target(raw: str) -> tuple[str | None, str | None]:
    value = raw.strip()
    if value.startswith("<") and value.endswith(">"):
        value = value[1:-1]
    elif re.search(r"[\s<>]", value):
        return None, "unsupported link destination; use <path with spaces> and omit titles"
    if not value.strip():
        return None, "local link has an empty destination"
    return value, None


def _validate_reference_definitions(document: SkillDocument) -> Iterator[str]:
    definition = re.compile(r"^[ \t]{0,3}\[[^\]\n]+\]:", re.MULTILINE)
    for match in definition.finditer(document.body):
        line = document.body_start_line + _line_number(document.body, match.start()) - 1
        yield (
            f"{document.path}:{line}: reference-style Markdown link definitions are unsupported; "
            "use an inline destination"
        )


def _validate_links(document: SkillDocument, root: Path) -> Iterator[str]:
    text = document.body
    yield from _validate_reference_definitions(document)
    for line, raw_target, parse_error in _markdown_links(text):
        actual_line = document.body_start_line + line - 1
        if parse_error is not None:
            yield f"{document.path}:{actual_line}: {parse_error}"
            continue
        if raw_target is None:
            continue
        target, target_error = _link_target(raw_target)
        if target_error is not None:
            yield f"{document.path}:{actual_line}: {target_error}"
            continue
        if target is None:
            yield f"{document.path}:{actual_line}: local link has an empty destination"
            continue
        if target.startswith("//"):
            continue
        if target.startswith("/") or re.match(r"^[A-Za-z]:[\\/]", target):
            yield f"{document.path}:{actual_line}: link {target!r} must be relative"
            continue
        if target.startswith("#"):
            continue
        if re.match(r"^[A-Za-z][A-Za-z0-9+.-]*:", target):
            continue

        local_target = target.split("#", 1)[0]
        if not local_target:
            continue
        candidate = (document.path.parent / local_target).resolve(strict=False)
        root_resolved = root.resolve(strict=False)
        if not _relative_to_root(candidate, root_resolved):
            yield f"{document.path}:{actual_line}: link {target!r} escapes the repository root"
        elif not candidate.is_file():
            yield f"{document.path}:{actual_line}: local link {target!r} does not exist"


def _make_targets(makefile: Path) -> tuple[set[str], str | None]:
    try:
        lines = makefile.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError) as error:
        return set(), str(error)

    targets: set[str] = set()
    target_definition = re.compile(r"^\s*([A-Za-z0-9][A-Za-z0-9_.-]*):(?::|[ \t;]|$)")
    for line in lines:
        match = target_definition.match(line)
        if match:
            targets.add(match.group(1))
    return targets, None


def _line_number(text: str, offset: int) -> int:
    return text.count("\n", 0, offset) + 1


def _make_contexts(text: str) -> Iterator[tuple[str, int, str]]:
    """Yield code/command contexts that may contain a make invocation."""

    offset = 0
    in_fence = False
    for raw_line in text.splitlines(keepends=True):
        line = raw_line.rstrip("\r\n")
        stripped = line.lstrip()
        if stripped.startswith("```") or stripped.startswith("~~~"):
            in_fence = not in_fence
        elif in_fence or COMMAND_LINE_RE.match(stripped):
            if MAKE_WORD_RE.search(line):
                yield line, offset, line
        offset += len(raw_line)

    for inline in re.finditer(r"`([^`\n]+)`", text):
        segment = inline.group(1)
        if MAKE_WORD_RE.search(segment):
            line_start = text.rfind("\n", 0, inline.start()) + 1
            line_end = text.find("\n", inline.end())
            if line_end < 0:
                line_end = len(text)
            yield segment, inline.start(1), text[line_start:line_end]


def _make_occurrences(text: str) -> list[MakeOccurrence]:
    occurrences: list[MakeOccurrence] = []
    seen_offsets: set[int] = set()
    for segment, base_offset, source in _make_contexts(text):
        match = MAKE_WORD_RE.search(segment)
        if match is None:
            continue
        absolute_offset = base_offset + match.start()
        if absolute_offset in seen_offsets:
            continue
        seen_offsets.add(absolute_offset)
        command = PLANNED_COMMAND_RE.sub("", segment).strip()
        command = re.sub(r"^(?:[-*+]\s+)?(?:\$[ \t]*)?", "", command)
        parsed = re.fullmatch(r"make[ \t]+([A-Za-z0-9][A-Za-z0-9_.-]*)", command)
        syntax_error = None if parsed else (
            "unsupported make command shape; use exactly `make <target>` "
            "with no options, assignments, extra targets, or shell operators"
        )
        occurrences.append(MakeOccurrence(
            _line_number(text, absolute_offset), parsed.group(1) if parsed else None,
            source, syntax_error,
        ))
    return occurrences


def _command_matches(text: str) -> Iterator[tuple[int, str, str]]:
    """Yield valid one-target make references for callers that need matches."""

    for occurrence in _make_occurrences(text):
        if occurrence.target is not None:
            yield occurrence.line, occurrence.target, occurrence.source


def _validate_body(document: SkillDocument, make_targets: set[str]) -> Iterator[str]:
    for line_number, line in enumerate(
        document.body.splitlines(), start=document.body_start_line
    ):
        for marker in PLACEHOLDER_RE.finditer(line):
            yield (
                f"{document.path}:{line_number}: unfinished scaffold marker "
                f"{marker.group(0)!r}; replace it or remove it"
            )

    occurrences = _make_occurrences(document.body)
    planned_lines = Counter(
        occurrence.line
        for occurrence in occurrences
        if PLANNED_COMMAND_RE.search(occurrence.source)
    )
    ambiguous_planned_lines: set[int] = set()
    for occurrence in occurrences:
        actual_line = document.body_start_line + occurrence.line - 1
        if occurrence.syntax_error is not None:
            yield f"{document.path}:{actual_line}: {occurrence.syntax_error}"
            continue
        if planned_lines[occurrence.line] and planned_lines[occurrence.line] != 1:
            if occurrence.line not in ambiguous_planned_lines:
                ambiguous_planned_lines.add(occurrence.line)
                yield (
                    f"{document.path}:{actual_line}: planned marker is ambiguous for multiple "
                    "make commands on the same line"
                )
        elif planned_lines[occurrence.line]:
            continue
        if occurrence.target not in make_targets:
            yield (
                f"{document.path}:{actual_line}: make target {occurrence.target!r} is not defined in Makefile; "
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
        path_error = _skill_path_error(path, root)
        if path_error is not None:
            errors.append(path_error)
            continue
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
        path_error = _skill_path_error(required_path, root, required=True)
        if path_error is not None:
            errors.append(path_error)

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
