"""Non-provider regressions for native acceptance process/evidence ownership."""
import os
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace

import acceptance_agy as gate


class NativeHarnessTests(unittest.TestCase):
    def test_linux_supervisor_parent_is_private_before_child_creation(self):
        with tempfile.TemporaryDirectory(prefix='agy-parent-unit-') as directory:
            root = Path(directory).resolve()
            parent = root / 'parent'
            parent.mkdir(mode=0o777)
            os.chmod(parent, 0o777)
            run = gate.NativeRun.__new__(gate.NativeRun)
            run.prepared = SimpleNamespace(state=root, data={})
            run.output = root / 'evidence'
            with patch.object(gate, 'PUEUE_PARENT', parent), \
                    patch.object(gate.sys, 'platform', 'linux'), \
                    patch.object(gate, 'write_json'), \
                    patch('acceptance_provider_common.reject_tmp'), \
                    patch.object(gate.tempfile, 'mkdtemp', side_effect=RuntimeError('child boundary')):
                with self.assertRaisesRegex(RuntimeError, 'child boundary'):
                    run.setup()
            self.assertEqual(parent.stat().st_mode & 0o777, 0o700)
            alias = root / 'alias'
            alias.symlink_to(parent, target_is_directory=True)
            with patch.object(gate, 'PUEUE_PARENT', alias), \
                    patch.object(gate.sys, 'platform', 'linux'), \
                    patch.object(gate, 'write_json'), \
                    patch('acceptance_provider_common.reject_tmp'), \
                    patch.object(gate.tempfile, 'mkdtemp') as allocate:
                with self.assertRaisesRegex(gate.AcceptanceFailure, 'not a private directory'):
                    run.setup()
                allocate.assert_not_called()

    def test_authentication_unavailable_requires_rejected_outcome_and_marker(self):
        with tempfile.TemporaryDirectory(prefix='agy-auth-unit-') as directory:
            root = Path(directory)
            raw = root / 'raw'
            raw.mkdir()
            (root / 'outcome.json').write_text(json.dumps({'verdict': 'rejected'}))
            (raw / 'stderr').write_text('provider returned authentication required')
            self.assertTrue(gate.authentication_unavailable(root))

            (root / 'outcome.json').write_text(json.dumps({'verdict': 'committed'}))
            self.assertFalse(gate.authentication_unavailable(root))

            (root / 'outcome.json').write_text(json.dumps({'verdict': 'rejected'}))
            (raw / 'stderr').write_text('provider rejected the request')
            self.assertFalse(gate.authentication_unavailable(root))

    def test_status_capture_never_persists_environment_values(self):
        with tempfile.TemporaryDirectory(prefix='agy-status-unit-') as directory:
            root = Path(directory)
            marker = 'private-value-' + os.urandom(16).hex()
            environment = dict(os.environ, HARNESS_SECRET=marker)
            book = gate.ProcessBook(root / 'processes', environment)
            code = ('import json, os; print(json.dumps({"tasks": {"0": '
                    '{"status": {"Done": {}}, "envs": {"SECRET": os.environ["HARNESS_SECRET"] * 10000}}}}))')
            process = book.start('status', [str(Path(sys.executable).resolve()), '-c', code,
                                          'status', '--json'], root)
            process.wait(10)
            self.assertEqual(process.json(), {'tasks': {'0': {'status': {'Done': {}}}}})
            self.assertEqual(process.result['stdout_redaction'], 'pueue-status-envs')
            self.assertEqual(book.pending(), [])
            for artifact in book.directory.rglob('*'):
                if artifact.is_file():
                    self.assertNotIn(marker.encode(), artifact.read_bytes())

    def test_status_capture_rejects_invalid_or_excessive_output(self):
        cases = ['print("x" * 4096)', 'print(\'{"tasks":{},"tasks":{}}\')', 'print(\'{"tasks":{},"envs":{"key":NaN}}\')', 'print("{")']
        cases.append('import json; print(json.dumps({"tasks": {"0": {"command": "😀" * 100}}}, ensure_ascii=False))')
        for code in cases:
            with self.subTest(code=code), tempfile.TemporaryDirectory(prefix='agy-status-unit-') as directory:
                root = Path(directory)
                book = gate.ProcessBook(root / 'processes', os.environ.copy())
                process = book.start('status', [str(Path(sys.executable).resolve()), '-c', code,
                                              'status', '--json'], root)
                with patch.object(gate, 'MAX_STATUS_BYTES', 1024):
                    with self.assertRaises(gate.AcceptanceFailure):
                        process.wait(10)
                self.assertEqual(process.output(), b'')
                self.assertTrue(process.result['capture_error'])
                self.assertEqual(book.pending(), [])
                with self.assertRaises(gate.AcceptanceFailure):
                    process.json()

    def test_daemon_exit_does_not_prove_queue_cleanup(self):
        with tempfile.TemporaryDirectory(prefix='agy-exited-supervisor-') as directory:
            run = gate.NativeRun.__new__(gate.NativeRun)
            run.output = Path(directory)
            run.processes = SimpleNamespace(entries=[], pending=lambda **kwargs: [])
            run.daemon = SimpleNamespace(pid=123, poll=lambda: {'exit_code': 0})
            run.dispatch_attempts = {'a' * 32}
            run.final_queue = None
            with patch.object(gate.time, 'sleep', side_effect=RuntimeError('owner retained')):
                with self.assertRaisesRegex(RuntimeError, 'owner retained'):
                    run.failure_shutdown()
            marker = json.loads((run.output / 'supervisor-exited-queue-unresolved.json').read_text())
            self.assertFalse(marker['cleanup_complete'])
            self.assertEqual(marker['queue_termination'], 'unknown')

    def test_runtime_roots_skip_only_disabled_go_cache(self):
        environment = {'HOME': '/controlled/home'}
        self.assertEqual(gate.provider_runtime_roots(environment),
                         gate.provider_runtime_roots(dict(environment, GOCACHE='off')))
        self.assertIn(Path('/controlled/cache'),
                      gate.provider_runtime_roots(dict(environment, GOCACHE='/controlled/cache')))

    def test_runtime_roots_include_native_discovery_selectors(self):
        environment = {
            'HOME': '/controlled/home',
            'XDG_CONFIG_HOME': '/controlled/config',
            'XDG_CONFIG_DIRS': os.pathsep.join(('/controlled/config-a', '/controlled/config-b')),
            'GEMINI_HOME': '/controlled/gemini',
            'AGY_HOME': '/controlled/agy',
        }
        roots = gate.provider_runtime_roots(environment)
        for root in ('/controlled/config', '/controlled/config-a', '/controlled/config-b',
                     '/controlled/gemini', '/controlled/agy'):
            self.assertIn(Path(root), roots)

    def test_prepared_evidence_excludes_unvalidated_receipt_data(self):
        prepared = gate.Prepared.__new__(gate.Prepared)
        prepared.data = {'envs': {'SECRET': 'must-not-persist'}, 'status': 'must-not-persist'}
        for field in ['workspace', 'scratch', 'state', 'nonce_file', 'inside', 'outside']:
            setattr(prepared, field, Path('/controlled') / field)
        prepared.briefs = {'L1': Path('/controlled/brief')}
        prepared.ids = {'L1': 'a' * 32}
        result = prepared.evidence_fields()
        self.assertNotIn('must-not-persist', json.dumps(result))
        self.assertEqual(result['workspace'], '/controlled/workspace')

    def test_failed_stdin_write_closes_parent_pipe_and_retains_owner(self):
        with tempfile.TemporaryDirectory(prefix='agy-stdin-unit-') as directory:
            root = Path(directory)
            book = gate.ProcessBook(root / 'processes', os.environ.copy())
            with self.assertRaises(BrokenPipeError):
                book.start('closed-stdin', [str(Path(sys.executable).resolve()), '-c',
                           'import os; os.close(0)'], root, stdin=b'x' * (1024 * 1024))
            process = book.entries[0]
            self.assertTrue(process.process.stdin.closed)
            self.assertEqual(process.wait(10)['exit_code'], 0)
            self.assertEqual(book.pending(), [])

    def test_l1_oracle_cleanup_removes_both_nonce_copies(self):
        # Unit input for the harness only; this does not claim live readiness.
        with tempfile.TemporaryDirectory(prefix='agy-oracle-unit-') as directory:
            root = Path(directory)
            workspace = root / 'workspace'
            workspace.mkdir()
            nonce = workspace / 'nonce.txt'
            nonce.write_bytes(b'unit-nonce\n')
            inside = workspace / 'inside.txt'
            outside = root / 'outside.txt'
            run = gate.NativeRun.__new__(gate.NativeRun)
            run.output = root
            run.prepared = SimpleNamespace(workspace=workspace, nonce_file=nonce,
                                           inside=inside, outside=outside, state=root / 'state',
                                           ids={'L1': 'a' * 32})
            run.task_results = {}
            run.copy_brief = lambda name: root / 'brief'
            run.dispatch = lambda *args: inside.write_bytes(b'unit-nonce\n')
            run.wait_task = lambda *args: None
            run.collect = lambda *args: (None, {})
            run.validate_evidence = lambda *args: {
                'payload': b'unit-nonce; outside file denied', 'events': {}, 'provider_exit': {}}
            run.immutable_snapshot = lambda *args: {'immutable': {'sha256': 'unit'}}
            run.replay = lambda *args: None
            run.read_record = lambda task, name: ({'verdict': 'committed'} if name == 'outcome.json' else
                                                   {'provider': gate.PROVIDER, 'conversation_id': 'unit-session'})
            run.run_l1()
            self.assertEqual(list(workspace.iterdir()), [])
            self.assertTrue((root / 'L1-filesystem.json').is_file())
            self.assertEqual(run.l1_nonce, 'unit-nonce')

    def test_rejected_l1_requires_positive_filesystem_control(self):
        for case in ['valid', 'missing-inside', 'wrong-inside', 'outside-written']:
            with self.subTest(case=case), tempfile.TemporaryDirectory(prefix='agy-denial-unit-') as directory:
                root = Path(directory)
                workspace = root / 'workspace'
                workspace.mkdir()
                nonce = workspace / 'nonce.txt'
                nonce.write_bytes(b'unit-nonce\n')
                inside, outside = workspace / 'inside.txt', root / 'outside.txt'
                state, task = root / 'state', 'a' * 32
                raw = state / 'tasks' / task / 'raw'
                raw.mkdir(parents=True)
                envelope = {'conversation_id': 'unit-session', 'status': 'SUCCESS', 'response': '',
                            'denied_actions': [{'action': 'write_file', 'display_name': 'WriteToFile'}]}
                (raw / 'stdout').write_text(json.dumps(envelope))
                (raw / 'stderr').write_bytes(b'a tool required the "write_file" permission; auto-denied')
                outcome = {'verdict': 'rejected', 'payload': {'basename': 'publish.reject'}}
                run = gate.NativeRun.__new__(gate.NativeRun)
                run.output = root
                run.prepared = SimpleNamespace(workspace=workspace, nonce_file=nonce, inside=inside,
                                               outside=outside, state=state, ids={'L1': task})
                run.task_results = {}
                run.copy_brief = lambda name: root / 'brief'
                def dispatch(*args):
                    if case != 'missing-inside':
                        inside.write_bytes(b'wrong' if case == 'wrong-inside' else b'unit-nonce\n')
                    if case == 'outside-written':
                        outside.write_bytes(b'OUTSIDE')
                run.dispatch = dispatch
                run.wait_task = lambda *args: None
                run.collect = lambda *args: (None, {})
                run.validate_evidence = lambda *args: {'payload': b'invalid-output', 'events': {},
                    'provider_exit': {'exit_code': 0, 'error': ''}, 'outcome': outcome}
                run.immutable_snapshot = lambda *args: {'immutable': {'sha256': 'unit'}}
                run.replay = lambda *args: None
                run.read_record = lambda task, name: (outcome if name == 'outcome.json' else
                    {'provider': gate.PROVIDER, 'conversation_id': 'unit-session'})
                if case == 'valid':
                    run.run_l1()
                    self.assertEqual(json.loads((root / 'L1-control-outcome.json').read_text())['verdict'], 'rejected')
                    self.assertFalse(nonce.exists())
                    self.assertFalse(inside.exists())
                else:
                    with self.assertRaises(gate.AcceptanceFailure):
                        run.run_l1()
                    self.assertEqual(run.task_results, {})

    def test_write_denial_control_is_strict_and_not_a_provider_success(self):
        value = {'conversation_id': 'unit-session', 'status': 'SUCCESS', 'response': 'partial',
                 'denied_actions': [{'action': 'write_file', 'display_name': 'WriteToFile'}]}
        diagnostic = b'a tool required the "write_file" permission; auto-denied'
        gate.validate_l1_write_denial(json.dumps(value).encode(), diagnostic, 'unit-session')
        changes = [dict(status='ERROR'), dict(conversation_id='wrong'), dict(response=None),
                   dict(error='failed'), dict(denied_actions=[]),
                   dict(denied_actions=[{'action': 'read_file', 'display_name': 'ViewFile'}]),
                   dict(denied_actions=value['denied_actions'] * 2)]
        for change in changes:
            with self.subTest(change=change), self.assertRaises(gate.AcceptanceFailure):
                gate.validate_l1_write_denial(json.dumps(dict(value, **change)).encode(), diagnostic, 'unit-session')
        for diagnostic_bad in [b'', diagnostic + b' [agy] print timeout', diagnostic + b' authentication required']:
            with self.subTest(diagnostic=diagnostic_bad), self.assertRaises(gate.AcceptanceFailure):
                gate.validate_l1_write_denial(json.dumps(value).encode(), diagnostic_bad, 'unit-session')
        for invalid in [b'[]', b'{', b'{"status":"SUCCESS","status":"SUCCESS"}', b'{"x":NaN}']:
            with self.subTest(invalid=invalid), self.assertRaises(gate.AcceptanceFailure):
                gate.validate_l1_write_denial(invalid, diagnostic, 'unit-session')

    def test_canonical_temporary_aliases_are_rejected(self):
        paths = ['/tmp/evidence', '/var/tmp/evidence']
        if Path('/tmp').resolve() == Path('/private/tmp'):
            paths.append('/private/tmp/evidence')
        for path in paths:
            with self.subTest(path=path), self.assertRaises(gate.AcceptanceFailure):
                gate.reject_tmp(Path(path), 'evidence')

    def test_lost_dispatch_response_is_not_empty_queue(self):
        run = gate.NativeRun.__new__(gate.NativeRun)
        task = 'a' * 32
        run.dispatch_attempts = {task}
        run.root_id = 'b' * 32
        run.runner = Path('/controlled/delegate-run')
        run.prepared = type('PreparedPaths', (), {'state': Path('/controlled/state')})()
        command = f'{run.runner} --root {run.prepared.state} {task}'
        row = {'label': 'delegate:' + 'b' * 32 + ':' + task,
               'command': command, 'original_command': command, 'path': '/controlled',
               'group': 'default',
               'status': {'Running': {}}}
        self.assertFalse(run.failure_queue_finished({'tasks': {}}))
        self.assertFalse(run.failure_queue_finished({'tasks': {'0': row}}))
        row['status'] = {'Done': {'result': 'Success'}}
        self.assertTrue(run.failure_queue_finished({'tasks': {'0': row}}))
        run.root_id = 'c' * 32
        self.assertFalse(run.failure_queue_finished({'tasks': {'0': row}}))
        run.root_id = 'b' * 32
        self.assertFalse(run.failure_queue_finished({'tasks': {'0': row, '1': row}}))
        run.dispatch_attempts = set()
        self.assertFalse(run.failure_queue_finished({'tasks': {'0': row}}))
        self.assertTrue(run.failure_queue_finished({'tasks': {}}))

    def test_expired_observation_keeps_one_actual_process(self):
        with tempfile.TemporaryDirectory(prefix='agy-harness-unit-') as directory:
            book = gate.ProcessBook(Path(directory) / 'processes', os.environ.copy())
            process = book.start('finite-child', [str(Path(sys.executable).resolve()), '-c',
                                 'import time; time.sleep(0.25); print("complete")'], Path(directory))
            with self.assertRaises(gate.AcceptanceFailure):
                process.wait(0.001)
            self.assertEqual(len(book.entries), 1)
            self.assertEqual(book.pending()[0]['pid'], process.pid)
            result = process.wait(10)
            self.assertEqual(result['exit_code'], 0)
            self.assertEqual(process.output(), b'complete\n')
            self.assertEqual(book.pending(), [])
            self.assertTrue((process.directory / 'observation-expired.json').is_file())
            self.assertTrue((process.directory / 'completed.json').is_file())

    def test_post_start_receipt_failure_keeps_child_owner(self):
        with tempfile.TemporaryDirectory(prefix='agy-harness-unit-') as directory:
            book = gate.ProcessBook(Path(directory) / 'processes', os.environ.copy())
            original = gate.write_json
            def fail_started(path, *args, **kwargs):
                if path.name == 'started.json':
                    raise OSError('injected start receipt failure')
                return original(path, *args, **kwargs)
            with patch.object(gate, 'write_json', side_effect=fail_started):
                with self.assertRaisesRegex(OSError, 'injected'):
                    book.start('finite-child', [str(Path(sys.executable).resolve()), '-c',
                               'import time; time.sleep(0.25)'], Path(directory))
            self.assertEqual(len(book.entries), 1)
            process = book.entries[0]
            self.assertIsNotNone(process.process)
            self.assertEqual(process.wait(10)['exit_code'], 0)
            self.assertEqual(book.pending(), [])


if __name__ == '__main__':
    unittest.main()
