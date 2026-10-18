"""Cross-tag publication and proof-retention constraints for release workflows."""

from pathlib import Path
import re
import unittest


WORKFLOWS = Path(__file__).resolve().parent.parent / ".github/workflows"


class ReleaseWorkflowContractTests(unittest.TestCase):
    def test_every_tag_uses_one_workflow_wide_publication_lock(self):
        source = (WORKFLOWS / "release.yml").read_text(encoding="utf-8")
        concurrency = source.split("\nconcurrency:\n", 1)[1].split("\njobs:\n", 1)[0]
        group = re.search(r"^  group: ([^\n]+)$", concurrency, re.MULTILINE)
        self.assertIsNotNone(group)
        if group is None:
            raise AssertionError("workflow-wide publication lock missing")
        self.assertEqual(group.group(1), "release-promotion")
        self.assertNotIn("${{", group.group(1))
        self.assertIn("  queue: max", concurrency)
        self.assertIn("  cancel-in-progress: false", concurrency)
        self.assertNotRegex(source.split("\njobs:\n", 1)[1], r"(?m)^ +concurrency:")

    def test_native_reports_have_a_documented_public_promotion_window(self):
        source = (WORKFLOWS / "release-native-acceptance.yml").read_text(encoding="utf-8")
        self.assertIn("retention-days: 90", source)
        documentation = (WORKFLOWS.parent.parent / "docs/contributing.md").read_text(encoding="utf-8")
        self.assertIn("Attempt reports request 90-day retention", documentation)
        self.assertIn("all tags", documentation)


if __name__ == "__main__":
    unittest.main()
