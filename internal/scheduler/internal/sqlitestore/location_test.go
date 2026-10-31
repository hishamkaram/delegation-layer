package sqlitestore

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"

	"modernc.org/sqlite"
)

func TestDatabaseURIHasOnlyConnectionLocalSettings(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		uri := databaseURI("/state root/#odd?name%/.scheduler/schedules.sqlite", goos)
		parsed, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Scheme != "file" || parsed.Path != "/state root/#odd?name%/.scheduler/schedules.sqlite" || parsed.Host != "" {
			t.Fatalf("%s: uri %q does not round-trip its path", goos, uri)
		}
		query := parsed.Query()
		keys := slices.Sorted(func(yield func(string) bool) {
			for key := range query {
				if !yield(key) {
					return
				}
			}
		})
		if !reflect.DeepEqual(keys, []string{"_pragma", "_txlock", "mode"}) {
			t.Fatalf("%s: keys %v", goos, keys)
		}
		want := []string{"busy_timeout(5000)", "foreign_keys(1)", "synchronous(3)"}
		if goos == "darwin" {
			want = append(want, "fullfsync(1)")
		}
		if query.Get("mode") != "rw" || query.Get("_txlock") != "immediate" || !reflect.DeepEqual(query["_pragma"], want) {
			t.Fatalf("%s: settings %v", goos, query)
		}
	}
}

func TestSpecialCharacterPathsOpenOnlyTheirExactFile(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "state root #1 ?x=y %41 &mode=rwc")
	if err := os.MkdirAll(Directory(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(Directory(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatalf("initialize special path: %v", err)
	}
	store := openStore(t, root)
	createSchedule(t, store, testScheduleID)
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(root) {
		t.Fatalf("unexpected files beside the special root: %v %v", entries, err)
	}
	if _, err := os.Stat(DatabasePath(root)); err != nil {
		t.Fatalf("database not at its exact path: %v", err)
	}
}

func TestConnectionWithoutRequiredSettingsIsRefused(t *testing.T) {
	_, root := initializedStore(t)
	path := DatabasePath(root)
	plain := storeConnector(t, path)
	base, err := sqlite.NewConnector(rawURI(path))
	if err != nil {
		t.Fatal(err)
	}
	plain.base = base
	if _, err := openWithConnector(testContext(t), plain, path, testRootID, runtime.GOOS); !errors.Is(err, errConnectionConfig) {
		t.Fatalf("connection without settings err=%v, want connection config fault", err)
	}
}
