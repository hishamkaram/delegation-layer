//go:build linux || darwin

package lifetime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This test fixture owns native actors, exact direct-child Waits, descriptor
// copies and its pinned directory. It proves the bootstrap OFD mechanism, not
// production control-owner classification, daemon behavior or Stop authority.
type shutdownLeaseProof struct {
	directory       *nativeSchedulerDirectory
	binary, role    string
	deadline        time.Time
	files           []*os.File
	generation      shutdownLeaseFileID
	excluded        []shutdownLeaseFileID
	status, control *os.File
	listener        *net.UnixListener
	connection      *net.UnixConn
	caller          *shutdownLeaseChild
	refs            map[string]ProcessRef
	watches         map[string]*shutdownLeaseExitWatch
}

type shutdownLeaseFileID struct{ Device, Inode uint64 }

func shutdownLeaseFileIdentity(file *os.File) (shutdownLeaseFileID, error) {
	var stat unix.Stat_t
	err := unix.Fstat(int(file.Fd()), &stat)
	return shutdownLeaseFileID{Device: uint64(stat.Dev), Inode: stat.Ino}, err
}

func TestShutdownLeaseNativeProof(t *testing.T) {
	if os.Getenv("DELEGATE_SHUTDOWN_LEASE_PROOF") != "1" {
		t.Skip("explicit native C3 proof; set DELEGATE_SHUTDOWN_LEASE_PROOF=1 and DELEGATE_SHUTDOWN_LEASE_PUEUE")
	}
	signalCtx, stopSignals := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithTimeout(signalCtx, 150*time.Second)
	defer cancel()
	binary, err := verifyShutdownLeaseBinary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !t.Run("version-cancellation", testShutdownLeaseCancellation) {
		return
	}
	for _, role := range []string{"shutdown", "status", "log", "edit", "task-fixture", "provider-fixture", "editor-fixture", "pager-fixture"} {
		if !t.Run(role, func(t *testing.T) {
			proof := &shutdownLeaseProof{binary: binary, role: role, deadline: time.Now().Add(20 * time.Second), refs: make(map[string]ProcessRef), watches: make(map[string]*shutdownLeaseExitWatch)}
			t.Cleanup(func() { proof.cleanup(ctx, t) })
			caseCtx, caseCancel := context.WithDeadline(ctx, proof.deadline)
			defer caseCancel()
			if runErr := proof.run(caseCtx, t); runErr != nil {
				t.Fatal(runErr)
			}
		}) {
			return
		}
	}
}

func verifyShutdownLeaseBinary(ctx context.Context) (string, error) {
	candidate := os.Getenv("DELEGATE_SHUTDOWN_LEASE_PUEUE")
	if !filepath.IsAbs(candidate) {
		return "", errors.New("DELEGATE_SHUTDOWN_LEASE_PUEUE must name an absolute native fixture")
	}
	path, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("native Pueue fixture must be an executable regular file")
	}
	asset, err := shutdownLeaseAsset()
	if err != nil {
		return "", err
	}
	expected, err := shutdownLeasePinnedSHA(asset)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != expected {
		return "", errors.New("native Pueue differs from independently pinned release SHA256")
	}
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.Command(path, "--version")
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	output, err := runShutdownLeaseCommand(versionCtx, command)
	if err != nil {
		return "", err
	}
	if string(output) != "pueue 4.0.4\n" {
		return "", errors.New("native fixture did not report exact pinned Pueue 4.0.4")
	}
	return path, nil
}

func shutdownLeaseAsset() (string, error) {
	architecture := ""
	switch runtime.GOARCH {
	case "amd64":
		architecture = "x86_64"
	case "arm64":
		architecture = "aarch64"
	default:
		return "", errors.New("no independently pinned native fixture architecture")
	}
	platform := "unknown-linux-musl"
	if runtime.GOOS == "darwin" {
		platform = "apple-darwin"
	}
	return "pueue-" + architecture + "-" + platform, nil
}

func shutdownLeasePinnedSHA(asset string) (string, error) {
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "test-supervisor-sha256.txt"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != asset {
			continue
		}
		decoded, decodeErr := hex.DecodeString(fields[0])
		if decodeErr != nil || len(decoded) != sha256.Size {
			return "", errors.New("malformed independently pinned Pueue digest")
		}
		return fields[0], nil
	}
	return "", errors.New("native Pueue asset absent from independent release digests")
}

func (p *shutdownLeaseProof) run(ctx context.Context, t *testing.T) error {
	if err := p.prepare(ctx); err != nil {
		return err
	}
	if err := p.handoff(ctx); err != nil {
		return err
	}
	if gate := os.Getenv("DELEGATE_SHUTDOWN_LEASE_DRIVER_GATE"); gate != "" {
		return p.awaitDriverInterruption(ctx, gate)
	}
	if err := p.inspect(ctx, p.role == "shutdown"); err != nil {
		return err
	}
	if err := shutdownLeaseSignal(ctx, p.refs["owner"], syscall.SIGKILL); err != nil {
		return err
	}
	if err := p.watches["owner"].wait(ctx); err != nil {
		return err
	}
	if err := WaitProcessAbsent(ctx, p.refs["owner"]); err != nil {
		return err
	}
	if err := p.adopted(ctx); err != nil {
		return err
	}
	if err := p.ipcClosed(); err != nil {
		return err
	}
	if err := p.inspect(ctx, p.role == "shutdown"); err != nil {
		return err
	}
	expected := "acquired"
	if p.role == "shutdown" {
		expected = "busy"
	}
	if err := p.requireProbe(expected); err != nil {
		return err
	}
	if p.role != "shutdown" {
		t.Log("actual gated actor alive; bootstrap and excluded leases absent; IPC EOF/BrokenPipe proven")
		return nil
	}
	return p.finishShutdown(ctx, t)
}

func (p *shutdownLeaseProof) finishShutdown(ctx context.Context, t *testing.T) error {
	// Cached official 4.0.4 shutdown.rs sends exactly DaemonShutdown::Graceful
	// after authentication/version negotiation, then receives one response.
	if err := writeShutdownLeaseFrame(p.connection, []byte("0.31.1")); err != nil {
		return err
	}
	frame, err := readShutdownLeaseFrame(p.connection)
	if err != nil {
		return err
	}
	if !bytes.Equal(frame, []byte("\xa1\x6eDaemonShutdown\x68Graceful")) {
		return errors.New("actual native CLI did not send exact Graceful CBOR")
	}
	if err = p.inspect(ctx, true); err != nil {
		return err
	}
	if err = p.requireProbe("busy"); err != nil {
		return err
	}
	if err = p.replacement(); err != nil {
		return err
	}
	if err = writeShutdownLeaseFrame(p.connection, []byte("\xa1\x67Success\x68complete")); err != nil {
		return err
	}
	if err = p.watches["cli"].wait(ctx); err != nil {
		return err
	}
	if err = WaitProcessAbsent(ctx, p.refs["cli"]); err != nil {
		return err
	}
	if err = p.requireProbe("acquired"); err != nil {
		return err
	}
	t.Log("actual pinned Rust CLI retained bootstrap OFD before/after exact Graceful send and caller exit/owner SIGKILL; fresh EX busy until native CLI exit; replaced inode refused; orphan Wait belongs to native adopter")
	return nil
}

func (p *shutdownLeaseProof) prepare(ctx context.Context) error {
	var err error
	p.directory, err = newNativeSchedulerDirectory()
	if err != nil {
		return err
	}
	if len(filepath.Join(p.directory.path, "s")) >= 104 {
		return errors.New("native fixture socket path exceeds portable UNIX geometry")
	}
	if err = p.writeConfig(); err != nil {
		return err
	}
	p.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(p.directory.path, "s"), Net: "unix"})
	if err != nil {
		return err
	}
	if err = p.listener.SetDeadline(p.deadline); err != nil {
		return err
	}
	transfers, err := p.prepareFiles(ctx)
	if err != nil {
		return err
	}
	command, err := shutdownLeaseFixtureCommand(p.directory.path, "caller", p.role, p.binary, p.deadline)
	if err != nil {
		return err
	}
	command.ExtraFiles = transfers
	p.caller, err = startShutdownLeaseChild(command)
	closeErr := closeShutdownLeaseFiles(transfers)
	for _, file := range transfers {
		for index, owned := range p.files {
			if owned == file {
				p.files[index] = nil
			}
		}
	}
	return errors.Join(err, closeErr)
}

func (p *shutdownLeaseProof) prepareFiles(ctx context.Context) ([]*os.File, error) {
	var transfers []*os.File
	for _, name := range []string{"bootstrap", "queue", "task-maintenance", "lifetime", "instance"} {
		file, err := p.directory.root.OpenFile(name+".lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		p.files = append(p.files, file)
		transfers = append(transfers, file)
		if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return nil, err
		}
		identity, identityErr := shutdownLeaseFileIdentity(file)
		if identityErr != nil {
			return nil, identityErr
		}
		if name == "bootstrap" {
			p.generation = identity
		} else {
			p.excluded = append(p.excluded, identity)
		}
	}
	observed, err := shutdownLeaseDescriptors(ctx, os.Getpid())
	if err != nil {
		return nil, err
	}
	for _, file := range transfers {
		identity, identityErr := shutdownLeaseFileIdentity(file)
		if identityErr != nil {
			return nil, identityErr
		}
		if observed[int(file.Fd())] != identity {
			return nil, errors.New("native descriptor observer cannot represent exact held regular lease identities")
		}
	}
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p.status = statusRead
	p.files = append(p.files, statusRead, statusWrite)
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p.control = controlWrite
	p.files = append(p.files, controlRead, controlWrite)
	return append(transfers, statusWrite, controlRead), nil
}

func (p *shutdownLeaseProof) writeConfig() error {
	type sharedConfig struct {
		UseUnixSocket bool   `json:"use_unix_socket"`
		Directory     string `json:"pueue_directory"`
		Runtime       string `json:"runtime_directory"`
		Socket        string `json:"unix_socket_path"`
		Secret        string `json:"shared_secret_path"`
		PID           string `json:"pid_path"`
		Aliases       string `json:"alias_file"`
		Cert          string `json:"daemon_cert"`
		Key           string `json:"daemon_key"`
	}
	root := p.directory.path
	shared := sharedConfig{UseUnixSocket: true, Directory: root, Runtime: root, Socket: filepath.Join(root, "s"), Secret: filepath.Join(root, "secret"), PID: filepath.Join(root, "daemon.pid"), Aliases: filepath.Join(root, "aliases"), Cert: filepath.Join(root, "cert"), Key: filepath.Join(root, "key")}
	data, err := json.Marshal(struct {
		Shared sharedConfig `json:"shared"`
	}{Shared: shared})
	if err != nil {
		return err
	}
	return errors.Join(p.directory.write("config.json", data), p.directory.write("secret", []byte(shutdownLeaseSecret)))
}

func (p *shutdownLeaseProof) readRef(ctx context.Context, name string) (ProcessRef, error) {
	if err := waitShutdownLeaseFile(ctx, p.directory.path, name+".json"); err != nil {
		return ProcessRef{}, err
	}
	data, err := p.directory.root.ReadFile(name + ".json")
	if err != nil {
		return ProcessRef{}, err
	}
	var identity shutdownLeaseIdentity
	if err = json.Unmarshal(data, &identity); err != nil {
		return ProcessRef{}, err
	}
	return NewProcessRef(identity.PID, identity.Token)
}

func (p *shutdownLeaseProof) handoff(ctx context.Context) error {
	for _, name := range []string{"owner", "cli"} {
		ref, err := p.readRef(ctx, name)
		if err != nil {
			return err
		}
		p.refs[name] = ref
		watch, watchErr := newShutdownLeaseExitWatch(ref)
		if watchErr != nil {
			return fmt.Errorf("register %s exit: %w", name, watchErr)
		}
		p.watches[name] = watch
	}
	if err := p.caller.wait(ctx); err != nil {
		return err
	}
	connection, err := p.listener.AcceptUnix()
	if err != nil {
		return err
	}
	p.connection = connection
	if err = connection.SetDeadline(p.deadline); err != nil {
		return err
	}
	frame, err := readShutdownLeaseFrame(connection)
	if err != nil {
		return err
	}
	if string(frame) != shutdownLeaseSecret {
		return errors.New("owned native protocol gate secret mismatch")
	}
	return shutdownLeasePeer(connection, p.refs["cli"].PID())
}

func (p *shutdownLeaseProof) inspect(ctx context.Context, wantGeneration bool) error {
	ref := p.refs["cli"]
	if err := shutdownLeaseRequirePresent(ref); err != nil {
		return err
	}
	entries, err := shutdownLeaseDescriptors(ctx, ref.PID())
	if err != nil {
		return err
	}
	retained := false
	for _, identity := range entries {
		if identity == p.generation {
			retained = true
		}
		for _, excluded := range p.excluded {
			if identity == excluded {
				return errors.New("excluded queue/task/lifetime/instance lease leaked to actuator")
			}
		}
	}
	if retained != wantGeneration {
		return fmt.Errorf("actual actor bootstrap FD retention=%t, want %t", retained, wantGeneration)
	}
	return shutdownLeaseRequirePresent(ref)
}

func shutdownLeaseRequirePresent(ref ProcessRef) error {
	present, err := ProcessPresent(ref)
	if !present {
		return errors.Join(errors.New("exact gated actor is not live"), err)
	}
	return err
}

func (p *shutdownLeaseProof) adopted(ctx context.Context) error {
	return waitForEmpty(ctx, 10*time.Millisecond, func() (bool, error) {
		current, err := nestedObserveNativeContext(ctx, p.refs["cli"].PID(), "")
		if err != nil {
			return false, err
		}
		observed, err := NewProcessRef(current.PID, current.StartToken)
		if err != nil {
			return false, err
		}
		same, err := SameProcess(observed, p.refs["cli"])
		if !same {
			return false, errors.Join(errors.New("CLI identity changed during adoption"), err)
		}
		return current.Present && current.ParentPID != p.refs["owner"].PID(), err
	}, sleepContext)
}

func (p *shutdownLeaseProof) ipcClosed() error {
	if err := shutdownLeaseRequirePresent(p.refs["cli"]); err != nil {
		return err
	}
	if err := p.status.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	var value [1]byte
	n, readErr := p.status.Read(value[:])
	if n != 0 || !errors.Is(readErr, io.EOF) {
		return errors.Join(errors.New("status writer survived owner death"), readErr)
	}
	_, writeErr := p.control.Write([]byte("p"))
	if !errors.Is(writeErr, syscall.EPIPE) {
		return errors.Join(errors.New("control reader survived owner death"), writeErr)
	}
	return shutdownLeaseRequirePresent(p.refs["cli"])
}

func (p *shutdownLeaseProof) requireProbe(expected string) (resultErr error) {
	path := filepath.Join(p.directory.path, "bootstrap.lock")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	acquiredErr := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if acquiredErr != nil && !errors.Is(acquiredErr, unix.EWOULDBLOCK) {
		return acquiredErr
	}
	actual := "acquired"
	if acquiredErr != nil {
		actual = "busy"
	}
	descriptor, err := shutdownLeaseFileIdentity(file)
	if err != nil {
		return err
	}
	var named unix.Stat_t
	if err = unix.Lstat(path, &named); err != nil {
		return err
	}
	if descriptor != p.generation || descriptor != (shutdownLeaseFileID{Device: uint64(named.Dev), Inode: named.Ino}) {
		actual = "identity_changed"
	}
	if actual != expected {
		return fmt.Errorf("fresh independent OFD=%s, want %s", actual, expected)
	}
	return nil
}

func (p *shutdownLeaseProof) replacement() error {
	if err := p.directory.root.Rename("bootstrap.lock", "bootstrap.old"); err != nil {
		return err
	}
	replacement, err := p.directory.root.OpenFile("bootstrap.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err = replacement.Close(); err != nil {
		return err
	}
	if err = p.requireProbe("identity_changed"); err != nil {
		return err
	}
	if err = p.directory.root.Remove("bootstrap.lock"); err != nil {
		return err
	}
	if err = p.directory.root.Rename("bootstrap.old", "bootstrap.lock"); err != nil {
		return err
	}
	return p.requireProbe("busy")
}

func (p *shutdownLeaseProof) cleanup(parent context.Context, t *testing.T) {
	t.Helper()
	if p.directory == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	closeErr := p.closeActors(ctx)
	closeErr = errors.Join(closeErr, closeShutdownLeaseFiles(p.files))
	for _, watch := range p.watches {
		closeErr = errors.Join(closeErr, watch.close())
	}
	if closeErr != nil {
		t.Errorf("retained native C3 diagnostics %s: %v", p.directory.path, closeErr)
	}
	if err := p.directory.finish(closeErr == nil); err != nil {
		t.Error(err)
	}
}

func (p *shutdownLeaseProof) closeActors(ctx context.Context) (resultErr error) {
	if p.directory.root != nil {
		resultErr = p.directory.write("abort", []byte("no-more-launches\n"))
	}
	if p.connection != nil {
		resultErr = errors.Join(resultErr, p.connection.Close())
	}
	if p.listener != nil {
		resultErr = errors.Join(resultErr, p.listener.Close())
	}
	if p.control != nil {
		_, err := p.control.Write([]byte("q"))
		if !errors.Is(err, syscall.EPIPE) {
			resultErr = errors.Join(resultErr, err)
		}
	}
	if p.caller == nil {
		return resultErr
	}
	resultErr = errors.Join(resultErr, p.caller.close(ctx))
	for _, name := range []string{"owner", "cli"} {
		ref, ok := p.refs[name]
		if !ok {
			var err error
			ref, err = p.readRef(ctx, name)
			if err != nil {
				resultErr = errors.Join(resultErr, err)
				continue
			}
			p.refs[name] = ref
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

func testShutdownLeaseCancellation(t *testing.T) {
	directory, err := newNativeSchedulerDirectory()
	var child *shutdownLeaseChild
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var closeErr error
		if child != nil {
			closeErr = child.close(ctx)
		}
		if closeErr != nil {
			t.Errorf("retained version cancellation fixture %s: %v", directory.path, closeErr)
		}
		if finishErr := directory.finish(closeErr == nil); finishErr != nil {
			t.Error(finishErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	command, err := shutdownLeaseFixtureCommand(directory.path, "version-gate", "", "", time.Now().Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	child, err = startShutdownLeaseChild(command)
	if err != nil {
		t.Fatal(err)
	}
	if err = waitShutdownLeaseFile(ctx, directory.path, "version.json"); err != nil {
		t.Fatal(err)
	}
	cancel()
	settleErr := settleShutdownLeaseChild(ctx, child)
	if !errors.Is(settleErr, context.Canceled) {
		t.Fatalf("canceled version executor error=%v", settleErr)
	}
	select {
	case <-child.done:
	default:
		t.Fatal("canceled version executor returned without exact child Wait")
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
	defer cleanupCancel()
	if err = WaitProcessAbsent(cleanupCtx, child.ref); err != nil {
		t.Fatal(err)
	}
}

// Keep independently selected manifest parsing observable on ordinary discovery.
func TestShutdownLeasePinnedAsset(t *testing.T) {
	asset, err := shutdownLeaseAsset()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := shutdownLeasePinnedSHA(asset)
	if err != nil {
		t.Fatal(err)
	}
	if len(expected) != 64 {
		t.Fatal("independent fixture digest must be SHA256")
	}
	if _, err = strconv.ParseUint(expected[:8], 16, 32); err != nil {
		t.Fatal(err)
	}
}
