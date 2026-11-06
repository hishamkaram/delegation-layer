"""Bounded source installation and constructor cleanup without native actors."""

import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import acceptance_contributor as contributor
import acceptance_supervisor_native as native
from scripts.acceptance_tests.hermetic.test_installation import (
    forbid_program_content, source_tree, write_program,
)
from scripts.acceptance_tests.resources import DirectoryPinOwner
from scripts.delegate_acceptance.directories.operations import DirectoryOwner, rollback_owned_directories
from scripts.delegate_acceptance.hermetic import case as case_module
from scripts.delegate_acceptance.hermetic import installation
from scripts.delegate_acceptance.hermetic.cli import main as hermetic_main
from scripts.delegate_acceptance.processes.contracts import (
    BoundedCommandError, BoundedCommandResult, CommandFailure,
)


class SourceCommandTests(unittest.TestCase):
    def source(self, root):
        source_tree(root)
        for package in installation.SOURCE_PACKAGES.values():
            (root / package).mkdir(parents=True, exist_ok=True)
        return root

    def test_build_uses_exact_source_role_and_never_inspects_installed_output(self):
        with forbid_program_content(), tempfile.TemporaryDirectory() as temporary:
            source = self.source(Path(temporary).resolve(strict=True))
            target = source / "quoted ' ; install"
            target.mkdir()
            calls = []

            def build(arguments, environment, **options):
                calls.append((arguments, environment, options))
                write_program(Path(arguments[arguments.index("-o") + 1]))
                return BoundedCommandResult(0, b"", b"")

            with patch.object(installation, "run_bounded_command", build):
                for role in installation.SOURCE_PACKAGES:
                    result = installation.build_source_command(source, role, target / role)
                    self.assertEqual(result, target / role)
            for role, (arguments, environment, options) in zip(installation.SOURCE_PACKAGES, calls):
                self.assertEqual(arguments[-1], installation.SOURCE_PACKAGES[role])
                self.assertEqual(arguments[:3] if role == "scheduler_fixture" else arguments[:2],
                                 ["go", "test", "-c"] if role == "scheduler_fixture" else ["go", "build"])
                self.assertEqual(environment["GOTOOLCHAIN"], "local")
                self.assertEqual(options["cwd"], source)
                self.assertEqual(options["timeout"], 900)
                self.assertTrue(options["capture_stderr"])

    def test_invalid_role_occupied_target_and_missing_package_fail_before_launch(self):
        with forbid_program_content(), tempfile.TemporaryDirectory() as temporary:
            source = self.source(Path(temporary).resolve(strict=True))
            target = write_program(source / "installed")
            with patch.object(installation, "run_bounded_command") as command:
                with self.assertRaisesRegex(RuntimeError, "unknown fixture source role"):
                    installation.build_source_command(source, "arbitrary", source / "new")
                with self.assertRaisesRegex(RuntimeError, "occupied"):
                    installation.build_source_command(source, "harness_delegate", target)
                (source / installation.SOURCE_PACKAGES["harness_delegate"]).rmdir()
                with self.assertRaisesRegex(RuntimeError, "unavailable"):
                    installation.build_source_command(source, "harness_delegate", source / "new")
                command.assert_not_called()

    def test_build_failure_preserves_closure_cause_without_private_compiler_output(self):
        with forbid_program_content(), tempfile.TemporaryDirectory() as temporary:
            source = self.source(Path(temporary).resolve(strict=True))
            failure = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT)
            with patch.object(installation, "run_bounded_command", side_effect=failure):
                with self.assertRaisesRegex(RuntimeError, "command_termination_timeout") as raised:
                    installation.build_source_command(source, "harness_delegate", source / "new")
            self.assertIs(raised.exception.__cause__, failure)
            with patch.object(installation, "run_bounded_command", return_value=BoundedCommandResult(
                    1, b"private compiler output", b"private settings")):
                with self.assertRaisesRegex(RuntimeError, "exit 1") as raised:
                    installation.build_source_command(source, "harness_delegate", source / "new")
            self.assertNotIn("private", str(raised.exception))

    def test_driver_bindings_keep_config_integrity_without_program_reads(self):
        with forbid_program_content(), tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve(strict=True)
            source = self.source(root)
            pueue, pueued = write_program(root / "pueue"), write_program(root / "pueued")
            for module, cls, count in ((contributor, contributor.ContributorAcceptance, 3),
                                       (native, native.NativeSuite, 4)):
                with self.subTest(driver=cls.__name__):
                    process_owner = Mock()
                    version_dir = root / cls.__name__
                    version_dir.mkdir()
                    (version_dir / "stdout").write_text("pueue 4.0.4\n")
                    version = SimpleNamespace(directory=version_dir)
                    other_dir = version_dir / "daemon"
                    other_dir.mkdir()
                    (other_dir / "stdout").write_text("pueued 4.0.4\n")
                    discovery = SimpleNamespace(json=lambda: {"providers": [{"id": contributor.PROVIDER}]})
                    process_owner.run.side_effect = ([version, SimpleNamespace(directory=other_dir), discovery]
                                                     if module is contributor else RuntimeError("stop before actor"))
                    process_owner.start.side_effect = RuntimeError("stop before actor")
                    calls = []

                    def build(_source, role, target):
                        calls.append(role)
                        return write_program(target)

                    with patch.object(module, "Processes", return_value=process_owner), \
                            patch.object(module, "build_source_command", build):
                        owner = cls(source, pueue, pueued, root / (cls.__name__ + "-evidence"))
                    try:
                        self.assertEqual(len(calls), count)
                        with patch("platform.platform", return_value="actorless-host"), \
                                patch("platform.machine", return_value="actorless-machine"), \
                                self.assertRaisesRegex(RuntimeError, "stop before actor"):
                            owner.setup()
                        binding = json.loads((owner.output / "binding.json").read_text())
                        self.assertNotIn("binaries", binding)
                        self.assertEqual(binding["executables"]["pueue"], str(pueue))
                        self.assertTrue(binding["config_sha256"])
                        if isinstance(owner, native.NativeSuite):
                            profile = json.loads(owner.profile.read_text())
                            self.assertNotIn("provider_sha256", profile)
                            self.assertTrue(profile["provider_config_sha256"])
                            self.assertIn("provider ' ; literal", str(owner.provider))
                    finally:
                        self.assertTrue(rollback_owned_directories(owner.owned_directories))

    def test_all_constructors_retain_only_when_build_wait_is_unproven(self):
        with forbid_program_content(), tempfile.TemporaryDirectory() as temporary:
            root = self.source(Path(temporary).resolve(strict=True))
            pueue, pueued = write_program(root / "pueue"), write_program(root / "pueued")
            for module, cls in ((case_module, case_module.Case),
                                (contributor, contributor.ContributorAcceptance), (native, native.NativeSuite)):
                for code in (None, 1):
                    with self.subTest(driver=cls.__name__, exit_code=code):
                        directory_owner = DirectoryPinOwner(self)
                        if cls is case_module.Case:
                            owner = object.__new__(case_module.Case)
                        elif cls is contributor.ContributorAcceptance:
                            owner = object.__new__(contributor.ContributorAcceptance)
                        else:
                            owner = object.__new__(native.NativeSuite)
                        error = BoundedCommandError(CommandFailure.TERMINATION_TIMEOUT, code)
                        with patch.object(module, "DirectoryOwner", return_value=directory_owner), \
                                patch.object(module, "build_source_command", side_effect=error), \
                                patch.object(module, "Processes", return_value=Mock()):
                            with self.assertRaises((RuntimeError, BoundedCommandError)) as raised:
                                if isinstance(owner, case_module.Case):
                                    case_module.Case.__init__(owner, suite=SimpleNamespace(source_root=root), case_id="CLOSURE")
                                elif isinstance(owner, contributor.ContributorAcceptance):
                                    contributor.ContributorAcceptance.__init__(
                                        owner, root, pueue, pueued, root / (cls.__name__ + str(code)))
                                else:
                                    native.NativeSuite.__init__(
                                        owner, root, pueue, pueued, root / (cls.__name__ + str(code)))
                        self.assertEqual(owner.base.exists(), code is None)
                        if code is None:
                            self.assertIn(str(owner.base), str(raised.exception))
                            # This test has no actual child. Reacquire the retained
                            # fixture only after checking the production refusal.
                            cleanup = DirectoryOwner()
                            cleanup.adopt_created("retained fixture", owner.base, owner.base.parent)
                            self.assertTrue(cleanup.rollback())
                            self.assertFalse(owner.base.exists())

    def test_retired_tools_flag_is_refused_before_any_driver_is_created(self):
        for main in (contributor.main, native.main, hermetic_main):
            arguments = ["acceptance", "--tools", "/installed", "--output", "/evidence"]
            if main is not hermetic_main:
                arguments += ["--pueue", "/pueue", "--pueued", "/pueued"]
            with self.subTest(main=main.__module__), forbid_program_content(), \
                    patch("sys.argv", arguments), patch("sys.stderr"):
                with self.assertRaises(SystemExit) as raised:
                    main()
                self.assertEqual(raised.exception.code, 2)
