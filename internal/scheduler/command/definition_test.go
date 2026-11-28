package command

import (
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/recurrence"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestScheduleCreateRangeExhaustionIsOperational(t *testing.T) {
	input := CreateInput{Request: contract.Request{Config: task.TaskConfig{Budget: "1m"}}, ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Cron: "* * * * *", Timezone: "UTC", TimezoneSet: true, CreatedAt: time.Date(2262, 4, 11, 23, 47, 16, 854775807, time.UTC)}
	_, _, err := BuildDefinition(input)
	if !errors.Is(err, recurrence.ErrRangeExhausted) {
		t.Fatalf("range err=%v", err)
	}
}
