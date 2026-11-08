#!/usr/bin/env python3
"""Install only Pueue from fixed, verified historical distribution archives.

Archive integrity is checked on every invocation. Selected/installed executable
contents are never hashed, executed, cached or recorded in a new manifest.
"""

from __future__ import annotations

import argparse
from collections.abc import Generator, Mapping
from contextlib import contextmanager
from dataclasses import dataclass
import gzip
import hashlib
import os
from pathlib import Path
import platform
import secrets
import signal
import stat
import sys
import tarfile
from types import FrameType, MappingProxyType
from typing import IO, Literal
import urllib.error
import urllib.request

if __package__ in {None, ""}:
    import github_release_asset
else:
    from . import github_release_asset


RELEASE_URL = "https://github.com/hishamkaram/delegation-layer/releases/download/v0.3.1"
# Whole compressed distribution archives, not hashes of executable members.
ARCHIVE_PINS: Mapping[tuple[str, str], str] = MappingProxyType({
    ("darwin", "amd64"): "7e8bb4604e2891f604974a11592276c73f3690caa99e43fecca6416a3bf16282",
    ("darwin", "arm64"): "43f3be5957b4df05550e7e0ebb6dec971ae78c027204fbbf2fb8f0444ee6326b",
    ("linux", "amd64"): "a779a3fb89382ef9c695d6110878770d2709f34c78215465e08d6ab9e3af17ef",
    ("linux", "arm64"): "a1553e95f67aec07d50fa9f2b2a1c54188fa4518be6aeeef429596d912588475",
})
PAIR = ("pueue", "pueued")
MEMBERS = frozenset((*PAIR, "delegate", "delegate-run"))
MIB = 1024 * 1024
# Observed v0.3.1 maxima: 34,750,543 compressed; <66MB expanded;
# selected member 5,872,616; other member 27,161,408 bytes; four flat files.
MAX_ARCHIVE = 40 * MIB
MAX_TAR = 80 * MIB
MAX_SELECTED = 8 * MIB
MAX_OTHER = 32 * MIB
CHUNK = 128 * 1024
DIRECTORY_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC


class InstallError(Exception):
    """An archive or destination could not be safely installed."""


@dataclass(frozen=True)
class Destination:
    requested: Path
    fd: int
    lineage: tuple[tuple[int, str, int], ...]

    def check(self) -> None:
        for parent, name, child in self.lineage:
            named = os.stat(name, dir_fd=parent, follow_symlinks=False)
            held = os.fstat(child)
            if not stat.S_ISDIR(named.st_mode) or not os.path.samestat(named, held):
                raise InstallError("output directory identity changed")
        named = self.requested.lstat()
        if not stat.S_ISDIR(named.st_mode) or not os.path.samestat(named, os.fstat(self.fd)):
            raise InstallError("requested output directory identity changed")
        for role in PAIR:
            try:
                target = os.stat(role, dir_fd=self.fd, follow_symlinks=False)
            except FileNotFoundError:
                continue
            if not stat.S_ISREG(target.st_mode):
                raise InstallError(f"output {role} must be a regular file, not a symlink or special file")


@contextmanager
def destination(path: Path) -> Generator[Destination, None, None]:
    requested = path if path.is_absolute() else Path.cwd() / path
    if requested.is_symlink():
        raise InstallError("output directory must not be a symlink")
    # Native resolution supports legitimate parent aliases, e.g. macOS /var.
    canonical = requested.resolve(strict=False)
    descriptors = [os.open(canonical.anchor, DIRECTORY_FLAGS)]
    lineage: list[tuple[int, str, int]] = []
    operation_error: BaseException | None = None
    try:
        for name in canonical.parts[1:]:
            parent = descriptors[-1]
            try:
                os.mkdir(name, 0o755, dir_fd=parent)
            except FileExistsError:
                pass  # Opening with O_DIRECTORY/O_NOFOLLOW validates the existing component.
            child = os.open(name, DIRECTORY_FLAGS, dir_fd=parent)
            descriptors.append(child)
            lineage.append((parent, name, child))
        result = Destination(requested, descriptors[-1], tuple(lineage))
        result.check()
        yield result
    except BaseException as error:
        operation_error = error
        raise
    finally:
        faults: list[BaseException] = []
        for descriptor in reversed(descriptors):
            try:
                os.close(descriptor)
            except BaseException as error:
                faults.append(error)
        if faults:
            cleanup_error = BaseExceptionGroup("destination descriptor close faults", faults)
            if operation_error is not None:
                raise BaseExceptionGroup("installation and destination cleanup failed", [operation_error, cleanup_error])
            raise cleanup_error


def remove_staging(output: Destination, name: str, stage: int | None, identity: os.stat_result | None) -> None:
    faults: list[OSError] = []
    if stage is not None:
        try:
            if identity is None or not os.path.samestat(identity, os.fstat(stage)):
                raise OSError("staging handle identity changed")
            entries = os.listdir(stage)
        except OSError as error:
            entries = []
            faults.append(error)
        for entry in entries:
            try:
                os.unlink(entry, dir_fd=stage)
            except OSError as error:
                faults.append(error)
        try:
            os.close(stage)
        except OSError as error:
            faults.append(error)
    try:
        named = os.stat(name, dir_fd=output.fd, follow_symlinks=False)
        if identity is None or not stat.S_ISDIR(named.st_mode) or not os.path.samestat(identity, named):
            raise OSError("staging directory identity changed; replacement retained")
        os.rmdir(name, dir_fd=output.fd)
    except OSError as error:
        faults.append(error)
    if faults:
        detail = "; ".join(str(error) for error in faults)
        raise InstallError(f"could not clean staging {output.requested / name}: {detail}") from ExceptionGroup("staging cleanup faults", faults)


@contextmanager
def staging(output: Destination) -> Generator[int, None, None]:
    name = ".pueue-install-" + secrets.token_hex(12)
    os.mkdir(name, 0o700, dir_fd=output.fd)
    stage: int | None = None
    identity: os.stat_result | None = None
    operation_error: BaseException | None = None
    try:
        identity = os.stat(name, dir_fd=output.fd, follow_symlinks=False)
        stage = os.open(name, DIRECTORY_FLAGS, dir_fd=output.fd)
        actual = os.fstat(stage)
        if not os.path.samestat(identity, actual):
            raise InstallError("staging directory identity changed during open")
        identity = actual
        yield stage
    except BaseException as error:
        # This resource owner must also unwind interruption; nothing is swallowed.
        operation_error = error
        raise
    finally:
        try:
            remove_staging(output, name, stage, identity)
        except InstallError as error:
            if operation_error is not None:
                raise BaseExceptionGroup("installation and staging cleanup failed", [operation_error, error]) from error
            raise


@contextmanager
def stage_file(stage: int, name: str, mode: Literal["rb", "wb"]) -> Generator[IO[bytes], None, None]:
    flags = os.O_NOFOLLOW | os.O_CLOEXEC
    flags |= os.O_WRONLY | os.O_CREAT | os.O_EXCL if mode == "wb" else os.O_RDONLY
    descriptor = os.open(name, flags, 0o600, dir_fd=stage)
    with os.fdopen(descriptor, mode) as handle:
        yield handle


def copy_bounded(source: IO[bytes] | gzip.GzipFile, target: IO[bytes], limit: int) -> int:
    total = 0
    while chunk := source.read(min(CHUNK, limit - total + 1)):
        total += len(chunk)
        if total > limit:
            raise InstallError(f"archive stream exceeds its {limit}-byte bound")
        written = target.write(chunk)
        if written != len(chunk):
            raise InstallError("archive staging write made incomplete progress")
    return total


def download_archive(url: str, target: IO[bytes]) -> None:
    # Redirects are required by GitHub release assets; reject protocol downgrade.
    token = github_release_asset.validate_token(os.environ.get(github_release_asset.TOKEN_ENV))
    request = urllib.request.Request(url, headers={"User-Agent": "delegation-layer-pueue-installer"})
    opener = urllib.request.build_opener(github_release_asset.HTTPSRedirects())
    try:
        response = opener.open(request, timeout=30)
    except (urllib.error.URLError, TimeoutError, ConnectionError) as error:
        if not github_release_asset.transient_error(error):
            raise
        print(f"Release download failed ({error}); trying the exact pinned asset through GitHub's API.",
              file=sys.stderr)
        try:
            github_release_asset.download_asset(url, target, MAX_ARCHIVE, token=token)
        except (github_release_asset.DownloadError, OSError) as fallback:
            raise InstallError(f"release download failed ({error}); API fallback failed: {fallback}") from fallback
        return
    with response:
        if not response.url.startswith("https://"):
            raise InstallError("archive download redirected away from HTTPS")
        if copy_bounded(response, target, MAX_ARCHIVE) == 0:
            raise InstallError("archive download was empty")


def verify_archive(stage: int, expected: str) -> None:
    digest = hashlib.sha256()
    with stage_file(stage, "distribution.tar.gz", "rb") as archive:
        total = 0
        while chunk := archive.read(CHUNK):
            total += len(chunk)
            if total > MAX_ARCHIVE:
                raise InstallError("downloaded archive is oversized")
            digest.update(chunk)
    if total == 0 or digest.hexdigest() != expected:
        raise InstallError("downloaded distribution archive checksum mismatch")


def validate_members(container: IO[bytes]) -> dict[str, tarfile.TarInfo]:
    selected: dict[str, tarfile.TarInfo] = {}
    seen: set[str] = set()
    length = container.seek(0, os.SEEK_END)
    container.seek(0)
    while True:
        header = container.read(tarfile.BLOCKSIZE)
        if len(header) != tarfile.BLOCKSIZE:
            raise InstallError("truncated distribution tar header")
        if header == bytes(tarfile.BLOCKSIZE):
            # Require the tar end marker and reject appended/concatenated entries.
            tail = container.read(tarfile.BLOCKSIZE)
            if tail != bytes(tarfile.BLOCKSIZE):
                raise InstallError("invalid distribution tar end marker")
            while chunk := container.read(CHUNK):
                if any(chunk):
                    raise InstallError("unexpected trailing distribution tar data")
            break
        member = tarfile.TarInfo.frombuf(header, "utf-8", "strict")
        if member.name not in MEMBERS or member.name in seen or len(seen) >= len(MEMBERS):
            raise InstallError(f"unexpected or duplicate archive member: {member.name!r}")
        seen.add(member.name)
        limit = MAX_SELECTED if member.name in PAIR else MAX_OTHER
        if member.type not in {tarfile.REGTYPE, tarfile.AREGTYPE} or member.linkname:
            raise InstallError(f"archive member must be a plain regular file: {member.name}")
        if not 0 < member.size <= limit or not member.mode & 0o111 or member.mode & 0o7000:
            raise InstallError(f"invalid size or executable mode for archive member: {member.name}")
        member.offset_data = container.tell()
        following = member.offset_data + ((member.size + 511) // 512) * 512
        if following > length:
            raise InstallError(f"truncated archive member: {member.name}")
        if member.name in PAIR:
            selected[member.name] = member
        container.seek(following)
    if seen != MEMBERS:
        raise InstallError("archive is missing required distribution members")
    return selected


def copy_member(container: IO[bytes], target: IO[bytes], member: tarfile.TarInfo) -> None:
    container.seek(member.offset_data)
    remaining = member.size
    while remaining:
        chunk = container.read(min(CHUNK, remaining))
        if not chunk or target.write(chunk) != len(chunk):
            raise InstallError(f"incomplete archive member read/write: {member.name}")
        remaining -= len(chunk)


def stage_pair(stage: int) -> None:
    # Bound decompression before tar metadata parsing, including extension headers.
    # This remains an archive container; unselected executable members are never extracted.
    with stage_file(stage, "distribution.tar.gz", "rb") as compressed:
        with gzip.GzipFile(fileobj=compressed) as source, stage_file(stage, "distribution.tar", "wb") as target:
            copy_bounded(source, target, MAX_TAR)
    with stage_file(stage, "distribution.tar", "rb") as container:
        selected = validate_members(container)
        for role in PAIR:
            with stage_file(stage, role, "wb") as target:
                copy_member(container, target, selected[role])
                os.fchmod(target.fileno(), 0o755)


def install_pair(output_path: Path, system: str, architecture: str) -> None:
    key = (system, architecture)
    if key not in ARCHIVE_PINS:
        raise InstallError(f"unsupported Pueue platform: {system}/{architecture}")
    archive_name = f"delegation-layer_0.3.1_{system.title()}_{architecture}.tar.gz"
    with destination(output_path) as output, staging(output) as stage:
        with stage_file(stage, "distribution.tar.gz", "wb") as target:
            download_archive(f"{RELEASE_URL}/{archive_name}", target)
        verify_archive(stage, ARCHIVE_PINS[key])
        stage_pair(stage)
        output.check()
        try:
            for role in PAIR:
                os.replace(role, role, src_dir_fd=stage, dst_dir_fd=output.fd)
        except OSError as error:
            raise InstallError(f"could not publish Pueue pair; installation may be partial: {error}") from error
        output.check()


def host_platform() -> tuple[str, str]:
    system = platform.system().lower()
    machine = platform.machine().lower()
    architecture = {"x86_64": "amd64", "amd64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(machine, machine)
    if (system, architecture) not in ARCHIVE_PINS:
        raise InstallError(f"unsupported Pueue host: {system}/{machine}")
    return system, architecture


class InstallInterrupted(Exception):
    def __init__(self, number: int) -> None:
        super().__init__(f"installation interrupted by signal {number}")
        self.number = number


def interrupt_install(number: int, frame: FrameType | None) -> None:
    raise InstallInterrupted(number)


@contextmanager
def installation_signals() -> Generator[None, None, None]:
    # These are handlers for this installer, never signals sent to another actor.
    previous = []
    try:
        for number in (signal.SIGTERM, signal.SIGHUP):
            previous.append((number, signal.signal(number, interrupt_install)))
        yield
    finally:
        for number, handler in reversed(previous):
            signal.signal(number, handler)


class Arguments(argparse.Namespace):
    host: bool = False
    output: Path | None = None


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", action="store_true", help="install only this host's pair directly in OUTPUT")
    parser.add_argument("output", nargs="?", type=Path)
    args = Arguments()
    parser.parse_args(argv, namespace=args)
    if args.host and args.output is None:
        parser.error("--host requires OUTPUT")
    output = args.output if args.output is not None else Path(".release/pueue")
    try:
        with installation_signals():
            if args.host:
                install_pair(output, *host_platform())
            else:
                for system, architecture in ARCHIVE_PINS:
                    install_pair(output / system / architecture, system, architecture)
    except (InstallError, github_release_asset.DownloadError, OSError, tarfile.TarError, UnicodeError, EOFError) as error:
        print(f"Pueue archive installation failed: {error}", file=sys.stderr)
        return 1
    except InstallInterrupted as error:
        print(str(error), file=sys.stderr)
        return 128 + error.number
    except BaseExceptionGroup as error:
        # Preserve every operation/cleanup failure, including interrupted cleanup.
        print(f"Pueue installation/cleanup failed: {error!r}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("Pueue archive installation interrupted", file=sys.stderr)
        return 130
    print(f"Prepared bundled Pueue 4.0.4 in {output}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
