//go:build linux

package lifetime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadLinuxBootID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot-id")
	if err := os.WriteFile(path, []byte("12345678-1234-1234-1234-123456789abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readLinuxBootID(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "linux:12345678-1234-1234-1234-123456789abc" {
		t.Fatalf("boot ID = %q", got)
	}
}

func TestReadLinuxBootIDRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"", "has space", "line\nbreak"} {
		path := filepath.Join(t.TempDir(), "boot-id")
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readLinuxBootID(path); err == nil {
			t.Fatalf("readLinuxBootID accepted %q", value)
		}
	}
}
