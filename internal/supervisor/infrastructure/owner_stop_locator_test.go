package infrastructure

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestOwnerStopSnapshotAcceptsSameOwnerServingReleasePublishedAfterCapture(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{}, ownerServerFixtureSelection{
		artifacts: Artifacts{Runner: filepath.Join(parent, "missing-installed-runner")},
	})
	t.Logf("owned actual-server fixture root: %s", f.root)
	reader := contextPassiveReader(t, f.entry.journal)
	if _, err = f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	snapshot, err := readOwnerStopSnapshot(ctx, reader, f.entry.journal.pin, f.entry.operation.Intent().Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.records[3]) != 0 || snapshot.locator.servingReleased {
		t.Fatal("fixture already had a serving-release settlement before capture")
	}
	stopAndReleaseOwnerServerFixture(t, ctx, f)
	requireOwnerStopReleaseEvidence(t, ctx, reader, snapshot)
	snapshotErr := verifySelectedOwnerStopSnapshot(ctx, reader, snapshot.locator, snapshot.names, snapshot.records)
	current, err := readOwnerStopSequence(ctx, reader, snapshot.locator.pin, snapshot.locator.reference.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	current, err = matchSelectedOwnerStopLocator(ctx, current, snapshot.locator)
	if err != nil || !current.servingReleased {
		t.Fatalf("fresh same-generation observation rejected the serving release: %v", err)
	}
	result, err := StopOwnerServing(ctx, reader)
	expected := OwnerStopResult{
		State: OwnerServingReleased, ProcessClosure: OwnerProcessPending,
		Startup: snapshot.locator.reference, Process: snapshot.locator.assumption.Process,
	}
	if err != nil || result != expected {
		t.Fatalf("fresh public stop changed the released live owner: got=%+v want=%+v error=%v", result, expected, err)
	}
	t.Logf("actual server release preserved root and first three records; fresh public stop=%s/%s; captured snapshot error=%v", result.State, result.ProcessClosure, snapshotErr)
	if snapshotErr != nil {
		t.Errorf("lawful same-owner serving release published after capture was rejected: %v", snapshotErr)
	}
}

func TestOwnerStopSnapshotRejectsLateSettlementWithoutBoundServingRelease(t *testing.T) {
	for _, kind := range []string{"truncated", "zero byte", "wrong reference", "wrong assumption", "wrong publication", "wrong boot", "assumed absent", "previous boot"} {
		t.Run(kind, func(t *testing.T) {
			f := newStopLocatorFixture(t, true)
			snapshot, err := readOwnerStopSnapshot(t.Context(), f.reader, f.owner.journal.pin, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.records[3]) != 0 || snapshot.locator.servingReleased {
				t.Fatal("fixture already had a settlement before capture")
			}
			locator := snapshot.locator
			data, valid := nonServingStopFenceBytes(t, locator, kind)
			f.write(t, 3, data)
			if err = verifySelectedOwnerStopSnapshot(t.Context(), f.reader, locator, snapshot.names, snapshot.records); !errors.Is(err, ErrIdentity) {
				t.Fatalf("late settlement without the exact serving-release fence was accepted: %v", err)
			}
			if valid {
				current, readErr := readOwnerStopSequence(t.Context(), f.reader, locator.pin, locator.reference.Sequence)
				if readErr != nil || current.servingReleased {
					t.Fatalf("valid non-serving settlement changed its existing passive meaning: %v", readErr)
				}
			}
		})
	}
}

func stopAndReleaseOwnerServerFixture(t *testing.T, ctx context.Context, f *ownerServerFixture) {
	t.Helper()
	// The canonical fixture has already registered cleanup of its server,
	// served goroutine, entry, store and owned root before this launch.
	f.startContext(ctx)
	deadline, _ := ctx.Deadline()
	subject := f.requestContext(t, ctx, ownerStatus)
	request := ownerStopRequest{
		StopProtocol: ownerStopProtocolVersion, RootID: subject.RootID, Owner: subject.Owner,
		PublicationSHA256: subject.PublicationSHA256, Client: subject.Client,
		Deadline: deadline.UTC().Format(time.RFC3339Nano),
	}
	connection := f.dial(t)
	if err := connection.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	reply, err := readOwnerFrame[ownerStopReply](bufio.NewReader(connection))
	if err != nil {
		t.Fatal(err)
	}
	if expected := ownerStopFixtureReply(request, ownerStopAccepted); reply != expected {
		t.Fatalf("actual server stop changed its authenticated subject: got=%+v want=%+v", reply, expected)
	}
	select {
	case <-f.server.drained:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if serverErr := f.server.fault(); serverErr != nil {
		t.Fatal(serverErr)
	}
	released, err := f.entry.releaseServingContext(ctx)
	if err != nil || !released {
		t.Fatalf("actual owner serving release: released=%t error=%v", released, err)
	}
	// The actual server is drained and its listener is now closed. Leave its
	// served channel to the canonical cleanup without stopping the listener twice.
	f.server = nil
}

func nonServingStopFenceBytes(t *testing.T, locator ownerStopLocator, kind string) ([]byte, bool) {
	t.Helper()
	record := startupSettlement{
		Protocol: RecordProtocolVersion, Startup: locator.reference,
		AssumptionSHA256:  locator.publication.AssumptionSHA256,
		PublicationSHA256: locator.publicationSHA256, ObservedBootID: locator.bootID,
		SettledAt: "2026-11-05T00:00:00Z", Reason: settlementServingReleased,
	}
	valid, released := configureStopFence(kind, &record)
	if released {
		t.Fatal("negative fixture unexpectedly selected a serving-release reason")
	}
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
	return data, valid
}

func requireOwnerStopReleaseEvidence(t *testing.T, ctx context.Context, reader *taskdir.ReadOnlyStore, snapshot ownerStopSnapshot) {
	t.Helper()
	for index := range 3 {
		current, readErr := reader.ReadInfrastructureRecordContext(ctx, snapshot.names[index])
		if readErr != nil || !bytes.Equal(current, snapshot.records[index]) {
			t.Fatalf("serving release changed immutable record %d: %v", index, readErr)
		}
	}
	pin, err := reader.PinRootContext(ctx)
	if err != nil || !pin.Equal(snapshot.locator.pin) {
		t.Fatalf("serving release changed the selected root: %v", err)
	}
	settlement, err := reader.ReadInfrastructureRecordContext(ctx, snapshot.names[3])
	if err != nil {
		t.Fatal(err)
	}
	fenced, err := ownerStopReleased(settlement, snapshot.locator)
	if err != nil || !fenced {
		t.Fatalf("new settlement is not the selected owner's bound release: fenced=%t error=%v", fenced, err)
	}
}

func TestOwnerStopSnapshotInterruptedReadDoesNotInventIdentity(t *testing.T) {
	fixture := newStopLocatorFixture(t, false)
	snapshot, err := readOwnerStopSnapshot(t.Context(), fixture.reader, fixture.owner.journal.pin, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, deadline := range []bool{false, true} {
		name := "canceled"
		ctx, cancel := context.WithCancel(t.Context())
		if deadline {
			cancel()
			name = "deadline"
			ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		}
		cancel()
		t.Run(name, func(t *testing.T) {
			for _, verify := range []struct {
				name string
				call func() error
			}{
				{"selected", func() error {
					return verifySelectedOwnerStopSnapshot(ctx, fixture.reader, snapshot.locator, snapshot.names, snapshot.records)
				}},
				{"latest", func() error {
					return verifyOwnerStopSnapshot(ctx, fixture.reader, snapshot.locator, snapshot.names, snapshot.records)
				}},
			} {
				cause := verify.call()
				if !errors.Is(cause, ctx.Err()) || errors.Is(cause, ErrIdentity) {
					t.Errorf("%s interrupted snapshot invented identity: %v", verify.name, cause)
				}
			}
		})
	}
}

func TestOwnerStopSnapshotReadErrorKeepsConcreteFaults(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cause    error
		identity bool
	}{
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"evidence cancellation", errors.Join(task.ErrEvidenceFault, context.Canceled), false},
		{"identity cancellation", errors.Join(ErrIdentity, context.Canceled), true},
		{"permission cancellation", errors.Join(os.ErrPermission, context.Canceled), true},
		{"permission", os.ErrPermission, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ownerStopSnapshotReadError(tc.cause)
			if errors.Is(got, ErrIdentity) != tc.identity || !errors.Is(got, tc.cause) {
				t.Fatalf("snapshot read classification lost concrete cause: %v", got)
			}
		})
	}
}
