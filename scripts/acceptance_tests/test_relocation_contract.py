"""Pin every remaining moved baseline test to one current discovery location."""

from collections import Counter
import json
from pathlib import Path
import unittest


HERE = Path(__file__).resolve().parent


class TestRelocationContract(unittest.TestCase):
    def test_baseline_ids_have_exactly_one_relocation(self):
        contract = json.loads((HERE / "baseline_relocated_test_ids.json").read_text())
        rows = self.mapping_rows()
        self.assertEqual(contract["baseline_commit"], "8025ef6376baf6683acf0f44137948d4180a4c89")
        self.assertEqual(len(contract["test_ids"]), 353)
        self.assertEqual(Counter(row["baseline_id"] for row in rows), Counter(contract["test_ids"]))
        self.assertEqual(len(rows), len({row["baseline_id"] for row in rows}))
        self.assertEqual(len(rows), len({row["current_id"] for row in rows}))

    def test_targets_resolve_once_in_mandatory_discovery_namespace(self):
        for row in self.mapping_rows():
            with self.subTest(baseline=row["baseline_id"]):
                loader = unittest.TestLoader()
                suite = loader.loadTestsFromName(row["current_id"])
                self.assertEqual(loader.errors, [])
                self.assertEqual(suite.countTestCases(), 1)

    def mapping_rows(self):
        rows = [json.loads(line) for line in (HERE / "test_id_mapping.jsonl").read_text().splitlines()]
        for row in rows:
            self.assertIsInstance(row, dict)
            self.assertEqual(set(row), {"baseline_id", "current_id"})
            self.assertIsInstance(row["baseline_id"], str)
            self.assertIsInstance(row["current_id"], str)
            self.assertEqual(row["baseline_id"].rsplit(".", 2)[1:], row["current_id"].rsplit(".", 2)[1:])
        return rows
