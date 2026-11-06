"""Fixtures for candidate install proof: real archive layout, real npm TGZ, recording ports."""

from __future__ import annotations

from collections.abc import Callable, Generator
import contextlib
from dataclasses import dataclass
from functools import partial
from pathlib import Path
import hashlib
import io
import json
import shutil
import sys
import tarfile
import unittest
from unittest import mock

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.processes.contracts import BoundedCommandResult
from scripts.delegate_acceptance.release_verification import candidate_install as install
from scripts.delegate_acceptance.release_verification import commands, github
from scripts.delegate_acceptance.release_verification.assets import platform_identity
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    BootstrapObservation, BootstrapTarget, CandidateFiles, CandidateInstallFacts, CandidateInstallRequest,
)
from scripts.delegate_acceptance.release_verification.commands import command
from scripts.delegate_acceptance.release_verification.contract import BINARIES, new_receipt
from scripts.delegate_acceptance.release_verification.published_contract import npm_environment
from scripts.delegate_acceptance.release_verification.published_ports import ConsumerPorts, tool_path


REPO_ROOT = Path(__file__).resolve().parent.parent
SECRET = "candidate-install-test-secret-must-not-leak"
REPO = "hishamkaram/delegation-layer"
TREE_PREFIX = "candidate-install-test"
TREE_LABEL = "candidate install test fixture"
SKILL_ROOT = REPO_ROOT / "skills" / "agent-integration"


def package_version() -> str:
    return json.loads((REPO_ROOT / "package.json").read_text(encoding="utf-8"))["version"]


def fake_binary(name: str, version: str) -> bytes:
    """Shell stand-ins that answer only the pure default-path commands."""
    if name == "delegate":
        return f"""#!/bin/sh
case "$1" in
  version) printf 'delegate {version}\\n' ;;
  providers) printf '{{"providers":[]}}\\n' ;;
  capabilities) printf '{{}}\\n' ;;
  supervisor) printf '{{"command":"supervisor","supervisor_control":{{"state":"not_initialized"}}}}\\n' ;;
  *) exit 8 ;;
esac
""".encode()
    if name in {"pueue", "pueued"}:
        return f"#!/bin/sh\nprintf '{name} 4.0.4\\n'\n".encode()
    return b"#!/bin/sh\nexit 0\n"


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def checkout_skill_digests() -> dict[str, str]:
    """Independent oracle: digest the skill files of this checkout, which the package ships."""
    return {
        str(path.relative_to(SKILL_ROOT)): sha256(path.read_bytes())
        for path in sorted(SKILL_ROOT.rglob("*")) if path.is_file()
    }


def write_archive(directory: Path, tag: str, binaries: dict[str, bytes], *, checksum: str | None = None) -> tuple[Path, Path]:
    """Release-shaped archive plus checksums.txt; checksum overrides the recorded digest."""
    name = f"delegation-layer_{tag[1:]}_{platform_identity()['os']}_{platform_identity()['arch']}.tar.gz"
    archive = directory / name
    with tarfile.open(archive, "w:gz") as bundle:
        for member_name, data in binaries.items():
            member = tarfile.TarInfo(member_name)
            member.size, member.mode = len(data), 0o755
            bundle.addfile(member, io.BytesIO(data))
    checksums = directory / "checksums.txt"
    digest = checksum or sha256(archive.read_bytes())
    checksums.write_text(f"{digest}  {name}\n", encoding="ascii")
    return archive, checksums


Launch = Callable[..., BoundedCommandResult]


def _observed(runner: Launch, *, tree: FixtureTree, dependencies: tuple[FixtureTree, ...]) -> Launch:
    def launch(argv: list[str], environment: dict[str, str], **options: object) -> BoundedCommandResult:
        return tree.observe_operation(lambda: runner(argv, environment, **options), dependencies=dependencies)
    return launch


@contextlib.contextmanager
def observed_children(tree: FixtureTree, *, dependencies: tuple[FixtureTree, ...] = ()) -> Generator[None, None, None]:
    """Each release child's original failure reaches ``tree`` and ``dependencies`` before any conversion.

    Release children launch through the canonical runner as bound in ``commands`` (ports, Git and
    the by-name default-path checks) and in ``github`` (GitHub API requests). Each binding, including
    a fault a test installed beneath it, is wrapped in place until the block exits.
    """
    with contextlib.ExitStack() as patches:
        for module in (commands, github):
            launch = _observed(module.run_bounded_command, tree=tree, dependencies=dependencies)
            patches.enter_context(mock.patch.object(module, "run_bounded_command", launch))
        yield


@dataclass
class Call:
    name: str
    argv: tuple[str, ...]
    env: dict[str, str]
    cwd: Path | None


class RecordingPorts:
    """Bounded commands behind ConsumerPorts.run, recorded; closure failures reach the fixture.

    ``dependencies`` are the other trees a port child reads from or is contained by. The ``after``
    hook may mutate their files, so it never runs while any of these trees holds an unresolved child.
    """

    def __init__(self, tree: FixtureTree, *, missing: frozenset[str] = frozenset(),
                 after: Callable[[str], None] | None = None, execute=command,
                 dependencies: tuple[FixtureTree, ...] = ()):
        self.tree = tree
        self.dependencies = dependencies
        self.calls: list[Call] = []
        self.missing = missing
        self.after = after
        self.execute = execute

    def which(self, name: str) -> str | None:
        return None if name in self.missing else shutil.which(name)

    def run(self, receipt: dict, name: str, argv: list[str], env: dict[str, str], *,
            capture: bool = False, timeout: float = 30, cwd: Path | None = None) -> bytes:
        self.calls.append(Call(name, tuple(argv), dict(env), cwd))
        try:
            return self.tree.observe_operation(
                partial(self.execute, receipt, name, argv, env, capture=capture, timeout=timeout, cwd=cwd),
                dependencies=self.dependencies)
        finally:
            if self.after is not None and all(tree.unresolved is None for tree in (self.tree, *self.dependencies)):
                self.after(name)

    def ports(self) -> ConsumerPorts:
        return ConsumerPorts(
            run=self.run, which=self.which,
            inherited={"GH_TOKEN": SECRET, "NPM_TOKEN": SECRET, "SSL_CERT_FILE": "/etc/ssl/cert.pem"})


def accepted_bootstrap(target: BootstrapTarget) -> BootstrapObservation:
    """The accepted turn of exactly the target's expected binaries."""
    return fx.bootstrap(target.binaries)


class RecordingProbe:
    """A unit-isolation BootstrapProbe: records each target, then returns ``observe(target)``.

    It stands in only for the installed fixture turn that InstalledBootstrapProbe runs;
    the version-only fake binaries cannot run that turn.
    """

    def __init__(self, observe: Callable[[BootstrapTarget], BootstrapObservation] = accepted_bootstrap):
        self.targets: list[BootstrapTarget] = []
        self.observe = observe

    def __call__(self, target: BootstrapTarget) -> BootstrapObservation:
        self.targets.append(target)
        return self.observe(target)


def pack_checkout(destination: Path, scratch: Path) -> Path:
    """npm pack of this checkout through the shared bounded runner, outside the repo tree."""
    npm = shutil.which("npm")
    home, cache = scratch / "home", scratch / "cache"
    for directory in (home, cache):
        directory.mkdir()
    user_rc, global_rc = scratch / "user.npmrc", scratch / "global.npmrc"
    for config in (user_rc, global_rc):
        config.write_bytes(b"")
    env = npm_environment(
        path=tool_path(ConsumerPorts(run=command, which=shutil.which), ("npm", "node")), home=str(home),
        cache=str(cache), user_npmrc=str(user_rc), global_npmrc=str(global_rc), inherited={})
    receipt = new_receipt("candidate-install-pack", "v0.0.0", "a" * 40)
    assert npm is not None, "npm prerequisite was not resolved"
    output = command(receipt, "npm-pack", [
        npm, "pack", "--ignore-scripts", "--json", "--pack-destination", str(destination)],
        env, capture=True, timeout=180, cwd=REPO_ROOT)
    document = json.loads(output)
    entry = next(iter(document.values())) if isinstance(document, dict) else document[0]
    return destination / entry["filename"]


class CandidateCase(unittest.TestCase):
    """Candidate files, runner parent, receipt and recording ports inside one FixtureTree."""

    tag = "v" + package_version()
    package: Path | None = None
    # The tree holding ``package``, an input of every npm child.
    pack_tree: FixtureTree | None = None
    # The tree this case's tree is created in; its cleanup would remove a retained case tree.
    containing_tree: FixtureTree | None = None

    def setUp(self) -> None:
        self.dependencies = tuple(tree for tree in (self.pack_tree, self.containing_tree) if tree is not None)
        self.require_resolved(self.dependencies)
        parent = None if self.containing_tree is None else self.containing_tree.root
        self.tree = FixtureTree.open(parent, prefix=TREE_PREFIX, label=TREE_LABEL)
        self.addCleanup(self.tree.dispose)
        self.recording = RecordingPorts(self.tree, dependencies=self.dependencies)
        self.probe = RecordingProbe()
        self.root = self.tree.root
        self.candidate = self.root / "candidate"
        self.parent = self.root / "runner"
        for directory in (self.candidate, self.parent):
            directory.mkdir()
        self.receipt = new_receipt("candidate-install", self.tag, "a" * 40)
        self.binaries = {name: fake_binary(name, self.tag[1:]) for name in BINARIES}

    def require_resolved(self, trees: tuple[FixtureTree, ...]) -> None:
        """Refuse any write or launch while one of ``trees`` holds a child whose closure is unknown."""
        for tree in trees:
            self.assertIsNone(tree.unresolved, f"{tree.label} holds an unresolved child: {tree.root}")

    def finish_workspace(self) -> None:
        """Dispose of the tree the way a successful caller would; nothing may remain."""
        self.assertTrue(self.tree.dispose())
        self.assertFalse(self.root.exists())

    def request(self, *, binaries: dict[str, bytes] | None = None, checksum: str | None = None,
                package: Path | None = None, skill: dict[str, str] | None = None,
                repo: str = REPO) -> CandidateInstallRequest:
        self.require_resolved((self.tree, *self.dependencies))
        data = self.binaries if binaries is None else binaries
        archive, checksums = write_archive(self.candidate, self.tag, data, checksum=checksum)
        return CandidateInstallRequest(
            tag=self.tag, repo=repo, installer=REPO_ROOT / "install.sh",
            files=CandidateFiles(archive, checksums, package or self.package),
            binaries=tuple(sorted((name, sha256(self.binaries[name])) for name in BINARIES)),
            skill=tuple(sorted((checkout_skill_digests() if skill is None else skill).items())))

    def verify(self, request: CandidateInstallRequest, *, channels: tuple[str, ...],
               info: dict[str, str] | None = None) -> CandidateInstallFacts:
        """Install through the real entrypoint; ``info`` is the expected platform, the host's by default."""
        self.require_resolved((self.tree, *self.dependencies))
        layout = install.open_workspace(owner=self.tree.owner, parent=self.parent, receipt=self.receipt)
        self.layout = layout
        with observed_children(self.tree, dependencies=self.dependencies):
            return install.verify_candidate_install(
                request, ports=self.recording.ports(), probe=self.probe, receipt=self.receipt, layout=layout,
                info=platform_identity() if info is None else info, channels=channels)


class PackedCandidateCase(CandidateCase):
    """A CandidateCase whose class packs this checkout once, inside its own FixtureTree."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.pack_tree = FixtureTree.open(prefix=TREE_PREFIX, label=TREE_LABEL)
        cls.addClassCleanup(cls.pack_tree.dispose)
        (cls.pack_tree.root / "pack").mkdir()
        with observed_children(cls.pack_tree):
            cls.package = pack_checkout(cls.pack_tree.root / "pack", cls.pack_tree.root)
