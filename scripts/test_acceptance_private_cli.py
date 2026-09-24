"""Packaging regressions for the default private-supervisor CLI path."""

from pathlib import Path
import unittest


REPOSITORY = Path(__file__).resolve().parent.parent


class HomebrewBundleTests(unittest.TestCase):
    def test_formula_installs_bundled_supervisor_pair(self):
        script = (REPOSITORY / "scripts" / "publish-homebrew-formula.sh").read_text()
        self.assertIn('cat >"${formula}" <<EOF', script)
        template = script.split('cat >"${formula}" <<EOF', 1)[1].split("\nEOF", 1)[0]
        self.assertNotIn('depends_on "pueue"', template)
        for binary in ("delegate", "delegate-run"):
            self.assertIn(f'bin.install "{binary}"', template)
        self.assertIn('libexec.install "pueue", "pueued"', template)
        self.assertNotIn('bin.install "pueue"', template)
        self.assertNotIn('bin.install "pueued"', template)
