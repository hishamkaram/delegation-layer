package control

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func privateRestoreUnknownStateBytes(t *testing.T, data []byte, state string) []byte {
	t.Helper()
	var document struct {
		Tasks  map[string]map[string]json.RawMessage `json:"tasks"`
		Groups json.RawMessage                       `json:"groups"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document.Tasks["0"]["status"] = json.RawMessage(state)
	result, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func openPrivateRestoreFixture(t *testing.T, f privateRestoreFixture) *privateRestoreFiles {
	t.Helper()
	files, err := openPrivateRestore(t.Context(), f.pin, f.binding, f.resolution)
	t.Cleanup(func() {
		if closeErr := files.close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return &files
}

func TestPrivateRestoreRetainsWholeAncestryAndConfig(t *testing.T) {
	for _, changed := range []string{"root", "base", "data", "config", "config bytes", "state", "state bytes", "partial"} {
		t.Run(changed, func(t *testing.T) {
			f := newPrivateRestoreFixture(t, false)
			files := openPrivateRestoreFixture(t, f)
			mutatePrivateRestoreFixture(t, f, changed)
			if err := files.check(t.Context()); err == nil {
				t.Fatal("changed held/named source was accepted")
			}
		})
	}
}

func mutatePrivateRestoreFixture(t *testing.T, f privateRestoreFixture, changed string) {
	t.Helper()
	switch changed {
	case "config bytes":
		writePrivateRestoreBytes(t, f.binding.ConfigPath, []byte("different config"))
	case "state bytes":
		writePrivateRestoreBytes(t, f.state, []byte(`{"tasks":{},"groups":{}}`))
	case "partial":
		writePrivateRestoreBytes(t, f.state+".partial", nil)
	default:
		path := map[string]string{"root": f.root, "base": f.base, "data": filepath.Dir(f.state), "config": f.binding.ConfigPath, "state": f.state}[changed]
		moved := path + "-held"
		if err := os.Rename(path, moved); err != nil {
			t.Fatal(err)
		}
		// No actors exist. Restore the rooted Store's name before its cleanup.
		t.Cleanup(func() {
			if err := os.RemoveAll(path); err != nil {
				t.Error(err)
			}
			if err := os.Rename(moved, path); err != nil {
				t.Error(err)
			}
		})
		if changed == "root" || changed == "base" || changed == "data" {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		} else {
			writePrivateRestoreBytes(t, path, f.bytes)
		}
	}
}

func TestPrivateRestoreMissingIsRecheckedIncludingPartial(t *testing.T) {
	for _, changed := range []string{"data appears", "leaf appears", "partial appears"} {
		t.Run(changed, func(t *testing.T) {
			f := newPrivateRestoreFixture(t, false)
			path := f.state
			if changed == "data appears" {
				path = filepath.Dir(path)
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			files := openPrivateRestoreFixture(t, f)
			switch changed {
			case "data appears":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "leaf appears":
				writePrivateRestoreBytes(t, path, f.bytes)
			case "partial appears":
				writePrivateRestoreBytes(t, path+".partial", nil)
			}
			if err := files.check(t.Context()); err == nil {
				t.Fatal("absence was not revalidated")
			}
		})
	}
}

func TestPrivateRestoreGenerationIsRecheckedAfterRead(t *testing.T) {
	f := newPrivateRestoreFixture(t, false)
	files := openPrivateRestoreFixture(t, f)
	if _, err := parsePrivateRestoreStream(t.Context(), files.state, false, f.binding.ObservedVersion); err != nil {
		t.Fatal(err)
	}
	changed := f.binding
	changed.ClientSHA256 = task.ComputeSHA256([]byte("new generation"))
	writePrivateRestoreBinding(t, f, changed)
	if err := checkPrivateRestoreGeneration(t.Context(), f.pin, f.binding); !errors.Is(err, ErrBinding) {
		t.Fatalf("replacement publication: %v", err)
	}
}

func TestPrivateRestoreCloseCollectsEveryOwnedDescriptor(t *testing.T) {
	f := newPrivateRestoreFixture(t, false)
	files := openPrivateRestoreFixture(t, f)
	state, config := files.state, files.config
	if err := state.Close(); err != nil {
		t.Fatal(err)
	}
	err := files.close()
	*files = privateRestoreFiles{}
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("close failure discarded: %v", err)
	}
	if _, statErr := config.Stat(); !errors.Is(statErr, os.ErrClosed) {
		t.Fatalf("later descriptor leaked after first close fault: %v", statErr)
	}
}
