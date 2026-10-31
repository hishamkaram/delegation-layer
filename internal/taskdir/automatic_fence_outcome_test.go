package taskdir

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAutomaticFenceReferencesValidatedExistingOutcomeOnlyAfterIdleProof(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	userID := strings.Repeat("d", 32)
	stop, err := td.PrepareStop(userID, "user", time.Time{})
	must(t, err)
	must(t, stop.Release())
	// Populate an older winner that the canonical reader accepts without a
	// provider-start record. Its payload/raw facts are explicit fixture inputs;
	// no lifetime termination or worker completion is asserted.
	before := existingUnstartedOutcomeFixture(t, td)
	peer, err := store.OpenTask(td.TaskID)
	must(t, err)
	t.Cleanup(func() { must(t, peer.Close()) })
	must(t, peer.runLock.LockEXNonblocking())
	proven, digest, err := td.FenceUnlaunchedTaskContext(t.Context())
	must(t, err)
	if proven || digest != "" {
		t.Fatal("outcome existence alone bypassed run ownership")
	}
	must(t, peer.runLock.Unlock())
	proven, digest, err = td.FenceUnlaunchedTaskContext(t.Context())
	must(t, err)
	if !proven || digest != task.ComputeSHA256(before) {
		t.Fatalf("outcome reference: %v %q", proven, digest)
	}
	after, err := os.ReadFile(filepath.Join(td.Dir, "outcome.json"))
	must(t, err)
	if !bytes.Equal(before, after) {
		t.Fatal("fence replaced the validated terminal winner")
	}
	records, err := td.ReadStopRecords()
	must(t, err)
	if len(records) != 1 || records[0].Request.RequestID != userID {
		t.Fatal("outcome fence created another refusal request")
	}
}

func existingUnstartedOutcomeFixture(t *testing.T, td *TaskDir) []byte {
	t.Helper()
	_, meta, specHash, metaHash, err := td.PreparedIdentity()
	must(t, err)
	must(t, td.initializeRaw())
	emptySHA := task.ComputeSHA256(nil)
	manifest := []task.RawManifestEntry{
		{Path: "raw/stderr", Size: 0, SHA256: emptySHA},
		{Path: "raw/stdout", Size: 0, SHA256: emptySHA},
	}
	manifestData, err := task.MarshalCanonical(manifest)
	must(t, err)
	payload := []byte("previously refused admission")
	must(t, os.WriteFile(filepath.Join(td.Dir, "publish.reject"), payload, 0o600))
	outcome := task.OutcomeRecord{
		SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID,
		SpecSHA256: specHash, MetaSHA256: metaHash, Verdict: task.VerdictRejected,
		EvidenceSHA256: task.ComputeSHA256(manifestData), Predicate: meta.Predicate,
		Payload: task.PayloadDescriptor{Basename: "publish.reject", Length: int64(len(payload)), SHA256: task.ComputeSHA256(payload)},
	}
	must(t, task.ValidateOutcomeRecord(&outcome))
	must(t, td.commitRecord("outcome.json", outcome))
	data, err := task.MarshalCanonical(outcome)
	must(t, err)
	return data
}
