package infrastructure

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// The host below supplies data fixtures only. Native eligibility belongs to
// the fixed app host; no native command is executed in these election tests.
type producerTestHost struct {
	artifacts Artifacts
	calls     int
	prepare   func(context.Context, *taskdir.Store) error
}

func (h *producerTestHost) PrepareOutsideArtifacts(ctx context.Context, target *taskdir.Store) (Artifacts, error) {
	h.calls++
	if h.prepare != nil {
		if err := h.prepare(ctx, target); err != nil {
			return Artifacts{}, err
		}
	}
	return h.artifacts, nil
}

type producerDataFixture struct {
	journal *intentJournal
	lease   *Lease
	request EstablishmentRequest
	host    *producerTestHost
}

func producerFixture(t *testing.T) producerDataFixture {
	t.Helper()
	requireComponentEntryTestHost(t)
	j, lease := journalFixture(t)
	artifacts := Artifacts{Runner: filepath.Join(j.pin.Root(), "runner"), RunnerSHA256: task.ComputeSHA256([]byte("runner")), Client: filepath.Join(j.pin.Root(), "pueue"), ClientSHA256: task.ComputeSHA256([]byte("client")), Daemon: filepath.Join(j.pin.Root(), "pueued"), DaemonSHA256: task.ComputeSHA256([]byte("daemon"))}
	record := contextForJournal(t, j)
	record.OS = runtime.GOOS
	request := EstablishmentRequest{Context: record, Deadline: time.Now().Add(time.Minute), Requirements: ArtifactRequirements{RequiredRuntimeSHA256: artifacts.RunnerSHA256, RequiredClientSHA256: artifacts.ClientSHA256, RequiredDaemonSHA256: artifacts.DaemonSHA256}}
	return producerDataFixture{j, lease, request, &producerTestHost{artifacts: artifacts}}
}

func ownProducerAttempt(t *testing.T, attempt *StartupAttempt) {
	t.Helper()
	if attempt != nil {
		t.Cleanup(func() {
			if err := attempt.Close(); err != nil {
				t.Error(err)
			}
		})
	}
}

func seedProducerIntent(t *testing.T, f producerDataFixture, sequence uint64, expired bool) StartupIntent {
	t.Helper()
	intent := journalIntent(t, f.journal, sequence)
	intent.Inputs.Artifacts = f.host.artifacts
	reference, _, err := f.journal.stageContext(t.Context(), f.request.Context)
	if err != nil {
		t.Fatal(err)
	}
	intent.Inputs.Context = reference
	intent.Epoch = task.ComputeSHA256([]byte(intent.Epoch + string(rune(sequence))))[:32]
	intent.AcceptedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	intent.Deadline = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	if expired {
		intent.Deadline = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	}
	refreshIntentDigest(t, &intent)
	requireJournalPut(t, f.journal, f.lease, intent)
	return intent
}

func releaseProducerFixtureLease(t *testing.T, f producerDataFixture) {
	t.Helper()
	if err := f.lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProducerBusyEquivalentJoinsWithoutPreparation(t *testing.T) {
	f := producerFixture(t)
	intent := seedProducerIntent(t, f, 1, false)
	request := f.request
	// New requests obey the startup window; joining still preserves the older deadline.
	request.Deadline = time.Now().Add(30 * time.Second)
	attempt, err := EstablishOutside(t.Context(), f.journal.store, request, f.host)
	ownProducerAttempt(t, attempt)
	if err != nil || attempt == nil || attempt.Reference() != settlementReference(t, intent) || f.host.calls != 0 || attempt.NativeCompletion() != nil {
		t.Fatalf("busy join: host calls=%d err=%v", f.host.calls, err)
	}
	if attempt.intent.Deadline != intent.Deadline {
		t.Fatal("join renewed original startup deadline")
	}
}

func TestProducerFreeFutureIntentJoinsWithoutNewStart(t *testing.T) {
	f := producerFixture(t)
	intent := seedProducerIntent(t, f, 1, false)
	releaseProducerFixtureLease(t, f)
	starts := 0
	attempt, err := establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }})
	ownProducerAttempt(t, attempt)
	if err != nil || attempt == nil || attempt.Reference() != settlementReference(t, intent) || starts != 0 || f.host.calls != 0 {
		t.Fatalf("future join: starts=%d prepare=%d err=%v", starts, f.host.calls, err)
	}
	lease, acquired, err := f.journal.tryAcquire(t.Context())
	if lease != nil {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if err != nil || !acquired {
		t.Fatalf("passive join retained election: acquired=%t err=%v", acquired, err)
	}
}

func TestProducerBirthUsesCheckedRawScopeNotSelectedConfiguration(t *testing.T) {
	f := producerFixture(t)
	releaseProducerFixtureLease(t, f)
	f.request.Context.ControlEnvironment = []string{"DELEGATION_LAYER_PROCESS_SCOPE=pg:11111111111111111111111111111111", "DELEGATE_INFRASTRUCTURE_ORIGIN=foreign selected context", "SELECTED_ONLY=configuration"}
	original := f.request.Context.Clone()
	raw := os.Environ()
	refused := errors.New("owned native boundary refusal")
	starts := 0
	attempt, err := establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(command *exec.Cmd) error {
		starts++
		if !slices.Equal(command.Env, raw) {
			t.Error("birth used selected control scope instead of checked raw native scope")
		}
		if len(command.ExtraFiles) != 1 || command.SysProcAttr == nil {
			t.Error("birth lost fixed FD3 or independent native session")
		}
		invocation, handled, parseErr := parseOwnerInvocation(command.Args[1:])
		if parseErr != nil || !handled || invocation.root != f.journal.pin.Root() {
			t.Error("birth changed fixed owner argv")
		}
		return refused
	}})
	ownProducerAttempt(t, attempt)
	if !errors.Is(err, refused) || attempt == nil || starts != 1 || attempt.Reference().Sequence != 1 {
		t.Fatalf("birth refusal lost durable acceptance: starts=%d err=%v", starts, err)
	}
	stored, readErr := f.journal.readIntent(t.Context(), 1)
	if readErr != nil {
		t.Fatal(readErr)
	}
	contextRecord, readErr := readLaunchContext(t.Context(), attempt.reader, stored.Inputs.Context)
	if readErr != nil || !slices.Equal(contextRecord.ControlEnvironment, original.ControlEnvironment) {
		t.Fatalf("accepted selected context changed: %v", readErr)
	}
}

func TestProducerRequiresAllPriorSettlementsBeforeSuccessor(t *testing.T) {
	f := producerFixture(t)
	seedProducerIntent(t, f, 1, true)
	seedProducerIntent(t, f, 2, true)
	releaseProducerFixtureLease(t, f)
	starts := 0
	attempt, err := establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }})
	ownProducerAttempt(t, attempt)
	if err == nil || starts != 0 || f.host.calls != 0 {
		t.Fatalf("unsettled history allowed successor: starts=%d prepare=%d err=%v", starts, f.host.calls, err)
	}
	if _, readErr := f.journal.readIntent(t.Context(), 3); !task.RecordAbsent(readErr) {
		t.Fatalf("successor created despite unsettled predecessor: %v", readErr)
	}
}

func TestProducerExpiredUnassumedSettlesBeforeNewAcceptance(t *testing.T) {
	f := producerFixture(t)
	old := seedProducerIntent(t, f, 1, true)
	releaseProducerFixtureLease(t, f)
	refusal := errors.New("stop before actual native Start")
	attempt, err := establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(*exec.Cmd) error { return refusal }})
	ownProducerAttempt(t, attempt)
	if !errors.Is(err, refusal) || attempt == nil || attempt.Reference().Sequence != 2 {
		t.Fatalf("expired successor: %v", err)
	}
	record, readErr := f.journal.readSettlement(t.Context(), settlementReference(t, old))
	if readErr != nil || record.Reason != settlementUnassumedExpired {
		t.Fatalf("prior attempt not canonically settled: %v", readErr)
	}
}

func TestProducerMismatchAndCancellationRefuseWithoutPreparation(t *testing.T) {
	for _, fault := range []string{"runtime", "context", "canceled"} {
		t.Run(fault, func(t *testing.T) {
			f := producerFixture(t)
			seedProducerIntent(t, f, 1, false)
			request := f.request
			ctx := t.Context()
			switch fault {
			case "runtime":
				request.Requirements.RequiredRuntimeSHA256 = task.ComputeSHA256([]byte("other runtime"))
			case "context":
				request.Context.ControlEnvironment = append(slices.Clone(request.Context.ControlEnvironment), "DIFFERENT=context")
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			attempt, err := EstablishOutside(ctx, f.journal.store, request, f.host)
			ownProducerAttempt(t, attempt)
			if err == nil || f.host.calls != 0 {
				t.Fatalf("invalid join prepared target: %v", err)
			}
		})
	}
}

func TestProducerObservedChildUsesNativeIdentitySemantics(t *testing.T) {
	attempt := &StartupAttempt{child: &producerChild{pid: 7, identity: ProcessIdentity{PID: 7, StartToken: "darwin-v2:10:20:77"}}}
	if err := attempt.matchObservedChild(StartupAssumption{Process: ProcessIdentity{PID: 7, StartToken: "darwin-v2:11:21:77"}}); err != nil {
		t.Fatalf("same native uniqueid after exec refused: %v", err)
	}
	for _, identity := range []ProcessIdentity{{PID: 8, StartToken: "darwin-v2:11:21:77"}, {PID: 7, StartToken: "darwin-v2:10:20:78"}, {PID: 7, StartToken: "darwin-v2:10:20:bad"}} {
		if err := attempt.matchObservedChild(StartupAssumption{Process: identity}); err == nil {
			t.Fatal("different or malformed incarnation accepted")
		}
	}
}

func TestProducerBusyTerminalAttemptRefusesJoin(t *testing.T) {
	for _, settled := range []bool{false, true} {
		t.Run(fmt.Sprintf("settled=%t", settled), func(t *testing.T) {
			f := producerFixture(t)
			intent := seedProducerIntent(t, f, 1, true)
			if settled {
				if _, _, err := f.journal.settleStartup(t.Context(), f.lease, settlementReference(t, intent)); err != nil {
					t.Fatal(err)
				}
			}
			attempt, err := EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
			ownProducerAttempt(t, attempt)
			if !errors.Is(err, task.ErrLockBusy) || attempt != nil || f.host.calls != 0 {
				t.Fatalf("busy terminal attempt joined: attempt=%t err=%v", attempt != nil, err)
			}
		})
	}
}

func TestProducerObservationExpiredUnassumedRefusesPending(t *testing.T) {
	f := producerFixture(t)
	intent := seedProducerIntent(t, f, 1, true)
	attempt := &StartupAttempt{target: f.journal.store, pin: f.journal.pin, intent: intent}
	var err error
	attempt.reader, err = taskdir.OpenReadOnlyStoreContext(t.Context(), f.journal.pin.Root(), predicate.Default())
	ownProducerAttempt(t, attempt)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, ready, observeErr := attempt.Observe(ctx); !errors.Is(observeErr, context.DeadlineExceeded) || ready {
		t.Fatalf("expired unassumed remained pending: ready=%t err=%v", ready, observeErr)
	}
}

func TestProducerBusyCorruptSettlementPreservesFault(t *testing.T) {
	f := producerFixture(t)
	seedProducerIntent(t, f, 1, false)
	if err := f.journal.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(settlementName(1), startupSettlement{Protocol: ProtocolVersion})
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, attempt)
	if err == nil || errors.Is(err, task.ErrLockBusy) || attempt != nil || f.host.calls != 0 {
		t.Fatalf("corrupt settlement became clean busy/join: attempt=%t err=%v", attempt != nil, err)
	}
}

func TestProducerStartupWindowRefusesBeforeTargetWrites(t *testing.T) {
	f := producerFixture(t)
	releaseProducerFixtureLease(t, f)
	before := producerBudgetTree(t, f.journal.pin.Root())
	request := f.request
	request.Deadline = time.Now().Add(3 * time.Minute)
	starts := 0
	attempt, err := establishOutside(t.Context(), f.journal.store, request, f.host, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected native Start") }})
	ownProducerAttempt(t, attempt)
	if !errors.Is(err, ErrUnsupportedStartupBudget) || attempt != nil || f.host.calls != 0 || starts != 0 {
		t.Fatalf("oversized startup window accepted: attempt=%t prepare=%d starts=%d err=%v", attempt != nil, f.host.calls, starts, err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
		t.Fatal("startup-window refusal changed target bytes, modes or modification times")
	}
}

type producerBudgetPath struct {
	mode     fs.FileMode
	modified time.Time
	data     string
}

func producerBudgetTree(t *testing.T, root string) map[string]producerBudgetPath {
	t.Helper()
	result := map[string]producerBudgetPath{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		record := producerBudgetPath{mode: info.Mode(), modified: info.ModTime()}
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			record.data = string(data)
		}
		result[path] = record
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestProducerDeadlineWindowBoundaries(t *testing.T) {
	now := time.Date(2026, time.November, 4, 12, 0, 0, 0, time.UTC)
	// The verified default private-control window is 85 seconds, below this cap.
	for _, offset := range []time.Duration{time.Nanosecond, 85 * time.Second, MaximumStartupWindow - time.Nanosecond, MaximumStartupWindow} {
		if err := validateProducerDeadline(now.Add(offset), now); err != nil {
			t.Fatalf("within-window deadline refused: %v", err)
		}
	}
	if err := validateProducerDeadline(now.Add(MaximumStartupWindow+time.Nanosecond), now); !errors.Is(err, ErrUnsupportedStartupBudget) {
		t.Fatalf("above-window deadline accepted: %v", err)
	}
	for _, deadline := range []time.Time{now, now.Add(-time.Nanosecond)} {
		if err := validateProducerDeadline(deadline, now); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("elapsed deadline accepted: %v", err)
		}
	}
	if err := validateProducerDeadline(time.Time{}, now); !errors.Is(err, ErrIdentity) {
		t.Fatalf("zero deadline accepted: %v", err)
	}
}
