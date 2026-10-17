package control

import (
	"errors"
	"strings"
	"testing"
)

func TestMatchingObservationRequiresOneExactLabelAndTarget(t *testing.T) {
	identity := Identity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64)}
	label := identity.Label()
	if observation, err := matchingObservation([]Job{{ID: 7, Label: &label, State: StateRunning}}, identity, validBindingForTest()); err != nil || !observation.Matched || observation.State != StateRunning {
		t.Fatalf("positive match failed: %+v %v", observation, err)
	}
	wrong := "delegate:" + strings.Repeat("a", 32) + ":" + strings.Repeat("e", 32)
	if _, err := matchingObservation([]Job{{ID: 7, Label: &wrong, State: StateRunning}}, identity, validBindingForTest()); !errors.Is(err, ErrUnknown) {
		t.Fatalf("wrong label was accepted: %v", err)
	}
	target := int64(8)
	if _, err := matchingObservation([]Job{{ID: 7, Label: &label, State: StateRunning}}, Identity{RootID: identity.RootID, TaskID: identity.TaskID, SpecSHA256: identity.SpecSHA256, MetaSHA256: identity.MetaSHA256, NumericTaskID: &target}, validBindingForTest()); !errors.Is(err, ErrUnknown) {
		t.Fatalf("wrong numeric target was accepted: %v", err)
	}
}
