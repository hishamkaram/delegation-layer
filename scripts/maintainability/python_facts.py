"""Read maintained Python source and emit facts from the shared pure parser."""

import json
from pathlib import Path
import sys


if __name__ == "__main__" and not __package__:
    sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from scripts.maintainability import python_ast


def source_facts(path, known):
    return python_ast.parse_source_facts(path, Path(path).read_text(encoding="utf-8"), known)


def main():
    paths = json.load(sys.stdin)
    if not isinstance(paths, list) or not all(isinstance(path, str) for path in paths):
        raise ValueError("expected a list of source paths")
    known = {python_ast.module_name(path) for path in paths}
    json.dump([source_facts(path, known) for path in paths], sys.stdout)


if __name__ == "__main__":
    main()
