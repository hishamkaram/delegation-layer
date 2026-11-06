package infrastructure

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestContextChunksRoundTripEscapedAndNonUTF8Payload(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	record.ControlEnvironment = []string{"VALUE=" + strings.Repeat("\x01", contextChunkBytes+27), "RAW=\xff\xfe"}
	record.MaintenanceEnvironment = []string{"PATH=selected\xff"}
	reference, created, err := journal.stageContext(t.Context(), record)
	if err != nil || !created {
		t.Fatalf("stage: %v %v", created, err)
	}
	manifest, chunks, err := encodeLaunchContext(record)
	if err != nil || len(chunks) < 2 {
		t.Fatalf("multi-chunk fixture: %v", err)
	}
	if manifest.Head == "" || contextChunkCount(int(manifest.ControlBytes+manifest.MaintenanceBytes)) != len(chunks) {
		t.Fatal("chunk identities missing")
	}
	restored, err := journal.readContext(t.Context(), reference)
	if err != nil || !slices.Equal(record.ControlEnvironment, restored.ControlEnvironment) || !slices.Equal(record.MaintenanceEnvironment, restored.MaintenanceEnvironment) {
		t.Fatalf("lossless active roundtrip: %v", err)
	}
	reader := contextPassiveReader(t, journal)
	restored, err = readLaunchContext(t.Context(), reader, reference)
	if err != nil || !slices.Equal(record.ControlEnvironment, restored.ControlEnvironment) || !slices.Equal(record.MaintenanceEnvironment, restored.MaintenanceEnvironment) {
		t.Fatalf("lossless passive roundtrip: %v", err)
	}
}

func contextPassiveReader(t *testing.T, journal *intentJournal) *taskdir.ReadOnlyStore {
	t.Helper()
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), journal.pin.Root(), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return reader
}

func TestContextChunksEmptyRoles(t *testing.T) {
	record := launchContextFixture(t)
	record.ControlEnvironment, record.MaintenanceEnvironment = []string{}, []string{}
	manifest, chunks, err := encodeLaunchContext(record)
	if err != nil || manifest.Head != "" || len(chunks) != 0 {
		t.Fatalf("empty manifest: %v", err)
	}
	restored, err := decodeContextPayload(manifest, []byte{})
	if err != nil || restored.ControlEnvironment == nil || restored.MaintenanceEnvironment == nil {
		t.Fatalf("empty roles changed: %v", err)
	}
}

func TestContextChunksStorageAboveFormerRoleCap(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	record.ControlEnvironment = []string{"K=" + strings.Repeat("x", 20*1024*1024)}
	reference, created, err := journal.stageContext(t.Context(), record)
	if err != nil || !created {
		t.Fatalf("storage-only large role refused: %v %v", created, err)
	}
	restored, err := journal.readContext(t.Context(), reference)
	if err != nil || !slices.Equal(restored.ControlEnvironment, record.ControlEnvironment) {
		t.Fatalf("large storage roundtrip: %v", err)
	}
	requirePassiveContextSnapshot(t, journal, reference, record)
}

func writeChunkedContextFixture(t *testing.T, journal *intentJournal, manifest contextManifest, chunks []contextChunk) LaunchContextReference {
	t.Helper()
	for _, chunk := range chunks {
		digest, err := digestStartupRecord(chunk)
		if err != nil {
			t.Fatal(err)
		}
		name, err := contextChunkName(digest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = publishContextRecord(t.Context(), journal, name, chunk); err != nil {
			t.Fatal(err)
		}
	}
	digest, err := digestStartupRecord(manifest)
	if err != nil {
		t.Fatal(err)
	}
	reference := LaunchContextReference{SHA256: digest}
	name, err := contextName(reference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishContextRecord(t.Context(), journal, name, manifest); err != nil {
		t.Fatal(err)
	}
	return reference
}

func TestContextChunkShapeAndIdentityRefusals(t *testing.T) {
	for _, kind := range []string{"swapped", "missing", "truncated", "extra", "short-middle", "oversize", "protocol", "wrong-next", "missing-link", "extra-link", "early-tail"} {
		t.Run(kind, func(t *testing.T) { checkContextChunkRefusal(t, kind) })
	}
}

func checkContextChunkRefusal(t *testing.T, kind string) {
	t.Helper()
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	record.ControlEnvironment = []string{"K=" + strings.Repeat("a", contextChunkBytes) + strings.Repeat("b", contextChunkBytes) + "tail"}
	manifest, chunks, err := encodeLaunchContext(record)
	if err != nil {
		t.Fatal(err)
	}
	prepareChunkShapeFixture(t, kind, &manifest, chunks)
	reference := writeChunkedContextFixture(t, journal, manifest, chunks)
	damageChunkRecordFixture(t, journal, kind, chunks)

	requireContextBothReadersRefuse(t, journal, reference, "")
}

func requireContextBothReadersRefuse(t *testing.T, journal *intentJournal, reference LaunchContextReference, secret string) {
	t.Helper()
	_, activeErr := journal.readContext(t.Context(), reference)
	_, passiveErr := readLaunchContext(t.Context(), contextPassiveReader(t, journal), reference)
	for _, err := range []error{activeErr, passiveErr} {
		if err == nil || !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("invalid context accepted/lost evidence cause: %v", err)
		}
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatal("context diagnostics rendered payload")
		}
	}
}

func TestContextChunkMalformedDecodeRedactsBothReaders(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	reference, _, err := journal.stageContext(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := encodeLaunchContext(record)
	if err != nil {
		t.Fatal(err)
	}
	name, err := contextChunkName(manifest.Head)
	if err != nil {
		t.Fatal(err)
	}
	secret := "cHJpdmF0ZS1wYXlsb2Fk"
	if err = os.WriteFile(filepath.Join(journal.pin.Root(), "infrastructure", name), []byte(`{"protocol":1,"data":"`+secret+`","`+secret+`":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	requireContextBothReadersRefuse(t, journal, reference, secret)
}

func TestContextPayloadDuplicateKeyErrorsAreRedacted(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	manifest, _, err := encodeLaunchContext(record)
	if err != nil {
		t.Fatal(err)
	}
	secret := "private-duplicate-key"
	payload := []byte(secret + "=a\x00" + secret + "=b\x00")
	chunk := contextChunk{Protocol: RecordProtocolVersion, Data: payload}
	digest, err := digestStartupRecord(chunk)
	if err != nil {
		t.Fatal(err)
	}
	manifest.ControlBytes, manifest.MaintenanceBytes = int64(len(payload)), 0
	manifest.Head = digest
	reference := writeChunkedContextFixture(t, journal, manifest, []contextChunk{chunk})
	requireContextBothReadersRefuse(t, journal, reference, secret)
}

type interruptContextPublish struct {
	taskdir.BaseFaultInjector
	cancel    context.CancelFunc
	published string
}

func (f *interruptContextPublish) OnAfterLinkDirBarrier(path string) error {
	f.published = path
	f.cancel()
	return nil
}

func TestContextInterruptedStagePublishesNoManifest(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	record.ControlEnvironment = []string{"K=" + strings.Repeat("x", contextChunkBytes+4)}
	digest, err := record.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fault := &interruptContextPublish{cancel: cancel}
	journal.store.SetFaultInjector(fault)
	if _, created, stageErr := journal.stageContext(ctx, record); created || !errors.Is(stageErr, context.Canceled) {
		t.Fatalf("interrupted stage: %v %v", created, stageErr)
	}
	manifest, chunks, encodeErr := encodeLaunchContext(record)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	tailDigest, digestErr := digestStartupRecord(chunks[len(chunks)-1])
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	tailName, nameErr := contextChunkName(tailDigest)
	if nameErr != nil {
		t.Fatal(nameErr)
	}
	if filepath.Base(fault.published) != tailName || tailDigest == manifest.Head {
		t.Fatal("interrupted publisher did not publish the tail first")
	}
	name, err := contextName(LaunchContextReference{SHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(journal.pin.Root(), "infrastructure", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial stage published manifest: %v", err)
	}
}

func TestIntentRequiresCompleteBoundContext(t *testing.T) {
	for _, kind := range []string{"missing", "foreign", "incomplete"} {
		t.Run(kind, func(t *testing.T) { checkIntentContextIntegrity(t, kind) })
	}
}

func checkIntentContextIntegrity(t *testing.T, kind string) {
	t.Helper()
	journal, lease := journalFixture(t)
	intent := journalIntent(t, journal, 1)
	record := contextForJournal(t, journal)
	if kind == "foreign" {
		record.RootID = originTestRootID
	}
	manifest, chunks, err := encodeLaunchContext(record)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "incomplete" {
		record.ControlEnvironment = []string{"K=different"}
		manifest, chunks, err = encodeLaunchContext(record)
		if err != nil {
			t.Fatal(err)
		}
	}
	intent.Inputs.Context = invalidContextReferenceFixture(t, journal, kind, manifest, chunks)
	refreshIntentDigest(t, &intent)
	if created, putErr := journal.putIntent(t.Context(), lease, intent); created || putErr == nil {
		t.Fatalf("invalid %s context accepted: %v %v", kind, created, putErr)
	}
	if _, err = journal.readIntent(t.Context(), 1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid context created intent: %v", err)
	}
}

func prepareChunkShapeFixture(t *testing.T, kind string, manifest *contextManifest, chunks []contextChunk) {
	t.Helper()
	switch kind {
	case "short-middle":
		last := chunks[1].Data[len(chunks[1].Data)-1]
		chunks[1].Data = chunks[1].Data[:len(chunks[1].Data)-1]
		chunks[2].Data = append([]byte{last}, chunks[2].Data...)
	case "extra":
		chunks[2].Data = append(slices.Clone(chunks[2].Data), 'x')
	case "protocol":
		chunks[1].Protocol++
	}
	manifest.Head = linkContextFixture(t, chunks)
	switch kind {
	case "swapped":
		manifest.Head = chunks[0].Next
	case "wrong-next":
		chunks[0].Next = "invalid"
	case "missing-link":
		chunks[0].Next = task.ComputeSHA256([]byte("missing link"))
	case "extra-link":
		chunks[2].Next = manifest.Head
		manifest.Head = relinkContextFixturePrefix(t, chunks, 2)
	case "early-tail":
		chunks[1].Next = ""
		manifest.Head = relinkContextFixturePrefix(t, chunks, 1)
	}
	if kind == "wrong-next" || kind == "missing-link" {
		digest, err := digestStartupRecord(chunks[0])
		if err != nil {
			t.Fatal(err)
		}
		manifest.Head = digest
	}
}

func linkContextFixture(t *testing.T, chunks []contextChunk) string {
	t.Helper()
	if len(chunks) == 0 {
		return ""
	}
	chunks[len(chunks)-1].Next = ""
	return relinkContextFixturePrefix(t, chunks, len(chunks)-1)
}

func relinkContextFixturePrefix(t *testing.T, chunks []contextChunk, last int) string {
	t.Helper()
	digest, err := digestStartupRecord(chunks[last])
	if err != nil {
		t.Fatal(err)
	}
	for index := last - 1; index >= 0; index-- {
		chunks[index].Next = digest
		digest, err = digestStartupRecord(chunks[index])
		if err != nil {
			t.Fatal(err)
		}
	}
	return digest
}

func damageChunkRecordFixture(t *testing.T, journal *intentJournal, kind string, chunks []contextChunk) {
	t.Helper()
	digest, err := digestStartupRecord(chunks[1])
	if err != nil {
		t.Fatal(err)
	}
	name, err := contextChunkName(digest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(journal.pin.Root(), "infrastructure", name)
	switch kind {
	case "missing":
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	case "oversize":
		if err = os.WriteFile(path, bytes.Repeat([]byte{'x'}, task.MaxControlRecordSize+1), 0o600); err != nil {
			t.Fatal(err)
		}
	case "truncated":
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err = os.WriteFile(path, data[:len(data)-3], 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContextChunkPrefixCollisionPreservesWinner(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	manifest, _, err := encodeLaunchContext(record)
	if err != nil {
		t.Fatal(err)
	}
	name, err := contextChunkName(manifest.Head)
	if err != nil {
		t.Fatal(err)
	}
	winner := contextChunk{Protocol: RecordProtocolVersion, Data: []byte("different winner")}
	if _, err = publishContextRecord(t.Context(), journal, name, winner); err != nil {
		t.Fatal(err)
	}
	before, err := task.MarshalCanonical(winner)
	if err != nil {
		t.Fatal(err)
	}
	reference, created, err := journal.stageContext(t.Context(), record)
	if created || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("prefix collision accepted: %v %v", created, err)
	}
	after, err := os.ReadFile(filepath.Join(journal.pin.Root(), "infrastructure", name))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("collision winner replaced: %v", err)
	}
	manifestName, err := contextName(reference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(journal.pin.Root(), "infrastructure", manifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collision published manifest: %v", err)
	}
}

func TestContextCancellationAndClosedRefusal(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	reference, _, err := journal.stageContext(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	reader := contextPassiveReader(t, journal)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err = journal.stageContext(ctx, record); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stage: %v", err)
	}
	if _, err = journal.readContext(ctx, reference); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled active read: %v", err)
	}
	if _, err = readLaunchContext(ctx, reader, reference); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled passive read: %v", err)
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = journal.stageContext(t.Context(), record); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed stage: %v", err)
	}
	if _, err = journal.readContext(t.Context(), reference); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed read: %v", err)
	}
}

func TestContextZeroChunkManifestRoundTripsStorage(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	record.ControlEnvironment, record.MaintenanceEnvironment = []string{}, []string{}
	reference, _, err := journal.stageContext(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := journal.readContext(t.Context(), reference)
	if err != nil || restored.ControlEnvironment == nil || restored.MaintenanceEnvironment == nil || len(restored.ControlEnvironment) != 0 || len(restored.MaintenanceEnvironment) != 0 {
		t.Fatalf("zero-chunk active read: %v", err)
	}
	restored, err = readLaunchContext(t.Context(), contextPassiveReader(t, journal), reference)
	if err != nil || restored.ControlEnvironment == nil || restored.MaintenanceEnvironment == nil {
		t.Fatalf("zero-chunk passive read: %v", err)
	}
}

func TestContextPayloadRequiresExactRoleTerminators(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	record.ControlEnvironment, record.MaintenanceEnvironment = []string{"X=1"}, []string{"Y=2"}
	manifest, chunks, err := encodeLaunchContext(record)
	if err != nil {
		t.Fatal(err)
	}
	// The total and all chunk identities remain valid; only the declared role
	// boundary is moved inside an entry, which must not become another scope.
	manifest.ControlBytes--
	manifest.MaintenanceBytes++
	reference := writeChunkedContextFixture(t, journal, manifest, chunks)
	requireContextBothReadersRefuse(t, journal, reference, "")
}

type replaceRootDuringContextPublish struct {
	taskdir.BaseFaultInjector
	root  string
	moved bool
}

func (f *replaceRootDuringContextPublish) OnAfterLinkDirBarrier(string) error {
	if f.moved {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(f.root, "root.json"))
	if err != nil {
		return err
	}
	if err = os.Rename(f.root, f.root+"-retained"); err != nil {
		return err
	}
	f.moved = true
	if err = os.Mkdir(f.root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(f.root, "root.json"), data, 0o600)
}

func TestContextStageRechecksRootDuringPublication(t *testing.T) {
	journal, _ := journalFixture(t)
	record := contextForJournal(t, journal)
	root := journal.pin.Root()
	fault := &replaceRootDuringContextPublish{root: root}
	journal.store.SetFaultInjector(fault)
	t.Cleanup(func() {
		if !fault.moved {
			return
		}
		if cleanupErr := os.RemoveAll(root); cleanupErr != nil {
			t.Error(cleanupErr)
		}
		if cleanupErr := os.Rename(root+"-retained", root); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	reference, created, err := journal.stageContext(t.Context(), record)
	if created || !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("root changed during chunk publication: %v %v", created, err)
	}
	name, err := contextName(reference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(root+"-retained", "infrastructure", name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root replacement published manifest: %v", err)
	}
}

func invalidContextReferenceFixture(t *testing.T, journal *intentJournal, kind string, manifest contextManifest, chunks []contextChunk) LaunchContextReference {
	t.Helper()
	if kind == "missing" {
		return LaunchContextReference{SHA256: task.ComputeSHA256([]byte("missing"))}
	}
	reference := writeChunkedContextFixture(t, journal, manifest, chunks)
	if kind == "incomplete" {
		name, err := contextChunkName(manifest.Head)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Remove(filepath.Join(journal.pin.Root(), "infrastructure", name)); err != nil {
			t.Fatal(err)
		}
	}
	return reference
}

func TestContextManifestValidatesArithmeticAndHead(t *testing.T) {
	record := launchContextFixture(t)
	manifest, _, err := encodeLaunchContext(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"negative-control", "negative-maintenance", "overflow", "missing-head", "bad-head", "empty-with-head"} {
		changed := manifest
		switch kind {
		case "negative-control":
			changed.ControlBytes = -1
		case "negative-maintenance":
			changed.MaintenanceBytes = -1
		case "overflow":
			changed.ControlBytes = 1
			changed.MaintenanceBytes = 1<<63 - 1
		case "missing-head":
			changed.Head = ""
		case "bad-head":
			changed.Head = "bad"
		case "empty-with-head":
			changed.ControlBytes = 0
			changed.MaintenanceBytes = 0
		}
		if validateErr := changed.validate(); !errors.Is(validateErr, ErrIdentity) {
			t.Fatalf("invalid %s manifest accepted: %v", kind, validateErr)
		}
	}
	maximum := int(^uint(0) >> 1)
	expected := maximum / contextChunkBytes
	if maximum%contextChunkBytes != 0 {
		expected++
	}
	for _, pair := range [][2]int{{0, 0}, {1, 1}, {contextChunkBytes, 1}, {contextChunkBytes + 1, 2}, {maximum, expected}} {
		if contextChunkCount(pair[0]) != pair[1] {
			t.Fatalf("chunk count for %d: want %d", pair[0], pair[1])
		}
	}
}

type manifestOnlyContextReader struct {
	record     contextManifest
	chunkReads int
}

func (r *manifestOnlyContextReader) manifest(context.Context, string) (contextManifest, error) {
	return r.record, nil
}

func (r *manifestOnlyContextReader) chunk(context.Context, string) (contextChunk, error) {
	r.chunkReads++
	return contextChunk{}, ErrIdentity
}

func TestContextManifestInvalidCountsRefuseBeforeChunkIO(t *testing.T) {
	journal, _ := journalFixture(t)
	manifest, _, err := encodeLaunchContext(contextForJournal(t, journal))
	if err != nil {
		t.Fatal(err)
	}
	for _, counts := range [][2]int64{{-1, 0}, {0, -1}, {1<<63 - 1, 1}, {1, 1<<63 - 1}} {
		changed := manifest
		changed.ControlBytes, changed.MaintenanceBytes = counts[0], counts[1]
		digest, digestErr := digestStartupRecord(changed)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		reader := &manifestOnlyContextReader{record: changed}
		_, readErr := readContextRecords(t.Context(), reader, LaunchContextReference{SHA256: digest}, journal.pin)
		if !errors.Is(readErr, ErrIdentity) || reader.chunkReads != 0 {
			t.Fatalf("invalid counts reached chunk I/O: reads=%d error=%v", reader.chunkReads, readErr)
		}
	}
	maximum := int64(int(^uint(0) >> 1))
	manifest.ControlBytes, manifest.MaintenanceBytes = maximum, 0
	if err = manifest.validate(); err != nil {
		t.Fatalf("native integer representable count refused: %v", err)
	}
}
