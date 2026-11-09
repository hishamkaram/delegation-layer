"""Installed callback roles are metadata observations, independent of file mode."""

from __future__ import annotations

import os
from pathlib import Path
import unittest
from unittest import mock

from scripts.acceptance.evidence.digests import sha
from scripts.acceptance.evidence.source import MAX_SOURCE_FILE
from scripts.acceptance.evidence.source import VerificationError
from scripts.release_tests.release_fixture import FixtureTree
from scripts.release_tests.candidate_install.fixtures import deny_installed_script_reads
from scripts.release_verification.published_files import observe_installed_skill
from scripts.release_verification.published_files import require_installed_skill


CALLBACK = "scripts/delegate-routing-context.sh"
SOURCE = {"SKILL.md": b"# fixture skill\n", "references/routing-rule.json": b"{}\n",
          CALLBACK: b"#!/bin/sh\nexit 0\n"}


class InstalledSkillTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tree = FixtureTree.open(prefix="installed-skill", label="installed skill metadata fixture")
        self.addCleanup(self.tree.dispose)
        self.skill = self.tree.root / "skill"
        for relative, data in SOURCE.items():
            path = self.skill / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
            path.chmod(0o644)
        # Expected digests are source-input evidence, computed before installation observation.
        self.expected = {relative: sha(data) for relative, data in SOURCE.items()}

    def require(self):
        observation = observe_installed_skill(self.skill)
        require_installed_skill(self.expected, observation, "installed_skill_mismatch", "verify-skill")
        return observation

    def test_mode_0644_callback_is_never_read_and_full_inventory_is_preserved(self):
        with deny_installed_script_reads(self.skill):
            observed = self.require()
        self.assertEqual(observed.files, tuple(sorted(SOURCE)))
        self.assertEqual(dict(observed.data_digests), {path: digest for path, digest in self.expected.items()
                                                     if path != CALLBACK})
        (metadata,) = observed.executables
        self.assertEqual((metadata.path, metadata.size, metadata.mode), (CALLBACK, len(SOURCE[CALLBACK]), 0o644))

    def test_compatible_callback_update_remains_a_metadata_observation(self):
        (self.skill / CALLBACK).write_text("#!/bin/sh\n# compatible source update\nexit 0\n", encoding="utf-8")
        self.assertEqual(self.require().files, tuple(sorted(SOURCE)))

    def test_missing_extra_data_drift_and_callback_symlink_are_rejected(self):
        cases = ("missing-data", "missing-callback", "extra-data", "changed-data", "callback-symlink")
        for case in cases:
            with self.subTest(case=case):
                changed = self.skill / (CALLBACK if "callback" in case else "SKILL.md")
                original = SOURCE[CALLBACK if "callback" in case else "SKILL.md"]
                extra = self.skill / "extra.json"
                if case.startswith("missing"):
                    changed.unlink()
                elif case == "extra-data":
                    extra.write_bytes(b"{}")
                elif case == "changed-data":
                    changed.write_bytes(b"drift")
                else:
                    changed.unlink()
                    changed.symlink_to(self.skill / "SKILL.md")
                with self.assertRaises(VerificationError) as caught:
                    self.require()
                self.assertEqual(caught.exception.code, "installed_skill_invalid" if case == "callback-symlink"
                                 else "installed_skill_mismatch")
                if changed.is_symlink():
                    changed.unlink()
                changed.write_bytes(original)
                if extra.exists():
                    extra.unlink()

    def test_extra_script_role_is_not_read_and_still_fails_exact_inventory(self):
        unknown = self.skill / "scripts" / "unknown-helper.sh"
        unknown.write_bytes(b"#!/bin/sh\nexit 0\n")
        with deny_installed_script_reads(self.skill), self.assertRaises(VerificationError) as caught:
            self.require()
        self.assertEqual(caught.exception.code, "installed_skill_mismatch")

    def test_callback_size_bound_is_checked_from_metadata_without_reading_bytes(self):
        callback = self.skill / CALLBACK
        with callback.open("wb") as output:
            output.truncate(MAX_SOURCE_FILE + 1)
        with deny_installed_script_reads(self.skill), self.assertRaises(VerificationError) as caught:
            observe_installed_skill(self.skill)
        self.assertEqual(caught.exception.code, "installed_skill_invalid")

    def test_unclassified_executable_mode_is_refused_before_byte_read(self):
        root = self.tree.root / "unknown-role"
        root.mkdir()
        executable = root / "unknown-tool"
        executable.write_bytes(b"#!/bin/sh\nexit 0\n")
        executable.chmod(0o755)
        with mock.patch.object(Path, "open", side_effect=AssertionError("installed executable bytes read")), \
                mock.patch.object(os, "open", side_effect=AssertionError("installed executable bytes read")):
            with self.assertRaises(VerificationError) as caught:
                observe_installed_skill(root)
        self.assertEqual(caught.exception.code, "installed_skill_executable_unknown")


if __name__ == "__main__":
    unittest.main()
