"""Collect Python import, function and finite call facts without module evaluation.

Effects retain the existing builtin observations and a finite set of qualified
pathlib.Path / datetime calls. Import aliases are conservative module-wide
candidates: conditional imports, later rebinding and local shadowing can cause
over-reporting. Assignment aliases, wildcard imports, arbitrary return values,
Path variables and dynamic getattr calls are not resolved. Absence of an effect
is not a proof of semantic purity; policy.Check alone interprets these facts.
"""

import ast
import json
from pathlib import Path
import sys


PATH_CLASS_EFFECTS = frozenset({"pathlib.Path.home", "pathlib.Path.cwd"})
CLOCK_EFFECTS = frozenset({
    "datetime.datetime.now", "datetime.datetime.utcnow", "datetime.datetime.today",
    "datetime.date.today",
})
PATH_METHOD_EFFECTS = frozenset({
    "home", "cwd", "resolve", "absolute", "expanduser", "open",
    "read_text", "read_bytes", "write_text", "write_bytes",
    "exists", "stat", "lstat", "iterdir", "glob", "rglob",
    "is_dir", "is_file", "is_symlink", "is_mount", "is_socket", "is_fifo",
    "is_block_device", "is_char_device", "readlink", "owner", "group",
    "mkdir", "rmdir", "unlink", "touch", "rename", "replace", "chmod", "lchmod",
    "symlink_to", "hardlink_to", "link_to",
})
PATH_RESULT_METHODS = frozenset({
    "home", "cwd", "resolve", "absolute", "expanduser",
    "joinpath", "with_name", "with_suffix", "with_stem", "relative_to",
})


def import_aliases(tree):
    """Keep every explicit import candidate; do not guess Python scope binding."""
    aliases = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for item in node.names:
                local = item.asname or item.name.split(".")[0]
                target = item.name if item.asname else local
                aliases.setdefault(local, set()).add(target)
        elif isinstance(node, ast.ImportFrom) and not node.level and node.module:
            for item in node.names:
                if item.name != "*":
                    aliases.setdefault(item.asname or item.name, set()).add(node.module + "." + item.name)
    return aliases


def qualified_names(node, aliases):
    if isinstance(node, ast.Name):
        return aliases.get(node.id, set())
    if isinstance(node, ast.Attribute):
        return {name + "." + node.attr for name in qualified_names(node.value, aliases)}
    return set()


def path_expression(node, aliases):
    """Recognize only explicit Path construction and known Path-returning chains."""
    if isinstance(node, ast.Call):
        targets = qualified_names(node.func, aliases)
        if "pathlib.Path" in targets or targets & PATH_CLASS_EFFECTS:
            return True
        return (isinstance(node.func, ast.Attribute)
                and node.func.attr in PATH_RESULT_METHODS
                and path_expression(node.func.value, aliases))
    return (isinstance(node, ast.Attribute) and node.attr == "parent"
            and path_expression(node.value, aliases))


def call_effect_names(node, aliases):
    effects = qualified_names(node.func, aliases) & (PATH_CLASS_EFFECTS | CLOCK_EFFECTS)
    if (isinstance(node.func, ast.Attribute) and node.func.attr in PATH_METHOD_EFFECTS
            and path_expression(node.func.value, aliases)):
        effects.add("pathlib.Path." + node.func.attr)
    if (isinstance(node.func, ast.Name)
            and node.func.id in {"open", "input", "print", "exec", "eval", "__import__"}):
        effects.add(node.func.id)
    return sorted(effects)


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
    aliases = import_aliases(tree)
    imports = []
    functions = []
    effects = []
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
        elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            functions.append({"name": node.name, "line": node.lineno,
                              "length": node.end_lineno - node.lineno + 1})
        elif isinstance(node, ast.Call):
            effects.extend({"name": name, "line": node.lineno}
                           for name in call_effect_names(node, aliases))
    return {"path": path, "module": "py:" + name, "imports": imports,
            "functions": functions, "effects": effects}


def source_facts(path, known):
    return parse_source_facts(path, Path(path).read_text(encoding="utf-8"), known)


def main():
    paths = json.load(sys.stdin)
    if not isinstance(paths, list) or not all(isinstance(path, str) for path in paths):
        raise ValueError("expected a list of source paths")
    known = {module_name(path) for path in paths}
    json.dump([source_facts(path, known) for path in paths], sys.stdout)


if __name__ == "__main__":
    main()
