"""Installation checks preserve pinned artifacts and isolate diagnostic tools."""

from contextlib import ExitStack
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock

from scripts import static_tools as tools


class ShellCheckInstallationTests(unittest.TestCase):
    def test_checksum_failure_precedes_archive_read_and_installation(self):
        archive = shellcheck_archive(b"approved executable")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "bin").mkdir()
            installed = root / "bin/shellcheck"
            installed.write_bytes(b"existing executable")
            with mock.patch.object(tools, "ROOT", root), mock.patch.object(
                    tools, "shellcheck_target", return_value=("fixture.tar.gz", digest(archive))), mock.patch.object(
                    tools, "checked", side_effect=download_fixture(b"corrupt archive")), mock.patch.object(
                    tools.tarfile, "open") as open_archive, mock.patch.object(tools.shutil, "copy2") as copy:
                with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                    tools.install_shellcheck()
            open_archive.assert_not_called()
            copy.assert_not_called()
            self.assertEqual(installed.read_bytes(), b"existing executable")

    def test_supported_archives_install_only_the_verified_executable(self):
        targets = (
            ("Linux", "amd64", "linux.x86_64", "x86_64"),
            ("Linux", "aarch64", "linux.aarch64", "aarch64"),
            ("Darwin", "x86_64", "darwin.x86_64", "x86_64"),
            ("Darwin", "arm64", "darwin.aarch64", "aarch64"),
        )
        for system, machine, suffix, architecture in targets:
            with self.subTest(system=system, machine=machine), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                (root / "bin").mkdir()
                executable = f"executable for {system}/{machine}".encode()
                archive = shellcheck_archive(executable)
                with mock.patch.object(tools, "ROOT", root), mock.patch.object(
                        tools.platform, "system", return_value=system), mock.patch.object(
                        tools.platform, "machine", return_value=machine), mock.patch.dict(
                        tools.SHELLCHECK_DIGESTS, {(system, architecture): digest(archive)}), mock.patch.object(
                        tools, "checked", side_effect=download_fixture(archive)) as download:
                    tools.install_shellcheck()
                installed = root / "bin/shellcheck"
                self.assertEqual(installed.read_bytes(), executable)
                self.assertEqual(installed.stat().st_mode & 0o777, 0o755)
                command = download.call_args.args[0]
                self.assertTrue(command[-1].endswith(f"/shellcheck-v0.11.0.{suffix}.tar.gz"))

    def test_missing_archive_member_is_an_actionable_failure(self):
        archive = shellcheck_archive(b"unrelated", member="unexpected-file")
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "bin").mkdir()
            with mock.patch.object(tools, "ROOT", root), mock.patch.object(
                    tools, "shellcheck_target", return_value=("fixture.tar.gz", digest(archive))), mock.patch.object(
                    tools, "checked", side_effect=download_fixture(archive)):
                with self.assertRaisesRegex(ValueError, "executable"):
                    tools.install_shellcheck()
            self.assertFalse((root / "bin/shellcheck").exists())

    def test_unsupported_host_does_not_download(self):
        with mock.patch.object(tools.platform, "system", return_value="Windows"), mock.patch.object(
                tools.platform, "machine", return_value="AMD64"), mock.patch.object(tools, "checked") as download:
            with self.assertRaisesRegex(ValueError, "unsupported ShellCheck host"):
                tools.install_shellcheck()
        download.assert_not_called()


class JavaScriptInstallationTests(unittest.TestCase):
    def test_matching_lock_and_installed_dependencies_verify(self):
        with JavaScriptInstallationFixture() as root, mock.patch.object(tools, "ROOT", root), mock.patch.object(
                tools, "require_banner") as banner:
            tools.node_tools(install=False)
        self.assertTrue(any(call.args[1] == "v1.2.3" for call in banner.call_args_list))

    def test_stale_or_missing_lock_is_rejected_before_invoking_node(self):
        for name in ("package.json", "package-lock.json"):
            for missing in (False, True):
                with self.subTest(name=name, missing=missing), JavaScriptInstallationFixture() as root:
                    installed = root / "bin/.lint-js" / name
                    if missing:
                        installed.unlink()
                    else:
                        installed.write_text('{"stale": true}')
                    with mock.patch.object(tools, "ROOT", root), mock.patch.object(tools, "require_banner") as banner:
                        with self.assertRaises((OSError, ValueError)):
                            tools.node_tools(install=False)
                    banner.assert_not_called()

    def test_missing_installed_dependency_is_rejected(self):
        with JavaScriptInstallationFixture() as root:
            (root / "bin/.lint-js/node_modules/globals/package.json").unlink()
            with mock.patch.object(tools, "ROOT", root), mock.patch.object(tools, "require_banner"):
                with self.assertRaises((OSError, ValueError)):
                    tools.node_tools(install=False)

    def test_stale_installed_dependency_is_rejected(self):
        with JavaScriptInstallationFixture() as root:
            package = root / "bin/.lint-js/node_modules/@eslint/js/package.json"
            package.write_text('{"name":"@eslint/js","version":"0.0.1"}')
            with mock.patch.object(tools, "ROOT", root), mock.patch.object(tools, "require_banner"):
                with self.assertRaisesRegex(ValueError, "dependency"):
                    tools.node_tools(install=False)

    def test_install_uses_copied_lock_without_package_scripts(self):
        with JavaScriptInstallationFixture() as root:
            source = root / "tools/lint"
            destination = root / "bin/.lint-js"
            (destination / "package-lock.json").write_text('{"stale": true}')

            def install(command):
                self.assertEqual(command[:2], ["npm", "ci"])
                self.assertIn("--ignore-scripts", command)
                self.assertEqual(Path(command[command.index("--prefix") + 1]), destination)
                for name in ("package.json", "package-lock.json"):
                    self.assertEqual((destination / name).read_bytes(), (source / name).read_bytes())

            with mock.patch.object(tools, "ROOT", root), mock.patch.object(
                    tools, "checked", side_effect=install) as npm, mock.patch.object(tools, "require_banner"):
                tools.node_tools(install=True)
            npm.assert_called_once()


    def test_node_install_keeps_transitive_go_source_out_of_package_inventory(self):
        with JavaScriptInstallationFixture() as root:
            module = "example.invalid/static-tools-fixture"
            (root / "go.mod").write_text(f"module {module}\n\ngo 1.27.0\n")
            (root / "fixture.go").write_text("package fixture\n")
            environment = dict(os.environ, GOWORK="off", GOTOOLCHAIN="local",
                               GOPROXY="off", GOSUMDB="off")
            before = go_packages(root, environment)
            self.assertEqual(before, [module])

            def install(command):
                destination = Path(command[command.index("--prefix") + 1])
                manifest = json.loads((destination / "package.json").read_text())
                for name, version in manifest["dependencies"].items():
                    metadata = destination / "node_modules" / name / "package.json"
                    metadata.parent.mkdir(parents=True, exist_ok=True)
                    metadata.write_text(json.dumps({"name": name, "version": version}))
                transitive = destination / "node_modules/flatted/golang"
                transitive.mkdir(parents=True)
                (transitive / "fixture.go").write_text("package flatted\n")

            with mock.patch.object(tools, "ROOT", root), mock.patch.object(
                    tools, "checked", side_effect=install), mock.patch.object(tools, "require_banner"):
                tools.node_tools(install=True)
            self.assertEqual(go_packages(root, environment), before)

            # Prove this observes Go's loader rather than an empty or filtered scan.
            visible = root / "bin/visible-node-tools/node_modules/flatted/golang"
            visible.mkdir(parents=True)
            (visible / "fixture.go").write_text("package flatted\n")
            expected = sorted([module, module + "/bin/visible-node-tools/node_modules/flatted/golang"])
            self.assertEqual(go_packages(root, environment), expected)


class InstallationBoundaryTests(unittest.TestCase):
    def test_diagnostic_install_never_touches_mandatory_tools(self):
        for action in ("install", "verify"):
            with self.subTest(action=action), tempfile.TemporaryDirectory() as directory, ExitStack() as patches:
                root = Path(directory)
                requirements = root / "tools/diagnostics/requirements.txt"
                requirements.parent.mkdir(parents=True)
                requirements.write_text("ty==0.0.1 \\\n --hash=sha256:fixture\nzizmor==1.2.3 \\\n --hash=sha256:fixture\n")
                protected = root / "bin/.lint-js/package.json"
                protected.parent.mkdir(parents=True)
                protected.write_bytes(b"mandatory tool state")
                patches.enter_context(mock.patch.object(tools, "ROOT", root))
                patches.enter_context(mock.patch.object(tools.sys, "argv", ["static_tools.py", action, "--diagnostic"]))
                commands = patches.enter_context(mock.patch.object(tools, "checked"))
                banner = patches.enter_context(mock.patch.object(tools, "require_banner"))
                ty_version = patches.enter_context(mock.patch.object(tools, "require_ty_version"))
                shellcheck = patches.enter_context(mock.patch.object(tools, "install_shellcheck"))
                node = patches.enter_context(mock.patch.object(tools, "node_tools"))
                self.assertEqual(tools.main(), 0)
                shellcheck.assert_not_called()
                node.assert_not_called()
                self.assertEqual(protected.read_bytes(), b"mandatory tool state")
                ty_version.assert_called_once_with(root / "bin/diagnostics/bin/ty", "0.0.1")
                banner.assert_called_once_with([str(root / "bin/diagnostics/bin/zizmor"), "--version"], "zizmor 1.2.3")
                for call in commands.call_args_list:
                    command = call.args[0]
                    self.assertNotIn(command[0], ("go", "node", "npm", "curl"))
                    self.assertIn(str(root / "bin/diagnostics"), " ".join(command))
                self.assertEqual(commands.call_count, 2 if action == "install" else 0)

    def test_failed_install_is_reported_and_stops_downstream_work(self):
        for diagnostic in (False, True):
            with self.subTest(diagnostic=diagnostic), tempfile.TemporaryDirectory() as directory, ExitStack() as patches:
                root = Path(directory)
                output = io.StringIO()
                arguments = ["static_tools.py", "install"] + (["--diagnostic"] if diagnostic else [])
                patches.enter_context(mock.patch.object(tools, "ROOT", root))
                patches.enter_context(mock.patch.object(tools.sys, "argv", arguments))
                patches.enter_context(mock.patch.object(tools.sys, "stderr", output))
                shellcheck = patches.enter_context(mock.patch.object(tools, "install_shellcheck"))
                python = patches.enter_context(mock.patch.object(tools, "python_tools"))
                node = patches.enter_context(mock.patch.object(tools, "node_tools"))
                failure = subprocess.CalledProcessError(23, "fixture-installer")
                if diagnostic:
                    python.side_effect = failure
                else:
                    shellcheck.side_effect = failure
                self.assertEqual(tools.main(), 1)
                self.assertIn("Static analysis tools install failed", output.getvalue())
                self.assertIn("fixture-installer", output.getvalue())
                node.assert_not_called()
                if diagnostic:
                    shellcheck.assert_not_called()
                else:
                    python.assert_not_called()



def go_packages(root: Path, environment: dict[str, str]) -> list[str]:
    result = subprocess.run(["go", "list", "./..."], cwd=root, env=environment,
                            capture_output=True, text=True, check=True, timeout=30)
    return sorted(result.stdout.splitlines())


def shellcheck_archive(executable: bytes, *, member: str = "shellcheck-v0.11.0/shellcheck") -> bytes:
    output = io.BytesIO()
    with tarfile.open(fileobj=output, mode="w:gz") as archive:
        info = tarfile.TarInfo(member)
        info.size = len(executable)
        info.mode = 0o755
        archive.addfile(info, io.BytesIO(executable))
    return output.getvalue()


def download_fixture(data: bytes):
    def download(command: list[str], *, environment=None) -> None:
        if command[0] != "curl":
            raise AssertionError("archive fixture received a non-download command")
        Path(command[command.index("--output") + 1]).write_bytes(data)
    return download


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


class JavaScriptInstallationFixture:
    def __enter__(self) -> Path:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        dependencies = {"eslint": "1.2.3", "@eslint/js": "2.3.4", "globals": "3.4.5"}
        manifest = {"private": True, "dependencies": dependencies}
        lock = {"lockfileVersion": 3, "packages": {"": {"dependencies": dependencies}}}
        for base in (self.root / "tools/lint", self.root / "bin/.lint-js"):
            base.mkdir(parents=True)
            (base / "package.json").write_text(json.dumps(manifest))
            (base / "package-lock.json").write_text(json.dumps(lock))
        for name, version in dependencies.items():
            package = self.root / "bin/.lint-js/node_modules" / name / "package.json"
            package.parent.mkdir(parents=True)
            package.write_text(json.dumps({"name": name, "version": version}))
        return self.root

    def __exit__(self, exception_type, exception, traceback):
        self.temporary.cleanup()


if __name__ == "__main__":
    unittest.main()
