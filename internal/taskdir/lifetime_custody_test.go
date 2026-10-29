package taskdir

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// custodyRegistrationStore is one production registration writer with its
// reader, digest, and the canonical file it publishes.
type custodyRegistrationStore struct {
	record   func(context.Context, LifetimeRegistrationFacts) (*task.LifetimeRegistrationRecord, error)
	read     func(context.Context) (*task.LifetimeRegistrationRecord, error)
	digest   func(context.Context) (string, error)
	path     string
	deadline time.Time
}

const custodyRegistrationBootID = "custody-boot"

func custodyRegistrationStores() map[string]func(*testing.T) custodyRegistrationStore {
	return map[string]func(*testing.T) custodyRegistrationStore{
		"task":    newTaskCustodyRegistrationStore,
		"control": newControlCustodyRegistrationStore,
	}
}

func custodyRegistrationIntent(deadline time.Time) LifetimeIntentFacts {
	return LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "custody-scope-plan",
		BootID:    custodyRegistrationBootID,
		Deadline:  deadline,
	}
}

func newTaskCustodyRegistrationStore(t *testing.T) custodyRegistrationStore {
	t.Helper()
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	deadline := time.Now().Add(time.Minute).UTC()
	recordTaskLifetimeIntent(t, td, custodyRegistrationIntent(deadline))
	return custodyRegistrationStore{
		record:   td.RecordLifetimeRegistrationContext,
		read:     td.ReadLifetimeRegistrationContext,
		digest:   td.LifetimeRegistrationDigestContext,
		path:     filepath.Join(td.Dir, lifetimeDirectory, lifetimeRegistrationName),
		deadline: deadline,
	}
}

func newControlCustodyRegistrationStore(t *testing.T) custodyRegistrationStore {
	t.Helper()
	store := testStore(t)
	operationID := strings.Repeat("c", 32)
	control, err := store.OpenInspection(operationID, true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	deadline := time.Now().Add(time.Minute).UTC()
	lifetime, err := NewControlLifetime(control, store.RootID, operationID, task.LifetimeOwnerInspection,
		task.ComputeSHA256([]byte("custody-control-owner")), deadline)
	must(t, err)
	_, err = lifetime.RecordLifetimeIntentContext(t.Context(), custodyRegistrationIntent(deadline))
	must(t, err)
	return custodyRegistrationStore{
		record:   lifetime.RecordLifetimeRegistrationContext,
		read:     lifetime.ReadLifetimeRegistrationContext,
		digest:   lifetime.LifetimeRegistrationDigestContext,
		path:     filepath.Join(store.Root, "inspections", operationID, controlLifetimeRegistrationName),
		deadline: deadline,
	}
}

func (s custodyRegistrationStore) facts(placement *task.LifetimeCustodyPlacement) LifetimeRegistrationFacts {
	return LifetimeRegistrationFacts{
		ScopeKind:        task.LifetimeScopeProcessGroup,
		Assurance:        task.LifetimeAssuranceCooperative,
		ScopeID:          "custody-scope",
		BootID:           custodyRegistrationBootID,
		Deadline:         s.deadline,
		CustodyPlacement: placement,
	}
}

func (s custodyRegistrationStore) bytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(s.path)
	must(t, err)
	return data
}

func custodyTestPlacement(fill string) *task.LifetimeCustodyPlacement {
	return &task.LifetimeCustodyPlacement{
		Version:  task.LifetimeCustodyPlacementVersion,
		PoolRoot: "/var/lib/delegate/pool",
		PoolID:   strings.Repeat(fill, 64),
	}
}

func TestLifetimeRegistrationWritersPersistPlacementSnapshot(t *testing.T) {
	for name, open := range custodyRegistrationStores() {
		t.Run(name, func(t *testing.T) {
			testLifetimeRegistrationWriterPlacementSnapshot(t, open(t))
		})
	}
}

func testLifetimeRegistrationWriterPlacementSnapshot(t *testing.T, store custodyRegistrationStore) {
	t.Helper()
	want := custodyTestPlacement("a")
	callerPlacement := want.Clone()
	record, err := store.record(t.Context(), store.facts(callerPlacement))
	must(t, err)
	callerPlacement.PoolID = strings.Repeat("e", 64)
	if record.CustodyPlacement == nil || record.CustodyPlacement == callerPlacement || *record.CustodyPlacement != *want {
		t.Fatalf("returned placement = %+v, want unaliased %+v", record.CustodyPlacement, want)
	}
	record.CustodyPlacement.PoolID = strings.Repeat("f", 64)
	stored, err := store.read(t.Context())
	must(t, err)
	if stored.CustodyPlacement == nil || *stored.CustodyPlacement != *want {
		t.Fatalf("stored placement = %+v, want %+v", stored.CustodyPlacement, want)
	}
	published := store.bytes(t)
	if !bytes.Contains(published, []byte(`"custody_placement":{"version":1,"pool_root":"/var/lib/delegate/pool","pool_id":"`+want.PoolID+`"}`)) {
		t.Fatalf("published registration lacks placement: %s", published)
	}
	requireCustodyRegistrationDigest(t, store, published)
	requireCustodyRegistrationReplays(t, store, want, published)
}

// requireCustodyRegistrationReplays accepts only an exact placement replay and
// refuses placement-only conflicts without changing the published bytes.
func requireCustodyRegistrationReplays(t *testing.T, store custodyRegistrationStore, want *task.LifetimeCustodyPlacement, published []byte) {
	t.Helper()
	replay, err := store.record(t.Context(), store.facts(want.Clone()))
	must(t, err)
	if replay.CustodyPlacement == nil || *replay.CustodyPlacement != *want {
		t.Fatalf("replayed placement = %+v, want %+v", replay.CustodyPlacement, want)
	}
	for _, conflict := range []*task.LifetimeCustodyPlacement{custodyTestPlacement("b"), nil} {
		if _, err = store.record(t.Context(), store.facts(conflict)); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("placement-only replay %+v error = %v, want identity mismatch", conflict, err)
		}
	}
	if after := store.bytes(t); !bytes.Equal(after, published) {
		t.Fatalf("refused replays changed the registration:\n got %s\nwant %s", after, published)
	}
}

func TestLifetimeRegistrationWritersKeepLegacyBytesWithoutPlacement(t *testing.T) {
	for name, open := range custodyRegistrationStores() {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			record, err := store.record(t.Context(), store.facts(nil))
			must(t, err)
			if record.CustodyPlacement != nil {
				t.Fatalf("legacy registration placement = %+v, want nil", record.CustodyPlacement)
			}
			published := store.bytes(t)
			if bytes.Contains(published, []byte("custody_placement")) {
				t.Fatalf("legacy registration gained a placement member: %s", published)
			}
			canonical, err := task.MarshalCanonical(*record)
			must(t, err)
			if !bytes.Equal(canonical, published) {
				t.Fatalf("legacy registration bytes are not canonical:\n got %s\nwant %s", published, canonical)
			}
			requireCustodyRegistrationDigest(t, store, published)
			if _, err = store.record(t.Context(), store.facts(custodyTestPlacement("a"))); !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("placement over a legacy registration error = %v, want identity mismatch", err)
			}
			if after := store.bytes(t); !bytes.Equal(after, published) {
				t.Fatalf("refused placement changed the legacy registration: %s", after)
			}
		})
	}
}

func TestLifetimeRegistrationWritersRefuseInvalidPlacementBeforePublication(t *testing.T) {
	for name, open := range custodyRegistrationStores() {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			invalid := custodyTestPlacement("a")
			invalid.Version = 2
			if _, err := store.record(t.Context(), store.facts(invalid)); !errors.Is(err, task.ErrUnsupportedSchema) {
				t.Fatalf("invalid placement error = %v, want unsupported schema", err)
			}
			if _, err := store.read(t.Context()); !task.RecordAbsent(err) {
				t.Fatalf("invalid placement published a registration: %v", err)
			}
		})
	}
}

func requireCustodyRegistrationDigest(t *testing.T, store custodyRegistrationStore, published []byte) {
	t.Helper()
	digest, err := store.digest(t.Context())
	must(t, err)
	if want := task.ComputeSHA256(published); digest != want {
		t.Fatalf("registration digest = %s, want digest of published bytes %s", digest, want)
	}
}
