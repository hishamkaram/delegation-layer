"""Pure rules and the curl shim of candidate install proof, exercised through their real entrypoints."""

from __future__ import annotations

from pathlib import Path
import sys
import unittest

repository_root = str(Path(__file__).resolve().parents[3])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.release_tests.candidate_install import fixtures as support
from scripts.release_tests.release_fixture import FixtureTree
from scripts.release_verification import candidate_install_contract as rules
from scripts.release_verification.candidate_install_shim import prepare_shim
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.contract import new_receipt


INFO = {"os": "Linux", "kernel": "6", "arch": "amd64"}
ARCHIVE = f"https://github.com/{support.REPO}/releases/download/v1.2.3/delegation-layer_1.2.3_Linux_amd64.tar.gz"
CHECKSUMS = f"https://github.com/{support.REPO}/releases/download/v1.2.3/checksums.txt"


def request(**changes) -> rules.CandidateInstallRequest:
    values = {
        "tag": "v1.2.3", "repo": support.REPO, "installer": Path("/abs/install.sh"),
        "files": rules.CandidateFiles(Path("/abs/a.tar.gz"), Path("/abs/checksums.txt")),
        "skill": (),
    }
    values.update(changes)
    return rules.CandidateInstallRequest(**values)


class RuleTests(unittest.TestCase):
    def assertCode(self, code, call, *args):
        with self.assertRaises(VerificationError) as caught:
            call(*args)
        self.assertEqual(caught.exception.code, code)

    def test_release_urls_are_exact_for_repo_tag_and_platform(self):
        urls = rules.release_urls(request(), INFO)
        self.assertEqual(urls.expected(), (ARCHIVE, CHECKSUMS))

    def test_request_log_is_strict(self):
        self.assertEqual(rules.parse_request_log(b"served\tu1\ndenied\tu2\n"), (
            rules.RequestRecord("served", "u1"), rules.RequestRecord("denied", "u2")))
        for raw in (b"served\n", b"other\tu\n", b"served\t\n", b"served\ta b\n", b"\xff\n"):
            self.assertCode("candidate_request_log_invalid", rules.parse_request_log, raw)

    def test_requests_must_be_exactly_archive_then_checksums_with_no_refusal(self):
        urls = rules.release_urls(request(), INFO)
        def served(*items):
            return tuple(rules.RequestRecord("served", item) for item in items)

        rules.require_requests(served(ARCHIVE, CHECKSUMS), urls)
        for records in (served(), served(CHECKSUMS, ARCHIVE), served(ARCHIVE), served(ARCHIVE, CHECKSUMS, ARCHIVE)):
            self.assertCode("candidate_requests_unexpected", rules.require_requests, records, urls)
        denied = served(ARCHIVE, CHECKSUMS) + (rules.RequestRecord("denied", "https://example.invalid/x"),)
        self.assertCode("candidate_url_denied", rules.require_requests, denied, urls)

    def test_request_shape_checks(self):
        rules.validate_request(request())
        self.assertCode("invalid_release_tag", rules.validate_request, request(tag="1.2.3"))
        self.assertCode("repository_invalid", rules.validate_request, request(repo="a/b/c"))
        self.assertCode("candidate_path_invalid", rules.validate_request, request(installer=Path("install.sh")))
        files = rules.CandidateFiles(Path("/abs/a\tb.tar.gz"), Path("/abs/checksums.txt"))
        self.assertCode("candidate_path_invalid", rules.validate_request, request(files=files))

    def test_package_arguments_name_the_exact_local_tarball(self):
        self.assertEqual(
            rules.package_command_args(Path("/abs/p.tgz"), ["--target", "/t"]),
            ["exec", "--yes", "--package=/abs/p.tgz", "--", "delegation-layer", "--target", "/t"])


class ShimTests(unittest.TestCase):
    def setUp(self):
        self.tree = FixtureTree.open(prefix=support.TREE_PREFIX, label=support.TREE_LABEL)
        self.addCleanup(self.tree.dispose)
        self.recording = support.RecordingPorts(self.tree)
        self.root = self.tree.root
        self.served = self.root / "served.bin"
        self.served.write_bytes(b"candidate bytes")
        self.shim = prepare_shim(self.root, {ARCHIVE: self.served})
        self.receipt = new_receipt("shim", "v1.2.3", "a" * 40)

    def curl(self, *args: str) -> None:
        env = {"PATH": "/usr/bin:/bin", **self.shim.environment()}
        self.recording.run(
            self.receipt, "shim", [str(self.shim.directory / "curl"), *args], env, cwd=self.root)

    def assertDenied(self, *args: str):
        with self.assertRaises(VerificationError) as caught:
            self.curl(*args)
        self.assertEqual(caught.exception.code, "command_failed")
        self.assertEqual(self.receipt["commands"][-1]["exit_code"], 22)
        self.assertFalse((self.root / "out").exists())
        self.assertEqual(self.shim.requests()[-1].outcome, "denied")

    def test_answers_the_exact_url_with_the_exact_file_and_records_it(self):
        options = ["-fsSL", "--connect-timeout", "5", "--retry", "2"]
        self.curl(*options, "-o", str(self.root / "out"), ARCHIVE)
        self.assertEqual((self.root / "out").read_bytes(), b"candidate bytes")
        self.assertEqual(self.shim.requests(), (rules.RequestRecord("served", ARCHIVE),))

    def test_refuses_unknown_urls_lookups_redirect_options_and_extra_urls(self):
        out = str(self.root / "out")
        self.assertDenied("-fsSL", "-o", out, ARCHIVE + "x")
        self.assertDenied("-fsSL", "-o", "/dev/null", "-w", "%{url_effective}", "https://github.com/o/r/releases/latest")
        self.assertDenied("-fsSL", "--location", "-o", out, ARCHIVE)
        self.assertDenied("-fsSL", "-o", out, ARCHIVE, CHECKSUMS)
        self.assertDenied("-fsSL", ARCHIVE)
        self.assertDenied("-fsSL", "-o", out, "https://api.github.com/repos/o/r/releases/latest")
        self.assertEqual({record.outcome for record in self.shim.requests()}, {"denied"})

    def test_prepared_files_are_private_and_not_reusable(self):
        def mode(path):
            return path.stat().st_mode & 0o777

        self.assertEqual(mode(self.shim.directory / "curl"), 0o700)
        self.assertEqual(mode(self.shim.map_path), 0o600)
        self.assertEqual(mode(self.shim.log_path), 0o600)
        with self.assertRaises(FileExistsError):
            prepare_shim(self.root, {})


class InstalledSelectionTests(unittest.TestCase):
    """Selection proof reads metadata, retains native outcomes, and permits compatible updates."""

    def setUp(self):
        import tempfile
        self.temporary = tempfile.TemporaryDirectory(prefix="installed-selection-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()

    def installation(self, directory):
        from scripts.release_verification.contract import BINARIES, installed_executables
        directory.mkdir()
        paths = {}
        for name in BINARIES:
            path = directory / name
            path.write_text("fixture installation source\n", encoding="utf-8")
            path.chmod(0o700)
            paths[name] = path
        return installed_executables(paths)

    def test_metadata_inventory_does_not_read_executable_contents(self):
        from unittest.mock import patch
        from scripts.release_verification.assets import inspect_binaries
        selected = self.installation(self.root / "install")
        receipt = {"binaries": {}}
        with patch.object(Path, "open", side_effect=AssertionError("executable contents read")):
            self.assertEqual(inspect_binaries(self.root / "install", receipt), selected.as_mapping())
        self.assertTrue(all(set(value) == {"path"} for value in receipt["binaries"].values()))

    def test_bootstrap_refuses_foreign_selection_and_failed_protocol_outcome(self):
        from dataclasses import replace
        from scripts.release_tests import candidate_fixtures as fx
        selected = self.installation(self.root / "install")
        other = self.installation(self.root / "foreign")
        observed = fx.bootstrap(selected)
        self.assertIs(rules.require_bootstrap(observed, selected), observed)
        for foreign in (replace(observed, binaries=other), replace(observed, worker_result="protocol-error")):
            with self.subTest(foreign=foreign), self.assertRaises(VerificationError):
                rules.require_bootstrap(foreign, selected)
        self.assertEqual(observed.to_record()["binaries"], selected.to_record())

    def test_compatible_in_place_update_does_not_change_selection(self):
        from scripts.release_verification.assets import inspect_binaries
        selected = self.installation(self.root / "install")
        selected.pueue.write_text("compatible installation update\n", encoding="utf-8")
        self.assertEqual(inspect_binaries(self.root / "install", {"binaries": {}}), selected.as_mapping())

    def test_missing_nonexecutable_and_outside_roles_are_refused(self):
        from scripts.release_verification.assets import inspect_binaries
        selected = self.installation(self.root / "install")
        selected.pueue.chmod(0o600)
        with self.assertRaises(VerificationError):
            inspect_binaries(self.root / "install", {"binaries": {}})
        selected.pueue.unlink()
        with self.assertRaises(VerificationError):
            inspect_binaries(self.root / "install", {"binaries": {}})
        outside = self.root / "outside"
        outside.write_text("outside installation\n", encoding="utf-8")
        outside.chmod(0o700)
        selected.pueue.symlink_to(outside)
        with self.assertRaises(VerificationError):
            inspect_binaries(self.root / "install", {"binaries": {}})

    def test_role_inventory_is_exact_and_paths_are_absolute(self):
        from scripts.release_verification.contract import installed_executables
        selected = self.installation(self.root / "install")
        for paths in ({name: path for name, path in selected.as_mapping().items() if name != "pueue"},
                      {**selected.as_mapping(), "extra": self.root / "extra"},
                      {**selected.as_mapping(), "pueue": Path("relative")}):
            with self.subTest(paths=paths), self.assertRaises(VerificationError):
                installed_executables(paths)


if __name__ == "__main__":
    unittest.main()
