package host

import (
	"bufio"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/jonboulle/clockwork"
)

func TestHostPassiveObserveDoesNotCreateJournalOrTouchH(t *testing.T) {
	config := testConfig(t, "hold")
	query := queryFor(t, config)
	before, err := os.ReadDir(config.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	status, err := Observe(t.Context(), query)
	if err != nil || status.State != "no_host_observed" || status.Ready {
		t.Fatalf("passive no-host result: %+v %v", status, err)
	}
	after, err := os.ReadDir(config.Store.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("passive observation changed root entries: before=%v after=%v", before, after)
	}
	for index, entry := range before {
		if entry.Name() != after[index].Name() {
			t.Fatal("passive observation changed root entries")
		}
	}
	for _, name := range []string{"scheduler-host", ".scheduler-host.lock"} {
		if _, statErr := os.Lstat(filepath.Join(config.Store.Root, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("passive path %s created: %v", name, statErr)
		}
	}
}

func epochFixture(t *testing.T, config Config, endpoint *endpoint) epochRecord {
	t.Helper()
	ref, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return epochRecord{
		Protocol: ProtocolVersion, RootID: config.Store.RootID, Sequence: 1,
		Epoch: "0123456789abcdef0123456789abcdef", BuildID: config.BuildID, Host: identity(ref), UID: os.Geteuid(), Endpoint: endpoint.path,
	}
}

func TestHostMalformedAuthenticatedEndpointIsUnreachableNotStopped(t *testing.T) {
	config := testConfig(t, "hold")
	endpoint, err := newEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := endpoint.close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	epoch := epochFixture(t, config, endpoint)
	journal := journalForStore(t, config.Store)
	if _, err = publishRecord(t.Context(), journal, epochName(1), epoch); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, acceptErr := endpoint.listener.AcceptUnix()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		var request wireRequest
		readErr := readFrame(bufio.NewReader(conn), &request)
		response := wireResponse{Status: statusWire(Status{State: "ready", Epoch: "different", Protocol: ProtocolVersion, BuildID: config.BuildID, Host: epoch.Host})}
		done <- errors.Join(readErr, writeFrame(conn, response), conn.Close())
	}()
	query := queryFor(t, config)
	status, err := Observe(t.Context(), query)
	if !errors.Is(err, ErrIdentity) || status.State != "degraded" || status.LastError != "host_unreachable" || status.Ready {
		t.Fatalf("malformed owner treated as absent/stopped: %+v %v", status, err)
	}
	if err = receive(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestHostDelayedStopCannotStopSuccessorEpoch(t *testing.T) {
	config := testConfig(t, "hold")
	query := queryFor(t, config)
	done := runLocalHost(t, config, clockwork.NewRealClock())
	eventuallyStatus(t, query, func(status Status) bool { return status.Ready })
	old, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("first epoch: %t %v", present, err)
	}
	stopLocal(t, query, done)
	nextDone := runLocalHost(t, config, clockwork.NewRealClock())
	current := eventuallyStatus(t, query, func(status Status) bool { return status.Ready && status.Epoch != old.Epoch })
	next, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("second epoch: %t %v", present, err)
	}
	conn, err := net.Dial("unix", next.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	request := wireRequest{Protocol: ProtocolVersion, RootID: old.RootID, Epoch: old.Epoch, Host: old.Host, Client: old.Host, BuildID: old.BuildID, Action: "stop"}
	if err = writeFrame(conn, request); err != nil {
		t.Fatal(err)
	}
	var response wireResponse
	readErr := readFrame(bufio.NewReader(conn), &response)
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	if readErr == nil && response.Accepted {
		t.Fatal("successor accepted a predecessor's delayed stop")
	}
	status, err := Observe(t.Context(), query)
	if err != nil || !status.Ready || status.Epoch != current.Epoch || status.StopAccepted {
		t.Fatalf("stale stop affected successor: %+v %v", status, err)
	}
	stopLocal(t, query, nextDone)
}

func TestHostEndpointRemainsShortForDeepRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("a", 60), strings.Repeat("b", 60), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	config, err := fixtureConfig(store)
	if err != nil {
		t.Fatal(err)
	}
	done := runLocalHost(t, config, clockwork.NewRealClock())
	query := queryFor(t, config)
	eventuallyStatus(t, query, func(status Status) bool { return status.Ready })
	epoch, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present || strings.HasPrefix(epoch.Endpoint, root) {
		t.Fatalf("endpoint depends on deep root: %+v %v", epoch, err)
	}
	stopLocal(t, query, done)
}
