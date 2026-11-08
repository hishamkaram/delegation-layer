"""Consumer-owned ports and private workspace layout for published verification."""

from __future__ import annotations

from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from pathlib import Path
import os
import shutil
from typing import Protocol
from scripts.acceptance.evidence.source import command
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.published_files import create_private_file


SYSTEM_PATH = ("/usr/bin", "/bin", "/usr/sbin", "/sbin")


class ConsumerRun(Protocol):
    def __call__(self, receipt: dict, name: str, argv: list[str], env: dict[str, str], *,
                 capture: bool = False, timeout: float = 30,
                 cwd: Path | None = None) -> bytes: ...


@dataclass(frozen=True)
class ConsumerPorts:
    """Everything the channel verifiers may touch outside the private workspace.

    run is release_verification.commands.command (receipt-recording, bounded,
    keyword cwd); tests replace it, production never constructs another child owner
    for these ports. The candidate install observers also call the caller's
    BootstrapProbe, whose fixture children run through the private CLI's
    identity-pinned file capture instead; that capture is neither this run port
    nor bounded-output.
    """

    run: ConsumerRun
    which: Callable[[str], str | None]
    inherited: Mapping[str, str] = field(default_factory=dict)


def default_ports() -> ConsumerPorts:
    return ConsumerPorts(run=command, which=shutil.which, inherited=dict(os.environ))


@dataclass(frozen=True)
class ConsumerLayout:
    """Private directories below the single owned workspace root."""

    root: Path
    cwd: Path
    home: Path
    cache: Path
    user_npmrc: Path
    global_npmrc: Path
    pack: Path


def prepare_layout(root: Path) -> ConsumerLayout:
    layout = ConsumerLayout(
        root=root, cwd=root / "cwd", home=root / "home", cache=root / "cache",
        user_npmrc=root / "empty-user.npmrc", global_npmrc=root / "empty-global.npmrc",
        pack=root / "pack",
    )
    for directory in (layout.cwd, layout.home, layout.cache, layout.pack):
        directory.mkdir(mode=0o700)
    create_private_file(layout.user_npmrc)
    create_private_file(layout.global_npmrc)
    return layout


def tool_path(ports: ConsumerPorts, names: tuple[str, ...]) -> str:
    """Search path made only of the resolved tools' directories and system directories."""
    directories: list[str] = []
    for name in names:
        found = ports.which(name)
        if found is None:
            raise VerificationError(f"{name}_unavailable", "locate-tools")
        parent = str(Path(found).parent)
        if parent not in directories:
            directories.append(parent)
    return os.pathsep.join([*directories, *SYSTEM_PATH])
