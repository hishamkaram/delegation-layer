"""Private CLI installation boundary."""

import os
from pathlib import Path
import sys

from scripts.acceptance.private_cli.errors import InstallationFailure
from scripts.acceptance.private_cli.errors import InstallationPhase
from scripts.acceptance.private_cli.fingerprints import REPOSITORY
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.observation import inherited_environment
from scripts.acceptance.processes.operations import run_bounded_command


INSTALL_TIMEOUT_SECONDS = 900
MAX_INSTALL_OUTPUT = 1024 * 1024


def require_source_root(source_root: Path) -> Path:
    """Select the caller's checkout, including its current uncommitted source."""
    try:
        source = source_root.resolve(strict=True)
        required = ("go.mod", "cmd/delegate/main.go", "cmd/delegate-run/main.go")
        if not source.is_dir() or any(not (source / name).is_file() for name in required):
            raise RuntimeError("candidate source root must contain go.mod and both Delegate commands")
    except OSError as error:
        raise RuntimeError("candidate source root is unavailable: " + str(source_root)) from error
    return source


def _install_command(arguments: list[str], *, cwd: Path, phase: InstallationPhase) -> None:
    environment = inherited_environment()
    environment.update({"GOTOOLCHAIN": "local", "CGO_ENABLED": "0"})
    try:
        result = run_bounded_command(
            arguments, environment, cwd=cwd, timeout=INSTALL_TIMEOUT_SECONDS,
            capture=True, capture_stderr=True, max_output=MAX_INSTALL_OUTPUT,
        )
    except BoundedCommandError as error:
        raise InstallationFailure(phase, error.reason) from error
    if result.exit_code:
        # Compiler/download output can contain private paths or inherited settings.
        raise InstallationFailure(phase, result.exit_code)


def build_candidate_pair(source_root: Path, destination: Path) -> None:
    """Build directly into an owned installation; never read installed program contents."""
    source = require_source_root(source_root)
    for name in ("delegate", "delegate-run"):
        target = destination / name
        if target.exists() or target.is_symlink():
            raise RuntimeError("candidate installation destination is occupied: " + str(target))
    for name in ("delegate", "delegate-run"):
        _install_command(
            ["go", "build", "-buildvcs=false", "-ldflags", "-X main.version=dev",
             "-o", str(destination / name), "./cmd/" + name], cwd=source,
            phase=InstallationPhase.BUILD_DELEGATE if name == "delegate" else InstallationPhase.BUILD_DELEGATE_RUN,
        )
        target = destination / name
        if target.is_symlink() or not target.is_file() or not os.access(target, os.X_OK):
            raise RuntimeError("candidate build did not install executable: " + name)


def install_supervisor_pair(destination: Path) -> None:
    """Install the independently verified original archive through its one pin owner."""
    _install_command(
        [sys.executable, str(REPOSITORY / "scripts/fetch_pueue_assets.py"), "--host", str(destination)],
        cwd=REPOSITORY, phase=InstallationPhase.INSTALL_SUPERVISOR,
    )


def package_layout(root, symlinked):
    if not symlinked:
        install = root / "install"
        return install, install, root / "libexec"
    cellar = root / "Cellar" / "delegation-layer" / "0.1.0"
    return root / "bin", cellar / "bin", cellar / "libexec"


def install_package_layout(owner, source_root, root, symlinked):
    if not symlinked:
        install = owner.create("package install", root / "install", root).directory
        canonical_bin = install
        libexec = owner.create("package libexec", root / "libexec", root).directory
    else:
        cellar = owner.create("package Cellar", root / "Cellar", root).directory
        product = owner.create("package product", cellar / "delegation-layer", cellar).directory
        version = owner.create("package version", product / "0.1.0", product).directory
        canonical_bin = owner.create("package bin", version / "bin", version).directory
        libexec = owner.create("package libexec", version / "libexec", version).directory
        install = owner.create("package install links", root / "bin", root).directory
    build_candidate_pair(source_root, canonical_bin)
    if not symlinked:
        return install, install, libexec
    for name in ("delegate", "delegate-run"):
        link = install / name
        link.symlink_to(os.path.relpath(canonical_bin / name, install))
    return install, canonical_bin, libexec


def install_adjacent_candidate(owner, source_root, root):
    """Own one independent adjacent installation for the public-default-path proof."""
    directory = owner.create("adjacent candidate installation", root, root.parent).directory
    build_candidate_pair(source_root, directory)
    install_supervisor_pair(directory)
    return {name: directory / name for name in ("delegate", "delegate-run", "pueue", "pueued")}


def install_adjacent_package(owner, sources, root):
    """Select the existing adjacent installation in place; the caller retains its owner."""
    names = ("delegate", "delegate-run", "pueue", "pueued")
    if set(sources) != set(names):
        raise RuntimeError("packaged installation must select all four roles")
    selected = {name: sources[name].resolve(strict=True) for name in names}
    directory = selected["delegate"].parent
    for name, path in selected.items():
        if path != directory / name or not path.is_file() or not os.access(path, os.X_OK):
            raise RuntimeError("packaged installation is not an adjacent executable layout: " + name)
    return directory
