"""Reject acceptance modules that bypass identity-pinned directory ownership."""

from dataclasses import dataclass
import ast
from pathlib import Path
import sys


SCRIPT_DIRECTORY = Path(__file__).resolve().parent
SHARED_COMMON = (SCRIPT_DIRECTORY / "acceptance_supervisor_common.py").resolve()
COMMON_MODULE = "acceptance_supervisor_common"
COMMON_MODULES = frozenset({COMMON_MODULE, "scripts.acceptance_supervisor_common"})
PACKAGE_DIRECTORY = SCRIPT_DIRECTORY / "delegate_acceptance"
DIRECTORY_OWNER = (PACKAGE_DIRECTORY / "directories" / "operations.py").resolve()
DIRECTORY_JOURNAL = (PACKAGE_DIRECTORY / "directories" / "journal.py").resolve()
PROCESS_OWNER = (PACKAGE_DIRECTORY / "processes" / "operations.py").resolve()
DIRECTORY_REMOVAL_FILES = frozenset({DIRECTORY_OWNER, DIRECTORY_JOURNAL})
OWNER_MODULES = frozenset({
    "scripts.delegate_acceptance.directories.operations",
    "scripts.delegate_acceptance.processes.operations",
    "delegate_acceptance.directories.operations",
    "delegate_acceptance.processes.operations",
})
PROCESS_LIFECYCLE_TYPES = frozenset({"Process", "Processes", "OwnedProcess", "ProcessBook"})
DIRECTORY_LIFECYCLE_TYPES = frozenset({"DirectoryOwner", "OwnedDirectory"})
PROCESS_FACTORY_MODULE = "functools"
PROCESS_FACTORY_NAME = "partial"

# Only the identity-pinned fixture capture primitive may directly start its
# controlled subprocess. Acceptance entrypoints use the shared process owner.
DIRECT_FIXTURE_SUBPROCESS_FILES = frozenset({
    (PACKAGE_DIRECTORY / "private_cli" / "capture.py").resolve(),
})

# Release fixture tests outside the acceptance package whose children must also
# run through the shared process owner. Listed exactly; a missing file fails.
GOVERNED_TEST_FILES = ("test_homebrew_formula_render.py",)

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
    *COMMON_MODULES,
    *OWNER_MODULES,
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
    paths = set(SCRIPT_DIRECTORY.glob("acceptance_*.py"))
    paths.update((SCRIPT_DIRECTORY / "delegate_acceptance").rglob("*.py"))
    paths.update(SCRIPT_DIRECTORY / name for name in GOVERNED_TEST_FILES)
    return tuple(sorted(paths))


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


def _controlled_reference(module: str, symbol: str) -> ControlledReference:
    return (COMMON_MODULE if module in OWNER_MODULES | COMMON_MODULES else module, symbol)


def _owner_module_prefix(module: str) -> bool:
    return any(owner == module or owner.startswith(module + ".")
               for owner in OWNER_MODULES | COMMON_MODULES)


def _absolute_import_module(node: ast.ImportFrom, path: Path | None) -> str | None:
    if not node.level:
        return node.module
    if path is None:
        return None
    try:
        relative = path.relative_to(SCRIPT_DIRECTORY)
    except ValueError:
        return None
    package = ("scripts", *relative.parent.parts)
    if node.level > len(package):
        return None
    parent = package[:len(package) - node.level + 1]
    return ".".join((*parent, node.module)) if node.module else ".".join(parent)


def _attribute_reference(module: str, name: str) -> ControlledReference:
    qualified = module + "." + name
    if _owner_module_prefix(qualified):
        return qualified, None
    return _controlled_reference(module, name)


def _import_aliases(tree: ast.AST, path: Path | None = None) -> ControlledAliases:
    """Resolve one bounded set of import aliases used by the ownership gate."""

    aliases: ControlledAliases = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for imported in node.names:
                if imported.name not in CONTROLLED_MODULES and not _owner_module_prefix(imported.name):
                    continue
                bound_name = imported.asname or imported.name
                _add_alias(aliases, bound_name, (imported.name, None))
        elif isinstance(node, ast.ImportFrom):
            module = _absolute_import_module(node, path)
            if module is None:
                continue
            for imported in node.names:
                if module in CONTROLLED_MODULES and (imported.name in CONTROLLED_COMMON_REFERENCES or (
                        module in {"os", "shutil"} and
                        imported.name in {"rmdir", "rmtree", "removedirs"}
                ) or (module == "subprocess" and imported.name in SUBPROCESS_APIS)):
                    bound_name = imported.asname or imported.name
                    _add_alias(aliases, bound_name,
                               _controlled_reference(module, imported.name))
                elif module == PROCESS_FACTORY_MODULE and imported.name == PROCESS_FACTORY_NAME:
                    bound_name = imported.asname or imported.name
                    _add_alias(aliases, bound_name, (module, imported.name))
                else:
                    qualified = module + "." + imported.name
                    if _owner_module_prefix(qualified):
                        _add_alias(aliases, imported.asname or imported.name, (qualified, None))
    return aliases


def _references(
        node: ast.AST | None,
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
            _attribute_reference(module, node.attr)
            for module, symbol in _references(node.value, aliases)
            if symbol is None
        }
        dotted = _dotted_name(node)
        if dotted is not None:
            module, separator, name = dotted.rpartition(".")
            if separator and module in CONTROLLED_MODULES:
                references.add(_controlled_reference(module, name))
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
        node: ast.AST | None, aliases: ControlledAliases) -> bool:
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


def _call_violations(
        path: Path,
        tree: ast.AST,
        aliases: ControlledAliases,
        owner_names: set[str],
        owner_type_names: set[str],
) -> list[Violation]:
    violations: list[Violation] = []
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
        if (any(module == "subprocess" and symbol in SUBPROCESS_APIS
                for module, symbol in references) and
                path != PROCESS_OWNER and path not in DIRECT_FIXTURE_SUBPROCESS_FILES):
            violations.append(_violation(
                path, node, "direct-process-owner",
                "acceptance children must use the shared process owner "
                "(Process/Processes or run_bounded_command)"))
        if _is_directory_removal(node, aliases) and path not in DIRECTORY_REMOVAL_FILES:
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
    return violations


def _module_binding_names(node: ast.AST) -> set[str]:
    """Conservatively collect module writes, including evaluated definition expressions."""
    if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
        expressions = [*node.decorator_list, node.args]
        if node.returns is not None:
            expressions.append(node.returns)
        return {node.name} | {name for value in expressions for name in _module_binding_names(value)}
    if isinstance(node, ast.ClassDef):
        return {node.name} | {name for value in (*node.decorator_list, *node.bases, *node.keywords)
                              for name in _module_binding_names(value)}
    if isinstance(node, ast.Import):
        return {item.asname or item.name.split(".", 1)[0] for item in node.names}
    if isinstance(node, ast.ImportFrom):
        return {item.asname or item.name for item in node.names}
    if isinstance(node, ast.Name) and isinstance(node.ctx, (ast.Store, ast.Del)):
        return {node.id}
    if isinstance(node, ast.Attribute) and isinstance(node.ctx, (ast.Store, ast.Del)):
        name = _dotted_name(node)
        roots = {name.split(".", 1)[0]} if name is not None else set()
        return roots | _module_binding_names(node.value)
    names = {name for child in ast.iter_child_nodes(node) for name in _module_binding_names(child)}
    if isinstance(node, (ast.ExceptHandler, ast.MatchAs, ast.MatchStar)) and node.name is not None:
        names.add(node.name)
    if isinstance(node, ast.MatchMapping) and node.rest is not None:
        names.add(node.rest)
    return names


def _direct_schema_base(statement: ast.stmt) -> tuple[ast.Name, str] | None:
    """The exact import read allowed by the supported direct schema declaration."""
    if (not isinstance(statement, ast.ClassDef) or statement.decorator_list or
            len(statement.bases) != 1 or any(keyword.arg != "total" for keyword in statement.keywords)):
        return None
    base = statement.bases[0]
    if isinstance(base, ast.Name):
        return base, "TypedDict"
    if isinstance(base, ast.Attribute) and base.attr == "TypedDict" and isinstance(base.value, ast.Name):
        return base.value, "typing"
    return None


def _typed_dict_field_targets(tree: ast.Module) -> set[ast.Name]:
    """Recognize direct schema-only fields; ambiguous bindings keep the runtime checks."""
    bindings: dict[str, str] = {}
    base_reads = {reference[0] for statement in tree.body
                  if (reference := _direct_schema_base(statement)) is not None}
    untrusted_bindings = {name for node in ast.walk(tree) if isinstance(node, ast.Global) for name in node.names}
    untrusted_bindings.update(node.id for node in ast.walk(tree)
                              if isinstance(node, ast.Name) and isinstance(node.ctx, ast.Load)
                              and node not in base_reads)
    canonical_import_names: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom) and any(item.name == "*" for item in node.names):
            return set()
        if isinstance(node, ast.Import):
            canonical_import_names.update(item.asname or item.name for item in node.names if item.name == "typing")
        elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module == "typing":
            canonical_import_names.update(item.asname or item.name for item in node.names if item.name == "TypedDict")
    # These imports share module/factory objects, even when their local names differ.
    if canonical_import_names & untrusted_bindings:
        return set()
    fields: set[ast.Name] = set()
    for statement in tree.body:
        rebound = _module_binding_names(statement)
        if "*" in rebound:
            bindings.clear()
        for name in rebound:
            bindings.pop(name, None)
        if isinstance(statement, ast.ImportFrom):
            for item in statement.names:
                name = item.asname or item.name
                bindings.pop(name, None)
                if (statement.level == 0 and statement.module == "typing" and item.name == "TypedDict"
                        and name not in untrusted_bindings):
                    bindings[name] = "TypedDict"
        elif isinstance(statement, ast.Import):
            for item in statement.names:
                name = item.asname or item.name.split(".", 1)[0]
                bindings.pop(name, None)
                if item.name == "typing" and name not in untrusted_bindings:
                    bindings[name] = "typing"
        elif isinstance(statement, ast.ClassDef):
            reference = _direct_schema_base(statement)
            if reference is not None and bindings.get(reference[0].id) == reference[1]:
                fields.update(field.target for field in statement.body
                              if isinstance(field, ast.AnnAssign) and field.value is None
                              and isinstance(field.target, ast.Name))
    return fields


def find_violations(paths: tuple[Path, ...] | list[Path]) -> tuple[Violation, ...]:
    violations: list[Violation] = []
    for candidate in paths:
        path = Path(candidate).resolve()
        try:
            tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        except (OSError, SyntaxError) as error:
            violations.append(Violation(path, 0, 0, "source", str(error)))
            continue

        aliases = _import_aliases(tree, path)
        _collect_controlled_aliases(tree, aliases)
        for node in ast.walk(tree):
            if (isinstance(node, ast.ClassDef) and node.name in PROCESS_LIFECYCLE_TYPES
                    and path != PROCESS_OWNER):
                violations.append(_violation(
                    path, node, "duplicate-process-owner",
                    "process lifecycle classes must be defined in the canonical process owner"))
            if (isinstance(node, ast.ClassDef) and node.name in DIRECTORY_LIFECYCLE_TYPES
                    and path != DIRECTORY_OWNER):
                violations.append(_violation(
                    path, node, "duplicate-directory-owner",
                    "directory lifecycle classes must be defined in the canonical directory owner"))
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

        violations.extend(_call_violations(
            path, tree, aliases, owner_names, owner_type_names))

        schema_fields = _typed_dict_field_targets(tree)
        for node in ast.walk(tree):
            if isinstance(node, (ast.Name, ast.Attribute)) and node not in schema_fields:
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
