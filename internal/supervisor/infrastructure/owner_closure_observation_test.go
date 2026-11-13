package infrastructure

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestObserveOwnerProcessClosedReturnsPendingWithoutWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, original := newOwnerStopReleasedSuccessorFixture(t)
		before := stopRecordSnapshot(t, f.reader)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		defer cancel()
		result, err := ObserveOwnerProcessClosed(ctx, f.reader, original.reference)
		if err != nil || ctx.Err() != nil || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessPending || result.Startup != original.reference || result.Process != original.assumption.Process {
			t.Fatalf("one-shot observation waited or replaced the original live incarnation: %+v %v", result, err)
		}
		if !maps.Equal(before, stopRecordSnapshot(t, f.reader)) {
			t.Fatal("one-shot owner observation changed canonical records")
		}
		assertAvailable(t, f.lease.path, false)
	})
}

func TestObserveOwnerProcessClosedRetainsAbsentOriginalAfterSuccessor(t *testing.T) {
	f := newOwnerContactFixture(t)
	selectAbsentOwnerStopIncarnation(t, f)
	original := f.auth.request.Owner
	if err := errors.Join(f.auth.listener.Close(), f.lease.Close()); err != nil {
		t.Fatal(err)
	}
	publishOwnerStopSuccessorFixture(t, f)
	before := stopRecordSnapshot(t, f.reader)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := ObserveOwnerProcessClosed(ctx, f.reader, original)
	if err != nil || result.Startup != original || result.State != OwnerServingReleased || result.ProcessClosure != OwnerProcessClosed || result.Process != f.auth.assumption.Process {
		t.Fatalf("one-shot successor observation lost original exact-process absence: %+v %v", result, err)
	}
	if !maps.Equal(before, stopRecordSnapshot(t, f.reader)) {
		t.Fatal("one-shot owner observation changed canonical records")
	}
	assertAvailable(t, f.lease.path, false)
}

func TestObserveOwnerProcessClosedRefusesUnboundOrCanceledSubject(t *testing.T) {
	f := newOwnerContactFixture(t)
	before := stopRecordSnapshot(t, f.reader)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	wrong := f.auth.request.Owner
	wrong.IntentSHA256 = strings.Repeat("f", 64)
	result, err := ObserveOwnerProcessClosed(ctx, f.reader, wrong)
	if !errors.Is(err, ErrIdentity) || result.State != OwnerStopUnknown || result.ProcessClosure != OwnerProcessUnknown || result.Startup != wrong {
		t.Fatalf("unbound token selected another incarnation or closure: %+v %v", result, err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	result, err = ObserveOwnerProcessClosed(canceled, f.reader, f.auth.request.Owner)
	if !errors.Is(err, context.Canceled) || result.State != OwnerStopUnknown || result.ProcessClosure != OwnerProcessUnknown {
		t.Fatalf("canceled observation granted closure: %+v %v", result, err)
	}
	if !maps.Equal(before, stopRecordSnapshot(t, f.reader)) {
		t.Fatal("refused observation changed canonical records")
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}
