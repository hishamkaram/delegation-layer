"""Opt-in native candidate selection without changing user settings or logins."""

from dataclasses import dataclass
from collections import Counter
import hashlib
import json
import os
from pathlib import Path
import re
import shlex


@dataclass(frozen=True)
class PreparedContext:
    arguments: tuple[str, ...]
    skill: Path
    evidence: dict[str, object]


class PreparedSkillConfigurationChanged(ValueError):
    """The opt-in context would change an unrelated visible skill selection."""


PREPARED_CODEX_SHELL_CONTROLS = (
    "allow_login_shell=false", "features.shell_snapshot=false", "features.shell_snapshot_v2=false",
)


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _texts(document: object) -> list[str]:
    if not isinstance(document, list):
        raise ValueError("invalid native candidate prompt context")
    texts = []
    for item in document:
        if not isinstance(item, dict) or not isinstance(item.get("content", []), list):
            raise ValueError("invalid native candidate prompt context item")
        for block in item.get("content", []):
            if isinstance(block, dict) and isinstance(block.get("text"), str):
                texts.append(block["text"])
    return texts


def _skill_metadata(document: object) -> list[tuple[str, Path]]:
    """Resolve the native visible skill listing, including its root aliases."""
    text = "\n".join(_texts(document))
    aliases = {}
    for alias, root in re.findall(r"^- `([^`]+)` = `([^`]+)`$", text, re.MULTILINE):
        if alias in aliases and aliases[alias] != root:
            raise ValueError("ambiguous native skill root alias")
        aliases[alias] = root
    entries = []
    for name, entry in re.findall(r"^- ([^\n]+?): [^\n]*\(file: ([^)]+)\)", text, re.MULTILINE):
        first, separator, rest = entry.partition("/")
        path = Path(aliases[first]) / rest if separator and first in aliases else Path(entry)
        if not path.is_absolute() or path.name != "SKILL.md" or ".." in path.parts:
            raise ValueError("native skill metadata contains an unresolved candidate path")
        entries.append((name, path))
    return entries


def available_skill_paths(document: object) -> list[Path]:
    """Compare only the metadata this exact native prompt exposes."""
    return [path for _name, path in _skill_metadata(document)]


def skill_paths(document: object) -> list[Path]:
    """Return explicit agent-integration entries, excluding mentions in other descriptions."""
    return [path for name, path in _skill_metadata(document) if name.split(":")[-1] == "agent-integration"]


def verify_visible_skill_selection(baseline: object, prepared: object, skill: Path):
    """Permit only replacement of the baseline's explicit target skill entries."""
    paths = skill_paths(prepared)
    if len(paths) != 1 or paths[0].resolve() != skill.resolve():
        raise ValueError("native prompt context did not uniquely discover the candidate skill")
    expected = Counter(path.resolve() for path in available_skill_paths(baseline))
    expected.subtract(path.resolve() for path in skill_paths(baseline))
    expected[skill.resolve()] += 1
    if Counter(path.resolve() for path in available_skill_paths(prepared)) != expected:
        raise PreparedSkillConfigurationChanged(
            "prepared context changed unrelated visible skills; use a dedicated native context")


def _disable_arguments(paths: set[Path]) -> list[str]:
    entries = ["{path=" + json.dumps(str(path)) + ",enabled=false}" for path in sorted(paths, key=str)]
    return ["-c", "skills.config=[" + ",".join(entries) + "]"]


def _personal_paths() -> set[Path]:
    home = Path.home()
    paths = set()
    for root in (Path(os.environ.get("CODEX_HOME", str(home / ".codex"))) / "skills", home / ".agents/skills"):
        skill = root / "agent-integration/SKILL.md"
        paths.update((skill, skill.resolve()))
    return paths


def _selected_plugin_skill(skill: Path) -> bool:
    """A personal symlink into another profile's cache does not activate that plugin."""
    cache = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex"))) / "plugins/cache"
    return any(skill.is_relative_to(root) for root in (cache, cache.resolve()))


def require_source(skill: Path, source: Path):
    if not skill.is_file() or skill.read_bytes() != (source / "skills/agent-integration/SKILL.md").read_bytes():
        raise ValueError("prepared native candidate does not match current source")


def _claude_bootstrap(owner, source: Path, delegate: Path):
    """Make the owned root shim a complete in-place selection for the real plugin hook."""
    selected = {"delegate-run": delegate.with_name("delegate-run"),
                "pueue": Path(owner.client), "pueued": Path(owner.daemon_path)}
    directory = owner.run / "candidate-bin"
    for name, executable in selected.items():
        executable = executable.resolve(strict=True)
        if not executable.is_file() or not os.access(executable, os.X_OK):
            raise ValueError("prepared candidate requires installed " + name)
        (directory / name).symlink_to(executable)
    owner.processes.environment["DELEGATION_LAYER_INSTALL_DIR"] = str(directory)
    plugin = source / "plugins/delegation-layer"
    capture = owner.capture("prepared-plugin-bootstrap", ["/usr/bin/env", "CLAUDE_PLUGIN_ROOT=" + str(plugin),
                                                          "/bin/sh", str(plugin / "scripts/ensure-delegate.sh")])
    return {"bootstrap_stdout_sha256": _digest(capture)}


def prepare(owner, executable: str, harness: str, workspace: Path, source: Path,
            delegate: Path, *, model: str | None = None, routing_arguments: tuple[str, ...] = ()) -> PreparedContext:
    """Only validated native observations permit the opt-in path to bypass a shadow."""
    if harness == "claude":
        skill = source / "plugins/delegation-layer/skills/agent-integration/SKILL.md"
        require_source(skill, source)
        help_path = owner.capture("prepared-native-help", [executable, "--help"])
        help_text = help_path.read_text()
        if any(flag not in help_text for flag in ("--plugin-dir", "--settings", "--include-hook-events")):
            raise ValueError("installed Claude lacks prepared candidate controls")
        arguments = ("--plugin-dir", str(source / "plugins/delegation-layer"), "--settings",
                     json.dumps({"skillOverrides": {"agent-integration": "off"}}), "--include-hook-events")
        return PreparedContext(arguments, skill, {
            "harness": harness, "candidate_skill": str(skill), "skill_sha256": _digest(skill),
            "selection": "invocation-plugin", **_claude_bootstrap(owner, source, delegate),
        })
    if harness != "codex":
        raise ValueError("prepared native context supports only Claude and Codex")
    skill = workspace / ".agents/skills/agent-integration/SKILL.md"
    require_source(skill, source)
    disabled = _personal_paths()
    model_arguments = ["-c", "model=" + json.dumps(model)] if model is not None else []
    shell_arguments = [argument for control in PREPARED_CODEX_SHELL_CONTROLS for argument in ("-c", control)]
    discovery = owner.capture("prepared-discovery-context", [executable, *routing_arguments, *shell_arguments,
                                                             *model_arguments, "debug", "prompt-input"])
    baseline = json.loads(discovery.read_text())
    targets = skill_paths(baseline)
    for path in targets:
        if path.resolve() != skill.resolve():
            if _selected_plugin_skill(path):
                raise PreparedSkillConfigurationChanged(
                    "selected native plugin cache provides a conflicting skill; use a dedicated native context")
            disabled.update((path, path.resolve()))
    arguments = [*shell_arguments, *_disable_arguments(disabled)]
    context = owner.capture("prepared-native-context", [executable, *routing_arguments, *arguments, *model_arguments,
                                                        "debug", "prompt-input"])
    prepared_document = json.loads(context.read_text())
    verify_visible_skill_selection(baseline, prepared_document, skill)
    require_source(skill, source)
    return PreparedContext(tuple(arguments), skill, {
        "harness": harness, "candidate_skill": str(skill), "skill_sha256": _digest(skill),
        "selection": "invocation-skill-overrides", "disabled_skills": [str(path) for path in sorted(disabled, key=str)],
        "discovery_sha256": _digest(discovery), "context_sha256": _digest(context),
        "visible_skill_comparison": "baseline-with-target-replaced",
        "routing_arguments": list(routing_arguments),
        "shell_controls": list(PREPARED_CODEX_SHELL_CONTROLS),
    })


def _events(path: Path) -> list[dict]:
    result = []
    for line in path.read_text().splitlines():
        if not line.strip():
            continue
        event = json.loads(line)
        if not isinstance(event, dict):
            raise ValueError("invalid native routing event")
        result.append(event)
    return result


def _claude_read(events: list[dict], skill: Path, source: str) -> bool:
    requested = set()
    completed = set()
    bash_commands = {}
    for event in events:
        message = event.get("message")
        if not isinstance(message, dict) or not isinstance(message.get("content"), list):
            continue
        for block in message["content"]:
            if not isinstance(block, dict):
                continue
            if (event.get("type") == "assistant" and block.get("type") == "tool_use"
                    and block.get("name") == "Read" and isinstance(block.get("input"), dict)
                    and block["input"].get("file_path") == str(skill) and isinstance(block.get("id"), str)):
                offset, limit = block["input"].get("offset", 1), block["input"].get("limit", 2000)
                if type(offset) is int and offset == 1 and type(limit) is int and limit >= len(source.splitlines()):
                    requested.add(block["id"])
            if (event.get("type") == "assistant" and block.get("type") == "tool_use"
                    and block.get("name") == "Bash" and isinstance(block.get("input"), dict)
                    and isinstance(block["input"].get("command"), str) and isinstance(block.get("id"), str)):
                bash_commands[block["id"]] = block["input"]["command"]
            if (event.get("type") == "user" and block.get("type") == "tool_result"
                    and block.get("is_error") is not True and isinstance(block.get("tool_use_id"), str)
                    and (isinstance(block.get("content"), str) and block["content"].strip()
                         or isinstance(block.get("content"), list) and any(
                             isinstance(part, dict) and part.get("type") == "text"
                             and isinstance(part.get("text"), str) and part["text"].strip()
                             for part in block["content"]))):
                completed.add(block["tool_use_id"])
                command = bash_commands.get(block["tool_use_id"])
                if command is not None:
                    content = block["content"]
                    output = content if isinstance(content, str) else "\n".join(
                        part["text"] for part in content if isinstance(part, dict) and part.get("type") == "text"
                        and isinstance(part.get("text"), str))
                    if _shell_read(command, output, skill, source):
                        return True
    return bool(requested & completed) or _claude_skill_load(events, skill, source)


def _claude_skill_load(events: list[dict], skill: Path, source: str) -> bool:
    """Accept Claude's observed Skill tool protocol only with its full native body."""
    body = source
    if source.startswith("---\n"):
        frontmatter = source.split("\n---\n", 1)
        if len(frontmatter) != 2:
            return False
        body = frontmatter[1]
    requested = set()
    launched = False
    for event in events:
        message = event.get("message")
        if not isinstance(message, dict) or not isinstance(message.get("content"), list):
            continue
        for block in message["content"]:
            if not isinstance(block, dict):
                continue
            if (event.get("type") == "assistant" and block.get("type") == "tool_use"
                    and block.get("name") == "Skill" and isinstance(block.get("input"), dict)
                    and block["input"].get("skill") == "delegation-layer:agent-integration"
                    and isinstance(block.get("id"), str)):
                requested.add(block["id"])
            if (event.get("type") == "user" and block.get("type") == "tool_result"
                    and isinstance(block.get("tool_use_id"), str) and block["tool_use_id"] in requested
                    and block.get("is_error") is not True
                    and block.get("content") == "Launching skill: delegation-layer:agent-integration"):
                launched = True
            if (launched and event.get("type") == "user" and block.get("type") == "text"
                    and isinstance(block.get("text"), str)):
                heading, _, loaded = block["text"].partition("\n")
                if (heading == "Base directory for this skill: " + str(skill.parent)
                        and loaded.strip() == body.strip()):
                    return True
    return False


def _shell_read(command: str, output: str, skill: Path, source: str, *, relative_candidate: str | None = None) -> bool:
    """Recognize literal reads only; do not evaluate shell syntax or resolve variables."""
    if not skill.is_absolute() or not source.strip() or source.strip() not in output:
        return False
    try:
        wrapper = shlex.split(command)
        if len(wrapper) == 3 and wrapper[1] in ("-c", "-lc") and Path(wrapper[0]).name in ("sh", "bash", "zsh"):
            command = wrapper[2]
        separators = ";&|()\n"
        lexer = shlex.shlex(command, posix=True, punctuation_chars=separators)
        lexer.whitespace = " \t\r"
        lexer.whitespace_split = True
        segments = [[]]
        for token in lexer:
            if all(character in separators for character in token):
                segments.append([])
            else:
                segments[-1].append(token)
    except ValueError:
        return False
    literal_reads = [segment for segment in segments if segment and Path(segment[0]).name in ("cat", "sed")]
    if any(str(skill) in segment[1:] for segment in literal_reads):
        return True
    if relative_candidate is None or any(token in ("cd", "pushd", "popd") for segment in segments for token in segment):
        return False
    return any(relative_candidate in segment[1:] for segment in literal_reads)


def _codex_read(events: list[dict], skill: Path, source: str, workspace: Path) -> bool:
    # This is the harness's recorded submission workspace, not a shell CWD
    # evaluator or a claim about arbitrary per-call path/inode identity.
    relative = ".agents/skills/agent-integration/SKILL.md"
    relative_candidate = relative if workspace.is_absolute() and skill == workspace / relative else None
    for event in events:
        item = event.get("item")
        if (event.get("type") != "item.completed" or not isinstance(item, dict)
                or item.get("type") != "command_execution" or item.get("exit_code") != 0
                or item.get("status") != "completed" or not isinstance(item.get("command"), str)
                or not isinstance(item.get("aggregated_output"), str)):
            continue
        if _shell_read(item["command"], item["aggregated_output"], skill, source, relative_candidate=relative_candidate):
            return True
    return False


def verify_run(harness: str, controls: dict, transcript: Path, *, source_skill: Path,
               require_read: bool) -> dict[str, object]:
    """Model prose is never identity, hook or read evidence."""
    skill = Path(controls["candidate_skill"])
    if _digest(source_skill) != controls.get("skill_sha256"):
        raise ValueError("prepared candidate no longer matches current source")
    source = source_skill.read_text()
    events = _events(transcript)
    if harness == "claude":
        plugin = skill.parents[2]
        initialized = any(
            event.get("type") == "system" and event.get("subtype") == "init"
            and isinstance(event.get("plugins"), list)
            and [{"name": row.get("name"), "path": row.get("path")} for row in event["plugins"]
                 if isinstance(row, dict) and row.get("name") == "delegation-layer"]
            == [{"name": "delegation-layer", "path": str(plugin)}]
            for event in events)
        hooked = any(event.get("type") == "system" and event.get("subtype") == "hook_response"
                     and event.get("hook_event") == "SessionStart" and event.get("exit_code") == 0
                     and isinstance(event.get("stdout"), str) and str(skill) in event["stdout"]
                     for event in events)
        if not initialized or not hooked:
            raise ValueError("candidate plugin initialization and routing hook were not observed")
        read = _claude_read(events, skill, source)
    elif harness == "codex":
        if controls.get("selection") != "invocation-skill-overrides":
            raise ValueError("Codex candidate context was not verified")
        read = _codex_read(events, skill, source, transcript.resolve().parent / "workspace")
    else:
        raise ValueError("unsupported prepared native harness")
    if require_read and not read:
        raise ValueError("actual successful candidate skill read was not observed")
    return {"candidate_read": read, "read_required": require_read, "native_context_verified": True,
            "native_hook_observed": harness == "claude"}
