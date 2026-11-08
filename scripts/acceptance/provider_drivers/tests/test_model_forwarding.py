"""The convenience wrapper preserves explicit models and forwards required aliases."""

import os
from pathlib import Path
import tempfile
import unittest

from scripts.acceptance.processes.operations import run_bounded_command


WRAPPER = Path(__file__).resolve().parents[4] / "scripts/acceptance/native/run.sh"


class AcceptanceModelForwardingTests(unittest.TestCase):
    def invoke(self, provider: str, model_key: str, *options: str) -> list[str]:
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            interpreter = directory / "python3"
            interpreter.write_text('#!/bin/sh\nprintf "%s\\n" "$@"\n')
            interpreter.chmod(0o700)
            env = dict(os.environ)
            env.update({"PATH": str(directory) + os.pathsep + env["PATH"],
                        model_key: "gateway/exact-alias"})
            result = run_bounded_command(
                ["/bin/sh", str(WRAPPER), provider, *options], env,
                max_output=4096, capture=True,
            )
            self.assertEqual(result.exit_code, 0)
            return result.output.decode().splitlines()

    def test_ccr_and_opencode_environment_models_are_forwarded(self):
        for provider, key in (("ccr:print", "CCR_LIVE_MODEL"),
                              ("opencode:run", "OPENCODE_LIVE_MODEL")):
            with self.subTest(provider=provider):
                argv = self.invoke(provider, key)
                self.assertEqual(argv[-2:], ["--model", "gateway/exact-alias"])

    def test_explicit_model_always_wins(self):
        for options in (("--model", "requested"), ("--model=requested",)):
            with self.subTest(options=options):
                argv = self.invoke("ccr:print", "CCR_LIVE_MODEL", *options)
                self.assertNotIn("gateway/exact-alias", argv)
                self.assertEqual(argv[-len(options):], list(options))


if __name__ == "__main__":
    unittest.main()
