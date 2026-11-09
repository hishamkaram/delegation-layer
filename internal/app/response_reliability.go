package app

import (
	"fmt"
	"strings"
	"time"
)

func writeHumanScheduleReliability(output *strings.Builder, response Response) {
	if policy := response.SchedulePolicy; policy != nil {
		fmt.Fprintf(output, "quota_policy enabled=%t max_hold=%s revision=%d\n", policy.Enabled, policy.MaxHold, policy.Revision)
	}
	health := response.ScheduleHealth
	if health == nil {
		return
	}
	fmt.Fprintf(output, "schedule_health id=%s consecutive_non_committed=%d complete=%t unresolved=%d hold_deferred=%t", health.ScheduleID, health.ConsecutiveNonCommitted, health.Complete, health.Unresolved, health.HoldDeferred)
	if health.DominantFailureCode != "" {
		fmt.Fprintf(output, " dominant_failure_code=%s", health.DominantFailureCode)
	}
	output.WriteByte('\n')
	if occurrence := health.LastOccurrence; occurrence != nil {
		fmt.Fprintf(output, "last_occurrence task_id=%s due=%s\n", occurrence.TaskID, occurrence.Due.Format(time.RFC3339Nano))
	}
	if committed := health.LastCommitted; committed != nil {
		fmt.Fprintf(output, "last_committed task_id=%s due=%s", committed.TaskID, committed.Due.Format(time.RFC3339Nano))
		if committed.ObservedAt != nil {
			fmt.Fprintf(output, " observed_at=%s", committed.ObservedAt.Format(time.RFC3339Nano))
		}
		output.WriteByte('\n')
	}
	if hold := health.Hold; hold != nil {
		fmt.Fprintf(output, "quota_hold source_task_id=%s reason=%s created_at=%s until=%s capped=%t\n", hold.SourceTaskID, hold.Reason, hold.CreatedAt.Format(time.RFC3339Nano), hold.Until.Format(time.RFC3339Nano), hold.Capped)
	}
	if notice := health.LastNotification; notice != nil {
		fmt.Fprintf(output, "notification event_id=%s kind=%s result=%s\n", notice.EventID, notice.Kind, notice.Result)
	}
}
