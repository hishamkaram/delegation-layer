package app

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	lifetimeGuardianPublicFailure     = "error: task lifetime unavailable\n"
	lifetimeGuardianDiagnosticMarker  = "delegate: lifetime guardian failed: "
	lifetimeGuardianSecretSentinel    = "guardian-secret-sentinel-5f0c"
	lifetimeGuardianUnknownStringType = "unknown:*errors.errorString"
)

// TestRunRunnerGuardianFailureRecordsPrivateCause drives the real guardian
// control branch to a pre-registration failure: the saved task is absent, so
// opening its records fails before any worker starts. The state root carries a
// secret sentinel that no diagnostic may disclose.
func TestRunRunnerGuardianFailureRecordsPrivateCause(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := taskdir.InitStore(filepath.Join(base, lifetimeGuardianSecretSentinel))
	if err != nil {
		t.Fatal(err)
	}
	cleanupLifetimeTestResource(t, store)
	missingTaskID := strings.Repeat("e", 32)
	td, cause := store.OpenTask(missingTaskID)
	if cause == nil {
		cleanupLifetimeTestResource(t, td)
		t.Fatal("absent task unexpectedly opened")
	}
	if !errors.Is(cause, fs.ErrNotExist) {
		t.Fatalf("probe cause %v lacks the not-exist identity", cause)
	}

	var stdout, stderr bytes.Buffer
	code := RunRunner([]string{"--guardian", "--supervised", "--root", store.Root, missingTaskID}, &stdout, &stderr, Dependencies{})
	if code != 1 {
		t.Fatalf("guardian exit = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("guardian failure wrote stdout %q", stdout.String())
	}
	requireLifetimeGuardianFailure(t, stderr.String(), "causes="+lifetimeGuardianENOENTIdentity())
}

func TestLifetimeGuardianFailureNamesJoinedIdentitiesWithoutErrorText(t *testing.T) {
	secret := lifetimeGuardianSecretSentinel + "\nforged: line\x1b[31m"
	cause := errors.Join(
		fmt.Errorf("token=%s: %w", secret, task.ErrEvidenceFault),
		fmt.Errorf("argv %s: %w", secret, &fs.PathError{Op: "open", Path: secret, Err: syscall.ENOENT}),
		errors.New(secret),
	)
	var stderr bytes.Buffer
	if code := writeLifetimeGuardianFailure(&stderr, cause); code != 1 {
		t.Fatalf("guardian failure exit = %d, want 1", code)
	}
	requireLifetimeGuardianFailure(t, stderr.String(), "causes=task.ErrEvidenceFault,"+lifetimeGuardianENOENTIdentity()+","+lifetimeGuardianUnknownStringType)
}

func TestLifetimeGuardianFailureBoundsCauseCount(t *testing.T) {
	const omitted = 4
	causes := make([]error, 0, lifetimeGuardianCauseLimit+omitted)
	for range lifetimeGuardianCauseLimit + omitted {
		causes = append(causes, errors.New(lifetimeGuardianSecretSentinel))
	}
	var stderr bytes.Buffer
	if code := writeLifetimeGuardianFailure(&stderr, errors.Join(causes...)); code != 1 {
		t.Fatalf("guardian failure exit = %d, want 1", code)
	}
	want := "causes=" + strings.Repeat(lifetimeGuardianUnknownStringType+",", lifetimeGuardianCauseLimit) + fmt.Sprintf("omitted:%d", omitted)
	requireLifetimeGuardianFailure(t, stderr.String(), want)
}

func TestLifetimeGuardianFailureWriterFaultKeepsExitCode(t *testing.T) {
	if code := writeLifetimeGuardianFailure(errorWriter{err: errors.New("stderr unavailable")}, errors.New("cause")); code != 1 {
		t.Fatalf("guardian failure exit = %d, want 1", code)
	}
}

// requireLifetimeGuardianFailure requires exactly one timestamped private
// diagnostic naming wantCauses, then the unchanged public error line, and no
// injected secret anywhere.
func requireLifetimeGuardianFailure(t *testing.T, stderr, wantCauses string) {
	t.Helper()
	if strings.Contains(stderr, lifetimeGuardianSecretSentinel) {
		t.Fatalf("guardian stderr disclosed the injected sentinel: %q", stderr)
	}
	lines := strings.SplitAfter(stderr, "\n")
	if len(lines) != 3 || lines[2] != "" {
		t.Fatalf("guardian stderr = %q, want one private diagnostic line then one public error line", stderr)
	}
	if lines[1] != lifetimeGuardianPublicFailure {
		t.Fatalf("guardian public error = %q, want %q", lines[1], lifetimeGuardianPublicFailure)
	}
	timestamp, causes, found := strings.Cut(lines[0], lifetimeGuardianDiagnosticMarker)
	if !found || strings.TrimSpace(timestamp) == "" || causes != wantCauses+"\n" {
		t.Fatalf("private diagnostic = %q, want timestamped %q%s", lines[0], lifetimeGuardianDiagnosticMarker, wantCauses)
	}
}

// lifetimeGuardianENOENTIdentity is the native errno identity for ENOENT.
func lifetimeGuardianENOENTIdentity() string {
	return fmt.Sprintf("errno:%d", uintptr(syscall.ENOENT))
}
