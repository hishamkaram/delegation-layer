package pueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func parseConfigOK(t *testing.T, source string) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigOrdinaryYAMLAndJSON(t *testing.T) {
	yaml := "# handwritten YAML\nshared: {host: localhost, port: '6924'}\nclient:\n  read_local_logs: false\ndaemon:\n  callback: |\n    echo one\n    echo two\n"
	jsonForm := `{"shared":{"host":"localhost","port":"6924"},"client":{"read_local_logs":false},"daemon":{"callback":"echo one\necho two\n"}}`
	if !reflect.DeepEqual(parseConfigOK(t, yaml), parseConfigOK(t, jsonForm)) {
		t.Fatal("YAML and JSON semantics differ")
	}
	aliased := parseConfigOK(t, "shared:\n  pueue_directory: &data /private/data\n  runtime_directory: *data\nprofiles:\n  unused:\n    shared: {host: elsewhere}\n")
	if *aliased.Shared.PueueDirectory != *aliased.Shared.RuntimeDirectory || aliased.Shared.Host != "127.0.0.1" {
		t.Fatal("alias or base profile selection incorrect")
	}
}

func TestConfigDefaultsPreservePresence(t *testing.T) {
	absent := parseConfigOK(t, "{}")
	present := parseConfigOK(t, "shared: {}")
	if absent.Shared.UnixSocketPermissions == nil || *absent.Shared.UnixSocketPermissions != 0o700 || present.Shared.UnixSocketPermissions != nil {
		t.Fatal("lost omitted-vs-present section defaults")
	}
	if !absent.Shared.UseUnixSocket || !absent.Client.ReadLocalLogs || absent.Client.EditMode != "toml" || absent.Daemon.CallbackLogLines != 10 {
		t.Fatal("incorrect upstream defaults")
	}
	if absent.Daemon.Callback != nil || len(absent.Daemon.EnvVars) != 0 || absent.Shared.Port != "6924" {
		t.Fatal("incorrect option/default value")
	}
	profiles := parseConfigOK(t, "shared: {host: base}\nprofiles:\n  x:\n    shared: {host: profile}\n")
	if profiles.Shared.Host != "base" {
		t.Fatal("unexpected profile overlay")
	}
}

func TestConfigStrictRefusals(t *testing.T) {
	cases := map[string]string{
		"duplicate":                    "shared: {host: a, host: b}",
		"quoted duplicate":             "shared: {host: a, 'host': b}",
		"escaped duplicate":            "shared: {host: a, \"ho\\u0073t\": b}",
		"profile duplicate":            "profiles: {a: {}, a: {}}",
		"environment duplicate":        "daemon: {env_vars: {A: x, A: y}}",
		"merge":                        "shared: {<<: {host: other}}",
		"quoted merge":                 "shared: {'<<': {host: other}}",
		"custom tag":                   "shared: {host: !unexpected value}",
		"non-string key":               "shared: {1: x}",
		"unknown":                      "shared: {unknown: value}",
		"case alias":                   "shared: {Host: value}",
		"nested profiles":              "profiles: {a: {profiles: {b: {}}}}",
		"unknown in unused profile":    "profiles: {a: {shared: {unknown: value}}}",
		"invalid edit mode in profile": "profiles: {a: {client: {edit_mode: invalid}}}",
		"undefined alias":              "shared: *missing",
		"duplicate anchor":             "shared: {host: &a x, port: &a y}",
		"key alias":                    "shared: {host: &a port, *a: y}",
		"cycle":                        "profiles: &a {x: *a}",
		"empty":                        "",
		"null root":                    "null",
		"second document":              "{}\n---\n{}",
		"trailing malformed document":  "{}\n---\n[",
		"null section":                 "shared: null",
		"null required scalar":         "shared: {host: null}",
		"legacy boolean":               "shared: {use_unix_socket: yes}",
		"tagged legacy boolean":        "shared: {use_unix_socket: !!bool yes}",
		"quoted boolean":               "shared: {use_unix_socket: 'true'}",
		"tagged quoted boolean":        "shared: {use_unix_socket: !!bool 'true'}",
		"quoted integer":               "shared: {unix_socket_permissions: '448'}",
		"tagged quoted integer":        "shared: {unix_socket_permissions: !!int '448'}",
		"legacy octal":                 "shared: {unix_socket_permissions: 0700}",
		"underscored integer":          "shared: {unix_socket_permissions: 4_48}",
		"negative integer":             "shared: {unix_socket_permissions: -1}",
		"overflow":                     "shared: {unix_socket_permissions: 4294967296}",
		"floating integer":             "shared: {unix_socket_permissions: 448.0}",
		"numeric port coercion":        "shared: {port: 6924}",
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if c, err := ParseConfig([]byte(source)); !errors.Is(err, ErrConfiguration) || c != nil {
				t.Fatalf("accepted invalid input: config=%v err=%v", c, err)
			}
		})
	}
	parseConfigOK(t, "shared: {alias_file: null, use_unix_socket: TRUE, unix_socket_permissions: 0o700}\n# tail\n")
	for _, number := range []string{"0", "448", "0o700", "0x1c0", "0b111000000", "+448", "4294967295"} {
		parseConfigOK(t, "shared: {unix_socket_permissions: "+number+"}")
	}
}

func TestConfigResourceLimits(t *testing.T) {
	for name, source := range map[string]string{
		"bytes":     strings.Repeat(" ", MaxControlBytes+1),
		"depth":     "daemon: {shell_command: " + strings.Repeat("[", maxYAMLDepth) + "x" + strings.Repeat("]", maxYAMLDepth) + "}",
		"nodes":     environmentYAML(8200, 0),
		"aliases":   aliasYAML(maxYAMLAliases + 1),
		"expansion": environmentYAML(1000, 40),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(source)); !errors.Is(err, ErrConfiguration) {
				t.Fatalf("missing limit refusal: %v", err)
			}
		})
	}
	parseConfigOK(t, strings.Repeat(" ", MaxControlBytes-2)+"{}")
	parseConfigOK(t, aliasYAML(maxYAMLAliases))
	parseConfigOK(t, environmentYAML(1000, 20))
}

func aliasYAML(count int) string {
	var b strings.Builder
	b.WriteString("daemon:\n  env_vars:\n    original: &v value\n")
	for i := 0; i < count; i++ {
		if _, err := fmt.Fprintf(&b, "    A%d: *v\n", i); err != nil {
			panic(err)
		}
	}
	return b.String()
}

func environmentYAML(keys, profiles int) string {
	var b strings.Builder
	b.WriteString("daemon:\n  env_vars: &vars\n")
	for i := 0; i < keys; i++ {
		if _, err := fmt.Fprintf(&b, "    V%d: value\n", i); err != nil {
			panic(err)
		}
	}
	if profiles > 0 {
		b.WriteString("profiles:\n")
	}
	for i := 0; i < profiles; i++ {
		if _, err := fmt.Fprintf(&b, "  P%d:\n    daemon:\n      env_vars: *vars\n", i); err != nil {
			panic(err)
		}
	}
	return b.String()
}

func fixedResolution() ResolutionContext {
	return ResolutionContext{OS: "linux", Home: "/home/person", DataLocalDirectory: "/home/person/.local/share", ConfigDirectory: "/home/person/.config", RuntimeDirectory: "/run/user/1000", Username: "person", Cwd: "/work"}
}

func TestResolveDefaultsAndRelevantContext(t *testing.T) {
	config := parseConfigOK(t, "{}")
	r := fixedResolution()
	resolved := resolveConfigForTest(t, config, r)
	assertDefaultResolution(t, resolved)
	before := digestResolutionForTest(t, resolved)
	r.Cwd = "/irrelevant"
	same := resolveConfigForTest(t, config, r)
	unchanged := digestResolutionForTest(t, same)
	if before != unchanged {
		t.Fatal("fingerprint includes irrelevant cwd")
	}
	r.ConfigDirectory = "/different-config"
	changed := resolveConfigForTest(t, config, r)
	after := digestResolutionForTest(t, changed)
	if before == after || changed.Endpoint() != resolved.Endpoint() {
		t.Fatal("alias default drift not bound independently of endpoint")
	}
	r = fixedResolution()
	r.OS = "darwin"
	r.RuntimeDirectory = ""
	darwin := resolveConfigForTest(t, config, r)
	if darwin.RuntimeDirectory != darwin.PueueDirectory {
		t.Fatal("missing runtime should use data")
	}
}

func resolveConfigForTest(t *testing.T, config *Config, resolution ResolutionContext) ResolvedConfig {
	t.Helper()
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func digestResolutionForTest(t *testing.T, resolved ResolvedConfig) string {
	t.Helper()
	digest, err := resolved.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func assertDefaultResolution(t *testing.T, resolved ResolvedConfig) {
	t.Helper()
	if resolved.PueueDirectory != "/home/person/.local/share/pueue" || resolved.RuntimeDirectory != "/run/user/1000" || resolved.SocketPath != "/run/user/1000/pueue_person.socket" || resolved.AliasFile != "/home/person/.config/pueue/pueue_aliases.yml" {
		t.Fatalf("wrong defaults: %+v", resolved)
	}
}

func TestResolveRefusesAmbiguity(t *testing.T) {
	for name, source := range map[string]string{
		"relative":      "shared: {pueue_directory: relative}",
		"empty path":    "shared: {pueue_directory: ''}",
		"named user":    "shared: {pueue_directory: '~other/a'}",
		"TCP":           "shared: {use_unix_socket: false}",
		"socket length": "shared: {unix_socket_path: '/" + strings.Repeat("s", 107) + "'}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveConfig(parseConfigOK(t, source), fixedResolution()); !errors.Is(err, ErrConfiguration) {
				t.Fatalf("got %v", err)
			}
		})
	}
	config := parseConfigOK(t, "shared: {pueue_directory: '~/data', alias_file: '/literal/$VAR'}")
	r, err := ResolveConfig(config, fixedResolution())
	if err != nil {
		t.Fatal(err)
	}
	if r.PueueDirectory != "/home/person/data" || r.AliasFile != "/literal/$VAR" {
		t.Fatal("incorrect expansion")
	}
	missing := fixedResolution()
	missing.Home = ""
	if _, err := ResolveConfig(config, missing); !errors.Is(err, ErrConfiguration) {
		t.Fatal("missing home accepted")
	}
}

func privateConfig(t *testing.T, base string) []byte {
	t.Helper()
	shared := map[string]any{
		"pueue_directory": filepath.Join(base, "data"), "runtime_directory": filepath.Join(base, "run"),
		"unix_socket_path": filepath.Join(base, "socket"), "alias_file": filepath.Join(base, "aliases"),
		"pid_path": filepath.Join(base, "pid"), "shared_secret_path": filepath.Join(base, "secret"),
		"daemon_cert": filepath.Join(base, "cert"), "daemon_key": filepath.Join(base, "key"),
	}
	data, err := json.Marshal(map[string]any{"shared": shared, "daemon": map[string]any{"shell_command": []string{"/bin/sh", "-c", "{{ pueue_command_string }}"}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func canonicalTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "py-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestPrivateIsolation(t *testing.T) {
	base := canonicalTemp(t)
	config, err := ParseConfig(privateConfig(t, base))
	if err != nil {
		t.Fatal(err)
	}
	r, err := ResolveConfig(config, fixedResolution())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateIsolation(base); err != nil {
		t.Fatal(err)
	}
	outside := r
	outside.AliasFile = "/outside/aliases"
	if err := outside.ValidateIsolation(base); !errors.Is(err, ErrConfiguration) {
		t.Fatal("escaped alias accepted")
	}
	if err := os.Symlink("/outside", filepath.Join(base, "data")); err != nil {
		t.Fatal(err)
	}
	if err := r.ValidateIsolation(base); !errors.Is(err, ErrConfiguration) {
		t.Fatal("symlink accepted")
	}
	if err := os.Remove(filepath.Join(base, "data")); err != nil {
		t.Fatal(err)
	}
	callback := "echo injected"
	r.Daemon.Callback = &callback
	if err := r.ValidateIsolation(base); !errors.Is(err, ErrConfiguration) {
		t.Fatal("callback accepted")
	}
}
