#!/usr/bin/env python3
"""Test-owned root boundary, copied as delegate ahead of every routing backend."""

from dataclasses import dataclass
import json
import os
from pathlib import Path
import sys


@dataclass(frozen=True)
class Configuration:
    delegate: Path
    backend: Path
    state_root: Path
    calls: Path
    explicit_root: bool


def read_configuration(path: Path) -> Configuration:
    document = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(document, dict) or not isinstance(document.get("explicit_root"), bool):
        raise ValueError("invalid routing shim configuration")
    paths = {}
    for name in ("delegate", "backend", "state_root", "calls"):
        value = document.get(name)
        if not isinstance(value, str) or not Path(value).is_absolute():
            raise ValueError("routing shim requires an absolute " + name)
        paths[name] = Path(value)
    for name in ("delegate", "backend"):
        candidate = paths[name]
        if not candidate.is_file() or not os.access(candidate, os.X_OK):
            raise ValueError("routing shim executable unavailable: " + name)
        if candidate.samefile(path.parent / "delegate"):
            raise ValueError("routing shim cannot invoke itself")
    return Configuration(**paths, explicit_root=document["explicit_root"])


def routed_argv(argv: list[str], *, state_root: str, explicit_root: bool) -> list[str]:
    """Validate original agent intent before adding the harness-owned root."""
    roots = [index for index, value in enumerate(argv)
             if value in ("--root", "-root") or value.startswith(("--root=", "-root="))]
    if not explicit_root:
        if roots:
            raise ValueError("ordinary routing must omit every --root flag")
        return ["--root", state_root, *argv]
    if len(roots) != 1:
        raise ValueError("explicit routing requires exactly one --root flag")
    index = roots[0]
    if argv[index] in ("--root", "-root"):
        value = argv[index + 1] if index + 1 < len(argv) else None
    else:
        value = argv[index].partition("=")[2]
    if value != state_root:
        raise ValueError("explicit routing must preserve the exact supplied root")
    return list(argv)


def main() -> int:
    try:
        configuration = read_configuration(Path(__file__).resolve().with_name("runtime.json"))
        argv = sys.argv[1:]
        error = None
        try:
            forwarded = routed_argv(argv, state_root=str(configuration.state_root),
                                    explicit_root=configuration.explicit_root)
        except ValueError as rejected:
            error = str(rejected)
            forwarded = []
        # The attempt owns this append-only log. Record the ORIGINAL argv,
        # including refused calls, before any backend can observe the command.
        with configuration.calls.open("a", encoding="utf-8") as stream:
            stream.write(json.dumps({"argv": argv, "error": error}) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
        if error is not None:
            raise ValueError(error)
        os.execv(str(configuration.backend), [str(configuration.backend), *forwarded])
    except (OSError, ValueError) as error:
        print("routing root isolation refused: " + str(error), file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
