package contract

import (
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	testScheduleID = "0123456789abcdef0123456789abcdef"
	testTaskID     = "fedcba9876543210fedcba9876543210"
)

func at(seconds int64) time.Time { return time.Unix(seconds, 0).UTC() }

func digestOf(character byte) string { return strings.Repeat(string(character), 64) }

func testRequest() Request {
	return Request{
		Provider:     "codex:exec",
		CanonicalCwd: "/work/repository",
		Config: task.TaskConfig{
			Model:        "gpt",
			Permission:   "read-only",
			ReviewTarget: &task.ReviewTarget{Kind: task.ReviewTargetBase, Value: "main"},
		},
	}
}

func testDefinition() Definition {
	return Definition{
		DefinitionHeader: DefinitionHeader{
			ScheduleID: testScheduleID,
			CreatedAt:  at(1_000),
			Cron:       "0 9 * * 1-5",
			Timezone:   "Europe/Berlin",
			Request:    testRequest(),
		},
		Brief: []byte("summarize open issues"),
	}
}

func testOccurrence() Occurrence {
	return Occurrence{TaskID: testTaskID, ScheduleID: testScheduleID, Due: at(2_000), ArmedAt: at(2_001)}
}
