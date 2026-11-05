"""Native process tracing distinguishes Go threads from process launches."""

import unittest

from scripts.native_init_census import packages, process_events
from scripts.repair_editor_counterfactuals import CLOSE_LINE, ENTRY, overlay_source, require_counterfactual_result
from pathlib import Path


class NativeInitCensusTests(unittest.TestCase):
    def test_process_trace_keeps_exec_and_fork_but_excludes_go_threads(self):
        source = ('10 execve("/managed/delegate-run", ["--supervisor-repair-editor"], 0x0) = 0\n'
                  '10 clone3({flags=CLONE_VM|CLONE_THREAD}, 88) = -1 ENOSYS\n'
                  '10 clone(child_stack=NULL, flags=CLONE_VM|CLONE_THREAD <unfinished ...>\n'
                  '10 <... clone resumed>) = 11\n'
                  '10 vfork() = 12\n'
                  '12 execve("/child", [], 0x0) = 0\n')
        events = process_events(source)
        self.assertEqual(len(events), 3)
        self.assertIn('vfork()', events[1])

    def test_process_trace_rejects_incomplete_or_unpaired_calls(self):
        for source in ('10 fork(<unfinished ...>\n', '10 <... fork resumed>) = 12\n'):
            with self.subTest(source=source), self.assertRaises(ValueError):
                process_events(source)

    def test_go_package_decoder_rejects_nonobjects(self):
        self.assertEqual([item['ImportPath'] for item in packages(b'{"ImportPath":"a"}\n{"ImportPath":"b"}')], ['a', 'b'])
        with self.assertRaises(ValueError):
            packages(b'[]')

    def test_survivor_overlays_change_only_the_retained_copy(self):
        original = 'package control\nimport "os"\n' + CLOSE_LINE + '\n' + ENTRY
        keep = overlay_source(original, helper=Path('/owned/helper'), retain=True)
        close = overlay_source(original, helper=Path('/owned/helper'), retain=False)
        self.assertNotIn(CLOSE_LINE, keep)
        self.assertIn(CLOSE_LINE, close)
        self.assertEqual(keep.split('// Counterfactual only:', 1)[1], close.split('// Counterfactual only:', 1)[1])

    def test_survivor_overlay_refuses_source_drift(self):
        with self.assertRaises(RuntimeError):
            overlay_source('package changed', helper=Path('/owned/helper'), retain=True)

    def test_expected_red_never_hides_cleanup_failure(self):
        oracle = b'fresh actual RootAdmission EX=busy, want acquired'
        require_counterfactual_result('keep', 1, oracle)
        for fault in (b'retained native admission diagnostics', b'native admission directory finalization'):
            with self.subTest(fault=fault), self.assertRaises(RuntimeError):
                require_counterfactual_result('keep', 1, oracle + b'\n' + fault)


if __name__ == '__main__':
    unittest.main()
