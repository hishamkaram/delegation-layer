package infrastructure

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func historicalRootContextFixture(t *testing.T) (*intentJournal, *taskdir.ReadOnlyStore, task.RootContextCoordinates, string, string) {
	t.Helper()
	journal, _ := journalFixture(t)
	intent := journalIntent(t, journal, 1)
	intent.Inputs.RootDevice++
	refreshIntentDigest(t, &intent)
	path := putHistoricalFixture(t, journal, intent)
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), journal.pin.Root(), predicate.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	want := task.RootContextCoordinates{Device: intent.Inputs.RootDevice, Inode: intent.Inputs.RootInode}
	token, err := task.ConnectorRootToken(journal.pin.RootID(), want)
	if err != nil {
		t.Fatal(err)
	}
	return journal, reader, want, token, path
}

func TestFindHistoricalRootContext(t *testing.T) {
	journal, reader, want, token, path := historicalRootContextFixture(t)
	got, found, err := FindHistoricalRootContext(t.Context(), reader, token)
	if err != nil || !found || got != want {
		t.Fatalf("historical coordinates: got=%v found=%v err=%v", got, found, err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	currentToken, err := task.ConnectorRootToken(journal.pin.RootID(), task.RootContextCoordinates{Device: journal.pin.Device(), Inode: journal.pin.Inode()})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err = FindHistoricalRootContext(t.Context(), reader, currentToken); err != nil || found {
		t.Fatalf("unrecorded current coordinates: found=%v err=%v", found, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(after) {
		t.Fatalf("passive lookup changed intent: %v", err)
	}
	if err = os.WriteFile(filepath.Join(journal.pin.Root(), "infrastructure", intentName(2)), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err = FindHistoricalRootContext(t.Context(), reader, token); found || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("matched history concealed malformed record: found=%v err=%v", found, err)
	}
}
