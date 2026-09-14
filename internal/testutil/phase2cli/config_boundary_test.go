package phase2cli

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFixtureReadersRefuseSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("finite"), 0o700); err != nil {
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
		if _, err := readRegular(path, 32); err == nil {
			t.Fatalf("read accepted non-regular reference %s", path)
		}
		if _, err := hashRegular(path); err == nil {
			t.Fatalf("hash accepted non-regular reference %s", path)
		}
	}
	if data, err := readRegular(regular, 32); err != nil || string(data) != "finite" {
		t.Fatalf("regular file changed: data=%q err=%v", data, err)
	}
	if digest, err := hashRegular(regular); err != nil || len(digest) != 64 {
		t.Fatalf("regular executable hash failed: digest=%s err=%v", digest, err)
	}
}

func TestFixtureEnvironmentIsExplicitAndUnambiguous(t *testing.T) {
	for _, env := range [][]string{nil, {"NAME"}, {"=value"}, {"A=x\x00"}, {"A=x", "A=y"}} {
		if err := validateEnvironment(env); err == nil {
			t.Fatalf("accepted invalid environment %q", env)
		}
	}
	for _, env := range [][]string{{}, {"A=", "B=literal=value"}} {
		if err := validateEnvironment(env); err != nil {
			t.Fatalf("refused explicit environment %q: %v", env, err)
		}
	}
}
