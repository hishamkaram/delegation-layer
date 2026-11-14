"""Root identity readers preserve the explicit connector capability boundary."""

import json
from pathlib import Path
import tempfile
import unittest

from scripts.acceptance.evidence.records import _state_root_identity
from scripts.acceptance.evidence.validation import AcceptanceFailure


class RootRecordContractTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="root-record-contract-")
        self.addCleanup(directory.cleanup)
        self.state = Path(directory.name)
        self.record = {
            "schema_version": 1,
            "root_id": "0123456789abcdef0123456789abcdef",
            "created_at": "2026-10-13T00:00:00Z",
        }

    def read_with(self, extra):
        (self.state / "root.json").write_text(json.dumps({**self.record, **extra}), encoding="utf-8")
        return _state_root_identity(self.state, "fixture root")

    def test_legacy_and_known_capability_preserve_identity(self):
        expected = self.read_with({})
        for features in ([], ["namespace-admission-v1"], ["namespace-mutation-lease-v1"],
                         ["namespace-admission-v1", "namespace-mutation-lease-v1"],
                         ["namespace-admission-v1", "namespace-admission-v1"],
                         ["namespace-admission-v1", "namespace-mutation-lease-v1",
                          "workspace-constrained-connector-v1"],
                         ["workspace-constrained-connector-v1", "namespace-mutation-lease-v1",
                          "namespace-admission-v1", "workspace-constrained-connector-v1"],
                         ["namespace-admission-v1", "namespace-mutation-lease-v1",
                          "workspace-constrained-connector-v1", "connector-path-project-v1"]):
            with self.subTest(features=features):
                self.assertEqual(self.read_with({"required_features": features}), expected)

    def test_workspace_feature_requires_both_namespace_features(self):
        for features in (["workspace-constrained-connector-v1"],
                         ["namespace-admission-v1", "workspace-constrained-connector-v1"],
                         ["namespace-mutation-lease-v1", "workspace-constrained-connector-v1"]):
            with self.subTest(features=features):
                with self.assertRaisesRegex(AcceptanceFailure, "requires connector namespace features"):
                    self.read_with({"required_features": features})

    def test_path_feature_requires_namespace_and_workspace_features(self):
        dependencies = ["namespace-admission-v1", "namespace-mutation-lease-v1",
                        "workspace-constrained-connector-v1"]
        for missing in range(len(dependencies)):
            features = [feature for index, feature in enumerate(dependencies) if index != missing]
            features.append("connector-path-project-v1")
            with self.subTest(missing=dependencies[missing]):
                with self.assertRaisesRegex(AcceptanceFailure, "requires connector namespace"):
                    self.read_with({"required_features": features})

    def test_unknown_or_malformed_required_features_are_rejected(self):
        for features in (None, True, "namespace-admission-v1", [1], [None], ["future-v2"],
                         ["namespace-admission-v1", "future-v2"],
                         ["workspace-constrained-connector-v1", "future-v2"]):
            with self.subTest(features=features):
                with self.assertRaisesRegex(AcceptanceFailure, "unsupported required features"):
                    self.read_with({"required_features": features})

    def test_unknown_root_fields_remain_rejected(self):
        with self.assertRaisesRegex(AcceptanceFailure, "unknown or missing fields"):
            self.read_with({"required_features": ["namespace-admission-v1"], "unknown": True})
