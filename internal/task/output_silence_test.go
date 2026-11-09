package task

import (
	"testing"
	"time"
)

func TestOutputSilenceRequestedDurations(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		duration    time.Duration
	}{{"", "", 0}, {"0", "0s", 0}, {"0s", "0s", 0}, {"250ms", "250ms", 250 * time.Millisecond}, {"60s", "1m0s", time.Minute}} {
		got, err := NormalizeOutputSilenceTimeout(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("normalize %q=%q,%v", tc.input, got, err)
		}
		d, err := ParseOutputSilenceTimeout(got)
		if err != nil || d != tc.duration {
			t.Fatalf("parse %q=%s,%v", got, d, err)
		}
	}
	for _, input := range []string{"-1s", "soon", "999999999999999999h"} {
		if _, err := ParseOutputSilenceTimeout(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}
