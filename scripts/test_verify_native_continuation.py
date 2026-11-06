"""The continuation CLI preserves behavioral validator inputs without a candidate ledger."""

import contextlib
import io
from pathlib import Path
import unittest
from unittest.mock import patch
from typing import TypedDict

from scripts import verify_native_continuation as entry
from scripts.delegate_acceptance.native_evidence.contract import EvidenceError


class VerificationOptions(TypedDict):
    supplemental_root: Path
    canonical_aggregate: Path
    source_revision: str
    candidate_delegate: Path
    pi_control: Path
    environment: dict[str, str]
    checkout: Path


class ContinuationEntrypointTests(unittest.TestCase):
    def test_behavioral_inputs_reach_the_existing_validator_unchanged(self):
        options: VerificationOptions = dict(supplemental_root=Path("supplemental"), canonical_aggregate=Path("aggregate.json"),
                       source_revision="a" * 40, candidate_delegate=Path("delegate"), pi_control=Path("pi"),
                       environment={}, checkout=Path("checkout"))
        summary = {"kind": "native-acceptance-supplemental-summary"}
        with patch.object(entry, "validate_supplemental", return_value=summary) as validator:
            self.assertIs(entry.verify_native_continuation(**options), summary)
        validator.assert_called_once_with(**options)

    def test_cli_reports_a_validator_refusal_and_preserves_the_cause(self):
        arguments = ["--supplemental-root", "supplemental", "--canonical-aggregate", "aggregate.json",
                     "--source-revision", "a" * 40, "--candidate-delegate", "delegate", "--pi-control", "pi"]
        with patch.object(entry, "validate_supplemental", side_effect=EvidenceError("continuation_mismatch")), \
                contextlib.redirect_stderr(io.StringIO()) as error:
            self.assertEqual(entry.main(arguments), 1)
        self.assertIn("continuation_mismatch", error.getvalue())


if __name__ == "__main__":
    unittest.main()
