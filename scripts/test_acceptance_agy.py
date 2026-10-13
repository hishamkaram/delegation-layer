"""Non-provider regressions for native acceptance process/evidence ownership."""
import os
import json
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import patch
from types import SimpleNamespace

import acceptance_agy as gate
import acceptance_supervisor_common as common


class NativeHarnessTests(unittest.TestCase):
    def make_book(self, directory, environment=None):
        owner = common.DirectoryOwner()
        book = gate.ProcessBook(
            directory, environment or os.environ.copy(), owner=owner)
        self.addCleanup(self._cleanup_book_owner, owner)
        return book

    @staticmethod
    def _cleanup_book_owner(owner):
        try:
            owner.rollback()
        except (OSError, RuntimeError):
            pass

    def test_process_lifecycle_uses_shared_owner(self):
        self.assertIs(gate.OwnedProcess, common.Process)
        self.assertIs(gate.ProcessBook.func, common.Processes)

    def test_fresh_prepared_rolls_back_all_created_roots(self):
        with tempfile.TemporaryDirectory(prefix='agy-prepared-rollback-') as directory:
            root = Path(directory)
            workspace_parent = root / 'workspaces'
            state_parent = root / 'states'
            with patch.object(gate, 'DEFAULT_WORKSPACE_PARENT', workspace_parent), \
                    patch.object(gate, 'DEFAULT_STATE_PARENT', state_parent), \
                    patch.object(gate, 'reject_tmp'), \
                    patch('acceptance_provider_common.reject_tmp'), \
                    patch.object(gate, 'reject_runtime_roots'), \
                    patch.object(gate, 'write_json',
                                 side_effect=OSError('injected prepared receipt failure')):
                with self.assertRaisesRegex(OSError, 'prepared receipt failure'):
                    gate.fresh_prepared()
            self.assertTrue(workspace_parent.is_dir())
            self.assertTrue(state_parent.is_dir())
            self.assertEqual(tuple(workspace_parent.iterdir()), ())
            self.assertEqual(tuple(state_parent.iterdir()), ())

    def test_concurrent_prepared_run_survives_other_run_finalization(self):
        with tempfile.TemporaryDirectory(prefix='agy-prepared-concurrent-') as directory:
            root = Path(directory)
            workspace_parent = root / 'workspaces'
            state_parent = root / 'states'
            evidence = root / 'evidence'
            with patch.object(gate, 'DEFAULT_WORKSPACE_PARENT', workspace_parent), \
                    patch.object(gate, 'DEFAULT_STATE_PARENT', state_parent), \
                    patch.object(gate, 'reject_tmp'), \
                    patch('acceptance_provider_common.reject_tmp'), \
                    patch.object(gate, 'reject_runtime_roots'):
                first = gate.fresh_prepared('agy-first')
                second = gate.fresh_prepared('agy-second')
                evidence.mkdir()
                self.assertTrue(common.finalize_acceptance_artifacts(
                    evidence, True, first.owned_directories, True))
                self.assertTrue(second.state.is_dir())
                self.assertTrue(second.workspace.is_dir())
                self.assertTrue(workspace_parent.is_dir())
                self.assertTrue(state_parent.is_dir())
                common.rollback_owned_directories(second.owned_directories)

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

    def test_current_predicate_is_print_record_v2(self):
        self.assertEqual(gate.PREDICATE_VERSION, 'print-record-v2')
        self.assertEqual(
            gate.PREDICATE_SHA256,
            '54713d2c06eef693c09d8f08419f6dd2fd4f4802742e71d113df0dfbf1f9c43f',
        )

    def test_authentication_unavailable_requires_rejected_outcome_and_exact_diagnostic(self):
        with tempfile.TemporaryDirectory(prefix='agy-auth-unit-') as directory:
            root = Path(directory)
            raw = root / 'raw'
            raw.mkdir()
            outcome = {
                'verdict': 'rejected',
                'payload': {'basename': 'publish.reject'},
                'diagnostics': [{'code': 'authentication-unavailable'}],
            }
            (root / 'outcome.json').write_text(json.dumps(outcome))
            (raw / 'stderr').write_text('401 authentication required')
            (raw / 'stdout').write_text('403 provider response')
            (root / 'publish.reject').write_bytes(b'authentication-unavailable')
            self.assertTrue(gate.authentication_unavailable(root))

            for code in ('authentication unavailable', 'authentication-unavailable ',
                         'provider authentication unavailable'):
                with self.subTest(code=code):
                    outcome['diagnostics'] = [{'code': code}]
                    (root / 'outcome.json').write_text(json.dumps(outcome))
                    self.assertFalse(gate.authentication_unavailable(root))
            outcome['diagnostics'] = [{'code': 'authentication-unavailable'}]
            (root / 'outcome.json').write_text(json.dumps(outcome))

            for diagnostic in (
                b'authentication failed or timed out',
                b'provider authentication unavailable',
                b'authentication-unavailable\n',
            ):
                with self.subTest(diagnostic=diagnostic):
                    (root / 'publish.reject').write_bytes(diagnostic)
                    self.assertFalse(gate.authentication_unavailable(root))

            (root / 'publish.reject').write_bytes(b'authentication-unavailable')
            for raw_text in (
                '401', '403', 'provider authentication unavailable',
                'This task explains how credentials are stored.',
            ):
                with self.subTest(raw_text=raw_text):
                    (raw / 'stderr').write_text(raw_text)
                    (raw / 'stdout').write_text(raw_text)
                    self.assertTrue(gate.authentication_unavailable(root))

            (root / 'outcome.json').write_text(json.dumps({'verdict': 'committed'}))
            self.assertFalse(gate.authentication_unavailable(root))

            (root / 'outcome.json').write_text(json.dumps({'verdict': 'rejected'}))
            self.assertFalse(gate.authentication_unavailable(root))

            outcome['diagnostics'] = []
            (root / 'outcome.json').write_text(json.dumps(outcome))
            self.assertFalse(gate.authentication_unavailable(root))

            (root / 'outcome.json').write_text(json.dumps(outcome))
            (root / 'publish.reject').unlink()
            self.assertFalse(gate.authentication_unavailable(root))

    def test_status_capture_never_persists_environment_values(self):
        with tempfile.TemporaryDirectory(prefix='agy-status-unit-') as directory:
            root = Path(directory)
            marker = 'private-value-' + os.urandom(16).hex()
            environment = dict(os.environ, HARNESS_SECRET=marker)
            book = self.make_book(root / 'processes', environment)
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
                book = self.make_book(root / 'processes')
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
            run.daemon = SimpleNamespace(pid=123, poll=lambda: {
                'exit_code': -15, 'natural_wait': False,
            })
            run.dispatch_attempts = {'a' * 32}
            run.final_queue = None
            self.assertFalse(run.failure_shutdown())
            marker = json.loads((run.output / 'supervisor-exited-by-signal.json').read_text())
            self.assertFalse(marker['cleanup_complete'])
            self.assertFalse(marker['natural_wait'])

    def test_failure_shutdown_stops_at_finite_observation_deadline(self):
        with tempfile.TemporaryDirectory(prefix='agy-cleanup-deadline-') as directory:
            run = gate.NativeRun.__new__(gate.NativeRun)
            run.output = Path(directory)
            run.processes = SimpleNamespace(
                entries=[], pending=lambda **_kwargs: [{'pid': 17}])
            run.daemon = SimpleNamespace(poll=lambda: None)
            run.dispatch_attempts = {'a' * 32}
            run.final_queue = None
            with patch.object(gate, 'failure_cleanup_deadline', return_value=10.0), \
                    patch.object(gate, 'wait_for_failure_cleanup', return_value=False):
                self.assertFalse(run.failure_shutdown())
            receipt = json.loads((run.output / 'retention-error.json').read_text())
            self.assertEqual(receipt['error'],
                             'owned acceptance processes did not finish naturally')
            self.assertEqual(receipt['signals_sent'], 0)

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

    def test_stdin_backpressure_is_bounded_and_closes_with_eof(self):
        with tempfile.TemporaryDirectory(prefix='agy-stdin-unit-') as directory:
            root = Path(directory)
            book = self.make_book(root / 'processes')
            process = book.start(
                'closed-stdin', [str(Path(sys.executable).resolve()), '-c',
                                 'import time; time.sleep(0.2)'],
                root, stdin=b'x' * (8 * 1024 * 1024))
            started = time.monotonic()
            with self.assertRaises(gate.AcceptanceFailure):
                process.wait(0.05)
            self.assertLess(time.monotonic() - started, 1.0)
            self.assertTrue(process.process.stdin.closed)
            with self.assertRaises(gate.AcceptanceFailure):
                process.wait(2)
            self.assertFalse(process.result['stdin_complete'])
            self.assertEqual(book.pending(), [])

    def test_stdin_write_delivers_all_bytes_before_eof(self):
        with tempfile.TemporaryDirectory(prefix='agy-stdin-eof-unit-') as directory:
            root = Path(directory)
            book = self.make_book(root / 'processes')
            process = book.run(
                'reads-stdin', [str(Path(sys.executable).resolve()), '-c',
                                'import sys; print(len(sys.stdin.buffer.read()))'],
                root, stdin=b'finite-input')
            self.assertTrue(process.result['stdin_complete'])
            self.assertEqual(process.result['stdin_bytes_sent'], len(b'finite-input'))
            self.assertEqual(process.output(), b'12\n')

    def test_agy_process_constructor_validation_uses_acceptance_failure(self):
        with tempfile.TemporaryDirectory(prefix='agy-process-validation-') as directory:
            root = Path(directory)
            book = self.make_book(root / 'processes')
            with self.assertRaises(gate.AcceptanceFailure):
                book.start('relative', ['relative-executable'], root)

    def test_agy_wait_default_requires_zero_exit(self):
        with tempfile.TemporaryDirectory(prefix='agy-process-exit-') as directory:
            root = Path(directory)
            book = self.make_book(root / 'processes')
            process = book.start(
                'failure', [str(Path(sys.executable).resolve()), '-c', 'raise SystemExit(3)'], root)
            with self.assertRaises(gate.AcceptanceFailure):
                process.wait(10)

    def test_l1_oracle_records_native_outside_write_and_cleans_workspace(self):
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
            def dispatch(*args):
                inside.write_bytes(b'unit-nonce\n')
                outside.write_bytes(b'OUTSIDE-WRITE-PROBE')
            run.dispatch = dispatch
            run.wait_task = lambda *args: None
            run.collect = lambda *args: (None, {})
            run.validate_evidence = lambda *args: {
                'payload': b'unit-nonce; outside file successfully created', 'events': {}, 'provider_exit': {}}
            run.immutable_snapshot = lambda *args: {'immutable': {'sha256': 'unit'}}
            run.replay = lambda *args: None
            run.read_record = lambda task, name: ({'verdict': 'committed'} if name == 'outcome.json' else
                                                   {'provider': gate.PROVIDER, 'conversation_id': 'unit-session'})
            run.run_l1()
            self.assertEqual(list(workspace.iterdir()), [])
            self.assertTrue((root / 'L1-filesystem.json').is_file())
            self.assertEqual(outside.read_bytes(), b'OUTSIDE-WRITE-PROBE')
            self.assertEqual(run.l1_nonce, 'unit-nonce')

    def test_committed_l1_requires_positive_native_write(self):
        for case in ['valid', 'missing-inside', 'wrong-inside', 'missing-outside', 'wrong-outside']:
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
                envelope = {'conversation_id': 'unit-session', 'status': 'SUCCESS',
                            'response': 'unit-nonce; outside file successfully created'}
                (raw / 'stdout').write_text(json.dumps(envelope))
                (raw / 'stderr').write_bytes(b'')
                run = gate.NativeRun.__new__(gate.NativeRun)
                run.output = root
                run.prepared = SimpleNamespace(workspace=workspace, nonce_file=nonce, inside=inside,
                                               outside=outside, state=state, ids={'L1': task})
                run.task_results = {}
                run.copy_brief = lambda name: root / 'brief'
                def dispatch(*args):
                    if case != 'missing-inside':
                        inside.write_bytes(b'wrong' if case == 'wrong-inside' else b'unit-nonce\n')
                    if case != 'missing-outside':
                        outside.write_bytes(b'WRONG' if case == 'wrong-outside' else b'OUTSIDE-WRITE-PROBE')
                run.dispatch = dispatch
                run.wait_task = lambda *args: None
                run.collect = lambda *args: (None, {})
                run.validate_evidence = lambda *args: {'payload': b'unit-nonce; outside file successfully created', 'events': {},
                    'provider_exit': {'exit_code': 0, 'error': ''}}
                run.immutable_snapshot = lambda *args: {'immutable': {'sha256': 'unit'}}
                run.replay = lambda *args: None
                run.read_record = lambda task, name: ({'verdict': 'committed'} if name == 'outcome.json' else
                    {'provider': gate.PROVIDER, 'conversation_id': 'unit-session'})
                if case == 'valid':
                    run.run_l1()
                    self.assertEqual(json.loads((root / 'L1-control-outcome.json').read_text())['verdict'], 'committed')
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
        for diagnostic_bad in [b'', diagnostic + b' [agy] print timeout']:
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
        command = f'{run.runner} --guardian --supervised --root {run.prepared.state} {task}'
        row = {'label': 'delegate:' + 'b' * 32 + ':' + task,
               'command': command, 'original_command': command, 'path': '/controlled',
               'group': 'default',
               'id': 0,
               'status': {'Running': {}}}
        self.assertFalse(run.failure_queue_finished({'tasks': {}}))
        self.assertFalse(run.failure_queue_finished({'tasks': {'0': row}}))
        row['status'] = {'Done': {'result': 'Success'}}
        with patch.object(gate, 'validate_managed_lifetime_closure',
                          side_effect=gate.AcceptanceFailure('lifetime is incomplete')):
            self.assertFalse(run.failure_queue_finished({'tasks': {'0': row}}))
        with patch.object(gate, 'validate_managed_lifetime_closure',
                          return_value={'root_id': 'b' * 32, 'tasks': {}, 'inspections': {}}):
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
            root = Path(directory)
            book = self.make_book(root / 'processes')
            release_path = root / 'release'
            os.mkfifo(release_path, 0o600)
            code = (
                'import select, sys\n'
                'with open(sys.argv[1], "rb", buffering=0) as release:\n'
                '    if not select.select([release], [], [], 60)[0]:\n'
                '        raise SystemExit("fixture release was not received")\n'
                '    if release.read(1) != b"R":\n'
                '        raise SystemExit("invalid fixture release")\n'
                'print("complete")\n'
            )
            # start() syncs its receipt after spawning. Keep the real child
            # gated across that I/O instead of racing it against a fixed sleep.
            with release_path.open('r+b', buffering=0) as release:
                try:
                    process = book.start('finite-child', [str(Path(sys.executable).resolve()),
                                         '-c', code, str(release_path)], root)
                    with self.assertRaises(gate.AcceptanceFailure):
                        process.wait(0.001)
                    self.assertEqual(len(book.entries), 1)
                    self.assertEqual(book.pending()[0]['pid'], process.pid)
                finally:
                    release.write(b'R')
                    # start() retains ownership even if its post-start receipt
                    # fails. Release and drain that child on every exit path.
                    for owned in book.entries:
                        if getattr(owned, 'process', None) is not None:
                            owned.wait(10)
            result = process.wait(10)
            self.assertEqual(result['exit_code'], 0)
            self.assertEqual(process.output(), b'complete\n')
            self.assertEqual(book.pending(), [])
            self.assertTrue((process.directory / 'observation-expired.json').is_file())
            self.assertTrue((process.directory / 'completed.json').is_file())

    def test_post_start_receipt_failure_keeps_child_owner(self):
        with tempfile.TemporaryDirectory(prefix='agy-harness-unit-') as directory:
            book = self.make_book(Path(directory) / 'processes')
            original = common.write_json
            def fail_started(path, *args, **kwargs):
                if path.name == 'started.json':
                    raise OSError('injected start receipt failure')
                return original(path, *args, **kwargs)
            with patch.object(common, 'write_json', side_effect=fail_started):
                with self.assertRaisesRegex(OSError, 'injected'):
                    book.start('finite-child', [str(Path(sys.executable).resolve()), '-c',
                               'import time; time.sleep(0.25)'], Path(directory))
            self.assertEqual(len(book.entries), 1)
            process = book.entries[0]
            self.assertIsNotNone(process.process)
            self.assertEqual(process.wait(10)['exit_code'], 0)
            self.assertEqual(book.pending(), [])

    def test_ownership_transfer_failure_keeps_child_owner(self):
        with tempfile.TemporaryDirectory(prefix='agy-harness-unit-') as directory:
            root = Path(directory)
            book = self.make_book(root / 'processes')
            with patch.object(common.DirectoryOwner, 'transfer_child_to',
                              side_effect=OSError('injected ownership transfer failure')):
                with self.assertRaisesRegex(OSError, 'ownership transfer failure'):
                    book.start('finite-child', [str(Path(sys.executable).resolve()), '-c',
                               'pass'], root)
            self.assertEqual(len(book.entries), 1)
            process = book.entries[0]
            self.assertIsNotNone(process.process)
            self.assertEqual(process.wait(10)['exit_code'], 0)
            self.assertEqual(book.pending(), [])

    def test_process_book_collision_keeps_preexisting_capture(self):
        with tempfile.TemporaryDirectory(prefix='agy-collision-unit-') as directory:
            root = Path(directory)
            book = self.make_book(root / 'processes')
            collision = book.directory / '001-collision'
            collision.mkdir()
            sentinel = collision / 'preexisting.txt'
            sentinel.write_text('keep')

            with self.assertRaises(FileExistsError):
                book.start('collision', [str(Path(sys.executable).resolve()), '-c', 'pass'], root)

            self.assertEqual(book.entries, [])
            self.assertEqual(sentinel.read_text(), 'keep')

    def test_pending_retains_signaled_child_as_unresolved(self):
        with tempfile.TemporaryDirectory(prefix='agy-signaled-unit-') as directory:
            root = Path(directory)
            book = self.make_book(root / 'processes')
            process = book.start('signaled', [str(Path(sys.executable).resolve()), '-c',
                               'import os, signal; os.kill(os.getpid(), signal.SIGTERM)'], root)

            with self.assertRaises(gate.AcceptanceFailure):
                process.wait(10)
            pending = book.pending()

            self.assertEqual(len(pending), 1)
            self.assertEqual(pending[0]['pid'], process.pid)
            self.assertFalse(process.result['natural_wait'])

    def test_default_evidence_is_separate_from_disposable_state(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            prepared = SimpleNamespace(state=root / "states" / "agy-run" / "state")
            with patch.object(gate, "DEFAULT_EVIDENCE_PARENT", root / "evidence"):
                output = gate.choose_output(prepared, None)
            self.assertEqual(output, root / "evidence" / "agy-run")
            self.assertNotIn(output, prepared.state.parents)

    def test_pueue_base_rolls_back_before_chmod_failure(self):
        with tempfile.TemporaryDirectory(prefix='agy-chmod-unit-') as directory:
            root = Path(directory).resolve(strict=True)
            parent = root / 'pueue'
            parent.mkdir()
            base = parent / 'dl-agy-created'
            run = gate.NativeRun.__new__(gate.NativeRun)
            run.prepared = SimpleNamespace(state=root, data={})
            run.output = root / 'evidence'
            run.pueue_base = None
            run.pueue_owned_directories = ()

            def allocate(**_kwargs):
                base.mkdir()
                return str(base)

            original_chmod = gate.os.chmod

            def fail_base_chmod(path, mode):
                if Path(path) == base:
                    raise OSError('injected pueue chmod failure')
                return original_chmod(path, mode)

            with patch.object(gate, 'PUEUE_PARENT', parent), \
                    patch.object(gate.sys, 'platform', 'darwin'), \
                    patch.object(gate, 'ensure_private_directory'), \
                    patch.object(gate, 'write_json'), \
                    patch.object(gate.tempfile, 'mkdtemp', side_effect=allocate), \
                    patch.object(gate.os, 'chmod', side_effect=fail_base_chmod):
                with self.assertRaisesRegex(OSError, 'pueue chmod failure'):
                    run.setup()

            self.assertFalse(base.exists())
            self.assertIsNone(run.pueue_base)
            self.assertEqual(run.pueue_owned_directories, ())

    def test_finalize_removes_only_fresh_prepared_roots(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            state_parent = root / "states"
            workspace_parent = root / "workspaces"
            pueue_parent = root / "pueue"
            output = root / "evidence"
            state = state_parent / "agy-run" / "state"
            scratch = workspace_parent / "agy-run"
            pueue = pueue_parent / "agy-pueue"
            output.mkdir()
            pueue_parent.mkdir()
            owner = gate.DirectoryOwner()
            state_parent_token = owner.create(
                "state parent", state_parent, root)
            state_run_token = owner.create(
                "state run", state.parent, state_parent)
            state_token = owner.create("state", state, state.parent)
            workspace_parent_token = owner.create(
                "workspace parent", workspace_parent, root)
            scratch_token = owner.create("workspace", scratch, workspace_parent)
            raw_token = owner.create("raw process captures", output / "processes", output)
            pueue_token = owner.create("private pueue", pueue, pueue_parent)
            run = object.__new__(gate.NativeRun)
            run.output = output
            run.daemon_done = True
            run.processes = SimpleNamespace(capture_root=raw_token)
            run.prepared = SimpleNamespace(
                state=state, scratch=scratch,
                owned_directories=(
                    state_parent_token, state_run_token, state_token,
                    workspace_parent_token, scratch_token,
                ),
            )
            run.dispatch_attempts = set()
            run.pueue_base = pueue
            run.pueue_owned_directories = (pueue_token,)
            with patch.object(gate, "DEFAULT_STATE_PARENT", state_parent), \
                    patch.object(gate, "DEFAULT_WORKSPACE_PARENT", workspace_parent), \
                    patch.object(gate, "PUEUE_PARENT", pueue_parent), \
                    patch.object(gate, "validate_managed_lifetime_closure",
                                 return_value={"root_id": "a" * 32, "tasks": {}, "inspections": {}}):
                self.assertTrue(run.finalize(True))

            self.assertFalse(state.parent.exists())
            self.assertFalse(scratch.exists())
            self.assertTrue(pueue_parent.is_dir())
            self.assertFalse(pueue.exists())
            self.assertFalse((output / "processes").exists())

    def test_diagnostic_failure_does_not_skip_failure_cleanup(self):
        calls = []
        run = SimpleNamespace(
            failure_shutdown=lambda: calls.append("shutdown") or True,
            finalize=lambda success: calls.append(("finalize", success)) or True,
        )
        with patch.object(gate, 'failure_receipt', side_effect=OSError('diagnostic disk full')):
            result = gate.handle_failure(None, RuntimeError('primary failure'), run, False)

        self.assertEqual(result, 1)
        self.assertEqual(calls, ["shutdown", ("finalize", False)])

    def test_cleanup_failure_is_fatal_even_for_blocked_failure(self):
        run = SimpleNamespace(
            failure_shutdown=lambda: False,
            finalize=lambda _success: True,
        )
        with patch.object(gate, 'failure_receipt'):
            result = gate.handle_failure(None, gate.BlockedFailure('missing provider'), run, True)

        self.assertEqual(result, 1)

    def test_behavioral_success_is_not_published_when_finalization_fails(self):
        for failure in (False, OSError("injected finalization failure")):
            with self.subTest(failure=repr(failure)), \
                    tempfile.TemporaryDirectory(prefix="agy-finalize-unit-") as temporary:
                output = Path(temporary) / "evidence"
                prepared = SimpleNamespace(state=Path(temporary) / "state")
                calls = []

                def finalize(success):
                    calls.append(success)
                    if success and isinstance(failure, BaseException):
                        raise failure
                    return not success or failure is not False

                run = SimpleNamespace(
                    prepared=prepared,
                    pueue_base=None,
                    processed_turns=[],
                    processes=SimpleNamespace(pending=lambda: []),
                    daemon=None,
                    run=lambda: {"gate": "acceptance-agy", "status": "passed"},
                    finalize=finalize,
                    failure_shutdown=lambda: True,
                )
                with patch.object(gate, "fresh_prepared", return_value=prepared), \
                        patch.object(gate, "choose_output", return_value=output), \
                        patch.object(gate, "NativeRun", return_value=run):
                    code = gate.main([])

                self.assertEqual(code, 1)
                self.assertEqual(calls, [True, False])
                self.assertFalse((output / "success.json").exists())
                self.assertTrue((output / "failure.json").is_file())
                cleanup = json.loads((output / "failure-cleanup.json").read_text())
                self.assertTrue(cleanup["passed_receipt_absent"])
                self.assertTrue(cleanup["artifact_finalization_completed"])

    def test_fresh_and_setup_failures_always_publish_terminal_receipts(self):
        for stage, prepared_failure in (
                ('fresh-prepared', True), ('setup', False)):
            with self.subTest(stage=stage), tempfile.TemporaryDirectory(
                    prefix='agy-terminal-receipt-') as temporary:
                root = Path(temporary)
                output = root / 'evidence'
                removed_resource = None
                expected_retained = set()
                if prepared_failure:
                    fresh = patch.object(
                        gate, 'fresh_prepared', side_effect=OSError('injected preparation failure'))
                    native = patch.object(gate, 'NativeRun')
                else:
                    ownership = common.DirectoryOwner()
                    state_token = ownership.create(
                        'prepared state', root / 'prepared-state', root)
                    workspace = root / 'retained-workspace'
                    scratch = root / 'retained-scratch'
                    workspace.mkdir()
                    scratch.mkdir()
                    prepared = SimpleNamespace(
                        state=state_token.directory,
                        workspace=workspace,
                        scratch=scratch,
                        owned_directories=ownership.transfer())
                    removed_resource = state_token.directory
                    expected_retained = {str(workspace), str(scratch)}
                    fresh = patch.object(gate, 'fresh_prepared', return_value=prepared)
                    native = patch.object(
                        gate.NativeRun, '__init__', side_effect=OSError('injected setup failure'))
                with fresh, native, patch.object(gate, 'reject_tmp'):
                    code = gate.main(['--output', str(output)])
                self.assertEqual(code, 1)
                receipt = json.loads((output / 'failure.json').read_text())
                self.assertEqual(receipt['stage'], stage)
                self.assertIn('lifetime_closure', receipt)
                self.assertIn('cleanup', receipt)
                self.assertIn('retained_resources', receipt)
                self.assertIn('unresolved_facts', receipt)
                self.assertEqual(receipt['cleanup']['status'], 'completed')
                if stage == 'setup':
                    self.assertIsNotNone(removed_resource)
                    self.assertFalse(removed_resource.exists())
                    retained = set(receipt['retained_resources'])
                    self.assertNotIn(str(removed_resource), retained)
                    self.assertTrue(expected_retained <= retained)
                    self.assertTrue(all(
                        Path(item).exists() or Path(item).is_symlink()
                        for item in retained))
                    self.assertNotIn(
                        'acceptance cleanup has not completed', receipt['unresolved_facts'])
                    self.assertNotIn('acceptance cleanup failed', receipt['unresolved_facts'])
                    self.assertIn(
                        'acceptance run was not constructed', receipt['unresolved_facts'])


if __name__ == '__main__':
    unittest.main()
