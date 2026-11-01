package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This is a task-queue fixture, not host infrastructure. It exercises the
// existing queue owner while the resident holds E and its CLOEXEC pipe ends.
func fixturePueue(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	client, err := pueue.BindPrivate(ctx, os.Getenv("DELEGATE_TEST_PUEUE"), os.Getenv("DELEGATE_TEST_PUEUED"), os.Getenv("DL_HOST_ROOT"), pueue.Options{ObservationTimeout: 5 * time.Second, WaitForPendingClose: true})
	if err != nil {
		return err
	}
	return client.Close()
}

func TestHostEOFReleasesEWhileActualPueueRemainsAlive(t *testing.T) {
	clientPath, daemonPath := os.Getenv("DELEGATE_TEST_PUEUE"), os.Getenv("DELEGATE_TEST_PUEUED")
	if clientPath == "" || daemonPath == "" {
		t.Skip("native Pueue paths are not configured")
	}
	fixture := nativeQueue(t)
	config := fixture.config
	markers := config.Directory
	t.Cleanup(func() {
		if closeErr := fixture.closeQueue(clientPath, daemonPath); closeErr != nil {
			t.Error(closeErr)
		}
	})
	process := startHostFixture(t, config)
	query := queryFor(t, config)
	ready := eventuallyStatus(t, query, func(status Status) bool { return status.Ready })
	epoch, present, err := readLatest(t.Context(), query.Store, query.Store.RootID())
	if err != nil || !present {
		t.Fatalf("host endpoint: %t %v", present, err)
	}
	pinLostHostEndpoint(t, process, epoch.Endpoint)
	assertCapturedContext(t, markers, ready.Host.PID) // Written after queue bootstrap completes.
	client, reachable, err := pueue.ProbePrivate(t.Context(), clientPath, daemonPath, config.Store.Root, pueue.Options{ObservationTimeout: 5 * time.Second, WaitForPendingClose: true})
	if err != nil || !reachable || client == nil {
		t.Fatalf("existing task queue not ready: %t %v", reachable, err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, err = process.control.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	receive(t, process.done)
	if ready.Child == nil {
		t.Fatal("ready resident identity is absent")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err = awaitIdentityAbsent(ctx, *ready.Child); err != nil {
		t.Fatalf("queue inherited a host channel end: %v", err)
	}
	assertLeasesFree(t, config)
	if err = client.Ready(ctx); err != nil {
		t.Fatalf("task queue did not remain alive after host/resident exit: %v", err)
	}
}

type nativeQueueFixture struct {
	config Config
	parent *os.Root
	name   string
	info   os.FileInfo
	closed bool
}

// A native queue must not use automatic t.TempDir deletion: retain its unique
// root and evidence when canonical shutdown cannot establish queue closure.
func nativeQueue(t *testing.T) *nativeQueueFixture {
	t.Helper()
	directory := nativeQueueDirectory(t)
	parent, err := os.OpenRoot(filepath.Dir(directory))
	if err != nil {
		t.Fatal(err)
	}
	info, err := parent.Lstat(filepath.Base(directory))
	if err != nil {
		t.Fatal(errors.Join(err, parent.Close()))
	}
	fixture := &nativeQueueFixture{parent: parent, name: filepath.Base(directory), info: info}
	store, err := taskdir.InitStore(filepath.Join(directory, "state"))
	if err != nil {
		t.Fatal(errors.Join(err, parent.Close()))
	}
	t.Cleanup(func() {
		closeErr := store.Close()
		if fixture.closed {
			current, statErr := parent.Lstat(fixture.name)
			if statErr == nil && os.SameFile(current, info) {
				statErr = parent.RemoveAll(fixture.name)
			} else if statErr == nil {
				statErr = ErrIdentity
			}
			closeErr = errors.Join(closeErr, statErr)
		} else {
			t.Errorf("native queue closure unverified; retained owned fixture %s", directory)
		}
		if closeErr = errors.Join(closeErr, parent.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	fixture.config, err = fixtureConfig(store)
	if err != nil {
		t.Fatal(err)
	}
	markers := filepath.Join(directory, "markers")
	if err = os.Mkdir(markers, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.config.Directory = markers
	fixture.config.Environment = append(fixture.config.Environment, "DL_HOST_MODE=pueue", "DL_HOST_MARKERS="+markers, "DL_HOST_VISIBLE=")
	return fixture
}

func nativeQueueDirectory(t *testing.T) string {
	t.Helper()
	// Darwin's default TMPDIR can leave too little room for the private queue
	// socket. A short fixture directly under native HOME preserves the caller's
	// environment and avoids changing provider or global temporary settings.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".hq-")
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func (f *nativeQueueFixture) closeQueue(clientPath, daemonPath string) (resultErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, reachable, err := pueue.ProbePrivate(ctx, clientPath, daemonPath, f.config.Store.Root, pueue.Options{ObservationTimeout: 5 * time.Second, WaitForPendingClose: true})
	if err != nil {
		return err
	}
	if client == nil {
		return errors.New("native fixture queue closure is unverified")
	}
	defer func() { resultErr = errors.Join(resultErr, client.Close()) }()
	if reachable {
		if err = client.Shutdown(ctx); err != nil {
			return err
		}
	}
	if err = client.WaitUnavailable(ctx); err != nil {
		return err
	}
	f.closed = true
	return nil
}
