package control

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type privateRestoreFixture struct {
	root, base, state string
	pin               taskdir.RootPin
	binding           task.SupervisorRef
	resolution        ResolutionContext
	bytes             []byte
}

// Typed canonical historical records need no daemon or native actuation grant.
func newPrivateRestoreFixture(t *testing.T, compressed bool) privateRestoreFixture {
	t.Helper()
	store := repairEditorTestStore(t)
	f := privateRestoreFixture{root: store.Root, base: repairEditorSupervisorDirectory(t, store)}
	var err error
	f.pin, err = store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.resolution = ResolutionContext{OS: runtime.GOOS, Home: filepath.Dir(f.root), DataLocalDirectory: filepath.Join(filepath.Dir(f.root), "data-local"), ConfigDirectory: filepath.Join(filepath.Dir(f.root), "config"), Username: "restore-fixture"}
	config, err := privateConfigYAMLForResolution(f.base, f.resolution)
	if err != nil {
		t.Fatal(err)
	}
	if compressed {
		config = strings.Replace(config, "daemon:", "daemon:\n  compress_state_file: true", 1)
	}
	f.binding = validBindingForTest()
	f.binding.ConfigPath = PrivateConfigPath(f.root)
	f.binding.ConfigDigest = task.ComputeSHA256([]byte(config))
	resolved, err := resolvePrivateConfigSource([]byte(config), f.base, f.resolution)
	if err != nil {
		t.Fatal(err)
	}
	f.binding.ResolvedConfigSHA256, err = resolved.Digest()
	if err != nil {
		t.Fatal(err)
	}
	f.binding.Endpoint = resolved.Endpoint()
	setBindingResolution(&f.binding, f.resolution)
	writePrivateRestoreBytes(t, f.binding.ConfigPath, []byte(config))
	writePrivateRestoreBinding(t, f, f.binding)
	if err = os.Mkdir(filepath.Join(f.base, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.state = filepath.Join(f.base, "data", "state.json")
	f.bytes, err = os.ReadFile("../../testdata/status-4.0.4-queued.json")
	if err != nil {
		t.Fatal(err)
	}
	if compressed {
		f.state += ".gz"
		f.bytes = privateRestoreGzip(t, f.bytes)
	}
	writePrivateRestoreBytes(t, f.state, f.bytes)
	return f
}

func writePrivateRestoreBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writePrivateRestoreBinding(t *testing.T, f privateRestoreFixture, binding task.SupervisorRef) {
	t.Helper()
	data, err := task.MarshalCanonical(privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: binding})
	if err != nil {
		t.Fatal(err)
	}
	writePrivateRestoreBytes(t, privateSupervisorBindingPath(f.root), data)
	if read, found, readErr := ReadPrivateSupervisorBinding(f.root); readErr != nil || !found || read != binding {
		t.Fatalf("fixture publication: found=%t err=%v", found, readErr)
	}
}

func privateRestoreGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func observePrivateRestoreFixture(t *testing.T, f privateRestoreFixture) PrivateRestoreObservation {
	t.Helper()
	got, err := ObservePrivateRestore(t.Context(), f.pin, f.binding, ResolutionContext{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func requirePrivateRestoreFault(t *testing.T, f privateRestoreFixture, want error) {
	t.Helper()
	got, err := ObservePrivateRestore(t.Context(), f.pin, f.binding, ResolutionContext{})
	if !errors.Is(err, want) || got.Presence() != 0 || got.Source() != (PrivateRestoreSource{}) || len(got.Snapshot().Jobs) != 0 || len(got.Snapshot().Groups) != 0 {
		t.Fatalf("fault selected restore: presence=%v err=%v, want %v", got.Presence(), err, want)
	}
}

func TestObservePrivateRestoreReadsOnlySelectedFileWithoutWrites(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "gzip"}[compressed], func(t *testing.T) {
			f := newPrivateRestoreFixture(t, compressed)
			alternate := filepath.Join(f.base, "data", "state.json.gz")
			if compressed {
				alternate = filepath.Join(f.base, "data", "state.json")
			}
			writePrivateRestoreBytes(t, alternate, []byte("invalid unselected bytes"))
			writePrivateRestoreBytes(t, alternate+".partial", nil)
			if err := os.Chmod(f.state, 0o664); err != nil {
				t.Fatal(err)
			}
			before := snapshotSavedProbeTestState(t, f.root)
			got := observePrivateRestoreFixture(t, f)
			jobs := got.Snapshot().Jobs
			if got.Presence() != PrivateRestorePresent || got.Source() == (PrivateRestoreSource{}) || len(jobs) != 1 || jobs[0].State != StateQueued || jobs[0].Label == nil || *jobs[0].Label != "fixture:queued" {
				t.Fatalf("selected native state misread: %+v", jobs)
			}
			*jobs[0].Label = "changed"
			if *got.Snapshot().Jobs[0].Label != "fixture:queued" {
				t.Fatal("snapshot accessor leaked mutable label")
			}
			groups := got.Snapshot().Groups
			delete(groups, "default")
			if got.Snapshot().Groups["default"].ParallelTasks != 1 {
				t.Fatal("snapshot accessor leaked mutable groups")
			}
			if !maps.Equal(before, snapshotSavedProbeTestState(t, f.root)) {
				t.Fatal("restore observation wrote state")
			}
		})
	}
}

func TestObservePrivateRestoreRequiresFullPublishedGeneration(t *testing.T) {
	f := newPrivateRestoreFixture(t, false)
	for _, change := range []string{"client", "daemon", "missing publication", "malformed publication"} {
		t.Run(change, func(t *testing.T) {
			selected := f.binding
			switch change {
			case "client":
				selected.ClientSHA256 = task.ComputeSHA256([]byte("future client D"))
			case "daemon":
				selected.DaemonSHA256 = task.ComputeSHA256([]byte("future daemon D"))
				selected.DaemonExecutable = "/bin/sh"
			case "missing publication":
				if err := os.Remove(privateSupervisorBindingPath(f.root)); err != nil {
					t.Fatal(err)
				}
			case "malformed publication":
				writePrivateRestoreBytes(t, privateSupervisorBindingPath(f.root), []byte("{"))
			}
			got, err := ObservePrivateRestore(t.Context(), f.pin, selected, f.resolution)
			if !errors.Is(err, ErrBinding) || got.Presence() != 0 {
				t.Fatalf("non-active generation selected: presence=%v err=%v", got.Presence(), err)
			}
			writePrivateRestoreBinding(t, f, f.binding)
		})
	}
}

func TestObservePrivateRestoreMissingIsOnlySelectedLeafOrData(t *testing.T) {
	for _, missing := range []string{"leaf", "data", "config", "base", "root metadata"} {
		t.Run(missing, func(t *testing.T) {
			f := newPrivateRestoreFixture(t, false)
			path := map[string]string{"leaf": f.state, "data": filepath.Dir(f.state), "config": f.binding.ConfigPath, "base": f.base, "root metadata": filepath.Join(f.root, "root.json")}[missing]
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			got, err := ObservePrivateRestore(t.Context(), f.pin, f.binding, f.resolution)
			if missing == "leaf" || missing == "data" {
				if err != nil || got.Presence() != PrivateRestoreMissing || got.Source() != (PrivateRestoreSource{}) {
					t.Fatalf("confirmed native absence: presence=%v err=%v", got.Presence(), err)
				}
			} else if err == nil || got.Presence() != 0 {
				t.Fatalf("missing ancestry became native absence: presence=%v err=%v", got.Presence(), err)
			}
		})
	}
}

func TestObservePrivateRestorePartialAlwaysRefuses(t *testing.T) {
	for _, missing := range []bool{false, true} {
		for _, kind := range []string{"regular", "symlink", "directory"} {
			t.Run(kind+map[bool]string{false: " present", true: " missing"}[missing], func(t *testing.T) {
				f := newPrivateRestoreFixture(t, false)
				if missing {
					if err := os.Remove(f.state); err != nil {
						t.Fatal(err)
					}
				}
				path := f.state + ".partial"
				var err error
				switch kind {
				case "regular":
					writePrivateRestoreBytes(t, path, nil)
				case "symlink":
					err = os.Symlink("unavailable", path)
				case "directory":
					err = os.Mkdir(path, 0o700)
				}
				if err != nil {
					t.Fatal(err)
				}
				requirePrivateRestoreFault(t, f, ErrPrivateRestorePartial)
			})
		}
	}
}

func TestObservePrivateRestoreRejectsUnsafeNativeLeaf(t *testing.T) {
	for _, kind := range []string{"executable", "hardlink", "symlink", "directory", "public data", "public config"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrivateRestoreFixture(t, false)
			var err error
			switch kind {
			case "executable":
				err = os.Chmod(f.state, 0o700)
			case "hardlink":
				err = os.Link(f.state, f.state+"-link")
			case "symlink", "directory":
				if err = os.Remove(f.state); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					err = os.Symlink(f.binding.ConfigPath, f.state)
				} else {
					err = os.Mkdir(f.state, 0o700)
				}
			case "public data":
				err = os.Chmod(filepath.Dir(f.state), 0o755)
			case "public config":
				err = os.Chmod(f.binding.ConfigPath, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			requirePrivateRestoreFault(t, f, ErrPrivateRestoreLayout)
		})
	}
}

func TestObservePrivateRestoreResolutionNeverUsesAmbient(t *testing.T) {
	f := newPrivateRestoreFixture(t, false)
	t.Setenv("HOME", "/unrelated")
	t.Setenv("XDG_RUNTIME_DIR", "/foreign-runtime")
	observePrivateRestoreFixture(t, f)
	conflict := f.resolution
	conflict.Home = "/conflicting"
	if got, err := ObservePrivateRestore(t.Context(), f.pin, f.binding, conflict); !errors.Is(err, ErrBinding) || got.Presence() != 0 {
		t.Fatalf("resolution conflict ignored: %v", err)
	}
	legacy := f.binding
	legacy.ResolutionOS = ""
	legacy.ResolutionHome = ""
	legacy.ResolutionDataLocal = ""
	legacy.ResolutionConfig = ""
	legacy.ResolutionUsername = ""
	writePrivateRestoreBinding(t, f, legacy)
	got, err := ObservePrivateRestore(t.Context(), f.pin, legacy, f.resolution)
	if err != nil || got.Presence() != PrivateRestorePresent {
		t.Fatalf("explicit legacy resolution: %v", err)
	}
}

func TestObservePrivateRestoreCancellationHasNoSelection(t *testing.T) {
	f := newPrivateRestoreFixture(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, ctx := range []context.Context{nil, ctx} {
		got, err := ObservePrivateRestore(ctx, f.pin, f.binding, f.resolution)
		if err == nil || got.Presence() != 0 {
			t.Fatalf("invalid context selected restore: %v", err)
		}
	}
}
