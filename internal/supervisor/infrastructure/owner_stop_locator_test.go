package infrastructure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// These owned journal records have no listener or daemon. Actual self/lease
// metadata anchors their shape; the fixture proves no native placement.
type stopLocatorFixture struct {
	owner   ownerJournalFixture
	reader  *taskdir.ReadOnlyStore
	records [4][]byte
}

func newStopLocatorFixture(t *testing.T, future bool) *stopLocatorFixture {
	t.Helper()
	owner := ownerJournal(t)
	requireAssumptionPut(t, owner)
	if _, err := owner.journal.putPublication(t.Context(), owner.lease, owner.publication); err != nil {
		t.Fatal(err)
	}
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), owner.journal.pin.Root(), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	f := &stopLocatorFixture{owner: owner, reader: reader}
	data, err := reader.ReadInfrastructureRecordContext(t.Context(), intentName(1))
	if err != nil {
		t.Fatal(err)
	}
	if future {
		data = bytes.Replace(data, []byte(`"protocol":3`), []byte(`"protocol":99`), 1)
		data = stopExtra(data, `"future_payload":{"runner":"/unavailable/program","environment":["FUTURE=opaque"]}`)
	}
	f.rebind(t, data, future)
	return f
}

func stopExtra(data []byte, field string) []byte {
	prefix := bytes.Clone(bytes.TrimSuffix(bytes.TrimSpace(data), []byte("}")))
	return append(append(prefix, []byte(","+field+"}")...), '\n')
}

func (f *stopLocatorFixture) write(t *testing.T, index int, data []byte) {
	t.Helper()
	names := [4]string{intentName(1), assumptionName(1), publicationName(1), settlementName(1)}
	if err := os.WriteFile(filepath.Join(f.reader.Root(), "infrastructure", names[index]), data, 0o600); err != nil {
		t.Fatal(err)
	}
	f.records[index] = bytes.Clone(data)
}

func (f *stopLocatorFixture) rebind(t *testing.T, intent []byte, future bool) {
	t.Helper()
	reference := StartupReference{Sequence: 1, Epoch: f.owner.intent.Epoch, IntentSHA256: task.ComputeSHA256(intent)}
	assumption := f.owner.assumption
	assumption.Startup = reference
	if future {
		assumption.AssumedAt = "not a stop validity field"
	}
	data, err := task.MarshalCanonical(assumption)
	if err != nil {
		t.Fatal(err)
	}
	if future {
		data = stopExtra(data, `"future_assumption":{"not_a_birth_gate":true}`)
	}
	f.write(t, 0, intent)
	f.write(t, 1, data)
	publication := f.owner.publication
	publication.Startup, publication.AssumptionSHA256 = reference, task.ComputeSHA256(data)
	if future {
		publication.Protocol = 99
	}
	data, err = task.MarshalCanonical(publication)
	if err != nil {
		t.Fatal(err)
	}
	if future {
		data = stopExtra(data, `"future_publication":{"runtime":"uninterpreted"}`)
	}
	f.write(t, 2, data)
}

func (f *stopLocatorFixture) changeAssumption(t *testing.T, data []byte) {
	t.Helper()
	before := task.ComputeSHA256(f.records[1])
	f.write(t, 1, data)
	publication := bytes.Replace(f.records[2], []byte(before), []byte(task.ComputeSHA256(data)), 1)
	if bytes.Equal(publication, f.records[2]) {
		t.Fatal("fixture failed to preserve the raw assumption dependency")
	}
	f.write(t, 2, publication)
}

func stopJSONLiteral(t *testing.T, value string) string {
	t.Helper()
	encoded, err := task.MarshalCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSpace(encoded))
}

func stopRecordSnapshot(t *testing.T, reader *taskdir.ReadOnlyStore) map[string]string {
	t.Helper()
	// This fixture owns only journal data and empty lock files, never programs.
	directory := filepath.Join(reader.Root(), "infrastructure")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		info, statErr := entry.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("unexpected fixture journal entry: %v", statErr)
		}
		data, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		result[entry.Name()] = fmt.Sprintf("%s:%s", info.Mode(), data)
	}
	return result
}

func TestOwnerStopLocatorReadsFutureFieldsWithoutVersionedBirthValidation(t *testing.T) {
	f := newStopLocatorFixture(t, true)
	before := stopRecordSnapshot(t, f.reader)
	if _, _, err := readLatestIntent(t.Context(), f.reader); err == nil {
		t.Fatal("ordinary startup reader accepted unsupported future records")
	}
	locator, found, err := readOwnerStopLocator(t.Context(), f.reader)
	if err != nil || !found {
		t.Fatalf("stable stop locator reused versioned birth validation: found=%t error=%v", found, err)
	}
	if locator.reference.IntentSHA256 != task.ComputeSHA256(f.records[0]) || locator.publicationSHA256 != task.ComputeSHA256(f.records[2]) || locator.publication.AssumptionSHA256 != task.ComputeSHA256(f.records[1]) || locator.assumption.AssumedAt != "" || locator.publication.Protocol != 99 || locator.servingReleased {
		t.Fatal("stop locator reserialized future data or derived birth permission")
	}
	if !maps.Equal(before, stopRecordSnapshot(t, f.reader)) {
		t.Fatal("passive stop observation changed journal data")
	}
}

func TestOwnerStopLocatorReadsRetiredStarterWithoutRewriting(t *testing.T) {
	f := newStopLocatorFixture(t, false)
	if _, found, err := readLatestIntent(t.Context(), f.reader); err != nil || !found {
		t.Fatalf("current intent fixture is invalid: found=%t err=%v", found, err)
	}
	starter := fmt.Sprintf(`"starter":{"kind":"outside","root":%s,"root_id":"%s","pid":7,"start_token":"fixture:7"}`, stopJSONLiteral(t, f.reader.Root()), f.reader.RootID())
	f.rebind(t, stopExtra(f.records[0], starter), false)
	before := stopRecordSnapshot(t, f.reader)
	if _, _, err := readLatestIntent(t.Context(), f.reader); !errors.Is(err, task.ErrEvidenceFault) || task.RecordAbsent(err) {
		t.Fatalf("retired starter was accepted or treated as absent: %v", err)
	}
	locator, found, err := readOwnerStopLocator(t.Context(), f.reader)
	if err != nil || !found {
		t.Fatalf("stable stop locator rejected retired starter: found=%t err=%v", found, err)
	}
	if locator.reference.IntentSHA256 != task.ComputeSHA256(f.records[0]) || locator.publicationSHA256 != task.ComputeSHA256(f.records[2]) || locator.publication.AssumptionSHA256 != task.ComputeSHA256(f.records[1]) {
		t.Fatal("retired starter stop locator changed raw record bindings")
	}
	if !maps.Equal(before, stopRecordSnapshot(t, f.reader)) {
		t.Fatal("retired starter observation rewrote journal records")
	}
}

func TestOwnerStopLocatorAcceptsFrozenMinimalV3CommonFields(t *testing.T) {
	f := newStopLocatorFixture(t, false)
	pin := f.owner.journal.pin
	root := stopJSONLiteral(t, pin.Root())
	data := []byte(fmt.Sprintf(`{"protocol":3,"sequence":1,"epoch":"%s","inputs":{"root":%s,"root_id":"%s","root_device":%d,"root_inode":%d,"boot_id":"%s"}}`+"\n", f.owner.intent.Epoch, root, pin.RootID(), pin.Device(), pin.Inode(), f.owner.intent.Inputs.BootID))
	f.rebind(t, data, false)
	locator, found, err := readOwnerStopLocator(t.Context(), f.reader)
	if err != nil || !found || locator.reference.IntentSHA256 != task.ComputeSHA256(data) || locator.assumption.Process != f.owner.assumption.Process {
		t.Fatalf("minimal common fields became a full birth schema: found=%t error=%v", found, err)
	}
}

func TestOwnerStopLocatorRefusesMalformedPartialAndWrongSubjects(t *testing.T) {
	for _, kind := range []string{"root", "root inode", "boot", "sequence", "UID null", "UID alias", "UID changed", "process", "process token", "reference", "assumption digest", "endpoint", "device null", "socket", "truncated", "oversized", "missing assumption", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			f := newStopLocatorFixture(t, false)
			mutateStopLocatorRecord(t, f, kind)
			if _, _, err := readOwnerStopLocator(t.Context(), f.reader); err == nil {
				t.Fatal("invalid stop locator became an owner observation")
			}
		})
	}
}

func mutateStopLocatorRecord(t *testing.T, f *stopLocatorFixture, kind string) {
	t.Helper()
	type mutation struct {
		index         int
		before, after string
	}
	mutations := map[string]mutation{
		"root":              {0, f.reader.RootID(), strings.Repeat("f", 32)},
		"root inode":        {0, fmt.Sprintf(`"root_inode":%d`, f.owner.journal.pin.Inode()), `"root_inode":0`},
		"boot":              {0, f.owner.intent.Inputs.BootID, "unknown:boot"},
		"sequence":          {0, `"sequence":1`, `"sequence":2`},
		"UID null":          {1, fmt.Sprintf(`"uid":%d`, os.Geteuid()), `"uid":null`},
		"UID alias":         {1, `"uid"`, `"UID"`},
		"UID changed":       {1, fmt.Sprintf(`"uid":%d`, os.Geteuid()), fmt.Sprintf(`"uid":%d`, os.Geteuid()+1)},
		"process":           {1, fmt.Sprintf(`"pid":%d`, os.Getpid()), `"pid":0`},
		"process token":     {1, stopJSONLiteral(t, f.owner.assumption.Process.StartToken), stopJSONLiteral(t, "\x00")},
		"reference":         {1, f.owner.intent.Epoch, strings.Repeat("f", 32)},
		"assumption digest": {2, task.ComputeSHA256(f.records[1]), strings.Repeat("0", 64)},
		"endpoint":          {2, stopJSONLiteral(t, ownerEndpointPath(f.reader.Root(), 1)), stopJSONLiteral(t, ownerEndpointPath(f.reader.Root(), 2))},
		"device null":       {2, `"socket_device":0`, `"socket_device":null`},
		"socket":            {2, `"socket_inode":24`, `"socket_inode":0`},
	}
	if change, present := mutations[kind]; present {
		changed := bytes.Replace(f.records[change.index], []byte(change.before), []byte(change.after), 1)
		if bytes.Equal(changed, f.records[change.index]) {
			t.Fatal("record mutation did not change its named source fact")
		}
		if change.index == 1 {
			f.changeAssumption(t, changed)
		} else {
			f.write(t, change.index, changed)
		}
		return
	}
	switch kind {
	case "truncated":
		f.write(t, 0, []byte("{"))
		return
	case "oversized":
		f.write(t, 0, bytes.Repeat([]byte(" "), task.MaxControlRecordSize+1))
		return
	case "missing assumption":
		if err := os.Remove(filepath.Join(f.reader.Root(), "infrastructure", assumptionName(1))); err != nil {
			t.Fatal(err)
		}
		return
	case "duplicate":
		f.changeAssumption(t, stopExtra(f.records[1], fmt.Sprintf(`"UID":%d`, os.Geteuid())))
		return
	}
	t.Fatalf("unknown record mutation %q", kind)
}

func TestOwnerStopLocatorRechecksCapturedRecordsAndSelectedHead(t *testing.T) {
	for _, replacement := range []string{"publication", "new head", "empty fence"} {
		t.Run(replacement, func(t *testing.T) {
			f := newStopLocatorFixture(t, false)
			locator, _, err := readOwnerStopLocator(t.Context(), f.reader)
			if err != nil {
				t.Fatal(err)
			}
			records := f.records
			switch replacement {
			case "new head":
				next := bytes.Replace(f.records[0], []byte(`"sequence":1`), []byte(`"sequence":2`), 1)
				err = os.WriteFile(filepath.Join(f.reader.Root(), "infrastructure", intentName(2)), next, 0o600)
			case "empty fence":
				f.write(t, 3, nil)
			default:
				f.write(t, 2, stopExtra(f.records[2], `"replacement":true`))
			}
			if err != nil {
				t.Fatal(err)
			}
			names := [4]string{intentName(1), assumptionName(1), publicationName(1), settlementName(1)}
			if err = verifyOwnerStopSnapshot(t.Context(), f.reader, locator, names, records); err == nil {
				t.Fatal("captured stop subject silently followed changed journal data")
			}
		})
	}
}

func TestOwnerStopLocatorAbsenceAndCancellationGrantNothing(t *testing.T) {
	store := journalStore(t)
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, found, readErr := readOwnerStopLocator(t.Context(), reader); readErr != nil || found {
		t.Fatalf("missing intent became owner/native closure evidence: found=%t error=%v", found, readErr)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, readErr := readOwnerStopLocator(ctx, reader); !errors.Is(readErr, context.Canceled) {
		t.Fatalf("canceled passive read lost its cause: %v", readErr)
	}
	// Exercise the public boundary's invalid-context rejection deliberately.
	var invalidContext context.Context
	if _, _, readErr := readOwnerStopLocator(invalidContext, reader); !errors.Is(readErr, os.ErrInvalid) {
		t.Fatal("nil context was accepted")
	}
}

func otherStopBoot(boot string) string {
	last := "0"
	if strings.HasSuffix(boot, last) {
		last = "1"
	}
	return boot[:len(boot)-1] + last
}

func TestOwnerStopLocatorPreservesComparablePreviousBootData(t *testing.T) {
	f := newStopLocatorFixture(t, false)
	boot := otherStopBoot(f.owner.intent.Inputs.BootID)
	f.rebind(t, bytes.Replace(f.records[0], []byte(f.owner.intent.Inputs.BootID), []byte(boot), 1), false)
	locator, found, err := readOwnerStopLocator(t.Context(), f.reader)
	if err != nil || !found || locator.bootID != boot || lifetime.CompareBootIDs(locator.bootID, f.owner.intent.Inputs.BootID) != lifetime.BootDifferent {
		t.Fatalf("recorded previous boot became absence or current PID authority: found=%t error=%v", found, err)
	}
}

func TestOwnerStopLocatorRequiresExactBoundServingReleaseFence(t *testing.T) {
	for _, kind := range []string{"serving", "assumed absent", "previous boot", "unknown reason", "unassumed", "wrong boot", "wrong reference", "wrong assumption", "wrong publication", "invalid time", "truncated", "zero byte"} {
		t.Run(kind, func(t *testing.T) {
			f := newStopLocatorFixture(t, true)
			locator, _, err := readOwnerStopLocator(t.Context(), f.reader)
			if err != nil {
				t.Fatal(err)
			}
			record := startupSettlement{Protocol: 99, Startup: locator.reference, AssumptionSHA256: locator.publication.AssumptionSHA256, PublicationSHA256: locator.publicationSHA256, ObservedBootID: locator.bootID, SettledAt: "2026-11-05T00:00:00Z", Reason: settlementServingReleased}
			valid, released := configureStopFence(kind, &record)
			data, err := task.MarshalCanonical(record)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "truncated":
				data = []byte("{")
			case "zero byte":
				data = nil
			}
			f.write(t, 3, data)
			locator, found, readErr := readOwnerStopLocator(t.Context(), f.reader)
			if valid && (readErr != nil || !found || locator.servingReleased != released) || !valid && readErr == nil {
				t.Fatalf("fence escaped its exact common subject: found=%t released=%t error=%v", found, locator.servingReleased, readErr)
			}
		})
	}
}

func configureStopFence(kind string, record *startupSettlement) (valid, released bool) {
	switch kind {
	case "serving":
		return true, true
	case "assumed absent":
		record.Reason = settlementAssumedAbsent
		return true, false
	case "previous boot":
		record.Reason, record.ObservedBootID = settlementPreviousBoot, otherStopBoot(record.ObservedBootID)
		return true, false
	case "unknown reason":
		record.Reason = "future-semantic-reason"
	case "unassumed":
		record.Reason = settlementUnassumedExpired
	case "wrong boot":
		record.ObservedBootID = otherStopBoot(record.ObservedBootID)
	case "wrong reference":
		record.Startup.Epoch = strings.Repeat("f", 32)
	case "wrong assumption":
		record.AssumptionSHA256 = strings.Repeat("0", 64)
	case "wrong publication":
		record.PublicationSHA256 = strings.Repeat("0", 64)
	case "invalid time":
		record.SettledAt = "not-a-timestamp"
	}
	return false, false
}

func TestOwnerStopLocatorRefusesReplacedRoot(t *testing.T) {
	f := newStopLocatorFixture(t, false)
	root, displaced := f.reader.Root(), f.reader.Root()+"-displaced"
	moved, replaced := false, false
	t.Cleanup(func() {
		if replaced {
			if err := os.Remove(root); err != nil {
				t.Error(err)
			}
		}
		if moved {
			if err := os.Rename(displaced, root); err != nil {
				t.Error(err)
			}
		}
	})
	if err := os.Rename(root, displaced); err != nil {
		t.Fatal(err)
	}
	moved = true
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	replaced = true
	if _, _, err := readOwnerStopLocator(t.Context(), f.reader); err == nil {
		t.Fatal("replacement root inherited the original pinned owner")
	}
}
