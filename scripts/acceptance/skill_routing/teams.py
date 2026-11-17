"""Test-owned Team catalogs for routing observations, never admission authority."""

from __future__ import annotations

import json
import os
from pathlib import Path
import sys


SCENARIOS = frozenset({"default", "better-alternative", "unset", "empty", "stale", "ambiguous", "corrupt"})


def catalog_environment(home: Path) -> dict[str, str]:
    """Only the Delegate backend gets this home; the native lead keeps its login."""
    if not home.is_absolute():
        raise ValueError("routing catalog home must be absolute")
    return {"HOME": str(home), "XDG_CONFIG_HOME": str(home / "config")}


def backend_environment(runtime: dict, fixture: Path, command: str | None) -> dict[str, str]:
    environment = dict(os.environ, PATH=runtime["path"])
    selected = runtime.get("catalog_environment")
    if selected is None:
        if command == "teams":
            raise ValueError("Team discovery requires an isolated backend catalog")
        return environment
    expected = catalog_environment(fixture.resolve() / "catalog-home")
    if selected != expected:
        raise ValueError("routing catalog environment must use its owned fixture home")
    environment.update(expected)
    return environment


def definition(identity: str, *, specialized=False, alternate=False) -> dict:
    roles = [("research", "Research requirements and alternatives"),
             ("planning", "Plan changes and challenge architecture"),
             ("implementation", "Implement requested code changes"),
             ("testing", "Independently test requirements and report evidence"),
             ("review", "Independently review code correctness and regressions")]
    if specialized:
        roles = [("implementation", "Implement percentage discounts in Python without changing callers")]
    return {
        "schema_version": 1, "id": identity, "name": identity,
        "purpose": "Python percentage discount implementation specialists" if specialized else "Engineering work allocation",
        "roles": [{"id": role, "name": role, "responsibility": responsibility} for role, responsibility in roles],
        "members": [{"id": role, "name": role, "role_ids": [role],
                     "execution_defaults": {"provider": "codex:exec" if alternate or role == "implementation" else "claude:print",
                                            "model": "fixture-" + ("specialist" if specialized else identity) + "-" + role,
                                            "effort": "high"}} for role, _ in roles],
    }


def seed_commands(scenario: str, directory: Path, workspace: Path) -> list[list[str]]:
    """Prepare real CLI mutations; the caller owns exact child Wait and captures."""
    if scenario not in SCENARIOS:
        raise ValueError("unknown Team routing scenario")
    if scenario in {"empty", "corrupt"}:
        return []
    definitions = [definition("general")]
    if scenario == "better-alternative":
        definitions.append(definition("python-specialists", specialized=True))
    if scenario == "ambiguous":
        definitions.append(definition("alternative", alternate=True))
    commands = []
    for document in definitions:
        path = directory / (document["id"] + ".json")
        with path.open("x", encoding="utf-8") as stream:
            json.dump(document, stream)
        commands.append(["teams", "register", "--scope", "project", "--cwd", str(workspace),
                         "--definition", str(path), "--json"])
    if scenario not in {"unset", "ambiguous"}:
        commands.append(["teams", "default", "set", "--cwd", str(workspace),
                         "--team-scope", "project", "--team", "general", "--json"])
    if scenario == "stale":
        commands.append(["teams", "delete", "--scope", "project", "--cwd", str(workspace),
                         "--team", "general", "--expected-revision", "1", "--json"])
    return commands


def corrupt_catalog(home: Path) -> None:
    """A deliberately invalid test database is not a synthetic empty catalog."""
    config = home / "Library/Application Support" if sys.platform == "darwin" else home / "config"
    target = config / "delegation-layer-catalog/teams.sqlite"
    target.parent.mkdir(parents=True)
    with target.open("xb") as stream:
        stream.write(b"not a sqlite Team catalog\n")
