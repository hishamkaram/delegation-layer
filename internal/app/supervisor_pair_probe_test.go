package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestSupervisorPairProbeHealthyAdmissionSharesLeaseWithoutWrites(t *testing.T) {
	fixture := newSupervisorPairProbeFixture(t)
	release := holdSupervisorPairProbeLease(t, fixture.root)
	freezeSupervisorPairProbeTimes(t, fixture)
	before := snapshotSupervisorPairProbe(t, fixture.base)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := installStateSupervisorPairForAdmission(ctx, fixture.root, fixture.sourceClient, fixture.sourceDaemon); err != nil {
		t.Fatalf("healthy pair admission required exclusive ownership: %v", err)
	}
	assertSupervisorPairProbeUnchanged(t, fixture.base, before)
	assertSupervisorPairProbeNoStages(t, fixture.base)
	release()
}

func TestSupervisorPairProbeRecoveryFallbackSharesLeaseWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name          string
		missingMember bool
	}{
		{name: "healthy"},
		{name: "missing-member", missingMember: true},
	} {
		t.Run(tc.name, func(t *testing.T) { testSupervisorPairProbeRecoveryFallback(t, tc.missingMember) })
	}
}

func testSupervisorPairProbeRecoveryFallback(t *testing.T, missingMember bool) {
	t.Helper()
	fixture := newSupervisorPairProbeFixture(t)
	binding := savedSupervisorPairRef(t, fixture.sourceClient, fixture.sourceDaemon)
	for _, source := range []string{fixture.sourceClient, fixture.sourceDaemon} {
		if err := os.Remove(source); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", canonicalAppTestTempDir(t))
	missingDelegate := filepath.Join(canonicalAppTestTempDir(t), "missing", "delegate")
	if missingMember {
		if err := os.Remove(fixture.daemon); err != nil {
			t.Fatal(err)
		}
	}
	release := holdSupervisorPairProbeLease(t, fixture.root)
	freezeSupervisorPairProbeTimes(t, fixture)
	before := snapshotSupervisorPairProbe(t, fixture.base)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := installStateSupervisorPairForRecovery(ctx, fixture.root, missingDelegate, binding)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("recovery fallback waited for exclusive ownership: %v", err)
	}
	if missingMember {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing member without an available package was not rejected: %v", err)
		}
	} else {
		if err != nil {
			t.Fatalf("recovery rejected the healthy published pair: %v", err)
		}
		client, daemon := requireResolvedSupervisorPair(t, fixture.base, "saved client", "saved daemon")
		if client != fixture.client || daemon != fixture.daemon {
			t.Fatal("recovery changed the published pair identity")
		}
	}
	assertSupervisorPairProbeUnchanged(t, fixture.base, before)
	assertSupervisorPairProbeNoStages(t, fixture.base)
	release()
}

func TestSupervisorPairProbeChangedSourceWaitsForExclusiveOwnership(t *testing.T) {
	for _, member := range []string{"client", "daemon"} {
		t.Run(member, func(t *testing.T) { testSupervisorPairProbeChangedSource(t, member) })
	}
}

func testSupervisorPairProbeChangedSource(t *testing.T, member string) {
	t.Helper()
	fixture := newSupervisorPairProbeFixture(t)
	release := holdSupervisorPairProbeLease(t, fixture.root)
	// Probe once before changing the same source path to catch stale digest
	// reuse as well as accidental publication under a shared lease.
	installSupervisorPairProbeFixture(t, fixture)
	wantClient, wantDaemon := "saved client", "saved daemon"
	if member == "client" {
		wantClient = "changed client"
		writeRunnerFixture(t, fixture.sourceClient, wantClient)
	} else {
		wantDaemon = "changed daemon"
		writeRunnerFixture(t, fixture.sourceDaemon, wantDaemon)
	}
	freezeSupervisorPairProbeTimes(t, fixture)
	before := snapshotSupervisorPairProbe(t, fixture.base)
	assertSupervisorPairProbeWaits(t, fixture)
	assertSupervisorPairProbeUnchanged(t, fixture.base, before)
	release()

	installSupervisorPairProbeFixture(t, fixture)
	client, daemon := requireResolvedSupervisorPair(t, fixture.base, wantClient, wantDaemon)
	requireSupervisorPairGenerationChanged(t, fixture.client, fixture.daemon, client, daemon)
	requireRunnerFixture(t, fixture.client, "saved client")
	requireRunnerFixture(t, fixture.daemon, "saved daemon")
	assertSupervisorPairProbeNoStages(t, fixture.base)
}

func TestSupervisorPairProbeCorruptionFailsWithoutOverwrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(*testing.T, supervisorPairProbeFixture)
	}{
		{name: "client-bytes", tamper: func(t *testing.T, f supervisorPairProbeFixture) {
			writeRunnerFixture(t, f.client, "corrupted client")
		}},
		{name: "daemon-bytes", tamper: func(t *testing.T, f supervisorPairProbeFixture) {
			writeRunnerFixture(t, f.daemon, "corrupted daemon")
		}},
		{name: "pointer-record", tamper: func(t *testing.T, f supervisorPairProbeFixture) {
			if err := os.WriteFile(filepath.Join(f.base, stateSupervisorPairRecordName), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "client-permissions", tamper: func(t *testing.T, f supervisorPairProbeFixture) {
			if err := os.Chmod(f.client, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "daemon-symlink", tamper: func(t *testing.T, f supervisorPairProbeFixture) {
			if err := os.Remove(f.daemon); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.sourceDaemon, f.daemon); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSupervisorPairProbeFixture(t)
			tc.tamper(t, fixture)
			release := holdSupervisorPairProbeLease(t, fixture.root)
			freezeSupervisorPairProbeTimes(t, fixture)
			before := snapshotSupervisorPairProbe(t, fixture.base)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := installStateSupervisorPairForAdmission(ctx, fixture.root, fixture.sourceClient, fixture.sourceDaemon)
			if !errors.Is(err, pueue.ErrConfiguration) {
				t.Fatalf("corrupted private pair was repaired or deferred instead of rejected: %v", err)
			}
			assertSupervisorPairProbeUnchanged(t, fixture.base, before)
			assertSupervisorPairProbeNoStages(t, fixture.base)
			release()
		})
	}
}

func TestSupervisorPairProbeMissingMemberRepairsOnlyAfterLeaseRelease(t *testing.T) {
	for _, member := range []string{"client", "daemon"} {
		t.Run(member, func(t *testing.T) { testSupervisorPairProbeMissingMember(t, member) })
	}
}

func testSupervisorPairProbeMissingMember(t *testing.T, member string) {
	t.Helper()
	fixture := newSupervisorPairProbeFixture(t)
	missing := fixture.client
	if member == "daemon" {
		missing = fixture.daemon
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	release := holdSupervisorPairProbeLease(t, fixture.root)
	freezeSupervisorPairProbeTimes(t, fixture)
	before := snapshotSupervisorPairProbe(t, fixture.base)
	assertSupervisorPairProbeWaits(t, fixture)
	assertSupervisorPairProbeUnchanged(t, fixture.base, before)
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing member was repaired under a shared lease: %v", err)
	}
	release()

	installSupervisorPairProbeFixture(t, fixture)
	client, daemon := requireResolvedSupervisorPair(t, fixture.base, "saved client", "saved daemon")
	if client != fixture.client || daemon != fixture.daemon {
		t.Fatal("repair changed the immutable pair identity")
	}
	assertSupervisorPairProbeNoStages(t, fixture.base)
}

type supervisorPairProbeFixture struct {
	root, base                 string
	sourceClient, sourceDaemon string
	client, daemon             string
}

func newSupervisorPairProbeFixture(t *testing.T) supervisorPairProbeFixture {
	t.Helper()
	fixture := supervisorPairProbeFixture{root: canonicalAppTestTempDir(t)}
	fixture.base = filepath.Join(fixture.root, ".supervisor")
	fixture.sourceClient, fixture.sourceDaemon = writeSavedSupervisorPair(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := pueue.WithBootstrapLock(ctx, fixture.root, func() error {
		return installStateSupervisorPair(ctx, fixture.root, fixture.sourceClient, fixture.sourceDaemon)
	})
	if err != nil {
		t.Fatalf("install initial pair before the lock fault: %v", err)
	}
	fixture.client, fixture.daemon = requireResolvedSupervisorPair(t, fixture.base, "saved client", "saved daemon")
	return fixture
}

func installSupervisorPairProbeFixture(t *testing.T, fixture supervisorPairProbeFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := installStateSupervisorPairForAdmission(ctx, fixture.root, fixture.sourceClient, fixture.sourceDaemon); err != nil {
		t.Fatalf("prepare supervisor pair: %v", err)
	}
}

func assertSupervisorPairProbeWaits(t *testing.T, fixture supervisorPairProbeFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := installStateSupervisorPairForAdmission(ctx, fixture.root, fixture.sourceClient, fixture.sourceDaemon)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pair mutation did not wait for exclusive ownership: %v", err)
	}
}

// The test owns the holder goroutine. Callback entry proves the lease is held;
// both explicit release and test cleanup join completion before deleting files.
func holdSupervisorPairProbeLease(t *testing.T, root string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var resultErr error
	finish := sync.OnceFunc(func() {
		close(release)
		<-done
		cancel()
		if resultErr != nil {
			t.Errorf("shared supervisor lease holder failed: %v", resultErr)
		}
	})
	t.Cleanup(finish)
	go func() {
		resultErr = pueue.WithSupervisorLease(ctx, root, func() error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		close(done)
	}()
	select {
	case <-entered:
	case <-done:
		t.Fatalf("shared supervisor lease failed before entry: %v", resultErr)
	case <-ctx.Done():
		t.Fatalf("shared supervisor lease setup exceeded its deadline: %v", ctx.Err())
	}
	return finish
}

func freezeSupervisorPairProbeTimes(t *testing.T, fixture supervisorPairProbeFixture) {
	t.Helper()
	// A fixed old mtime makes even transient create/unlink staging observable
	// without sleeping for the filesystem's timestamp resolution.
	old := time.Unix(946684800, 0).UTC()
	for _, path := range []string{fixture.base, filepath.Dir(fixture.client), filepath.Join(fixture.base, stateSupervisorPairRecordName)} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

type supervisorPairProbeEntry struct {
	info os.FileInfo
	data []byte
}

func snapshotSupervisorPairProbe(t *testing.T, base string) map[string]supervisorPairProbeEntry {
	t.Helper()
	entries := make(map[string]supervisorPairProbeEntry)
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		saved := supervisorPairProbeEntry{info: info}
		if info.Mode().IsRegular() {
			saved.data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		entries[path] = saved
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func assertSupervisorPairProbeUnchanged(t *testing.T, base string, before map[string]supervisorPairProbeEntry) {
	t.Helper()
	after := snapshotSupervisorPairProbe(t, base)
	if len(before) != len(after) {
		t.Fatalf("pair probe changed directory entries: before=%d after=%d", len(before), len(after))
	}
	for path, original := range before {
		current, ok := after[path]
		if !ok {
			t.Fatalf("pair probe removed %s", path)
		}
		if !os.SameFile(original.info, current.info) || !original.info.ModTime().Equal(current.info.ModTime()) || original.info.Mode() != current.info.Mode() || !bytes.Equal(original.data, current.data) {
			t.Errorf("pair probe changed inode, mtime, mode, or bytes of %s", path)
		}
	}
}

func assertSupervisorPairProbeNoStages(t *testing.T, base string) {
	t.Helper()
	for path := range snapshotSupervisorPairProbe(t, base) {
		name := filepath.Base(path)
		if strings.HasPrefix(name, ".pueue-pair-") || strings.HasPrefix(name, ".delegate-run-") {
			t.Errorf("supervisor pair preparation left a stage: %s", path)
		}
	}
}
