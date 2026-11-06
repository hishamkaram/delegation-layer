package infrastructure

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// This fixture describes a released serving role while the actual observed
// incarnation is still this test process. It creates no daemon or native child.
func TestServingReleasedSettlementAllowsLiveWaiterHistory(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	j := f.entry.journal
	intent := f.entry.operation.Intent()
	reference := settlementReference(t, intent)
	evidence, err := loadSettlementEvidence(t.Context(), controlOwnerReader{controlIntentReader{j.control}}, j.pin, reference)
	if err != nil {
		t.Fatal(err)
	}
	record := startupSettlement{Protocol: RecordProtocolVersion, Startup: reference, ObservedBootID: j.bootID, SettledAt: time.Now().UTC().Format(time.RFC3339Nano), Reason: startupSettlementReason("serving-released")}
	if err = evidence.bind(&record); err != nil {
		t.Fatal(err)
	}
	if err = record.Validate(intent); err != nil {
		t.Fatalf("released serving role rejected live waiter evidence: %v", err)
	}
	if err = j.withLeaseTransaction(t.Context(), f.entry.lease, func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(settlementName(reference.Sequence), record)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	replay, created, err := j.settleStartup(t.Context(), f.entry.lease, reference)
	if err != nil || created || replay != record {
		t.Fatalf("live waiter changed durable serving release: created=%t error=%v", created, err)
	}
	if err = j.reobserveSettlement(t.Context(), reference, j.bootID); err != nil {
		t.Fatalf("prior released serving history treated waiter as active owner: %v", err)
	}
}

func drainedServingReleaseFixture(t *testing.T) *ownerServerFixture {
	t.Helper()
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if err := f.server.stopAcceptAndDrain(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.server.drained:
		f.server = nil
	default:
		t.Fatal("serving fixture did not drain")
	}
	return f
}

func TestServingReleaseRevokesGrantAndPermitsSuccessorElection(t *testing.T) {
	f := drainedServingReleaseFixture(t)
	original := f.entry.operation.Intent()
	selected := f.birthContext(t)
	grant := f.entry.grant
	released, err := f.entry.releaseServingContext(t.Context())
	if err != nil || !released {
		t.Fatalf("serving release: released=%t error=%v", released, err)
	}
	starts := 0
	if err = grant.GuardDaemonStart(t.Context(), f.root, func() error { starts++; return nil }); !errors.Is(err, os.ErrClosed) || starts != 0 {
		t.Fatalf("retired grant admitted a native Start: starts=%d error=%v", starts, err)
	}
	store, err := taskdir.OpenStore(f.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	refusal := errors.New("refuse before actual native Start")
	host := &producerTestHost{artifacts: original.Inputs.Artifacts}
	request := EstablishmentRequest{Context: selected, Deadline: time.Now().Add(time.Minute)}
	attempt, err := producerInvocation(t).establishOutside(t.Context(), store, request, host, producerNative{start: func(*exec.Cmd) error { starts++; return refusal }})
	ownProducerAttempt(t, attempt)
	if !errors.Is(err, refusal) || attempt == nil || starts != 1 || host.calls != 1 {
		t.Fatalf("released live waiter prevented successor election: starts=%d prepares=%d error=%v", starts, host.calls, err)
	}
	if attempt.intent.Sequence != original.Sequence+1 {
		t.Fatal("successor reused retired startup sequence")
	}
	if err = attempt.requireSettledHistory(t.Context(), attempt.intent.Sequence); err != nil {
		t.Fatalf("new history rejected live retired incarnation: %v", err)
	}
}

func TestServingReleaseProtectedDirectoryRetainsLease(t *testing.T) {
	f := drainedServingReleaseFixture(t)
	directory := filepath.Join(f.root, "infrastructure")
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Error(err)
		}
	}()
	released, err := f.entry.releaseServingContext(t.Context())
	if released || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unsafe record directory granted serving release: released=%t error=%v", released, err)
	}
	if _, statErr := os.Stat(filepath.Join(directory, settlementName(1))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed fence became release evidence: %v", statErr)
	}
	lease, acquired, leaseErr := TryAcquireLease(t.Context(), filepath.Join(f.root, instanceLeaseName))
	if lease != nil {
		defer func() {
			if closeErr := lease.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
	}
	if leaseErr != nil || acquired {
		t.Fatalf("unsafe record directory released serving lease: acquired=%t error=%v", acquired, leaseErr)
	}
}

type servingReleaseWriteFault struct {
	taskdir.BaseFaultInjector
	cause error
	calls int
}

func (f *servingReleaseWriteFault) OnStageWrite(destination string, data []byte) (int, error) {
	if filepath.Base(destination) == settlementName(1) {
		f.calls++
		return 0, f.cause
	}
	return len(data), nil
}

func TestServingReleaseWriteFailureReportsFaultWithoutClaimingRetirement(t *testing.T) {
	f := drainedServingReleaseFixture(t)
	fault := &servingReleaseWriteFault{cause: errors.New("serving-release persistence interrupted")}
	store := f.entry.store
	store.SetFaultInjector(fault)
	defer store.SetFaultInjector(nil)
	released, err := f.entry.releaseServingContext(t.Context())
	if !released || !errors.Is(err, fault.cause) || fault.calls != 1 {
		t.Fatalf("actual retirement Put failure lost: released=%t writes=%d error=%v", released, fault.calls, err)
	}
	if _, statErr := os.Stat(filepath.Join(f.root, "infrastructure", settlementName(1))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed Put fabricated release evidence: %v", statErr)
	}
	lease, acquired, leaseErr := TryAcquireLease(t.Context(), filepath.Join(f.root, instanceLeaseName))
	if lease != nil {
		defer func() {
			if closeErr := lease.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
	}
	if leaseErr != nil || !acquired {
		t.Fatalf("actual Put failure kept serving custody: acquired=%t error=%v", acquired, leaseErr)
	}
}

func TestServingReleaseWithoutOutstandingWaitClosesWithoutFence(t *testing.T) {
	f, waits := newDrainedResidentFixture(t)
	if err := finishResidentRuntime(t.Context(), f.entry, waits, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "infrastructure", settlementName(1))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("normal process exit invented a serving-release record: %v", err)
	}
}

func TestServingReleaseInvalidSettlementRetainsLeaseAfterEndpointCloses(t *testing.T) {
	f := drainedServingReleaseFixture(t)
	path := filepath.Join(f.root, "infrastructure", settlementName(1))
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	released, err := f.entry.releaseServingContext(t.Context())
	if released || err == nil {
		t.Fatalf("invalid existing evidence released serving custody: released=%t error=%v", released, err)
	}
	if f.entry.endpoint != nil || !f.entry.closed {
		t.Fatal("test did not reach the revoked, endpoint-closed record phase")
	}
	lease, acquired, leaseErr := TryAcquireLease(t.Context(), filepath.Join(f.root, instanceLeaseName))
	if lease != nil {
		defer func() {
			if closeErr := lease.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}()
	}
	if leaseErr != nil || acquired {
		t.Fatalf("record precondition fault lost actual lease: acquired=%t error=%v", acquired, leaseErr)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != "{}\n" {
		t.Fatalf("precondition failure rewrote evidence: data=%q error=%v", data, readErr)
	}
}
