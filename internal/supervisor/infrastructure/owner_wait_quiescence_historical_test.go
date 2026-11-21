package infrastructure

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Canonical recorded history models Home's prior boot/device tuple. Current
// serving uses the real same-process component fixture. This is not a native
// reboot, mount-renumbering experiment, or proof that Home's history is valid.
func TestRootDaemonWaitQuiescenceAcceptsSettledPriorBootDifferentDevice(t *testing.T) {
	f := newPriorBootQuiescenceFixture(t)
	before := producerBudgetTree(t, f.root)
	if err := f.server.waits.RequireRootDaemonWaitQuiescence(t.Context(), f.root); err != nil {
		t.Fatalf("validated prior-boot history with another device tuple blocked quiescence: %v", err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.root)) {
		t.Fatal("root-wide custody observation changed historical records")
	}
	if err := os.Remove(filepath.Join(f.root, "infrastructure", settlementName(1))); err != nil {
		t.Fatal(err)
	}
	if err := f.server.waits.RequireRootDaemonWaitQuiescence(t.Context(), f.root); err == nil {
		t.Fatal("prior boot/device tuple bypassed its required settlement evidence")
	}
}

func newPriorBootQuiescenceFixture(t *testing.T) *ownerServerFixture {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".qw-")
	if err != nil {
		t.Fatal(err)
	}
	f := &ownerServerFixture{root: root, entry: &ownedResidentEntry{}}
	t.Cleanup(func() { f.cleanup(t) })
	entry := f.entry
	entry.store, err = taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	entry.journal, err = openIntentJournal(t.Context(), entry.store, true)
	if err != nil {
		t.Fatal(err)
	}
	var acquired bool
	entry.lease, acquired, err = entry.journal.tryAcquire(t.Context())
	if err != nil || !acquired {
		t.Fatalf("historical fixture lease: acquired=%t err=%v", acquired, err)
	}
	selected := contextForJournal(t, entry.journal)
	selected.OS = runtime.GOOS
	reference, _, err := entry.journal.stageContext(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	publishPriorBootQuiescenceHistory(t, entry, reference)
	current := liveJournalIntent(t, entry.journal)
	current.Sequence = 2
	current.Epoch = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	current.Inputs.Context = reference
	refreshIntentDigest(t, &current)
	requireJournalPut(t, entry.journal, entry.lease, current)
	entry.operation, err = newRootOwnerOperation(current, selected)
	if err != nil {
		t.Fatal(err)
	}
	startup, err := ReferenceStartup(current)
	if err != nil {
		t.Fatal(err)
	}
	if err = entry.assumeAndPublish(t.Context(), startup); err != nil {
		t.Fatal(err)
	}
	f.server, err = newOwnerServer(t.Context(), entry, &ownerServerTestHost{})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func publishPriorBootQuiescenceHistory(t *testing.T, entry *ownedResidentEntry, selected LaunchContextReference) {
	t.Helper()
	j := entry.journal
	prior := liveJournalIntent(t, j)
	prior.Inputs.Context = selected
	prior.Inputs.RootDevice ^= 1
	prior.Inputs.BootID = differentQuiescenceBootID(t, j.bootID)
	refreshIntentDigest(t, &prior)
	assumption, publication := evidenceForIntent(t, prior)
	reference, err := ReferenceStartup(prior)
	if err != nil {
		t.Fatal(err)
	}
	evidence := settlementEvidence{intent: prior, assumption: &assumption, publication: &publication}
	settlement := startupSettlement{Protocol: RecordProtocolVersion, Startup: reference, ObservedBootID: j.bootID, SettledAt: time.Now().UTC().Format(time.RFC3339Nano), Reason: settlementPreviousBoot}
	if err = evidence.bind(&settlement); err != nil {
		t.Fatal(err)
	}
	if err = evidence.validate(settlement); err != nil {
		t.Fatal(err)
	}
	// The real historical record cannot be newly elected against today's
	// physical pin. Publish this bounded historical fixture directly through
	// the canonical typed transaction; production read/validation is unchanged.
	err = j.withLeaseTransaction(t.Context(), entry.lease, func(tx *taskdir.ControlTransaction) error {
		_, intentErr := tx.Put(intentName(1), prior)
		_, assumptionErr := tx.Put(assumptionName(1), assumption)
		_, publicationErr := tx.Put(publicationName(1), publication)
		_, settlementErr := tx.Put(settlementName(1), settlement)
		return errors.Join(intentErr, assumptionErr, publicationErr, settlementErr)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func differentQuiescenceBootID(t *testing.T, current string) string {
	t.Helper()
	if lifetime.CompareBootIDs(current, current) != lifetime.BootSame {
		t.Fatal("historical quiescence fixture needs a comparable current boot identity")
	}
	other := current[:len(current)-1] + "0"
	if lifetime.CompareBootIDs(current, other) == lifetime.BootSame {
		other = current[:len(current)-1] + "1"
	}
	return other
}
