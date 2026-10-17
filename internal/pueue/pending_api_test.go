package pueue_test

import (
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	_ func(*pueue.Pending) []string                           = (*pueue.Pending).Args
	_ func(*pueue.Pending) <-chan struct{}                    = (*pueue.Pending).Done
	_ func(*pueue.Pending, []string) bool                     = (*pueue.Pending).MatchesArgs
	_ func(*pueue.Pending) (pueue.CommandResult, bool)        = (*pueue.Pending).Result
	_ func(*pueue.Pending) (task.SupervisorRef, string, bool) = (*pueue.Pending).StatusIdentity
	_ func(*pueue.Pending) bool                               = (*pueue.Pending).VersionProbe
)

func TestPendingPublicMethodSurface(t *testing.T) {
	want := []string{"Args", "Done", "MatchesArgs", "Result", "StatusIdentity", "VersionProbe"}
	pending := reflect.TypeFor[*pueue.Pending]()
	got := make([]string, pending.NumMethod())
	for index := range got {
		got[index] = pending.Method(index).Name
	}
	if len(got) != len(want) {
		t.Fatalf("public Pending methods = %v, want observation-only methods %v", got, want)
	}
	for index, name := range want {
		if got[index] != name {
			t.Errorf("public Pending method %d = %q, want %q", index, got[index], name)
		}
	}
}
