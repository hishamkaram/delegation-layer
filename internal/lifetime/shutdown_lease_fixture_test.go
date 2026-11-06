//go:build linux || darwin

package lifetime

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	shutdownLeaseMode   = "DELEGATE_SHUTDOWN_LEASE_FIXTURE"
	shutdownLeaseSecret = "owned-shutdown-lease-gate"
)

// Each direct command has one Wait owner, installed on every successful Start.
// Cancellation settles that exact child through lifetime's native signal policy.
type shutdownLeaseChild struct {
	command *exec.Cmd
	ref     ProcessRef
	done    chan struct{}
	waitErr error
}

func startShutdownLeaseChild(command *exec.Cmd) (*shutdownLeaseChild, error) {
	child := &shutdownLeaseChild{command: command, done: make(chan struct{})}
	if err := command.Start(); err != nil {
		return nil, err
	}
	var observeErr error
	child.ref, observeErr = ObserveStartedProcess(command.Process.Pid)
	go func() { child.waitErr = command.Wait(); close(child.done) }()
	return child, observeErr
}

func (c *shutdownLeaseChild) wait(ctx context.Context) error {
	select {
	case <-c.done:
		return c.waitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *shutdownLeaseChild) close(ctx context.Context) error {
	select {
	case <-c.done:
		return nil
	default:
	}
	if err := validateProcessRef(c.ref); err != nil {
		return fmt.Errorf("retained unverified direct child %d: %w", c.command.Process.Pid, err)
	}
	signalErr := shutdownLeaseSignal(ctx, c.ref, syscall.SIGKILL)
	if errors.Is(signalErr, errProcessNoSuchEntity) {
		signalErr = nil
	}
	waitErr := c.wait(ctx)
	var exited *exec.ExitError
	if errors.As(waitErr, &exited) {
		waitErr = nil
	}
	return errors.Join(signalErr, waitErr, WaitProcessAbsent(ctx, c.ref))
}

func finalizeShutdownLeaseChild(ctx context.Context, child *shutdownLeaseChild) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return child.close(cleanupCtx)
}

func settleShutdownLeaseChild(ctx context.Context, child *shutdownLeaseChild) error {
	waitErr := child.wait(ctx)
	return errors.Join(waitErr, finalizeShutdownLeaseChild(ctx, child))
}

func runShutdownLeaseCommand(ctx context.Context, command *exec.Cmd) ([]byte, error) {
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	child, startErr := startShutdownLeaseChild(command)
	if child == nil {
		return nil, startErr
	}
	settleErr := settleShutdownLeaseChild(ctx, child)
	// The buffer is read only after the sole Wait has closed the output copier.
	select {
	case <-child.done:
		return output.Bytes(), errors.Join(startErr, settleErr)
	default:
		return nil, errors.Join(startErr, settleErr)
	}
}

func shutdownLeaseFixtureCommand(directory, mode, role, binary string, deadline time.Time) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := exec.Command(executable, "-test.run=^TestShutdownLeaseFixture$", "-test.timeout=45s")
	command.Dir = directory
	command.Env = []string{
		"HOME=" + directory, "PATH=/usr/bin:/bin", "GORACE=atexit_sleep_ms=0", shutdownLeaseMode + "=" + mode,
		"DELEGATE_SHUTDOWN_LEASE_DIRECTORY=" + directory, "DELEGATE_SHUTDOWN_LEASE_ROLE=" + role,
		"DELEGATE_SHUTDOWN_LEASE_BINARY=" + binary, "DELEGATE_SHUTDOWN_LEASE_DEADLINE=" + strconv.FormatInt(deadline.UnixNano(), 10),
	}
	return command, nil
}

func TestShutdownLeaseFixture(t *testing.T) {
	mode := os.Getenv(shutdownLeaseMode)
	if mode == "" {
		t.Skip("fixed native shutdown lease helper")
	}
	deadline, err := strconv.ParseInt(os.Getenv("DELEGATE_SHUTDOWN_LEASE_DEADLINE"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, deadline))
	defer cancel()
	if err = runShutdownLeaseFixture(ctx, mode); err != nil {
		t.Fatal(err)
	}
}

func runShutdownLeaseFixture(ctx context.Context, mode string) error {
	directory := os.Getenv("DELEGATE_SHUTDOWN_LEASE_DIRECTORY")
	switch mode {
	case "caller":
		return shutdownLeaseCaller(ctx, directory)
	case "owner":
		return shutdownLeaseOwner(ctx, directory)
	case "role":
		return shutdownLeaseRole(ctx, directory)
	case "driver-early", "driver-retained":
		return shutdownLeaseCleanupFixture(ctx, directory, mode)
	case "version-gate":
		ref, err := ObserveProcess(os.Getpid())
		if err != nil {
			return err
		}
		if err = writeShutdownLeaseIdentity(directory, "version", ref); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	default:
		return errors.New("unknown fixed shutdown lease fixture mode")
	}
}

func shutdownLeaseInherited() ([]*os.File, error) {
	files := make([]*os.File, 0, 7)
	for fd := 3; fd <= 9; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			return files, err
		}
		unix.CloseOnExec(fd)
		if fd >= 8 {
			if err := unix.SetNonblock(fd, true); err != nil {
				return files, err
			}
		}
		files = append(files, os.NewFile(uintptr(fd), "shutdown-lease-inherited"))
	}
	return files, nil
}

func shutdownLeaseMayLaunch(ctx context.Context, directory string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := os.Stat(filepath.Join(directory, "abort"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.Join(errors.New("native fixture launches closed"), err)
}

func shutdownLeaseCaller(ctx context.Context, directory string) (resultErr error) {
	if err := shutdownLeaseMayLaunch(ctx, directory); err != nil {
		return err
	}
	files, err := shutdownLeaseInherited()
	defer func() { resultErr = errors.Join(resultErr, closeShutdownLeaseFiles(files)) }()
	if err != nil {
		return err
	}
	command, err := shutdownLeaseFixtureCommand(directory, "owner", os.Getenv("DELEGATE_SHUTDOWN_LEASE_ROLE"), os.Getenv("DELEGATE_SHUTDOWN_LEASE_BINARY"), shutdownLeaseDeadline(ctx))
	if err != nil {
		return err
	}
	command.ExtraFiles = files
	child, err := startShutdownLeaseChild(command)
	if child == nil {
		return err
	}
	if err != nil {
		return errors.Join(err, finalizeShutdownLeaseChild(ctx, child))
	}
	if err = writeShutdownLeaseIdentity(directory, "owner", child.ref); err != nil {
		return errors.Join(err, finalizeShutdownLeaseChild(ctx, child))
	}
	// The direct child already has its single Wait. After handoff this caller
	// exits; the native adopter owns any later Wait, never the proof driver.
	if err = waitShutdownLeaseFile(ctx, directory, "cli.json"); err != nil {
		return errors.Join(err, finalizeShutdownLeaseChild(ctx, child))
	}
	return nil
}

func shutdownLeaseOwner(ctx context.Context, directory string) (resultErr error) {
	if err := shutdownLeaseMayLaunch(ctx, directory); err != nil {
		return err
	}
	files, err := shutdownLeaseInherited()
	defer func() { resultErr = errors.Join(resultErr, closeShutdownLeaseFiles(files)) }()
	if err != nil {
		return err
	}
	command, err := shutdownLeaseActuator(ctx, directory)
	if err != nil {
		return err
	}
	if os.Getenv("DELEGATE_SHUTDOWN_LEASE_ROLE") == "shutdown" {
		command.ExtraFiles = []*os.File{files[0]}
	}
	child, err := startShutdownLeaseChild(command)
	if child == nil {
		return err
	}
	if err != nil {
		return errors.Join(err, finalizeShutdownLeaseChild(ctx, child))
	}
	if err = writeShutdownLeaseIdentity(directory, "cli", child.ref); err != nil {
		return errors.Join(err, finalizeShutdownLeaseChild(ctx, child))
	}
	control := files[6]
	if err = control.SetReadDeadline(shutdownLeaseDeadline(ctx)); err != nil {
		return errors.Join(err, finalizeShutdownLeaseChild(ctx, child))
	}
	var instruction [1]byte
	_, readErr := control.Read(instruction[:])
	return errors.Join(readErr, finalizeShutdownLeaseChild(ctx, child))
}

func shutdownLeaseDeadline(ctx context.Context) time.Time {
	deadline, _ := ctx.Deadline()
	return deadline
}

func shutdownLeaseActuator(ctx context.Context, directory string) (*exec.Cmd, error) {
	role, binary := os.Getenv("DELEGATE_SHUTDOWN_LEASE_ROLE"), os.Getenv("DELEGATE_SHUTDOWN_LEASE_BINARY")
	if role == "shutdown" || role == "status" || role == "log" || role == "edit" {
		arguments := []string{"-c", filepath.Join(directory, "config.json"), role}
		if role == "log" || role == "edit" {
			arguments = append(arguments, "1")
		}
		command := exec.Command(binary, arguments...)
		command.Env = []string{"HOME=" + directory, "PATH=/usr/bin:/bin", "LANG=C"}
		return command, nil
	}
	return shutdownLeaseFixtureCommand(directory, "role", role, binary, shutdownLeaseDeadline(ctx))
}

func shutdownLeaseRole(ctx context.Context, directory string) (resultErr error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(directory, "s"))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	if err = connection.SetDeadline(shutdownLeaseDeadline(ctx)); err != nil {
		return err
	}
	if err = writeShutdownLeaseFrame(connection, []byte(shutdownLeaseSecret)); err != nil {
		return err
	}
	var release [1]byte
	_, err = connection.Read(release[:])
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

type shutdownLeaseIdentity struct {
	PID   int    `json:"pid"`
	Token string `json:"token"`
}

func writeShutdownLeaseIdentity(directory, name string, ref ProcessRef) error {
	return publishNativeFixtureRecord(filepath.Join(directory, name+".json"), false, func(writer io.Writer) error {
		return json.NewEncoder(writer).Encode(shutdownLeaseIdentity{PID: ref.PID(), Token: ref.StartToken()})
	})
}

func waitShutdownLeaseFile(ctx context.Context, directory, name string) error {
	return waitForEmpty(ctx, 10*time.Millisecond, func() (bool, error) {
		_, err := os.Stat(filepath.Join(directory, name))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}, sleepContext)
}

func readShutdownLeaseFrame(reader io.Reader) ([]byte, error) {
	var header [8]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint64(header[:])
	if size > 4096 {
		return nil, errors.New("oversized shutdown fixture frame")
	}
	data := make([]byte, int(size))
	_, err := io.ReadFull(reader, data)
	return data, err
}

func writeShutdownLeaseFrame(writer io.Writer, data []byte) error {
	var header [8]byte
	binary.BigEndian.PutUint64(header[:], uint64(len(data)))
	_, err := io.Copy(writer, io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(data)))
	return err
}

func closeShutdownLeaseFiles(files []*os.File) (resultErr error) {
	for _, file := range files {
		if file != nil {
			resultErr = errors.Join(resultErr, file.Close())
		}
	}
	return resultErr
}

// A private test handshake lets the parent signal the exact independent driver
// only after it owns both descendants. It grants no cleanup or native authority.
type shutdownLeaseDriverRecord struct {
	Directory string                `json:"directory"`
	Owner     shutdownLeaseIdentity `json:"owner"`
	CLI       shutdownLeaseIdentity `json:"cli"`
}

func (p *shutdownLeaseProof) awaitDriverInterruption(ctx context.Context, gate string) error {
	owner, cli := p.refs["owner"], p.refs["cli"]
	record := shutdownLeaseDriverRecord{Directory: p.directory.path, Owner: shutdownLeaseIdentity{PID: owner.PID(), Token: owner.StartToken()}, CLI: shutdownLeaseIdentity{PID: cli.PID(), Token: cli.StartToken()}}
	if err := publishNativeFixtureRecord(filepath.Join(gate, "ready.json"), false, func(writer io.Writer) error { return json.NewEncoder(writer).Encode(record) }); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

type shutdownLeaseDriverProbe struct {
	directory *nativeSchedulerDirectory
	child     *shutdownLeaseChild
	output    bytes.Buffer
}

func TestShutdownLeaseDriverInterruption(t *testing.T) {
	if os.Getenv("DELEGATE_SHUTDOWN_LEASE_PROOF") != "1" {
		t.Skip("explicit native C3 driver interruption proof")
	}
	probe := &shutdownLeaseDriverProbe{}
	t.Cleanup(func() { probe.cleanup(t) })
	var err error
	probe.directory, err = newNativeSchedulerDirectory()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestShutdownLeaseNativeProof$/^shutdown$", "-test.timeout=1m", "-test.v")
	command.Env = append(os.Environ(), "DELEGATE_SHUTDOWN_LEASE_DRIVER_GATE="+probe.directory.path, "GORACE=atexit_sleep_ms=0")
	command.Stdout, command.Stderr = &probe.output, &probe.output
	if err = ConfigureIndependentSession(command); err != nil {
		t.Fatal(err)
	}
	probe.child, err = startShutdownLeaseChild(command)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	if err = waitShutdownLeaseFile(ctx, probe.directory.path, "ready.json"); err != nil {
		t.Fatal(err)
	}
	if err = nativeSchedulerHostSignal(ctx, probe.child.ref, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitErr := probe.child.wait(ctx)
	var exited *exec.ExitError
	if !errors.As(waitErr, &exited) || exited.ExitCode() != 1 {
		t.Fatalf("interrupted proof driver must fail normally after cleanup: %v\n%s", waitErr, probe.closedOutput())
	}
	record, err := probe.observation()
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []shutdownLeaseIdentity{record.Owner, record.CLI} {
		ref, refErr := NewProcessRef(identity.PID, identity.Token)
		if refErr != nil {
			t.Fatal(refErr)
		}
		if err = WaitProcessAbsent(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = os.Lstat(record.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted driver retained its native case directory %s: %v", record.Directory, err)
	}
	t.Log("real SIGINT canceled exact independent driver; its pinned case and native owner/CLI closed before direct driver Wait")
}

func (p *shutdownLeaseDriverProbe) observation() (shutdownLeaseDriverRecord, error) {
	var record shutdownLeaseDriverRecord
	data, err := p.directory.root.ReadFile("ready.json")
	if err != nil {
		return record, err
	}
	err = json.Unmarshal(data, &record)
	if err == nil && !filepath.IsAbs(record.Directory) {
		err = errors.New("driver fixture directory is not absolute")
	}
	return record, err
}

func (p *shutdownLeaseDriverProbe) stopDriver(ctx context.Context) error {
	if p.child == nil {
		return nil
	}
	select {
	case <-p.child.done:
		return nil
	default:
	}
	signalErr := nativeSchedulerHostSignal(ctx, p.child.ref, syscall.SIGTERM)
	cooperativeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	waitErr := p.child.wait(cooperativeCtx)
	cancel()
	select {
	case <-p.child.done:
		var exited *exec.ExitError
		if errors.As(waitErr, &exited) {
			waitErr = nil
		}
		return errors.Join(signalErr, waitErr)
	default:
		return errors.Join(signalErr, fmt.Errorf("driver cooperative cleanup did not settle exact Wait: %w", waitErr), finalizeShutdownLeaseChild(ctx, p.child))
	}
}

func (p *shutdownLeaseDriverProbe) release(ctx context.Context) error {
	closeErr := p.stopDriver(ctx)
	if p.child == nil {
		return closeErr
	}
	record, readErr := p.observation()
	if readErr != nil {
		return errors.Join(closeErr, fmt.Errorf("driver PID=%d ref=%d/%q; actors UNKNOWN; inner directory UNKNOWN: %w", p.child.command.Process.Pid, p.child.ref.PID(), p.child.ref.StartToken(), readErr))
	}
	return errors.Join(closeErr, p.closeRecordedActors(ctx, record), shutdownLeaseRetainedDirectory(record.Directory))
}

func (p *shutdownLeaseDriverProbe) closeRecordedActors(ctx context.Context, record shutdownLeaseDriverRecord) (closeErr error) {
	for _, identity := range []shutdownLeaseIdentity{record.Owner, record.CLI} {
		ref, refErr := NewProcessRef(identity.PID, identity.Token)
		closeErr = errors.Join(closeErr, refErr)
		if refErr != nil {
			continue
		}
		present, observeErr := ProcessPresent(ref)
		closeErr = errors.Join(closeErr, observeErr)
		if present {
			closeErr = errors.Join(closeErr, shutdownLeaseSignal(ctx, ref, syscall.SIGKILL))
		}
		closeErr = errors.Join(closeErr, WaitProcessAbsent(ctx, ref))
	}
	return closeErr
}

func shutdownLeaseRetainedDirectory(directory string) error {
	_, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inner directory %s remains unverified and retained: %w", directory, err)
	}
	return fmt.Errorf("inner directory %s retained; cleanup has no original creation token", directory)
}

func (p *shutdownLeaseDriverProbe) cleanup(t *testing.T) {
	t.Helper()
	if p.directory == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	closeErr := p.release(ctx)
	if closeErr != nil {
		t.Errorf("retained driver diagnostics %s: %v", p.directory.path, closeErr)
	}
	if finishErr := p.directory.finish(closeErr == nil); finishErr != nil {
		t.Error(finishErr)
	}
}

func (p *shutdownLeaseDriverProbe) closedOutput() string {
	if p.child == nil {
		return "driver did not start"
	}
	select {
	case <-p.child.done:
		return p.output.String()
	default:
		return "output retained until exact child Wait"
	}
}

// These fixed native helpers have one process and no descendants. The retained
// directory belongs to the parent test's original token, never this observation.
func shutdownLeaseCleanupFixture(ctx context.Context, directory, mode string) error {
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ref, err := ObserveProcess(os.Getpid())
	if err != nil {
		return err
	}
	if mode == "driver-retained" {
		inner := os.Getenv("DELEGATE_SHUTDOWN_LEASE_RETAIN_DIRECTORY")
		if !filepath.IsAbs(inner) {
			return errors.New("retained cleanup fixture requires original parent-owned directory")
		}
		if err = publishNativeFixtureRecord(filepath.Join(inner, "fixture-member"), false, func(writer io.Writer) error {
			_, writeErr := io.WriteString(writer, "owned native fixture\n")
			return writeErr
		}); err != nil {
			return err
		}
		identity := shutdownLeaseIdentity{PID: ref.PID(), Token: ref.StartToken()}
		record := shutdownLeaseDriverRecord{Directory: inner, Owner: identity, CLI: identity}
		if err = publishNativeFixtureRecord(filepath.Join(directory, "ready.json"), false, func(writer io.Writer) error { return json.NewEncoder(writer).Encode(record) }); err != nil {
			return err
		}
	}
	if err = writeShutdownLeaseIdentity(directory, "entered", ref); err != nil {
		return err
	}
	<-signalCtx.Done()
	return publishNativeFixtureRecord(filepath.Join(directory, "driver.closed"), false, func(writer io.Writer) error {
		_, writeErr := io.WriteString(writer, "cooperative cleanup ran\n")
		return writeErr
	})
}

func TestShutdownLeaseDriverCleanupFaults(t *testing.T) {
	if os.Getenv("DELEGATE_SHUTDOWN_LEASE_PROOF") != "1" {
		t.Skip("explicit native C3 driver cleanup faults")
	}
	for _, mode := range []string{"driver-early", "driver-retained"} {
		if !t.Run(mode, func(t *testing.T) { testShutdownLeaseDriverCleanupFault(t, mode) }) {
			return
		}
	}
}

func testShutdownLeaseDriverCleanupFault(t *testing.T, mode string) {
	probe := &shutdownLeaseDriverProbe{}
	var inner *nativeSchedulerDirectory
	t.Cleanup(func() { finishShutdownLeaseCleanupFault(t, probe, inner) })
	var err error
	probe.directory, err = newNativeSchedulerDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if mode == "driver-retained" {
		inner, err = newNativeSchedulerDirectory()
		if err != nil {
			t.Fatal(err)
		}
	}
	command, err := shutdownLeaseFixtureCommand(probe.directory.path, mode, "", "", time.Now().Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if inner != nil {
		command.Env = append(command.Env, "DELEGATE_SHUTDOWN_LEASE_RETAIN_DIRECTORY="+inner.path)
	}
	if err = ConfigureIndependentSession(command); err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = &probe.output, &probe.output
	probe.child, err = startShutdownLeaseChild(command)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err = waitShutdownLeaseFile(ctx, probe.directory.path, "entered.json"); err != nil {
		t.Fatal(err)
	}
	releaseErr := probe.release(ctx)
	if _, err = probe.directory.root.Stat("driver.closed"); err != nil {
		t.Fatalf("driver was killed before its cooperative cleanup: %v", err)
	}
	select {
	case <-probe.child.done:
	default:
		t.Fatal("cleanup returned before exact driver Wait")
	}
	if err = WaitProcessAbsent(ctx, probe.child.ref); err != nil {
		t.Fatal(err)
	}
	if releaseErr == nil {
		t.Fatal("cleanup accepted incomplete native driver evidence")
	}
	if mode == "driver-early" {
		requireShutdownLeaseUnknownDriver(t, releaseErr, probe.child.ref)
	} else {
		requireShutdownLeaseRetainedInner(t, releaseErr, inner)
	}
	if err = probe.directory.check(); err != nil {
		t.Fatalf("cleanup disposed unknown/retained outer diagnostics: %v", err)
	}
	t.Logf("expected conservative cleanup refusal: %v", releaseErr)
}

func requireShutdownLeaseUnknownDriver(t *testing.T, fault error, ref ProcessRef) {
	t.Helper()
	if !errors.Is(fault, os.ErrNotExist) {
		t.Fatalf("missing readiness lost its read cause: %v", fault)
	}
	for _, required := range []string{"actors UNKNOWN", "inner directory UNKNOWN", strconv.Itoa(ref.PID()), ref.StartToken()} {
		if !strings.Contains(fault.Error(), required) {
			t.Fatalf("missing-ready diagnostic %q lacks %q", fault, required)
		}
	}
}

func requireShutdownLeaseRetainedInner(t *testing.T, fault error, inner *nativeSchedulerDirectory) {
	t.Helper()
	if !strings.Contains(fault.Error(), inner.path) || !strings.Contains(fault.Error(), "retained") {
		t.Fatalf("inner directory omitted from diagnostic: %v", fault)
	}
	if err := inner.check(); err != nil {
		t.Fatalf("cleanup altered parent-owned inner fixture: %v", err)
	}
	if _, err := inner.root.Stat("fixture-member"); err != nil {
		t.Fatal(err)
	}
}

func finishShutdownLeaseCleanupFault(t *testing.T, probe *shutdownLeaseDriverProbe, inner *nativeSchedulerDirectory) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	closeErr := probe.stopDriver(ctx)
	if probe.child != nil {
		closeErr = errors.Join(closeErr, WaitProcessAbsent(ctx, probe.child.ref))
	}
	if closeErr != nil {
		t.Errorf("retained native cleanup fault fixture: %v", closeErr)
	}
	// The test creator still holds both ORIGINAL creation tokens. This is not
	// deletion based on ready.json, and runs only after its sole actor is absent.
	if inner != nil {
		if err := inner.finish(closeErr == nil); err != nil {
			t.Error(err)
		}
	}
	if probe.directory != nil {
		if err := probe.directory.finish(closeErr == nil); err != nil {
			t.Error(err)
		}
	}
}
