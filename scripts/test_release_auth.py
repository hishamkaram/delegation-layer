"""Deterministic tests for explicit GitHub authentication modes of release verification."""

import os
from argparse import Namespace
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import verify_release_archive as verifier
from scripts.delegate_acceptance.release_verification import assets, cli, verification
from scripts.delegate_acceptance.release_verification.contract import VerificationError


SECRET = "release-auth-secret-must-not-leak"
TOKEN_NAMES = ("GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN")


class GitHubEnvironmentTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="release-auth-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve(strict=True)
        patcher = patch.dict(os.environ, {"HOME": str(self.root), "PATH": os.defpath})
        patcher.start()
        self.addCleanup(patcher.stop)
        for name in (*TOKEN_NAMES, "GH_CONFIG_DIR", "XDG_CONFIG_HOME"):
            os.environ.pop(name, None)

    def write_hosts(self, directory):
        directory.mkdir(parents=True)
        (directory / "hosts.yml").write_text("github.com:\n  user: someone\n", encoding="utf-8")

    def test_environment_mode_is_the_default_and_requires_a_token(self):
        with self.assertRaises(VerificationError) as caught:
            verifier.gh_environment()
        self.assertEqual(caught.exception.code, "github_auth_unavailable")
        self.write_hosts(self.root / ".config" / "gh")
        with self.assertRaises(VerificationError):
            verifier.gh_environment(auth_mode="environment")

    def test_environment_mode_forwards_either_hosted_token_without_prompting(self):
        for name in ("GH_TOKEN", "GITHUB_TOKEN"):
            with self.subTest(name), patch.dict(os.environ, {name: SECRET}):
                env = verifier.gh_environment()
                self.assertEqual((env["GH_TOKEN"], env["GH_PROMPT_DISABLED"]), (SECRET, "1"))

    def test_local_config_mode_uses_stored_config_and_drops_every_token(self):
        self.write_hosts(self.root / ".config" / "gh")
        with patch.dict(os.environ, {name: SECRET for name in TOKEN_NAMES}):
            env = verifier.gh_environment(auth_mode="local-config")
        self.assertFalse(set(TOKEN_NAMES) & set(env))
        self.assertNotIn(SECRET, env.values())
        self.assertEqual(env["GH_PROMPT_DISABLED"], "1")

    def test_local_config_locations_follow_gh_precedence(self):
        self.write_hosts(self.root / "explicit")
        with patch.dict(os.environ, {"GH_CONFIG_DIR": str(self.root / "explicit")}):
            self.assertNotIn("GH_TOKEN", verifier.gh_environment(auth_mode="local-config"))
        self.write_hosts(self.root / "xdg" / "gh")
        with patch.dict(os.environ, {"XDG_CONFIG_HOME": str(self.root / "xdg")}):
            self.assertNotIn("GH_TOKEN", verifier.gh_environment(auth_mode="local-config"))

    def test_local_config_leaves_storage_resolution_to_gh(self):
        # gh supports credential stores and symlinked dotfile configuration.
        # Environment preparation must not impose another storage policy.
        environment = verifier.gh_environment(auth_mode="local-config")
        self.assertEqual(environment["GH_PROMPT_DISABLED"], "1")
        (self.root / ".config" / "gh").mkdir(parents=True)
        real = self.root / "real-hosts"
        real.write_text("github.com:\n  git_protocol: https\n", encoding="utf-8")
        (self.root / ".config" / "gh" / "hosts.yml").symlink_to(real)
        environment = verifier.gh_environment(auth_mode="local-config")
        self.assertEqual(environment["GH_PROMPT_DISABLED"], "1")

    def test_unknown_mode_is_rejected(self):
        with patch.dict(os.environ, {"GH_TOKEN": SECRET}), self.assertRaises(VerificationError) as caught:
            verifier.gh_environment(auth_mode="token-extraction")
        self.assertEqual(caught.exception.code, "github_auth_mode_invalid")


class AuthForwardingTests(unittest.TestCase):
    def verify_namespace(self, **extra):
        return Namespace(
            receipt="receipt.json", assets_dir="a", extract_dir="e", mode="archive-smoke", tag="v1.2.3",
            repo="owner/repository", source_revision="a" * 40, **extra)

    def captured_modes(self, args):
        modes = []

        def environment(*, auth_mode):
            modes.append(auth_mode)
            raise VerificationError("stop_after_auth", "test")

        with tempfile.TemporaryDirectory(prefix="release-auth-forward-") as directory:
            runner = Path(directory).resolve(strict=True)
            with patch.dict(os.environ, {"RUNNER_TEMP": str(runner)}), \
                    patch.object(verification, "gh_environment", environment), \
                    patch.object(verification, "source_fingerprint", return_value={"sha256": "0" * 64, "files": []}):
                args.receipt = str(runner / "receipt.json")
                args.assets_dir = str(runner / "delegation-layer-release")
                args.extract_dir = str(runner / "delegation-layer-release" / "extracted")
                self.assertEqual(verification.verify(args), 1)
        return modes

    def test_verify_forwards_the_requested_mode_and_defaults_to_environment(self):
        self.assertEqual(self.captured_modes(self.verify_namespace(github_auth="local-config")), ["local-config"])
        self.assertEqual(self.captured_modes(self.verify_namespace()), ["environment"])

    def test_cli_exposes_the_mode_on_verify_and_published_only_with_known_values(self):
        seen = []
        with patch.object(cli, "verify", lambda args: seen.append(args.github_auth) or 0):
            verify = ["verify", "--mode", "archive-smoke", "--assets-dir", "a", "--extract-dir", "e",
                      "--receipt", "r", "--tag", "v1.2.3", "--repo", "o/r", "--source-revision", "a" * 40]
            self.assertEqual(cli.main(verify), 0)
            self.assertEqual(cli.main([*verify, "--github-auth", "local-config"]), 0)
            with self.assertRaises(SystemExit) as caught:
                cli.main([*verify, "--github-auth", "token"])
            self.assertEqual(caught.exception.code, 2)
        self.assertEqual(seen, ["environment", "local-config"])
        published = []
        with patch.object(cli, "run_published", lambda args: published.append(vars(args)) or 0):
            argv = ["published", "--channel", "npm", "--tag", "v1.2.3", "--repo", "o/r",
                    "--source-revision", "a" * 40, "--archive-receipt", "ar", "--extract-dir", "e",
                    "--receipt", "r"]
            self.assertEqual(cli.main(argv), 0)
            self.assertEqual(cli.main([*argv, "--github-auth", "local-config"]), 0)
            with self.assertRaises(SystemExit):
                cli.main([*argv[:2], "pypi", *argv[3:]])
        self.assertEqual([item["github_auth"] for item in published], ["environment", "local-config"])
        self.assertEqual(published[0]["channel"], "npm")

    def test_gh_modes_are_owned_once(self):
        self.assertEqual(assets.GITHUB_AUTH_MODES, ("environment", "local-config"))


if __name__ == "__main__":
    unittest.main()
