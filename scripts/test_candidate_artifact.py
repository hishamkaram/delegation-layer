"""The candidate loader binds GitHub metadata, the locator and the manifest before returning."""

from dataclasses import replace
from pathlib import Path
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance.testing.record_fixtures import object_at
from scripts.acceptance.testing.record_fixtures import array_at
from scripts.acceptance.evidence.digests import sha
from scripts.release_verification.candidate_manifest import CANDIDATE_WORKFLOW_PATH
from scripts.release_verification.candidate_manifest import MANIFEST_PATH
from scripts.release_verification.candidate_manifest import MAX_CONTAINER_BYTES
from scripts.release_verification.candidate_manifest import parse_manifest
from scripts.release_verification.candidate_manifest import payload_paths
from scripts.test_candidate_artifact_support import (
    ARTIFACT_ID, COMMIT, DOWNLOAD, ENDPOINTS, FULL_SEQUENCE, OTHER_COMMIT, REPOSITORY, REPOSITORY_ID,
    RUN_ATTEMPT, RUN_ID, VERSION, WORKFLOW_ID, CandidateArtifactCase, manifest_bytes, payload_files,
)


BEFORE_DOWNLOAD = FULL_SEQUENCE[:-1]


_DELETE = object()


def setting(kind: str, *path_and_value):
    """A metadata edit: set ``replies[kind][path...] = value``; ``_DELETE`` removes the key."""
    *keys, last, value = path_and_value

    def change(replies):
        target = replies[kind]
        for key in keys:
            target = target[key]
        if value is _DELETE:
            del target[last]
        else:
            target[last] = value
    return change


def resized(delta: int):
    def change(replies):
        replies["artifact"]["size_in_bytes"] += delta
    return change


class LoadTests(CandidateArtifactCase):
    def test_exact_candidate_loads_once_and_returns_independent_observations(self):
        served = self.serve()
        loaded = self.load(served.locator)
        self.assertEqual(self.requested(), FULL_SEQUENCE)
        self.assert_only_github_children()
        payload = loaded.payload
        self.assertEqual(sorted(child.name for child in payload.iterdir()), list(payload_paths(VERSION)))
        for name, data in served.files.items():
            self.assertEqual((payload / name).read_bytes(), data)
        self.assertEqual((payload / MANIFEST_PATH).read_bytes(), served.manifest)
        self.assertEqual(loaded.manifest, parse_manifest(served.manifest))
        container = served.container.read_bytes()
        self.assertEqual((loaded.container.size, loaded.container.sha256), (len(container), sha(container)))
        self.assertEqual(loaded.container.path.read_bytes(), container)
        record = loaded.observation_record()
        self.assertEqual(record, {
            "candidate_id": sha(served.manifest), "repository": REPOSITORY, "repository_id": REPOSITORY_ID,
            "version": VERSION, "source_commit": COMMIT,
            "workflow": {"id": WORKFLOW_ID, "path": CANDIDATE_WORKFLOW_PATH, "state": "active"},
            "attempt": {"run_id": RUN_ID, "run_attempt": RUN_ATTEMPT, "workflow_id": WORKFLOW_ID,
                        "head_sha": COMMIT, "path": CANDIDATE_WORKFLOW_PATH,
                        "status": "completed", "conclusion": "success"},
            "artifact": {"id": ARTIFACT_ID, "name": f"development-candidate-{RUN_ID}-{RUN_ATTEMPT}",
                         "size": len(container), "sha256": sha(container), "run_id": RUN_ID,
                         "head_sha": COMMIT},
            "container": {"path": str(loaded.container.path), "size": len(container),
                          "sha256": sha(container)},
            "payload": {"path": str(payload), "files": list(payload_paths(VERSION))},
        })
        object_at(record, "workflow")["state"] = "edited"
        array_at(record, "payload", "files").clear()
        self.assertEqual(object_at(loaded.observation_record(), "workflow")["state"], "active")
        self.assertEqual(object_at(loaded.observation_record(), "payload")["files"], list(payload_paths(VERSION)))

    def test_workflow_and_attempt_state_are_recorded_not_gated(self):
        def change(replies):
            replies["workflow"]["state"] = "disabled_manually"
            replies["attempt"].update(conclusion="failure", path=CANDIDATE_WORKFLOW_PATH + "@refs/heads/task")
        loaded = self.load(self.serve(change=change).locator)
        self.assertEqual(loaded.workflow.state, "disabled_manually")
        self.assertEqual((loaded.attempt.conclusion, loaded.attempt.path),
                         ("failure", CANDIDATE_WORKFLOW_PATH + "@refs/heads/task"))
        in_progress = self.serve(change=setting("attempt", "conclusion", None))
        self.assertIsNone(self.load(in_progress.locator).attempt.conclusion)


class MetadataRefusalTests(CandidateArtifactCase):
    def refused_before_download(self, code: str, change) -> None:
        self.log_path.unlink(missing_ok=True)
        self.refused(code, self.serve(change=change).locator)
        requested = self.requested()
        self.assertTrue(requested)
        self.assertEqual(requested, BEFORE_DOWNLOAD[:len(requested)])
        self.assertEqual(self.payload_names(), [])

    def test_foreign_repository_workflow_artifact_and_attempt_are_refused_before_download(self):
        cases = (
            ("candidate_repository_mismatch", setting("repository", "full_name", "other/delegation-layer")),
            ("github_id_invalid", setting("repository", "id", "1368219645")),
            ("candidate_workflow_mismatch", setting("workflow", "path", ".github/workflows/release.yml")),
            ("candidate_workflow_mismatch", setting("attempt", "workflow_id", 361347599)),
            ("candidate_artifact_mismatch", setting("artifact", "id", ARTIFACT_ID + 1)),
            ("candidate_artifact_mismatch", setting("artifact", "name", "untrusted-payload")),
            ("candidate_artifact_mismatch", setting("artifact", "workflow_run", "id", RUN_ID + 1)),
            ("candidate_artifact_mismatch", setting("artifact", "workflow_run", "repository_id", 7)),
            ("candidate_artifact_mismatch", setting("artifact", "workflow_run", "head_repository_id", 7)),
            ("candidate_attempt_mismatch", setting("attempt", "id", RUN_ID + 1)),
            ("candidate_source_mismatch", setting("attempt", "run_attempt", RUN_ATTEMPT - 1)),
            ("candidate_attempt_mismatch", setting("attempt", "repository", "id", 7)),
            ("candidate_attempt_mismatch", setting("attempt", "head_repository", "id", 7)),
            ("candidate_source_mismatch", setting("attempt", "head_sha", OTHER_COMMIT)),
            ("candidate_artifact_expired", setting("artifact", "expired", True)),
        )
        for code, change in cases:
            with self.subTest(code=code):
                self.refused_before_download(code, change)

    def test_digest_size_and_field_types_are_refused_before_download(self):
        cases = (
            ("candidate_field_invalid", setting("artifact", "digest", None)),
            ("candidate_field_invalid", setting("artifact", "expired", "false")),
            ("candidate_field_invalid", setting("artifact", "expired", _DELETE)),
            ("candidate_field_invalid", setting("artifact", "size_in_bytes", True)),
            ("candidate_field_invalid", setting("artifact", "size_in_bytes", 0)),
            ("candidate_artifact_too_large", setting("artifact", "size_in_bytes", MAX_CONTAINER_BYTES + 1)),
            ("candidate_field_invalid", setting("workflow", "state", _DELETE)),
            ("candidate_field_invalid", setting("attempt", "path", None)),
            ("candidate_field_invalid", setting("attempt", "status", 3)),
            ("candidate_field_invalid", setting("attempt", "head_sha", "HEAD")),
            ("github_id_invalid", setting("attempt", "workflow_id", _DELETE)),
            ("github_object_invalid", setting("attempt", "repository", None)),
            ("github_object_invalid", setting("artifact", "workflow_run", [])),
        )
        for code, change in cases:
            with self.subTest(code=code, change=change):
                self.refused_before_download(code, change)

    def test_a_reply_that_is_not_an_object_or_is_missing_keeps_its_github_error(self):
        served = self.serve(change=lambda replies: replies.update(workflow=[1]))
        self.refused("github_object_invalid", served.locator)
        served = self.serve()
        routes = self.routes_path.read_text().replace(ENDPOINTS["attempt"], "missing")
        self.routes_path.write_text(routes)
        self.refused("github_request_failed", served.locator)
        self.assertNotIn(DOWNLOAD, self.requested())


class ContainerBindingTests(CandidateArtifactCase):
    def test_container_bytes_must_equal_metadata_and_locator_and_stay_retained(self):
        served = self.serve(change=resized(1))
        self.refused("candidate_container_mismatch", served.locator)
        self.assertEqual(self.requested(), FULL_SEQUENCE)
        container = self.root / f"load-{self.loads}" / "container.zip"
        self.assertEqual(container.read_bytes(), served.container.read_bytes())
        self.assertEqual(self.payload_names(), [])

    def test_a_container_larger_than_its_metadata_keeps_the_transport_error(self):
        served = self.serve(change=resized(-1))
        self.refused("command_output_exceeded", served.locator)
        self.assertEqual(self.requested(), FULL_SEQUENCE)
        self.assertTrue((self.root / f"load-{self.loads}" / "container.zip").exists())

    def test_a_locator_digest_the_metadata_also_claims_must_match_the_downloaded_bytes(self):
        lie = "0" * 64
        served = self.serve(change=setting("artifact", "digest", "sha256:" + lie))
        self.refused("candidate_container_mismatch", replace(served.locator, artifact_archive_sha256=lie))
        self.assertEqual(self.payload_names(), [])

    def test_foreign_manifest_or_source_commit_is_refused_before_any_payload_write(self):
        files = payload_files()

        def other_head(replies):
            replies["artifact"]["workflow_run"]["head_sha"] = OTHER_COMMIT
            replies["attempt"]["head_sha"] = OTHER_COMMIT
        foreign = self.serve(files=files, manifest=manifest_bytes(files), change=other_head)
        self.refused("candidate_source_mismatch", foreign.locator)
        self.assertEqual(self.requested()[-1], DOWNLOAD)
        self.assertEqual(self.payload_names(), [])


class DestinationTests(CandidateArtifactCase):
    def test_existing_container_is_never_overwritten(self):
        served = self.serve()
        container, payload = self.destinations()
        container.write_bytes(b"keep")
        self.refused("github_download_destination_invalid", served.locator, container=container,
                     payload=payload)
        self.assertEqual(container.read_bytes(), b"keep")
        self.assertEqual(self.requested(), BEFORE_DOWNLOAD)

    def test_payload_must_be_an_empty_directory_apart_from_the_container(self):
        served = self.serve()
        container, payload = self.destinations()
        (payload / "checksums.txt").write_bytes(b"keep")
        self.refused("candidate_payload_directory_invalid", served.locator, container=container,
                     payload=payload)
        self.assertEqual((payload / "checksums.txt").read_bytes(), b"keep")
        link = payload.parent / "linked-payload"
        link.symlink_to(payload)
        for target, destination in ((payload.parent / "missing", container), (link, container),
                                    (payload, payload / "container.zip")):
            (payload / "checksums.txt").unlink(missing_ok=True)
            with self.subTest(payload=target.name, container=destination.name):
                self.refused("candidate_payload_directory_invalid", served.locator,
                             container=destination, payload=target)
        self.assertEqual(self.requested(), [])


if __name__ == "__main__":
    unittest.main()
