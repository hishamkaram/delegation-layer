package pueue

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestResolvePrivateSupervisorSuccessorRequiresConnectedPublishedHistory(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		history    [][2]string
		published  string
		unrecorded bool
		wantFound  bool
	}{
		{name: "A to B", history: [][2]string{{"A", "B"}}, published: "B", wantFound: true},
		{name: "A to B to C", history: [][2]string{{"A", "B"}, {"B", "C"}}, published: "C", wantFound: true},
		{name: "reversed B to C then A to B", history: [][2]string{{"B", "C"}, {"A", "B"}}, published: "C"},
		{name: "disconnected A to B and X to C", history: [][2]string{{"A", "B"}, {"X", "C"}}, published: "C"},
		{name: "unrecorded endpoint", history: [][2]string{{"A", "B"}}, published: "B", unrecorded: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root, bindings := newPrivateSuccessorTestState(t)
			seedPrivateSuccessorTestHistory(t, root, bindings, testCase.history, testCase.published)
			saved := bindings["A"]
			if testCase.unrecorded {
				saved.Endpoint = "unix:" + filepath.Join(root, privateSupervisorDirectory, "run", "unrecorded.sock")
			}
			want := task.SupervisorRef{}
			if testCase.wantFound {
				want = bindings[testCase.published]
			}
			assertPrivateSuccessorTestResolution(t, root, saved, want, testCase.wantFound, nil)
		})
	}
}

func TestResolvePrivateSupervisorSuccessorRetainsIncompletePublication(t *testing.T) {
	for _, blocker := range []string{"migration journal", "identity publication", "recovery alias", "bootstrap journal"} {
		t.Run(blocker, func(t *testing.T) {
			root, bindings := newPrivateSuccessorTestState(t)
			seedPrivateSuccessorTestHistory(t, root, bindings, [][2]string{{"A", "B"}}, "B")
			seedPrivateSuccessorTestBlocker(t, root, bindings, blocker)
			if complete, err := PrivateSupervisorMigrationComplete(root, bindings["B"]); err != nil || complete {
				t.Fatalf("fixture is not a valid incomplete publication: complete=%t err=%v", complete, err)
			}
			assertPrivateSuccessorTestResolution(t, root, bindings["A"], task.SupervisorRef{}, false, ErrPrivateSupervisorMigrationRequired)
		})
	}
}

func TestPrivateSupervisorMigrationCompleteRejectsMismatchedDaemonIdentity(t *testing.T) {
	root, bindings := newPrivateSuccessorTestState(t)
	seedPrivateSuccessorTestHistory(t, root, bindings, [][2]string{{"A", "B"}}, "B")
	base := filepath.Join(root, privateSupervisorDirectory)
	writePrivateSuccessorTestRecord(t, privateDaemonIdentityPath(base), privateDaemonIdentity{
		Executable: bindings["A"].DaemonExecutable,
		SHA256:     bindings["A"].DaemonSHA256,
	})
	complete, err := PrivateSupervisorMigrationComplete(root, bindings["B"])
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Fatal("completed generation accepted a daemon identity from its predecessor")
	}
}

func TestPrivateSupervisorMigrationEdgeCompleteRequiresExactStoppedSuccessor(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		history        [][2]string
		published      string
		predecessor    string
		successor      string
		want           bool
		wantGuardError bool
	}{
		{name: "exact edge", history: [][2]string{{"A", "B"}}, published: "B", predecessor: "A", successor: "B", want: true},
		{name: "indirect edge", history: [][2]string{{"A", "B"}, {"B", "C"}}, published: "C", predecessor: "A", successor: "C"},
		{name: "unrelated successor", history: [][2]string{{"A", "B"}, {"X", "C"}}, published: "C", predecessor: "A", successor: "C", wantGuardError: true},
		{name: "unrelated predecessor", history: [][2]string{{"A", "B"}}, published: "B", predecessor: "X", successor: "B"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root, bindings := newPrivateSuccessorTestState(t)
			seedPrivateSuccessorTestHistory(t, root, bindings, testCase.history, testCase.published)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var complete bool
			err := WithBootstrapLock(ctx, root, func() error {
				var checkErr error
				complete, checkErr = PrivateSupervisorMigrationEdgeCompleteWithLock(
					ctx,
					root,
					bindings[testCase.predecessor],
					bindings[testCase.successor],
				)
				return checkErr
			})
			if err != nil || complete != testCase.want {
				t.Fatalf("migration edge complete=%t err=%v, want complete=%t", complete, err, testCase.want)
			}
			if testCase.wantGuardError {
				if err = WithBootstrapLock(ctx, root, func() error {
					return PublishPrivateSupervisorBindingIfAbsent(root, bindings["A"])
				}); !errors.Is(err, ErrBinding) {
					t.Fatalf("unrelated successor relaxed the predecessor binding guard: %v", err)
				}
			}
		})
	}
}

func TestPrivateSupervisorMigrationEdgePublishedAllowsPendingRecoveryCleanup(t *testing.T) {
	root, bindings := newPrivateSuccessorTestState(t)
	seedPrivateSuccessorTestHistory(t, root, bindings, [][2]string{{"A", "B"}}, "B")
	seedPrivateSuccessorTestBlocker(t, root, bindings, "recovery alias")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var published, complete bool
	err := WithBootstrapLock(ctx, root, func() error {
		var checkErr error
		published, checkErr = PrivateSupervisorMigrationEdgePublishedWithLock(ctx, root, bindings["A"], bindings["B"])
		if checkErr != nil {
			return checkErr
		}
		complete, checkErr = PrivateSupervisorMigrationEdgeCompleteWithLock(ctx, root, bindings["A"], bindings["B"])
		return checkErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if !published {
		t.Fatal("durable A-to-B edge was not recognized while recovery cleanup remained")
	}
	if complete {
		t.Fatal("A-to-B edge was reported complete before recovery cleanup")
	}
}

func TestPrivateSupervisorMigrationEdgeCompleteRejectsChangedSuccessorDigest(t *testing.T) {
	root, bindings := newPrivateSuccessorTestState(t)
	seedPrivateSuccessorTestHistory(t, root, bindings, [][2]string{{"A", "B"}}, "B")
	base := filepath.Join(root, privateSupervisorDirectory)
	history := privateMigrationHistory{
		SchemaVersion: task.SchemaVersion,
		Migrations: []privateMigrationRecord{{
			SchemaVersion: task.SchemaVersion,
			Legacy:        privateDaemonIdentityForBinding(bindings["A"]),
			Successor:     privateDaemonIdentityForBinding(bindings["B"]),
			LegacyPID:     101,
			LegacyStopped: true,
		}},
	}
	history.Migrations[0].Successor.SHA256 = task.ComputeSHA256([]byte("different successor bytes"))
	writePrivateSuccessorTestRecord(t, privateMigrationHistoryPath(base), history)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var complete bool
	err := WithBootstrapLock(ctx, root, func() error {
		var checkErr error
		complete, checkErr = PrivateSupervisorMigrationEdgeCompleteWithLock(ctx, root, bindings["A"], bindings["B"])
		return checkErr
	})
	if err != nil || complete {
		t.Fatalf("changed successor digest was accepted: complete=%t err=%v", complete, err)
	}
}

func TestResolvePrivateSupervisorSuccessorRejectsIncompleteHistory(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		legacyPID     int
		legacyStopped bool
	}{
		{name: "live predecessor", legacyPID: 101},
		{name: "missing predecessor pid", legacyPID: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root, bindings := newPrivateSuccessorTestState(t)
			seedPrivateSuccessorTestHistory(t, root, bindings, [][2]string{{"A", "B"}}, "B")
			base := filepath.Join(root, privateSupervisorDirectory)
			writePrivateSuccessorTestRecord(t, privateMigrationHistoryPath(base), privateMigrationHistory{
				SchemaVersion: task.SchemaVersion,
				Migrations: []privateMigrationRecord{{
					SchemaVersion: task.SchemaVersion,
					Legacy:        privateDaemonIdentityForBinding(bindings["A"]),
					Successor:     privateDaemonIdentityForBinding(bindings["B"]),
					LegacyPID:     testCase.legacyPID,
					LegacyStopped: testCase.legacyStopped,
				}},
			})
			assertPrivateSuccessorTestResolution(t, root, bindings["A"], task.SupervisorRef{}, false, ErrBinding)
		})
	}
}

type privateSuccessorTestResolver func(context.Context, string, task.SupervisorRef) (task.SupervisorRef, bool, error)

func assertPrivateSuccessorTestResolution(t *testing.T, root string, saved, want task.SupervisorRef, wantFound bool, wantErr error) {
	t.Helper()
	for _, resolver := range []struct {
		name    string
		resolve privateSuccessorTestResolver
	}{
		{name: "shared lease", resolve: ResolvePrivateSupervisorSuccessor},
		{name: "exclusive owner", resolve: resolvePrivateSuccessorTestWithLock},
	} {
		t.Run(resolver.name, func(t *testing.T) {
			before := snapshotPrivateSuccessorTestState(t, root)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			got, found, err := resolver.resolve(ctx, root, saved)
			if !errors.Is(err, wantErr) || found != wantFound || got != want {
				t.Errorf("wrong successor: found=%t binding=%+v err=%v; want found=%t binding=%+v err=%v", found, got, err, wantFound, want, wantErr)
			}
			if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, root)) {
				t.Fatal("successor resolution changed state-root files")
			}
		})
	}
}

func resolvePrivateSuccessorTestWithLock(ctx context.Context, root string, saved task.SupervisorRef) (task.SupervisorRef, bool, error) {
	var current task.SupervisorRef
	var found bool
	err := WithBootstrapLock(ctx, root, func() error {
		var resolveErr error
		current, found, resolveErr = ResolvePrivateSupervisorSuccessorWithLock(ctx, root, saved)
		return resolveErr
	})
	return current, found, err
}

func newPrivateSuccessorTestState(t *testing.T) (string, map[string]task.SupervisorRef) {
	t.Helper()
	_, root := newPrivateSupervisorTestRoots(t)
	base := filepath.Join(root, privateSupervisorDirectory)
	makePrivateTestDirectory(t, base)
	// The lease may open this existing lock, but resolution must publish no
	// records. No inspection group journal or runnable executable is seeded.
	writePrivateSuccessorTestBytes(t, filepath.Join(base, "bootstrap.lock"), nil)
	bindings := make(map[string]task.SupervisorRef)
	for _, name := range []string{"A", "B", "C", "X"} {
		binding := validBindingForTest()
		binding.ConfigPath = PrivateConfigPath(root)
		binding.ConfigDigest = task.ComputeSHA256(privateSuccessorTestConfig(root, name))
		binding.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("resolved " + name))
		binding.Endpoint = "unix:" + filepath.Join(base, "run", name+".sock")
		binding.ClientExecutable = filepath.Join(base, "removed-"+name, "pueue")
		binding.ClientSHA256 = task.ComputeSHA256([]byte("client " + name))
		binding.DaemonExecutable = filepath.Join(base, "removed-"+name, "pueued")
		binding.DaemonSHA256 = task.ComputeSHA256([]byte("daemon " + name))
		bindings[name] = binding
	}
	return root, bindings
}

func privateSuccessorTestConfig(root, name string) []byte {
	base := filepath.Join(root, privateSupervisorDirectory)
	return []byte(strings.ReplaceAll(privateConfigYAML(base), filepath.Join(base, "run", "pueue.sock"), filepath.Join(base, "run", name+".sock")))
}

func seedPrivateSuccessorTestHistory(t *testing.T, root string, bindings map[string]task.SupervisorRef, edges [][2]string, published string) {
	t.Helper()
	base := filepath.Join(root, privateSupervisorDirectory)
	history := privateMigrationHistory{SchemaVersion: task.SchemaVersion}
	for index, edge := range edges {
		migration := privateMigrationRecord{
			SchemaVersion: task.SchemaVersion,
			Legacy:        privateDaemonIdentityForBinding(bindings[edge[0]]),
			Successor:     privateDaemonIdentityForBinding(bindings[edge[1]]),
			LegacyPID:     101 + index,
			LegacyStopped: true,
		}
		if _, err := marshalPrivateMigrationRecord(migration); err != nil {
			t.Fatal(err)
		}
		history.Migrations = append(history.Migrations, migration)
	}
	writePrivateSuccessorTestRecord(t, privateMigrationHistoryPath(base), history)
	current := bindings[published]
	writePrivateSuccessorTestRecord(t, privateSupervisorBindingPath(root), privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: current})
	writePrivateSuccessorTestRecord(t, privateDaemonIdentityPath(base), privateDaemonIdentityForBinding(current))
	writePrivateSuccessorTestBytes(t, current.ConfigPath, privateSuccessorTestConfig(root, published))
	if complete, err := PrivateSupervisorMigrationComplete(root, current); err != nil || !complete {
		t.Fatalf("published fixture is incomplete: complete=%t err=%v", complete, err)
	}
}

func seedPrivateSuccessorTestBlocker(t *testing.T, root string, bindings map[string]task.SupervisorRef, blocker string) {
	t.Helper()
	base := filepath.Join(root, privateSupervisorDirectory)
	switch blocker {
	case "migration journal":
		writePrivateSuccessorTestRecord(t, privateMigrationRecordPath(base), privateMigrationRecord{
			SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(bindings["A"]),
			Successor: privateDaemonIdentityForBinding(bindings["B"]), LegacyPID: 101, LegacyStopped: true,
		})
	case "identity publication":
		writePrivateSuccessorTestRecord(t, privateIdentityPublicationPath(root), privateIdentityPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: bindings["B"]})
	case "recovery alias":
		config := privateSuccessorTestConfig(root, "A")
		writePrivateSuccessorTestBytes(t, privateRecoveryConfigPath(root), config)
		writePrivateSuccessorTestRecord(t, privateRecoveryRecordPath(root), privateRecoveryBindingRecord{
			SchemaVersion: task.SchemaVersion, Supervisor: bindings["A"],
			ConfigPath: privateRecoveryConfigPath(root), ConfigSHA256: task.ComputeSHA256(config),
		})
	case "bootstrap journal":
		writePrivateSuccessorTestRecord(t, privateBootstrapRecordPath(root), privateBootstrapRecord{SchemaVersion: task.SchemaVersion})
	default:
		t.Fatalf("unknown publication blocker %q", blocker)
	}
}

func writePrivateSuccessorTestRecord(t *testing.T, path string, record any) {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	writePrivateSuccessorTestBytes(t, path, data)
}

func writePrivateSuccessorTestBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type privateSuccessorTestFile struct {
	mode     fs.FileMode
	modified time.Time
	contents string
}

func snapshotPrivateSuccessorTestState(t *testing.T, root string) map[string]privateSuccessorTestFile {
	t.Helper()
	files := make(map[string]privateSuccessorTestFile)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		file := privateSuccessorTestFile{mode: info.Mode(), modified: info.ModTime()}
		if !entry.IsDir() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			file.contents = string(data)
		}
		files[path] = file
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
