"""Official historical archive inputs; never hash selected executable contents."""

from dataclasses import dataclass
import gzip
import hashlib
import os
from pathlib import Path
import platform
import tarfile
import tempfile

from scripts import github_release_asset
from scripts.acceptance.evidence.validation import require
from scripts.fetch_pueue_assets import copy_bounded


MAX_ARCHIVE = 128 << 20
MAX_EXPANDED = 256 << 20
MEMBERS = frozenset(("delegate", "delegate-run", "pueue", "pueued"))


@dataclass(frozen=True)
class Release:
    version: str
    commit: str
    schema: int
    checksums_sha256: str
    archives: tuple[tuple[str, str, str], ...]

    def asset(self, system: str, machine: str) -> tuple[str, str]:
        architecture = {"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(machine)
        for os_name, arch, digest in self.archives:
            if system.lower() == os_name and architecture == arch:
                return f"delegation-layer_{self.version}_{os_name.capitalize()}_{arch}.tar.gz", digest
        raise RuntimeError("historical scheduler release does not support this host")

    @property
    def url(self) -> str:
        return "https://github.com/hishamkaram/delegation-layer/releases/download/v" + self.version


RELEASES = (
    Release("0.4.2", "5a8964a4e5f94c52513918d46f4baa7f50273429", 1,
            "617974bc2a33700b2272c1f8e0f52172e028b0803978f540472ecb9ae2de3c4f", (
                ("darwin", "amd64", "2daa42a0a88503976216bb06230535dc5e28503e20285f4af6e66f9c6373aaab"),
                ("darwin", "arm64", "a99017e5418b98189a7ae38cfdd175a27e4a6c8f4a706bb03d75456df79adfb1"),
                ("linux", "amd64", "318ac02fd3a5802950ccc61b430edbaf283e83648ea1be9efbe785ed13a7b40a"),
                ("linux", "arm64", "bf048970128f506815c7958a76c0550f2f413638629aaf88132cf0bfb6b22181"))),
    Release("0.5.1", "0eed5e28288628490c6fb0297616f4d64d4584c9", 2,
            "305eb10e560ce317266746f91c33082752e81252764afc63c2d8d08e541b84fa", (
                ("darwin", "amd64", "a847d189198b18584f3012a61e31ff827d23f0caa95de85fbb9b6cecfbc39c37"),
                ("darwin", "arm64", "2433175d23d3e8bdfd0953f162b57817efe896ae9ac38cdfb7cd7e8d555e58a1"),
                ("linux", "amd64", "e65e0611255dc5241094e4164ffad19ce9b0073ffec07c80f0ec61d26f84694f"),
                ("linux", "arm64", "32dd6973636bcf8ff55f699daa84309e45a93e114962876feb2427d3ddf15f81"))),
)


# Verified against the official v0.7.0 release metadata and checksums.txt.
# Integrity covers distribution archives, never selected executable contents.
RECOVERY_RELEASE = Release("0.7.0", "3321d341cf0e993864e2ab6a0a0534910bc8586b", 2,
    "1da44fe7f69ee5c927153425be088268bc7042a5427db0077134724106b69d26", (
        ("darwin", "amd64", "79c8206ed2a127208e6c0979aa66cce5045b9791ba2ba503b8a5535846a19d84"),
        ("darwin", "arm64", "bdff48d1e9df1ce728a49a620a2c2075c8fa46991caf4d290fe8f8a3d64a6fbe"),
        ("linux", "amd64", "37e3a1b72da7396a1d2b3e080f6f262d84811c726d1e235668130ce39a129efc"),
        ("linux", "arm64", "284d2695ff33fb53eef30eda1dc0689b22e68d290d61a555e2bd7579ddba9cfe")))


def verified_download(url: str, target, expected: str, maximum: int) -> None:
    """Use the shared bounded exact-asset transport; integrity covers archives only."""
    github_release_asset.download_asset(url, target, maximum,
                                        token=os.environ.get(github_release_asset.TOKEN_ENV))
    target.seek(0)
    digest = hashlib.sha256()
    size = 0
    while chunk := target.read(128 << 10):
        size += len(chunk)
        require(size <= maximum, "release asset exceeded its byte bound")
        digest.update(chunk)
    require(size > 0 and digest.hexdigest() == expected, "official release archive/checksums integrity mismatch")
    target.seek(0)


def extract_pair(archive, destination: Path) -> None:
    """Validate the entire distribution before extracting into a fresh owned bin."""
    with tempfile.TemporaryFile() as expanded:
        with gzip.GzipFile(fileobj=archive) as compressed:
            copy_bounded(compressed, expanded, MAX_EXPANDED)
        expanded.seek(0)
        with tarfile.open(fileobj=expanded, mode="r:") as bundle:
            members = bundle.getmembers()
            require(len(members) == 4 and {item.name for item in members} == MEMBERS,
                    "release archive member inventory changed")
            require(all(item.isfile() and not item.linkname and 0 < item.size <= MAX_EXPANDED
                        and item.mode & 0o111 and not item.mode & 0o7000 for item in members),
                    "release archive contains unsafe member metadata")
            require(all(not (destination / name).exists() and not (destination / name).is_symlink()
                        for name in ("delegate", "delegate-run")), "release installation is occupied")
            for name in ("delegate", "delegate-run"):
                source = bundle.extractfile(name)
                if source is None:
                    raise RuntimeError("release archive lacks selected executable")
                with source, (destination / name).open("xb") as target:
                    copy_bounded(source, target, MAX_EXPANDED)
                (destination / name).chmod(0o700)


@dataclass(frozen=True)
class ReleaseInstaller:
    release: Release
    evidence: dict[str, object]

    def __call__(self, destination: Path) -> None:
        asset, digest = self.release.asset(platform.system(), platform.machine())
        with tempfile.TemporaryFile() as checksums, tempfile.TemporaryFile() as archive:
            verified_download(self.release.url + "/checksums.txt", checksums, self.release.checksums_sha256, 64 << 10)
            rows = checksums.read().decode("ascii").splitlines()
            matching = [line.split() for line in rows if line.split()[-1:] == [asset]]
            require(matching == [[digest, asset]], "official checksums do not bind the selected archive")
            verified_download(self.release.url + "/" + asset, archive, digest, MAX_ARCHIVE)
            extract_pair(archive, destination)
        self.evidence.update(version=self.release.version, source_commit=self.release.commit,
                             archive=asset, archive_sha256=digest,
                             checksums_sha256=self.release.checksums_sha256,
                             installation=str(destination), verification="official-pinned-distribution")
