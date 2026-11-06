package verifiedexec

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocatePathUsesInstalledMetadataWithoutReadingProgram(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "installed")
	if err = os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o111); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias")
	if err = os.Symlink(executable, alias); err != nil {
		t.Fatal(err)
	}
	requireInstalledPathSelections(t, executable, alias)
	// An installer may replace the selected path; metadata lookup follows the
	// current installation rather than an admitted byte identity.
	next := filepath.Join(directory, "next")
	if err = os.WriteFile(next, []byte("#!/bin/sh\nexit 7\n"), 0o111); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(next, executable); err != nil {
		t.Fatal(err)
	}
	if got, lookupErr := LocatePath(alias); lookupErr != nil || got != executable {
		t.Fatalf("updated selection=%q error=%v", got, lookupErr)
	}
	if err = os.Chmod(executable, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = LocatePath(alias); err == nil {
		t.Fatal("non-executable installation accepted")
	}
	if _, err = LocatePath(directory); err == nil {
		t.Fatal("directory accepted as executable")
	}
	if err = os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	if _, err = LocatePath(alias); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing installed role lost native cause: %v", err)
	}
}

func requireInstalledPathSelections(t *testing.T, executable, alias string) {
	t.Helper()
	for _, selected := range []string{executable, alias} {
		got, lookupErr := LocatePath(selected)
		if lookupErr != nil || got != executable {
			t.Fatalf("selected %q: path=%q error=%v", selected, got, lookupErr)
		}
	}
}
