"""The install producer observes the host platform and executed installer bytes its witness records.

Fixture evidence only: fake binaries, local candidate files and the curl shim, never a live provider.
"""

from __future__ import annotations

from dataclasses import replace
from pathlib import Path
import shutil
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import test_candidate_install_support as support
from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.release_verification.assets import platform_identity
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    CHANNEL_NPM, CHANNEL_SHELL, EXPECTED_DEFAULT_STATE, INSTALLER_REPO_PATH, CandidateInstallFacts,
    CandidateInstallRequest, InstallChannel, InstallerBinding, InstallProof, ManifestArtifactBinding,
    SourceFileBinding, encode_install_proof, encode_install_witness, read_install_proof,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    ArtifactEntry, ArtifactKind, CandidateManifest, npm_package_path,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness import BoundCandidate
from scripts.delegate_acceptance.release_verification.contract import VerificationError, parse_binary_digests


SHELL = (CHANNEL_SHELL,)
NPM = (CHANNEL_NPM,)
ROOT_INSTALLER = support.REPO_ROOT / "install.sh"


def consumed_entry(entry: ArtifactEntry, files: dict[str, Path]) -> ArtifactEntry:
    path = files.get(entry.path)
    if path is None:
        return entry
    data = path.read_bytes()
    return replace(entry, size=len(data), sha256=support.sha256(data))


def expected_candidate(request: CandidateInstallRequest) -> BoundCandidate:
    """The caller's verified candidate: fixture placeholders except the files this run consumed."""
    base = fx.manifest(support.package_version())
    files = {request.files.archive.name: request.files.archive, "checksums.txt": request.files.checksums}
    if request.files.package is not None:
        files[npm_package_path(base.version)] = request.files.package
    manifest = CandidateManifest(
        version=base.version, build_tag=base.build_tag, source=base.source, build=base.build,
        artifacts=tuple(consumed_entry(entry, files) for entry in base.artifacts))
    return BoundCandidate(locator=fx.locator_for(manifest), manifest=manifest)


def expected_proof(candidate: BoundCandidate, request: CandidateInstallRequest, facts: CandidateInstallFacts,
                   channel: InstallChannel, installer: InstallerBinding | None = None) -> InstallProof:
    """Bind the expected candidate to the exact witness bytes the producer returned."""
    refs = fx.consumed(candidate, platform=facts.platform, channel=channel)
    if installer is None:
        installer = ManifestArtifactBinding(next(ref for ref in refs if ref.kind is ArtifactKind.NPM_PACKAGE))
    os_name, arch = facts.platform
    return InstallProof(
        candidate_id=candidate.candidate_id, os=os_name, arch=arch, channel=channel, consumed=refs,
        installer=installer, harness=fx.INSTALL_HARNESS,
        binaries=parse_binary_digests(request.binary_hashes(), "expected-binaries"),
        delegate_version=candidate.manifest.version, default_state=EXPECTED_DEFAULT_STATE,
        bootstrap=facts.installs[0].bootstrap, witness_sha256=support.sha256(encode_install_witness(facts)))


def read(candidate: BoundCandidate, proof: InstallProof, facts: CandidateInstallFacts,
         installer_source: bytes | None) -> InstallProof:
    return read_install_proof(
        encode_install_proof(proof), encode_install_witness(facts), manifest=candidate.manifest,
        installer_source=installer_source, repository=candidate.locator.repository)


class ObservationCase(support.CandidateCase):
    def assertRefused(self, code, stage, request, *, channels=SHELL, info=None):
        with self.assertRaises(VerificationError) as caught:
            self.verify(request, channels=channels, info=info)
        self.assertEqual((caught.exception.code, caught.exception.stage), (code, stage))

    def installer_copy(self) -> Path:
        """A fixture-owned copy of the root install.sh whose bytes differ from the original."""
        copy = self.candidate / "install.sh"
        copy.write_bytes(ROOT_INSTALLER.read_bytes() + b"\n# candidate install fixture copy\n")
        return copy


class PlatformObservationTests(ObservationCase):
    def test_facts_record_the_observed_host_platform(self):
        actual = platform_identity()
        facts = self.verify(self.request(), channels=SHELL, info={"os": actual["os"], "arch": actual["arch"]})
        self.assertEqual(facts.platform, (actual["os"], actual["arch"]))
        self.assertEqual(facts.to_record()["platform"], {"os": actual["os"], "arch": actual["arch"]})
        self.finish_workspace()

    def test_expected_platform_mismatch_is_refused_before_any_child(self):
        actual = platform_identity()
        other_os = "Darwin" if actual["os"] == "Linux" else "Linux"
        other_arch = "arm64" if actual["arch"] == "amd64" else "amd64"
        for info in ({**actual, "os": other_os}, {**actual, "arch": other_arch}, {"os": actual["os"]}):
            with self.subTest(info=info):
                self.assertRefused("candidate_platform_mismatch", "inspect-platform", self.request(), info=info)
                self.assertFalse((self.layout.root / CHANNEL_SHELL).exists())
        self.assertEqual(self.recording.calls, [])

    def test_malformed_requests_keep_their_identity_before_the_platform_check(self):
        mismatch = {**platform_identity(), "os": "Plan9"}
        self.assertRefused("candidate_channels_invalid", "validate-channels", self.request(),
                           channels=(), info=mismatch)
        self.assertRefused("repository_invalid", "validate-repository", self.request(repo="not a repo"),
                           info=mismatch)
        self.assertEqual(self.recording.calls, [])


class ShellInstallerObservationTests(ObservationCase):
    def test_witness_binds_the_executed_installer_bytes(self):
        installer = self.installer_copy()
        request = replace(self.request(), installer=installer)
        facts = self.verify(request, channels=SHELL)
        (shell,) = facts.installs
        executed = installer.read_bytes()
        self.assertEqual(shell.installer_sha256, support.sha256(executed))
        self.assertNotEqual(shell.installer_sha256, support.sha256(ROOT_INSTALLER.read_bytes()))
        self.assertEqual({role for role, _ in facts.candidate_files}, {"archive", "checksums"})
        candidate = expected_candidate(request)
        binding = SourceFileBinding(source_commit=candidate.manifest.source.commit,
                                    repo_path=INSTALLER_REPO_PATH, sha256=support.sha256(executed))
        proof = expected_proof(candidate, request, facts, InstallChannel.SHELL, binding)
        self.assertEqual(read(candidate, proof, facts, executed), proof)
        with self.assertRaises(VerificationError) as caught:
            read(candidate, proof, facts, ROOT_INSTALLER.read_bytes())
        self.assertEqual(caught.exception.code, "install_source_mismatch")
        self.finish_workspace()

    def test_installer_changed_during_install_is_refused(self):
        installer = self.installer_copy()
        request = replace(self.request(), installer=installer)

        def tamper(name):
            if name == "candidate-shell-install":
                installer.write_bytes(b"#!/bin/sh\nexit 0\n")

        self.recording.after = tamper
        self.assertRefused("candidate_file_changed", "verify-candidate", request)
        self.assertEqual([call.name for call in self.recording.calls], ["candidate-shell-install"])

    def test_missing_or_unsafe_installer_is_refused_before_any_child(self):
        link = self.candidate / "linked-install.sh"
        link.symlink_to(self.installer_copy())
        for installer, stage in ((self.candidate / "absent.sh", "bounded-read"), (link, "validate-paths")):
            with self.subTest(installer=installer.name):
                self.assertRefused("candidate_file_invalid", stage, replace(self.request(), installer=installer))
        self.assertEqual(self.recording.calls, [])


@unittest.skipUnless(shutil.which("npm") and shutil.which("node"), "npm and node are required for the npm candidate channel")
class NpmInstallerObservationTests(support.PackedCandidateCase):
    def test_witness_binds_the_whole_package_as_the_installer(self):
        request = self.request()
        facts = self.verify(request, channels=NPM)
        (npm,) = facts.installs
        package = support.sha256(self.package.read_bytes())
        self.assertEqual(npm.installer_sha256, package)
        self.assertEqual(dict(facts.candidate_files)["package"], package)
        candidate = expected_candidate(request)
        proof = expected_proof(candidate, request, facts, InstallChannel.NPM)
        self.assertEqual(proof.installer.sha256, package)
        self.assertEqual(read(candidate, proof, facts, None), proof)
        self.finish_workspace()

    def test_each_channel_records_the_installer_it_executed(self):
        facts = self.verify(self.request(), channels=(CHANNEL_SHELL, CHANNEL_NPM))
        self.assertEqual({item.channel: item.installer_sha256 for item in facts.installs}, {
            CHANNEL_SHELL: support.sha256(ROOT_INSTALLER.read_bytes()),
            CHANNEL_NPM: support.sha256(self.package.read_bytes()),
        })
        self.finish_workspace()


if __name__ == "__main__":
    unittest.main()
