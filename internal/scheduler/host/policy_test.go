package host

import (
	"testing"
	"time"
)

func TestRestartPolicyExactDelaysCapAndHealthyReset(t *testing.T) {
	want := []time.Duration{1, 2, 4, 8, 16, 30, 60, 120, 300, 300, 300, 300}
	failures := 0
	for index, seconds := range want {
		var delay time.Duration
		failures, delay = afterExit(failures, 0)
		if delay != seconds*time.Second {
			t.Fatalf("failure %d: delay=%s, want %s", index+1, delay, seconds*time.Second)
		}
	}
	if _, delay := afterExit(failures, 5*time.Minute-time.Nanosecond); delay != 300*time.Second {
		t.Fatalf("reset before five continuous healthy minutes: %s", delay)
	}
	if count, delay := afterExit(failures, 5*time.Minute); count != 1 || delay != time.Second {
		t.Fatalf("healthy reset count=%d delay=%s", count, delay)
	}
	for range 10000 {
		failures, _ = afterExit(failures, 0)
	}
	if _, delay := afterExit(failures, 0); delay != 300*time.Second {
		t.Fatalf("indefinite cap changed: %s", delay)
	}
}
