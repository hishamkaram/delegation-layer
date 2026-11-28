package app

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLegacyScheduleOmissionResolvesPerOccurrenceWithoutRewritingDefinition(t *testing.T) {
	fixture := newTeamsScheduleFixture(t)
	historical := fixture.create(t)
	// Seed the historical omission through the durable contract. The fresh
	// creation command rejects this shape, while persisted definitions accept it.
	historical.ScheduleID = strings.Repeat("e", 32)
	historical.Request.Config.Budget = ""
	if err := fixture.store.CreateSchedule(t.Context(), historical); err != nil {
		t.Fatal(err)
	}
	fixture.arguments.ScheduleID = historical.ScheduleID
	before, digest := teamsScheduleStoredRequest(t, fixture.state.Root, historical.ScheduleID)
	for _, id := range []string{strings.Repeat("b", 32), strings.Repeat("c", 32)} {
		if _, err := fixture.dispatch(t, id); !errors.Is(err, errTeamsScheduleRefusal) {
			t.Fatalf("occurrence did not reach preparation: %v", err)
		}
	}
	captured := fixture.captured()
	if len(captured) != 2 {
		t.Fatalf("preparations=%d", len(captured))
	}
	for _, request := range captured {
		if request.RequestedConfig.Budget != "30m0s" || request.BudgetNanos != int64(30*time.Minute) {
			t.Fatalf("historical omission changed: %+v", request.RequestedConfig)
		}
	}
	after, currentDigest := teamsScheduleStoredRequest(t, fixture.state.Root, historical.ScheduleID)
	if !bytes.Equal(before, after) || currentDigest != digest {
		t.Fatal("occurrence rewrote historical schedule bytes")
	}
}
