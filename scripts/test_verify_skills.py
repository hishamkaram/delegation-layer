"""Behavioral tests for the repository skill verifier."""

from contextlib import redirect_stderr
from io import StringIO
from pathlib import Path
import tempfile
import unittest

import verify_skills as checker


class VerifySkillsTests(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory(prefix="verify-skills-")
        self.root = Path(self.tempdir.name)
        (self.root / "skills").mkdir()
        (self.root / "Makefile").write_text("check:\n\t@true\n", encoding="utf-8")

    def tearDown(self):
        self.tempdir.cleanup()

    def write_skill(self, name="demo", body="A small skill.\n", metadata=None):
        directory = self.root / "skills" / name
        directory.mkdir(parents=True, exist_ok=True)
        fields = metadata or {
            "name": name,
            "description": "A useful skill description.",
        }
        frontmatter = "---\n" + "".join(f"{key}: {value}\n" for key, value in fields.items()) + "---\n"
        (directory / "SKILL.md").write_text(frontmatter + body, encoding="utf-8")

    def verify_demo(self):
        return checker.verify(self.root, required_skills=("demo",))

    def test_current_repository_has_valid_skills(self):
        errors = checker.verify(Path(__file__).resolve().parent.parent)
        self.assertEqual(errors, [])

    def test_malformed_metadata_reports_duplicate_and_blank_fields(self):
        directory = self.root / "skills" / "demo"
        directory.mkdir(parents=True)
        (directory / "SKILL.md").write_text(
            "---\nname: demo\nname: demo\ndescription: []\n---\nBody.\n",
            encoding="utf-8",
        )
        errors = self.verify_demo()
        joined = "\n".join(errors)
        self.assertIn("SKILL.md", joined)
        self.assertIn("duplicate metadata field 'name'", joined)
        self.assertIn("must begin with an ASCII letter", joined)

    def test_missing_required_skill_is_an_error(self):
        errors = checker.verify(self.root, required_skills=("missing",))
        self.assertEqual(len(errors), 1)
        self.assertIn("required skill is missing", errors[0])

    def test_missing_metadata_field_is_an_error(self):
        self.write_skill(metadata={"name": "demo"})
        errors = self.verify_demo()
        self.assertTrue(any("missing 'description'" in error for error in errors))

    def test_blank_metadata_field_is_an_error(self):
        self.write_skill(metadata={"name": "demo", "description": ""})
        errors = self.verify_demo()
        self.assertTrue(any("description' must be non-blank" in error for error in errors))

    def test_required_skill_cannot_be_relocated_to_nested_directory(self):
        nested = self.root / "skills" / "extra" / "demo"
        nested.mkdir(parents=True)
        (nested / "SKILL.md").write_text(
            "---\nname: demo\ndescription: A nested skill.\n---\nBody.\n",
            encoding="utf-8",
        )
        errors = self.verify_demo()
        self.assertTrue(any("skills/demo/SKILL.md" in error for error in errors))

    def test_duplicate_name_and_directory_mismatch_are_rejected(self):
        self.write_skill("demo")
        nested = self.root / "skills" / "extra" / "demo"
        nested.mkdir(parents=True)
        (nested / "SKILL.md").write_text(
            "---\nname: demo\ndescription: Another skill.\n---\nBody.\n",
            encoding="utf-8",
        )
        self.write_skill("other", metadata={"name": "demo", "description": "Another skill."})
        errors = checker.verify(self.root, required_skills=("demo",))
        joined = "\n".join(errors)
        self.assertIn("duplicate skill name 'demo'", joined)
        self.assertIn("must match its skill directory 'other'", joined)

    def test_broken_relative_link_reports_file_and_line(self):
        self.write_skill(body="See [the missing contract](../../agents/nope.md).\n")
        errors = self.verify_demo()
        self.assertEqual(len(errors), 1)
        self.assertIn("SKILL.md:5", errors[0])
        self.assertIn("does not exist", errors[0])

    def test_empty_relative_link_reports_diagnostic(self):
        self.write_skill(body="See [the missing contract]().\n")
        errors = self.verify_demo()
        self.assertEqual(len(errors), 1)
        self.assertIn("empty destination", errors[0])

    def test_stale_make_target_is_rejected(self):
        self.write_skill(body="Run `make missing-target`.\n")
        errors = self.verify_demo()
        self.assertEqual(len(errors), 1)
        self.assertIn("make target 'missing-target'", errors[0])
        self.assertIn("Makefile", errors[0])

    def test_phony_declaration_without_rule_is_not_a_target(self):
        self.root.joinpath("Makefile").write_text(
            ".PHONY: declared-only\ncheck:\n\t@true\n", encoding="utf-8"
        )
        self.write_skill(body="Run `make declared-only`.\n")
        errors = self.verify_demo()
        self.assertEqual(len(errors), 1)
        self.assertIn("declared-only", errors[0])

    def test_planned_make_target_is_allowed_with_explicit_marker(self):
        self.write_skill(body="Run `make future-target` [planned] after implementation.\n")
        self.assertEqual(self.verify_demo(), [])

    def test_valid_links_commands_and_ordinary_prose_pass(self):
        (self.root / "guide.md").write_text("# Guide\n", encoding="utf-8")
        self.write_skill(
            body=(
                "Please make a plan before editing; that is ordinary prose.\n"
                "Read [the guide](../../guide.md#start).\n"
                "Run `make check`.\n"
                "External [reference](https://example.com/docs).\n"
                "Keep a todo list for follow-up notes.\n"
            )
        )
        self.assertEqual(self.verify_demo(), [])

    def test_quoted_description_allows_yaml_punctuation(self):
        self.write_skill(metadata={"name": "demo", "description": '"A description: with punctuation."'})
        self.assertEqual(self.verify_demo(), [])

    def test_unquoted_yaml_special_scalars_are_rejected(self):
        for value in ("[]", "yes", "2026-09-14", "0x10"):
            with self.subTest(value=value):
                self.write_skill(metadata={"name": "demo", "description": value})
                errors = self.verify_demo()
                self.assertTrue(any("metadata field 'description'" in error for error in errors))

    def test_explicit_scaffold_marker_fails_without_flagging_lowercase_words(self):
        self.write_skill(body="Keep a todo list, but remove TODO: finish this section.\n")
        errors = self.verify_demo()
        self.assertEqual(len(errors), 1)
        self.assertIn("unfinished scaffold marker 'TODO'", errors[0])

    def test_cli_returns_nonzero_and_actionable_diagnostic(self):
        self.write_skill(body="Run `make stale`.\n")
        output = StringIO()
        with redirect_stderr(output):
            status = checker.main([str(self.root)])
        self.assertEqual(status, 1)
        self.assertIn("verify-skills:", output.getvalue())
        self.assertIn("stale", output.getvalue())


if __name__ == "__main__":
    unittest.main()
