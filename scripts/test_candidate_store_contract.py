"""Structure, identity and append-only validity of the development candidate store."""

from __future__ import annotations

from dataclasses import FrozenInstanceError, fields
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.release_verification import candidate_store_contract as store
from scripts.delegate_acceptance.release_verification.candidate_history import (
    MAX_PUBLICATION_BYTES, FindingRef, PublicationAuthorization, encode_publication, parse_publication,
)
from scripts.delegate_acceptance.release_verification.candidate_install_contract import InstallChannel
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.published_contract import git_blob_sha
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    AppendAmbiguous, AppendConfirmed, AppendRefused, EntryFamily, EntryName, StoredBlob, StoreLocation,
    TransitionKind, TreeEntry, check_root, classify_transition, entry_name, git_tree_sha, parse_entry_name,
    record_blob, require_blob,
)


# Objects the GitHub API returned in the R7 genesis-marker run.
R7_MARKER_BYTES = b'{"kind":"temporary-store-marker","schema_version":1}\n'
R7_MARKER_BLOB = "d7efe8b8cf1795b66be6232ca9bada172763aab4"
R7_GENESIS_TREE = "7f0ed6f3e1df2191b21a1ed6b9467cf4d073883c"
# Blob contents and tree SHAs the GitHub API returned in the R3 path-history run.
R3_ONE, R3_OTHER = b"synthetic version one\n", b"unrelated candidate\n"
R3_SUBTREE_A = "9b36de21d0459ab59d6e3febbf0af94a1531db01"
# Computed by `git mktree` (git 2.43.0): blob "a.json", tree "a", blob "a0".
GIT_DIRECTORY_ORDER_TREE = "c014202395d83383446ede7ef0776a11efa63daf"
EMPTY_TREE = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
CANDIDATES_SHA = "c" * 40
DIGESTS = tuple(f"{index:064x}" for index in range(1, 1100))
LINUX, DARWIN = ("Linux", "amd64"), ("Darwin", "arm64")


def blob(name: str, sha: str, mode: str = "100644") -> TreeEntry:
    return TreeEntry(name, mode, "blob", sha)


def tree(name: str, sha: str) -> TreeEntry:
    return TreeEntry(name, "040000", "tree", sha)


def record(name: str, data: bytes | None = None) -> TreeEntry:
    return blob(name, git_blob_sha(data if data is not None else name.encode()))


def directory(*names: str) -> frozenset[TreeEntry]:
    return frozenset(record(name) for name in names)


class StoreCase(unittest.TestCase):
    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)


class GitObjectTests(StoreCase):
    def test_blob_and_tree_names_match_github_server_objects(self):
        self.assertEqual(git_blob_sha(R7_MARKER_BYTES), R7_MARKER_BLOB)
        self.assertEqual(git_tree_sha((blob("STORE.json", R7_MARKER_BLOB),)), R7_GENESIS_TREE)
        one, other = git_blob_sha(R3_ONE), git_blob_sha(R3_OTHER)
        marker = blob("fixture-marker", other)
        subtree_a = git_tree_sha((blob("manifest.json", one),))
        self.assertEqual(subtree_a, R3_SUBTREE_A)
        subtree_b = git_tree_sha((blob("manifest.json", other),))
        vectors = {
            "2527cc1adb08660cd7582f632eb9b9bd4b500b30": (marker,),
            "5d1983c147ff7da734ec3b7a443f12e0e73032ee": (
                tree("candidates", git_tree_sha((tree("a", subtree_a),))), marker),
            "67ebf61fbd9b591279f4e51cbcfd319e4888a7d7": (
                tree("candidates", git_tree_sha((tree("a", subtree_a), tree("b", subtree_b)))), marker),
            "24914ceca7af3de6b2d9baf929ce979d4b1a15ca": (tree("candidates", git_tree_sha((
                tree("a", git_tree_sha((blob("manifest.json", one, "100755"),))), tree("b", subtree_b)))), marker),
            "7f037a78aac40936d84e0324403bf790269bb324": (
                tree("candidates", git_tree_sha((tree("b", subtree_b),))), marker,
                tree("outside", git_tree_sha((blob("a.json", one),)))),
            "4982f908a5f84d6fe9d47fa98994e35fda0e05e1": (blob("candidates", other), marker),
        }
        for expected, entries in vectors.items():
            with self.subTest(expected=expected):
                self.assertEqual(git_tree_sha(entries), expected)
                self.assertEqual(git_tree_sha(tuple(reversed(entries))), expected)
        self.assertEqual(git_tree_sha(()), EMPTY_TREE)

    def test_a_directory_sorts_as_if_its_name_ended_in_a_slash(self):
        one = git_blob_sha(R3_ONE)
        entries = (tree("a", R3_SUBTREE_A), blob("a0", one), blob("a.json", one))
        self.assertEqual(git_tree_sha(entries), GIT_DIRECTORY_ORDER_TREE)

    def test_tree_entries_are_exact_git_entries(self):
        self.assertCode("store_tree_invalid", git_tree_sha, (blob("a", "1" * 40), blob("a", "2" * 40)))
        self.assertCode("store_tree_invalid", git_tree_sha, [blob("a", "1" * 40)])
        for name, mode, kind, sha in (
                ("a/b", "100644", "blob", "1" * 40), ("..", "100644", "blob", "1" * 40),
                ("", "100644", "blob", "1" * 40), ("a\0", "100644", "blob", "1" * 40),
                ("a", "100644", "tree", "1" * 40), ("a", "40000", "tree", "1" * 40),
                ("a", "100644", "blob", "A" * 40), ("a", "100644", "blob", "1" * 64)):
            with self.subTest(name=name, mode=mode, kind=kind, sha=sha):
                self.assertCode("store_tree_entry_invalid", TreeEntry, name, mode, kind, sha)


class RootTests(StoreCase):
    def test_root_is_exactly_the_marker_and_an_optional_candidates_directory(self):
        marker = blob("STORE.json", R7_MARKER_BLOB)
        self.assertIsNone(check_root((marker,), marker_blob=R7_MARKER_BLOB, truncated=False))
        self.assertEqual(check_root((tree("candidates", CANDIDATES_SHA), marker),
                                    marker_blob=R7_MARKER_BLOB, truncated=False), CANDIDATES_SHA)

    def test_a_root_that_could_fake_absence_is_refused(self):
        marker = blob("STORE.json", R7_MARKER_BLOB)
        refused = {
            "store_marker_mismatch": (
                (blob("STORE.json", "1" * 40),), (blob("STORE.json", R7_MARKER_BLOB, "100755"),),
                (tree("STORE.json", R7_MARKER_BLOB),), (), (tree("candidates", CANDIDATES_SHA),)),
            "store_root_invalid": (
                (marker, blob("candidates", CANDIDATES_SHA)),
                (marker, blob("candidates", CANDIDATES_SHA, "120000")),
                (marker, tree("Candidates", CANDIDATES_SHA)),
                (marker, tree("candidates", CANDIDATES_SHA), blob("README.md", "1" * 40)),
                (marker, blob("README.md", "1" * 40))),
            "store_tree_invalid": ((marker, marker), (marker, blob("STORE.json", "1" * 40))),
        }
        for code, roots in refused.items():
            for entries in roots:
                with self.subTest(code=code, entries=entries):
                    self.assertCode(code, check_root, entries, marker_blob=R7_MARKER_BLOB, truncated=False)
        for truncated in (True, None, 0):
            self.assertCode("store_tree_truncated", check_root, (marker,), marker_blob=R7_MARKER_BLOB,
                            truncated=truncated)
        self.assertCode("store_location_invalid", check_root, (marker,), marker_blob="1" * 64, truncated=False)


class NameTests(StoreCase):
    def test_known_names_round_trip(self):
        known = {
            "manifest.json": EntryName(EntryFamily.MANIFEST),
            "locator.json": EntryName(EntryFamily.LOCATOR),
            "publication.json": EntryName(EntryFamily.PUBLICATION),
            "attempt-0001.intent.json": EntryName(EntryFamily.INTENT, attempt=1),
            "attempt-0012.claim.Darwin-arm64.json": EntryName(EntryFamily.CLAIM, attempt=12, platform=DARWIN),
            "attempt-0003.proof.Linux-amd64.zip": EntryName(EntryFamily.PROOF, attempt=3, platform=LINUX),
            "attempt-10000.intent.json": EntryName(EntryFamily.INTENT, attempt=10000),
            "attempt-123456.proof.Linux-amd64.zip": EntryName(EntryFamily.PROOF, attempt=123456, platform=LINUX),
            f"triage.{DIGESTS[0]}.json": EntryName(EntryFamily.TRIAGE, digest=DIGESTS[0]),
            f"review.{DIGESTS[1]}.json": EntryName(EntryFamily.REVIEW, digest=DIGESTS[1]),
            f"closure.{DIGESTS[2]}.json": EntryName(EntryFamily.CLOSURE, digest=DIGESTS[2]),
            "install.Linux-amd64.shell.proof.json": EntryName(
                EntryFamily.INSTALL_PROOF, platform=LINUX, channel=InstallChannel.SHELL),
            "install.Darwin-arm64.brew.witness.json": EntryName(
                EntryFamily.INSTALL_WITNESS, platform=DARWIN, channel=InstallChannel.BREW),
        }
        for text, name in known.items():
            with self.subTest(text=text):
                self.assertEqual(parse_entry_name(text), name)
                self.assertEqual(entry_name(name), text)

    def test_every_platform_and_channel_has_one_name(self):
        platforms = (("Darwin", "amd64"), ("Darwin", "arm64"), ("Linux", "amd64"), ("Linux", "arm64"))
        rendered = set()
        for platform in platforms:
            for name in (EntryName(EntryFamily.CLAIM, attempt=2, platform=platform),
                         EntryName(EntryFamily.PROOF, attempt=2, platform=platform),
                         *(EntryName(family, platform=platform, channel=channel)
                           for family in (EntryFamily.INSTALL_PROOF, EntryFamily.INSTALL_WITNESS)
                           for channel in (InstallChannel.SHELL, InstallChannel.NPM, InstallChannel.BREW))):
                text = entry_name(name)
                self.assertEqual(parse_entry_name(text), name)
                rendered.add(text)
        self.assertEqual(len(rendered), 4 * 8)

    def test_unknown_unsafe_and_non_canonical_names_are_refused(self):
        for text in (
                "attempt-1.intent.json", "attempt-00001.intent.json", "attempt-0000.intent.json",
                "attempt--001.intent.json", "attempt-0001.claim.Windows-amd64.json",
                "attempt-0001.claim.Linux-386.json", "attempt-0001.claim.linux-amd64.json",
                "attempt-0001.proof.Linux-amd64.json", "attempt-0001.claim.Linux-amd64.zip",
                "install.Linux-amd64.pip.proof.json", "install.Linux-amd64.shell.json",
                "install.Linux-amd64.Shell.proof.json", "STORE.json", "../manifest.json", "manifest.json.bak",
                " manifest.json", "Manifest.json", f"triage.{'A' * 64}.json", f"closure.{'1' * 63}.json",
                f"review.{DIGESTS[0]}.zip", "publication.zip", "", None, b"manifest.json"):
            with self.subTest(text=text):
                self.assertCode("store_entry_name_invalid", parse_entry_name, text)
        for arguments in (dict(attempt=1), dict(attempt=True, platform=LINUX), dict(attempt=0, platform=LINUX),
                          dict(attempt=1, platform=("Windows", "amd64")),
                          dict(attempt=1, platform=LINUX, digest=DIGESTS[0])):
            with self.subTest(arguments=arguments):
                self.assertCode("store_entry_name_invalid", EntryName, EntryFamily.CLAIM, **arguments)


class BlobTests(StoreCase):
    def test_compact_records_and_proof_zips_use_their_owner_bounds(self):
        claim = "attempt-0001.claim.Linux-amd64.json"
        self.assertEqual(require_blob(record(claim, b"{}"), b"{}").family, EntryFamily.CLAIM)
        big_claim = b"x" * (4 * 1024 + 1)
        self.assertCode("store_blob_too_large", require_blob, record(claim, big_claim), big_claim)
        zip_name = "attempt-0001.proof.Linux-amd64.zip"
        generous = b"z" * (300 * 1024)
        self.assertEqual(require_blob(record(zip_name, generous), generous).family, EntryFamily.PROOF)
        oversized = b"z" * ((1 << 20) + 1)
        self.assertCode("store_blob_too_large", require_blob, record(zip_name, oversized), oversized)
        self.assertCode("store_blob_mismatch", require_blob, record(claim, b"{}"), b"{ }")

    def test_only_regular_non_executable_files_are_records(self):
        sha = git_blob_sha(b"{}")
        for entry in (blob("manifest.json", sha, "100755"), blob("manifest.json", sha, "120000"),
                      tree("manifest.json", sha)):
            with self.subTest(mode=entry.mode):
                self.assertCode("store_entry_invalid", require_blob, entry, b"{}")

    def test_record_blob_is_the_canonical_regular_file_of_its_entry_name(self):
        for name, data in ((EntryName(EntryFamily.CLAIM, attempt=1, platform=LINUX), b'{"claim":1}'),
                           (EntryName(EntryFamily.PROOF, attempt=12, platform=DARWIN), b"PK zip bytes"),
                           (EntryName(EntryFamily.MANIFEST), R7_MARKER_BYTES)):
            with self.subTest(name=entry_name(name)):
                stored = record_blob(name, data)
                self.assertEqual(stored, StoredBlob(TreeEntry(entry_name(name), "100644", "blob",
                                                              git_blob_sha(data)), data))
                self.assertEqual(parse_entry_name(stored.entry.name), name)
        self.assertEqual(record_blob(EntryName(EntryFamily.MANIFEST), R7_MARKER_BYTES).entry.sha, R7_MARKER_BLOB)

    def test_record_blob_keeps_the_family_bound_and_refuses_untyped_input(self):
        claim = EntryName(EntryFamily.CLAIM, attempt=1, platform=LINUX)
        self.assertCode("store_blob_too_large", record_blob, claim, b"x" * (4 * 1024 + 1))
        self.assertCode("store_entry_invalid", record_blob, entry_name(claim), b"{}")
        self.assertCode("store_entry_invalid", record_blob, claim, bytearray(b"{}"))


class PublicationBlobTests(StoreCase):
    """The publication family is bounded by its codec owner; admitted bytes are not authority."""

    NAME = "publication.json"

    @staticmethod
    def freeze_record() -> PublicationAuthorization:
        return PublicationAuthorization(
            candidate_id=DIGESTS[0], attempt=1, evidence_parent="1" * 40, source_commit="2" * 40,
            source_tree="3" * 40, proof_sha256s=(DIGESTS[1],), install_sha256s=(DIGESTS[2],),
            triage_sha256s=(), review_sha256=DIGESTS[3])

    def test_the_family_bound_is_the_codec_owners_bound(self):
        self.assertEqual(store.FAMILY_MAX_BYTES[EntryFamily.PUBLICATION], MAX_PUBLICATION_BYTES)
        self.assertEqual(set(store.FAMILY_MAX_BYTES), set(EntryFamily))

    def test_the_codecs_canonical_bytes_are_admitted(self):
        data = encode_publication(self.freeze_record())
        self.assertEqual(require_blob(record(self.NAME, data), data).family, EntryFamily.PUBLICATION)

    def test_the_exact_owner_limit_is_accepted_and_one_more_byte_is_refused(self):
        exact = b" " * MAX_PUBLICATION_BYTES
        self.assertEqual(require_blob(record(self.NAME, exact), exact).family, EntryFamily.PUBLICATION)
        over = exact + b" "
        self.assertCode("store_blob_too_large", require_blob, record(self.NAME, over), over)

    def test_digest_and_mode_refusals_still_apply(self):
        data = encode_publication(self.freeze_record())
        self.assertCode("store_blob_mismatch", require_blob, record(self.NAME, data), data + b" ")
        sha = git_blob_sha(data)
        for entry in (blob(self.NAME, sha, "100755"), blob(self.NAME, sha, "120000"), tree(self.NAME, sha)):
            with self.subTest(mode=entry.mode):
                self.assertCode("store_entry_invalid", require_blob, entry, data)

    def test_admission_is_structural_so_invalid_publication_bytes_stay_bounded_bytes(self):
        # Like every other family, the store checks identity and size only. The codec
        # owner alone decides whether the bytes are a freeze record, and the readiness
        # owner alone decides whether that record authorizes publication.
        for data in (b"{}", b"not json"):
            with self.subTest(data=data):
                self.assertEqual(require_blob(record(self.NAME, data), data).family, EntryFamily.PUBLICATION)
                with self.assertRaises(VerificationError):
                    parse_publication(data)


class TransitionTests(StoreCase):
    def classify(self, parent, *added):
        return classify_transition(parent, parent | directory(*added))

    def test_each_allowed_group_classifies(self):
        registered = directory("manifest.json", "locator.json")
        self.assertEqual(classify_transition(frozenset(), registered).kind, TransitionKind.REGISTER)
        triage = f"triage.{DIGESTS[0]}.json"
        closures = (f"closure.{DIGESTS[1]}.json", f"closure.{DIGESTS[2]}.json")
        groups = {
            TransitionKind.INTENT: ("attempt-0001.intent.json",),
            TransitionKind.CLAIM: ("attempt-0001.claim.Linux-amd64.json",),
            TransitionKind.PROOF: ("attempt-0001.proof.Linux-amd64.zip",),
            TransitionKind.REVIEW: (f"review.{DIGESTS[3]}.json",),
            TransitionKind.FREEZE: ("publication.json",),
            TransitionKind.INSTALL: ("install.Linux-amd64.npm.proof.json", "install.Linux-amd64.npm.witness.json"),
        }
        for kind, names in groups.items():
            with self.subTest(kind=kind):
                result = self.classify(registered, *names)
                self.assertEqual((result.kind, tuple(entry.name for entry in result.added)),
                                 (kind, tuple(sorted(names))))
        self.assertEqual(self.classify(registered, triage).kind, TransitionKind.TRIAGE)
        result = self.classify(registered, triage, *closures)
        self.assertEqual((result.kind, len(result.added)), (TransitionKind.TRIAGE, 3))

    def test_other_groups_are_refused(self):
        registered = directory("manifest.json", "locator.json")
        triage = f"triage.{DIGESTS[0]}.json"
        for names in (
                (), ("manifest.json",), ("attempt-0001.intent.json", "attempt-0002.intent.json"),
                ("attempt-0001.claim.Linux-amd64.json", "attempt-0001.proof.Linux-amd64.zip"),
                ("install.Linux-amd64.npm.proof.json",), ("install.Linux-amd64.npm.witness.json",),
                ("install.Linux-amd64.npm.proof.json", "install.Linux-amd64.shell.witness.json"),
                ("install.Linux-amd64.npm.proof.json", "install.Darwin-arm64.npm.witness.json"),
                (f"closure.{DIGESTS[1]}.json",), (triage, f"triage.{DIGESTS[1]}.json"),
                (triage, f"review.{DIGESTS[1]}.json"), ("publication.json", f"review.{DIGESTS[1]}.json")):
            with self.subTest(names=names):
                self.assertCode("store_transition_invalid", self.classify, registered, *names)
        self.assertCode("store_transition_invalid", classify_transition, frozenset(), directory("manifest.json"))

    def test_existing_entries_never_change(self):
        parent = directory("manifest.json", "locator.json", "attempt-0001.intent.json")
        intent = record("attempt-0002.intent.json")
        rewritten = (parent - directory("attempt-0001.intent.json")) | {record("attempt-0001.intent.json", b"x")}
        deleted = parent - directory("locator.json")
        renamed = (parent - directory("attempt-0001.intent.json")) | directory("attempt-0003.intent.json")
        for child in (rewritten | {intent}, deleted | {intent}, renamed):
            with self.subTest(child=sorted(entry.name for entry in child)):
                self.assertCode("store_entry_rewritten", classify_transition, parent, child)
        moded = (parent - directory("locator.json")) | {blob("locator.json", git_blob_sha(b"locator.json"), "100755")}
        self.assertCode("store_entry_invalid", classify_transition, parent, moded | {intent})
        duplicate = parent | {intent, record("attempt-0002.intent.json", b"other")}
        self.assertCode("store_candidate_tree_invalid", classify_transition, parent, duplicate)
        self.assertCode("store_entry_name_invalid", classify_transition, parent, parent | directory("notes.txt"))
        self.assertCode("store_candidate_tree_invalid", classify_transition, tuple(parent), parent | {intent})

    def test_one_candidate_directory_holds_at_most_1024_entries(self):
        base = directory("manifest.json", "locator.json")
        closures = tuple(f"closure.{digest}.json" for digest in DIGESTS[1:1022])
        full = self.classify(base, f"triage.{DIGESTS[0]}.json", *closures)
        self.assertEqual(len(full.added), 1022)
        parent = base | directory(f"triage.{DIGESTS[0]}.json", *closures)
        self.assertEqual(len(parent), 1024)
        self.assertCode("store_candidate_tree_invalid", classify_transition, parent,
                        parent | directory("attempt-0001.intent.json"))


class LocationAndOutcomeTests(StoreCase):
    def test_store_location_is_exact(self):
        location = StoreLocation("hishamkaram/delegation-layer", "development/candidates", "a" * 40, R7_MARKER_BLOB)
        self.assertEqual(location.marker_blob, R7_MARKER_BLOB)
        # `git check-ref-format --branch` accepts these and refuses any component ending in ".lock".
        for branch in ("development/candidates/store", "topic.locked/child", "a/b.c/d"):
            self.assertEqual(StoreLocation("owner/name", branch, "a" * 40, R7_MARKER_BLOB).branch, branch)
        for branch in ("topic.lock/child", "topic/inside.lock/child"):
            self.assertCode("store_location_invalid", StoreLocation, "owner/name", branch, "a" * 40, R7_MARKER_BLOB)
        for repository, branch, genesis, marker in (
                ("delegation-layer", "store", "a" * 40, R7_MARKER_BLOB),
                ("owner/name", "a..b", "a" * 40, R7_MARKER_BLOB), ("owner/name", "store.lock", "a" * 40, R7_MARKER_BLOB),
                ("owner/name", "/store", "a" * 40, R7_MARKER_BLOB), ("owner/name", "store/", "a" * 40, R7_MARKER_BLOB),
                ("owner/name", "a//b", "a" * 40, R7_MARKER_BLOB), ("owner/name", ".store", "a" * 40, R7_MARKER_BLOB),
                ("owner/name", "store", "a" * 64, R7_MARKER_BLOB), ("owner/name", "store", "a" * 40, "")):
            with self.subTest(repository=repository, branch=branch):
                self.assertCode("store_location_invalid", StoreLocation, repository, branch, genesis, marker)

    def test_append_outcomes_are_immutable_facts_without_launch_authority(self):
        records = (DIGESTS[0], DIGESTS[1])
        outcomes = (
            AppendConfirmed("1" * 40, "2" * 40, TransitionKind.CLAIM, records),
            AppendRefused("claim_exists", (FindingRef(code="claim_exists", subject="Linux/amd64"),)),
            AppendAmbiguous("1" * 40, "2" * 40, "f" * 64, records, "ref_update_unconfirmed"),
        )
        expected_fields = (
            {"parent", "commit", "kind", "record_sha256s"}, {"reason", "findings"},
            {"parent", "proposed", "nonce", "record_sha256s", "failure"})
        for outcome, names in zip(outcomes, expected_fields):
            with self.subTest(outcome=type(outcome).__name__):
                self.assertEqual({field.name for field in fields(outcome)}, names)
                with self.assertRaises(FrozenInstanceError):
                    outcome.parent = "3" * 40
        for build in (
                lambda: AppendConfirmed("1" * 40, "1" * 40, TransitionKind.CLAIM, records),
                lambda: AppendConfirmed("1" * 40, "2" * 40, TransitionKind.CLAIM, tuple(reversed(records))),
                lambda: AppendConfirmed("1" * 40, "2" * 40, TransitionKind.CLAIM, ()),
                lambda: AppendConfirmed("1" * 40, "2" * 40, "claim", records),
                lambda: AppendRefused("Claim exists", ()),
                lambda: AppendRefused("claim_exists", ("claim_exists",)),
                lambda: AppendAmbiguous("1" * 40, "2" * 40, "f" * 63, records, "ref_update_unconfirmed"),
                lambda: AppendAmbiguous("1" * 40, "2" * 40, "f" * 64, records, "")):
            self.assertCode("store_append_outcome_invalid", build)
        self.assertEqual(store.MAX_CANDIDATE_COMMITS, 512)


if __name__ == "__main__":
    unittest.main()
