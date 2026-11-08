package command

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

type definitionReader struct {
	contract.Store
	definition contract.Definition
}

func (s definitionReader) GetSchedule(context.Context, string) (contract.Schedule, error) {
	return contract.Schedule{Definition: s.definition, State: contract.StateEnabled}, nil
}

func TestScheduleCreateReadbackUsesCapturedDefinition(t *testing.T) {
	definition := contract.Definition{DefinitionHeader: contract.DefinitionHeader{
		ScheduleID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Cron: "0 9 * * *", Timezone: "UTC", Request: contract.Request{Auto: true, CanonicalCwd: t.TempDir()},
	}, Brief: []byte("private brief content")}
	store := definitionReader{definition: definition}
	match, err := readBackDefinition(t.Context(), store, definition)
	if err != nil || !match {
		t.Fatalf("matching definition: match=%t err=%v", match, err)
	}
	definition.CreatedAt = definition.CreatedAt.Add(time.Nanosecond)
	match, err = readBackDefinition(t.Context(), store, definition)
	if match || !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("different identity: match=%t err=%v", match, err)
	}
}
