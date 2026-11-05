"""Run static checks over the maintainability owner's explicit source inventory."""

import argparse
import os
from pathlib import Path
import subprocess
import sys


ROOT = Path(__file__).resolve().parent.parent
LANGUAGES = {"python": ".py", "javascript": ".mjs", "shell": ".sh"}
WORKFLOW_GAPS = {"release.yml": "unsupported concurrency.queue"}


def source_paths(language: str, *, root: Path = ROOT) -> list[str]:
    result = subprocess.run(["go", "run", "./cmd/maintainability", "--sources", language],
                            cwd=root, capture_output=True, check=False, timeout=120)
    if result.returncode:
        reason = result.stderr.decode(errors="replace").strip() if result.stderr else "no diagnostic output"
        raise ValueError(f"cannot enumerate {language} sources (exit {result.returncode}): {reason}")
    records = result.stdout.split(b"\0")
    if not records or records.pop() != b"" or not records:
        raise ValueError(f"{language} inventory is empty or not NUL terminated")
    paths = [os.fsdecode(record) for record in records]
    if len(set(paths)) != len(paths) or paths != sorted(paths):
        raise ValueError(f"{language} inventory contains duplicates or is unsorted")
    for path in paths:
        candidate = Path(path)
        if not candidate.is_absolute() or not candidate.is_relative_to(root) or candidate.suffix != LANGUAGES[language]:
            raise ValueError(f"invalid {language} inventory path: {path}")
    return paths


def batches(paths: list[str], *, byte_limit: int = 24000) -> list[list[str]]:
    result: list[list[str]] = []
    current: list[str] = []
    size = 0
    for path in paths:
        length = len(os.fsencode(path)) + 1
        if length > byte_limit:
            raise ValueError("source path exceeds the command argument budget")
        if current and size + length > byte_limit:
            result.append(current)
            current, size = [], 0
        current.append(path)
        size += length
    if current:
        result.append(current)
    return result


def execute(command: list[str]) -> int:
    result = subprocess.run(command, cwd=ROOT, check=False, timeout=600)
    if result.returncode < 0:
        raise ValueError(f"{command[0]} terminated by signal {-result.returncode}")
    return result.returncode


def workflow_paths() -> list[str]:
    paths = []
    for path in sorted((ROOT / ".github/workflows").iterdir()):
        if not path.is_file() or path.suffix not in (".yml", ".yaml"):
            continue
        if path.name in WORKFLOW_GAPS:
            print(f"actionlint: {path.name} remains unverified ({WORKFLOW_GAPS[path.name]})", flush=True)
        else:
            paths.append(str(path))
    if not paths:
        raise ValueError("no supported workflow files found")
    return paths


def commands(check: str) -> tuple[list[str], str | None]:
    if check == "python":
        return [str(ROOT / "bin/lint/bin/ruff"), "check", "--no-fix", "--config", str(ROOT / "ruff.toml")], "python"
    if check == "javascript":
        return ["node", str(ROOT / "bin/.lint-js/node_modules/eslint/bin/eslint.js"),
                "--no-config-lookup", "--config", str(ROOT / "eslint.config.mjs")], "javascript"
    if check == "shell":
        return [str(ROOT / "bin/shellcheck")], "shell"
    if check == "workflows":
        return [str(ROOT / "bin/actionlint"), "-shellcheck", str(ROOT / "bin/shellcheck"), "-pyflakes", ""], None
    if check == "ty":
        return [str(ROOT / "bin/diagnostics/bin/ty"), "check", "--config-file", str(ROOT / "ty.toml")], "python"
    if check == "zizmor":
        return [str(ROOT / "bin/diagnostics/bin/zizmor"), "--offline"], None
    raise ValueError(f"unknown static check {check}")


def run_check(check: str) -> int:
    command, language = commands(check)
    if language:
        paths = source_paths(language)
    elif check == "workflows":
        paths = workflow_paths()
    else:
        paths = [str(ROOT / ".github/workflows")]
    print(f"{check}: checking {len(paths)} explicit input paths", flush=True)
    code = 0
    for batch in batches(paths):
        code = max(code, execute(command + batch))
    return code


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("check", choices=("all", "diagnostics", "python", "javascript", "shell", "workflows", "ty", "zizmor"))
    args = parser.parse_args()
    checks = {"all": ("python", "javascript", "shell", "workflows"),
              "diagnostics": ("ty", "zizmor")}.get(args.check, (args.check,))
    code = 0
    try:
        for check in checks:
            code = max(code, run_check(check))
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"Static analysis could not complete: {error}", file=sys.stderr)
        return 2
    return code


if __name__ == "__main__":
    raise SystemExit(main())
