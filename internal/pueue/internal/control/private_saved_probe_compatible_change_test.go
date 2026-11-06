package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestProbeSavedPrivateCompatibleRechecksSelectedPublication(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	damageCompatibleProbePair(t, f, "missing client")
	dClient, dDaemon := installPrivateSupervisorPair(t, filepath.Join(f.base, "available-D"))
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) {
		changed := f.successor
		changed.ClientExecutable += "-replacement"
		writePrivateSuccessorTestRecord(t, privateSupervisorBindingPath(f.stateRoot), privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: changed})
		return dClient, dDaemon, nil
	})
	closeSavedProbeTestClient(t, client, err)
	if client != nil || !errors.Is(err, ErrBinding) {
		t.Fatalf("D silently retargeted publication: client=%t err=%v", client != nil, err)
	}
	assertPrivateDaemonStartCount(t, f.stateRoot, 2)
}

func TestProbeSavedPrivateCompatibleMissingMemberCannotHideOtherStatFault(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	damageCompatibleProbePair(t, f, "missing client")
	damageCompatibleProbePair(t, f, "missing daemon")
	if err := os.Symlink(f.daemonPath, f.daemonPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removePrivateTestPath(t, f.daemonPath) })
	called := 0
	client, err := ProbeSavedPrivateCompatible(t.Context(), f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) { called++; return "", "", nil })
	closeSavedProbeTestClient(t, client, err)
	if client != nil || err == nil || called != 0 {
		t.Fatalf("other member stat fault hidden: calls=%d err=%v", called, err)
	}
}

func TestProbeSavedPrivateCompatibleCancellationDoesNotSelectD(t *testing.T) {
	f := newCompletedPrivateSuccessorTestFixture(t, true)
	damageCompatibleProbePair(t, f, "missing client")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := 0
	client, err := ProbeSavedPrivateCompatible(ctx, f.stateRoot, f.saved, f.options, func(context.Context) (string, string, error) { called++; return "", "", nil })
	closeSavedProbeTestClient(t, client, err)
	if client != nil || !errors.Is(err, context.Canceled) || called != 0 {
		t.Fatalf("canceled observation selected D: calls=%d err=%v", called, err)
	}
}
