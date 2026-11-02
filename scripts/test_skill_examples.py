"""Execute the shipped parallel-dispatch example with controlled responses."""

import itertools
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


REPO = Path(__file__).resolve().parent.parent


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
                    with tempfile.TemporaryDirectory(prefix="delegate-single-example-") as temp:
                        root = Path(temp)
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
                        result = subprocess.run(
                            [*shell, "-c", example], cwd=workspace, env=env, text=True,
                            capture_output=True, timeout=10, check=False,
                        )
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
        with tempfile.TemporaryDirectory(prefix="delegate-skill-example-") as temp:
            root = Path(temp)
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
            result = subprocess.run(
                [*shell, "-c", example], env=env, text=True,
                capture_output=True, timeout=10, check=False,
            )
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


if __name__ == "__main__":
    unittest.main()
