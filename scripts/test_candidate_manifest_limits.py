"""Each manifest artifact kind is bounded by the reader that consumes it."""

import hashlib
import json
from dataclasses import replace
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.candidate_fixtures import manifest
from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.release_verification.candidate_build_artifacts import (
    BuildContext, observed_files,
)
from scripts.delegate_acceptance.release_verification.candidate_build_plan import BuildRequest
from scripts.delegate_acceptance.release_verification.candidate_codec import (
    encode_record, load_record,
)
from scripts.delegate_acceptance.release_verification.candidate_install import (
    CHANNEL_SHELL, verify_candidate_install,
)
from scripts.delegate_acceptance.release_verification.candidate_install_contract import (
    CandidateFiles, CandidateInstallRequest,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    CANDIDATE_WORKFLOW_PATH, MANIFEST_PATH, MAX_CONTAINER_BYTES, MAX_MANIFEST_BYTES, PAYLOAD_MEMBER_COUNT,
    STAGE, ArtifactEntry, ArtifactKind, candidate_artifact_name, encode_manifest, entry_limit,
    expected_inventory, parse_manifest, payload_paths,
)
from scripts.delegate_acceptance.release_verification.contract import MAX_ARCHIVE, VerificationError
from scripts.delegate_acceptance.release_verification.published_files import read_package_bytes


MIB = 1024 * 1024
KIB = 1024
KIND_LIMITS = {
    ArtifactKind.ARCHIVE: 256 * MIB, ArtifactKind.NPM_PACKAGE: 16 * MIB,
    ArtifactKind.CHECKSUMS: 64 * KIB, ArtifactKind.HOMEBREW_FORMULA: 64 * KIB,
}
VERSION = "0.1.37"
REPOSITORY_ROOT = Path(__file__).resolve().parent.parent


def sized(kind: ArtifactKind, size: int):
    """The canonical manifest with every entry of ``kind`` set to ``size`` bytes."""
    value = manifest()
    return replace(value, artifacts=tuple(
        replace(entry, size=size) if entry.kind is kind else entry for entry in value.artifacts))


def sparse(path: Path, size: int) -> Path:
    """A file of ``size`` bytes that occupies no data blocks."""
    with path.open("xb") as handle:
        handle.truncate(size)
    return path


def refused_code(test: unittest.TestCase, function, *arguments, **keywords) -> VerificationError:
    with test.assertRaises(VerificationError) as caught:
        function(*arguments, **keywords)
    return caught.exception


class EntryLimitTests(unittest.TestCase):
    def test_each_kind_is_bounded_by_its_own_reader(self):
        self.assertEqual({kind: entry_limit(kind) for kind in ArtifactKind}, KIND_LIMITS)

    def test_complete_manifest_roundtrips_at_every_kind_limit(self):
        for kind, limit in KIND_LIMITS.items():
            with self.subTest(kind=kind.value):
                value = sized(kind, limit)
                self.assertEqual(parse_manifest(encode_manifest(value)), value)

    def test_one_byte_past_a_kind_limit_is_refused_by_construction_and_by_parsing(self):
        for kind, limit in KIND_LIMITS.items():
            with self.subTest(kind=kind.value):
                error = refused_code(self, sized, kind, limit + 1)
                self.assertEqual((error.code, error.stage), ("candidate_artifact_invalid", STAGE))
                record = load_record(encode_manifest(sized(kind, limit)), max_bytes=MAX_MANIFEST_BYTES,
                                     stage=STAGE)
                for artifact in record["artifacts"]:
                    if artifact["kind"] == kind.value:
                        artifact["size"] = limit + 1
                error = refused_code(self, parse_manifest, encode_record(record, STAGE))
                self.assertEqual((error.code, error.stage), ("candidate_artifact_invalid", STAGE))

    def test_a_size_valid_for_the_archive_is_refused_for_every_smaller_kind(self):
        size = KIND_LIMITS[ArtifactKind.NPM_PACKAGE] + 1
        self.assertEqual(sized(ArtifactKind.ARCHIVE, size).archive("Linux", "amd64").size, size)
        for kind in (ArtifactKind.CHECKSUMS, ArtifactKind.HOMEBREW_FORMULA, ArtifactKind.NPM_PACKAGE):
            with self.subTest(kind=kind.value):
                self.assertEqual(refused_code(self, sized, kind, size).code, "candidate_artifact_invalid")

    def test_invalid_kind_or_size_keeps_the_canonical_error(self):
        template = dict(path="checksums.txt", os=None, arch=None, sha256="a" * 64)
        for kind in (None, "checksums", 0, object()):
            with self.subTest(kind=kind):
                error = refused_code(self, ArtifactEntry, kind=kind, size=1, **template)
                self.assertEqual((error.code, error.stage), ("candidate_artifact_invalid", STAGE))
        for size in (0, -1, True, 1.0, "1", None):
            with self.subTest(size=size):
                error = refused_code(self, ArtifactEntry, kind=ArtifactKind.CHECKSUMS, size=size,
                                     **template)
                self.assertEqual((error.code, error.stage), ("candidate_artifact_invalid", STAGE))

    def test_manifest_path_is_owned_by_the_inventory_beside_its_seven_files(self):
        self.assertEqual(MANIFEST_PATH, "candidate-manifest.json")
        self.assertNotIn(MANIFEST_PATH, [path for path, _kind, _os, _arch in expected_inventory(VERSION)])


class PayloadNamesTests(unittest.TestCase):
    def test_payload_is_the_flat_inventory_and_its_manifest(self):
        inventory = [path for path, _kind, _os, _arch in expected_inventory(VERSION)]
        self.assertEqual(payload_paths(VERSION), tuple(sorted([*inventory, MANIFEST_PATH])))
        self.assertTrue(all("/" not in path for path in payload_paths(VERSION)))

    def test_member_count_and_container_budget_derive_from_the_inventory(self):
        self.assertEqual(PAYLOAD_MEMBER_COUNT, len(payload_paths(VERSION)))
        self.assertEqual(PAYLOAD_MEMBER_COUNT, 8)
        self.assertEqual(MAX_CONTAINER_BYTES, PAYLOAD_MEMBER_COUNT * entry_limit(ArtifactKind.ARCHIVE))
        self.assertEqual(MAX_CONTAINER_BYTES, 2 * 1024 * MIB)
        worst_payload = sum(entry_limit(kind) for _path, kind, _os, _arch in expected_inventory(VERSION))
        self.assertGreater(MAX_CONTAINER_BYTES, worst_payload + MAX_MANIFEST_BYTES)

    def test_artifact_name_and_workflow_match_the_ci_upload(self):
        workflow = REPOSITORY_ROOT / CANDIDATE_WORKFLOW_PATH
        source = workflow.read_text()
        self.assertTrue(source.startswith("name: CI\n"))
        expression = "development-candidate-${{ github.run_id }}-${{ github.run_attempt }}"
        self.assertIn(f"          name: {expression}\n", source)
        rendered = expression.replace("${{ github.run_id }}", "901").replace("${{ github.run_attempt }}", "2")
        self.assertEqual(candidate_artifact_name(901, 2), rendered)


class ConsumerBoundaryTests(unittest.TestCase):
    """The admission limit and the real consumer's refusal agree at the same byte."""

    def setUp(self):
        self.descriptors = open_descriptors()
        self.addCleanup(lambda: self.assertEqual(open_descriptors(), self.descriptors))
        self.tree = FixtureTree.open(prefix="candidate-limits-test", label="candidate limits fixture")
        self.addCleanup(self.tree.dispose)
        self.root = self.tree.root

    def output_directory(self) -> Path:
        output = self.root / "output"
        output.mkdir()
        for index, (path, _kind, _os, _arch) in enumerate(expected_inventory(VERSION)):
            (output / path).write_bytes(b"file %d\n" % index)
        return output

    def build_context(self, output: Path) -> BuildContext:
        return BuildContext(
            request=BuildRequest(version=VERSION, commit="a" * 40, base_commit="c" * 40,
                                 repository="hishamkaram/delegation-layer"),
            receipt={}, environment={}, clone=self.root / "clone", work=self.root / "work",
            output=output, owner=DirectoryOwner(), goreleaser=self.root / "goreleaser")

    def install_request(self, **files: Path) -> CandidateInstallRequest:
        archive = files.pop("archive", None) or self.root / "archive.tar.gz"
        installer = self.root / "install.sh"
        return CandidateInstallRequest(
            tag="v" + VERSION, repo="hishamkaram/delegation-layer", installer=installer,
            files=CandidateFiles(archive=archive, **files), binaries=(), skill=())

    def test_package_reader_refuses_the_first_byte_the_manifest_refuses(self):
        at_limit = sparse(self.root / "at-limit.tgz", entry_limit(ArtifactKind.NPM_PACKAGE))
        past_limit = sparse(self.root / "past-limit.tgz", entry_limit(ArtifactKind.NPM_PACKAGE) + 1)
        self.assertEqual(len(read_package_bytes(at_limit)), 16 * MIB)
        self.assertIsNotNone(sized(ArtifactKind.NPM_PACKAGE, at_limit.stat().st_size))
        error = refused_code(self, read_package_bytes, past_limit)
        self.assertEqual((error.code, error.stage), ("npm_pack_too_large", "bounded-read"))
        self.assertEqual(refused_code(self, sized, ArtifactKind.NPM_PACKAGE,
                                      past_limit.stat().st_size).code, "candidate_artifact_invalid")

    def test_builder_observes_every_kind_up_to_its_limit_and_refuses_past_it(self):
        output = self.output_directory()
        context = self.build_context(output)
        files = observed_files(context)
        self.assertEqual(sorted(files), sorted(path for path, _kind, _os, _arch in expected_inventory(VERSION)))
        for path, kind in ((output / "checksums.txt", ArtifactKind.CHECKSUMS),
                           (output / "delegation-layer.rb", ArtifactKind.HOMEBREW_FORMULA),
                           (output / f"delegation-layer-{VERSION}.tgz", ArtifactKind.NPM_PACKAGE)):
            with self.subTest(kind=kind.value):
                path.unlink()
                sparse(path, entry_limit(kind))
                self.assertEqual(observed_files(context)[path.name][0], entry_limit(kind))
                path.unlink()
                sparse(path, entry_limit(kind) + 1)
                error = refused_code(self, observed_files, context)
                self.assertEqual((error.code, error.stage), ("candidate_output_unreadable", "bounded-read"))
                path.unlink()
                path.write_bytes(b"restored\n")

    def test_builder_still_admits_an_archive_up_to_the_archive_limit(self):
        output = self.output_directory()
        archive = output / next(path for path, kind, _os, _arch in expected_inventory(VERSION)
                                if kind is ArtifactKind.ARCHIVE)
        archive.unlink()
        sparse(archive, MAX_ARCHIVE)
        self.assertEqual(observed_files(self.build_context(output))[archive.name][0], MAX_ARCHIVE)
        archive.unlink()
        sparse(archive, MAX_ARCHIVE + 1)
        error = refused_code(self, observed_files, self.build_context(output))
        self.assertEqual((error.code, error.stage), ("candidate_output_unreadable", "bounded-read"))

    def test_installer_refuses_a_candidate_file_past_its_kind_limit_before_any_effect(self):
        archive = sparse(self.root / "archive.tar.gz", 1)
        sparse(self.root / "install.sh", 1)
        for role, kind in (("package", ArtifactKind.NPM_PACKAGE), ("checksums", ArtifactKind.CHECKSUMS),
                           ("formula", ArtifactKind.HOMEBREW_FORMULA)):
            with self.subTest(role=role):
                path = sparse(self.root / f"{role}.bin", entry_limit(kind) + 1)
                request = self.install_request(archive=archive, **{role: path})
                ports = Mock()
                probe = Mock(side_effect=AssertionError("bootstrap must not run"))
                error = refused_code(self, verify_candidate_install, request, ports=ports, probe=probe,
                                     receipt={}, layout=Mock(), info={}, channels=(CHANNEL_SHELL,))
                self.assertEqual((error.code, error.stage), ("candidate_file_invalid", "bounded-read"))
                probe.assert_not_called()
                self.assertEqual(ports.mock_calls, [])


def mutated(data: bytes, change) -> bytes:
    record = json.loads(data)
    change(record)
    return encode_record(record, "test")


class ManifestTests(unittest.TestCase):
    def setUp(self):
        self.manifest = manifest()
        self.data = encode_manifest(self.manifest)

    def assertCode(self, code, function, *args):
        with self.assertRaises(VerificationError) as caught:
            function(*args)
        self.assertEqual(caught.exception.code, code)

    def test_round_trip_owns_exact_inventory(self):
        self.assertEqual(parse_manifest(self.data), self.manifest)
        paths = [entry.path for entry in self.manifest.artifacts]
        self.assertEqual(paths, sorted([
            "checksums.txt", "delegation-layer-0.1.37.tgz", "delegation-layer.rb",
            "delegation-layer_0.1.37_Darwin_amd64.tar.gz", "delegation-layer_0.1.37_Darwin_arm64.tar.gz",
            "delegation-layer_0.1.37_Linux_amd64.tar.gz", "delegation-layer_0.1.37_Linux_arm64.tar.gz",
        ]))
        self.assertNotIn(hashlib.sha256(self.data).hexdigest(), self.data.decode())

    def test_non_canonical_and_duplicate_key_bytes_are_rejected(self):
        pretty = json.dumps(json.loads(self.data), indent=2, sort_keys=True).encode()
        self.assertCode("candidate_record_not_canonical", parse_manifest, pretty)
        duplicate = self.data.replace(b'{"artifacts"', b'{"artifacts":[],"artifacts"', 1)
        self.assertCode("candidate_record_invalid", parse_manifest, duplicate)
        for constant in (b"NaN", b"Infinity", b"-Infinity"):
            self.assertCode("candidate_record_invalid", parse_manifest, b'{"version":' + constant + b"}\n")
        self.assertCode("candidate_record_invalid", parse_manifest, b"\xff")
        self.assertCode("candidate_record_too_large", parse_manifest, b" " * (64 * 1024 + 1))

    def test_unsafe_artifact_paths_are_rejected(self):
        for path in ("../checksums.txt", "/checksums.txt", "dist\\checksums.txt", "./checksums.txt",
                     "dist//checksums.txt", "", "check\nsums.txt"):
            data = mutated(self.data, lambda record, path=path: record["artifacts"][0].update(path=path))
            self.assertCode("candidate_artifact_path_invalid", parse_manifest, data)

    def test_duplicate_missing_and_mislabeled_artifacts_are_rejected(self):
        data = mutated(self.data, lambda record: record["artifacts"].insert(1, record["artifacts"][0]))
        self.assertCode("candidate_artifact_duplicate", parse_manifest, data)
        data = mutated(self.data, lambda record: record["artifacts"].pop())
        self.assertCode("candidate_artifact_inventory_mismatch", parse_manifest, data)
        data = mutated(self.data, lambda record: record["artifacts"][0].update(arch="arm64"))
        self.assertCode("candidate_artifact_inventory_mismatch", parse_manifest, data)
        data = mutated(self.data, lambda record: record["artifacts"][0].update(path="delegation-layer-9.9.9.tgz"))
        self.assertCode("candidate_artifact_inventory_mismatch", parse_manifest, data)
        data = mutated(self.data, lambda record: record["artifacts"].reverse())
        self.assertCode("candidate_artifact_inventory_mismatch", parse_manifest, data)

    def test_artifact_digest_and_size_errors_are_rejected(self):
        for field, value in (("sha256", "A" * 64), ("sha256", "a" * 63), ("size", 0), ("size", True),
                             ("kind", "binary")):
            data = mutated(self.data, lambda record, f=field, v=value: record["artifacts"][0].update({f: v}))
            self.assertCode("candidate_artifact_invalid", parse_manifest, data)

    def test_identity_errors_are_rejected(self):
        cases = (
            ("candidate_build_tag_mismatch", lambda r: r.update(build_tag="v0.1.38")),
            ("candidate_field_invalid", lambda r: r.update(version="v0.1.37")),
            ("candidate_field_invalid", lambda r: r["source"].update(commit="A" * 40)),
            ("candidate_field_invalid", lambda r: r["source"].update(tree="short")),
            ("candidate_field_invalid", lambda r: r["build"].update(go_sum_sha256="0")),
            ("candidate_field_invalid", lambda r: r["build"]["toolchains"].update(go="go1.27.3")),
            ("candidate_record_fields_invalid", lambda r: r["build"]["toolchains"].pop("node")),
            ("candidate_record_fields_invalid", lambda r: r.update(self_sha256="0" * 64)),
            ("candidate_manifest_kind_invalid", lambda r: r.update(schema_version=1)),
        )
        for code, change in cases:
            self.assertCode(code, parse_manifest, mutated(self.data, change))



class DecoderBoundaryTests(unittest.TestCase):
    """Every candidate decoder shares one strict reader and header refusal."""

    def setUp(self):
        self.records = (
            (parse_manifest, encode_manifest(manifest()), "candidate_manifest_kind_invalid"),
        )

    def test_schema_version_must_be_exact_integer_two(self):
        for parse, data, code in self.records:
            self.assertEqual(json.loads(data)["schema_version"], 2)
            parse(data)
            for version in (True, 2.0, [2], {"version": 2}, "2", None, 0, 1):
                changed = mutated(data, lambda record, v=version: record.update(schema_version=v))
                with self.subTest(parse=parse.__name__, version=version):
                    with self.assertRaises(VerificationError) as caught:
                        parse(changed)
                    self.assertEqual(caught.exception.code, code)
            for kind in (["wrong"], {"kind": 1}, None):
                changed = mutated(data, lambda record, k=kind: record.update(kind=k))
                with self.subTest(parse=parse.__name__, kind=kind):
                    with self.assertRaises(VerificationError) as caught:
                        parse(changed)
                    self.assertEqual(caught.exception.code, code)

    def test_hostile_nesting_is_a_typed_refusal(self):
        # Deeper than the interpreter's JSON decoder recursion limit; each
        # decoder refuses with its size bound or the shared strict reader.
        depth = 10500
        for parse, _, _ in self.records:
            expected = "candidate_record_invalid"
            for nested in (b"[" * depth + b"]" * depth, b'{"a":' * depth + b"1" + b"}" * depth):
                with self.subTest(parse=parse.__name__, shape=nested[:1]):
                    with self.assertRaises(VerificationError) as caught:
                        parse(nested)
                    self.assertEqual(caught.exception.code, expected)

    def test_nested_unknown_field_is_refused_by_the_schema(self):
        for parse, data, _ in self.records:
            nested = mutated(data, lambda record: record.update(padding=[[[[1]]]]))
            with self.subTest(parse=parse.__name__):
                with self.assertRaises(VerificationError) as caught:
                    parse(nested)
                self.assertEqual(caught.exception.code, "candidate_record_fields_invalid")

    def test_brackets_inside_strings_are_ordinary_text(self):
        data = encode_manifest(manifest())
        quoted = mutated(data, lambda record: record["artifacts"][0].update(path='[[[[[[[[[\\"{{{{{{{{{'))
        with self.assertRaises(VerificationError) as caught:
            parse_manifest(quoted)
        self.assertEqual(caught.exception.code, "candidate_artifact_path_invalid")

    def test_unhashable_enum_values_are_typed_refusals(self):
        data = encode_manifest(manifest())
        with self.assertRaises(VerificationError) as caught:
            parse_manifest(mutated(data, lambda record: record["artifacts"][0].update(kind=["archive"])))
        self.assertEqual(caught.exception.code, "candidate_artifact_invalid")


if __name__ == "__main__":
    unittest.main()
