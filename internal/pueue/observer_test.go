package pueue

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestClientObserverCannotChangeCommandAndRetainsNaturalCompletion(t *testing.T) {
	fake := newFakeSupervisorPaths(t, "ok", "pueue "+FixtureVersion)
	var mu sync.Mutex
	var events []CommandEvent
	observer := func(event CommandEvent) {
		mu.Lock()
		defer mu.Unlock()
		copy := event
		copy.Argv = slices.Clone(event.Argv)
		events = append(events, copy)
		// Neither the pending command nor another event aliases this slice.
		event.Argv[0] = "observer must not change the executable"
	}
	_, err := Bind(context.Background(), fake.executable, fake.configPath, Options{
		ObservationTimeout: 2 * time.Second, Environment: fake.environment, Observer: observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("incomplete ownership events: %+v", events)
	}
	wantArgv := []string{fake.executable, "-c", fake.configPath, "--version"}
	for i, event := range events {
		if event.Stage != []string{"entry", "started", "completed"}[i] || !slices.Equal(event.Argv, wantArgv) || event.CommandID != events[0].CommandID {
			t.Fatalf("changed event or command: %+v", event)
		}
		if err := task.ValidateTaskID(event.CommandID); err != nil {
			t.Fatal(err)
		}
	}
	if events[0].PID != 0 || events[0].ExitCode != -1 || events[1].PID <= 0 || events[2].PID != events[1].PID || events[2].ExitCode != 0 {
		t.Fatalf("entry/Start/completion facts conflated: %+v", events)
	}
}
