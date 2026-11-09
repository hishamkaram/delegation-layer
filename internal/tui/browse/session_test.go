package browse

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/history"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestSessionRetainsCatalogUntilExplicitCloseRetrySucceeds(t *testing.T) {
	for _, operation := range []string{"close", "rebuild"} {
		t.Run(operation, func(t *testing.T) { checkSessionCloseRetry(t, operation) })
	}
}

func checkSessionCloseRetry(t *testing.T, operation string) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	must(t, err)
	t.Cleanup(func() { must(t, store.Close()) })
	home, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	options := history.Options{Root: store.Root, Environment: []string{"HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(home, "cache")}, Registry: predicate.Default()}
	s := New(options)
	must(t, s.open(t.Context()))
	original, err := s.Catalog()
	must(t, err)
	t.Cleanup(func() {
		s.closeCatalog = (*history.Catalog).Close
		must(t, s.Close())
		// The test retains the exact original handle even on the expected RED
		// implementation, whose defect loses it from the session owner.
		must(t, original.Close())
	})
	must(t, s.scan(t.Context(), original))
	sourcePin := *s.sourcePin
	before, err := os.Stat(original.CachePath())
	must(t, err)
	closeFailure := errors.New("controlled catalog close failure")
	blocked := true
	closeCalls := 0
	s.closeCatalog = func(c *history.Catalog) error {
		closeCalls++
		if c != original {
			t.Fatal("close retried against a replacement catalog")
		}
		if blocked {
			return closeFailure
		}
		return c.Close()
	}
	mutate := func() error { return s.Close() }
	if operation == "rebuild" {
		mutate = func() error { return s.Rebuild(t.Context()) }
	}
	if err = mutate(); !errors.Is(err, closeFailure) {
		t.Fatalf("close failure lost: %v", err)
	}
	if s.catalog != original {
		t.Fatal("failed catalog close lost the session's cleanup handle")
	}
	checkPendingCatalogClose(t, s, original, closeFailure)
	if closeCalls != 1 {
		t.Fatal("passive observation retried catalog closure")
	}
	after, err := os.Stat(original.CachePath())
	must(t, err)
	if !os.SameFile(before, after) || !s.sourcePin.Equal(sourcePin) {
		t.Fatal("blocked cleanup replaced cache files or the source binding")
	}
	options.Rebuild = true
	if other, openErr := history.Open(t.Context(), options); !errors.Is(openErr, history.ErrCacheBusy) {
		if other != nil {
			must(t, other.Close())
		}
		t.Fatalf("retained catalog no longer protected cache-use lease: %v", openErr)
	}
	blocked = false
	must(t, mutate())
	if closeCalls != 2 {
		t.Fatal("explicit retry did not settle the retained handle once")
	}
	checkExplicitCloseResult(t, s, original, operation == "rebuild")
	// Restored before the cleanup's final close of a replacement, if any.
	s.closeCatalog = (*history.Catalog).Close
}

func checkExplicitCloseResult(t *testing.T, s *Session, original *history.Catalog, rebuild bool) {
	t.Helper()
	current, err := s.Catalog()
	must(t, err)
	if rebuild {
		if current == nil || current == original || !current.Rebuilt() {
			t.Fatal("settled rebuild did not expose its replacement catalog")
		}
	} else if current != nil {
		t.Fatal("successful close retained an active catalog")
	}
}

func checkPendingCatalogClose(t *testing.T, s *Session, original *history.Catalog, failure error) {
	t.Helper()
	current, err := s.Catalog()
	if current != nil || !errors.Is(err, failure) || !strings.Contains(s.Description(), failure.Error()) {
		t.Fatalf("failed close was not hidden with a visible error: %p %v %q", current, err, s.Description())
	}
	if err = s.open(t.Context()); !errors.Is(err, failure) {
		t.Fatalf("automatic reopen bypassed unresolved cleanup: %v", err)
	}
	if err = s.scan(context.Background(), original); !errors.Is(err, failure) {
		t.Fatalf("scan used a catalog whose close remains unresolved: %v", err)
	}
}

func TestInitialBindingPermitsOnlyAbsentRootRecords(t *testing.T) {
	for _, kind := range []string{"absent", "empty-directory", "invalid-metadata", "file"} {
		t.Run(kind, func(t *testing.T) { checkInitialBinding(t, kind) })
	}
}

func checkInitialBinding(t *testing.T, kind string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	switch kind {
	case "empty-directory":
		must(t, os.Mkdir(root, 0o700))
	case "invalid-metadata":
		store, err := taskdir.InitStore(root)
		must(t, err)
		must(t, store.Close())
		must(t, os.WriteFile(filepath.Join(root, "root.json"), []byte("{}"), 0o600))
	case "file":
		must(t, os.WriteFile(root, []byte("not a root"), 0o600))
	}
	s := New(history.Options{Root: root, Registry: predicate.Default()})
	err := s.AssertBinding(t.Context())
	if kind == "absent" || kind == "empty-directory" {
		must(t, err)
		if kind == "absent" {
			if _, statErr := os.Lstat(root); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("initial binding initialized an absent root")
			}
		} else {
			entries, readErr := os.ReadDir(root)
			must(t, readErr)
			if len(entries) != 0 {
				t.Fatal("initial binding wrote into an uninitialized directory")
			}
		}
		// The task-store owner, not browse, decides initialization. Both absent
		// paths and existing empty directories are supported InitStore inputs.
		store, initErr := taskdir.InitStore(root)
		must(t, initErr)
		must(t, store.Close())
		return
	}
	if err == nil || errors.Is(err, history.ErrRootChanged) {
		t.Fatalf("initial invalid root did not expose its concrete failure: %v", err)
	}
	if openErr := s.open(t.Context()); openErr == nil {
		t.Fatal("initial invalid root was accepted by session opening")
	}
	if _, visibleErr := s.Catalog(); visibleErr == nil || !strings.Contains(s.Description(), visibleErr.Error()) {
		t.Fatal("initial source failure was not visible to the browse consumer")
	}
}

func TestBindingCancellationDoesNotInvalidateObservedIdentity(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	must(t, err)
	t.Cleanup(func() { must(t, store.Close()) })
	s := New(history.Options{Root: store.Root, Registry: predicate.Default()})
	must(t, s.AssertBinding(t.Context()))
	pin := *s.sourcePin
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = s.AssertBinding(ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, history.ErrRootChanged) || !s.sourcePin.Equal(pin) {
		t.Fatalf("cancelled read invalidated the stable source context: %v", err)
	}
	must(t, s.AssertBinding(t.Context()))
}

func TestSessionScanIncludesPassiveObservations(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	must(t, err)
	t.Cleanup(func() { must(t, store.Close()) })
	home, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	id := strings.Repeat("a", 32)
	destination := remote.Identity{Host: "fixture", Port: 22, User: "fixture", HostKey: "pinned", Root: home, RootID: strings.Repeat("b", 32)}
	brief := []byte("brief")
	intent := remote.Intent{Version: remote.ProtocolVersion, OriginRootID: store.RootID, TaskID: id, Device: "fixture-device", Executable: "delegate", Destination: destination, Provider: "fixture:test", Config: task.TaskConfig{Permission: "read-only", Budget: "1m"}, RemoteCwd: home, Brief: remote.Descriptor{Name: "brief.md", Length: int64(len(brief)), SHA256: task.ComputeSHA256(brief)}}
	reference, err := remote.OpenReference(t.Context(), store, id, true)
	must(t, err)
	t.Cleanup(func() { must(t, reference.Close()) })
	must(t, reference.SaveIntent(t.Context(), intent, brief))
	envelope, err := json.Marshal(map[string]string{"task_id": id, "root_id": destination.RootID, "admission": "admitted", "liveness": "undetermined", "publication": "committed"})
	must(t, err)
	must(t, reference.Observe(t.Context(), remote.Response{Version: remote.ProtocolVersion, Identity: destination, Envelope: envelope}))
	s := New(history.Options{Root: store.Root, Environment: []string{"HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(home, "cache")}, Registry: predicate.Default()})
	t.Cleanup(func() { must(t, s.Close()) })
	must(t, s.open(t.Context()))
	c, err := s.Catalog()
	must(t, err)
	must(t, s.scan(t.Context(), c))
	page, err := c.Query(t.Context(), history.Query{Publication: "committed"})
	must(t, err)
	if len(page.Rows) != 1 || page.Progress.Observed != 1 || page.Rows[0].Observation.Source != "origin-cache" {
		t.Fatalf("session wired a metadata-only scan: %+v", page)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
