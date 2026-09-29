package verifiedexec

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func TestShouldMirrorParentStopsAtSharedTemporaryRoots(t *testing.T) {
	temporaryRoot := filepath.Clean(os.TempDir())
	if shouldMirrorParent(temporaryRoot) {
		t.Fatalf("shared temporary root %q would be enumerated", temporaryRoot)
	}
	if !shouldMirrorParent(filepath.Join(temporaryRoot, "provider-package")) {
		t.Fatalf("provider-owned child of temporary root was not eligible for mirroring")
	}
}

func TestCanonicalDarwinSystemExecutableExcludesWritableDataVolume(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin system-volume policy")
	}
	if useCanonicalDarwinSystemExecutable("/System/Volumes/Data/Users/provider") {
		t.Fatal("writable Data-volume path was treated as sealed system code")
	}
	if !useCanonicalDarwinSystemExecutable("/usr/bin/sh") {
		t.Fatal("sealed system executable was not kept canonical")
	}
}

func TestPortableCommandPreservesRelativeNodeImports(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
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
	command, temporaryFiles, err := buildPortableCommand(provider.Path, provider.SHA256, root, []string{"PATH=" + filepath.Dir(node)}, nil, source, temporaryDir)
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
	command.Env = []string{"PATH=" + filepath.Dir(node)}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err = command.Run(); err != nil {
		t.Fatalf("portable node command failed: %v (%s)", err, output.String())
	}
	if output.String() != "relative import resolved\n" {
		t.Fatalf("unexpected portable node output: %q", output.String())
	}
}

func TestPortableCommandPreservesParentRelativeNodeImports(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	packageRoot := filepath.Join(root, "package")
	bin := filepath.Join(packageRoot, "bin")
	nodeModules := filepath.Join(packageRoot, "node_modules")
	for _, directory := range []string{bin, nodeModules} {
		if mkdirErr := os.MkdirAll(directory, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
	}
	if err = os.WriteFile(filepath.Join(nodeModules, "helper.mjs"), []byte("console.log('parent import resolved')\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	providerPath := filepath.Join(bin, "provider#cli")
	if err = os.WriteFile(providerPath, []byte("#!/usr/bin/env node\nimport '../node_modules/helper.mjs'\n"), 0o700); err != nil {
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
	command, temporaryFiles, err := buildPortableCommand(provider.Path, provider.SHA256, packageRoot, []string{"PATH=" + filepath.Dir(node)}, nil, source, temporaryDir)
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
	command.Dir = packageRoot
	command.Env = []string{"PATH=" + filepath.Dir(node)}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err = command.Run(); err != nil {
		t.Fatalf("portable node parent-relative command failed: %v (%s)", err, output.String())
	}
	if output.String() != "parent import resolved\n" {
		t.Fatalf("unexpected portable parent-relative output: %q", output.String())
	}
}

func TestPortableCommandPreservesNestedParentRelativeNodeImports(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	packageRoot := filepath.Join(root, "package")
	bin := filepath.Join(packageRoot, "bin", "sub")
	nodeModules := filepath.Join(packageRoot, "node_modules")
	for _, directory := range []string{bin, nodeModules} {
		if mkdirErr := os.MkdirAll(directory, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
	}
	if err = os.WriteFile(filepath.Join(nodeModules, "helper.mjs"), []byte("console.log('nested import resolved')\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	providerPath := filepath.Join(bin, "provider#cli")
	if err = os.WriteFile(providerPath, []byte("#!/usr/bin/env node\nimport '../../node_modules/helper.mjs'\n"), 0o700); err != nil {
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
	command, temporaryFiles, err := buildPortableCommand(provider.Path, provider.SHA256, packageRoot, []string{"PATH=" + filepath.Dir(node)}, nil, source, temporaryDir)
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
	command.Dir = packageRoot
	command.Env = []string{"PATH=" + filepath.Dir(node)}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err = command.Run(); err != nil {
		t.Fatalf("portable node nested parent-relative command failed: %v (%s)", err, output.String())
	}
	if output.String() != "nested import resolved\n" {
		t.Fatalf("unexpected portable nested parent-relative output: %q", output.String())
	}
}

func TestNewCommandInDirectoryResolvesRelativePathFromLaunchDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if mkdirErr := os.Mkdir(bin, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	if symlinkErr := os.Symlink("/bin/sh", filepath.Join(bin, "sh")); symlinkErr != nil {
		t.Fatal(symlinkErr)
	}
	providerPath := filepath.Join(root, "provider-cli")
	if writeErr := os.WriteFile(providerPath, []byte("#!/usr/bin/env sh\nprintf 'relative path resolved\\n'\n"), 0o700); writeErr != nil {
		t.Fatal(writeErr)
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
