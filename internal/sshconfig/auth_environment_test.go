package sshconfig

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestUbuntuDefaultsAndOrderedSendEnv(t *testing.T) {
	c := configuration(t, "Host worker\n HostName localhost\n SendEnv FIRST LC_*\n SendEnv -LC_* EXTRA\n GSSAPIAuthentication no\n", "Host *\n SendEnv LANG LC_*\n HashKnownHosts yes\n GSSAPIAuthentication yes\n", nil)
	got, err := c.Resolve("worker")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"FIRST", "EXTRA", "LANG", "LC_*"}
	if !reflect.DeepEqual(got.SendEnv, want) || got.GSSAPIAuthentication || !got.PublicKeyAllowed() {
		t.Fatalf("defaults/precedence = %#v", got)
	}
	if !got.SendsEnvironment("LC_CTYPE") || got.SendsEnvironment("lc_ctype") || got.SendsEnvironment("SECRET") {
		t.Fatal("environment name matching changed")
	}
}

func TestSendEnvRemovalMatchesStoredPatterns(t *testing.T) {
	c := configuration(t, "Host worker\nSendEnv LC_* LC_X LANG\nSendEnv -LC_?\nSendEnv -LC_*\n", "", nil)
	got, err := c.Resolve("worker")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.SendEnv, []string{"LANG"}) {
		t.Fatal(got.SendEnv)
	}
}

func TestAuthenticationSelection(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		allowed        bool
	}{
		{"default", "", true},
		{"optional gssapi", "GSSAPIAuthentication yes\n", true},
		{"gssapi then key", "PreferredAuthentications gssapi-with-mic,publickey\n", true},
		{"gssapi only", "PreferredAuthentications gssapi-with-mic\n", false},
		{"password only", "PreferredAuthentications password\n", false},
		{"key disabled", "PubkeyAuthentication no\nPreferredAuthentications publickey\n", false},
		{"first scalar wins", "PubkeyAuthentication yes\nPubkeyAuthentication no\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := configuration(t, "Host worker\n"+tc.settings, "", nil)
			got, err := c.Resolve("worker")
			if err != nil {
				t.Fatal(err)
			}
			if got.PublicKeyAllowed() != tc.allowed {
				t.Fatalf("allowed=%v want=%v", got.PublicKeyAllowed(), tc.allowed)
			}
		})
	}
}

func TestGSSAPISelectionRequiresBothPolicyAndPreferredMethod(t *testing.T) {
	for _, test := range []struct {
		settings string
		selected bool
	}{
		{"", false},
		{"GSSAPIAuthentication yes\n", true},
		{"GSSAPIAuthentication yes\nPreferredAuthentications publickey\n", false},
		{"GSSAPIAuthentication yes\nPreferredAuthentications gssapi-with-mic,publickey\n", true},
		{"GSSAPIAuthentication no\nPreferredAuthentications gssapi-with-mic\n", false},
	} {
		connection, err := configuration(t, "Host worker\n"+test.settings, "", nil).Resolve("worker")
		if err != nil || connection.GSSAPISelected() != test.selected {
			t.Fatalf("selection for %q = %v, error=%v", test.settings, connection.GSSAPISelected(), err)
		}
	}
}

func TestConfigurationErrorsRetainSourceWithoutValues(t *testing.T) {
	for _, setting := range []string{"PubkeyAuthentication secret-value", "PreferredAuthentications secret-value", "GSSAPIDelegateCredentials secret-value", "IdentityFile ${secret-value}"} {
		t.Run(setting, func(t *testing.T) {
			c := configuration(t, "Host worker\n"+setting+"\n", "", nil)
			_, err := c.Resolve("worker")
			var configured *Error
			if !errors.As(err, &configured) || configured.File == "" || configured.Line != 2 || configured.Directive == "" {
				t.Fatalf("missing typed source: %v", err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatal("configuration value exposed")
			}
		})
	}
}

func TestUnsupportedAuthenticationVariants(t *testing.T) {
	for _, value := range []string{"host-bound", "unbound", "maybe"} {
		c := configuration(t, "Host worker\nPubkeyAuthentication "+value+"\n", "", nil)
		if _, err := c.Resolve("worker"); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
