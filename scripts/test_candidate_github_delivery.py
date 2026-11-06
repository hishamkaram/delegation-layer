"""Exact-byte GitHub release delivery through the real client, command owner and fixture ``gh``."""

from __future__ import annotations

import json
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts import test_candidate_github_delivery_support as support
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.release_verification import assets
from scripts.delegate_acceptance.release_verification.candidate_github_delivery import (
    PreparedRelease, prepare_release_assets,
)
from scripts.delegate_acceptance.release_verification.candidate_manifest import MANIFEST_PATH, payload_paths
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import (
    VERIFICATION_SUMMARY, release_body, release_text,
)
from scripts.delegate_acceptance.release_verification.contract import MAX_OUTPUT, VerificationError
from scripts.test_candidate_artifact_support import REPOSITORY, VERSION, manifest_bytes, payload_files
from scripts.test_candidate_github_delivery_support import (
    MERGED, NOTES, OTHER, PAYLOAD_DIRECTORY, TAG, DeliveryCase, Fault, artifact, promotion,
)


NAMES = payload_paths(VERSION)
CREATE = f"POST repos/{REPOSITORY}/releases"
# The read-only notes generation; it saves nothing on GitHub.
GENERATE = f"POST repos/{REPOSITORY}/releases/generate-notes"
TAG_REFS = f"GET repos/{REPOSITORY}/git/matching-refs/tags/{TAG}"
CREATE_REF = f"POST repos/{REPOSITORY}/git/refs"
UPLOAD = f"UPLOAD {TAG}"
LIST = f"GET repos/{REPOSITORY}/releases?per_page=30&page=1"
LATEST = f"GET repos/{REPOSITORY}/releases/latest"
# The fixture's first created release.
CREATED = 501
# Generated notes whose release replies exceed the default output limit, far below the 1 MiB release page.
LARGE_NOTES = NOTES + "".join(f"* Change {n} by @maintainer in #{n}\n" for n in range(2000))


def by_id(release_id: int) -> str:
    return f"GET repos/{REPOSITORY}/releases/{release_id}"


class PreparationTests(DeliveryCase):
    def test_first_preparation_creates_one_exact_draft_and_uploads_each_basename(self):
        before = {path.name: path.read_bytes() for path in self.payload.iterdir()}
        prepared = self.prepare()
        self.assertEqual(self.mutations(), [GENERATE, CREATE, *([UPLOAD] * len(NAMES))])
        generate = next(entry for entry in self.log() if entry["key"] == GENERATE)
        # With no published release there is no previous tag to pin.
        self.assertEqual(generate["body"], {"tag_name": TAG, "target_commitish": MERGED})
        create = next(entry for entry in self.log() if entry["key"] == CREATE)
        body = release_text(NOTES, self.promotion)
        self.assertEqual(create["body"], {"tag_name": TAG, "target_commitish": MERGED, "name": TAG,
                                          "draft": True, "prerelease": False, "body": body})
        self.assertTrue(body.startswith(NOTES.rstrip() + "\n\n<details>\n"))
        self.assertEqual(prepared.observed.body, body)
        self.assertEqual([argv for argv, _cwd in self.uploads],
                         [["gh", "release", "upload", TAG, name, "--repo", REPOSITORY] for name in NAMES])
        self.assertEqual({cwd for _argv, cwd in self.uploads}, {self.payload})
        uploads = [entry for entry in self.log() if entry["key"] == UPLOAD]
        self.assertEqual({entry["cwd"] for entry in uploads}, {str(self.payload)})
        self.assertTrue(all("--clobber" not in entry["argv"] for entry in uploads))
        self.assertEqual(self.state()["refs"], {})
        self.assertEqual((prepared.observed.draft, prepared.observed.target_commit), (True, MERGED))
        self.assertEqual({asset.name: asset.sha256 for asset in prepared.observed.assets},
                         {name: self.expected_digest(name) for name in NAMES})
        self.assertEqual({path.name: path.read_bytes() for path in self.payload.iterdir()}, before)
        self.assertEqual(sorted(path.name for path in self.bodies.iterdir()),
                         ["release-draft.json", "release-notes.json"])

    def test_partial_draft_uploads_only_missing_files(self):
        present = NAMES[:3]
        release_id = self.seed_release(draft=True, names=present)
        prepared = self.prepare()
        self.assertEqual(self.mutations(), [UPLOAD] * (len(NAMES) - 3))
        self.assertEqual([argv[4] for argv, _cwd in self.uploads], list(NAMES[3:]))
        self.assertEqual(prepared.release_id, release_id)
        self.assertEqual(len(prepared.observed.assets), len(NAMES))

    def test_complete_published_release_skips_without_writes(self):
        self.seed_release(draft=False, names=NAMES, target="main", tag_commit=MERGED, latest=True)
        for resume in (False, True):
            with self.subTest(resume_published=resume):
                prepared = self.prepare(resume_published=resume)
                self.assertEqual(self.publish(prepared, resume_published=resume), prepared)
                self.assertEqual(prepared.observed.target_commit, MERGED)
        self.assertEqual(self.mutations(), [])
        # Each edge read GitHub's own latest release once per invocation.
        self.assertEqual([entry["key"] for entry in self.log()].count(LATEST), 4)
        self.assertEqual(list(self.bodies.iterdir()), [])

    def test_existing_wrong_tag_refuses_before_any_mutation(self):
        self.edit(lambda state: state["refs"].update({f"refs/tags/{TAG}": {"type": "commit", "sha": OTHER}}))
        self.refused("candidate_release_tag_mismatch", self.prepare)
        self.seed_release(draft=True, names=NAMES[:2])
        self.refused("candidate_release_tag_mismatch", self.prepare)
        self.assertEqual(self.mutations(), [])

    def test_annotated_tag_resolves_through_its_object_to_the_merged_commit(self):
        annotated = "ab" * 20
        self.edit(lambda state: (
            state["refs"].update({f"refs/tags/{TAG}": {"type": "tag", "sha": annotated}}),
            state["tag_objects"].update({annotated: {"type": "commit", "sha": OTHER}})))
        self.refused("candidate_release_tag_mismatch", self.prepare)
        self.assertEqual(self.mutations(), [])
        self.edit(lambda state: state["tag_objects"].update({annotated: {"type": "commit", "sha": MERGED}}))
        prepared = self.prepare()
        self.assertEqual(prepared.observed.target_commit, MERGED)
        self.publish(prepared)
        self.assertNotIn(CREATE_REF, self.mutations())
        self.assertIn(f"GET repos/{REPOSITORY}/git/tags/{annotated}", [entry["key"] for entry in self.log()])

    def test_prefix_sibling_tag_is_absence_and_publish_creates_the_exact_tag(self):
        sibling = f"refs/tags/{TAG}0"
        self.edit(lambda state: state["refs"].update({sibling: {"type": "commit", "sha": OTHER}}))
        self.publish(self.prepare())
        create = next(entry for entry in self.log() if entry["key"] == CREATE_REF)
        self.assertEqual(create["body"], {"ref": f"refs/tags/{TAG}", "sha": MERGED})
        self.assertEqual(self.state()["refs"][sibling], {"type": "commit", "sha": OTHER})

    def test_failed_or_malformed_tag_lookup_is_never_absence(self):
        cases = (
            ("github_request_failed", Fault(TAG_REFS)),
            ("github_response_invalid", Fault(TAG_REFS, output="not json", exit=0)),
            ("candidate_tag_refs_invalid", Fault(TAG_REFS, output='{"ref": "refs/tags/x"}', exit=0)),
            ("candidate_tag_refs_invalid", Fault(TAG_REFS, output='[{"ref": "refs/heads/x"}]', exit=0)),
            ("candidate_tag_refs_invalid", Fault(TAG_REFS, output='[{"ref": 7}]', exit=0)),
            ("github_object_invalid", Fault(TAG_REFS, output='["refs/tags/x"]', exit=0)),
            ("candidate_tag_refs_invalid", Fault(TAG_REFS, exit=0, output="[" + ",".join(
                [f'{{"ref": "refs/tags/{TAG}{n}"}}' for n in range(101)]) + "]")),
            ("candidate_tag_refs_invalid", Fault(TAG_REFS, exit=0, output=(
                f'[{{"ref": "refs/tags/{TAG}"}}, {{"ref": "refs/tags/{TAG}"}}]'))),
        )
        for code, fault in cases:
            with self.subTest(code=code, output=fault.output):
                self.fault(fault)
                self.refused(code, self.prepare)
        self.assertEqual(self.mutations(), [])

    def test_failed_tag_resolution_after_exact_listing_stops(self):
        self.fault(Fault(TAG_REFS, output=f'[{{"ref": "refs/tags/{TAG}"}}]', exit=0))
        self.refused("command_failed", self.prepare)
        self.assertEqual(self.mutations(), [])

    def test_draft_with_absent_tag_must_target_the_merged_commit(self):
        self.seed_release(draft=True, names=NAMES[:1], target="main")
        self.refused("candidate_release_target_mismatch", self.prepare)
        self.assertEqual(self.mutations(), [])

    def test_published_release_without_its_tag_or_assets_refuses(self):
        self.seed_release(draft=False, names=NAMES, target=MERGED)
        self.refused("candidate_release_tag_missing", self.prepare)
        self.edit(lambda state: (state["refs"].update({f"refs/tags/{TAG}": {"type": "commit", "sha": MERGED}}),
                                 state["releases"][0]["assets"].pop()))
        self.refused("candidate_release_published_incomplete", self.prepare)
        self.assertEqual(self.mutations(), [])

    def test_conflicting_extra_or_unfinished_assets_refuse_without_writes(self):
        cases = (
            ("candidate_release_asset_conflict", {"digest": "sha256:" + "0" * 64}),
            ("candidate_release_asset_conflict", {"size": 1}),
            ("candidate_release_asset_unexpected", {"name": "unrelated.txt"}),
            ("candidate_release_asset_not_uploaded", {"state": "starter"}),
            ("candidate_release_asset_invalid", {"digest": None}),
            ("candidate_release_asset_invalid", {"digest": "md5:" + "0" * 32}),
            ("candidate_release_asset_invalid", {"size": "12"}),
            ("github_id_invalid", {"id": 0}),
        )
        self.seed_release(draft=True, names=NAMES[:2])
        original = self.state()
        for code, change in cases:
            with self.subTest(code=code, change=change):
                self.save(original)
                self.edit(lambda state: state["releases"][0]["assets"][0].update(change))
                self.refused(code, self.prepare)
        self.assertEqual(self.mutations(), [])

    def test_resume_refuses_every_draft_tag_or_upload_mutation(self):
        self.refused("candidate_resume_mutation_refused", self.prepare, resume_published=True)
        self.seed_release(draft=True, names=NAMES[:2])
        self.refused("candidate_resume_mutation_refused", self.prepare, resume_published=True)
        self.edit(lambda state: state["releases"].clear())
        self.seed_release(draft=True, names=NAMES)
        prepared = self.prepare()
        self.refused("candidate_resume_mutation_refused", self.publish, prepared, resume_published=True)
        self.assertEqual(self.mutations(), [])


class ReleaseBodyTests(DeliveryCase):
    def ack(self, **changes) -> str:
        """A creation or publication reply naming the exact release except for ``changes``."""
        return json.dumps({"id": 501, "tag_name": TAG, "target_commitish": MERGED, "draft": True,
                           "prerelease": False, "body": release_body(self.promotion), **changes})

    def test_missing_null_or_malformed_body_is_never_an_observation(self):
        for draft in (True, False):
            for change in ({"body": None}, {"body": 7}, {"body": ["text"]}, None):
                with self.subTest(draft=draft, change=change):
                    self.edit(lambda state: state["releases"].clear())
                    self.seed_release(draft=draft, names=NAMES, tag_commit=None if draft else MERGED)
                    self.edit(lambda state: state["releases"][0].pop("body") if change is None
                              else state["releases"][0].update(change))
                    self.refused("candidate_release_row_invalid", self.prepare)
        self.assertEqual(self.mutations(), [])

    def test_existing_release_with_another_body_is_refused_and_never_rewritten(self):
        exact = release_body(self.promotion)
        changed = payload_files(**{NAMES[-1]: b"other bytes\n"})
        other = release_body(promotion(changed, manifest_bytes(changed)))
        bodies = ("", exact + "\n", exact.replace(self.expected_digest(NAMES[0]), "0" * 64), other)
        for draft in (True, False):
            for resume in (False, True):
                for body in bodies:
                    with self.subTest(draft=draft, resume_published=resume, body=body[:20]):
                        self.edit(lambda state: state["releases"].clear())
                        self.seed_release(draft=draft, names=NAMES, tag_commit=None if draft else MERGED)
                        self.edit(lambda state: state["releases"][0].update(body=body))
                        self.refused("candidate_release_body_mismatch", self.prepare, resume_published=resume)
                        self.assertEqual(self.state()["releases"][0]["body"], body)
        self.assertEqual(self.mutations(), [])

    def test_draft_acknowledgement_with_another_body_stops_without_retry(self):
        # The legacy machine-only body is not the composed body this draft was created with.
        for body in ("rewritten", release_body(self.promotion)):
            with self.subTest(body=body[:20]):
                self.edit(lambda state: state["releases"].clear())
                for path in self.bodies.iterdir():
                    path.unlink()
                calls = len(self.mutations())
                self.fault(Fault(CREATE, output=self.ack(body=body), exit=0, apply=True))
                self.refused("candidate_release_ack_invalid", self.prepare)
                self.assertEqual(self.mutations()[calls:], [GENERATE, CREATE])
                self.assertEqual(self.log()[-1]["key"], CREATE)
        (self.bodies / "release-draft.json").unlink()
        self.prepare()
        self.assertEqual(self.mutations()[4:], [*([UPLOAD] * len(NAMES))])

    def test_large_draft_acknowledgement_is_read_within_the_release_page_limit(self):
        self.edit(lambda state: state.update(notes=LARGE_NOTES))
        body = release_text(LARGE_NOTES, self.promotion)
        self.assertTrue(MAX_OUTPUT < len(body.encode()) < 2 * MAX_OUTPUT)
        prepared = self.prepare()
        self.assertEqual(self.mutations(), [GENERATE, CREATE, *([UPLOAD] * len(NAMES))])
        create = next(entry for entry in self.log() if entry["key"] == CREATE)
        self.assertEqual(create["body"]["body"], body)
        self.assertEqual((prepared.observed.body, self.state()["releases"][0]["body"]), (body, body))

    def test_body_lost_after_an_exact_draft_acknowledgement_is_refused_not_prepared(self):
        self.edit(lambda state: state.update(rewrite_body=""))
        self.refused("candidate_release_body_mismatch", self.prepare)
        self.assertEqual(self.mutations(), [GENERATE, CREATE])
        self.assertEqual(self.state()["releases"][0]["body"], "")
        for body in self.bodies.iterdir():
            body.unlink()
        self.refused("candidate_release_body_mismatch", self.prepare)
        self.assertEqual(self.mutations(), [GENERATE, CREATE])

    def test_publication_acknowledgement_with_another_body_stops_after_one_call(self):
        prepared = self.prepare()
        patch_key = f"PATCH repos/{REPOSITORY}/releases/{prepared.release_id}"
        self.fault(Fault(patch_key, output=self.ack(id=prepared.release_id, draft=False, body="x"),
                         exit=0, apply=True))
        self.refused("candidate_release_ack_invalid", self.publish, prepared)
        self.assertEqual(self.mutations()[-2:], [CREATE_REF, patch_key])
        self.assertEqual(self.log()[-1]["key"], patch_key)

    def test_failed_or_malformed_notes_generation_stops_before_any_write(self):
        def reply(**changes) -> str:
            value = {"name": TAG, "body": NOTES, **changes}
            return json.dumps({key: item for key, item in value.items() if item is not ...})

        cases = (
            ("github_request_failed", Fault(GENERATE)),
            ("github_request_failed", Fault(GENERATE, output='{"message":"Not Found","status":"404"}')),
            ("github_response_invalid", Fault(GENERATE, output="not json", exit=0)),
            *(("candidate_release_notes_invalid", Fault(GENERATE, output=output, exit=0)) for output in (
                "[]", reply(name=...), reply(name=7), reply(body=...), reply(body=None), reply(body=7),
                reply(body=""), reply(body=" \r\n\t"), reply(body=f"x\n{VERIFICATION_SUMMARY}\n"))),
        )
        for code, fault in cases:
            with self.subTest(code=code, output=fault.output):
                for path in self.bodies.iterdir():
                    path.unlink()
                calls = len(self.mutations())
                self.fault(fault)
                self.refused(code, self.prepare)
                self.assertEqual(self.mutations()[calls:], [GENERATE])
                self.assertEqual((self.state()["releases"], self.state()["refs"], self.uploads), ([], {}, []))
                self.assertFalse((self.bodies / "release-draft.json").exists())

    def test_existing_composed_release_is_accepted_and_never_regenerated(self):
        composed = "Edited notes\r\n\r\n" + release_text(NOTES, self.promotion).removeprefix(NOTES.rstrip())
        self.seed_release(draft=True, names=NAMES[:2], body=composed)
        prepared = self.prepare()
        self.assertEqual((prepared.observed.body, self.mutations()), (composed, [UPLOAD] * (len(NAMES) - 2)))
        patch_key = f"PATCH repos/{REPOSITORY}/releases/{prepared.release_id}"
        # Publication acknowledges the body it read and accepted, never the recomputed legacy text.
        self.fault(Fault(patch_key, output=self.ack(id=prepared.release_id, draft=False,
                                                    body=release_body(self.promotion)), exit=0, apply=True))
        self.refused("candidate_release_ack_invalid", self.publish, prepared)
        (self.bodies / "release-publish.json").unlink()
        published = self.publish(prepared)
        self.assertEqual((published.observed.draft, published.observed.body), (False, composed))
        for resume in (False, True):
            self.assertEqual(self.prepare(resume_published=resume).observed.body, composed)
        self.assertNotIn(GENERATE, [entry["key"] for entry in self.log()])
        self.assertEqual(self.state()["releases"][0]["body"], composed)

    def test_large_publication_acknowledgement_is_read_within_the_release_page_limit(self):
        # A seeded complete draft, so a creation reply can never fail in place of the publication reply.
        composed = release_text(LARGE_NOTES, self.promotion)
        self.assertTrue(MAX_OUTPUT < len(composed.encode()) < 2 * MAX_OUTPUT)
        self.seed_release(draft=True, names=NAMES, body=composed)
        prepared = self.prepare()
        self.assertEqual((self.mutations(), prepared.observed.body), ([], composed))
        published = self.publish(prepared)
        patch_key = f"PATCH repos/{REPOSITORY}/releases/{prepared.release_id}"
        self.assertEqual(self.mutations(), [CREATE_REF, patch_key])
        self.assertEqual((published.observed.draft, published.observed.body), (False, composed))
        self.assertEqual(self.state()["releases"][0]["body"], composed)

    def test_body_lost_after_an_exact_publication_acknowledgement_is_refused_not_published(self):
        prepared = self.prepare()
        patch_key = f"PATCH repos/{REPOSITORY}/releases/{prepared.release_id}"
        self.edit(lambda state: state.update(rewrite_body="rewritten"))
        self.refused("candidate_release_body_mismatch", self.publish, prepared)
        writes = self.mutations()
        self.assertEqual(writes[-2:], [CREATE_REF, patch_key])
        for resume in (False, True):
            self.refused("candidate_release_body_mismatch", self.publish, prepared, resume_published=resume)
        self.assertEqual(self.mutations(), writes)
        self.assertEqual(self.state()["releases"][0]["body"], "rewritten")


class AssetListingTests(DeliveryCase):
    def setUp(self):
        super().setUp()
        self.seed_release(draft=True)

    def rows(self, page: int, count: int = 100) -> list[dict]:
        return [{"id": 1000 * page + n, "name": f"other-{page}-{n}", "size": 1, "state": "uploaded",
                 "digest": "sha256:" + "0" * 64} for n in range(count)]

    def test_late_duplicate_id_or_name_is_refused_after_the_full_listing(self):
        duplicate_id = {**self.rows(2, 1)[0], "id": 1000}
        self.edit(lambda state: state["asset_pages"].update({"1": self.rows(1), "2": [duplicate_id]}))
        self.refused("candidate_release_asset_duplicate", self.prepare)
        late_name = {**self.rows(2, 1)[0], "name": "other-1-0"}
        self.edit(lambda state: state["asset_pages"].update({"2": [late_name]}))
        self.refused("candidate_release_asset_duplicate", self.prepare)
        self.assertEqual(self.mutations(), [])

    def test_malformed_page_and_full_final_page_never_end_the_listing(self):
        for code, pages in (("github_release_assets_invalid", {"1": {"rows": []}}),
                            ("github_release_assets_invalid", {"1": self.rows(1, 101)}),
                            ("github_release_assets_invalid", {"1": self.rows(1), "2": {"late": True}}),
                            ("github_object_invalid", {"1": ["row"]})):
            with self.subTest(code=code):
                self.edit(lambda state: state.update(asset_pages=pages))
                self.refused(code, self.prepare)
        self.edit(lambda state: state.update(asset_pages={}, asset_fill=True))
        error = self.refused("github_release_assets_pagination_incomplete", self.prepare)
        self.assertEqual(error.stage, "github-release-assets")
        listed = [entry["key"] for entry in self.log() if "/assets?" in entry["key"]]
        self.assertTrue(listed[-1].endswith("per_page=100&page=100"))
        self.assertEqual(self.mutations(), [])

    def test_short_page_completes_the_listing_without_reading_further(self):
        listed = [{"id": 700 + index, "name": name, "size": len(self.payload.joinpath(name).read_bytes()),
                   "state": "uploaded", "digest": "sha256:" + self.expected_digest(name)}
                  for index, name in enumerate(NAMES)]
        self.edit(lambda state: state["asset_pages"].update({"1": listed, "2": {"late": True}}))
        prepared = self.prepare()
        self.assertEqual(len(prepared.observed.assets), len(NAMES))
        self.assertFalse(any(entry["key"].endswith("&page=2") for entry in self.log()))
        self.assertEqual(self.mutations(), [])


class MutationAcknowledgementTests(DeliveryCase):
    def test_unknown_draft_acknowledgement_stops_after_one_call(self):
        for code, fault in (("candidate_release_ack_invalid", Fault(CREATE, output='{"id": 9}', exit=0, apply=True)),
                            ("github_request_failed", Fault(CREATE, apply=True))):
            with self.subTest(code=code):
                self.edit(lambda state: state["releases"].clear())
                for body in self.bodies.iterdir():
                    body.unlink()
                self.fault(fault)
                calls = len(self.log())
                self.refused(code, self.prepare)
                self.assertEqual([entry["key"] for entry in self.log()[calls:]][-1], CREATE)
        self.assertEqual(self.mutations(), [GENERATE, CREATE, GENERATE, CREATE])
        for body in self.bodies.iterdir():
            body.unlink()
        self.prepare()
        self.assertEqual(self.mutations().count(CREATE), 2)
        self.assertEqual(self.mutations().count(UPLOAD), len(NAMES))

    def test_failed_upload_stops_without_retry_or_inferred_success(self):
        self.seed_release(draft=True)
        self.fault(Fault(UPLOAD, apply=True))
        self.refused("command_failed", self.prepare)
        self.assertEqual(self.mutations(), [UPLOAD])
        self.assertEqual(self.log()[-1]["key"], UPLOAD)
        self.prepare()
        self.assertEqual(self.mutations(), [UPLOAD] * len(NAMES))

    def test_existing_draft_body_path_is_never_reused(self):
        (self.bodies / "release-draft.json").write_bytes(b"{}")
        self.refused("github_request_body_path_invalid", self.prepare)
        self.assertEqual(self.mutations(), [GENERATE])
        (self.bodies / "release-draft.json").unlink()
        self.refused("github_request_body_path_invalid", self.prepare)
        self.assertEqual((self.mutations(), self.state()["releases"]), ([GENERATE], []))

    def test_unknown_tag_or_publish_acknowledgement_stops_after_one_call(self):
        prepared = self.prepare()
        self.fault(Fault(CREATE_REF, output='{"ref": "refs/tags/other"}', exit=0, apply=True))
        self.refused("candidate_tag_ack_invalid", self.publish, prepared)
        self.assertEqual(self.mutations()[-1], CREATE_REF)
        (self.bodies / "tag-ref.json").unlink()
        patch_key = f"PATCH repos/{REPOSITORY}/releases/{prepared.release_id}"
        self.fault(Fault(patch_key, output='{"draft": false}', exit=0, apply=True))
        self.refused("candidate_release_ack_invalid", self.publish, prepared)
        self.assertEqual(self.mutations()[-2:], [CREATE_REF, patch_key])
        (self.bodies / "release-publish.json").unlink()
        published = self.publish(prepared)
        self.assertFalse(published.observed.draft)
        self.assertEqual(self.mutations().count(CREATE_REF), 1)
        self.assertEqual(self.mutations().count(patch_key), 1)


class PublicationTests(DeliveryCase):
    def test_publish_creates_the_tag_then_publishes_the_same_complete_release_once(self):
        prepared = self.prepare()
        uploads = len(self.uploads)
        published = self.publish(prepared)
        patch_key = f"PATCH repos/{REPOSITORY}/releases/{prepared.release_id}"
        self.assertEqual(self.mutations()[-2:], [CREATE_REF, patch_key])
        publish = next(entry for entry in self.log() if entry["key"] == patch_key)
        self.assertEqual(publish["body"], {"draft": False, "make_latest": "true"})
        self.assertEqual(len(self.uploads), uploads)
        self.assertEqual((published.release_id, published.observed.draft, published.observed.target_commit),
                         (prepared.release_id, False, MERGED))
        self.assertEqual(published.observed.assets, prepared.observed.assets)
        writes = len(self.mutations())
        self.assertEqual(self.publish(prepared), published)
        self.assertEqual(self.prepare(), published)
        self.assertEqual(len(self.mutations()), writes)

    def test_existing_merged_tag_is_published_without_ref_creation(self):
        self.seed_release(draft=True, names=NAMES, tag_commit=MERGED)
        self.publish(self.prepare())
        self.assertEqual(self.mutations(), [f"PATCH repos/{REPOSITORY}/releases/51"])

    def test_publish_refuses_incomplete_or_different_release_without_writes(self):
        self.seed_release(draft=True, names=NAMES)
        prepared = self.prepare()
        self.refused("candidate_release_not_prepared", self.publish,
                     PreparedRelease(release_id=prepared.release_id + 1, observed=prepared.observed))
        self.edit(lambda state: state["releases"][0]["assets"].pop())
        self.refused("candidate_release_not_prepared", self.publish, prepared)
        self.edit(lambda state: state["releases"].clear())
        # The listing may lag behind the prepared release; only its failed read by ID refuses.
        self.refused("github_request_failed", self.publish, prepared)
        self.assertEqual(self.mutations(), [])


class LatestGuardTests(DeliveryCase):
    """Before any write, and again before the tag and publication, GitHub's own latest release is read."""

    def setUp(self):
        super().setUp()
        self.pristine = self.state()
        self.patch_key = f"PATCH repos/{REPOSITORY}/releases/{CREATED}"
        self.since = 0

    def fresh(self) -> None:
        """Restore the empty remote, free the body paths and observe only later requests."""
        self.save(self.pristine)
        for body in self.bodies.iterdir():
            body.unlink()
        self.since = len(self.log())

    def keys(self) -> list[str]:
        return [entry["key"] for entry in self.log()[self.since:]]

    def writes(self) -> list[str]:
        return [key for key in self.keys() if key.startswith(("POST", "PATCH", "PUT", "DELETE", "UPLOAD"))]

    def latest_reads(self) -> int:
        return self.keys().count(LATEST)

    def assertDelivered(self, *, latest_reads: int) -> None:
        self.publish(self.prepare())
        self.assertEqual(self.writes(), [GENERATE, CREATE, *([UPLOAD] * len(NAMES)), CREATE_REF, self.patch_key])
        self.assertEqual((self.state()["latest"], self.latest_reads()), (CREATED, latest_reads))

    def test_without_a_published_release_the_complete_listing_is_absence(self):
        arrangements = {
            "empty": lambda: None,
            "draft and prerelease only": lambda: (self.seed_other("v0.1.40", draft=True),
                                                  self.seed_other("v0.1.41", release_id=41, prerelease=True)),
        }
        for name, arrange in arrangements.items():
            with self.subTest(name=name):
                self.fresh()
                arrange()
                self.assertDelivered(latest_reads=0)

    def test_an_older_latest_is_read_by_each_edge_and_never_inferred_from_the_listing(self):
        # A higher listed version is not latest; ``0.1.9`` sorts after ``0.1.37`` only as text.
        arrangements = {
            "older latest": lambda: (self.seed_other("v0.1.50", release_id=41),
                                     self.seed_other("v0.1.36", latest=True)),
            "numeric order": lambda: self.seed_other("v0.1.9", latest=True),
        }
        for (name, arrange), previous in zip(arrangements.items(), ("v0.1.36", "v0.1.9")):
            with self.subTest(name=name):
                self.fresh()
                arrange()
                self.assertDelivered(latest_reads=2)
                # The notes range starts at the latest tag the guard checked, pinned explicitly.
                generate = next(entry for entry in self.log()[self.since:] if entry["key"] == GENERATE)
                self.assertEqual(generate["body"], {"tag_name": TAG, "target_commitish": MERGED,
                                                    "previous_tag_name": previous})

    def test_a_partial_draft_under_an_older_latest_uploads_only_missing_files(self):
        self.seed_other("v0.1.36", latest=True)
        self.seed_release(draft=True, names=NAMES[:3])
        self.publish(self.prepare())
        self.assertEqual(self.writes(), [*([UPLOAD] * (len(NAMES) - 3)),
                                            CREATE_REF, f"PATCH repos/{REPOSITORY}/releases/51"])
        self.assertEqual(self.state()["latest"], 51)

    def test_a_newer_or_equal_latest_refuses_before_any_write(self):
        equal = json.dumps({"id": 40, "tag_name": TAG, "draft": False, "prerelease": False})
        for name, partial, fault in (("newer", False, None), ("newer with partial draft", True, None),
                                     ("equal", False, Fault(LATEST, output=equal, exit=0))):
            with self.subTest(name=name):
                self.fresh()
                self.seed_other("v0.1.40", latest=True)
                if partial:
                    self.seed_release(draft=True, names=NAMES[:2])
                if fault is not None:
                    self.fault(fault)
                error = self.refused("candidate_version_not_newer", self.prepare)
                self.assertEqual(error.stage, "decide-candidate-delivery")
                self.assertEqual((self.writes(), self.uploads), ([], []))

    def test_a_latest_published_after_preparation_refuses_before_the_tag_or_publication(self):
        prepared = self.prepare()
        writes = self.writes()
        self.seed_other("v0.1.40", latest=True)
        self.refused("candidate_version_not_newer", self.publish, prepared)
        self.assertEqual(self.writes(), writes)
        self.assertEqual(self.state()["refs"], {})
        self.assertTrue(self.state()["releases"][0]["draft"])

    def test_a_complete_release_is_accepted_only_while_it_is_latest(self):
        self.seed_release(draft=False, names=NAMES, target="main", tag_commit=MERGED, latest=True)
        original = self.state()
        for other in ("v0.1.40", "v0.1.36"):
            for resume in (False, True):
                with self.subTest(other=other, resume_published=resume):
                    self.save(original)
                    prepared = self.prepare(resume_published=resume)
                    self.seed_other(other, latest=True)
                    self.refused("candidate_version_resume_not_latest", self.publish, prepared,
                                 resume_published=resume)
                    self.refused("candidate_version_resume_not_latest", self.prepare, resume_published=resume)
        self.assertEqual(self.writes(), [])

    def test_a_lost_publication_acknowledgement_resumes_without_writes(self):
        prepared = self.prepare()
        self.fault(Fault(self.patch_key, apply=True))
        self.refused("github_request_failed", self.publish, prepared)
        writes = self.writes()
        self.assertEqual(writes[-2:], [CREATE_REF, self.patch_key])
        for resume in (False, True):
            with self.subTest(resume_published=resume):
                again = self.prepare(resume_published=resume)
                self.assertFalse(again.observed.draft)
                self.assertEqual(self.publish(again, resume_published=resume), again)
        self.assertEqual((self.writes(), self.state()["latest"]), (writes, CREATED))

    def test_a_failed_or_malformed_latest_read_is_never_absence(self):
        not_found = '{"message":"Not Found","status":"404"}'

        def row(**changes) -> str:
            value = {"id": 40, "tag_name": "v0.1.36", "draft": False, "prerelease": False, **changes}
            return json.dumps({key: item for key, item in value.items() if item is not ...})

        cases = (
            ("github_request_failed", None),
            ("github_request_failed", Fault(LATEST)),
            ("github_request_failed", Fault(LATEST, output=not_found)),
            ("github_response_invalid", Fault(LATEST, output="not json", exit=0)),
            ("github_object_invalid", Fault(LATEST, output="[]", exit=0)),
            *(("candidate_github_latest_invalid", Fault(LATEST, output=row(**change), exit=0)) for change in (
                {"tag_name": ...}, {"tag_name": 7}, {"tag_name": "0.1.36"}, {"draft": True}, {"draft": 0},
                {"draft": None}, {"draft": ...}, {"prerelease": True}, {"prerelease": "false"},
                {"prerelease": ...})),
            ("candidate_field_invalid", Fault(LATEST, output=row(tag_name="vfoo"), exit=0)),
            ("candidate_field_invalid", Fault(LATEST, output=row(tag_name="v0.1"), exit=0)),
        )
        for code, fault in cases:
            with self.subTest(code=code, output=None if fault is None else fault.output):
                self.fresh()
                # Unset, the fixture answers ``releases/latest`` as not found although a release is published.
                self.seed_other("v0.1.36", latest=fault is not None)
                if fault is not None:
                    self.fault(fault)
                self.refused(code, self.prepare)
                self.assertEqual((self.writes(), self.latest_reads()), ([], 1))

    def test_a_failed_malformed_or_late_invalid_inventory_stops_before_latest(self):
        page_two = f"GET repos/{REPOSITORY}/releases?per_page=30&page=2"
        stable = json.dumps([{"id": 900 + n, "tag_name": f"v0.0.{n}", "draft": False, "prerelease": False}
                             for n in range(30)])
        empty = Fault(LIST, output="[]", exit=0)
        cases = (
            ("candidate_github_latest_invalid", {"draft": "false"}, []),
            ("candidate_github_latest_invalid", {"prerelease": None}, []),
            ("github_request_failed", None, [empty, Fault(LIST)]),
            ("github_releases_invalid", None, [empty, Fault(LIST, output='{"rows": []}', exit=0)]),
            # Discovery reads both pages; the guard's positive first page cannot end its listing early.
            ("github_releases_invalid", None, [
                Fault(LIST, output=stable, exit=0), Fault(page_two, output="[]", exit=0),
                Fault(LIST, output=stable, exit=0), Fault(page_two, output='{"late": true}', exit=0)]),
        )
        for code, flags, faults in cases:
            with self.subTest(code=code, flags=flags, faults=len(faults)):
                self.fresh()
                if flags is not None:
                    self.seed_other("v0.1.36", latest=True, draft=flags.get("draft", False),
                                    prerelease=flags.get("prerelease", False))
                for fault in faults:
                    self.fault(fault)
                self.refused(code, self.prepare)
                self.assertEqual((self.writes(), self.latest_reads()), ([], 0))
                self.assertEqual(self.state()["faults"], [])


class KnownReleaseTests(DeliveryCase):
    """Once the draft exists, every read names its ID; the listing may omit it but never name another."""

    def keys(self, since: int = 0) -> list[str]:
        return [entry["key"] for entry in self.log()[since:]]

    def reset(self, original: dict, **changes) -> int:
        """Restore the pristine remote with ``changes`` and free the body paths; return the log length."""
        self.save({**original, **changes})
        for body in self.bodies.iterdir():
            body.unlink()
        return len(self.log())

    def test_created_draft_missing_from_a_complete_listing_is_confirmed_by_its_id(self):
        self.edit(lambda state: state["unlisted"].append(CREATED))
        prepared = self.prepare()
        observe = [TAG_REFS, LIST, by_id(CREATED), f"{by_id(CREATED)}/assets?per_page=100&page=1"]
        # The second listing is the latest guard's own complete inventory, which holds no published release.
        self.assertEqual(self.keys(), [TAG_REFS, LIST, LIST, GENERATE, CREATE, *observe,
                                       *([UPLOAD] * len(NAMES)), *observe])
        self.assertEqual(prepared.release_id, CREATED)
        self.assertEqual({asset.name: asset.sha256 for asset in prepared.observed.assets},
                         {name: self.expected_digest(name) for name in NAMES})

    def test_listing_lag_after_the_first_upload_is_confirmed_by_id(self):
        def hide(count: int) -> None:
            if count == 1:
                self.edit(lambda state: state["unlisted"].append(CREATED))

        prepared = self.prepare(before_upload=hide)
        self.assertEqual(prepared.release_id, CREATED)
        self.assertEqual(self.mutations(), [GENERATE, CREATE, *([UPLOAD] * len(NAMES))])
        self.assertEqual(len(prepared.observed.assets), len(NAMES))

    def test_listing_lag_around_publication_is_confirmed_by_id(self):
        patch_key = f"PATCH repos/{REPOSITORY}/releases/{CREATED}"
        prepared = self.prepare()
        original = self.state()
        listed = json.dumps([{key: value for key, value in row.items() if key != "assets"}
                             for row in original["releases"]])
        # Hidden for the whole publication, then only after the publication write; the
        # second listing is the latest guard's own inventory before the write.
        for faults in ([], [Fault(LIST, output=listed, exit=0), Fault(LIST, output=listed, exit=0),
                            Fault(LIST, output="[]", exit=0)]):
            with self.subTest(faults=len(faults)):
                calls = self.reset(original, unlisted=[] if faults else [CREATED])
                for fault in faults:
                    self.fault(fault)
                published = self.publish(prepared)
                self.assertEqual([key for key in self.keys(calls) if key.startswith(("POST", "PATCH"))],
                                 [CREATE_REF, patch_key])
                self.assertEqual((published.release_id, published.observed.draft), (CREATED, False))
                self.assertEqual(published.observed.assets, prepared.observed.assets)
                self.assertEqual(self.state()["faults"], [])

    def test_another_row_by_id_is_refused_after_the_one_creation(self):
        body = release_body(self.promotion)
        unrelated = {"id": 9, "name": "unrelated.txt", "size": 1, "state": "uploaded",
                     "digest": "sha256:" + "0" * 64}
        cases = (
            ("candidate_release_identity_conflict", {"id": CREATED + 1}, {}),
            ("github_id_invalid", {"id": str(CREATED)}, {}),
            ("candidate_release_row_invalid", {"tag_name": TAG + "0"}, {}),
            ("candidate_release_row_invalid", {"draft": "true"}, {}),
            ("candidate_release_tag_missing", {"draft": False}, {}),
            ("candidate_release_identity_mismatch", {"prerelease": True}, {}),
            ("candidate_release_target_mismatch", {"target_commitish": OTHER}, {}),
            ("candidate_release_body_mismatch", {"body": body + "\n"}, {}),
            ("candidate_release_asset_unexpected", {}, {"asset_pages": {"1": [unrelated]}}),
        )
        original = self.state()
        for code, row, state in cases:
            with self.subTest(code=code, row=row, state=state):
                calls = self.reset(original, unlisted=[CREATED], **state)
                if row:
                    self.fault(Fault(by_id(CREATED), exit=0, output=json.dumps({
                        "id": CREATED, "tag_name": TAG, "target_commitish": MERGED, "draft": True,
                        "prerelease": False, "body": body, **row})))
                self.refused(code, self.prepare)
                keys = self.keys(calls)
                self.assertIn(by_id(CREATED), keys)
                self.assertEqual([key for key in keys if key.startswith(("POST", "UPLOAD"))], [GENERATE, CREATE])

    def test_failed_read_by_id_never_adopts_the_listed_release(self):
        original = self.state()
        not_found = '{"message":"Not Found","status":"404"}'
        for fault in (Fault(by_id(CREATED)), Fault(by_id(CREATED), output=not_found)):
            with self.subTest(output=fault.output):
                calls = self.reset(original)
                self.fault(fault)
                self.refused("github_request_failed", self.prepare)
                keys = self.keys(calls)
                # Discovery, the latest guard's inventory and the confirmation each list once.
                self.assertEqual((keys[-1], keys.count(LIST)), (by_id(CREATED), 3))
                self.assertEqual([key for key in keys if key.startswith(("POST", "UPLOAD"))], [GENERATE, CREATE])

    def test_failed_read_by_id_stops_publication_before_any_write(self):
        prepared = self.prepare()
        writes = self.mutations()
        self.fault(Fault(by_id(CREATED)))
        self.refused("github_request_failed", self.publish, prepared)
        self.assertEqual((self.mutations(), self.keys()[-1]), (writes, by_id(CREATED)))
        self.assertTrue(self.state()["releases"][0]["draft"])

    def test_listing_that_names_another_release_or_fails_refuses_without_reading_by_id(self):
        row = {"tag_name": TAG, "target_commitish": MERGED, "draft": True, "prerelease": False,
               "body": release_body(self.promotion)}
        cases = (
            ("candidate_release_draft_unconfirmed",
             [Fault(LIST, exit=0, output=json.dumps([{**row, "id": 777}]))], {}),
            ("github_release_ambiguous", [Fault(LIST, exit=0, output=json.dumps(
                [{**row, "id": CREATED}, {**row, "id": 777}]))], {}),
            ("github_releases_invalid", [Fault(LIST, exit=0, output='{"rows": []}')], {}),
            ("github_request_failed", [Fault(LIST)], {}),
            ("github_releases_pagination_incomplete", [], {"release_fill": True}),
        )
        original = self.state()
        for code, faults, state in cases:
            with self.subTest(code=code):
                calls = self.reset(original, **state)
                # The initial discovery and the latest guard read the real, empty listing first.
                for fault in ([Fault(LIST, exit=0, output="[]")] * 2 if faults else []) + faults:
                    self.fault(fault)
                self.refused(code, self.prepare)
                keys = self.keys(calls)
                self.assertNotIn(by_id(CREATED), keys)
                self.assertEqual([key for key in keys if key.startswith(("POST", "UPLOAD"))], [GENERATE, CREATE])


class UnprovenClosureRetentionTests(DeliveryCase):
    """Command-owner failures outside the observed client still retain the containing tree."""

    def test_unproven_upload_closure_retains_the_tree_and_stops(self):
        injected = VerificationError("command_timeout", "candidate-release-upload")

        def fail(count: int) -> None:
            raise injected

        raised = self.refused("command_timeout", self.prepare, before_upload=fail)
        self.assertEqual(len(self.uploads), 1)
        self.assert_retained(raised, injected, [GENERATE, CREATE])

    def test_unproven_tag_resolution_in_preparation_retains_the_tree(self):
        self.edit(lambda state: state["refs"].update({f"refs/tags/{TAG}": {"type": "commit", "sha": MERGED}}))
        injected = VerificationError("command_timeout", "github-release-ref")
        with patch.object(assets, "command", side_effect=injected) as resolver:
            raised = self.refused("command_timeout", self.prepare)
        self.assertEqual(resolver.call_count, 1)
        self.assert_retained(raised, injected, [])

    def test_an_original_unconverted_upload_failure_reaches_the_tree_through_the_port(self):
        injected = BoundedCommandError(CommandFailure.TIMEOUT)
        real = support.command

        def upload_fault(receipt, name, argv, env, **options):
            if name == "candidate-release-upload":
                raise injected
            return real(receipt, name, argv, env, **options)

        with patch.object(support, "command", upload_fault), self.assertRaises(BoundedCommandError) as caught:
            self.prepare()
        self.assertEqual(len(self.uploads), 1)
        self.assert_retained(caught.exception, injected, [GENERATE, CREATE])

    def test_unproven_tag_resolution_after_ref_creation_retains_the_tree(self):
        prepared = self.prepare()
        writes = self.mutations()
        injected = VerificationError("command_timeout", "github-release-ref")
        with patch.object(assets, "command", side_effect=injected) as resolver:
            raised = self.refused("command_timeout", self.publish, prepared)
        self.assertEqual(resolver.call_count, 1)
        self.assert_retained(raised, injected, [*writes, CREATE_REF])


class LocalPayloadTests(DeliveryCase):
    def test_changed_extra_or_symlinked_payload_refuses_before_any_request(self):
        name = NAMES[0]
        original = (self.payload / name).read_bytes()
        (self.payload / name).write_bytes(original + b"!")
        self.refused("candidate_delivery_payload_mismatch", self.prepare)
        (self.payload / name).write_bytes(original[:-1] + b"!")
        self.refused("candidate_delivery_payload_mismatch", self.prepare)
        copy = self.tree.root / "copy"
        copy.write_bytes(original)
        (self.payload / name).unlink()
        (self.payload / name).symlink_to(copy)
        self.refused("candidate_delivery_payload_mismatch", self.prepare)
        (self.payload / name).unlink()
        (self.payload / name).write_bytes(original)
        (self.payload / "extra").write_bytes(b"x")
        self.refused("candidate_delivery_payload_inventory_mismatch", self.prepare)
        (self.payload / "extra").unlink()
        (self.payload / MANIFEST_PATH).unlink()
        self.refused("candidate_delivery_payload_inventory_mismatch", self.prepare)
        self.assertEqual(self.log(), [])

    def test_bytes_changed_between_uploads_stop_before_the_next_upload(self):
        def change_next(count: int) -> None:
            if count == 1:
                with (self.payload / NAMES[1]).open("ab") as sink:
                    sink.write(b"!")

        self.refused("candidate_delivery_payload_mismatch", self.prepare, before_upload=change_next)
        self.assertEqual(self.mutations(), [GENERATE, CREATE, UPLOAD])

    def test_noncanonical_overlapping_or_foreign_directories_refuse_before_any_request(self):
        link = self.tree.root / "linked"
        link.symlink_to(self.payload.parent, target_is_directory=True)
        self.refused("candidate_delivery_path_invalid", self.prepare, payload=link / PAYLOAD_DIRECTORY)
        self.refused("candidate_delivery_path_invalid", self.prepare,
                     payload=self.payload / ".." / PAYLOAD_DIRECTORY)
        self.refused("candidate_delivery_path_invalid", self.prepare,
                     payload=Path(os.path.relpath(self.payload)))
        self.refused("candidate_delivery_path_invalid", self.prepare, bodies=self.tree.root / "missing")
        self.refused("candidate_delivery_directories_overlap", self.prepare, bodies=self.payload.parent)
        changed = payload_files(**{NAMES[-1]: b"other bytes\n"})
        other = promotion(changed, manifest_bytes(changed))
        self.refused("candidate_delivery_artifact_mismatch", prepare_release_assets, self.promotion,
                     artifact(other, self.payload), client=self.client(), ports=self.ports(),
                     work_directory=self.bodies)
        self.assertEqual(self.log(), [])


if __name__ == "__main__":
    unittest.main()
