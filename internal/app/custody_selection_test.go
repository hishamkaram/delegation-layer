package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	guardianfixture "github.com/hishamkaram/delegation-layer/internal/guardian/testsupport"
	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// custodyPrepareProbe replaces the pool identity owner's Prepare with scripted
// answers per root and records each call's deadline.
type custodyPrepareProbe struct {
	mu        sync.Mutex
	answers   map[string]error
	roots     []string
	deadlines []time.Time
}

func (p *custodyPrepareProbe) prepare(ctx context.Context, root string) (string, error) {
	deadline, _ := ctx.Deadline()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.roots = append(p.roots, root)
	p.deadlines = append(p.deadlines, deadline)
	if err := p.answers[root]; err != nil {
		return "", err
	}
	return strings.Repeat("e", 64), nil
}

func newProbedCustodySelector(t *testing.T, environment []string, answers func(preferred string) map[string]error) (lifetimeCustodySelector, *custodyPrepareProbe) {
	t.Helper()
	stateRoot := testsupportcore.CanonicalTempDir(t)
	selector := newLifetimeCustodySelector(stateRoot, strings.Repeat("1", 32), environment, time.Now().Add(time.Minute))
	probe := &custodyPrepareProbe{answers: answers(filepath.Join(stateRoot, lifetimeCustodyDirectory))}
	selector.prepare = probe.prepare
	return selector, probe
}

func TestLifetimeCustodySelectorFallsBackOnlyForCleanNoExec(t *testing.T) {
	temporary := testsupportcore.CanonicalTempDir(t)
	fallback := filepath.Join(temporary, lifetimeCustodyTemporaryPrefix+strings.Repeat("1", 32))
	cleanNoExec := fmt.Errorf("prepare: %w", errors.Join(fmt.Errorf("%w: root", verifiedexec.ErrPoolNoExec)))
	otherFault := errors.New("permission denied")
	tests := []custodySelectionCase{
		{name: "preferred usable", wantRoots: 1},
		{name: "clean noexec", preferred: cleanNoExec, wantRoots: 2, wantRoot: fallback},
		{name: "noexec joined with another cause", preferred: errors.Join(cleanNoExec, otherFault), wantRoots: 1, wantErrs: []error{verifiedexec.ErrPoolNoExec, otherFault}},
		{name: "other refusal", preferred: otherFault, wantRoots: 1, wantErrs: []error{otherFault}},
		{name: "both unusable", preferred: cleanNoExec, fallback: otherFault, wantRoots: 2, wantErrs: []error{verifiedexec.ErrPoolNoExec, otherFault}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) { requireCustodySelection(t, temporary, fallback, test) })
	}
}

type custodySelectionCase struct {
	name      string
	preferred error
	fallback  error
	wantRoots int
	wantRoot  string
	wantErrs  []error
}

func requireCustodySelection(t *testing.T, temporary, fallback string, test custodySelectionCase) {
	t.Helper()
	selector, probe := newProbedCustodySelector(t, []string{"TMPDIR=" + temporary}, func(preferred string) map[string]error {
		return map[string]error{preferred: test.preferred, fallback: test.fallback}
	})
	placement, err := selector.selectPlacement(context.Background())
	if len(probe.roots) != test.wantRoots {
		t.Fatalf("prepared roots %v, want %d", probe.roots, test.wantRoots)
	}
	if test.wantErrs != nil {
		for _, want := range test.wantErrs {
			if !errors.Is(err, want) {
				t.Fatalf("selection error %v does not keep %v", err, want)
			}
		}
		if placement != nil {
			t.Fatalf("refused selection returned placement %+v", placement)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	want := test.wantRoot
	if want == "" {
		want = filepath.Join(selector.stateRoot, lifetimeCustodyDirectory)
	}
	if placement.PoolRoot != want || task.ValidateLifetimeCustodyPlacement(placement) != nil {
		t.Fatalf("placement %+v, want valid placement at %s", placement, want)
	}
}

func TestLifetimeCustodySelectorUsesCapturedTemporaryRootOnce(t *testing.T) {
	base := testsupportcore.CanonicalTempDir(t)
	if err := os.Mkdir(filepath.Join(base, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	environment := []string{"TMPDIR=alias"}
	selector, probe := newProbedCustodySelector(t, environment, func(preferred string) map[string]error {
		return map[string]error{preferred: verifiedexec.ErrPoolNoExec}
	})
	// The relative TMPDIR resolves against the captured working directory, and
	// a later change to the caller's environment is never reread.
	selector.cwd, selector.cwdErr = base, nil
	environment[0] = "TMPDIR=/elsewhere"
	placement, err := selector.selectPlacement(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "real", lifetimeCustodyTemporaryPrefix+strings.Repeat("1", 32)); placement.PoolRoot != want || probe.roots[1] != want {
		t.Fatalf("fallback root %q (prepared %v), want canonical %q", placement.PoolRoot, probe.roots, want)
	}

	selector.cwdErr = errors.New("working directory unavailable")
	if _, err = selector.selectPlacement(context.Background()); !errors.Is(err, selector.cwdErr) || !errors.Is(err, verifiedexec.ErrPoolNoExec) {
		t.Fatalf("relative TMPDIR without a working directory: %v", err)
	}
}

func TestLifetimeCustodySelectorBoundsEveryPrepareByRunDeadline(t *testing.T) {
	selector, probe := newProbedCustodySelector(t, []string{"TMPDIR=" + testsupportcore.CanonicalTempDir(t)}, func(preferred string) map[string]error {
		return map[string]error{preferred: verifiedexec.ErrPoolNoExec}
	})
	if _, err := selector.selectPlacement(context.Background()); err != nil {
		t.Fatal(err)
	}
	for index, deadline := range probe.deadlines {
		if !deadline.Equal(selector.deadline) {
			t.Fatalf("prepare %d deadline %v, want run deadline %v", index, deadline, selector.deadline)
		}
	}
	earlier := time.Now().Add(time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), earlier)
	defer cancel()
	probe.deadlines = nil
	if _, err := selector.selectPlacement(ctx); err != nil {
		t.Fatal(err)
	}
	if !probe.deadlines[0].Equal(earlier) {
		t.Fatalf("prepare deadline %v, want the caller's earlier %v", probe.deadlines[0], earlier)
	}
	selector.deadline = time.Now().Add(-time.Second)
	selector.prepare = prepareLifetimeCustodyPool
	if _, err := selector.selectPlacement(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("selection after the run deadline: %v, want deadline exceeded", err)
	}
	if _, err := os.Lstat(filepath.Join(selector.stateRoot, lifetimeCustodyDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired selection created the pool: %v", err)
	}
}

// TestLifetimeCustodySelectorPersistsStateRootPlacementThroughGuardian drives
// the production selector through the guardian and both production journal
// families, then releases a bound view only once closure is proven, including
// after a guardian Run error.
func TestLifetimeCustodySelectorPersistsStateRootPlacementThroughGuardian(t *testing.T) {
	for name, newStore := range custodyContractStores() {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			closure, ok := s.records.(guardian.ClosureRecords)
			if !ok {
				t.Fatalf("%T is not a closure record family", s.records)
			}
			selector := newLifetimeCustodySelector(s.store.Root, s.rootID, nil, s.deadline)
			fixture := guardianfixture.NewRecordFixture(t, s.rootID, s.taskID, s.ownerKind, s.deadline, s.records, s.acquire,
				func(options *guardian.Options) { options.SelectPlacement = selector.selectPlacement })
			fixture.Hello(t)
			registration, _ := readCustodyContractRegistration(t, s)
			want := filepath.Join(s.store.Root, lifetimeCustodyDirectory)
			if placement := registration.CustodyPlacement; placement == nil || placement.PoolRoot != want {
				t.Fatalf("registered placement %+v, want state-root pool %s", placement, want)
			}
			bindCustodyTestView(t, registration)
			if err := releaseLifetimeCustody(t.Context(), closure); err != nil || !custodyTestPending(t, registration) {
				t.Fatalf("release before closure: error %v, pending %t; want no-op", err, custodyTestPending(t, registration))
			}
			fixture.CloseTask(t)
			fixture.ReleaseWorker()
			if err := fixture.WaitRun(t); err != nil {
				t.Fatal(err)
			}
			requireGuardianReleaseAfterRunError(t, s, closure, registration)
		})
	}
}

// requireGuardianReleaseAfterRunError proves the post-run release keeps the
// guardian's own error and touches nothing without closure records.
func requireGuardianReleaseAfterRunError(t *testing.T, s custodyContractStore, closure guardian.ClosureRecords, registration *task.LifetimeRegistrationRecord) {
	t.Helper()
	var failed *guardian.Guardian
	wantErr := failed.Run(context.Background())
	parsed := runnerArguments{TaskID: s.taskID, Inspection: true}
	if err := runGuardianWithWorkspaceCompletion(failed, lifetimeGuardianRunState{}, s.store, parsed, Dependencies{}); err == nil || err.Error() != wantErr.Error() || !custodyTestPending(t, registration) {
		t.Fatalf("run without closure records: error %v, want %v and no release", err, wantErr)
	}
	if err := runGuardianWithWorkspaceCompletion(failed, lifetimeGuardianRunState{closure: closure}, s.store, parsed, Dependencies{}); err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("failed run error %v, want unchanged %v", err, wantErr)
	}
	if custodyTestPending(t, registration) {
		t.Fatal("guardian did not release custody after a proven closure")
	}
}

func TestLifetimeGuardianSelectsCustodyOnlyWithClosureRecords(t *testing.T) {
	workspaceRun := lifetimeGuardianRunState{ownerKind: task.LifetimeOwnerWorkspace, deadline: time.Now().Add(time.Minute)}
	if workspaceRun.placementSelector(testsupportcore.CanonicalTempDir(t), strings.Repeat("1", 32), nil) != nil {
		t.Fatal("a workspace guardian run selects custody")
	}
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	taskRun := lifetimeGuardianRunState{ownerKind: task.LifetimeOwnerTask, closure: td, deadline: time.Now().Add(time.Minute)}
	if taskRun.placementSelector(store.Root, store.RootID, nil) == nil {
		t.Fatal("a task guardian run does not select custody")
	}
}
