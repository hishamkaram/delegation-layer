package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestRepairEditorDispatchPrecedesRunner(t *testing.T) {
	if os.Getenv("DELEGATE_TEST_EARLY_EDITOR") == "1" {
		os.Exit(run([]string{"--supervisor-repair-editor"}, os.Stdout, os.Stderr))
	}
	command := exec.Command(os.Args[0], "-test.run=^TestRepairEditorDispatchPrecedesRunner$")
	command.Env = []string{"DELEGATE_TEST_EARLY_EDITOR=1", "GORACE=atexit_sleep_ms=0"}
	output, err := command.CombinedOutput()
	if err == nil || command.ProcessState.ExitCode() != 1 || len(output) != 0 {
		t.Fatalf("private editor did not fail before runner parsing/output: code=%v output=%q err=%v", command.ProcessState, output, err)
	}
}
