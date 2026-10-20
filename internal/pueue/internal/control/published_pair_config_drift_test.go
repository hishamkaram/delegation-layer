package control

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// driftPublishedTestConfig retargets the published config endpoint, the only
// change the canonical upgrade makes, and returns the drifted bytes.
func driftPublishedTestConfig(t *testing.T, published task.SupervisorRef) []byte {
	t.Helper()
	data, err := os.ReadFile(published.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	socket := strings.TrimPrefix(published.Endpoint, "unix:")
	drifted := strings.Replace(string(data), socket, socket+"-upgrade", 1)
	if drifted == string(data) {
		t.Fatalf("published config does not name its endpoint %q", socket)
	}
	writePrivateTestFile(t, published.ConfigPath, drifted)
	return []byte(drifted)
}

func observePublishedTestPairContext(t *testing.T, stateRoot string, derive func(context.Context) (context.Context, context.CancelFunc)) (PublishedPairAvailability, error) {
	t.Helper()
	var availability PublishedPairAvailability
	err := WithBootstrapLockContext(t.Context(), stateRoot, func(ctx context.Context) error {
		observed, cancel := derive(ctx)
		defer cancel()
		var observeErr error
		availability, observeErr = ObservePublishedPrivatePairWithLock(observed, stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
		return observeErr
	})
	return availability, err
}

// Config-only drift of a complete, present pair is a fact only when the
// canonical config parsed and resolved and both executables proved their
// published provenance. Every fault beside the drift returns no fact. No
// observation changes a record or starts a daemon.
func TestObservePublishedPairConfigDriftRequiresCleanProof(t *testing.T) {
	cases := []configDriftCase{
		{name: "clean endpoint drift"},
		{name: "malformed config", causes: []error{ErrConfiguration}, detail: "YAML syntax", inject: func(t *testing.T, published task.SupervisorRef, _ []byte) {
			writePrivateTestFile(t, published.ConfigPath, "shared: [")
		}},
		{name: "missing config", causes: []error{ErrConfiguration, os.ErrNotExist}, detail: "open regular file", inject: func(t *testing.T, published task.SupervisorRef, _ []byte) {
			removePrivateTestPath(t, published.ConfigPath)
		}},
		{name: "unreadable config", causes: []error{ErrConfiguration}, detail: "", inject: func(t *testing.T, published task.SupervisorRef, _ []byte) {
			removePrivateTestPath(t, published.ConfigPath)
			makePrivateTestDirectory(t, published.ConfigPath)
		}},
		{name: "symlinked config", causes: []error{ErrConfiguration}, detail: "symlink path component", inject: func(t *testing.T, published task.SupervisorRef, drifted []byte) {
			target := published.ConfigPath + ".target"
			writePrivateTestFile(t, target, string(drifted))
			removePrivateTestPath(t, published.ConfigPath)
			if err := os.Symlink(target, published.ConfigPath); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "resolve fault", causes: []error{errUnixSocketPathTooLong}, detail: "", inject: func(t *testing.T, published task.SupervisorRef, _ []byte) {
			writePrivateTestFile(t, published.ConfigPath, "shared: {unix_socket_path: '/"+strings.Repeat("s", 107)+"'}")
		}},
		{name: "changed client", causes: []error{ErrBinding}, provenance: true, inject: func(t *testing.T, published task.SupervisorRef, _ []byte) {
			writeExecutable(t, published.ClientExecutable, privateClientFixture+"\n# changed\n")
		}},
		{name: "changed daemon", causes: []error{ErrBinding}, provenance: true, inject: func(t *testing.T, published task.SupervisorRef, _ []byte) {
			data, err := os.ReadFile(published.DaemonExecutable)
			if err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, published.DaemonExecutable, string(data)+"\n# changed\n")
		}},
		{name: "canceled", cause: context.Canceled, derive: func(ctx context.Context) (context.Context, context.CancelFunc) {
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			return canceled, cancel
		}},
		{name: "deadline", cause: context.DeadlineExceeded, derive: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithDeadline(ctx, time.Now().Add(-time.Second))
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, original, _ := newPrivateReconciliationFixture(t)
			drifted := driftPublishedTestConfig(t, original)
			if testCase.inject != nil {
				testCase.inject(t, original, drifted)
			}
			derive := testCase.derive
			if derive == nil {
				derive = func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} }
			}
			before := snapshotPublishedPairRecords(t, fixture.stateRoot)
			availability, err := observePublishedTestPairContext(t, fixture.stateRoot, derive)
			testCase.assertObserved(t, availability, err)
			before.assertUnchanged(t, fixture.stateRoot)
		})
	}
}

type configDriftCase struct {
	name   string
	inject func(*testing.T, task.SupervisorRef, []byte)
	derive func(context.Context) (context.Context, context.CancelFunc)
	// cause is a context fault, returned without the migration projection.
	cause error
	// causes are the canonical reader's own sentinels beside the projection;
	// detail is its wrapping text where a sentinel alone is shared.
	causes []error
	detail string
	// provenance is an executable mismatch: bare ErrBinding, never a config
	// read fault.
	provenance bool
}

// assertObserved requires a clean drift to be the fact with no error, and a
// fault to be no fact carrying its own cause: the context error, or the
// migration-required projection with the reader's exact underlying causes.
func (c configDriftCase) assertObserved(t *testing.T, availability PublishedPairAvailability, err error) {
	t.Helper()
	clean := c.inject == nil && c.derive == nil
	switch {
	case clean && (availability != PublishedPairConfigChanged || err != nil):
		t.Fatalf("clean config drift = %d, %v; want the drift fact", availability, err)
	case !clean && (availability != 0 || err == nil):
		t.Fatalf("faulted config drift = %d, %v; want no fact and its fault", availability, err)
	case c.cause != nil && !errors.Is(err, c.cause):
		t.Fatalf("context fault = %v, want %v", err, c.cause)
	case !clean && c.cause == nil && !errors.Is(err, ErrPrivateSupervisorMigrationRequired):
		t.Fatalf("fault = %v, want migration-required", err)
	}
	c.assertCauses(t, err)
}

func (c configDriftCase) assertCauses(t *testing.T, err error) {
	t.Helper()
	if c.detail != "" && !strings.Contains(err.Error(), c.detail) {
		t.Fatalf("fault = %v, want the reader's %q cause", err, c.detail)
	}
	if c.provenance && errors.Is(err, ErrConfiguration) {
		t.Fatalf("executable mismatch = %v, want no config read fault", err)
	}
	for _, cause := range c.causes {
		if !errors.Is(err, cause) {
			t.Fatalf("fault = %v lost its underlying cause %v", err, cause)
		}
	}
}

// Strict preparation of the same clean drift keeps its exact refusal.
func TestPreparePublishedPrivateRefusesCleanConfigDrift(t *testing.T) {
	fixture, original, _ := newPrivateReconciliationFixture(t)
	driftPublishedTestConfig(t, original)
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		client, _, prepareErr := PreparePublishedPrivateWithLock(ctx, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
		if client != nil {
			return errors.Join(errors.New("strict preparation accepted a drifted config"), client.Close())
		}
		return prepareErr
	})
	if !errors.Is(err, ErrBinding) || !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("strict preparation of config drift = %v", err)
	}
}

// Close injection is unavailable through observation, so the settlement it
// shares is tested directly: a close fault discards any fact and keeps the
// drift projection beside the fault; a projection-free fact keeps the fault.
func TestSettlePublishedPairAvailabilityKeepsProjectionBesideCloseFault(t *testing.T) {
	closeFault := errors.New("close fault")
	if availability, err := settlePublishedPairAvailability(PublishedPairConfigChanged, closeFault); availability != 0 || !errors.Is(err, closeFault) || !errors.Is(err, ErrBinding) || !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("drift close fault = %d, %v", availability, err)
	}
	if availability, err := settlePublishedPairAvailability(PublishedPairReady, closeFault); availability != 0 || !errors.Is(err, closeFault) || err.Error() != closeFault.Error() {
		t.Fatalf("ready close fault = %d, %v", availability, err)
	}
	if availability, err := settlePublishedPairAvailability(PublishedPairConfigChanged, nil); availability != PublishedPairConfigChanged || err != nil {
		t.Fatalf("clean drift settlement = %d, %v", availability, err)
	}
	if got, want := PublishedPairConfigChanged.ProjectedError(), privatePublishedRecoveryError(ErrBinding); got == nil || got.Error() != want.Error() || !errors.Is(got, ErrBinding) {
		t.Fatalf("drift projection = %v, want %v", got, want)
	}
	for _, availability := range []PublishedPairAvailability{PublishedPairReady, PublishedPairNotPublished, PublishedPairIncomplete, PublishedPairMemberMissing} {
		if err := availability.ProjectedError(); err != nil {
			t.Fatalf("fact %d projected %v", availability, err)
		}
	}
}
