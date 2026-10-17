"""Assets for exact release verification."""

from __future__ import annotations

from pathlib import Path
from pathlib import PurePosixPath
import json
import os
import platform
import shutil
import stat
import tarfile
from ..directories.operations import DirectoryOwner
from .commands import (command)
from .contract import (BINARIES, CHECKSUM_RE, MAX_ARCHIVE, MAX_BINARY, MAX_OUTPUT, REVISION_RE, VerificationError, binary_version)
from .filesystem import (_bounded_children, _bounded_file_bytes, _reject_parent_components, _reject_symlink_components, bounded_digest)


def platform_identity() -> dict[str, str]:
    os_name = {"Linux": "Linux", "Darwin": "Darwin"}.get(platform.system())
    arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(
        platform.machine().lower()
    )
    if os_name is None or arch is None:
        raise VerificationError("unsupported_native_platform", "inspect-platform")
    return {"os": os_name, "kernel": platform.release(), "arch": arch}


def release_paths(args, receipt_path: Path) -> tuple[Path, Path, Path]:
    value = os.environ.get("RUNNER_TEMP")
    if not value:
        raise VerificationError("runner_temp_unavailable", "validate-paths")
    runner_value = Path(value)
    _reject_parent_components(runner_value, "unsafe_release_paths")
    _reject_symlink_components(runner_value, "unsafe_release_paths")
    _reject_parent_components(Path(args.assets_dir), "unsafe_release_paths")
    _reject_symlink_components(Path(args.assets_dir), "unsafe_release_paths")
    _reject_parent_components(Path(args.extract_dir), "unsafe_release_paths")
    _reject_symlink_components(Path(args.extract_dir), "unsafe_release_paths")
    _reject_parent_components(receipt_path, "unsafe_release_paths")
    _reject_symlink_components(receipt_path, "unsafe_release_paths")
    runner = runner_value.resolve()
    assets, extracted = runner / "delegation-layer-release", runner / "delegation-layer-release" / "extracted"
    if Path(args.assets_dir).resolve() != assets or Path(args.extract_dir).resolve() != extracted:
        raise VerificationError("unsafe_release_paths", "validate-paths")
    if assets.is_symlink() or extracted.is_symlink():
        raise VerificationError("unsafe_release_paths", "validate-paths")
    try:
        receipt_path.resolve().relative_to(assets)
    except ValueError:
        return runner, assets, extracted
    raise VerificationError("receipt_inside_cleanup_scope", "validate-paths")


def resolve_release_identity(repo: str, tag: str, receipt: dict, env: dict[str, str]) -> dict[str, str]:
    ref = f"refs/tags/{tag}"
    raw = command(receipt, "github-release-ref", [
        "gh", "api", f"repos/{repo}/git/ref/tags/{tag}",
        "--jq", ".ref + \"\\t\" + .object.type + \"\\t\" + .object.sha",
    ], env, capture=True, timeout=30)
    try:
        observed_ref, object_type, object_sha = raw.decode("ascii").strip().split("\t")
    except (UnicodeError, ValueError):
        raise VerificationError("release_ref_invalid", "resolve-release-identity") from None
    if observed_ref != ref or not REVISION_RE.fullmatch(object_sha):
        raise VerificationError("release_ref_invalid", "resolve-release-identity")
    depth = 0
    while object_type == "tag":
        depth += 1
        if depth > 3:
            raise VerificationError("release_tag_chain_too_deep", "resolve-release-identity")
        raw = command(receipt, "github-release-tag-object", [
            "gh", "api", f"repos/{repo}/git/tags/{object_sha}",
            "--jq", ".object.type + \"\\t\" + .object.sha",
        ], env, capture=True, timeout=30)
        try:
            object_type, object_sha = raw.decode("ascii").strip().split("\t")
        except (UnicodeError, ValueError):
            raise VerificationError("release_tag_object_invalid", "resolve-release-identity") from None
        if not REVISION_RE.fullmatch(object_sha):
            raise VerificationError("release_tag_object_invalid", "resolve-release-identity")
    if object_type != "commit":
        raise VerificationError("release_commit_invalid", "resolve-release-identity")
    return {"repository": repo, "tag": tag, "ref": ref, "commit_sha": object_sha}


def gh_environment() -> dict[str, str]:
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if not token:
        raise VerificationError("github_auth_unavailable", "github-auth")
    env = os.environ.copy()
    env.update({"GH_TOKEN": token, "GH_PROMPT_DISABLED": "1"})
    return env


def remember_directory(receipt: dict, token) -> None:
    receipt["cleanup"]["directory_identities"][str(token.directory.resolve())] = {
        "path": str(token.directory.resolve()),
        "parent": str(token.parent.resolve()),
        "device": token.identity[0], "inode": token.identity[1],
        "parent_device": token.parent_identity[0], "parent_inode": token.parent_identity[1],
    }


def verify_checksum(assets: Path, name: str, receipt: dict) -> Path:
    archive, checksums = assets / name, assets / "checksums.txt"
    if any(path.is_symlink() or not path.is_file() for path in (archive, checksums)):
        others = [item for item in _bounded_children(assets)
                  if item.name.startswith("delegation-layer_") and item.name.endswith(".tar.gz")]
        code = "archive_path_mismatch" if not archive.exists() and others else "release_asset_missing"
        raise VerificationError(code, "locate-assets")
    if archive.stat().st_size > MAX_ARCHIVE:
        raise VerificationError("archive_too_large", "verify-checksum")
    if checksums.stat().st_size > MAX_OUTPUT:
        raise VerificationError("checksum_file_too_large", "verify-checksum")
    try:
        raw = _bounded_file_bytes(checksums, MAX_OUTPUT, "checksum_file_too_large")
    except VerificationError:
        raise VerificationError("checksum_file_too_large", "verify-checksum") from None
    try:
        lines = raw.decode("ascii").splitlines()
    except UnicodeError:
        raise VerificationError("checksum_file_invalid", "verify-checksum") from None
    entries = {}
    for line in lines:
        match = CHECKSUM_RE.fullmatch(line)
        if match is None or match.group(2) in entries:
            raise VerificationError("checksum_file_invalid", "verify-checksum")
        entries[match.group(2)] = match.group(1).lower()
    archive_hash = bounded_digest(archive, MAX_ARCHIVE, "archive_too_large")
    if entries.get(name) != archive_hash:
        raise VerificationError("archive_checksum_mismatch", "verify-checksum")
    receipt["artifact"].update({
        "archive_name": name, "archive_path": str(archive.resolve()), "archive_sha256": archive_hash,
        "checksums_path": str(checksums.resolve()),
        "checksums_sha256": bounded_digest(checksums, MAX_OUTPUT, "checksum_file_too_large"),
    })
    return archive


def extract_archive(archive: Path, target: Path, owner: DirectoryOwner, receipt: dict) -> None:
    token = owner.create("release extraction", target, target.parent)
    remember_directory(receipt, token)
    receipt["artifact"]["extraction_path"] = str(target.resolve())
    found = set()
    try:
        with tarfile.open(archive, "r|gz") as bundle:
            for count, member in enumerate(bundle, 1):
                parts = PurePosixPath(member.name).parts
                if (count > 256 or len(member.name) > 4096 or member.size < 0
                        or member.size > MAX_BINARY or member.name.startswith("/")
                        or "\\" in member.name or ".." in parts):
                    raise VerificationError("archive_member_invalid", "extract-archive")
                if member.name in BINARIES:
                    if not member.isfile() or member.name in found:
                        raise VerificationError("archive_member_invalid", "extract-archive")
                    source = bundle.extractfile(member)
                    if source is None:
                        raise VerificationError("archive_member_invalid", "extract-archive")
                    with source, (target / member.name).open("xb") as output:
                        shutil.copyfileobj(source, output, 1024 * 1024)
                    (target / member.name).chmod(member.mode & 0o777)
                    found.add(member.name)
                elif not member.isdir() and not (member.isfile() and len(parts) == 1):
                    raise VerificationError("archive_member_invalid", "extract-archive")
    except (OSError, tarfile.TarError):
        raise VerificationError("archive_invalid", "extract-archive") from None
    if found != set(BINARIES):
        raise VerificationError("release_binary_missing", "extract-archive")


def inspect_binaries(directory: Path, receipt: dict) -> dict[str, Path]:
    paths = {}
    for name in BINARIES:
        path = directory / name
        try:
            info = path.lstat()
        except OSError:
            raise VerificationError("release_binary_missing", "inspect-binaries") from None
        resolved = path.resolve(strict=True)
        if (not stat.S_ISREG(info.st_mode) or not os.access(path, os.X_OK)
                or resolved.parent != directory.resolve() or info.st_size > MAX_BINARY):
            raise VerificationError("release_binary_invalid", "inspect-binaries")
        receipt["binaries"][name] = {
            "path": str(resolved), "sha256": bounded_digest(path, MAX_BINARY, "release_binary_invalid"),
        }
        paths[name] = resolved
    return paths


def exercise_default_path(paths: dict[str, Path], directory: Path, receipt: dict,
                          package_version: str, info: dict[str, str], home: Path) -> None:
    config = home / ".config"
    config.mkdir()
    if info["os"] == "Darwin":
        (home / "Library" / "Application Support").mkdir(parents=True)
    search_path = os.pathsep.join((str(directory), "/usr/bin", "/bin", "/usr/sbin", "/sbin"))
    env = {"PATH": search_path, "HOME": str(home), "XDG_CONFIG_HOME": str(config),
           "TMPDIR": str(home), "LANG": "C", "LC_ALL": "C"}
    for name in ("delegate", "pueue", "pueued"):
        found = shutil.which(name, path=search_path)
        if found is None or Path(found).resolve() != paths[name]:
            raise VerificationError("binary_path_mismatch", f"resolve-{name}")

    actual = binary_version(command(receipt, "delegate-version", ["delegate", "version"], env, capture=True), "delegate")
    receipt["delegate_build_identity"] = {"version": actual}
    if actual != package_version:
        raise VerificationError("delegate_version_mismatch", "verify-delegate-version")
    for name in ("pueue", "pueued"):
        receipt["binaries"][name]["version"] = binary_version(
            command(receipt, f"{name}-version", [name, "--version"], env, capture=True), name
        )
    for label, argv in (
        ("delegate-providers", ["delegate", "providers", "--json"]),
        ("delegate-capabilities", ["delegate", "capabilities", "--provider", "pi:json", "--json"]),
    ):
        command(receipt, label, argv, env)

    base = home / "Library" / "Application Support" if info["os"] == "Darwin" else config
    state = base / "delegation-layer"
    before = "present" if state.exists() else "absent"
    argv = ["delegate", "supervisor", "status", "--json"]
    try:
        status = json.loads(command(receipt, "delegate-default-supervisor-status", argv, env, capture=True))
        observed = status["supervisor_control"]["state"]
    except (UnicodeError, json.JSONDecodeError, KeyError, TypeError):
        raise VerificationError("default_path_observation_invalid", "observe-default-state") from None
    after = "present" if state.exists() else "absent"
    receipt["default_path_observation"] = {
        "path_source": "PATH", "command_name": "delegate-default-supervisor-status",
        "explicit_path_overrides": [],
        "isolated_home": str(home), "default_state_root": str(state),
        "state_before": before, "observed_state": observed, "state_after": after,
        "delegate_pueue_config_override": "unset",
    }
    if status.get("command") != "supervisor" or (before, observed, after) != ("absent", "not_initialized", "absent"):
        raise VerificationError("default_path_observation_mismatch", "observe-default-state")
