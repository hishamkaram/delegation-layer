"""Git fixture and fake external tools for development-candidate build tests.

The fixture is a real Git repository holding a copy of the build harness. The
external tools (go, goreleaser, npm, node) are small scripts that behave like
the real ones for the argv the builder issues and record nothing but the
names of the variables they were started with. Failure modes are chosen by
writing a word into `modes/<tool>`.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
import json
import os
from pathlib import Path
import shutil
import sys
from typing import TypeVar
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError
from scripts.delegate_acceptance.processes.operations import run_bounded_command
from scripts.delegate_acceptance.release_verification.commands import command
from scripts.delegate_acceptance.release_verification.contract import MAX_OUTPUT, VerificationError
from scripts.delegate_acceptance.release_verification.published_contract import (
    UNRESOLVED_OWNERSHIP_CODES,
)


REPOSITORY = Path(__file__).resolve().parent.parent
T = TypeVar("T")
COMMAND_TIMEOUT_SECONDS = 120
VERSION = "0.1.37"
HARNESS_PATHS = (
    "scripts/build_development_candidate.py", "scripts/render-homebrew-formula.sh",
    "scripts/delegate_acceptance", "scripts/maintainability", ".goreleaser.yaml",
    ".github/workflows/ci.yml",
)
GIT_ENVIRONMENT = {
    "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_AUTHOR_NAME": "Fixture",
    "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "Fixture",
    "GIT_COMMITTER_EMAIL": "fixture@example.invalid", "GIT_AUTHOR_DATE": "2026-01-01T00:00:00Z",
    "GIT_COMMITTER_DATE": "2026-01-01T00:00:00Z",
}
FETCH_SCRIPT = """#!/usr/bin/env bash
set -euo pipefail
root="${1:-.release/pueue}"
for platform in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
    mkdir -p "${root}/${platform}"
    printf 'pueue %s\\n' "${platform}" >"${root}/${platform}/pueue"
    printf 'pueued %s\\n' "${platform}" >"${root}/${platform}/pueued"
done
"""
PREAMBLE = """#!{python}
import json, os, pathlib, subprocess, sys
ROOT = pathlib.Path(__file__).resolve().parents[{depth}]
MODE = (ROOT / "modes" / "{tool}").read_text().split() if (ROOT / "modes" / "{tool}").exists() else []
with (ROOT / "environment.log").open("a") as log:
    log.write(json.dumps(sorted(os.environ)) + "\\n")
"""
FAKE_GO = """
args = sys.argv[1:]
if args == ["version"]:
    print("go version go%s linux/amd64" % ("1.25.8" if "old" in MODE else "1.27.3"))
elif args == ["env", "GOROOT"]:
    print(ROOT / ("elsewhere" if "foreign-root" in MODE else "goroot"))
elif args[:2] == ["version", "-m"]:
    revision, modified = pathlib.Path(args[2]).read_text().split("stamp:")[1].split()[0].split(":")
    print(args[2] + ": go1.27.3\\n\\tpath\\texample.invalid/delegate\\n\\tbuild\\tvcs=git")
    print("\\tbuild\\tvcs.revision=" + revision + "\\n\\tbuild\\tvcs.modified=" + modified)
else:
    sys.exit(64)
"""
FAKE_GORELEASER = """
import gzip, hashlib, io, tarfile
args = sys.argv[1:]
if args == ["--version"]:
    print("GoReleaser banner\\nGitVersion:    2.18.1\\nGitCommit:     deadbeef")
    sys.exit(0)
if args != ["release", "--clean", "--skip=publish"] or "fail" in MODE:
    sys.exit(3)
def git(*words):
    return subprocess.run(["git", *words], capture_output=True, text=True, check=True).stdout.strip()
tag = git("describe", "--exact-match", "--tags", "--match", "v*")
if git("status", "--porcelain"):
    sys.exit(4)
head, version = git("rev-parse", "HEAD"), tag[1:]
revision = "f" * 40 if "wrong-revision" in MODE else head
stamp = "stamp:%s:%s\\n" % (revision, "true" if "modified" in MODE else "false")
dist, checksums = pathlib.Path("dist"), []
dist.mkdir()
for os_name, arch in (("darwin", "amd64"), ("darwin", "arm64"), ("linux", "amd64"), ("linux", "arm64")):
    name = "delegation-layer_%s_%s_%s.tar.gz" % (version, os_name.title(), arch)
    bundle = pathlib.Path(".release/pueue") / os_name / arch
    members = {"delegate": b"delegate " + stamp.encode(), "delegate-run": b"run " + stamp.encode(),
               "pueue": (bundle / "pueue").read_bytes(), "pueued": (bundle / "pueued").read_bytes()}
    if "missing-pueue" in MODE and (os_name, arch) == ("darwin", "arm64"):
        del members["pueued"]
    raw = io.BytesIO()
    with gzip.GzipFile(fileobj=raw, mode="wb", mtime=0) as zipped, tarfile.open(fileobj=zipped, mode="w") as tar:
        for member, data in members.items():
            info = tarfile.TarInfo(member)
            info.size, info.mode, info.mtime = len(data), 0o755, 0
            tar.addfile(info, io.BytesIO(data))
    digest = hashlib.sha256(raw.getvalue()).hexdigest()
    if "corrupt-checksum" in MODE and (os_name, arch) == ("linux", "amd64"):
        digest = "0" * 64
    if not ("drop-archive" in MODE and (os_name, arch) == ("linux", "arm64")):
        (dist / name).write_bytes(raw.getvalue())
    checksums.append("%s  %s" % (digest, name))
if "extra-checksum" in MODE:
    checksums.append("%s  other-asset.sbom" % ("e" * 64))
(dist / "checksums.txt").write_text("\\n".join(checksums) + "\\n")
"""
FAKE_NPM = """
args = sys.argv[1:]
if args[:3] != ["pack", "--ignore-scripts", "--json"] or args[3] != "--pack-destination":
    sys.exit(64)
version = json.loads(pathlib.Path("package.json").read_text())["version"]
name = "wrong.tgz" if "wrong-name" in MODE else "delegation-layer-%s.tgz" % version
pathlib.Path(args[4], name).write_bytes(("fake npm package %s\\n" % version).encode())
if "stray-file" in MODE:
    pathlib.Path(args[4], "stray.txt").write_text("stray")
print(json.dumps([{"filename": name}]))
"""
FAKE_NODE = """
print("v22.11.0")
"""


def git(repository: Path, *arguments: str, tree: FixtureTree) -> str:
    """Run one finite fixture Git command; an unproven child closure is recorded on ``tree``."""
    environment = {"PATH": os.environ["PATH"], "HOME": str(repository), **GIT_ENVIRONMENT}
    # Detached automatic maintenance could recreate .git after the fixture tree was removed.
    output = tree.observe_operation(lambda: command(
        {"commands": []}, "fixture-git", ["git", "-c", "maintenance.auto=false", *arguments],
        environment, capture=True, timeout=COMMAND_TIMEOUT_SECONDS, cwd=repository))
    return output.decode().strip()


@dataclass(frozen=True)
class CliResult:
    """Exit code and the interleaved stdout and stderr of one finite builder run."""

    exit_code: int
    output: str


def _write_tool(path: Path, body: str, *, tool: str, depth: int) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(PREAMBLE.format(python=sys.executable, tool=tool, depth=depth) + body)
    path.chmod(0o755)


class CandidateFixture:
    """One committed harness repository plus the fake tools a build runs with."""

    def __init__(self, tree: FixtureTree):
        self.fixture_tree = tree
        self.root = tree.root
        root = self.root
        self.repository = root / "source"
        self.tools = root / "tools"
        (root / "home").mkdir()
        self._create_tools()
        self._create_repository()
        self.base = git(self.repository, "rev-parse", "HEAD~1", tree=self.fixture_tree)
        self.head = git(self.repository, "rev-parse", "HEAD", tree=self.fixture_tree)
        self.tree = git(self.repository, "rev-parse", "HEAD^{tree}", tree=self.fixture_tree)

    def _create_tools(self) -> None:
        _write_tool(self.tools / "goroot" / "bin" / "go", FAKE_GO, tool="go", depth=2)
        (self.tools / "bin").mkdir(parents=True)
        (self.tools / "bin" / "go").symlink_to(self.tools / "goroot" / "bin" / "go")
        for tool, body in (("goreleaser", FAKE_GORELEASER), ("npm", FAKE_NPM), ("node", FAKE_NODE)):
            _write_tool(self.tools / "bin" / tool, body, tool=tool, depth=1)
        (self.tools / "modes").mkdir()

    def _create_repository(self) -> None:
        self.repository.mkdir()
        git(self.repository, "init", "--quiet", "--initial-branch", "main", tree=self.fixture_tree)
        (self.repository / ".gitignore").write_text("/dist/\n/.release/\n__pycache__/\n")
        git(self.repository, "add", ".", tree=self.fixture_tree)
        git(self.repository, "commit", "--quiet", "-m", "base", tree=self.fixture_tree)
        for relative in HARNESS_PATHS:
            source, target = REPOSITORY / relative, self.repository / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            if source.is_dir():
                shutil.copytree(source, target, ignore=shutil.ignore_patterns("__pycache__"))
            else:
                shutil.copy2(source, target)
        (self.repository / ".go-version").write_text("1.27.3\n")
        (self.repository / "go.sum").write_text("example.invalid/dependency v1.0.0 h1:fixture\n")
        (self.repository / "package.json").write_text(
            '{"name": "delegation-layer", "version": "%s"}\n' % VERSION)
        fetch = self.repository / "scripts" / "fetch-pueue-assets.sh"
        fetch.write_text(FETCH_SCRIPT)
        fetch.chmod(0o755)
        git(self.repository, "add", ".", tree=self.fixture_tree)
        git(self.repository, "commit", "--quiet", "-m", "head", tree=self.fixture_tree)

    def require_resolved(self, action: str) -> None:
        """Refuse ``action`` while a child that used this fixture has no proven closure."""
        if self.fixture_tree.unresolved is not None:
            raise RuntimeError(f"{action} refused: a child of {self.root} has no proven closure")

    def set_mode(self, tool: str, *modes: str) -> None:
        self.require_resolved("fixture mode change")
        path = self.tools / "modes" / tool
        path.unlink(missing_ok=True)
        if modes:
            path.write_text(" ".join(modes))

    def clear_modes(self) -> None:
        self.require_resolved("fixture mode reset")
        for path in (self.tools / "modes").iterdir():
            path.unlink()
        (self.tools / "environment.log").unlink(missing_ok=True)

    def environment(self, **extra: str) -> dict[str, str]:
        return {"PATH": os.pathsep.join([str(self.tools / "bin"), os.environ["PATH"]]), "HOME": str(self.root / "home"),
                **extra}

    def tool_environment_names(self) -> set[str]:
        log = self.tools / "environment.log"
        names: set[str] = set()
        for line in log.read_text().splitlines() if log.exists() else ():
            names.update(json.loads(line))
        return names

    def restore(self) -> None:
        self.require_resolved("fixture checkout restore")
        git(self.repository, "checkout", "--quiet", "--", ".", tree=self.fixture_tree)
        git(self.repository, "clean", "--quiet", "-fd", tree=self.fixture_tree)

    def builder(self, case: FixtureTree, *arguments: str,
                environment: dict[str, str] | None = None) -> CliResult:
        """Run the harness CLI as a bounded child; closure facts are recorded on both trees."""
        self.require_resolved("fixture builder launch")
        script = self.repository / "scripts" / "build_development_candidate.py"
        argv = ["sh", "-c", 'exec "$@" 2>&1', "sh", sys.executable, "-I", "-B", str(script), *arguments]
        try:
            result = run_bounded_command(argv, environment or self.environment(), max_output=MAX_OUTPUT,
                                         capture=True, timeout=COMMAND_TIMEOUT_SECONDS)
        except BoundedCommandError as error:
            failure = VerificationError(error.reason.value, "fixture-cli")
            self._observe(failure, case)
            raise failure from None
        if "--receipt" in arguments:
            self._observe_terminal_receipt(Path(arguments[arguments.index("--receipt") + 1]), case,
                                           stage="nested-builder")
        return CliResult(result.exit_code, result.output.decode("utf-8", errors="replace"))

    def _observe(self, failure: VerificationError, case: FixtureTree) -> None:
        for tree in (case, self.fixture_tree):
            tree.observe(failure)

    def _observe_terminal_receipt(self, path: Path, case: FixtureTree, *, stage: str) -> None:
        """A builder that retained a tree for unproven closure keeps its containing fixture too."""
        if not path.exists():
            return
        try:
            receipt = json.loads(path.read_text())
            code = (receipt.get("failure") or {}).get("code")
            dispositions = set(receipt["cleanup"]["dispositions"].values())
        except (OSError, ValueError, KeyError, AttributeError):
            code, dispositions = "command_capture_unavailable", set()
        if code in UNRESOLVED_OWNERSHIP_CODES:
            self._observe(VerificationError(code, stage), case)
        elif dispositions & {"retained", "failed"} and receipt.get("verdict") != "pass":
            self._observe(VerificationError("command_termination_failed", stage), case)

    def render(self, case: FixtureTree, tag: str, checksums: Path) -> bytes:
        """Run the fixture's canonical renderer over ``case``'s checksums; closure is recorded on both trees."""
        return case.observe_operation(lambda: command(
            {"commands": []}, "fixture-render",
            ["bash", str(self.repository / "scripts" / "render-homebrew-formula.sh"), tag, str(checksums)],
            {"PATH": self.environment()["PATH"]}, capture=True, timeout=COMMAND_TIMEOUT_SECONDS),
            dependencies=(self.fixture_tree,))

    def run_in_process(self, case: FixtureTree, build: Callable[[], T], receipt: Path) -> T:
        """Run the builder in this process; its failure and terminal receipt reach both trees as for the CLI."""
        self.require_resolved("fixture in-process build")
        try:
            return case.observe_operation(build, dependencies=(self.fixture_tree,))
        finally:
            self._observe_terminal_receipt(receipt, case, stage="in-process-builder")

    def build_arguments(self, scratch: Path, **overrides: str) -> list[str]:
        values = {
            "version": VERSION, "commit": self.head, "base-commit": self.base,
            "repository": "hishamkaram/delegation-layer", "work-dir": str(scratch / "work"),
            "output-dir": str(scratch / "payload"), "receipt": str(scratch / "receipt.json"),
            "goreleaser": str(self.tools / "bin" / "goreleaser"),
        }
        values.update(overrides)
        return ["build", *(item for key, value in values.items() for item in (f"--{key}", value))]


class FixtureCase(unittest.TestCase):
    """One fixture tree per class and one disposed case tree per test, both under FixtureTree."""

    fixture: CandidateFixture

    @classmethod
    def setUpClass(cls):
        tree = FixtureTree.open(prefix="candidate-build-fixture", label="candidate build fixture")
        cls.addClassCleanup(tree.dispose)
        cls.fixture = CandidateFixture(tree)

    def setUp(self):
        self.fixture.clear_modes()
        self.case = FixtureTree.open(self.fixture.root, prefix="candidate-build-case",
                                     label="candidate build case")
        self.addCleanup(self.case.dispose)
        self.scratch = self.case.root
        self.addCleanup(self.fixture.restore)

    def run_builder(self, *arguments: str, environment: dict[str, str] | None = None) -> CliResult:
        return self.fixture.builder(self.case, *arguments, environment=environment)

    def build(self, *, environment: dict[str, str] | None = None, **overrides: str) -> CliResult:
        return self.run_builder(*self.fixture.build_arguments(self.scratch, **overrides),
                                environment=environment)

    def receipt(self) -> dict:
        return json.loads((self.scratch / "receipt.json").read_text())

    def source_snapshot(self) -> tuple[str, str, str]:
        repository = self.fixture.repository
        owner = self.fixture.fixture_tree
        return (git(repository, "for-each-ref", tree=owner), git(repository, "tag", "--list", tree=owner),
                git(repository, "status", "--porcelain=v1", "--untracked-files=all", tree=owner))
