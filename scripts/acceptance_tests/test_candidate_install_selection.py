"""An install proof has one canonical digest, and readiness selects exactly the required installs."""

from __future__ import annotations

from collections import Counter
from dataclasses import replace
import unittest

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.candidate_attempts import CellStatus
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    INSTALLER_REPO_PATH, ArtifactRef, HarnessIdentity, InstallChannel, SourceFileBinding,
    encode_install_proof, parse_install_proof,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import PLATFORMS, ArtifactKind
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    AttemptState, evaluate_attempt, evaluate_readiness,
)
from scripts.delegate_acceptance.release_verification.contract import BinaryDigests


DARWIN_ARM = ("Darwin", "arm64")
LINUX_AMD = ("Linux", "amd64")
PLANNED = (DARWIN_ARM, LINUX_AMD)
SHELL, NPM = InstallChannel.SHELL, InstallChannel.NPM
OTHER_BINARIES = BinaryDigests(delegate="8" * 64, delegate_run="5" * 64, pueue="6" * 64, pueued="7" * 64)
CODEX_WRITE = fx.case_id("codex:exec", "write")


class InstallDigestTests(unittest.TestCase):
    def setUp(self):
        self.candidate = fx.bound()
        self.shell = fx.install(self.candidate, platform=DARWIN_ARM, channel=SHELL)

    def test_digest_is_the_canonical_encoded_proof_digest(self):
        for channel in InstallChannel:
            proof = fx.install(self.candidate, platform=DARWIN_ARM, channel=channel)
            with self.subTest(channel=channel):
                data = encode_install_proof(proof)
                self.assertEqual(proof.sha256, sha(data))
                self.assertEqual(parse_install_proof(data).sha256, proof.sha256)
                self.assertNotEqual(proof.sha256, proof.witness_sha256)
                self.assertNotEqual(proof.sha256, proof.installer.sha256)

    def test_digest_changes_with_every_bound_fact(self):
        archive = self.shell.consumed_ref(ArtifactKind.ARCHIVE)
        other_archive = ArtifactRef(kind=archive.kind, path=archive.path, sha256="9" * 64)
        mutations = {
            "witness": replace(self.shell, witness_sha256=sha(b"another witness")),
            "harness_commit": replace(self.shell, harness=HarnessIdentity(commit="8" * 40, sha256="9" * 64)),
            "harness_sha256": replace(self.shell, harness=HarnessIdentity(commit="9" * 40, sha256="8" * 64)),
            "installer": replace(self.shell, installer=SourceFileBinding(
                source_commit=fx.COMMIT, repo_path=INSTALLER_REPO_PATH, sha256=sha(b"#!/bin/sh\nexit 0\n"))),
            "artifact": replace(self.shell, consumed=tuple(other_archive if ref == archive else ref
                                                           for ref in self.shell.consumed)),
            "binary": replace(self.shell, binaries=OTHER_BINARIES),
        }
        digests = {name: proof.sha256 for name, proof in mutations.items()}
        for name, proof in mutations.items():
            with self.subTest(mutation=name):
                self.assertNotEqual(proof, self.shell)
                self.assertEqual(proof.sha256, sha(encode_install_proof(proof)))
                self.assertNotEqual(proof.sha256, self.shell.sha256)
        self.assertEqual(len(set(digests.values())), len(digests))


class InstallSelectionTests(unittest.TestCase):
    def setUp(self):
        self.candidate = fx.bound()

    def readiness(self, history, *, observed=None):
        return evaluate_readiness(candidate=self.candidate, observed=observed or fx.observed(),
                                  scope=fx.scope(), history=history, review=fx.review(self.candidate))

    def single_attempt(self, installs):
        intent = fx.intent(self.candidate, platforms=PLANNED)
        proofs = tuple(fx.proof(self.candidate, attempt=1, platform=platform) for platform in PLANNED)
        return fx.history(self.candidate, intents=(intent,), proof_records=proofs, installs=installs)

    def replaced_attempt(self, installs):
        """Attempt 1 failed on every platform; reviewed attempt 2 plans only ``PLANNED``."""
        first = fx.intent(self.candidate)
        failed = fx.cells(overrides={CODEX_WRITE: (CellStatus.FAIL, None)})
        prior = tuple(fx.proof(self.candidate, attempt=1, platform=platform, results=failed)
                      for platform in PLATFORMS)
        base = fx.history(self.candidate, intents=(first,), proof_records=prior, installs=installs)
        triage = fx.triage(self.candidate, attempt=1, prior_proofs=prior,
                           explained=evaluate_attempt(self.candidate.manifest, base, 1).findings,
                           approved=fx.approval(attempt=2, platforms=PLANNED))
        second = fx.intent(self.candidate, attempt=2, platforms=PLANNED, triage_sha256=triage.sha256)
        latest = tuple(fx.proof(self.candidate, attempt=2, platform=platform) for platform in PLANNED)
        return fx.history(self.candidate, intents=(first, second), proof_records=prior + latest,
                          triages=(triage,), installs=installs)

    def assertSelectsExactlyPlanned(self, history, result):
        self.assertTrue(result.ready, result.findings)
        by_digest = {install.sha256: (install.platform, install.channel) for install in history.installs}
        self.assertEqual(len(by_digest), len(history.installs))
        selected = Counter(by_digest[digest] for digest in result.install_sha256s)
        self.assertEqual(selected, Counter((platform, channel) for platform in PLANNED
                                           for channel in InstallChannel))
        self.assertEqual(result.install_sha256s, tuple(sorted(result.install_sha256s)))
        expected = tuple(sorted(install.sha256 for install in history.installs if install.platform in PLANNED))
        self.assertEqual(result.install_sha256s, expected)

    def test_ready_selects_each_planned_platform_channel_install_once(self):
        history = self.single_attempt(fx.installs_for(self.candidate, PLANNED))
        self.assertSelectsExactlyPlanned(history, self.readiness(history))

    def test_unplanned_platform_install_is_excluded(self):
        history = self.single_attempt(fx.installs_for(self.candidate, PLATFORMS))
        result = self.readiness(history)
        self.assertSelectsExactlyPlanned(history, result)
        unplanned = {install.sha256 for install in history.installs if install.platform not in PLANNED}
        self.assertEqual(len(unplanned), (len(PLATFORMS) - len(PLANNED)) * len(InstallChannel))
        self.assertFalse(unplanned & set(result.install_sha256s))

    def test_installs_of_platforms_only_an_earlier_attempt_planned_are_excluded(self):
        history = self.replaced_attempt(fx.installs_for(self.candidate, PLATFORMS))
        result = self.readiness(history)
        self.assertSelectsExactlyPlanned(history, result)
        self.assertEqual([verdict.state for verdict in result.attempt_verdicts],
                         [AttemptState.FAIL, AttemptState.PASS])
        self.assertEqual(result.proof_sha256s,
                         tuple(sorted(proof.proof_sha256 for proof in history.proofs_for(2))))
        self.assertEqual(result.replacements, history.triages)

    def test_missing_or_mismatched_required_install_is_refused_with_no_selection(self):
        complete = fx.installs_for(self.candidate, PLATFORMS)
        missing = fx.install(self.candidate, platform=LINUX_AMD, channel=NPM)
        mismatched = fx.install(self.candidate, platform=LINUX_AMD, channel=NPM, binaries=OTHER_BINARIES)
        cases = {
            "install_proof_missing": tuple(item for item in complete if item != missing),
            "install_binary_mismatch": tuple(mismatched if item == missing else item for item in complete),
        }
        for code, installs in cases.items():
            with self.subTest(code=code):
                result = self.readiness(self.single_attempt(installs))
                self.assertFalse(result.ready)
                self.assertEqual({finding.code for finding in result.findings}, {code})
                self.assertEqual(result.install_sha256s, ())

    def test_any_other_refusal_also_returns_no_selected_installs(self):
        history = self.single_attempt(fx.installs_for(self.candidate, PLANNED))
        result = self.readiness(history, observed=fx.observed(head="f" * 40))
        self.assertFalse(result.ready)
        self.assertEqual(result.install_sha256s, ())
        empty = self.readiness(fx.history(self.candidate, intents=()))
        self.assertEqual((empty.ready, empty.install_sha256s), (False, ()))


if __name__ == "__main__":
    unittest.main()
