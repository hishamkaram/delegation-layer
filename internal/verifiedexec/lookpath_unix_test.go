//go:build unix

package verifiedexec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLookPathUsesSelectedEnvironment(t *testing.T) {
	ambient, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	name := "selected-program"
	for _, directory := range []string{ambient, first, second} {
		writeEnvironmentExecutable(t, filepath.Join(directory, name), 0o700)
	}
	t.Setenv("PATH", ambient)
	for _, directory := range []string{first, second} {
		path, err := LookPath(name, []string{"PATH=" + directory})
		if err != nil || path != filepath.Join(directory, name) {
			t.Fatalf("lookup = %q, %v; want selected %q", path, err, directory)
		}
	}
	path, err := LookPath(name, []string{"PATH=" + first, "PATH=" + second})
	if err != nil || path != filepath.Join(first, name) {
		t.Fatalf("duplicate PATH lookup = %q, %v; want first entry", path, err)
	}
	if os.Getenv("PATH") != ambient {
		t.Fatal("lookup changed the caller's environment")
	}
}

func TestLookPathMissingOrEmptyPathDoesNotInheritOrSearchCurrentDirectory(t *testing.T) {
	amb := t.TempDir()
	name := "ambient-program"
	writeEnvironmentExecutable(t, filepath.Join(amb, name), 0o700)
	t.Setenv("PATH", amb)
	t.Chdir(amb)
	for _, environment := range [][]string{nil, {}, {"PATH="}, {"PATH=", "PATH=" + amb}} {
		path, err := LookPath(name, environment)
		if path != "" || !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("lookup = %q, %v; want not found with no selected PATH", path, err)
		}
	}
}

func TestLookPathPreservesNativeExplicitExecutableErrors(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "executable")
	plain := filepath.Join(root, "plain")
	writeEnvironmentExecutable(t, executable, 0o700)
	writeEnvironmentExecutable(t, plain, 0o600)
	t.Chdir(root)
	for _, name := range []string{executable, plain, root, filepath.Join(root, "missing"), "./executable", "./plain", "", ".", ".."} {
		wantPath, wantErr := exec.LookPath(name)
		path, err := LookPath(name, nil)
		if path != wantPath {
			t.Fatalf("lookup(%q) = %q; native lookup = %q", name, path, wantPath)
		}
		if wantErr == nil {
			if err != nil {
				t.Fatalf("lookup(%q) = %v; native lookup succeeded", name, err)
			}
			continue
		}
		var native *exec.Error
		if !errors.As(wantErr, &native) {
			t.Fatalf("native lookup(%q) did not return exec.Error: %v", name, wantErr)
		}
		cause := native.Err
		var pathError *os.PathError
		if errors.As(cause, &pathError) {
			cause = pathError.Err
		}
		if !errors.Is(err, cause) || err.Error() != wantErr.Error() {
			t.Fatalf("lookup(%q) = %v; want native error identity %v", name, err, wantErr)
		}
	}
}

func TestLookPathSearchSkipsUnusableCandidates(t *testing.T) {
	first, second, third := t.TempDir(), t.TempDir(), t.TempDir()
	name := "selected-program"
	writeEnvironmentExecutable(t, filepath.Join(first, name), 0o600)
	if err := os.Mkdir(filepath.Join(second, name), 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvironmentExecutable(t, filepath.Join(third, name), 0o700)
	path, err := LookPath(name, []string{"PATH=" + first + string(os.PathListSeparator) + second + string(os.PathListSeparator) + third})
	if err != nil || path != filepath.Join(third, name) {
		t.Fatalf("lookup = %q, %v; want later executable", path, err)
	}
	path, err = LookPath(name, []string{"PATH=" + first + string(os.PathListSeparator) + second})
	if path != "" || !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("unusable search candidates returned %q, %v", path, err)
	}
}

func TestLookPathRelativeSearchPreservesErrDot(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	name := "selected-program"
	writeEnvironmentExecutable(t, filepath.Join(root, name), 0o700)
	if err := os.Mkdir("bin", 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvironmentExecutable(t, filepath.Join(root, "bin", name), 0o700)
	for _, value := range []string{".", "bin", string(os.PathListSeparator) + root} {
		path, err := LookPath(name, []string{"PATH=" + value})
		if path == "" || !errors.Is(err, exec.ErrDot) {
			t.Fatalf("relative lookup = %q, %v; want ErrDot", path, err)
		}
	}
}

func TestLookPathConcurrentEnvironmentsDoNotInterfere(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	name := "selected-program"
	for _, directory := range []string{first, second} {
		writeEnvironmentExecutable(t, filepath.Join(directory, name), 0o700)
	}
	ambient := os.Getenv("PATH")
	start := make(chan struct{})
	type observation struct {
		path string
		err  error
		want string
	}
	results := make(chan observation, 2)
	for _, directory := range []string{first, second} {
		go func() {
			<-start
			path, err := LookPath(name, []string{"PATH=" + directory})
			results <- observation{path: path, err: err, want: filepath.Join(directory, name)}
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.err != nil || result.path != result.want {
			t.Errorf("concurrent lookup = %q, %v; want %q", result.path, result.err, result.want)
		}
	}
	if os.Getenv("PATH") != ambient {
		t.Fatal("concurrent lookups changed ambient PATH")
	}
}
