//go:build linux || darwin

package lifetime

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
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

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue/protocol"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

type (
	admissionNativeSubject struct {
		RootID string `json:"root_id"`
		TaskID string `json:"task_id"`
		Group  string `json:"group"`
		Label  string `json:"label"`
	}
	admissionNativeActor struct {
		PID     int    `json:"pid"`
		Token   string `json:"token"`
		Command string `json:"command"`
	}
	admissionNativeAdd   struct{ command, path, group, label string }
	admissionNativeProof struct {
		managed   managedShutdownLeaseProof
		workspace *nativeSchedulerDirectory
		store     *taskdir.Store
		role      string
		subject   admissionNativeSubject
		add       admissionNativeAdd
		handoff   bool
		survivor  *repairEditorSurvivorProof
	}
)

func TestManagedAdmissionLeaseNativeProof(t *testing.T) {
	if os.Getenv("DELEGATE_ADMISSION_LEASE_PROOF") != "1" {
		t.Skip("explicit native managed RootAdmission actuator proof")
	}
	signalCtx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, 170*time.Second)
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
		t.Fatalf("actual built managed owner required: %v", err)
	}
	for _, role := range []string{"ordinary-add", "inspection-add", "workspace-add", "ordinary-enqueue", "inspection-enqueue", "workspace-enqueue", "ordinary-recovery", "inspection-recovery", "workspace-recovery", "negative-status", "negative-log", "negative-edit", "negative-task-fixture", "negative-provider-fixture", "negative-editor-fixture", "negative-pager-fixture", "managed-repair", "managed-repair-restore"} {
		if !t.Run(role, func(t *testing.T) {
			p := &admissionNativeProof{role: role}
			p.managed = managedShutdownLeaseProof{fixture: fixture, owner: owner, role: role}
			p.managed.proof = shutdownLeaseProof{binary: binary, role: role, deadline: time.Now().Add(20 * time.Second), refs: make(map[string]ProcessRef), watches: make(map[string]*shutdownLeaseExitWatch)}
			t.Cleanup(func() { p.cleanup(ctx, t) })
			caseCtx, caseCancel := context.WithDeadline(ctx, p.managed.proof.deadline)
			defer caseCancel()
			if runErr := p.run(caseCtx, t); runErr != nil {
				t.Fatal(runErr)
			}
		}) {
			return
		}
	}
}

func (p *admissionNativeProof) prepare(ctx context.Context) error {
	var err error
	p.managed.root, err = newNativeSchedulerDirectory()
	if err != nil {
		return err
	}
	p.workspace, err = newNativeSchedulerDirectory()
	if err != nil {
		return err
	}
	p.store, err = taskdir.InitStoreWithPredicatesContext(ctx, p.managed.root.path, predicate.Default())
	if err != nil {
		return err
	}
	if err = p.managed.root.root.Mkdir(".supervisor", 0o700); err != nil {
		return err
	}
	p.managed.proof.directory, err = managedShutdownSupervisorDirectory(p.managed.root)
	if err != nil {
		return err
	}
	base := p.managed.proof.directory.path
	if len(filepath.Join(base, "s")) >= 104 {
		return errors.New("native admission fixture exceeds Unix endpoint geometry")
	}
	if err = p.config(); err != nil {
		return err
	}
	p.managed.proof.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(base, "s"), Net: "unix"})
	if err != nil {
		return err
	}
	if err = p.managed.proof.listener.SetDeadline(p.managed.proof.deadline); err != nil {
		return err
	}
	if p.survivor != nil {
		if err = p.survivor.prepare(p.managed.proof.directory, p.managed.proof.deadline); err != nil {
			return err
		}
	}
	return p.startCaller()
}

func (p *admissionNativeProof) config() error {
	d := p.managed.proof.directory
	for _, name := range []string{"data", "run"} {
		if err := d.root.Mkdir(name, 0o700); err != nil {
			return err
		}
	}
	if err := d.write("data/shared_secret", []byte(shutdownLeaseSecret)); err != nil {
		return err
	}
	base := d.path
	data := "client:\n  show_confirmation_questions: false\nshared:\n  pueue_directory: " + strconv.Quote(filepath.Join(base, "data")) + "\n  runtime_directory: " + strconv.Quote(filepath.Join(base, "run")) + "\n  alias_file: " + strconv.Quote(filepath.Join(base, "aliases.yml")) + "\n  use_unix_socket: true\n  unix_socket_path: " + strconv.Quote(filepath.Join(base, "s")) + "\n  pid_path: " + strconv.Quote(filepath.Join(base, "run", "pueue.pid")) + "\n  shared_secret_path: " + strconv.Quote(filepath.Join(base, "data", "shared_secret")) + "\ndaemon:\n  shell_command: [\"/bin/sh\", \"-c\", \"{{ pueue_command_string }}\"]\n"
	if p.repair() {
		// A real native edit must succeed without this user shell ever running.
		data = strings.Replace(data, "/bin/sh", "/unavailable/user-shell", 1)
		data += "  env_vars: {EDITOR: unsafe, DELEGATE_PUEUE_EDIT_ROOT: unsafe}\n"
	}
	for _, name := range []string{"operator.yml", "pueue.yml"} {
		if err := d.write(name, []byte(data)); err != nil {
			return err
		}
	}
	file, err := d.root.OpenFile("bootstrap.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	return file.Close()
}

func (p *admissionNativeProof) startCaller() error {
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	p.managed.proof.status = statusRead
	p.managed.proof.files = append(p.managed.proof.files, statusRead, statusWrite)
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	p.managed.proof.control = controlWrite
	p.managed.proof.files = append(p.managed.proof.files, controlRead, controlWrite)
	entry, flag := "TestAdmissionControlCallerFixture", "DELEGATE_ADMISSION_LEASE_CALLER=1"
	if p.repair() {
		entry, flag = "TestRepairControlCallerFixture", "DELEGATE_ADMISSION_LEASE_REPAIR=1"
	}
	if p.negative() {
		entry, flag = "TestAdmissionControlNegativeFixture", "DELEGATE_ADMISSION_LEASE_NEGATIVE=1"
	}
	helper, err := os.Executable()
	if err != nil {
		return err
	}
	command := exec.Command(p.managed.fixture, "-test.run=^"+entry+"$", "-test.timeout=45s")
	command.Dir = p.store.Root
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "GORACE=atexit_sleep_ms=0", flag, "DELEGATE_ADMISSION_LEASE_DIRECTORY=" + p.store.Root, "DELEGATE_ADMISSION_LEASE_PUEUE=" + p.managed.proof.binary, "DELEGATE_ADMISSION_LEASE_OWNER=" + p.managed.owner, "DELEGATE_ADMISSION_LEASE_HELPER=" + helper, "DELEGATE_MANAGED_SHUTDOWN_SECRET=" + shutdownLeaseSecret, "DELEGATE_ADMISSION_LEASE_WORKSPACE=" + p.workspace.path, "DELEGATE_ADMISSION_LEASE_ROLE=" + p.role, "DELEGATE_ADMISSION_LEASE_DEADLINE=" + strconv.FormatInt(p.managed.proof.deadline.UnixNano(), 10)}
	command.ExtraFiles = []*os.File{statusWrite, controlRead}
	p.managed.proof.caller, err = startShutdownLeaseChild(command)
	closeErr := closeShutdownLeaseFiles([]*os.File{statusWrite, controlRead})
	p.managed.proof.files[1], p.managed.proof.files[2] = nil, nil
	return errors.Join(err, closeErr)
}

func (p *admissionNativeProof) run(ctx context.Context, t *testing.T) error {
	if err := p.prepare(ctx); err != nil {
		return err
	}
	if err := p.serveUntilTarget(ctx); err != nil {
		return err
	}
	if err := p.handoffActors(ctx); err != nil {
		return err
	}
	if p.repair() {
		return p.finishRepair(ctx, t)
	}
	if err := p.loseOwner(ctx); err != nil {
		return err
	}
	expected := "busy"
	if p.negative() {
		expected = "acquired"
	}
	if err := p.probe(ctx, expected); err != nil {
		return err
	}
	if p.negative() {
		return p.finishNegative(ctx, t)
	}
	return p.finish(ctx, t)
}

func (p *admissionNativeProof) handoffActors(ctx context.Context) error {
	proof := &p.managed.proof
	if err := p.captureDescriptors(ctx); err != nil {
		return err
	}
	for _, name := range []string{"owner", "cli"} {
		watch, err := newShutdownLeaseExitWatch(proof.refs[name])
		if err != nil {
			return err
		}
		proof.watches[name] = watch
	}
	if err := proof.directory.write("managed.release-caller", []byte("handoff\n")); err != nil {
		return err
	}
	if err := proof.caller.wait(ctx); err != nil {
		return err
	}
	p.handoff = true
	return proof.inspect(ctx, !p.negative())
}

func (p *admissionNativeProof) loseOwner(ctx context.Context) error {
	proof := &p.managed.proof
	if err := shutdownLeaseSignal(ctx, proof.refs["owner"], syscall.SIGKILL); err != nil {
		return err
	}
	if err := proof.watches["owner"].wait(ctx); err != nil {
		return err
	}
	if err := WaitProcessAbsent(ctx, proof.refs["owner"]); err != nil {
		return err
	}
	if err := proof.adopted(ctx); err != nil {
		return err
	}
	if err := proof.ipcClosed(); err != nil {
		return err
	}
	return proof.inspect(ctx, !p.negative())
}

func (p *admissionNativeProof) negative() bool { return strings.HasPrefix(p.role, "negative-") }

func (p *admissionNativeProof) repair() bool { return strings.HasPrefix(p.role, "managed-repair") }

func (p *admissionNativeProof) finishNegative(ctx context.Context, t *testing.T) error {
	proof := &p.managed.proof
	if !strings.HasSuffix(p.role, "-fixture") {
		if err := writeShutdownLeaseFrame(proof.connection, []byte("0.31.1")); err != nil {
			return err
		}
		if _, err := readShutdownLeaseFrame(proof.connection); err != nil {
			return err
		}
	}
	if err := proof.inspect(ctx, false); err != nil {
		return err
	}
	if err := p.probe(ctx, "acquired"); err != nil {
		return err
	}
	if err := proof.connection.Close(); err != nil {
		return err
	}
	proof.connection = nil
	if err := proof.watches["cli"].wait(ctx); err != nil {
		return err
	}
	if err := WaitProcessAbsent(ctx, proof.refs["cli"]); err != nil {
		return err
	}
	t.Log("actual managed negative actuator: Root SH and other leases absent before/after request; caller and owner loss leave IPC closed and fresh Root EX acquirable while actuator lives")
	return nil
}

func (p *admissionNativeProof) serveUntilTarget(ctx context.Context) error {
	for {
		active, err := p.acceptNativeCommand(ctx)
		if err != nil {
			return err
		}
		if p.negative() {
			return nil
		}
		if p.repair() {
			if active.Command != p.role {
				return errors.New("native managed repair fixture did not start edit")
			}
			return nil
		}
		if err = p.subjectRecord(); err != nil {
			return err
		}
		target := "enqueue"
		if strings.HasSuffix(p.role, "-add") {
			target = "add"
		}
		if active.Command == target {
			return nil
		}
		if err = p.serveSetup(ctx, active.Command); err != nil {
			return err
		}
		if err = p.settleSetup(ctx); err != nil {
			return err
		}
	}
}

func (p *admissionNativeProof) acceptNativeCommand(ctx context.Context) (admissionNativeActor, error) {
	connection, err := p.managed.proof.listener.AcceptUnix()
	if err != nil {
		return admissionNativeActor{}, err
	}
	p.managed.proof.connection = connection
	if err = connection.SetDeadline(p.managed.proof.deadline); err != nil {
		return admissionNativeActor{}, err
	}
	active, err := p.activePeer(ctx, connection)
	if err != nil {
		return active, err
	}
	secret, err := readShutdownLeaseFrame(connection)
	if err != nil {
		return active, err
	}
	if string(secret) != shutdownLeaseSecret {
		return active, errors.New("native admission secret differs")
	}
	return active, nil
}

func (p *admissionNativeProof) settleSetup(ctx context.Context) error {
	proof := &p.managed.proof
	if err := proof.connection.Close(); err != nil {
		return err
	}
	proof.connection = nil
	for _, name := range []string{"cli", "owner"} {
		if err := WaitProcessAbsent(ctx, proof.refs[name]); err != nil {
			return err
		}
	}
	return nil
}

func (p *admissionNativeProof) activePeer(ctx context.Context, connection *net.UnixConn) (admissionNativeActor, error) {
	var active admissionNativeActor
	err := waitForEmpty(ctx, 10*time.Millisecond, func() (bool, error) {
		data, err := p.managed.proof.directory.root.ReadFile("active-cli.json")
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err = task.DecodeStrict(data, &active); err != nil {
			return false, err
		}
		ref, err := NewProcessRef(active.PID, active.Token)
		if err != nil {
			return false, err
		}
		present, err := ProcessPresent(ref)
		if err != nil || !present {
			return false, err
		}
		if err = shutdownLeasePeer(connection, ref.PID()); err != nil {
			return false, err
		}
		return true, p.track(ctx, ref)
	}, sleepContext)
	return active, err
}

func (p *admissionNativeProof) track(ctx context.Context, cli ProcessRef) error {
	native, err := nestedObserveNativeContext(ctx, cli.PID(), "")
	if err != nil {
		return err
	}
	current, err := NewProcessRef(native.PID, native.StartToken)
	if err != nil {
		return err
	}
	if same, sameErr := SameProcess(current, cli); sameErr != nil || !same {
		return errors.Join(errors.New("active native CLI changed"), sameErr)
	}
	owner, err := ObserveProcess(native.ParentPID)
	if err != nil {
		return err
	}
	observed, err := nestedObserveNativeContext(ctx, owner.PID(), "")
	if err != nil {
		return err
	}
	if observed.ParentPID != p.managed.proof.caller.ref.PID() {
		return errors.New("active managed owner lacks fixed caller ancestry")
	}
	p.managed.proof.refs["cli"], p.managed.proof.refs["owner"] = cli, owner
	return nil
}

func (p *admissionNativeProof) subjectRecord() error {
	data, err := p.managed.proof.directory.root.ReadFile("subject.json")
	if err != nil {
		return err
	}
	if err = task.DecodeStrict(data, &p.subject); err != nil {
		return err
	}
	if p.subject.RootID != p.store.RootID || task.ValidateTaskID(p.subject.TaskID) != nil {
		return errors.New("native subject identity mismatch")
	}
	label := (protocol.Identity{RootID: p.subject.RootID, TaskID: p.subject.TaskID}).Label()
	if !strings.HasPrefix(p.role, "ordinary-") {
		label = (protocol.InspectionIdentity{RootID: p.subject.RootID, TaskID: p.subject.TaskID}).Label()
	}
	if p.subject.Label != label {
		return errors.New("native subject label mismatch")
	}
	return nil
}

func (p *admissionNativeProof) serveSetup(ctx context.Context, command string) error {
	connection := p.managed.proof.connection
	if err := writeShutdownLeaseFrame(connection, []byte("0.31.1")); err != nil {
		return err
	}
	frame, err := readShutdownLeaseFrame(connection)
	if err != nil {
		return err
	}
	switch command {
	case "status":
		if !bytes.Equal(frame, admissionCBORText("Status")) {
			return errors.New("native setup did not request Status")
		}
		return writeShutdownLeaseFrame(connection, p.statusReply())
	case "add":
		p.add, err = p.decodeAdd(frame)
		if err != nil {
			return err
		}
		return writeShutdownLeaseFrame(connection, admissionAddedReply())
	default:
		return errors.New("unexpected native setup request")
	}
}

func (p *admissionNativeProof) captureDescriptors(ctx context.Context) error {
	var lock unix.Stat_t
	if err := unix.Lstat(filepath.Join(p.store.Root, taskdir.RootAdmissionLockName), &lock); err != nil {
		return err
	}
	p.managed.proof.generation = shutdownLeaseFileID{Device: uint64(lock.Dev), Inode: lock.Ino}
	for _, name := range []string{"bootstrap", "queue", "task-maintenance", "lifetime", "instance"} {
		var stat unix.Stat_t
		if err := unix.Lstat(filepath.Join(p.managed.proof.directory.path, name+".lock"), &stat); err != nil {
			return err
		}
		p.managed.proof.excluded = append(p.managed.proof.excluded, shutdownLeaseFileID{Device: uint64(stat.Dev), Inode: stat.Ino})
	}
	if strings.HasPrefix(p.role, "workspace-") {
		// This fixture uses the canonical private config. The production
		// withPrivateQueueIdentityLease holds private.go's actual identity inode.
		var queue unix.Stat_t
		if err := unix.Lstat(filepath.Join(p.managed.proof.directory.path, "queue-identity.lock"), &queue); err != nil {
			return err
		}
		p.managed.proof.excluded = append(p.managed.proof.excluded, shutdownLeaseFileID{Device: uint64(queue.Dev), Inode: queue.Ino})
	}
	observed, err := shutdownLeaseDescriptors(ctx, p.managed.proof.refs["owner"].PID())
	if err != nil {
		return err
	}
	for _, expected := range append([]shutdownLeaseFileID{p.managed.proof.generation}, p.managed.proof.excluded...) {
		found := false
		for _, actual := range observed {
			if actual == expected {
				found = true
			}
		}
		if !found {
			return errors.New("native observer cannot represent held managed admission leases")
		}
	}
	return nil
}

func (p *admissionNativeProof) probe(ctx context.Context, expected string) error {
	lease, err := p.store.TryAcquireRootAdmissionExclusiveContext(ctx)
	releaseErr := lease.Release()
	actual := "acquired"
	if task.AllErrorCauses(err, func(cause error) bool { return errors.Is(cause, task.ErrLockBusy) }) {
		actual = "busy"
	} else if err != nil {
		return errors.Join(err, releaseErr)
	}
	if releaseErr != nil {
		return releaseErr
	}
	var named unix.Stat_t
	if err = unix.Lstat(filepath.Join(p.store.Root, taskdir.RootAdmissionLockName), &named); err != nil {
		return err
	}
	if p.managed.proof.generation != (shutdownLeaseFileID{Device: uint64(named.Dev), Inode: named.Ino}) {
		actual = "identity_changed"
	}
	if actual != expected {
		return fmt.Errorf("fresh actual RootAdmission EX=%s, want %s", actual, expected)
	}
	return nil
}

func (p *admissionNativeProof) finish(ctx context.Context, t *testing.T) error {
	connection := p.managed.proof.connection
	if err := writeShutdownLeaseFrame(connection, []byte("0.31.1")); err != nil {
		return err
	}
	frame, err := readShutdownLeaseFrame(connection)
	if err != nil {
		return err
	}
	if strings.HasSuffix(p.role, "-add") {
		if _, err = p.decodeAdd(frame); err != nil {
			return err
		}
	} else if !bytes.Equal(frame, admissionEnqueueRequest()) {
		return errors.New("native CLI did not send exact single-ID Enqueue CBOR")
	}
	if err = p.managed.proof.inspect(ctx, true); err != nil {
		return err
	}
	if err = p.probe(ctx, "busy"); err != nil {
		return err
	}
	if err = p.replacement(ctx); err != nil {
		return err
	}
	if err = p.probe(ctx, "busy"); err != nil {
		return err
	}
	reply := admissionSuccessReply()
	if strings.HasSuffix(p.role, "-add") {
		reply = admissionAddedReply()
	}
	if err = writeShutdownLeaseFrame(connection, reply); err != nil {
		return err
	}
	if err = p.managed.proof.watches["cli"].wait(ctx); err != nil {
		return err
	}
	if err = WaitProcessAbsent(ctx, p.managed.proof.refs["cli"]); err != nil {
		return err
	}
	if err = p.probe(ctx, "acquired"); err != nil {
		return err
	}
	t.Log("actual public admission/recovery consumer: Root SH retained by pinned Rust CLI before/after last request, creator exit and managed owner death; fresh actual EX busy until CLI absence; other leases/IPC excluded; replaced inode not accepted")
	return nil
}

func (p *admissionNativeProof) replacement(ctx context.Context) (resultErr error) {
	root := p.managed.root.root
	name := taskdir.RootAdmissionLockName
	if err := root.Rename(name, name+".old"); err != nil {
		return err
	}
	created := false
	defer func() {
		if created {
			resultErr = errors.Join(resultErr, root.Remove(name))
		}
		resultErr = errors.Join(resultErr, root.Rename(name+".old", name))
	}()
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	created = true
	if err = file.Close(); err != nil {
		return err
	}
	if err = p.probe(ctx, "identity_changed"); err != nil {
		return err
	}
	return nil
}

func (p *admissionNativeProof) cleanup(parent context.Context, t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	proof := &p.managed.proof
	closeErr := p.stopActors(ctx)
	for _, watch := range proof.watches {
		closeErr = errors.Join(closeErr, watch.close())
	}
	closeErr = errors.Join(closeErr, closeShutdownLeaseFiles(proof.files))
	if p.store != nil {
		closeErr = errors.Join(closeErr, p.store.Close())
	}
	if proof.caller != nil && !p.handoff {
		closeErr = errors.Join(closeErr, fmt.Errorf("caller PID=%d token=%s handoff/owned command closure unproved; remaining actor identities unknown", proof.caller.ref.PID(), proof.caller.ref.StartToken()))
	}
	p.finishDirectories(t, closeErr)
}

func (p *admissionNativeProof) stopActors(ctx context.Context) error {
	proof := &p.managed.proof
	if proof.directory == nil {
		return nil
	}
	closeErr := proof.directory.write("abort", []byte("no-more-launches\n"))
	if p.survivor != nil {
		closeErr = errors.Join(closeErr, p.survivor.close(ctx))
	}
	if proof.connection != nil {
		closeErr = errors.Join(closeErr, proof.connection.Close())
		proof.connection = nil
	}
	if proof.listener != nil {
		closeErr = errors.Join(closeErr, proof.listener.Close())
		proof.listener = nil
	}
	if proof.caller == nil {
		return closeErr
	}
	cooperative, cancel := context.WithTimeout(ctx, time.Second)
	waitErr := proof.caller.wait(cooperative)
	cancel()
	if !p.handoff && !errors.Is(waitErr, context.DeadlineExceeded) && !errors.Is(waitErr, context.Canceled) {
		closed, readErr := proof.directory.root.ReadFile("caller-native-closed")
		p.handoff = readErr == nil && string(closed) == "owned command Waits closed\n"
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			closeErr = errors.Join(closeErr, readErr)
		}
	}
	return errors.Join(closeErr, p.closeKnownActors(ctx))
}

func (p *admissionNativeProof) finishDirectories(t *testing.T, closeErr error) {
	t.Helper()
	for _, directory := range []*nativeSchedulerDirectory{p.managed.proof.directory, p.managed.root, p.workspace} {
		if directory == nil {
			continue
		}
		if closeErr != nil {
			t.Errorf("retained native admission diagnostics %s: %v", directory.path, closeErr)
		}
		if err := directory.finish(closeErr == nil); err != nil {
			t.Errorf("native admission directory finalization %s: %v", directory.path, err)
			closeErr = errors.Join(closeErr, err)
		}
	}
}

// Admission setup may run several finite commands. Only identities observed
// from the fixed caller's actual child tree are cleanup-owned by this driver.
func (p *admissionNativeProof) closeKnownActors(ctx context.Context) error {
	proof := &p.managed.proof
	resultErr := proof.caller.close(ctx)
	for _, name := range []string{"owner", "cli"} {
		ref, known := proof.refs[name]
		if !known {
			continue
		}
		present, err := ProcessPresent(ref)
		resultErr = errors.Join(resultErr, err)
		if present {
			signalErr := shutdownLeaseSignal(ctx, ref, syscall.SIGKILL)
			if !errors.Is(signalErr, errProcessNoSuchEntity) {
				resultErr = errors.Join(resultErr, signalErr)
			}
		}
		resultErr = errors.Join(resultErr, WaitProcessAbsent(ctx, ref))
	}
	return resultErr
}
