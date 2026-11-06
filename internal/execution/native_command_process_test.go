package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/testutil/goroutinecheck"
)

func TestMain(m *testing.M) {
	if code, handled := RunCommandOwnerMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(goroutinecheck.Run(m))
}

func TestGoroutineLeakGuard(t *testing.T) { goroutinecheck.VerifyInstalled(t) }

func TestNativeCommandExitHelper(t *testing.T) {
	switch os.Getenv("DELEGATE_NATIVE_COMMAND_TEST") {
	case "success":
		if _, err := os.Stdout.WriteString("preserved output\n"); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	case "exit128":
		os.Exit(128)
	case "native-bytes":
		if os.Args[len(os.Args)-2] != "" {
			t.Fatal("empty native argument changed")
		}
		marker, err := os.ReadFile("native-byte-probe")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stdout.Write(append([]byte(os.Args[len(os.Args)-1]), marker...)); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
}

func TestNativeCommandDedicatedOwnerPreservesNativeArgumentAndDirectoryBytes(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if runtime.GOOS == "linux" {
		directory = filepath.Join(directory, "project-\xff")
		if mkdirErr := os.Mkdir(directory, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		replacement := filepath.Join(filepath.Dir(directory), "project-\ufffd")
		if mkdirErr := os.Mkdir(replacement, 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		if writeErr := os.WriteFile(filepath.Join(replacement, "native-byte-probe"), []byte("wrong-directory"), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	marker := []byte("correct-directory")
	if writeErr := os.WriteFile(filepath.Join(directory, "native-byte-probe"), marker, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	argument := []byte("literal-\xfe")
	want := append(append([]byte{}, argument...), marker...)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.Command(executable, "-test.run=^TestNativeCommandExitHelper$", "--", "", string(argument))
	command.Dir = directory
	command.Env = append(os.Environ(), "DELEGATE_NATIVE_COMMAND_TEST=native-bytes")
	var output bytes.Buffer
	command.Stdout = &output
	if runErr := RunIsolatedCommand(ctx, executable, command); runErr != nil || !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("native command bytes changed: output=%x want=%x error=%v", output.Bytes(), want, runErr)
	}
}

func TestNativeCommandDedicatedOwnerPreservesOutputAndNaturalExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"success", "exit128"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			command := exec.Command(executable, "-test.run=^TestNativeCommandExitHelper$")
			command.Dir = t.TempDir()
			command.Env = append(os.Environ(), "DELEGATE_NATIVE_COMMAND_TEST="+mode)
			var output bytes.Buffer
			command.Stdout = &output
			runErr := RunIsolatedCommand(ctx, executable, command)
			if mode == "success" {
				if runErr != nil || !bytes.Equal(output.Bytes(), []byte("preserved output\n")) {
					t.Fatalf("output/exit lost: %q, %v", output.String(), runErr)
				}
				return
			}
			var exit interface{ ExitCode() int }
			if !errors.As(runErr, &exit) || exit.ExitCode() != 128 {
				t.Fatalf("natural exit not transported: %v", runErr)
			}
		})
	}
}
