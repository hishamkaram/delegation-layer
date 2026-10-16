package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestProvisionRetainsWorktreeAddStderrAndExitCode(t *testing.T) {
	fixture := newCheckoutFixture(t)
	if err := os.RemoveAll(fixture.checkout.CheckoutPath); err != nil {
		t.Fatal(err)
	}
	runner := newRepositoryFixtureRunner(fixture.root, fixture.checkout.CheckoutPath, fixture.checkout.AdminDir, testBaseOID)
	baseHandler := runner.handler
	const diagnostic = "fatal: distinctive worktree add diagnostic"
	runner.handler = func(command *exec.Cmd) error {
		if slices.Equal(command.Args[3:], []string{"worktree", "add", "--detach", "--lock", fixture.checkout.CheckoutPath, testBaseOID}) {
			if _, err := io.WriteString(command.Stderr, diagnostic+"\n"); err != nil {
				return err
			}
			return gitTestExitError(128)
		}
		return baseHandler(command)
	}

	_, err := (Git{Runner: runner}).Provision(context.Background(), fixture.checkout.Source, fixture.checkout.CheckoutPath)
	if err == nil {
		t.Fatal("provision unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), diagnostic) {
		t.Fatalf("provision error = %v, want Git stderr %q", err, diagnostic)
	}
	assertGitTestExitCode(t, err, 128)
}

func TestGitRunNilStderrRetainsBoundedFailureDiagnosticAndDrains(t *testing.T) {
	payload := gitCommandOverflowPayload()
	sentinel := gitTestExitError(129)
	written := 0
	runner := &recordingGitRunner{handler: func(command *exec.Cmd) error {
		stderr, ok := command.Stderr.(*boundedBuffer)
		if !ok {
			t.Errorf("stderr writer type = %T, want *boundedBuffer", command.Stderr)
			return sentinel
		}
		n, err := stderr.Write(payload)
		written = n
		if err != nil {
			return err
		}
		if stderr.Len() != 64*1024 || !stderr.exceeded {
			t.Errorf("bounded stderr = len %d, exceeded %t; want len %d, exceeded true", stderr.Len(), stderr.exceeded, 64*1024)
		}
		return sentinel
	}}

	err := (Git{Runner: runner}).run(context.Background(), canonicalWorkspaceTestDir(t), []string{"status"}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("Git command unexpectedly succeeded")
	}
	if written != len(payload) {
		t.Fatalf("stderr bytes reported written = %d, want %d", written, len(payload))
	}
	if !strings.Contains(err.Error(), "fatal: bounded Git stderr prefix") {
		t.Fatalf("error = %v, want retained stderr prefix", err)
	}
	if !strings.Contains(err.Error(), "[git stderr truncated at 65536 bytes]") {
		t.Fatalf("error = %v, want truncation marker", err)
	}
	if strings.Contains(err.Error(), "TAIL-NOT-RETAINED") {
		t.Fatalf("error retained bytes beyond the bounded prefix: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want original error identity", err)
	}
	assertGitTestExitCode(t, err, 129)
}

func TestGitRunNilStderrAllowsSuccessfulOverflow(t *testing.T) {
	payload := gitCommandOverflowPayload()
	runner := &recordingGitRunner{handler: func(command *exec.Cmd) error {
		stderr, ok := command.Stderr.(*boundedBuffer)
		if !ok {
			t.Errorf("stderr writer type = %T, want *boundedBuffer", command.Stderr)
			return errors.New("unexpected stderr writer")
		}
		n, err := stderr.Write(payload)
		if err != nil {
			return err
		}
		if n != len(payload) || stderr.Len() != 64*1024 || !stderr.exceeded {
			t.Errorf("successful overflow write = n %d, len %d, exceeded %t; want n %d, len %d, exceeded true", n, stderr.Len(), stderr.exceeded, len(payload), 64*1024)
		}
		return nil
	}}

	if err := (Git{Runner: runner}).run(context.Background(), canonicalWorkspaceTestDir(t), []string{"status"}, nil, nil, nil, nil); err != nil {
		t.Fatalf("successful Git command with progress stderr: %v", err)
	}
}

func TestGitRunPreservesExplicitStderrWriterWithoutDuplication(t *testing.T) {
	const diagnostic = "provided stderr diagnostic\n"
	sentinel := gitTestExitError(7)
	var captured bytes.Buffer
	runner := &recordingGitRunner{handler: func(command *exec.Cmd) error {
		if _, err := io.WriteString(command.Stderr, diagnostic); err != nil {
			return err
		}
		return sentinel
	}}

	err := (Git{Runner: runner}).run(context.Background(), canonicalWorkspaceTestDir(t), []string{"status"}, nil, nil, nil, &captured)
	if err == nil {
		t.Fatal("Git command unexpectedly succeeded")
	}
	if captured.String() != diagnostic {
		t.Fatalf("captured stderr = %q, want %q", captured.String(), diagnostic)
	}
	if strings.Contains(err.Error(), "provided stderr diagnostic") {
		t.Fatalf("supplied stderr was duplicated into error: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want original error identity", err)
	}
	assertGitTestExitCode(t, err, 7)
}

func assertGitTestExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var exitCoder gitExitCoder
	if !errors.As(err, &exitCoder) {
		t.Fatalf("error = %v, does not preserve an exit code", err)
	}
	if got := exitCoder.ExitCode(); got != want {
		t.Fatalf("exit code = %d, want %d", got, want)
	}
}

func gitCommandOverflowPayload() []byte {
	const limit = 64 * 1024
	prefix := []byte("fatal: bounded Git stderr prefix\n")
	payload := make([]byte, 0, limit+len("TAIL-NOT-RETAINED"))
	payload = append(payload, prefix...)
	payload = append(payload, bytes.Repeat([]byte{'q'}, limit-len(prefix))...)
	payload = append(payload, []byte("TAIL-NOT-RETAINED")...)
	return payload
}
