//go:build linux

package pueue

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxPrivateProcessAbsentError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "wrapped ESRCH", err: fmt.Errorf("inspect process: %w", unix.ESRCH), want: true},
		{name: "wrapped not-exist", err: fmt.Errorf("inspect process: %w", os.ErrNotExist), want: true},
		{name: "other error", err: fmt.Errorf("inspect process: %w", unix.EACCES), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isLinuxPrivateProcessAbsentError(test.err); got != test.want {
				t.Fatalf("isLinuxPrivateProcessAbsentError(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestParseLinuxPrivateProcessStatCapturesIncarnation(t *testing.T) {
	fields := make([]string, 20)
	for index := range fields {
		fields[index] = "1"
	}
	fields[0] = "S"
	fields[19] = "987654"
	observation, err := parseLinuxPrivateProcessStat([]byte("42 (pueued worker) " + strings.Join(fields, " ") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Present || observation.StartToken != "linux:987654" {
		t.Fatalf("unexpected process observation: %+v", observation)
	}
}

func TestParseLinuxPrivateProcessStatTreatsZombieAsUnavailable(t *testing.T) {
	fields := make([]string, 20)
	for index := range fields {
		fields[index] = "1"
	}
	fields[0] = "Z"
	observation, err := parseLinuxPrivateProcessStat([]byte("42 (pueued) " + strings.Join(fields, " ")))
	if err != nil {
		t.Fatal(err)
	}
	if observation.Present {
		t.Fatalf("zombie process remained available: %+v", observation)
	}
}
