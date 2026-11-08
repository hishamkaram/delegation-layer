"""Standard unittest discovery collects the current behavioral suites once."""

from collections import Counter
import inspect
from pathlib import Path
import unittest


def test_cases(suite):
    for item in suite:
        if isinstance(item, unittest.TestSuite):
            yield from test_cases(item)
        elif isinstance(item, unittest.TestCase):
            yield item
        else:
            raise TypeError(f"unexpected discovery result: {type(item).__name__}")


def case_identity(case):
    # Normalize module aliases by their actual source without freezing test IDs.
    source = inspect.getsourcefile(type(case))
    if source is None:
        raise ValueError(f"test case has no source: {case.id()}")
    return Path(source).resolve(), type(case).__qualname__, case.id().rsplit(".", 1)[-1]


class TestDiscovery(unittest.TestCase):
    def test_standard_discovery_collects_current_acceptance_cases_once(self):
        acceptance = (Path(__file__).resolve().parents[4] / 'scripts/acceptance')
        scripts = acceptance.parent
        loader = unittest.TestLoader()
        suite = loader.discover(str(scripts), pattern="test_*.py", top_level_dir=str(scripts))
        self.assertEqual(loader.errors, [])
        collected = Counter(case_identity(case) for case in test_cases(suite))
        self.assertTrue(collected, "standard discovery collected no tests")
        self.assertEqual({key: count for key, count in collected.items() if count != 1}, {})

        acceptance_loader = unittest.TestLoader()
        acceptance_suite = acceptance_loader.discover(
            str(acceptance), pattern="test_*.py", top_level_dir=str(scripts))
        self.assertEqual(acceptance_loader.errors, [])
        expected = Counter(case_identity(case) for case in test_cases(acceptance_suite))
        self.assertTrue(expected, "acceptance discovery collected no tests")
        self.assertEqual({key: count for key, count in expected.items() if count != 1}, {})
        actual = Counter({key: count for key, count in collected.items()
                          if key[0].is_relative_to(acceptance)})
        self.assertEqual(actual, expected)


if __name__ == "__main__":
    unittest.main()
