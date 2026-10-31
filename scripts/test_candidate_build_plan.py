"""Tests for the pure development-candidate build rules."""

from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification import candidate_build_plan as plan
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    ArtifactKind, encode_manifest, expected_inventory, parse_manifest,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError


COMMIT = "a1" * 20
TREE = "b2" * 20
BASE = "c3" * 20
REQUEST = plan.BuildRequest(version="0.1.37", commit=COMMIT, base_commit=BASE,
                            repository="hishamkaram/delegation-layer")
GORELEASER_BANNER = (b"  ____       ____      _\n |  _ \\ ___| | ___  __ _ ___  ___ _ __\n"
                     b"GitVersion:    2.18.1\nGitCommit:     deadbeef\nBuiltBy:       goreleaser\n")


def refused(test, code, function, *arguments, **keywords):
    with test.assertRaises(VerificationError) as caught:
        function(*arguments, **keywords)
    test.assertEqual(caught.exception.code, code)


def inputs():
    return plan.build_inputs(config_sha256="1" * 64, go_sum_sha256="2" * 64,
                             go="1.27.3", goreleaser="2.18.1", python="3.12.13", node="22.11.0")


def files(version="0.1.37"):
    return {path: (100 + index, sha(path.encode()))
            for index, (path, _kind, _os, _arch) in enumerate(expected_inventory(version))}


class ToolOutputTests(unittest.TestCase):
    def test_version_formats_on_linux_and_macos(self):
        for system in ("linux/amd64", "darwin/arm64", "linux/arm64"):
            self.assertEqual(plan.parse_go_version(f"go version go1.27.3 {system}\n".encode()), "1.27.3")
        self.assertEqual(plan.parse_node_version(b"v22.11.0\n"), "22.11.0")
        self.assertEqual(plan.parse_python_version(b"Python 3.12.13\n"), "3.12.13")
        self.assertEqual(plan.parse_goreleaser_version(GORELEASER_BANNER), "2.18.1")
        self.assertEqual(plan.parse_goreleaser_version(GORELEASER_BANNER.replace(b"2.18.1", b"v2.18.1")),
                         "2.18.1")

    def test_malformed_version_output_is_refused(self):
        refused(self, "go_version_output_invalid", plan.parse_go_version, b"go version go1.27 linux/amd64\n")
        refused(self, "go_version_output_invalid", plan.parse_go_version,
                b"go version devel +abc linux/amd64\n")
        refused(self, "go_version_output_invalid", plan.parse_go_version,
                b"go version go1.27.3 linux/amd64\nextra\n")
        refused(self, "node_version_output_invalid", plan.parse_node_version, b"22.11.0\n")
        refused(self, "python_version_output_invalid", plan.parse_python_version, b"Python 3.12\n")
        refused(self, "goreleaser_version_output_invalid", plan.parse_goreleaser_version, b"2.18.1\n")
        refused(self, "goreleaser_version_output_invalid", plan.parse_goreleaser_version,
                GORELEASER_BANNER + b"GitVersion:    2.18.2\n")

    def test_go_pin_is_one_stable_line(self):
        self.assertEqual(plan.parse_go_pin(b"1.27.3\n"), "1.27.3")
        for data in (b"1.27.3", b"1.27.3\n\n", b"1.27.3\r\n", b"1.27\n", b"go1.27.3\n", b"1.27.3\nx\n", b""):
            with self.subTest(data=data):
                with self.assertRaises(VerificationError):
                    plan.parse_go_pin(data)

    def test_observed_go_must_equal_the_pin(self):
        plan.require_pinned_go("1.27.3", "1.27.3")
        refused(self, "go_toolchain_pin_mismatch", plan.require_pinned_go, "1.25.8", "1.27.3")


class PackageTests(unittest.TestCase):
    def test_package_version_must_match_the_request(self):
        plan.require_package_version(b'{"name": "delegation-layer", "version": "0.1.37"}', REQUEST)
        refused(self, "package_version_mismatch", plan.require_package_version,
                b'{"name": "delegation-layer", "version": "0.1.36"}', REQUEST)
        refused(self, "package_name_mismatch", plan.require_package_version,
                b'{"name": "other", "version": "0.1.37"}', REQUEST)
        refused(self, "package_json_invalid", plan.require_package_version, b"not json", REQUEST)
        with self.assertRaises(VerificationError):
            plan.require_package_version(b'{"name": "delegation-layer", "version": "0.1.37-rc.1"}', REQUEST)

    def test_request_rejects_unstable_or_malformed_identity(self):
        for changes in ({"version": "0.1.37-rc.1"}, {"version": "v0.1.37"}, {"commit": "abc"},
                        {"base_commit": "G" * 40}, {"repository": "no-slash"}):
            with self.subTest(changes=changes):
                values = {"version": "0.1.37", "commit": COMMIT, "base_commit": BASE,
                          "repository": "hishamkaram/delegation-layer", **changes}
                with self.assertRaises(VerificationError):
                    plan.BuildRequest(**values)
        self.assertEqual(REQUEST.tag, "v0.1.37")

    def test_npm_pack_report_must_name_the_one_expected_tarball(self):
        good = b'[{"id": "delegation-layer@0.1.37", "filename": "delegation-layer-0.1.37.tgz"}]'
        self.assertEqual(plan.packed_filename(good, "0.1.37"), "delegation-layer-0.1.37.tgz")
        for report in (b'[{"filename": "other.tgz"}]', b"[]", b'{"filename": "delegation-layer-0.1.37.tgz"}',
                       b'[{"filename": "delegation-layer-0.1.37.tgz"}, {"filename": "x.tgz"}]', b"npm notice"):
            with self.subTest(report=report):
                refused(self, "npm_pack_output_invalid", plan.packed_filename, report, "0.1.37")


class VcsStampTests(unittest.TestCase):
    def stamp(self, revision=COMMIT, modified="false"):
        return (f"x: go1.27.3\n\tpath\texample.invalid/delegate\n\tbuild\tvcs=git\n"
                f"\tbuild\tvcs.revision={revision}\n\tbuild\tvcs.time=2026-01-01T00:00:00Z\n"
                f"\tbuild\tvcs.modified={modified}\n").encode()

    def test_stamp_must_name_the_built_commit_unmodified(self):
        plan.require_vcs_identity(self.stamp(), COMMIT)
        refused(self, "go_vcs_revision_mismatch", plan.require_vcs_identity, self.stamp("d4" * 20), COMMIT)
        refused(self, "go_vcs_modified", plan.require_vcs_identity, self.stamp(modified="true"), COMMIT)

    def test_missing_or_duplicated_stamp_is_refused(self):
        bare = b"x: go1.27.3\n\tbuild\t-buildvcs=false\n"
        refused(self, "go_vcs_revision_mismatch", plan.require_vcs_identity, bare, COMMIT)
        refused(self, "go_vcs_stamp_invalid", plan.require_vcs_identity,
                self.stamp() + b"\tbuild\tvcs.revision=" + COMMIT.encode() + b"\n", COMMIT)
        refused(self, "go_vcs_modified", plan.require_vcs_identity,
                self.stamp().replace(b"\tbuild\tvcs.modified=false\n", b""), COMMIT)


class ManifestTests(unittest.TestCase):
    def test_manifest_binds_request_tree_inputs_and_exact_inventory(self):
        manifest = plan.build_manifest(REQUEST, tree=TREE, inputs=inputs(), files=files())
        self.assertEqual((manifest.version, manifest.build_tag), ("0.1.37", "v0.1.37"))
        self.assertEqual((manifest.source.commit, manifest.source.tree, manifest.source.base_commit),
                         (COMMIT, TREE, BASE))
        self.assertEqual(manifest.build.toolchains.go, "1.27.3")
        self.assertEqual([entry.kind for entry in manifest.artifacts].count(ArtifactKind.ARCHIVE), 4)
        self.assertEqual(parse_manifest(encode_manifest(manifest)), manifest)

    def test_missing_extra_or_zero_length_files_are_refused(self):
        missing = files()
        missing.pop("checksums.txt")
        refused(self, "candidate_files_mismatch", plan.build_manifest, REQUEST, tree=TREE,
                inputs=inputs(), files=missing)
        refused(self, "candidate_files_mismatch", plan.build_manifest, REQUEST, tree=TREE,
                inputs=inputs(), files={**files(), "extra.txt": (1, "e" * 64)})
        refused(self, "candidate_artifact_invalid", plan.build_manifest, REQUEST, tree=TREE,
                inputs=inputs(), files={**files(), "checksums.txt": (0, "e" * 64)})

    def test_wrong_version_inventory_is_refused(self):
        refused(self, "candidate_files_mismatch", plan.build_manifest, REQUEST, tree=TREE,
                inputs=inputs(), files=files("0.1.36"))


if __name__ == "__main__":
    unittest.main()
