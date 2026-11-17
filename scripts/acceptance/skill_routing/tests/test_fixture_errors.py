"""Keep catalog configuration refusals distinct from child-boundary failures."""

import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from scripts.acceptance.skill_routing import delegate_fixture


class FixtureErrorOwnershipTests(unittest.TestCase):
    def test_child_value_error_retains_its_owner_instead_of_claiming_catalog_isolation(self):
        with tempfile.TemporaryDirectory(prefix="delegate-fixture-error-") as temporary:
            root = Path(temporary)
            (root / "runtime.json").write_text(json.dumps({"delegate": "/candidate/delegate", "path": "/fixture/tools"}))
            stderr = io.StringIO()
            with patch.dict(delegate_fixture.os.environ, {"DELEGATE_ROUTING_FIXTURE": str(root)}), \
                    patch.object(delegate_fixture.sys, "argv", ["delegate", "providers", "--json"]), \
                    patch.object(delegate_fixture, "run_bounded_command", side_effect=ValueError("child capture rejected")), \
                    contextlib.redirect_stderr(stderr), self.assertRaisesRegex(ValueError, "child capture rejected"):
                delegate_fixture.main()
            self.assertNotIn("catalog isolation", stderr.getvalue())
