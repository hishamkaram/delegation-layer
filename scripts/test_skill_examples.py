"""Execute shipped dispatch examples with owned finite processes."""

from contextlib import contextmanager
import itertools
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance.directories.operations import DirectoryOwner
from scripts.acceptance.processes.operations import Processes


REPO = Path(__file__).resolve().parent.parent
EXAMPLE_TIMEOUT_SECONDS = 10
EXAMPLE_DRAIN_SECONDS = 10


@contextmanager
def example_fixture(prefix):
    owner = DirectoryOwner()
    root = Path(tempfile.mkdtemp(prefix=prefix)).resolve(strict=True)
    owner.adopt_created("skill example", root, root.parent)
    processes = None
    try:
        processes = Processes(root / "captures", owner=owner, environment_keys_only=True)
        yield root, processes
    finally:
        try:
            if processes is not None and (
                processes.drain(timeout=EXAMPLE_DRAIN_SECONDS) or not processes.finished_naturally()
            ):
                raise RuntimeError("example shell closure unknown; retained fixture: " + str(root))
        except BaseException:
            owner.release()
            raise
        owner.rollback()


def run_example_script(processes, argv, cwd, environment, *, timeout=EXAMPLE_TIMEOUT_SECONDS):
    child = processes.start("example", argv, cwd, environment=environment)
    code = child.wait(timeout=timeout, expected=None)
    if not isinstance(code, int):
        raise AssertionError("example process must return its observed exit code")
    return subprocess.CompletedProcess(
        argv, code, child.output().decode("utf-8"),
        child.stderr_path.read_text(encoding="utf-8"),
    )


class CollaborationExampleTests(unittest.TestCase):
    def test_read_only_capture_keeps_created_absent_and_uncertain_responses(self):
        source = (REPO / "skills/agent-integration/examples/read-only-task.md").read_text(
            encoding="utf-8"
        )
        example = source.split("```sh\n", 1)[1].split("```", 1)[0]
        for shell, use_alias, ambient_root in itertools.product(
            (("/bin/sh",), ("/bin/bash", "-e")), (False, True), (False, True)
        ):
            for record, code in (("created", 0), ("created", 1),
                                 ("not_created", 2), ("unknown", 1)):
                with self.subTest(shell=shell, alias=use_alias, ambient=ambient_root, record=record, code=code):
                    with example_fixture("delegate-single-example-") as (root, processes):
                        workspace = root / "workspace"
                        workspace.mkdir()
                        if use_alias:
                            alias = root / "workspace-alias"
                            alias.symlink_to(workspace, target_is_directory=True)
                            workspace = alias
                        response = {"task_id": "a" * 32, "task_record": record}
                        stub = root / "delegate"
                        stub.write_text(
                            '#!/bin/sh\n'
                            'printf "called\\n" >>"$EXAMPLE_CALLS"\n'
                            'printf "%s\\n" "$@" >"$EXAMPLE_ARGV"\n'
                            'printf "%s\\n" "$EXAMPLE_RESPONSE"\n'
                            'exit "$EXAMPLE_EXIT"\n',
                            encoding="utf-8",
                        )
                        stub.chmod(0o700)
                        env = dict(os.environ, PATH=str(root) + os.pathsep + os.defpath)
                        env.pop("DELEGATE_ROOT", None)
                        if ambient_root:
                            env["DELEGATE_ROOT"] = str(root / "unrelated-ambient-state")
                        env.update(
                            PWD=str(workspace), TMPDIR=str(root),
                            EXAMPLE_CALLS=str(root / "calls"), EXAMPLE_ARGV=str(root / "argv"),
                            EXAMPLE_RESPONSE=json.dumps(response), EXAMPLE_EXIT=str(code),
                        )
                        result = run_example_script(processes, [*shell, "-c", example], workspace, env)
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertEqual((root / "calls").read_text(), "called\n")
                        argv = (root / "argv").read_text().splitlines()
                        self.assertEqual(argv[:2], ["dispatch", "--auto"])
                        self.assertFalse(any(value == "--root" or value.startswith("--root=") for value in argv))
                        self.assertFalse((root / "unrelated-ambient-state").exists())
                        emitted_workspace = Path(argv[argv.index("--cwd") + 1])
                        self.assertTrue(emitted_workspace.is_absolute())
                        self.assertEqual(emitted_workspace.resolve(), workspace.resolve())
                        self.assertEqual(argv[argv.index("--permission") + 1], "read-only")
                        brief = Path(argv[argv.index("--brief") + 1])
                        self.assertTrue(brief.is_file())
                        saved = Path(result.stdout.splitlines()[0].removeprefix("Saved response: "))
                        self.assertEqual(json.loads(saved.read_text()), response)
                        self.assertIn(f"Dispatch exit: {code}; saved response: {saved}", result.stdout)

    def test_parallel_capture_preserves_each_failure_and_success(self):
        source = (REPO / "skills/agent-integration/references/collaboration.md").read_text(
            encoding="utf-8"
        )
        example = source.split("```sh\n", 1)[1].split("```", 1)[0]
        for shell, ambient_root in itertools.product((("/bin/sh",), ("/bin/bash", "-e")), (False, True)):
            for exits in ((2, 4), (0, 2), (2, 0), (0, 0)):
                with self.subTest(shell=shell, exits=exits, ambient=ambient_root):
                    self.run_example(example, shell, exits, ambient_root=ambient_root)

    def run_example(self, example, shell, exits, *, ambient_root):
        with example_fixture("delegate-skill-example-") as (root, processes):
            stub = root / "delegate"
            stub.write_text(
                "#!" + sys.executable + "\n"
                "import json,os,sys\nfrom pathlib import Path\n"
                "argv=sys.argv[1:]\nprovider=argv[argv.index('--provider')+1]\n"
                "code={'provider-a':os.environ['EXIT_A'],'provider-b':os.environ['EXIT_B']}[provider]\n"
                "(Path(os.environ['EXAMPLE_TEMP'])/('argv.'+provider)).write_text(json.dumps(argv))\n"
                "print(json.dumps({'provider':provider}))\n"
                "print('diagnostic:'+provider,file=sys.stderr)\nsys.exit(int(code))\n",
                encoding="utf-8",
            )
            stub.chmod(0o700)
            # An explicit template keeps BSD/GNU mktemp inside this test's owner.
            temporary = root / "mktemp"
            temporary.write_text(
                '#!/bin/sh\n[ "$#" = 1 ] && [ "$1" = -d ] || exit 99\n'
                'exec /usr/bin/mktemp -d "$EXAMPLE_TEMP/receipts.XXXXXXXX"\n',
                encoding="utf-8",
            )
            temporary.chmod(0o700)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.defpath)
            env.pop("DELEGATE_ROOT", None)
            if ambient_root:
                env["DELEGATE_ROOT"] = str(root / "unrelated-ambient-state")
            env.update(
                PROVIDER_A="provider-a", PROVIDER_B="provider-b",
                BRIEF_A=str(root / "a.md"), BRIEF_B=str(root / "b.md"),
                WORKSPACE_A=str(root), WORKSPACE_B=str(root),
                EXIT_A=str(exits[0]), EXIT_B=str(exits[1]), EXAMPLE_TEMP=str(root),
            )
            result = run_example_script(processes, [*shell, "-c", example], root, env)
            self.assertEqual(result.returncode, 0, result.stderr)
            receipts = list(root.glob("receipts.*"))
            self.assertEqual(len(receipts), 1, result.stdout)
            for label, code in zip(("a", "b"), exits):
                argv = json.loads((root / ("argv.provider-" + label)).read_text())
                self.assertEqual(argv[0], "dispatch")
                self.assertFalse(any(value == "--root" or value.startswith("--root=") for value in argv))
                prefix = receipts[0] / ("review-" + label)
                self.assertEqual(
                    json.loads(prefix.with_suffix(".json").read_text()),
                    {"provider": "provider-" + label},
                )
                self.assertEqual(prefix.with_suffix(".exit").read_text(), f"{code}\n")
                self.assertEqual(
                    prefix.with_suffix(".stderr").read_text(),
                    f"diagnostic:provider-{label}\n",
                )
            self.assertFalse((root / "unrelated-ambient-state").exists())

    def test_timeout_preserves_failure_and_joins_writer_before_removal(self):
        owner = DirectoryOwner()
        rollback = owner.rollback
        drain = Processes.drain
        registry = None
        release = None
        marker = None
        removed_after_join = []

        def release_and_drain(processes, *args, **kwargs):
            if release is None:
                raise AssertionError("regression fixture release path is missing")
            release.touch()
            return drain(processes, *args, **kwargs)

        def observe_removal():
            if registry is None or marker is None:
                raise AssertionError("regression fixture process was not initialized")
            removed_after_join.append(registry.finished_naturally() and marker.is_file())
            # Safely join even the intentionally broken baseline before allowing
            # actual deletion. The assertion below still detects its bad ordering.
            if registry.drain(timeout=10):
                owner.release()
                raise RuntimeError("regression writer closure unknown; retained fixture")
            return rollback()

        with (patch(f"{__name__}.DirectoryOwner", return_value=owner),
              patch.object(owner, "rollback", side_effect=observe_removal),
              patch.object(Processes, "drain", release_and_drain),
              self.assertRaisesRegex(RuntimeError, "process observation expired")):
            with example_fixture("delegate-delayed-example-") as (root, registry):
                release, marker = root / "release", root / "writer-finished"
                stub = root / "delayed.py"
                stub.write_text(
                    "from pathlib import Path\nimport sys,time\n"
                    "release,marker=map(Path,sys.argv[1:])\n"
                    "deadline=time.monotonic()+5\n"
                    "while not release.exists():\n"
                    "    if time.monotonic()>=deadline: sys.exit(2)\n"
                    "    time.sleep(0.005)\n"
                    "marker.write_text('writer finished')\n",
                    encoding="utf-8",
                )
                run_example_script(
                    registry, [str(Path(sys.executable).resolve(strict=True)), str(stub),
                               str(release), str(marker)], root, dict(os.environ), timeout=0,
                )
        self.assertEqual(removed_after_join, [True])
        self.assertFalse(root.exists())


if __name__ == "__main__":
    unittest.main()
