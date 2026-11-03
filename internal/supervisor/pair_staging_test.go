package supervisor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
)

func TestStatePairContentStagingPreservesSelection(t *testing.T) {
	for _, selected := range []bool{false, true} {
		name := "unselected"
		if selected {
			name = "selected"
		}
		t.Run(name, func(t *testing.T) { testStatePairContentStaging(t, selected) })
	}
}

func testStatePairContentStaging(t *testing.T, selected bool) {
	t.Helper()
	target := supervisorfixture.CanonicalTempDir(t)
	base := filepath.Join(target, ".supervisor")
	if err := ensureSupervisorDirectory(target, base); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if selected {
		oldClient, oldDaemon := statePairStagingSources(t, "old-client", "old-daemon")
		if err := installStateSupervisorPair(t.Context(), target, oldClient, oldDaemon); err != nil {
			t.Fatal(err)
		}
		var err error
		before, err = os.ReadFile(filepath.Join(base, stateSupervisorPairRecordName))
		if err != nil {
			t.Fatal(err)
		}
	}
	client, daemon := statePairStagingSources(t, "new-client", "new-daemon")
	record, err := stateSupervisorPairIdentity(t.Context(), client, daemon)
	if err != nil {
		t.Fatal(err)
	}
	// This operation copies immutable members only. Selecting this pair belongs
	// to the target's separate canonical preparation/migration transaction.
	if err = stageStateSupervisorPairContentAddressed(t.Context(), base, client, daemon, record); err != nil {
		t.Fatal(err)
	}
	requireStatePairSelectionUnchanged(t, base, before, selected)
	directory := filepath.Join(base, stateSupervisorPairDirectoryPrefix+record.PairSHA256)
	supervisorfixture.RequireExecutable(t, filepath.Join(directory, "pueue"), "new-client")
	supervisorfixture.RequireExecutable(t, filepath.Join(directory, "pueued"), "new-daemon")
}

func statePairStagingSources(t *testing.T, clientContents, daemonContents string) (string, string) {
	t.Helper()
	source := supervisorfixture.CanonicalTempDir(t)
	client, daemon := filepath.Join(source, "pueue"), filepath.Join(source, "pueued")
	supervisorfixture.WriteExecutable(t, client, clientContents)
	supervisorfixture.WriteExecutable(t, daemon, daemonContents)
	return client, daemon
}

func requireStatePairSelectionUnchanged(t *testing.T, base string, before []byte, selected bool) {
	t.Helper()
	after, err := os.ReadFile(filepath.Join(base, stateSupervisorPairRecordName))
	if !selected {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("content-only staging published a target selection: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("content-only staging changed the target's published selection")
	}
}
