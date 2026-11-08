"""Every native success branch publishes the one shutdown and closure fragment."""

from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import MagicMock, patch

from scripts.acceptance.native import scenarios as scenarios_impl
from scripts.acceptance.native.constants import TIMEOUT_SOURCE_RUNNER_RESULTS


FRAGMENT = {"natural_daemon_shutdown": True, "lifetime_closure": {"kind": "native-lifetime-closure"}}
FRESH, RESUME = "1" * 32, "2" * 32


class SuccessReceiptFragmentTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-success-fragment-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        for helper, value in (("workspace_snapshot", {}), ("task_digest_snapshot", {}),
                              ("require_same_session", None), ("require_replay_unchanged", None),
                              ("done_result", sorted(TIMEOUT_SOURCE_RUNNER_RESULTS)[0])):
            patcher = patch.object(scenarios_impl, helper, return_value=value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def owner(self, **values) -> MagicMock:
        owner = MagicMock()
        owner.args = SimpleNamespace(model=None, effort=None)
        owner.mode, owner.scenario = "read-only", "read-only"
        owner.output = self.root
        owner.lifecycle_receipt.return_value = dict(FRAGMENT)
        owner.provider_runtime_receipt.return_value = {}
        owner.production_acp_receipt.return_value = {}
        owner.native_mode_receipt.return_value = None
        owner.tasks = {"fresh": FRESH, "resume": RESUME}
        for name, value in values.items():
            setattr(owner, name, value)
        return owner

    def assert_fragment_after_shutdown(self, owner: MagicMock) -> None:
        receipt = owner.success_receipt
        self.assertEqual({key: receipt[key] for key in FRAGMENT}, FRAGMENT)
        names = [call[0] for call in owner.mock_calls]
        self.assertLess(names.index("shutdown"), names.index("lifecycle_receipt"))
        owner.lifecycle_receipt.assert_called_once_with()

    def test_resumable_review_and_model_discovery_branches(self):
        for label, run, values in (
                ("resumable", scenarios_impl._run_resumable, {"provider": "claude:print"}),
                ("review", scenarios_impl._run_review, {"provider": "codex:review"}),
                ("model-discovery", scenarios_impl._run_model_discovery,
                 {"provider": "claude:print", "model_discovery_exit_code": 0,
                  "model_discovery_summary": {"status": "available"}})):
            with self.subTest(branch=label):
                owner = self.owner(**values)
                owner.dispatch.return_value = FRESH
                owner.continue_task.return_value = RESUME
                run(owner)
                self.assert_fragment_after_shutdown(owner)

    def test_timeout_resume_branch(self):
        source_directory = self.root / "source"
        source_directory.mkdir()
        owner = self.owner(provider="codex:exec", mode="workspace-write", scenario="timeout-resume")
        owner.dispatch.return_value = FRESH
        owner.continue_public.return_value = RESUME
        owner.wait_timed_out.return_value = {"continuation": {"state": "resumed"}}
        owner.collect_timed_out_source.return_value = {"publication": "unpublished"}
        owner.timeout_source_binding.return_value = source_directory
        scenarios_impl.run_timeout_resume(owner)
        self.assert_fragment_after_shutdown(owner)
        self.assertEqual(owner.success_receipt["tasks"], {"timeout_source": FRESH, "timeout_resume": RESUME})

    def test_interactive_branch(self):
        state = self.root / "state"
        interaction = state / "tasks" / FRESH / "raw" / "interaction"
        interaction.parent.mkdir(parents=True)
        interaction.write_bytes(b"sealed interaction\n")
        owner = self.owner(provider="claude:print", scenario="interactive", state=state)
        owner.dispatch.return_value = FRESH
        owner.interactive_request.return_value = ("request-1", "turn-1")
        owner.reply_interaction.return_value = ("message-1", {"state": "queued"})
        owner.wait_for_interaction.side_effect = [
            ({}, 1, [{"kind": "request"}]),
            ({"closed": True, "deliveries": [{"message_id": "message-1", "state": "acknowledged"}]},
             2, [{"kind": "delivery"}]),
        ]
        scenarios_impl.run_interactive(owner)
        self.assert_fragment_after_shutdown(owner)


if __name__ == "__main__":
    unittest.main()
