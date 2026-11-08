package command

import (
	"errors"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/recurrence"
)

func TestScheduleCreateRangeExhaustionIsOperational(t *testing.T) {
	input := CreateInput{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Cron: "* * * * *", Timezone: "UTC", TimezoneSet: true, CreatedAt: time.Date(2262, 4, 11, 23, 47, 16, 854775807, time.UTC)}
	_, _, err := BuildDefinition(input)
	if !errors.Is(err, recurrence.ErrRangeExhausted) {
		t.Fatalf("range err=%v", err)
	}
}
