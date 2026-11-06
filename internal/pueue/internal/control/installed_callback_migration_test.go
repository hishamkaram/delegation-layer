package control

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Finite original shell fixtures prove control policy and canonical writes.
// Historical executable digests below are frozen record data; no old program
// is installed or executed. This is not a native Pueue lifetime proof.
func TestExplicitHistoricalCallbackMigrationUsesInstalledTarget(t *testing.T) {
	for _, mode := range []string{"quiescent", "active work", "interrupted preparation", "warm historical target", "stopped historical target"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newInstalledCallbackMigrationFixture(t, mode == "active work")
			if mode == "warm historical target" || mode == "stopped historical target" {
				fixture.assertHistoricalTarget(t, mode == "stopped historical target")
				return
			}
			fixture.migrate(t, mode == "interrupted preparation")
			if mode == "active work" {
				fixture.assertBusy(t)
				return
			}
			fixture.assertMigrated(t)
		})
	}
}

func TestHistoricalCallbackMigrationRefusesStoppedAliasBeforeIntent(t *testing.T) {
	fixture := newInstalledCallbackMigrationFixture(t, false)
	fixture.prepareCandidateForMigration(t)
	root := fixture.private.stateRoot
	removePrivateTestDaemonState(t, root)
	assertPrivateTestPathMissing(t, privateMigrationRecordPath(filepath.Join(root, privateSupervisorDirectory)))
	before := snapshotPrivateSuccessorTestState(t, root)
	fixture.migratePreparedCandidate(t)
	fixture.assertStoppedAliasRefusal(t, before)
}

func TestPreparedHistoricalCallbackMigrationPreservesPreparedAuthority(t *testing.T) {
	for _, retarget := range []bool{false, true} {
		t.Run(map[bool]string{false: "same candidate", true: "distinct installed C"}[retarget], func(t *testing.T) {
			fixture := newInstalledCallbackMigrationFixture(t, false)
			fixture.prepareCandidateForMigration(t)
			// Produce the journal through its real owner, stopping at the work gate.
			// No shutdown flag or LegacyStopped authorization has been committed.
			fixture.candidate.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
				return ErrPrivateSupervisorMigrationBusy
			}
			fixture.migratePreparedCandidate(t)
			if !errors.Is(fixture.err, ErrPrivateSupervisorMigrationBusy) {
				t.Fatalf("fixture did not stop at the prepared journal: %v", fixture.err)
			}
			root := fixture.private.stateRoot
			journal, found, err := loadPrivateMigrationRecord(filepath.Join(root, privateSupervisorDirectory))
			if err != nil || !found || !privateMigrationPrepared(journal) {
				t.Fatalf("fixture journal already authorized shutdown: found=%t journal=%+v err=%v", found, journal, err)
			}
			fixture.candidate.options.PrivateMigrationAuthorizer = fixture.options.PrivateMigrationAuthorizer
			if retarget {
				fixture.prepareDistinctInstalledCandidate(t, journal)
			}
			removePrivateTestDaemonState(t, root)
			before := snapshotPrivateSuccessorTestState(t, root)
			fixture.migratePreparedCandidate(t)
			if retarget {
				expected := journal
				expected.Successor = privateDaemonIdentityForBinding(fixture.candidate.Binding())
				fixture.assertRetargetedPreparedJournal(t, before, expected)
			} else {
				fixture.assertStoppedAliasRefusal(t, before)
			}
		})
	}
}

func TestHistoricalCallbackMigrationUsesWarmAliasAfterPrimaryConfigChange(t *testing.T) {
	fixture := newInstalledCallbackMigrationFixture(t, false)
	fixture.prepareCandidateForMigration(t)
	root := fixture.private.stateRoot
	if string(readPrivateSupervisorFile(t, fixture.historical.ConfigPath)) == string(fixture.data) {
		t.Fatal("fixture did not change the primary config for the installed callback")
	}
	alias, found, err := readValidatedPrivateRecoveryAlias(root)
	if err != nil || !found || !alias.matches(fixture.historical) {
		t.Fatalf("callback preparation lost matching historical alias: found=%t err=%v", found, err)
	}
	fixture.migratePreparedCandidate(t)
	fixture.assertMigrated(t)
}

func TestHistoricalCallbackMigrationPreservesMalformedPreparedConfig(t *testing.T) {
	fixture := newInstalledCallbackMigrationFixture(t, false)
	fixture.prepareCandidateForMigration(t)
	configPath := fixture.historical.ConfigPath
	preparedConfig := readPrivateSupervisorFile(t, configPath)
	t.Cleanup(func() {
		if err := os.WriteFile(configPath, preparedConfig, 0o600); err != nil {
			t.Error(err)
		}
	})
	writePrivateTestFile(t, configPath, "{")
	before := snapshotPrivateSuccessorTestState(t, fixture.private.stateRoot)
	fixture.migratePreparedCandidate(t)
	if !errors.Is(fixture.err, ErrBinding) {
		t.Fatalf("malformed prepared config lost its binding refusal: %v", fixture.err)
	}
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, fixture.private.stateRoot)) {
		t.Fatal("malformed config refusal changed prepared config, alias, or canonical records")
	}
	if len(fixture.waits) != 0 {
		t.Fatal("malformed config refusal started a daemon")
	}
}

func TestHistoricalMigrationUsesSavedInstalledClientOnceBeforeJournal(t *testing.T) {
	f := newInstalledCallbackMigrationFixture(t, false)
	root := f.private.stateRoot
	clientA, daemonA := installPrivateSupervisorPair(t, filepath.Join(f.owned, "saved-A"))
	logPath := filepath.Join(f.owned, "recovery-client.log")
	f.options.Environment = append(os.Environ(), "PRIVATE_RECOVERY_CLIENT_LOG="+logPath)
	for _, selected := range []struct{ path, label string }{{clientA, "A"}, {f.private.clientPath, "B"}} {
		source := strings.Replace(privateClientFixture, "set -eu\n", "set -eu\nprintf '"+selected.label+" %s\\n' \"$*\" >> \"$PRIVATE_RECOVERY_CLIENT_LOG\"\n", 1)
		writeExecutable(t, selected.path, source)
	}
	f.historical.ClientExecutable, f.historical.DaemonExecutable = clientA, daemonA
	writePrivateSuccessorTestRecord(t, privateSupervisorBindingPath(root), privateSupervisorBindingRecord{SchemaVersion: task.SchemaVersion, Binding: f.historical})
	identityPath := privateDaemonIdentityPath(filepath.Join(root, privateSupervisorDirectory))
	writePrivateSuccessorTestRecord(t, identityPath, privateDaemonIdentityForBinding(f.historical))
	f.bindingBefore = readPrivateSupervisorFile(t, privateSupervisorBindingPath(root))
	f.daemonBefore = readPrivateSupervisorFile(t, identityPath)
	f.prepareCandidateForMigration(t)
	writePrivateTestFile(t, logPath, "")
	denied := errors.New("journal publication denied")
	f.candidate.options.PrivateGenerationGuard = generationGuardFunc(func(context.Context, string, func() error) error { return denied })
	f.migratePreparedCandidate(t)
	if !errors.Is(f.err, denied) || !errors.Is(f.err, ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("historical recovery did not reach journal publication: %v", f.err)
	}
	log := string(readPrivateSupervisorFile(t, logPath))
	if strings.Count(log, "status") != 1 || strings.Contains(log, "B ") || !strings.Contains(log, "A ") || !strings.Contains(log, privateRecoveryConfigPath(root)) {
		t.Fatalf("recovery did not select saved A exactly once through its alias: %q", log)
	}
	assertPrivateTestPathMissing(t, privateMigrationRecordPath(filepath.Join(root, privateSupervisorDirectory)))
	if len(f.waits) != 0 {
		t.Fatal("journal denial started a daemon")
	}
}

func TestHistoricalRecoveryReplaysPendingAliasWithMalformedPrimary(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "stopped"}[stopped], func(t *testing.T) {
			testHistoricalRecoveryPendingAliasWithMalformedPrimary(t, stopped)
		})
	}
}

func testHistoricalRecoveryPendingAliasWithMalformedPrimary(t *testing.T, stopped bool) {
	t.Helper()
	f := newInstalledCallbackMigrationFixture(t, false)
	f.prepareCandidateForMigration(t)
	root := f.private.stateRoot
	record := f.stageAliasWithMalformedPrimary(t)
	if stopped {
		removePrivateTestDaemonState(t, root)
	}
	recovered, started := f.recoverHistoricalAlias(t)
	if stopped {
		if !errors.Is(f.err, ErrHistoricalTargetNotReady) || recovered != nil {
			t.Fatalf("stopped staged alias gained restart: client=%t err=%v", recovered != nil, f.err)
		}
	} else if f.err != nil || recovered == nil {
		t.Fatalf("live staged alias depended on malformed primary config: %v", f.err)
	}
	got, found, readErr := readPrivateRecoveryBinding(root)
	if readErr != nil || !found || got != record {
		t.Fatalf("canonical recovery did not replay exact alias: found=%t record=%+v err=%v", found, got, readErr)
	}
	assertPrivateTestPathMissing(t, privateRecoveryPendingRecordPath(root))
	assertPrivateTestPathMissing(t, privateRecoveryPendingConfigPath(root))
	assertPrivateTestPathMissing(t, privateMigrationRecordPath(filepath.Join(root, privateSupervisorDirectory)))
	f.assertCanonicalGenerationUnchanged(t, "alias replay changed historical generation")
	if started || len(f.waits) != 0 || string(readPrivateSupervisorFile(t, f.historical.ConfigPath)) != "{" {
		t.Fatal("staged alias recovery started a daemon or changed malformed primary config")
	}
}

func TestHistoricalMigrationJournalFailureClosesTemporaryRecoveryWithoutAlias(t *testing.T) {
	f := newInstalledCallbackMigrationFixture(t, false)
	root := f.private.stateRoot
	base := filepath.Join(root, privateSupervisorDirectory)
	config, changed, err := privateInstalledCallbackConfig(f.data, base, f.runner)
	if err != nil || !changed {
		t.Fatalf("prepare installed callback config: changed=%t err=%v", changed, err)
	}
	config, err = configWithSocketPath(config, filepath.Join(base, "x"))
	if err != nil {
		t.Fatal(err)
	}
	writePrivateSuccessorTestBytes(t, f.historical.ConfigPath, config)
	f.err = WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		var prepareErr error
		f.candidate, prepareErr = PreparePrivateForMigrationWithLock(ctx, f.private.clientPath, f.private.daemonPath, root, f.options)
		return prepareErr
	})
	if f.err != nil {
		t.Fatal(f.err)
	}
	if f.candidate.Binding().Endpoint == f.historical.Endpoint {
		t.Fatal("successor endpoint did not differ from the historical endpoint")
	}
	denied := errors.New("journal publication denied")
	var temporary string
	f.candidate.options.PrivateGenerationGuard = generationGuardFunc(func(context.Context, string, func() error) error {
		entries, readErr := os.ReadDir(base)
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".migration-pueue-") {
				temporary = filepath.Join(base, entry.Name())
			}
		}
		return denied
	})
	f.migratePreparedCandidate(t)
	if !errors.Is(f.err, denied) || temporary == "" {
		t.Fatalf("journal publication did not retain canonical temporary recovery: temporary=%q err=%v", temporary, f.err)
	}
	assertPrivateTestPathMissing(t, temporary)
	for _, path := range []string{privateMigrationRecordPath(base), privateRecoveryRecordPath(root), privateRecoveryConfigPath(root), privateRecoveryPendingRecordPath(root), privateRecoveryPendingConfigPath(root)} {
		assertPrivateTestPathMissing(t, path)
	}
	f.assertCanonicalGenerationUnchanged(t, "journal failure changed historical generation")
	if len(f.waits) != 0 || string(readPrivateSupervisorFile(t, f.historical.ConfigPath)) != string(config) {
		t.Fatal("journal failure started a daemon or changed prepared config")
	}
}

func TestStoppedInstalledCallbackPreflightPresentEmpty(t *testing.T) {
	f := newStoppedInstalledCallbackFixture(t)
	f.prepareCandidateForMigration(t)
	if len(f.waits) != 2 {
		t.Fatalf("callback preparation Starts=%d, want initial and one recovered predecessor", len(f.waits))
	}
}

func TestStoppedInstalledCallbackPreflightRefusesNativeFaults(t *testing.T) {
	for _, fault := range []string{"missing", "partial", "corrupt", "unsafe layout", "unknown state", "missing policy", "live PID absent socket", "refused live PID", "refused no PID", "invalid observation", "source rename", "source missing", "late PID"} {
		t.Run(fault, func(t *testing.T) {
			f := newStoppedInstalledCallbackFixture(t)
			configureStoppedCallbackFault(t, f, fault)
			before := snapshotPrivateSuccessorTestState(t, f.private.stateRoot)
			prepareStoppedCallbackForTest(t, f)
			if f.err == nil || errors.Is(f.err, ErrPrivateSupervisorMigrationBusy) {
				t.Fatalf("unsafe stopped callback did not hard-refuse: %v", f.err)
			}
			f.assertHistoricalGenerationUnchanged(t, "offline refusal changed canonical generation/config")
			assertPrivateTestPathMissing(t, privateRecoveryRecordPath(f.private.stateRoot))
			assertPrivateTestPathMissing(t, privateRecoveryConfigPath(f.private.stateRoot))
			assertPrivateTestPathMissing(t, privateMigrationRecordPath(filepath.Join(f.private.stateRoot, privateSupervisorDirectory)))
			if len(f.waits) != 1 {
				t.Fatalf("offline refusal started %d extra daemons", len(f.waits)-1)
			}
			if fault != "source rename" && fault != "source missing" && fault != "late PID" && !maps.Equal(before, snapshotPrivateSuccessorTestState(t, f.private.stateRoot)) {
				t.Fatal("offline refusal mutated private state")
			}
		})
	}
}

func TestStoppedInstalledCallbackPreflightMissingDataDoesNotBecomeEmpty(t *testing.T) {
	f := newStoppedInstalledCallbackFixture(t)
	root := f.private.stateRoot
	candidate, err := Bind(t.Context(), f.private.clientPath, f.historical.ConfigPath, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := candidate.CloseAfterPending(f.err); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = attachPrivateDaemon(candidate, f.private.daemonPath); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, privateSupervisorDirectory, "data")
	removePrivateTestPath(t, filepath.Join(data, "state.json"))
	removePrivateTestPath(t, data)
	before := snapshotPrivateSuccessorTestState(t, root)
	// The candidate already exists: isolate the callback preflight from the
	// outer PreparePrivate layout initializer, which owns directory creation.
	f.err = WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		var prepareErr error
		f.candidate, prepareErr = preparePrivateInstalledCallback(ctx, root, candidate)
		return prepareErr
	})
	if !errors.Is(f.err, ErrPrivateSupervisorMigrationRequired) || errors.Is(f.err, ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("missing native data became empty restore: %v", f.err)
	}
	if len(f.waits) != 1 || !maps.Equal(before, snapshotPrivateSuccessorTestState(t, root)) {
		t.Fatal("missing native data preflight started or mutated private state")
	}
}

func configureStoppedCallbackFault(t *testing.T, f *installedCallbackMigrationFixture, fault string) {
	t.Helper()
	base := filepath.Join(f.private.stateRoot, privateSupervisorDirectory)
	state := filepath.Join(base, "data", "state.json")
	switch fault {
	case "missing":
		removePrivateTestPath(t, state)
	case "unknown state":
		data, err := os.ReadFile("../../testdata/status-4.0.4-queued.json")
		if err != nil {
			t.Fatal(err)
		}
		writePrivateRestoreBytes(t, state, privateRestoreUnknownStateBytes(t, data, `{"Future":{}}`))
	case "partial":
		writePrivateTestFile(t, state+".partial", "unfinished")
	case "corrupt":
		writePrivateTestFile(t, state, "{")
	case "unsafe layout":
		if err := os.Chmod(state, 0o700); err != nil {
			t.Fatal(err)
		}
	case "missing policy":
		f.options.PrivateMigrationAuthorizer = nil
	case "live PID absent socket", "refused live PID":
		writePrivateTestFile(t, filepath.Join(base, "run", "pueue.pid"), strconv.Itoa(os.Getpid()))
		f.options.processObserver = nil
		if fault == "refused live PID" {
			listenRefusedPrivateTestSocket(t, strings.TrimPrefix(f.historical.Endpoint, "unix:"))
		}
	case "refused no PID":
		listenRefusedPrivateTestSocket(t, strings.TrimPrefix(f.historical.Endpoint, "unix:"))
	case "invalid observation":
		f.options.Environment = append(os.Environ(), "PRIVATE_INVALID_STATUS=1")
		writePrivateTestFile(t, filepath.Join(base, "ready"), "ready")
	case "source rename", "source missing", "late PID":
		configureStoppedCallbackHook(t, f, fault, state)
	}
}

func configureStoppedCallbackHook(t *testing.T, f *installedCallbackMigrationFixture, fault, state string) {
	t.Helper()
	f.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
		switch fault {
		case "source rename":
			data := readPrivateSupervisorFile(t, state)
			writePrivateRestoreBytes(t, state+".replacement", data)
			return os.Rename(state+".replacement", state)
		case "source missing":
			return os.Remove(state)
		case "late PID":
			return os.WriteFile(filepath.Join(f.private.stateRoot, privateSupervisorDirectory, "run", "pueue.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		return nil
	}
	if fault == "late PID" {
		f.options.processObserver = nil
	}
}

func TestStoppedInstalledCallbackPreflightPreservesRawStates(t *testing.T) {
	states := []struct {
		name, source string
		want         State
	}{
		{"running", `{"Running":{"enqueued_at":"2026-09-13T20:00:00Z","start":"2026-09-13T20:00:01Z"}}`, StateRunning},
		{"paused", `{"Paused":{"enqueued_at":"2026-09-13T20:00:00Z","start":"2026-09-13T20:00:01Z"}}`, StateRunning},
		{"queued", `{"Queued":{"enqueued_at":"2026-09-13T20:00:00Z"}}`, StateQueued},
		{"stashed", `{"Stashed":{"enqueue_at":null}}`, StateStashed},
		{"locked", `{"Locked":{"previous_status":{"Queued":{"enqueued_at":"2026-09-13T20:00:00Z"}}}}`, StateUnknown},
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			f := newStoppedInstalledCallbackFixture(t)
			data, err := os.ReadFile("../../testdata/status-4.0.4-queued.json")
			if err != nil {
				t.Fatal(err)
			}
			restore := filepath.Join(f.private.stateRoot, privateSupervisorDirectory, "data", "state.json")
			writePrivateRestoreBytes(t, restore, privateRestoreUnknownStateBytes(t, data, state.source))
			calls := 0
			f.options.PrivateTerminalEvidencePreparer = stoppedCallbackRawStatePreparer(t, f.historical, state.want, &calls)
			f.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
				if calls != 1 {
					t.Fatal("authorizer ran before terminal evidence preparer")
				}
				calls++
				return ErrPrivateSupervisorMigrationBusy
			}
			before := snapshotPrivateSuccessorTestState(t, f.private.stateRoot)
			prepareStoppedCallbackForTest(t, f)
			if calls != 2 || !errors.Is(f.err, ErrPrivateSupervisorMigrationRequired) || errors.Is(f.err, ErrPrivateSupervisorMigrationBusy) || privateRecoveryReason(f.err) != "predecessor_pair_unavailable" {
				t.Fatalf("offline Busy projected as live wait: calls=%d err=%v", calls, f.err)
			}
			if len(f.waits) != 1 || !maps.Equal(before, snapshotPrivateSuccessorTestState(t, f.private.stateRoot)) {
				t.Fatal("offline Busy started a daemon or mutated state/alias")
			}
		})
	}
}

func stoppedCallbackRawStatePreparer(t *testing.T, expected task.SupervisorRef, state State, calls *int) func(context.Context, string, task.SupervisorRef, []Job) error {
	t.Helper()
	return func(_ context.Context, _ string, binding task.SupervisorRef, jobs []Job) error {
		if binding != expected || len(jobs) != 1 || jobs[0].State != state {
			t.Fatalf("raw state changed before preparer: binding=%+v jobs=%+v", binding, jobs)
		}
		*calls++
		return nil
	}
}

type retainedCallbackPolicyError struct{ cause error }

func (e *retainedCallbackPolicyError) Error() string           { return e.cause.Error() }
func (e *retainedCallbackPolicyError) Unwrap() error           { return e.cause }
func (e *retainedCallbackPolicyError) RetainsResidentCleanup() {}

func TestStoppedInstalledCallbackPreflightPreservesPolicyErrorTree(t *testing.T) {
	independent := errors.New("independent policy close failure")
	retained := &retainedCallbackPolicyError{cause: ErrPrivateSupervisorMigrationBusy}
	for _, policy := range []struct {
		name string
		err  error
		pure bool
	}{
		{"pure", ErrPrivateSupervisorMigrationBusy, true},
		{"wrapped pure", fmt.Errorf("queue policy: %w", ErrPrivateSupervisorMigrationBusy), true},
		{"mixed close", errors.Join(ErrPrivateSupervisorMigrationBusy, independent), false},
		{"mixed cancellation", errors.Join(ErrPrivateSupervisorMigrationBusy, context.Canceled), false},
		{"retained", retained, false},
	} {
		t.Run(policy.name, func(t *testing.T) {
			f := newStoppedInstalledCallbackFixture(t)
			f.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error { return policy.err }
			before := snapshotPrivateSuccessorTestState(t, f.private.stateRoot)
			prepareStoppedCallbackForTest(t, f)
			if policy.pure {
				if !errors.Is(f.err, ErrPrivateSupervisorMigrationRequired) || errors.Is(f.err, ErrPrivateSupervisorMigrationBusy) || privateRecoveryReason(f.err) != "predecessor_pair_unavailable" {
					t.Fatalf("pure offline Busy was not hard-refused: %v", f.err)
				}
			} else if !errors.Is(f.err, policy.err) {
				t.Fatalf("offline Busy conversion discarded policy error tree: want=%v got=%v", policy.err, f.err)
			}
			if len(f.waits) != 1 || !maps.Equal(before, snapshotPrivateSuccessorTestState(t, f.private.stateRoot)) {
				t.Fatal("policy refusal started a daemon or changed alias/config/state")
			}
		})
	}
}

func TestStoppedInstalledCallbackPreflightRefusedDeadPID(t *testing.T) {
	f := newStoppedInstalledCallbackFixture(t)
	pidPath := filepath.Join(f.private.stateRoot, privateSupervisorDirectory, "run", "pueue.pid")
	writePrivateTestFile(t, pidPath, strconv.Itoa(f.stoppedPID))
	// The PID is from the actual joined initial fixture daemon, not an invented process.
	f.options.processObserver = nil
	listenRefusedPrivateTestSocket(t, strings.TrimPrefix(f.historical.Endpoint, "unix:"))
	f.prepareCandidateForMigration(t)
	if len(f.waits) != 2 {
		t.Fatalf("dead-PID refusal recovery Starts=%d, want initial plus temporary predecessor", len(f.waits))
	}
}

func TestStoppedInstalledCallbackLateFaultStopsThroughAlias(t *testing.T) {
	f := newStoppedInstalledCallbackFixture(t)
	denied := errors.New("injected callback failure after committed config rename")
	f.options.PrivateGenerationGuard = generationGuardFunc(func(_ context.Context, _ string, commit func() error) error {
		if err := commit(); err != nil {
			return err
		}
		if string(readPrivateSupervisorFile(t, f.historical.ConfigPath)) != string(f.data) {
			return denied
		}
		return nil
	})
	prepareStoppedCallbackForTest(t, f)
	if !errors.Is(f.err, denied) || len(f.waits) != 2 {
		t.Fatalf("late config fault not reached after temporary recovery: waits=%d err=%v", len(f.waits), f.err)
	}
	if string(readPrivateSupervisorFile(t, f.historical.ConfigPath)) == string(f.data) {
		t.Fatal("late fault happened before actual config rename")
	}
	assertPrivateTestPathMissing(t, filepath.Join(f.private.stateRoot, privateSupervisorDirectory, "ready"))
	f.assertCanonicalGenerationUnchanged(t, "safe late failure published a successor")
	assertPrivateTestPathMissing(t, privateMigrationRecordPath(filepath.Join(f.private.stateRoot, privateSupervisorDirectory)))
	for _, done := range f.waits {
		select {
		case <-done:
		case <-time.After(privateTestObservationTimeout):
			t.Fatal("safe late fault did not join actual daemon Wait")
		}
	}
}

func TestStoppedInstalledCallbackLiveBusyRetainsRecovery(t *testing.T) {
	f := newStoppedInstalledCallbackFixture(t)
	calls := 0
	f.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
		calls++
		if calls == 1 {
			return nil
		}
		return ErrPrivateSupervisorMigrationBusy
	}
	prepareStoppedCallbackForTest(t, f)
	if calls != 2 || !errors.Is(f.err, ErrPrivateSupervisorMigrationBusy) || len(f.waits) != 2 {
		t.Fatalf("live queue refusal lost recovery ownership: calls=%d waits=%d err=%v", calls, len(f.waits), f.err)
	}
	if _, err := os.Stat(filepath.Join(f.private.stateRoot, privateSupervisorDirectory, "ready")); err != nil {
		t.Fatalf("live Busy stopped an unproved queue: %v", err)
	}
	if _, found, err := readPrivateRecoveryBinding(f.private.stateRoot); err != nil || !found {
		t.Fatalf("live Busy lost existing recovery alias: found=%t err=%v", found, err)
	}
	f.assertHistoricalGenerationUnchanged(t, "live Busy rewrote callback or generation")
}

func prepareStoppedCallbackForTest(t *testing.T, f *installedCallbackMigrationFixture) {
	t.Helper()
	f.err = WithBootstrapLockContext(t.Context(), f.private.stateRoot, func(ctx context.Context) error {
		var prepareErr error
		f.candidate, prepareErr = PreparePrivateForMigrationWithLock(ctx, f.private.clientPath, f.private.daemonPath, f.private.stateRoot, f.options)
		return prepareErr
	})
}

// Extend the existing callback fixture through the real binding/publication and
// supported daemon lifecycle. The saved pair is removed only after its Wait.
func newStoppedInstalledCallbackFixture(t *testing.T) *installedCallbackMigrationFixture {
	t.Helper()
	f := newInstalledCallbackMigrationFixture(t, false)
	root := f.private.stateRoot
	removePrivateTestDaemonState(t, root)
	oldClient, oldDaemon := installPrivateSupervisorPair(t, filepath.Join(f.owned, "installed-A"))
	oldRunner := filepath.Join(filepath.Dir(oldClient), "delegate-run")
	writeExecutable(t, oldRunner, "#!/bin/sh\nexit 0\n")
	base := filepath.Join(root, privateSupervisorDirectory)
	config, changed, err := privateInstalledCallbackConfig(f.data, base, oldRunner)
	if err != nil || !changed {
		t.Fatalf("prepare original installed callback: changed=%t err=%v", changed, err)
	}
	writePrivateRestoreBytes(t, f.historical.ConfigPath, config)
	f.data = config
	old, err := Bind(t.Context(), oldClient, f.historical.ConfigPath, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := old.CloseAfterPending(f.err); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = attachPrivateDaemon(old, oldDaemon); err != nil {
		t.Fatal(err)
	}
	f.historical = old.Binding()
	if err = publishPrivateDaemonIdentity(base, f.historical); err != nil {
		t.Fatal(err)
	}
	f.stopInitialInstalledCallbackDaemon(t, old)

	for _, path := range []string{oldClient, oldDaemon, oldRunner} {
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	makePrivateTestDirectory(t, filepath.Join(base, "data"))
	writePrivateTestFile(t, filepath.Join(base, "data", "state.json"), `{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":0}}}`)
	f.bindingBefore = readPrivateSupervisorFile(t, privateSupervisorBindingPath(root))
	f.daemonBefore = readPrivateSupervisorFile(t, privateDaemonIdentityPath(base))
	return f
}

func (f *installedCallbackMigrationFixture) stopInitialInstalledCallbackDaemon(t *testing.T, old *Client) {
	t.Helper()
	root := f.private.stateRoot
	var err error
	if err = old.StartPrivate(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	f.stoppedPID, _, err = old.privateDaemonPID()
	if err != nil || f.stoppedPID <= 0 {
		t.Fatalf("initial daemon has no actual PID: pid=%d err=%v", f.stoppedPID, err)
	}
	if err = old.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = old.WaitUnavailable(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.waits) != 1 {
		t.Fatalf("initial daemon actual Wait count=%d", len(f.waits))
	}
	select {
	case <-f.waits[0]:
	case <-time.After(privateTestObservationTimeout):
		t.Fatal("initial daemon actual Wait did not close")
	}
}

// The fixture owns its candidate, finite-daemon Wait handles and temporary
// installation. Cleanup removes paths only after closing the candidate and
// observing every owned daemon's actual Wait.
type installedCallbackMigrationFixture struct {
	owned         string
	private       privateSupervisorTestFixture
	options       Options
	historical    task.SupervisorRef
	data          []byte
	bindingBefore []byte
	daemonBefore  []byte
	runner        string
	stoppedPID    int
	candidate     *Client
	waits         []<-chan struct{}
	err           error
}

func (f *installedCallbackMigrationFixture) prepareCandidateForMigration(t *testing.T) {
	t.Helper()
	root := f.private.stateRoot
	f.err = WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		var prepareErr error
		f.candidate, prepareErr = PreparePrivateForMigrationWithLock(ctx, f.private.clientPath, f.private.daemonPath, root, f.options)
		return prepareErr
	})
	if f.err != nil {
		t.Fatalf("prepare installed callback candidate: %v", f.err)
	}
	f.assertPrepared(t)
}

func (f *installedCallbackMigrationFixture) migratePreparedCandidate(t *testing.T) {
	t.Helper()
	f.err = WithBootstrapLockContext(t.Context(), f.private.stateRoot, func(ctx context.Context) error {
		return f.candidate.MigratePrivateSupervisorWithLock(ctx, f.private.stateRoot)
	})
}

func (f *installedCallbackMigrationFixture) stageAliasWithMalformedPrimary(t *testing.T) privateRecoveryBindingRecord {
	t.Helper()
	root := f.private.stateRoot
	record, found, err := readPrivateRecoveryBinding(root)
	if err != nil || !found {
		t.Fatalf("missing prepared alias: found=%t err=%v", found, err)
	}
	writePrivateSuccessorTestRecord(t, privateRecoveryPendingRecordPath(root), privateRecoveryPublicationRecord{SchemaVersion: task.SchemaVersion, Binding: record})
	if err = os.Rename(record.ConfigPath, privateRecoveryPendingConfigPath(root)); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPath(t, privateRecoveryRecordPath(root))
	prepared := readPrivateSupervisorFile(t, f.historical.ConfigPath)
	t.Cleanup(func() {
		if restoreErr := os.WriteFile(f.historical.ConfigPath, prepared, 0o600); restoreErr != nil {
			t.Error(restoreErr)
		}
	})
	writePrivateTestFile(t, f.historical.ConfigPath, "{")
	return record
}

func (f *installedCallbackMigrationFixture) recoverHistoricalAlias(t *testing.T) (*Client, bool) {
	t.Helper()
	var recovered *Client
	var started bool
	root := f.private.stateRoot
	f.err = WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		var recoveryErr error
		recovered, started, recoveryErr = RecoverSavedWithPair(ctx, root, f.historical, f.private.clientPath, f.private.daemonPath, f.options)
		return recoveryErr
	})
	t.Cleanup(func() {
		if closeErr := recovered.CloseAfterPending(f.err); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return recovered, started
}

func (f *installedCallbackMigrationFixture) prepareDistinctInstalledCandidate(t *testing.T, prepared privateMigrationRecord) {
	t.Helper()
	root := f.private.stateRoot
	clientPath, daemonPath := installPrivateSupervisorPair(t, filepath.Join(f.owned, "installed-C"))
	options := f.options
	options.RuntimeClientExecutable = clientPath
	f.err = WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		candidate, prepareErr := PreparePrivateForMigrationWithLock(ctx, clientPath, daemonPath, root, options)
		if prepareErr != nil {
			return errors.Join(prepareErr, candidate.CloseAfterPending(prepareErr))
		}
		if closeErr := f.candidate.CloseAfterPending(f.err); closeErr != nil {
			return errors.Join(closeErr, candidate.CloseAfterPending(closeErr))
		}
		f.candidate = candidate
		return nil
	})
	if f.err != nil {
		t.Fatalf("prepare distinct installed C: %v", f.err)
	}
	selected := f.candidate.Binding()
	if selected.ClientExecutable != clientPath || selected.DaemonExecutable != daemonPath || privateDaemonIdentityMatchesBinding(prepared.Successor, selected) {
		t.Fatalf("fixture did not independently select C: selected=%+v prepared=%+v", selected, prepared)
	}
	f.assertPrepared(t)
	current, found, err := loadPrivateMigrationRecord(filepath.Join(root, privateSupervisorDirectory))
	if err != nil || !found || current != prepared {
		t.Fatalf("preparing C changed the accepted B journal: found=%t current=%+v err=%v", found, current, err)
	}
}

func (f *installedCallbackMigrationFixture) assertStoppedAliasRefusal(t *testing.T, before map[string]privateSuccessorTestFile) {
	t.Helper()
	if !maps.Equal(before, snapshotPrivateSuccessorTestState(t, f.private.stateRoot)) {
		t.Error("stopped historical alias changed config, alias, binding, witness, or journal")
	}
	if len(f.waits) != 0 {
		t.Error("stopped historical alias started a daemon")
	}
	f.assertStoppedHistoricalMigrationError(t)
}

func (f *installedCallbackMigrationFixture) assertRetargetedPreparedJournal(t *testing.T, before map[string]privateSuccessorTestFile, expected privateMigrationRecord) {
	t.Helper()
	root := f.private.stateRoot
	path := privateMigrationRecordPath(filepath.Join(root, privateSupervisorDirectory))
	data, err := task.MarshalCanonical(expected)
	if err != nil {
		t.Fatal(err)
	}
	after := snapshotPrivateSuccessorTestState(t, root)
	if after[path].contents != string(data) || after[path].mode != before[path].mode {
		t.Fatalf("reversible replacement did not publish exact prepared C: record=%+v", expected)
	}
	if !privateMigrationPrepared(expected) || expected.LegacyPID != 0 || expected.LegacyProcessStart != "" {
		t.Fatalf("replacement fixture granted shutdown authority: %+v", expected)
	}
	// Atomic journal replacement changes its parent's directory timestamp.
	// Every other file and its metadata must remain exact.
	for name, prior := range before {
		if name == path {
			continue
		}
		actual := after[name]
		if prior.mode.IsDir() {
			actual.modified = prior.modified
		}
		if actual != prior {
			t.Fatalf("reversible journal replacement changed %s", name)
		}
	}
	for name := range after {
		if _, existed := before[name]; !existed {
			t.Fatalf("reversible journal replacement created unexpected state %s", name)
		}
	}
	if len(f.waits) != 0 {
		t.Fatalf("prepared C started %d daemons", len(f.waits))
	}
	f.assertStoppedHistoricalMigrationError(t)
}

func (f *installedCallbackMigrationFixture) assertStoppedHistoricalMigrationError(t *testing.T) {
	t.Helper()
	if !errors.Is(f.err, ErrHistoricalTargetNotReady) || !errors.Is(f.err, ErrPrivateSupervisorMigrationRequired) || privateRecoveryReason(f.err) != "historical_target_not_ready" {
		t.Fatalf("stopped historical alias lost its non-retryable refusal: %v", f.err)
	}
}

func newInstalledCallbackMigrationFixture(t *testing.T, busy bool) *installedCallbackMigrationFixture {
	t.Helper()
	owned, err := os.MkdirTemp("", "dl-callback-")
	if err != nil {
		t.Fatal(err)
	}
	f := &installedCallbackMigrationFixture{owned: owned}
	t.Cleanup(func() { f.cleanup(t) })
	owned, err = filepath.EvalSymlinks(owned)
	if err != nil {
		t.Fatal(err)
	}
	f.owned = owned
	root := filepath.Join(owned, "state")
	writePrivateStateRootRecord(t, root)
	f.private = privateSupervisorTestFixture{stateRoot: root, clientPath: filepath.Join(owned, "pueue"), daemonPath: filepath.Join(owned, "pueued")}
	writeExecutable(t, f.private.clientPath, privateClientFixture)
	writeExecutable(t, f.private.daemonPath, privateDaemonFixture)
	f.options = privateMigrationTestOptions(root, privateTestObservationTimeout)
	f.prepareHistoricalGeneration(t)
	base := filepath.Join(root, privateSupervisorDirectory)
	writePrivateTestFile(t, filepath.Join(base, "ready"), "ready\n")
	makePrivateTestDirectory(t, filepath.Join(base, "run"))
	writePrivateTestFile(t, filepath.Join(base, "run", "pueue.pid"), "31415\n")
	if busy {
		writePrivateTestFile(t, filepath.Join(base, "test-status.json"), preparedPrivateObservationStatus)
	}
	f.runner = filepath.Join(filepath.Dir(f.private.clientPath), "delegate-run")
	writeExecutable(t, f.runner, "#!/bin/sh\nexit 0\n")
	f.options.PrivateTerminalEventRunner = f.runner
	f.options.RuntimeClientExecutable = f.private.clientPath
	f.options.DaemonWaitOwner = daemonWaitOwnerFunc(func(_ string, _ int, done <-chan struct{}) error {
		f.waits = append(f.waits, done)
		return nil
	})
	f.bindingBefore = readPrivateSupervisorFile(t, privateSupervisorBindingPath(root))
	f.daemonBefore = readPrivateSupervisorFile(t, privateDaemonIdentityPath(base))
	return f
}

func (f *installedCallbackMigrationFixture) prepareHistoricalGeneration(t *testing.T) {
	t.Helper()
	root := f.private.stateRoot
	base := filepath.Join(root, privateSupervisorDirectory)
	resolution, err := privateResolution(f.options)
	if err != nil {
		t.Fatal(err)
	}
	configPath, _, err := ensurePrivateConfigContext(t.Context(), root, resolution, f.options.PrivateGenerationGuard)
	if err != nil {
		t.Fatal(err)
	}
	f.data = readPrivateSupervisorFile(t, configPath)
	resolved, err := resolvePrivateConfigSource(f.data, base, resolution)
	if err != nil {
		t.Fatal(err)
	}
	_, historical := installedIdentityLegacyRef(t)
	pair := filepath.Join(base, "pueue-pair-"+strings.Repeat("a", 64))
	historical.ClientExecutable, historical.DaemonExecutable = filepath.Join(pair, "pueue"), filepath.Join(pair, "pueued")
	historical.ConfigPath, historical.ConfigDigest, historical.Endpoint = configPath, task.ComputeSHA256(f.data), resolved.Endpoint()
	historical.ResolvedConfigSHA256, err = resolved.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err = publishPrivateDaemonIdentity(base, historical); err != nil {
		t.Fatal(err)
	}
	if err = clearPrivateBootstrapPending(root); err != nil {
		t.Fatal(err)
	}
	f.historical = historical
}

func (f *installedCallbackMigrationFixture) cleanup(t *testing.T) {
	t.Helper()
	closed := true
	if f.candidate != nil {
		if closeErr := f.candidate.CloseAfterPending(f.err); closeErr != nil {
			t.Error(closeErr)
			closed = false
		}
	}
	for _, done := range f.waits {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("finite fixture daemon Wait remained open")
			closed = false
		}
	}
	if closed {
		if removeErr := os.RemoveAll(f.owned); removeErr != nil {
			t.Error(removeErr)
		}
	} else {
		t.Logf("retained callback migration fixture with unresolved closure: %s", f.owned)
	}
}

func (f *installedCallbackMigrationFixture) assertHistoricalTarget(t *testing.T, stopped bool) {
	t.Helper()
	if stopped {
		removePrivateTestDaemonState(t, f.private.stateRoot)
	}
	f.candidate, f.err = prepareExactPublishedPair(t.Context(), f.historical, f.options)
	if stopped {
		if f.candidate != nil || !errors.Is(f.err, ErrHistoricalTargetNotReady) {
			t.Fatalf("stopped historical target gained restart: client=%t err=%v", f.candidate != nil, f.err)
		}
	} else if f.err != nil || f.candidate == nil || f.candidate.Binding() != f.historical || f.candidate.RuntimeBinding().ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || f.candidate.RuntimeBinding().ClientExecutable != f.private.clientPath {
		t.Fatalf("exact current historical target lost installed warm reuse: client=%t err=%v", f.candidate != nil, f.err)
	}
	f.assertHistoricalGenerationUnchanged(t, "historical target probe changed config or canonical generation")
	if len(f.waits) != 0 {
		t.Fatal("historical target probe launched daemon")
	}
}

func (f *installedCallbackMigrationFixture) assertHistoricalGenerationUnchanged(t *testing.T, message string) {
	t.Helper()
	if string(readPrivateSupervisorFile(t, f.historical.ConfigPath)) != string(f.data) {
		t.Fatal(message)
	}
	f.assertCanonicalGenerationUnchanged(t, message)
}

func (f *installedCallbackMigrationFixture) assertCanonicalGenerationUnchanged(t *testing.T, message string) {
	t.Helper()
	root := f.private.stateRoot
	base := filepath.Join(root, privateSupervisorDirectory)
	if string(readPrivateSupervisorFile(t, privateSupervisorBindingPath(root))) != string(f.bindingBefore) || string(readPrivateSupervisorFile(t, privateDaemonIdentityPath(base))) != string(f.daemonBefore) {
		t.Fatal(message)
	}
}

func (f *installedCallbackMigrationFixture) migrate(t *testing.T, interrupted bool) {
	t.Helper()
	root := f.private.stateRoot
	f.err = WithBootstrapLockContext(t.Context(), root, func(ctx context.Context) error {
		var prepareErr error
		f.candidate, prepareErr = PreparePrivateForMigrationWithLock(ctx, f.private.clientPath, f.private.daemonPath, root, f.options)
		if prepareErr != nil {
			return prepareErr
		}
		f.assertPrepared(t)
		if interrupted {
			accepted := f.candidate.Binding()
			if closeErr := f.candidate.Close(); closeErr != nil {
				return closeErr
			}
			f.candidate, prepareErr = PreparePrivateForMigrationWithLock(ctx, f.private.clientPath, f.private.daemonPath, root, f.options)
			if prepareErr != nil {
				return prepareErr
			}
			if f.candidate.Binding() != accepted {
				t.Fatal("interrupted preparation changed selected target")
			}
		}
		return f.candidate.MigratePrivateSupervisorWithLock(ctx, root)
	})
}

func (f *installedCallbackMigrationFixture) assertPrepared(t *testing.T) {
	t.Helper()
	if f.candidate.Binding().ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || f.candidate.Binding().ClientSHA256 != "" || f.candidate.Binding().DaemonSHA256 != "" {
		t.Fatal("callback preparation manufactured a legacy executable identity")
	}
	if f.candidate.Binding().ConfigDigest == f.historical.ConfigDigest {
		t.Fatal("accepted target did not bind actual installed callback config bytes")
	}
	f.assertCanonicalGenerationUnchanged(t, "callback preparation rewrote live historical generation records")
	if string(readPrivateSupervisorFile(t, privateRecoveryConfigPath(f.private.stateRoot))) != string(f.data) {
		t.Fatal("callback preparation lost exact historical config")
	}
}

func (f *installedCallbackMigrationFixture) assertBusy(t *testing.T) {
	t.Helper()
	if !errors.Is(f.err, ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("active historical work did not block callback migration: %v", f.err)
	}
	f.assertHistoricalGenerationUnchanged(t, "active historical work changed config or generation")
	if len(f.waits) != 0 {
		t.Fatal("active historical work launched successor")
	}
}

func (f *installedCallbackMigrationFixture) assertMigrated(t *testing.T) {
	t.Helper()
	if f.err != nil {
		t.Fatalf("quiescent historical callback migration failed: %v", f.err)
	}
	root := f.private.stateRoot
	published, found, err := ReadPrivateSupervisorBinding(root)
	if err != nil || !found || f.candidate == nil || published != f.candidate.Binding() {
		t.Fatalf("installed target was not canonically published: found=%t err=%v", found, err)
	}
	callback, err := privateInstalledTerminalEventCallback(root, f.runner)
	if err != nil {
		t.Fatal(err)
	}
	config, parseErr := ParseConfig(readPrivateSupervisorFile(t, f.historical.ConfigPath))
	if parseErr != nil || config.Daemon.Callback == nil || *config.Daemon.Callback != callback {
		t.Fatal("published config lost selected callback")
	}
	assertPrivateDaemonIdentityMatchesBinding(t, root, published)
	if pending, pendingErr := PrivateSupervisorMigrationPending(root); pendingErr != nil || pending {
		t.Fatalf("migration did not settle: pending=%t err=%v", pending, pendingErr)
	}
	if len(f.waits) != 1 {
		t.Fatalf("successor started %d times, want once", len(f.waits))
	}
}
