package infrastructure

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestOwnerStopClientObservesReleasedServingWhileProcessRemainsWaiter(t *testing.T) {
	f := drainedServingReleaseFixture(t)
	reader := contextPassiveReader(t, f.entry.journal)
	if released, err := f.entry.releaseServingContext(t.Context()); err != nil || !released {
		t.Fatalf("owned serving release: %t %v", released, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := StopOwnerServing(ctx, reader)
	if err != nil || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessPending || result.Startup.Sequence != 1 {
		t.Fatalf("retired live waiter was mislabeled: %+v %v", result, err)
	}
	if _, err = os.Stat(ownerEndpointPath(f.root, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("positive release retained endpoint: %v", err)
	}
}

func TestOwnerStopClientRequiresFreeExistingLeaseAndAbsentEndpoint(t *testing.T) {
	for _, changed := range []string{"held", "missing", "endpoint"} {
		t.Run(changed, func(t *testing.T) {
			f := drainedServingReleaseFixture(t)
			reader := contextPassiveReader(t, f.entry.journal)
			if released, err := f.entry.releaseServingContext(t.Context()); err != nil || !released {
				t.Fatalf("owned serving release: %t %v", released, err)
			}
			leasePath := changeOwnerStopReleaseEvidence(t, f.root, changed)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			result, err := StopOwnerServing(ctx, reader)
			if err == nil || result.State == OwnerServingReleased || result.ProcessClosure == OwnerProcessClosed {
				t.Fatalf("incomplete native release became closure: %+v %v", result, err)
			}
			if changed == "missing" {
				if _, err = os.Stat(leasePath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stop recreated missing election lease: %v", err)
				}
			}
		})
	}
}

func changeOwnerStopReleaseEvidence(t *testing.T, root, changed string) string {
	t.Helper()
	leasePath := filepath.Join(root, instanceLeaseName)
	switch changed {
	case "held":
		acquire(t, leasePath)
	case "missing":
		if err := os.Remove(leasePath); err != nil {
			t.Fatal(err)
		}
	case "endpoint":
		if err := os.WriteFile(ownerEndpointPath(root, 1), []byte("replacement data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return leasePath
}

func TestOwnerStopClientFreeLeaseAndSocketAbsenceDoNotCloseLiveOwner(t *testing.T) {
	f := newOwnerContactFixture(t)
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := StopOwnerServing(ctx, f.reader)
	if err == nil || result.State != OwnerStopUnknown || result.ProcessClosure != OwnerProcessPending {
		t.Fatalf("free flock and absent socket fabricated serving release: %+v %v", result, err)
	}
}

func writeOwnerStopServingFence(ctx context.Context, f ownerContactFixture) error {
	reader := controlOwnerReader{controlIntentReader{f.journal.control}}
	evidence, err := loadSettlementEvidence(ctx, reader, f.journal.pin, f.auth.request.Owner)
	if err != nil {
		return err
	}
	record := startupSettlement{Protocol: RecordProtocolVersion, Startup: f.auth.request.Owner, ObservedBootID: f.journal.bootID, SettledAt: time.Now().UTC().Format(time.RFC3339Nano), Reason: settlementServingReleased}
	if err = evidence.bind(&record); err != nil {
		return err
	}
	return f.journal.withLeaseTransaction(ctx, f.lease, func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(settlementName(record.Startup.Sequence), record)
		return putErr
	})
}

func TestOwnerStopClientUnknownWireDoesNotResendAndRetainsPositiveRelease(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	join := serveOwnerStopFixture(t, f, func(connection *net.UnixConn) error {
		if _, err := readOwnerFrame[ownerStopRequest](bufio.NewReader(connection)); err != nil {
			return err
		}
		if err := writeOwnerStopServingFence(ctx, f); err != nil {
			return err
		}
		return errors.Join(f.auth.listener.Close(), f.lease.Close()) // Natural EOF, no acknowledgement.
	})
	result, err := StopOwnerServing(ctx, f.reader)
	if !errors.Is(err, io.EOF) || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessPending {
		t.Fatalf("unknown exchange lost original fault or same-owner release: %+v %v", result, err)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(ownerEndpointPath(f.auth.root, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive closure observation recreated endpoint: %v", err)
	}
}

func TestOwnerStopClientAuthenticatesAndObservesActualServerRelease(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missingRunner := filepath.Join(parent, "missing-installed-runner")
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{}, ownerServerFixtureSelection{artifacts: Artifacts{Runner: missingRunner}})
	reader := contextPassiveReader(t, f.entry.journal)
	if _, err = f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	f.start()
	released := make(chan error, 1)
	go func() {
		select {
		case <-f.server.drained:
			_, releaseErr := f.entry.releaseServingContext(ctx)
			released <- releaseErr
		case <-ctx.Done():
			released <- ctx.Err()
		}
	}()
	t.Cleanup(func() {
		cancel()
		if releaseErr := <-released; releaseErr != nil {
			t.Error(releaseErr)
			return
		}
		if serverErr := f.server.fault(); serverErr != nil {
			t.Error(serverErr)
			return
		}
		// The canonical server has drained and the entry has released its
		// listener. Do not re-run stopAccepting on that now-closed listener.
		f.server = nil
	})
	result, err := StopOwnerServing(ctx, reader)
	if err != nil || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessPending {
		t.Fatalf("actual authenticated stop/release boundary: %+v %v", result, err)
	}
}

func TestOwnerStopClientProvesExactProcessAbsenceUnderFreeLease(t *testing.T) {
	f := newOwnerContactFixture(t)
	selectAbsentOwnerStopIncarnation(t, f)
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := StopOwnerServing(ctx, f.reader)
	if err != nil || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessClosed {
		t.Fatalf("exact absent incarnation was not observed: %+v %v", result, err)
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func selectAbsentOwnerStopIncarnation(t *testing.T, f ownerContactFixture) {
	t.Helper()
	// Keep the real current PID but select a different valid native incarnation.
	// The kernel observer must report that exact historical reference absent;
	// this test process and its matching socket remain alive and untouched.
	parts := strings.Split(f.auth.assumption.Process.StartToken, ":")
	last := len(parts) - 1
	value, err := strconv.ParseUint(parts[last], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if value == ^uint64(0) {
		value--
	} else {
		value++
	}
	parts[last] = strconv.FormatUint(value, 10)
	f.auth.assumption.Process.StartToken = strings.Join(parts, ":")
	reference, err := f.auth.assumption.Process.Reference()
	if err != nil {
		t.Fatal(err)
	}
	if present, observeErr := lifetime.ProcessPresent(reference); observeErr != nil || present {
		t.Fatalf("historical incarnation is not absent: %t %v", present, observeErr)
	}
	f.auth.rebind(t)
	assumption, err := task.MarshalCanonical(f.auth.assumption)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := task.MarshalCanonical(f.auth.publication)
	if err != nil {
		t.Fatal(err)
	}
	// Owned historical record DATA, not executable identity/contents. The
	// ordinary fixture's current live-assumption writer cannot author history.
	for name, data := range map[string][]byte{assumptionName(1): assumption, publicationName(1): publication} {
		if err = os.WriteFile(filepath.Join(f.auth.root, "infrastructure", name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOwnerStopClosurePollingObservesAbsentOriginalAfterSuccessorElection(t *testing.T) {
	f := newOwnerContactFixture(t)
	selectAbsentOwnerStopIncarnation(t, f)
	selected, found, err := readOwnerStopLocator(t.Context(), f.reader)
	if err != nil || !found || selected.servingReleased {
		t.Fatalf("select absent owner without a serving fence: found=%t released=%t error=%v", found, selected.servingReleased, err)
	}
	if err = errors.Join(f.auth.listener.Close(), f.lease.Close()); err != nil {
		t.Fatal(err)
	}
	publishOwnerStopSuccessorFixture(t, f)
	// A future birth's socket is a contact trap, not an assumption/publication.
	// The selected sequence's positive absence must be sufficient on its own.
	future, err := net.ListenUnix("unix", &net.UnixAddr{Name: ownerEndpointPath(f.reader.Root(), 2), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := future.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := awaitOwnerStopClosure(ctx, f.reader, selected, OwnerStopResult{State: OwnerStopUnknown, ProcessClosure: OwnerProcessUnknown, Startup: selected.reference})
	if err != nil || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessClosed || result.Startup != selected.reference {
		t.Fatalf("successor lease hid positive original-process absence: %+v %v", result, err)
	}
	if result.Process != selected.assumption.Process {
		t.Fatalf("successor replaced selected process projection: %+v", result.Process)
	}
	assertAvailable(t, f.lease.path, false)
	if _, err = os.Stat(filepath.Join(f.reader.Root(), "infrastructure", settlementName(1))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closure observation created a serving fence: %v", err)
	}
	requireNoOwnerContactConnection(t, future)
}

func publishOwnerStopSuccessorFixture(t *testing.T, f ownerContactFixture) {
	t.Helper()
	successor, err := publishOwnerStopSuccessor(t.Context(), f)
	if successor != nil {
		t.Cleanup(func() {
			if closeErr := successor.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
}

// The successor owns an actual native instance lease and a canonical next
// intent. Its birth is deliberately still unassumed: stop must neither read
// successor completeness as original-owner evidence nor contact that birth.
func publishOwnerStopSuccessor(ctx context.Context, f ownerContactFixture) (*Lease, error) {
	lease, acquired, err := f.journal.tryAcquire(ctx)
	if err != nil || !acquired {
		return lease, errors.Join(ErrIdentity, err)
	}
	intent := f.auth.intent
	intent.Sequence++
	intent.Epoch = task.ComputeSHA256([]byte(intent.Epoch + "successor"))[:32]
	created, err := f.journal.putIntent(ctx, lease, intent)
	if err != nil || !created {
		return lease, errors.Join(ErrIdentity, err)
	}
	return lease, nil
}

func releaseOwnerStopFixture(ctx context.Context, f ownerContactFixture) error {
	if err := writeOwnerStopServingFence(ctx, f); err != nil {
		return err
	}
	return errors.Join(f.auth.listener.Close(), f.lease.Close())
}

func TestOwnerStopClientAcceptedReleaseSurvivesSuccessorElection(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var successor *Lease
	// serveOwnerStopFixture registers its join afterward, so every path joins
	// the exchange before touching the successor lease written by that turn.
	t.Cleanup(func() {
		if successor != nil {
			if closeErr := successor.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
	})
	join := serveOwnerStopFixture(t, f, func(connection *net.UnixConn) error {
		request, err := readOwnerFrame[ownerStopRequest](bufio.NewReader(connection))
		if err != nil {
			return err
		}
		if err = releaseOwnerStopFixture(ctx, f); err != nil {
			return err
		}
		successor, err = publishOwnerStopSuccessor(ctx, f)
		if err != nil {
			return err
		}
		return writeOwnerFrame(connection, ownerStopFixtureReply(request, ownerStopAccepted))
	})
	result, err := StopOwnerServing(ctx, f.reader)
	if joinErr := join(); joinErr != nil {
		t.Fatal(joinErr)
	}
	if err != nil || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessPending || result.Startup != f.auth.request.Owner {
		t.Fatalf("accepted original release was replaced by successor state: %+v %v", result, err)
	}
	assertAvailable(t, f.lease.path, false)
}

func TestOwnerStopClosurePollingSurvivesSuccessorElection(t *testing.T) {
	f, selected := newOwnerStopReleasedSuccessorFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := awaitOwnerStopClosure(ctx, f.reader, selected, OwnerStopResult{State: OwnerStopUnknown, ProcessClosure: OwnerProcessPending, Startup: selected.reference})
	if err != nil || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessPending || result.Startup != selected.reference {
		t.Fatalf("poll lost the selected owner's release after successor election: %+v %v", result, err)
	}
	assertAvailable(t, f.lease.path, false)
}

func newOwnerStopReleasedSuccessorFixture(t *testing.T) (ownerContactFixture, ownerStopLocator) {
	t.Helper()
	f := newOwnerContactFixture(t)
	selected, found, err := readOwnerStopLocator(t.Context(), f.reader)
	if err != nil || !found {
		t.Fatalf("select original owner: found=%t error=%v", found, err)
	}
	if err = releaseOwnerStopFixture(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	publishOwnerStopSuccessorFixture(t, f)
	return f, selected
}

func TestOwnerStopSuccessorDoesNotReplaceOriginalEvidence(t *testing.T) {
	for _, record := range []string{intentName(1), assumptionName(1), publicationName(1), settlementName(1)} {
		t.Run(record, func(t *testing.T) {
			f, selected := newOwnerStopReleasedSuccessorFixture(t)
			if err := os.WriteFile(filepath.Join(f.reader.Root(), "infrastructure", record), []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := observeOwnerStopClosure(t.Context(), f.reader, selected)
			if err == nil || result.State == OwnerServingReleased || result.ProcessClosure == OwnerProcessClosed {
				t.Fatalf("successor replaced unreadable original evidence: %+v %v", result, err)
			}
		})
	}
}

func TestOwnerStopSuccessorDoesNotReplaceOriginalEndpointEvidence(t *testing.T) {
	f, selected := newOwnerStopReleasedSuccessorFixture(t)
	if err := os.WriteFile(selected.publication.Endpoint.Path, []byte("replacement data"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := observeOwnerStopClosure(t.Context(), f.reader, selected)
	if err == nil || result.State == OwnerServingReleased || result.ProcessClosure == OwnerProcessClosed {
		t.Fatalf("successor hid replaced original endpoint: %+v %v", result, err)
	}
}

func TestOwnerStopSuccessorCannotReplaceMissingOriginalFence(t *testing.T) {
	f, selected := newOwnerStopReleasedSuccessorFixture(t)
	if err := os.Remove(filepath.Join(f.reader.Root(), "infrastructure", settlementName(1))); err != nil {
		t.Fatal(err)
	}
	result, err := observeOwnerStopClosure(t.Context(), f.reader, selected)
	if err != nil || result.State != OwnerStopUnknown || result.ProcessClosure != OwnerProcessPending {
		t.Fatalf("successor became positive serving-release evidence: %+v %v", result, err)
	}
}

func TestOwnerStopSuccessorRequiresBoundAcceptedIntent(t *testing.T) {
	for _, changed := range []string{"empty", "partial", "root", "boot"} {
		t.Run(changed, func(t *testing.T) {
			f, selected := newOwnerStopReleasedSuccessorFixture(t)
			path := filepath.Join(f.reader.Root(), "infrastructure", intentName(2))
			data := changedOwnerStopSuccessor(t, f, changed)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := observeOwnerStopClosure(t.Context(), f.reader, selected)
			if err == nil || result.State == OwnerServingReleased || result.ProcessClosure == OwnerProcessClosed {
				t.Fatalf("invalid successor intent waived native lease observation: %+v %v", result, err)
			}
		})
	}
}

func changedOwnerStopSuccessor(t *testing.T, f ownerContactFixture, changed string) []byte {
	t.Helper()
	switch changed {
	case "empty":
		return nil
	case "partial":
		return []byte("{}\n")
	}
	intent, err := f.journal.readIntent(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	switch changed {
	case "root":
		intent.Inputs.RootID = task.ComputeSHA256([]byte(intent.Inputs.RootID + "foreign"))[:32]
	case "boot":
		intent.Inputs.BootID = otherStopBoot(intent.Inputs.BootID)
	}
	data, err := task.MarshalCanonical(intent)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
