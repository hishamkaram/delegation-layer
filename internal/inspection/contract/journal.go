package contract

import (
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func RequestDigest(record RequestRecord) string {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return ""
	}
	return task.ComputeSHA256(data)
}

func CanonicalNow(now time.Time) string {
	if now.IsZero() {
		return ""
	}
	return now.UTC().Format(time.RFC3339Nano)
}
