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

    def test_external_symlinked_skill_file_is_rejected(self):
        with tempfile.TemporaryDirectory(prefix="verify-skills-outside-") as outside_dir:
            outside = Path(outside_dir) / "SKILL.md"
            outside.write_text(
                "---\nname: demo\ndescription: External content.\n---\nBody.\n",
                encoding="utf-8",
            )
            directory = self.root / "skills" / "demo"
            directory.mkdir(parents=True)
            (directory / "SKILL.md").symlink_to(outside)
            errors = self.verify_demo()
        self.assertTrue(any("must not be a symlink" in error for error in errors))

    def test_broken_symlinked_required_skill_is_reported(self):
        directory = self.root / "skills" / "demo"
        directory.mkdir(parents=True)
        (directory / "SKILL.md").symlink_to(directory / "missing.md")
        errors = self.verify_demo()
        self.assertTrue(any("must not be a symlink" in error for error in errors))

    def test_symlinked_skill_directory_is_rejected_before_reading(self):
        with tempfile.TemporaryDirectory(prefix="verify-skills-outside-") as outside_dir:
            outside = Path(outside_dir) / "demo"
            outside.mkdir()
            (outside / "SKILL.md").write_text(
                "---\nname: demo\ndescription: External content.\n---\nBody.\n",
                encoding="utf-8",
            )
            (self.root / "skills" / "demo").symlink_to(outside, target_is_directory=True)
            errors = self.verify_demo()
        self.assertTrue(any("symlinked directory" in error for error in errors))

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

    def test_reference_links_and_definitions_are_rejected_explicitly(self):
        self.write_skill(body="See [the contract][contract].\n\n[contract]: ../../guide.md\n")
        errors = self.verify_demo()
        joined = "\n".join(errors)
        self.assertIn("reference-style Markdown links are unsupported", joined)
        self.assertIn("reference-style Markdown link definitions are unsupported", joined)

    def test_balanced_parenthesized_and_angle_destinations_pass(self):
        docs = self.root / "docs"
        docs.mkdir()
        (docs / "foo_(bar).md").write_text("# Guide\n", encoding="utf-8")
        (docs / "foo bar.md").write_text("# Guide\n", encoding="utf-8")
        self.write_skill(
            body=(
                "Read [the balanced guide](../../docs/foo_(bar).md).\n"
                "Read [the spaced guide](<../../docs/foo bar.md>).\n"
            )
        )
        self.assertEqual(self.verify_demo(), [])

    def test_absolute_and_windows_link_paths_are_rejected_before_scheme_skip(self):
        for target in ("/tmp/outside.md", "C:/outside.md", r"C:\\outside.md"):
            with self.subTest(target=target):
                self.write_skill(body=f"Read [outside]({target}).\n")
                errors = self.verify_demo()
                self.assertEqual(len(errors), 1)
                self.assertIn("must be relative", errors[0])

    def test_stale_make_target_is_rejected(self):
        self.write_skill(body="Run `make missing-target`.\n")
        errors = self.verify_demo()
        self.assertEqual(len(errors), 1)
        self.assertIn("make target 'missing-target'", errors[0])
        self.assertIn("Makefile", errors[0])

    def test_unsupported_make_options_extra_targets_and_assignments_are_rejected(self):
        bodies = (
            "Run `make check missing-target`.\n",
            "Run `make -j missing-target`.\n",
            "Run `make missing-target --no-print-directory`.\n",
            "Run `make missing-target VAR=1`.\n",
            "Run `make`.\n",
        )
        for body in bodies:
            with self.subTest(body=body):
                self.write_skill(body=body)
                errors = self.verify_demo()
                self.assertTrue(any("unsupported make command shape" in error for error in errors))

    def test_command_shaped_one_target_without_inline_code_is_checked(self):
        self.write_skill(body="make check\n")
        self.assertEqual(self.verify_demo(), [])

    def test_planned_marker_cannot_bypass_another_command_on_the_line(self):
        self.write_skill(body="Run `make future-target` [planned] and `make missing-target`.\n")
        errors = self.verify_demo()
        joined = "\n".join(errors)
        self.assertIn("planned marker is ambiguous", joined)
        self.assertIn("make target 'future-target'", joined)
        self.assertIn("make target 'missing-target'", joined)

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

    def test_planned_markers_in_command_lines_and_fences(self):
        for body in ("make future [planned]\n", "```sh\nmake future (planned)\n```\n",
                     "Run `make future [planned]`.\n"):
            with self.subTest(body=body):
                self.write_skill(body=body)
                self.assertEqual(self.verify_demo(), [])

    def test_target_names_without_a_make_invocation_are_not_commands(self):
        self.write_skill(body="The `make-lint` and `make.foo` targets are names.\n")
        self.assertEqual(self.verify_demo(), [])

    def test_make_named_targets_are_not_additional_commands(self):
        for target in ("make", "make-lint", "make.foo", "remake"):
            with self.subTest(target=target):
                (self.root / "Makefile").write_text(target + ":\n\t@true\n")
                self.write_skill(body="Run `make " + target + "`.\n")
                self.assertEqual(self.verify_demo(), [])

    def test_protocol_relative_external_url_is_not_a_local_path(self):
        self.write_skill(body="[external](//example.com/docs)\n")
        self.assertEqual(self.verify_demo(), [])

    def test_empty_angle_destination_and_titles_are_rejected(self):
        for destination in ('<>', '<> "title"', '<../../guide.md> "title"',
                            '../../guide.md "title"'):
            with self.subTest(destination=destination):
                (self.root / "guide.md").write_text("Guide\n")
                self.write_skill(body="[guide](" + destination + ")\n")
                self.assertTrue(self.verify_demo())

    def test_angle_destination_parentheses_are_literal(self):
        (self.root / "guide)name.md").write_text("Guide\n")
        self.write_skill(body="[guide](<../../guide)name.md>)\n")
        self.assertEqual(self.verify_demo(), [])

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
