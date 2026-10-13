package lifetime

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestReferencesRejectInvalidValues(t *testing.T) {
	tests := []struct {
		name       string
		kind       Kind
		assurance  Assurance
		identifier string
	}{
		{name: "unknown kind", kind: Kind("unknown"), assurance: AssuranceExact, identifier: "scope"},
		{name: "cgroup assurance", kind: KindCgroupV2, assurance: AssuranceCooperative, identifier: "scope"},
		{name: "process group assurance", kind: KindProcessGroup, assurance: AssuranceExact, identifier: "scope"},
		{name: "empty identifier", kind: KindProcessGroup, assurance: AssuranceCooperative},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRef(test.kind, test.assurance, test.identifier); !errors.Is(err, ErrInvalidRef) {
				t.Fatalf("NewRef error = %v, want ErrInvalidRef", err)
			}
		})
	}

	if _, err := Open(Ref{}); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Open zero ref error = %v, want ErrInvalidRef", err)
	}
	if _, err := NewLaunchRef(0, "linux:1", 0); !errors.Is(err, ErrInvalidLaunchRef) {
		t.Fatalf("invalid launch PID error = %v, want ErrInvalidLaunchRef", err)
	}
	if _, err := NewLaunchRef(1, "", 0); !errors.Is(err, ErrInvalidLaunchRef) {
		t.Fatalf("empty launch token error = %v, want ErrInvalidLaunchRef", err)
	}
	if _, err := NewLaunchRef(1, "linux:1", -1); !errors.Is(err, ErrInvalidLaunchRef) {
		t.Fatalf("negative launch PGID error = %v, want ErrInvalidLaunchRef", err)
	}
}

func TestWaitForEmptyUsesInjectedPolling(t *testing.T) {
	observations := []bool{false, false, true}
	sleeps := 0
	err := waitForEmpty(context.Background(), time.Millisecond, func() (bool, error) {
		value := observations[0]
		observations = observations[1:]
		return value, nil
	}, func(context.Context, time.Duration) error {
		sleeps++
		return nil
	})
	if err != nil {
		t.Fatalf("waitForEmpty returned error: %v", err)
	}
	if sleeps != 2 {
		t.Fatalf("sleep calls = %d, want 2", sleeps)
	}
}

func TestWaitForEmptyReturnsObservationError(t *testing.T) {
	wantErr := errors.New("observation failed")
	err := waitForEmpty(context.Background(), time.Millisecond, func() (bool, error) {
		return false, wantErr
	}, func(context.Context, time.Duration) error {
		t.Fatal("sleep should not run after an observation error")
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("waitForEmpty error = %v, want %v", err, wantErr)
	}
}

func TestPrepareRejectsBlankIDs(t *testing.T) {
	for _, test := range []struct {
		rootID string
		taskID string
	}{
		{rootID: "", taskID: "task"},
		{rootID: "root", taskID: ""},
		{rootID: " ", taskID: "task"},
		{rootID: "root", taskID: "task\x00suffix"},
	} {
		if _, err := Prepare(test.rootID, "task", test.taskID); !errors.Is(err, ErrInvalidRef) {
			t.Fatalf("Prepare(%q, %q, %q) error = %v, want ErrInvalidRef", test.rootID, "task", test.taskID, err)
		}
	}
}

func TestProcessGroupReferenceShape(t *testing.T) {
	identifier := "pg:" + strings.Repeat("a", 32)
	ref, err := NewRef(KindProcessGroup, AssuranceCooperative, identifier)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Kind() != KindProcessGroup || ref.Assurance() != AssuranceCooperative || ref.Identifier() != identifier {
		t.Fatalf("reference accessors returned %#v", ref)
	}
	if _, err := Open(ref); err != nil && !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open valid process-group ref error = %v", err)
	}
}

func TestProcessGroupTestScopesUseUniqueIdentifiers(t *testing.T) {
	first, _ := commandForProcessGroup(t, "true")
	second, _ := commandForProcessGroup(t, "true")
	t.Cleanup(func() {
		cleanupProcessGroupTestScope(t, first)
		cleanupProcessGroupTestScope(t, second)
	})
	if first.Reference().Identifier() == second.Reference().Identifier() {
		t.Fatal("process-group test scopes reused their identifier")
	}
}

func TestProcessGroupOwnerBindingRecordsOwnerAndWorkerAnchor(t *testing.T) {
	scope, _ := commandForProcessGroup(t, "true")
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })
	implementation, ok := scope.impl.(*processGroupScope)
	if !ok {
		t.Fatal("scope implementation is not a process-group scope")
	}
	if !implementation.ownerBound || implementation.owner.PID() <= 1 || implementation.owner.StartToken() == "" {
		t.Fatalf("owner binding = %#v, want a live owner identity", implementation.owner)
	}
	if err := scope.TrackOwnerProcess(implementation.owner); err != nil {
		t.Fatal(err)
	}
	if len(implementation.anchors) != 1 || implementation.anchors[0] != implementation.owner {
		t.Fatalf("owner anchors = %#v, want the tracked worker identity", implementation.anchors)
	}
}

func testProcessGroupRef(t *testing.T) Ref {
	t.Helper()
	plan := processGroupPlan("lifetime-test", "task", t.Name()+"-"+t.TempDir())
	ref, err := NewRef(plan.Kind(), plan.Assurance(), plan.Identifier())
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestScopeNilAndClosedCallsFailClosed(t *testing.T) {
	var scope *Scope
	if err := scope.Configure(nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Configure error = %v, want ErrClosed", err)
	}
	if _, err := scope.ObserveStarted(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil ObserveStarted error = %v, want ErrClosed", err)
	}
	if err := scope.Terminate(context.Background(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Terminate error = %v, want ErrClosed", err)
	}
	if empty, err := scope.Empty(nil); empty || !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Empty = (%t, %v), want false/ErrClosed", empty, err)
	}
	if err := scope.Release(); err != nil {
		t.Fatalf("nil Release error = %v", err)
	}
}

func commandForProcessGroup(t *testing.T, script string) (*Scope, *exec.Cmd) {
	t.Helper()
	ref := testProcessGroupRef(t)
	scope, err := Open(ref)
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			t.Skipf("process groups are unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err = scope.BindOwnerProcess(); err != nil {
		cleanupProcessGroupTestScope(t, scope)
		t.Fatalf("BindOwnerProcess: %v", err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = t.TempDir()
	if err := scope.Configure(cmd); err != nil {
		cleanupProcessGroupTestScope(t, scope)
		t.Fatalf("Configure: %v", err)
	}
	return scope, cmd
}
