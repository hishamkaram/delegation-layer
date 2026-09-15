package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCLIVersionAcceptsChangedReportedVersion(t *testing.T) {
	for _, output := range []string{"provider 99.42.7 (nightly)\n", "v2026.09\n", "custom build"} {
		got, err := ParseCLIVersion([]byte(output))
		if err != nil || got != strings.TrimSpace(output) {
			t.Fatalf("version %q: got %q err=%v", output, got, err)
		}
	}
}

func TestParseCLIVersionRejectsInvalidOutput(t *testing.T) {
	for _, output := range [][]byte{nil, []byte("  \n"), []byte("provider\x00version\n"), {0xff}} {
		if _, err := ParseCLIVersion(output); err == nil {
			t.Fatalf("invalid version output %q was accepted", output)
		}
	}
}

func TestContainsCLIFlagRequiresCompleteToken(t *testing.T) {
	if !ContainsCLIFlag([]byte("  --sandbox   --output-last-message\n"), "--sandbox") {
		t.Fatal("advertised flag was not found")
	}
	for _, output := range []string{"--sandboxed", "prefix--sandbox", "--sandbox_value"} {
		if ContainsCLIFlag([]byte(output), "--sandbox") {
			t.Fatalf("partial flag %q was accepted", output)
		}
	}
}

func TestLocateCLIRejectsMissingAndNonExecutable(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	if _, err := LocateCLIPath(missing); err == nil {
		t.Fatal("missing executable was accepted")
	}
	nonExecutable := filepath.Join(root, "not-executable")
	if err := os.WriteFile(nonExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LocateCLIPath(nonExecutable); err == nil {
		t.Fatal("non-executable file was accepted")
	}
}

func TestLocateCLIPathFingerprintsRegularExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' version\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := LocateCLIPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Path == "" || info.SHA256 == "" || info.Version != "" {
		t.Fatalf("unexpected static executable info: %+v", info)
	}
}
