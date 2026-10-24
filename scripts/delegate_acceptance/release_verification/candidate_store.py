"""The only I/O owner of the development candidate store branch.

Strict reads resolve the branch head H once, observe the store boundary, and
read everything else at that immutable commit: the pinned orphan genesis and
marker history, the fixed-size root, and one candidate's own path history.
History reads take a supplied H instead and read the same history there, with
no branch, rule or ruleset request. No read lists the other candidates, scans
the whole branch or caches across loads. Every failed read refuses; only a
complete empty history behind positive root checks is absence.

The public ``load_candidate`` and ``append`` read a fresh verified history and
hand canonical replay the facts only this edge can observe: the actual commit
that introduced each stored review and triage, and the exact installer source
bytes at each installer binding's own commit. ``load_candidate_at_head`` hands
replay the same facts for the history at a supplied H, and
``load_candidate_at_branch_head`` supplies the H it resolves once from the
branch, observing no boundary. Replay alone decides their meaning; closure
facts come only from the native lifetime owner. The
private writer proposes one single-parent commit on H with a fresh nonce and
moves the branch with exactly one non-forced update, and only ``append`` calls
it after admission on a strict snapshot. A loaded snapshot or history, a
readback or an acknowledged append is a stored fact, never a launch, review,
closure or publication authority.
"""

from __future__ import annotations

import base64
import binascii
import secrets
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import quote, urlencode

from .candidate_attempts import ReviewProvenance, parse_review
from .candidate_closure import ClosureResolution
from .candidate_history import parse_triage
from .candidate_install_contract import INSTALLER_REPO_PATH, SourceFileBinding, parse_install_proof
from .candidate_store_contract import (
    FAMILY_MAX_BYTES, GIT_OBJECT_RE, MAX_CANDIDATE_COMMITS, STORE_MARKER_NAME,
    AppendAmbiguous, AppendConfirmed, AppendOutcome, CandidateHistoryAtHead, EntryFamily, RawCandidateSnapshot,
    RawTransaction, StoreBoundary, StoredBlob, StoreLocation, Transition, TreeEntry, candidate_path, check_root,
    classify_transition, git_tree_sha, parse_entry_name,
)
from .candidate_store_replay import CandidateSnapshot, admit_append, replay_candidate
from .contract import MAX_OUTPUT, MAX_SOURCE_FILE, VerificationError
from .github import GitHubClient, object_value, positive_id
from .published_contract import git_blob_sha


STAGE = "candidate-store-io"


# One commit-listing page. A GitHub commit row with author and committer user
# objects is a few KiB, so a full page stays well inside the listing bound.
HISTORY_PAGE_SIZE = 100


MAX_HISTORY_PAGE_BYTES = 2 << 20


# One page of effective branch rules; a full page could hide more rules.
RULES_PAGE_SIZE = 100


# A candidate directory holds at most MAX_CANDIDATE_ENTRIES short rows.
MAX_TREE_BYTES = 1 << 20


# Ref, commit, repository, ruleset and created-object replies are small.
MAX_OBJECT_BYTES = MAX_OUTPUT


# The rules that keep the store branch append-only. Each must come from an
# active repository ruleset of this repository that nobody may bypass.
REQUIRED_BRANCH_RULES = frozenset({"deletion", "non_fast_forward", "required_linear_history"})


# Base64 reply envelope: GitHub wraps content every 60 characters, and JSON
# escapes each newline as two bytes.
_BLOB_ENVELOPE_BYTES = 4096


# An installer source is a regular Git file; a link, submodule or directory is not its bytes.
_INSTALLER_SOURCE_KINDS = frozenset({("100644", "blob"), ("100755", "blob")})


# The stored records whose introducing transaction replay needs, by the codec that names their reviewer.
_REVIEWED_PARSERS = {EntryFamily.REVIEW: parse_review, EntryFamily.TRIAGE: parse_triage}


@dataclass(frozen=True)
class _HistoryRow:
    commit: str
    parents: tuple[str, ...]


def _refuse(code: str) -> VerificationError:
    return VerificationError(code, STAGE)


def _git_object(value: object, code: str) -> str:
    if not isinstance(value, str) or GIT_OBJECT_RE.fullmatch(value) is None:
        raise _refuse(code)
    return value


def _base(location: StoreLocation) -> str:
    return f"repos/{location.repository}"


def _require_location(client: object, location: object) -> None:
    if not isinstance(client, GitHubClient) or not isinstance(location, StoreLocation):
        raise _refuse("store_request_invalid")


def load_raw_candidate(client: GitHubClient, location: StoreLocation, candidate_id: str) -> RawCandidateSnapshot:
    """Return the structurally verified history of one candidate at one immutable head.

    Raises VerificationError for every failed or inconclusive read, including
    a request that may have failed only because a path is absent: GitHub
    errors never stand for absence.
    """
    _require_location(client, location)
    path = candidate_path(candidate_id)
    head = _resolve_head(client, location)
    boundary = _verify_boundary(client, location)
    history = _history_at(client, location, candidate_id, path=path, head=head)
    return RawCandidateSnapshot(location=location, candidate_id=candidate_id, head=head,
                                head_tree=history.head_tree, boundary=boundary, transactions=history.transactions)


def load_candidate_history(client: GitHubClient, location: StoreLocation, candidate_id: str, *,
                           head: str) -> CandidateHistoryAtHead:
    """Return the structurally verified history of one candidate at the supplied immutable ``head``.

    It checks the configured repository's identity and then reads exactly
    what the strict load reads at its head, with the same refusals. It never
    reads the branch, its rules or its rulesets, so it proves nothing about
    which commit the store branch names or whether that branch is protected.
    """
    _require_location(client, location)
    path = candidate_path(candidate_id)
    _git_object(head, "store_head_invalid")
    _repository_id(client, location)
    return _history_at(client, location, candidate_id, path=path, head=head)


def _history_at(client: GitHubClient, location: StoreLocation, candidate_id: str, *, path: str,
                head: str) -> CandidateHistoryAtHead:
    """Read the pinned marker, root and one candidate's history at ``head`` and prove their continuity."""
    head_tree = _verify_marker(client, location, head)
    rows = _path_history(client, location, head, path, MAX_CANDIDATE_COMMITS)
    transactions = _transactions(client, location, path, rows)
    if transactions and transactions[-1].commit != head:
        final, _ = _tree(client, location, f"{head}:{path}")
        if final != transactions[-1].tree:
            raise _refuse("store_history_discontinuous")
    return CandidateHistoryAtHead(location=location, candidate_id=candidate_id, head=head, head_tree=head_tree,
                                  transactions=transactions)


def load_candidate(client: GitHubClient, location: StoreLocation, candidate_id: str, *,
                   closure_resolutions: tuple[ClosureResolution, ...]) -> CandidateSnapshot | None:
    """Replay the candidate's fresh verified history; None when it is absent.

    ``closure_resolutions`` are the native lifetime owner's facts for the
    stored closures; a missing, forged or unused fact refuses in replay.
    """
    raw = load_raw_candidate(client, location, candidate_id)
    facts = _facts(client, location, raw, ())
    return replay_candidate(raw, review_provenance=facts.provenance, closure_resolutions=closure_resolutions,
                            installer_sources=facts.installer_sources)


def load_candidate_at_head(client: GitHubClient, location: StoreLocation, candidate_id: str, *, head: str,
                           closure_resolutions: tuple[ClosureResolution, ...]) -> CandidateSnapshot | None:
    """Replay the candidate's verified history at the supplied ``head``; None when it is absent there.

    The result holds semantically validated facts at that head, with the
    introductions and installer sources this edge observed there. It does
    not authenticate ``head`` as the protected store branch head: a commit on
    an unprotected branch that descends from the same genesis loads too. Only
    a separately verified coordinator handoff may accept the result, and
    then only as readiness at that head; it never grants an append, launch,
    freeze or publication.
    """
    history = load_candidate_history(client, location, candidate_id, head=head)
    facts = _facts(client, location, history, ())
    return replay_candidate(history, review_provenance=facts.provenance, closure_resolutions=closure_resolutions,
                            installer_sources=facts.installer_sources)


def load_candidate_at_branch_head(client: GitHubClient, location: StoreLocation, candidate_id: str, *,
                                  closure_resolutions: tuple[ClosureResolution, ...]) -> CandidateSnapshot | None:
    """Replay the candidate's verified history at the branch head H read once; None when it is absent there.

    The request is checked before any read. H is what the branch ref named
    at that one read; every later read is pinned to H, so a ref that moves
    afterwards changes nothing, and the result says nothing about the
    branch now. Errors and absence are exactly those of
    ``load_candidate_at_head`` at H. It reads no rule or ruleset, so it does
    not verify branch protection, and its result never grants an append,
    launch, freeze or publication.
    """
    _require_location(client, location)
    candidate_path(candidate_id)
    head = _resolve_head(client, location)
    return load_candidate_at_head(client, location, candidate_id, head=head, closure_resolutions=closure_resolutions)


def append(client: GitHubClient, location: StoreLocation, candidate_id: str, additions: tuple[StoredBlob, ...], *,
           closure_resolutions: tuple[ClosureResolution, ...], work_directory: Path,
           observe_failure: Callable[[VerificationError], None]) -> AppendOutcome:
    """Append one group on a fresh verified head only if canonical replay admits it there.

    The head is read here, never taken from the caller. Every refusal, a
    ``ReplayDenied`` with its exact findings included, raises unchanged before
    any write; ``work_directory`` and ``observe_failure`` follow the private
    writer's contract. An outcome is a stored fact and grants no launch.
    """
    if not isinstance(additions, tuple) or not all(isinstance(blob, StoredBlob) for blob in additions):
        raise _refuse("store_request_invalid")
    raw = load_raw_candidate(client, location, candidate_id)
    facts = _facts(client, location, raw, additions)
    admit_append(raw, additions, review_provenance=facts.provenance, closure_resolutions=closure_resolutions,
                 installer_sources=facts.installer_sources)
    return _append_transaction(client, location, raw, additions, work_directory=work_directory,
                               observe_failure=observe_failure)


@dataclass(frozen=True)
class _ObservedFacts:
    """The facts this edge observed for one replay: introductions and installer source bytes."""

    provenance: tuple[ReviewProvenance, ...]
    installer_sources: tuple[tuple[SourceFileBinding, bytes], ...]


def _facts(client: GitHubClient, location: StoreLocation, raw: RawCandidateSnapshot | CandidateHistoryAtHead,
           additions: tuple[StoredBlob, ...]) -> _ObservedFacts:
    """Observe the trusted facts of the verified history plus a proposed group.

    Each stored review and triage gets the provenance of the verified commit
    that introduced it, under the reviewer label its codec parses; a proposed
    one has no introduction yet. Each distinct installer source binding is
    read once. Whether a fact is the right one is replay's decision.
    """
    provenance = []
    for transaction in raw.transactions:
        for blob in transaction.blobs:
            parse = _REVIEWED_PARSERS.get(parse_entry_name(blob.entry.name).family)
            if parse is not None:
                provenance.append(ReviewProvenance(artifact_sha256=blob.sha256, reviewer=parse(blob.data).reviewer,
                                                   transaction_commit=transaction.commit))
    sources: dict[SourceFileBinding, bytes] = {}
    for blob in (*(blob for transaction in raw.transactions for blob in transaction.blobs), *additions):
        if parse_entry_name(blob.entry.name).family is EntryFamily.INSTALL_PROOF:
            binding = parse_install_proof(blob.data).installer
            if isinstance(binding, SourceFileBinding) and binding not in sources:
                sources[binding] = _installer_source(client, location, binding)
    return _ObservedFacts(provenance=tuple(provenance), installer_sources=tuple(sources.items()))


def _resolve_head(client: GitHubClient, location: StoreLocation) -> str:
    reply = object_value(client.json(f"{_base(location)}/git/ref/heads/{location.branch}",
                                     limit=MAX_OBJECT_BYTES), STAGE)
    target = object_value(reply.get("object"), STAGE)
    if reply.get("ref") != f"refs/heads/{location.branch}" or target.get("type") != "commit":
        raise _refuse("store_head_invalid")
    return _git_object(target.get("sha"), "store_head_invalid")


def _verify_boundary(client: GitHubClient, location: StoreLocation) -> StoreBoundary:
    """Prove the configured repository and its active no-bypass append-only rules."""
    base = _base(location)
    repository_id = _repository_id(client, location)
    rules = client.json(f"{base}/rules/branches/{location.branch}?per_page={RULES_PAGE_SIZE}",
                        limit=MAX_HISTORY_PAGE_BYTES)
    if not isinstance(rules, list) or len(rules) >= RULES_PAGE_SIZE:
        raise _refuse("store_branch_rules_invalid")
    providers: dict[int, set[str]] = {}
    for value in rules:
        rule = object_value(value, STAGE)
        kind = rule.get("type")
        if (isinstance(kind, str) and kind in REQUIRED_BRANCH_RULES
                and rule.get("ruleset_source_type") == "Repository"
                and rule.get("ruleset_source") == location.repository):
            providers.setdefault(positive_id(rule.get("ruleset_id"), STAGE), set()).add(kind)
    trusted = tuple(ruleset_id for ruleset_id in sorted(providers)
                    if _ruleset_trusted(client, location, ruleset_id))
    if set().union(*(providers[ruleset_id] for ruleset_id in trusted)) != REQUIRED_BRANCH_RULES:
        raise _refuse("store_branch_rules_missing")
    return StoreBoundary(repository_id=repository_id, ruleset_ids=trusted)


def _repository_id(client: GitHubClient, location: StoreLocation) -> int:
    """The identity of the configured repository, read through the authenticated client."""
    repository = object_value(client.json(_base(location), limit=MAX_OBJECT_BYTES), STAGE)
    if repository.get("full_name") != location.repository:
        raise _refuse("store_repository_mismatch")
    return positive_id(repository.get("id"), STAGE)


def _ruleset_trusted(client: GitHubClient, location: StoreLocation, ruleset_id: int) -> bool:
    """An active repository branch ruleset whose bypass list is visible and empty."""
    ruleset = object_value(client.json(f"{_base(location)}/rulesets/{ruleset_id}", limit=MAX_OBJECT_BYTES),
                           STAGE)
    return (ruleset.get("id") == ruleset_id and ruleset.get("enforcement") == "active"
            and ruleset.get("target") == "branch" and ruleset.get("source_type") == "Repository"
            and ruleset.get("source") == location.repository and ruleset.get("bypass_actors") == [])


def _verify_marker(client: GitHubClient, location: StoreLocation, head: str) -> str:
    """Prove the pinned orphan genesis and unchanged marker; return H's root tree.

    The marker's complete history at H must be exactly the genesis commit,
    whose root holds the marker alone, and H's own root must pass the root
    check. A rewritten, deleted or restored marker leaves extra history.
    """
    rows = _path_history(client, location, head, STORE_MARKER_NAME, HISTORY_PAGE_SIZE - 1)
    if rows != (_HistoryRow(location.genesis_commit, ()),):
        raise _refuse("store_genesis_mismatch")
    genesis_tree, genesis_parents = _commit(client, location, location.genesis_commit)
    _, genesis_entries = _tree(client, location, genesis_tree)
    if genesis_parents or genesis_entries != (TreeEntry(STORE_MARKER_NAME, "100644", "blob", location.marker_blob),):
        raise _refuse("store_genesis_mismatch")
    head_tree, _ = _commit(client, location, head)
    _require_root(client, location, head_tree)
    return head_tree


def _require_root(client: GitHubClient, location: StoreLocation, root_tree: str) -> None:
    _, entries = _tree(client, location, root_tree)
    check_root(entries, marker_blob=location.marker_blob, truncated=False)


def _commit(client: GitHubClient, location: StoreLocation, commit: str) -> tuple[str, tuple[str, ...]]:
    """Return a commit's root tree and its actual parent edges."""
    reply = object_value(client.json(f"{_base(location)}/git/commits/{commit}", limit=MAX_OBJECT_BYTES), STAGE)
    tree = object_value(reply.get("tree"), STAGE)
    if reply.get("sha") != commit:
        raise _refuse("store_commit_invalid")
    return _git_object(tree.get("sha"), "store_commit_invalid"), _parents(reply.get("parents"))


def _parents(value: object) -> tuple[str, ...]:
    if not isinstance(value, list):
        raise _refuse("store_commit_invalid")
    return tuple(_git_object(object_value(item, STAGE).get("sha"), "store_commit_invalid") for item in value)


def _tree(client: GitHubClient, location: StoreLocation, tree_ish: str) -> tuple[str, tuple[TreeEntry, ...]]:
    """Return one complete tree listing whose entries recompute its identity.

    ``tree_ish`` is a tree SHA or a ``<commit>:<path>`` route. A failed
    request refuses: it may equally be a transport fault or a missing path.
    """
    reply = client.json(f"{_base(location)}/git/trees/{quote(tree_ish, safe='')}", limit=MAX_TREE_BYTES)
    sha, entries = _listing(reply)
    if ":" not in tree_ish and sha != tree_ish:
        raise _refuse("store_tree_mismatch")
    return sha, entries


def _listing(value: object) -> tuple[str, tuple[TreeEntry, ...]]:
    """Parse a trees API reply; it must be complete and hash to its own SHA."""
    reply = object_value(value, STAGE)
    sha = _git_object(reply.get("sha"), "store_tree_invalid")
    rows = reply.get("tree")
    if reply.get("truncated") is not False or not isinstance(rows, list):
        raise _refuse("store_tree_truncated")
    entries = tuple(_tree_entry(row) for row in rows)
    if git_tree_sha(entries) != sha:
        raise _refuse("store_tree_mismatch")
    return sha, entries


def _tree_entry(value: object) -> TreeEntry:
    row = object_value(value, STAGE)
    fields = tuple(row.get(key) for key in ("path", "mode", "type", "sha"))
    name, mode, kind, sha = (field if isinstance(field, str) else "" for field in fields)
    return TreeEntry(name=name, mode=mode, type=kind, sha=sha)


def _path_history(client: GitHubClient, location: StoreLocation, head: str, path: str,
                  bound: int) -> tuple[_HistoryRow, ...]:
    """Return every commit at ``head`` that changed ``path``, oldest first.

    Pagination must end on a short (possibly empty) page within ``bound``
    rows; a repeated commit or an overlong history refuses. Ordering is
    checked later against actual parent edges, never commit dates.
    """
    rows: list[_HistoryRow] = []
    for page in range(1, bound // HISTORY_PAGE_SIZE + 2):
        query = urlencode({"sha": head, "path": path, "per_page": HISTORY_PAGE_SIZE, "page": page})
        listing = client.json(f"{_base(location)}/commits?{query}", limit=MAX_HISTORY_PAGE_BYTES)
        if not isinstance(listing, list) or len(listing) > HISTORY_PAGE_SIZE:
            raise _refuse("store_history_invalid")
        for value in listing:
            row = object_value(value, STAGE)
            rows.append(_HistoryRow(_git_object(row.get("sha"), "store_history_invalid"),
                                    _parents(row.get("parents"))))
        if len({row.commit for row in rows}) != len(rows):
            raise _refuse("store_history_duplicate")
        if len(rows) > bound:
            raise _refuse("store_history_too_long")
        if len(listing) < HISTORY_PAGE_SIZE:
            return tuple(reversed(rows))
    raise _refuse("store_history_too_long")


def _transactions(client: GitHubClient, location: StoreLocation, path: str,
                  rows: tuple[_HistoryRow, ...]) -> tuple[RawTransaction, ...]:
    """Replay each listed commit against its actual single parent.

    The first parent's root must pass the root check; history before it
    holds no candidate directory because the complete listing reaches back
    to the verified genesis. Each later parent must hold exactly the
    directory the previous listed commit produced.
    """
    blobs: dict[str, bytes] = {}
    transactions: list[RawTransaction] = []
    entries: frozenset[TreeEntry] = frozenset()
    for row in rows:
        if len(row.parents) != 1:
            raise _refuse("store_commit_not_linear")
        parent = row.parents[0]
        previous = transactions[-1] if transactions else None
        if previous is None:
            parent_tree = None
            _require_root(client, location, _commit(client, location, parent)[0])
        elif parent == previous.commit:
            parent_tree = previous.tree
        else:
            parent_tree, _ = _tree(client, location, f"{parent}:{path}")
            if parent_tree != previous.tree:
                raise _refuse("store_history_discontinuous")
        tree, listing = _tree(client, location, f"{row.commit}:{path}")
        child = frozenset(listing)
        if len(child) != len(listing):
            raise _refuse("store_tree_invalid")
        transition = classify_transition(entries, child)
        stored = tuple(_stored_blob(client, location, entry, blobs) for entry in transition.added)
        transactions.append(RawTransaction(commit=row.commit, parent=parent, parent_tree=parent_tree, tree=tree,
                                           transition=transition, blobs=stored))
        entries = child
    return tuple(transactions)


def _stored_blob(client: GitHubClient, location: StoreLocation, entry: TreeEntry,
                 fetched: dict[str, bytes]) -> StoredBlob:
    """Fetch one record's bytes at most once per load and prove their identity.

    A family without a codec bound refuses before any request is made.
    """
    limit = FAMILY_MAX_BYTES.get(parse_entry_name(entry.name).family)
    if limit is None:
        raise _refuse("store_family_codec_missing")
    if entry.sha not in fetched:
        fetched[entry.sha] = _blob(client, location, entry.sha, limit)
    return StoredBlob(entry=entry, data=fetched[entry.sha])


def _blob_reply_limit(size: int) -> int:
    encoded = 4 * ((size + 2) // 3)
    return encoded + 2 * (encoded // 60 + 1) + _BLOB_ENVELOPE_BYTES


def _blob(client: GitHubClient, location: StoreLocation, sha: str, limit: int) -> bytes:
    reply = object_value(client.json(f"{_base(location)}/git/blobs/{sha}", limit=_blob_reply_limit(limit)),
                         STAGE)
    content, size = reply.get("content"), reply.get("size")
    if reply.get("sha") != sha or reply.get("encoding") != "base64" or not isinstance(content, str):
        raise _refuse("store_blob_invalid")
    try:
        data = base64.b64decode(content.replace("\n", ""), validate=True)
    except (binascii.Error, ValueError):
        raise _refuse("store_blob_invalid") from None
    if isinstance(size, bool) or size != len(data):
        raise _refuse("store_blob_invalid")
    return data


def _installer_source(client: GitHubClient, location: StoreLocation, binding: SourceFileBinding) -> bytes:
    """Read the exact installer file at the binding's own source commit in the store repository.

    This proves only which bytes that commit holds; whether the commit and
    digest are the candidate's stays with the canonical install reader. Only
    a complete verified root listing without the file is missing; every
    failed read refuses as itself.
    """
    if binding.repo_path != INSTALLER_REPO_PATH:
        raise _refuse("store_installer_source_invalid")
    root, _ = _commit(client, location, binding.source_commit)
    _, entries = _tree(client, location, root)
    entry = next((item for item in entries if item.name == INSTALLER_REPO_PATH), None)
    if entry is None:
        raise _refuse("store_installer_source_missing")
    if (entry.mode, entry.type) not in _INSTALLER_SOURCE_KINDS:
        raise _refuse("store_installer_source_invalid")
    data = _blob(client, location, entry.sha, MAX_SOURCE_FILE)
    if len(data) > MAX_SOURCE_FILE or git_blob_sha(data) != entry.sha:
        raise _refuse("store_installer_source_invalid")
    return data


@dataclass
class _RequestFiles:
    """Names this execution's request bodies inside the caller-owned directory.

    The writer creates them through the transport and never removes them; the
    caller retains or disposes of the directory under its closure policy.
    """

    directory: Path
    nonce: str
    count: int = 0

    def next(self, label: str) -> Path:
        self.count += 1
        return self.directory / f"store-append-{self.nonce[:16]}-{self.count:02d}-{label}.json"


def _append_transaction(client: GitHubClient, location: StoreLocation, snapshot: RawCandidateSnapshot,
                        additions: tuple[StoredBlob, ...], *, work_directory: Path,
                        observe_failure: Callable[[VerificationError], None]) -> AppendOutcome:
    """Append one structural transition after ``snapshot.head`` with a single ref update.

    Private: a structurally valid group may still be semantically forbidden,
    so only the replay-admitting ``append`` may call this. Any refusal
    before the ref update raises unchanged; the caller observes it with the
    owner of ``work_directory`` before disposing of it. After the update may
    have run, only an exact matching acknowledgement confirms; every other
    result is ambiguous and is never resolved by a readback here.

    ``observe_failure`` is that same lifetime owner's observation (for
    example ``FixtureTree.observe``). A failed update is the one exception
    this writer turns into a value, so the owner receives the original
    exception, with its cause chain, before ``AppendAmbiguous`` is returned.
    """
    transition = _planned(client, location, snapshot, additions, work_directory, observe_failure)
    nonce = secrets.token_hex(32)
    files = _RequestFiles(work_directory, nonce)
    base = _base(location)
    for blob in {blob.entry.sha: blob for blob in additions}.values():
        created = object_value(client.json_body(
            f"{base}/git/blobs", {"content": base64.b64encode(blob.data).decode("ascii"), "encoding": "base64"},
            method="POST", body_path=files.next("blob"), limit=MAX_OBJECT_BYTES), STAGE)
        if created.get("sha") != blob.entry.sha:
            raise _refuse("store_append_object_mismatch")
    commit = _proposed_commit(client, location, snapshot, transition, nonce, files)
    digests = tuple(sorted({blob.sha256 for blob in additions}))
    try:
        reply = client.json_body(f"{base}/git/refs/heads/{location.branch}", {"sha": commit, "force": False},
                                 method="PATCH", body_path=files.next("ref"), limit=MAX_OBJECT_BYTES)
    except VerificationError as error:
        observe_failure(error)
        return AppendAmbiguous(parent=snapshot.head, proposed=commit, nonce=nonce, record_sha256s=digests,
                               failure=error.code)
    if not _acknowledged(reply, location, commit):
        return AppendAmbiguous(parent=snapshot.head, proposed=commit, nonce=nonce, record_sha256s=digests,
                               failure="store_append_ack_mismatch")
    return AppendConfirmed(parent=snapshot.head, commit=commit, kind=transition.kind, record_sha256s=digests)


def _planned(client: object, location: object, snapshot: object, additions: object,
             work_directory: object, observe_failure: object) -> Transition:
    """Refuse, before any request, additions that are not one exact new group."""
    _require_location(client, location)
    if (not isinstance(snapshot, RawCandidateSnapshot) or snapshot.location != location
            or not isinstance(work_directory, Path) or not callable(observe_failure)
            or not isinstance(additions, tuple) or not additions
            or not all(isinstance(blob, StoredBlob) for blob in additions)):
        raise _refuse("store_request_invalid")
    entries = tuple(blob.entry for blob in additions)
    transition = classify_transition(snapshot.entries, snapshot.entries | frozenset(entries))
    if transition.added != tuple(sorted(entries, key=lambda entry: entry.name)):
        raise _refuse("store_transition_invalid")
    return transition


def _proposed_commit(client: GitHubClient, location: StoreLocation, snapshot: RawCandidateSnapshot,
                     transition: Transition, nonce: str, files: _RequestFiles) -> str:
    """Create and verify the child commit of H that adds exactly the transition."""
    base, path = _base(location), candidate_path(snapshot.candidate_id)
    rows = [{"path": f"{path}/{entry.name}", "mode": entry.mode, "type": entry.type, "sha": entry.sha}
            for entry in transition.added]
    root, root_entries = _listing(client.json_body(
        f"{base}/git/trees", {"base_tree": snapshot.head_tree, "tree": rows},
        method="POST", body_path=files.next("tree"), limit=MAX_OBJECT_BYTES))
    check_root(root_entries, marker_blob=location.marker_blob, truncated=False)
    # No terminal LF: GitHub's create and read replies omit one, and the reply must equal this exactly.
    message = (f"Append {transition.kind.value} to development candidate {snapshot.candidate_id}\n\n"
               f"Store-Nonce: {nonce}")
    created = object_value(client.json_body(
        f"{base}/git/commits", {"message": message, "tree": root, "parents": [snapshot.head]},
        method="POST", body_path=files.next("commit"), limit=MAX_OBJECT_BYTES), STAGE)
    commit = _git_object(created.get("sha"), "store_append_object_mismatch")
    tree = created.get("tree")
    if (not isinstance(tree, dict) or tree.get("sha") != root or created.get("message") != message
            or _parents(created.get("parents")) != (snapshot.head,)):
        raise _refuse("store_append_object_mismatch")
    expected = git_tree_sha(tuple(snapshot.entries | frozenset(transition.added)))
    if _tree(client, location, f"{commit}:{path}")[0] != expected:
        raise _refuse("store_append_object_mismatch")
    return commit


def _acknowledged(reply: object, location: StoreLocation, commit: str) -> bool:
    if not isinstance(reply, dict):
        return False
    target = reply.get("object")
    return (reply.get("ref") == f"refs/heads/{location.branch}" and isinstance(target, dict)
            and target.get("sha") == commit)
