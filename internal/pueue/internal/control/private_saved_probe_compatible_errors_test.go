package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
)

func TestProbeSavedPrivateCompatibleUnavailableIsOnlyPureSelectorFact(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	damageCompatibleProbePair(t, f, "missing client")
	closeFault := errors.New("resolver cleanup failed")
	for _, row := range []struct {
		name        string
		cause       error
		unavailable bool
	}{
		{"sentinel", ErrCompatiblePairUnavailable, true},
		{"wrapped", fmt.Errorf("no bundle: %w", ErrCompatiblePairUnavailable), true},
		{"joined pure", errors.Join(ErrCompatiblePairUnavailable, ErrCompatiblePairUnavailable), true},
		{"permission", errors.Join(ErrCompatiblePairUnavailable, os.ErrPermission), false},
		{"context", errors.Join(ErrCompatiblePairUnavailable, context.Canceled), false},
		{"close", errors.Join(ErrCompatiblePairUnavailable, closeFault), false},
		{"raw missing", os.ErrNotExist, false},
		{"configuration", ErrConfiguration, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			before := snapshotSavedProbeTestState(t, f.stateRoot)
			called := 0
			client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) { called++; return "", "", row.cause })
			closeSavedProbeTestClient(t, client, err)
			if client != nil || called != 1 {
				t.Fatalf("selector result: client=%t calls=%d", client != nil, called)
			}
			if row.unavailable {
				if err != nil {
					t.Fatalf("positive no-candidate fact blocked owner route: %v", err)
				}
			} else if !errors.Is(err, ErrBinding) || !errors.Is(err, row.cause) {
				t.Fatalf("independent selector cause was lost or softened: %v", err)
			}
			assertSavedProbeTestUnchanged(t, f.stateRoot, before)
		})
	}
}

func TestProbeSavedPrivateCompatibleRestoresBindingTagForCompletedFault(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	damageCompatibleProbePair(t, f, "missing client")
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) { return "", "", ErrUnknown })
	closeSavedProbeTestClient(t, client, err)
	if client != nil || !errors.Is(err, ErrBinding) || !errors.Is(err, ErrUnknown) {
		t.Fatalf("completed probe fault lost old binding classification: %v", err)
	}
}

func TestProbeSavedPrivateCompatibleRetainsPendingExceptionAndJoinedFault(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	damageCompatibleProbePair(t, f, "missing client")
	pending := transport.NewPending([]string{"component-selector"}, nil)
	// This component handle owns no process. Settle it before the fixture paths
	// close, and prove the retained shared generation hold has actually released.
	t.Cleanup(func() {
		transport.CompletePending(pending, CommandResult{})
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
		defer cancel()
		lock, err := acquireBootstrapLock(ctx, filepath.Join(f.stateRoot, privateSupervisorDirectory, "bootstrap.lock"))
		if closeErr := closeBootstrapLock(lock); closeErr != nil {
			t.Error(closeErr)
		}
		if err != nil {
			t.Error(err)
		}
	})
	closeFault := errors.New("independent selector cleanup fault")
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) {
		return "", "", errors.Join(&InFlightError{Pending: pending}, closeFault)
	})
	closeSavedProbeTestClient(t, client, err)
	if client != nil || errors.Is(err, ErrBinding) || !errors.Is(err, closeFault) || pendingFrom(err) != pending {
		t.Fatalf("pending probe acquired binding classification or lost custody: %v", err)
	}
}

func TestProbeSavedPrivateCompatibleExactValidConfigDriftIsNotReady(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	data, err := os.ReadFile(f.successor.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writePrivateSuccessorTestBytes(t, f.successor.ConfigPath, data) })
	drift := append(append([]byte(nil), data...), []byte("\n# valid YAML changed after publication\n")...)
	if _, err = ParseConfig(drift); err != nil {
		t.Fatal(err)
	}
	writePrivateSuccessorTestBytes(t, f.successor.ConfigPath, drift)
	before := snapshotSavedProbeTestState(t, f.stateRoot)
	called := 0
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) { called++; return "", "", nil })
	closeSavedProbeTestClient(t, client, err)
	if client != nil || err != nil || called != 0 {
		t.Fatalf("successful exact binding mismatch blocked owner route: client=%t calls=%d err=%v", client != nil, called, err)
	}
	assertSavedProbeTestUnchanged(t, f.stateRoot, before)
}

func TestExactPrivatePairComparisonReadFaultStaysHard(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	data, err := os.ReadFile(f.successor.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writePrivateSuccessorTestBytes(t, f.successor.ConfigPath, data) })
	resolution, _, err := resolutionFromBinding(f.successor)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(f.successor.ConfigPath); err != nil {
		t.Fatal(err)
	}
	client, _, _, err := probeExactPrivatePair(t.Context(), f.successor, resolution, f.options)
	closeSavedProbeTestClient(t, client, err)
	if !errors.Is(err, ErrBinding) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrapped read fault became successful mismatch: %v", err)
	}
}
