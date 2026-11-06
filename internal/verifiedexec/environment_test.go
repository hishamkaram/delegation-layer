package verifiedexec

import (
	"os"
	"testing"
)

func TestTemporaryRootDoesNotInheritAmbientEnvironment(t *testing.T) {
	t.Setenv("TMPDIR", "")
	nativeDefault := os.TempDir()
	ambient, selected := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", ambient)
	for _, environment := range [][]string{nil, {}, {"TMPDIR="}} {
		if root := TemporaryRoot(environment); root != nativeDefault {
			t.Fatalf("temporary root = %q; want native default %q", root, nativeDefault)
		}
	}
	if root := TemporaryRoot([]string{"TMPDIR=" + selected}); root != selected {
		t.Fatalf("temporary root = %q; want selected %q", root, selected)
	}
}

func TestEnvironmentValueReadsFirstWellFormedEntryWithoutAmbientFallback(t *testing.T) {
	t.Setenv("DELEGATE_ENVIRONMENT_VALUE_TEST", "ambient")
	tests := []struct {
		name        string
		environment []string
		want        string
	}{
		{name: "first entry wins", environment: []string{"KEY=first", "KEY=second"}, want: "first"},
		{name: "empty first entry wins", environment: []string{"KEY=", "KEY=second"}, want: ""},
		{name: "malformed entry ignored", environment: []string{"KEY", "KEYX=other", "KEY=value"}, want: "value"},
		{name: "missing key is empty", environment: []string{"OTHER=value"}, want: ""},
		{name: "nil never reads ambient", environment: nil, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key := "KEY"
			if test.environment == nil {
				key = "DELEGATE_ENVIRONMENT_VALUE_TEST"
			}
			if got := EnvironmentValue(test.environment, key); got != test.want {
				t.Fatalf("EnvironmentValue(%q, %q) = %q, want %q", test.environment, key, got, test.want)
			}
		})
	}
}

func writeEnvironmentExecutable(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
}
