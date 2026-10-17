package guardian

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRunnerLifetimeLaunchGatePreservesProviderInvocation(t *testing.T) {
	runner := filepath.Join(t.TempDir(), "delegate-run")
	provider := filepath.Join(t.TempDir(), "provider")
	extra, err := os.CreateTemp(t.TempDir(), "provider-extra-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLifetimeLaunchGateTestFile(t, extra) })

	cmd := exec.Command(provider, "--help", "value with spaces")
	cmd.ExtraFiles = []*os.File{extra}
	gate, err := NewLaunchGateFactory(runner)(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gate.Abort(); err != nil {
			t.Errorf("abort launch gate: %v", err)
		}
	})

	wantArgs := []string{
		runner,
		LaunchGateMode,
		lifetimeLaunchGateFD,
		"4",
		"--",
		provider,
		provider,
		"--help",
		"value with spaces",
	}
	if cmd.Path != runner || !reflect.DeepEqual(cmd.Args, wantArgs) {
		t.Fatalf("wrapped command = path %q args %q, want path %q args %q", cmd.Path, cmd.Args, runner, wantArgs)
	}
	if len(cmd.ExtraFiles) != 2 || cmd.ExtraFiles[0] != extra {
		t.Fatalf("wrapped extra files = %d, want original plus gate", len(cmd.ExtraFiles))
	}
}

func TestLifetimeLaunchGateWaitsForReleaseBeforeExec(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLifetimeLaunchGateTestFile(t, writer) })

	provider := filepath.Join(t.TempDir(), "provider")
	providerArgs := []string{"provider-name", "--help"}
	gateFD := duplicateLifetimeLaunchGateFD(t, reader)
	execCalled := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- RunLaunchGate(
			[]string{lifetimeLaunchGateFD, gateFD, "--", provider, providerArgs[0], providerArgs[1]},
			func(path string, args, _ []string) error {
				if path != provider || !reflect.DeepEqual(args, providerArgs) {
					t.Errorf("exec invocation = path %q args %q, want path %q args %q", path, args, provider, providerArgs)
				}
				execCalled <- struct{}{}
				return nil
			},
		)
	}()

	select {
	case <-execCalled:
		t.Fatal("provider exec ran before guardian release")
	default:
	}
	if _, err = writer.Write([]byte{lifetimeLaunchToken}); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-execCalled:
	default:
		t.Fatal("provider exec did not run after guardian release")
	}
}

func TestLifetimeLaunchGateRejectsClosedGate(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(t.TempDir(), "provider")
	gateFD := duplicateLifetimeLaunchGateFD(t, reader)
	executed := false
	err = RunLaunchGate(
		[]string{lifetimeLaunchGateFD, gateFD, "--", provider, "provider"},
		func(string, []string, []string) error {
			executed = true
			return nil
		},
	)
	if err == nil || executed {
		t.Fatalf("closed gate result = err %v executed %t, want error/false", err, executed)
	}
}

func duplicateLifetimeLaunchGateFD(t *testing.T, file *os.File) string {
	t.Helper()
	fd, err := unix.Dup(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		closeErr := unix.Close(fd)
		t.Fatalf("close source: %v; close duplicate: %v", err, closeErr)
	}
	return strconv.Itoa(fd)
}

func closeLifetimeLaunchGateTestFile(t *testing.T, file *os.File) {
	t.Helper()
	if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("close test file: %v", err)
	}
}
