package infrastructure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const originTestRootID = "0123456789abcdef0123456789abcdef"

func testOrigin(t *testing.T) Origin {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	origin, err := NewOrigin(root, originTestRootID)
	if err != nil {
		t.Fatal(err)
	}
	return origin
}

func TestOriginRebaseOwnsSnapshotAndDropsPreviousEpoch(t *testing.T) {
	origin := testOrigin(t)
	previous := OriginEnvironment + `={"root":"/old","root_id":"` + originTestRootID + `","epoch":"stale"}`
	environment := []string{"HOME=selected", previous, "LANG=selected", previous}
	before := slices.Clone(environment)
	rebased, err := WithOrigin(environment, origin)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(environment, before) || len(rebased) != 3 || rebased[0] != before[0] || rebased[1] != before[2] {
		t.Fatal("rebasing changed selected values or retained duplicate origin entries")
	}
	if strings.Contains(rebased[2], "epoch") {
		t.Fatal("origin retained a launch-time epoch")
	}
	received, found, err := ReadOrigin(rebased)
	if err != nil || !found || received != origin || received.Root() != origin.Root() || received.RootID() != origin.RootID() {
		t.Fatalf("origin round trip: found=%v err=%v", found, err)
	}
	rebased[0] = "HOME=changed"
	if environment[0] != "HOME=selected" {
		t.Fatal("launch snapshot aliases its caller")
	}
	if _, err := WithOrigin(nil, Origin{}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("zero origin accepted: %v", err)
	}
}

func TestReadOriginRefusesMalformedRouting(t *testing.T) {
	valid := `{"root":"/selected","root_id":"` + originTestRootID + `"}`
	for _, raw := range []string{
		"", "null", valid + " {}", strings.Replace(valid, "}", `,"epoch":"stale"}`, 1),
		strings.Replace(valid, `"root":`, `"root":"/other","root":`, 1),
		strings.Replace(valid, `"/selected"`, `"relative"`, 1),
		strings.Replace(valid, originTestRootID, "invalid", 1),
		strings.Replace(valid, "/selected", "/selected/../other", 1),
		strings.Replace(valid, "/selected", "/selected\\u0000", 1),
		strings.Replace(valid, "/selected", "/"+string([]byte{0xff}), 1),
	} {
		if _, found, err := ReadOrigin([]string{OriginEnvironment + "=" + raw}); !found || !errors.Is(err, ErrIdentity) {
			t.Fatalf("malformed origin not refused: found=%v err=%v", found, err)
		}
	}
	for _, entries := range [][]string{
		{OriginEnvironment},
		{OriginEnvironment + "=" + valid, OriginEnvironment + "=" + valid},
	} {
		if _, found, err := ReadOrigin(entries); !found || !errors.Is(err, ErrIdentity) {
			t.Fatalf("ambiguous origin accepted: found=%v err=%v", found, err)
		}
	}
	if _, found, err := ReadOrigin([]string{"HOME=selected"}); found || err != nil {
		t.Fatalf("absent origin synthesized: found=%v err=%v", found, err)
	}
}

func TestOriginValidateRootIsObservationalAndIdentityBound(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "root")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	origin, err := NewOrigin(store.Root, store.RootID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = origin.ValidateRoot(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(root)
	if err != nil || len(before) != len(after) {
		t.Fatalf("origin observation created state: %v", err)
	}
	wrong, err := NewOrigin(root, originTestRootID)
	if err != nil {
		t.Fatal(err)
	}
	if err = wrong.ValidateRoot(t.Context()); !errors.Is(err, ErrIdentity) {
		t.Fatalf("replaced root identity accepted: %v", err)
	}
	alias := filepath.Join(parent, "alias")
	if err = os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	aliased, err := NewOrigin(alias, store.RootID)
	if err != nil {
		t.Fatal(err)
	}
	if err := aliased.ValidateRoot(t.Context()); !errors.Is(err, ErrIdentity) {
		t.Fatalf("noncanonical origin accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := origin.ValidateRoot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestOriginValidateRootNeverInitializesMissingStore(t *testing.T) {
	origin := testOrigin(t)
	if err := origin.ValidateRoot(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing metadata: %v", err)
	}
	entries, err := os.ReadDir(origin.Root())
	if err != nil || len(entries) != 0 {
		t.Fatalf("observation initialized missing store: %v", err)
	}
}

func TestOriginEncodingRetainsControlRecordBound(t *testing.T) {
	origin, err := NewOrigin(string(filepath.Separator)+strings.Repeat("x", task.MaxControlRecordSize), originTestRootID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = WithOrigin(nil, origin); !errors.Is(err, ErrIdentity) || !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("origin encoder emitted an unreadable record: %v", err)
	}
}
