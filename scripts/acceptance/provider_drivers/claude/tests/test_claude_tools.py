"""Reject fabricated positive controls and malformed terminal stream fields."""
import json
from pathlib import Path
import unittest
import sys


repository_root = str(Path(__file__).resolve().parents[5])
if repository_root not in sys.path:
    sys.path.insert(0, repository_root)

from scripts.acceptance.provider_drivers.claude import protocol as gate
from scripts.acceptance.evidence.validation import AcceptanceFailure
from scripts.acceptance.provider_drivers.tests.claude_fixtures import NONCE
from scripts.acceptance.provider_drivers.tests.claude_fixtures import answer_event
from scripts.acceptance.provider_drivers.tests.claude_fixtures import fresh_stream
from scripts.acceptance.provider_drivers.tests.claude_fixtures import init_event
from scripts.acceptance.provider_drivers.tests.claude_fixtures import stream


class ToolEvidenceTests(unittest.TestCase):
    def test_terminal_fields(self):
        for field in ("errors", "permission_denials"):
            result = answer_event()
            result[field] = None
            with self.subTest(field=field), self.assertRaises(AcceptanceFailure):
                gate.parse_claude_events(stream([init_event(), result]))
            result[field] = []
            gate.parse_claude_events(stream([init_event(), result]))
        for subtype in ("timeout", "timed_out", "timed-out", "refusal"):
            with self.subTest(subtype=subtype), self.assertRaises(AcceptanceFailure):
                gate.parse_claude_events(stream([init_event(), answer_event(),
                    {"type": "system", "subtype": subtype}]))

    def test_tool_argument_and_provenance(self):
        workspace = Path('/workspace')
        nonce_file = workspace / 'nonce.txt'
        baseline = [json.loads(line) for line in fresh_stream(nonce_file=nonce_file, workspace=workspace).splitlines()]
        for mutation in ('nonce_aux', 'result_first', 'assistant_result', 'user_use', 'duplicate_use'):
            values = json.loads(json.dumps(baseline))
            if mutation.endswith('_aux'):
                name = "Bash"
                use = next(v for v in values if v.get('type') == 'assistant' and
                           v['message']['content'][0]['name'] == name)
                inputs = use['message']['content'][0]['input']
                key = 'command'
                inputs['auxiliary'] = '/elsewhere'
                inputs[key] = '/elsewhere'
            elif mutation == 'result_first':
                values[1], values[2] = values[2], values[1]
            elif mutation == 'assistant_result':
                values[2]['type'] = 'assistant'
                values[2]['message']['role'] = 'assistant'
            elif mutation == 'user_use':
                values[1]['type'] = 'user'
                values[1]['message']['role'] = 'user'
            else:
                values.insert(2, values[1])
            with self.subTest(mutation=mutation), self.assertRaises(AcceptanceFailure):
                parsed = gate.parse_claude_events(stream(values))
                gate.validate_fresh_controls(parsed, nonce_file, workspace, Path('/sibling'), NONCE)
