package verifiedexec

// Legacy identity fixtures and read-only guard test helpers.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// failAt makes the production operation named by point fail with err on every
// attempt until the returned function is called.
func failAt(pool *Pool, point poolPoint, err error) (reset func()) {
	pool.faults.inject = func(got poolPoint) error {
		if got == point {
			return err
		}
		return nil
	}
	return func() { pool.faults.inject = nil }
}

func rootNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

func identityStages(t *testing.T, root string) []string {
	t.Helper()
	var stages []string
	for _, name := range rootNames(t, root) {
		if strings.HasPrefix(name, poolIdentityStagePrefix) {
			stages = append(stages, name)
		}
	}
	return stages
}

// markerID reads the marker with the production strict reader's own decoder.
func markerID(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, poolIdentityName))
	if err != nil {
		t.Fatal(err)
	}
	id, err := decodePoolIdentity(raw)
	if err != nil {
		t.Fatalf("marker is not canonical: %v", err)
	}
	return id
}

// plantMarker writes marker bytes with an exact mode, replacing any marker.
func plantMarker(t *testing.T, root string, content []byte, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, poolIdentityName)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func canonicalMarker(t *testing.T, id string) []byte {
	t.Helper()
	return []byte(`{"version":1,"pool_id":"` + id + `"}`)
}

func requireNoIdentityStage(t *testing.T, root string) {
	t.Helper()
	if stages := identityStages(t, root); len(stages) != 0 {
		t.Fatalf("marker stages remain: %v", stages)
	}
}

func TestNewPoolWithIdentityRefusesAnEmptyOrMalformedID(t *testing.T) {
	root := filepath.Join(scratch(t), "pool")
	for _, bad := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 32) + "\n" + strings.Repeat("a", 31)} {
		if pool, err := NewPoolWithIdentity(root, bad); !errors.Is(err, errPoolIdentityInvalid) || pool != nil {
			t.Fatalf("NewPoolWithIdentity(%q) gave %v, %v; want %v and no pool", bad, pool, err, errPoolIdentityInvalid)
		}
	}
	if pool, err := NewPoolWithIdentity("relative", strings.Repeat("a", 64)); !errors.Is(err, errPoolRootInvalid) || pool != nil {
		t.Fatalf("a relative root gave %v, %v; want %v", pool, err, errPoolRootInvalid)
	}
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a constructor touched the disk: %v", err)
	}
}

// holdGate takes the pool's exclusive gate the way another live publisher
// would, and returns the function that releases it. It is also registered as a
// cleanup, so an early failure still releases it exactly once.
func holdGate(t *testing.T, pool *Pool) (release func()) {
	t.Helper()
	gate, err := taskdir.OpenLockFileContext(context.Background(), pool.gatePath(), taskdir.LockLevelNone)
	if err != nil {
		t.Fatal(err)
	}
	if err = gate.LockEXNonblocking(); err != nil {
		t.Fatal(errors.Join(err, gate.Close()))
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if closeErr := errors.Join(gate.Unlock(), gate.Close()); closeErr != nil {
				t.Errorf("release the held gate: %v", closeErr)
			}
		})
	}
	t.Cleanup(release)
	return release
}
