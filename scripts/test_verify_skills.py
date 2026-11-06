"""Behavioral tests for the repository skill verifier."""

from contextlib import redirect_stderr
import copy
from io import StringIO
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import skill_eval_cases
import verify_skills as checker


class SkillFixture(unittest.TestCase):
    """Temporary repository root with a Makefile, plus skill-writing helpers."""

    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory(prefix="verify-skills-")
        self.root = Path(self.tempdir.name).resolve()
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

    def write_authority_targets(self, name="go-reviewer"):
        required = checker.REQUIRED_AUTHORITY_LINKS[name]
        for relative in required:
            target = self.root / relative.split("#", 1)[0]
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text("# Contract\n", encoding="utf-8")
        return required


class VerifySkillsTests(SkillFixture):
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

    def test_authority_links_are_required_for_go_reviewer(self):
        self.write_authority_targets()
        self.write_skill(name="go-reviewer", body="A review skill.\n")
        errors = checker.verify(self.root, required_skills=("go-reviewer",))
        self.assertTrue(any("required authoritative link" in error for error in errors))

    def test_authority_links_pass_when_all_required_destinations_are_linked(self):
        required = self.write_authority_targets()
        links = "\n".join(
            f"Read [contract]({Path('..') / '..' / relative})."
            for relative in required
        )
        self.write_skill(name="go-reviewer", body=links + "\n")
        self.assertEqual(checker.verify(self.root, required_skills=("go-reviewer",)), [])

    def test_required_authorities_are_the_relevant_exact_sections(self):
        self.assertEqual(checker.REQUIRED_AUTHORITY_LINKS, {
            "add-an-adapter": (
                "agents/_data/adapter-contract.md#2-launch-planning-and-preflight-policy",
                "agents/_data/adapter-contract.md#4-evidence-capture-and-publication-predicates",
            ),
            "go-reviewer": (
                "agents/_data/maintainability-contract.md#review-dimensions",
                "agents/_data/code-quality-floor.md#3-testing-and-verification-discipline",
            ),
            "live-e2e": (
                "agents/_data/maintainability-contract.md#provider-checks",
                "agents/_data/delegation-invariants.md#3-supervision-deadlines-and-forbidden-signals",
            ),
            "supervisor-architect": (
                "agents/_data/maintainability-contract.md#recovery-and-verification",
                "agents/_data/delegation-invariants.md#4-supervisor-admission-ordering",
            ),
        })

    def test_authority_whole_file_links_do_not_satisfy_required_sections(self):
        required = self.write_authority_targets()
        links = "\n".join(
            f"[contract](../../{relative.split('#', 1)[0]})" for relative in required
        )
        self.write_skill(name="go-reviewer", body=links)
        errors = checker.verify(self.root, required_skills=("go-reviewer",))
        self.assertTrue(any("required authoritative link" in error for error in errors))

    def test_authority_wrong_sections_do_not_satisfy_required_sections(self):
        required = self.write_authority_targets()
        links = "\n".join(
            f"[contract](../../{relative.split('#', 1)[0]}#wrong-section)"
            for relative in required
        )
        self.write_skill(name="go-reviewer", body=links)
        errors = checker.verify(self.root, required_skills=("go-reviewer",))
        self.assertTrue(any("required authoritative link" in error for error in errors))

    def test_authority_sections_accept_canonical_paths_and_angle_links(self):
        required = self.write_authority_targets()
        links = "\n".join(
            f"[contract](<../../{relative.replace('_data/', '_data/../_data/')}>)"
            for relative in required
        )
        self.write_skill(name="go-reviewer", body=links)
        self.assertEqual(checker.verify(self.root, required_skills=("go-reviewer",)), [])

    def test_two_authority_sections_in_one_file_are_required_separately(self):
        name = "add-an-adapter"
        required = self.write_authority_targets(name)
        for index, missing in enumerate(required):
            with self.subTest(missing=missing):
                links = "\n".join(
                    f"[contract](../../{relative})"
                    for other_index, relative in enumerate(required) if other_index != index
                )
                self.write_skill(name=name, body=links)
                errors = checker.verify(self.root, required_skills=(name,))
                self.assertEqual(errors, [
                    f"{self.root / 'skills' / name / 'SKILL.md'}: "
                    f"required authoritative link to {missing!r} is missing",
                ])

    def test_fixture_resolves_owned_temp_alias_for_exact_authority_diagnostics(self):
        with tempfile.TemporaryDirectory(prefix="verify-skills-alias-") as directory:
            parent = Path(directory).resolve()
            target = parent / "target"
            target.mkdir()
            alias = parent / "alias"
            alias.symlink_to(target, target_is_directory=True)
            temporary = tempfile.TemporaryDirectory(prefix="repository-", dir=alias)
            with temporary:
                fixture = SkillFixture()
                with patch.object(tempfile, "TemporaryDirectory", return_value=temporary):
                    fixture.setUp()
                raw_root = Path(temporary.name)
                canonical_root = raw_root.resolve()
                self.assertNotEqual(raw_root, canonical_root)
                self.assertEqual(fixture.root, canonical_root)
                name = "add-an-adapter"
                required = fixture.write_authority_targets(name)
                fixture.write_skill(name=name, body=f"[contract](../../{required[0]})")
                expected = [
                    f"{canonical_root / 'skills' / name / 'SKILL.md'}: "
                    f"required authoritative link to {required[1]!r} is missing",
                ]
                self.assertEqual(checker.verify(raw_root, required_skills=(name,)), expected)

    def test_authority_sections_cannot_be_satisfied_by_external_paths(self):
        required = self.write_authority_targets()
        with tempfile.TemporaryDirectory(prefix="verify-skills-outside-") as outside:
            relative_path, _, fragment = required[0].partition("#")
            target = Path(outside) / relative_path
            target.parent.mkdir(parents=True)
            target.write_text("# Contract\n", encoding="utf-8")
            for destination in (
                f"https://example.invalid/{required[0]}",
                f"//example.invalid/{required[0]}",
                f"../../../{Path(outside).name}/{relative_path}#{fragment}",
            ):
                with self.subTest(destination=destination):
                    links = [f"[external]({destination})"]
                    links.extend(f"[contract](../../{relative})" for relative in required[1:])
                    self.write_skill(name="go-reviewer", body="\n".join(links))
                    errors = checker.verify(self.root, required_skills=("go-reviewer",))
                    self.assertTrue(any(
                        "required authoritative link" in error and repr(required[0]) in error
                        for error in errors
                    ))

    def test_default_required_skills_include_supervisor_architect(self):
        for name in checker.REQUIRED_SKILLS:
            if name != "supervisor-architect":
                self.write_skill(name)

        errors = checker.verify(self.root)

        self.assertTrue(any(
            "skills/supervisor-architect/SKILL.md" in error and
            "required skill is missing" in error
            for error in errors
        ))

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


EVAL_SKILLS = ("docs-updater", "go-implementer", "go-reviewer", "go-test-writer", "live-e2e")


def json_locations(node, prefix=()):
    """Yield the path of every object member and list element below node."""
    members = node.items() if isinstance(node, dict) else enumerate(node) if isinstance(node, list) else ()
    for key, child in members:
        yield (*prefix, key)
        yield from json_locations(child, (*prefix, key))


def set_at(document, location, value):
    target = document
    for key in location[:-1]:
        target = target[key]
    target[location[-1]] = value


def differing_leaves(left, right, prefix=""):
    """Return the JSON pointers at which two parsed documents differ."""
    if isinstance(left, dict) and isinstance(right, dict) and left.keys() == right.keys():
        return {pointer for key in left for pointer in differing_leaves(left[key], right[key], f"{prefix}/{key}")}
    if isinstance(left, list) and isinstance(right, list) and len(left) == len(right):
        return {pointer for index, pair in enumerate(zip(left, right)) for pointer in differing_leaves(*pair, f"{prefix}/{index}")}
    return set() if left == right else {prefix or "/"}


def eval_scenario(scenario_id, outcome):
    return {
        "id": scenario_id,
        "visible": {"prompt": "Do the task.", "inputs": [{"path": "env/input.txt", "content": "data\n"}]},
        "hidden": {
            "expected": {
                "outcome": outcome,
                "required_actions": [{"type": "read-file", "target": "env/input.txt"}],
                "forbidden_actions": [{"type": "edit-file", "target": "env/input.txt"}],
                "required_artifacts": [
                    {"path": "report/result.json", "check": "json-field", "locator": "/status", "value": outcome},
                ],
            },
            "rationale": "Why this outcome is the right one.",
        },
    }


def eval_document(skill="demo"):
    case = eval_scenario("first-case", "implemented")
    case["control"] = eval_scenario("first-control", "blocked")
    return {"schema_version": 1, "skill": skill, "cases": [case]}


class SkillEvalCaseTests(SkillFixture):
    """Structural eval-case checks run through the shipped verifier."""

    def write_cases(self, document, name="demo"):
        directory = self.root / "skills" / name / "evals"
        directory.mkdir(parents=True, exist_ok=True)
        text = document if isinstance(document, str) else json.dumps(document)
        (directory / "cases.json").write_text(text, encoding="utf-8")

    def errors_for(self, mutate):
        document = eval_document()
        mutate(document)
        self.write_skill()
        self.write_cases(document)
        return "\n".join(self.verify_demo())

    def test_valid_cases_and_skills_without_evals_pass(self):
        self.write_skill()
        self.assertEqual(self.verify_demo(), [])
        self.write_cases(eval_document())
        self.assertEqual(self.verify_demo(), [])

    def test_current_repository_ships_valid_eval_cases_for_owning_skills(self):
        repository = Path(__file__).resolve().parent.parent
        for skill in EVAL_SKILLS:
            with self.subTest(skill):
                document = json.loads((repository / "skills" / skill / "evals" / "cases.json").read_text(encoding="utf-8"))
                self.assertEqual(document["skill"], skill)
                self.assertTrue(all(case["control"] for case in document["cases"]))

    def test_shape_and_vocabulary_violations_are_rejected(self):
        def case(document):
            return document["cases"][0]

        mutations = {
            "wrong skill": (lambda d: d.update(skill="other"), "skill must be 'demo'"),
            "wrong version": (lambda d: d.update(schema_version=True), "schema_version must be 1"),
            "no cases": (lambda d: d.update(cases=[]), "non-empty list"),
            "extra key": (lambda d: case(d).update(note="x"), "unsupported field 'note'"),
            "no control": (lambda d: case(d).pop("control"), "missing field 'control'"),
            "bad outcome": (lambda d: case(d)["hidden"]["expected"].update(outcome="great"), "outcome: must be one of"),
            "bad action": (lambda d: case(d)["hidden"]["expected"]["required_actions"][0].update(type="think"), "type: must be one of"),
            "bad check": (lambda d: case(d)["hidden"]["expected"]["required_artifacts"][0].update(check="grep"), "check: must be one of"),
            "bad pointer": (lambda d: case(d)["hidden"]["expected"]["required_artifacts"][0].update(locator="status"), "JSON pointer"),
            "unsafe path": (lambda d: case(d)["visible"]["inputs"][0].update(path="../escape.txt"), "safe relative path"),
            "duplicate fixture": (lambda d: case(d)["visible"]["inputs"].append(dict(case(d)["visible"]["inputs"][0])), "duplicate fixture path"),
            "blank prompt": (lambda d: case(d)["visible"].update(prompt=" "), "non-blank text"),
            "bad id": (lambda d: case(d).update(id="Not A Slug"), "lowercase hyphenated"),
        }
        for label, (mutate, expected) in mutations.items():
            with self.subTest(label):
                self.assertIn(expected, self.errors_for(mutate))

    def test_cases_must_grade_something_and_the_control_must_discriminate(self):
        def empty(document):
            expected = document["cases"][0]["hidden"]["expected"]
            expected["required_actions"], expected["required_artifacts"] = [], []

        def overlap(document):
            expected = document["cases"][0]["hidden"]["expected"]
            expected["forbidden_actions"] = copy.deepcopy(expected["required_actions"])

        def same_control(document):
            case = document["cases"][0]
            case["control"]["hidden"]["expected"] = copy.deepcopy(case["hidden"]["expected"])

        self.assertIn("at least one required action or artifact", self.errors_for(empty))
        self.assertIn("both required and forbidden", self.errors_for(overlap))
        self.assertIn("does not discriminate", self.errors_for(same_control))

    def test_identifiers_must_be_unique_and_must_not_leak_into_visible_text(self):
        def duplicate_control(document):
            document["cases"][0]["control"]["id"] = "first-case"

        def leak(document):
            document["cases"][0]["visible"]["prompt"] = "Handle the first-control scenario."

        def repeat(document):
            document["cases"].append(copy.deepcopy(document["cases"][0]))

        self.assertIn("must differ from the case id", self.errors_for(duplicate_control))
        self.assertIn("must not contain scenario id 'first-control'", self.errors_for(leak))
        self.assertIn("duplicate id", self.errors_for(repeat))

    def test_unreadable_or_conflicting_case_files_are_reported(self):
        self.write_skill()
        for label, text, expected in (
            ("duplicate keys", '{"skill": "demo", "skill": "demo"}', "duplicate key"),
            ("not json", "{", "cannot read cases"),
            ("not an object", "[]", "must be an object"),
        ):
            with self.subTest(label):
                self.write_cases(text)
                self.assertIn(expected, "\n".join(self.verify_demo()))
        cases = self.root / "skills" / "demo" / "evals" / "cases.json"
        cases.unlink()
        cases.symlink_to(self.root / "Makefile")
        self.assertIn("regular file", "\n".join(self.verify_demo()))

    def test_untyped_json_values_get_diagnostics_instead_of_exceptions(self):
        junk = ([], {}, True, None, 1, 1.5)
        must_reject = {
            "skill": ("skill",),
            "case id": ("cases", 0, "id"),
            "fixture path": ("cases", 0, "visible", "inputs", 0, "path"),
            "outcome": ("cases", 0, "hidden", "expected", "outcome"),
            "action type": ("cases", 0, "hidden", "expected", "required_actions", 0, "type"),
            "action target": ("cases", 0, "hidden", "expected", "required_actions", 0, "target"),
            "artifact check": ("cases", 0, "hidden", "expected", "required_artifacts", 0, "check"),
            "artifact path": ("cases", 0, "hidden", "expected", "required_artifacts", 0, "path"),
            "artifact locator": ("cases", 0, "hidden", "expected", "required_artifacts", 0, "locator"),
        }
        for label, location in must_reject.items():
            for value in junk:
                with self.subTest(field=label, value=value):
                    document = eval_document()
                    set_at(document, location, value)
                    errors = skill_eval_cases.validate_cases(document, "demo")
                    self.assertTrue(errors and all(isinstance(error, str) for error in errors))

    def test_every_shipped_field_survives_every_json_type(self):
        repository = Path(__file__).resolve().parent.parent
        original = json.loads((repository / "skills" / "go-reviewer" / "evals" / "cases.json").read_text(encoding="utf-8"))
        self.assertEqual(skill_eval_cases.validate_cases(original, "go-reviewer"), [])
        for location in json_locations(original):
            for value in ([], {}, True, None, 0, 1.5, "x"):
                with self.subTest(location=location, value=value):
                    document = copy.deepcopy(original)
                    set_at(document, location, value)
                    errors = skill_eval_cases.validate_cases(document, "go-reviewer")
                    self.assertTrue(all(isinstance(error, str) for error in errors))

    def test_fixture_path_that_is_not_text_does_not_enter_the_duplicate_set(self):
        document = eval_document()
        inputs = document["cases"][0]["visible"]["inputs"]
        inputs.extend([{"path": [], "content": "a"}, {"path": [], "content": "b"}])
        errors = skill_eval_cases.validate_cases(document, "demo")
        self.assertEqual(sum("safe relative path" in error or "non-blank text" in error for error in errors), 2)
        self.assertFalse(any("duplicate fixture path" in error for error in errors))

    def test_judgment_controls_differ_from_their_case_in_exactly_one_fact(self):
        repository = Path(__file__).resolve().parent.parent
        expected = (
            ("go-reviewer", 0, "checks/results.json", {"/checks/1/exit_code"}),
            ("live-e2e", 0, "evidence/acceptance-receipt.json", {"/executed_binary/path"}),
            ("live-e2e", 2, "evidence/proposal.json", {"/subject"}),
            ("live-e2e", 3, "evidence/proposal.json", {"/subject"}),
        )
        for skill, index, fixture, pointers in expected:
            with self.subTest(skill):
                document = json.loads((repository / "skills" / skill / "evals" / "cases.json").read_text(encoding="utf-8"))
                case = document["cases"][index]
                control = case["control"]
                self.assertEqual(case["visible"]["prompt"], control["visible"]["prompt"])
                fixtures = {item["path"]: item["content"] for item in case["visible"]["inputs"]}
                control_fixtures = {item["path"]: item["content"] for item in control["visible"]["inputs"]}
                self.assertEqual(fixtures.keys(), control_fixtures.keys())
                self.assertEqual([path for path in fixtures if fixtures[path] != control_fixtures[path]], [fixture])
                self.assertEqual(differing_leaves(json.loads(fixtures[fixture]), json.loads(control_fixtures[fixture])), pointers)

    def test_reviewer_pair_judges_only_synthetic_check_results(self):
        repository = Path(__file__).resolve().parent.parent
        document = json.loads((repository / "skills/go-reviewer/evals/cases.json").read_text(encoding="utf-8"))
        case = document["cases"][0]
        self.assertEqual(case["visible"]["prompt"], case["control"]["visible"]["prompt"])
        for scenario, exit_code, verdict in ((case, 1, "hold"), (case["control"], 0, "approve")):
            with self.subTest(verdict=verdict):
                inputs = scenario["visible"]["inputs"]
                self.assertEqual([item["path"] for item in inputs], ["checks/results.json"])
                self.assertEqual(json.loads(inputs[0]["content"]), {"checks": [
                    {"kind": "fixture", "exit_code": 0},
                    {"kind": "native", "exit_code": exit_code},
                ]})
                expected = scenario["hidden"]["expected"]
                self.assertEqual(expected["outcome"], verdict)
                self.assertEqual(expected["required_actions"], [
                    {"type": "read-file", "target": "checks/results.json"},
                ])
                self.assertEqual(expected["forbidden_actions"], [
                    {"type": "edit-file", "target": "checks/results.json"},
                ])
                self.assertEqual(expected["required_artifacts"], [{
                    "path": "review/record.json", "check": "json-field",
                    "locator": "/verdict", "value": verdict,
                }])

    def test_paired_assignments_are_identical_and_do_not_leak_the_hidden_answer(self):
        repository = Path(__file__).resolve().parent.parent
        for skill in ("docs-updater", "go-test-writer"):
            with self.subTest(skill):
                document = json.loads((repository / "skills" / skill / "evals" / "cases.json").read_text(encoding="utf-8"))
                for case in document["cases"]:
                    control = case["control"]
                    prompt = case["visible"]["prompt"]
                    self.assertEqual(prompt, control["visible"]["prompt"])
                    for scenario in (case, control):
                        expected = scenario["hidden"]["expected"]
                        self.assertNotIn(expected["outcome"], prompt)
                        values = [artifact["value"] for artifact in expected["required_artifacts"]]
                        if skill == "docs-updater":
                            self.assertFalse([value for value in values if value in prompt])
                        else:
                            # Output label choices are offered to both sides, so naming one never selects it.
                            both = {artifact["value"] for side in (case, control) for artifact in side["hidden"]["expected"]["required_artifacts"]}
                            self.assertTrue(all(value in prompt for value in both))

    def test_first_run_pair_differs_only_in_the_recorded_run(self):
        repository = Path(__file__).resolve().parent.parent
        document = json.loads((repository / "skills" / "go-test-writer" / "evals" / "cases.json").read_text(encoding="utf-8"))
        case = document["cases"][0]
        fixtures = {item["path"]: item["content"] for item in case["visible"]["inputs"]}
        control = {item["path"]: item["content"] for item in case["control"]["visible"]["inputs"]}
        self.assertEqual(fixtures.keys(), control.keys())
        self.assertEqual([path for path in fixtures if fixtures[path] != control[path]], ["env/first-run.txt"])
        for scenario in (case, case["control"]):
            forbidden = {(action["type"], action["target"]) for action in scenario["hidden"]["expected"]["forbidden_actions"]}
            self.assertEqual(forbidden, {("edit-file", "internal/task/limit.go"), ("edit-file", "internal/task/limit_test.go")})

    def test_scoped_pairs_share_constraints_offer_every_label_and_vary_one_input(self):
        repository = Path(__file__).resolve().parent.parent

        def pair(skill, index):
            document = json.loads((repository / "skills" / skill / "evals" / "cases.json").read_text(encoding="utf-8"))
            case = document["cases"][index]
            return case, case["control"]

        def fixtures(scenario):
            return {item["path"]: item["content"] for item in scenario["visible"]["inputs"]}

        def differing(case, control):
            left, right = fixtures(case), fixtures(control)
            self.assertEqual(left.keys(), right.keys())
            return sorted(path for path in left if left[path] != right[path])

        def labels(case, control, locator):
            return {
                artifact["value"]
                for scenario in (case, control)
                for artifact in scenario["hidden"]["expected"]["required_artifacts"]
                if artifact["locator"] == locator
            }

        # Tool facts and the permission constraint are shared; the assignment scope is the tested variable.
        case, control = pair("go-implementer", 0)
        self.assertEqual(fixtures(case)["env/tools.json"], fixtures(control)["env/tools.json"])
        sentences = [set(scenario["visible"]["prompt"].split(". ")) for scenario in (case, control)]
        self.assertGreaterEqual(len([text for text in sentences[0] & sentences[1] if len(text) > 40]), 2)
        self.assertTrue(labels(case, control, "/status") <= {"implemented", "blocked"})
        self.assertTrue(all(label in case["visible"]["prompt"] and label in control["visible"]["prompt"] for label in ("implemented", "blocked")))

        # Proposal and implementation arms get the same valid project; only the assignment differs.
        case, control = pair("go-implementer", 1)
        self.assertEqual(fixtures(case), fixtures(control))
        self.assertTrue({".go-version", "go.mod", "internal/task/limit_test.go"} <= fixtures(case).keys())
        for label in labels(case, control, "/status"):
            self.assertIn(label, case["visible"]["prompt"])
            self.assertIn(label, control["visible"]["prompt"])

        # Synthetic check pair: identical assignment, one counterfactual fact.
        case, control = pair("go-reviewer", 0)
        self.assertEqual(case["visible"]["prompt"], control["visible"]["prompt"])
        self.assertEqual(differing(case, control), ["checks/results.json"])
        for label in labels(case, control, "/verdict"):
            self.assertIn(label, case["visible"]["prompt"])

        # Joined-error pair: identical assignment, rule, and project metadata; only code and tests differ.
        case, control = pair("go-reviewer", 1)
        self.assertEqual(case["visible"]["prompt"], control["visible"]["prompt"])
        self.assertEqual(differing(case, control), ["internal/example/classify.go", "internal/example/classify_test.go"])
        for label in labels(case, control, "/verdict"):
            self.assertIn(label, case["visible"]["prompt"])
        # The graded cause is a manual judgment of the requested reason, never an ordinal artifact locator.
        for scenario in (case, control):
            self.assertIn("file, severity, and reason", scenario["visible"]["prompt"])
            self.assertEqual(
                [artifact["locator"] for artifact in scenario["hidden"]["expected"]["required_artifacts"]
                 if artifact["locator"].startswith("/findings/")], [])


if __name__ == "__main__":
    unittest.main()
