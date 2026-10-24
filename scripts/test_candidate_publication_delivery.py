"""Hosted exact-byte publication composed over the real receipt, ``finish_gate`` and shared report contract.

Every observation and delivery edge is replaced by a recording stand-in that
appends the same command records its real owner would; the receipt, cleanup,
report writer and Root's ``require_published``/``require_failed`` readers run.
"""

from __future__ import annotations

import contextlib
import io
import json
from pathlib import Path
import sys
from types import SimpleNamespace
import unittest
from unittest import mock

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.acceptance_tests.release_fixture import FixtureTree
from scripts.delegate_acceptance.native_release.contract import PRODUCTION_STORE, PublicationRequest
from scripts.delegate_acceptance.native_release.publication_contract import (
    REPORT_FIELD, PublicationDispatch, ReportBinding, require_failed, require_published,
)
from scripts.delegate_acceptance.processes.contracts import BoundedCommandError, CommandFailure
from scripts.delegate_acceptance.release_verification import commands, github
from scripts.delegate_acceptance.release_verification import candidate_publication_delivery as delivery
from scripts.delegate_acceptance.release_verification.candidate_artifact import (
    ArtifactObservation, AttemptObservation, CandidateArtifact, WorkflowObservation,
)
from scripts.delegate_acceptance.release_verification.candidate_github_delivery import PreparedRelease
from scripts.delegate_acceptance.release_verification.candidate_manifest import payload_paths
from scripts.delegate_acceptance.release_verification.commands import command
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import ArtifactDownload
from scripts.delegate_acceptance.release_verification.published_contract import npm_integrity, npm_shasum
from scripts.delegate_acceptance.release_verification.published_ports import ConsumerPorts, default_ports
from scripts import publish_development_candidate as cli
from scripts.test_candidate_publication_contract import (
    BUILD, VERSION, payload_files, promotion_for, release,
)
from scripts.test_native_publication_contract import NPM
from scripts.test_candidate_artifact_support import COMMIT, REPOSITORY
from scripts.test_candidate_github_delivery_support import MERGED, TAG, DeliveryCase, artifact
from scripts.test_candidate_npm_delivery import _FAKE_NPM
from scripts.test_candidate_source_support import ChildFault, settle_retained
from scripts.test_release_github_transport import ObservedGitHubClient


PROMOTION = promotion_for()
INVOCATION = "5" * 32
HARNESS = "9" * 40
SECRETS = {"GH_TOKEN": "main-token-secret", "HOMEBREW_TAP_TOKEN": "tap-token-secret",
           "NPM_TOKEN": "npm-token-secret"}
EDGES = ("observe_harness", "preflight", "observe_publication_source", "observe_required_checks",
         "load_candidate_artifact", "prepare_release_assets", "deliver_npm", "publish_prepared_release",
         "deliver_tap")
WRITERS = ("prepare_release_assets", "deliver_npm", "publish_prepared_release", "deliver_tap")


def dispatch(*, resume: bool = False) -> PublicationDispatch:
    request = PublicationRequest(store=PRODUCTION_STORE, reference=PROMOTION.reference, readiness_run_id=901,
                                 readiness_run_attempt=2, resume_published=resume)
    return PublicationDispatch(request=request, invocation_id=INVOCATION, harness=HARNESS)


def run_environment(value: PublicationDispatch, **changes: str) -> dict[str, str]:
    repository = PRODUCTION_STORE.repository
    return {"GITHUB_REPOSITORY": repository, "GITHUB_SHA": value.harness, "GITHUB_RUN_ID": "7001",
            "GITHUB_RUN_ATTEMPT": "1", "GITHUB_REF": "refs/heads/main", "GITHUB_EVENT_NAME": "workflow_dispatch",
            "GITHUB_WORKFLOW_REF": f"{repository}/.github/workflows/release.yml@refs/heads/main", **changes}


class World:
    """Recording stand-ins for every edge; ``fail`` names one edge and the error it raises."""

    def __init__(self, test: unittest.TestCase):
        self.test = test
        self.calls: list[str] = []
        self.fail: tuple[str, BaseException] | None = None
        self.private: object = False
        self.harness = (HARNESS, "f" * 64, True)
        self.artifact_manifest = PROMOTION.candidate.manifest
        self.resume: list[bool] = []

    def _enter(self, name: str) -> None:
        self.calls.append(name)

    def _maybe_fail(self, name: str) -> None:
        if self.fail is not None and self.fail[0] == name:
            raise self.fail[1]

    def observe_harness(self, receipt, environment, *, checkout, seeds):
        self._enter("observe_harness")
        self.test.assertEqual(seeds, (delivery.CLI_SEED,))
        receipt["commands"].append({"name": "git-head", "argv": ["git", "rev-parse", "HEAD"], "exit_code": 0})
        self._maybe_fail("observe_harness")
        return self.harness

    def preflight(self, client, request, *, verify_store_protection):
        self._enter("preflight")
        # The hosted run reads the store protection-unverified; strict protection is Root's own read.
        self.test.assertIs(verify_store_protection, False)
        self.test.assertEqual(client.environment["GH_TOKEN"], SECRETS["GH_TOKEN"])
        for name in ("HOMEBREW_TAP_TOKEN", "NPM_TOKEN"):
            self.test.assertNotIn(name, client.environment)
        client.receipt["commands"].append({"name": "github-api", "endpoint": "repos/x", "method": "GET",
                                           "exit_code": 0})
        self._maybe_fail("preflight")
        snapshot = SimpleNamespace(head="d" * 40, candidate=PROMOTION.candidate,
                                   history=SimpleNamespace(publication=PROMOTION.freeze))
        return SimpleNamespace(repository_private=self.private, pull_request=17, snapshot=snapshot,
                               target=SimpleNamespace(commit=BUILD), artifact=SimpleNamespace(id=5),
                               protection_unverified=True)

    def observe_publication_source(self, client, *, candidate, pull_request, merged_commit):
        self._enter("observe_publication_source")
        self.test.assertEqual((pull_request, merged_commit), (17, PROMOTION.reference.merged_commit))
        self._maybe_fail("observe_publication_source")
        return PROMOTION.source

    def observe_required_checks(self, client, *, context):
        self._enter("observe_required_checks")
        self._maybe_fail("observe_required_checks")
        return SimpleNamespace(checks=(SimpleNamespace(context="ci"),))

    def load_candidate_artifact(self, client, locator, *, container, payload):
        self._enter("load_candidate_artifact")
        self.test.assertEqual(locator, PROMOTION.candidate.locator)
        self._maybe_fail("load_candidate_artifact")
        return CandidateArtifact(
            locator=locator, manifest=self.artifact_manifest, repository_id=1,
            workflow=WorkflowObservation(id=2, path=".github/workflows/ci.yml", state="active"),
            attempt=AttemptObservation(run_id=3, run_attempt=1, workflow_id=2, head_sha=BUILD, path="p",
                                       status="completed", conclusion="success"),
            artifact=ArtifactObservation(id=4, name="candidate", size=1, sha256="0" * 64, run_id=3,
                                         head_sha=BUILD),
            container=ArtifactDownload(path=container, size=1, sha256="0" * 64), payload=payload)

    def prepare_release_assets(self, promotion, artifact, *, client, ports, work_directory, resume_published):
        self._enter("prepare_release_assets")
        self.test.assertEqual(promotion, PROMOTION)
        self.resume.append(resume_published)
        client.receipt["commands"].append({"name": "github-api", "endpoint": "repos/x/releases", "method": "POST",
                                           "exit_code": None})
        self._maybe_fail("prepare_release_assets")
        return PreparedRelease(release_id=55, observed=release(*payload_paths(VERSION)))

    def deliver_npm(self, artifact, *, ports, receipt, environment, resume_published):
        self._enter("deliver_npm")
        self.resume.append(resume_published)
        for name in ("GH_TOKEN", "HOMEBREW_TAP_TOKEN"):
            self.test.assertNotIn(name, environment)
        user_config = Path(environment["NPM_CONFIG_USERCONFIG"]).read_bytes()
        if resume_published:
            # A resume only reads npm: no token is forwarded even though the job supplies one.
            self.test.assertNotIn("NPM_TOKEN", environment)
            self.test.assertEqual(user_config, b"")
        else:
            self.test.assertEqual(environment["NPM_TOKEN"], SECRETS["NPM_TOKEN"])
            self.test.assertEqual(user_config, b"//registry.npmjs.org/:_authToken=${NPM_TOKEN}\n")
        self.test.assertEqual(Path(environment["NPM_CONFIG_GLOBALCONFIG"]).read_bytes(), b"")
        receipt["commands"].append({"name": "candidate-npm-publish",
                                    "argv": ["/usr/bin/npm", "publish", "x.tgz"], "exit_code": None})
        self._maybe_fail("deliver_npm")
        return NPM

    def publish_prepared_release(self, promotion, prepared, *, client, work_directory, resume_published):
        self._enter("publish_prepared_release")
        self.resume.append(resume_published)
        self.test.assertEqual(prepared.release_id, 55)
        self._maybe_fail("publish_prepared_release")
        return PreparedRelease(release_id=55, observed=release(*payload_paths(VERSION), draft=False))

    def deliver_tap(self, artifact, *, client, work_directory):
        self._enter("deliver_tap")
        self.test.assertEqual(client.environment["GH_TOKEN"], SECRETS["HOMEBREW_TAP_TOKEN"])
        self.test.assertNotIn("NPM_TOKEN", client.environment)
        self._maybe_fail("deliver_tap")
        return "2" * 40


class PublicationCase(unittest.TestCase):
    def setUp(self):
        self.tree = FixtureTree.open(prefix="candidate-publication", label="candidate publication test tree")
        self.addCleanup(self.tree.dispose)
        self.world = World(self)
        for name in EDGES:
            patcher = mock.patch.object(delivery, name, side_effect=getattr(self.world, name))
            patcher.start()
            self.addCleanup(patcher.stop)
        self.output = self.tree.root / "publication"

    def publish(self, *, value: PublicationDispatch | None = None, **environment: str) -> int:
        value = value or dispatch()
        request = delivery.HostedPublication(dispatch=value, run=delivery.hosted_run(run_environment(value), value),
                                             output_dir=self.output, checkout=Path(__file__).resolve().parent)
        parent = {"PATH": "/usr/bin:/bin", "HOME": str(self.tree.root), **SECRETS, **environment}
        return delivery.publish_candidate(request, environment={k: v for k, v in parent.items() if v},
                                          ports=default_ports())

    def receipt(self) -> dict:
        return json.loads((self.output / delivery.RECEIPT_NAME).read_text(encoding="utf-8"))

    def binding(self, value: PublicationDispatch | None = None) -> ReportBinding:
        return ReportBinding.of(value or dispatch(), run_id=7001, run_attempt=1)

    def assertNoSecret(self) -> None:
        text = (self.output / delivery.RECEIPT_NAME).read_text(encoding="utf-8")
        for secret in SECRETS.values():
            self.assertNotIn(secret, text)


class CompositionTests(PublicationCase):
    def test_a_delivered_run_is_finalized_cleaned_up_and_reads_as_published(self):
        self.assertEqual(self.publish(), 0)
        self.assertEqual(self.world.calls, list(EDGES))
        receipt = self.receipt()
        report = require_published(receipt, binding=self.binding(), promotion=PROMOTION)
        self.assertIs(report["first_mutation_started"], True)
        self.assertEqual(report["delivery"]["tap"], {"blob_sha": "2" * 40})
        self.assertEqual(report["delivery"]["npm"]["latest"], VERSION)
        self.assertEqual(receipt["cleanup"]["retained_paths"], [])
        self.assertFalse((self.output / delivery.WORKSPACE_NAME).exists())
        self.assertEqual((receipt["source_revision"], receipt["harness_revision"]), (HARNESS, HARNESS))
        self.assertEqual(self.world.resume, [False, False, False])
        self.assertIs(receipt["candidate_publication"]["preflight"]["protection_unverified"], True)
        self.assertNoSecret()

    def test_resume_is_passed_unchanged_to_every_canonical_edge(self):
        value = dispatch(resume=True)
        self.assertEqual(self.publish(value=value), 0)
        self.assertEqual(self.world.resume, [True, True, True])
        require_published(self.receipt(), binding=self.binding(value), promotion=PROMOTION)

    def test_an_edge_failure_stops_every_later_write_and_reads_only_as_failed(self):
        for edge in EDGES:
            with self.subTest(edge=edge):
                self.setUp()
                self.world.fail = (edge, VerificationError("edge_refused", "edge-stage"))
                self.assertEqual(self.publish(), 1)
                self.assertEqual(self.world.calls, list(EDGES[:EDGES.index(edge) + 1]))
                receipt = self.receipt()
                failure = require_failed(receipt, binding=self.binding(), promotion=PROMOTION)
                self.assertEqual((failure.code, failure.stage), ("edge_refused", "edge-stage"))
                self.assertFalse(failure.delivery_confirmed)
                # Mutation start is derived from the records the edge appended before it failed.
                self.assertEqual(failure.first_mutation_started, edge in WRITERS)
                with self.assertRaises(Exception):
                    require_published(receipt, binding=self.binding(), promotion=PROMOTION)
                self.assertNoSecret()

    def test_an_unexpected_fault_is_a_sanitized_failure_not_a_success(self):
        self.world.fail = ("deliver_tap", RuntimeError("tap-token-secret leaked text"))
        self.assertEqual(self.publish(), 1)
        failure = require_failed(self.receipt(), binding=self.binding(), promotion=PROMOTION)
        self.assertEqual((failure.code, failure.stage), ("verification_internal_error", "deliver-tap"))
        self.assertTrue(failure.first_mutation_started)
        self.assertNoSecret()

    def test_an_interrupt_retains_the_workspace_and_still_reports_the_binding(self):
        self.world.fail = ("deliver_npm", KeyboardInterrupt())
        with self.assertRaises(KeyboardInterrupt):
            self.publish()
        receipt = self.receipt()
        failure = require_failed(receipt, binding=self.binding(), promotion=PROMOTION)
        self.assertEqual(failure.code, "verification_interrupted")
        self.assertTrue(failure.first_mutation_started)
        self.assertEqual(self.world.calls[-1], "deliver_npm")
        self.assertTrue((self.output / delivery.WORKSPACE_NAME).is_dir())

    def test_failed_cleanup_after_confirmed_delivery_is_never_published(self):
        target = "scripts.delegate_acceptance.release_verification.cleanup.resume_directory_cleanup"
        with mock.patch(target, side_effect=OSError("cleanup failed")):
            self.assertEqual(self.publish(), 1)
        receipt = self.receipt()
        failure = require_failed(receipt, binding=self.binding(), promotion=PROMOTION)
        self.assertEqual((failure.code, failure.delivery_confirmed), ("release_cleanup_failed", True))
        with self.assertRaises(Exception):
            require_published(receipt, binding=self.binding(), promotion=PROMOTION)


class RefusalBeforeWriteTests(PublicationCase):
    def assertRefusedBeforeWrite(self, code: str, last_call: str) -> None:
        self.assertEqual(self.publish_result, 1)
        failure = require_failed(self.receipt(), binding=self.binding(), promotion=PROMOTION)
        self.assertEqual(failure.code, code)
        self.assertFalse(failure.first_mutation_started)
        self.assertEqual(self.world.calls[-1], last_call)
        self.assertFalse(set(WRITERS) & set(self.world.calls))
        self.assertNoSecret()

    def test_private_or_unknown_visibility_refuses_after_preflight_before_any_write(self):
        for private, code in ((True, "publication_repository_private"), (None, "repository_visibility_unknown"),
                              ("false", "repository_visibility_unknown")):
            with self.subTest(private=private):
                self.setUp()
                self.world.private = private
                self.publish_result = self.publish()
                self.assertRefusedBeforeWrite(code, "preflight")

    def test_each_missing_credential_refuses_before_any_request(self):
        for name in SECRETS:
            with self.subTest(missing=name):
                self.setUp()
                self.publish_result = self.publish(**{name: ""})
                self.assertRefusedBeforeWrite(f"publication_credentials_missing:{name.lower()}", "observe_harness")

    def test_an_uncommitted_or_different_harness_refuses_before_any_request(self):
        for harness in (("8" * 40, "f" * 64, True), (HARNESS, "f" * 64, False)):
            with self.subTest(harness=harness):
                self.setUp()
                self.world.harness = harness
                self.publish_result = self.publish()
                self.assertRefusedBeforeWrite("publication_harness_mismatch", "observe_harness")

    def test_a_loaded_payload_of_another_manifest_refuses_before_any_write(self):
        other = payload_files(**{"delegation-layer.rb": b"another formula\n"})
        self.world.artifact_manifest = promotion_for(other).candidate.manifest
        self.publish_result = self.publish()
        self.assertRefusedBeforeWrite("candidate_publication_artifact_mismatch", "load_candidate_artifact")


class RunBindingTests(unittest.TestCase):
    def test_the_run_is_bound_to_h_main_dispatch_and_its_own_attempt(self):
        value = dispatch()
        run = delivery.hosted_run(run_environment(value), value)
        self.assertEqual((run.run_id, run.run_attempt, run.harness), (7001, 1, HARNESS))
        for name, wrong in (("GITHUB_SHA", "8" * 40), ("GITHUB_REF", "refs/heads/other"),
                            ("GITHUB_EVENT_NAME", "push"), ("GITHUB_REPOSITORY", "fork/delegation-layer"),
                            ("GITHUB_WORKFLOW_REF", "x/y/.github/workflows/release.yml@refs/heads/other"),
                            ("GITHUB_RUN_ID", "0"), ("GITHUB_RUN_ATTEMPT", "01")):
            with self.subTest(name=name):
                with self.assertRaises(VerificationError) as caught:
                    delivery.hosted_run(run_environment(value, **{name: wrong}), value)
                self.assertEqual(caught.exception.code, f"publication_run_mismatch:{name.lower()}")
        missing = run_environment(value)
        del missing["GITHUB_RUN_ID"]
        with self.assertRaises(VerificationError):
            delivery.hosted_run(missing, value)

    def test_mutation_start_is_derived_only_from_write_records(self):
        reads = [{"name": "github-api", "method": "GET"}, {"name": "candidate-npm-latest",
                                                           "argv": ["/usr/bin/npm", "view", "x"]},
                 {"name": "git", "argv": ["git", "publish"]}]
        self.assertFalse(delivery.first_mutation_started({"commands": reads}))
        for write in ({"name": "github-api", "method": method} for method in ("POST", "PUT", "PATCH", "DELETE")):
            self.assertTrue(delivery.first_mutation_started({"commands": [*reads, write]}))
        for argv in (["gh", "release", "upload", "v1", "x"], ["/opt/node/bin/npm", "publish", "x.tgz"]):
            self.assertTrue(delivery.first_mutation_started({"commands": [{"name": "c", "argv": argv}]}))


class CliTests(unittest.TestCase):
    def test_malformed_inputs_or_run_refuse_before_any_effect(self):
        tree = FixtureTree.open(prefix="candidate-publication-cli", label="candidate publication cli tree")
        self.addCleanup(tree.dispose)
        output = tree.root / "out"
        value = dispatch()
        valid = {name.upper(): text for name, text in value.inputs().items()}
        cases = {
            "missing input": ({k: v for k, v in valid.items() if k != "HARNESS"}, run_environment(value)),
            "foreign digest": ({**valid, "REQUEST_SHA256": "0" * 64}, run_environment(value)),
            "non-canonical resume": ({**valid, "RESUME_PUBLISHED": "True"}, run_environment(value)),
            "other run head": (valid, run_environment(value, GITHUB_SHA="8" * 40)),
        }
        for name, (inputs, run) in cases.items():
            with self.subTest(name=name):
                with (mock.patch.dict(cli.os.environ, {**inputs, **run, **SECRETS}, clear=True),
                      mock.patch.object(cli, "publish_candidate") as publish):
                    self.assertEqual(cli.main(["--output-dir", str(output)]), 1)
                publish.assert_not_called()
                self.assertFalse(output.exists())

    def test_valid_inputs_reach_the_publisher_with_the_bound_run(self):
        value = dispatch()
        inputs = {name.upper(): text for name, text in value.inputs().items()}
        with (mock.patch.dict(cli.os.environ, {**inputs, **run_environment(value)}, clear=True),
              mock.patch.object(cli, "publish_candidate", return_value=0) as publish):
            self.assertEqual(cli.main(["--output-dir", "/nonexistent/out"]), 0)
        request = publish.call_args.args[0]
        self.assertEqual((request.dispatch, request.run.run_id, request.checkout), (value, 7001, cli.CHECKOUT))
        self.assertEqual(REPORT_FIELD, "publication")


class CredentialModeTests(unittest.TestCase):
    def test_normal_needs_every_credential_and_resume_needs_only_main_and_tap(self):
        config = Path("/owned/npm")
        base = {"PATH": "/usr/bin:/bin", **SECRETS}
        normal = delivery._environments(base, config, resume_published=False)
        self.assertEqual(normal.npm["NPM_TOKEN"], SECRETS["NPM_TOKEN"])
        self.assertEqual(normal.npm_user_config, delivery.NPM_USER_CONFIG)
        resumed = delivery._environments(base, config, resume_published=True)
        self.assertNotIn("NPM_TOKEN", resumed.npm)
        self.assertEqual(resumed.npm_user_config, b"")
        self.assertEqual((resumed.github["GH_TOKEN"], resumed.tap["GH_TOKEN"]),
                         (SECRETS["GH_TOKEN"], SECRETS["HOMEBREW_TAP_TOKEN"]))
        without_npm = {k: v for k, v in base.items() if k != "NPM_TOKEN"}
        self.assertNotIn("NPM_TOKEN", delivery._environments(without_npm, config, resume_published=True).npm)
        for resume, missing in ((False, "NPM_TOKEN"), (False, "GH_TOKEN"), (False, "HOMEBREW_TAP_TOKEN"),
                                (True, "GH_TOKEN"), (True, "HOMEBREW_TAP_TOKEN")):
            with self.subTest(resume=resume, missing=missing):
                with self.assertRaises(VerificationError) as caught:
                    delivery._environments({k: v for k, v in base.items() if k != missing}, config,
                                           resume_published=resume)
                self.assertEqual(caught.exception.code, f"publication_credentials_missing:{missing.lower()}")


class ResumeEdgeTests(DeliveryCase):
    """An explicit resume through the real GitHub release and npm edges over the finite fake ``gh`` and ``npm``.

    Only the read-only observations before delivery and the tap edge are recording stand-ins;
    the canonical release and npm decisions, transports and command owner run.
    """

    def setUp(self) -> None:
        super().setUp()
        root = self.tree.root
        self.npm = root / "bin" / "npm"
        self.npm.write_text(_FAKE_NPM.replace("{python}", sys.executable, 1))
        self.npm.chmod(0o755)
        self.npm_state, self.npm_log = root / "npm-state.json", root / "npm-invocations.jsonl"
        tgz = next(name for name in self.files if name.endswith(".tgz"))
        identity = {"integrity": npm_integrity(self.files[tgz]), "shasum": npm_shasum(self.files[tgz]),
                    "gitHead": COMMIT}
        self.npm_state.write_text(json.dumps({
            "versions": [VERSION], "packages": {VERSION: identity}, "latest": VERSION,
            "publish_version": VERSION, "publish_override": None, "publish_sets_latest": True,
            "hide_listings": 0, "stale_latest": 0, "stale_value": VERSION, "faults": []}))
        self.npm_environments: list[dict[str, str]] = []
        self.npm_user_configs: list[bytes] = []
        self.tap_calls: list[dict[str, str]] = []
        self.value = self.dispatch_for_fixture()
        self.output = self.tree.root / "publication"
        self.patch_edges()

    def dispatch_for_fixture(self) -> PublicationDispatch:
        request = PublicationRequest(store=PRODUCTION_STORE, reference=self.promotion.reference,
                                     readiness_run_id=901, readiness_run_attempt=2, resume_published=True)
        return PublicationDispatch(request=request, invocation_id=INVOCATION, harness=HARNESS)

    def patch_edges(self) -> None:
        promotion, payload = self.promotion, self.payload
        context = SimpleNamespace(
            repository_private=False, pull_request=17, target=SimpleNamespace(commit=COMMIT),
            artifact=SimpleNamespace(id=5), protection_unverified=True,
            snapshot=SimpleNamespace(head="d" * 40, candidate=promotion.candidate,
                                     history=SimpleNamespace(publication=promotion.freeze)))
        fixture = {"FIXTURE_STATE": str(self.state_path), "FIXTURE_LOG": str(self.log_path)}
        real_child_environment = delivery.child_environment

        def tap(artifact_value, *, client, work_directory):
            self.tap_calls.append(dict(client.environment))
            return "2" * 40

        stand_ins = {
            "observe_harness": lambda receipt, environment, **_: (HARNESS, "f" * 64, True),
            "preflight": lambda client, request, *, verify_store_protection: context,
            "observe_publication_source": lambda client, **_: promotion.source,
            "observe_required_checks": lambda client, **_: SimpleNamespace(checks=()),
            "load_candidate_artifact": lambda client, locator, **_: artifact(promotion, payload),
            "deliver_tap": tap,
            # Fixture routing only: each child still receives exactly the module's own environment.
            "child_environment": lambda parent: {**real_child_environment(parent), **fixture},
            "GitHubClient": lambda receipt, environment: ObservedGitHubClient(receipt, environment, self.tree),
        }
        for name, value in stand_ins.items():
            patcher = mock.patch.object(delivery, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def recording_ports(self) -> ConsumerPorts:
        """The canonical command owner; npm children are recorded, then pointed at the npm fixture state."""
        github_ports = self.ports()

        def run(receipt, name, argv, env, **options):
            if argv[0] != str(self.npm):
                return github_ports.run(receipt, name, argv, env, **options)
            self.npm_environments.append(dict(env))
            self.npm_user_configs.append(Path(env["NPM_CONFIG_USERCONFIG"]).read_bytes())
            routed = {**env, "FIXTURE_STATE": str(self.npm_state), "FIXTURE_LOG": str(self.npm_log)}
            return self.tree.observe_operation(lambda: command(receipt, name, argv, routed, **options))

        return ConsumerPorts(run=run, which=lambda name: str(self.npm) if name == "npm" else None)

    def run_resume(self, **environment: str) -> int:
        run = delivery.hosted_run(run_environment(self.value), self.value)
        request = delivery.HostedPublication(dispatch=self.value, run=run, output_dir=self.output,
                                             checkout=Path(__file__).resolve().parent)
        parent = {"PATH": f"{self.tree.root / 'bin'}:/usr/bin:/bin", "HOME": str(self.tree.root),
                  "GH_TOKEN": SECRETS["GH_TOKEN"], "HOMEBREW_TAP_TOKEN": SECRETS["HOMEBREW_TAP_TOKEN"],
                  **environment}
        # Tag-resolver children run outside the observed client and ports, and ``finish_gate`` turns
        # their converted failure into a report; the runner forwards each original failure to the tree.
        runner = commands.run_bounded_command

        def observed(argv: list[str], env: dict[str, str], **options: object):
            return self.tree.observe_operation(lambda: runner(argv, env, **options))

        with mock.patch.object(commands, "run_bounded_command", observed):
            return delivery.publish_candidate(request, environment=parent, ports=self.recording_ports())

    def receipt_value(self) -> dict:
        return json.loads((self.output / delivery.RECEIPT_NAME).read_text(encoding="utf-8"))

    def npm_keys(self) -> list[str]:
        if not self.npm_log.exists():
            return []
        return [json.loads(line).get("key") for line in self.npm_log.read_text().splitlines()]

    def published(self) -> None:
        self.seed_release(draft=False, names=payload_paths(VERSION), target="main", tag_commit=MERGED)

    def test_resume_without_an_npm_token_finishes_through_the_canonical_edges(self):
        self.published()
        for supplied in ({}, {"NPM_TOKEN": SECRETS["NPM_TOKEN"]}):
            with self.subTest(supplied=tuple(supplied)):
                self.output = self.tree.root / f"publication-{len(supplied)}"
                self.assertEqual(self.run_resume(**supplied), 0)
                binding = ReportBinding.of(self.value, run_id=7001, run_attempt=1)
                report = require_published(self.receipt_value(), binding=binding, promotion=self.promotion)
                self.assertFalse(report["first_mutation_started"])
                self.assertEqual(self.mutations(), [])
                self.assertNotIn("publish", self.npm_keys())
                self.assertIn("latest", self.npm_keys())
                for env in self.npm_environments:
                    self.assertNotIn("NPM_TOKEN", env)
                self.assertEqual(set(self.npm_user_configs), {b""})
                self.assertEqual(self.tap_calls[-1]["GH_TOKEN"], SECRETS["HOMEBREW_TAP_TOKEN"])
                self.assertNotIn("NPM_TOKEN", self.tap_calls[-1])
                self.assertNotIn(SECRETS["NPM_TOKEN"], (self.output / delivery.RECEIPT_NAME).read_text())

    def test_resume_refuses_absent_or_conflicting_destinations_with_zero_writes(self):
        def npm_change(change):
            def apply() -> None:
                self.published()
                state = json.loads(self.npm_state.read_text())
                change(state)
                self.npm_state.write_text(json.dumps(state))
            return apply

        def release_change(**seed):
            return lambda: self.seed_release(**seed)

        # A complete listing of another version is absence; an empty listing is unknown, never absence.
        absent = npm_change(lambda state: state.update(versions=["0.1.36"], packages={}, latest="0.1.36"))
        conflict = npm_change(lambda state: state["packages"][VERSION].update(shasum="0" * 40))
        stale = npm_change(lambda state: state.update(versions=[VERSION, "0.1.36"], latest="0.1.36"))
        incomplete = payload_paths(VERSION)[:2]
        cases = (
            ("npm absent", absent, "candidate_resume_mutation_refused", "decide-candidate-delivery"),
            ("npm conflict", conflict, "candidate_npm_conflict", "decide-candidate-delivery"),
            ("npm latest", stale, "candidate_npm_latest_mismatch", "deliver-candidate-npm"),
            ("release absent", lambda: None, "candidate_resume_mutation_refused", "decide-candidate-delivery"),
            ("release draft", release_change(draft=True, names=payload_paths(VERSION)),
             "candidate_resume_mutation_refused", "decide-candidate-delivery"),
            ("release incomplete", release_change(draft=False, names=incomplete, target="main", tag_commit=MERGED),
             "candidate_release_published_incomplete", "decide-candidate-delivery"),
        )
        for name, arrange, code, stage in cases:
            with self.subTest(case=name):
                self.doCleanups()
                self.setUp()
                arrange()
                self.assertEqual(self.run_resume(), 1)
                binding = ReportBinding.of(self.value, run_id=7001, run_attempt=1)
                failure = require_failed(self.receipt_value(), binding=binding, promotion=self.promotion)
                self.assertEqual((failure.code, failure.stage), (code, stage))
                self.assertFalse(failure.first_mutation_started)
                self.assertFalse(failure.delivery_confirmed)
                self.assertEqual(self.mutations(), [])
                self.assertEqual(self.uploads, [])
                self.assertNotIn("publish", self.npm_keys())
                self.assertEqual(self.tap_calls, [])
                for env in self.npm_environments:
                    self.assertNotIn("NPM_TOKEN", env)

    def test_an_unproven_tag_resolver_child_retains_the_tree_and_is_never_followed(self):
        # The resume resolves the published tag once while preparing and once while confirming publication.
        endpoint = f"repos/{REPOSITORY}/git/ref/tags/{TAG}"
        for occurrence, npm_read in ((1, False), (2, True)):
            with self.subTest(occurrence=occurrence):
                self.doCleanups()
                self.setUp()
                self.published()
                resolutions: list[list[str]] = []

                def matches(argv: list[str], occurrence: int = occurrence) -> bool:
                    if argv[1:3] != ["api", endpoint]:
                        return False
                    resolutions.append(argv)
                    return len(resolutions) == occurrence

                # Every fixture child, gh transport and command alike, passes the same counter.
                fault = ChildFault(matches)
                for module in (commands, github):
                    patcher = mock.patch.object(module, "run_bounded_command", fault)
                    patcher.start()
                    self.addCleanup(patcher.stop)
                self.assertEqual(self.run_resume(), 1)
                # The tree holds the original typed failure, not the receipt's converted copy.
                self.assertIs(type(self.tree.unresolved), BoundedCommandError)
                self.assertIs(self.tree.unresolved.reason, CommandFailure.TIMEOUT)
                binding = ReportBinding.of(self.value, run_id=7001, run_attempt=1)
                failure = require_failed(self.receipt_value(), binding=binding, promotion=self.promotion)
                self.assertEqual((failure.code, failure.stage), ("command_timeout", "github-release-ref"))
                self.assertFalse(failure.first_mutation_started)
                self.assertFalse(failure.delivery_confirmed)
                self.assertEqual(self.mutations(), [])
                self.assertEqual(self.uploads, [])
                self.assertNotIn("publish", self.npm_keys())
                self.assertEqual("latest" in self.npm_keys(), npm_read)
                self.assertEqual(self.tap_calls, [])
                with contextlib.redirect_stderr(io.StringIO()):
                    self.doCleanups()
                settle_retained(self, self.tree, fault, (self.state_path, self.log_path,
                                                         self.output / delivery.RECEIPT_NAME))


if __name__ == "__main__":
    unittest.main()
