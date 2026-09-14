package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckpointStageTraceSkipsCleanupForUnlinkedStage(t *testing.T) {
	for _, checkpoint := range []string{"before-payload-cleanup", "fail-payload-cleanup-barrier"} {
		t.Run(checkpoint, func(t *testing.T) { testUnlinkedStageCleanup(t, checkpoint) })
	}
}

func testUnlinkedStageCleanup(t *testing.T, checkpoint string) {
	t.Helper()
	base := t.TempDir()
	events := filepath.Join(base, "events")
	stage := filepath.Join(base, "stage.candidate.tmp")
	dest := filepath.Join(base, "result.txt")
	injector := &checkpointFaultInjector{checkpoint: checkpoint, events: events, timeout: time.Second}

	if err := injector.OnBeforeStageCreate(stage, dest); err != nil {
		t.Fatal(err)
	}
	if err := injector.OnCleanupUnlink(stage); err != nil {
		t.Fatal(err)
	}
	if err := injector.OnCleanupDirBarrier(base); err != nil {
		t.Fatalf("unlinked cleanup barrier: %v", err)
	}
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "stage-create:result.txt") {
		t.Fatalf("stage creation was not traced: %q", data)
	}
	if strings.Contains(string(data), "payload-cleanup") || strings.Contains(string(data), "cleanup-barrier") {
		t.Fatalf("unlinked candidate reached cleanup checkpoint: %q", data)
	}
	if trace := injector.stages[stage]; trace.linkAttempted {
		t.Fatal("unlinked candidate was marked as a link attempt")
	}
}

func TestCheckpointStageTracePreservesAttemptedCleanupAndUnknownRefusal(t *testing.T) {
	base := t.TempDir()
	events := filepath.Join(base, "events")
	stage := filepath.Join(base, "stage.linked.tmp")
	dest := filepath.Join(base, "result.txt")
	injector := &checkpointFaultInjector{checkpoint: "fail-payload-cleanup-barrier", events: events, timeout: time.Second}

	if err := injector.OnBeforeStageCreate(stage, dest); err != nil {
		t.Fatal(err)
	}
	if err := injector.OnLink(stage, dest); err != nil {
		t.Fatal(err)
	}
	if !injector.stages[stage].linkAttempted {
		t.Fatal("link attempt was not recorded")
	}
	// Cleanup faults remain observable when a later link step fails before the
	// post-link barrier, such as a destination collision.
	if err := injector.OnCleanupUnlink(stage); err != nil {
		t.Fatal(err)
	}
	if err := injector.OnCleanupDirBarrier(base); err == nil || !strings.Contains(err.Error(), "fail-payload-cleanup-barrier") {
		t.Fatalf("linked cleanup barrier error = %v", err)
	}
	if err := injector.OnCleanupUnlink(filepath.Join(base, "stage.unknown.tmp")); err == nil || !strings.Contains(err.Error(), "no destination trace") {
		t.Fatalf("unknown cleanup stage error = %v", err)
	}
	if _, err := os.Stat(events); errors.Is(err, os.ErrNotExist) {
		t.Fatal("linked stage trace was not recorded")
	}
}
