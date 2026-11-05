package taskdir

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func ownAdmissionCopies(t *testing.T, fixture *rootAdmissionFixture, files []*os.File) {
	t.Helper()
	t.Cleanup(func() {
		for _, file := range files {
			if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				fixture.failed = true
				t.Error(err)
			}
		}
	})
}

func duplicateAdmission(t *testing.T, fixture *rootAdmissionFixture, lease *RootAdmissionLease) *os.File {
	t.Helper()
	files, err := lease.DuplicateControlLeaseFiles()
	ownAdmissionCopies(t, fixture, files)
	if err != nil || len(files) != 1 {
		t.Fatalf("SH duplicate: count=%d error=%v", len(files), err)
	}
	return files[0]
}

func TestRootAdmissionTransferredParentReleasePreservesCopies(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	lease := fixture.shared(t, fixture.store)
	first := duplicateAdmission(t, fixture, lease)
	second := duplicateAdmission(t, fixture, lease)
	flags, err := unix.FcntlInt(first.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("duplicate not CLOEXEC: %v", err)
	}
	must(t, lease.CheckRootContext(t.Context(), fixture.other(t)))
	// An unlock fault would make an ordinary LockFile.Close fail. Transferred
	// admission must close only, so no LOCK_UN is attempted on the shared OFD.
	fault := &rootAdmissionUnlockFault{name: rootAdmissionLockName, failures: 1, fault: errors.New("unexpected unlock")}
	fixture.store.SetFaultInjector(fault)
	defer fault.clear()
	must(t, lease.Release())
	requireRootAdmissionBusy(t, fixture, fixture.store)
	must(t, first.Close())
	requireRootAdmissionBusy(t, fixture, fixture.store)
	must(t, second.Close())
	fault.clear()
	exclusive := fixture.exclusive(t, fixture.store)
	must(t, exclusive.Release())
	if files, duplicateErr := lease.DuplicateControlLeaseFiles(); len(files) != 0 || !errors.Is(duplicateErr, os.ErrClosed) {
		t.Fatalf("released admission forwarded: %v", duplicateErr)
	}
}

func TestRootAdmissionTransferRefusesNonSharedAuthority(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	for _, lease := range []*RootAdmissionLease{nil, {}, fixture.exclusive(t, fixture.store)} {
		files, err := lease.DuplicateControlLeaseFiles()
		ownAdmissionCopies(t, fixture, files)
		if err == nil || len(files) != 0 {
			t.Fatalf("non-SH admission forwarded: %v", err)
		}
	}
}

func TestRootAdmissionTransferFailureRetainsOriginal(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	lease := fixture.shared(t, fixture.store)
	failure := errors.New("duplicate failed")
	var attempted *os.File
	lease.duplicateFile = func(file *os.File) (*os.File, error) {
		duplicate, err := duplicateRootAdmissionFile(file)
		if err != nil {
			return nil, err
		}
		attempted = duplicate
		return duplicate, failure
	}
	files, err := lease.DuplicateControlLeaseFiles()
	ownAdmissionCopies(t, fixture, files)
	if !errors.Is(err, failure) || len(files) != 0 {
		t.Fatalf("duplicate failure lost: %v", err)
	}
	if attempted == nil {
		t.Fatal("native duplicate not attempted")
	}
	if _, err = attempted.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed copy leaked: %v", err)
	}
	must(t, lease.CheckRootContext(t.Context(), fixture.store))
	requireRootAdmissionBusy(t, fixture, fixture.store)
	lease.duplicateFile = nil
	copy := duplicateAdmission(t, fixture, lease)
	must(t, lease.Release())
	requireRootAdmissionBusy(t, fixture, fixture.store)
	must(t, copy.Close())
	must(t, fixture.exclusive(t, fixture.store).Release())
}

func TestRootAdmissionTransferSerializesRelease(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	lease := fixture.shared(t, fixture.store)
	entered, resume := make(chan struct{}), make(chan struct{})
	lease.duplicateFile = func(file *os.File) (*os.File, error) {
		close(entered)
		<-resume
		return duplicateRootAdmissionFile(file)
	}
	type result struct {
		files []*os.File
		err   error
	}
	duplicated := make(chan result, 1)
	go func() { files, err := lease.DuplicateControlLeaseFiles(); duplicated <- result{files, err} }()
	<-entered
	released := make(chan error, 1)
	go func() { released <- lease.Release() }()
	close(resume)
	outcome := <-duplicated
	ownAdmissionCopies(t, fixture, outcome.files)
	must(t, outcome.err)
	must(t, <-released)
	if len(outcome.files) != 1 {
		t.Fatal("native copy missing")
	}
	requireRootAdmissionBusy(t, fixture, fixture.store)
	must(t, outcome.files[0].Close())
	must(t, fixture.exclusive(t, fixture.store).Release())
}

func TestRootAdmissionTransferredIdentityAndCancellation(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	lease, err := fixture.store.AcquireRootAdmissionContext(ctx)
	fixture.own(lease)
	must(t, err)
	cancel()
	if err = lease.CheckRootContext(ctx, fixture.store); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	copy := duplicateAdmission(t, fixture, lease)
	path := filepath.Join(fixture.store.Root, rootAdmissionLockName)
	must(t, os.Chmod(path, 0o644))
	files, duplicateErr := lease.DuplicateControlLeaseFiles()
	ownAdmissionCopies(t, fixture, files)
	must(t, os.Chmod(path, 0o600))
	if duplicateErr == nil || len(files) != 0 {
		t.Fatal("unsafe transferred inode forwarded")
	}
	saved := path + ".saved"
	must(t, os.Rename(path, saved))
	replacement, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if createErr != nil {
		must(t, os.Rename(saved, path))
		t.Fatal(createErr)
	}
	must(t, replacement.Close())
	files, duplicateErr = lease.DuplicateControlLeaseFiles()
	ownAdmissionCopies(t, fixture, files)
	must(t, os.Remove(path))
	must(t, os.Rename(saved, path))
	if !errors.Is(duplicateErr, task.ErrIdentityMismatch) || len(files) != 0 {
		t.Fatalf("replaced inode forwarded: %v", duplicateErr)
	}
	must(t, lease.CheckRootContext(t.Context(), fixture.store))
	must(t, lease.Release())
	requireRootAdmissionBusy(t, fixture, fixture.store)
	must(t, copy.Close())
}

// This finite child proves inherited OFD retention and release on child exit;
// it does not assert admission policy for any application command.
func TestRootAdmissionTransferInheritedProcess(t *testing.T) {
	if admissionInheritedChildMode() {
		runAdmissionInheritedChild(t)
		return
	}
	fixture := newRootAdmissionFixture(t)
	lease := fixture.shared(t, fixture.store)
	copy := duplicateAdmission(t, fixture, lease)
	input, output, waited := startAdmissionInheritedChild(t, fixture, copy, "root-admission-child")
	ready, err := readAdmissionChildReady(output)
	must(t, err)
	if ready != "ready\n" {
		t.Fatal("child did not confirm inherited file")
	}
	must(t, lease.Release())
	must(t, copy.Close())
	requireRootAdmissionBusy(t, fixture, fixture.store)
	must(t, input.Close())
	must(t, joinAdmissionChild(waited))
	must(t, fixture.exclusive(t, fixture.store).Release())
}

func TestRootAdmissionTransferMissingReadinessClosesChild(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	lease := fixture.shared(t, fixture.store)
	copy := duplicateAdmission(t, fixture, lease)
	input, output, waited := startAdmissionInheritedChild(t, fixture, copy, "root-admission-silent-child")
	must(t, lease.Release())
	must(t, copy.Close())
	requireRootAdmissionBusy(t, fixture, fixture.store)
	_, err := readAdmissionChildReady(output)
	// Retire the actual child before reporting any protocol assertion failure.
	closeErr := input.Close()
	waitErr := joinAdmissionChild(waited)
	must(t, closeErr)
	must(t, waitErr)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("missing readiness did not expire: %v", err)
	}
	must(t, fixture.exclusive(t, fixture.store).Release())
}

func admissionInheritedChildMode() bool {
	if len(os.Args) < 2 {
		return false
	}
	mode := os.Args[len(os.Args)-1]
	return mode == "root-admission-child" || mode == "root-admission-silent-child"
}

func startAdmissionInheritedChild(t *testing.T, fixture *rootAdmissionFixture, copy *os.File, mode string) (io.WriteCloser, *os.File, *chan error) {
	t.Helper()
	binary, err := os.Executable()
	must(t, err)
	binary, err = filepath.EvalSymlinks(binary)
	must(t, err)
	command := exec.Command(binary, "-test.run=^TestRootAdmissionTransferInheritedProcess$", "--", mode)
	command.ExtraFiles = []*os.File{copy}
	input, err := command.StdinPipe()
	must(t, err)
	output, writer, err := os.Pipe()
	if err != nil {
		must(t, input.Close())
		t.Fatal(err)
	}
	command.Stdout = writer
	var waited chan error
	t.Cleanup(func() { finishAdmissionInheritedChild(t, fixture, input, output, &waited) })
	t.Cleanup(func() {
		if closeErr := writer.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			fixture.failed = true
			t.Error(closeErr)
		}
	})
	must(t, command.Start())
	waited = make(chan error, 1)
	// This is the sole Wait owner, registered immediately after successful Start.
	completion := waited
	go func() { completion <- command.Wait() }()
	must(t, writer.Close())
	return input, output, &waited
}

func readAdmissionChildReady(output *os.File) (string, error) {
	if err := output.SetReadDeadline(time.Now().Add(rootAdmissionTestTimeout)); err != nil {
		return "", err
	}
	return bufio.NewReader(output).ReadString('\n')
}

func joinAdmissionChild(waited *chan error) error {
	if *waited == nil {
		return nil
	}
	timer := time.NewTimer(rootAdmissionTestTimeout)
	defer timer.Stop()
	select {
	case err := <-*waited:
		*waited = nil
		return err
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

func TestRootAdmissionTransferredRootReplacementRefused(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	lease := fixture.shared(t, fixture.store)
	copy := duplicateAdmission(t, fixture, lease)
	root := fixture.store.Root
	saved := root + ".original"
	must(t, os.Rename(root, saved))
	must(t, os.Mkdir(root, 0o700))
	files, err := lease.DuplicateControlLeaseFiles()
	ownAdmissionCopies(t, fixture, files)
	must(t, os.Remove(root))
	must(t, os.Rename(saved, root))
	if err == nil || len(files) != 0 {
		t.Fatal("replacement root forwarded")
	}
	must(t, lease.CheckRootContext(t.Context(), fixture.store))
	must(t, lease.Release())
	requireRootAdmissionBusy(t, fixture, fixture.store)
	must(t, copy.Close())
}

func runAdmissionInheritedChild(t *testing.T) {
	file := os.NewFile(3, "inherited-admission")
	if file == nil {
		t.Fatal("missing inherited file")
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := file.Stat(); err != nil {
		t.Fatal(err)
	}
	if os.Args[len(os.Args)-1] != "root-admission-silent-child" {
		if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

func finishAdmissionInheritedChild(t *testing.T, fixture *rootAdmissionFixture, input io.WriteCloser, output io.ReadCloser, waited *chan error) {
	closeErr := input.Close()
	if closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		fixture.failed = true
		t.Error(closeErr)
	}
	if waitErr := joinAdmissionChild(waited); waitErr != nil {
		fixture.failed = true
		t.Errorf("inherited child closure unresolved: %v", waitErr)
	}
	if closeErr = output.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
		fixture.failed = true
		t.Error(closeErr)
	}
}

func TestRootAdmissionTransferFailedExportAlreadyCloseOnly(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	lease := fixture.shared(t, fixture.store)
	lease.duplicateFile = func(*os.File) (*os.File, error) { return nil, unix.EMFILE }
	files, err := lease.DuplicateControlLeaseFiles()
	ownAdmissionCopies(t, fixture, files)
	if !errors.Is(err, unix.EMFILE) || len(files) != 0 {
		t.Fatalf("failed export lost: %v", err)
	}
	must(t, lease.CheckRootContext(t.Context(), fixture.store))
	requireRootAdmissionBusy(t, fixture, fixture.store)
	fault := &rootAdmissionUnlockFault{name: rootAdmissionLockName, failures: 1, fault: errors.New("unexpected unlock after failed export")}
	fixture.store.SetFaultInjector(fault)
	defer fault.clear()
	must(t, lease.Release())
	fault.clear()
	must(t, fixture.exclusive(t, fixture.store).Release())
}

func TestRootAdmissionTransferReadinessDeadline(t *testing.T) {
	output, writer, err := os.Pipe()
	must(t, err)
	t.Cleanup(func() { must(t, output.Close()) })
	t.Cleanup(func() {
		if closeErr := writer.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			t.Error(closeErr)
		}
	})
	read := make(chan error, 1)
	go func() { _, readErr := readAdmissionChildReady(output); read <- readErr }()
	// The independent rescue closes the writer and joins the same read even if
	// the implementation loses its deadline. This fault test cannot strand I/O.
	timer := time.NewTimer(rootAdmissionTestTimeout + time.Second)
	defer timer.Stop()
	select {
	case err = <-read:
	case <-timer.C:
		must(t, writer.Close())
		select {
		case err = <-read:
		case <-time.After(rootAdmissionTestTimeout):
			t.Fatal("owned readiness read closure unresolved after writer close")
		}
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("readiness deadline missing: %v", err)
	}
}
