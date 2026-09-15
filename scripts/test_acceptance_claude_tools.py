"""Reject fabricated positive controls and malformed terminal stream fields."""
import json
from pathlib import Path
import unittest

import acceptance_claude as gate
from acceptance_provider_common import AcceptanceFailure
from test_acceptance_claude import NONCE, answer_event, fresh_stream, init_event, stream


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
        baseline = [json.loads(line) for line in fresh_stream(nonce_file, workspace).splitlines()]
        for mutation in ('read_aux', 'glob_aux', 'grep_aux', 'result_first', 'assistant_result', 'user_use', 'duplicate_use'):
            values = json.loads(json.dumps(baseline))
            if mutation.endswith('_aux'):
                name = mutation.split('_')[0].capitalize()
                use = next(v for v in values if v.get('type') == 'assistant' and
                           v['message']['content'][0]['name'] == name)
                inputs = use['message']['content'][0]['input']
                key = 'file_path' if name == 'Read' else 'path'
                inputs['auxiliary'] = inputs[key]
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
