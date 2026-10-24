"""Publication reference codec, truthful promotion statement and per-destination delivery decisions."""

from __future__ import annotations

from dataclasses import FrozenInstanceError, replace
import json
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.candidate_history import (
    PublicationAuthorization, encode_publication,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    MANIFEST_PATH, ArtifactEntry, BuildInputs, CandidateLocator, CandidateManifest, SourceIdentity,
    Toolchains, encode_manifest, expected_inventory, npm_package_path, payload_paths,
)
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import (
    MAX_REFERENCE_BYTES, Absent, CandidatePromotion, CandidateReference, Delivery, ObservedRelease,
    VERIFICATION_SUMMARY, ReleaseAsset, TapFormula, decide_npm, decide_release, decide_tap, encode_promotion,
    encode_reference, npm_package, parse_reference, release_body, release_body_matches, release_text,
    require_latest_delivery, verification_block,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness import (
    BoundCandidate, PublicationSource, publication_source,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.published_contract import (
    PackageIdentity, git_blob_sha, npm_integrity, npm_shasum, parse_npm_view, registry_tarball_url,
)


VERSION = "0.1.37"
TAG = "v" + VERSION
BUILD = "a1" * 20
MERGED = "e5" * 20
BASE = "c3" * 20
TREE = "b2" * 20
FREEZE_COMMIT = "f7" * 20
TGZ = npm_package_path(VERSION)
FORMULA = "delegation-layer.rb"


def payload_files(**overrides: bytes) -> dict[str, bytes]:
    """Distinct bytes for each of the seven inventory files."""
    files = {path: f"{path} candidate bytes\n".encode() for path, *_ in expected_inventory(VERSION)}
    files.update(overrides)
    return files


def manifest_for(files: dict[str, bytes]) -> CandidateManifest:
    inputs = BuildInputs(harness_commit=BUILD, harness_sha256="3" * 64, goreleaser_config_sha256="1" * 64,
                         go_sum_sha256="2" * 64,
                         toolchains=Toolchains(go="1.27.3", goreleaser="2.18.1", python="3.12.13",
                                               node="22.11.0"))
    artifacts = tuple(ArtifactEntry(path=path, kind=kind, os=os_name, arch=arch, size=len(files[path]),
                                    sha256=sha(files[path]))
                      for path, kind, os_name, arch in expected_inventory(VERSION))
    return CandidateManifest(version=VERSION, build_tag=TAG,
                             source=SourceIdentity(commit=BUILD, tree=TREE, base_commit=BASE),
                             build=inputs, artifacts=artifacts)


def bound(files: dict[str, bytes]) -> BoundCandidate:
    manifest = manifest_for(files)
    locator = CandidateLocator(repository="owner/delegation-layer", run_id=39714439980, run_attempt=2,
                               artifact_id=12399736217, artifact_archive_sha256="9" * 64,
                               manifest_sha256=sha(encode_manifest(manifest)))
    return BoundCandidate(locator=locator, manifest=manifest)


def freeze_for(candidate: BoundCandidate, **changes: object) -> PublicationAuthorization:
    values: dict[str, object] = {
        "candidate_id": candidate.candidate_id, "attempt": 1, "evidence_parent": "d4" * 20,
        "source_commit": BUILD, "source_tree": TREE, "proof_sha256s": ("5" * 64,),
        "install_sha256s": ("6" * 64,), "triage_sha256s": (), "review_sha256": "7" * 64, **changes}
    return PublicationAuthorization(**values)


def promotion_for(files: dict[str, bytes] | None = None) -> CandidatePromotion:
    candidate = bound(payload_files() if files is None else files)
    return CandidatePromotion(
        reference=CandidateReference(candidate_id=candidate.candidate_id, freeze_commit=FREEZE_COMMIT,
                                     merged_commit=MERGED),
        candidate=candidate, freeze=freeze_for(candidate),
        source=publication_source(manifest=candidate.manifest, merged_commit=MERGED, merged_tree=TREE))


def release(*names: str, draft: bool = True, digests: dict[str, str] | None = None,
            tag: str = TAG, target: str = MERGED, prerelease: bool = False,
            body: str | None = None) -> ObservedRelease:
    files = payload_files()
    expected = {name: sha(data) for name, data in files.items()}
    expected[MANIFEST_PATH] = sha(encode_manifest(manifest_for(files)))
    expected.update(digests or {})
    return ObservedRelease(tag=tag, target_commit=target, draft=draft, prerelease=prerelease,
                           assets=tuple(ReleaseAsset(name=name, sha256=expected[name]) for name in names),
                           body=release_body(promotion_for(files)) if body is None else body)


# The public v0.1.37 release body as published, then after its maintainers added readable notes;
# their SHA-256 digests are the archived page readback's and the edit verification's ``notes_sha256``.
V37_CANONICAL = (
    "Development candidate promotion\n"
    "\n"
    "Promotion predicate (canonical JSON):\n"
    "{\"candidate_id\":\"46c6ef444a1a00ef03ee6a4979c8a6e05d01524f1c3004759d070eab70ca2fc6\",\"freeze\":{\"co"
    "mmit\":\"374ecfa90f9e2e312f11901c635ae836c092e217\",\"record_sha256\":\"60a59852ca236f095f04569a25a737"
    "d028fec6b4031f7b129300966c5206913c\"},\"kind\":\"delegation-layer-candidate-promotion\",\"locator\":{\"a"
    "rtifact_archive_sha256\":\"3f526432ae337c2a73578c5f0175d12bb69b0935af0e954b0bba6d6c14319319\",\"arti"
    "fact_id\":12556412583,\"manifest_sha256\":\"46c6ef444a1a00ef03ee6a4979c8a6e05d01524f1c3004759d070eab"
    "70ca2fc6\",\"repository\":\"hishamkaram/delegation-layer\",\"run_attempt\":1,\"run_id\":40084104373},\"mer"
    "ged\":{\"commit\":\"2bf74534e51cee82cc8b6de6df03032e5eabbcce\",\"tree\":\"304b0f9a70e129703f5a7f4e486ec7"
    "554860bcc6\"},\"original_build\":{\"base_commit\":\"95277e06024fce0135af6bb8b9d3e722837effb3\",\"commit\""
    ":\"4391ad3dfbd9a33d1562ca8b0c358b487972ab85\",\"tree\":\"304b0f9a70e129703f5a7f4e486ec7554860bcc6\"},\""
    "schema_version\":1}\n"
    "\n"
    "Payload SHA-256 digests:\n"
    "46c6ef444a1a00ef03ee6a4979c8a6e05d01524f1c3004759d070eab70ca2fc6  candidate-manifest.json\n"
    "4b26d7e43b37f591a7b730fba81cd0636aec8648c216e652b9a90cb3fc8d2d47  checksums.txt\n"
    "b490041f008ec5d0f0fb1f9a09cb38fb8d16eb6e936c43e46a0da2b863e97294  delegation-layer-0.1.37.tgz\n"
    "f18ca9dfdef4cb322e52b75d59d2556a36df99145125415e9cb034f9935dfea6  delegation-layer.rb\n"
    "f9ac83009d936c21d0af94fa57689e53dcd22ea1855d71bf3d075eed8ac49084  delegation-layer_0.1.37_Darwin"
    "_amd64.tar.gz\n"
    "6f3c9fea154bb8d7a4df2440b63fe5152ef6b01913572cad25d5928dbf79f9f2  delegation-layer_0.1.37_Darwin"
    "_arm64.tar.gz\n"
    "617e7733125b927a1d7f8151d72de49997c7baad24dce5deff01ae69e7a952f4  delegation-layer_0.1.37_Linux_"
    "amd64.tar.gz\n"
    "1ef5197aec80a21e143a42add23547d86bfa0170b3a9e6ca72d20884a7311e5d  delegation-layer_0.1.37_Linux_"
    "arm64.tar.gz"
)


V37_PUBLIC = (
    "This maintenance release improves release verification, installation checks, and agent guidance."
    "\n"
    "\n"
    "## What changed\n"
    "\n"
    "- **Verify before publishing:** build an immutable candidate and test it during development, the"
    "n publish those same verified files.\n"
    "- **Test real installations:** shell, npm, and Homebrew checks now run a task through the instal"
    "led CLI, runner, and bundled supervisor on Linux and macOS.\n"
    "- **Require development readiness:** source-bound test, installation, and review evidence is che"
    "cked before merging and publishing.\n"
    "- **Recover partial publications safely:** confirm existing GitHub, npm, and Homebrew content an"
    "d complete only the missing delivery steps.\n"
    "- **Clearer agent and contributor guidance:** distinguish setup blockers, test results, evidence"
    ", and review approval, with focused skill evaluation cases.\n"
    "- **Read-only evidence checks:** verify handoff indexes against their recorded source and files "
    "without rewriting them.\n"
    "\n"
    "Implemented in [#101](https://github.com/hishamkaram/delegation-layer/pull/101).\n"
    "\n"
    "**Full changelog:** [v0.1.36…v0.1.37](https://github.com/hishamkaram/delegation-layer/compare/v0"
    ".1.36...v0.1.37)\n"
    "\n"
    "<details>\n"
    "<summary>Build verification details</summary>\n"
    "\n"
    "```text\n"
    "Development candidate promotion\n"
    "\n"
    "Promotion predicate (canonical JSON):\n"
    "{\"candidate_id\":\"46c6ef444a1a00ef03ee6a4979c8a6e05d01524f1c3004759d070eab70ca2fc6\",\"freeze\":{\"co"
    "mmit\":\"374ecfa90f9e2e312f11901c635ae836c092e217\",\"record_sha256\":\"60a59852ca236f095f04569a25a737"
    "d028fec6b4031f7b129300966c5206913c\"},\"kind\":\"delegation-layer-candidate-promotion\",\"locator\":{\"a"
    "rtifact_archive_sha256\":\"3f526432ae337c2a73578c5f0175d12bb69b0935af0e954b0bba6d6c14319319\",\"arti"
    "fact_id\":12556412583,\"manifest_sha256\":\"46c6ef444a1a00ef03ee6a4979c8a6e05d01524f1c3004759d070eab"
    "70ca2fc6\",\"repository\":\"hishamkaram/delegation-layer\",\"run_attempt\":1,\"run_id\":40084104373},\"mer"
    "ged\":{\"commit\":\"2bf74534e51cee82cc8b6de6df03032e5eabbcce\",\"tree\":\"304b0f9a70e129703f5a7f4e486ec7"
    "554860bcc6\"},\"original_build\":{\"base_commit\":\"95277e06024fce0135af6bb8b9d3e722837effb3\",\"commit\""
    ":\"4391ad3dfbd9a33d1562ca8b0c358b487972ab85\",\"tree\":\"304b0f9a70e129703f5a7f4e486ec7554860bcc6\"},\""
    "schema_version\":1}\n"
    "\n"
    "Payload SHA-256 digests:\n"
    "46c6ef444a1a00ef03ee6a4979c8a6e05d01524f1c3004759d070eab70ca2fc6  candidate-manifest.json\n"
    "4b26d7e43b37f591a7b730fba81cd0636aec8648c216e652b9a90cb3fc8d2d47  checksums.txt\n"
    "b490041f008ec5d0f0fb1f9a09cb38fb8d16eb6e936c43e46a0da2b863e97294  delegation-layer-0.1.37.tgz\n"
    "f18ca9dfdef4cb322e52b75d59d2556a36df99145125415e9cb034f9935dfea6  delegation-layer.rb\n"
    "f9ac83009d936c21d0af94fa57689e53dcd22ea1855d71bf3d075eed8ac49084  delegation-layer_0.1.37_Darwin"
    "_amd64.tar.gz\n"
    "6f3c9fea154bb8d7a4df2440b63fe5152ef6b01913572cad25d5928dbf79f9f2  delegation-layer_0.1.37_Darwin"
    "_arm64.tar.gz\n"
    "617e7733125b927a1d7f8151d72de49997c7baad24dce5deff01ae69e7a952f4  delegation-layer_0.1.37_Linux_"
    "amd64.tar.gz\n"
    "1ef5197aec80a21e143a42add23547d86bfa0170b3a9e6ca72d20884a7311e5d  delegation-layer_0.1.37_Linux_"
    "arm64.tar.gz\n"
    "```\n"
    "\n"
    "</details>\n"
)


V37_CANONICAL_SHA256 = "c6d20f56976dc850947c2c036628447f5e42191bceb1e793667f19154c8dbae3"


V37_PUBLIC_SHA256 = "dbe1b0211b25f11aaac6362fae02cf576ddeff9e64391d16fbc900f057e0f6b9"


class ContractCase(unittest.TestCase):
    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)


class ReferenceCodecTests(ContractCase):
    REFERENCE = CandidateReference(candidate_id="0a" * 32, freeze_commit=FREEZE_COMMIT, merged_commit=MERGED)

    def test_encoding_is_exactly_the_three_sorted_fields_and_round_trips(self):
        data = encode_reference(self.REFERENCE)
        self.assertEqual(data, (f'{{"candidate_id":"{"0a" * 32}","freeze_commit":"{FREEZE_COMMIT}",'
                                f'"merged_commit":"{MERGED}"}}\n').encode())
        self.assertEqual(parse_reference(data), self.REFERENCE)

    def test_the_largest_valid_reference_fits_the_decoding_bound(self):
        widest = CandidateReference(candidate_id="1" * 64, freeze_commit="2" * 64, merged_commit="3" * 64)
        data = encode_reference(widest)
        self.assertLessEqual(len(data), MAX_REFERENCE_BYTES)
        self.assertEqual(parse_reference(data), widest)

    def test_extra_or_missing_fields_are_refused(self):
        record = json.loads(encode_reference(self.REFERENCE))
        extra = {**record, "native_validation_run_id": "1"}
        missing = {key: value for key, value in record.items() if key != "freeze_commit"}
        for value in (extra, missing):
            data = (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()
            self.assertCode("candidate_record_fields_invalid", parse_reference, data)

    def test_malformed_field_values_are_refused(self):
        record = json.loads(encode_reference(self.REFERENCE))
        for key, value in (("candidate_id", "0A" * 32), ("candidate_id", "0a" * 20),
                           ("freeze_commit", "f7" * 19 + "f"), ("merged_commit", "main"),
                           ("merged_commit", 1), ("freeze_commit", None)):
            data = (json.dumps({**record, key: value}, sort_keys=True, separators=(",", ":")) + "\n").encode()
            with self.subTest(key=key, value=value):
                self.assertCode("candidate_field_invalid", parse_reference, data)

    def test_non_canonical_duplicate_and_oversized_input_is_refused(self):
        data = encode_reference(self.REFERENCE)
        self.assertCode("candidate_record_not_canonical", parse_reference, data.rstrip(b"\n"))
        self.assertCode("candidate_record_not_canonical", parse_reference, data.replace(b",", b", ", 1))
        self.assertCode("candidate_record_not_canonical", parse_reference, b"[]\n")
        duplicate = data.replace(b"{", b'{"merged_commit":"x",', 1)
        self.assertLessEqual(len(duplicate), MAX_REFERENCE_BYTES)
        self.assertCode("candidate_record_invalid", parse_reference, duplicate)
        oversized = data[:-2] + b',"pad":"' + b"x" * MAX_REFERENCE_BYTES + b'"}\n'
        self.assertCode("candidate_record_too_large", parse_reference, oversized)

    def test_reference_is_immutable(self):
        with self.assertRaises(FrozenInstanceError):
            setattr(self.REFERENCE, "merged_commit", BUILD)


class PromotionTests(ContractCase):
    def predicate(self, promotion: CandidatePromotion) -> dict:
        return json.loads(encode_promotion(promotion))

    def test_original_build_and_merge_stay_distinct_facts(self):
        value = self.predicate(promotion_for())
        self.assertEqual(value["original_build"], {"commit": BUILD, "tree": TREE, "base_commit": BASE})
        self.assertEqual(value["merged"], {"commit": MERGED, "tree": TREE})
        self.assertEqual(set(value), {"schema_version", "kind", "candidate_id", "locator", "freeze",
                                      "original_build", "merged"})

    def test_candidate_locator_and_exact_freeze_record_are_bound(self):
        promotion = promotion_for()
        value = self.predicate(promotion)
        candidate = promotion.candidate
        self.assertEqual(value["candidate_id"], sha(encode_manifest(candidate.manifest)))
        self.assertEqual(value["locator"]["manifest_sha256"], value["candidate_id"])
        self.assertEqual(value["locator"]["artifact_id"], candidate.locator.artifact_id)
        self.assertEqual(value["freeze"], {"commit": FREEZE_COMMIT,
                                           "record_sha256": sha(encode_publication(promotion.freeze))})

    def test_encoding_is_deterministic_and_canonical(self):
        first, second = encode_promotion(promotion_for()), encode_promotion(promotion_for())
        self.assertEqual(first, second)
        canonical = (json.dumps(json.loads(first), sort_keys=True, separators=(",", ":")) + "\n").encode()
        self.assertEqual(first, canonical)

    def test_all_eight_subjects_are_the_exact_payload_file_digests(self):
        files = payload_files()
        promotion = promotion_for(files)
        subjects = {subject.name: subject.sha256 for subject in promotion.subjects}
        self.assertEqual(tuple(subject.name for subject in promotion.subjects), payload_paths(VERSION))
        self.assertEqual(len(subjects), 8)
        expected = {name: sha(data) for name, data in files.items()}
        expected[MANIFEST_PATH] = sha(encode_manifest(manifest_for(files)))
        self.assertEqual(subjects, expected)

    def test_changing_one_file_changes_only_its_subject_and_the_manifest(self):
        before = {subject.name: subject.sha256 for subject in promotion_for().subjects}
        changed = payload_files(**{"checksums.txt": b"different checksums\n"})
        after = {subject.name: subject.sha256 for subject in promotion_for(changed).subjects}
        self.assertEqual({name for name in before if before[name] != after[name]},
                         {"checksums.txt", MANIFEST_PATH})

    def test_reference_must_name_this_candidate_and_merge(self):
        promotion = promotion_for()
        for changes in ({"candidate_id": "0" * 64}, {"merged_commit": BUILD}):
            with self.subTest(changes=changes):
                self.assertCode("candidate_reference_mismatch", replace, promotion,
                                reference=replace(promotion.reference, **changes))

    def test_freeze_must_bind_this_candidate_and_its_built_source(self):
        promotion = promotion_for()
        for changes in ({"candidate_id": "0" * 64}, {"source_commit": MERGED}, {"source_tree": "0" * 40}):
            with self.subTest(changes=changes):
                self.assertCode("candidate_promotion_freeze_mismatch", replace, promotion,
                                freeze=freeze_for(promotion.candidate, **changes))

    def test_source_equality_is_decided_by_the_publication_source_owner(self):
        promotion = promotion_for()
        self.assertCode("publication_tree_mismatch", replace, promotion,
                        source=PublicationSource(build_commit=BUILD, merged_commit=MERGED, tree="0" * 40))
        self.assertCode("candidate_promotion_source_mismatch", replace, promotion,
                        source=PublicationSource(build_commit=MERGED, merged_commit=MERGED, tree=TREE))


class ReleaseBodyTests(ContractCase):
    def test_body_is_exactly_the_canonical_predicate_and_every_subject_digest(self):
        files = payload_files()
        promotion = promotion_for(files)
        digests = {name: sha(data) for name, data in files.items()}
        digests[MANIFEST_PATH] = sha(encode_manifest(manifest_for(files)))
        lines = release_body(promotion).split("\n")
        self.assertEqual(lines[:3], ["Development candidate promotion", "",
                                     "Promotion predicate (canonical JSON):"])
        self.assertEqual((lines[3] + "\n").encode(), encode_promotion(promotion))
        self.assertEqual(lines[4:6], ["", "Payload SHA-256 digests:"])
        self.assertEqual(lines[6:], [f"{digests[name]}  {name}" for name in payload_paths(VERSION)])

    def test_body_keeps_original_build_and_merge_distinct_and_claims_no_builder(self):
        predicate = json.loads(release_body(promotion_for()).split("\n")[3])
        self.assertEqual(predicate["original_build"]["commit"], BUILD)
        self.assertEqual(predicate["merged"]["commit"], MERGED)
        self.assertNotEqual(BUILD, MERGED)
        self.assertNotIn("built by", release_body(promotion_for()).lower())

    def test_body_is_deterministic_and_follows_every_payload_byte(self):
        self.assertEqual(release_body(promotion_for()), release_body(promotion_for()))
        changed = promotion_for(payload_files(**{"checksums.txt": b"different checksums\n"}))
        self.assertNotEqual(release_body(changed), release_body(promotion_for()))


class VerificationBlockTests(ContractCase):
    CANONICAL = release_body(promotion_for())
    BLOCK = verification_block(CANONICAL)
    NOTES = "## What's Changed\n* Promote by @maintainer in #101\n"

    def test_block_holds_the_exact_canonical_body_in_the_collapsed_wrapper(self):
        self.assertEqual(self.BLOCK, "<details>\n<summary>Build verification details</summary>\n\n```text\n"
                         + self.CANONICAL + "\n```\n\n</details>\n")
        composed = release_text(self.NOTES, promotion_for())
        self.assertEqual(composed, self.NOTES.rstrip() + "\n\n" + self.BLOCK)
        self.assertTrue(release_body_matches(composed, self.CANONICAL))

    def test_legacy_body_and_any_human_prefix_before_one_exact_block_match(self):
        for body in (self.CANONICAL, "Notes\n" + self.BLOCK, "Edited\r\nnotes\r\n\r\n" + self.BLOCK,
                     "  # Title\n\n- one\n\n" + self.BLOCK):
            with self.subTest(body=body[:12]):
                self.assertTrue(release_body_matches(body, self.CANONICAL))

    def test_the_corrected_public_v0_1_37_page_matches_its_published_canonical_body(self):
        self.assertEqual((sha(V37_CANONICAL.encode()), sha(V37_PUBLIC.encode())),
                         (V37_CANONICAL_SHA256, V37_PUBLIC_SHA256))
        self.assertTrue(release_body_matches(V37_CANONICAL, V37_CANONICAL))
        self.assertTrue(release_body_matches(V37_PUBLIC, V37_CANONICAL))
        self.assertFalse(release_body_matches(V37_PUBLIC, self.CANONICAL))
        self.assertFalse(release_body_matches(V37_PUBLIC.replace("4b26d7e4", "4b26d7e5"), V37_CANONICAL))

    def test_tampered_missing_truncated_duplicate_or_trailing_blocks_are_refused(self):
        other = release_body(promotion_for(payload_files(**{TGZ: b"other\n"})))
        digest = self.CANONICAL.split("\n")[6][:64]
        prefix = "Notes\n\n"
        bodies = {
            "missing": prefix.rstrip(), "block_only": self.BLOCK, "blank_prefix": " \n\t\n" + self.BLOCK,
            "other_candidate": prefix + verification_block(other),
            "one_digest": prefix + self.BLOCK.replace(digest, "8" * 64),
            "missing_final_newline": prefix + self.BLOCK.removesuffix("\n"),
            "truncated_footer": prefix + self.BLOCK.removesuffix("</details>\n"),
            "duplicate_opener": prefix + self.BLOCK + self.BLOCK,
            "duplicate_crlf_opener": f"<details>\r\n{VERIFICATION_SUMMARY}\r\n\r\n" + prefix + self.BLOCK,
            "text_after_footer": prefix + self.BLOCK + "more\n",
            "newline_after_footer": prefix + self.BLOCK + "\n",
            "bad_boundary": "Notes" + self.BLOCK,
            "verification_crlf": prefix + self.BLOCK.replace("\n", "\r\n"),
            "canonical_crlf": prefix + verification_block(self.CANONICAL.replace("\n", "\r\n")),
            "legacy_newline": self.CANONICAL + "\n",
            "other_summary": prefix + self.BLOCK.replace("Build verification", "Build Verification"),
            "other_fence": prefix + self.BLOCK.replace("```text", "```json"),
        }
        for label, body in bodies.items():
            with self.subTest(body=label):
                self.assertFalse(release_body_matches(body, self.CANONICAL))
                self.assertCode("candidate_release_body_mismatch", decide_release, promotion=promotion_for(),
                                observed=release(*payload_paths(VERSION), body=body), resume_published=False)

    def test_untrusted_notes_must_be_non_blank_text_without_the_reserved_summary(self):
        for notes in (None, 7, b"notes", "", " \r\n\t", f"x\n{VERIFICATION_SUMMARY}\n"):
            with self.subTest(notes=notes):
                self.assertCode("candidate_release_notes_invalid", release_text, notes, promotion_for())
        composed = release_text("Line\r\nnext\r\n\r\n", promotion_for())
        self.assertEqual(composed, "Line\r\nnext\n\n" + self.BLOCK)


class ReleaseDecisionTests(ContractCase):
    PROMOTION = promotion_for()
    ALL = payload_paths(VERSION)

    def decide(self, observed, *, resume_published: bool = False):
        return decide_release(promotion=self.PROMOTION, observed=observed, resume_published=resume_published)

    def test_absent_release_creates_a_draft_with_every_payload_file(self):
        plan = self.decide(Absent.ABSENT)
        self.assertEqual((plan.create_draft, plan.upload, plan.publish), (True, self.ALL, True))

    def test_partial_matching_draft_uploads_only_the_missing_assets(self):
        present = (MANIFEST_PATH, "checksums.txt", TGZ)
        plan = self.decide(release(*present))
        self.assertFalse(plan.create_draft)
        self.assertEqual(plan.upload, tuple(name for name in self.ALL if name not in present))
        self.assertTrue(plan.publish)

    def test_complete_draft_only_remains_to_be_published(self):
        plan = self.decide(release(*self.ALL))
        self.assertEqual((plan.create_draft, plan.upload, plan.publish), (False, (), True))

    def test_complete_public_release_is_skipped(self):
        for resume in (False, True):
            with self.subTest(resume_published=resume):
                self.assertTrue(self.decide(release(*self.ALL, draft=False), resume_published=resume).complete)

    def test_public_release_with_missing_assets_is_refused(self):
        self.assertCode("candidate_release_published_incomplete", self.decide,
                        release(*self.ALL[1:], draft=False))

    def test_release_targeting_the_build_commit_instead_of_the_merge_is_refused(self):
        self.assertCode("candidate_release_target_mismatch", self.decide, release(*self.ALL, target=BUILD))
        self.assertCode("candidate_release_target_mismatch", self.decide,
                        release(*self.ALL, draft=False, target=BUILD))

    def test_other_tag_or_prerelease_is_refused(self):
        self.assertCode("candidate_release_identity_mismatch", self.decide, release(*self.ALL, tag="v0.1.36"))
        self.assertCode("candidate_release_identity_mismatch", self.decide, release(*self.ALL, prerelease=True))

    def test_unexpected_asset_is_refused_even_beside_matching_assets(self):
        observed = release(MANIFEST_PATH)
        observed = replace(observed, assets=observed.assets + (ReleaseAsset(name="notes.txt", sha256="8" * 64),))
        self.assertCode("candidate_release_asset_unexpected", self.decide, observed)

    def test_asset_with_different_bytes_is_refused(self):
        self.assertCode("candidate_release_asset_conflict", self.decide,
                        release(MANIFEST_PATH, TGZ, digests={TGZ: "8" * 64}))

    def test_resume_never_creates_uploads_or_publishes(self):
        for observed in (Absent.ABSENT, release(MANIFEST_PATH), release(*self.ALL)):
            with self.subTest(observed=observed):
                self.assertCode("candidate_resume_mutation_refused", self.decide, observed,
                                resume_published=True)

    def test_missing_malformed_or_tampered_body_is_refused_for_draft_and_published(self):
        lines = release_body(self.PROMOTION).split("\n")
        predicate = json.loads(lines[3])
        predicate["merged"]["commit"] = BUILD
        other_predicate = json.dumps(predicate, sort_keys=True, separators=(",", ":"))
        subject = lines[6]
        bodies = {
            "missing": "", "trailing_newline": "\n".join(lines) + "\n",
            "truncated": "\n".join(lines[:-1]), "predicate": "\n".join(lines[:3] + [other_predicate] + lines[4:]),
            "subject": "\n".join(lines[:6] + ["8" * 64 + subject[64:]] + lines[7:]),
            "other_candidate": release_body(promotion_for(payload_files(**{TGZ: b"other\n"}))),
        }
        for draft in (True, False):
            for resume in (False, True):
                for label, body in bodies.items():
                    with self.subTest(draft=draft, resume_published=resume, body=label):
                        self.assertCode("candidate_release_body_mismatch", self.decide,
                                        release(*self.ALL, draft=draft, body=body), resume_published=resume)

    def test_composed_body_is_accepted_for_draft_and_published(self):
        composed = release_text("Notes\r\n", self.PROMOTION)
        self.assertEqual(self.decide(release(*self.ALL[1:], body=composed)).upload, self.ALL[:1])
        for resume in (False, True):
            with self.subTest(resume_published=resume):
                self.assertTrue(self.decide(release(*self.ALL, draft=False, body=composed),
                                            resume_published=resume).complete)

    def test_identity_and_target_refusals_precede_the_body_check(self):
        self.assertCode("candidate_release_identity_mismatch", self.decide,
                        release(*self.ALL, tag="v0.1.36", body=""))
        self.assertCode("candidate_release_target_mismatch", self.decide, release(*self.ALL, target=BUILD, body=""))

    def test_inconsistent_listing_is_not_an_observation(self):
        asset = ReleaseAsset(name=TGZ, sha256="8" * 64)
        self.assertCode("candidate_release_observation_invalid", ObservedRelease, tag=TAG, target_commit=MERGED,
                        draft=True, prerelease=False, assets=(asset, asset), body="")
        self.assertCode("candidate_release_observation_invalid", ReleaseAsset, name=TGZ, sha256="8" * 63)
        self.assertCode("candidate_release_observation_invalid", ObservedRelease, tag=TAG, target_commit=MERGED,
                        draft=1, prerelease=False, assets=(), body="")
        self.assertCode("candidate_release_observation_invalid", ObservedRelease, tag=TAG, target_commit=MERGED,
                        draft=True, prerelease=False, assets=(), body=None)


class LatestDeliveryTests(ContractCase):
    def require(self, version, latest, resume_published=False):
        require_latest_delivery(version=version, latest_version=latest, resume_published=resume_published)

    def test_strictly_newer_version_is_ordered_numerically(self):
        for version, latest in (("0.10.0", "0.9.0"), ("0.1.10", "0.1.9"), ("1.0.0", "0.99.99"),
                                (VERSION, "0.1.36")):
            with self.subTest(version=version, latest=latest):
                self.require(version, latest)

    def test_first_release_has_no_latest(self):
        self.require(VERSION, Absent.ABSENT)

    def test_equal_version_is_permitted_only_on_published_resume(self):
        self.require(VERSION, VERSION, resume_published=True)
        self.assertCode("candidate_version_not_newer", self.require, VERSION, VERSION)

    def test_older_version_is_always_refused(self):
        self.assertCode("candidate_version_not_newer", self.require, "0.9.0", "0.10.0")
        self.assertCode("candidate_version_resume_not_latest", self.require, "0.9.0", "0.10.0", True)

    def test_resume_requires_this_version_to_be_the_latest(self):
        for latest in (Absent.ABSENT, "0.1.36"):
            with self.subTest(latest=latest):
                self.assertCode("candidate_version_resume_not_latest", self.require, VERSION, latest, True)

    def test_malformed_versions_and_flags_are_refused(self):
        for value in ("0.1", "v0.1.37", "01.1.1", "1.0.0-rc.1", "1.0.0 ", "", None, True, 1):
            with self.subTest(value=value):
                self.assertCode("candidate_field_invalid", self.require, value, "0.1.36")
                self.assertCode("candidate_field_invalid", self.require, VERSION, value)
        for flag in (1, None, "true"):
            with self.subTest(flag=flag):
                self.assertCode("candidate_field_invalid", self.require, VERSION, "0.1.36", flag)


class NpmDecisionTests(ContractCase):
    TGZ_BYTES = payload_files()[TGZ]
    MANIFEST = manifest_for(payload_files())

    def identity(self, data: bytes, *, version: str = VERSION) -> PackageIdentity:
        return PackageIdentity(name="delegation-layer", version=version, integrity=npm_integrity(data),
                               shasum=npm_shasum(data), tarball=registry_tarball_url(version), git_head=None)

    def test_package_identity_comes_from_the_exact_candidate_tgz(self):
        expected = npm_package(self.MANIFEST, self.TGZ_BYTES)
        self.assertEqual((expected.integrity, expected.shasum),
                         (npm_integrity(self.TGZ_BYTES), npm_shasum(self.TGZ_BYTES)))
        self.assertCode("candidate_npm_package_digest_mismatch", npm_package, self.MANIFEST, b"repacked\n")

    def test_absent_publishes_and_matching_skips(self):
        expected = npm_package(self.MANIFEST, self.TGZ_BYTES)
        self.assertIs(decide_npm(expected=expected, observed=Absent.ABSENT, resume_published=False),
                      Delivery.ACT)
        for resume in (False, True):
            self.assertIs(decide_npm(expected=expected, observed=self.identity(self.TGZ_BYTES),
                                     resume_published=resume), Delivery.SKIP)

    def test_canonical_view_parser_output_with_the_build_commit_is_accepted(self):
        raw = json.dumps({"name": "delegation-layer", "version": VERSION, "gitHead": BUILD,
                          "dist": {"integrity": npm_integrity(self.TGZ_BYTES),
                                   "shasum": npm_shasum(self.TGZ_BYTES),
                                   "tarball": registry_tarball_url(VERSION)}}).encode()
        observed = parse_npm_view(raw, VERSION, release_commit=BUILD)
        self.assertIs(decide_npm(expected=npm_package(self.MANIFEST, self.TGZ_BYTES), observed=observed,
                                 resume_published=False), Delivery.SKIP)

    def test_registry_digest_mismatch_is_refused(self):
        self.assertCode("candidate_npm_conflict", decide_npm,
                        expected=npm_package(self.MANIFEST, self.TGZ_BYTES),
                        observed=self.identity(b"other package bytes\n"), resume_published=False)

    def test_other_version_is_refused(self):
        self.assertCode("candidate_npm_identity_mismatch", decide_npm,
                        expected=npm_package(self.MANIFEST, self.TGZ_BYTES),
                        observed=self.identity(self.TGZ_BYTES, version="0.1.36"), resume_published=False)

    def test_resume_never_publishes(self):
        self.assertCode("candidate_resume_mutation_refused", decide_npm,
                        expected=npm_package(self.MANIFEST, self.TGZ_BYTES), observed=Absent.ABSENT,
                        resume_published=True)


class TapDecisionTests(ContractCase):
    FORMULA_BYTES = payload_files()[FORMULA]
    MANIFEST = manifest_for(payload_files())

    def decide(self, observed, formula: bytes | None = None):
        return decide_tap(manifest=self.MANIFEST, formula=self.FORMULA_BYTES if formula is None else formula,
                          observed=observed)

    def tap(self, content: bytes) -> TapFormula:
        return TapFormula(content=content, blob_sha=git_blob_sha(content))

    def test_absent_formula_is_created(self):
        plan = self.decide(Absent.ABSENT)
        self.assertEqual((plan.action, plan.base_blob_sha), (Delivery.ACT, None))

    def test_exact_formula_bytes_are_skipped(self):
        self.assertIs(self.decide(self.tap(self.FORMULA_BYTES)).action, Delivery.SKIP)

    def test_previous_formula_is_replaced_only_from_its_observed_blob(self):
        previous = b"class DelegationLayer < Formula\n  version \"0.1.36\"\nend\n"
        plan = self.decide(self.tap(previous))
        self.assertEqual((plan.action, plan.base_blob_sha), (Delivery.ACT, git_blob_sha(previous)))

    def test_formula_that_is_not_the_candidate_payload_is_refused(self):
        rendered = b"class DelegationLayer < Formula\nend\n"
        self.assertCode("candidate_formula_digest_mismatch", self.decide, self.tap(rendered), rendered)

    def test_observation_with_a_foreign_blob_name_is_refused(self):
        self.assertCode("candidate_tap_observation_invalid", TapFormula, content=self.FORMULA_BYTES,
                        blob_sha=git_blob_sha(b"other\n"))


if __name__ == "__main__":
    unittest.main()
