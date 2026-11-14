package app

import (
	"strings"
	"testing"
)

func TestGatewayCommandGrammarAndExplicitPolicy(t *testing.T) {
	for _, argv := range [][]string{
		{"gateway", "connect", "https://gateway.example", "--headless", "--provider", "claude:print", "--json"},
		{"gateway", "connect", "https://gateway.example", "--headless", "--token-file", "-"},
		{"gateway", "workspace", "allow", ".", "--alias", "approved"},
		{"gateway", "workspace", "update", strings.Repeat("a", 32), "--alias", "renamed", "--expected-revision", "1"},
		{"gateway", "workspace", "remove", strings.Repeat("a", 32), "--expected-revision", "2"},
		{"gateway", "status"},
		{"gateway", "doctor"},
		{"gateway", "disconnect"},
		{"gateway", "disconnect", "--revoke", "--headless"},
		{"gateway", "connect", "https://gateway.example", "--foreground"},
		{"gateway", "serve"},
		{"gateway", "service", "install"},
		{"gateway", "service", "start"},
		{"gateway", "service", "stop"},
		{"gateway", "service", "status"},
		{"gateway", "service", "uninstall"},
	} {
		if _, err := ParseArguments(argv); err != nil {
			t.Fatalf("parse %v: %v", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"gateway", "connect", "http://gateway.example"},
		{"gateway", "connect", "https://gateway.example/mcp"},
		{"gateway", "connect", "https://gateway.example", "--model", "selected"},
		{"gateway", "connect", "https://gateway.example", "--provider", "claude:print", "--provider", "codex:exec"},
		{"gateway", "connect", "https://gateway.example", "--root", "relative"},
		{"gateway", "workspace", "allow"},
		{"gateway", "workspace", "update", strings.Repeat("a", 32), "--alias", "changed"},
		{"gateway", "workspace", "update", strings.Repeat("a", 32), "--expected-revision", "1"},
		{"gateway", "serve", "extra"},
		{"gateway", "disconnect", "--headless"},
		{"gateway", "service", "restart"},
	} {
		if _, err := ParseArguments(argv); err == nil {
			t.Fatalf("accepted %v", argv)
		}
	}
}

func TestGatewayHelpUsesRegisteredNestedFlags(t *testing.T) {
	for _, tc := range []struct {
		path  []string
		flags []string
	}{
		{path: []string{"gateway", "connect"}, flags: []string{"--profile", "--foreground", "--headless", "--token-file"}},
		{path: []string{"gateway", "workspace", "allow"}, flags: []string{"--profile", "--alias"}},
		{path: []string{"gateway", "disconnect"}, flags: []string{"--profile", "--revoke", "--headless"}},
		{path: []string{"gateway", "service", "start"}, flags: []string{"--profile"}},
		{path: []string{"gateway", "serve"}, flags: []string{"--profile"}},
	} {
		text, err := HelpText(tc.path)
		if err != nil {
			t.Fatalf("missing nested help for %v: %v", tc.path, err)
		}
		for _, flag := range tc.flags {
			if !strings.Contains(text, flag) {
				t.Fatalf("missing %s help for %v", flag, tc.path)
			}
		}
	}
}
