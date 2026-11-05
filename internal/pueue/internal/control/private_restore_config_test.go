package control

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestObservePrivateRestoreChecksExactConfigIdentityAndData(t *testing.T) {
	for _, changed := range []string{"raw digest", "resolved digest", "endpoint", "nested data", "missing resolved digest"} {
		t.Run(changed, func(t *testing.T) {
			f := newPrivateRestoreFixture(t, false)
			switch changed {
			case "raw digest":
				f.binding.ConfigDigest = task.ComputeSHA256([]byte("different config"))
			case "resolved digest":
				f.binding.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("different resolution"))
			case "endpoint":
				f.binding.Endpoint = "unix:" + filepath.Join(f.base, "other.sock")
			case "missing resolved digest":
				// This legacy ref remains invalid for the selected-byte census.
				f.binding.ResolvedConfigSHA256 = ""
			case "nested data":
				f = privateRestoreNestedDataFixture(t, f)
			}
			if changed == "missing resolved digest" {
				data, err := task.MarshalCanonical(privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: f.binding})
				if err != nil {
					t.Fatal(err)
				}
				writePrivateRestoreBytes(t, privateSupervisorBindingPath(f.root), data)
			} else {
				writePrivateRestoreBinding(t, f, f.binding)
			}
			requirePrivateRestoreFault(t, f, ErrBinding)
		})
	}
}

func privateRestoreNestedDataFixture(t *testing.T, f privateRestoreFixture) privateRestoreFixture {
	t.Helper()
	data, err := os.ReadFile(f.binding.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), filepath.Join(f.base, "data"), filepath.Join(f.base, "nested", "data"), 1))
	writePrivateRestoreBytes(t, f.binding.ConfigPath, data)
	f.binding.ConfigDigest = task.ComputeSHA256(data)
	resolved, err := resolvePrivateConfigSource(data, f.base, f.resolution)
	if err != nil {
		t.Fatal(err)
	}
	f.binding.ResolvedConfigSHA256, err = resolved.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPrivateRestoreNativeModesAllowEveryReadWriteSubset(t *testing.T) {
	f := newPrivateRestoreFixture(t, false)
	file, err := os.Open(f.state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	for mode := os.FileMode(0); mode <= 0o666; mode++ {
		if mode&0o111 != 0 {
			continue
		}
		if err = file.Chmod(mode); err != nil {
			t.Fatal(err)
		}
		if _, err = privateRestoreFileSource(file, false); err != nil {
			t.Fatalf("native umask mode %#o refused: %v", mode, err)
		}
	}
}

func TestObservePrivateRestoreRejectsSymlinkedAncestry(t *testing.T) {
	for _, changed := range []string{"config", "base", "data", "root"} {
		t.Run(changed, func(t *testing.T) {
			f := newPrivateRestoreFixture(t, false)
			path := map[string]string{"config": f.binding.ConfigPath, "base": f.base, "data": filepath.Dir(f.state), "root": f.root}[changed]
			target := path + "-real"
			if err := os.Rename(path, target); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.Remove(path); err != nil {
					t.Error(err)
				}
				if err := os.Rename(target, path); err != nil {
					t.Error(err)
				}
			})
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			got, err := ObservePrivateRestore(t.Context(), f.pin, f.binding, f.resolution)
			if err == nil || got.Presence() != 0 {
				t.Fatal("symlinked ancestry became selected restore")
			}
		})
	}
}
