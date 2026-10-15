"""Execute the shipped parallel-dispatch example with controlled responses."""

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


REPO = Path(__file__).resolve().parent.parent


class CollaborationExampleTests(unittest.TestCase):
    def test_parallel_capture_preserves_each_failure_and_success(self):
        source = (REPO / "skills/agent-integration/references/collaboration.md").read_text(
            encoding="utf-8"
        )
        example = source.split("```sh\n", 1)[1].split("```", 1)[0]
        for shell in (("/bin/sh",), ("/bin/bash", "-e")):
            for exits in ((2, 4), (0, 2), (2, 0), (0, 0)):
                with self.subTest(shell=shell, exits=exits):
                    self.run_example(example, shell, exits)

    def run_example(self, example, shell, exits):
        with tempfile.TemporaryDirectory(prefix="delegate-skill-example-") as temp:
            root = Path(temp)
            stub = root / "delegate"
            stub.write_text(
                "#!/bin/sh\n"
                "while [ \"$1\" != --provider ]; do shift; done\n"
                "provider=$2\n"
                "case $provider in\n"
                "  provider-a) code=$EXIT_A ;;\n"
                "  provider-b) code=$EXIT_B ;;\n"
                "  *) exit 99 ;;\n"
                "esac\n"
                "printf '{\"provider\":\"%s\"}\\n' \"$provider\"\n"
                "printf 'diagnostic:%s\\n' \"$provider\" >&2\n"
                "exit \"$code\"\n",
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


if __name__ == "__main__":
    unittest.main()
