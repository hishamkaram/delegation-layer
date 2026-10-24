"""Pure structure and validity of the append-only development candidate store.

The store is one branch. Its root tree holds exactly ``STORE.json``, the
pinned marker blob, and optionally a ``candidates`` tree with one flat
directory per candidate id. This module owns Git object identity at that
boundary, the closed entry-name grammar, per-family byte bounds and the
structural append-only rule between two candidate directory states.

It performs no I/O and decodes no record or ZIP. Whether a transition is
allowed for the candidate's history (registration first, a claimed platform
before its proof, nothing after the freeze), triage references, review
provenance, readiness and freeze authority stay with the runtime replay, the
codec owners and ``candidate_readiness``. Append outcomes are facts; none of
them grants a launch.
"""

from __future__ import annotations

from dataclasses import dataclass
from enum import Enum
import hashlib
import re

from ..native_evidence.public_contract import MAX_PUBLIC_ZIP_BYTES
from .candidate_attempts import MAX_CLAIM_BYTES, MAX_RECORD_BYTES
from .candidate_history import MAX_PUBLICATION_BYTES, FindingRef
from .candidate_install_contract import MAX_INSTALL_PROOF_BYTES, MAX_INSTALL_WITNESS_BYTES, InstallChannel
from .candidate_manifest import MAX_LOCATOR_BYTES, MAX_MANIFEST_BYTES, PLATFORMS, REPOSITORY_RE
from .contract import HEX_SHA256, VerificationError
from .published_contract import git_blob_sha


STAGE = "validate-candidate-store"


STORE_MARKER_NAME = "STORE.json"


CANDIDATES_TREE_NAME = "candidates"


# Per-candidate bounds a reader enforces on one candidate's path history and
# directory; they are not limits on the whole branch or on product tasks.
MAX_CANDIDATE_COMMITS = 512


MAX_CANDIDATE_ENTRIES = 1024


# GitHub repositories use SHA-1 object names (verified by the R3 and R7 runs).
GIT_OBJECT_RE = re.compile(r"^[0-9a-f]{40}$")


_RECORD_MODE = "100644"


_TREE_MODE = "040000"


# The (mode, type) pairs a Git tree entry may have, in the form the GitHub
# trees API reports them. A store reader still refuses all but blobs of
# ``_RECORD_MODE`` and directories of ``_TREE_MODE``.
GIT_ENTRY_KINDS = frozenset({
    ("100644", "blob"), ("100755", "blob"), ("120000", "blob"), (_TREE_MODE, "tree"), ("160000", "commit"),
})


_BRANCH_SEGMENT_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]*(\.[A-Za-z0-9_-]+)*$")


def candidate_commit_count_valid(count: int) -> bool:
    """One candidate's path history may hold ``count`` commits; a store append checks its resulting count."""
    return type(count) is int and 0 <= count <= MAX_CANDIDATE_COMMITS


def _git_object(value: object) -> bool:
    return isinstance(value, str) and GIT_OBJECT_RE.fullmatch(value) is not None


def _branch_valid(value: object) -> bool:
    """Git refuses a branch any of whose slash-separated components ends in ".lock"."""
    return (isinstance(value, str) and 0 < len(value) <= 200
            and all(_BRANCH_SEGMENT_RE.fullmatch(segment) is not None and not segment.endswith(".lock")
                    for segment in value.split("/")))


@dataclass(frozen=True)
class StoreLocation:
    """The one store branch and the trusted genesis and marker it must keep."""

    repository: str
    branch: str
    genesis_commit: str
    marker_blob: str

    def __post_init__(self) -> None:
        if (not isinstance(self.repository, str) or REPOSITORY_RE.fullmatch(self.repository) is None
                or not _branch_valid(self.branch) or not _git_object(self.genesis_commit)
                or not _git_object(self.marker_blob)):
            raise VerificationError("store_location_invalid", STAGE)


@dataclass(frozen=True)
class TreeEntry:
    """One direct entry of a Git tree as the trees API lists it."""

    name: str
    mode: str
    type: str
    sha: str

    def __post_init__(self) -> None:
        if (not isinstance(self.name, str) or not 0 < len(self.name.encode()) <= 255
                or any(char in self.name for char in "/\0") or self.name in {".", ".."}
                or (self.mode, self.type) not in GIT_ENTRY_KINDS or not _git_object(self.sha)):
            raise VerificationError("store_tree_entry_invalid", STAGE)


def _unique_names(entries: frozenset[TreeEntry] | tuple[TreeEntry, ...]) -> bool:
    return (all(isinstance(entry, TreeEntry) for entry in entries)
            and len({entry.name for entry in entries}) == len(entries))


def git_tree_sha(entries: tuple[TreeEntry, ...]) -> str:
    """Return the Git SHA-1 object name of a tree with exactly these direct entries.

    Git orders entries by name bytes, comparing a directory as if its name
    ended in "/", and writes a directory's mode without its leading zero.
    """
    if not isinstance(entries, tuple) or not _unique_names(entries):
        raise VerificationError("store_tree_invalid", STAGE)
    ordered = sorted(entries, key=lambda entry: entry.name.encode() + (b"/" if entry.type == "tree" else b""))
    body = b"".join(entry.mode.lstrip("0").encode() + b" " + entry.name.encode() + b"\0" + bytes.fromhex(entry.sha)
                    for entry in ordered)
    return hashlib.sha1(b"tree %d\0" % len(body) + body, usedforsecurity=False).hexdigest()


def _require_listing(entries: tuple[TreeEntry, ...], truncated: bool) -> None:
    """A tree listing is complete and names each entry once."""
    if truncated is not False:
        raise VerificationError("store_tree_truncated", STAGE)
    if not isinstance(entries, tuple) or not _unique_names(entries):
        raise VerificationError("store_tree_invalid", STAGE)


def check_root(entries: tuple[TreeEntry, ...], *, marker_blob: str, truncated: bool) -> str | None:
    """Return the ``candidates`` tree SHA, or None when no candidate exists yet.

    The root must be exactly the pinned marker blob plus at most a candidates
    directory. Only after this positive check may an empty candidate path
    history be read as absence: a blob, symlink or extra entry in its place
    refuses instead.
    """
    if not _git_object(marker_blob):
        raise VerificationError("store_location_invalid", STAGE)
    _require_listing(entries, truncated)
    if TreeEntry(STORE_MARKER_NAME, _RECORD_MODE, "blob", marker_blob) not in entries:
        raise VerificationError("store_marker_mismatch", STAGE)
    others = [entry for entry in entries if entry.name != STORE_MARKER_NAME]
    if not others:
        return None
    if len(others) != 1 or (others[0].name, others[0].mode, others[0].type) != (CANDIDATES_TREE_NAME, _TREE_MODE, "tree"):
        raise VerificationError("store_root_invalid", STAGE)
    return others[0].sha


class EntryFamily(Enum):
    MANIFEST = "manifest"
    LOCATOR = "locator"
    INTENT = "intent"
    CLAIM = "claim"
    PROOF = "proof"
    TRIAGE = "triage"
    REVIEW = "review"
    CLOSURE = "closure"
    INSTALL_PROOF = "install-proof"
    INSTALL_WITNESS = "install-witness"
    PUBLICATION = "publication"


_FIXED_NAMES = {
    EntryFamily.MANIFEST: "manifest.json", EntryFamily.LOCATOR: "locator.json",
    EntryFamily.PUBLICATION: "publication.json",
}


_ATTEMPT_FAMILIES = frozenset({EntryFamily.INTENT, EntryFamily.CLAIM, EntryFamily.PROOF})


_PLATFORM_FAMILIES = frozenset({
    EntryFamily.CLAIM, EntryFamily.PROOF, EntryFamily.INSTALL_PROOF, EntryFamily.INSTALL_WITNESS,
})


_DIGEST_FAMILIES = frozenset({EntryFamily.TRIAGE, EntryFamily.REVIEW, EntryFamily.CLOSURE})


_INSTALL_FAMILIES = frozenset({EntryFamily.INSTALL_PROOF, EntryFamily.INSTALL_WITNESS})


# Attempt numbers are zero-padded to at least this many digits; larger numbers
# keep all their digits, so rendering never truncates or caps.
ATTEMPT_DIGITS = 4


@dataclass(frozen=True)
class EntryName:
    """The parsed identity of one candidate directory entry.

    Only the fields of ``family`` are set: an attempt for intents, claims and
    proofs; an (os, arch) platform for claims, proofs and install files; a
    channel for install files; a SHA-256 for triages, reviews and closures.
    """

    family: EntryFamily
    attempt: int | None = None
    platform: tuple[str, str] | None = None
    channel: InstallChannel | None = None
    digest: str | None = None

    def __post_init__(self) -> None:
        family = self.family
        if (not isinstance(family, EntryFamily)
                or (self.attempt is not None) != (family in _ATTEMPT_FAMILIES)
                or (self.platform is not None) != (family in _PLATFORM_FAMILIES)
                or (self.channel is not None) != (family in _INSTALL_FAMILIES)
                or (self.digest is not None) != (family in _DIGEST_FAMILIES)
                or (self.attempt is not None
                    and (isinstance(self.attempt, bool) or not isinstance(self.attempt, int) or self.attempt < 1))
                or (self.platform is not None and self.platform not in PLATFORMS)
                or (self.channel is not None and not isinstance(self.channel, InstallChannel))
                or (self.digest is not None
                    and (not isinstance(self.digest, str) or HEX_SHA256.fullmatch(self.digest) is None))):
            raise VerificationError("store_entry_name_invalid", STAGE)


def entry_name(name: EntryName) -> str:
    """Render the only file name of an entry identity; ``parse_entry_name`` inverts it."""
    family = name.family
    if family in _FIXED_NAMES:
        return _FIXED_NAMES[family]
    if family in _DIGEST_FAMILIES:
        return f"{family.value}.{name.digest}.json"
    platform = f"{name.platform[0]}-{name.platform[1]}" if name.platform is not None else ""
    if family in _INSTALL_FAMILIES and name.channel is not None:
        role = "proof" if family is EntryFamily.INSTALL_PROOF else "witness"
        return f"install.{platform}.{name.channel.value}.{role}.json"
    attempt = f"attempt-{name.attempt:0{ATTEMPT_DIGITS}d}"
    if family is EntryFamily.INTENT:
        return f"{attempt}.intent.json"
    if family is EntryFamily.CLAIM:
        return f"{attempt}.claim.{platform}.json"
    return f"{attempt}.proof.{platform}.zip"


_PLATFORM_TOKENS = {f"{os_name}-{arch}": (os_name, arch) for os_name, arch in PLATFORMS}


_CHANNEL_TOKENS = {channel.value: channel for channel in InstallChannel}


_ENTRY_PATTERNS = (
    (EntryFamily.INTENT, re.compile(r"attempt-([0-9]+)\.intent\.json")),
    (EntryFamily.CLAIM, re.compile(r"attempt-([0-9]+)\.claim\.([A-Za-z0-9]+-[A-Za-z0-9]+)\.json")),
    (EntryFamily.PROOF, re.compile(r"attempt-([0-9]+)\.proof\.([A-Za-z0-9]+-[A-Za-z0-9]+)\.zip")),
    (EntryFamily.TRIAGE, re.compile(r"triage\.([0-9a-f]{64})\.json")),
    (EntryFamily.REVIEW, re.compile(r"review\.([0-9a-f]{64})\.json")),
    (EntryFamily.CLOSURE, re.compile(r"closure\.([0-9a-f]{64})\.json")),
    (EntryFamily.INSTALL_PROOF, re.compile(r"install\.([A-Za-z0-9]+-[A-Za-z0-9]+)\.([a-z]+)\.proof\.json")),
    (EntryFamily.INSTALL_WITNESS, re.compile(r"install\.([A-Za-z0-9]+-[A-Za-z0-9]+)\.([a-z]+)\.witness\.json")),
)


def _parsed_name(family: EntryFamily, groups: tuple[str, ...]) -> EntryName | None:
    if family in _DIGEST_FAMILIES:
        return EntryName(family, digest=groups[0])
    if family in _INSTALL_FAMILIES:
        platform, channel = _PLATFORM_TOKENS.get(groups[0]), _CHANNEL_TOKENS.get(groups[1])
        if platform is None or channel is None:
            return None
        return EntryName(family, platform=platform, channel=channel)
    attempt = int(groups[0])
    if attempt < 1:
        return None
    if family is EntryFamily.INTENT:
        return EntryName(family, attempt=attempt)
    platform = _PLATFORM_TOKENS.get(groups[1])
    return None if platform is None else EntryName(family, attempt=attempt, platform=platform)


def parse_entry_name(value: object) -> EntryName:
    """Return the identity of a canonical entry file name; anything else refuses."""
    if isinstance(value, str):
        fixed = next((family for family, text in _FIXED_NAMES.items() if text == value), None)
        if fixed is not None:
            return EntryName(fixed)
        for family, pattern in _ENTRY_PATTERNS:
            match = pattern.fullmatch(value)
            parsed = None if match is None else _parsed_name(family, match.groups())
            if parsed is not None and entry_name(parsed) == value:
                return parsed
    raise VerificationError("store_entry_name_invalid", STAGE)


# The byte bound of each family is the bound of the codec that owns it; the
# publication bound belongs to candidate_history. A blob within its bound is
# structurally admitted bytes only: it never implies a valid freeze record,
# a publication authorization, or readiness.
FAMILY_MAX_BYTES = {
    EntryFamily.MANIFEST: MAX_MANIFEST_BYTES, EntryFamily.LOCATOR: MAX_LOCATOR_BYTES,
    EntryFamily.INTENT: MAX_RECORD_BYTES, EntryFamily.CLAIM: MAX_CLAIM_BYTES,
    EntryFamily.PROOF: MAX_PUBLIC_ZIP_BYTES, EntryFamily.TRIAGE: MAX_RECORD_BYTES,
    EntryFamily.REVIEW: MAX_RECORD_BYTES, EntryFamily.CLOSURE: MAX_RECORD_BYTES,
    EntryFamily.PUBLICATION: MAX_PUBLICATION_BYTES,
    EntryFamily.INSTALL_PROOF: MAX_INSTALL_PROOF_BYTES, EntryFamily.INSTALL_WITNESS: MAX_INSTALL_WITNESS_BYTES,
}


def _record_entry(entry: object) -> EntryName:
    """A candidate directory holds only regular, non-executable files with canonical names."""
    if not isinstance(entry, TreeEntry) or (entry.mode, entry.type) != (_RECORD_MODE, "blob"):
        raise VerificationError("store_entry_invalid", STAGE)
    return parse_entry_name(entry.name)


def require_blob(entry: TreeEntry, data: bytes) -> EntryName:
    """Return the entry's identity once ``data`` is its exact bounded Git blob."""
    name = _record_entry(entry)
    limit = FAMILY_MAX_BYTES.get(name.family)
    if limit is None:
        raise VerificationError("store_family_codec_missing", STAGE)
    if not isinstance(data, bytes) or len(data) > limit:
        raise VerificationError("store_blob_too_large", STAGE)
    if git_blob_sha(data) != entry.sha:
        raise VerificationError("store_blob_mismatch", STAGE)
    return name


class TransitionKind(Enum):
    REGISTER = "register"
    INTENT = "intent"
    CLAIM = "claim"
    PROOF = "proof"
    INSTALL = "install"
    TRIAGE = "triage"
    REVIEW = "review"
    FREEZE = "freeze"


_SINGLE_FAMILY_KINDS = {
    EntryFamily.INTENT: TransitionKind.INTENT, EntryFamily.CLAIM: TransitionKind.CLAIM,
    EntryFamily.PROOF: TransitionKind.PROOF, EntryFamily.REVIEW: TransitionKind.REVIEW,
    EntryFamily.PUBLICATION: TransitionKind.FREEZE,
}


@dataclass(frozen=True)
class Transition:
    """One structural append: its kind and the added entries, ordered by file name."""

    kind: TransitionKind
    added: tuple[TreeEntry, ...]


def _candidate_directory(entries: object) -> dict[str, EntryName]:
    if (not isinstance(entries, frozenset) or len(entries) > MAX_CANDIDATE_ENTRIES
            or not _unique_names(entries)):
        raise VerificationError("store_candidate_tree_invalid", STAGE)
    return {entry.name: _record_entry(entry) for entry in entries}


def _transition_kind(names: list[EntryName]) -> TransitionKind | None:
    families = sorted(name.family.value for name in names)
    if len(names) == 1 and names[0].family in _SINGLE_FAMILY_KINDS:
        return _SINGLE_FAMILY_KINDS[names[0].family]
    if families == ["locator", "manifest"]:
        return TransitionKind.REGISTER
    if families == ["install-proof", "install-witness"]:
        first, second = names
        same = (first.platform, first.channel) == (second.platform, second.channel)
        return TransitionKind.INSTALL if same else None
    if families.count("triage") == 1 and all(family in {"triage", "closure"} for family in families):
        return TransitionKind.TRIAGE
    return None


def classify_transition(parent: frozenset[TreeEntry], child: frozenset[TreeEntry]) -> Transition:
    """Classify one append between consecutive states of one candidate directory.

    The child keeps every parent entry identical and adds exactly one allowed
    group; a replaced, deleted, renamed or re-moded entry, an empty change or
    any other group refuses. Whether the group is allowed at this point of
    the candidate's history is the replay owner's decision.
    """
    _candidate_directory(parent)
    names = _candidate_directory(child)
    if not parent <= child:
        raise VerificationError("store_entry_rewritten", STAGE)
    added = tuple(sorted(child - parent, key=lambda entry: entry.name))
    kind = _transition_kind([names[entry.name] for entry in added]) if added else None
    if kind is None:
        raise VerificationError("store_transition_invalid", STAGE)
    return Transition(kind=kind, added=added)


def candidate_path(candidate_id: str) -> str:
    """The store path of one candidate directory; the id is its manifest SHA-256."""
    if not isinstance(candidate_id, str) or HEX_SHA256.fullmatch(candidate_id) is None:
        raise VerificationError("store_candidate_id_invalid", STAGE)
    return f"{CANDIDATES_TREE_NAME}/{candidate_id}"


@dataclass(frozen=True)
class StoredBlob:
    """One candidate record file: its checked tree entry and exact bounded bytes.

    Construction proves only Git identity, the canonical name and the family
    byte bound. The bytes are not decoded; their meaning stays with the codecs.
    """

    entry: TreeEntry
    data: bytes

    def __post_init__(self) -> None:
        if not isinstance(self.entry, TreeEntry):
            raise VerificationError("store_entry_invalid", STAGE)
        require_blob(self.entry, self.data)

    @property
    def sha256(self) -> str:
        return hashlib.sha256(self.data).hexdigest()


def record_blob(name: EntryName, data: bytes) -> StoredBlob:
    """The record file of ``data`` under the canonical name of ``name``; StoredBlob checks the rest."""
    if not isinstance(name, EntryName) or not isinstance(data, bytes):
        raise VerificationError("store_entry_invalid", STAGE)
    return StoredBlob(TreeEntry(entry_name(name), _RECORD_MODE, "blob", git_blob_sha(data)), data)


@dataclass(frozen=True)
class RawTransaction:
    """One verified single-parent commit that appended to one candidate directory.

    ``parent_tree`` and ``tree`` are the directory tree identities the reader
    observed at ``parent`` and ``commit``; ``parent_tree`` is None before the
    first append. ``blobs`` hold exactly the entries the transition added.
    """

    commit: str
    parent: str
    parent_tree: str | None
    tree: str
    transition: Transition
    blobs: tuple[StoredBlob, ...]

    def __post_init__(self) -> None:
        if (not _git_object(self.commit) or not _git_object(self.parent) or self.commit == self.parent
                or (self.parent_tree is not None and not _git_object(self.parent_tree))
                or not _git_object(self.tree) or self.tree == self.parent_tree
                or not isinstance(self.transition, Transition) or not isinstance(self.blobs, tuple)
                or not all(isinstance(blob, StoredBlob) for blob in self.blobs)
                or tuple(blob.entry for blob in self.blobs) != self.transition.added):
            raise VerificationError("store_transaction_invalid", STAGE)


@dataclass(frozen=True)
class StoreBoundary:
    """Repository facts the reader observed through its authenticated client.

    They describe the store as it is now (repository identity and the active
    no-bypass rulesets that keep the branch append-only). They never name the
    principal who wrote any historical commit.
    """

    repository_id: int
    ruleset_ids: tuple[int, ...]

    def __post_init__(self) -> None:
        values = (self.repository_id, *self.ruleset_ids) if isinstance(self.ruleset_ids, tuple) else ()
        if (not values or not self.ruleset_ids
                or any(isinstance(value, bool) or not isinstance(value, int) or value < 1 for value in values)
                or list(self.ruleset_ids) != sorted(set(self.ruleset_ids))):
            raise VerificationError("store_boundary_invalid", STAGE)


@dataclass(frozen=True)
class RawCandidateSnapshot:
    """The structurally verified history of one candidate directory at head ``head``.

    Transactions are ordered by their actual parent edges, from the first
    append. An empty history means the candidate is absent at ``head``. The
    snapshot is a fact about stored bytes: it decides no launch, review,
    closure, readiness or publication.
    """

    location: StoreLocation
    candidate_id: str
    head: str
    head_tree: str
    boundary: StoreBoundary
    transactions: tuple[RawTransaction, ...]

    def __post_init__(self) -> None:
        if not isinstance(self.boundary, StoreBoundary):
            raise VerificationError("store_snapshot_invalid", STAGE)
        _validate_history(self)

    @property
    def entries(self) -> frozenset[TreeEntry]:
        """The candidate directory at ``head``: every entry the transactions added."""
        return _entries(self.transactions)


@dataclass(frozen=True)
class CandidateHistoryAtHead:
    """The structurally verified history of one candidate directory at a supplied commit ``head``.

    It holds the same validated facts as ``RawCandidateSnapshot`` without any
    observation of the store boundary: nothing here says that ``head`` is, or
    ever was, the protected store branch head. Only a separately trusted
    caller may give it that meaning. It is a read input only; admission and
    the store writer refuse it.
    """

    location: StoreLocation
    candidate_id: str
    head: str
    head_tree: str
    transactions: tuple[RawTransaction, ...]

    def __post_init__(self) -> None:
        _validate_history(self)

    @property
    def entries(self) -> frozenset[TreeEntry]:
        """The candidate directory at ``head``: every entry the transactions added."""
        return _entries(self.transactions)


def _validate_history(value: RawCandidateSnapshot | CandidateHistoryAtHead) -> None:
    """The one structural validity rule of a candidate history at one head, with or without a boundary."""
    if (not isinstance(value.location, StoreLocation) or not _git_object(value.head)
            or not _git_object(value.head_tree) or not isinstance(value.transactions, tuple)
            or not all(isinstance(item, RawTransaction) for item in value.transactions)
            or len({item.commit for item in value.transactions}) != len(value.transactions)
            or not candidate_commit_count_valid(len(value.transactions))):
        raise VerificationError("store_snapshot_invalid", STAGE)
    candidate_path(value.candidate_id)
    entries: frozenset[TreeEntry] = frozenset()
    for item in value.transactions:
        child = entries | frozenset(item.transition.added)
        if (item.parent_tree != (git_tree_sha(tuple(entries)) if entries else None)
                or classify_transition(entries, child) != item.transition
                or item.tree != git_tree_sha(tuple(child))):
            raise VerificationError("store_snapshot_invalid", STAGE)
        entries = child


def _entries(transactions: tuple[RawTransaction, ...]) -> frozenset[TreeEntry]:
    return frozenset(entry for item in transactions for entry in item.transition.added)


def _sorted_digests(values: object) -> bool:
    return (isinstance(values, tuple) and bool(values)
            and all(isinstance(value, str) and HEX_SHA256.fullmatch(value) is not None for value in values)
            and list(values) == sorted(set(values)))


_OUTCOME_CODE_RE = re.compile(r"^[a-z][a-z_]{0,63}$")


def _outcome_code(value: object) -> bool:
    return isinstance(value, str) and _OUTCOME_CODE_RE.fullmatch(value) is not None


@dataclass(frozen=True)
class AppendConfirmed:
    """The branch moved from ``parent`` to exactly ``commit`` with these records."""

    parent: str
    commit: str
    kind: TransitionKind
    record_sha256s: tuple[str, ...]

    def __post_init__(self) -> None:
        if (not _git_object(self.parent) or not _git_object(self.commit) or self.parent == self.commit
                or not isinstance(self.kind, TransitionKind) or not _sorted_digests(self.record_sha256s)):
            raise VerificationError("store_append_outcome_invalid", STAGE)


@dataclass(frozen=True)
class AppendRefused:
    """The append was refused before the call could move the branch."""

    reason: str
    findings: tuple[FindingRef, ...]

    def __post_init__(self) -> None:
        if (not _outcome_code(self.reason) or not isinstance(self.findings, tuple)
                or not all(isinstance(item, FindingRef) for item in self.findings)):
            raise VerificationError("store_append_outcome_invalid", STAGE)


@dataclass(frozen=True)
class AppendAmbiguous:
    """The branch may or may not hold ``proposed``; only a later read can tell."""

    parent: str
    proposed: str
    nonce: str
    record_sha256s: tuple[str, ...]
    failure: str

    def __post_init__(self) -> None:
        if (not _git_object(self.parent) or not _git_object(self.proposed) or self.parent == self.proposed
                or not isinstance(self.nonce, str) or HEX_SHA256.fullmatch(self.nonce) is None
                or not _sorted_digests(self.record_sha256s) or not _outcome_code(self.failure)):
            raise VerificationError("store_append_outcome_invalid", STAGE)


AppendOutcome = AppendConfirmed | AppendRefused | AppendAmbiguous
