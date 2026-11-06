package control

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestPublishedPrivateRecoveryClientPreservesShutdownConstruction(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	original := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	options := privateMigrationTestOptions(fixture.stateRoot, privateTestObservationTimeout)
	options.RuntimeClientExecutable = fixture.clientPath
	options.ControlOwnerExecutable = os.Args[0]
	client, pairErr, err := preparePublishedPrivatePair(t.Context(), original.Binding(), options)
	t.Cleanup(func() {
		if closeErr := client.CloseAfterPending(err); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil || pairErr != nil {
		t.Fatalf("published pair preparation: pair=%v operation=%v", pairErr, err)
	}
	requireQualifiedPrivateShutdown(t, fixture.stateRoot, client)
}

func TestRecordedPrivateMigrationClientPreservesShutdownConstruction(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	original := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	options := privateMigrationTestOptions(fixture.stateRoot, privateTestObservationTimeout)
	options.ControlOwnerExecutable = os.Args[0]
	var client *Client
	var prepareErr error
	t.Cleanup(func() {
		if closeErr := client.CloseAfterPending(prepareErr); closeErr != nil {
			t.Error(closeErr)
		}
	})
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		client, prepareErr = PrepareRecordedPrivateMigrationWithLock(ctx, fixture.stateRoot, original.Binding(), options)
		return prepareErr
	})
	if err != nil {
		t.Fatal(err)
	}
	requireQualifiedPrivateShutdown(t, fixture.stateRoot, client)
}

func requireQualifiedPrivateShutdown(t *testing.T, root string, client *Client) {
	t.Helper()
	// The setup daemon is finite and its original owner joins its actual Wait.
	// The same client must refuse an unscoped shutdown and accept the real EX.
	if err := client.Shutdown(t.Context()); !errors.Is(err, ErrBinding) {
		t.Fatalf("qualified private client accepted shutdown without EX: %v", err)
	}
	err := WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		return client.Shutdown(ctx)
	})
	if err != nil {
		t.Fatalf("canonical preparation lost supported shutdown: %v", err)
	}
}
