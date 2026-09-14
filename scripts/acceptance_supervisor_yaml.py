"""Compiled checks of the strict YAML boundary; native parity is called by R."""
import copy
import json
from pathlib import Path

from acceptance_supervisor_common import read_json, require, write_json


def block_yaml(config):
    lines = ["# Ordinary pueue YAML with comments and a literal block string."]
    for section, values in config.items():
        if not values:
            lines.append(section + ": {}")
            continue
        lines.append(section + ":")
        for key, value in values.items():
            if key == "status_datetime_format":
                lines.append("  status_datetime_format: |-")
                lines.extend("    " + line for line in value.split("\n"))
            else:
                lines.append("  %s: %s" % (key, json.dumps(value)))
    return "\n".join(lines) + "\n"


class YAMLChecks:
    def __init__(self, directory, processes, probe, base, config):
        self.directory = Path(directory)
        self.directory.mkdir(mode=0o700)
        self.processes, self.probe, self.base = processes, probe, Path(base)
        self.config = config
        self.results = []
        self.native_positive = []

    def check(self, identity, name, text, valid, isolate=False, native=False):
        path = self.directory / (identity + "-" + name + ".yml")
        path.write_text(text)
        path.chmod(0o600)
        args = [self.probe, "isolate" if isolate else "parse", path]
        if isolate:
            args.append(self.base)
        proc = self.processes.run(identity + "-" + name, args, self.base, expected=0 if valid else 2)
        report = proc.json() if valid else None
        if not valid:
            error = (proc.directory / "stderr").read_text()
            require(error.strip(), identity + " refused without a diagnostic")
        self.results.append({"id": identity, "name": name, "valid": valid,
                             "process": str(proc.directory), "config": str(path)})
        if native:
            require(valid, "native parity must only receive accepted isolated inputs")
            self.native_positive.append(path)
        return report

    def run(self):
        baseline = self.check("Y01", "json", json.dumps(self.config), True, True, True)
        ordinary = self.check("Y01", "block", block_yaml(self.config), True, True, True)
        require(baseline["parsed"] == ordinary["parsed"] and baseline["resolved_sha256"] == ordinary["resolved_sha256"],
                "ordinary YAML differs from JSON equivalent")
        require(baseline["config_sha256"] != ordinary["config_sha256"], "byte identity flattened")
        duplicates = {
            "path": "shared: {pid_path: /one, pid_path: /two}\n",
            "escaped": 'shared: {pid_path: /one, "pid_\\u0070ath": /two}\n',
            "profile": "profiles: {a: {}, a: {}}\n",
            "env": "daemon: {env_vars: {A: x, A: y}}\n",
        }
        for name, text in duplicates.items():
            self.check("Y02", name, text, False)
        # Both aliases are actual values, not merge keys. The selected base
        # daemon aliases a validated unused profile's complete daemon mapping.
        alias = "profiles:\n  unused:\n    daemon: &d " + json.dumps(self.config["daemon"]) + "\n"
        alias += "daemon: *d\nclient: " + json.dumps(self.config["client"]) + "\n"
        alias += "shared: " + json.dumps(self.config["shared"]) + "\n"
        aliased = self.check("Y03", "mapping-alias", alias, True, True, True)
        require(aliased["parsed"] == baseline["parsed"], "alias changes selected settings")
        self.check("Y03", "scalar-alias", "shared: {host: &h localhost, port: *h}\n", True)
        for name, text in {
            "cycle": "profiles: &p {a: {shared: *p}}\n",
            "undefined": "shared: *missing\n",
            "duplicate-anchor": "shared: {host: &a x, port: &a y}\n",
            "key-alias": "shared: {host: &h pid_path, *h: /a}\n",
            "size": "#" + "x" * (1024 * 1024) + "\n{}\n",
            "alias-count": "daemon: {env_vars: {A: &x value, " + ", ".join("A%d: *x" % n for n in range(129)) + "}}\n",
        }.items():
            self.check("Y04", name, text, False)
        self.check("Y04", "alias-boundary", "daemon: {env_vars: {A: &x value, " + ", ".join("A%d: *x" % n for n in range(128)) + "}}\n", True)
        for name, text in {
            "merge": "shared: {<<: {host: localhost}}\n",
            "quoted-merge": 'shared: {"<<": {host: localhost}}\n',
            "tag": "shared: !custom {}\n",
            "key": "shared: {true: x}\n",
            "unknown": "shared: {unknown: x}\n",
            "case": "Shared: {}\n",
        }.items():
            self.check("Y05", name, text, False)
        for name, text in {"empty": "", "null": "null\n", "second": "{}\n---\n{}\n", "trailing-bad": "{}\n---\n[\n"}.items():
            self.check("Y06", name, text, False)
        self.check("Y06", "trailing-comments", "{}\n  # final comment\n", True)
        for value in ("true", "True", "TRUE", "false", "False", "FALSE"):
            self.check("Y07", "bool-" + value, "client: {dark_mode: " + value + "}\n", True)
        for value in ("yes", "no", "on", "off"):
            self.check("Y07", "legacy-" + value, "client: {dark_mode: " + value + "}\n", False)
        self.check("Y07", "optional-null", "shared: {pid_path: null}\n", True)
        self.check("Y07", "required-null", "client: {dark_mode: null}\n", False)
        for name, value, valid in (("max", "4294967295", True), ("overflow", "4294967296", False),
                                   ("octal", "0o600", True), ("hex", "0x180", True),
                                   ("binary", "0b110000000", True), ("legacy-octal", "0600", False),
                                   ("float", "384.0", False), ("string", '"384"', False), ("negative", "-1", False)):
            self.check("Y07", name, "shared: {unix_socket_permissions: " + value + "}\n", valid)
        absent = self.check("Y08", "omitted", "{}\n", True)
        empty = self.check("Y08", "empty-section", "shared: {}\n", True)
        require(absent["parsed"]["Shared"]["UnixSocketPermissions"] == 448, "omitted shared default changed")
        require(empty["parsed"]["Shared"]["UnixSocketPermissions"] is None, "present shared acquired absent default")
        partial = {"shared": self.config["shared"]}
        self.check("Y08", "private-partial", json.dumps(partial), True, native=True)
        profile = copy.deepcopy(self.config)
        profile["profiles"] = {"other": {"shared": {"unix_socket_path": str(self.base / "other.sock")}}, "partial": {"client": {}}}
        selected = self.check("Y09", "base-selection", json.dumps(profile), True, True, True)
        require(selected["resolved"]["SocketPath"] == self.config["shared"]["unix_socket_path"], "unused profile selected")
        self.isolation_checks()
        write_json(self.directory / "compiled-checks.json", {
            "cases": self.results, "native_parity_pending": [str(p) for p in self.native_positive],
            "unit_complements": {"Y04": "depth/original-node/expanded-node boundary corpus",
                                 "Y10": "injected Linux/macOS resolution and tilde/literal dollar/relative cases",
                                 "Y11": "fresh binding comparisons and historical collection"}})
        return self.native_positive

    def isolation_checks(self):
        for name, mutate in (
            ("escape", lambda c: c["shared"].update(pid_path="/outside/private-base.pid")),
            ("socket-length", lambda c: c["shared"].update(unix_socket_path=str(self.base / ("s" * 150)))),
            ("callback", lambda c: c["daemon"].update(callback="inert forbidden callback")),
            ("env", lambda c: c["daemon"].update(env_vars={"INERT": "value"})),
            ("shell", lambda c: c["daemon"].update(shell_command=["/bin/false"])),
        ):
            changed = copy.deepcopy(self.config)
            mutate(changed)
            self.check("Y12", name, json.dumps(changed), False, True)
        link = self.base / "alias-link"
        link.symlink_to(self.base / "aliases.yml")
        changed = copy.deepcopy(self.config)
        changed["shared"]["alias_file"] = str(link)
        self.check("Y12", "symlink", json.dumps(changed), False, True)
        require(read_json(self.directory / "Y01-json.yml") == self.config, "baseline config changed")
