package taskdir

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestLogRangeLiveAppendAndSealCursorContinuity(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	permit := consumeStart(t, td)
	writer, err := td.OpenRawWriter("stdout")
	must(t, err)
	first := []byte{0, 255, 'x', 'y'}
	_, err = writer.Write(first)
	must(t, err)
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	range1, err := reader.ReadTaskLogRange(t.Context(), td.TaskID, "stdout", 0, 2, "")
	must(t, err)
	if !bytes.Equal(range1.Data, first[:2]) || range1.Length != 2 || range1.Sealed || range1.WholeSHA256 != "" || range1.RangeSHA256 != task.ComputeSHA256(first[:2]) {
		t.Fatalf("live range: %+v", range1)
	}
	_, err = writer.Write([]byte("z"))
	must(t, err)
	range2, err := reader.ReadTaskLogRange(t.Context(), td.TaskID, "stdout", 2, MaxLogRangeBytes, range1.Cursor)
	must(t, err)
	if !bytes.Equal(range2.Data, []byte{'x', 'y', 'z'}) {
		t.Fatalf("appended bytes: %q", range2.Data)
	}
	must(t, writer.Close())
	_, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, permit.Release())
	sealed, err := reader.ReadTaskLogRange(t.Context(), td.TaskID, "stdout", 5, MaxLogRangeBytes, range2.Cursor)
	must(t, err)
	if !sealed.Sealed || len(sealed.Data) != 0 || sealed.WholeSHA256 != task.ComputeSHA256(append(first, 'z')) {
		t.Fatalf("sealed EOF: %+v", sealed)
	}
}

func TestLogRangeRefusesReplacementTruncationAndForeignCursor(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	path := filepath.Join(td.Dir, "raw", "stdout")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte("original"), 0o600))
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	observed, err := reader.ReadTaskLogRange(t.Context(), td.TaskID, "stdout", 0, 2, "")
	must(t, err)
	if _, err = reader.ReadTaskLogRange(t.Context(), td.TaskID, "stderr", 2, 2, observed.Cursor); !errors.Is(err, ErrLogCursor) {
		t.Fatalf("foreign stream cursor: %v", err)
	}
	must(t, os.Truncate(path, 3))
	if _, err = reader.ReadTaskLogRange(t.Context(), td.TaskID, "stdout", 2, 2, observed.Cursor); !errors.Is(err, ErrLogTruncated) {
		t.Fatalf("truncation: %v", err)
	}
	must(t, os.Rename(path, path+".old"))
	must(t, os.WriteFile(path, []byte("replacement"), 0o600))
	if _, err = reader.ReadTaskLogRange(t.Context(), td.TaskID, "stdout", 2, 2, observed.Cursor); !errors.Is(err, ErrLogObjectReplaced) {
		t.Fatalf("replacement: %v", err)
	}
	for _, limit := range []int64{0, -1, MaxLogRangeBytes + 1} {
		if _, err = reader.ReadTaskLogRange(t.Context(), td.TaskID, "stdout", 0, limit, ""); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = reader.ReadTaskLogRange(ctx, td.TaskID, "stdout", 0, 1, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}

func TestLogRangeReadsOnlyRequestedBytesAndKeepsSealDigestAsExpectedMetadata(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	permit := consumeStart(t, td)
	writer, err := td.OpenRawWriter("stdout")
	must(t, err)
	_, err = writer.Write([]byte("original"))
	must(t, err)
	must(t, writer.Close())
	_, err = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, permit.Release())
	// Corruption outside the selected range must not cause a hidden whole-file
	// scan. Full evidence consumers still reject it through their existing API.
	must(t, os.WriteFile(filepath.Join(td.Dir, "raw", "stdout"), []byte("oriOTHER"), 0o600))
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	observed, err := reader.ReadTaskLogRange(t.Context(), td.TaskID, "stdout", 0, 3, "")
	must(t, err)
	if string(observed.Data) != "ori" || observed.WholeSHA256 != task.ComputeSHA256([]byte("original")) || observed.RangeSHA256 != task.ComputeSHA256([]byte("ori")) {
		t.Fatalf("bounded observation: %+v", observed)
	}
	if _, err = td.LogDescriptors(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("full evidence validation lost corruption check: %v", err)
	}
}
