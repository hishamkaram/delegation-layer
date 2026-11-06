package host

import (
	"testing"
	"time"
)

func TestRestartPolicyExactDelaysCapAndHealthyReset(t *testing.T) {
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
		5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}
	failures := 0
	for index, expected := range want {
		var delay time.Duration
		failures, delay = afterExit(failures, 0)
		if delay != expected {
			t.Fatalf("failure %d: delay=%s, want %s", index+1, delay, expected)
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
