package task

import (
	"errors"
	"time"
)

const StopCauseOutputSilence = "output-silence"

// ParseOutputSilenceTimeout preserves omission and accepts explicit disable.
// The runner owns this policy, independently of provider-native timeouts.
func ParseOutputSilenceTimeout(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration < 0 {
		return 0, errors.New("output silence timeout must be a nonnegative duration")
	}
	return duration, nil
}

func NormalizeOutputSilenceTimeout(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	duration, err := ParseOutputSilenceTimeout(value)
	if err != nil {
		return "", err
	}
	return duration.String(), nil
}

// IsDeadlineStopCause identifies finite runner expiry, excluding user cancellation.
func IsDeadlineStopCause(cause string) bool {
	return cause == "budget" || cause == StopCauseOutputSilence
}
