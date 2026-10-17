"""Reject false modern contention evidence without IO or subprocesses."""

from copy import deepcopy
import unittest

from scripts.delegate_acceptance.private_cli.contention_policy import (
    require_boundary_identity,
    require_boundary_order,
    require_content_identity,
    require_control_response,
    require_lease_mode,
    require_same_lock,
)


class ModernContentionPolicyTests(unittest.TestCase):
    def test_requires_exact_shared_exclusive_and_unlocked_facts(self):
        for mode, flags in (("shared", (True, False)), ("exclusive", (False, False)), ("unlocked", (True, True))):
            require_lease_mode(mode, *flags)
            for shared, exclusive in ((False, True), (True, False), (False, False), (True, True), (1, False)):
                if type(shared) is bool and (shared, exclusive) == flags:
                    continue
                with self.subTest(mode=mode, flags=(shared, exclusive)), self.assertRaisesRegex(RuntimeError, "lease"):
                    require_lease_mode(mode, shared, exclusive)
        with self.assertRaises(RuntimeError):
            require_lease_mode("unknown", True, False)

    def test_boundary_binds_cli_owner_client_nonce_and_leased_command(self):
        receipt = {
            "schema_version": 1,
            "nonce": "nonce",
            "cli_pid": 100,
            "control_owner_pid": 101,
            "client_pid": 102,
            "entered_ns": 3,
            "command": "status-json",
            "arguments": ["status", "--json"],
            "config": "/config",
        }
        self.assertIs(require_boundary_identity(receipt, "nonce", 100, "/config"), receipt)
        for key, bad in (
            ("schema_version", True),
            ("nonce", "other"),
            ("cli_pid", 103),
            ("cli_pid", True),
            ("control_owner_pid", 100),
            ("client_pid", 101),
            ("entered_ns", 0),
            ("command", "version"),
            ("arguments", ["--version"]),
            ("config", "/other"),
        ):
            with self.subTest(key=key, bad=bad), self.assertRaises(RuntimeError):
                require_boundary_identity({**receipt, key: bad}, "nonce", 100, "/config")

    def test_checks_every_lock_observation_and_boundary_order(self):
        lock = {"path": "/state/.supervisor/bootstrap.lock", "device": 1, "inode": 2, "file_mode": "0o600"}
        require_same_lock(lock, dict(lock), dict(lock), dict(lock))
        for key, bad in (("path", "/other"), ("device", 2), ("inode", 3), ("file_mode", "0o644")):
            with self.subTest(key=key), self.assertRaisesRegex(RuntimeError, "lock identity"):
                require_same_lock(lock, {**lock, key: bad}, lock)
        require_boundary_order({"entered_ns": 1}, 2, {"entered_ns": 3}, 4)
        with self.assertRaisesRegex(RuntimeError, "release order"):
            require_boundary_order({"entered_ns": 1}, 3, {"entered_ns": 2}, 4)

    def test_preserves_nested_content_and_exact_response_identity(self):
        content = {"task": {"meta": "digest", "closure": {"registration": {"scope_id": "exact"}}}}
        require_content_identity(content, deepcopy(content), "check")
        changed = deepcopy(content)
        changed["task"]["closure"]["registration"]["scope_id"] = "changed"
        with self.assertRaisesRegex(RuntimeError, "immutable"):
            require_content_identity(content, changed, "check")
        response = {"root_id": "root", "task_id": "task", "supervisor_control": {"state": "blocked"}}
        require_control_response(response, "root", task_id="task", migration_state="blocked")
        for root, task, state in (
            ("other", "task", "blocked"),
            ("root", "other", "blocked"),
            ("root", "task", "migrated"),
        ):
            with self.subTest(root=root, task=task, state=state), self.assertRaises(RuntimeError):
                require_control_response(response, root, task_id=task, migration_state=state)
