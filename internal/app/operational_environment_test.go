package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func operationalTestStore(t *testing.T) *taskdir.Store {
	t.Helper()
	parent := ""
	var store *taskdir.Store
	retain := false
	t.Cleanup(func() {
		if store != nil {
			if err := store.Close(); err != nil {
				t.Errorf("close operational fixture; retain %s: %v", parent, err)
				return
			}
		}
		if !retain && parent != "" {
			if err := os.RemoveAll(parent); err != nil {
				t.Errorf("remove operational fixture %s: %v", parent, err)
			}
		}
	})
	directory, err := os.MkdirTemp("", "dl-origin-")
	if err != nil {
		t.Fatal(err)
	}
	parent = directory
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	store, err = taskdir.InitStore(filepath.Join(canonical, "state"))
	if err != nil {
		retain = true
		t.Fatalf("initialize operational fixture; retain %s: %v", parent, err)
	}
	return store
}

func TestOperationalEnvironmentRebasesOnlyOrigin(t *testing.T) {
	a, b := operationalTestStore(t), operationalTestStore(t)
	incoming, err := infrastructure.NewOrigin(a.Root, a.RootID)
	if err != nil {
		t.Fatal(err)
	}
	// Lifetime owns this private marker. Its literal here is only a preservation
	// assertion; app production code never reads or classifies it.
	selected := []string{"CUSTOM=unchanged", "RAW=\xff", "PWD=first", "PWD=second", "DELEGATION_LAYER_PROCESS_SCOPE=finite-owner"}
	polluted, err := infrastructure.WithOrigin(selected, incoming)
	if err != nil {
		t.Fatal(err)
	}
	polluted = append(polluted, infrastructure.OriginEnvironment+"=malformed old locator")
	original := slices.Clone(polluted)
	result, err := operationalEnvironment(t.Context(), b, polluted)
	if err != nil {
		t.Fatal(err)
	}
	assertOperationalOrigin(t, result, b)
	if !slices.Equal(withoutOperationalTestOrigin(result), selected) || !slices.Equal(polluted, original) {
		t.Fatal("overlay changed non-origin bytes or caller snapshot")
	}
	result[0] = "changed owned result"
	if !slices.Equal(polluted, original) {
		t.Fatal("overlay result aliased caller snapshot")
	}
	for _, environment := range [][]string{nil, {}} {
		got, prepareErr := operationalEnvironment(t.Context(), b, environment)
		if prepareErr != nil || len(got) != 1 {
			t.Fatal("empty selected scope inherited ambient entries")
		}
		assertOperationalOrigin(t, got, b)
	}
}

func TestOperationalEnvironmentRefusesInvalidStoreContext(t *testing.T) {
	store := operationalTestStore(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := operationalEnvironment(canceled, store, []string{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
	if _, err := operationalEnvironment(t.Context(), nil, []string{}); !errors.Is(err, os.ErrInvalid) {
		t.Fatalf("nil store: %v", err)
	}
	rootID := store.RootID
	store.RootID = "malformed"
	if _, err := operationalEnvironment(t.Context(), store, []string{}); err == nil {
		t.Fatal("malformed root identity accepted")
	}
	store.RootID = rootID
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := operationalEnvironment(t.Context(), store, []string{}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed store: %v", err)
	}
}

func replaceOperationalTestRoot(t *testing.T, store *taskdir.Store) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.Root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(store.Root, store.Root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(store.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, "root.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Both directories belong to operationalTestStore's parent. Its actual root
	// handle closes before that parent's cleanup, even when the path was replaced.
}

func assertOperationalOrigin(t *testing.T, environment []string, store *taskdir.Store) {
	t.Helper()
	origin, found, err := infrastructure.ReadOrigin(environment)
	if err != nil || !found || origin.Root() != store.Root || origin.RootID() != store.RootID {
		t.Fatal("launch did not carry its actual owning root locator")
	}
}

func withoutOperationalTestOrigin(environment []string) []string {
	result := []string{}
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if key != infrastructure.OriginEnvironment {
			result = append(result, entry)
		}
	}
	return result
}
