package pueue

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestProbePrivateWithGateRetainsLeaseThroughGate(t *testing.T) {
	fixture, original := supervisorLeaseTestBind(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	entered := make(chan struct{})
	release := make(chan struct{})
	releaseGate := sync.OnceFunc(func() { close(release) })
	done := make(chan struct{})
	var client *Client
	var ready bool
	var probeErr error
	t.Cleanup(func() {
		releaseGate()
		cancel()
		<-done
		supervisorLeaseTestSettleClient(t, client, probeErr)
	})
	go func() {
		client, ready, probeErr = ProbePrivateWithGate(ctx, fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: 10 * time.Second}, func(gateContext context.Context, _ *Client, _ QueueSnapshot) (bool, error) {
			close(entered)
			select {
			case <-release:
				return true, nil
			case <-gateContext.Done():
				return false, gateContext.Err()
			}
		})
		close(done)
	}()
	select {
	case <-entered:
	case <-done:
		t.Fatalf("probe returned before gate entry: ready=%t err=%v", ready, probeErr)
	case <-ctx.Done():
		t.Fatalf("probe gate did not enter within its setup bound: %v", ctx.Err())
	}

	supervisorLeaseTestBlocked(t, fixture.stateRoot, WithBootstrapLock)
	releaseGate()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("probe did not return after releasing its gate: %v", ctx.Err())
	}
	if probeErr != nil || !ready || client == nil {
		t.Fatalf("healthy gated probe failed: client=%v ready=%t err=%v", client, ready, probeErr)
	}
	if client.Binding() != original.Binding() {
		t.Fatalf("gated probe changed generation: got=%+v want=%+v", client.Binding(), original.Binding())
	}
	transitionEntered := false
	if err := WithBootstrapLock(ctx, fixture.stateRoot, func() error {
		transitionEntered = true
		return nil
	}); err != nil || !transitionEntered {
		t.Fatalf("gate completion retained its lease: entered=%t err=%v", transitionEntered, err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestProbePrivateGroupsLeavesFreshStateUntouched(t *testing.T) {
	stateRoot := filepath.Join(canonicalTemp(t), "state")
	client, ready, err := ProbePrivateGroupsWithGate(t.Context(), "/missing/pueue", "/missing/pueued", stateRoot, Options{ObservationTimeout: time.Second}, nil)
	if err != nil || ready || client != nil {
		t.Fatalf("fresh private probe = client=%v ready=%t err=%v, want nil false nil", client, ready, err)
	}
	if _, statErr := os.Stat(stateRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("shared private probe provisioned fresh state root: %v", statErr)
	}
}

func TestPrivateProbeOptionsPreservesCallerControlDeadline(t *testing.T) {
	caller, cancelCaller := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelCaller()
	wantDeadline, bounded := caller.Deadline()
	if !bounded {
		t.Fatal("caller control context has no deadline")
	}

	probe, cancelProbe, options, err := privateProbeOptions(caller, Options{ObservationTimeout: time.Second})
	if err != nil {
		t.Fatalf("prepare bounded probe options: %v", err)
	}
	defer cancelProbe()
	gotDeadline, bounded := probe.Deadline()
	if !bounded || !gotDeadline.Equal(wantDeadline) {
		t.Fatalf("probe deadline=%v bounded=%t, want caller deadline %v", gotDeadline, bounded, wantDeadline)
	}
	if options.ObservationTimeout != time.Second {
		t.Fatalf("probe changed per-command observation timeout: got=%s want=%s", options.ObservationTimeout, time.Second)
	}
}

func TestProbePrivateDefersIdentityPublicationToExclusivePrepare(t *testing.T) {
	fixture, original := supervisorLeaseTestBind(t)
	saved := original.Binding()
	successor, pendingData := probeScopeTestStagePublication(t, fixture.stateRoot, original)
	daemonBefore := readPrivateDaemonIdentity(t, fixture.stateRoot)

	probeScopeTestNotReady(t, fixture, nil)
	if got := readPrivateSupervisorFile(t, privateIdentityPublicationPath(fixture.stateRoot)); !bytes.Equal(got, pendingData) {
		t.Fatalf("shared probe modified the pending publication: got=%s want=%s", got, pendingData)
	}
	probeScopeTestPublishedBinding(t, fixture.stateRoot, saved)
	if got := readPrivateDaemonIdentity(t, fixture.stateRoot); got != daemonBefore {
		t.Fatalf("shared probe changed daemon identity: got=%+v want=%+v", got, daemonBefore)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var prepared *Client
	err := WithBootstrapLock(ctx, fixture.stateRoot, func() error {
		var prepareErr error
		prepared, prepareErr = PreparePrivateWithLock(ctx, fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: 10 * time.Second})
		return prepareErr
	})
	supervisorLeaseTestSettleClient(t, prepared, err)
	if err != nil || prepared == nil {
		t.Fatalf("exclusive preparation did not finish the existing publication: client=%v err=%v", prepared, err)
	}
	if !task.SameSupervisorIdentity(prepared.Binding(), successor) {
		t.Fatalf("exclusive preparation bound the wrong generation: got=%+v want=%+v", prepared.Binding(), successor)
	}
	probeScopeTestPublishedBinding(t, fixture.stateRoot, successor)
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, successor)
	assertPrivateTestPathMissing(t, privateIdentityPublicationPath(fixture.stateRoot))
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestProbePrivateDoesNotRecreateMissingDataDirectory(t *testing.T) {
	fixture, original := supervisorLeaseTestBind(t)
	dataPath := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "data")
	// Remove, rather than RemoveAll, also establishes that the fixture's data
	// directory is empty before applying the fault.
	if removeErr := os.Remove(dataPath); removeErr != nil {
		t.Fatal(removeErr)
	}

	probeScopeTestNotReady(t, fixture, nil)
	assertPrivateTestPathMissing(t, dataPath)
	probeScopeTestPublishedBinding(t, fixture.stateRoot, original.Binding())
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func TestProbePrivateRejectsRegularDataPathWithIndependentCredentials(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	base := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	makePrivateTestDirectory(t, base)
	config := strings.Replace(privateConfigYAML(base),
		"  shared_secret_path: "+strconv.Quote(filepath.Join(base, "data", "shared_secret")),
		strings.Join([]string{
			"  shared_secret_path: " + strconv.Quote(filepath.Join(base, "shared_secret")),
			"  daemon_cert: " + strconv.Quote(filepath.Join(base, "daemon.cert")),
			"  daemon_key: " + strconv.Quote(filepath.Join(base, "daemon.key")),
		}, "\n"), 1)
	if err := os.WriteFile(PrivateConfigPath(fixture.stateRoot), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	original, err := BindPrivate(ctx, fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: 10 * time.Second})
	supervisorLeaseTestSettleClient(t, original, err)
	if err != nil {
		t.Fatalf("bind healthy fixture with credentials outside data: %v", err)
	}
	dataPath := filepath.Join(base, "data")
	if removeErr := os.Remove(dataPath); removeErr != nil {
		t.Fatal(removeErr)
	}
	fileData := []byte("regular file at the configured data directory\n")
	if writeErr := os.WriteFile(dataPath, fileData, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	resolution, err := original.resolution()
	if err != nil {
		t.Fatal(err)
	}
	// Keep config/isolation validation successful after the fault: credentials
	// under data would instead fail with ENOTDIR and mask the directory check.
	if _, err := readPrivateConfig(original.Binding().ConfigPath, base, resolution); err != nil {
		t.Fatalf("fixture masks the directory-type check with an invalid config: %v", err)
	}

	probeScopeTestNotReady(t, fixture, ErrConfiguration)
	if got := readPrivateSupervisorFile(t, dataPath); !bytes.Equal(got, fileData) {
		t.Fatalf("probe changed the regular data file: got=%q want=%q", got, fileData)
	}
	probeScopeTestPublishedBinding(t, fixture.stateRoot, original.Binding())
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}

func probeScopeTestNotReady(t *testing.T, fixture privateSupervisorTestFixture, wantErr error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	gateEntered := false
	client, ready, err := ProbePrivateWithGate(ctx, fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: 10 * time.Second}, func(context.Context, *Client, QueueSnapshot) (bool, error) {
		gateEntered = true
		return true, nil
	})
	supervisorLeaseTestSettleClient(t, client, err)
	if client != nil || ready || gateEntered || !errors.Is(err, wantErr) {
		t.Fatalf("probe admitted an incomplete supervisor: client=%v ready=%t gate=%t err=%v wantErr=%v", client, ready, gateEntered, err, wantErr)
	}
}

func probeScopeTestStagePublication(t *testing.T, stateRoot string, current *Client) (task.SupervisorRef, []byte) {
	t.Helper()
	saved := current.Binding()
	appendPrivateSupervisorFile(t, saved.ConfigPath, "\n# pending comment-only successor\n")
	successor, err := current.readBinding()
	if err != nil {
		t.Fatal(err)
	}
	if successor.ConfigDigest == saved.ConfigDigest || successor.ResolvedConfigSHA256 != saved.ResolvedConfigSHA256 || successor.Endpoint != saved.Endpoint {
		t.Fatalf("comment changed more than the config bytes: saved=%+v successor=%+v", saved, successor)
	}
	pendingData, err := task.MarshalCanonical(privateIdentityPublicationRecord{
		SchemaVersion: task.SchemaVersion,
		Binding:       successor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := publishPrivateRecord(filepath.Dir(saved.ConfigPath), privateIdentityPublicationPath(stateRoot), pendingData); err != nil {
		t.Fatal(err)
	}
	if pending, found, err := readPrivateIdentityPublication(stateRoot); err != nil || !found || pending.Binding != successor {
		t.Fatalf("publication fixture is not a valid pending successor: found=%t pending=%+v err=%v", found, pending, err)
	}
	return successor, pendingData
}

func probeScopeTestPublishedBinding(t *testing.T, stateRoot string, want task.SupervisorRef) {
	t.Helper()
	got, found, err := loadPrivateSupervisorBinding(stateRoot)
	if err != nil || !found || got != want {
		t.Fatalf("unexpected published supervisor: found=%t got=%+v want=%+v err=%v", found, got, want, err)
	}
}

func TestProbePrivateGateRetainsLeaseForPendingStatus(t *testing.T) {
	fixture, _ := supervisorLeaseTestBind(t)
	base := canonicalTemp(t)
	started, release := filepath.Join(base, "started"), filepath.Join(base, "release")
	defer releasePrivateStatusOnCleanup(t, release)
	client, ready, err := ProbePrivateWithGate(t.Context(), fixture.clientPath, fixture.daemonPath, fixture.stateRoot, Options{ObservationTimeout: 10 * time.Second}, func(ctx context.Context, current *Client, _ QueueSnapshot) (bool, error) {
		faultContext, cancel := context.WithCancel(ctx)
		defer cancel()
		current.options.Environment = append(os.Environ(), "PRIVATE_STATUS_STARTED="+started, "PRIVATE_STATUS_RELEASE="+release)
		current.options.Observer = func(event CommandEvent) {
			if event.Stage == "started" && strings.Contains("\x00"+strings.Join(event.Argv, "\x00")+"\x00", "\x00status\x00") {
				cancel()
			}
		}
		_, statusErr := current.Snapshot(faultContext)
		return statusErr == nil, statusErr
	})
	pending := pendingFrom(err)
	if client != nil || ready || pending == nil {
		t.Fatalf("gate lost its pending status command: client=%v ready=%t err=%v", client, ready, err)
	}
	supervisorLeaseTestBlocked(t, fixture.stateRoot, WithBootstrapLock)
	releasePrivateStatus(t, release)
	waitPending(t, pending)
	supervisorLeaseTestEnters(t, fixture.stateRoot, WithBootstrapLock)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 1)
}
