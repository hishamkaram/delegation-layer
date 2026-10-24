//go:build unix

package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMaintenanceOwnerUsesExplicitEnvironment(t *testing.T) {
	t.Setenv("LANG", "issue91-caller")
	for _, test := range []struct {
		name        string
		environment []string
		want        string
	}{
		{name: "nil is explicit empty", want: "unset"},
		{name: "empty", environment: []string{}, want: "unset"},
		{name: "selected", environment: []string{"LANG=issue91-selected"}, want: "issue91-selected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := runMaintenanceOwnerMarker(t, func(ctx context.Context, executable, root string) (<-chan error, error) {
				return StartQueueMaintenanceOwnerWithEnvironment(ctx, executable, root, test.environment)
			})
			if got != test.want {
				t.Fatalf("maintenance marker = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMaintenanceOwnerCompatibilityStartKeepsBoundedCurrentEnvironment(t *testing.T) {
	t.Setenv("LANG", "issue91-caller")
	got := runMaintenanceOwnerMarker(t, StartQueueMaintenanceOwner)
	if got != "issue91-caller" {
		t.Fatalf("compatibility start marker = %q, want the bounded current environment", got)
	}
}

func runMaintenanceOwnerMarker(t *testing.T, start func(context.Context, string, string) (<-chan error, error)) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "maintenance-owner")
	script := "#!/bin/sh\nprintf '%s' \"${LANG-unset}\" >\"$3/marker\"\n"
	if err = os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	done, err := start(t.Context(), executable, root)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatalf("maintenance child Wait: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(root, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	return string(marker)
}
