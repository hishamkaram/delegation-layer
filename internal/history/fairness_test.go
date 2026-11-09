package history

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestCombinedScanObservesCanonicalEvidenceBeforeMetadataPassCompletes(t *testing.T) {
	f := newSource(t)
	// Keep one full observation batch valid even if the directory's first
	// enumeration batch omits one candidate. Other candidates model partial
	// admissions, avoiding hundreds of fsynced fixture records for a scheduling test.
	for i := 1; i <= DefaultObservationBatch+1; i++ {
		f.task(t, i, "2026-01-01T00:00:00Z")
	}
	for i := DefaultObservationBatch + 2; i <= ScanBatchSize+1; i++ {
		require(t, os.Mkdir(filepath.Join(f.store.Root, "tasks", fmt.Sprintf("%032x", i)), 0o700))
	}
	before := snapshot(t, f.store.Root)
	c := openCatalog(t, f.options)
	other := openCatalog(t, f.options)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	paused := false
	c.afterScanBatch = func(ctx context.Context) error {
		if paused {
			return nil
		}
		paused = true
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var scanErr error
	go func() { _, scanErr = c.ScanAndObserve(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-entered:
	case <-done:
		t.Fatalf("scan ended before batch gate: %v", scanErr)
	case <-ctx.Done():
		t.Fatal("scan never reached committed-batch boundary")
	}
	assertPartialObservationProgress(t, ctx, c)
	if _, err := other.RefreshObservations(ctx, 1); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("second session gained observation write authority: %v", err)
	}
	close(release)
	<-done
	require(t, scanErr)
	progress, err := c.Progress(ctx)
	require(t, err)
	// The directory pass finished even though interrupted admissions remain
	// visible entry faults. Mid-pass incompleteness is asserted at the gate.
	if progress.Incomplete || progress.Indexed != ScanBatchSize+1 || progress.Observed != DefaultObservationBatch+1 || progress.Faults != ScanBatchSize-DefaultObservationBatch {
		t.Fatalf("scan did not finish: %+v", progress)
	}
	if !reflect.DeepEqual(before, snapshot(t, f.store.Root)) {
		t.Fatal("interleaved observation changed authoritative source state")
	}
}

func assertPartialObservationProgress(t *testing.T, ctx context.Context, c *Catalog) {
	t.Helper()
	page, err := c.Query(ctx, Query{Admission: "not-admitted"})
	require(t, err)
	if !page.Progress.Incomplete || page.Progress.Indexed != ScanBatchSize {
		t.Fatalf("scan gate was not before completion: %+v", page.Progress)
	}
	if page.Progress.Observed != DefaultObservationBatch || len(page.Rows) != DefaultObservationBatch {
		t.Fatalf("canonical observation queue starved behind full metadata scan: %+v rows=%d", page.Progress, len(page.Rows))
	}
	for _, row := range page.Rows {
		if row.Observation == nil || row.Observation.Source != "passive" || row.Observation.Admission != task.AdmissionNotAdmitted {
			t.Fatalf("row has no canonical passive admission evidence: %+v", row)
		}
	}
}
