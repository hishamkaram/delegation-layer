package fakeprovider

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProviderConfigRefusesSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link, pipe := filepath.Join(dir, "link"), filepath.Join(dir, "pipe")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, pipe} {
		if _, err := LoadProviderConfig(path); err == nil {
			t.Fatalf("accepted non-regular provider configuration %s", path)
		}
	}
}
