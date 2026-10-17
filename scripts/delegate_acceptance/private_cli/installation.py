"""Private CLI installation boundary."""

from scripts.delegate_acceptance.private_cli.fingerprints import file_fingerprint
import os
import shutil


def install_fixture(source, destination):
    if not source.is_file() or not os.access(source, os.X_OK):
        raise RuntimeError("required executable is missing: " + str(source))
    shutil.copyfile(source, destination)
    destination.chmod(0o700)


def package_layout(root, symlinked):
    if not symlinked:
        install = root / "install"
        return install, install, root / "libexec"
    cellar = root / "Cellar" / "delegation-layer" / "0.1.0"
    return root / "bin", cellar / "bin", cellar / "libexec"


def install_package_layout(owner, delegate, runner, root, symlinked):
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
    install_fixture(delegate, canonical_bin / "delegate")
    install_fixture(runner, canonical_bin / "delegate-run")
    if not symlinked:
        return install, install, libexec
    for name in ("delegate", "delegate-run"):
        link = install / name
        link.symlink_to(os.path.relpath(canonical_bin / name, install))
    return install, canonical_bin, libexec


def install_adjacent_package(owner, sources, root):
    """Copy the four release binaries into one archive-style install directory."""
    install = owner.create("packaged default install", root, root.parent).directory
    for name in ("delegate", "delegate-run", "pueue", "pueued"):
        install_fixture(sources[name], install / name)
        copied = install / name
        source_fingerprint = file_fingerprint(sources[name])
        copied_fingerprint = file_fingerprint(copied)
        if (
            copied.is_symlink()
            or copied_fingerprint.get("bytes") != source_fingerprint.get("bytes")
            or copied_fingerprint.get("sha256") != source_fingerprint.get("sha256")
        ):
            raise RuntimeError("packaged binary copy changed identity: " + name)
    return install
