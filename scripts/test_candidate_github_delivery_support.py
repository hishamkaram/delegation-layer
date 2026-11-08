"""A finite GitHub release fixture behind the real client, command owner and ``gh`` children.

The fake ``gh`` is a real child found through PATH. It keeps one release
repository in a test-owned JSON state file: releases, their assets, tag refs and
annotated tag objects. It answers the exact ``gh api`` argv the canonical client
and resolver build, uploads files named relative to its working directory, and
appends one log line per invocation. A fault replaces one matching reply,
optionally after applying its effect, to model lost or unknown acknowledgements.
A set ``rewrite_body`` makes the next release write store that body while still
acknowledging the requested one, to model a body lost after the write. Release
IDs in ``unlisted`` stay readable by ID but are left out of the release listing,
to model a listing that lags behind a created draft; a set ``release_fill``
fills every listing page with other tags once any release exists. GitHub's
latest release is the explicit ``latest`` ID, never derived from the listing or
from versions: ``releases/latest`` answers that row, or not found when unset, and
only a release write with ``make_latest`` ``"true"`` moves it. The read-only
``generate-notes`` request answers ``notes`` as the body for the requested tag
and changes no state; a ``None`` ``notes`` answers not found.
Nothing touches the network or a provider.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
import json
from pathlib import Path
import shutil
import sys
import unittest

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.release_tests.release_fixture import FixtureTree
from scripts.release_tests.release_fixture import open_descriptors
from scripts.acceptance.evidence.digests import sha
from scripts.release_verification.candidate_artifact import ArtifactObservation
from scripts.release_verification.candidate_artifact import AttemptObservation
from scripts.release_verification.candidate_artifact import CandidateArtifact
from scripts.release_verification.candidate_artifact import WorkflowObservation
from scripts.release_verification.candidate_github_delivery import PreparedRelease
from scripts.release_verification.candidate_github_delivery import prepare_release_assets
from scripts.release_verification.candidate_github_delivery import publish_prepared_release
from scripts.release_verification.candidate_manifest import CANDIDATE_WORKFLOW_PATH
from scripts.release_verification.candidate_manifest import MANIFEST_PATH
from scripts.release_verification.candidate_manifest import parse_manifest
from scripts.release_verification.candidate_publication_contract import ReleaseSubject
from scripts.release_verification.candidate_publication_contract import release_body
from scripts.acceptance.evidence.source import command
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.github import ArtifactDownload
from scripts.release_verification.published_ports import ConsumerPorts
from scripts.test_candidate_artifact_support import COMMIT, REPOSITORY, VERSION, locator, manifest_bytes, payload_files
from scripts.test_release_github_transport import ObservedGitHubClient


TAG = "v" + VERSION
# GitHub-generated release notes as the fixture answers them; trailing whitespace is presentation.
NOTES = "## What's Changed\r\n* Promote the candidate by @maintainer in #101\r\n\n"
MERGED = COMMIT
OTHER = "d4" * 20
# Parent components a shell or ``gh``'s ``file#label`` syntax would misread.
PAYLOAD_DIRECTORY = "payload dir #1 ü漢"

_FAKE_GH = r'''#!{python}
import hashlib, json, os, sys
argv = sys.argv[1:]
state_path = os.environ["FIXTURE_STATE"]
with open(state_path) as source:
    state = json.load(source)
repo = state["repo"]
log = {"argv": argv, "cwd": os.getcwd()}

def finish(reply, code=0):
    with open(os.environ["FIXTURE_LOG"], "a") as sink:
        sink.write(json.dumps(log) + "\n")
    with open(state_path + ".new", "w") as sink:
        json.dump(state, sink)
    os.replace(state_path + ".new", state_path)
    if reply is not None:
        sys.stdout.write(reply if isinstance(reply, str) else json.dumps(reply))
    sys.exit(code)

def release(tag):
    return next((row for row in state["releases"] if row["tag_name"] == tag), None)

def lookup(value, path):
    for key in path.strip(".").split("."):
        value = value[key]
    return value

def tag_refs(prefix):
    rows = [{"ref": ref, "object": target} for ref, target in sorted(state["refs"].items())
            if ref.startswith(prefix)]
    return rows + state["extra_tag_refs"]

def asset_page(row, page):
    fill = state["asset_fill"]
    if fill:
        return [{"id": 100000 + page * 100 + n, "name": "extra-%d-%d" % (page, n), "size": 1,
                 "state": "uploaded", "digest": "sha256:" + "0" * 64} for n in range(100)]
    override = state["asset_pages"].get(str(page))
    return override if override is not None else row["assets"][(page - 1) * 100:page * 100]

def get(endpoint):
    path, _, query = endpoint.partition("?")
    params = dict(item.split("=") for item in query.split("&")) if query else {}
    base = "repos/" + repo + "/"
    if path == base + "releases/latest":
        row = next((row for row in state["releases"] if row["id"] == state["latest"]), None)
        return None if row is None else {key: value for key, value in row.items() if key != "assets"}
    if path == base + "releases":
        page = int(params["page"])
        if state["release_fill"] and state["releases"]:
            return [{"id": 900000 + page * 100 + n, "tag_name": "other-%d-%d" % (page, n)} for n in range(30)]
        listed = [row for row in state["releases"] if row["id"] not in state["unlisted"]]
        return listed[(page - 1) * 30:page * 30]
    if path.startswith(base + "releases/") and path.endswith("/assets"):
        release_id = int(path.split("/")[-2])
        row = next(row for row in state["releases"] if row["id"] == release_id)
        return asset_page(row, int(params["page"]))
    if path.startswith(base + "releases/") and path[len(base + "releases/"):].isdigit():
        release_id = int(path[len(base + "releases/"):])
        row = next((row for row in state["releases"] if row["id"] == release_id), None)
        return None if row is None else {key: value for key, value in row.items() if key != "assets"}
    if path.startswith(base + "git/matching-refs/"):
        return tag_refs("refs/" + path[len(base + "git/matching-refs/"):])
    if path.startswith(base + "git/ref/"):
        ref = "refs/" + path[len(base + "git/ref/"):]
        target = state["refs"].get(ref)
        return None if target is None else {"ref": ref, "object": target}
    if path.startswith(base + "git/tags/"):
        target = state["tag_objects"].get(path.rsplit("/", 1)[1])
        return None if target is None else {"object": target}
    return None

def write(method, endpoint, body):
    base = "repos/" + repo + "/"
    if method == "POST" and endpoint == base + "releases/generate-notes":
        notes = state["notes"]
        return None if notes is None else {"name": body["tag_name"], "body": notes}
    if method == "POST" and endpoint == base + "releases":
        state["next_id"] += 1
        row = {"id": state["next_id"], "assets": [], **body}
        state["releases"].append(row)
        return rewritten(row)
    if method == "POST" and endpoint == base + "git/refs":
        if body["ref"] in state["refs"]:
            return None
        state["refs"][body["ref"]] = {"type": "commit", "sha": body["sha"]}
        return {"ref": body["ref"], "object": state["refs"][body["ref"]]}
    if method == "PATCH" and endpoint.startswith(base + "releases/"):
        row = next(row for row in state["releases"] if row["id"] == int(endpoint.rsplit("/", 1)[1]))
        row.update(body)
        if body.get("make_latest") == "true":
            state["latest"] = row["id"]
        return rewritten(row)
    return None

def rewritten(row):
    reply = {key: value for key, value in row.items() if key != "assets"}
    if state["rewrite_body"] is not None:
        row["body"], state["rewrite_body"] = state["rewrite_body"], None
    return reply

def upload(tag, names):
    row = release(tag)
    if row is None or "--clobber" in argv:
        return None
    for name in names:
        if any(asset["name"] == name for asset in row["assets"]):
            return None
        with open(name, "rb") as source:
            data = source.read()
        state["next_id"] += 1
        row["assets"].append({"id": state["next_id"], "name": os.path.basename(name), "size": len(data),
                              "state": "uploaded", "digest": "sha256:" + hashlib.sha256(data).hexdigest()})
    return ""

if argv[0] == "release":
    key = "UPLOAD " + argv[2]
    act = lambda: upload(argv[2], argv[3:argv.index("--repo")])
    jq = None
else:
    method = argv[argv.index("--method") + 1] if "--method" in argv else "GET"
    endpoint = argv[7] if "--method" in argv else argv[1]
    key = method + " " + endpoint
    jq = argv[argv.index("--jq") + 1] if "--jq" in argv else None
    if "--input" in argv:
        with open(argv[argv.index("--input") + 1]) as source:
            log["body"] = json.load(source)
        act = lambda: write(method, endpoint, log["body"])
    else:
        act = lambda: get(endpoint)
log["key"] = key
fault = next((item for item in state["faults"]
              if key.startswith(item["key"]) and key[len(item["key"]):][:1] != "/"), None)
if fault is not None:
    state["faults"].remove(fault)
    if fault["apply"]:
        act()
    finish(fault["output"], fault["exit"])
reply = act()
if reply is None:
    finish('{"message":"Not Found","status":"404"}', 1)
if jq is not None:
    reply = "\t".join(str(lookup(reply, part)) for part in jq.split(' + "\\t" + ')) + "\n"
finish(reply)
'''


def promotion(files: dict[str, bytes], manifest: bytes) -> ReleaseSubject:
    """The exact built source and payload chosen for delivery."""
    return ReleaseSubject(locator=locator(b"container", manifest), manifest=parse_manifest(manifest))


def artifact(target: ReleaseSubject, payload: Path) -> CandidateArtifact:
    """The loader's result for ``target`` with its payload at ``payload``; observations are inert."""
    candidate = target
    return CandidateArtifact(
        locator=candidate.locator, manifest=candidate.manifest, repository_id=1,
        workflow=WorkflowObservation(id=2, path=CANDIDATE_WORKFLOW_PATH, state="active"),
        attempt=AttemptObservation(run_id=candidate.locator.run_id, run_attempt=candidate.locator.run_attempt,
                                   workflow_id=2, head_sha=COMMIT, path=CANDIDATE_WORKFLOW_PATH,
                                   status="completed", conclusion="success"),
        artifact=ArtifactObservation(id=candidate.locator.artifact_id, name="candidate", size=1,
                                     sha256=candidate.locator.artifact_archive_sha256,
                                     run_id=candidate.locator.run_id, head_sha=COMMIT),
        container=ArtifactDownload(path=payload.parent / "container.zip", size=1,
                                   sha256=candidate.locator.artifact_archive_sha256),
        payload=payload)


@dataclass(frozen=True)
class Fault:
    """Replace the reply of the first request whose key starts with ``key``, once.

    The rest of the request key may not begin a further path segment, so a
    fault for an endpoint never answers a request for one of its subpaths.
    """

    key: str
    output: str | None = None
    exit: int = 1
    apply: bool = False


class DeliveryCase(unittest.TestCase):
    """A test-owned tree holding the fake ``gh``, its state, the payload and the body directory."""

    def setUp(self) -> None:
        self.descriptors = open_descriptors()
        self.addCleanup(lambda: self.assertEqual(open_descriptors(), self.descriptors))
        self.tree = FixtureTree.open(prefix="candidate-github-delivery-test",
                                     label="candidate GitHub delivery fixture")
        self.addCleanup(self.tree.dispose)
        root = self.tree.root
        (root / "bin").mkdir()
        gh = root / "bin" / "gh"
        gh.write_text(_FAKE_GH.replace("{python}", sys.executable, 1))
        gh.chmod(0o755)
        self.state_path, self.log_path = root / "state.json", root / "invocations.jsonl"
        self.files = payload_files()
        self.manifest = manifest_bytes(self.files)
        self.promotion = promotion(self.files, self.manifest)
        self.payload = root / "case" / PAYLOAD_DIRECTORY
        self.payload.mkdir(parents=True)
        for name, data in {**self.files, MANIFEST_PATH: self.manifest}.items():
            (self.payload / name).write_bytes(data)
        self.bodies = root / "case" / "bodies"
        self.bodies.mkdir()
        self.uploads: list[tuple[list[str], Path | None]] = []
        # Every client shares one receipt, so each real child's Wait fact stays inspectable.
        self.receipt: dict = {"commands": []}
        self.save({"repo": REPOSITORY, "releases": [], "refs": {}, "tag_objects": {}, "next_id": 500,
                   "faults": [], "asset_pages": {}, "asset_fill": False, "extra_tag_refs": [],
                   "rewrite_body": None, "unlisted": [], "release_fill": False, "latest": None,
                   "notes": NOTES})

    def state(self) -> dict:
        return json.loads(self.state_path.read_text())

    def save(self, state: dict) -> None:
        self.state_path.write_text(json.dumps(state))

    def edit(self, change) -> None:
        state = self.state()
        change(state)
        self.save(state)

    def fault(self, fault: Fault) -> None:
        self.edit(lambda state: state["faults"].append(asdict(fault)))

    def expected_digest(self, name: str) -> str:
        return sha({**self.files, MANIFEST_PATH: self.manifest}[name])

    def client(self) -> ObservedGitHubClient:
        environment = {"PATH": str(self.tree.root / "bin"), "FIXTURE_STATE": str(self.state_path),
                       "FIXTURE_LOG": str(self.log_path), "GH_TOKEN": "private-canary"}
        return ObservedGitHubClient(self.receipt, environment, self.tree)

    def ports(self, before_upload=None) -> ConsumerPorts:
        """The canonical command owner, recording each upload argv and cwd before it runs."""
        def run(receipt, name, argv, env, **options):
            self.uploads.append((list(argv), options.get("cwd")))
            if before_upload is not None:
                before_upload(len(self.uploads))
            return self.tree.observe_operation(lambda: command(receipt, name, argv, env, **options))

        return ConsumerPorts(run=run, which=shutil.which)

    def prepare(self, *, resume_published: bool = False, payload: Path | None = None,
                bodies: Path | None = None, before_upload=None,
                target: ReleaseSubject | None = None) -> PreparedRelease:
        chosen = self.promotion if target is None else target
        # Tag-resolver children run outside the observed client and ports.
        return self.tree.observe_operation(lambda: prepare_release_assets(
            chosen, artifact(chosen, self.payload if payload is None else payload),
            client=self.client(), ports=self.ports(before_upload),
            work_directory=self.bodies if bodies is None else bodies, resume_published=resume_published))

    def publish(self, prepared: PreparedRelease, *, resume_published: bool = False) -> PreparedRelease:
        # Tag-resolver children run outside the observed client.
        return self.tree.observe_operation(lambda: publish_prepared_release(
            self.promotion, prepared, client=self.client(), work_directory=self.bodies,
            resume_published=resume_published))

    def assert_retained(self, raised: BaseException, injected: BaseException, writes: list[str]) -> None:
        """An unproven closure retains the tree; remove it only once every real child's Wait is known."""
        self.assertIs(raised, injected)
        self.assertIs(self.tree.unresolved, injected)
        self.assertEqual(self.mutations(), writes)
        self.assertFalse(self.tree.dispose())
        self.assertTrue(self.state_path.is_file() and self.log_path.is_file())
        self.assertEqual(open_descriptors(), self.descriptors)
        # Each logged child is one receipt record whose exit code its owner read after Wait;
        # the injected failure launched no child, so it added neither.
        records = self.receipt["commands"]
        self.assertEqual(len(records), len(self.log()))
        self.assertTrue(all(type(record["exit_code"]) is int for record in records))
        self.tree.remove_retained()
        self.assertFalse(self.tree.root.exists())

    def refused(self, code: str, operation, *args, **kwargs) -> VerificationError:
        with self.assertRaises(VerificationError) as caught:
            operation(*args, **kwargs)
        self.assertEqual(caught.exception.code, code)
        return caught.exception

    def log(self) -> list[dict]:
        if not self.log_path.exists():
            return []
        return [json.loads(line) for line in self.log_path.read_text().splitlines()]

    def mutations(self) -> list[str]:
        """Every logged request sent with a write method, in order.

        The read-only notes generation is a POST; it stays listed so its place
        before the draft creation is checked.
        """
        return [entry["key"] for entry in self.log()
                if entry["key"].startswith(("POST", "PATCH", "PUT", "DELETE", "UPLOAD"))]

    def seed_release(self, *, draft: bool, names: tuple[str, ...] = (), target: str = MERGED,
                     tag_commit: str | None = None, digests: dict[str, str] | None = None,
                     latest: bool = False, body: str | None = None) -> int:
        """Store one release with the named assets and ``body``; ``tag_commit`` also creates its tag.

        The body defaults to the exact legacy ``release_body``. ``latest`` makes
        it GitHub's latest release explicitly.
        """
        payload = {**self.files, MANIFEST_PATH: self.manifest}
        assets = [{"id": 600 + index, "name": name, "size": len(payload[name]), "state": "uploaded",
                   "digest": "sha256:" + (digests or {}).get(name, sha(payload[name]))}
                  for index, name in enumerate(names)]

        def change(state: dict) -> None:
            state["releases"].append({"id": 51, "tag_name": TAG, "target_commitish": target, "draft": draft,
                                      "prerelease": False, "assets": assets,
                                      "body": release_body(self.promotion) if body is None else body})
            if tag_commit is not None:
                state["refs"][f"refs/tags/{TAG}"] = {"type": "commit", "sha": tag_commit}
            if latest:
                state["latest"] = 51

        self.edit(change)
        return 51

    def seed_other(self, tag: str, *, release_id: int = 40, draft: object = False, prerelease: object = False,
                   latest: bool = False) -> int:
        """Store another version's release row; ``latest`` makes it GitHub's latest release explicitly."""
        def change(state: dict) -> None:
            state["releases"].append({"id": release_id, "tag_name": tag, "target_commitish": OTHER,
                                      "draft": draft, "prerelease": prerelease, "assets": [], "body": ""})
            if latest:
                state["latest"] = release_id

        self.edit(change)
        return release_id
