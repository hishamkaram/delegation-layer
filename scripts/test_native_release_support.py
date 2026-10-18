"""Finite native-release fixtures; no provider or remote commands are executed."""

from __future__ import annotations

import hashlib
from pathlib import Path
import tempfile
import unittest

from scripts.delegate_acceptance.directories.journal import directory_identity_record
from scripts.delegate_acceptance.directories.operations import DirectoryOwner
from scripts.delegate_acceptance.native_release.contract import Invocation, ReleaseRequest, WORKFLOW
from scripts.delegate_acceptance.native_release.receipt import Receipt
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient
from scripts.delegate_acceptance.release_verification.native_history import proof_title


REPO = "hishamkaram/delegation-layer"
TAG = "v1.2.3"
HEAD = "a" * 40
RELEASE = "b" * 40
SOURCE = "c" * 64
PAYLOAD = b"immutable public proof fixture"
PROOF_HASH = hashlib.sha256(PAYLOAD).hexdigest()


class ReleaseFixture(unittest.TestCase):
    def setUp(self) -> None:
        temporary = tempfile.TemporaryDirectory(prefix="native-release-test-", dir=Path.home())
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve()
        self.checkout = self.base / "checkout"
        self.checkout.mkdir()
        self.root = self.base / f"{TAG}-{'1' * 32}"
        owner = DirectoryOwner()
        token = owner.create("test invocation", self.root, self.base)
        request = ReleaseRequest(repo=REPO, tag=TAG, opencode_model="vendor/exact-model",
                                 ccr_model="exact-alias", no_submit=False)
        invocation = Invocation(invocation_id="1" * 32, request=request,
                                checkout=self.checkout, root=self.root,
                                platform_os="Linux", platform_arch="amd64",
                                directory_identity=directory_identity_record(token))
        self.receipt = Receipt.create(invocation, ["run", "--tag", TAG])
        owner.release()
        self.client = FakeGitHub(self.receipt)

    def complete(self, name: str, result: dict[str, object],
                 intent: dict[str, object] | None = None) -> None:
        self.receipt.intend(name, intent or {})
        self.receipt.complete(name, result)

    def preflight_facts(self) -> dict[str, object]:
        return {"repository_id": 41, "release_id": 51, "release_revision": RELEASE,
                "harness_revision": HEAD, "protected_main_revision": HEAD,
                "source_harness_sha256": SOURCE, "candidate_harness": False,
                "actor_id": 61, "actor_login": "maintainer"}

    def seed_project(self, *, verdict: str = "PASS") -> None:
        self.complete("preflight", self.preflight_facts())
        self.receipt.invocation.proof.write_bytes(PAYLOAD)
        self.complete("project", {"proof_sha256": PROOF_HASH, "size": len(PAYLOAD),
                                  "status": "PASS" if verdict == "PASS" else "BLOCKED",
                                  "case_results_verdict": verdict, "pass_count": int(verdict == "PASS"),
                                  "failed_count": int(verdict == "FAIL"), "unresolved_case_count": 0})

    def seed_upload(self) -> None:
        self.seed_project()
        self.client.assets = [self.client.asset()]
        self.complete("upload", {"asset_id": 71, "proof_sha256": PROOF_HASH,
                                 "size": len(PAYLOAD), "name": self.receipt.invocation.proof_name})


class FakeGitHub(GitHubClient):
    def __init__(self, receipt: Receipt):
        super().__init__({"commands": []}, {"PATH": "/usr/bin:/bin", "HOME": str(Path.home())})
        self.invocation = receipt.invocation
        self.assets: list[object] = []
        self.runs: list[object] = []
        self.calls: list[tuple[str, str, dict[str, str] | None]] = []
        self.posts = 0
        self.lose_dispatch_response = False
        self.repo_inaccessible = False
        self.branch_revision = HEAD
        self.actor_id = 61
        self.release_draft = True
        self.asset_pages: dict[int, list[object]] = {}
        self.run_pages: dict[int, list[object]] = {}
        self.total_runs = 0

    def asset(self, *, asset_id: int = 71) -> dict[str, object]:
        return {"id": asset_id, "name": self.invocation.proof_name, "size": len(PAYLOAD),
                "digest": f"sha256:{PROOF_HASH}", "state": "uploaded", "uploader": {"id": 61}}

    def run(self, *, run_id: int = 81) -> dict[str, object]:
        return {"id": run_id, "display_title": proof_title(TAG, self.invocation.invocation_id, PROOF_HASH),
                "head_sha": HEAD, "head_branch": "main", "event": "workflow_dispatch",
                "path": f".github/workflows/{WORKFLOW}", "status": "queued", "run_attempt": 1,
                "html_url": f"https://github.com/{REPO}/actions/runs/{run_id}"}

    def json(self, endpoint: str, *, method: str = "GET",
             fields: dict[str, str] | None = None, limit: int = 64 << 10) -> dict[str, object] | list[object]:
        self.calls.append((method, endpoint, fields))
        if endpoint == f"repos/{REPO}":
            if self.repo_inaccessible:
                raise VerificationError("github_request_failed", "github-api")
            return {"id": 41, "full_name": REPO, "permissions": {"push": True}}
        if endpoint == "user":
            return {"id": self.actor_id, "login": "maintainer", "type": "User"}
        if endpoint.endswith("/branches/main"):
            return {"protected": True, "commit": {"sha": self.branch_revision}}
        if endpoint.endswith(f"/releases/tags/{TAG}"):
            return {"id": 51, "tag_name": TAG, "draft": self.release_draft, "prerelease": False}
        if "/assets?" in endpoint:
            page = int(endpoint.rsplit("page=", 1)[1])
            return self.asset_pages.get(page, self.assets if page == 1 else [])
        if "/runs?" in endpoint:
            page = int(endpoint.rsplit("page=", 1)[1])
            rows = self.run_pages.get(page, self.runs if page == 1 else [])
            return {"total_count": self.total_runs or len(self.runs), "workflow_runs": rows}
        if endpoint.endswith("/dispatches") and method == "POST":
            self.posts += 1
            self.runs = [self.run()]
            if self.lose_dispatch_response:
                raise VerificationError("github_response_invalid", "github-api")
            return {"workflow_run_id": 81, "run_url": f"https://api.github.com/repos/{REPO}/actions/runs/81",
                    "html_url": f"https://github.com/{REPO}/actions/runs/81"}
        if endpoint.endswith("/actions/runs/81"):
            return self.run()
        raise AssertionError(f"unexpected GitHub request: {method} {endpoint}")

    def bytes(self, endpoint: str, *, limit: int) -> bytes:
        if "/releases/assets/" not in endpoint:
            raise AssertionError(endpoint)
        return PAYLOAD
