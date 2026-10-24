"""A finite in-memory GitHub Git database behind the real ``GitHubClient``.

``FakeGitHub`` stands in for the canonical bounded command owner: it answers
the exact ``gh api`` argv the client builds, hashes every object as Git does,
and applies ``force: false`` ref updates only as fast-forwards. Faults replace
one matching reply, optionally after applying its effect, so tests can model
lost acknowledgements and unproven child closure without a network or child.
"""

from __future__ import annotations

import base64
import hashlib
import json
from dataclasses import dataclass, field
from pathlib import Path
from urllib.parse import parse_qs, unquote

from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError, BoundedCommandResult, CommandFailure,
)
from scripts.delegate_acceptance.release_verification.candidate_store_contract import (
    StoreLocation, TreeEntry, git_tree_sha,
)
from scripts.delegate_acceptance.release_verification.published_contract import git_blob_sha


REPOSITORY = "owner/store"
BRANCH = "development-evidence"
MARKER_BYTES = b'{"kind":"delegate-candidate-store","schema_version":1}\n'
RULESET_ID = 7
NOT_FOUND = BoundedCommandResult(1, b'{"message":"Not Found","status":"404"}')
REFUSED = BoundedCommandResult(1, b'{"message":"Update is not a fast forward","status":"422"}')
TIMEOUT = BoundedCommandError(CommandFailure.TIMEOUT)

Files = dict[str, tuple[str, str]]


# Git entry types other than a file blob, by mode: a directory and a submodule commit.
_ENTRY_TYPES = {"040000": "tree", "160000": "commit"}


@dataclass(frozen=True)
class Commit:
    tree: str
    parents: tuple[str, ...]
    message: str


@dataclass
class Fault:
    """Replace the reply of every matching request (``remaining`` times, None for always)."""

    method: str
    needle: str
    result: BoundedCommandResult | None = None
    error: BaseException | None = None
    apply: bool = False
    remaining: int | None = 1


@dataclass
class FakeGitHub:
    repository: str = REPOSITORY
    branch: str = BRANCH
    blobs: dict[str, bytes] = field(default_factory=dict)
    trees: dict[str, tuple[TreeEntry, ...]] = field(default_factory=dict)
    commits: dict[str, Commit] = field(default_factory=dict)
    refs: dict[str, str] = field(default_factory=dict)
    calls: list[tuple[str, str]] = field(default_factory=list)
    faults: list[Fault] = field(default_factory=list)
    rules: list[dict[str, object]] = field(default_factory=list)
    rulesets: dict[int, dict[str, object]] = field(default_factory=dict)
    repository_reply: dict[str, object] = field(default_factory=dict)

    def __post_init__(self) -> None:
        self.repository_reply = {"id": 42, "full_name": self.repository, "private": True}
        self.rules = [{"type": kind, "ruleset_source_type": "Repository", "ruleset_source": self.repository,
                       "ruleset_id": RULESET_ID}
                      for kind in ("deletion", "non_fast_forward", "required_linear_history")]
        self.rulesets = {RULESET_ID: {
            "id": RULESET_ID, "name": "candidate store", "target": "branch", "source_type": "Repository",
            "source": self.repository, "enforcement": "active", "bypass_actors": [],
        }}

    # Object database -------------------------------------------------------------

    def blob(self, data: bytes) -> str:
        sha = git_blob_sha(data)
        self.blobs[sha] = data
        return sha

    def tree(self, entries: tuple[TreeEntry, ...]) -> str:
        sha = git_tree_sha(entries)
        self.trees[sha] = tuple(sorted(entries, key=lambda entry: entry.name))
        return sha

    def commit(self, tree: str, parents: tuple[str, ...], message: str) -> str:
        body = f"tree {tree}\n" + "".join(f"parent {parent}\n" for parent in parents)
        body += "author Fake <fake@example.invalid> 0 +0000\ncommitter Fake <fake@example.invalid> 0 +0000\n\n"
        data = (body + message).encode()
        sha = hashlib.sha1(b"commit %d\0" % len(data) + data, usedforsecurity=False).hexdigest()
        self.commits[sha] = Commit(tree, parents, message)
        return sha

    def source(self, commit: str, files: Files) -> None:
        """Store a source commit of this repository under the name a candidate manifest pins.

        The name is not recomputed: readers check a commit reply against the
        name they requested, never its Git hash.
        """
        self.commits[commit] = Commit(self.build(files), (), f"source {commit[:8]}")

    def build(self, files: Files) -> str:
        """Store nested trees for flat ``path -> (mode, sha)`` files; return the root tree."""
        children: dict[str, Files] = {}
        entries: list[TreeEntry] = []
        for path, (mode, sha) in files.items():
            head, _, rest = path.partition("/")
            if rest:
                children.setdefault(head, {})[rest] = (mode, sha)
            else:
                entries.append(TreeEntry(head, mode, _ENTRY_TYPES.get(mode, "blob"), sha))
        entries.extend(TreeEntry(name, "040000", "tree", self.build(nested)) for name, nested in children.items())
        return self.tree(tuple(entries))

    def flatten(self, tree: str, prefix: str = "") -> Files:
        files: Files = {}
        for entry in self.trees[tree]:
            if entry.type == "tree":
                files.update(self.flatten(entry.sha, prefix + entry.name + "/"))
            else:
                files[prefix + entry.name] = (entry.mode, entry.sha)
        return files

    def resolve(self, tree_ish: str) -> str | None:
        """The tree named by a tree SHA, a commit SHA or ``<commit>:<path>``."""
        name, _, path = tree_ish.partition(":")
        tree = self.commits[name].tree if name in self.commits else name if name in self.trees else None
        for part in (path.split("/") if path else ()):
            entry = next((item for item in self.trees.get(tree or "", ()) if item.name == part), None)
            tree = entry.sha if entry is not None and entry.type == "tree" else None
        return tree

    def path_identity(self, commit: str, path: str) -> tuple[str, str] | None:
        tree, parts = self.commits[commit].tree, path.split("/")
        for index, part in enumerate(parts):
            entry = next((item for item in self.trees.get(tree, ()) if item.name == part), None)
            if entry is None:
                return None
            if index == len(parts) - 1:
                return entry.mode, entry.sha
            if entry.type != "tree":
                return None
            tree = entry.sha
        return None

    def history(self, head: str, path: str) -> list[str]:
        """Commits that changed ``path`` along first parents, newest first, as ``git log -- path``."""
        found, current = [], head
        while current is not None:
            parents = self.commits[current].parents
            before = self.path_identity(parents[0], path) if parents else None
            if self.path_identity(current, path) != before:
                found.append(current)
            current = parents[0] if parents else None
        return found

    def descends(self, commit: str, ancestor: str) -> bool:
        pending = [commit]
        while pending:
            current = pending.pop()
            if current == ancestor:
                return True
            pending.extend(self.commits[current].parents)
        return False

    # Replies ---------------------------------------------------------------------

    def tree_reply(self, sha: str) -> dict[str, object]:
        rows = [{"path": entry.name, "mode": entry.mode, "type": entry.type, "sha": entry.sha,
                 "url": f"https://api.github.invalid/{entry.type}s/{entry.sha}"} for entry in self.trees[sha]]
        return {"sha": sha, "tree": rows, "truncated": False}

    def commit_reply(self, sha: str) -> dict[str, object]:
        """Reply as GitHub does: the created and read message lacks the request's terminal LF.

        Observed live on both the create and the read reply; only the reply is
        modelled, never the stored commit object or its identity.
        """
        commit = self.commits[sha]
        return {"sha": sha, "tree": {"sha": commit.tree}, "message": commit.message.removesuffix("\n"),
                "parents": [self.parent_reply(parent) for parent in commit.parents]}

    @staticmethod
    def parent_reply(sha: str) -> dict[str, object]:
        return {"sha": sha, "url": f"https://api.github.invalid/commits/{sha}",
                "html_url": f"https://github.invalid/commit/{sha}"}

    def blob_reply(self, sha: str) -> dict[str, object]:
        encoded = base64.b64encode(self.blobs[sha]).decode()
        wrapped = "\n".join(encoded[index:index + 60] for index in range(0, len(encoded), 60)) + "\n"
        return {"sha": sha, "size": len(self.blobs[sha]), "content": wrapped, "encoding": "base64"}

    def ref_reply(self) -> dict[str, object]:
        return {"ref": f"refs/heads/{self.branch}",
                "object": {"type": "commit", "sha": self.refs[self.branch]}}

    def get(self, route: str) -> object:
        if route == "":
            return self.repository_reply
        if route.startswith("/rules/branches/"):
            return self.rules if route.split("?")[0] == f"/rules/branches/{self.branch}" else None
        if route.startswith("/rulesets/"):
            return self.rulesets.get(int(route.rsplit("/", 1)[1]))
        if route == f"/git/ref/heads/{self.branch}":
            return self.ref_reply() if self.branch in self.refs else None
        if route.startswith("/git/commits/"):
            sha = route.rsplit("/", 1)[1]
            return self.commit_reply(sha) if sha in self.commits else None
        if route.startswith("/git/trees/"):
            tree = self.resolve(unquote(route.rsplit("/", 1)[1]))
            return None if tree is None else self.tree_reply(tree)
        if route.startswith("/git/blobs/"):
            sha = route.rsplit("/", 1)[1]
            return self.blob_reply(sha) if sha in self.blobs else None
        if route.startswith("/commits?"):
            query = {key: value[0] for key, value in parse_qs(route.split("?", 1)[1]).items()}
            size, page = int(query["per_page"]), int(query["page"])
            listed = self.history(query["sha"], query["path"])[(page - 1) * size:page * size]
            return [{"sha": sha, "parents": [self.parent_reply(parent) for parent in self.commits[sha].parents],
                     "commit": {"author": {"date": "2026-01-01T00:00:00Z"}}} for sha in listed]
        return None

    def write(self, method: str, route: str, body: dict) -> object:
        if method == "POST" and route == "/git/blobs":
            return {"sha": self.blob(base64.b64decode(body["content"]))}
        if method == "POST" and route == "/git/trees":
            files = self.flatten(body["base_tree"]) if "base_tree" in body else {}
            files.update({row["path"]: (row["mode"], row["sha"]) for row in body["tree"]})
            return self.tree_reply(self.build(files))
        if method == "POST" and route == "/git/commits":
            return self.commit_reply(self.commit(body["tree"], tuple(body["parents"]), body["message"]))
        if method == "PATCH" and route == f"/git/refs/heads/{self.branch}":
            current = self.refs[self.branch]
            if body["force"] is not False or not self.descends(body["sha"], current):
                return REFUSED
            self.refs[self.branch] = body["sha"]
            return self.ref_reply()
        return None

    def __call__(self, argv: list[str], _environment: dict[str, str], *, max_output: int,
                 **_options: object) -> BoundedCommandResult:
        method, endpoint = argv[3], argv[8]
        prefix = f"repos/{self.repository}"
        self.calls.append((method, endpoint))
        fault = next((item for item in self.faults if item.method == method and item.needle in endpoint), None)
        if fault is not None:
            if fault.remaining is not None:
                fault.remaining -= 1
                if fault.remaining == 0:
                    self.faults.remove(fault)
            if not fault.apply:
                return self._fail(fault)
        if not endpoint.startswith(prefix):
            return NOT_FOUND
        route = endpoint[len(prefix):]
        if method == "GET":
            value = self.get(route)
        else:
            value = self.write(method, route, json.loads(Path(argv[argv.index("--input") + 1]).read_bytes()))
        if fault is not None:
            return self._fail(fault)
        if isinstance(value, BoundedCommandResult):
            return value
        if value is None:
            return NOT_FOUND
        data = json.dumps(value).encode()
        if len(data) > max_output:
            raise BoundedCommandError(CommandFailure.OUTPUT_EXCEEDED)
        return BoundedCommandResult(0, data)

    @staticmethod
    def _fail(fault: Fault) -> BoundedCommandResult:
        if fault.error is not None:
            raise fault.error
        assert fault.result is not None
        return fault.result


class Store:
    """Builds one store branch history in a ``FakeGitHub`` through ordinary commits."""

    def __init__(self, github: FakeGitHub):
        self.github = github
        self.marker = github.blob(MARKER_BYTES)
        self.files: Files = {"STORE.json": ("100644", self.marker)}
        self.genesis = github.commit(github.build(self.files), (), "genesis")
        github.refs[github.branch] = self.genesis
        self.head = self.genesis

    @property
    def location(self) -> StoreLocation:
        return StoreLocation(self.github.repository, self.github.branch, self.genesis, self.marker)

    def put(self, path: str, data: bytes, mode: str = "100644") -> None:
        self.files[path] = (mode, self.github.blob(data))

    def step(self, label: str = "step", *, parents: tuple[str, ...] | None = None) -> str:
        """Commit the current files on the head (or on ``parents``) and move the branch to it."""
        root = self.github.build(self.files)
        self.head = self.github.commit(root, (self.head,) if parents is None else parents, label)
        self.github.refs[self.github.branch] = self.head
        return self.head
