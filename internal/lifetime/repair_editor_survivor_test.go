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
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The overlay editor intentionally orphans one finite fixture. Its native
// adopter owns orphan reaping; this driver owns its exact release/exit watch.
// The closure oracle always expects EX acquisition, including the bad mutant.
func TestRepairEditorSurvivorNativeProof(t *testing.T) {
	if os.Getenv("DELEGATE_REPAIR_SURVIVOR_PROOF") != "1" {
		t.Skip("explicit real-binary repair survivor counterfactual")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
	expected, err := strconv.ParseBool(os.Getenv("DELEGATE_REPAIR_SURVIVOR_EXPECT_FD"))
	if err != nil {
		t.Fatal(err)
	}
	p := &admissionNativeProof{role: "managed-repair", survivor: &repairEditorSurvivorProof{expectFD: expected}}
	p.managed = managedShutdownLeaseProof{fixture: fixture, owner: owner, role: p.role}
	p.managed.proof = shutdownLeaseProof{binary: binary, role: p.role, deadline: time.Now().Add(20 * time.Second), refs: make(map[string]ProcessRef), watches: make(map[string]*shutdownLeaseExitWatch)}
	t.Cleanup(func() { p.cleanup(ctx, t) })
	if err = p.run(ctx, t); err != nil {
		t.Fatal(err)
	}
	if err = shutdownLeaseRequirePresent(p.survivor.ref); err != nil {
		t.Fatal(err)
	}
	t.Log("surviving native child is live after CLI exit while fresh root EX acquires")
}

type repairEditorSurvivorProof struct {
	directory      *nativeSchedulerDirectory
	listener       *net.UnixListener
	connection     *net.UnixConn
	ref            ProcessRef
	watch          *shutdownLeaseExitWatch
	expectFD       bool
	launchExpected bool
	captured       bool
}

type repairEditorSurvivorRecord struct {
	PID         int    `json:"pid"`
	Token       string `json:"token"`
	EditorPID   int    `json:"editor_pid"`
	EditorToken string `json:"editor_token"`
}

func (p *repairEditorSurvivorProof) prepare(directory *nativeSchedulerDirectory, deadline time.Time) error {
	p.directory = directory
	if err := directory.write("survivor-deadline", []byte(strconv.FormatInt(deadline.UnixNano(), 10))); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(directory.path, "v"), Net: "unix"})
	p.listener = listener
	if err != nil {
		return err
	}
	return listener.SetDeadline(deadline)
}

func (p *repairEditorSurvivorProof) capture(ctx context.Context, admission shutdownLeaseFileID) error {
	if p.captured {
		return nil
	}
	connection, err := p.listener.AcceptUnix()
	p.connection = connection
	if err != nil {
		return err
	}
	deadline, bounded := ctx.Deadline()
	if !bounded {
		return errors.New("survivor capture requires original deadline")
	}
	if err = connection.SetDeadline(deadline); err != nil {
		return err
	}
	data, err := p.directory.root.ReadFile("survivor.json")
	if err != nil {
		return err
	}
	var record repairEditorSurvivorRecord
	if err = task.DecodeStrict(data, &record); err != nil {
		return err
	}
	p.ref, err = NewProcessRef(record.PID, record.Token)
	if err != nil {
		return err
	}
	if _, err = NewProcessRef(record.EditorPID, record.EditorToken); err != nil {
		return err
	}
	if err = shutdownLeasePeer(connection, p.ref.PID()); err != nil {
		return err
	}
	p.watch, err = newShutdownLeaseExitWatch(p.ref)
	if err != nil {
		return err
	}
	frame, err := readShutdownLeaseFrame(connection)
	if err != nil || string(frame) != "repair-survivor-v1" {
		return errors.Join(errors.New("survivor native handshake differs"), err)
	}
	p.captured = true
	return p.checkDescriptor(ctx, admission)
}

func (p *repairEditorSurvivorProof) checkDescriptor(ctx context.Context, admission shutdownLeaseFileID) error {
	files, err := shutdownLeaseDescriptors(ctx, p.ref.PID())
	if err != nil {
		return err
	}
	retained := false
	for _, identity := range files {
		retained = retained || identity == admission
	}
	if retained != p.expectFD {
		return fmt.Errorf("surviving child inherited RootSH=%t, want %t", retained, p.expectFD)
	}
	return nil
}

func (p *repairEditorSurvivorProof) close(ctx context.Context) error {
	var resultErr error
	if p.launchExpected && !p.captured {
		resultErr = errors.New("survivor launch attempted without verified identity; retain original evidence")
	}
	if p.connection != nil {
		resultErr = errors.Join(resultErr, p.connection.Close())
		p.connection = nil
	}
	if p.listener != nil {
		resultErr = errors.Join(resultErr, p.listener.Close())
		p.listener = nil
	}
	if p.captured {
		resultErr = errors.Join(resultErr, p.watch.wait(ctx), WaitProcessAbsent(ctx, p.ref), p.watch.close())
		if resultErr == nil {
			p.captured = false
			p.launchExpected = false
		}
	}
	return resultErr
}

func TestRepairEditorSurvivorFixture(t *testing.T) {
	if os.Getenv("DELEGATE_REPAIR_SURVIVOR_CHILD") != "1" {
		t.Skip("fixed finite repair counterfactual child")
	}
	deadline, err := strconv.ParseInt(os.Getenv("DELEGATE_REPAIR_SURVIVOR_DEADLINE"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, deadline))
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(os.Getenv("DELEGATE_REPAIR_SURVIVOR_BASE"), "v"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := connection.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = connection.SetDeadline(time.Unix(0, deadline)); err != nil {
		t.Fatal(err)
	}
	if err = writeShutdownLeaseFrame(connection, []byte("repair-survivor-v1")); err != nil {
		t.Fatal(err)
	}
	var release [1]byte
	if _, err = connection.Read(release[:]); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}
