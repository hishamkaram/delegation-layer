"""Resolve static Python imports without evaluating inspected source.

Release evidence fingerprints use these facts to follow maintained import
closures. Both ``scripts.package`` and ``package`` imports resolve to the same
repository module when that module is present in the inspected source tree.
"""

import ast


def module_name(path):
    relative = path.removeprefix("scripts/").removesuffix(".py")
    return relative.removesuffix("/__init__").replace("/", ".")


def imported_module(current, node, package_file):
    if not node.level:
        return node.module or ""
    anchor = current.split(".") if package_file else current.split(".")[:-1]
    parent = anchor[:len(anchor) - node.level + 1]
    if node.module:
        parent.append(node.module)
    return ".".join(parent)


def canonical_module(name, known):
    repository_name = name.removeprefix("scripts.")
    return repository_name if repository_name in known else name


def parse_source_facts(path, source, known):
    name = module_name(path)
    tree = ast.parse(source, filename=path)
    imports = []
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            imports.extend({"target": "py:" + canonical_module(item.name, known), "line": node.lineno}
                           for item in node.names)
        elif isinstance(node, ast.ImportFrom):
            base = imported_module(name, node, path.endswith("/__init__.py"))
            base = canonical_module(base, known)
            candidates = [canonical_module(base + "." + item.name, known) for item in node.names]
            targets = {candidate if candidate in known else base for candidate in candidates}
            imports.extend({"target": "py:" + target, "line": node.lineno}
                           for target in sorted(targets))
    return {"path": path, "module": "py:" + name, "imports": imports}
