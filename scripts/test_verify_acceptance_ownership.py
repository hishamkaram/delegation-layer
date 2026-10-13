"""Focused tests for the acceptance ownership AST gate."""

from pathlib import Path
import tempfile
import unittest

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


if __name__ == "__main__":
    unittest.main()
