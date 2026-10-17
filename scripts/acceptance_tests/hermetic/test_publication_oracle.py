"""Collection must retain outcome authority as well as its publication label."""

from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.hermetic import scenarios_publication as scenarios


class PublicationOracleTests(unittest.TestCase):
    def run_rejected_case(self, outcome):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "reject.txt").write_text("empty_answer\n")
            case = SimpleNamespace(
                provider_settings={"scenario": "empty"},
                run_command=lambda *_args, **_kwargs: (4, {"publication": "rejected", "outcome": outcome}),
                task_dir=lambda: root,
            )
            response = {"publication": "rejected", "payload": {"basename": "reject.txt"}}
            with (
                patch.object(scenarios, "complete_success_case", return_value=response),
                patch.object(scenarios, "require_provider_evidence"),
                patch.object(scenarios, "assert_counts"),
            ):
                scenarios.run_p03(case)

    def test_rejected_collection_requires_its_rejected_outcome(self):
        for outcome in (None, {}, {"verdict": "committed"}):
            with self.subTest(outcome=outcome):
                with self.assertRaisesRegex(RuntimeError, "rejected outcome"):
                    self.run_rejected_case(outcome)

    def test_rejected_collection_with_matching_authority_passes(self):
        self.run_rejected_case({"verdict": "rejected"})
