"""Real-entrypoint tests: the original install.sh (shell and packaged npm) over local candidate files."""

from __future__ import annotations

from pathlib import Path
import json
import shutil
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import test_candidate_install_support as support
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    CHANNEL_NPM, CHANNEL_SHELL, RequestRecord,
)
from scripts.delegate_acceptance.release_verification import published_contract
from scripts.delegate_acceptance.release_verification.contract import BINARIES, VerificationError


SHELL = (CHANNEL_SHELL,)


class ShellInstallTests(support.CandidateCase):
    def served(self, facts):
        urls = [f"https://github.com/{support.REPO}/releases/download/{self.tag}/{name}"
                for name in (Path(self.request().files.archive).name, "checksums.txt")]
        return tuple(RequestRecord("served", url) for url in urls)

    def assertRefused(self, code, request, *, channels=SHELL):
        with self.assertRaises(VerificationError) as caught:
            self.verify(request, channels=channels)
        self.assertEqual(caught.exception.code, code)
        return caught.exception

    def requests_log(self, channel=CHANNEL_SHELL):
        return (self.layout.root / channel / "shim-requests.log").read_text(encoding="utf-8").splitlines()

    def test_original_installer_installs_exact_binaries_from_exact_urls(self):
        facts = self.verify(self.request(), channels=SHELL)
        (install,) = facts.installs
        self.assertEqual(install.requests, self.served(facts))
        self.assertEqual(dict(install.binaries), {n: support.sha256(self.binaries[n]) for n in BINARIES})
        self.assertEqual(install.delegate_version, self.tag[1:])
        self.assertEqual(install.default_state, ("absent", "not_initialized", "absent"))
        self.assertEqual({role for role, _ in facts.candidate_files}, {"archive", "checksums"})
        self.finish_workspace()

    def test_children_run_outside_checkout_with_scrubbed_environment_and_no_leaks(self):
        facts = self.verify(self.request(), channels=SHELL)
        self.assertTrue(self.recording.calls)
        for call in self.recording.calls:
            self.assertFalse(call.cwd.is_relative_to(support.REPO_ROOT), call.name)
            self.assertTrue(call.cwd.is_relative_to(self.layout.root), call.name)
            self.assertNotIn(support.SECRET, json.dumps(call.env), call.name)
        installer = self.recording.calls[0]
        self.assertEqual(installer.env["DELEGATION_LAYER_VERSION"], self.tag)
        self.assertTrue(installer.env["PATH"].startswith(str(self.layout.root / "shell" / "shim")))
        for text in (json.dumps(self.receipt), json.dumps(facts.to_record())):
            self.assertNotIn(support.SECRET, text)
        self.finish_workspace()

    def test_checksum_mismatch_is_refused_by_the_original_installer(self):
        self.assertRefused("command_failed", self.request(checksum="0" * 64))
        self.assertEqual([line.split("\t")[0] for line in self.requests_log()], ["served", "served"])
        self.assertFalse((self.layout.root / "shell" / "bin" / "delegate").exists())

    def test_binary_content_not_matching_expected_hash_is_refused(self):
        changed = {**self.binaries, "pueue": b"#!/bin/sh\nprintf 'pueue 9.9.9\\n'\n"}
        self.assertRefused("installed_binary_hash_mismatch", self.request(binaries=changed))

    def test_archive_missing_a_binary_installs_nothing(self):
        partial = {name: data for name, data in self.binaries.items() if name != "pueued"}
        self.assertRefused("command_failed", self.request(binaries=partial))
        self.assertFalse((self.layout.root / "shell" / "bin" / "delegate").exists())

    def test_unrecognized_url_is_denied_and_recorded(self):
        error = self.assertRefused("candidate_url_denied", self.request(repo="someone/else"))
        self.assertEqual(error.stage, "verify-requests")
        self.assertTrue(all(line.startswith("denied\t") for line in self.requests_log()))

    def test_candidate_file_changed_during_install_is_refused(self):
        request = self.request()
        self.recording.after = lambda name: request.files.checksums.write_text("tampered\n")
        self.assertRefused("candidate_file_changed", request)

    def test_missing_prerequisite_is_explicit_and_unused_tools_are_not_required(self):
        self.recording.missing = frozenset({"sh"})
        self.assertRefused("sh_unavailable", self.request())
        self.recording.missing = frozenset({"npm", "node"})
        facts = self.verify(self.request(), channels=SHELL)
        self.assertEqual([item.channel for item in facts.installs], [CHANNEL_SHELL])
        self.finish_workspace()

    def test_invalid_requests_fail_before_any_child(self):
        for request, channels, code in (
            (self.request(repo="not a repo"), SHELL, "repository_invalid"),
            (self.request(), (), "candidate_channels_invalid"),
            (self.request(), ("cask",), "candidate_channels_invalid"),
            (self.request(), ("brew",), "candidate_formula_missing"),
            (self.request(), (CHANNEL_SHELL, CHANNEL_SHELL), "candidate_channels_invalid"),
        ):
            self.assertRefused(code, request, channels=channels)
        self.assertEqual(self.recording.calls, [])


@unittest.skipUnless(shutil.which("npm") and shutil.which("node"), "npm and node are required for the npm candidate channel")
class NpmInstallTests(support.PackedCandidateCase):
    def test_exact_tgz_installs_binaries_and_skill_through_the_shell_installer(self):
        facts = self.verify(self.request(), channels=support.install.CHANNELS)
        shell, npm = facts.installs
        self.assertEqual(shell.skill_files, 0)
        self.assertEqual(npm.skill_files, len(support.checkout_skill_digests()))
        self.assertEqual(shell.binaries, npm.binaries)
        for item in facts.installs:
            self.assertEqual(len(item.requests), 2)
            self.assertEqual({r.outcome for r in item.requests}, {"served"})
        npm_calls = [call for call in self.recording.calls if call.name.startswith("candidate-npm-")]
        self.assertEqual([call.name for call in npm_calls], ["candidate-npm-install-cli", "candidate-npm-skill"])
        for call in npm_calls:
            self.assertIn(f"--package={self.package}", call.argv)
            self.assertEqual(call.env["npm_config_offline"], "true")
            self.assertEqual(Path(call.env["npm_config_userconfig"]).stat().st_size, 0)
            self.assertNotIn(support.SECRET, json.dumps(call.env))
            self.assertTrue(call.cwd.is_relative_to(self.layout.root))
            self.assertFalse(call.cwd.is_relative_to(support.REPO_ROOT))
        self.finish_workspace()

    def test_install_needs_no_registry(self):
        with patch.object(published_contract, "NPM_REGISTRY", "http://127.0.0.1:9/"):
            facts = self.verify(self.request(), channels=(CHANNEL_NPM,))
        self.assertEqual([item.channel for item in facts.installs], [CHANNEL_NPM])
        self.finish_workspace()

    def test_packaged_installer_refuses_a_bad_checksum_through_npm(self):
        with self.assertRaises(VerificationError) as caught:
            self.verify(self.request(checksum="0" * 64), channels=(CHANNEL_NPM,))
        self.assertEqual(caught.exception.code, "command_failed")
        self.assertEqual(len((self.layout.root / "npm" / "shim-requests.log").read_text().splitlines()), 2)
        self.assertFalse((self.layout.root / "npm" / "bin" / "delegate").exists())

    def test_packaged_skill_not_matching_expected_hashes_runs_no_child(self):
        wrong = {**support.checkout_skill_digests(), "SKILL.md": "0" * 64}
        with self.assertRaises(VerificationError) as caught:
            self.verify(self.request(skill=wrong), channels=(CHANNEL_NPM,))
        self.assertEqual(caught.exception.code, "npm_pack_skill_mismatch")
        self.assertEqual(self.recording.calls, [])

    def test_wrong_expected_binary_hash_is_refused_for_the_npm_install(self):
        request = self.request()
        wrong = tuple((name, "0" * 64 if name == "delegate" else digest) for name, digest in request.binaries)
        with self.assertRaises(VerificationError) as caught:
            self.verify(type(request)(**{**request.__dict__, "binaries": wrong}), channels=(CHANNEL_NPM,))
        self.assertEqual(caught.exception.code, "installed_binary_hash_mismatch")

    def test_missing_npm_is_explicit(self):
        self.recording.missing = frozenset({"npm"})
        with self.assertRaises(VerificationError) as caught:
            self.verify(self.request(), channels=(CHANNEL_NPM,))
        self.assertEqual(caught.exception.code, "npm_unavailable")


if __name__ == "__main__":
    unittest.main()
