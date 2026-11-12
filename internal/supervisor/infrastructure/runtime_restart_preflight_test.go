package infrastructure

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeRestartPreflightIsBoundReadOnlyAndSanitizesFault(t *testing.T) {
	for _, fault := range []bool{false, true} {
		t.Run(map[bool]string{false: "custody", true: "unknown on fault"}[fault], func(t *testing.T) {
			exerciseRuntimeRestartPreflight(t, fault)
		})
	}
}

func exerciseRuntimeRestartPreflight(t *testing.T, fault bool) {
	t.Helper()
	var calls atomic.Int32
	f := newOwnerServerFixture(t, &ownerServerTestHost{preflight: func(_ context.Context, operation Operation, selected LaunchContext, wait DaemonWaitOwner) (RuntimeRestartCustody, error) {
		calls.Add(1)
		inputs := operation.Intent().Inputs
		if inputs.Context.ValidateContext(selected, inputs.Root, inputs.RootID) != nil || wait == nil {
			t.Error("preflight lost recorded context or exact resident registry")
		}
		if fault {
			return RuntimeRestartCustodyAvailable, errors.New("private preflight diagnostic")
		}
		return RuntimeRestartCustodyAvailable, nil
	}})
	f.start()
	before := producerBudgetTree(t, f.root)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	owner := f.request(t, ownerStatus).Owner
	observed, err := ReadRuntimeRestartPreflight(ctx, contextPassiveReader(t, f.entry.journal), owner)
	want := RuntimeRestartCustodyAvailable
	if fault {
		want = RuntimeRestartCustodyUnknown
	}
	if (err != nil) != fault || observed.Validate() != nil || observed.From != (RuntimeRestartReference{RootID: f.entry.operation.Intent().Inputs.RootID, Startup: owner}) || observed.Custody != want || calls.Load() != 1 {
		t.Fatalf("preflight lost exact dated custody: %+v calls=%d err=%v", observed, calls.Load(), err)
	}
	if err != nil && strings.Contains(err.Error(), "private preflight diagnostic") {
		t.Fatal("wire exposed private host diagnostic")
	}
	if !maps.Equal(before, producerBudgetTree(t, f.root)) {
		t.Fatal("passive preflight changed root records or started work")
	}
}

func TestRuntimeRestartPreflightLegacyCapabilityRefusesBeforeContact(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	before := producerBudgetTree(t, f.journal.pin.Root())
	if observed, err := ReadRuntimeRestartPreflight(ctx, f.reader, f.auth.request.Owner); !errors.Is(err, ErrOwnerIncompatible) || observed != (RuntimeRestartPreflight{}) {
		t.Fatalf("legacy owner was probed through unsupported protocol: %+v %v", observed, err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
		t.Fatal("unsupported preflight changed root")
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func TestRuntimeRestartPreflightRejectsForeignContextBeforeHost(t *testing.T) {
	var calls atomic.Int32
	f := newOwnerServerFixture(t, &ownerServerTestHost{preflight: func(context.Context, Operation, LaunchContext, DaemonWaitOwner) (RuntimeRestartCustody, error) {
		calls.Add(1)
		return RuntimeRestartCustodyAvailable, nil
	}})
	selected := f.birthContext(t)
	selected.ControlEnvironment = append(selected.ControlEnvironment, "FOREIGN_CONTEXT=selected")
	reference, _, err := f.entry.journal.stageContext(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	request := f.request(t, ownerRuntimeRestartPreflight)
	request.Context = reference
	f.start()
	connection := f.dial(t)
	if err = writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	reply := readServerReply(t, connection)
	if reply.Code != ownerRefused || reply.RestartPreflight != (RuntimeRestartPreflight{}) || calls.Load() != 0 {
		t.Fatalf("foreign preflight context reached Host: calls=%d reply=%+v", calls.Load(), reply)
	}
}
