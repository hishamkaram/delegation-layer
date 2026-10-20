package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Each error comes from a real launch through the lineage's native scope,
// which owns Start and Wait. Only an ordinary Git exit status is a false
// result; wrapping keeps it, and any other or additional fault is returned.
func TestNativeLineageGitExitSeparatesOrdinaryExitFromFaults(t *testing.T) {
	lineage := newNativeLineage(t)
	if succeeded, err := lineage.gitExitsZero(lineage.main, "rev-parse", "HEAD"); !succeeded || err != nil {
		t.Fatalf("clean Git exit: succeeded=%v err=%v", succeeded, err)
	}
	missing := strings.Repeat("0", 40) + "^{commit}"
	if succeeded, err := lineage.gitExitsZero(lineage.main, "cat-file", "-e", missing); succeeded || err != nil {
		t.Fatalf("ordinary Git nonzero exit: succeeded=%v err=%v", succeeded, err)
	}
	nonzero := exec.Command("git", "-C", lineage.main, "cat-file", "-e", missing)
	exitErr := lineage.launch(nonzero)
	if nonzero.ProcessState == nil || nonzero.ProcessState.ExitCode() == 0 || !ordinaryGitExit(exitErr) {
		t.Fatalf("joined nonzero Git: state=%v err=%v", nonzero.ProcessState, exitErr)
	}
	if !ordinaryGitExit(fmt.Errorf("query: %w", exitErr)) {
		t.Fatalf("wrapped ordinary exit was not classified as ordinary: %v", exitErr)
	}
	if ordinaryGitExit(errors.Join(exitErr, errors.New("native scope cleanup fault"))) {
		t.Fatal("a joined native fault became an ordinary Git exit")
	}
	// The native scope reads only its deadline; an elapsed one is refused by
	// the production budget before Start.
	expired := lineage
	ctx, cancel := context.WithDeadline(lineage.scope.ctx, time.Now().Add(-time.Second))
	defer cancel()
	expired.scope.ctx = ctx
	if succeeded, err := expired.gitExitsZero(lineage.main, "rev-parse", "HEAD"); succeeded || err == nil || ordinaryGitExit(err) {
		t.Fatalf("expired native scope became an ordinary result: succeeded=%v err=%v", succeeded, err)
	}
	t.Setenv("PATH", t.TempDir())
	if succeeded, err := lineage.gitExitsZero(lineage.main, "rev-parse", "HEAD"); succeeded || err == nil {
		t.Fatalf("Git launch fault became an ordinary result: succeeded=%v err=%v", succeeded, err)
	}
}
