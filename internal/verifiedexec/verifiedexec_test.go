package verifiedexec

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortableCommandPreservesExecutableDirectoryLayout(t *testing.T) {
	executable, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := LocatePath(executable)
	if err != nil {
		t.Fatal(err)
	}
	source, err := Open(provider.Path, provider.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	temporaryDir := t.TempDir()
	command, temporaryFiles, err := buildPortableCommand(provider.Path, provider.SHA256, "", nil, nil, source, temporaryDir)
	if err != nil {
		if closeErr := source.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := source.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if cleanupErr := removeTemporaryFiles(temporaryFiles); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	command.Stdout = new(bytes.Buffer)
	command.Stderr = new(bytes.Buffer)
	if err = command.Run(); err != nil {
		t.Fatal(err)
	}
	relativeDirectory, err := filepath.Rel(string(filepath.Separator), filepath.Dir(provider.Path))
	if err != nil {
		t.Fatal(err)
	}
	expectedPrefix := filepath.Join(temporaryDir, "executable", relativeDirectory) + string(filepath.Separator)
	if !strings.HasPrefix(command.Path, expectedPrefix) {
		t.Fatalf("portable executable path = %q, want private mirror under %q", command.Path, expectedPrefix)
	}
}

func TestPortableCommandPreservesRelativeNodeImports(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin#runtime")
	if err = os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	nodeBytes, err := os.ReadFile(node)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(bin, "node"), nodeBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "helper.mjs"), []byte("console.log('relative import resolved')\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	providerPath := filepath.Join(root, "provider#cli")
	if err = os.WriteFile(providerPath, []byte("#!/usr/bin/env node\nimport './helper.mjs'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := LocatePath(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := Open(provider.Path, provider.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	temporaryDir := t.TempDir()
	command, temporaryFiles, err := buildPortableCommand(provider.Path, provider.SHA256, root, []string{"PATH=" + bin}, nil, source, temporaryDir)
	if err != nil {
		if closeErr := source.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := source.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if cleanupErr := removeTemporaryFiles(temporaryFiles); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	command.Dir = root
	command.Env = []string{"PATH=" + bin}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err = command.Run(); err != nil {
		t.Fatalf("portable node command failed: %v (%s)", err, output.String())
	}
	if output.String() != "relative import resolved\n" {
		t.Fatalf("unexpected portable node output: %q", output.String())
	}
}

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
