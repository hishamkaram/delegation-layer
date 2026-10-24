"""Install evidence builds one canonical proof and witness pair from exactly one observed install.

Pure fixture values only: the fixture candidate, its witnesses and the fixture install.sh bytes.
"""

from __future__ import annotations

from dataclasses import replace
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    CHANNEL_RULES, EXPECTED_DEFAULT_STATE, REQUEST_DENIED, REQUEST_SERVED, CandidateInstallFacts,
    HarnessIdentity, InstallChannel, RequestRecord, encode_install_proof, encode_install_witness,
    parse_install_proof, read_install_proof,
)
from scripts.delegate_acceptance.release_verification.candidate_install_evidence import install_evidence
from scripts.delegate_acceptance.release_verification.candidate_manifest import PLATFORMS
from scripts.delegate_acceptance.release_verification.contract import VerificationError


DARWIN_ARM = ("Darwin", "arm64")
LINUX_AMD = ("Linux", "amd64")
SHELL, NPM, BREW = InstallChannel.SHELL, InstallChannel.NPM, InstallChannel.BREW
CHANNEL_SOURCE = object()  # build() supplies the installer bytes the observed channel requires.


def default_source(channel: InstallChannel) -> bytes | None:
    """The shell channel runs the source commit's install.sh; the others run a candidate artifact."""
    return fx.INSTALL_SH if CHANNEL_RULES[channel].installer_kind is None else None


class EvidenceCase(unittest.TestCase):
    def setUp(self):
        self.candidate = fx.bound()

    def facts(self, channel: InstallChannel, platform: tuple[str, str] = DARWIN_ARM) -> CandidateInstallFacts:
        return fx.witness(self.candidate, platform=platform, channel=channel)

    def build(self, facts, *, source: object = CHANNEL_SOURCE, manifest=None, locator=None,
              harness=fx.INSTALL_HARNESS):
        if source is CHANNEL_SOURCE:
            source = default_source(InstallChannel(facts.installs[0].channel))
        return install_evidence(
            facts, self.candidate.manifest if manifest is None else manifest,
            self.candidate.locator if locator is None else locator, installer_source=source, harness=harness)

    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)

    def with_observation(self, facts: CandidateInstallFacts, **changes) -> CandidateInstallFacts:
        return replace(facts, installs=(replace(facts.installs[0], **changes),))


class SuccessTests(EvidenceCase):
    def test_every_channel_on_every_manifest_platform_reads_back_canonically(self):
        oses = set()
        for platform in PLATFORMS:
            for channel in InstallChannel:
                with self.subTest(platform=platform, channel=channel):
                    facts = self.facts(channel, platform)
                    value, proof, witness = self.build(facts)
                    expected = fx.install(self.candidate, platform=platform, channel=channel)
                    self.assertEqual(value, expected)
                    self.assertEqual(proof, encode_install_proof(expected))
                    self.assertEqual(witness, encode_install_witness(facts))
                    self.assertEqual(value.witness_sha256, sha(witness))
                    self.assertEqual(parse_install_proof(proof), value)
                    self.assertEqual(read_install_proof(
                        proof, witness, manifest=self.candidate.manifest,
                        installer_source=default_source(channel), repository=fx.REPOSITORY), value)
                    oses.add(platform[0])
        self.assertEqual(oses, {"Darwin", "Linux"})

    def test_harness_is_recorded_as_given_not_authenticated(self):
        """A well-formed substituted harness passes the codec; harness provenance is the producer's check."""
        substituted = HarnessIdentity(commit="8" * 40, sha256="8" * 64)
        value, proof, _ = self.build(self.facts(SHELL), harness=substituted)
        self.assertEqual(value.harness, substituted)
        self.assertEqual(parse_install_proof(proof).harness, substituted)
        self.assertCode("install_proof_invalid", self.build, self.facts(SHELL),
                        harness=(substituted.commit, substituted.sha256))


class ObservationTests(EvidenceCase):
    def test_exactly_one_observation(self):
        shell, npm = self.facts(SHELL), self.facts(NPM)
        for installs in ((), (shell.installs[0], npm.installs[0]), (shell.installs[0], shell.installs[0]),
                         [shell.installs[0]], ({"channel": "shell"},)):
            with self.subTest(installs=installs):
                self.assertCode("install_witness_mismatch", self.build, replace(shell, installs=installs),
                                source=fx.INSTALL_SH)

    def test_platform_is_present_and_a_manifest_platform(self):
        facts = self.facts(SHELL)
        for platform in (None, ("Windows", "amd64"), list(DARWIN_ARM), ("Darwin",)):
            with self.subTest(platform=platform):
                self.assertCode("install_witness_mismatch", self.build, replace(facts, platform=platform))

    def test_platform_must_be_the_one_whose_archive_was_consumed(self):
        for channel in InstallChannel:
            with self.subTest(channel=channel):
                moved = replace(self.facts(channel, DARWIN_ARM), platform=LINUX_AMD)
                self.assertCode("install_witness_mismatch", self.build, moved)

    def test_channel_is_derived_from_the_sole_observation(self):
        for channel in ("pip", "Shell", None, ["shell"]):
            with self.subTest(channel=channel):
                self.assertCode("install_witness_mismatch", self.build,
                                self.with_observation(self.facts(SHELL), channel=channel), source=fx.INSTALL_SH)
        for facts, label in ((self.facts(SHELL), NPM), (self.facts(NPM), SHELL), (self.facts(BREW), NPM),
                             (self.facts(NPM), BREW)):
            with self.subTest(label=label):
                self.assertCode("install_witness_mismatch", self.build,
                                self.with_observation(facts, channel=label.value))

    def test_npm_run_relabelled_shell_cannot_claim_the_source_installer(self):
        facts = self.facts(NPM)
        relabelled = replace(
            self.with_observation(facts, channel=SHELL.value),
            candidate_files=tuple(pair for pair in facts.candidate_files if pair[0] != "package"))
        self.assertCode("install_source_mismatch", self.build, relabelled)


class CandidateFileTests(EvidenceCase):
    def test_roles_are_exactly_the_channel_roles(self):
        shell, npm, brew = (dict(self.facts(channel).candidate_files) for channel in (SHELL, NPM, BREW))
        cases = {
            "shell-extra-package": (SHELL, {**shell, "package": npm["package"]}),
            "shell-extra-formula": (SHELL, {**shell, "formula": brew["formula"]}),
            "shell-missing-checksums": (SHELL, {"archive": shell["archive"]}),
            "npm-missing-package": (NPM, shell),
            "brew-extra-checksums": (BREW, {**brew, "checksums": shell["checksums"]}),
            "brew-renamed-formula": (BREW, {"archive": brew["archive"], "package": brew["formula"]}),
            "unknown-role": (SHELL, {**shell, "installer": shell["archive"]}),
            "empty": (SHELL, {}),
        }
        for name, (channel, files) in cases.items():
            with self.subTest(name=name):
                facts = replace(self.facts(channel), candidate_files=tuple(sorted(files.items())))
                self.assertCode("install_witness_mismatch", self.build, facts)

    def test_duplicate_role_refuses_before_dictionary_conversion(self):
        for channel in InstallChannel:
            facts = self.facts(channel)
            archive = next(pair for pair in facts.candidate_files if pair[0] == "archive")
            for duplicate in (archive, ("archive", "0" * 64)):
                with self.subTest(channel=channel, duplicate=duplicate):
                    doubled = replace(facts, candidate_files=(*facts.candidate_files, duplicate))
                    if duplicate == archive:
                        # The control: dict conversion alone would hide this repetition entirely.
                        self.assertEqual(encode_install_witness(doubled), encode_install_witness(facts))
                    self.assertCode("install_witness_mismatch", self.build, doubled)

    def test_candidate_files_shape(self):
        facts = self.facts(SHELL)
        for files in (list(facts.candidate_files), (("archive",),), ("archive",), ((["archive"], "0" * 64),), None):
            with self.subTest(files=files):
                self.assertCode("install_witness_mismatch", self.build, replace(facts, candidate_files=files))

    def test_file_digests_must_be_the_manifest_artifacts(self):
        for channel in InstallChannel:
            facts = self.facts(channel)
            for role, _ in facts.candidate_files:
                with self.subTest(channel=channel, role=role):
                    changed = tuple((name, "0" * 64 if name == role else digest)
                                    for name, digest in facts.candidate_files)
                    self.assertCode("install_witness_mismatch", self.build, replace(facts, candidate_files=changed))


class ObservedValueTests(EvidenceCase):
    def test_tag_version_and_default_state(self):
        for channel in InstallChannel:
            facts = self.facts(channel)
            with self.subTest(channel=channel):
                self.assertCode("install_witness_mismatch", self.build, replace(facts, tag="v0.1.38"))
                self.assertCode("install_proof_version_mismatch", self.build,
                                self.with_observation(facts, delegate_version="0.1.38"))
                for state in (("absent", "initialized", "absent"), list(EXPECTED_DEFAULT_STATE)):
                    self.assertCode("install_proof_invalid", self.build,
                                    self.with_observation(facts, default_state=state))

    def test_binaries_are_exact(self):
        facts = self.facts(SHELL)
        binaries = facts.installs[0].binaries
        self.assertCode("binary_digests_invalid", self.build, self.with_observation(facts, binaries=binaries[1:]))
        self.assertCode("binary_digests_invalid", self.build, self.with_observation(
            facts, binaries=(*binaries, ("extra", "0" * 64))))
        self.assertCode("binary_digests_invalid", self.build, self.with_observation(
            facts, binaries=((binaries[0][0], "A" * 64), *binaries[1:])))
        self.assertCode("install_witness_mismatch", self.build, self.with_observation(
            facts, binaries=((["delegate"], "0" * 64), *binaries[1:])))
        doubled = self.with_observation(facts, binaries=(*binaries, binaries[0]))
        self.assertEqual(encode_install_witness(doubled), encode_install_witness(facts))
        self.assertCode("install_witness_mismatch", self.build, doubled)

    def test_release_requests_are_exactly_the_served_archive_then_checksums(self):
        for channel in (SHELL, NPM):
            facts = self.facts(channel)
            archive, checksums = facts.installs[0].requests
            for requests in (
                    (), (archive,), (checksums, archive), (archive, checksums, archive),
                    (RequestRecord(REQUEST_DENIED, archive.url), checksums),
                    (archive, checksums, RequestRecord(REQUEST_DENIED, "https://example.invalid/x")),
                    tuple(RequestRecord(REQUEST_SERVED, item.url.replace("/v0.1.37/", "/v0.1.38/"))
                          for item in (archive, checksums)),
                    tuple(RequestRecord(REQUEST_SERVED, item.url.replace(fx.REPOSITORY, "other/project"))
                          for item in (archive, checksums))):
                with self.subTest(channel=channel, requests=requests):
                    self.assertCode("install_witness_mismatch", self.build,
                                    self.with_observation(facts, requests=requests))

    def test_brew_requests_nothing(self):
        shell = self.facts(SHELL).installs[0]
        self.assertCode("install_witness_mismatch", self.build,
                        self.with_observation(self.facts(BREW), requests=shell.requests[:1]))


class InstallerSourceTests(EvidenceCase):
    def test_shell_requires_the_exact_source_commit_bytes(self):
        facts = self.facts(SHELL)
        for source in (None, fx.INSTALL_SH + b"#", b"", fx.INSTALL_SH.decode()):
            with self.subTest(source=source):
                self.assertCode("install_source_mismatch", self.build, facts, source=source)
        other = b"#!/bin/sh\necho other installer\n"
        self.assertCode("install_source_mismatch", self.build,
                        self.with_observation(facts, installer_sha256=sha(other)))
        self.assertCode("candidate_field_invalid", self.build, self.with_observation(facts, installer_sha256=None))

    def test_artifact_channels_refuse_installer_bytes(self):
        for channel in (NPM, BREW):
            with self.subTest(channel=channel):
                self.assertCode("install_source_unexpected", self.build, self.facts(channel), source=fx.INSTALL_SH)

    def test_artifact_channels_bind_the_executed_artifact(self):
        for channel in (NPM, BREW):
            with self.subTest(channel=channel):
                self.assertCode("install_witness_mismatch", self.build,
                                self.with_observation(self.facts(channel), installer_sha256=sha(fx.INSTALL_SH)))


class CandidateBindingTests(EvidenceCase):
    def test_locator_must_bind_the_manifest(self):
        other = fx.manifest("0.1.38")
        facts = self.facts(SHELL)
        self.assertCode("candidate_manifest_digest_mismatch", self.build, facts, manifest=other)
        self.assertCode("candidate_manifest_digest_mismatch", self.build, facts, locator=fx.locator_for(other))
        self.assertCode("install_proof_candidate_mismatch", self.build, facts, manifest=self.candidate)
        self.assertCode("install_proof_candidate_mismatch", self.build, facts, locator=self.candidate)

    def test_locator_repository_is_the_expected_release_repository(self):
        for channel in (SHELL, NPM):
            with self.subTest(channel=channel):
                moved = replace(self.candidate.locator, repository="other-owner/delegation-layer")
                self.assertCode("install_witness_mismatch", self.build, self.facts(channel), locator=moved)


class ReadbackTests(EvidenceCase):
    def read(self, proof: bytes, witness: bytes, channel: InstallChannel):
        return read_install_proof(proof, witness, manifest=self.candidate.manifest,
                                  installer_source=default_source(channel), repository=fx.REPOSITORY)

    def test_altered_witness_or_proof_bytes_refuse(self):
        for channel in InstallChannel:
            _, proof, witness = self.build(self.facts(channel))
            digest = fx.NATIVE_BINARIES.delegate.encode()
            with self.subTest(channel=channel):
                self.assertCode("install_witness_digest_mismatch", self.read,
                                proof, witness.replace(digest, b"0" * 64), channel)
                self.assertCode("install_witness_mismatch", self.read,
                                proof.replace(digest, b"0" * 64), witness, channel)
                self.assertCode("candidate_record_not_canonical", self.read, proof + b"\n", witness, channel)


if __name__ == "__main__":
    unittest.main()
