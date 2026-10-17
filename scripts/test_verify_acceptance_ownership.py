"""Focused tests for the acceptance ownership AST gate."""

from pathlib import Path
import io
import tempfile
import unittest
from unittest.mock import patch

import verify_acceptance_ownership as verifier


class AcceptanceOwnershipVerifierTests(unittest.TestCase):
    def violations(self, source: str):
        with tempfile.TemporaryDirectory(prefix="acceptance-ast-") as directory:
            path = Path(directory) / "acceptance_fixture.py"
            path.write_text(source, encoding="utf-8")
            return verifier.find_violations((path,))

    def test_recursive_removal_is_rejected(self):
        violations = self.violations("import shutil\nshutil.rmtree(root)\n")
        self.assertEqual([item.kind for item in violations], ["recursive-removal"])

    def test_imported_removal_alias_is_rejected(self):
        violations = self.violations(
            "from shutil import rmtree as remove_tree\nremove_tree(root)\n")
        self.assertEqual([item.kind for item in violations], ["recursive-removal"])

    def test_qualified_module_alias_is_rejected(self):
        violations = self.violations(
            "import shutil as filesystem\nfilesystem.rmtree(root)\n")
        self.assertEqual([item.kind for item in violations], ["recursive-removal"])

    def test_local_removal_assignment_alias_is_rejected(self):
        violations = self.violations(
            "import shutil\n"
            "remove_tree = shutil.rmtree\n"
            "remove_tree(root)\n")
        self.assertEqual([item.kind for item in violations], ["recursive-removal"])

    def test_direct_rmdir_is_rejected_outside_the_shared_owner(self):
        violations = self.violations("import os\nos.rmdir(root)\npath.rmdir()\n")
        self.assertEqual(
            [item.kind for item in violations], ["recursive-removal", "recursive-removal"])

    def test_register_is_rejected_but_unrelated_register_is_allowed(self):
        violations = self.violations(
            "from acceptance_supervisor_common import DirectoryOwner\n"
            "owner = DirectoryOwner()\nowner.register('root', path, parent)\n"
            "poller.register(fd)\n")
        self.assertEqual([item.kind for item in violations], ["legacy-register"])

    def test_direct_directory_owner_register_is_rejected(self):
        violations = self.violations("DirectoryOwner.register(path)\n")
        self.assertEqual([item.kind for item in violations], ["legacy-register"])

    def test_aliased_directory_owner_register_is_rejected(self):
        violations = self.violations(
            "from acceptance_supervisor_common import DirectoryOwner as Owner\n"
            "Owner.register(path)\n")
        self.assertEqual([item.kind for item in violations], ["legacy-register"])

    def test_boolean_and_path_raw_capture_authority_is_rejected(self):
        violations = self.violations(
            "def finalize(raw_process_captures, raw_process_captures_owned):\n"
            "    return finalize_acceptance_artifacts(raw_process_captures=raw_process_captures, "
            "raw_process_captures_owned=raw_process_captures_owned)\n")
        self.assertEqual(
            {item.kind for item in violations}, {"raw-capture-authority"})

    def test_token_only_raw_capture_call_is_allowed(self):
        violations = self.violations(
            "def finalize(raw_process_captures_token):\n"
            "    return finalize_acceptance_artifacts("
            "raw_process_captures_token=raw_process_captures_token)\n")
        self.assertEqual(violations, ())

    def test_process_capture_requires_one_caller_owner(self):
        violations = self.violations(
            "processes = Processes(output / 'processes')\n"
            "owner.adopt_created('raw process captures', output / 'processes', output)\n")
        self.assertEqual([item.kind for item in violations], ["process-capture-owner"])

    def test_aliased_process_capture_requires_one_caller_owner(self):
        violations = self.violations(
            "from acceptance_supervisor_common import Processes as OwnedProcesses\n"
            "processes = OwnedProcesses(output / 'processes')\n")
        self.assertEqual([item.kind for item in violations], ["process-capture-owner"])

    def test_process_constructor_alias_cannot_bypass_owner_requirement(self):
        violations = self.violations(
            "from acceptance_supervisor_common import Processes\n"
            "ProcessFactory = Processes\n"
            "processes = ProcessFactory(output / 'processes')\n")
        self.assertEqual([item.kind for item in violations], ["process-capture-owner"])

    def test_chained_process_constructor_alias_cannot_bypass_owner_requirement(self):
        violations = self.violations(
            "from acceptance_supervisor_common import Processes\n"
            "First = Processes\n"
            "Second = First\n"
            "processes = Second(output / 'processes')\n")
        self.assertEqual([item.kind for item in violations], ["process-capture-owner"])

    def test_partial_process_factory_requires_owner_or_capture_root(self):
        violations = self.violations(
            "from functools import partial\n"
            "from acceptance_supervisor_common import Processes\n"
            "ProcessBook = partial(Processes, stdout_capture_factory=factory)\n")
        self.assertEqual([item.kind for item in violations], ["process-capture-owner"])

    def test_partial_process_factory_with_owner_is_allowed_through_alias(self):
        violations = self.violations(
            "from functools import partial as make_factory\n"
            "from acceptance_supervisor_common import Processes as P\n"
            "ProcessBook = make_factory(P, owner=owner)\n"
            "processes = ProcessBook(output / 'processes')\n")
        self.assertEqual(violations, ())

    def test_local_partial_alias_retains_process_owner_validation(self):
        violations = self.violations(
            "import functools\n"
            "import acceptance_supervisor_common as common\n"
            "make_factory = functools.partial\n"
            "process_factory = common.Processes\n"
            "ProcessBook = make_factory(process_factory, stdout_capture_factory=factory)\n")
        self.assertEqual([item.kind for item in violations], ["process-capture-owner"])

    def test_direct_and_aliased_popen_are_rejected(self):
        violations = self.violations(
            "import subprocess as process\n"
            "from subprocess import Popen as spawn\n"
            "process.Popen(command)\n"
            "spawn(command)\n")
        self.assertEqual(
            [item.kind for item in violations], ["direct-process-owner", "direct-process-owner"])

    def test_local_popen_assignment_alias_is_rejected(self):
        violations = self.violations(
            "import subprocess\n"
            "spawn = subprocess.Popen\n"
            "spawn(command)\n")
        self.assertEqual([item.kind for item in violations], ["direct-process-owner"])

    def test_process_capture_accepts_owner_or_existing_token(self):
        violations = self.violations(
            "first = Processes(path, owner=ownership)\n"
            "second = Processes(path, capture_root=token)\n")
        self.assertEqual(violations, ())

    def test_literal_recursive_rm_subprocess_is_rejected(self):
        violations = self.violations(
            "import subprocess as process\n"
            "process.run(['/bin/rm', '-rf', str(root)])\n")
        self.assertEqual([item.kind for item in violations], ["subprocess-removal"])

    def test_literal_recursive_rm_options_are_rejected(self):
        violations = self.violations(
            "from subprocess import check_call as execute\n"
            "execute(('rm', '-r', '-f', target))\n")
        self.assertEqual([item.kind for item in violations], ["subprocess-removal"])

    def test_dynamic_or_shell_removal_is_left_unclassified(self):
        violations = self.violations(
            "import subprocess\n"
            "command = ['rm', '-rf', str(root)]\n"
            "subprocess.run(command)\n"
            "subprocess.run('rm -rf ' + str(root), shell=True)\n"
            "program = 'rm'\n"
            "subprocess.run([program, '-rf', str(root)])\n")
        self.assertEqual(violations, ())

    def test_qualified_common_owner_calls_are_allowed(self):
        violations = self.violations(
            "import acceptance_supervisor_common as common\n"
            "owner = common.DirectoryOwner()\n"
            "token = owner.create('root', path, parent)\n"
            "common.Processes(path, owner=owner)\n"
            "common.remove_owned_directory(token)\n")
        self.assertEqual(violations, ())

    def test_shared_common_module_is_the_only_allowlisted_path(self):
        self.assertEqual(verifier.find_violations((verifier.SHARED_COMMON,)), ())

    def test_provider_cannot_define_a_second_process_lifecycle_owner(self):
        violations = self.violations("class ProcessBook:\n    pass\n")
        self.assertEqual([item.kind for item in violations], ["duplicate-process-owner"])


class AcceptanceOwnershipDiscoveryTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="acceptance-discovery-")
        self.addCleanup(directory.cleanup)
        self.scripts = Path(directory.name) / "scripts"
        self.scripts.mkdir()
        self.enterContext(patch.object(verifier, "SCRIPT_DIRECTORY", self.scripts))

    def write_source(self, relative: str, source: str = "") -> Path:
        path = self.scripts / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(source, encoding="utf-8")
        return path

    def test_discovery_includes_all_nested_package_python_files_in_order(self):
        expected = {
            self.write_source("acceptance_existing.py"),
            self.write_source("delegate_acceptance/__init__.py"),
            self.write_source("delegate_acceptance/owners.py"),
            self.write_source("delegate_acceptance/nested/__init__.py"),
            self.write_source("delegate_acceptance/nested/neutral.py"),
            self.write_source("delegate_acceptance/nested/test_helpers.py"),
        }
        self.write_source("unrelated.py")
        self.write_source("acceptance_tests/test_helpers.py")
        self.write_source("delegate_acceptance_backup/owners.py")
        self.write_source("delegate_acceptance/nested/README.md")
        self.assertEqual(verifier.production_paths(), tuple(sorted(expected)))

    def test_missing_package_keeps_legacy_discovery(self):
        expected = self.write_source("acceptance_existing.py")
        self.assertEqual(verifier.production_paths(), (expected,))

    def test_default_gate_rejects_nested_renamed_modules_and_aliases(self):
        cases = (
            ("import shutil as fs\nremove = fs.rmtree\nremove(root)\n",
             "recursive-removal"),
            ("import subprocess as sp\nspawn = sp.Popen\nspawn(argv)\n",
             "direct-process-owner"),
            ("from acceptance_supervisor_common import Processes as P\n"
             "Factory = P\nFactory(path)\n", "process-capture-owner"),
            ("from acceptance_supervisor_common import DirectoryOwner as D\n"
             "Alias = D\nAlias.register(path)\n", "legacy-register"),
            ("from acceptance_supervisor_common import remove_owned_directory as remove\n"
             "remove(directory=path, parent=parent)\n", "path-only-removal"),
            ("import functools as f\n"
             "from acceptance_supervisor_common import Processes as P\n"
             "bind = f.partial\nbind(P)\n", "process-capture-owner"),
            ("class ProcessBook:\n    pass\n", "duplicate-process-owner"),
        )
        for index, (source, kind) in enumerate(cases):
            with self.subTest(kind=kind):
                path = self.write_source(
                    f"delegate_acceptance/nested/case_{index}/neutral.py", source)
                self.addCleanup(path.unlink)
                violations = verifier.find_violations(verifier.production_paths())
                self.assertIn((path, kind), [(item.path, item.kind) for item in violations])
                with patch.object(verifier.sys, "stderr", io.StringIO()) as errors:
                    self.assertEqual(verifier.main(), 1)
                self.assertIn(f"{path}:", errors.getvalue())

    def test_default_gate_accepts_nested_canonical_owner_calls_and_aliases(self):
        path = self.write_source(
            "delegate_acceptance/nested/neutral.py",
            "import acceptance_supervisor_common as common\n"
            "from functools import partial as bind\n"
            "Owner = common.DirectoryOwner\n"
            "owner = Owner()\n"
            "token = owner.create('capture', path, parent)\n"
            "Factory = common.Processes\n"
            "Factory(path, capture_root=token)\n"
            "OwnedFactory = bind(Factory, owner=owner)\n"
            "OwnedFactory(path)\n"
            "remove = common.remove_owned_directory\n"
            "remove(token)\n")
        self.assertEqual(verifier.production_paths(), (path,))
        with patch.object(verifier.sys, "stderr", io.StringIO()) as errors:
            self.assertEqual(verifier.main(), 0)
        self.assertEqual(errors.getvalue(), "")

    def test_nested_copies_of_owner_and_fixture_filenames_are_not_exempt(self):
        common = self.write_source(
            "delegate_acceptance/nested/acceptance_supervisor_common.py",
            "import shutil\nshutil.rmtree(root)\nclass Process:\n    pass\n")
        fixture = self.write_source(
            "delegate_acceptance/nested/acceptance_private_cli.py",
            "from subprocess import Popen as spawn\nspawn(argv)\n")
        violations = verifier.find_violations(verifier.production_paths())
        self.assertEqual(
            {(item.path, item.kind) for item in violations},
            {(common, "recursive-removal"), (common, "duplicate-process-owner"),
             (fixture, "direct-process-owner")})


class AcceptanceOwnershipExceptionTests(unittest.TestCase):
    def test_owner_and_fixture_exceptions_remain_exact_canonical_paths(self):
        scripts = Path(verifier.__file__).resolve().parent
        self.assertEqual(verifier.SHARED_COMMON, scripts / "acceptance_supervisor_common.py")
        self.assertEqual(
            verifier.DIRECT_FIXTURE_SUBPROCESS_FILES,
            frozenset({scripts / "delegate_acceptance" / "private_cli" / "capture.py"}))

    def test_only_exact_fixture_path_can_call_direct_popen(self):
        scripts = Path(verifier.__file__).resolve().parent
        paths = (
            scripts / "delegate_acceptance" / "private_cli" / "capture.py",
            scripts / "acceptance_private_cli.py",
            scripts / "acceptance_private_cli_copy.py",
            scripts / "delegate_acceptance" / "acceptance_private_cli.py",
        )
        source = "import subprocess as sp\nspawn = sp.Popen\nspawn(argv)\n"
        with patch.object(Path, "read_text", return_value=source):
            violations = verifier.find_violations(paths)
        self.assertEqual(
            {(item.path, item.kind) for item in violations},
            {(path, "direct-process-owner") for path in paths[1:]})

    def test_fixture_exception_does_not_allow_directory_or_process_ownership(self):
        scripts = Path(verifier.__file__).resolve().parent
        source = (
            "import subprocess\nsubprocess.Popen(argv)\n"
            "import shutil\nshutil.rmtree(root)\n"
            "subprocess.run(['rm', '-rf', root])\n"
            "class Processes:\n    pass\n")
        with patch.object(Path, "read_text", return_value=source):
            violations = verifier.find_violations((scripts / "delegate_acceptance" / "private_cli" / "capture.py",))
        self.assertEqual(
            {item.kind for item in violations},
            {"recursive-removal", "subprocess-removal", "duplicate-process-owner"})


class CanonicalPackageOwnershipTests(unittest.TestCase):
    def violations(self, path: Path, source: str):
        with patch.object(Path, "read_text", return_value=source):
            return verifier.find_violations((path,))

    def test_exact_operations_files_have_separate_process_and_directory_permissions(self):
        source = (
            "import os\nimport subprocess\n"
            "class Process:\n    pass\n"
            "class DirectoryOwner:\n    pass\n"
            "subprocess.Popen(argv)\nos.rmdir(root)\n")
        cases = (
            (verifier.PROCESS_OWNER, {"duplicate-directory-owner", "recursive-removal"}),
            (verifier.DIRECTORY_OWNER, {"duplicate-process-owner", "direct-process-owner"}),
            (verifier.DIRECTORY_JOURNAL,
             {"duplicate-process-owner", "duplicate-directory-owner", "direct-process-owner"}),
            (verifier.SHARED_COMMON,
             {"duplicate-process-owner", "duplicate-directory-owner",
              "direct-process-owner", "recursive-removal"}),
        )
        for path, expected in cases:
            with self.subTest(path=path):
                self.assertEqual({item.kind for item in self.violations(path, source)}, expected)

    def test_operation_exceptions_do_not_allow_recursive_rm_or_missing_capture_owner(self):
        source = (
            "import subprocess\nsubprocess.run(['rm', '-rf', root])\n"
            "from acceptance_supervisor_common import Processes\nProcesses(path)\n")
        for path in (verifier.PROCESS_OWNER, verifier.DIRECTORY_OWNER,
                     verifier.DIRECTORY_JOURNAL):
            with self.subTest(path=path):
                self.assertEqual(
                    {item.kind for item in self.violations(path, source)},
                    {"subprocess-removal", "process-capture-owner"})

    def test_neutral_nested_operations_filename_is_not_an_owner(self):
        source = "class DirectoryOwner:\n    pass\nimport os\nos.rmdir(root)\n"
        path = verifier.PROCESS_OWNER.parent / "nested" / "operations.py"
        self.assertEqual(
            {item.kind for item in self.violations(path, source)},
            {"duplicate-directory-owner", "recursive-removal"})

    def test_canonical_package_import_and_assignment_aliases_require_capture_owner(self):
        imports = (
            "from delegate_acceptance.processes.operations import Processes as P\n",
            "import delegate_acceptance.processes.operations as p\nP = p.Processes\n",
            "from delegate_acceptance.processes import operations as p\nP = p.Processes\n",
            "import delegate_acceptance.processes.operations\n"
            "P = delegate_acceptance.processes.operations.Processes\n",
        )
        path = verifier.PROCESS_OWNER.parent / "nested" / "neutral.py"
        canonical_imports = tuple(statement.replace("delegate_acceptance", "scripts.delegate_acceptance")
                                  for statement in imports)
        for statement in (*imports, *canonical_imports):
            with self.subTest(statement=statement):
                self.assertEqual(
                    [item.kind for item in self.violations(path, statement + "P(path)\n")],
                    ["process-capture-owner"])
                self.assertEqual(
                    self.violations(path, statement + "P(path, owner=owner)\n"), ())

    def test_canonical_directory_aliases_cannot_bypass_token_or_register_rules(self):
        source = (
            "from delegate_acceptance.directories import operations as directory\n"
            "Owner = directory.DirectoryOwner\nOwner.register(path)\n"
            "remove = directory.remove_owned_directory\nremove(path, parent)\n")
        path = verifier.DIRECTORY_OWNER.parent / "nested" / "neutral.py"
        for statement in (source, source.replace("delegate_acceptance", "scripts.delegate_acceptance")):
            with self.subTest(source=statement):
                self.assertEqual(
                    {item.kind for item in self.violations(path, statement)},
                    {"legacy-register", "path-only-removal"})

    def test_namespaced_facade_aliases_preserve_all_owner_requirements(self):
        imports = (
            "from scripts.acceptance_supervisor_common import Processes as Factory\n",
            "import scripts.acceptance_supervisor_common as common\nFactory = common.Processes\n",
            "from scripts import acceptance_supervisor_common as common\nFactory = common.Processes\n",
            "import scripts.acceptance_supervisor_common\n"
            "Factory = scripts.acceptance_supervisor_common.Processes\n",
        )
        path = verifier.PROCESS_OWNER.parent / "nested" / "neutral.py"
        for statement in imports:
            with self.subTest(statement=statement):
                self.assertEqual(
                    [item.kind for item in self.violations(path, statement + "Factory(path)\n")],
                    ["process-capture-owner"])
                self.assertEqual(
                    self.violations(path, statement + "Factory(path, capture_root=token)\n"), ())
        source = (
            "from scripts.acceptance_supervisor_common import DirectoryOwner as Owner\n"
            "from scripts.acceptance_supervisor_common import remove_owned_directory as remove\n"
            "Owner.register(path)\nremove(path, parent)\n")
        self.assertEqual({item.kind for item in self.violations(path, source)},
                         {"legacy-register", "path-only-removal"})

    def test_relative_owner_imports_reject_alias_bypasses_and_accept_tokens(self):
        statements = (
            "from ..processes.operations import Processes as Factory\n",
            "from ..processes import operations as owner\nFactory = owner.Processes\n",
            "from .. import processes as owner\nFactory = owner.operations.Processes\n",
            "from ...delegate_acceptance.processes.operations import Processes as Factory\n",
        )
        path = verifier.PROCESS_OWNER.parent.parent / "nested" / "neutral.py"
        for statement in statements:
            with self.subTest(statement=statement):
                self.assertEqual(
                    [item.kind for item in self.violations(path, statement + "Alias = Factory\nAlias(path)\n")],
                    ["process-capture-owner"])
                self.assertEqual(
                    self.violations(path, statement + "Factory(path, capture_root=token)\n"), ())

    def test_relative_directory_and_facade_aliases_keep_token_rules(self):
        sources = (
            "from ..directories import operations as owner\n"
            "Owner = owner.DirectoryOwner\nremove = owner.remove_owned_directory\n",
            "from .. import directories as owner\n"
            "Owner = owner.operations.DirectoryOwner\nremove = owner.operations.remove_owned_directory\n",
            "from ... import acceptance_supervisor_common as owner\n"
            "Owner = owner.DirectoryOwner\nremove = owner.remove_owned_directory\n",
        )
        path = verifier.DIRECTORY_OWNER.parent.parent / "nested" / "neutral.py"
        for source in sources:
            with self.subTest(source=source):
                self.assertEqual(
                    {item.kind for item in self.violations(path, source + "Owner.register(path)\nremove(path, parent)\n")},
                    {"legacy-register", "path-only-removal"})
                self.assertEqual(
                    self.violations(path, source + "owner = Owner()\nremove(token)\n"), ())

    def test_relative_imports_use_the_current_package_for_init_files(self):
        path = verifier.PROCESS_OWNER.parent.parent / "nested" / "__init__.py"
        source = "from ..processes.operations import Processes as Alias\nAlias(path)\n"
        self.assertEqual([item.kind for item in self.violations(path, source)],
                         ["process-capture-owner"])


if __name__ == "__main__":
    unittest.main()
