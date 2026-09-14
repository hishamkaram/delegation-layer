package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRunProviderReleasesContinuationOnCommittedAndRejected(t *testing.T) {
	cases := []struct {
		name        string
		executable  string
		arguments   []string
		publication task.Publication
	}{
		{name: "committed", executable: "/usr/bin/printf", arguments: []string{"answer\n"}, publication: task.PublicationCommitted},
		{name: "rejected", executable: "/usr/bin/true", publication: task.PublicationRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, td, req := newAppTestTaskWithPrior(t, false, testPriorSession())
			defer closeAppTestTask(t, store, td)
			successor, _ := newAppTaskInStore(t, store, strings.Repeat("d", 32), false, testPriorSession())
			defer func() {
				closeErr := successor.Close()
				if closeErr != nil {
					t.Error(closeErr)
				}
			}()
			if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
				t.Fatal(err)
			}
			rewriteAppProviderExecutable(t, td, tc.executable)
			profile := appTestExecutionProfile(req, tc.executable, tc.arguments)
			result := runProvider(newResponse("dispatch"), td, req, profile, nil, Dependencies{})
			if result.code != 0 || result.err != nil {
				t.Fatalf("terminal provider run failed: %+v", result)
			}
			if result.response.Publication != tc.publication.String() || result.response.Outcome == nil {
				t.Fatalf("terminal winner was not preserved: %+v", result.response)
			}
			if err := successor.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
				t.Fatalf("successor could not claim released session: %v", err)
			}
		})
	}
}

func TestRunRunnerReleasesAlreadyTerminalAndSealedContinuations(t *testing.T) {
	t.Run("sealed recovery", func(t *testing.T) { assertRunRunnerReleasesContinuation(t, false) })
	t.Run("already terminal", func(t *testing.T) { assertRunRunnerReleasesContinuation(t, true) })
}

func assertRunRunnerReleasesContinuation(t *testing.T, alreadyTerminal bool) {
	t.Helper()
	store, td, req := newAppTestTaskWithPrior(t, true, testPriorSession())
	defer closeAppTestTask(t, store, td)
	successor, _ := newAppTaskInStore(t, store, strings.Repeat("e", 32), false, testPriorSession())
	defer func() {
		closeErr := successor.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	if alreadyTerminal {
		if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
			t.Fatalf("preparing terminal winner cleanup=%v collect=%v", cleanupErr, collectErr)
		}
	}
	result := runRunner(runnerArguments{Root: store.Root, TaskID: req.TaskID}, Dependencies{})
	if result.code != 0 || result.err != nil {
		t.Fatalf("recovery run failed: %+v", result)
	}
	if result.response.Outcome == nil || result.response.Publication != task.PublicationCommitted.String() {
		t.Fatalf("recovery lost terminal winner: %+v", result.response)
	}
	if err := successor.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatalf("successor could not claim recovered session: %v", err)
	}
}

func TestDispatchExistingReleasesAlreadyTerminalContinuation(t *testing.T) {
	store, td, req := newAppTestTaskWithPrior(t, true, testPriorSession())
	defer closeAppTestTask(t, store, td)
	successor, _ := newAppTaskInStore(t, store, strings.Repeat("5", 32), false, testPriorSession())
	defer func() {
		if closeErr := successor.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatalf("preparing terminal winner cleanup=%v collect=%v", cleanupErr, collectErr)
	}
	if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	result := dispatchExisting(Arguments{PueueConfig: "/tmp/app-pueue.yml"}, Dependencies{}, store, td, *req, newResponse("dispatch"))
	if result.code != 0 || result.err != nil || result.response.Outcome == nil {
		t.Fatalf("existing terminal dispatch failed: %+v", result)
	}
	if err := successor.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatalf("successor could not claim dispatched session: %v", err)
	}
}

func TestCollectReleasesRecoveredContinuation(t *testing.T) {
	store, td, req := newAppTestTaskWithPrior(t, true, testPriorSession())
	defer closeAppTestTask(t, store, td)
	successor, _ := newAppTaskInStore(t, store, strings.Repeat("f", 32), false, testPriorSession())
	defer func() {
		closeErr := successor.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	result := collect(Arguments{Root: store.Root, TaskID: req.TaskID}, storeDependencies{})
	if result.code != 0 || result.err != nil || result.response.Outcome == nil {
		t.Fatalf("collect recovery failed: %+v", result)
	}
	if err := successor.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatalf("successor could not claim collected session: %v", err)
	}
}

func TestContinuationClaimRetainedWithoutTerminalEvidenceOrAfterActiveRun(t *testing.T) {
	store, td, req := newAppTestTaskWithPrior(t, false, testPriorSession())
	defer closeAppTestTask(t, store, td)
	successor, _ := newAppTaskInStore(t, store, strings.Repeat("1", 32), false, testPriorSession())
	defer func() {
		closeErr := successor.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	if err := releaseContinuationSession(td, req, nil); err != nil {
		t.Fatalf("unknown continuation outcome changed the claim: %v", err)
	}
	if err := successor.ClaimSession(req.Provider, req.PriorSession.ConversationID); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("unknown continuation did not retain claim: %v", err)
	}
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}()
	outcome := &task.OutcomeRecord{EvidenceSHA256: task.ComputeSHA256([]byte("unpublished"))}
	if err = releaseContinuationSession(td, req, outcome); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("active runner was not retained: %v", err)
	}
	if err = successor.ClaimSession(req.Provider, req.PriorSession.ConversationID); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("active runner claim changed after failed release: %v", err)
	}
}

func TestRunProviderUnknownOutcomeRetainsContinuation(t *testing.T) {
	store, td, req := newAppTestTaskWithPrior(t, false, testPriorSession())
	defer closeAppTestTask(t, store, td)
	successor, _ := newAppTaskInStore(t, store, strings.Repeat("7", 32), false, testPriorSession())
	defer func() {
		if closeErr := successor.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	profile := appTestExecutionProfile(req, "/usr/bin/true", nil)
	profile.Identity = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
		return nil, errInjectedIdentity
	}
	result := runProvider(newResponse("dispatch"), td, req, profile, nil, Dependencies{})
	if result.code != 1 || !errors.Is(result.err, errInjectedIdentity) || result.response.Outcome != nil {
		t.Fatalf("unknown provider outcome was not retained: %+v", result)
	}
	if err := successor.ClaimSession(req.Provider, req.PriorSession.ConversationID); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("unknown provider outcome freed session: %v", err)
	}
}

func TestContinuationSessionSerializesOverlappingSuccessors(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	prior := testPriorSession()
	owner, ownerReq := newAppTaskInStore(t, store, strings.Repeat("6", 32), true, prior)
	defer func() {
		if closeErr := owner.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if _, cleanupErr, collectErr := owner.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatalf("preparing session owner winner cleanup=%v collect=%v", cleanupErr, collectErr)
	}
	winner, err := owner.Inspect()
	if err != nil || winner.Outcome == nil {
		t.Fatalf("inspecting session owner winner: inspection=%+v err=%v", winner, err)
	}
	if err = owner.ClaimSession(ownerReq.Provider, ownerReq.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	if err = releaseContinuationSession(owner, ownerReq, winner.Outcome); err != nil {
		t.Fatal(err)
	}
	first, firstReq := newAppTaskInStore(t, store, strings.Repeat("2", 32), false, prior)
	second, secondReq := newAppTaskInStore(t, store, strings.Repeat("3", 32), false, prior)
	defer func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	}()
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go claimContinuationAfter(start, results, &wg, first, firstReq)
	go claimContinuationAfter(start, results, &wg, second, secondReq)
	close(start)
	wg.Wait()
	close(results)
	claimed, busy, unexpected := countSessionAdmissionResults(results)
	if unexpected != nil {
		t.Fatalf("overlapping successor returned unexpected error: %v", unexpected)
	}
	if claimed != 1 || busy != 1 {
		t.Fatalf("session admission was not serialized: claimed=%d busy=%d", claimed, busy)
	}
}

func claimContinuationAfter(start <-chan struct{}, results chan<- error, wg *sync.WaitGroup, td *taskdir.TaskDir, req *task.TaskRecord) {
	defer wg.Done()
	<-start
	results <- td.ClaimSession(req.Provider, req.PriorSession.ConversationID)
}

func countSessionAdmissionResults(results <-chan error) (claimed, busy int, unexpected error) {
	for err := range results {
		switch {
		case err == nil:
			claimed++
		case errors.Is(err, task.ErrSessionBusy):
			busy++
		default:
			unexpected = err
		}
	}
	return claimed, busy, unexpected
}

func TestContinuationReleaseFailurePreservesWinnerAndRetries(t *testing.T) {
	store, td, req := newAppTestTaskWithPrior(t, false, testPriorSession())
	defer closeAppTestTask(t, store, td)
	successor, _ := newAppTaskInStore(t, store, strings.Repeat("4", 32), false, testPriorSession())
	defer func() {
		closeErr := successor.Close()
		if closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	rewriteAppProviderExecutable(t, td, "/usr/bin/printf")
	releaseErr := errors.New("release durability failed")
	store.SetFaultInjector(&releaseOnlyFailureInjector{err: releaseErr})
	profile := appTestExecutionProfile(req, "/usr/bin/printf", []string{"answer\n"})
	result := runProvider(newResponse("dispatch"), td, req, profile, nil, Dependencies{})
	if result.code != 1 || !errors.Is(result.err, releaseErr) {
		t.Fatalf("release failure was not operational: %+v", result)
	}
	if result.response.Outcome == nil || result.response.Publication != task.PublicationCommitted.String() {
		t.Fatalf("release failure lost terminal winner: %+v", result.response)
	}
	if err := successor.ClaimSession(req.Provider, req.PriorSession.ConversationID); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("failed release freed session: %v", err)
	}
	store.SetFaultInjector(nil)
	retry := runRunner(runnerArguments{Root: store.Root, TaskID: req.TaskID}, Dependencies{})
	if retry.code != 0 || retry.err != nil || retry.response.Outcome == nil {
		t.Fatalf("terminal release retry failed: %+v", retry)
	}
	if err := successor.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatalf("successor could not claim after release retry: %v", err)
	}
}

func testPriorSession() *task.PriorSession {
	return &task.PriorSession{Provider: "fixture:test", ConversationID: "continuation", PredecessorTaskID: strings.Repeat("a", 32)}
}

func rewriteAppProviderExecutable(t *testing.T, td *taskdir.TaskDir, executable string) {
	t.Helper()
	path := filepath.Join(td.Dir, "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta task.MetaRecord
	if err = json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	meta.ProviderExecutable = executable
	data, err = task.MarshalCanonical(&meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func appTestExecutionProfile(req *task.TaskRecord, executable string, arguments []string) PreparedProfile {
	return PreparedProfile{
		Plan:            execution.Plan{Executable: executable, Arguments: arguments, Directory: req.CanonicalCwd, Predicate: task.FixturePredicateRef()},
		ObservedVersion: "fixture-v2",
		Effective:       task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("app-test"))},
	}
}

type releaseOnlyFailureInjector struct {
	taskdir.BaseFaultInjector
	err error
}

func (i *releaseOnlyFailureInjector) OnLink(_, destination string) error {
	if strings.HasSuffix(destination, ".release.json") {
		return i.err
	}
	return nil
}

var _ taskdir.FaultInjector = (*releaseOnlyFailureInjector)(nil)

var errInjectedIdentity = errors.New("identity setup failed")
