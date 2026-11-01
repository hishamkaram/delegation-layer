package host

import "time"

// The host loop alone owns the count. The policy is pure and caps its index,
// so indefinite failures cannot overflow or terminate restart supervision.
func afterExit(failures int, healthy time.Duration) (int, time.Duration) {
	if healthy >= 5*time.Minute {
		failures = 0
	}
	delays := [...]time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second,
	}
	failures = min(failures+1, len(delays))
	return failures, delays[failures-1]
}
