"""Real-entrypoint tests: the original install.sh (shell and packaged npm) over local candidate files."""

from __future__ import annotations

from dataclasses import replace
from pathlib import Path
import contextlib
import io
import json
import shutil
import stat
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import test_candidate_install_support as support
from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.release_verification.candidate_install_bootstrap import InstalledBootstrapProbe
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    CHANNEL_NPM, CHANNEL_SHELL, BootstrapTarget, RequestRecord,
)
from scripts.delegate_acceptance.release_verification import published_contract
from scripts.delegate_acceptance.release_verification.contract import BINARIES, VerificationError, installed_executables


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
        self.assertEqual(install.binaries.as_mapping(), {n: (self.layout.root / "shell" / "bin" / n).resolve() for n in BINARIES})
        self.assertEqual(install.delegate_version, self.tag[1:])
        self.assertEqual(install.default_state, ("absent", "not_initialized", "absent"))
        self.assertEqual({role for role, _ in facts.candidate_files}, {"archive", "checksums"})
        self.finish_workspace()

    def test_children_run_outside_checkout_with_scrubbed_environment_and_no_leaks(self):
        facts = self.verify(self.request(), channels=SHELL)
        self.assertTrue(self.recording.calls)
        for call in self.recording.calls:
            assert call.cwd is not None
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

    def test_compatible_executable_update_is_allowed(self):
        changed = {**self.binaries, "pueue": b"#!/bin/sh\nprintf 'pueue 9.9.9\\n'\n"}
        facts = self.verify(self.request(binaries=changed), channels=SHELL)
        self.assertEqual(facts.installs[0].bootstrap.outcome, "committed")

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
        checksums = request.files.checksums
        assert checksums is not None

        def tamper(_name: str) -> None:
            checksums.write_text("tampered\n")

        self.recording.after = tamper
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


class ShellBootstrapGateTests(support.CandidateCase):
    """The installed fixture turn gates acceptance after the version, catalog, hash and fresh-status checks."""

    def assertRefused(self, code, request):
        with self.assertRaises(VerificationError) as caught:
            self.verify(request, channels=SHELL)
        self.assertEqual(caught.exception.code, code)

    def default_path_children(self):
        """The by-name default-path children; they run through the receipt-recording command runner."""
        return [record["name"] for record in self.receipt["commands"]]

    def test_the_probe_runs_once_on_the_installed_layout_after_every_default_path_check(self):
        request = self.request()
        seen = []

        def observe(target):
            seen.append(self.default_path_children())
            return support.accepted_bootstrap(target)

        self.probe = support.RecordingProbe(observe)
        facts = self.verify(request, channels=SHELL)
        bin_dir = self.layout.root / "shell" / "bin"
        expected = installed_executables({name: (bin_dir / name).resolve() for name in BINARIES})
        self.assertEqual(self.probe.targets, [BootstrapTarget(
            command_directory=bin_dir, delegate=(bin_dir / "delegate").resolve(), supervisor_directory=bin_dir,
            binaries=expected)])
        self.assertEqual(seen, [self.default_path_children()])
        self.assertEqual(seen[0][-1], "delegate-default-supervisor-status")
        self.assertEqual(facts.installs[0].bootstrap, fx.bootstrap(expected))
        self.assertEqual(facts.to_record()["installs"][0]["bootstrap"], fx.bootstrap(expected).to_record())
        self.finish_workspace()

    def test_incompatible_delegate_protocol_refuses_before_the_probe(self):
        changed = {**self.binaries, "delegate": b"#!/bin/sh\nexit 9\n"}
        self.assertRefused("command_failed", self.request(binaries=changed))
        self.assertEqual(self.probe.targets, [])

    def test_a_decoy_pair_turn_fails_although_version_catalog_hashes_and_fresh_status_pass(self):
        self.probe = support.RecordingProbe(lambda target: fx.bootstrap(
            replace(target.binaries, pueue=self.root / "foreign" / "pueue", pueued=self.root / "foreign" / "pueued")))
        self.assertRefused("install_bootstrap_mismatch", self.request())
        self.assertEqual(len(self.probe.targets), 1)
        self.assertTrue(self.default_path_children())

    def test_an_uncommitted_unclosed_or_foreign_runner_turn_fails(self):
        for field, value in (("outcome", "rejected"), ("worker_result", "wait-error"),
                             ("termination_cause", "budget"), ("scope_empty", False), ("shutdown", "pid-absent")):
            with self.subTest(field=field):
                self.probe = support.RecordingProbe(
                    lambda target, field=field, value=value: replace(fx.bootstrap(target.binaries), **{field: value}))
                self.assertRefused("install_bootstrap_mismatch", self.request())
        self.probe = support.RecordingProbe(lambda target: fx.bootstrap(replace(target.binaries, delegate_run=self.root / "foreign" / "delegate-run")))
        self.assertRefused("install_bootstrap_mismatch", self.request())

    def test_the_real_probe_refuses_the_version_only_delegate_and_retains_its_root(self):
        self.receipt["candidate_install"] = {}
        locations = {}
        real = InstalledBootstrapProbe(receipt=self.receipt, locations=locations, parent=self.root)
        self.probe = support.RecordingProbe(
            lambda target: self.tree.observe_operation(lambda: real(target), closure_required=True))
        self.assertRefused("candidate_bootstrap_unresolved", self.request())
        facts = self.receipt["candidate_install"]["bootstrap"]
        root = locations["bootstrap_root"]
        self.assertEqual((facts["status"], facts["root"], facts["failure"]["type"]), ("failed", str(root), "RuntimeError"))
        self.assertIn("exit=8", facts["failure"]["message"])
        (pending,) = facts["unresolved_ownership"]
        self.assertEqual(len(pending["task_ids"]), 1)
        self.assertFalse(Path(pending["config"]).exists())
        self.assertTrue(root.is_dir())
        # The test's own proof: the shell stand-in exited 8 without a child and no supervisor config exists.
        dispatch = json.loads((root / "r" / "packaged-dispatch.json").read_text(encoding="utf-8"))
        self.assertEqual(dispatch["exit_code"], 8)
        self.assertEqual(dispatch["failure_class"], "nonzero_exit")
        self.assertEqual((dispatch["stdout_bytes"], dispatch["stderr_bytes"]), (0, 0))
        arguments = dispatch["arguments"]
        self.assertEqual(arguments[0], "dispatch")
        self.assertEqual(arguments[arguments.index("--id") + 1], pending["task_ids"][0])
        selected = self.layout.root / "shell" / "bin" / "delegate"
        metadata = selected.lstat()
        self.assertTrue(stat.S_ISREG(metadata.st_mode))
        self.assertTrue(metadata.st_mode & 0o111)
        self.assertEqual(metadata.st_size, len(self.binaries["delegate"]))
        self.assertEqual(Path(facts["delegate"]).resolve(strict=True), selected.resolve(strict=True))
        self.assertEqual(self.probe.targets[0].delegate, selected.resolve(strict=True))
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertFalse(self.tree.dispose())
        self.tree.remove_retained()
        self.assertFalse(self.root.exists())


@unittest.skipUnless(shutil.which("npm") and shutil.which("node"), "npm and node are required for the npm candidate channel")
class NpmInstallTests(support.PackedCandidateCase):
    def test_exact_tgz_installs_binaries_and_skill_through_the_shell_installer(self):
        facts = self.verify(self.request(), channels=support.install.CHANNELS)
        shell, npm = facts.installs
        self.assertEqual(shell.skill_files, 0)
        self.assertEqual(npm.skill_files, len(support.checkout_skill_digests()))
        self.assertNotEqual(shell.binaries, npm.binaries)
        self.assertEqual(set(shell.binaries.as_mapping()), set(npm.binaries.as_mapping()))
        for item in facts.installs:
            self.assertEqual(len(item.requests), 2)
            self.assertEqual({r.outcome for r in item.requests}, {"served"})
        npm_calls = [call for call in self.recording.calls if call.name.startswith("candidate-npm-")]
        self.assertEqual([call.name for call in npm_calls], ["candidate-npm-install-cli", "candidate-npm-skill"])
        for call in npm_calls:
            assert call.cwd is not None
            self.assertIn(f"--package={self.package}", call.argv)
            self.assertEqual(call.env["npm_config_offline"], "true")
            self.assertEqual(Path(call.env["npm_config_userconfig"]).stat().st_size, 0)
            self.assertNotIn(support.SECRET, json.dumps(call.env))
            self.assertTrue(call.cwd.is_relative_to(self.layout.root))
            self.assertFalse(call.cwd.is_relative_to(support.REPO_ROOT))
        self.finish_workspace()

    def test_each_archive_channel_probes_its_own_installed_directory(self):
        self.verify(self.request(), channels=support.install.CHANNELS)
        self.assertEqual([(target.command_directory, target.supervisor_directory) for target in self.probe.targets],
                         [(self.layout.root / name / "bin",) * 2 for name in support.install.CHANNELS])
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

    def test_wrong_selected_installation_is_refused_for_the_npm_install(self):
        self.probe = support.RecordingProbe(lambda target: fx.bootstrap(replace(
            target.binaries, delegate=self.root / "foreign" / "delegate")))
        with self.assertRaises(VerificationError) as caught:
            self.verify(self.request(), channels=(CHANNEL_NPM,))
        self.assertEqual(caught.exception.code, "install_bootstrap_mismatch")

    def test_missing_npm_is_explicit(self):
        self.recording.missing = frozenset({"npm"})
        with self.assertRaises(VerificationError) as caught:
            self.verify(self.request(), channels=(CHANNEL_NPM,))
        self.assertEqual(caught.exception.code, "npm_unavailable")


if __name__ == "__main__":
    unittest.main()
