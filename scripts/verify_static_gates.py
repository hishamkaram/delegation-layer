"""Prove new analyzers reject bad inputs and accept corrected counterparts."""

import argparse
from pathlib import Path
import subprocess
import sys
import tempfile

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import static_analysis as analysis


ROOT = analysis.ROOT


def prove(name: str, command: list[str], path: Path, bad: str, good: str, diagnostic: str) -> None:
    path.write_text(bad)
    rejected = subprocess.run(command + [str(path)], cwd=ROOT, text=True,
                              capture_output=True, timeout=60, check=False)
    if rejected.returncode == 0 or diagnostic not in rejected.stdout + rejected.stderr:
        raise RuntimeError(f"{name}: invalid fixture was not rejected for {diagnostic}: "
                           f"{rejected.stdout}{rejected.stderr}")
    path.write_text(good)
    accepted = subprocess.run(command + [str(path)], cwd=ROOT, text=True,
                              capture_output=True, timeout=60, check=False)
    if accepted.returncode:
        raise RuntimeError(f"{name}: corrected fixture failed: {accepted.stdout}{accepted.stderr}")
    print(f"PASS: {name} rejecting and corrected fixtures")


def lint_fixtures(directory: Path) -> None:
    ruff, _ = analysis.commands("python")
    cases = (
        ("F401", "import os\n", "import os as os\n"),
        ("F821", "value = undefined\n", "value = 1\n"),
        ("F841", "def example():\n    value = 1\n", "def example():\n    return 1\n"),
        ("B006", "def example(value=[]):\n    return value\n", "def example(value=None):\n    return value\n"),
        ("B012", "def example():\n    try:\n        return 1\n    finally:\n        return 2\n",
         "def example():\n    try:\n        return 1\n    finally:\n        print('done')\n"),
        ("B904", "try:\n    int('bad')\nexcept ValueError:\n    raise RuntimeError('invalid')\n",
         "try:\n    int('bad')\nexcept ValueError as error:\n    raise RuntimeError('invalid') from error\n"),
    )
    for rule, bad, good in cases:
        prove("Ruff " + rule, ruff, directory / "fixture.py", bad, good, rule)
    prove("ShellCheck", analysis.commands("shell")[0], directory / "fixture.sh",
          '#!/bin/sh\nexport value="$(false)"\n', '#!/bin/sh\nvalue="$(false)"\nexport value\n', "SC2155")
    # ESLint intentionally receives a maintained-root path so its files selector applies.
    with tempfile.TemporaryDirectory(prefix="static-proof-", dir=ROOT / "bin") as local:
        prove("ESLint", analysis.commands("javascript")[0], Path(local) / "fixture.mjs",
              "function example() { try { return 1; } finally { return 2; } }\nexample();\n",
              "function example() { return 1; }\nexample();\n", "no-unsafe-finally")
    prove("jactionlint", analysis.commands("workflows")[0],
          directory / "fixture.yml", "name: test\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo '${{ invalid.expression }}'\n",
          "name: test\non: push\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo valid\n", "undefined")
    queue_workflow = ("name: test\non: push\nconcurrency:\n  group: fixture\n  queue: {queue}\n"
                      "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo valid\n")
    prove("jactionlint concurrency queue", analysis.commands("workflows")[0],
          directory / "fixture.yml", queue_workflow.format(queue="invalid"),
          queue_workflow.format(queue="max"), "queue")


def type_fixtures(directory: Path) -> None:
    for name, module in (("namespaced", "scripts.static_tools"), ("bare", "static_tools")):
        prove("ty " + name, analysis.commands("ty")[0],
              directory / "fixture.py", f"from {module} import shellcheck_target\nvalue: int = shellcheck_target('Linux', 'x86_64')\n",
              f"from {module} import shellcheck_target\nvalue: tuple[str, str] = shellcheck_target('Linux', 'x86_64')\n",
              "invalid-assignment")


def security_fixtures(directory: Path) -> None:
    workflow = ("name: test\non: push\npermissions:\n  contents: read\n"
                "jobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"
                "      - uses: actions/checkout@{revision}\n"
                "        with:\n          persist-credentials: false\n")
    prove("zizmor unpinned action", analysis.commands("zizmor")[0], directory / "fixture.yml",
          workflow.format(revision="v4"),
          workflow.format(revision="fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09"), "unpinned-uses")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--diagnostic", action="store_true")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="static-proof-hidden-") as temporary:
        directory = Path(temporary) / ".hidden parent"
        directory.mkdir()
        if not args.diagnostic:
            lint_fixtures(directory)
        type_fixtures(directory)
        security_fixtures(directory)


if __name__ == "__main__":
    main()
