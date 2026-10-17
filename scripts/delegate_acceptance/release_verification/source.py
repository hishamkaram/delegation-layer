"""Hash the actual static Python import closure without importing inspected code."""

from __future__ import annotations

import hashlib
import json
from pathlib import Path

from scripts.maintainability.python_ast import module_name, parse_source_facts
from .contract import MAX_SOURCE_FILE, SOURCE_FILES, VerificationError
from .filesystem import _bounded_file_bytes


PARSER_SOURCE = "scripts/maintainability/python_ast.py"


def _python_modules(root: Path) -> dict[str, list[str]]:
    modules: dict[str, list[str]] = {}
    for path in sorted((root / "scripts").rglob("*.py")):
        name = path.relative_to(root).as_posix()
        module = module_name(name)
        modules.setdefault(module, []).append(name)
    return modules


def _import_source_paths(module: str, modules: dict[str, list[str]]) -> list[str]:
    components = module.split(".")
    # Hash all maintained candidates without guessing the runtime import search path.
    return [path for size in range(1, len(components) + 1)
            for path in modules.get(".".join(components[:size]), [])]


def fingerprint_sources(root: Path, seeds: tuple[str, ...]) -> dict[str, object]:
    modules = _python_modules(root)
    pending = [*seeds, PARSER_SOURCE]
    observed: dict[str, bytes] = {}
    while pending:
        name = pending.pop()
        if name in observed:
            continue
        data = _bounded_file_bytes(root / name, MAX_SOURCE_FILE, "source_file_invalid")
        observed[name] = data
        if not name.endswith(".py"):
            continue
        pending.extend(_import_source_paths(module_name(name), modules))
        try:
            facts = parse_source_facts(name, data.decode("utf-8"), set(modules))
        except (UnicodeError, SyntaxError, RecursionError) as error:
            raise VerificationError("source_file_invalid", "source-fingerprint") from error
        for item in facts["imports"]:
            target = item["target"].removeprefix("py:")
            pending.extend(_import_source_paths(target, modules))
    files = [{"path": name, "sha256": hashlib.sha256(data).hexdigest()}
             for name, data in sorted(observed.items())]
    encoded = json.dumps(files, sort_keys=True, separators=(",", ":")).encode()
    return {"sha256": hashlib.sha256(encoded).hexdigest(), "files": files}


def source_fingerprint(kind: str) -> dict[str, object]:
    root = Path(__file__).resolve().parents[3]
    names = (
        "scripts/verify_release_archive.py", "scripts/acceptance_supervisor_common.py",
        *SOURCE_FILES[kind],
    )
    return fingerprint_sources(root, names)
