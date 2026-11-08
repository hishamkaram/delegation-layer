"""npm registry consumer verification: package identity, CLI and skill outside the checkout."""

from __future__ import annotations

from dataclasses import dataclass
from scripts.release_verification.assets import exercise_default_path
from scripts.release_verification.assets import inspect_binaries
from scripts.release_verification.contract import installed_executables
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.published_contract import ArchiveFacts
from scripts.release_verification.published_contract import InstallFacts
from scripts.release_verification.published_contract import NPM_PACKAGE
from scripts.release_verification.published_contract import NpmFacts
from scripts.release_verification.published_contract import PackageIdentity
from scripts.release_verification.published_contract import SkillManifest
from scripts.release_verification.published_contract import check_package_bytes
from scripts.release_verification.published_contract import compare_hashes
from scripts.release_verification.published_contract import decide_latest
from scripts.release_verification.published_contract import npm_environment
from scripts.release_verification.published_contract import parse_npm_latest
from scripts.release_verification.published_contract import parse_npm_view
from scripts.release_verification.published_contract import parse_pack_filename
from scripts.release_verification.published_contract import record_fact
from scripts.release_verification.published_files import installed_skill_digests
from scripts.release_verification.published_files import read_package
from scripts.release_verification.published_files import read_package_bytes
from scripts.release_verification.published_files import require_exact_binaries
from scripts.release_verification.published_ports import ConsumerLayout
from scripts.release_verification.published_ports import ConsumerPorts
from scripts.release_verification.published_ports import tool_path
from scripts.release_verification.published_source import fetch_skill_manifest
from scripts.release_verification.published_source import github_latest_tag


_VIEW_FIELDS = ("name", "version", "dist.integrity", "dist.shasum", "dist.tarball", "gitHead")


@dataclass(frozen=True)
class _NpmSession:
    """The one private npm environment every registry child of this run shares."""

    ports: ConsumerPorts
    receipt: dict
    npm: str
    env: dict[str, str]
    layout: ConsumerLayout

    def run(self, name: str, args: list[str], *, capture: bool = False, timeout: int = 120) -> bytes:
        return self.ports.run(
            self.receipt, name, [self.npm, *args], self.env,
            capture=capture, timeout=timeout, cwd=self.layout.cwd,
        )

    def exec_package(self, name: str, spec: str, args: list[str], *, timeout: int) -> None:
        self.run(name, ["exec", "--yes", f"--package={NPM_PACKAGE}@{spec}", "--", NPM_PACKAGE, *args],
                 timeout=timeout)


def _open_session(ports: ConsumerPorts, receipt: dict, layout: ConsumerLayout) -> _NpmSession:
    npm = ports.which("npm")
    if npm is None:
        raise VerificationError("npm_unavailable", "locate-tools")
    env = npm_environment(
        path=tool_path(ports, ("npm", "node")), home=str(layout.home), cache=str(layout.cache),
        user_npmrc=str(layout.user_npmrc), global_npmrc=str(layout.global_npmrc),
        inherited=ports.inherited,
    )
    return _NpmSession(ports, receipt, npm, env, layout)


def _registry_identity(session: _NpmSession, archive: ArchiveFacts) -> tuple[PackageIdentity, dict[str, str]]:
    spec = f"{NPM_PACKAGE}@{archive.package_version}"
    identity = parse_npm_view(
        session.run("npm-view", ["view", spec, *_VIEW_FIELDS, "--json"], capture=True, timeout=60),
        archive.package_version, archive.release_commit,
    )
    record_fact(session.receipt, "npm_package", {
        "name": identity.name, "version": identity.version,
        "integrity": identity.integrity, "tarball": identity.tarball,
    })
    filename = parse_pack_filename(session.run(
        "npm-pack", ["pack", spec, "--json", "--pack-destination", str(session.layout.pack)],
        capture=True, timeout=180,
    ), identity)
    data = read_package_bytes(session.layout.pack / filename)
    check_package_bytes(data, identity)
    contents = read_package(data)
    if (contents.manifest.get("name") != identity.name
            or contents.manifest.get("version") != identity.version):
        raise VerificationError("npm_pack_manifest_mismatch", "npm-pack")
    record_fact(session.receipt, "npm_pack_integrity", "verified-against-registry-metadata")
    return identity, contents.skill_digests


def _install(
        session: _NpmSession, *, label: str, spec: str, tag: str | None,
        archive: ArchiveFacts, manifest: SkillManifest, info: dict[str, str]) -> InstallFacts:
    base = session.layout.root / label
    base.mkdir(mode=0o700)
    bin_dir, skill_dir = base / "bin", base / "skill"
    version_args = [] if tag is None else ["--version", tag]
    install_args = ["install-cli", *version_args, "--install-dir", str(bin_dir)]
    session.exec_package(f"npm-exec-{label}-install-cli", spec, install_args, timeout=600)
    session.exec_package(f"npm-exec-{label}-skill", spec, ["--target", str(skill_dir)], timeout=180)
    require_exact_binaries(bin_dir)
    explicit = label == "explicit"
    ledger = session.receipt if explicit else {"binaries": {}}
    paths = inspect_binaries(bin_dir, ledger)
    installed = installed_executables(paths)
    skill = installed_skill_digests(skill_dir)
    compare_hashes(manifest.digests(), skill, "installed_skill_mismatch", "verify-skill")
    if explicit:
        home = base / "exercise-home"
        home.mkdir(mode=0o700)
        exercise_default_path(paths, bin_dir, session.receipt, archive.package_version, info, home,
                              cwd=session.layout.cwd)
    return InstallFacts(label, spec, installed, len(skill))


def verify_npm_consumer(
        *, ports: ConsumerPorts, receipt: dict, archive: ArchiveFacts, layout: ConsumerLayout,
        info: dict[str, str], repo: str, github_env: dict[str, str]) -> NpmFacts:
    """Verify the registry package, its CLI install and its skill against the exact release.

    Registry-side children get an allowlisted, token-free environment, private
    cache/home/empty npmrc, explicit registry and a cwd outside any checkout.
    """
    session = _open_session(ports, receipt, layout)
    identity, package_skill = _registry_identity(session, archive)
    manifest = fetch_skill_manifest(ports, receipt, repo, archive.release_commit, github_env)
    compare_hashes(manifest.digests(), package_skill, "npm_pack_skill_mismatch", "verify-package-skill")
    record_fact(receipt, "skill_source", {
        "commit": manifest.commit,
        "files": [{"path": item.path, "sha256": item.sha256} for item in manifest.files],
    })
    decision = decide_latest(
        parse_npm_latest(session.run(
            "npm-view-latest", ["view", NPM_PACKAGE, "dist-tags.latest", "--json"],
            capture=True, timeout=60)),
        github_latest_tag(ports, receipt, repo, github_env), archive.tag,
    )
    record_fact(receipt, "latest", {
        "npm": decision.npm_latest, "github": decision.github_latest,
        "default_pass": "run" if decision.run_default else "skipped", "reason": decision.reason,
    })
    common = {"archive": archive, "manifest": manifest, "info": info}
    installs = [_install(session, label="explicit", spec=archive.package_version, tag=archive.tag, **common)]
    record_fact(receipt, "explicit_install", "verified")
    if decision.run_default:
        installs.append(_install(session, label="default", spec="latest", tag=None, **common))
        record_fact(receipt, "default_install", "verified")
    return NpmFacts(identity, manifest.commit, "run" if decision.run_default else "skipped", tuple(installs))
