package verifiedexec

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestNewCommandInDirectoryResolvesRelativePathFromLaunchDirectory(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", filepath.Join(bin, "sh")); err != nil {
		t.Fatal(err)
	}
	providerPath := filepath.Join(root, "provider-cli")
	if err := os.WriteFile(providerPath, []byte("#!/usr/bin/env sh\nprintf 'relative path resolved\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := LocatePath(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	command, err := NewCommandInDirectory(provider.Path, provider.SHA256, root, []string{"PATH=bin"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := command.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	var output bytes.Buffer
	command.Cmd.Dir = root
	command.Cmd.Stdout = &output
	if err = command.Cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "relative path resolved\n" {
		t.Fatalf("unexpected output: %q", output.String())
	}
}
