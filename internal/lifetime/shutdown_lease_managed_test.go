//go:build linux || darwin

package lifetime

import (
	"context"
	"debug/buildinfo"
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

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"golang.org/x/sys/unix"
)

// The production control package and real delegate-run execute in separate
// children, avoiding a lifetime-test -> Pueue -> lifetime import cycle. This
// owner retains the existing C3 native observers, exact Waits and directory pins.
type managedShutdownLeaseProof struct {
	proof                shutdownLeaseProof
	root                 *nativeSchedulerDirectory
	fixture, owner, role string
}

func TestManagedShutdownLeaseNativeProof(t *testing.T) {
	if os.Getenv("DELEGATE_SHUTDOWN_LEASE_PROOF") != "1" {
		t.Skip("explicit native managed control-owner C3 proof")
	}
	signalCtx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, 150*time.Second)
	defer cancel()
	binary, err := verifyShutdownLeaseBinary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := managedShutdownFixturePath("DELEGATE_SHUTDOWN_LEASE_CONTROL_FIXTURE")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := managedShutdownFixturePath("DELEGATE_SHUTDOWN_LEASE_CONTROL_OWNER")
	if err != nil {
		t.Fatal(err)
	}
	image, err := buildinfo.ReadFile(owner)
	if err != nil || image.Path != "github.com/hishamkaram/delegation-layer/cmd/delegate-run" {
		t.Fatalf("managed owner must be the actual built delegate-run: %v", err)
	}
	for _, role := range []string{"shutdown", "shutdown-migration", "shutdown-recovered", "status", "log", "edit", "task-fixture", "provider-fixture", "editor-fixture", "pager-fixture"} {
		if !t.Run(role, func(t *testing.T) {
			p := &managedShutdownLeaseProof{fixture: fixture, owner: owner, role: role}
			proofRole := role
			if strings.HasPrefix(role, "shutdown") {
				proofRole = "shutdown"
			}
			p.proof = shutdownLeaseProof{binary: binary, role: proofRole, deadline: time.Now().Add(20 * time.Second), refs: make(map[string]ProcessRef), watches: make(map[string]*shutdownLeaseExitWatch)}
			t.Cleanup(func() { p.cleanup(ctx, t) })
			caseCtx, caseCancel := context.WithDeadline(ctx, p.proof.deadline)
			defer caseCancel()
			if runErr := p.run(caseCtx, t); runErr != nil {
				t.Fatal(runErr)
			}
		}) {
			return
		}
	}
}

func managedShutdownFixturePath(key string) (string, error) {
	path := os.Getenv(key)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must name an absolute native fixture", key)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not an executable regular file", key)
	}
	return resolved, nil
}

func (p *managedShutdownLeaseProof) prepare(ctx context.Context) error {
	var err error
	p.root, err = newNativeSchedulerDirectory()
	if err != nil {
		return err
	}
	if err = p.root.root.Mkdir(".supervisor", 0o700); err != nil {
		return err
	}
	p.proof.directory, err = managedShutdownSupervisorDirectory(p.root)
	if err != nil {
		return err
	}
	base := p.proof.directory.path
	if len(filepath.Join(base, "s")) >= 104 {
		return errors.New("managed proof UNIX endpoint exceeds native geometry")
	}
	if err = p.writeRootRecord(); err != nil {
		return err
	}
	p.proof.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(base, "s"), Net: "unix"})
	if err != nil {
		return err
	}
	if err = p.proof.listener.SetDeadline(p.proof.deadline); err != nil {
		return err
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	p.proof.status = statusRead
	p.proof.files = append(p.proof.files, statusRead, statusWrite)
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	p.proof.control = controlWrite
	p.proof.files = append(p.proof.files, controlRead, controlWrite)
	helper, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(p.fixture, "--test-qualified-control-caller", p.root.path, p.proof.binary, p.owner, p.role, helper, strconv.FormatInt(p.proof.deadline.UnixNano(), 10))
	command.Dir = p.root.path
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	command.Env = []string{"HOME=" + p.root.path, "PATH=/usr/bin:/bin", "LANG=C", "GORACE=atexit_sleep_ms=0", "DELEGATE_MANAGED_SHUTDOWN_SECRET=" + shutdownLeaseSecret}
	command.ExtraFiles = []*os.File{statusWrite, controlRead}
	p.proof.caller, err = startShutdownLeaseChild(command)
	closeErr := closeShutdownLeaseFiles([]*os.File{statusWrite, controlRead})
	p.proof.files[1], p.proof.files[2] = nil, nil
	return errors.Join(err, closeErr)
}

func managedShutdownSupervisorDirectory(root *nativeSchedulerDirectory) (directory *nativeSchedulerDirectory, resultErr error) {
	directory = &nativeSchedulerDirectory{home: root.path, path: filepath.Join(root.path, ".supervisor"), name: ".supervisor", parentInfo: root.info}
	var err error
	directory.parent, err = os.OpenRoot(root.path)
	if err != nil {
		return directory, err
	}
	directory.root, err = directory.parent.OpenRoot(".supervisor")
	if err != nil {
		return directory, err
	}
	directory.info, err = directory.root.Stat(".")
	if err != nil {
		return directory, err
	}
	return directory, directory.check()
}

func (p *managedShutdownLeaseProof) writeRootRecord() error {
	rootID, err := randomid.NewRootID()
	if err != nil {
		return err
	}
	record := task.RootRecord{SchemaVersion: task.SchemaVersion, RootID: rootID, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = task.ValidateRootRecord(&record); err != nil {
		return err
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return err
	}
	return p.root.write("root.json", data)
}

func (p *managedShutdownLeaseProof) observeManagedOwner(ctx context.Context) error {
	cli, err := p.proof.readRef(ctx, "cli")
	if err != nil {
		return err
	}
	p.proof.refs["cli"] = cli
	native, err := nestedObserveNativeContext(ctx, cli.PID(), "")
	if err != nil {
		return err
	}
	owner, err := ObserveProcess(native.ParentPID)
	if err != nil {
		return err
	}
	ownerNative, err := nestedObserveNativeContext(ctx, owner.PID(), "")
	if err != nil {
		return err
	}
	if ownerNative.ParentPID != p.proof.caller.ref.PID() {
		return errors.New("managed owner is not the fixed caller's native child")
	}
	p.proof.refs["owner"] = owner
	if err = writeShutdownLeaseIdentity(p.proof.directory.path, "owner", owner); err != nil {
		return err
	}
	return nil
}

func (p *managedShutdownLeaseProof) handoff(ctx context.Context) error {
	if err := p.observeManagedOwner(ctx); err != nil {
		return err
	}
	for _, name := range []string{"owner", "cli"} {
		watch, watchErr := newShutdownLeaseExitWatch(p.proof.refs[name])
		if watchErr != nil {
			return watchErr
		}
		p.proof.watches[name] = watch
	}
	if err := p.captureLeaseIdentities(ctx); err != nil {
		return err
	}
	if err := p.proof.directory.write("managed.release-caller", []byte("handoff\n")); err != nil {
		return err
	}
	if err := p.proof.caller.wait(ctx); err != nil {
		return err
	}
	connection, err := p.proof.listener.AcceptUnix()
	p.proof.connection = connection
	if err != nil {
		return err
	}
	if err = p.proof.connection.SetDeadline(p.proof.deadline); err != nil {
		return err
	}
	secret, err := readShutdownLeaseFrame(p.proof.connection)
	if err != nil {
		return err
	}
	if string(secret) != shutdownLeaseSecret {
		return errors.New("managed native CLI handshake differs from owned gate")
	}
	return shutdownLeasePeer(p.proof.connection, p.proof.refs["cli"].PID())
}

func (p *managedShutdownLeaseProof) captureLeaseIdentities(ctx context.Context) error {
	var bootstrap unix.Stat_t
	if err := unix.Lstat(filepath.Join(p.proof.directory.path, "bootstrap.lock"), &bootstrap); err != nil {
		return err
	}
	p.proof.generation = shutdownLeaseFileID{Device: uint64(bootstrap.Dev), Inode: bootstrap.Ino}
	for _, name := range []string{"queue", "task-maintenance", "lifetime", "instance"} {
		var stat unix.Stat_t
		if err := unix.Lstat(filepath.Join(p.proof.directory.path, name+".lock"), &stat); err != nil {
			return err
		}
		p.proof.excluded = append(p.proof.excluded, shutdownLeaseFileID{Device: uint64(stat.Dev), Inode: stat.Ino})
	}
	observed, err := shutdownLeaseDescriptors(ctx, p.proof.refs["owner"].PID())
	if err != nil {
		return err
	}
	// Native observer must identify every held regular lease in the REAL owner.
	for _, expected := range append([]shutdownLeaseFileID{p.proof.generation}, p.proof.excluded...) {
		found := false
		for _, identity := range observed {
			if identity == expected {
				found = true
			}
		}
		if !found {
			return errors.New("native descriptor observer cannot represent actual managed owner leases")
		}
	}
	return nil
}

func (p *managedShutdownLeaseProof) run(ctx context.Context, t *testing.T) error {
	if err := p.prepare(ctx); err != nil {
		return err
	}
	if err := p.handoff(ctx); err != nil {
		return err
	}
	privateShutdown := strings.HasPrefix(p.role, "shutdown")
	if err := p.proof.inspect(ctx, privateShutdown); err != nil {
		return err
	}
	if err := shutdownLeaseSignal(ctx, p.proof.refs["owner"], syscall.SIGKILL); err != nil {
		return err
	}
	if err := p.proof.watches["owner"].wait(ctx); err != nil {
		return fmt.Errorf("managed owner exit observation: %w", err)
	}
	if err := WaitProcessAbsent(ctx, p.proof.refs["owner"]); err != nil {
		return err
	}
	if err := p.proof.adopted(ctx); err != nil {
		return err
	}
	if err := p.proof.ipcClosed(); err != nil {
		return err
	}
	if err := p.proof.inspect(ctx, privateShutdown); err != nil {
		return err
	}
	expected := "acquired"
	if privateShutdown {
		expected = "busy"
	}
	if err := p.proof.requireProbe(expected); err != nil {
		return err
	}
	if !privateShutdown {
		t.Log("production managed owner: no bootstrap/queue/task/lifetime/instance/IPC leak to negative role")
		return nil
	}
	if err := p.proof.finishShutdown(ctx, t); err != nil {
		return fmt.Errorf("managed qualified CLI final-send/exit oracles: %w", err)
	}
	return nil
}

func (p *managedShutdownLeaseProof) cleanup(ctx context.Context, t *testing.T) {
	t.Helper()
	p.proof.cleanup(ctx, t)
	if p.root == nil {
		return
	}
	_, inspectErr := os.Lstat(filepath.Join(p.root.path, ".supervisor"))
	removed := errors.Is(inspectErr, os.ErrNotExist)
	if !removed {
		t.Errorf("retained managed C3 root diagnostics %s: supervisor closure/removal unproven: %v", p.root.path, inspectErr)
	}
	if err := p.root.finish(removed); err != nil {
		t.Error(err)
	}
}

func TestManagedShutdownLeaseFixture(t *testing.T) {
	if os.Getenv("DELEGATE_MANAGED_SHUTDOWN_ROLE") != "1" {
		t.Skip("fixed harmless managed-control native actor")
	}
	deadline, err := strconv.ParseInt(os.Getenv("DELEGATE_MANAGED_SHUTDOWN_DEADLINE"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, deadline))
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(os.Getenv("DELEGATE_MANAGED_SHUTDOWN_BASE"), "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := connection.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = connection.SetDeadline(shutdownLeaseDeadline(ctx)); err != nil {
		t.Fatal(err)
	}
	if err = writeShutdownLeaseFrame(connection, []byte(os.Getenv("DELEGATE_MANAGED_SHUTDOWN_SECRET"))); err != nil {
		t.Fatal(err)
	}
	var release [1]byte
	_, err = connection.Read(release[:])
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}
