"""Noexec state: execution probe, fresh registrations, and legacy custody cleanup."""

from contextlib import redirect_stderr
import io
import os
from pathlib import Path
import stat
import sys
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance_tests.record_fixtures import object_at
from scripts.acceptance_tests.private_cli.fixtures import (
    canonical_pool_marker,
    create_private_pool_directory,
    reported_as_another_user,
    write_pool_marker,
)
from scripts.delegate_acceptance.directories.policy import require_private_directory
from scripts.delegate_acceptance.private_cli import application, arguments, noexec_state
from scripts.delegate_acceptance.private_cli.configuration import (
    DEFAULT_STOP_SCENARIO,
    EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
    EXTERNAL_NOEXEC_STATE_SCENARIO,
    SUPERVISOR_UPGRADE_RECOVERY_SCENARIO,
)
from scripts.delegate_acceptance.private_cli.errors import AcceptanceBlocked
from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError,
    BoundedCommandResult,
    CommandFailure,
)

POOL_A = "a" * 64
POOL_B = "b" * 64
TASK = "1" * 32
OTHER_TASK = "2" * 32
INSPECTION = "3" * 32


def invocation(*extra, scenario=EXTERNAL_NOEXEC_STATE_SCENARIO):
    return [
        "acceptance-private-cli", "--delegate", "/b/delegate", "--runner", "/b/delegate-run",
        "--pueue", "/b/pueue", "--pueued", "/b/pueued", "--output", "/b/receipt.json",
        "--scenario", scenario, *extra,
    ]


def parse(*extra, scenario=EXTERNAL_NOEXEC_STATE_SCENARIO):
    with patch.object(sys, "argv", invocation(*extra, scenario=scenario)):
        return arguments.parse_arguments()


class PrivateDirectoryPredicateTests(unittest.TestCase):
    """The shared pure predicate judges only the facts and owner it is given."""

    OWNER = 4242

    @staticmethod
    def info(mode, uid):
        return os.stat_result((mode, 1, 1, 1, uid, 0, 0, 0, 0, 0))

    def test_a_directory_of_the_given_owner_without_group_or_world_bits_is_private(self):
        for mode in (0o700, 0o500, 0o000):
            with self.subTest(mode=oct(mode)):
                require_private_directory(self.info(stat.S_IFDIR | mode, self.OWNER), "refused", owner_uid=self.OWNER)

    def test_shared_bits_another_owner_or_a_non_directory_is_refused(self):
        refused = {
            "group-bit": self.info(stat.S_IFDIR | 0o750, self.OWNER),
            "world-bit": self.info(stat.S_IFDIR | 0o701, self.OWNER),
            "other-owner": self.info(stat.S_IFDIR | 0o700, self.OWNER + 1),
            "regular-file": self.info(stat.S_IFREG | 0o700, self.OWNER),
            "symlink": self.info(stat.S_IFLNK | 0o700, self.OWNER),
        }
        for name, info in refused.items():
            with self.subTest(case=name):
                self.assertRaisesRegex(
                    RuntimeError, "^refused$", require_private_directory, info, "refused", owner_uid=self.OWNER
                )


class NoexecArgumentsTests(unittest.TestCase):
    def rejected(self, *extra, scenario=EXTERNAL_NOEXEC_STATE_SCENARIO):
        errors = io.StringIO()
        with redirect_stderr(errors), self.assertRaises(SystemExit) as raised:
            parse(*extra, scenario=scenario)
        self.assertEqual(raised.exception.code, 2)
        return errors.getvalue()

    def test_scenario_accepts_one_task_and_a_noexec_state_parent(self):
        args, workers, _ = parse("--parallel-tasks", "1", "--noexec-state-parent", "/mnt/noexec")
        self.assertEqual(args.noexec_state_parent, Path("/mnt/noexec"))
        self.assertEqual((args.scenario, workers), (EXTERNAL_NOEXEC_STATE_SCENARIO, 1))

    def test_scenario_requires_the_noexec_state_parent(self):
        self.assertIn("--noexec-state-parent is required by", self.rejected("--parallel-tasks", "1"))

    def test_other_scenarios_never_accept_a_noexec_state_parent(self):
        for scenario in ("capacity", DEFAULT_STOP_SCENARIO, EXTERNAL_INSPECTION_HANDOFF_SCENARIO,
                         SUPERVISOR_UPGRADE_RECOVERY_SCENARIO):
            with self.subTest(scenario=scenario):
                message = self.rejected("--noexec-state-parent", "/mnt/noexec", scenario=scenario)
                self.assertIn("only valid for, --scenario " + EXTERNAL_NOEXEC_STATE_SCENARIO, message)

    def test_scenario_runs_exactly_one_external_task(self):
        for count in ("0", "2", "200"):
            with self.subTest(count=count):
                self.assertIn(
                    "--parallel-tasks must be exactly 1",
                    self.rejected("--parallel-tasks", count, "--noexec-state-parent", "/mnt/noexec"),
                )

    def test_scenario_rejects_runtime_only_and_upgraded_installations(self):
        for extra in (["--runtime-only"], ["--upgraded-delegate", "/c/delegate"], ["--upgraded-runner", "/c/runner"]):
            with self.subTest(extra=extra):
                message = self.rejected("--parallel-tasks", "1", "--noexec-state-parent", "/mnt/noexec", *extra)
                self.assertIn("current installation only", message)

    def test_rejection_happens_before_any_fixture_setup(self):
        with patch.object(sys, "argv", invocation("--parallel-tasks", "1")), \
                patch.object(application, "prepare_fixture") as setup, \
                redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            application.main()
        setup.assert_not_called()


class NoexecProbeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-noexec-probe-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.noexec = self.root / "noexec"
        self.execution = self.root / "execution"
        self.noexec.mkdir()
        self.execution.mkdir()

    def run_probe(self, *outcomes):
        with patch.object(noexec_state, "run_bounded_command", side_effect=list(outcomes)) as run:
            try:
                return noexec_state.require_execution_refused(self.noexec, self.execution), run
            except AcceptanceBlocked as blocked:
                return blocked, run

    def test_refusal_is_proven_only_against_an_executing_control(self):
        evidence, run = self.run_probe(
            BoundedCommandResult(0, b""), BoundedCommandError(CommandFailure.START_FAILED)
        )
        self.assertEqual(evidence, {"control": "executed", "state_container": "execution-refused"})
        self.assertEqual(
            [call.args[0] for call in run.call_args_list],
            [[str(self.execution / ".execution-probe")], [str(self.noexec / ".execution-probe")]],
        )
        self.assertEqual(list(self.noexec.iterdir()) + list(self.execution.iterdir()), [])

    def test_an_executable_state_directory_is_blocked(self):
        blocked, _ = self.run_probe(BoundedCommandResult(0, b""), BoundedCommandResult(0, b""))
        self.assertEqual(blocked.code, "noexec-state-parent-executable")
        self.assertEqual(list(self.noexec.iterdir()), [])

    def test_a_control_that_cannot_run_blocks_before_judging_the_state_directory(self):
        for control in (BoundedCommandError(CommandFailure.START_FAILED), BoundedCommandResult(7, b"")):
            with self.subTest(control=control):
                blocked, run = self.run_probe(control)
                self.assertEqual(blocked.code, "execution-probe-unavailable")
                self.assertEqual(run.call_count, 1)

    def test_other_failures_in_the_state_directory_are_inconclusive_not_refusals(self):
        for outcome in (BoundedCommandError(CommandFailure.TIMEOUT), BoundedCommandResult(3, b"")):
            with self.subTest(outcome=outcome):
                blocked, _ = self.run_probe(BoundedCommandResult(0, b""), outcome)
                self.assertEqual(blocked.code, "noexec-state-probe-inconclusive")
                self.assertEqual(list(self.noexec.iterdir()), [])

    def test_an_unwritable_directory_blocks(self):
        self.noexec.rmdir()
        blocked, _ = self.run_probe(BoundedCommandResult(0, b""))
        self.assertEqual(blocked.code, "execution-probe-unavailable")

    def test_a_real_probe_executes_in_an_executable_directory_and_leaves_nothing(self):
        self.assertEqual(noexec_state._probe_execution(self.execution), "executed")
        self.assertEqual(list(self.execution.iterdir()), [])


class UnplacedRegistrationTests(unittest.TestCase):
    def setUp(self):
        self.closures: dict[str, object] = {
            "tasks": {TASK: {"registration": {"scope_kind": "process-group"}}},
            "inspections": {INSPECTION: {"registration": {"scope_kind": "process-group"}}},
        }

    def test_each_registered_task_and_inspection_is_reported(self):
        object_at(self.closures, "tasks")[OTHER_TASK] = {"registration": {"scope_kind": "process-group"}}
        self.assertEqual(noexec_state.require_unplaced_registrations(self.closures),
                         {"tasks": sorted([TASK, OTHER_TASK]), "inspections": [INSPECTION]})

    def test_any_present_placement_in_either_namespace_is_refused(self):
        for kind, identity in (("tasks", TASK), ("inspections", INSPECTION)):
            registration = object_at(self.closures, kind, identity, "registration")
            for value in (None, {}, "", {"version": 1, "pool_id": POOL_A, "pool_root": "/pool"}):
                with self.subTest(kind=kind, value=value):
                    registration["custody_placement"] = value
                    with self.assertRaisesRegex(RuntimeError, "registration contains custody placement"):
                        noexec_state.require_unplaced_registrations(self.closures)
            del registration["custody_placement"]

    def test_another_valid_registration_cannot_vouch_for_a_placed_one(self):
        object_at(self.closures, "tasks")[OTHER_TASK] = {"registration": {"custody_placement": None}}
        with self.assertRaisesRegex(RuntimeError, "task " + OTHER_TASK):
            noexec_state.require_unplaced_registrations(self.closures)

    def test_genuinely_unregistered_rows_are_skipped(self):
        for kind in ("tasks", "inspections"):
            object_at(self.closures, kind)[OTHER_TASK] = {"registration": None}
            object_at(self.closures, kind)["unlaunched"] = {}
        self.assertEqual(noexec_state.require_unplaced_registrations(self.closures),
                         {"tasks": [TASK], "inspections": [INSPECTION]})

    def test_each_namespace_needs_at_least_one_registration(self):
        for kind in ("tasks", "inspections"):
            original = self.closures[kind]
            for rows in ({}, {OTHER_TASK: {"registration": None}}, {OTHER_TASK: {}}):
                with self.subTest(kind=kind, rows=rows):
                    self.closures[kind] = rows
                    with self.assertRaisesRegex(RuntimeError, "no " + kind + " lifetime registration"):
                        noexec_state.require_unplaced_registrations(self.closures)
            self.closures[kind] = original

    def test_missing_or_malformed_proof_members_are_refused(self):
        for value in (None, [], {}, {"tasks": self.closures["tasks"]},
                      {"tasks": [], "inspections": self.closures["inspections"]},
                      {"tasks": {TASK: None}, "inspections": self.closures["inspections"]},
                      {"tasks": {TASK: {"registration": []}}, "inspections": self.closures["inspections"]}):
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                noexec_state.require_unplaced_registrations(value)


class CustodyTree:
    """Real directories for recorded pools; the closure maps are plain published-record shapes."""

    def __init__(self, test):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-noexec-custody-")
        test.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.state = self.root / "noexec" / "state"
        self.execution = self.root / "execution"
        self.state.mkdir(parents=True)
        self.execution.mkdir()

    def pool(self, name, pool_id, parent=None, marker=True):
        root = create_private_pool_directory((parent or self.execution) / name)
        create_private_pool_directory(root / "views")
        if marker:
            write_pool_marker(root, canonical_pool_marker(pool_id))
        return root

    @staticmethod
    def registration(root, pool_id, overrides=None):
        record = {"version": 1, "pool_root": str(root), "pool_id": pool_id}
        record.update(overrides or {})
        return {"scope_kind": "process-group", "custody_placement": record}

    @staticmethod
    def closures(tasks, inspections):
        return {
            "root_id": "r" * 32,
            "tasks": {name: {"registration": registration} for name, registration in tasks.items()},
            "inspections": {name: {"registration": registration} for name, registration in inspections.items()},
        }

    def prove(self, closures):
        return noexec_state.custody_release_pending(closures, (self.state, self.execution))


class LegacyCustodyCleanupTests(unittest.TestCase):
    def setUp(self):
        self.tree = CustodyTree(self)
        self.pool_a = self.tree.pool("pool-a", POOL_A)
        self.pool_b = self.tree.pool("pool-b", POOL_B)
        self.good_task = self.tree.registration(self.pool_a, POOL_A)
        self.good_inspection = self.tree.registration(self.pool_b, POOL_B)

    def assert_refused(self, closures, pattern):
        with self.assertRaisesRegex(RuntimeError, pattern):
            self.tree.prove(closures)

    def test_each_legacy_pool_with_no_owner_views_is_released(self):
        self.assertEqual(self.tree.prove(
            self.tree.closures({TASK: self.good_task}, {INSPECTION: self.good_inspection})
        ), ())

    def test_shared_legacy_pools_still_validate_each_record(self):
        shared = self.tree.registration(self.pool_a, POOL_A)
        self.assertEqual(self.tree.prove(self.tree.closures({TASK: shared, OTHER_TASK: shared},
                                                          {INSPECTION: shared})), ())
        broken = self.tree.registration(self.pool_a, POOL_B)
        self.assert_refused(
            self.tree.closures({TASK: shared, OTHER_TASK: broken}, {INSPECTION: shared}),
            "conflicting identities",
        )

    def test_unplaced_or_unregistered_rows_need_no_legacy_cleanup(self):
        closures = self.tree.closures({TASK: self.good_task}, {INSPECTION: self.good_inspection})
        closures["inspections"][OTHER_TASK] = {"registration": None}
        closures["tasks"][OTHER_TASK] = {"registration": {"scope_kind": "process-group"}}
        self.assertEqual(self.tree.prove(closures), ())

    def test_legacy_cleanup_accepts_owned_state_and_execution_pools(self):
        self.assertEqual(self.tree.prove(
            self.tree.closures({TASK: self.tree.registration(self.pool_a, POOL_A)}, {
                INSPECTION: self.tree.registration(self.tree.pool(".custody", POOL_B, parent=self.tree.state), POOL_B)
            }),
        ), ())

    def test_a_pool_outside_or_equal_to_an_owned_root_is_refused(self):
        elsewhere = self.tree.pool("elsewhere", POOL_B, parent=self.tree.root)
        for root, pool_id in ((elsewhere, POOL_B), (self.tree.execution, POOL_B)):
            with self.subTest(root=root.name):
                write_pool_marker(root, canonical_pool_marker(pool_id))
                self.assert_refused(
                    self.tree.closures({TASK: self.tree.registration(root, pool_id)}, {INSPECTION: self.good_inspection}),
                    "task " + TASK + " custody pool is outside the fixture-owned roots",
                )

    def test_a_symlinked_alias_of_a_valid_pool_is_not_canonical(self):
        alias = self.tree.execution / "alias"
        alias.symlink_to(self.pool_a)
        self.assert_refused(
            self.tree.closures({TASK: self.tree.registration(alias, POOL_A)}, {INSPECTION: self.good_inspection}),
            "task " + TASK + " custody pool root is not canonical",
        )

    def test_a_missing_pool_or_marker_is_unreadable(self):
        missing = self.tree.execution / "missing"
        self.assert_refused(
            self.tree.closures({TASK: self.tree.registration(missing, POOL_A)}, {INSPECTION: self.good_inspection}),
            "task " + TASK + " custody pool or its comparison root is unreadable",
        )
        unmarked = self.tree.pool("unmarked", POOL_A, marker=False)
        self.assert_refused(
            self.tree.closures({TASK: self.tree.registration(unmarked, POOL_A)}, {INSPECTION: self.good_inspection}),
            "task " + TASK + " custody pool identity marker is unreadable",
        )

    def test_the_marker_must_name_the_registered_pool_id(self):
        self.assert_refused(
            self.tree.closures({TASK: self.tree.registration(self.pool_a, POOL_B)}, {INSPECTION: self.good_inspection}),
            "task " + TASK + " custody pool marker does not match its registration",
        )

    def prove_good(self):
        return self.tree.prove(self.tree.closures({TASK: self.good_task}, {INSPECTION: self.good_inspection}))

    def test_the_canonical_private_marker_is_accepted(self):
        self.assertEqual(self.prove_good(), ())

    def test_a_marker_that_is_not_the_canonical_version_one_record_is_refused(self):
        # Go's decodePoolIdentity accepts only its own json.Marshal bytes; json.loads accepts most of these.
        valid = canonical_pool_marker(POOL_A).decode()
        contents = {
            "other version": '{"version":2,"pool_id":"%s"}' % POOL_A,
            "unknown field": '{"version":1,"pool_id":"%s","x":1}' % POOL_A,
            "boolean version": '{"version":true,"pool_id":"%s"}' % POOL_A,
            "array": "[]",
            "duplicate key": '{"version":1,"version":1,"pool_id":"%s"}' % POOL_A,
            "duplicate id": '{"version":1,"pool_id":"%s","pool_id":"%s"}' % (POOL_B, POOL_A),
            "trailing newline": valid + "\n",
            "leading whitespace": " " + valid,
            "inner whitespace": '{"version": 1, "pool_id": "%s"}' % POOL_A,
            "reordered keys": '{"pool_id":"%s","version":1}' % POOL_A,
            "float version": '{"version":1.0,"pool_id":"%s"}' % POOL_A,
            "escaped id": '{"version":1,"pool_id":"\\u0061%s"}' % POOL_A[1:],
            "uppercase id": canonical_pool_marker(POOL_A.upper()).decode(),
            "short id": canonical_pool_marker("abc").decode(),
        }
        for name, content in contents.items():
            with self.subTest(name=name):
                write_pool_marker(self.pool_a, content.encode())
                self.assertRaisesRegex(
                    RuntimeError, "task " + TASK + " custody pool identity marker is not the canonical version 1 record",
                    self.prove_good)
        for name, content in {"not json": b"not json", "empty": b""}.items():
            with self.subTest(name=name):
                write_pool_marker(self.pool_a, content)
                self.assertRaisesRegex(RuntimeError, "task " + TASK + " custody pool identity marker is not JSON",
                                       self.prove_good)

    def test_a_marker_that_is_not_private_to_this_user_is_refused(self):
        for mode in (0o644, 0o640, 0o400, 0o700, 0o4600):
            with self.subTest(mode=oct(mode)):
                write_pool_marker(self.pool_a, canonical_pool_marker(POOL_A), mode)
                self.assertRaisesRegex(
                    RuntimeError, "task " + TASK + " custody pool identity marker has an unsafe owner or mode",
                    self.prove_good)
        write_pool_marker(self.pool_a, canonical_pool_marker(POOL_A))
        with reported_as_another_user(self.pool_a / ".identity"):
            self.assertRaisesRegex(
                RuntimeError, "task " + TASK + " custody pool identity marker has an unsafe owner or mode",
                self.prove_good)

    def test_a_pool_root_without_group_or_world_bits_is_accepted(self):
        for mode in (0o700, 0o500):
            with self.subTest(mode=oct(mode)):
                self.pool_a.chmod(mode)
                self.addCleanup(self.pool_a.chmod, 0o700)
                self.assertEqual(self.prove_good(), ())

    def test_a_pool_root_that_is_not_private_to_this_user_is_refused(self):
        message = "task " + TASK + " custody pool root is not a private directory of this user"
        for mode in (0o755, 0o750, 0o705, 0o701):
            with self.subTest(mode=oct(mode)):
                self.pool_a.chmod(mode)
                self.assertRaisesRegex(RuntimeError, message, self.prove_good)
        self.pool_a.chmod(0o700)
        with reported_as_another_user(self.pool_a):
            self.assertRaisesRegex(RuntimeError, message, self.prove_good)

    def test_an_oversized_directory_or_fifo_marker_is_not_a_bounded_regular_file(self):
        oversized = self.tree.pool("oversized", POOL_A, marker=False)
        write_pool_marker(oversized, canonical_pool_marker(POOL_A) + b" " * noexec_state.MAX_POOL_IDENTITY_BYTES)
        directory = self.tree.pool("directory", POOL_A, marker=False)
        (directory / ".identity").mkdir(mode=0o700)
        fifo = self.tree.pool("fifo", POOL_A, marker=False)
        os.mkfifo(fifo / ".identity", 0o600)
        for root in (oversized, directory, fifo):
            with self.subTest(root=root.name):
                self.assert_refused(
                    self.tree.closures({TASK: self.tree.registration(root, POOL_A)}, {INSPECTION: self.good_inspection}),
                    "task " + TASK + " custody pool identity marker is not a bounded regular file",
                )

    def test_a_symlinked_marker_is_not_a_regular_file(self):
        marker = self.pool_a / ".identity"
        target = self.tree.root / "target-marker"
        target.write_text(marker.read_text())
        marker.unlink()
        marker.symlink_to(target)
        self.assert_refused(
            self.tree.closures({TASK: self.good_task}, {INSPECTION: self.good_inspection}),
            "task " + TASK + " custody pool identity marker is not a bounded regular file",
        )

    def test_malformed_placements_are_refused_by_shape(self):
        shapes = {
            "version": ({"version": 2}, "version is unsupported"),
            "boolean version": ({"version": True}, "version is unsupported"),
            "extra field": ({"extra": "x"}, "unknown or missing fields"),
            "dot segment": ({"pool_root": str(self.pool_a.parent) + "/./pool-a"}, "not a clean absolute path"),
            "embedded NUL": ({"pool_root": str(self.pool_a) + "\x00"}, "not a clean absolute path"),
            "double leading slash": ({"pool_root": "/" + str(self.pool_a)}, "not a clean absolute path"),
            "uppercase id": ({"pool_id": POOL_A.upper()}, "pool id is not 64 lowercase hex"),
            "short id": ({"pool_id": "a" * 63}, "pool id is not 64 lowercase hex"),
            "relative root": ({"pool_root": "pool-a"}, "not a clean absolute path"),
            "parent segment": ({"pool_root": str(self.pool_a) + "/../pool-a"}, "not a clean absolute path"),
            "trailing separator": ({"pool_root": str(self.pool_a) + "/"}, "not a clean absolute path"),
            "numeric root": ({"pool_root": 7}, "pool root is not text"),
        }
        for name, (change, pattern) in shapes.items():
            with self.subTest(name=name):
                registration = self.tree.registration(self.pool_a, POOL_A, change)
                self.assert_refused(self.tree.closures({TASK: registration}, {INSPECTION: self.good_inspection}), pattern)


if __name__ == "__main__":
    unittest.main()
