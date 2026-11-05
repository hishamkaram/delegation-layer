"""Structural validation of a skill's unprimed evaluation case data.

The checker validates shape only: exact key sets, closed vocabularies, safe
relative fixture paths, and that the agent-visible half carries no scenario ID.
It never runs a model and never reads a prompt, rationale, or expected outcome
for meaning.
"""

from __future__ import annotations

import json
from pathlib import Path
import re

SCHEMA_VERSION = 1
CASES_PATH = Path("evals") / "cases.json"
MAX_CASES_BYTES = 256 * 1024
ID_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
OUTCOMES = frozenset({
    "approve", "hold", "pass", "blocked", "implemented", "proposal-only", "no-change",
})
ACTION_TYPES = frozenset({
    "read-file", "write-file", "edit-file", "run-command", "report-status", "request-clarification",
})
ARTIFACT_CHECKS = frozenset({"json-field", "line", "no-line", "absent"})
_SCENARIO_KEYS = frozenset({"id", "visible", "hidden"})
_CASE_KEYS = _SCENARIO_KEYS | {"control"}


def _object(value: object, where: str, keys: frozenset[str]) -> tuple[dict, list[str]]:
    if not isinstance(value, dict):
        return {}, [f"{where}: must be an object"]
    errors = [f"{where}: missing field {key!r}" for key in sorted(keys - value.keys())]
    errors += [f"{where}: unsupported field {key!r}" for key in sorted(value.keys() - keys)]
    return value, errors


def _text(value: object, where: str) -> list[str]:
    if not isinstance(value, str) or not value.strip() or any(ord(char) < 32 and char not in "\n\t" for char in value):
        return [f"{where}: must be non-blank text without control characters"]
    return []


def _relative_path(value: object, where: str) -> list[str]:
    errors = _text(value, where)
    if errors or not isinstance(value, str):
        return errors
    parts = value.split("/")
    if "\\" in value or value.startswith("/") or re.match(r"^[A-Za-z]:", value) or any(part in ("", ".", "..") for part in parts):
        return [f"{where}: must be a safe relative path"]
    return []


def _member(value: object, allowed: frozenset[str]) -> bool:
    """Membership is only meaningful for text; untrusted lists and dicts are unhashable."""
    return isinstance(value, str) and value in allowed


def _list(value: object, where: str) -> list:
    return value if isinstance(value, list) else []


def _inputs(value: object, where: str) -> list[str]:
    if not isinstance(value, list):
        return [f"{where}: must be a list"]
    errors: list[str] = []
    seen: set[str] = set()
    for index, item in enumerate(value):
        entry, entry_errors = _object(item, f"{where}[{index}]", frozenset({"path", "content"}))
        errors += entry_errors
        if entry_errors:
            continue
        errors += _relative_path(entry["path"], f"{where}[{index}].path")
        if not isinstance(entry["content"], str):
            errors.append(f"{where}[{index}].content: must be a string")
        if isinstance(entry["path"], str):
            if entry["path"] in seen:
                errors.append(f"{where}[{index}].path: duplicate fixture path")
            seen.add(entry["path"])
    return errors


def _actions(value: object, where: str) -> list[str]:
    if not isinstance(value, list):
        return [f"{where}: must be a list"]
    errors: list[str] = []
    for index, item in enumerate(value):
        entry, entry_errors = _object(item, f"{where}[{index}]", frozenset({"type", "target"}))
        errors += entry_errors
        if not entry_errors:
            if not _member(entry["type"], ACTION_TYPES):
                errors.append(f"{where}[{index}].type: must be one of {sorted(ACTION_TYPES)}")
            errors += _text(entry["target"], f"{where}[{index}].target")
    return errors


def _artifact(entry: dict, where: str) -> list[str]:
    check, locator, value = entry["check"], entry["locator"], entry["value"]
    errors = _relative_path(entry["path"], f"{where}.path")
    if not _member(check, ARTIFACT_CHECKS):
        return errors + [f"{where}.check: must be one of {sorted(ARTIFACT_CHECKS)}"]
    if check == "json-field":
        if not isinstance(locator, str) or not locator.startswith("/"):
            errors.append(f"{where}.locator: must be a JSON pointer starting with '/'")
        if value is None or isinstance(value, (list, dict, float)):
            errors.append(f"{where}.value: must be a string, integer, or boolean")
    else:
        if locator != "":
            errors.append(f"{where}.locator: must be empty unless check is json-field")
        if check == "absent" and value is not None:
            errors.append(f"{where}.value: must be null for check 'absent'")
        if check != "absent":
            errors += _text(value, f"{where}.value")
    return errors


def _artifacts(value: object, where: str) -> list[str]:
    if not isinstance(value, list):
        return [f"{where}: must be a list"]
    errors: list[str] = []
    for index, item in enumerate(value):
        entry, entry_errors = _object(item, f"{where}[{index}]", frozenset({"path", "check", "locator", "value"}))
        errors += entry_errors or _artifact(entry, f"{where}[{index}]")
    return errors


def _expected(value: object, where: str) -> list[str]:
    expected, errors = _object(value, where, frozenset({"outcome", "required_actions", "forbidden_actions", "required_artifacts"}))
    if errors:
        return errors
    if not _member(expected["outcome"], OUTCOMES):
        errors.append(f"{where}.outcome: must be one of {sorted(OUTCOMES)}")
    errors += _actions(expected["required_actions"], f"{where}.required_actions")
    errors += _actions(expected["forbidden_actions"], f"{where}.forbidden_actions")
    errors += _artifacts(expected["required_artifacts"], f"{where}.required_artifacts")
    if errors:
        return errors
    if not expected["required_actions"] and not expected["required_artifacts"]:
        errors.append(f"{where}: needs at least one required action or artifact")
    required = {json.dumps(item, sort_keys=True) for item in expected["required_actions"]}
    if required & {json.dumps(item, sort_keys=True) for item in expected["forbidden_actions"]}:
        errors.append(f"{where}: an action cannot be both required and forbidden")
    return errors


def _scenario(value: object, where: str, keys: frozenset[str]) -> tuple[dict, list[str]]:
    scenario, errors = _object(value, where, keys)
    if errors:
        return scenario, errors
    if not isinstance(scenario["id"], str) or ID_RE.fullmatch(scenario["id"]) is None:
        errors.append(f"{where}.id: must be lowercase hyphenated text")
    visible, visible_errors = _object(scenario["visible"], f"{where}.visible", frozenset({"prompt", "inputs"}))
    hidden, hidden_errors = _object(scenario["hidden"], f"{where}.hidden", frozenset({"expected", "rationale"}))
    errors += visible_errors + hidden_errors
    if visible_errors or hidden_errors:
        return scenario, errors
    errors += _text(visible["prompt"], f"{where}.visible.prompt") + _inputs(visible["inputs"], f"{where}.visible.inputs")
    errors += _text(hidden["rationale"], f"{where}.hidden.rationale") + _expected(hidden["expected"], f"{where}.hidden.expected")
    return scenario, errors


def _visible_text(scenario: dict) -> str:
    visible = scenario["visible"]
    return "\n".join([visible["prompt"], *(f"{item['path']}\n{item['content']}" for item in visible["inputs"])])


def _case(value: object, where: str) -> tuple[list[str], list[str]]:
    """Return diagnostics and the scenario IDs the case declares."""
    case, errors = _scenario(value, where, _CASE_KEYS)
    if errors:
        return errors, []
    control, control_errors = _scenario(case["control"], f"{where}.control", _SCENARIO_KEYS)
    if control_errors:
        return control_errors, [case["id"]]
    ids = [case["id"], control["id"]]
    if case["id"] == control["id"]:
        errors.append(f"{where}.control.id: must differ from the case id")
    if case["hidden"]["expected"] == control["hidden"]["expected"]:
        errors.append(f"{where}.control: expected must differ from the case, or it does not discriminate")
    for scenario_id in ids:
        if scenario_id in _visible_text(case) or scenario_id in _visible_text(control):
            errors.append(f"{where}: visible text must not contain scenario id {scenario_id!r}")
    return errors, ids


def validate_cases(document: object, skill: str) -> list[str]:
    """Return structural diagnostics for one parsed cases document."""
    root, errors = _object(document, "cases.json", frozenset({"schema_version", "skill", "cases"}))
    if errors:
        return errors
    if type(root["schema_version"]) is not int or root["schema_version"] != SCHEMA_VERSION:
        errors.append(f"cases.json: schema_version must be {SCHEMA_VERSION}")
    if root["skill"] != skill:
        errors.append(f"cases.json: skill must be {skill!r}")
    cases = _list(root["cases"], "cases")
    if not cases:
        errors.append("cases.json: cases must be a non-empty list")
    seen: set[str] = set()
    for index, case in enumerate(cases):
        case_errors, ids = _case(case, f"cases[{index}]")
        errors += case_errors
        errors += [f"cases[{index}]: duplicate id {name!r}" for name in ids if name in seen]
        seen.update(ids)
    return errors


def _unique_pairs(pairs: list[tuple[str, object]]) -> dict:
    keys = [key for key, _ in pairs]
    if len(set(keys)) != len(keys):
        raise ValueError("duplicate key")
    return dict(pairs)


def check_skill(skill_dir: Path) -> list[str]:
    """Return diagnostics for a skill directory's cases file; none when it ships no evals."""
    path = skill_dir / CASES_PATH
    if not path.is_symlink() and not path.exists():
        return []
    if path.is_symlink() or not path.is_file():
        return [f"{path}: cases file must be a regular file"]
    try:
        if path.stat().st_size > MAX_CASES_BYTES:
            return [f"{path}: cases file exceeds {MAX_CASES_BYTES} bytes"]
        document = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=_unique_pairs)
    except (OSError, UnicodeError, ValueError, RecursionError) as error:
        return [f"{path}: cannot read cases: {error}"]
    return [f"{path}: {message}" for message in validate_cases(document, skill_dir.name)]
