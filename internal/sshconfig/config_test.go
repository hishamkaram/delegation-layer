package sshconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configuration(t *testing.T, user, system string, extra map[string]string) *Config {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".ssh")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, data := range extra {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	u, s := filepath.Join(dir, "config"), filepath.Join(home, "system")
	if err := os.WriteFile(u, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s, []byte(system), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(Options{Home: home, UserConfig: u, SystemConfig: s, LocalUser: "local"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPrecedenceAndIncludes(t *testing.T) {
	c := configuration(t, "Host alpha beta\n HostName=example.test\n User first\n IdentityFile ~/key-one\n Include inner\nHost * !beta\n Port 2200\n IdentityFile ~/key-two\nUser fallback\n", "User system\nPort 2222\nUserKnownHostsFile /first /second\nUserKnownHostsFile /ignored\n", map[string]string{"inner": "IdentityFile ~/included\nUser ignored\n"})
	got, err := c.Resolve("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.HostName != "example.test" || got.User != "first" || got.Port != 2200 || len(got.IdentityFiles) != 3 {
		t.Fatalf("unexpected resolved connection: %#v", got)
	}
	if len(got.UserKnownHostsFiles) != 2 || got.UserKnownHostsFiles[0] != "/first" {
		t.Fatalf("known-hosts lists do not use first value: %#v", got)
	}
	beta, err := c.Resolve("beta")
	if err != nil {
		t.Fatal(err)
	}
	if beta.Port != 2222 {
		t.Fatalf("negated Host matched beta: %d", beta.Port)
	}
	if strings.Join(c.Aliases(), ",") != "alpha,beta" {
		t.Fatal(c.Aliases())
	}
}

func TestConditionalInclude(t *testing.T) {
	c := configuration(t, "Host selected\nInclude inner\nHost other\nUser other-user\n", "", map[string]string{"inner": "User selected-user\n"})
	selected, err := c.Resolve("selected")
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.Resolve("other")
	if err != nil {
		t.Fatal(err)
	}
	if selected.User != "selected-user" || other.User != "other-user" {
		t.Fatalf("conditional include leaked: %s %s", selected.User, other.User)
	}
}

func TestUnsupportedSelectedDirective(t *testing.T) {
	for _, directive := range []string{"ProxyJump gateway", "ProxyCommand command", "Match exec command", "StrictHostKeyChecking no", "IdentityFile ${SECRET}"} {
		t.Run(directive, func(t *testing.T) {
			c := configuration(t, "Host selected\n"+directive+"\n", "", nil)
			if _, err := c.Resolve("selected"); err == nil {
				t.Fatal("unsupported directive accepted")
			}
		})
	}
}

func TestIncludeCycle(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config")
	if err := os.WriteFile(path, []byte("Include "+path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(Options{Home: home, UserConfig: path, SystemConfig: filepath.Join(home, "absent")})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle not diagnosed: %v", err)
	}
}

func TestTokenExpansion(t *testing.T) {
	c := configuration(t, "Host alpha\nHostName node-%h\nUser remote\nPort 2201\nIdentityFile ~/%r-%n-%h-%p\n", "", nil)
	r, err := c.Resolve("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(r.IdentityFiles[0], "remote-alpha-node-alpha-2201") {
		t.Fatal(r.IdentityFiles)
	}
}

func TestIncludedHostScopeContinuesInline(t *testing.T) {
	c := configuration(t, "Host selected\nInclude inner\nPort 2205\n", "", map[string]string{"inner": "Host other\nUser included\n"})
	selected, err := c.Resolve("selected")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Port != 22 {
		t.Fatal("included Host scope did not continue into outer file")
	}
	other, err := c.Resolve("other")
	if err != nil {
		t.Fatal(err)
	}
	if other.User != "local" {
		t.Fatal("inactive Include executed Host directives")
	}
}

func TestHostPatternsTreatBracketsLiterally(t *testing.T) {
	c := configuration(t, "Host node[12]\nUser bracket\nHost node? !node2\nPort 2202\n", "", nil)
	for alias, want := range map[string]string{"node1": "local", "node[12]": "bracket"} {
		resolved, err := c.Resolve(alias)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.User != want {
			t.Fatalf("%s user=%s want=%s", alias, resolved.User, want)
		}
	}
	node1, err := c.Resolve("node1")
	if err != nil {
		t.Fatal(err)
	}
	node2, err := c.Resolve("node2")
	if err != nil {
		t.Fatal(err)
	}
	if node1.Port != 2202 || node2.Port != 22 {
		t.Fatal("wildcard or negation semantics changed")
	}
}

func TestExplicitConfigDoesNotReadDefaultSources(t *testing.T) {
	source := filepath.Join(t.TempDir(), "selected.conf")
	var reads []string
	config, err := Load(Options{Home: t.TempDir(), ConfigFile: source, LocalUser: "tester", ReadFile: func(name string) ([]byte, error) {
		reads = append(reads, name)
		if name != source {
			t.Fatalf("read unselected source %s", name)
		}
		return []byte("Host selected\n HostName example.test\n"), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(reads) != 1 || len(config.Aliases()) != 1 {
		t.Fatalf("wrong selected source: %v", reads)
	}
}

func TestExplicitMissingConfigFails(t *testing.T) {
	_, err := Load(Options{Home: t.TempDir(), ConfigFile: filepath.Join(t.TempDir(), "missing.conf")})
	if err == nil {
		t.Fatal("missing explicit source silently ignored")
	}
}
