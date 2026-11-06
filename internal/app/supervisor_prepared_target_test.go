package app

import (
	"bytes"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPreparedTargetBeforePrivateJournalKeepsExactSelection(t *testing.T) {
	first := supervisorGenerationTestBinding(t.TempDir(), "first", true)
	second := supervisorGenerationTestBinding(t.TempDir(), "second", true)
	for _, test := range []struct {
		name      string
		proposals []task.SupervisorRef
		published task.SupervisorRef
		found     bool
		want      task.SupervisorRef
	}{
		{"unique", []task.SupervisorRef{first}, second, true, first},
		{"identical proposals", []task.SupervisorRef{first, first}, task.SupervisorRef{}, false, first},
		{"exact published", []task.SupervisorRef{first, second}, second, true, second},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, err := task.MarshalCanonical(test.proposals)
			if err != nil {
				t.Fatal(err)
			}
			selected, decision, err := selectPreparedTargetBeforePrivateJournal(test.proposals, test.published, test.found)
			if err != nil || decision != preparedTargetRecorded || selected != test.want {
				t.Fatalf("accepted target changed: %+v %v", selected, err)
			}
			after, err := task.MarshalCanonical(test.proposals)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("selection rewrote prepared records")
			}
		})
	}
}

func TestPreparedTargetBeforePrivateJournalLeavesAmbiguityToCurrentResolver(t *testing.T) {
	first := supervisorGenerationTestBinding(t.TempDir(), "first", true)
	second := supervisorGenerationTestBinding(t.TempDir(), "second", true)
	unrelated := supervisorGenerationTestBinding(t.TempDir(), "unrelated", true)
	selected, decision, err := selectPreparedTargetBeforePrivateJournal([]task.SupervisorRef{first, second}, unrelated, true)
	if selected != (task.SupervisorRef{}) || decision != preparedTargetUndecided || err != nil {
		t.Fatalf("ambiguous proposals manufactured private authority: %+v decision=%v err=%v", selected, decision, err)
	}
}
