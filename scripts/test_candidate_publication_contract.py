"""Publication reference codec, truthful promotion statement and per-destination delivery decisions."""

from __future__ import annotations

from dataclasses import replace
import json
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.delegate_acceptance.evidence.digests import sha
from scripts.delegate_acceptance.release_verification.candidate_manifest import (
    MANIFEST_PATH, ArtifactEntry, BuildInputs, CandidateLocator, CandidateManifest, SourceIdentity,
    Toolchains, encode_manifest, expected_inventory, npm_package_path, payload_paths,
)
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import (
    Absent, ReleaseSubject, Delivery, ObservedRelease, VERIFICATION_SUMMARY, ReleaseAsset,
    TapFormula, decide_npm, decide_release, decide_tap, npm_package, release_body,
    release_body_matches, release_text, require_latest_delivery, verification_block,
)
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.published_contract import (
    PackageIdentity, git_blob_sha, npm_integrity, npm_shasum, parse_npm_view, registry_tarball_url,
)


VERSION = "0.1.37"
TAG = "v" + VERSION
BUILD = "a1" * 20
MERGED = BUILD
BASE = "c3" * 20
TREE = "b2" * 20
TGZ = npm_package_path(VERSION)
FORMULA = "delegation-layer.rb"


def payload_files(**overrides: bytes) -> dict[str, bytes]:
    """Distinct bytes for each of the seven inventory files."""
    files = {path: f"{path} candidate bytes\n".encode() for path, *_ in expected_inventory(VERSION)}
    files.update(overrides)
    return files


def manifest_for(files: dict[str, bytes]) -> CandidateManifest:
    inputs = BuildInputs(goreleaser_config_sha256="1" * 64,
                         go_sum_sha256="2" * 64,
                         toolchains=Toolchains(go="1.27.3", goreleaser="2.18.1", python="3.12.13",
                                               node="22.11.0"))
    artifacts = tuple(ArtifactEntry(path=path, kind=kind, os=os_name, arch=arch, size=len(files[path]),
                                    sha256=sha(files[path]))
                      for path, kind, os_name, arch in expected_inventory(VERSION))
    return CandidateManifest(version=VERSION, build_tag=TAG,
                             source=SourceIdentity(commit=BUILD, tree=TREE, base_commit=BASE),
                             build=inputs, artifacts=artifacts)


def promotion_for(files: dict[str, bytes] | None = None) -> ReleaseSubject:
    manifest = manifest_for(payload_files() if files is None else files)
    locator = CandidateLocator(repository="owner/delegation-layer", run_id=39714439980, run_attempt=2,
                               artifact_id=12399736217, artifact_archive_sha256="9" * 64,
                               manifest_sha256=sha(encode_manifest(manifest)))
    return ReleaseSubject(locator=locator, manifest=manifest)


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


class ContractCase(unittest.TestCase):
    def assertCode(self, code, function, *args, **kwargs):
        with self.assertRaises(VerificationError) as caught:
            function(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)


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

    def test_release_targeting_a_commit_other_than_the_built_source_is_refused(self):
        self.assertCode("candidate_release_target_mismatch", self.decide, release(*self.ALL, target="f" * 40))
        self.assertCode("candidate_release_target_mismatch", self.decide,
                        release(*self.ALL, draft=False, target="f" * 40))

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
        source_line = next(line for line in lines if BUILD in line)
        subject = lines[6]
        bodies = {
            "missing": "", "trailing_newline": "\n".join(lines) + "\n",
            "truncated": "\n".join(lines[:-1]), "source": "\n".join(lines).replace(source_line, source_line.replace(BUILD, "f" * 40)),
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
        self.assertCode("candidate_release_target_mismatch", self.decide, release(*self.ALL, target="f" * 40, body=""))

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
