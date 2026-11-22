package infrastructurehost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// The exact boundary after the outer observation is deterministic: A has now
// completed, but its inspection edge remains stale. Historical records are
// DATA; the real rooted store/EX and authenticated request client are exercised.
func TestUpgradeReadyCurrentContactsCustodianWithoutRetirement(t *testing.T) {
	store, client, _ := newOwnedPreparationProbe(t)
	pin, operation := seedCompletedInspectionRefresh(t, store, client.Binding())
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	startup, reference, stops, contacts := newUpgradeInspectionPeer(t, ctx, store, pin, operation)
	reader, err := taskdir.OpenReadOnlyStoreContext(ctx, store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	observed, terminal, err := releaseUpgradeOwnerRefreshServing(ctx, reader, pin, operation, startup)
	if err != nil || !terminal || !observed.Completed || observed.OperationID != pueue.UpgradeOperationID(operation) || stops.Load() != 0 {
		t.Fatalf("completed derived cleanup retired custodian: terminal=%t completed=%t stops=%d err=%v", terminal, observed.Completed, stops.Load(), err)
	}
	contact := infrastructurePreparationContact{resources: &infrastructureRoutingResources{reader: reader, target: store}, selection: infrastructureRoutingSelection{owner: startup}}
	// Even a caller requesting refresh cannot retire an owner before ReadyCurrent fences A.
	outcome, err := contact.requestUpgrade(ctx, pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeReadyCurrent, OperationID: operation, Context: reference}, true)
	if err != nil || outcome.Condition != infrastructure.UpgradePending || !outcome.Report.Completed || outcome.Report.OperationID != operation || contacts.Load() != 1 || stops.Load() != 0 {
		t.Fatalf("completed cleanup did not contact current owner: outcome=%+v contacts=%d stops=%d err=%v", outcome, contacts.Load(), stops.Load(), err)
	}
}

// The authored daemon is finite and joined. Removing its fixture readiness
// marker models an unavailable endpoint, not closure of a real Pueue daemon.
func TestUpgradeInspectionOrdinaryEnsurePreservesExistingStartupAuthority(t *testing.T) {
	store, client, owner := newOwnedPreparationProbe(t)
	pin, _ := seedCompletedInspectionRefresh(t, store, client.Binding())
	base := filepath.Dir(client.Binding().ConfigPath)
	// The fixture listener is separately owned from its finite daemon role.
	// A live listener plus refused CLI status correctly means a hard refusal,
	// not daemon absence. Close and join that exact owner before withdrawing
	// readiness; its normal cleanup remains idempotent.
	if err := owner.listener.Close(); err != nil {
		t.Fatal(err)
	}
	<-owner.listenerDone
	if err := os.Remove(filepath.Join(base, "ready")); err != nil {
		t.Fatal(err)
	}
	selected := infrastructure.LaunchContext{Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS, SupervisorExecutable: client.Binding().ClientExecutable, ControlEnvironment: []string{}, MaintenanceEnvironment: []string{}}
	digest, err := selected.Digest()
	if err != nil {
		t.Fatal(err)
	}
	observation, err := infrastructure.NewPreparationObservation(owner, infrastructure.LaunchContextReference{SHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	options := pueue.Options{ObservationTimeout: 10 * time.Second, WaitForPendingClose: true, PrivateGenerationGuard: owner, DaemonWaitOwner: owner, DaemonStartGuard: infrastructurePreparationGuard(func(ctx context.Context, root string, start func() error) error {
		starts.Add(1)
		return owner.GuardDaemonStart(ctx, root, start)
	})}
	preparation := infrastructurePreparationOwner{store: store, options: options, observation: observation, lookup: installation.ExecutableLookup{InitialExecutable: selected.SupervisorExecutable}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	outcome, err := preparation.performUpgrade(ctx, pin, infrastructure.UpgradeRequest{Action: infrastructure.UpgradeEnsure}, selected)
	if err != nil || outcome.Condition != infrastructure.UpgradeOK || outcome.Report != (infrastructure.UpgradeReport{}) || starts.Load() != 1 {
		t.Fatalf("ordinary restart was replaced by endless no-Start pending: starts=%d outcome=%+v err=%v", starts.Load(), outcome, err)
	}
	present, matches, err := inspection.GroupHeadMatchesBinding(ctx, store, client.Binding())
	if err != nil || !present || !matches {
		t.Fatalf("ordinary startup did not finish derived readiness: %v", err)
	}
}

func seedCompletedInspectionRefresh(t *testing.T, store *taskdir.Store, binding task.SupervisorRef) (taskdir.RootPin, infrastructure.UpgradeOperationID) {
	t.Helper()
	pin, a, _ := seedCompletedUpgradeAndCurrentDrain(t, store, binding)
	// This helper's B was only historical fixture DATA; it has no native work.
	if err := os.Remove(filepath.Join(store.Root, ".supervisor", "bootstrap.pending")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root, ".supervisor", "migration-history.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data, legacy := prependHistoricalInspectionRefresh(t, data, binding)
	writeFixtureFile(t, path, data)
	control, err := store.OpenInspectionGroup(true)
	if err != nil {
		t.Fatal(err)
	}
	_, putErr := control.Put("request.json", supervisor.InspectionGroup(store.RootID, legacy))
	if err = errors.Join(putErr, control.Close()); err != nil {
		t.Fatal(err)
	}
	due, err := supervisor.UpgradeInspectionCleanupDue(t.Context(), store, pueue.UpgradeOperationID(a))
	if err != nil || !due {
		t.Fatalf("fixture has no completed current cleanup: due=%t err=%v", due, err)
	}
	return pin, a
}

func prependHistoricalInspectionRefresh(t *testing.T, data []byte, binding task.SupervisorRef) ([]byte, task.SupervisorRef) {
	t.Helper()
	var history struct {
		SchemaVersion int               `json:"schema_version"`
		Migrations    []json.RawMessage `json:"migrations"`
	}
	if err := json.Unmarshal(data, &history); err != nil {
		t.Fatal(err)
	}
	var prior map[string]json.RawMessage
	if err := json.Unmarshal(history.Migrations[0], &prior); err != nil {
		t.Fatal(err)
	}
	delete(prior, "upgrade_generation")
	var identity map[string]json.RawMessage
	if err := json.Unmarshal(prior["legacy"], &identity); err != nil {
		t.Fatal(err)
	}
	legacy := binding
	legacy.ConfigDigest = task.ComputeSHA256([]byte("prior inspection head"))
	digest, err := json.Marshal(legacy.ConfigDigest)
	identity["config_digest"] = digest
	if err != nil {
		t.Fatal(err)
	}
	prior["legacy"], err = json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	history.Migrations = append([]json.RawMessage{ordinary}, history.Migrations...)
	data, err = task.MarshalCanonical(history)
	if err != nil {
		t.Fatal(err)
	}
	return data, legacy
}

func newUpgradeInspectionPeer(t *testing.T, ctx context.Context, store *taskdir.Store, pin taskdir.RootPin, operation infrastructure.UpgradeOperationID) (infrastructure.StartupReference, infrastructure.LaunchContextReference, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	selected := infrastructure.LaunchContext{Protocol: infrastructure.RecordProtocolVersion, Root: store.Root, RootID: store.RootID, OS: runtime.GOOS, SupervisorExecutable: "/fixture/pueue", ControlEnvironment: []string{}, MaintenanceEnvironment: []string{}}
	reference, err := infrastructure.StageLaunchContext(ctx, store, selected)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	intent := infrastructure.StartupIntent{Protocol: infrastructure.RecordProtocolVersion, Sequence: 1, Epoch: strings.Repeat("a", 32), Inputs: infrastructure.StartupInputs{Kind: infrastructure.StartupRootOwner, Root: pin.Root(), RootID: pin.RootID(), BootID: boot, RootDevice: pin.Device(), RootInode: pin.Inode(), Artifacts: infrastructure.Artifacts{Runner: canonicalTestExecutable(t)}, Context: reference}, AcceptedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), Deadline: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	intent.InputsSHA256, err = intent.Inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	startup, err := infrastructure.ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	stops, contacts := new(atomic.Int32), new(atomic.Int32)
	endpoint := serveUpgradeInspectionPeer(t, ctx, pin, startup, operation, stops, contacts)
	lease, acquired, err := infrastructure.TryAcquireLease(ctx, filepath.Join(store.Root, ".infrastructure-owner.lock"))
	if err != nil || !acquired {
		t.Fatalf("owner lease: acquired=%t err=%v", acquired, err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	publishUpgradeStopPeer(t, ctx, store, intent, startup, endpoint)
	return startup, reference, stops, contacts
}

func serveUpgradeInspectionPeer(t *testing.T, ctx context.Context, pin taskdir.RootPin, startup infrastructure.StartupReference, operation infrastructure.UpgradeOperationID, stops, contacts *atomic.Int32) infrastructure.EndpointIdentity {
	t.Helper()
	path := filepath.Join(pin.Root(), "0000000000001")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	joined := make(chan struct{})
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
		<-joined
	})
	go func() {
		defer close(joined)
		for {
			connection, acceptErr := listener.AcceptUnix()
			if errors.Is(acceptErr, net.ErrClosed) {
				return
			}
			if acceptErr != nil {
				t.Error(acceptErr)
				return
			}
			if replyErr := replyUpgradeInspection(ctx, connection, pin, startup, operation, stops, contacts); replyErr != nil {
				t.Error(replyErr)
			}
		}
	}()
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	var socket unix.Stat_t
	if err = unix.Lstat(path, &socket); err != nil {
		t.Fatal(err)
	}
	return infrastructure.EndpointIdentity{Path: path, DirectoryDev: pin.Device(), DirectoryInode: pin.Inode(), SocketDev: uint64(socket.Dev), SocketInode: uint64(socket.Ino)}
}

// A finite wire double echoes the authenticated subject and bounded pending
// projection. Native mutation/phase behavior is not implemented by this peer.
func replyUpgradeInspection(ctx context.Context, connection *net.UnixConn, pin taskdir.RootPin, startup infrastructure.StartupReference, operation infrastructure.UpgradeOperationID, stops, contacts *atomic.Int32) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return err
	}
	var request map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(connection, task.MaxControlRecordSize)).Decode(&request); err != nil {
		return err
	}
	if _, stop := request["stop_protocol"]; stop {
		stops.Add(1)
		request["code"] = json.RawMessage(`"busy"`)
		return json.NewEncoder(connection).Encode(request)
	}
	var actual infrastructure.UpgradeRequest
	if err := json.Unmarshal(request["request"], &actual); err != nil {
		return err
	}
	var err error
	if actual.Action == infrastructure.UpgradeStatus {
		outcome := infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradeOK, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}}
		request["outcome"], err = json.Marshal(outcome)
		if err != nil {
			return err
		}
		return json.NewEncoder(connection).Encode(request)
	}
	contacts.Add(1)
	if actual.OperationID != operation || actual.Action != infrastructure.UpgradeReadyCurrent {
		return infrastructure.ErrIdentity
	}
	observed, _, err := pueue.ObservePrivateUpgrade(ctx, pin, pueue.UpgradeOperationID(operation))
	if err != nil {
		return err
	}
	outcome := infrastructure.UpgradeOutcome{Condition: infrastructure.UpgradePending, Preflight: infrastructure.UpgradePreflight{State: infrastructure.UpgradePreflightPassed}, ReasonCode: "inspection_edge_pending", Report: infrastructure.UpgradeReport{OperationID: operation, Mode: infrastructure.UpgradeMode(observed.Mode), Phase: infrastructure.UpgradeMigrating, Predecessor: observed.Predecessor, Completed: true}}
	request["outcome"], err = json.Marshal(outcome)
	if err != nil {
		return err
	}
	return json.NewEncoder(connection).Encode(request)
}
