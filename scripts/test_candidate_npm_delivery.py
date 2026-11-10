"""Exact-TGZ npm delivery through the real command owner and a finite fixture ``npm``.

The fake ``npm`` is a real child. It keeps one registry package in a test-owned
JSON state file, answers only the exact argv the edge builds, digests the file a
publish names, and appends one log line per invocation. A fault replaces one
matching reply, optionally after applying its effect; a successful publish may
stay unlisted, or leave ``latest`` stale, for a number of reads. The adapter-only
fake clock also drives elapsed-time visibility through the command hook. Nothing touches the
network, a real registry or a credential; no fixture here is live proof.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass
import json
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from scripts.release_tests.release_fixture import FixtureTree
from scripts.acceptance.testing.resources import open_descriptors
from scripts.acceptance.evidence.digests import sha
from scripts.acceptance.processes.contracts import BoundedCommandError
from scripts.acceptance.processes.contracts import CommandFailure
from scripts.release_verification.candidate_artifact import ArtifactObservation
from scripts.release_verification.candidate_artifact import AttemptObservation
from scripts.release_verification.candidate_artifact import CandidateArtifact
from scripts.release_verification.candidate_artifact import WorkflowObservation
from scripts.release_verification.candidate_manifest import CANDIDATE_WORKFLOW_PATH
from scripts.release_verification.candidate_manifest import MANIFEST_PATH
from scripts.release_verification.candidate_manifest import npm_package_path
from scripts.release_verification.candidate_manifest import parse_manifest
from scripts.release_verification import candidate_npm_delivery as candidate_npm_delivery
from scripts.release_verification.candidate_npm_delivery import CONFIRMATION_INTERVAL_SECONDS
from scripts.release_verification.candidate_npm_delivery import deliver_npm
from scripts.acceptance.evidence.source import command
from scripts.acceptance.evidence.source import MAX_OUTPUT
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_verification.github import ArtifactDownload
from scripts.release_verification.published_contract import NPM_REGISTRY
from scripts.release_verification.published_contract import npm_integrity
from scripts.release_verification.published_contract import npm_shasum
from scripts.release_verification.published_ports import ConsumerPorts
from scripts.test_candidate_artifact_support import COMMIT, VERSION, locator, manifest_bytes, payload_files


TGZ = npm_package_path(VERSION)
OLDER = "0.1.36"
OTHER_COMMIT = "e5" * 20
TOKEN_CANARY = "npm-token-canary-5b1f"
USERCONFIG_CANARY = "userconfig-canary-9c2e"
VERSIONS, METADATA, LATEST, PUBLISH = (
    "candidate-npm-versions", "candidate-npm-metadata", "candidate-npm-latest", "candidate-npm-publish")
CONFIRM_VERSIONS, CONFIRM_METADATA, CONFIRM_LATEST = (
    "candidate-npm-confirm-versions", "candidate-npm-confirm-metadata", "candidate-npm-confirm-latest")
CONFIRM_ROUND = [CONFIRM_VERSIONS, CONFIRM_METADATA, CONFIRM_LATEST]

_FAKE_NPM = r'''#!{python}
import base64, hashlib, json, os, sys
argv = sys.argv[1:]
state_path = os.environ["FIXTURE_STATE"]
with open(state_path) as source:
    state = json.load(source)
log = {"argv": argv, "cwd": os.getcwd(), "token_variable": "NODE_AUTH_TOKEN" in os.environ}

def finish(reply, code=0):
    with open(os.environ["FIXTURE_LOG"], "a") as sink:
        sink.write(json.dumps(log) + "\n")
    with open(state_path + ".new", "w") as sink:
        json.dump(state, sink)
    os.replace(state_path + ".new", state_path)
    if reply is not None:
        sys.stdout.write(reply)
    sys.exit(code)

def metadata(version):
    package = state["packages"][version]
    row = {"name": "delegation-layer", "version": version,
           "dist": {"integrity": package["integrity"], "shasum": package["shasum"],
                    "tarball": "https://registry.npmjs.org/delegation-layer/-/delegation-layer-%s.tgz" % version}}
    if "gitHead" in package:
        row["gitHead"] = package["gitHead"]
    return json.dumps([row])

def publish():
    with open(argv[1], "rb") as source:
        data = source.read()
    log["published_sha256"] = hashlib.sha256(data).hexdigest()
    version = state["publish_version"]
    state["versions"].append(version)
    state["packages"][version] = state["publish_override"] or {
        "integrity": "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode(),
        "shasum": hashlib.sha1(data).hexdigest(), "gitHead": None}
    state["latest"] = version
    return ""

def listing():
    if state["hide_listings"] and state["publish_version"] in state["versions"]:
        state["hide_listings"] -= 1
        return json.dumps([item for item in state["versions"] if item != state["publish_version"]])
    return json.dumps(state["versions"])

def latest():
    if state["stale_latest"] and state["latest"] == state["publish_version"]:
        state["stale_latest"] -= 1
        return json.dumps(state["stale_value"])
    return json.dumps(state["latest"])

if argv[-2:] != ["--registry", "https://registry.npmjs.org/"]:
    finish(None, 2)
body = argv[:-2]
if body[0] == "publish":
    key, act = "publish", publish
elif body[:3] == ["view", "delegation-layer", "versions"]:
    key, act = "versions", lambda: listing()
elif body[:3] == ["view", "delegation-layer", "dist-tags.latest"]:
    key, act = "latest", lambda: latest()
elif body[0] == "view" and body[1].startswith("delegation-layer@"):
    version = body[1].split("@", 1)[1]
    key = "metadata"
    act = lambda: metadata(version) if version in state["packages"] else None
else:
    finish(None, 2)
log["key"] = key
fault = next((item for item in state["faults"] if item["key"] == key), None)
if fault is not None and fault["after"]:
    fault["after"] -= 1
    fault = None
if fault is not None:
    state["faults"].remove(fault)
    if fault["apply"]:
        act()
    finish(fault["output"], fault["exit"])
reply = act()
if reply is None:
    finish('{"error":{"code":"E404","summary":"Not Found"}}', 1)
finish(reply)
'''


@dataclass
class FakeClock:
    """Adapter-only elapsed time; the real child command owner keeps its native clock."""

    elapsed: float = 0
    oversleep: float = 0

    def monotonic(self) -> float:
        return self.elapsed

    def sleep(self, seconds: float) -> None:
        self.elapsed += seconds + self.oversleep


@dataclass(frozen=True)
class Fault:
    """Replace the reply of the request with this ``key`` after ``after`` matches pass, once.

    ``apply`` runs the request's effect first.
    """

    key: str
    output: str | None = None
    exit: int = 1
    apply: bool = False
    after: int = 0


def artifact(manifest: bytes, payload: Path) -> CandidateArtifact:
    """The loader's result for ``manifest`` with its payload at ``payload``; observations are inert."""
    bound = locator(b"container", manifest)
    return CandidateArtifact(
        locator=bound, manifest=parse_manifest(manifest), repository_id=1,
        workflow=WorkflowObservation(id=2, path=CANDIDATE_WORKFLOW_PATH, state="active"),
        attempt=AttemptObservation(run_id=bound.run_id, run_attempt=bound.run_attempt, workflow_id=2,
                                   head_sha=COMMIT, path=CANDIDATE_WORKFLOW_PATH, status="completed",
                                   conclusion="success"),
        artifact=ArtifactObservation(id=bound.artifact_id, name="candidate", size=1,
                                     sha256=bound.artifact_archive_sha256, run_id=bound.run_id, head_sha=COMMIT),
        container=ArtifactDownload(path=payload.parent / "container.zip", size=1,
                                   sha256=bound.artifact_archive_sha256),
        payload=payload)


class NpmDeliveryCase(unittest.TestCase):
    """A test-owned tree holding the fake ``npm``, its registry state and the candidate payload."""

    def setUp(self) -> None:
        self.descriptors = open_descriptors()
        self.addCleanup(lambda: self.assertEqual(open_descriptors(), self.descriptors))
        self.tree = FixtureTree.open(prefix="candidate-npm-delivery-test", label="candidate npm delivery fixture")
        self.addCleanup(self.tree.dispose)
        root = self.tree.root
        (root / "bin").mkdir()
        self.npm = root / "bin" / "npm"
        self.npm.write_text(_FAKE_NPM.replace("{python}", sys.executable, 1))
        self.npm.chmod(0o755)
        self.state_path, self.log_path = root / "state.json", root / "invocations.jsonl"
        self.files = payload_files()
        self.data = self.files[TGZ]
        self.manifest = manifest_bytes(self.files)
        self.payload = root / "payload"
        self.payload.mkdir()
        for name, data in {**self.files, MANIFEST_PATH: self.manifest}.items():
            (self.payload / name).write_bytes(data)
        self.receipt: dict = {"commands": []}
        self.calls: list[str] = []
        self.sleeps: list[float] = []
        self.clock = FakeClock()
        self.read_timeouts: list[int] = []
        self.round_starts: list[float] = []
        # Independent copies of each environment the edge hands the command owner, in call order.
        self.forwarded: list[dict[str, str]] = []
        # The release workflow's shape: a literal registry-scoped token in an owned userconfig and no
        # token variable.
        (root / USERCONFIG_CANARY).write_text(f"//registry.npmjs.org/:_authToken={TOKEN_CANARY}\n")
        self.environment = {
            "PATH": "/usr/bin:/bin", "FIXTURE_STATE": str(self.state_path), "FIXTURE_LOG": str(self.log_path),
            "NPM_CONFIG_USERCONFIG": str(root / USERCONFIG_CANARY),
        }
        self.save({"versions": ["0.1.32", OLDER], "packages": {OLDER: self.identity(b"older")}, "latest": OLDER,
                   "publish_version": VERSION, "publish_override": None,
                   "hide_listings": 0, "stale_latest": 0, "stale_value": OLDER, "faults": []})

    def reset(self) -> None:
        """Dispose of this case's tree through its cleanups, then open a fresh one for the next subtest."""
        self.doCleanups()
        self.setUp()

    @staticmethod
    def identity(data: bytes, **extra: object) -> dict:
        return {"integrity": npm_integrity(data), "shasum": npm_shasum(data), **extra}

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

    def publish_existing(self, data: bytes, **extra: object) -> None:
        """The registry already lists the target version with ``data``'s identity and latest at it."""
        def change(state: dict) -> None:
            state["versions"].append(VERSION)
            state["packages"][VERSION] = self.identity(data, **extra)
            state["latest"] = VERSION
        self.edit(change)

    def ports(self, before=None, inject: dict[str, BaseException] | None = None) -> ConsumerPorts:
        """The canonical command owner; ``before`` may change the payload and ``inject`` replaces a child."""
        def run(receipt, name, argv, env, **options):
            self.calls.append(name)
            if name == CONFIRM_VERSIONS:
                self.round_starts.append(self.clock.elapsed)
            if name.startswith("candidate-npm-confirm"):
                self.read_timeouts.append(options["timeout"])
            self.forwarded.append(dict(env))
            if before is not None:
                before(name)
            if inject is not None and name in inject:
                failure = inject[name]

                def launch():
                    raise failure
                return self.tree.observe_operation(launch)
            return self.tree.observe_operation(lambda: command(receipt, name, argv, env, **options))

        return ConsumerPorts(run=run, which=lambda name: str(self.npm) if name == "npm" else None)

    def deliver(self, *, resume_published: bool = False, environment: dict[str, str] | None = None,
                before=None, inject=None):
        def pause(seconds):
            self.sleeps.append(seconds)
            self.clock.sleep(seconds)

        with patch.object(candidate_npm_delivery, "sleep", pause), patch.object(
                candidate_npm_delivery, "monotonic", self.clock.monotonic):
            return self.tree.observe_operation(lambda: deliver_npm(
                artifact(self.manifest, self.payload), ports=self.ports(before, inject), receipt=self.receipt,
                environment=self.environment if environment is None else environment,
                resume_published=resume_published))

    def refused(self, code: str, **kwargs) -> VerificationError:
        with self.assertRaises(VerificationError) as caught:
            self.deliver(**kwargs)
        self.assertEqual(caught.exception.code, code)
        return caught.exception

    def log(self) -> list[dict]:
        if not self.log_path.exists():
            return []
        return [json.loads(line) for line in self.log_path.read_text().splitlines()]

    def keys(self) -> list[str]:
        return [entry["key"] for entry in self.log()]

    def assert_environment_unchanged(self, environment: dict[str, str]) -> None:
        """Every child got exactly ``environment`` at the command-owner boundary, and no token variable.

        The child's own ``os.environ`` is not compared: an interpreter may add variables at startup.
        """
        self.assertEqual(len(self.forwarded), len(self.receipt["commands"]))
        self.assertEqual(len(self.forwarded), len(self.log()))
        for forwarded in self.forwarded:
            self.assertEqual(forwarded, environment)
        self.assertFalse(any(entry["token_variable"] for entry in self.log()))

    def assert_no_secret_recorded(self) -> None:
        recorded = json.dumps(self.receipt)
        self.assertNotIn(TOKEN_CANARY, recorded)
        self.assertNotIn(USERCONFIG_CANARY, recorded)
        for record in self.receipt["commands"]:
            self.assertEqual(set(record), {"name", "argv", "exit_code", "cwd"})
            self.assertEqual(record["argv"][-2:], ["--registry", NPM_REGISTRY])

    def assert_retained(self, raised: BaseException, injected: BaseException) -> None:
        """An unproven closure retains the tree; remove it only after proving no child launched for it."""
        self.assertIs(raised, injected)
        self.assertIs(self.tree.unresolved, injected)
        self.assertFalse(self.tree.dispose())
        self.assertEqual(open_descriptors(), self.descriptors)
        # Every logged child is one receipt record whose exit code its owner read after Wait; the
        # injected failure launched no child, so it added neither a log line nor a record.
        records = self.receipt["commands"]
        self.assertEqual(len(records), len(self.log()))
        self.assertTrue(all(type(record["exit_code"]) is int for record in records))
        self.tree.remove_retained()
        self.assertFalse(self.tree.root.exists())


class PublishTests(NpmDeliveryCase):
    def test_absent_version_publishes_the_exact_tgz_once_then_confirms_identity_and_latest(self):
        identity = self.deliver()
        self.assertEqual(self.keys(), ["versions", "latest", "publish", "versions", "metadata", "latest"])
        self.assertEqual(self.calls, [VERSIONS, LATEST, PUBLISH, *CONFIRM_ROUND])
        self.assertEqual(self.sleeps, [])
        (publish,) = (entry for entry in self.log() if entry["key"] == "publish")
        self.assertEqual(publish["argv"], ["publish", str(self.payload / TGZ), "--access", "public",
                                           "--ignore-scripts", "--tag", "latest", "--registry", NPM_REGISTRY])
        self.assertEqual(publish["published_sha256"], sha(self.data))
        self.assertEqual((identity.version, identity.integrity, identity.shasum),
                         (VERSION, npm_integrity(self.data), npm_shasum(self.data)))
        self.assertIsNone(identity.git_head)
        self.assertTrue(all(entry["cwd"] == str(self.payload) for entry in self.log()))
        self.assertFalse(any(word in entry["argv"] for entry in self.log()
                             for word in ("pack", "--provenance", "dist-tag", "exec", "install")))
        self.assertEqual((self.payload / TGZ).read_bytes(), self.data)
        self.assert_no_secret_recorded()

    def test_publish_needs_no_credential_variable_and_passes_the_environment_unchanged(self):
        for label, dropped in (("userconfig only", ()), ("neither", ("NPM_CONFIG_USERCONFIG",))):
            with self.subTest(environment=label):
                self.reset()
                environment = {key: value for key, value in self.environment.items() if key not in dropped}
                supplied = dict(environment)
                self.assertNotIn("NODE_AUTH_TOKEN", supplied)
                self.deliver(environment=supplied)
                self.assertEqual(supplied, environment)
                self.assertEqual(self.keys().count("publish"), 1)
                self.assert_environment_unchanged(environment)
                self.assert_no_secret_recorded()

    def test_failed_publish_stops_without_retry_or_observation(self):
        for apply in (False, True):
            with self.subTest(applied=apply):
                self.reset()
                self.fault(Fault("publish", apply=apply))
                self.refused("command_failed")
                self.assertEqual(self.keys(), ["versions", "latest", "publish"])
                self.assertEqual(self.calls, [VERSIONS, LATEST, PUBLISH])
                self.assertEqual((self.receipt["commands"][-1]["exit_code"], self.sleeps), (1, []))

    def test_unknown_publish_outcome_stops_and_retains_the_tree(self):
        injected = VerificationError("command_timeout", PUBLISH)
        with self.assertRaises(VerificationError) as caught:
            self.deliver(inject={PUBLISH: injected})
        self.assertEqual(self.calls, [VERSIONS, LATEST, PUBLISH])
        self.assertEqual((self.keys(), self.sleeps), (["versions", "latest"], []))
        self.assert_retained(caught.exception, injected)


class ConfirmationTests(NpmDeliveryCase):
    """Read-only rounds after a known successful publish; nothing is published or tagged again."""

    def assert_one_publish_succeeded(self) -> None:
        publishes = [record for record in self.receipt["commands"] if record["name"] == PUBLISH]
        self.assertEqual([record["exit_code"] for record in publishes], [0])
        self.assertFalse(any("dist-tag" in entry["argv"] for entry in self.log()))

    def test_unlisted_then_stale_latest_then_target_latest_confirms(self):
        self.edit(lambda state: state.update(hide_listings=1, stale_latest=1))
        identity = self.deliver()
        self.assertEqual(self.calls, [VERSIONS, LATEST, PUBLISH, CONFIRM_VERSIONS, *CONFIRM_ROUND, *CONFIRM_ROUND])
        self.assertEqual(self.sleeps, [CONFIRMATION_INTERVAL_SECONDS] * 2)
        self.assertEqual(identity.integrity, npm_integrity(self.data))
        self.assert_one_publish_succeeded()

    def visibility(self, *, listed_after=0, latest_after=0, costs=None):
        """Registry replies follow elapsed time; command cost is charged before its reply."""
        def before(name):
            self.clock.elapsed += (costs or {}).get(name, 0)
            if name == CONFIRM_VERSIONS and self.clock.elapsed < listed_after:
                self.edit(lambda state: state.update(hide_listings=1))
            if name == CONFIRM_LATEST and self.clock.elapsed < latest_after:
                self.edit(lambda state: state.update(stale_latest=1))
        return before

    def test_registry_visibility_after_four_minutes_confirms_without_republishing(self):
        for field in ("listed_after", "latest_after"):
            with self.subTest(field=field):
                self.reset()
                identity = self.deliver(before=self.visibility(**{field: 240}))
                self.assertEqual(identity.integrity, npm_integrity(self.data))
                self.assertEqual(self.round_starts, list(range(0, 241, 10)))
                self.assert_one_publish_succeeded()

    def test_expired_window_is_pending_without_a_pointless_final_pause(self):
        self.refused("candidate_npm_confirmation_pending", before=self.visibility(listed_after=1000))
        self.assertEqual(self.round_starts, list(range(0, 600, 10)))
        self.assertEqual(self.sleeps, [10] * 59)
        self.assertEqual(self.clock.elapsed, 590)
        self.assert_one_publish_succeeded()

    def test_varying_read_cost_consumes_the_same_window(self):
        for field, costs, starts, end in (
                ("listed_after", {CONFIRM_VERSIONS: 7}, list(range(0, 596, 17)), 602),
                ("latest_after", {CONFIRM_VERSIONS: 2, CONFIRM_METADATA: 3, CONFIRM_LATEST: 5},
                 list(range(0, 581, 20)), 590)):
            with self.subTest(field=field):
                self.reset()
                self.refused("candidate_npm_confirmation_pending",
                             before=self.visibility(**{field: 1000}, costs=costs))
                self.assertEqual(self.round_starts, starts)
                self.assertEqual(self.clock.elapsed, end)
                self.assert_one_publish_succeeded()

    def test_prepublication_time_does_not_consume_confirmation_window(self):
        self.clock.elapsed = 900
        hook = self.visibility(listed_after=2140, costs={VERSIONS: 200, LATEST: 200, PUBLISH: 600})
        self.deliver(before=hook)
        self.assertEqual(self.round_starts, list(range(1900, 2141, 10)))
        self.assert_one_publish_succeeded()

    def test_sleep_rechecks_deadline_before_admitting_another_round(self):
        for oversleep in (590, 591):
            with self.subTest(oversleep=oversleep):
                self.reset()
                self.clock.oversleep = oversleep
                self.refused("candidate_npm_confirmation_pending", before=self.visibility(listed_after=1000))
                self.assertEqual(self.round_starts, [0])
                self.assertEqual(self.sleeps, [10])
                self.assertEqual(self.clock.elapsed, 10 + oversleep)
                self.assert_one_publish_succeeded()

    def test_last_admitted_round_keeps_full_read_budgets_and_can_finish_late(self):
        hook = self.visibility(listed_after=590)

        def before(name):
            if name.startswith("candidate-npm-confirm") and self.clock.elapsed >= 590:
                self.clock.elapsed += 60
            hook(name)

        self.deliver(before=before)
        self.assertEqual(self.round_starts, list(range(0, 600, 10)))
        self.assertEqual(self.clock.elapsed, 770)
        self.assertTrue(self.read_timeouts)
        self.assertEqual(set(self.read_timeouts), {60})
        self.assert_one_publish_succeeded()

    def test_command_timeout_remains_a_hard_fault_after_successful_publish(self):
        injected = VerificationError("command_timeout", CONFIRM_METADATA)
        with self.assertRaises(VerificationError) as caught:
            self.deliver(inject={CONFIRM_METADATA: injected})
        self.assertEqual(self.calls, [VERSIONS, LATEST, PUBLISH, CONFIRM_VERSIONS, CONFIRM_METADATA])
        self.assertEqual(self.sleeps, [])
        self.assert_one_publish_succeeded()
        self.assert_retained(caught.exception, injected)

    def test_errors_and_conflicts_stop_the_first_or_a_later_round(self):
        cases = (
            ("malformed listing", {}, Fault("versions", output="not json", exit=0, after=1),
             "candidate_npm_versions_invalid", [CONFIRM_VERSIONS], 0),
            ("failed later listing", {"hide_listings": 1}, Fault("versions", after=2),
             "command_failed", [CONFIRM_VERSIONS, CONFIRM_VERSIONS], 1),
            ("metadata not found", {}, Fault("metadata", output='{"error":{"code":"E404"}}'),
             "command_failed", [CONFIRM_VERSIONS, CONFIRM_METADATA], 0),
            ("oversized metadata", {}, Fault("metadata", output=" " * (MAX_OUTPUT + 1), exit=0),
             "command_output_exceeded", [CONFIRM_VERSIONS, CONFIRM_METADATA], 0),
            ("conflicting bytes", {"hide_listings": 1, "publish_override": self.identity(b"other bytes")}, None,
             "candidate_npm_conflict", [CONFIRM_VERSIONS, CONFIRM_VERSIONS, CONFIRM_METADATA], 1),
            ("conflicting revision", {"publish_override": self.identity(self.data, gitHead=OTHER_COMMIT)}, None,
             "npm_package_revision_mismatch", [CONFIRM_VERSIONS, CONFIRM_METADATA], 0),
            ("malformed later latest", {"stale_latest": 1}, Fault("latest", output='"next"', exit=0, after=2),
             "npm_latest_invalid", [*CONFIRM_ROUND, *CONFIRM_ROUND], 1),
        )
        for label, change, fault, code, rounds, pauses in cases:
            with self.subTest(case=label):
                self.reset()
                self.edit(lambda state, change=change: state.update(change))
                if fault is not None:
                    self.fault(fault)
                self.refused(code)
                self.assertEqual(self.calls, [VERSIONS, LATEST, PUBLISH, *rounds])
                self.assertEqual(self.sleeps, [CONFIRMATION_INTERVAL_SECONDS] * pauses)
                self.assert_one_publish_succeeded()

    def test_unknown_confirmation_read_stops_and_retains_the_tree(self):
        self.edit(lambda state: state.update(hide_listings=1))
        injected = BoundedCommandError(CommandFailure.TIMEOUT)
        with self.assertRaises(BoundedCommandError) as caught:
            self.deliver(inject={CONFIRM_METADATA: injected})
        self.assertEqual(self.calls, [VERSIONS, LATEST, PUBLISH, CONFIRM_VERSIONS, CONFIRM_VERSIONS,
                                      CONFIRM_METADATA])
        self.assertEqual(self.sleeps, [CONFIRMATION_INTERVAL_SECONDS])
        self.assert_retained(caught.exception, injected)


class ObservationTests(NpmDeliveryCase):
    def test_existing_exact_package_skips_with_optional_git_head(self):
        for extra in ({"gitHead": None}, {}, {"gitHead": COMMIT}):
            with self.subTest(git_head=extra):
                self.reset()
                self.publish_existing(self.data, **extra)
                identity = self.deliver()
                self.assertEqual(self.keys(), ["versions", "metadata", "latest"])
                self.assertEqual(self.calls, [VERSIONS, METADATA, LATEST])
                self.assertEqual(self.sleeps, [])
                self.assertEqual(identity.git_head, extra.get("gitHead"))
                self.assert_no_secret_recorded()

    def test_existing_package_with_other_bytes_or_revision_refuses(self):
        cases = ((b"other bytes", {}, "candidate_npm_conflict"),
                 (None, {"gitHead": OTHER_COMMIT}, "npm_package_revision_mismatch"))
        for data, extra, code in cases:
            with self.subTest(code=code):
                self.reset()
                self.publish_existing(self.data if data is None else data, **extra)
                self.refused(code)
                self.assertEqual(self.keys(), ["versions", "metadata"])

    def test_existing_exact_package_with_other_latest_refuses_without_retagging(self):
        self.publish_existing(self.data)
        self.edit(lambda state: state.update(latest=OLDER))
        self.refused("candidate_npm_latest_mismatch")
        self.assertEqual((self.keys(), self.sleeps), (["versions", "metadata", "latest"], []))

    def test_failed_or_malformed_listing_is_never_absence(self):
        cases = (
            (Fault("versions", output='{"error":{"code":"E404"}}'), "command_failed"),
            (Fault("versions", output="not json", exit=0), "candidate_npm_versions_invalid"),
            (Fault("versions", output="{}", exit=0), "candidate_npm_versions_invalid"),
            (Fault("versions", output="[]", exit=0), "candidate_npm_versions_invalid"),
            (Fault("versions", output='"0.1.36"', exit=0), "candidate_npm_versions_invalid"),
            (Fault("versions", output='["0.1.36", 1]', exit=0), "candidate_npm_versions_invalid"),
            (Fault("versions", output='["0.1.36", "0.1.36"]', exit=0), "candidate_npm_versions_invalid"),
            (Fault("versions", output=" " * (MAX_OUTPUT + 1) + "[]", exit=0), "command_output_exceeded"),
        )
        for fault, code in cases:
            with self.subTest(code=code, output=(fault.output or "")[:20]):
                self.reset()
                self.fault(fault)
                self.refused(code)
                self.assertEqual(self.calls, [VERSIONS])
                self.assertEqual(self.keys(), ["versions"])

    def test_listed_version_with_malformed_metadata_refuses(self):
        self.publish_existing(self.data)
        self.fault(Fault("metadata", output='[{"name": "delegation-layer"}, {}]', exit=0))
        self.refused("npm_view_invalid")
        self.assertEqual(self.keys(), ["versions", "metadata"])

    def test_resume_refuses_absent_package_and_accepts_the_exact_one(self):
        self.refused("candidate_resume_mutation_refused", resume_published=True)
        self.assertEqual(self.keys(), ["versions"])
        self.reset()
        self.publish_existing(self.data)
        self.assertEqual(self.deliver(resume_published=True).integrity, npm_integrity(self.data))
        self.assertEqual(self.keys(), ["versions", "metadata", "latest"])
        self.assertEqual(self.sleeps, [])

    def test_original_unproven_listing_failure_reaches_the_tree(self):
        injected = BoundedCommandError(CommandFailure.TIMEOUT)
        with self.assertRaises(BoundedCommandError) as caught:
            self.deliver(inject={VERSIONS: injected})
        self.assertEqual(self.keys(), [])
        self.assert_retained(caught.exception, injected)


class LatestGuardTests(NpmDeliveryCase):
    """Before publishing, npm's own ``latest`` must be strictly older than the target."""

    def test_newer_or_equal_npm_latest_refuses_without_publishing(self):
        for latest in ("0.1.40", VERSION):
            with self.subTest(latest=latest):
                self.reset()
                self.edit(lambda state, latest=latest: state.update(latest=latest))
                self.refused("candidate_version_not_newer")
                self.assertEqual(self.calls, [VERSIONS, LATEST])
                self.assertEqual((self.keys(), self.sleeps), (["versions", "latest"], []))
                self.assertEqual(self.state()["latest"], latest)

    def test_older_npm_latest_is_read_once_before_the_one_publish(self):
        self.deliver()
        self.assertEqual(self.calls[:3], [VERSIONS, LATEST, PUBLISH])
        self.assertEqual((self.calls.count(LATEST), self.calls.count(PUBLISH)), (1, 1))

    def test_failed_or_malformed_latest_read_stops_with_its_original_error(self):
        for fault, code in ((Fault("latest", output='{"error":{"code":"E404"}}'), "command_failed"),
                            (Fault("latest", output='"next"', exit=0), "npm_latest_invalid"),
                            (Fault("latest", output="[]", exit=0), "npm_latest_invalid")):
            with self.subTest(code=code, output=fault.output):
                self.reset()
                self.fault(fault)
                self.refused(code)
                self.assertEqual(self.calls, [VERSIONS, LATEST])
                self.assertEqual(self.keys(), ["versions", "latest"])

    def test_unknown_latest_read_stops_and_retains_the_tree(self):
        injected = BoundedCommandError(CommandFailure.TIMEOUT)
        with self.assertRaises(BoundedCommandError) as caught:
            self.deliver(inject={LATEST: injected})
        self.assertEqual(self.calls, [VERSIONS, LATEST])
        self.assertEqual(self.keys(), ["versions"])
        self.assert_retained(caught.exception, injected)

    def test_existing_exact_package_at_latest_skips_without_publish_or_retag(self):
        self.publish_existing(self.data)
        self.deliver()
        self.assertEqual(self.calls, [VERSIONS, METADATA, LATEST])
        self.assertFalse(any(word in entry["argv"] for entry in self.log() for word in ("publish", "dist-tag")))


class LocalPackageTests(NpmDeliveryCase):
    def test_tampered_package_refuses_before_any_child(self):
        (self.payload / TGZ).write_bytes(b"tampered")
        self.refused("candidate_npm_package_digest_mismatch")
        self.assertEqual((self.calls, self.receipt["commands"]), ([], []))

    def test_package_changed_after_observation_refuses_before_publish(self):
        def replace(path: Path) -> None:
            path.unlink()
            os.symlink(self.payload / MANIFEST_PATH, path)

        changes = {"rewritten": lambda path: path.write_bytes(self.data + b"x"), "symlinked": replace,
                   "same size": lambda path: path.write_bytes(bytes(len(self.data)))}
        for label, change in changes.items():
            with self.subTest(change=label):
                self.reset()

                def before(name: str, change=change) -> None:
                    if name == VERSIONS:
                        change(self.payload / TGZ)

                self.refused("candidate_npm_package_changed", before=before)
                # The npm latest guard reads before the payload recheck, which still precedes any publish.
                self.assertEqual(self.keys(), ["versions", "latest"])


if __name__ == "__main__":
    unittest.main()
