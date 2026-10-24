"""Install proofs bind each channel's installer and witness to the exact candidate."""

from __future__ import annotations

from dataclasses import replace
import json
import unittest

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.native_evidence import contract as native_contract
from scripts.delegate_acceptance.native_evidence.contract import canonical_json
from scripts.delegate_acceptance.release_verification import candidate_attempts as attempts
from scripts.delegate_acceptance.release_verification.candidate_history import FindingRef
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    CHANNEL_NPM, CHANNEL_SHELL, EXPECTED_DEFAULT_STATE, REQUEST_DENIED, REQUEST_SERVED, ArtifactRef,
    CandidateInstallFacts, InstallChannel, InstallObservation, ManifestArtifactBinding, RequestRecord,
    SourceFileBinding, encode_install_proof, encode_install_witness, parse_install_proof,
    read_install_proof,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import PLATFORMS, ArtifactKind
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    AttemptState, decide_platform_claim, evaluate_attempt, evaluate_readiness,
)
from scripts.delegate_acceptance.release_verification import contract as release_contract
from scripts.delegate_acceptance.release_verification.contract import (
    BinaryDigests, VerificationError, parse_binary_digests,
)


DARWIN_ARM = ("Darwin", "arm64")
LINUX_AMD = ("Linux", "amd64")
SHELL, NPM, BREW = InstallChannel.SHELL, InstallChannel.NPM, InstallChannel.BREW


def ref(candidate, kind, platform=DARWIN_ARM) -> ArtifactRef:
    entry = (candidate.manifest.archive(*platform) if kind is ArtifactKind.ARCHIVE
             else next(item for item in candidate.manifest.artifacts if item.kind is kind))
    return ArtifactRef(kind=entry.kind, path=entry.path, sha256=entry.sha256)


def record(data: bytes) -> dict:
    return json.loads(data)


class InstallCase(unittest.TestCase):
    def setUp(self):
        self.candidate = fx.bound()

    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)

    def evidence(self, channel, platform=DARWIN_ARM):
        proof = fx.install(self.candidate, platform=platform, channel=channel)
        facts = fx.witness(self.candidate, platform=platform, channel=channel)
        return proof, encode_install_witness(facts)

    def read(self, proof, witness, *, source=fx.INSTALL_SH, manifest=None, repository=fx.REPOSITORY):
        installer_source = source if isinstance(proof.installer, SourceFileBinding) else None
        return read_install_proof(encode_install_proof(proof), witness,
                                  manifest=manifest or self.candidate.manifest,
                                  installer_source=installer_source, repository=repository)

    def rebound(self, proof, facts):
        """Return the proof re-bound to a different witness, and that witness's bytes."""
        witness = encode_install_witness(facts)
        return replace(proof, witness_sha256=sha(witness)), witness


class ReaderTests(InstallCase):
    def test_every_channel_reads_on_every_fixture_platform(self):
        expected = {
            SHELL: {ArtifactKind.ARCHIVE, ArtifactKind.CHECKSUMS},
            NPM: {ArtifactKind.ARCHIVE, ArtifactKind.CHECKSUMS, ArtifactKind.NPM_PACKAGE},
            BREW: {ArtifactKind.ARCHIVE, ArtifactKind.HOMEBREW_FORMULA},
        }
        archives = set()
        for platform in PLATFORMS:
            for channel in InstallChannel:
                with self.subTest(platform=platform, channel=channel):
                    proof, witness = self.evidence(channel, platform)
                    self.assertEqual(self.read(proof, witness), proof)
                    self.assertEqual(parse_install_proof(encode_install_proof(proof)), proof)
                    self.assertEqual({item.kind for item in proof.consumed}, expected[channel])
                    archives.add(proof.consumed_ref(ArtifactKind.ARCHIVE).path)
                    requests = record(witness)["installs"][0]["requests"]
                    self.assertEqual(len(requests), 0 if channel is BREW else 2)
        self.assertEqual(archives, {self.candidate.manifest.archive(*platform).path for platform in PLATFORMS})
        self.assertEqual((CHANNEL_SHELL, CHANNEL_NPM), (SHELL.value, NPM.value))

    def test_shell_binds_exact_source_commit_install_sh(self):
        proof, witness = self.evidence(SHELL)
        self.assertCode("install_source_mismatch", self.read, proof, witness, source=fx.INSTALL_SH + b"#")
        self.assertCode("install_source_mismatch", self.read, proof, witness, source=None)
        moved = replace(proof, installer=replace(proof.installer, source_commit="f" * 40))
        self.assertCode("install_source_commit_mismatch", self.read, moved, witness)
        self.assertCode("install_proof_invalid", replace, proof,
                        installer=replace(proof.installer, repo_path="scripts/install.sh"))

    def test_npm_binds_the_executed_package_not_root_install_sh(self):
        proof, witness = self.evidence(NPM)
        self.assertEqual(proof.installer, ManifestArtifactBinding(ref(self.candidate, ArtifactKind.NPM_PACKAGE)))
        shell_binding = fx.installer(proof.consumed, SHELL)
        self.assertCode("install_proof_invalid", replace, proof, installer=shell_binding)
        raw = record(encode_install_proof(proof))
        raw["installer"] = {"source_file": {"source_commit": fx.COMMIT, "repo_path": "install.sh",
                                            "sha256": sha(fx.INSTALL_SH)}}
        self.assertCode("install_proof_invalid", parse_install_proof, canonical_json(raw))
        facts = fx.witness(self.candidate, platform=DARWIN_ARM, channel=NPM)
        confused = replace(facts, installs=(replace(facts.installs[0], installer_sha256=sha(fx.INSTALL_SH)),))
        self.assertCode("install_witness_mismatch", self.read, *self.rebound(proof, confused))
        self.assertCode("install_source_unexpected", read_install_proof, encode_install_proof(proof),
                        witness, manifest=self.candidate.manifest, installer_source=fx.INSTALL_SH,
                        repository=fx.REPOSITORY)

    def test_brew_binds_formula_role_and_requests_nothing(self):
        proof, witness = self.evidence(BREW)
        package = ManifestArtifactBinding(ref(self.candidate, ArtifactKind.NPM_PACKAGE))
        self.assertCode("install_proof_invalid", replace, proof, installer=package)
        facts = fx.witness(self.candidate, platform=DARWIN_ARM, channel=BREW)
        renamed = replace(facts, candidate_files=tuple(
            ("package" if role == "formula" else role, digest) for role, digest in facts.candidate_files))
        self.assertCode("install_witness_mismatch", self.read, *self.rebound(proof, renamed))
        checksums = ref(self.candidate, ArtifactKind.CHECKSUMS)
        extra = replace(facts, candidate_files=tuple(sorted((*facts.candidate_files, ("checksums", checksums.sha256)))))
        self.assertCode("install_witness_mismatch", self.read, *self.rebound(proof, extra))
        archive = self.candidate.manifest.archive(*DARWIN_ARM)
        fetched = replace(facts, installs=(replace(facts.installs[0], requests=(
            RequestRecord(REQUEST_SERVED, f"https://github.com/{fx.REPOSITORY}/releases/download/"
                                          f"{self.candidate.manifest.build_tag}/{archive.path}"),)),))
        self.assertCode("install_witness_mismatch", self.read, *self.rebound(proof, fetched))

    def test_consumed_refs_are_exactly_the_channel_set(self):
        proof, _ = self.evidence(SHELL)
        package = ref(self.candidate, ArtifactKind.NPM_PACKAGE)
        archive = proof.consumed_ref(ArtifactKind.ARCHIVE)
        for consumed in (
                tuple(sorted((*proof.consumed, package), key=lambda item: item.path)),
                (archive,),
                (archive, archive),
                tuple(reversed(proof.consumed)),
                (archive, replace(archive, kind=ArtifactKind.CHECKSUMS)),
                tuple(sorted((archive, package), key=lambda item: item.path))):
            with self.subTest(consumed=consumed):
                self.assertCode("install_proof_invalid", replace, proof, consumed=consumed)
        self.assertCode("install_artifact_ref_invalid", replace, archive, path="../archive.tar.gz")

    def test_refs_resolve_to_manifest_entries_of_the_native_platform(self):
        proof, witness = self.evidence(SHELL)
        checksums = proof.consumed_ref(ArtifactKind.CHECKSUMS)
        archive = proof.consumed_ref(ArtifactKind.ARCHIVE)

        def with_ref(old, new):
            return replace(proof, consumed=tuple(sorted(
                (new if item == old else item for item in proof.consumed), key=lambda item: item.path)))

        self.assertCode("install_artifact_unknown", self.read, with_ref(checksums, replace(checksums, sha256="0" * 64)),
                        witness)
        self.assertCode("install_artifact_unknown", self.read,
                        with_ref(archive, replace(archive, path="other.tar.gz")), witness)
        foreign = ref(self.candidate, ArtifactKind.ARCHIVE, LINUX_AMD)
        self.assertCode("install_archive_mismatch", self.read, with_ref(archive, foreign), witness)
        other = fx.manifest("0.1.38")
        self.assertCode("install_proof_candidate_mismatch", self.read, proof, witness, manifest=other)

    def test_version_default_state_binaries_and_harness(self):
        proof, witness = self.evidence(BREW)
        self.assertCode("install_proof_version_mismatch", self.read, replace(proof, delegate_version="0.1.38"),
                        witness)
        self.assertCode("install_proof_invalid", replace, proof, default_state=("absent", "initialized", "absent"))
        self.assertCode("install_proof_invalid", replace, proof, default_state=list(EXPECTED_DEFAULT_STATE))
        other = BinaryDigests(*("8" * 64 for _ in range(4)))
        self.assertCode("install_witness_mismatch", self.read, replace(proof, binaries=other), witness)
        self.assertCode("binary_digests_invalid", replace, fx.NATIVE_BINARIES, pueue="A" * 64)
        raw = record(encode_install_proof(proof))
        del raw["binaries"]["pueued"]
        self.assertCode("binary_digests_invalid", parse_install_proof, canonical_json(raw))
        raw = record(encode_install_proof(proof))
        raw["harness"]["commit"] = "not-a-revision"
        self.assertCode("candidate_field_invalid", parse_install_proof, canonical_json(raw))
        raw = record(encode_install_proof(proof))
        raw["default_state"] = ["absent", "not_initialized"]
        self.assertCode("install_proof_invalid", parse_install_proof, canonical_json(raw))

    def test_reader_requires_the_exact_witness_bytes_and_facts(self):
        proof, witness = self.evidence(SHELL)
        self.assertCode("install_witness_digest_mismatch", self.read, proof, witness.replace(b"shell", b"Shell"))
        facts = fx.witness(self.candidate, platform=DARWIN_ARM, channel=SHELL)
        npm = fx.witness(self.candidate, platform=DARWIN_ARM, channel=NPM).installs[0]
        observation = facts.installs[0]
        archive_url, checksums_url = (item.url for item in observation.requests)
        variants = {
            "platform": replace(facts, platform=LINUX_AMD),
            "legacy-platform": replace(facts, platform=None),
            "legacy-installer": replace(facts, installs=(replace(observation, installer_sha256=None),)),
            "tag": replace(facts, tag="v0.1.38"),
            "two-channels": replace(facts, installs=(observation, npm)),
            "version": replace(facts, installs=(replace(observation, delegate_version="0.1.38"),)),
            "state": replace(facts, installs=(replace(observation, default_state=("absent", "absent", "absent")),)),
            "channel": replace(facts, installs=(replace(observation, channel=CHANNEL_NPM),)),
        }
        for requests in (
                (RequestRecord(REQUEST_SERVED, checksums_url), RequestRecord(REQUEST_SERVED, archive_url)),
                (*observation.requests, RequestRecord(REQUEST_SERVED, archive_url)),
                (RequestRecord(REQUEST_DENIED, archive_url), RequestRecord(REQUEST_SERVED, checksums_url)),
                tuple(RequestRecord(REQUEST_SERVED, item.url.replace("/v0.1.37/", "/v0.1.38/"))
                      for item in observation.requests),
                tuple(RequestRecord(REQUEST_SERVED, item.url.replace(fx.REPOSITORY, "a/b/c"))
                      for item in observation.requests),
                ()):
            variants[f"requests-{len(variants)}"] = replace(facts, installs=(replace(observation, requests=requests),))
        for name, variant in variants.items():
            with self.subTest(name=name):
                self.assertCode("install_witness_mismatch", self.read, *self.rebound(proof, variant))
        legacy = record(witness)
        del legacy["platform"]
        self.assertCode("candidate_record_fields_invalid", self.read, *self.rebound_raw(proof, canonical_json(legacy)))
        loose = json.dumps(record(witness)).encode()
        self.assertCode("candidate_record_not_canonical", self.read, *self.rebound_raw(proof, loose))

    def rebound_raw(self, proof, witness):
        return replace(proof, witness_sha256=sha(witness)), witness

    def test_expected_repository_comes_from_the_caller_not_the_witness(self):
        for channel in (SHELL, NPM):
            proof, _ = self.evidence(channel)
            facts = fx.witness(self.candidate, platform=DARWIN_ARM, channel=channel)
            observation = facts.installs[0]
            for other in ("other-owner/delegation-layer", "hishamkaram/other-project"):
                with self.subTest(channel=channel, repository=other):
                    requests = tuple(RequestRecord(item.outcome, item.url.replace(fx.REPOSITORY, other))
                                     for item in observation.requests)
                    proof_bytes, witness = self.rebound(
                        proof, replace(facts, installs=(replace(observation, requests=requests),)))
                    self.assertCode("install_witness_mismatch", self.read, proof_bytes, witness)
                    self.assertEqual(self.read(proof_bytes, witness, repository=other), proof_bytes)
        for channel in InstallChannel:
            proof, witness = self.evidence(channel)
            for repository in ("delegation-layer", "a/b/c", "owner/name\n", "", None):
                with self.subTest(channel=channel, repository=repository):
                    self.assertCode("repository_invalid", self.read, proof, witness, repository=repository)

    def test_proof_codec_is_strict(self):
        proof, _ = self.evidence(NPM)
        data = encode_install_proof(proof)
        raw = record(data)
        self.assertCode("candidate_record_invalid", parse_install_proof,
                        data[:-1] + b',"channel":"npm"}')
        for mutate, code in (
                (lambda value: value.update(extra=1), "candidate_record_fields_invalid"),
                (lambda value: value.update(channel="pip"), "install_proof_invalid"),
                (lambda value: value.update(schema_version=True), "install_proof_kind_invalid"),
                (lambda value: value.update(os="Windows"), "install_proof_invalid"),
                (lambda value: value["consumed"][0].update(kind="tarball"), "install_artifact_ref_invalid"),
                (lambda value: value.update(installer={"manifest_artifact": value["consumed"][0],
                                                       "source_file": {}}), "install_binding_invalid")):
            changed = json.loads(json.dumps(raw))
            mutate(changed)
            with self.subTest(code=code, changed=sorted(changed)):
                self.assertCode(code, parse_install_proof, canonical_json(changed))
        self.assertCode("candidate_record_not_canonical", parse_install_proof, json.dumps(raw).encode())


class HistoryTests(InstallCase):
    def test_history_installs_are_unique_per_platform_channel_and_candidate(self):
        intent = fx.intent(self.candidate)
        shell = fx.install(self.candidate, platform=DARWIN_ARM, channel=SHELL)
        foreign = replace(shell, candidate_id="9" * 64)
        for installs in ((shell, shell), (foreign,), (encode_install_proof(shell),)):
            with self.subTest(installs=len(installs)):
                self.assertCode("candidate_history_invalid", fx.history, self.candidate, intents=(intent,),
                                installs=installs)
        history = fx.history(self.candidate, intents=(intent,), installs=(shell,))
        self.assertIs(history.install_for(DARWIN_ARM, SHELL), shell)
        self.assertIsNone(history.install_for(DARWIN_ARM, NPM))


class ReadinessTests(InstallCase):
    def readiness(self, history):
        return evaluate_readiness(candidate=self.candidate, observed=fx.observed(), scope=fx.scope(),
                                  history=history, review=fx.review(self.candidate))

    def planned(self, platforms, *, installs=None, binaries=fx.NATIVE_BINARIES):
        intent = fx.intent(self.candidate, platforms=platforms)
        proofs = tuple(fx.proof(self.candidate, attempt=1, platform=platform, binaries=binaries)
                       for platform in platforms)
        return fx.history(self.candidate, intents=(intent,), proof_records=proofs, installs=installs)

    def test_two_planned_platforms_need_only_their_installs(self):
        platforms = (DARWIN_ARM, LINUX_AMD)
        history = self.planned(platforms, installs=fx.installs_for(self.candidate, platforms))
        result = self.readiness(history)
        self.assertTrue(result.ready, result.findings)

    def test_each_missing_channel_blocks_ready_with_its_subject(self):
        platforms = (DARWIN_ARM, LINUX_AMD)
        complete = fx.installs_for(self.candidate, platforms)
        for missing in complete:
            with self.subTest(platform=missing.platform, channel=missing.channel):
                history = self.planned(platforms, installs=tuple(item for item in complete if item != missing))
                result = self.readiness(history)
                subject = f"{missing.os}/{missing.arch}/{missing.channel.value}"
                self.assertFalse(result.ready)
                self.assertEqual(result.findings, (FindingRef(code="install_proof_missing", subject=subject),))

    def test_missing_installs_do_not_block_claims_or_attempt_verdicts(self):
        history = self.planned((DARWIN_ARM, LINUX_AMD), installs=())
        self.assertEqual(evaluate_attempt(self.candidate.manifest, history, 1).state, AttemptState.PASS)
        self.assertEqual(len(self.readiness(history).findings), 6)
        intent = fx.intent(self.candidate, platforms=(DARWIN_ARM, LINUX_AMD))
        unclaimed = fx.history(self.candidate, intents=(intent,), installs=())
        claim = fx.claim(self.candidate, attempt=1, platform=DARWIN_ARM)
        decision = decide_platform_claim(candidate=self.candidate, history=unclaimed, claim=claim)
        self.assertTrue(decision.allowed, decision.findings)

    def test_installs_must_match_native_binaries_and_archive(self):
        platforms = (DARWIN_ARM, LINUX_AMD)
        other = BinaryDigests(*("8" * 64 for _ in range(4)))
        installs = tuple(fx.install(self.candidate, platform=item.platform, channel=item.channel,
                                    binaries=other if item.platform == LINUX_AMD and item.channel is NPM
                                    else fx.NATIVE_BINARIES)
                         for item in fx.installs_for(self.candidate, platforms))
        result = self.readiness(self.planned(platforms, installs=installs))
        self.assertEqual(result.findings, (FindingRef(code="install_binary_mismatch", subject="Linux/amd64/npm"),))
        shell = fx.install(self.candidate, platform=DARWIN_ARM, channel=SHELL)
        archive = shell.consumed_ref(ArtifactKind.ARCHIVE)
        crossed = replace(shell, consumed=tuple(sorted(
            (ref(self.candidate, ArtifactKind.ARCHIVE, LINUX_AMD) if item == archive else item
             for item in shell.consumed), key=lambda item: item.path)))
        installs = tuple(crossed if item == shell else item for item in fx.installs_for(self.candidate, platforms))
        result = self.readiness(self.planned(platforms, installs=installs))
        self.assertEqual(result.findings, (FindingRef(code="install_archive_mismatch", subject="Darwin/arm64/shell"),))


class SharedContractTests(InstallCase):
    def test_native_proof_carries_exact_binary_digests_from_inward_owners(self):
        native = fx.proof(self.candidate, attempt=1, platform=DARWIN_ARM)
        self.assertEqual(native.binaries.as_mapping(), {
            "delegate": "4" * 64, "delegate-run": "5" * 64, "pueue": "6" * 64, "pueued": "7" * 64})
        self.assertCode("candidate_proof_invalid", replace, native, binaries=native.binaries.as_mapping())
        self.assertEqual(parse_binary_digests(native.binaries.as_mapping(), "test"), native.binaries)
        self.assertCode("binary_digests_invalid", parse_binary_digests,
                        {**native.binaries.as_mapping(), "extra": "4" * 64}, "test")
        self.assertIs(attempts.BlockReason, native_contract.BlockReason)
        self.assertIs(EXPECTED_DEFAULT_STATE, release_contract.EXPECTED_DEFAULT_STATE)


class WitnessCompatibilityTests(unittest.TestCase):
    def test_legacy_witness_records_absent_facts_explicitly(self):
        observation = InstallObservation(channel=CHANNEL_SHELL, requests=(), binaries=(), skill_files=0,
                                         delegate_version="0.1.37", default_state=EXPECTED_DEFAULT_STATE)
        facts = CandidateInstallFacts(tag="v0.1.37", candidate_files=(), installs=(observation,))
        value = facts.to_record()
        self.assertIsNone(value["platform"])
        self.assertIsNone(value["installs"][0]["installer_sha256"])


if __name__ == "__main__":
    unittest.main()
