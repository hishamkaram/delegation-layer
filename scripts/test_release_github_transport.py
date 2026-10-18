"""GitHub transport uses the canonical bounded owner without logging secrets."""

import unittest
from unittest.mock import patch

from scripts.delegate_acceptance.processes.contracts import BoundedCommandResult
from scripts.delegate_acceptance.release_verification.contract import VerificationError
from scripts.delegate_acceptance.release_verification.github import GitHubClient, positive_id


class GitHubTransportTests(unittest.TestCase):
    def test_json_request_is_bounded_and_records_only_safe_transport_facts(self):
        receipt = {"commands": []}
        env = {"GH_TOKEN": "private-canary"}
        client = GitHubClient(receipt, env)
        with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                   return_value=BoundedCommandResult(0, b'{"workflow_run_id":17}')) as run:
            value = client.json("repos/owner/repo/actions/workflows/proof.yml/dispatches",
                                method="POST", fields={"ref": "main", "inputs[tag]": "v1.2.3"})
        self.assertEqual(value, {"workflow_run_id": 17})
        self.assertEqual(run.call_args.args[1], env)
        self.assertEqual(run.call_args.kwargs["timeout"], 120)
        self.assertNotIn("private-canary", repr(run.call_args.args[0]))
        self.assertNotIn("private-canary", repr(receipt))
        self.assertEqual(receipt["commands"][0]["exit_code"], 0)

    def test_scalar_or_malformed_response_is_rejected(self):
        for data in (b'null', b'42', b'{bad'):
            with self.subTest(data=data):
                client = GitHubClient({"commands": []}, {})
                with patch("scripts.delegate_acceptance.release_verification.github.run_bounded_command",
                           return_value=BoundedCommandResult(0, data)):
                    with self.assertRaisesRegex(VerificationError, "github_response_invalid"):
                        client.json("repos/owner/repo")

    def test_id_does_not_accept_boolean_or_string(self):
        for value in (True, "17", 0, -1):
            with self.subTest(value=value):
                with self.assertRaisesRegex(VerificationError, "github_id_invalid"):
                    positive_id(value, "test")


if __name__ == "__main__":
    unittest.main()
