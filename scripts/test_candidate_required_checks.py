"""Required-check observation through the real command owner, GitHub client and finite fake ``gh`` children.

The fake ``gh`` is a real child found through PATH. It answers the exact
``gh pr checks`` argv and the exact GitHub API endpoints from a test-owned JSON
state file, logs one line per invocation, and answers anything else as not
found. The context names, apps and IDs are synthetic: the observer must derive
every required context from the configured policy. Nothing touches the network
or a provider.
"""

from __future__ import annotations

from dataclasses import FrozenInstanceError
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests import candidate_fixtures as fx
from scripts.acceptance_tests.release_fixture import FixtureTree, open_descriptors
from scripts.delegate_acceptance.native_release.contract import PublicationRequest
from scripts.delegate_acceptance.release_verification import commands, github
from scripts.delegate_acceptance.release_verification.candidate_history import PublicationAuthorization
from scripts.delegate_acceptance.release_verification.candidate_merge_observation import (
    JOB_SOURCE, MAX_RULE_PAGES, READINESS_SOURCE, RULE_PAGE_SIZE, RequiredCheck, RequiredChecks,
    observe_required_checks,
)
from scripts.delegate_acceptance.release_verification.candidate_publication_contract import CandidateReference
from scripts.delegate_acceptance.release_verification.candidate_publication_preflight import (
    PublicationContext, ReadinessArtifact,
)
from scripts.delegate_acceptance.release_verification.candidate_readiness_check import STATUS_CONTEXT, Target
from scripts.delegate_acceptance.release_verification.candidate_store_contract import StoreLocation
from scripts.delegate_acceptance.release_verification.candidate_store_replay import CandidateSnapshot
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.test_candidate_source_support import ChildFault, settle_retained
from scripts.test_release_github_transport import ObservedGitHubClient


REPOSITORY = fx.REPOSITORY
NUMBER = 32
COMMIT = fx.COMMIT  # B, the merged pull request's head
MERGED = "e5" * 20  # M, never a check target
FREEZE = "f7" * 20
READINESS_RUN = 9001
ACTIONS_APP = 15368
CONTEXTS = ("checks / lint", "checks / unit", "checks / race", "checks / native")
CHECKS_ARGV = ["pr", "checks", "32", "--repo", REPOSITORY, "--required", "--json", "name,bucket,state,link,workflow"]
CHECKS_KEY = " ".join(CHECKS_ARGV)
HEADERS = ["-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2026-03-10"]
BRANCH_KEY = f"GET repos/{REPOSITORY}/branches/main"


def rules_key(page: int) -> str:
    return f"GET repos/{REPOSITORY}/rules/branches/main?per_page=100&page={page}"


RULES_KEY = rules_key(1)
READINESS_URL = f"https://github.com/{REPOSITORY}/actions/runs/{READINESS_RUN}"

_FAKE_GH = r'''#!{python}
import json, os, sys
argv = sys.argv[1:]
with open(os.environ["FIXTURE_STATE"]) as source:
    replies = json.load(source)
key = " ".join(argv) if argv[0] == "pr" else argv[2] + " " + argv[7]
with open(os.environ["FIXTURE_LOG"], "a") as sink:
    sink.write(json.dumps({"argv": argv, "key": key}) + "\n")
reply = replies.get(key)
if reply is None:
    sys.stdout.write('{"message":"Not Found","status":"404"}')
    sys.exit(1)
sys.stdout.write(reply["output"] if isinstance(reply["output"], str) else json.dumps(reply["output"]))
sys.exit(reply["exit"])
'''


def publication_context(*, readiness_run: int = READINESS_RUN) -> PublicationContext:
    """A typed preflight result for PR 32 with head B; only N, B, the repository and the run are read."""
    candidate = fx.bound()
    publication = PublicationAuthorization(
        candidate_id=candidate.candidate_id, attempt=1, evidence_parent=fx.TRANSACTION, source_commit=COMMIT,
        source_tree=fx.TREE, proof_sha256s=("8" * 64,), install_sha256s=("9" * 64,), triage_sha256s=(),
        review_sha256="7" * 64)
    review = fx.review(candidate)
    snapshot = CandidateSnapshot(
        head="1" * 40, candidate=candidate, reviews=(review,), freeze_commit=FREEZE,
        history=fx.history(candidate, intents=(fx.intent(candidate),), publication=publication))
    request = PublicationRequest(
        store=StoreLocation(REPOSITORY, "candidate-store", "2" * 40, "3" * 40),
        reference=CandidateReference(candidate.candidate_id, FREEZE, MERGED),
        readiness_run_id=readiness_run, readiness_run_attempt=1, resume_published=False)
    return PublicationContext(
        request=request, snapshot=snapshot, review=review, pull_request=NUMBER,
        target=Target(COMMIT, fx.TREE), artifact=ReadinessArtifact(id=7, sha256="a" * 64),
        harness_commit=fx.HARNESS, harness_sha256="b" * 64, protection_unverified=False,
        repository_private=True)


def branch(contexts: tuple[str, ...] = CONTEXTS, *, apps: dict[str, object] | None = None,
           level: object = "non_admins", protected: object = True, enabled: object = True) -> dict[str, object]:
    """The branch reply shape: ``contexts`` and ``checks`` are two views of one required inventory."""
    apps = apps or {}
    checks = [{"context": name, "app_id": apps.get(name, ACTIONS_APP)} for name in contexts]
    return {"name": "main", "commit": {"sha": MERGED}, "protected": protected,
            "protection": {"enabled": enabled, "required_status_checks": {
                "enforcement_level": level, "contexts": list(contexts), "checks": checks}},
            "protection_url": f"https://api.github.com/repos/{REPOSITORY}/branches/main/protection"}


def ids(index: int) -> tuple[int, int, int]:
    """The synthetic run, job and check-run IDs of the ``index``-th required job."""
    return 7000 + index, 8000 + index, 8000 + index


def job_url(run: int, job: int, repository: str = REPOSITORY) -> str:
    return f"https://github.com/{repository}/actions/runs/{run}/job/{job}"


def row(name: str, link: str, **changes: object) -> dict[str, object]:
    return {"name": name, "bucket": "pass", "state": "SUCCESS", "link": link, "workflow": "Checks", **changes}


def job(context: str, run: int, job_id: int, check: int, **changes: object) -> dict[str, object]:
    return {"id": job_id, "run_id": run, "run_attempt": 1, "head_sha": COMMIT, "name": context,
            "status": "completed", "conclusion": "success", "workflow_name": "Checks",
            "check_run_url": f"https://api.github.com/repos/{REPOSITORY}/check-runs/{check}", **changes}


def check_run(context: str, check: int, app: object = ACTIONS_APP, **changes: object) -> dict[str, object]:
    return {"id": check, "head_sha": COMMIT, "name": context, "status": "completed", "conclusion": "success",
            "app": {"id": app, "slug": "synthetic-app"}, **changes}


def job_key(job_id: int) -> str:
    return f"GET repos/{REPOSITORY}/actions/jobs/{job_id}"


def check_key(check: int) -> str:
    return f"GET repos/{REPOSITORY}/check-runs/{check}"


class RequiredChecksCase(unittest.TestCase):
    def setUp(self) -> None:
        self.descriptors = open_descriptors()
        self.addCleanup(lambda: self.assertEqual(open_descriptors(), self.descriptors))
        self.tree = FixtureTree.open(prefix="candidate-required-checks-test", label="required checks fixture")
        self.addCleanup(self.tree.dispose)
        root = self.tree.root
        (root / "bin").mkdir()
        gh = root / "bin" / "gh"
        gh.write_text(_FAKE_GH.replace("{python}", sys.executable, 1))
        gh.chmod(0o755)
        self.state_path, self.log_path = root / "state.json", root / "invocations.jsonl"
        self.receipt: dict = {"commands": []}
        self.replies: dict[str, dict[str, object]] = {}
        self.configure(CONTEXTS)

    def configure(self, contexts: tuple[str, ...], *, apps: dict[str, int] | None = None) -> None:
        """Main requires ``contexts``; each passed on B as its own Actions job, except the readiness status."""
        apps = apps or {}
        self.reply(BRANCH_KEY, branch(contexts, apps=apps))
        self.reply(RULES_KEY, [{"type": "pull_request", "ruleset_id": 3}, {"type": "deletion", "ruleset_id": 3}])
        rows = []
        for index, name in enumerate(contexts):
            if name == STATUS_CONTEXT:
                rows.append(row(name, READINESS_URL, workflow=""))
                continue
            run, job_id, check = ids(index)
            rows.append(row(name, job_url(run, job_id)))
            self.reply(job_key(job_id), job(name, run, job_id, check))
            self.reply(check_key(check), check_run(name, check, apps.get(name, ACTIONS_APP)))
        self.reply(CHECKS_KEY, rows)
        self.configured_rows = tuple(rows)

    def reply(self, key: str, output: object, exit_code: int = 0) -> None:
        self.replies[key] = {"output": output, "exit": exit_code}
        self.state_path.write_text(json.dumps(self.replies))

    def rows(self) -> list[dict[str, object]]:
        """The configured passing rows, unaffected by any reply a test wrote since."""
        return [dict(item) for item in self.configured_rows]

    def observe(self, context: object | None = None) -> RequiredChecks:
        environment = {"PATH": str(self.tree.root / "bin"), "FIXTURE_STATE": str(self.state_path),
                       "FIXTURE_LOG": str(self.log_path), "GH_TOKEN": "private-canary"}
        client = ObservedGitHubClient(self.receipt, environment, self.tree)
        target = publication_context() if context is None else context
        # The checks query runs through the command owner, outside the observed client.
        return self.tree.observe_operation(lambda: observe_required_checks(client, context=target))

    def refused(self, code: str, keys: list[str] | None = None) -> VerificationError:
        """Refuse with ``code``; when given, ``keys`` are exactly the reads made, all of them GETs."""
        with self.assertRaises(VerificationError) as caught:
            self.observe()
        self.assertEqual(caught.exception.code, code)
        if keys is not None:
            self.assertEqual(self.keys(), keys)
        self.assert_read_only()
        return caught.exception

    def log(self) -> list[dict]:
        if not self.log_path.exists():
            return []
        return [json.loads(line) for line in self.log_path.read_text().splitlines()]

    def keys(self) -> list[str]:
        return [entry["key"] for entry in self.log()]

    def assert_read_only(self) -> None:
        for entry in self.log():
            argv = entry["argv"]
            if argv[0] == "pr":
                self.assertEqual(argv, CHECKS_ARGV)
            else:
                self.assertEqual(argv[:8], ["api", "--method", "GET", *HEADERS, argv[7]])
        self.assertNotIn("private-canary", repr(self.receipt))

    def reset_log(self) -> None:
        self.log_path.unlink(missing_ok=True)


PREFIX = [BRANCH_KEY, RULES_KEY, CHECKS_KEY]


def job_keys(count: int) -> list[str]:
    return [key for index in range(count) for key in (job_key(ids(index)[1]), check_key(ids(index)[2]))]


class HappyPathTests(RequiredChecksCase):
    def test_each_configured_job_is_bound_to_its_run_job_and_check_run_on_b(self):
        observed = self.observe()
        expected = tuple(RequiredCheck(context=name, app_id=ACTIONS_APP, source=JOB_SOURCE, run_id=ids(index)[0],
                                       job_id=ids(index)[1], check_run_id=ids(index)[2])
                         for index, name in enumerate(CONTEXTS))
        self.assertEqual(observed, RequiredChecks(pull_request=NUMBER, commit=COMMIT,
                                                  enforcement_level="non_admins", checks=expected))
        self.assertEqual(self.keys(), [*PREFIX, *job_keys(len(CONTEXTS))])
        self.assert_read_only()
        self.assertEqual(self.receipt["commands"][2],
                         {"name": "github-pull-request-required-checks", "argv": ["gh", *CHECKS_ARGV], "exit_code": 0})
        self.assertEqual([record["name"] for record in self.receipt["commands"]],
                         ["github-api", "github-api", "github-pull-request-required-checks",
                          *["github-api"] * (2 * len(CONTEXTS))])
        with self.assertRaises(FrozenInstanceError):
            setattr(observed.checks[0], "app_id", 1)
        self.assertTrue(self.tree.dispose())

    def test_a_renamed_policy_is_observed_as_configured_with_its_own_apps(self):
        renamed = ("build (linux)", "build (darwin)")
        self.configure(renamed, apps={"build (darwin)": 4242})
        observed = self.observe()
        self.assertEqual([(check.context, check.app_id) for check in observed.checks],
                         [("build (linux)", ACTIONS_APP), ("build (darwin)", 4242)])
        self.assertEqual(self.keys(), [*PREFIX, *job_keys(2)])

    def test_everyone_enforcement_is_accepted_and_reported(self):
        self.reply(BRANCH_KEY, branch(level="everyone"))
        self.assertEqual(self.observe().enforcement_level, "everyone")

    def test_an_activated_readiness_context_binds_only_the_selected_run_url(self):
        self.configure((STATUS_CONTEXT, *CONTEXTS[:1]))
        observed = self.observe()
        self.assertEqual(observed.checks[0], RequiredCheck(
            context=STATUS_CONTEXT, app_id=ACTIONS_APP, source=READINESS_SOURCE, run_id=READINESS_RUN,
            job_id=None, check_run_id=None))
        # The readiness status reads no job or check run; only the job row does.
        self.assertEqual(self.keys(), [*PREFIX, job_key(ids(1)[1]), check_key(ids(1)[2])])


class InputTests(RequiredChecksCase):
    def test_a_context_that_is_not_a_preflight_result_refuses_before_any_read(self):
        with self.assertRaises(VerificationError) as caught:
            self.observe(context={"pull_request": NUMBER})
        self.assertEqual(caught.exception.code, "required_checks_request_invalid")
        self.assertEqual((self.log(), self.receipt["commands"]), ([], []))


class PolicyTests(RequiredChecksCase):
    """Every refused policy stops after the one branch read."""

    def assert_policy(self, code: str, reply: object) -> None:
        self.reset_log()
        self.reply(BRANCH_KEY, reply)
        self.refused(code, [BRANCH_KEY])

    def test_unprotected_or_unenforced_main_is_refused(self):
        for reply in (branch(protected=False), branch(enabled=False), branch(level="off")):
            with self.subTest(reply=reply["protected"]):
                self.assert_policy("required_checks_policy_unenforced", reply)

    def test_malformed_policy_is_refused(self):
        no_status = branch()
        no_status["protection"] = {"enabled": True}
        bad_contexts = branch()
        bad_contexts["protection"]["required_status_checks"]["contexts"] = "checks / lint"
        for reply in (branch(level="admins"), branch(level=["non_admins"]), branch(level=None),
                      branch(protected=1), branch(enabled="true"), {**branch(), "name": "release"},
                      {**branch(), "protection": None}, no_status, bad_contexts, branch(("",))):
            with self.subTest(reply=json.dumps(reply)[:60]):
                self.assert_policy("required_checks_policy_invalid", reply)

    def test_an_empty_required_inventory_is_refused(self):
        self.assert_policy("required_checks_policy_empty", branch(()))

    def test_inconsistent_contexts_and_checks_are_refused(self):
        def changed(contexts: list[str], checks: list[object]) -> dict[str, object]:
            reply = branch()
            reply["protection"]["required_status_checks"].update(contexts=contexts, checks=checks)
            return reply
        entry = {"context": CONTEXTS[0], "app_id": ACTIONS_APP}
        cases = (changed([CONTEXTS[0]], []), changed([CONTEXTS[0]], [entry, entry]),
                 changed([CONTEXTS[0], CONTEXTS[0]], [entry]),
                 changed([CONTEXTS[0]], [entry, {"context": CONTEXTS[1], "app_id": ACTIONS_APP}]),
                 changed([CONTEXTS[0]], [{"context": CONTEXTS[1], "app_id": ACTIONS_APP}]))
        for reply in cases:
            with self.subTest(reply=reply["protection"]["required_status_checks"]):
                self.assert_policy("required_checks_policy_inconsistent", reply)

    def test_an_unbindable_app_source_is_refused_never_guessed(self):
        for app in (None, -1, 0, True, "15368", 15368.0):
            with self.subTest(app=app):
                self.assert_policy("required_checks_app_unbindable", branch(apps={CONTEXTS[2]: app}))

    def test_a_failed_policy_read_stops(self):
        self.reply(BRANCH_KEY, "", exit_code=1)
        self.refused("github_request_failed", [BRANCH_KEY])


class RulesTests(RequiredChecksCase):
    def test_a_ruleset_that_requires_checks_is_unsupported(self):
        self.reply(RULES_KEY, [{"type": "deletion"}, {"type": "required_status_checks", "parameters": {}}])
        self.refused("required_checks_ruleset_unsupported", [BRANCH_KEY, RULES_KEY])

    def test_malformed_rules_are_refused(self):
        for reply in ({"rules": []}, [{"ruleset_id": 3}], [{"type": 7}], [{"type": ""}], ["deletion"]):
            with self.subTest(reply=reply):
                self.reset_log()
                self.reply(RULES_KEY, reply)
                code = "github_object_invalid" if reply == ["deletion"] else "required_checks_rules_invalid"
                self.refused(code, [BRANCH_KEY, RULES_KEY])

    def test_rules_are_read_to_the_first_short_page(self):
        second = rules_key(2)
        self.reply(RULES_KEY, [{"type": "deletion"}] * RULE_PAGE_SIZE)
        self.reply(second, [{"type": "required_status_checks"}])
        self.refused("required_checks_ruleset_unsupported", [BRANCH_KEY, RULES_KEY, second])

    def test_an_unending_rule_listing_is_incomplete(self):
        pages = [rules_key(page) for page in range(1, MAX_RULE_PAGES + 1)]
        for key in pages:
            self.reply(key, [{"type": "deletion"}] * RULE_PAGE_SIZE)
        self.refused("required_checks_rules_incomplete", [BRANCH_KEY, *pages])


class CheckRowTests(RequiredChecksCase):
    """``gh`` exits 0 with its rows; every refused row stops before any job read."""

    def assert_rows(self, code: str, output: object) -> None:
        self.reset_log()
        self.reply(CHECKS_KEY, output)
        self.refused(code, PREFIX)

    def test_failed_skipped_and_pending_rows_exiting_zero_are_refused(self):
        for bucket, state in (("fail", "FAILURE"), ("skipping", "SKIPPED"), ("pending", "IN_PROGRESS"),
                              ("pending", "PENDING"), ("cancel", "CANCELLED"), ("pass", "NEUTRAL"),
                              ("skipping", "SUCCESS")):
            with self.subTest(bucket=bucket, state=state):
                rows = self.rows()
                self.assert_rows("required_check_not_successful",
                                 [*rows[:3], {**rows[3], "bucket": bucket, "state": state}])

    def test_missing_duplicated_unexpected_or_empty_rows_are_refused(self):
        rows = self.rows()
        extra = row("checks / extra", job_url(1, 2))
        for output in (rows[:3], [*rows, rows[1]], [*rows, extra], [*rows[:3], extra], []):
            with self.subTest(names=[item["name"] for item in output]):
                self.assert_rows("required_checks_inventory_mismatch", output)

    def test_malformed_rows_are_refused(self):
        first, *rest = self.rows()
        missing = {key: value for key, value in first.items() if key != "workflow"}
        duplicate_key = json.dumps(rest)[:-1] + ', {"name": "a", "name": "b"}]'
        for output in ("not json", {"checks": []}, [*rest, missing], [*rest, {**first, "event": "push"}],
                       [*rest, {**first, "link": None}], [*rest, {**first, "workflow": 3}],
                       duplicate_key, "[NaN]"):
            with self.subTest(output=str(output)[:40]):
                self.assert_rows("required_checks_reply_invalid", output)

    def test_a_non_object_row_is_refused(self):
        self.assert_rows("github_object_invalid", [*self.rows()[:3], "checks / native"])

    def test_a_failed_checks_query_keeps_the_command_identity(self):
        self.reply(CHECKS_KEY, "", exit_code=8)
        error = self.refused("command_failed", PREFIX)
        self.assertEqual(error.stage, "github-pull-request-required-checks")
        self.assertEqual(self.receipt["commands"][-1]["exit_code"], 8)

    def test_a_job_row_with_a_foreign_or_unbound_link_is_refused(self):
        foreign = job_url(7000, 8000, "someone/delegation-layer")
        for link in (foreign, READINESS_URL, job_url(7000, 8000) + "/", job_url(0, 8000), job_url(7000, 0),
                     job_url(7000, 8000).replace("/8000", "/08000"), "https://example.com/job/1", ""):
            with self.subTest(link=link):
                rows = self.rows()
                self.assert_rows("required_check_link_invalid", [{**rows[0], "link": link}, *rows[1:]])


class ReadinessRowTests(RequiredChecksCase):
    def setUp(self) -> None:
        super().setUp()
        self.configure((STATUS_CONTEXT, *CONTEXTS[:1]))

    def test_a_readiness_row_for_another_run_or_a_job_is_refused_before_any_job_read(self):
        for link in (f"https://github.com/{REPOSITORY}/actions/runs/{READINESS_RUN + 1}", READINESS_URL + "/",
                     f"https://github.com/someone/delegation-layer/actions/runs/{READINESS_RUN}",
                     job_url(READINESS_RUN, 1)):
            with self.subTest(link=link):
                self.reset_log()
                rows = self.rows()
                self.reply(CHECKS_KEY, [{**rows[0], "link": link}, rows[1]])
                self.refused("required_check_readiness_link_mismatch", PREFIX)

    def test_the_readiness_run_comes_from_the_context_not_the_row(self):
        with self.assertRaises(VerificationError) as caught:
            self.observe(publication_context(readiness_run=READINESS_RUN + 1))
        self.assertEqual(caught.exception.code, "required_check_readiness_link_mismatch")

    def test_a_missing_required_readiness_row_is_refused(self):
        self.reply(CHECKS_KEY, self.rows()[1:])
        self.refused("required_checks_inventory_mismatch", PREFIX)


class JobTests(RequiredChecksCase):
    """A refused job stops at its own read; earlier jobs were bound, later ones never read."""

    def assert_job(self, code: str, **changes: object) -> None:
        self.reset_log()
        run, job_id, check = ids(1)
        self.reply(job_key(job_id), job(CONTEXTS[1], run, job_id, check, **changes))
        self.refused(code, [*PREFIX, *job_keys(1), job_key(job_id)])

    def test_a_job_that_is_not_the_linked_job_on_b_is_refused(self):
        for changes in ({"head_sha": MERGED}, {"run_id": 7999}, {"id": 1}, {"name": CONTEXTS[2]},
                        {"run_id": True}, {"head_sha": None}):
            with self.subTest(changes=changes):
                self.assert_job("required_check_job_mismatch", **changes)

    def test_an_unfinished_or_failed_job_is_refused(self):
        for changes in ({"status": "in_progress", "conclusion": None}, {"conclusion": "failure"},
                        {"conclusion": "skipped"}):
            with self.subTest(changes=changes):
                self.assert_job("required_check_job_not_successful", **changes)

    def test_a_foreign_or_malformed_check_run_url_is_refused(self):
        for url in (f"https://api.github.com/repos/someone/delegation-layer/check-runs/{ids(1)[2]}",
                    f"https://github.com/{REPOSITORY}/check-runs/{ids(1)[2]}",
                    f"https://api.github.com/repos/{REPOSITORY}/check-runs/0", None, 8001):
            with self.subTest(url=url):
                self.assert_job("required_check_run_url_invalid", check_run_url=url)


class CheckRunTests(RequiredChecksCase):
    def assert_check_run(self, code: str, reply: dict[str, object]) -> None:
        self.reset_log()
        self.reply(check_key(ids(2)[2]), reply)
        self.refused(code, [*PREFIX, *job_keys(3)])

    def test_a_check_run_of_another_head_name_or_identity_is_refused(self):
        name, check = CONTEXTS[2], ids(2)[2]
        for reply in (check_run(name, check, head_sha=MERGED), check_run(CONTEXTS[0], check),
                      check_run(name, check, id=1), check_run(name, check, app=None),
                      {**check_run(name, check), "app": None}, check_run(name, check, app=True)):
            with self.subTest(reply=reply):
                self.assert_check_run("required_check_run_mismatch", reply)

    def test_an_unfinished_or_failed_check_run_is_refused(self):
        self.assert_check_run("required_check_run_not_successful",
                              check_run(CONTEXTS[2], ids(2)[2], conclusion="neutral"))

    def test_a_check_run_from_another_app_is_refused(self):
        self.assert_check_run("required_check_app_mismatch", check_run(CONTEXTS[2], ids(2)[2], app=4242))


class UnprovenClosureTests(RequiredChecksCase):
    """An unknown Wait at either canonical port retains the tree and stops every later read."""

    def observe_with(self, fault: ChildFault, code_stage: tuple[str, str]) -> None:
        with (patch.object(commands, "run_bounded_command", fault),
              patch.object(github, "run_bounded_command", fault)):
            error = self.refused(code_stage[0])
        self.assertEqual((error.code, error.stage), code_stage)
        self.assertIs(self.tree.unresolved, error)
        self.assertIsNone(self.receipt["commands"][-1]["exit_code"])

    def test_unknown_checks_query_wait_retains_and_stops(self):
        fault = ChildFault(lambda argv: argv[:3] == ["gh", "pr", "checks"])
        self.observe_with(fault, ("command_timeout", "github-pull-request-required-checks"))
        self.assertEqual((fault.faulted, fault.launched, self.keys()),
                         ([["gh", *CHECKS_ARGV]], 2, [BRANCH_KEY, RULES_KEY]))
        settle_retained(self, self.tree, fault, (self.state_path, self.log_path))

    def test_unknown_job_wait_retains_and_stops(self):
        endpoint = job_key(ids(1)[1]).removeprefix("GET ")
        fault = ChildFault(lambda argv: argv[-1] == endpoint)
        self.observe_with(fault, ("command_timeout", "github-api"))
        self.assertEqual(fault.faulted, [["gh", "api", "--method", "GET", *HEADERS, endpoint]])
        self.assertEqual((fault.launched, self.keys()), (5, [*PREFIX, *job_keys(1)]))
        settle_retained(self, self.tree, fault, (self.state_path, self.log_path))


if __name__ == "__main__":
    unittest.main()
