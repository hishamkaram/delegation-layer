"""Collect Python import, function and finite call facts without module evaluation.

Effects retain builtin observations and finite qualified pathlib.Path / datetime
calls and ambient stdlib references. Import aliases are conservative module-wide
candidates: conditional imports, later rebinding and local shadowing can cause
over-reporting. Assignment aliases, wildcard imports, arbitrary return values,
Path variables and dynamic getattr calls are not resolved. Absence of an effect
is not a proof of semantic purity; policy.Check alone interprets these facts.

Calls count plain positional values and recursively expand starred tuple/list
literals. Dynamic starred values have unknown arity and add no inferred count;
known values in the same call still count. Keyword arguments are not positional.
"""

import ast


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
AMBIENT_REFERENCES = frozenset({
    "shutil.copy", "shutil.copy2", "shutil.copyfile", "shutil.copyfileobj",
    "shutil.copytree", "shutil.copymode", "shutil.copystat", "shutil.rmtree",
    "shutil.move", "shutil.chown", "shutil.disk_usage", "shutil.which",
    "shutil.make_archive", "shutil.unpack_archive", "shutil.get_terminal_size",
    "tempfile.TemporaryFile", "tempfile.NamedTemporaryFile", "tempfile.SpooledTemporaryFile",
    "tempfile.TemporaryDirectory", "tempfile.mkstemp", "tempfile.mkdtemp", "tempfile.mktemp",
    "tempfile.gettempdir", "tempfile.gettempdirb", "tempfile.tempdir",
    "fcntl.fcntl", "fcntl.ioctl", "fcntl.flock", "fcntl.lockf",
    "signal.signal", "signal.getsignal", "signal.alarm", "signal.pause",
    "signal.raise_signal", "signal.pthread_kill", "signal.pthread_sigmask",
    "signal.setitimer", "signal.getitimer", "signal.set_wakeup_fd", "signal.siginterrupt",
    "signal.sigpending", "signal.sigwait", "signal.sigwaitinfo", "signal.sigtimedwait",
    "threading.Thread", "threading.Timer", "threading.Lock", "threading.RLock",
    "threading.Condition", "threading.Event", "threading.Semaphore", "threading.Barrier",
    "threading.BoundedSemaphore", "threading.local", "threading.current_thread",
    "threading.main_thread", "threading.get_ident", "threading.get_native_id",
    "threading.enumerate", "threading.active_count", "threading.settrace", "threading.setprofile",
    "sys.stdin", "sys.stdout", "sys.stderr", "sys.__stdin__", "sys.__stdout__", "sys.__stderr__",
    "sys.argv", "sys.orig_argv", "sys.path", "sys.modules", "sys.exit", "sys._getframe",
    "sys.settrace", "sys.gettrace", "sys.setprofile", "sys.getprofile", "sys.addaudithook",
    "secrets.choice", "secrets.randbelow", "secrets.randbits", "secrets.SystemRandom",
    "secrets.token_bytes", "secrets.token_hex", "secrets.token_urlsafe",
    "uuid.getnode", "uuid.uuid1", "uuid.uuid4", "uuid.uuid6", "uuid.uuid7",
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


def positional_argument_count(arguments):
    """Count only known values without evaluating starred expressions."""
    count = 0
    for argument in arguments:
        if not isinstance(argument, ast.Starred):
            count += 1
        elif isinstance(argument.value, (ast.Tuple, ast.List)):
            count += positional_argument_count(argument.value.elts)
    return count


def parse_source_facts(path, source, known):
    name = module_name(path)
    tree = ast.parse(source, filename=path)
    aliases = import_aliases(tree)
    imports = []
    functions = []
    calls = []
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
            calls.append({"name": ast.unparse(node.func), "line": node.lineno,
                          "positional_arguments": positional_argument_count(node.args)})
            effects.extend({"name": name, "line": node.lineno}
                           for name in call_effect_names(node, aliases))
        elif isinstance(node, (ast.Name, ast.Attribute)):
            effects.extend({"name": name, "line": node.lineno}
                           for name in sorted(qualified_names(node, aliases) & AMBIENT_REFERENCES))
    return {"path": path, "module": "py:" + name, "imports": imports,
            "functions": functions, "calls": calls, "effects": effects}
