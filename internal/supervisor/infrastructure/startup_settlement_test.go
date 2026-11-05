package infrastructure

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func settlementIntent(t *testing.T, j *intentJournal, expired bool) StartupIntent {
	t.Helper()
	intent := journalIntent(t, j, 1)
	now := time.Now().UTC()
	intent.AcceptedAt = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	intent.Deadline = now.Add(time.Hour).Format(time.RFC3339Nano)
	if expired {
		intent.Deadline = now.Add(-time.Hour).Format(time.RFC3339Nano)
	}
	return intent
}

func settlementReference(t *testing.T, intent StartupIntent) StartupReference {
	t.Helper()
	reference, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

func TestStartupSettlementExpiredUnassumedIsCreateOnce(t *testing.T) {
	j, lease := journalFixture(t)
	intent := settlementIntent(t, j, true)
	requireJournalPut(t, j, lease, intent)
	reference := settlementReference(t, intent)
	record, created, err := j.settleStartup(t.Context(), lease, reference)
	if err != nil || !created || record.Reason != settlementUnassumedExpired {
		t.Fatalf("settlement created=%t reason=%s err=%v", created, record.Reason, err)
	}
	replay, recreated, err := j.settleStartup(t.Context(), lease, reference)
	if err != nil || recreated || replay != record {
		t.Fatalf("retry changed settlement: created=%t err=%v", recreated, err)
	}
	stored, err := j.readSettlement(t.Context(), reference)
	if err != nil || stored != record {
		t.Fatalf("history: %v", err)
	}
}

func TestStartupSettlementRefusesUnexpiredOrLiveAssumption(t *testing.T) {
	for _, assumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unassumed", true: "assumed live"}[assumed], func(t *testing.T) {
			j, lease := journalFixture(t)
			intent := settlementIntent(t, j, false)
			requireJournalPut(t, j, lease, intent)
			if assumed {
				assumption, _ := evidenceForIntent(t, intent)
				if _, err := j.putAssumption(t.Context(), lease, assumption); err != nil {
					t.Fatal(err)
				}
			}
			_, created, err := j.settleStartup(t.Context(), lease, settlementReference(t, intent))
			if err == nil || created {
				t.Fatalf("unsafe settlement created=%t err=%v", created, err)
			}
			requireNoSettlement(t, j, intent.Sequence)
		})
	}
}

// This finite native child is joined before any store/fixture cleanup. Its
// recorded incarnation, not an invented PID or token, supplies absent evidence.
func TestStartupSettlementAssumedNativeChildAbsent(t *testing.T) {
	requireComponentEntryTestHost(t)
	j, lease := journalFixture(t)
	intent := settlementIntent(t, j, true)
	requireJournalPut(t, j, lease, intent)
	childPath, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(childPath)
	waited := false
	t.Cleanup(func() {
		if !waited {
			if waitErr := command.Wait(); waitErr != nil {
				t.Errorf("join finite child: %v", waitErr)
			}
		}
	})
	if err = command.Start(); err != nil {
		waited = true
		t.Fatal(err)
	}
	child, observeErr := lifetime.ObserveStartedProcess(command.Process.Pid)
	waitErr := command.Wait()
	waited = true
	if observeErr != nil || waitErr != nil {
		t.Fatalf("native child observe=%v wait=%v", observeErr, waitErr)
	}
	assumption, publication := evidenceForIntent(t, intent)
	assumption.Process = ProcessIdentity{PID: child.PID(), StartToken: child.StartToken()}
	publication.AssumptionSHA256, err = assumption.Digest(intent)
	if err != nil {
		t.Fatal(err)
	}
	writeOwnerEvidenceFixture(t, j, assumptionName(1), assumption)
	writeOwnerEvidenceFixture(t, j, publicationName(1), publication)
	record, created, err := j.settleStartup(t.Context(), lease, assumption.Startup)
	if err != nil || !created || record.Reason != settlementAssumedAbsent || record.PublicationSHA256 == "" {
		t.Fatalf("absent child settlement: created=%t record=%+v err=%v", created, record, err)
	}
}

func TestStartupSettlementPreviousBootDoesNotProbeOldPID(t *testing.T) {
	j, lease := journalFixture(t)
	intent := settlementIntent(t, j, false)
	intent.Inputs.BootID = otherStableBoot(t, j.bootID)
	intent.Inputs.RootInode++
	refreshIntentDigest(t, &intent)
	putHistoricalFixture(t, j, intent)
	assumption, _ := evidenceForIntent(t, intent)
	writeOwnerEvidenceFixture(t, j, assumptionName(1), assumption)
	record, created, err := j.settleStartup(t.Context(), lease, assumption.Startup)
	if err != nil || !created || record.Reason != settlementPreviousBoot {
		t.Fatalf("previous boot settlement: created=%t err=%v", created, err)
	}
	calls := 0
	reason, err := observeStartupSettlement(intent, &assumption, j.bootID, time.Now(), func(lifetime.ProcessRef) (bool, error) {
		calls++
		return false, errors.New("old PID must not be observed")
	})
	if err != nil || reason != settlementPreviousBoot || calls != 0 {
		t.Fatalf("previous boot probe count=%d reason=%s err=%v", calls, reason, err)
	}
	history, err := j.readSettlement(t.Context(), assumption.Startup)
	if err != nil || history != record {
		t.Fatalf("historical read: %v", err)
	}
}

func TestStartupSettlementDeadlineAndUnknownBoot(t *testing.T) {
	intent := startupIntentFixture(t)
	deadline, err := canonicalStartupTime(intent.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		when time.Time
		want bool
	}{{"before", deadline.Add(-time.Nanosecond), false}, {"at", deadline, true}} {
		t.Run(test.name, func(t *testing.T) {
			reason, observeErr := observeStartupSettlement(intent, nil, intent.Inputs.BootID, test.when, lifetime.ProcessPresent)
			if (observeErr == nil) != test.want || (test.want && reason != settlementUnassumedExpired) {
				t.Fatalf("deadline boundary reason=%s err=%v", reason, observeErr)
			}
		})
	}
	if _, err = observeStartupSettlement(intent, nil, "unknown", deadline, lifetime.ProcessPresent); err == nil {
		t.Fatal("unknown observing boot accepted")
	}
}

func TestStartupSettlementRejectsBadEvidence(t *testing.T) {
	for _, fault := range []string{"malformed assumption name", "malformed settlement name", "orphan assumption", "orphan publication", "publication without assumption", "malformed assumption", "malformed publication", "wrong publication digest", "wrong assumption reference", "same boot wrong native", "not latest", "corrupt settlement", "conflicting settlement"} {
		t.Run(fault, func(t *testing.T) {
			j, lease := journalFixture(t)
			intent := settlementIntent(t, j, true)
			if fault == "same boot wrong native" {
				intent.Inputs.RootInode++
				refreshIntentDigest(t, &intent)
				putHistoricalFixture(t, j, intent)
			} else {
				requireJournalPut(t, j, lease, intent)
			}
			reference := settlementReference(t, intent)
			corruptSettlementFixture(t, j, lease, intent, fault)
			_, created, err := j.settleStartup(t.Context(), lease, reference)
			if err == nil || created {
				t.Fatalf("bad evidence accepted: created=%t err=%v", created, err)
			}
			if fault == "wrong publication digest" && !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("publication binding fault lost: %v", err)
			}
		})
	}
}

func corruptSettlementFixture(t *testing.T, j *intentJournal, lease *Lease, intent StartupIntent, fault string) {
	t.Helper()
	assumption, publication := evidenceForIntent(t, intent)
	switch fault {
	case "malformed assumption name":
		writeOwnerEvidenceFixture(t, j, "assumption-1.json", assumption)
	case "malformed settlement name":
		writeSettlementFixtureBytes(t, j, "settlement-1.json", []byte("{}"))
	case "orphan assumption":
		writeOwnerEvidenceFixture(t, j, assumptionName(2), assumption)
	case "orphan publication":
		writeOwnerEvidenceFixture(t, j, publicationName(2), publication)
	case "publication without assumption":
		writeOwnerEvidenceFixture(t, j, publicationName(1), publication)
	case "malformed assumption":
		writeSettlementFixtureBytes(t, j, assumptionName(1), []byte("{"))
	case "malformed publication":
		writeOwnerEvidenceFixture(t, j, assumptionName(1), assumption)
		writeSettlementFixtureBytes(t, j, publicationName(1), []byte("{"))
	case "wrong publication digest":
		publication.AssumptionSHA256 = task.ComputeSHA256([]byte("wrong assumption bytes"))
		writeOwnerEvidenceFixture(t, j, assumptionName(1), assumption)
		writeOwnerEvidenceFixture(t, j, publicationName(1), publication)
	case "wrong assumption reference":
		assumption.Startup.IntentSHA256 = task.ComputeSHA256([]byte("wrong intent"))
		writeOwnerEvidenceFixture(t, j, assumptionName(1), assumption)
	case "not latest":
		next := settlementIntent(t, j, true)
		next.Sequence = 2
		next.Epoch = "11111111111111111111111111111111"
		requireJournalPut(t, j, lease, next)
	case "corrupt settlement":
		writeSettlementFixtureBytes(t, j, settlementName(1), []byte("{"))
	case "conflicting settlement":
		record := startupSettlement{Protocol: ProtocolVersion, Startup: assumption.Startup, AssumptionSHA256: task.ComputeSHA256([]byte("absent evidence")), ObservedBootID: j.bootID, SettledAt: time.Now().UTC().Format(time.RFC3339Nano), Reason: settlementAssumedAbsent}
		err := j.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
			_, putErr := tx.Put(settlementName(1), record)
			return putErr
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartupSettlementRejectsWrongLeaseCancellationAndClosed(t *testing.T) {
	j, lease := journalFixture(t)
	intent := settlementIntent(t, j, true)
	requireJournalPut(t, j, lease, intent)
	reference := settlementReference(t, intent)
	_, otherLease := journalFixture(t)
	if _, created, err := j.settleStartup(t.Context(), otherLease, reference); err == nil || created {
		t.Fatal("foreign lease accepted")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, created, err := j.settleStartup(canceled, lease, reference); !errors.Is(err, context.Canceled) || created {
		t.Fatalf("canceled settlement: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, created, err := j.settleStartup(t.Context(), lease, reference); err == nil || created {
		t.Fatal("closed lease accepted")
	}
	requireNoSettlement(t, j, 1)
}

func requireNoSettlement(t *testing.T, j *intentJournal, sequence uint64) {
	t.Helper()
	var record startupSettlement
	if err := j.control.Read(settlementName(sequence), &record); !task.RecordAbsent(err) {
		t.Fatalf("unexpected settlement record: %v", err)
	}
}

func writeSettlementFixtureBytes(t *testing.T, j *intentJournal, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(j.pin.Root(), "infrastructure", name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStartupSettlementConcurrentRetryHasOneWinner(t *testing.T) {
	j, lease := journalFixture(t)
	intent := settlementIntent(t, j, true)
	requireJournalPut(t, j, lease, intent)
	reference := settlementReference(t, intent)
	type outcome struct {
		record  startupSettlement
		created bool
		err     error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			record, created, err := j.settleStartup(t.Context(), lease, reference)
			results <- outcome{record, created, err}
		}()
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.created == second.created || first.record != second.record {
		t.Fatalf("concurrent settlement: first=%+v second=%+v", first, second)
	}
}

func TestStartupSettlementRootReplacementRetainsOriginal(t *testing.T) {
	j, lease := journalFixture(t)
	intent := settlementIntent(t, j, true)
	requireJournalPut(t, j, lease, intent)
	root := j.pin.Root()
	moved := root + "-settlement-original"
	rootData, err := os.ReadFile(filepath.Join(root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	renamed := false
	replacement := false
	t.Cleanup(func() {
		if replacement {
			if removeErr := os.RemoveAll(root); removeErr != nil {
				t.Errorf("remove owned replacement: %v", removeErr)
				return
			}
		}
		if renamed {
			if restoreErr := os.Rename(moved, root); restoreErr != nil {
				t.Errorf("restore original root: %v", restoreErr)
			}
		}
	})
	if err = os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	renamed = true
	if err = os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement = true
	if err = os.WriteFile(filepath.Join(root, "root.json"), rootData, 0o600); err != nil {
		t.Fatal(err)
	}
	_, created, err := j.settleStartup(t.Context(), lease, settlementReference(t, intent))
	if err == nil || created {
		t.Fatalf("replacement accepted: created=%t err=%v", created, err)
	}
	for _, directory := range []string{root, moved} {
		if _, statErr := os.Lstat(filepath.Join(directory, "infrastructure", settlementName(1))); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("replacement attempt wrote settlement: %v", statErr)
		}
	}
}

func TestStartupSettlementShapeIsDataOnly(t *testing.T) {
	intent := startupIntentFixture(t)
	record := startupSettlement{Protocol: ProtocolVersion, Startup: settlementReference(t, intent), ObservedBootID: intent.Inputs.BootID, SettledAt: intent.Deadline, Reason: settlementUnassumedExpired}
	if err := record.Validate(intent); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"protocol", "reference", "unknown boot", "reason", "publication without assumption", "premature", "hash"} {
		t.Run(fault, func(t *testing.T) {
			changed := record
			switch fault {
			case "protocol":
				changed.Protocol++
			case "reference":
				changed.Startup.Sequence++
			case "unknown boot":
				changed.ObservedBootID = "unknown"
			case "reason":
				changed.Reason = "caller-selected"
			case "publication without assumption":
				changed.PublicationSHA256 = task.ComputeSHA256([]byte("publication"))
			case "premature":
				changed.SettledAt = intent.AcceptedAt
			case "hash":
				changed.AssumptionSHA256 = "bad"
			}
			if err := changed.Validate(intent); err == nil {
				t.Fatal("malformed settlement accepted")
			}
		})
	}
}

// These readers model a named immutable record disappearing after enumeration;
// they delegate all other I/O to the actual rooted production journal.
type missingSettlementEvidenceReader struct {
	ownerRecordReader
	missing string
	fault   error
}

func (r missingSettlementEvidenceReader) assumption(ctx context.Context, name string) (StartupAssumption, error) {
	if r.missing == "assumption" {
		return StartupAssumption{}, r.fault
	}
	return r.ownerRecordReader.assumption(ctx, name)
}

func (r missingSettlementEvidenceReader) publication(ctx context.Context, name string) (OwnerPublication, error) {
	if r.missing == "publication" {
		return OwnerPublication{}, r.fault
	}
	return r.ownerRecordReader.publication(ctx, name)
}

func TestStartupSettlementListedEvidenceMissingIsFault(t *testing.T) {
	for _, kind := range []string{"assumption", "publication"} {
		t.Run(kind, func(t *testing.T) {
			fixture := ownerJournal(t)
			writeOwnerEvidenceFixture(t, fixture.journal, assumptionName(1), fixture.assumption)
			if kind == "publication" {
				writeOwnerEvidenceFixture(t, fixture.journal, publicationName(1), fixture.publication)
			}
			reader := missingSettlementEvidenceReader{ownerRecordReader: controlOwnerReader{controlIntentReader{fixture.journal.control}}, missing: kind, fault: os.ErrNotExist}
			_, err := validateSettlementEvidenceInventory(t.Context(), reader, fixture.journal.pin, fixture.assumption.Startup)
			if !errors.Is(err, task.ErrEvidenceFault) || !errors.Is(err, os.ErrNotExist) || task.RecordAbsent(err) {
				t.Fatalf("listed missing evidence treated as absence: %v", err)
			}
			reader.fault = errors.Join(os.ErrNotExist, context.Canceled)
			_, err = validateSettlementEvidenceInventory(t.Context(), reader, fixture.journal.pin, fixture.assumption.Startup)
			if !errors.Is(err, context.Canceled) || task.RecordAbsent(err) {
				t.Fatalf("joined context fault lost: %v", err)
			}
		})
	}
}

func TestStartupSettlementMissingIntentAndBadReference(t *testing.T) {
	j, lease := journalFixture(t)
	if _, created, err := j.settleStartup(t.Context(), lease, StartupReference{}); err == nil || created || task.RecordAbsent(err) {
		t.Fatalf("invalid reference misclassified: %v", err)
	}
	intent := settlementIntent(t, j, true)
	reference := settlementReference(t, intent)
	if _, created, err := j.settleStartup(t.Context(), lease, reference); !task.RecordAbsent(err) || created {
		t.Fatalf("missing intent: %v", err)
	}
	requireJournalPut(t, j, lease, intent)
	reference.IntentSHA256 = task.ComputeSHA256([]byte("wrong intent bytes"))
	if _, created, err := j.settleStartup(t.Context(), lease, reference); err == nil || created {
		t.Fatalf("wrong full intent digest accepted: %v", err)
	}
	requireNoSettlement(t, j, 1)
}

func TestStartupSettlementReplayStillRefusesLiveIncarnation(t *testing.T) {
	fixture := ownerJournal(t)
	requireAssumptionPut(t, fixture)
	digest, err := fixture.assumption.Digest(fixture.intent)
	if err != nil {
		t.Fatal(err)
	}
	record := startupSettlement{Protocol: ProtocolVersion, Startup: fixture.assumption.Startup, AssumptionSHA256: digest, ObservedBootID: fixture.journal.bootID, SettledAt: time.Now().UTC().Format(time.RFC3339Nano), Reason: settlementAssumedAbsent}
	// A corrupt but structurally valid claim is historical data, not proof of
	// native absence. Only the actual settlement writer may observe that fact.
	err = fixture.journal.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(settlementName(1), record)
		return putErr
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := fixture.journal.readSettlement(t.Context(), record.Startup)
	if err != nil || data != record {
		t.Fatalf("historical data reader performed native policy: %v", err)
	}
	_, created, err := fixture.journal.settleStartup(t.Context(), fixture.lease, record.Startup)
	if err == nil || created {
		t.Fatalf("existing claim replaced live native evidence: created=%t err=%v", created, err)
	}
	data, err = fixture.journal.readSettlement(t.Context(), record.Startup)
	if err != nil || data != record {
		t.Fatalf("refusal changed immutable record: %v", err)
	}
}

func TestStartupSettlementMalformedDarwinAssumptionIsNotAbsence(t *testing.T) {
	j, lease := journalFixture(t)
	intent := settlementIntent(t, j, true)
	requireJournalPut(t, j, lease, intent)
	assumption, _ := evidenceForIntent(t, intent)
	assumption.Process.StartToken = "darwin-v2:10:20:x"
	// Inject a canonical, structurally corrupt historical record through the
	// underlying store. The journal writer correctly rejects this malformed ref.
	if err := j.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(assumptionName(intent.Sequence), assumption)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	_, created, err := j.settleStartup(t.Context(), lease, settlementReference(t, intent))
	if !errors.Is(err, lifetime.ErrInvalidLaunchRef) || created {
		t.Fatalf("corrupt incarnation became absence proof: created=%t err=%v", created, err)
	}
	requireNoSettlement(t, j, intent.Sequence)
}
