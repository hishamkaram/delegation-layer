"""Candidate Homebrew observation: fake Brew children, real binary children, real proof reader.

Only ``brew`` invocations are faked. Every delegate and supervisor command runs through the real
bounded command owner against fixture executables, so PATH and symlink resolution are real.
None of this is native Homebrew evidence.
"""

from __future__ import annotations

import contextlib
from dataclasses import replace
import io
from pathlib import Path
import json
import shutil
import sys
import unittest
from unittest import mock

repository_root = str(Path(__file__).resolve().parents[3])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.release_tests.release_fixture import FixtureTree
from scripts.acceptance.evidence.digests import sha
from scripts.acceptance.processes import operations as operations
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.contracts import CommandFailure
from scripts.release_verification import candidate_install as install
from scripts.release_verification import candidate_install_brew as brew_install
from scripts.release_verification import published_homebrew as published_homebrew
from scripts.release_verification.assets import exercise_default_path
from scripts.release_verification.assets import inspect_binaries
from scripts.release_verification.candidate_install_brew import observe_brew_install
from scripts.release_verification.candidate_install_contract import BootstrapTarget
from scripts.release_verification.candidate_install_contract import CandidateFiles
from scripts.release_verification.candidate_install_contract import CandidateInstallFacts
from scripts.release_verification.candidate_install_contract import CandidateInstallRequest
from scripts.acceptance.evidence import source as commands
from scripts.acceptance.evidence.source import command
from scripts.release_verification.contract import BINARIES
from scripts.release_verification.contract import EXPECTED_DEFAULT_STATE
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import new_receipt
from scripts.release_verification.contract import installed_executables
from scripts.release_verification.published_contract import homebrew_prefixes
from scripts.release_verification.published_ports import ConsumerPorts
from scripts.release_tests import candidate_fixtures as fx
from scripts.release_tests.candidate_install.fixtures import (
    REPO, REPO_ROOT, SECRET, RecordingProbe, fake_binary, observed_children, write_archive,
)
from scripts.test_candidate_source_support import ChildFault, settle_retained


VERSION = "9.8.7"
TAG = "v" + VERSION
DARWIN = {"os": "Darwin", "kernel": "24.0.0", "arch": "arm64"}
LINUX = {"os": "Linux", "kernel": "6.8.0", "arch": "amd64"}
UNSUPPORTED = {"os": "FreeBSD", "kernel": "14.1", "arch": "amd64"}
FORMULA = b'class DelegationLayer < Formula\n  url "https://example.invalid/unpublished.tar.gz"\nend\n'
# Current Homebrew advertises ``trust`` and refuses an untrusted tap formula; legacy Brew lacks the command.
CURRENT_COMMANDS = b"cache\ncommands\ninfo\ninstall\ntap-new\ntest\ntrust\nuntrust\n"
LEGACY_COMMANDS = b"cache\ncommands\ninfo\ninstall\ntap-new\ntest\n"


class BrewPorts:
    """``brew`` children are fakes keyed by command name; every other child really runs.

    Every child failure reaches the fixture tree, which alone decides whether to retain it.
    """

    def __init__(self, case: BrewCase):
        self.case = case
        self.calls: list[tuple[str, list[str], dict[str, str]]] = []
        self.overrides: dict = {}
        self.execute = command

    def run(self, receipt, name, argv, env, *, capture=False, timeout=30, cwd=None):
        self.calls.append((name, list(argv), dict(env)))

        def child() -> bytes:
            if not name.startswith(("homebrew-", "candidate-brew-")):
                return self.execute(receipt, name, argv, env, capture=capture, timeout=timeout, cwd=cwd)
            receipt["commands"].append({"name": name, "argv": argv, "exit_code": 0})
            handler = self.overrides.get(name) or getattr(self.case, "brew_" + name.replace("-", "_"), None)
            if handler is None:
                raise AssertionError("unmodelled brew child: " + name)
            return handler(argv, env)

        return self.case.tree.observe_operation(child)

    def names(self) -> list[str]:
        return [name for name, _argv, _env in self.calls]

    def ports(self) -> ConsumerPorts:
        return ConsumerPorts(run=self.run, which={"brew": str(self.case.brew)}.get,
                             inherited={"GH_TOKEN": SECRET, "SSL_CERT_FILE": "/etc/ssl/cert.pem",
                                        "HOMEBREW_NO_INSTALL_FROM_API": "1"})

    def brew_calls(self) -> list[tuple[str, list[str], dict[str, str]]]:
        return [call for call in self.calls if call[0].startswith(("homebrew-", "candidate-brew-"))]


class BrewCase(unittest.TestCase):
    def setUp(self) -> None:
        self.tree = FixtureTree.open(prefix="candidate-brew-test", label="candidate brew test fixture")
        self.addCleanup(self.tree.dispose)
        root = self.tree.root
        self.prefix = root / "homebrew"
        self.keg = self.prefix / "Cellar" / "delegation-layer" / VERSION
        self.tap_dir = self.prefix / "Library" / "Taps" / "candidate"
        self.outside = root / "outside"
        for directory in (root / "candidate", root / "runner", self.prefix / "bin", self.outside):
            directory.mkdir(parents=True)
        self.brew = self.prefix / "bin" / "brew"
        self.brew.write_bytes(b"#!/bin/sh\nexit 97\n")
        self.binaries = {name: fake_binary(name, VERSION) for name in BINARIES}
        self.archive, _checksums = write_archive(root / "candidate", TAG, self.binaries)
        self.formula = root / "candidate" / "delegation-layer.rb"
        self.formula.write_bytes(FORMULA)
        self.receipt = new_receipt("candidate-install", TAG, "a" * 40)
        self.layout = install.open_workspace(owner=self.tree.owner, parent=root / "runner", receipt=self.receipt)
        self.recording = BrewPorts(self)
        self.probe = RecordingProbe()
        self.commands = CURRENT_COMMANDS
        self.trusted: dict[str, set[str]] = {}  # private HOME -> exactly granted formula specs
        # Only the prefix value moves into the tree; which OS has a prefix stays the real policy.
        patch = mock.patch.object(published_homebrew, "homebrew_prefixes",
                                  lambda os_name: (str(self.prefix),) if homebrew_prefixes(os_name) else ())
        patch.start()
        self.addCleanup(patch.stop)

    def request(self, *, binaries: dict[str, bytes] | None = None) -> CandidateInstallRequest:
        return CandidateInstallRequest(
            tag=TAG, repo=REPO, installer=REPO_ROOT / "install.sh",
            files=CandidateFiles(self.archive, formula=self.formula),
            skill=())

    def observe(self, request: CandidateInstallRequest | None = None):
        return observe_brew_install(request or self.request(), ports=self.recording.ports(), probe=self.probe,
                                    receipt=self.receipt, layout=self.layout, info=DARWIN)

    def assertCode(self, code: str, request: CandidateInstallRequest | None = None) -> None:
        with self.assertRaises(VerificationError) as caught:
            self.observe(request)
        self.assertEqual(caught.exception.code, code)

    def tearDown_case(self) -> None:
        """Give each subtest a fresh workspace and tap; the fixture tree still owns all of it.

        A child the tree holds unresolved may still use the tap, so nothing is reset beneath it.
        """
        self.assertIsNone(self.tree.unresolved, "no reset beneath an unresolved child")
        self.recording = BrewPorts(self)
        self.receipt = new_receipt("candidate-install", TAG, "a" * 40)
        self.layout = install.open_workspace(owner=self.tree.owner, parent=self.tree.root / "runner",
                                             receipt=self.receipt)
        if self.tap_dir.exists():
            shutil.rmtree(self.tap_dir)

    def cache_file(self, env) -> Path:
        return Path(env["HOMEBREW_CACHE"]) / "downloads" / f"0123--delegation-layer-{VERSION}.tar.gz"

    def brew_homebrew_prefix(self, argv, env):
        return f"{self.prefix}\n".encode()

    def brew_homebrew_taps(self, argv, env):
        return b""

    def brew_homebrew_installed(self, argv, env):
        return b"git\n"

    def brew_candidate_brew_commands(self, argv, env):
        return self.commands

    def brew_candidate_brew_trust(self, argv, env):
        if (self.tap_dir / "Formula" / "delegation-layer.rb").read_bytes() != FORMULA:
            raise VerificationError("command_failed", "candidate-brew-trust")
        self.trusted.setdefault(env["HOME"], set()).add(argv[-1])
        return b""

    def brew_candidate_brew_test(self, argv, env):
        return b""

    def brew_candidate_brew_tap_new(self, argv, env):
        self.tap_dir.mkdir(parents=True)
        return b""

    def brew_candidate_brew_tap_repository(self, argv, env):
        return f"{self.tap_dir}\n".encode()

    def brew_candidate_brew_cache_path(self, argv, env):
        if b"trust" in self.commands.split() and argv[-1] not in self.trusted.get(env["HOME"], ()):
            raise VerificationError("command_failed", "candidate-brew-cache-path")  # untrusted tap formula
        return f"{self.cache_file(env)}\n".encode()

    def brew_candidate_brew_install(self, argv, env):
        if self.cache_file(env).read_bytes() != self.archive.read_bytes():
            raise VerificationError("command_failed", "candidate-brew-install")
        for name, data in self.binaries.items():
            path = self.keg / ("libexec" if name in ("pueue", "pueued") else "bin") / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
            path.chmod(0o755)
        (self.prefix / "opt").mkdir(exist_ok=True)
        for link, target in ((self.prefix / "opt" / "delegation-layer", self.keg),
                             (self.prefix / "bin" / "delegate", self.keg / "bin" / "delegate")):
            link.unlink(missing_ok=True)
            link.symlink_to(target)
        return b""

    def brew_candidate_brew_info(self, argv, env):
        tap = argv[-1].rsplit("/", 1)[0]
        return json.dumps({"formulae": [{"name": "delegation-layer", "tap": tap, "versions": {"stable": VERSION},
                                         "installed": [{"version": VERSION}]}]}).encode()

    def brew_candidate_brew_keg(self, argv, env):
        return f"{self.prefix / 'opt' / 'delegation-layer'}\n".encode()


class ObservationTest(BrewCase):
    def test_fresh_install_observes_formula_cache_keg_linked_delegate_and_default_state(self):
        observation = self.observe()
        self.assertEqual(observation.channel, "brew")
        self.assertEqual((observation.requests, observation.skill_files), ((), 0))
        self.assertEqual(observation.installer_sha256, sha(FORMULA))
        self.assertEqual(observation.binaries.as_mapping(), {name: (self.keg / ("libexec" if name in {"pueue", "pueued"} else "bin") / name).resolve() for name in BINARIES})
        self.assertEqual((observation.delegate_version, observation.default_state), (VERSION, EXPECTED_DEFAULT_STATE))
        names = self.recording.names()
        self.assertEqual(names[:5], ["homebrew-prefix", "homebrew-taps", "homebrew-installed",
                                     "candidate-brew-commands", "candidate-brew-tap-new"])
        self.assertEqual(names[5:], [
            "candidate-brew-tap-repository", "candidate-brew-trust", "candidate-brew-cache-path",
            "candidate-brew-install", "candidate-brew-info", "candidate-brew-keg", "delegate-version", "pueue-version",
            "pueued-version",
            "delegate-providers", "delegate-capabilities", "delegate-default-supervisor-status",
            "candidate-brew-test"])
        argv = {name: argv for name, argv, _env in self.recording.calls}
        keg = self.keg.resolve()
        self.assertEqual(argv["pueue-version"][0], str(keg / "libexec" / "pueue"))
        self.assertEqual(argv["pueued-version"][0], str(keg / "libexec" / "pueued"))
        self.assertEqual(argv["delegate-default-supervisor-status"], ["delegate", "supervisor", "status", "--json"])
        self.assertEqual(argv["delegate-version"][0], "delegate")
        self.assertEqual(argv["candidate-brew-install"][1:],
                         ["install", "--build-from-source", "--formula", self.receipt["candidate_brew"]["spec"]])
        self.assertEqual((self.tap_dir / "Formula" / "delegation-layer.rb").read_bytes(), FORMULA)
        self.assertEqual(self.receipt["candidate_brew"]["cache"]["state"], "absent-seeded")
        self.assertEqual(self.receipt["default_path_observation"]["path_source"], "PATH")

    def test_onboarding_reuses_linked_delegate_before_bootstrap_without_reimplementing_libexec(self):
        events = []
        setup = mock.Mock()
        setup.fresh.side_effect = lambda: events.append("fresh")
        setup.verify_repeat.side_effect = lambda delegate: events.append("repeat") or {"repeat": "completed"}
        self.probe = RecordingProbe(lambda target: events.append("bootstrap") or fx.bootstrap(target.binaries))
        ports = self.recording.ports()
        ports = replace(ports, which=lambda name: str(self.brew) if name == "brew" else shutil.which(name))
        with mock.patch.object(brew_install, "CandidateSetup", return_value=setup) as constructor:
            observation = observe_brew_install(
                self.request(), ports=ports, probe=self.probe, receipt=self.receipt,
                layout=self.layout, info=DARWIN, onboarding=True)
        self.assertEqual(events, ["fresh", "repeat", "bootstrap"])
        args, options = constructor.call_args
        self.assertEqual(args[6], self.prefix / "bin")
        self.assertIs(options["select_directory"], False)
        setup.verify_repeat.assert_called_once_with(self.keg / "bin" / "delegate")
        self.assertEqual(observation.onboarding, {"repeat": "completed"})
        self.assertEqual(self.probe.targets[0].supervisor_directory, self.keg / "libexec")

    def test_brew_children_are_credential_free_with_owned_cache_and_logs(self):
        self.observe()
        for name, _argv, env in self.recording.calls:
            self.assertNotIn("GH_TOKEN", env, name)
            self.assertNotIn(SECRET, json.dumps(env), name)
        brew_calls = self.recording.brew_calls()
        self.assertIn("candidate-brew-test", [name for name, _argv, _env in brew_calls])
        for name, _argv, env in brew_calls:
            self.assertNotIn("HOMEBREW_NO_INSTALL_FROM_API", env, name)
        env = next(env for name, _argv, env in self.recording.calls if name == "candidate-brew-install")
        for key in ("HOMEBREW_CACHE", "HOMEBREW_LOGS", "HOME"):
            self.assertTrue(Path(env[key]).is_relative_to(self.layout.root), key)

    def test_a_standalone_supervisor_pair_on_path_does_not_block_or_replace_keg_supervisors(self):
        for name in ("pueue", "pueued"):
            other = self.prefix / "bin" / name
            other.write_bytes(f"#!/bin/sh\nprintf '{name} 0.0.1\\n'\n".encode())
            other.chmod(0o755)
        self.observe()
        self.assertEqual(self.receipt["binaries"]["pueue"]["version"], "4.0.4")
        self.assertEqual(self.receipt["binaries"]["pueued"]["version"], "4.0.4")

    def test_the_probe_targets_the_linked_bin_and_the_keg_libexec_pair_before_brew_test(self):
        seen = []

        def observe(target):
            seen.append(self.recording.names())
            return fx.bootstrap(target.binaries)

        self.probe = RecordingProbe(observe)
        observation = self.observe()
        keg = self.keg.resolve()
        expected = installed_executables({name: keg / ("libexec" if name in {"pueue", "pueued"} else "bin") / name for name in BINARIES})
        self.assertEqual(self.probe.targets, [BootstrapTarget(
            command_directory=self.prefix / "bin", delegate=keg / "bin" / "delegate",
            supervisor_directory=keg / "libexec", binaries=expected)])
        self.assertEqual(seen[0][-1], "delegate-default-supervisor-status")
        self.assertEqual(self.recording.names()[-1], "candidate-brew-test")
        self.assertEqual(observation.bootstrap, fx.bootstrap(expected))

    def test_a_turn_through_a_path_pair_instead_of_the_keg_pair_is_refused_before_brew_test(self):
        standalone = {}
        for name in ("pueue", "pueued"):
            other = self.prefix / "bin" / name
            standalone[name] = f"#!/bin/sh\nprintf '{name} 4.0.4\\n'\n# standalone\n".encode()
            other.write_bytes(standalone[name])
            other.chmod(0o755)
        self.probe = RecordingProbe(lambda target: fx.bootstrap(replace(
            target.binaries, pueue=self.prefix / "bin" / "pueue", pueued=self.prefix / "bin" / "pueued")))
        self.assertCode("install_bootstrap_mismatch")
        self.assertEqual(len(self.probe.targets), 1)
        self.assertNotIn("candidate-brew-test", self.recording.names())

    def test_compatible_in_place_keg_update_reaches_the_probe(self):
        self.binaries["pueued"] = b"#!/bin/sh\nprintf 'pueued 4.0.4\\n'\n# compatible update\n"
        observation = self.observe()
        self.assertEqual(observation.bootstrap.outcome, "committed")
        self.assertEqual(len(self.probe.targets), 1)

    def test_linked_delegate_must_resolve_to_the_verified_keg(self):
        def install_elsewhere(argv, env):
            self.brew_candidate_brew_install(argv, env)
            link = self.prefix / "bin" / "delegate"
            link.unlink()
            stray = self.outside / "delegate"
            stray.write_bytes(self.binaries["delegate"])
            stray.chmod(0o755)
            link.symlink_to(stray)
            return b""

        self.recording.overrides["candidate-brew-install"] = install_elsewhere
        self.assertCode("brew_linked_delegate_mismatch")
        self.assertNotIn("delegate-version", self.recording.names())


class TrustTest(BrewCase):
    """Current Brew trusts only the exact validated formula, in the private HOME, before the cache query."""

    def observe_or_fail(self):
        try:
            return self.observe()
        except VerificationError as error:
            self.fail(f"current Homebrew observation failed: {error.code} at {error.stage}")

    def test_the_exact_formula_is_trusted_in_the_private_home_after_validation_and_before_the_cache(self):
        self.observe_or_fail()
        calls = self.recording.brew_calls()
        names = [name for name, _argv, _env in calls]
        argv = {name: argv for name, argv, _env in calls}
        spec = self.receipt["candidate_brew"]["spec"]
        self.assertEqual(argv["candidate-brew-commands"], [str(self.brew), "commands", "--quiet"])
        self.assertEqual(argv["candidate-brew-trust"], [str(self.brew), "trust", "--formula", spec])
        self.assertLess(names.index("candidate-brew-commands"), names.index("candidate-brew-tap-new"))
        self.assertEqual(names[names.index("candidate-brew-tap-repository") + 1:][:2],
                         ["candidate-brew-trust", "candidate-brew-cache-path"])
        self.assertEqual([name for name in names if "trust" in name], ["candidate-brew-trust"])
        self.assertFalse(any("--version" in arguments or "--tap" in arguments for arguments in argv.values()))
        homes = {env["HOME"] for _name, _argv, env in calls}
        self.assertEqual(len(homes), 1)
        (home,) = homes
        self.assertTrue(Path(home).is_relative_to(self.layout.root))
        self.assertEqual(self.trusted, {home: {spec}})
        self.assertEqual(self.receipt["candidate_brew"].get("trust"), "formula")

    def test_legacy_brew_without_trust_installs_without_a_trust_call(self):
        self.commands = LEGACY_COMMANDS
        self.observe_or_fail()
        self.assertNotIn("candidate-brew-trust", self.recording.names())
        self.assertEqual(self.receipt["candidate_brew"].get("trust"), "not-advertised")
        self.assertEqual(self.trusted, {})

    def test_an_unusable_command_inventory_stops_before_the_tap_cache_or_install(self):
        def failed(argv, env):
            raise VerificationError("command_failed", "candidate-brew-commands")

        cases = {"failed": (failed, "command_failed"), "empty": (b"", "homebrew_commands_invalid"),
                 "undecodable": (b"install\ntap-new\n\xff\n", "homebrew_commands_invalid"),
                 "no tap-new": (b"install\ntrust\n", "homebrew_commands_invalid"),
                 "no install": (b"installer\ntap-new\ntrust\n", "homebrew_commands_invalid")}
        for label, (reply, code) in cases.items():
            with self.subTest(label):
                self.tearDown_case()
                self.recording.overrides["candidate-brew-commands"] = (
                    reply if callable(reply) else lambda argv, env, reply=reply: reply)
                self.assertCode(code)
                self.assertEqual(self.recording.names()[-1], "candidate-brew-commands")
                self.assertFalse(self.tap_dir.exists())

    def test_a_trust_fault_stops_before_the_cache_and_install_and_keeps_the_tap(self):
        def refused(argv, env):
            raise VerificationError("command_failed", "candidate-brew-trust")

        self.recording.overrides["candidate-brew-trust"] = refused
        self.assertCode("command_failed")
        self.assertEqual(self.recording.names()[-1], "candidate-brew-trust")
        self.assertEqual(self.receipt["candidate_brew"].get("trust"), "pending")
        self.assertEqual((self.tap_dir / "Formula" / "delegation-layer.rb").read_bytes(), FORMULA)


class RefusalTest(BrewCase):
    def test_non_fresh_homebrew_is_refused_before_the_tap_exists(self):
        self.recording.overrides["homebrew-installed"] = lambda argv, env: b"delegation-layer\n"
        self.assertCode("homebrew_not_fresh")
        self.assertNotIn("candidate-brew-tap-new", self.recording.names())
        self.assertFalse(self.tap_dir.exists())

    def test_cache_path_escapes_are_refused_before_any_directory_is_created(self):
        def link_out(argv, env):
            cache = Path(env["HOMEBREW_CACHE"])
            (cache / "link").symlink_to(self.outside, target_is_directory=True)
            return f"{cache / 'link' / 'nested' / 'x.tar.gz'}\n".encode()

        def link_file(argv, env):
            target = self.cache_file(env)
            target.parent.mkdir(parents=True)
            target.symlink_to(self.outside / "planted.tar.gz")
            return f"{target}\n".encode()

        cases = {
            "final symlink": link_file,
            "parent": lambda argv, env: f"{env['HOMEBREW_CACHE']}/../outside/nested/x.tar.gz\n".encode(),
            "root": lambda argv, env: f"{self.outside / 'nested' / 'x.tar.gz'}\n".encode(),
            "relative": lambda argv, env: b"downloads/x.tar.gz\n",
            "symlink": link_out,
        }
        for label, handler in cases.items():
            with self.subTest(label):
                self.tearDown_case()
                self.recording.overrides["candidate-brew-cache-path"] = handler
                self.assertCode("homebrew_cache_unexpected")
                self.assertNotIn("candidate-brew-install", self.recording.names())
                self.assertEqual(list(self.outside.iterdir()), [])

    def test_a_reset_after_an_unresolved_child_refuses_and_keeps_the_tap(self):
        injected = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)

        def unconverted(receipt, name, argv, env, **options):
            raise injected

        self.recording.execute = unconverted
        refusal = AssertionError("no child may launch in a synthetic closure fault")
        with mock.patch.object(operations.subprocess, "Popen", mock.Mock(side_effect=refusal)) as launches:
            with self.assertRaises(BoundedCommandError):
                self.observe()
        self.assertIs(self.tree.unresolved, injected)
        # The proof: the synthetic fault launched nothing, so the retained tree may go once the case ends.
        self.assertEqual(launches.call_count, 0)

        def settle() -> None:
            with contextlib.redirect_stderr(io.StringIO()):
                self.assertFalse(self.tree.dispose())
            self.tree.remove_retained()
        self.addCleanup(settle)
        layout = self.layout
        with self.assertRaises(AssertionError):
            self.tearDown_case()
        self.assertTrue((self.tap_dir / "Formula" / "delegation-layer.rb").is_file())
        self.assertIs(self.layout, layout)

    def test_existing_matching_cache_is_accepted_and_foreign_bytes_are_never_replaced(self):
        def preexisting(data):
            def handler(argv, env):
                target = self.cache_file(env)
                target.parent.mkdir(parents=True)
                target.write_bytes(data)
                return f"{target}\n".encode()
            return handler

        self.recording.overrides["candidate-brew-cache-path"] = preexisting(self.archive.read_bytes())
        self.observe()
        self.assertEqual(self.receipt["candidate_brew"]["cache"]["state"], "existing-matching")
        self.tearDown_case()
        self.recording.overrides["candidate-brew-cache-path"] = preexisting(b"foreign")
        self.assertCode("homebrew_cache_conflict")
        self.assertEqual(Path(self.receipt["candidate_brew"]["cache"]["path"]).read_bytes(), b"foreign")
        self.assertNotIn("candidate-brew-install", self.recording.names())

    def test_wrong_version_and_layout_are_refused(self):
        self.binaries["delegate"] = fake_binary("delegate", "0.0.1")
        self.assertCode("delegate_version_mismatch", self.request())
        self.assertEqual(self.receipt["delegate_build_identity"], {"version": "0.0.1"})
        self.tearDown_case()
        self.binaries["delegate"] = fake_binary("delegate", VERSION)
        self.recording.overrides["candidate-brew-info"] = lambda argv, env: self.brew_candidate_brew_info(
            ["x", "other/tap/delegation-layer"], env)
        self.assertCode("homebrew_version_mismatch")

    def test_changed_formula_or_archive_bytes_fail_the_observation(self):
        for label, target in (("formula", self.formula), ("archive", self.archive)):
            with self.subTest(label):
                self.tearDown_case()
                original = target.read_bytes()

                def tampering(argv, env, target=target):
                    target.write_bytes(target.read_bytes() + b"\n")
                    return b""

                self.recording.overrides["candidate-brew-test"] = tampering
                self.assertCode("candidate_file_changed")
                target.write_bytes(original)
                self.recording.overrides.clear()

    def test_malformed_keg_response_fails_typed_before_any_binary_is_exercised(self):
        keg = self.prefix / "opt" / "delegation-layer"
        cases = {
            "empty": b"", "blank": b"\n", "whitespace": b"  \t\n", "malformed utf-8": b"\xff\xfe\n",
            "valid first of two lines": f"{keg}\n{keg}\n".encode(),
        }
        for label, raw in cases.items():
            with self.subTest(label):
                self.tearDown_case()
                self.recording.overrides["candidate-brew-keg"] = lambda argv, env, raw=raw: raw
                with self.assertRaises(VerificationError) as caught:
                    self.observe()
                self.assertEqual((caught.exception.code, caught.exception.stage),
                                 ("candidate-brew-keg_invalid", "candidate-install-brew"))
                self.assertNotIn("delegate-version", self.recording.names())

    def test_formula_is_required_and_an_unsupported_os_is_refused_before_any_child(self):
        request = CandidateInstallRequest(TAG, REPO, REPO_ROOT / "install.sh", CandidateFiles(self.archive),
                                          ())
        self.assertCode("candidate_formula_missing", request)
        for info in (UNSUPPORTED, {"kernel": "6", "arch": "amd64"}):
            with self.subTest(info=info), self.assertRaises(VerificationError) as caught:
                observe_brew_install(self.request(), ports=self.recording.ports(), probe=self.probe,
                                     receipt=self.receipt, layout=self.layout, info=info)
            self.assertEqual((caught.exception.code, caught.exception.stage),
                             ("homebrew_host_unsupported", "homebrew-observe"))
        self.assertEqual(self.recording.calls, [])

    def test_a_prefix_of_the_other_os_is_refused_under_the_real_policy_before_the_tap_exists(self):
        for info, reported in ((LINUX, "/opt/homebrew"), (LINUX, "/usr/local"),
                               (DARWIN, "/home/linuxbrew/.linuxbrew")):
            with self.subTest(os=info["os"], prefix=reported):
                self.tearDown_case()
                self.recording.overrides["homebrew-prefix"] = lambda argv, env, reported=reported: (
                    f"{reported}\n".encode())
                with mock.patch.object(published_homebrew, "homebrew_prefixes", homebrew_prefixes):
                    with self.assertRaises(VerificationError) as caught:
                        observe_brew_install(self.request(), ports=self.recording.ports(), probe=self.probe,
                                             receipt=self.receipt, layout=self.layout, info=info)
                self.assertEqual(caught.exception.code, "homebrew_prefix_unsupported")
                self.assertEqual(self.recording.names(), ["homebrew-prefix"])
                self.assertFalse(self.tap_dir.exists())


class ComposerTest(BrewCase):
    def test_default_channels_stay_shell_and_npm_and_brew_is_explicit(self):
        self.assertEqual(install.CHANNELS, ("shell", "npm"))
        self.assertIn("brew", install.SUPPORTED_CHANNELS)

    def compose(self, info: dict[str, str]) -> CandidateInstallFacts:
        return install.verify_candidate_install(
            self.request(), ports=self.recording.ports(), probe=self.probe, receipt=self.receipt,
            layout=self.layout, info=info, channels=("brew",))

    def test_brew_composes_on_the_supported_host_os_with_exact_formula_archive_binaries_and_test(self):
        actual = install.platform_identity()
        facts = self.compose(actual)
        self.assertEqual(facts.platform, (actual["os"], actual["arch"]))
        self.assertEqual(dict(facts.candidate_files),
                         {"archive": sha(self.archive.read_bytes()), "formula": sha(FORMULA)})
        (observation,) = facts.installs
        self.assertEqual((observation.channel, observation.installer_sha256), ("brew", sha(FORMULA)))
        self.assertEqual(observation.binaries.as_mapping(), {name: (self.keg / ("libexec" if name in {"pueue", "pueued"} else "bin") / name).resolve() for name in BINARIES})
        self.assertEqual((observation.delegate_version, observation.default_state), (VERSION, EXPECTED_DEFAULT_STATE))
        self.assertEqual((self.tap_dir / "Formula" / "delegation-layer.rb").read_bytes(), FORMULA)
        cache = self.receipt["candidate_brew"]["cache"]
        self.assertEqual((Path(cache["path"]).read_bytes(), cache["state"]),
                         (self.archive.read_bytes(), "absent-seeded"))
        self.assertEqual(self.receipt["candidate_brew"]["prefix"], str(self.prefix))
        self.assertEqual(self.recording.names()[-1], "candidate-brew-test")

    def test_an_unsupported_host_os_is_refused_before_any_child(self):
        with mock.patch.object(install, "platform_identity", return_value=dict(UNSUPPORTED)):
            with self.assertRaises(VerificationError) as caught:
                self.compose(UNSUPPORTED)
        self.assertEqual((caught.exception.code, caught.exception.stage),
                         ("homebrew_host_unsupported", "homebrew-observe"))
        self.assertEqual(self.recording.calls, [])

    def test_a_host_other_than_the_expected_platform_is_refused_before_any_child(self):
        actual = install.platform_identity()
        expected = dict(actual, os="Darwin" if actual["os"] == "Linux" else "Linux")
        with self.assertRaises(VerificationError) as caught:
            self.compose(expected)
        self.assertEqual(caught.exception.code, "candidate_platform_mismatch")
        self.assertEqual(self.recording.calls, [])


class PrefixPolicyTest(unittest.TestCase):
    """The one OS prefix policy and the shared freshness reader, with no fixture override."""

    def test_each_os_has_exactly_its_own_prefixes_and_any_other_os_has_none(self):
        self.assertEqual(homebrew_prefixes("Darwin"), ("/opt/homebrew", "/usr/local"))
        self.assertEqual(homebrew_prefixes("Linux"), ("/home/linuxbrew/.linuxbrew",))
        for os_name in ("FreeBSD", "Windows", "darwin", "linux", ""):
            with self.subTest(os_name=os_name):
                self.assertEqual(homebrew_prefixes(os_name), ())

    def fresh(self, host_os: str, prefix: str, names: list[str]) -> str:
        """The shared freshness reader over fake ``brew`` replies; ``names`` records each child."""
        replies = {"homebrew-prefix": f"{prefix}\n".encode(), "homebrew-taps": b"homebrew/core\n",
                   "homebrew-installed": b"git\n"}

        def run(name, argv, *, capture=False, timeout=120):
            names.append(name)
            return replies[name]

        return published_homebrew.require_fresh(run, "/brew", "owner/tap", host_os=host_os)

    def test_freshness_accepts_only_the_prefix_of_the_reported_host_os(self):
        accepted = (("Darwin", "/opt/homebrew"), ("Darwin", "/usr/local"), ("Linux", "/home/linuxbrew/.linuxbrew"))
        for host_os, prefix in accepted:
            with self.subTest(host_os=host_os, prefix=prefix):
                names: list[str] = []
                self.assertEqual(self.fresh(host_os, prefix, names), prefix)
                self.assertEqual(names, ["homebrew-prefix", "homebrew-taps", "homebrew-installed"])
        refused = (("Linux", "/opt/homebrew"), ("Linux", "/usr/local"), ("Darwin", "/home/linuxbrew/.linuxbrew"),
                   ("FreeBSD", "/opt/homebrew"), ("FreeBSD", "/home/linuxbrew/.linuxbrew"))
        for host_os, prefix in refused:
            with self.subTest(host_os=host_os, prefix=prefix):
                names = []
                with self.assertRaises(VerificationError) as caught:
                    self.fresh(host_os, prefix, names)
                self.assertEqual((caught.exception.code, caught.exception.stage),
                                 ("homebrew_prefix_unsupported", "homebrew-observe"))
                self.assertEqual(names, ["homebrew-prefix"])


class FixtureLifetimeTest(BrewCase):
    """A real child's unresolved failure reaches the fixture tree, which then retains it."""

    def test_an_unresolved_child_failure_retains_the_tree_until_zero_launches_are_proven(self):
        injected = VerificationError("command_timeout", "delegate-version")

        def timed_out(receipt, name, argv, env, **options):
            raise injected

        self.recording.execute = timed_out
        refusal = AssertionError("no child may launch in a synthetic closure fault")
        with mock.patch.object(operations.subprocess, "Popen", mock.Mock(side_effect=refusal)) as launches:
            with self.assertRaises(VerificationError) as caught:
                self.observe()
        self.assertIs(caught.exception, injected)
        self.assertIs(self.tree.unresolved, injected)
        self.assertEqual(self.recording.names()[-1], "delegate-version")
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            self.assertFalse(self.tree.dispose())
        self.assertIn(f"(command_timeout): {self.tree.root}", stderr.getvalue())
        self.assertTrue(self.layout.root.is_dir())
        self.assertEqual(launches.call_count, 0)
        self.tree.remove_retained()
        self.assertFalse(self.tree.root.exists())

    def test_an_original_unconverted_command_failure_retains_the_tree(self):
        injected = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)

        def unconverted(receipt, name, argv, env, **options):
            raise injected

        self.recording.execute = unconverted
        refusal = AssertionError("no child may launch in a synthetic closure fault")
        with mock.patch.object(operations.subprocess, "Popen", mock.Mock(side_effect=refusal)) as launches:
            with self.assertRaises(BoundedCommandError) as caught:
                self.observe()
        self.assertIs(caught.exception, injected)
        self.assertIs(self.tree.unresolved, injected)
        with contextlib.redirect_stderr(io.StringIO()) as stderr:
            self.assertFalse(self.tree.dispose())
        self.assertIn(f"(command_termination_timeout): {self.tree.root}", stderr.getvalue())
        self.assertEqual(launches.call_count, 0)
        self.tree.remove_retained()
        self.assertFalse(self.tree.root.exists())


class LegacyDefaultPathTest(unittest.TestCase):
    """The flat archive layout keeps its three PATH resolutions, error codes and command order."""

    def setUp(self) -> None:
        self.tree = FixtureTree.open(prefix="candidate-brew-legacy", label="legacy default path fixture")
        self.addCleanup(self.tree.dispose)
        self.bin = self.tree.root / "bin"
        self.bin.mkdir()
        for name in BINARIES:
            path = self.bin / name
            path.write_bytes(fake_binary(name, VERSION))
            path.chmod(0o755)
        self.receipt = new_receipt("legacy", TAG, "a" * 40)
        self.paths = inspect_binaries(self.bin, self.receipt)

    def exercise(self, home_name: str = "home") -> None:
        home = self.tree.root / home_name
        home.mkdir()
        with observed_children(self.tree):
            exercise_default_path(self.paths, self.bin, self.receipt, VERSION, DARWIN, home, cwd=self.tree.root)

    def test_flat_layout_runs_by_name_in_the_legacy_order(self):
        self.exercise()
        self.assertEqual([item["name"] for item in self.receipt["commands"]], [
            "delegate-version", "pueue-version", "pueued-version", "delegate-providers",
            "delegate-capabilities", "delegate-default-supervisor-status"])
        self.assertTrue(all(item["argv"][0] in {"delegate", "pueue", "pueued"} for item in self.receipt["commands"]))
        self.assertEqual(self.receipt["default_path_observation"]["observed_state"], "not_initialized")

    def test_each_name_must_resolve_from_the_one_directory(self):
        for name in ("delegate", "pueue", "pueued"):
            with self.subTest(name):
                moved = self.tree.root / f"moved-{name}"
                moved.mkdir()
                (self.bin / name).rename(moved / name)
                with self.assertRaises(VerificationError) as caught:
                    self.exercise(f"home-{name}")
                self.assertEqual((caught.exception.code, caught.exception.stage),
                                 ("binary_path_mismatch", f"resolve-{name}"))
                (moved / name).rename(self.bin / name)

    def test_an_unproven_default_path_child_retains_the_tree(self):
        fault = ChildFault(lambda argv: argv[:1] == ["delegate"])
        with mock.patch.object(commands, "run_bounded_command", fault), \
                self.assertRaises(VerificationError) as caught:
            self.exercise()
        self.assertEqual((caught.exception.code, caught.exception.stage), ("command_timeout", "delegate-version"))
        self.assertIs(type(self.tree.unresolved), BoundedCommandError)
        assert isinstance(self.tree.unresolved, BoundedCommandError)
        self.assertIs(self.tree.unresolved.reason, CommandFailure.TIMEOUT)
        settle_retained(self, self.tree, fault, (self.tree.root / "home", self.bin))


if __name__ == "__main__":
    unittest.main()
