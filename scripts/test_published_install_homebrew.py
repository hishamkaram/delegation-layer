"""Deterministic tests for Homebrew published-consumer verification over mock ports."""

import hashlib
import json
from pathlib import Path
import platform
import sys
import unittest
from unittest.mock import NonCallableMock, patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import verify_release_archive as verifier
from scripts import test_published_install as base
from scripts.delegate_acceptance.release_verification.contract import VerificationError


TAP = "owner/tap"
FORMULA = f"{TAP}/delegation-layer"


class FakeBrewWorld(base.FakeWorld):
    def __init__(self, extract, keg):
        super().__init__(extract)
        self.keg = keg
        self.prefix = "/opt/homebrew"
        self.taps = ["homebrew/core"]
        self.installed = ["git"]
        self.info_version = "1.2.3"
        self.info_tap = TAP
        self.keg_response = None

    def which(self, name):
        return "/opt/homebrew/bin/brew" if name == "brew" else None

    def brew(self, argv):
        verb = argv[1]
        if verb == "--prefix" and len(argv) == 2:
            return self.prefix.encode() + b"\n"
        if verb == "--prefix":
            return str(self.keg).encode() + b"\n" if self.keg_response is None else self.keg_response
        if verb == "tap":
            return "\n".join(self.taps).encode()
        if verb == "list":
            return "\n".join(self.installed).encode()
        if verb == "install":
            self.install_keg()
            return b""
        assert verb == "info", argv
        return json.dumps({"formulae": [{
            "name": "delegation-layer", "tap": self.info_tap, "versions": {"stable": self.info_version},
            "installed": [{"version": self.info_version}],
        }]}).encode()

    def install_keg(self):
        for name in verifier.BINARIES:
            directory = self.keg / ("libexec" if name in {"pueue", "pueued"} else "bin")
            directory.mkdir(parents=True, exist_ok=True)
            data = self.binary_override.get(name, (self.extract / name).read_bytes())
            (directory / name).write_bytes(data)
            (directory / name).chmod(0o755)


class HomebrewTests(base.PublishedBase):
    channel = "homebrew"

    def setUp(self):
        patcher = patch("platform.system", return_value="Darwin")
        patcher.start()
        self.addCleanup(patcher.stop)
        super().setUp()
        self.world = FakeBrewWorld(self.extract, self.root / "Cellar" / "delegation-layer" / "1.2.3")

    def brew_calls(self):
        return [call for call in self.world.calls if call["argv"][0].endswith("/brew")]

    def test_fresh_install_binds_version_and_all_four_hashes_including_libexec_supervisor(self):
        ports = self.world.ports(self.tree, {"HOMEBREW_GITHUB_API_TOKEN": base.SECRET, "GH_TOKEN": base.SECRET})
        self.assertEqual(base.published.verify_published(self.request(), ports=ports), 0)
        receipt = self.receipt()
        self.assertEqual(receipt["verdict"], "pass")
        self.assertEqual(receipt["published"]["verified"]["homebrew_formula"], {"version": "1.2.3", "tap": TAP})
        self.assertEqual(receipt["binaries"]["pueued"]["path"],
                         str(self.world.keg.resolve() / "libexec" / "pueued"))
        self.assertEqual(receipt["binaries"]["delegate-run"]["sha256"],
                         hashlib.sha256(base.binary_script("delegate-run").encode()).hexdigest())
        self.assertEqual(receipt["binaries"]["pueue"]["version"], "4.0.4")
        install = next(call for call in self.brew_calls() if call["name"] == "homebrew-install")
        self.assertEqual(install["argv"][1:], ["install", "--formula", FORMULA])
        for call in self.brew_calls():
            self.assertNotIn("GH_TOKEN", call["env"])
            self.assertNotIn("HOMEBREW_GITHUB_API_TOKEN", call["env"])
            self.assertEqual(call["env"]["HOMEBREW_NO_AUTO_UPDATE"], "1")
            self.assertTrue(call["env"]["HOME"].endswith("/home"))
        self.assertEqual(receipt["cleanup"]["status"], "complete")
        self.assertEqual(self.workspaces(), [])
        self.assertTrue(any("retained, not uninstalled" in item for item in receipt["unresolved_limits"]))

    def test_non_fresh_homebrew_is_rejected_before_installing(self):
        for field, value in (("taps", [TAP]), ("installed", ["delegation-layer"]), ("installed", [FORMULA])):
            with self.subTest(field=field, value=value):
                self.reset()
                setattr(self.world, field, value)
                self.assertFailed("homebrew_not_fresh")
                self.assertFalse([call for call in self.brew_calls() if call["name"] == "homebrew-install"])

    def test_unsupported_prefix_is_rejected_so_linuxbrew_never_counts(self):
        self.world.prefix = "/home/linuxbrew/.linuxbrew"
        self.assertFailed("homebrew_prefix_unsupported")

    def test_each_installed_binary_must_match_the_archive_including_libexec(self):
        for name in verifier.BINARIES:
            with self.subTest(name):
                self.reset()
                self.world.binary_override = {name: b"#!/bin/sh\nexit 9\n"}
                receipt = self.assertFailed("installed_binary_hash_mismatch")
                self.assertEqual(receipt["published"]["verified"]["homebrew_install"], "completed-and-retained")

    def test_formula_version_and_tap_must_match(self):
        for attribute, value in (("info_version", "1.2.4"), ("info_tap", "other/tap")):
            with self.subTest(attribute):
                self.reset()
                setattr(self.world, attribute, value)
                self.assertFailed("homebrew_version_mismatch")

    def test_malformed_keg_response_fails_typed_before_any_binary_is_exercised(self):
        cases = {
            "empty": b"", "blank": b"\n", "whitespace": b"  \t\n", "malformed utf-8": b"\xff\xfe\n",
            "valid first of two lines": None,
        }
        for label, raw in cases.items():
            with self.subTest(label):
                self.reset()
                self.world.keg_response = raw if raw is not None else (
                    f"{self.world.keg}\n{self.world.keg}\n".encode())
                receipt = self.assertFailed("homebrew_keg_invalid")
                self.assertEqual(receipt["failure"]["stage"], "homebrew-observe")
                self.assertFalse([call for call in self.world.calls if call["name"] == "delegate-version"])
                self.assertEqual(receipt["published"]["verified"]["homebrew_install"], "completed-and-retained")

    def test_missing_keg_binary_is_a_typed_failure(self):
        self.world.install_keg = lambda: None
        self.assertFailed("homebrew_binary_invalid")

    def test_install_timeout_retains_workspace_and_reports_unresolved_ownership(self):
        injected = VerificationError("command_timeout", "homebrew-install")
        self.world.raise_on = {"homebrew-install": injected}
        receipt = self.assertFailed("command_timeout")
        self.assertEqual(receipt["published"]["unresolved_ownership"]["stage"], "homebrew-install")
        self.assertNotIn("homebrew_install", receipt["published"]["verified"])
        self.assertEqual(receipt["cleanup"]["removed_paths"], [])
        self.settle_port_fault(injected)

    def test_reset_restores_every_patch_before_the_fresh_fixture(self):
        previous = self.tree.root
        self.reset()
        self.assertFalse(previous.exists())
        self.assertEqual(platform.system(), "Darwin")
        self.doCleanups()
        self.assertFalse(self.tree.root.exists())
        self.assertNotIsInstance(platform.system, NonCallableMock)

    def test_missing_brew_is_reported(self):
        self.world.which = lambda name: None
        self.assertFailed("brew_unavailable")


class HomebrewLinuxTests(base.PublishedBase):
    channel = "homebrew"

    def setUp(self):
        patcher = patch("platform.system", return_value="Linux")
        patcher.start()
        self.addCleanup(patcher.stop)
        super().setUp()
        self.world = FakeBrewWorld(self.extract, self.root / "Cellar")

    def test_linux_is_refused_without_touching_brew(self):
        self.assertFailed("homebrew_requires_macos")
        self.assertFalse([call for call in self.world.calls if call["argv"][0].endswith("/brew")])


if __name__ == "__main__":
    unittest.main()
