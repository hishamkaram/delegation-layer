package infrastructure

import (
	"errors"
	"testing"
)

func TestOwnerWireBindsReplaySelectorAndEstablishmentPurpose(t *testing.T) {
	f := newOwnerAuthFixture(t)
	request, reply := ownerReplySubjectFixture(f, ownerEnsureOwn)
	request.InitialResultFor = request.Owner
	reply.InitialResultFor = request.Owner
	if err := validateOwnerReplyBinding(f.intent, f.assumption, f.publication, request, reply); err != nil {
		t.Fatal(err)
	}
	changed := reply
	changed.InitialResultFor = StartupReference{}
	if err := validateOwnerReplyBinding(f.intent, f.assumption, f.publication, request, changed); !errors.Is(err, ErrIdentity) {
		t.Fatalf("reply failed to echo explicit selector: %v", err)
	}
	request, reply = ownerReplySubjectFixture(f, ownerEnsureOther)
	request.Initial = InitialOperation{Kind: InitialMigrateOwn}
	reply.Initial = request.Initial
	reply.InitialResultFor = reply.Startup
	if err := validateOwnerReplyBinding(f.intent, f.assumption, f.publication, request, reply); err != nil {
		t.Fatalf("establishment-only reply not bound: %v", err)
	}
	changed = reply
	changed.InitialResultFor = request.Owner
	if err := changed.validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("establishment selector accepted A instead of B: %v", err)
	}
	changed = reply
	changed.Initial = InitialOperation{}
	if err := validateOwnerReplyBinding(f.intent, f.assumption, f.publication, request, changed); !errors.Is(err, ErrIdentity) {
		t.Fatalf("establishment purpose not echoed: %v", err)
	}
	changed = reply
	changed.Disposition = Disposition{Kind: DispositionWarmReuse}
	if err := changed.validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("A passed B preparation outcome through establishment: %v", err)
	}
}

func TestOwnerWireRejectsForeignSelectorsFailuresAndReports(t *testing.T) {
	f := newOwnerAuthFixture(t)
	for _, action := range []ownerAction{ownerStatus, ownerEnsureOther} {
		request, _ := ownerReplySubjectFixture(f, action)
		request.InitialResultFor = request.Owner
		if err := request.validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("%s accepted replay selector: %v", action, err)
		}
	}
	_, reply := ownerReplySubjectFixture(f, ownerEnsureOwn)
	reply.Code = ownerFault
	reply.Failure = PreparationFailure{Causes: PreparationCauses(1 << 31)}
	if err := reply.validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("unknown failure bits accepted: %v", err)
	}
	_, reply = ownerReplySubjectFixture(f, ownerStatus)
	reply.Code = ownerFault
	reply.Failure = PreparationFailure{IndependentFault: true}
	if err := reply.validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("status transported a preparation failure: %v", err)
	}
	_, reply = ownerReplySubjectFixture(f, ownerEnsureOwn)
	reply.Report = ManualMigrationReport{State: ManualMigrationNotInitialized, Legacy: ManualMigrationLegacyNotPresent, Active: ManualMigrationActiveNone}
	if err := reply.validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("Own transported a manual-only report: %v", err)
	}
}
