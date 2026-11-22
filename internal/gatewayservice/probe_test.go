package gatewayservice

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"testing"
)

func TestMacServiceBackgroundCallerUsesExplicitGUIDomain(t *testing.T) {
	f := newServiceFixture(t, MacOS)
	f.runner.guiName = "Background"
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatalf("available same-user GUI domain refused: %v", err)
	}
	status, err := f.manager.Start(f.ctx)
	if err != nil || status.State != StateRegistered {
		t.Fatalf("explicit GUI registration failed: %+v %v", status, err)
	}
	// A registered job can have no active connector process.
	f.runner.active = false
	status, err = f.manager.Status(f.ctx)
	if err != nil || status.State != StateRegistered {
		t.Fatalf("registration became execution readiness: %+v %v", status, err)
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	target := domain + "/" + f.manager.name
	if !slices.ContainsFunc(f.runner.calls, func(args []string) bool {
		return slices.Equal(args, []string{"bootstrap", domain, f.manager.unitPath()})
	}) {
		t.Fatalf("missing exact GUI bootstrap: %v", f.runner.calls)
	}
	for _, args := range f.runner.calls {
		if !slices.Equal(args, []string{"print", domain}) && !slices.Equal(args, []string{"print", target}) && !slices.Equal(args, []string{"bootstrap", domain, f.manager.unitPath()}) {
			t.Fatalf("caller-relative or fallback invocation: %v", args)
		}
	}
}

func TestMacServiceRegistrationUsesTargetInsteadOfCallerRelativeList(t *testing.T) {
	f := newServiceFixture(t, MacOS)
	if _, err := f.manager.Install(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.runner.loaded = true
	// The caller-relative lookup cannot see the target GUI registration.
	f.runner.faultOperation = "list"
	f.runner.faultResult = Result{ExitCode: 113}
	status, err := f.manager.Status(f.ctx)
	if err != nil || status.State != StateRegistered {
		t.Fatalf("target registration was lost through caller-relative lookup: %+v %v", status, err)
	}
}

func TestMacServiceProbeFaultsNeverProveAbsence(t *testing.T) {
	cleanupFault := errors.New("joined probe cleanup fault")
	for _, tc := range []struct {
		name    string
		results []probeResult
		want    error
		cause   error
	}{
		{"missing GUI", []probeResult{{code: 113}}, ErrGUIRequired, nil},
		{"denied GUI", []probeResult{{code: 1}}, ErrGUIRequired, nil},
		{"GUI cleanup", []probeResult{{code: 0, err: cleanupFault}}, ErrGUIRequired, cleanupFault},
		{"denied label", []probeResult{{}, {code: 1}}, ErrManager, nil},
		{"unknown label", []probeResult{{}, {code: 112}}, ErrManager, nil},
		{"invalid exit", []probeResult{{}, {code: 256}}, ErrManager, nil},
		{"label cleanup", []probeResult{{}, {code: 113, err: cleanupFault}}, ErrManager, cleanupFault},
		{"logout at absence recheck", []probeResult{{}, {code: 113}, {code: 113}}, ErrGUIRequired, nil},
		{"cancellation and cleanup", []probeResult{{}, {code: 113, err: errors.Join(context.Canceled, cleanupFault)}}, ErrManager, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceFixture(t, MacOS)
			f.runner.probeResults = tc.results
			status, err := f.manager.Status(f.ctx)
			if !errors.Is(err, tc.want) || status.State != StateUnknown || tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("ambiguous probe became a disposition: %+v %v", status, err)
			}
			if len(f.runner.probeResults) != 0 {
				t.Fatal("required target/domain probe was omitted")
			}
		})
	}
}

func TestMacRegisteredLabelWithoutPrivateUnitIsForeign(t *testing.T) {
	f := newServiceFixture(t, MacOS)
	f.runner.loaded = true
	status, err := f.manager.Status(f.ctx)
	if !errors.Is(err, ErrForeignService) || status.State != StateUnknown {
		t.Fatalf("unowned GUI registration was adopted: %+v %v", status, err)
	}
}

func TestServiceRejectsAnotherUID(t *testing.T) {
	f := newServiceFixture(t, MacOS)
	c := f.manager.config
	c.UID++
	if _, err := New(c); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("another user's manager was accepted: %v", err)
	}
}
