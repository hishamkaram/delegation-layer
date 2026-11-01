package app

import (
	"path/filepath"
	"testing"
)

func TestSchedulerHostArguments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state with spaces")
	for _, action := range []string{"serve", "status", "stop"} {
		for _, argv := range [][]string{
			{"--root", root, "scheduler", action, "--json"},
			{"scheduler", "--root", root, "--json", action},
			{"scheduler", action, "--root=" + root, "--json"},
		} {
			got, err := ParseArguments(argv)
			if err != nil {
				t.Fatalf("ParseArguments(%q): %v", argv, err)
			}
			if got.Command != "scheduler" || got.SchedulerAction != action || got.Root != root || !got.JSON {
				t.Fatalf("ParseArguments(%q) changed request: %+v", argv, got)
			}
		}
	}
}

func TestSchedulerResidentFlagsRequireExactPair(t *testing.T) {
	for _, argv := range [][]string{
		{"scheduler", "run", "--scheduler-host-read-fd=3"},
		{"scheduler", "run", "--scheduler-host-read-fd=0", "--scheduler-host-write-fd=0"},
		{"scheduler", "run", "--scheduler-host-read-fd=4", "--scheduler-host-write-fd=3"},
		{"scheduler", "serve", "--scheduler-host-read-fd=3", "--scheduler-host-write-fd=4"},
	} {
		if _, err := ParseArguments(argv); err == nil {
			t.Fatalf("invalid resident request accepted: %q", argv)
		}
	}
	got, err := ParseArguments([]string{"scheduler", "run", "--scheduler-host-read-fd=3", "--scheduler-host-write-fd=4"})
	if err != nil || !got.schedulerResident || got.schedulerHostReadFD != 3 || got.schedulerHostWriteFD != 4 {
		t.Fatalf("private child argv changed: %+v, %v", got, err)
	}
}
