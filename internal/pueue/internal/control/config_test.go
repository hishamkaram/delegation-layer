package control

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestResolveUsesNativeUnixSocketByteLimit(t *testing.T) {
	for _, test := range []struct {
		goos  string
		limit int
	}{
		{goos: "darwin", limit: 103},
		{goos: "linux", limit: 107},
	} {
		t.Run(test.goos, func(t *testing.T) {
			resolution := fixedResolution()
			resolution.OS = test.goos
			for _, path := range []string{
				"/" + strings.Repeat("s", test.limit-1),
				"/" + strings.Repeat("é", (test.limit-1)/2) + strings.Repeat("s", (test.limit-1)%2),
			} {
				if len(path) != test.limit {
					t.Fatalf("boundary fixture length = %d, want %d", len(path), test.limit)
				}
				if _, err := ResolveConfig(parseConfigOK(t, "shared: {unix_socket_path: '"+path+"'}"), resolution); err != nil {
					t.Fatalf("native-limit socket path was rejected: %v", err)
				}
				if _, err := ResolveConfig(parseConfigOK(t, "shared: {unix_socket_path: '"+path+"s'}"), resolution); !errors.Is(err, errUnixSocketPathTooLong) {
					t.Fatalf("over-limit socket path error = %v", err)
				}
			}
		})
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
