package payload

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

// The size is a representative fixture, not a maximum supported raw-log size
// or a deadline claim. Canonical terminal evidence still validates its bytes.
func TestRemoteRetirementWithLargeSealedRawStream(t *testing.T) {
	const rawBytes int64 = 64 << 20
	store, td, request := retirementLimitsTask(t, rawBytes)
	// This budget covers retirement, after immutable fixture construction.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	reader := retirementReader(t, store)
	canceled, stop := context.WithCancel(ctx)
	stop()
	report, err := Retire(canceled, store, request.TaskID, remotedata.Destination, true)
	if !errors.Is(err, context.Canceled) || report.Applied {
		t.Fatalf("pre-canceled retirement changed bytes: %+v %v", report, err)
	}
	if _, present, readErr := ReadRetirement(ctx, reader, request.TaskID, remotedata.Destination); readErr != nil || present {
		t.Fatalf("pre-canceled retirement published fence: present=%v %v", present, readErr)
	}
	started := time.Now()
	report, err = Retire(ctx, store, request.TaskID, remotedata.Destination, true)
	elapsed := time.Since(started)
	t.Logf("raw_stderr_bytes=%d retire_elapsed=%s caller_budget=2m caller_context_error=%v applied=%v retirement_error=%v", rawBytes, elapsed, ctx.Err(), report.Applied, err)
	if err != nil || ctx.Err() != nil || !report.Applied {
		t.Fatalf("large-log retirement failed: %+v %v context=%v", report, err, ctx.Err())
	}
	assertRetirementRawRetained(t, td, rawBytes)
	if _, err = os.Stat(request.CanonicalCwd); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired input workspace remains: %v", err)
	}
}

func TestRemoteRetirementKeepsCanonicalRawEvidenceValidation(t *testing.T) {
	store, td, request := retirementLimitsTask(t, 1<<20)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	raw, err := os.OpenFile(filepath.Join(td.Dir, "raw", "stderr"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := raw.WriteAt([]byte{1}, 512)
	if err = errors.Join(writeErr, raw.Sync(), raw.Close()); err != nil {
		t.Fatal(err)
	}
	report, err := Retire(ctx, store, request.TaskID, remotedata.Destination, true)
	if !errors.Is(err, task.ErrEvidenceFault) || report.Applied {
		t.Fatalf("corrupt canonical evidence authorized retirement: %+v %v", report, err)
	}
	reader := retirementReader(t, store)
	if _, present, readErr := ReadRetirement(ctx, reader, request.TaskID, remotedata.Destination); readErr != nil || present {
		t.Fatalf("invalid evidence published fence: present=%v %v", present, readErr)
	}
	if _, err = os.Stat(request.CanonicalCwd); err != nil {
		t.Fatalf("invalid evidence lost input workspace: %v", err)
	}
}

func retirementLimitsTask(t *testing.T, rawBytes int64) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	// Staging and creation use their context-aware APIs. Start and seal are
	// synchronous fixture-owner operations with no context-taking API.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	store, seed, original := lifecyclefixture.NewAppTestTask(t, false)
	_, meta, err := seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	request := *original
	request.TaskID = strings.Repeat("f", 32)
	intent := retirementLimitsInputs(t, ctx, store, request.TaskID)
	digest, err := remote.IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	request.CanonicalCwd = intent.RemoteCwd
	request.Remote = &task.RemoteBinding{StorageLayout: task.RemoteDataOrganized, OriginRootID: intent.OriginRootID, IntentSHA256: digest, InputSHA256: intent.InputSHA256}
	meta.TaskID, meta.SpecSHA256 = request.TaskID, ""
	td, err := store.CreateTaskContext(ctx, request.TaskID, &request, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	retirementLimitsSeal(t, td, rawBytes)
	return store, td, &request
}

func retirementLimitsInputs(t *testing.T, ctx context.Context, store *taskdir.Store, id string) remote.Intent {
	t.Helper()
	source := t.TempDir()
	data := []byte("owned input bytes")
	if err := os.WriteFile(filepath.Join(source, "input.txt"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := snapshot.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	manifest := remotefiles.Manifest{Entries: []remotefiles.Entry{{Path: "input.txt", Size: int64(len(data)), SHA256: task.ComputeSHA256(data)}}}
	intent := remoteTransferIntent(t, store, store, id, manifest)
	intent.StorageLayout = task.RemoteDataOrganized
	intent.RemoteCwd, err = LayoutInputWorkspace(intent.StorageLayout, store.Root, store.RootID, id)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := remote.Coordinator{Exchange: &remoteFileExchange{store: store}}
	cwd, err := UploadInputs(ctx, coordinator, intent, manifest, snapshot)
	if err != nil || cwd != intent.RemoteCwd {
		t.Fatalf("stage owned input workspace: cwd=%q %v", cwd, err)
	}
	return intent
}

func retirementLimitsSeal(t *testing.T, td *taskdir.TaskDir, rawBytes int64) {
	t.Helper()
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	for _, operation := range []func() error{permit.Consume, func() error { return td.RecordStarted(0) }, func() error {
		return td.WriteRawFrom(strings.NewReader("answer\n"), io.LimitReader(retirementZeroReader{}, rawBytes))
	}} {
		if err = operation(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	seedRemoteCaptureClosure(t, td)
	collectContext, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	winner, cleanup, err := td.CollectContext(collectContext, task.FixturePredicateRef())
	if err != nil || cleanup != nil || winner == nil {
		t.Fatalf("canonical fixture publication failed: %+v %v", winner, errors.Join(err, cleanup))
	}
}

func assertRetirementRawRetained(t *testing.T, td *taskdir.TaskDir, rawBytes int64) {
	t.Helper()
	info, err := os.Stat(filepath.Join(td.Dir, "raw", "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != rawBytes {
		t.Fatalf("retirement changed raw stream: size=%d want=%d", info.Size(), rawBytes)
	}
	inspection, err := td.Inspect()
	if err != nil || !inspection.Terminal() {
		t.Fatalf("retirement invalidated canonical terminal evidence: %+v %v", inspection, err)
	}
}

type retirementZeroReader struct{}

func (retirementZeroReader) Read(buffer []byte) (int, error) { clear(buffer); return len(buffer), nil }
