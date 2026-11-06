package control

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRunnerReplacementAcceptsValidatedInstalledHistoryWithoutReadingHistoricalFiles(t *testing.T) {
	root := canonicalTemp(t)
	rootID, taskID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	client := &Client{}
	client.binding.ConfigPath = PrivateConfigPath(root)
	current := filepath.Join(root, "delegate-run")
	if err := os.WriteFile(current, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := task.MarshalCanonical(task.RootRecord{SchemaVersion: task.SchemaVersion, RootID: rootID, CreatedAt: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "root.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := RunnerCommandReplacement{
		NumericID: 7, RootID: rootID, TaskID: taskID, RootPath: root,
		Label:                  "delegate:" + rootID + ":" + taskID,
		OldRunner:              filepath.Join(root, "removed-old-install", "delegate-run"),
		NewRunner:              current,
		PreviousManagedRunners: []string{filepath.Join(root, "removed-prior-install", "delegate-run")},
	}
	if err := validateRunnerCommandReplacement(client, replacement); err != nil {
		t.Fatalf("validated installed runner history was refused: %v", err)
	}
	for _, invalid := range []string{"relative/delegate-run", "", filepath.Join(root, "prior") + "/../delegate-run", filepath.Join(root, "bad\x00runner")} {
		replacement.PreviousManagedRunners = []string{invalid}
		if err := validateRunnerCommandReplacement(client, replacement); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("malformed previous path %q: %v", invalid, err)
		}
	}
}
