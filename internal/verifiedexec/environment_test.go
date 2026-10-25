package verifiedexec

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNewCommandWithoutSelectedPathUsesLocalInterpreter(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	env, err := exec.LookPath("env")
	if err != nil {
		t.Fatal(err)
	}
	launch, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if symlinkErr := os.Symlink(shell, filepath.Join(launch, "sh")); symlinkErr != nil {
		t.Fatal(symlinkErr)
	}
	ambient := t.TempDir()
	writeEnvironmentExecutable(t, filepath.Join(ambient, "sh"), 0o700)
	providerPath := filepath.Join(launch, "provider-cli")
	if writeErr := os.WriteFile(providerPath, []byte("#!"+env+" sh\nprintf 'LOCAL_INTERPRETER'\n"), 0o700); writeErr != nil {
		t.Fatal(writeErr)
	}
	provider, err := LocatePath(providerPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ambient)
	command, err := NewCommandInDirectory(provider.Path, provider.SHA256, launch, []string{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := command.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	command.Cmd.Dir, command.Cmd.Env = launch, []string{}
	var output bytes.Buffer
	command.Cmd.Stdout, command.Cmd.Stderr = &output, &output
	if err := command.Cmd.Run(); err != nil {
		t.Fatalf("local interpreter failed: %v (%s)", err, output.String())
	}
	if output.String() != "LOCAL_INTERPRETER" {
		t.Fatalf("local interpreter output = %q", output.String())
	}
}

func TestInterpreterWithoutSelectedPathUsesLaunchDirectory(t *testing.T) {
	ambient, launch := t.TempDir(), t.TempDir()
	name := "selected-interpreter"
	writeEnvironmentExecutable(t, filepath.Join(ambient, name), 0o700)
	writeEnvironmentExecutable(t, filepath.Join(launch, name), 0o700)
	t.Setenv("PATH", ambient)
	for _, environment := range [][]string{nil, {}, {"PATH="}} {
		path, err := locateInEnvironment(name, environment, launch)
		if err != nil || path != filepath.Join(launch, name) {
			t.Fatalf("interpreter = %q, %v; want launch-directory executable", path, err)
		}
	}
}

func TestInterpreterWithoutSelectedPathDoesNotEscapeLaunchDirectory(t *testing.T) {
	ambient, launch := t.TempDir(), t.TempDir()
	name := "ambient-only-interpreter"
	writeEnvironmentExecutable(t, filepath.Join(ambient, name), 0o700)
	t.Setenv("PATH", ambient)
	for _, environment := range [][]string{nil, {}, {"PATH="}} {
		if path, err := locateInEnvironment(name, environment, launch); err == nil {
			t.Fatalf("interpreter escaped selected environment: %q", path)
		}
	}
}

func TestInterpreterPreservesFirstSelectedPath(t *testing.T) {
	first, last := t.TempDir(), t.TempDir()
	name := "selected-interpreter"
	writeEnvironmentExecutable(t, filepath.Join(first, name), 0o700)
	writeEnvironmentExecutable(t, filepath.Join(last, name), 0o700)
	path, err := locateInEnvironment(name, []string{"PATH=" + first, "PATH=" + last}, t.TempDir())
	if err != nil || path != filepath.Join(first, name) {
		t.Fatalf("interpreter = %q, %v; want first selected PATH", path, err)
	}
}

func TestPortableCommandUsesSelectedTemporaryDirectory(t *testing.T) {
	ambient, selected := t.TempDir(), t.TempDir()
	t.Setenv("TMPDIR", ambient)
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
	command, err := newPortableCommand(provider.Path, provider.SHA256, "", []string{"TMPDIR=" + selected}, nil, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := command.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if filepath.Dir(command.temporaryDir) != selected {
		t.Fatalf("temporary parent = %q; want selected %q", filepath.Dir(command.temporaryDir), selected)
	}
	entries, err := os.ReadDir(ambient)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("ambient temporary directory changed: %d entries", len(entries))
	}
}

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

func TestShouldMirrorParentUsesSelectedTemporaryRootAndCanonicalAlias(t *testing.T) {
	ambient := t.TempDir()
	selected, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "temporary-alias")
	if err := os.Symlink(selected, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", ambient)
	environment := []string{"TMPDIR=" + alias}
	for _, path := range []string{selected, alias} {
		if shouldMirrorParent(path, environment) {
			t.Fatalf("selected temporary root %q would be enumerated", path)
		}
	}
	if !shouldMirrorParent(filepath.Join(selected, "provider-package"), environment) {
		t.Fatal("provider-owned directory was not eligible for mirroring")
	}
}

func TestShadowDirectoryDoesNotEnumerateSelectedTemporaryRoot(t *testing.T) {
	selected := t.TempDir()
	provider := filepath.Join(selected, "package", "bin")
	if err := os.MkdirAll(provider, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvironmentExecutable(t, filepath.Join(selected, "unrelated"), 0o600)
	mirror, err := shadowDirectory(provider, t.TempDir(), "provider-cli", []string{"TMPDIR=" + selected})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(filepath.Dir(mirror)), "unrelated")); !os.IsNotExist(err) {
		t.Fatalf("shared temporary sibling was mirrored: %v", err)
	}
}

func writeEnvironmentExecutable(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("executable fixture\n"), mode); err != nil {
		t.Fatal(err)
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
