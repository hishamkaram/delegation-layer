"""Lifetime launch count, canonical byte budget, and Go policy parity checks."""

from __future__ import annotations

import ast
import json
from pathlib import Path
import re
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.evidence.limits import MAX_CONTROL_BYTES
from scripts.delegate_acceptance.evidence.timestamps import _canonical_timestamp
from scripts.delegate_acceptance.evidence.validation import AcceptanceFailure
from scripts.delegate_acceptance.lifetime import evidence, identity
from scripts.delegate_acceptance.lifetime.policy import (
    _account_lifetime_launch,
    _validate_lifetime_termination,
)
from scripts.delegate_acceptance.private_cli import capacity_records


ROOT = "a" * 32
TASK = "b" * 32
REGISTRATION = "c" * 64
GO_INTERNAL = Path(__file__).resolve().parents[3] / "internal"
# Go encoding/json output for these records was produced independently with the
# pinned go1.27.2 toolchain and matches the launch-set digest of commit d30054d.
ESCAPED_LAUNCH = {
    "schema_version": 1, "root_id": ROOT, "task_id": TASK,
    "registration_sha256": REGISTRATION, "sequence": 1, "pid": 101,
    "process_start": "start<a>&b\u2028c\u2029\u00e9\u6f22", "pgid": 0,
    "started_at": "2026-09-15T00:00:05Z",
}
GROUPED_LAUNCH = {
    "pgid": 77, "started_at": "2026-09-15T00:00:06.123456789Z", "schema_version": 1,
    "root_id": ROOT, "task_id": TASK, "registration_sha256": REGISTRATION,
    "sequence": 2, "pid": 102, "process_start": "plain",
}
GO_ESCAPED_LAUNCH_BYTES = 321
GO_GROUPED_LAUNCH_BYTES = 303
GO_LAUNCH_SET_SHA256 = "bd74ffd288891e3241b5ed0249e0cfcc59da4f2a6b727e03d5124445d0ab7fc8"
OWNER_KINDS = ("task", "inspection", "workspace")
UNBOUNDED = 1 << 62


def go_package_source(package: str) -> str:
    return "\n".join(path.read_text(encoding="utf-8")
                     for path in sorted((GO_INTERNAL / package).glob("*.go"))
                     if not path.name.endswith("_test.go"))


def go_task_constants(*names: str, package: str = "task") -> dict[str, int]:
    """Evaluate integer constant declarations from one Go package's source."""
    source = go_package_source(package)

    def value(name: str) -> int:
        match = re.search(r"^\s*(?:const\s+)?" + name + r"\s+(?:[A-Za-z0-9]+\s+)?=\s*([^\n/]+)",
                          source, re.MULTILINE)
        if match is None:
            raise AssertionError("Go constant is absent: " + name)
        return evaluate(ast.parse(match.group(1).strip(), mode="eval").body)

    def evaluate(node: ast.expr) -> int:
        if isinstance(node, ast.Constant) and type(node.value) is int:
            return node.value
        if isinstance(node, ast.Name):
            return value(node.id)
        if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Mult):
            return evaluate(node.left) * evaluate(node.right)
        if isinstance(node, ast.BinOp) and isinstance(node.op, ast.LShift):
            return evaluate(node.left) << evaluate(node.right)
        raise AssertionError("unsupported Go constant expression: " + ast.dump(node))

    return {name: value(name) for name in names}


def go_string_constants(package: str, *names: str) -> dict[str, str]:
    source = go_package_source(package)
    values = {}
    for name in names:
        match = re.search(r"^\s*" + name + r'\s*=\s*"([^"\\]*)"', source, re.MULTILINE)
        if match is None:
            raise AssertionError("Go string constant is absent: " + name)
        values[name] = match.group(1)
    return values


def launch_record(sequence: int) -> dict[str, object]:
    return {
        "schema_version": 1, "root_id": ROOT, "task_id": TASK,
        "registration_sha256": REGISTRATION, "sequence": sequence, "pid": 100 + sequence,
        "process_start": "start-%d" % sequence, "started_at": "2026-09-15T00:00:05Z",
    }


def sized_launch(sequence: int, canonical_bytes: int) -> dict[str, object]:
    """Return a launch whose Go canonical record is exactly canonical_bytes long."""
    record = launch_record(sequence)
    # ASCII without <, >, or & needs no Go escaping; MarshalCanonical appends a newline.
    base = len(json.dumps(record, separators=(",", ":"))) + 1
    record["process_start"] += "x" * (canonical_bytes - base)
    return record


ESCAPED_RECORD_CHARACTERS = 180000


class LifetimeLaunchCapacityTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="delegate-lifetime-capacity-")
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)

    def write_launches(self, owner_kind: str, records: list[dict[str, object]]) -> None:
        for record in records:
            name = identity._lifetime_record_name(owner_kind, "launch", record["sequence"])
            # Indented, sorted files prove accounting uses Go's canonical record bytes.
            (self.directory / name).write_text(json.dumps(record, sort_keys=True, indent=2))

    def write_compact_launch(self, owner_kind: str, record: dict[str, object]) -> int:
        name = identity._lifetime_record_name(owner_kind, "launch", record["sequence"])
        data = json.dumps(record, separators=(",", ":")).encode()
        (self.directory / name).write_bytes(data)
        return len(data)

    def clear(self) -> None:
        for path in self.directory.iterdir():
            path.unlink()

    def read_launches(self, owner_kind: str) -> list[dict[str, object]]:
        return evidence._read_lifetime_launches(
            directory=self.directory, evidence="fixture", owner_kind=owner_kind, recovered=False,
            registration_created=_canonical_timestamp("2026-09-15T00:00:01Z", "registration"),
            registration_digest=REGISTRATION, root=ROOT, task=TASK, window_deadline=None)

    def test_python_limits_project_the_go_task_policy(self):
        self.assertEqual(identity.MAX_LIFETIME_LAUNCHES, 4096)
        self.assertEqual(identity.MAX_LIFETIME_LAUNCH_BYTES, 64 * MAX_CONTROL_BYTES)
        for consumer in (evidence, capacity_records):
            with self.subTest(consumer=consumer.__name__):
                self.assertIs(consumer.MAX_LIFETIME_LAUNCHES, identity.MAX_LIFETIME_LAUNCHES)
                self.assertIs(consumer.MAX_LIFETIME_LAUNCH_BYTES, identity.MAX_LIFETIME_LAUNCH_BYTES)
        go = go_task_constants("MaxControlRecordSize", "MaxLifetimeLaunches", "MaxLifetimeLaunchBytes")
        self.assertEqual(go["MaxControlRecordSize"], MAX_CONTROL_BYTES)
        self.assertEqual(go["MaxLifetimeLaunches"], identity.MAX_LIFETIME_LAUNCHES)
        self.assertEqual(go["MaxLifetimeLaunchBytes"], identity.MAX_LIFETIME_LAUNCH_BYTES)

    def test_inventory_refuses_excess_before_consuming_more_names(self):
        launch_names = [identity._lifetime_record_name("task", "launch", sequence)
                        for sequence in range(1, 66)]

        def names():
            yield from launch_names
            raise AssertionError("inventory consumed names after its bound")

        bounded = identity._lifetime_launch_sequences(
            "task", ["notes.json", "launch.json", *launch_names[:64]], limit=64, label="over")
        self.assertEqual(bounded, list(range(1, 65)))
        with self.assertRaisesRegex(AcceptanceFailure, "^over$"):
            identity._lifetime_launch_sequences("task", names(), limit=64, label="over")

    def test_launch_namespace_projects_go_taskdir_naming(self):
        naming = go_string_constants(
            "taskdir", "lifetimeLaunchSuffix", "controlLifetimeLaunchPrefix",
            "controlLifetimeLaunchSuffix")
        digits = go_task_constants("lifetimeLaunchSequenceDigits", package="taskdir")
        self.assertEqual(identity._LIFETIME_LAUNCH_NAMING, {
            "task": ("", naming["lifetimeLaunchSuffix"]),
            "inspection": (naming["controlLifetimeLaunchPrefix"], naming["controlLifetimeLaunchSuffix"]),
            "workspace": (naming["controlLifetimeLaunchPrefix"], naming["controlLifetimeLaunchSuffix"]),
        })
        self.assertEqual(identity._LIFETIME_LAUNCH_SEQUENCE_DIGITS,
                         digits["lifetimeLaunchSequenceDigits"])

    def test_launch_shaped_names_outside_the_namespace_are_refused(self):
        unrelated = {
            "task": ("intent.json", "owner.lock", "launch.json", "1.launch.json.staging"),
            "inspection": ("lifetime-intent.json", "lifetime-launch.json", "request.json"),
            "workspace": ("lifetime-unlaunched.json", "start.json", "lifetime-launchx-1.json"),
        }
        for owner_kind in OWNER_KINDS:
            prefix, suffix = identity._LIFETIME_LAUNCH_NAMING[owner_kind]
            for name in unrelated[owner_kind]:
                with self.subTest(owner_kind=owner_kind, unrelated=name):
                    self.assertIsNone(identity._lifetime_launch_sequence(owner_kind, name))
            for sequence in (1, 4096):
                name = identity._lifetime_record_name(owner_kind, "launch", sequence)
                with self.subTest(owner_kind=owner_kind, valid=name):
                    self.assertEqual(identity._lifetime_launch_sequence(owner_kind, name), sequence)
            malformed = ("", "abc", "1", "0" * 18 + "1", "0" * 20 + "1", "+" + "0" * 18 + "1",
                         "\u0661" * 20, "18446744073709551616", "99999999999999999999")
            outside = ("0" * 20, "00000000000000004097", "18446744073709551615")
            for digits, message in ([(value, "malformed") for value in malformed] +
                                    [(value, "outside 1..4096") for value in outside]):
                name = prefix + digits + suffix
                with self.subTest(owner_kind=owner_kind, refused=name):
                    with self.assertRaisesRegex(AcceptanceFailure, message):
                        identity._lifetime_launch_sequence(owner_kind, name)

    def test_malformed_launch_name_is_refused_before_any_record_read(self):
        bad = {"task": "abc.launch.json", "inspection": "lifetime-launch-abc.json",
               "workspace": "lifetime-launch-00000000000000000000.json"}
        for owner_kind in OWNER_KINDS:
            with self.subTest(owner_kind=owner_kind):
                for path in self.directory.iterdir():
                    path.unlink()
                self.write_launches(owner_kind, [launch_record(1)])
                (self.directory / bad[owner_kind]).touch()
                with patch.object(evidence, "_read_control_record",
                                  side_effect=AssertionError("launch record was read")) as read:
                    with self.assertRaisesRegex(AcceptanceFailure, "launch sequence is invalid"):
                        self.read_launches(owner_kind)
                read.assert_not_called()

    def test_malformed_launch_name_cannot_prove_unregistered_absence(self):
        for owner_kind in OWNER_KINDS:
            prefix, suffix = identity._LIFETIME_LAUNCH_NAMING[owner_kind]
            with self.subTest(owner_kind=owner_kind):
                for path in self.directory.iterdir():
                    path.unlink()
                (self.directory / (prefix + "18446744073709551616" + suffix)).touch()
                with self.assertRaisesRegex(AcceptanceFailure, "contradictory native records"):
                    evidence._validate_intent_only_lifetime_absence(
                        self.directory, ROOT, TASK, owner_kind, {}, self.directory / "intent.json",
                        {"result": "absent"})

    def test_inventory_refuses_a_noncontiguous_later_name(self):
        for owner_kind in OWNER_KINDS:
            names = [identity._lifetime_record_name(owner_kind, "launch", sequence)
                     for sequence in (*range(1, 66), 67)]
            with self.subTest(owner_kind=owner_kind):
                with self.assertRaisesRegex(AcceptanceFailure, "gap"):
                    identity._lifetime_launch_sequences(owner_kind, iter(names), limit=4096, label="gap")

    def test_every_owner_accepts_64_and_65_launches(self):
        for owner_kind in OWNER_KINDS:
            for count in (64, 65):
                with self.subTest(owner_kind=owner_kind, count=count):
                    for path in self.directory.iterdir():
                        path.unlink()
                    self.write_launches(owner_kind, [launch_record(sequence)
                                                     for sequence in range(1, count + 1)])
                    self.assertEqual(len(self.read_launches(owner_kind)), count)

    def test_launch_4097_is_refused_before_any_record_read(self):
        for sequence in range(1, 4098):
            (self.directory / identity._lifetime_record_name("workspace", "launch", sequence)).touch()
        with patch.object(evidence, "_read_control_record",
                          side_effect=AssertionError("launch record was read")) as read:
            with self.assertRaisesRegex(AcceptanceFailure, "launch sequence is invalid"):
                self.read_launches("workspace")
        read.assert_not_called()

    def test_reader_refuses_a_gap_or_a_record_naming_another_sequence(self):
        self.write_launches("task", [launch_record(1), launch_record(3)])
        with self.assertRaisesRegex(AcceptanceFailure, "launch sequence is invalid"):
            self.read_launches("task")
        (self.directory / identity._lifetime_record_name("task", "launch", 3)).write_text(
            json.dumps(launch_record(2)))
        (self.directory / identity._lifetime_record_name("task", "launch", 3)).rename(
            self.directory / identity._lifetime_record_name("task", "launch", 2))
        (self.directory / identity._lifetime_record_name("task", "launch", 1)).write_text(
            json.dumps(launch_record(2)))
        with self.assertRaisesRegex(AcceptanceFailure, "launch binding mismatch"):
            self.read_launches("task")

    def test_canonical_record_bytes_match_go_encoding(self):
        self.assertEqual(_account_lifetime_launch(0, ESCAPED_LAUNCH, limit=UNBOUNDED, label="launch"),
                         GO_ESCAPED_LAUNCH_BYTES)
        self.assertEqual(_account_lifetime_launch(0, GROUPED_LAUNCH, limit=UNBOUNDED, label="launch"),
                         GO_GROUPED_LAUNCH_BYTES)
        without_pgid = {key: value for key, value in ESCAPED_LAUNCH.items() if key != "pgid"}
        self.assertEqual(_account_lifetime_launch(0, without_pgid, limit=UNBOUNDED, label="launch"),
                         GO_ESCAPED_LAUNCH_BYTES)
        decoded = len(json.dumps(ESCAPED_LAUNCH, ensure_ascii=False, separators=(",", ":")).encode())
        self.assertNotEqual(decoded, GO_ESCAPED_LAUNCH_BYTES)

    def test_existing_launch_set_digest_is_unchanged(self):
        termination = {
            "schema_version": 1, "root_id": ROOT, "task_id": TASK,
            "registration_sha256": REGISTRATION, "launch_count": 2,
            "launch_set_sha256": GO_LAUNCH_SET_SHA256, "cause": "completed",
            "scope_empty": True, "observed_at": "2026-09-15T00:00:07Z",
        }

        def validate(record: dict[str, object]) -> None:
            _validate_lifetime_termination(
                evidence="fixture", launches=[ESCAPED_LAUNCH, GROUPED_LAUNCH], recovered=False,
                registration_created=_canonical_timestamp("2026-09-15T00:00:01Z", "registration"),
                registration_digest=REGISTRATION, root=ROOT, task=TASK, termination=record, wait={})

        validate(termination)
        with self.assertRaisesRegex(AcceptanceFailure, "termination binding mismatch"):
            validate({**termination, "launch_set_sha256": "d" * 64})

    def test_aggregate_byte_budget_accepts_exact_and_refuses_one_more_byte(self):
        self.write_launches("inspection", [ESCAPED_LAUNCH, GROUPED_LAUNCH])
        exact = GO_ESCAPED_LAUNCH_BYTES + GO_GROUPED_LAUNCH_BYTES
        with patch.object(evidence, "MAX_LIFETIME_LAUNCH_BYTES", exact):
            self.assertEqual(len(self.read_launches("inspection")), 2)
        with patch.object(evidence, "MAX_LIFETIME_LAUNCH_BYTES", exact - 1):
            with self.assertRaisesRegex(AcceptanceFailure, "launch 2 .* byte budget"):
                self.read_launches("inspection")

    def test_each_record_must_fit_one_canonical_control_record(self):
        exact = sized_launch(1, MAX_CONTROL_BYTES)
        over = sized_launch(1, MAX_CONTROL_BYTES + 1)
        self.assertEqual(_account_lifetime_launch(7, exact, limit=UNBOUNDED, label="launch"),
                         7 + MAX_CONTROL_BYTES)
        used = 7
        with self.assertRaisesRegex(AcceptanceFailure, "^launch exceeds the control record bound$"):
            used = _account_lifetime_launch(used, over, limit=UNBOUNDED, label="launch")
        self.assertEqual(used, 7)
        for owner_kind in OWNER_KINDS:
            with self.subTest(owner_kind=owner_kind):
                self.clear()
                self.write_compact_launch(owner_kind, exact)
                self.assertEqual(self.read_launches(owner_kind), [exact])
                self.assertLessEqual(self.write_compact_launch(owner_kind, over), MAX_CONTROL_BYTES)
                with self.assertRaisesRegex(AcceptanceFailure, "launch 1 .* control record bound"):
                    self.read_launches(owner_kind)

    def test_escaping_can_push_a_small_raw_record_over_the_canonical_bound(self):
        record = launch_record(1)
        record["process_start"] = "<" * ESCAPED_RECORD_CHARACTERS
        for owner_kind in OWNER_KINDS:
            with self.subTest(owner_kind=owner_kind):
                self.clear()
                self.assertLess(self.write_compact_launch(owner_kind, record), MAX_CONTROL_BYTES // 5)
                with self.assertRaisesRegex(AcceptanceFailure, "launch 1 .* control record bound"):
                    self.read_launches(owner_kind)

    def test_record_bound_precedes_the_aggregate_and_keeps_the_total(self):
        small = _account_lifetime_launch(0, launch_record(1), limit=UNBOUNDED, label="launch")
        exact = sized_launch(2, MAX_CONTROL_BYTES)
        self.assertEqual(_account_lifetime_launch(small, exact, limit=small + MAX_CONTROL_BYTES,
                                                  label="launch"), small + MAX_CONTROL_BYTES)
        used = small
        with self.assertRaisesRegex(AcceptanceFailure, "byte budget"):
            used = _account_lifetime_launch(used, exact, limit=small + MAX_CONTROL_BYTES - 1,
                                            label="launch")
        with self.assertRaisesRegex(AcceptanceFailure, "control record bound"):
            used = _account_lifetime_launch(used, sized_launch(2, MAX_CONTROL_BYTES + 1),
                                            limit=small, label="launch")
        self.assertEqual(used, small)


if __name__ == "__main__":
    unittest.main()
