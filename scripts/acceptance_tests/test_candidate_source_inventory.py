"""Canonical changed-path and registry encodings bind exactly the observed source facts."""

from __future__ import annotations

import json
import unittest

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.candidate_attempts import (
    MatrixScope, ReviewedChangeScope,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import SourceIdentity
from scripts.delegate_acceptance.release_verification.candidate_source_inventory import (
    MAX_REGISTRY_FILES, REGISTRY_PATH, ChangedPathInventory, ObservedSourceChange, RegistryFile,
    RegistryFingerprint, TreeEntry, changed_path_inventory, registry_fingerprint, registry_module_files,
    reviewed_change_scope, tree_entries,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError


FRAGMENT_A = "agents/_data/subsystems/a.json"
FRAGMENT_B = "agents/_data/subsystems/b.json"


def master(*files: object, version: object = 1) -> bytes:
    return json.dumps({"version": version, "module": "fixture", "module_files": list(files)}).encode()


class InventoryCase(unittest.TestCase):
    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)


class ChangedPathTests(InventoryCase):
    def test_inventory_keeps_exact_names_in_byte_order(self):
        names = ["z.txt", "new\nline", " lead", "trail ", "B.txt", "tab\there", "dir/café", "a b/c"]
        output = b"".join(name.encode() + b"\0" for name in names)
        inventory = changed_path_inventory(output)
        expected = sorted(name.encode() for name in names)
        self.assertEqual(inventory.paths, tuple(name.decode() for name in expected))
        self.assertEqual(inventory.encoded, b"".join(name + b"\0" for name in expected))
        self.assertEqual(inventory.count, len(names))
        self.assertEqual(inventory.sha256, sha(inventory.encoded))
        self.assertIn(" lead", inventory.paths)
        self.assertIn("trail ", inventory.paths)
        self.assertIn("new\nline", inventory.paths)

    def test_empty_diff_is_the_empty_encoding(self):
        inventory = changed_path_inventory(b"")
        self.assertEqual((inventory.paths, inventory.encoded, inventory.count), ((), b"", 0))
        self.assertEqual(inventory.sha256, sha(b""))

    def test_malformed_git_output_is_refused_not_repaired(self):
        cases = (
            ("git_output_invalid", b"a.txt"),
            ("git_output_invalid", b"a.txt\0a.txt\0"),
            ("git_output_invalid", b"a.txt\0\0"),
            ("git_output_invalid", b"a/../b\0"),
            ("git_output_invalid", b"/abs\0"),
            ("git_output_invalid", b"a//b\0"),
            ("source_path_not_utf8", b"bad\xff\0"),
        )
        for code, output in cases:
            with self.subTest(output=output):
                self.assertCode(code, changed_path_inventory, output)

    def test_inventory_value_requires_sorted_unique_canonical_paths(self):
        for paths in (("b", "a"), ("a", "a"), ["a"], ("",), ("a/./b",), ("\ud800",), ("a\0b",)):
            with self.subTest(paths=paths):
                self.assertCode("source_changed_paths_invalid", ChangedPathInventory, paths=paths)


class RegistryTests(InventoryCase):
    def test_module_files_are_read_from_the_supported_version(self):
        self.assertEqual(registry_module_files(master(FRAGMENT_B, FRAGMENT_A)), (FRAGMENT_B, FRAGMENT_A))
        self.assertEqual(registry_module_files(master()), ())

    def test_registry_master_refusals(self):
        many = [f"agents/_data/subsystems/{index}.json" for index in range(MAX_REGISTRY_FILES + 1)]
        cases = (
            ("registry_master_invalid", b"{"),
            ("registry_master_invalid", b'{"version": 1, "version": 1, "module_files": []}'),
            ("registry_master_invalid", b"[]"),
            ("registry_version_unsupported", master(version=2)),
            ("registry_version_unsupported", master(version=True)),
            ("registry_version_unsupported", master(version="1")),
            ("registry_version_unsupported", b'{"module_files": []}'),
            ("registry_module_files_invalid", b'{"version": 1}'),
            ("registry_module_files_invalid", b'{"version": 1, "module_files": "a.json"}'),
            ("registry_module_files_invalid", master(*many)),
            ("registry_fragment_path_invalid", master("../escape.json")),
            ("registry_fragment_path_invalid", master("/abs.json")),
            ("registry_fragment_path_invalid", master("agents/./a.json")),
            ("registry_fragment_path_invalid", master("agents//a.json")),
            ("registry_fragment_path_invalid", master("agents/")),
            ("registry_fragment_path_invalid", master("")),
            ("registry_fragment_path_invalid", master(3)),
            ("registry_fragment_duplicate", master(FRAGMENT_A, FRAGMENT_A)),
            ("registry_fragment_duplicate", master(REGISTRY_PATH)),
        )
        for code, data in cases:
            with self.subTest(code=code, data=data[:60]):
                self.assertCode(code, registry_module_files, data)

    def test_fingerprint_encodes_every_registry_file_path_and_blob_digest(self):
        blobs = {FRAGMENT_B: b"[b]\n", REGISTRY_PATH: master(FRAGMENT_A, FRAGMENT_B), FRAGMENT_A: b"[a]\n"}
        fingerprint = registry_fingerprint(blobs)
        expected = b"".join(path.encode() + b"\0" + sha(blobs[path]).encode() + b"\0"
                            for path in sorted(blobs, key=str.encode))
        self.assertEqual(fingerprint.encoded, expected)
        self.assertEqual(fingerprint.sha256, sha(expected))
        # "." (0x2e) sorts before "/" (0x2f), so the master precedes its fragment directory.
        self.assertEqual([item.path for item in fingerprint.files], [REGISTRY_PATH, FRAGMENT_A, FRAGMENT_B])

    def test_fragment_change_alone_changes_the_fingerprint(self):
        blobs = {REGISTRY_PATH: master(FRAGMENT_A), FRAGMENT_A: b"[a]\n"}
        original = registry_fingerprint(blobs).sha256
        self.assertNotEqual(registry_fingerprint({**blobs, FRAGMENT_A: b"[a ]\n"}).sha256, original)
        renamed = {REGISTRY_PATH: blobs[REGISTRY_PATH], FRAGMENT_B: blobs[FRAGMENT_A]}
        self.assertNotEqual(registry_fingerprint(renamed).sha256, original)

    def test_fingerprint_value_requires_the_master_and_sorted_unique_files(self):
        digest = "1" * 64
        master_file = RegistryFile(path=REGISTRY_PATH, sha256=digest)
        fragment = RegistryFile(path=FRAGMENT_A, sha256=digest)
        for files in ((), (fragment,), (fragment, master_file), (master_file, fragment, fragment),
                      [master_file, fragment], (master_file, "file")):
            with self.subTest(files=files):
                self.assertCode("registry_fingerprint_invalid", RegistryFingerprint, files=files)
        self.assertCode("registry_fragment_path_invalid", RegistryFile, path="../a.json", sha256=digest)
        self.assertCode("candidate_field_invalid", RegistryFile, path=FRAGMENT_A, sha256="A" * 64)


class TreeEntryTests(InventoryCase):
    def test_entries_decode_exact_paths_and_modes(self):
        oid = "a" * 40
        output = (f"100644 blob {oid}\t{FRAGMENT_A}\x00100755 blob {oid}\tbin/tool\0"
                  f"120000 blob {oid}\tlink.json\x00040000 tree {oid}\tdir\x00160000 commit {oid}\tmodule\0"
                  f"100644 blob {oid}\tname\nwith newline\0").encode()
        entries = tree_entries(output)
        self.assertEqual(entries[FRAGMENT_A], TreeEntry(mode="100644", kind="blob", object_id=oid))
        self.assertEqual({path for path, entry in entries.items() if entry.regular_blob},
                         {FRAGMENT_A, "bin/tool", "name\nwith newline"})
        self.assertEqual(tree_entries(b""), {})

    def test_malformed_listing_is_refused(self):
        oid = "a" * 40
        for output, code in ((f"100644 blob {oid}\ta".encode(), "git_output_invalid"),
                             (f"100644 blob {oid}\ta\x00100644 blob {oid}\ta\0".encode(), "git_output_invalid"),
                             (f"100644 blob {oid[:39]}\ta\0".encode(), "git_output_invalid"),
                             (f"10064 blob {oid}\ta\0".encode(), "git_output_invalid"),
                             (f"100644 blob {oid}\tbad".encode() + b"\xff\0", "source_path_not_utf8")):
            with self.subTest(output=output):
                self.assertCode(code, tree_entries, output)


class ReviewedScopeTests(InventoryCase):
    def setUp(self):
        self.source = SourceIdentity(commit=fx.COMMIT, tree=fx.TREE, base_commit=fx.BASE)
        self.observed = ObservedSourceChange(
            source=self.source, base_tree=fx.BASE_TREE,
            changed_paths=changed_path_inventory(b"a.txt\0b.txt\0"),
            registry=registry_fingerprint({REGISTRY_PATH: master()}))

    def test_reviewed_scope_binds_observed_facts_and_reviewer_classification(self):
        change = fx.scope(MatrixScope.AFFECTED_PROVIDERS, ("opencode",))
        reviewed = reviewed_change_scope(self.observed, scope=change, rationale=fx.RATIONALE)
        self.assertEqual(reviewed, ReviewedChangeScope(
            scope=change, base_commit=fx.BASE, base_tree=fx.BASE_TREE, changed_path_count=2,
            changed_paths_sha256=sha(b"a.txt\0b.txt\0"),
            subsystem_registry_sha256=self.observed.registry.sha256, rationale=fx.RATIONALE))

    def test_reviewed_scope_refuses_unobserved_or_unreviewed_inputs(self):
        self.assertCode("source_observation_invalid", reviewed_change_scope, None, scope=fx.scope(),
                        rationale=fx.RATIONALE)
        self.assertCode("candidate_review_invalid", reviewed_change_scope, self.observed, scope=None,
                        rationale=fx.RATIONALE)
        self.assertCode("candidate_review_invalid", reviewed_change_scope, self.observed, scope=fx.scope(),
                        rationale="")
        for field, value in (("source", None), ("changed_paths", ("a.txt",)), ("registry", "f" * 64)):
            with self.subTest(field=field):
                fields = {"source": self.source, "base_tree": fx.BASE_TREE,
                          "changed_paths": self.observed.changed_paths, "registry": self.observed.registry}
                self.assertCode("source_observation_invalid", ObservedSourceChange, **{**fields, field: value})
        self.assertCode("candidate_field_invalid", ObservedSourceChange, source=self.source, base_tree="x",
                        changed_paths=self.observed.changed_paths, registry=self.observed.registry)


if __name__ == "__main__":
    unittest.main()
