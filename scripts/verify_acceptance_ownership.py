"""Reject acceptance modules that bypass identity-pinned directory ownership."""

from dataclasses import dataclass
import ast
from pathlib import Path
import sys


SCRIPT_DIRECTORY = Path(__file__).resolve().parent
SHARED_COMMON = (SCRIPT_DIRECTORY / "acceptance_supervisor_common.py").resolve()
COMMON_MODULE = "acceptance_supervisor_common"
PROCESS_LIFECYCLE_TYPES = frozenset({"Process", "Processes", "OwnedProcess", "ProcessBook"})
PROCESS_FACTORY_MODULE = "functools"
PROCESS_FACTORY_NAME = "partial"
SUBPROCESS_LIFECYCLE_APIS = frozenset({"Popen"})

# The private-CLI acceptance driver is the documented narrow exception for a
# directly controlled fixture subprocess. Provider/native harnesses have no
# exception and must use the shared process owner.
DIRECT_FIXTURE_SUBPROCESS_FILES = frozenset({
    (SCRIPT_DIRECTORY / "acceptance_private_cli.py").resolve(),
})

DIRECTORY_REMOVAL_REFERENCES = frozenset({
    ("shutil", "rmtree"),
    ("shutil", "removedirs"),
    ("os", "rmdir"),
})
SUBPROCESS_APIS = frozenset({
    "Popen",
    "call",
    "check_call",
    "check_output",
    "run",
})
CONTROLLED_MODULES = frozenset({
    "os",
    "shutil",
    "subprocess",
    PROCESS_FACTORY_MODULE,
    COMMON_MODULE,
})
CONTROLLED_COMMON_REFERENCES = frozenset({
    "DirectoryOwner",
    "Processes",
    "remove_owned_directory",
})
CONTROLLED_DIRECT_NAMES = frozenset({
    "rmdir",
    "rmtree",
    "removedirs",
    *CONTROLLED_COMMON_REFERENCES,
})


@dataclass(frozen=True)
class Violation:
    path: Path
    line: int
    column: int
    kind: str
    message: str

    def __str__(self) -> str:
        return f"{self.path}:{self.line}:{self.column}: {self.kind}: {self.message}"


def production_paths() -> tuple[Path, ...]:
    return tuple(sorted(SCRIPT_DIRECTORY.glob("acceptance_*.py")))


def _is_shared_common(path: Path) -> bool:
    return path.resolve() == SHARED_COMMON


def _name(node: ast.AST) -> str | None:
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        return node.attr
    return None


def _dotted_name(node: ast.AST) -> str | None:
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        parent = _dotted_name(node.value)
        if parent is not None:
            return f"{parent}.{node.attr}"
    return None


ControlledReference = tuple[str, str | None]
ControlledAliases = dict[str, set[ControlledReference]]


def _add_alias(
        aliases: ControlledAliases, name: str, reference: ControlledReference) -> None:
    aliases.setdefault(name, set()).add(reference)


def _import_aliases(tree: ast.AST) -> ControlledAliases:
    """Resolve one bounded set of import aliases used by the ownership gate."""

    aliases: ControlledAliases = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for imported in node.names:
                if imported.name not in CONTROLLED_MODULES:
                    continue
                bound_name = imported.asname or imported.name
                _add_alias(aliases, bound_name, (imported.name, None))
        elif isinstance(node, ast.ImportFrom) and node.module in CONTROLLED_MODULES:
            for imported in node.names:
                if imported.name in CONTROLLED_COMMON_REFERENCES or (
                        node.module in {"os", "shutil"} and
                        imported.name in {"rmdir", "rmtree", "removedirs"}
                ) or (node.module == "subprocess" and imported.name in SUBPROCESS_APIS):
                    bound_name = imported.asname or imported.name
                    _add_alias(aliases, bound_name, (node.module, imported.name))
                elif node.module == PROCESS_FACTORY_MODULE and imported.name == PROCESS_FACTORY_NAME:
                    bound_name = imported.asname or imported.name
                    _add_alias(aliases, bound_name, (node.module, imported.name))
    return aliases


def _references(
        node: ast.AST,
        aliases: ControlledAliases,
) -> frozenset[ControlledReference]:
    """Return conservative controlled references without evaluating code."""

    if isinstance(node, ast.Name):
        references = aliases.get(node.id)
        if references:
            return frozenset(references)
        if node.id in CONTROLLED_DIRECT_NAMES:
            if node.id in CONTROLLED_COMMON_REFERENCES:
                return frozenset({(COMMON_MODULE, node.id)})
            if node.id == "rmdir":
                return frozenset({("os", node.id)})
            return frozenset({("shutil", node.id)})
        return frozenset()
    if isinstance(node, ast.Attribute):
        references = {
            (module, node.attr)
            for module, symbol in _references(node.value, aliases)
            if symbol is None
        }
        dotted = _dotted_name(node)
        if dotted is not None:
            module, separator, name = dotted.rpartition(".")
            if separator and module in CONTROLLED_MODULES:
                references.add((module, name))
        return frozenset(references)
    return frozenset()


def _collect_controlled_aliases(tree: ast.AST, aliases: ControlledAliases) -> None:
    """Propagate a conservative may-alias set through simple name assignments."""

    changed = True
    while changed:
        changed = False
        for node in ast.walk(tree):
            if not isinstance(node, (ast.Assign, ast.AnnAssign)):
                continue
            targets = node.targets if isinstance(node, ast.Assign) else (node.target,)
            references = _references(node.value, aliases)
            if not references:
                continue
            for target in targets:
                if not isinstance(target, ast.Name):
                    continue
                known = aliases.setdefault(target.id, set())
                additions = set(references) - known
                if additions:
                    known.update(additions)
                    changed = True


def _is_common_reference(
        node: ast.AST, symbol: str, aliases: ControlledAliases) -> bool:
    return (COMMON_MODULE, symbol) in _references(node, aliases)


def _is_process_reference(node: ast.AST, aliases: ControlledAliases) -> bool:
    return (COMMON_MODULE, "Processes") in _references(node, aliases)


def _is_partial_reference(node: ast.AST, aliases: ControlledAliases) -> bool:
    return (PROCESS_FACTORY_MODULE, PROCESS_FACTORY_NAME) in _references(node, aliases)


def _owner_receiver(
        node: ast.AST,
        owner_names: set[str],
        owner_type_names: set[str],
        aliases: ControlledAliases,
) -> bool:
    if _is_common_reference(node, "DirectoryOwner", aliases):
        return True
    value = _name(node)
    if value is None:
        return False
    return value in owner_names or value in owner_type_names or value == "owner" or \
        value.endswith("_owner") or value.endswith("_ownership")


def _owned_directory_constructor(
        node: ast.AST, aliases: ControlledAliases) -> bool:
    if not isinstance(node, ast.Call):
        return False
    return _is_common_reference(node.func, "DirectoryOwner", aliases)


def _assignment_name(target: ast.AST) -> str | None:
    if isinstance(target, ast.Name):
        return target.id
    if isinstance(target, ast.Attribute):
        return target.attr
    return None


def _violation(path: Path, node: ast.AST, kind: str, message: str) -> Violation:
    return Violation(path, getattr(node, "lineno", 0),
                     getattr(node, "col_offset", 0) + 1, kind, message)


def _is_directory_removal(node: ast.Call, aliases: ControlledAliases) -> bool:
    if _references(node.func, aliases) & DIRECTORY_REMOVAL_REFERENCES:
        return True
    return isinstance(node.func, ast.Attribute) and node.func.attr == "rmdir"


def _argv_argument(node: ast.Call) -> ast.AST | None:
    if node.args:
        return node.args[0]
    for keyword in node.keywords:
        if keyword.arg == "args":
            return keyword.value
    return None


def _literal_recursive_rm(node: ast.Call, aliases: ControlledAliases) -> bool:
    references = _references(node.func, aliases)
    if not any(module == "subprocess" and symbol in SUBPROCESS_APIS
               for module, symbol in references):
        return False
    argv = _argv_argument(node)
    if not isinstance(argv, (ast.List, ast.Tuple)) or not argv.elts:
        return False
    command = argv.elts[0]
    if not isinstance(command, ast.Constant) or not isinstance(command.value, str):
        return False
    if command.value.rsplit("/", 1)[-1] != "rm":
        return False
    for element in argv.elts[1:]:
        if not isinstance(element, ast.Constant) or not isinstance(element.value, str):
            continue
        option = element.value
        if option == "--":
            break
        if option in {"--recursive", "-r", "-R"}:
            return True
        if option.startswith("-") and not option.startswith("--") and "r" in option[1:]:
            return True
    return False


def find_violations(paths: tuple[Path, ...] | list[Path]) -> tuple[Violation, ...]:
    violations: list[Violation] = []
    for candidate in paths:
        path = Path(candidate).resolve()
        if _is_shared_common(path):
            continue
        try:
            tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        except (OSError, SyntaxError) as error:
            violations.append(Violation(path, 0, 0, "source", str(error)))
            continue

        aliases = _import_aliases(tree)
        _collect_controlled_aliases(tree, aliases)
        for node in ast.walk(tree):
            if isinstance(node, ast.ClassDef) and node.name in PROCESS_LIFECYCLE_TYPES:
                violations.append(_violation(
                    path, node, "duplicate-process-owner",
                    "process lifecycle classes must be defined in the shared supervisor common module"))
        owner_type_names = {
            name for name, references in aliases.items()
            if (COMMON_MODULE, "DirectoryOwner") in references
        }
        owner_type_names.add("DirectoryOwner")
        owner_names: set[str] = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.Assign) and _owned_directory_constructor(node.value, aliases):
                for target in node.targets:
                    name = _assignment_name(target)
                    if name is not None:
                        owner_names.add(name)
            elif isinstance(node, ast.AnnAssign) and _owned_directory_constructor(node.value, aliases):
                name = _assignment_name(node.target)
                if name is not None:
                    owner_names.add(name)

        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            process_constructor = _is_process_reference(node.func, aliases)
            if (_is_partial_reference(node.func, aliases) and node.args and
                    _is_process_reference(node.args[0], aliases)):
                keywords = {keyword.arg for keyword in node.keywords}
                if ("owner" in keywords) == ("capture_root" in keywords):
                    violations.append(_violation(
                        path, node, "process-capture-owner",
                        "partial Processes factories require exactly one owner or capture_root binding"))
            references = _references(node.func, aliases)
            if (any(module == "subprocess" and symbol in SUBPROCESS_LIFECYCLE_APIS
                    for module, symbol in references) and
                    path not in DIRECT_FIXTURE_SUBPROCESS_FILES):
                violations.append(_violation(
                    path, node, "direct-process-owner",
                    "acceptance process lifecycle must use the shared Process/Processes owner"))
            if _is_directory_removal(node, aliases):
                violations.append(_violation(
                    path, node, "recursive-removal",
                    "acceptance code must remove disposable directories through the shared owner"))
            if _literal_recursive_rm(node, aliases):
                violations.append(_violation(
                    path, node, "subprocess-removal",
                    "literal recursive rm subprocess calls are forbidden; use the shared owner"))
            if (isinstance(node.func, ast.Attribute) and node.func.attr == "register" and
                    _owner_receiver(node.func.value, owner_names, owner_type_names, aliases)):
                violations.append(_violation(
                    path, node, "legacy-register",
                    "DirectoryOwner.register is forbidden; use create or adopt_created"))
            if _is_common_reference(node.func, "remove_owned_directory", aliases) and (
                    len(node.args) != 1 or any(keyword.arg in {"directory", "parent"}
                                               for keyword in node.keywords)):
                violations.append(_violation(
                    path, node, "path-only-removal",
                    "remove_owned_directory requires one identity token"))
            if process_constructor:
                keywords = {keyword.arg for keyword in node.keywords}
                if ("owner" in keywords) == ("capture_root" in keywords):
                    violations.append(_violation(
                        path, node, "process-capture-owner",
                        "Processes requires exactly one caller-owned owner or capture_root"))
            for keyword in node.keywords:
                if keyword.arg in {
                    "raw_process_captures", "raw_process_captures_owned",
                    "raw_process_captures_path", "raw_process_captures_directory",
                }:
                    violations.append(_violation(
                        path, keyword, "raw-capture-authority",
                        "raw process capture cleanup requires raw_process_captures_token"))

        for node in ast.walk(tree):
            if isinstance(node, (ast.Name, ast.Attribute)):
                name = _name(node)
                if name in {
                    "raw_process_captures", "raw_process_captures_owned",
                    "raw_process_captures_path", "raw_process_captures_directory",
                }:
                    violations.append(_violation(
                        path, node, "raw-capture-authority",
                        "raw process capture cleanup requires an identity token"))

    return tuple(sorted(violations, key=lambda item: (str(item.path), item.line,
                                                       item.column, item.kind)))


def main(argv: list[str] | None = None) -> int:
    paths = tuple(Path(value) for value in argv) if argv else production_paths()
    violations = find_violations(paths)
    for violation in violations:
        print(violation, file=sys.stderr)
    return 1 if violations else 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
