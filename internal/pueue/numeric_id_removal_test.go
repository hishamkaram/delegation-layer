package pueue

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func TestRemoveCompletedTaskSerializesSubmissionAndRejectsReusedID(t *testing.T) {
	fixture := newNumericIDRemovalTestFixture(t)
	removal := holdNumericIDRemovalTestCommand(t, fixture.supervisor.client, fixture.oldIdentity)

	// Removal has already observed the old terminal row and started remove.
	// A valid Submit must wait before it can consume its permit or issue add.
	faultContext, cancelFault := context.WithTimeout(t.Context(), 50*time.Millisecond)
	blocked, submitErr := fixture.supervisor.client.Submit(faultContext, fixture.permit, fixture.launch)
	cancelFault()
	for _, pending := range pendingHandles(submitErr) {
		waitPending(t, pending)
	}
	if !errors.Is(submitErr, context.DeadlineExceeded) || blocked.Matched {
		t.Fatalf("submission interleaved with numeric-ID removal: observation=%+v err=%v", blocked, submitErr)
	}
	removal.finish(t)
	gatePath := filepath.Join(filepath.Dir(fixture.supervisor.configPath), "numeric-remove-release")
	if removeErr := os.Remove(gatePath); removeErr != nil {
		t.Fatal(removeErr)
	}
	assertNumericIDRemovalTestReuse(t, fixture)
}

func assertNumericIDRemovalTestReuse(t *testing.T, fixture numericIDRemovalTestFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// Retrying the same unconsumed permit must now work. The fixture reuses ID
	// zero only after remove emptied its queue, just as Pueue's allocator does.
	added, addErr := fixture.supervisor.client.Submit(ctx, fixture.permit, fixture.launch)
	for _, pending := range pendingHandles(addErr) {
		waitPending(t, pending)
	}
	if addErr != nil || !added.Matched || added.NumericTaskID != 0 || added.State != StateQueued {
		t.Fatalf("submission did not reuse the empty queue's ID after removal: observation=%+v err=%v", added, addErr)
	}
	if staleErr := fixture.supervisor.client.RemoveCompletedTask(ctx, fixture.oldIdentity); !errors.Is(staleErr, ErrUnknown) {
		t.Fatalf("stale identity was allowed to remove a different terminal task at reused ID zero: %v", staleErr)
	}
	retained, reconcileErr := fixture.supervisor.client.Reconcile(ctx, added.Identity)
	if reconcileErr != nil || !retained.Matched || retained.NumericTaskID != 0 || retained.State != StateEnded {
		t.Fatalf("stale removal changed the replacement row: observation=%+v err=%v", retained, reconcileErr)
	}
	commands := readInvocations(t, fixture.supervisor.logPath)
	if countCommand(commands, "remove") != 1 || countCommand(commands, "add") != 1 {
		t.Fatalf("unexpected numeric-ID mutations: %v", commands)
	}
}

type numericIDRemovalTestFixture struct {
	supervisor  *fakeSupervisor
	permit      *taskdir.SubmissionPermit
	launch      Launch
	oldIdentity Identity
}

func newNumericIDRemovalTestFixture(t *testing.T) numericIDRemovalTestFixture {
	t.Helper()
	private := newPrivateSupervisorTestFixture(t)
	writeExecutable(t, private.clientPath, numericIDRemovalTestScript+privateClientFixture)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := BindPrivate(ctx, private.clientPath, private.daemonPath, private.stateRoot, Options{ObservationTimeout: 10 * time.Second})
	supervisorLeaseTestSettleClient(t, client, err)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(client.Binding().ConfigPath)
	fake := &fakeSupervisor{
		client: client, executable: private.clientPath, configPath: client.Binding().ConfigPath,
		statusPath: filepath.Join(base, "test-status.json"), logPath: filepath.Join(base, "numeric-mutations.log"),
	}
	newTask := newTaskFixture(t, client.Binding())
	permit, err := newTask.taskDir.PrepareSubmission(client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	record, err := permit.Record()
	if err != nil {
		t.Fatal(err)
	}
	oldTaskID := "0" + record.TaskID[1:]
	if record.TaskID[0] == '0' {
		oldTaskID = "1" + record.TaskID[1:]
	}
	id := int64(0)
	oldIdentity := Identity{
		RootID: record.RootID, TaskID: oldTaskID, NumericTaskID: &id,
		SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64),
	}
	writeStatusFor(t, fake, oldIdentity.RootID, oldIdentity.TaskID, id, StateEnded)
	empty := statusTestPayload(t, map[string]any{}, map[string]any{})
	if writeErr := os.WriteFile(filepath.Join(base, "numeric-empty.json"), empty, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	replacement := *fake
	replacement.statusPath = filepath.Join(base, "numeric-replacement.json")
	writeStatusFor(t, &replacement, record.RootID, record.TaskID, id, StateEnded)
	return numericIDRemovalTestFixture{
		supervisor: fake, permit: permit, oldIdentity: oldIdentity,
		launch: Launch{RunnerExecutable: private.clientPath, RootPath: newTask.store.Root},
	}
}

func holdNumericIDRemovalTestCommand(t *testing.T, client *Client, identity Identity) *supervisorLeaseTestCallback {
	t.Helper()
	gatePath := filepath.Join(filepath.Dir(client.Binding().ConfigPath), "numeric-remove-release")
	if err := unix.Mkfifo(gatePath, 0o600); err != nil {
		t.Fatal(err)
	}
	gate, err := os.OpenFile(gatePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := gate.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	remover, err := client.WithObservationTimeout(10 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	call := &supervisorLeaseTestCallback{entered: make(chan struct{}), done: make(chan struct{})}
	signalStarted := sync.OnceFunc(func() { close(call.entered) })
	remover.options.Observer = func(event CommandEvent) {
		if event.Stage == "started" && len(event.Argv) > 3 && event.Argv[3] == "remove" {
			signalStarted()
		}
	}
	call.release = sync.OnceFunc(func() {
		if _, writeErr := gate.WriteString("release\n"); writeErr != nil {
			t.Error(writeErr)
		}
	})
	t.Cleanup(func() {
		call.release()
		cancel()
		<-call.done
		for _, pending := range pendingHandles(call.err) {
			waitPending(t, pending)
		}
	})
	go func() {
		call.err = remover.RemoveCompletedTask(ctx, identity)
		close(call.done)
	}()
	select {
	case <-call.entered:
	case <-call.done:
		t.Fatalf("removal returned before its mutation command started: %v", call.err)
	case <-ctx.Done():
		t.Fatalf("removal did not reach its controlled command boundary: %v", ctx.Err())
	}
	return call
}

const numericIDRemovalTestScript = `#!/bin/sh
set -eu
case "${3-}" in
  --version) printf '%s\n' 'pueue 4.0.4'; exit 0 ;;
  remove|add)
    base=${2%/*}
    {
      printf 'argc=%s\n' "$#"
      for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
      printf '%s\n' '---'
    } >> "$base/numeric-mutations.log"
    if [ "$3" = remove ]; then
      [ "${4-}" = 0 ] || exit 64
      if [ -p "$base/numeric-remove-release" ]; then
        IFS= read -r release < "$base/numeric-remove-release"
      fi
      cp "$base/numeric-empty.json" "$base/test-status.json"
      exit 0
    fi
    cmp -s "$base/numeric-empty.json" "$base/test-status.json" || exit 65
    cp "$base/numeric-replacement.json" "$base/test-status.json"
    printf '%s\n' 0
    exit 0
    ;;
esac
`
