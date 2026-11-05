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

    def test_schema_only_typed_dict_fields_do_not_claim_capture_authority(self):
        for declaration, base in (
                ("from typing import TypedDict", "TypedDict"),
                ("from typing import TypedDict as Schema", "Schema"),
                ("import typing", "typing.TypedDict"),
                ("import typing as types", "types.TypedDict"),
                ("from typing import NamedTuple as Schema, TypedDict as Schema", "Schema"),
                ("import collections as typing, typing", "typing.TypedDict")):
            for keyword in ("", ", total=False"):
                with self.subTest(declaration=declaration, keyword=keyword):
                    violations = self.violations(
                        f"{declaration}\nclass CleanupReceipt({base}{keyword}):\n"
                        "    raw_process_captures: str\n")
                    self.assertEqual(violations, ())

    def test_typed_dict_field_exemption_does_not_cover_runtime_authority(self):
        schema = ("from typing import TypedDict\nclass Receipt(TypedDict):\n"
                  "    raw_process_captures: str\n")
        for runtime in (
                "raw_process_captures = path\n",
                "capture = raw_process_captures\n",
                "capture = receipt.raw_process_captures\n",
                "receipt.raw_process_captures = path\n",
                "finalize_acceptance_artifacts(raw_process_captures=False)\n"):
            with self.subTest(runtime=runtime):
                violations = self.violations(schema + runtime)
                self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])
                self.assertEqual(violations[0].line, 4)

    def test_typed_dict_runtime_field_defaults_are_rejected(self):
        violations = self.violations(
            "from typing import TypedDict\nclass Receipt(TypedDict):\n"
            "    raw_process_captures: str = path\n")
        self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_non_schema_annotations_cannot_claim_the_typed_dict_exemption(self):
        for declaration, base in (
                ("class TypedDict: pass", "TypedDict"),
                ("from typing import TypedDict\nTypedDict = object", "TypedDict"),
                ("import typing\ntyping = object()", "typing.TypedDict"),
                ("import typing\ntyping.TypedDict = object", "typing.TypedDict"),
                ("from typing import TypedDict\nif condition:\n    TypedDict = object", "TypedDict")):
            with self.subTest(declaration=declaration):
                violations = self.violations(
                    f"{declaration}\nclass Receipt({base}):\n"
                    "    raw_process_captures: str\n")
                self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_schema_imports_obey_each_alias_binding_in_source_order(self):
        for declaration, base in (
                ("from typing import TypedDict as Schema, NamedTuple as Schema", "Schema"),
                ("from typing import TypedDict, NamedTuple as TypedDict", "TypedDict"),
                ("import typing, collections as typing", "typing.TypedDict")):
            with self.subTest(declaration=declaration):
                violations = self.violations(
                    f"{declaration}\nclass Receipt({base}):\n"
                    "    raw_process_captures: str\n")
                self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_module_evaluated_bindings_invalidate_schema_imports(self):
        changes = (
            "def f(value=(Schema := object)): pass\n",
            "def f(*, value=(Schema := object)): pass\n",
            "def f(value: (Schema := object)): pass\n",
            "def f() -> (Schema := object): pass\n",
            "async def f(value=(Schema := object)): pass\n",
            "@decorate((Schema := object))\ndef f(): pass\n",
            "class Prior((Schema := object)): pass\n",
            "class Prior(object, option=(Schema := object)): pass\n",
            "@decorate((Schema := object))\nclass Prior: pass\n",
            "try: pass\nexcept Exception as Schema: pass\n",
            "match value:\n    case Schema: pass\n",
            "match value:\n    case [*Schema]: pass\n",
            "match value:\n    case {'x': x, **Schema}: pass\n",
            "(Schema := other).x = 1\n",
            "del (Schema := other).x\n",
            "from other import *\n",
            "if condition:\n    from other import *\n",
        )
        for change in changes:
            with self.subTest(change=change):
                violations = self.violations(
                    "from typing import TypedDict as Schema\n" + change +
                    "class Receipt(Schema):\n    raw_process_captures: str\n")
                self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_wildcard_imports_cannot_reestablish_schema_trust(self):
        for module in ("typing", "other"):
            with self.subTest(module=module):
                violations = self.violations(
                    f"from {module} import *\n"
                    "TypedDict.x = replacement\n"
                    "from typing import TypedDict as Schema\n"
                    "class Receipt(Schema):\n    raw_process_captures: str\n")
                self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_global_rebinding_anywhere_keeps_schema_fields_governed(self):
        for position in ("before", "after"):
            with self.subTest(position=position):
                schema = "class Receipt(Schema):\n    raw_process_captures: str\n"
                rebind = "def change():\n    global Schema\n    Schema = object\n"
                body = rebind + schema if position == "before" else schema + rebind
                violations = self.violations("from typing import TypedDict as Schema\n" + body)
                self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_attribute_mutation_anywhere_invalidates_schema_module_imports(self):
        changes = (
            "class Mutator:\n    types.TypedDict = object\n",
            "def mutate():\n    types.TypedDict = object\n",
            "class Mutator:\n    del types.TypedDict\n",
            "def mutate():\n    del types.TypedDict\n",
        )
        schema = "class Receipt(types.TypedDict):\n    raw_process_captures: str\n"
        for change in changes:
            for position in ("before", "after"):
                with self.subTest(change=change, position=position):
                    body = change + schema if position == "before" else schema + change
                    violations = self.violations("import typing as types\n" + body)
                    self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_module_object_escape_keeps_schema_fields_governed(self):
        escapes = (
            "alias = types\nalias.TypedDict = replacement\n",
            "inspect_module(types)\n",
            "def get_module(): return types\n",
            "stored = [types]\n",
            "def inspect_default(module=types): pass\n",
            "types.__dict__['TypedDict'] = replacement\n",
            "inspect_type(types.TypedDict)\n",
            "other = types.TypedDict\n",
        )
        schema = "class Receipt(types.TypedDict):\n    raw_process_captures: str\n"
        for escape in escapes:
            for position in ("before", "after"):
                with self.subTest(escape=escape, position=position):
                    body = escape + schema if position == "before" else schema + escape
                    violations = self.violations("import typing as types\n" + body)
                    self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_imported_factory_escape_keeps_schema_fields_governed(self):
        escapes = (
            "alias = Schema\nalias.__mro_entries__ = replacement\n",
            "inspect_type(Schema)\n",
            "def get_factory(): return Schema\n",
            "stored = {'factory': Schema}\n",
            "Schema.__dict__['__mro_entries__'] = replacement\n",
            "Schema.__globals__['TypedDict'] = replacement\n",
        )
        schema = "class Receipt(Schema):\n    raw_process_captures: str\n"
        for escape in escapes:
            for position in ("before", "after"):
                with self.subTest(escape=escape, position=position):
                    body = escape + schema if position == "before" else schema + escape
                    violations = self.violations("from typing import TypedDict as Schema\n" + body)
                    self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_module_aliases_share_one_canonical_typing_trust_group(self):
        cases = (
            ("import typing as t\nimport typing as u\n", "u.TypedDict = object\n", "t.TypedDict"),
            ("import typing as u\nimport typing as t\n", "u.TypedDict = object\n", "t.TypedDict"),
            ("import typing as t, typing as u\n", "u.TypedDict = object\n", "t.TypedDict"),
            ("import typing as t\n", "t.TypedDict = object\nfrom typing import TypedDict as Schema\n", "Schema"),
            ("from typing import TypedDict as Schema\nimport typing as t\n", "t.TypedDict = object\n", "Schema"),
            ("import typing as t\n", "def mutate():\n    import typing as u\n    u.TypedDict = object\n", "t.TypedDict"),
        )
        for imports, escape, base in cases:
            with self.subTest(imports=imports, escape=escape):
                violations = self.violations(imports + escape +
                                             f"class Receipt({base}):\n    raw_process_captures: str\n")
                self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_factory_aliases_share_the_canonical_typing_trust_group(self):
        cases = (
            ("from typing import TypedDict as First\nfrom typing import TypedDict as Second\n", "First"),
            ("from typing import TypedDict as Second\nfrom typing import TypedDict as First\n", "First"),
            ("from typing import TypedDict as First, TypedDict as Second\n", "First"),
        )
        for imports, escaped in cases:
            with self.subTest(imports=imports):
                violations = self.violations(imports + f"{escaped}.__mro_entries__ = replacement\n"
                                             "class Receipt(Second):\n    raw_process_captures: str\n")
                self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])
        for imports, escape, base in (
                ("import typing as t\nfrom typing import TypedDict as Schema\n",
                 "Schema.__mro_entries__ = replacement\n", "t.TypedDict"),
                ("from typing import TypedDict as Schema\n",
                 "def mutate():\n    from typing import TypedDict as Other\n    inspect_type(Other)\n", "Schema")):
            with self.subTest(imports=imports, escape=escape):
                violations = self.violations(imports + escape +
                                             f"class Receipt({base}):\n    raw_process_captures: str\n")
                self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_shadowed_or_global_import_names_revoke_the_canonical_group(self):
        cases = (
            ("import typing as t\nimport typing as u\n", "u = object()\ninspect_module(u)\n", "t.TypedDict"),
            ("from typing import TypedDict as First, TypedDict as Second\n",
             "First = object()\ninspect_type(First)\n", "Second"),
            ("import typing as t\nimport typing as u\n",
             "def change():\n    global u\n    u = object()\n", "t.TypedDict"),
            ("from typing import TypedDict as First, TypedDict as Second\n",
             "def change():\n    global First\n    First = object()\n", "Second"),
        )
        for imports, escape, base in cases:
            with self.subTest(imports=imports, escape=escape):
                violations = self.violations(imports + escape +
                                             f"class Receipt({base}):\n    raw_process_captures: str\n")
                self.assertEqual([item.kind for item in violations], ["raw-capture-authority"])

    def test_canonical_import_can_be_reestablished_after_plain_rebinding(self):
        for declaration, base, rebound in (
                ("import typing as t\n", "t.TypedDict", "t"),
                ("from typing import TypedDict as Schema\n", "Schema", "Schema")):
            with self.subTest(declaration=declaration):
                violations = self.violations(declaration + f"{rebound} = object\n" + declaration +
                                             f"class Receipt({base}):\n    raw_process_captures: str\n")
                self.assertEqual(violations, ())

    def test_only_direct_undecorated_canonical_schema_bases_are_exempted(self):
        field = "    raw_process_captures: str\n"
        cases = (
            "from typing import TypedDict\n@decorate\nclass Receipt(TypedDict):\n" + field,
            "from typing import TypedDict\nclass Parent(TypedDict): pass\nclass Receipt(Parent):\n" + field,
            "from typing import TypedDict\n@decorate\nclass Parent(TypedDict): pass\nclass Receipt(Parent):\n" + field,
            "from other import TypedDict\nclass Receipt(TypedDict):\n" + field,
            "import typing\nclass Receipt(typing.NamedTuple):\n" + field,
            "from .typing import TypedDict\nclass Receipt(TypedDict):\n" + field,
            "from typing import TypedDict\nclass Receipt(TypedDict, Other):\n" + field,
            "class Receipt(TypedDict):\n" + field + "from typing import TypedDict\n",
            "from typing import TypedDict\nclass Receipt(TypedDict, metaclass=Factory):\n" + field,
            "from typing import TypedDict\nclass Receipt(TypedDict, closed=True):\n" + field,
            "from typing import TypedDict\nclass Receipt(TypedDict, **options):\n" + field,
        )
        for source in cases:
            with self.subTest(source=source):
                self.assertEqual([item.kind for item in self.violations(source)], ["raw-capture-authority"])

    def test_schema_fields_do_not_authorize_identityless_cleanup(self):
        schema = ("from typing import TypedDict\nclass Receipt(TypedDict):\n"
                  "    raw_process_captures: str\n")
        for cleanup, expected in (
                ("remove_owned_directory(directory=path, parent=parent)\n", "path-only-removal"),
                ("Processes(path)\n", "process-capture-owner")):
            with self.subTest(cleanup=cleanup):
                self.assertEqual([item.kind for item in self.violations(schema + cleanup)], [expected])

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
        self.assertEqual(
            [item.kind for item in violations], ["direct-process-owner", "subprocess-removal"])

    def test_literal_recursive_rm_options_are_rejected(self):
        violations = self.violations(
            "from subprocess import check_call as execute\n"
            "execute(('rm', '-r', '-f', target))\n")
        self.assertEqual(
            [item.kind for item in violations], ["direct-process-owner", "subprocess-removal"])

    def test_dynamic_or_shell_removal_is_left_unclassified(self):
        violations = self.violations(
            "import subprocess\n"
            "command = ['rm', '-rf', str(root)]\n"
            "subprocess.run(command)\n"
            "subprocess.run('rm -rf ' + str(root), shell=True)\n"
            "program = 'rm'\n"
            "subprocess.run([program, '-rf', str(root)])\n")
        # Removal stays unclassified; every raw child still needs the shared owner.
        self.assertEqual([item.kind for item in violations], ["direct-process-owner"] * 3)

    def test_every_canonical_subprocess_api_and_its_aliases_are_rejected(self):
        for api in sorted(verifier.SUBPROCESS_APIS):
            with self.subTest(api=api):
                violations = self.violations(f"import subprocess\nsubprocess.{api}(argv)\n")
                self.assertEqual([item.kind for item in violations], ["direct-process-owner"])
        for source in (
            "import subprocess as sp\nsp.run(argv)\n",
            "from subprocess import run\nrun(argv)\n",
            "from subprocess import run as execute\nexecute(argv)\n",
            "import subprocess\nexecute = subprocess.run\nagain = execute\nagain(argv)\n",
            "from subprocess import check_output as read\nalias = read\nalias(argv)\n",
        ):
            with self.subTest(source=source):
                self.assertEqual(
                    [item.kind for item in self.violations(source)], ["direct-process-owner"])

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
        workspace = Path(directory.name) / "source"
        workspace.mkdir()
        alias = Path(directory.name) / "alias"
        alias.symlink_to(workspace, target_is_directory=True)
        self.scripts = alias.resolve() / "scripts"
        self.scripts.mkdir()
        self.enterContext(patch.object(verifier, "SCRIPT_DIRECTORY", self.scripts))
        self.governed = self.write_source("test_homebrew_formula_render.py")

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
            self.governed,
        }
        self.write_source("unrelated.py")
        self.write_source("acceptance_tests/test_helpers.py")
        self.write_source("delegate_acceptance_backup/owners.py")
        self.write_source("delegate_acceptance/nested/README.md")
        self.assertEqual(verifier.production_paths(), tuple(sorted(expected)))

    def test_missing_package_keeps_legacy_discovery(self):
        expected = self.write_source("acceptance_existing.py")
        self.assertEqual(verifier.production_paths(), tuple(sorted({expected, self.governed})))

    def test_default_gate_rejects_nested_renamed_modules_and_aliases(self):
        cases = (
            ("import shutil as fs\nremove = fs.rmtree\nremove(root)\n",
             "recursive-removal"),
            ("import subprocess as sp\nspawn = sp.Popen\nspawn(argv)\n",
             "direct-process-owner"),
            ("import subprocess as sp\nexecute = sp.run\nexecute(argv)\n",
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
        self.assertEqual(verifier.production_paths(), tuple(sorted({path, self.governed})))
        with patch.object(verifier.sys, "stderr", io.StringIO()) as errors:
            self.assertEqual(verifier.main(), 0)
        self.assertEqual(errors.getvalue(), "")

    def test_default_gate_rejects_raw_children_in_the_governed_test(self):
        for source in ("import subprocess\nsubprocess.run(argv, check=False)\n",
                       "from subprocess import run as execute\nexecute(argv)\n"):
            with self.subTest(source=source):
                self.governed.write_text(source, encoding="utf-8")
                violations = verifier.find_violations(verifier.production_paths())
                self.assertEqual([(item.path, item.kind) for item in violations],
                                 [(self.governed, "direct-process-owner")])
                with patch.object(verifier.sys, "stderr", io.StringIO()) as errors:
                    self.assertEqual(verifier.main(), 1)
                self.assertIn(f"{self.governed}:", errors.getvalue())

    def test_missing_governed_test_fails_the_default_gate(self):
        self.governed.unlink()
        violations = verifier.find_violations(verifier.production_paths())
        self.assertEqual([(item.path, item.kind) for item in violations], [(self.governed, "source")])

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

    def test_canonical_owners_may_call_every_subprocess_api(self):
        scripts = Path(verifier.__file__).resolve().parent
        source = "import subprocess\n" + "".join(
            f"subprocess.{api}(argv)\n" for api in sorted(verifier.SUBPROCESS_APIS))
        for path in (verifier.PROCESS_OWNER,
                     scripts / "delegate_acceptance" / "private_cli" / "capture.py"):
            with self.subTest(path=path), patch.object(Path, "read_text", return_value=source):
                self.assertEqual(verifier.find_violations((path,)), ())

    def test_real_homebrew_test_is_governed_and_uses_the_shared_owner(self):
        path = Path(verifier.__file__).resolve().parent / "test_homebrew_formula_render.py"
        self.assertIn(path, verifier.production_paths())
        self.assertEqual(verifier.find_violations((path,)), ())

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
        cases = (
            (verifier.PROCESS_OWNER, {"subprocess-removal", "process-capture-owner"}),
            (verifier.DIRECTORY_OWNER,
             {"subprocess-removal", "process-capture-owner", "direct-process-owner"}),
            (verifier.DIRECTORY_JOURNAL,
             {"subprocess-removal", "process-capture-owner", "direct-process-owner"}),
        )
        for path, expected in cases:
            with self.subTest(path=path):
                self.assertEqual({item.kind for item in self.violations(path, source)}, expected)

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
