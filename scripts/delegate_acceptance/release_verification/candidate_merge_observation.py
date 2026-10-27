"""Observe that a merged pull request published one bound candidate's built tree on main.

This module only reads. It runs the exact proven ``gh pr view`` merge query
through the canonical command owner, reads the merge commit M and the current
main ref through the canonical GitHub client, compares M with that pinned main
commit, and returns the canonical ``publication_source``, which alone owns the
tree rule. It keeps no state or cache; every request failure or malformed reply
refuses before any later read.

``observe_required_checks`` then observes, for the same pull request N and its
head B, that every status check main's classic branch protection requires now
passed. GitHub's configuration is the only authority for which contexts and apps
are required; nothing here names a job, workflow or app. Each required Actions
context is B's one latest check run of that name under the configured app, read
on the commit itself and bound to the Actions job and check run that ran on B;
pull request N's rollup may omit check suites GitHub did not associate with N,
so it never decides those. The canonical readiness status is read from N's
required rollup, where GitHub's own ``isRequired`` attributes it to the
configured app, and is bound to the selected readiness run URL alone. The
result is an observation: it grants no merge, freeze or publication.
"""

from __future__ import annotations

from dataclasses import dataclass
import json
import re
from urllib.parse import quote

from scripts.delegate_acceptance.native_evidence.public_contract import PublicProofError, parse_json
from .candidate_publication_preflight import PublicationContext
from .candidate_readiness import BoundCandidate, PublicationSource, publication_source
from .candidate_readiness_check import API_URL, SERVER_URL, STATUS_CONTEXT
from .commands import command
from .contract import MAX_OUTPUT, REVISION_RE, VerificationError
from .github import REQUEST_TIMEOUT, GitHubClient, object_value


STAGE = "observe-publication-source"
MERGE_COMMAND = "github-pull-request-merge"
BASE_REF = "main"
MAIN_REF = "refs/heads/main"
MERGE_FIELDS = ("number", "state", "headRefOid", "baseRefName", "mergeCommit", "mergedAt")
MAX_OBJECT_BYTES = 1 << 20
# Page 2 of one-commit pages omits the per-file list; the proven page was 15060 bytes.
COMPARE_QUERY = "per_page=1&page=2"
MAX_COMPARE_BYTES = 8 << 20
ANCESTOR_STATUSES = frozenset({"ahead", "identical"})
CHECKS_STAGE = "observe-required-checks"
CHECKS_COMMAND = "github-pull-request-required-checks"
CHECK_FIELDS = ("name", "bucket", "state", "link", "workflow")
# ``off`` is a known level that enforces nothing; any other value is malformed.
ENFORCEMENT_LEVELS = frozenset({"off", "non_admins", "everyone"})
ENFORCED_LEVELS = frozenset({"non_admins", "everyone"})
# Rulesets may require checks too; this observer reads only classic protection and refuses them.
UNSUPPORTED_RULE = "required_status_checks"
RULE_PAGE_SIZE = 100
MAX_RULE_PAGES = 10
# Two rows per page: a second latest run of one name and app is visible, and refused, on the first page.
CHECK_RUN_PAGE_SIZE = 2
READINESS_SOURCE = "readiness-status"
JOB_SOURCE = "actions-job"
# A positive decimal identifier within the signed 64-bit range GitHub uses.
_ID = r"([1-9][0-9]{0,18})"


def observe_publication_source(client: GitHubClient, *, candidate: BoundCandidate, pull_request: int,
                               merged_commit: str) -> PublicationSource:
    """Require PR ``pull_request`` to have merged the built commit as M, with M on main.

    The repository and built commit B come from ``candidate``; ``pull_request``
    comes from the caller's validated readiness receipt, never from a lookup.
    """
    if type(pull_request) is not int or pull_request <= 0:
        raise VerificationError("publication_pull_request_invalid", STAGE)
    if not isinstance(merged_commit, str) or REVISION_RE.fullmatch(merged_commit) is None:
        raise VerificationError("publication_merged_commit_invalid", STAGE)
    repository = candidate.locator.repository
    raw = command(client.receipt, MERGE_COMMAND, [
        "gh", "pr", "view", str(pull_request), "--repo", repository, "--json", ",".join(MERGE_FIELDS),
    ], client.environment, capture=True, timeout=REQUEST_TIMEOUT, max_output=MAX_OUTPUT)
    _require_merge(raw, pull_request=pull_request, head_commit=candidate.manifest.source.commit,
                   merged_commit=merged_commit)
    source = publication_source(manifest=candidate.manifest, merged_commit=merged_commit,
                                merged_tree=_commit_tree(client, repository, merged_commit))
    _require_on_main(client, repository, merged_commit, _main_commit(client, repository))
    return source


def _require_merge(raw: bytes, *, pull_request: int, head_commit: str, merged_commit: str) -> None:
    """Decode the exact six-field reply; the PR must have merged B into main as M."""
    try:
        value = json.loads(raw)
    except (UnicodeError, ValueError, RecursionError):
        raise VerificationError("pull_request_merge_invalid", STAGE) from None
    reply = object_value(value, STAGE)
    if set(reply) != set(MERGE_FIELDS):
        raise VerificationError("pull_request_merge_invalid", STAGE)
    number = reply["number"]
    if type(number) is not int or number != pull_request:
        raise VerificationError("pull_request_identity_mismatch", STAGE)
    if reply["state"] != "MERGED":
        raise VerificationError("pull_request_not_merged", STAGE)
    if reply["baseRefName"] != BASE_REF:
        raise VerificationError("pull_request_base_mismatch", STAGE)
    if reply["headRefOid"] != head_commit:
        raise VerificationError("pull_request_head_mismatch", STAGE)
    merge = reply["mergeCommit"]
    if not isinstance(merge, dict) or set(merge) != {"oid"} or merge["oid"] != merged_commit:
        raise VerificationError("pull_request_merge_commit_mismatch", STAGE)
    merged_at = reply["mergedAt"]
    if not isinstance(merged_at, str) or not merged_at:
        raise VerificationError("pull_request_merge_invalid", STAGE)


def _commit_tree(client: GitHubClient, repository: str, commit: str) -> str:
    """M's own commit object; its identity must be M. Its tree's form is checked by the policy."""
    reply = object_value(client.json(f"repos/{repository}/git/commits/{commit}",
                                     limit=MAX_OBJECT_BYTES), STAGE)
    tree = reply.get("tree")
    tree_sha = tree.get("sha") if isinstance(tree, dict) else None
    if reply.get("sha") != commit or not isinstance(tree_sha, str):
        raise VerificationError("publication_commit_mismatch", STAGE)
    return tree_sha


def _main_commit(client: GitHubClient, repository: str) -> str:
    """Pin main to the exact commit it names now, so the comparison never follows a moving ref."""
    reply = object_value(client.json(f"repos/{repository}/git/ref/heads/{BASE_REF}",
                                     limit=MAX_OBJECT_BYTES), STAGE)
    target = reply.get("object")
    sha = target.get("sha") if isinstance(target, dict) else None
    if (reply.get("ref") != MAIN_REF or not isinstance(target, dict) or target.get("type") != "commit"
            or not isinstance(sha, str) or REVISION_RE.fullmatch(sha) is None):
        raise VerificationError("publication_main_ref_invalid", STAGE)
    return sha


def _require_on_main(client: GitHubClient, repository: str, merged_commit: str, main_commit: str) -> None:
    """Only the comparison metadata owns ancestry: main must contain M and lack nothing of it."""
    reply = object_value(client.json(
        f"repos/{repository}/compare/{merged_commit}...{main_commit}?{COMPARE_QUERY}",
        limit=MAX_COMPARE_BYTES), STAGE)
    status, ahead, behind = reply.get("status"), reply.get("ahead_by"), reply.get("behind_by")
    if (not isinstance(status, str) or type(ahead) is not int or ahead < 0
            or type(behind) is not int or behind < 0):
        raise VerificationError("publication_main_comparison_invalid", STAGE)
    if status not in ANCESTOR_STATUSES or behind != 0:
        raise VerificationError("publication_merge_not_on_main", STAGE)
    if (_sha(reply.get("merge_base_commit")) != merged_commit
            or _sha(reply.get("base_commit")) != merged_commit):
        raise VerificationError("publication_main_ancestry_mismatch", STAGE)
    identical = main_commit == merged_commit
    if (status == "identical") != identical or (ahead == 0) != identical:
        raise VerificationError("publication_main_comparison_invalid", STAGE)


def _sha(value: object) -> object:
    return value.get("sha") if isinstance(value, dict) else None


@dataclass(frozen=True)
class RequiredCheck:
    """One context main requires, the app configured for it and the evidence that it passed on B.

    ``source`` is ``READINESS_SOURCE`` for the canonical readiness status, whose
    run is the selected readiness run and whose requirement GitHub's own
    ``isRequired`` decided; it has no job or check run. For ``JOB_SOURCE`` the
    run, job and check run are the Actions records read back on B.
    """

    context: str
    app_id: int
    source: str
    run_id: int
    job_id: int | None
    check_run_id: int | None


@dataclass(frozen=True)
class RequiredChecks:
    """Every check main's protection requires, observed passing on pull request N's head B, in policy order."""

    pull_request: int
    commit: str
    enforcement_level: str
    checks: tuple[RequiredCheck, ...]


def observe_required_checks(client: GitHubClient, *, context: PublicationContext) -> RequiredChecks:
    """Require every check main's classic protection names now to have passed on B for pull request N.

    Precondition: the caller has already run ``observe_publication_source`` for
    this same ``context`` (its N, B and the M it merged), so N's head is B. The
    repository, N, B and the readiness run all come from ``context``. Every read
    is fresh; the first refusal stops every later read and nothing is mutated.
    """
    if not isinstance(client, GitHubClient) or not isinstance(context, PublicationContext):
        raise _checks_refuse("required_checks_request_invalid")
    repository, commit = context.request.repository, context.target.commit
    level, required = _branch_policy(client, repository)
    _refuse_required_check_rules(client, repository)
    listed = {name: _latest_check_run(client, repository, commit=commit, name=name, app_id=app_id)
              for name, app_id in required.items() if name != STATUS_CONTEXT}
    if STATUS_CONTEXT in required:
        raw = command(client.receipt, CHECKS_COMMAND, [
            "gh", "pr", "checks", str(context.pull_request), "--repo", repository, "--required",
            "--json", ",".join(CHECK_FIELDS),
        ], client.environment, capture=True, timeout=REQUEST_TIMEOUT, max_output=MAX_OUTPUT)
        readiness_url = f"{SERVER_URL}/{repository}/actions/runs/{context.request.readiness_run_id}"
        _require_readiness_row(raw, required, readiness_url=readiness_url)
    checks = []
    for name, app_id in required.items():
        if name == STATUS_CONTEXT:
            checks.append(RequiredCheck(context=name, app_id=app_id, source=READINESS_SOURCE,
                                        run_id=context.request.readiness_run_id, job_id=None, check_run_id=None))
            continue
        listed_run, link = listed[name]
        run_id, job_id = _job_link(link, repository=repository)
        check_run = _job_check_run(client, repository, commit=commit, name=name, run_id=run_id, job_id=job_id)
        if check_run != listed_run:
            raise _checks_refuse("required_check_run_mismatch")
        _require_check_run(client, repository, commit=commit, name=name, check_run=check_run, app_id=app_id)
        checks.append(RequiredCheck(context=name, app_id=app_id, source=JOB_SOURCE, run_id=run_id,
                                    job_id=job_id, check_run_id=check_run))
    return RequiredChecks(pull_request=context.pull_request, commit=commit, enforcement_level=level,
                          checks=tuple(checks))


def _checks_refuse(code: str) -> VerificationError:
    return VerificationError(code, CHECKS_STAGE)


def _exact_int(value: object, expected: int) -> bool:
    return type(value) is int and value == expected


def _branch_policy(client: GitHubClient, repository: str) -> tuple[str, dict[str, int]]:
    """Main's enforced level and its complete required contexts, each mapped to its one configured app.

    ``contexts`` and ``checks`` are two views of one inventory and must agree
    exactly. Only a positive app ID is accepted: a null or ``-1`` source is not
    an app this observer can bind, so it refuses instead of guessing.
    """
    reply = object_value(client.json(f"repos/{repository}/branches/{BASE_REF}", limit=MAX_OBJECT_BYTES),
                         CHECKS_STAGE)
    protection = reply.get("protection")
    if (reply.get("name") != BASE_REF or type(reply.get("protected")) is not bool
            or not isinstance(protection, dict) or type(protection.get("enabled")) is not bool):
        raise _checks_refuse("required_checks_policy_invalid")
    if reply["protected"] is not True or protection["enabled"] is not True:
        raise _checks_refuse("required_checks_policy_unenforced")
    status_checks = protection.get("required_status_checks")
    if not isinstance(status_checks, dict):
        raise _checks_refuse("required_checks_policy_invalid")
    level = status_checks.get("enforcement_level")
    if not isinstance(level, str) or level not in ENFORCEMENT_LEVELS:
        raise _checks_refuse("required_checks_policy_invalid")
    if level not in ENFORCED_LEVELS:
        raise _checks_refuse("required_checks_policy_unenforced")
    contexts, configured = status_checks.get("contexts"), status_checks.get("checks")
    if (not isinstance(contexts, list) or not isinstance(configured, list)
            or any(not isinstance(name, str) or not name for name in contexts)):
        raise _checks_refuse("required_checks_policy_invalid")
    if not contexts:
        raise _checks_refuse("required_checks_policy_empty")
    required: dict[str, int] = {}
    for value in configured:
        check = object_value(value, CHECKS_STAGE)
        name, app_id = check.get("context"), check.get("app_id")
        if not isinstance(name, str) or name in required:
            raise _checks_refuse("required_checks_policy_inconsistent")
        if type(app_id) is not int or app_id <= 0:
            raise _checks_refuse("required_checks_app_unbindable")
        required[name] = app_id
    if len(set(contexts)) != len(contexts) or set(contexts) != set(required):
        raise _checks_refuse("required_checks_policy_inconsistent")
    return level, {name: required[name] for name in contexts}


def _refuse_required_check_rules(client: GitHubClient, repository: str) -> None:
    """Read every rule active on main; a ruleset that requires checks is outside the classic policy read here."""
    for page in range(1, MAX_RULE_PAGES + 1):
        rows = client.json(f"repos/{repository}/rules/branches/{BASE_REF}?per_page={RULE_PAGE_SIZE}&page={page}",
                           limit=MAX_OBJECT_BYTES)
        if not isinstance(rows, list) or len(rows) > RULE_PAGE_SIZE:
            raise _checks_refuse("required_checks_rules_invalid")
        for value in rows:
            rule_type = object_value(value, CHECKS_STAGE).get("type")
            if not isinstance(rule_type, str) or not rule_type:
                raise _checks_refuse("required_checks_rules_invalid")
            if rule_type == UNSUPPORTED_RULE:
                raise _checks_refuse("required_checks_ruleset_unsupported")
        if len(rows) < RULE_PAGE_SIZE:
            return
    raise _checks_refuse("required_checks_rules_incomplete")


def _latest_check_run(client: GitHubClient, repository: str, *, commit: str, name: str,
                      app_id: int) -> tuple[int, str]:
    """B's one latest check run named ``name`` from the configured app, which must have passed; its ID and link.

    The listing keeps every status, so a pending latest run is seen and refused,
    never hidden behind an older completed one. Zero or several latest runs are
    a missing or ambiguous context; neither is resolved by picking a row.
    """
    query = f"check_name={quote(name, safe='')}&app_id={app_id}&filter=latest&per_page={CHECK_RUN_PAGE_SIZE}"
    reply = object_value(client.json(f"repos/{repository}/commits/{commit}/check-runs?{query}",
                                     limit=MAX_OBJECT_BYTES), CHECKS_STAGE)
    total, rows = reply.get("total_count"), reply.get("check_runs")
    if (type(total) is not int or total < 0 or not isinstance(rows, list)
            or len(rows) != min(total, CHECK_RUN_PAGE_SIZE)):
        raise _checks_refuse("required_checks_reply_invalid")
    if total != 1:
        raise _checks_refuse("required_checks_inventory_mismatch")
    run = object_value(rows[0], CHECKS_STAGE)
    check_run, link, app = run.get("id"), run.get("details_url"), run.get("app")
    if (type(check_run) is not int or check_run <= 0 or not isinstance(link, str)
            or not isinstance(app, dict) or type(app.get("id")) is not int):
        raise _checks_refuse("required_checks_reply_invalid")
    if run.get("name") != name or run.get("head_sha") != commit:
        raise _checks_refuse("required_check_run_mismatch")
    if app["id"] != app_id:
        raise _checks_refuse("required_check_app_mismatch")
    if (run.get("status"), run.get("conclusion")) != ("completed", "success"):
        raise _checks_refuse("required_check_not_successful")
    return check_run, link


def _require_readiness_row(raw: bytes, required: dict[str, int], *, readiness_url: str) -> None:
    """N's required rollup holds exactly one readiness row, which passed at exactly the selected run.

    ``gh`` writes the JSON rows before it would judge them and then exits 0, so
    exit 0 alone proves nothing: every row's form is judged here, strictly. A
    row GitHub requires must be a context the policy requires. Other required
    rows may be absent or in any state: B's own check runs decide those.
    """
    try:
        value = parse_json(raw, "required_checks_reply_invalid")
    except PublicProofError:
        raise _checks_refuse("required_checks_reply_invalid") from None
    if not isinstance(value, list):
        raise _checks_refuse("required_checks_reply_invalid")
    rows: dict[object, dict[str, object]] = {}
    for item in value:
        row = object_value(item, CHECKS_STAGE)
        if set(row) != set(CHECK_FIELDS) or any(not isinstance(row[field], str) for field in CHECK_FIELDS):
            raise _checks_refuse("required_checks_reply_invalid")
        if row["name"] in rows or row["name"] not in required:
            raise _checks_refuse("required_checks_inventory_mismatch")
        rows[row["name"]] = row
    row = rows.get(STATUS_CONTEXT)
    if row is None:
        raise _checks_refuse("required_checks_inventory_mismatch")
    if (row["bucket"], row["state"]) != ("pass", "SUCCESS"):
        raise _checks_refuse("required_check_not_successful")
    if row["link"] != readiness_url:
        raise _checks_refuse("required_check_readiness_link_mismatch")


def _job_link(link: str, *, repository: str) -> tuple[int, int]:
    """The run and job of this repository's Actions job page that the listed check run links to."""
    match = re.fullmatch(re.escape(f"{SERVER_URL}/{repository}/actions/runs/") + _ID + "/job/" + _ID, link)
    if match is None:
        raise _checks_refuse("required_check_link_invalid")
    return int(match.group(1)), int(match.group(2))


def _job_check_run(client: GitHubClient, repository: str, *, commit: str, name: str, run_id: int,
                   job_id: int) -> int:
    """The linked job ran on B in that run under the context's name and succeeded; return its check run."""
    job = object_value(client.json(f"repos/{repository}/actions/jobs/{job_id}", limit=MAX_OBJECT_BYTES),
                       CHECKS_STAGE)
    if not (_exact_int(job.get("id"), job_id) and _exact_int(job.get("run_id"), run_id)
            and job.get("head_sha") == commit and job.get("name") == name):
        raise _checks_refuse("required_check_job_mismatch")
    if (job.get("status"), job.get("conclusion")) != ("completed", "success"):
        raise _checks_refuse("required_check_job_not_successful")
    url = job.get("check_run_url")
    match = (re.fullmatch(re.escape(f"{API_URL}/repos/{repository}/check-runs/") + _ID, url)
             if isinstance(url, str) else None)
    if match is None:
        raise _checks_refuse("required_check_run_url_invalid")
    return int(match.group(1))


def _require_check_run(client: GitHubClient, repository: str, *, commit: str, name: str, check_run: int,
                       app_id: int) -> None:
    """The job's check run is B's, under the same name, succeeded, and was created by the configured app."""
    reply = object_value(client.json(f"repos/{repository}/check-runs/{check_run}", limit=MAX_OBJECT_BYTES),
                         CHECKS_STAGE)
    app = reply.get("app")
    if not (_exact_int(reply.get("id"), check_run) and reply.get("head_sha") == commit
            and reply.get("name") == name and isinstance(app, dict) and type(app.get("id")) is int):
        raise _checks_refuse("required_check_run_mismatch")
    if (reply.get("status"), reply.get("conclusion")) != ("completed", "success"):
        raise _checks_refuse("required_check_run_not_successful")
    if app["id"] != app_id:
        raise _checks_refuse("required_check_app_mismatch")
