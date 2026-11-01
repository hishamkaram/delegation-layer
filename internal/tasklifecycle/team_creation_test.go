package tasklifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestTeamCreationConflictRequiresOnlyKnownConflictCauses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"request conflict", task.ErrRequestConflict, true},
		{"wrapped conflict", fmt.Errorf("creation: %w", task.ErrRequestConflict), true},
		{"joined conflicts", errors.Join(task.ErrRequestConflict, task.ErrRequestConflict), true},
		{"nested conflicts", fmt.Errorf("creation: %w", errors.Join(task.ErrRequestConflict, fmt.Errorf("record: %w", task.ErrRequestConflict))), true},
		{"counterfeit message", errors.New(task.ErrRequestConflict.Error()), false},
		{"IO fault", io.ErrUnexpectedEOF, false},
		{"joined IO fault", errors.Join(task.ErrRequestConflict, io.ErrUnexpectedEOF), false},
		{"joined unlock fault", errors.Join(task.ErrRequestConflict, fmt.Errorf("unlock: %w", os.ErrPermission)), false},
		{"nested mixed fault", fmt.Errorf("creation: %w", errors.Join(errors.Join(task.ErrRequestConflict, task.ErrRequestConflict), io.ErrClosedPipe)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := teamCreationConflict(tc.err); got != tc.want {
				t.Fatalf("teamCreationConflict(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func teamRaceCandidate(fixture teamDispatchFixture) task.TaskRecord {
	candidate := *fixture.request
	candidate.Team = fixture.request.Team.Clone()
	candidate.Team.TeamRevision++
	candidate.Team.TeamName = "Updated team"
	candidate.Team.Roles[0].Responsibility = "Updated responsibility."
	candidate.Provider = "future-agent:exec"
	candidate.RequestedConfig.Model, candidate.RequestedConfig.Effort = "member-model-v2", "member-effort-v2"
	return candidate
}

func TestTeamCreationRebasesMatchingIntentToPersistedWinner(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	path := filepath.Join(fixture.td.Dir, "task.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate := teamRaceCandidate(fixture)
	if err = rebaseExistingTeam(fixture.td, &candidate); err != nil {
		t.Fatalf("matching raw intent did not adopt winner: %v", err)
	}
	if !reflect.DeepEqual(&candidate, fixture.request) {
		t.Fatalf("loser retained mutable defaults or revision: %+v, want %+v", candidate, fixture.request)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("adoption rewrote immutable winner: %v", err)
	}
	candidate.Team.Target.Team.Project.Path = filepath.Join(candidate.CanonicalCwd, "caller-mutation")
	candidate.Team.Roles[0].Name = "Changed caller role"
	stored, err := fixture.td.RecordedRequest()
	if err != nil || !reflect.DeepEqual(stored, fixture.request) {
		t.Fatalf("adopted candidate shares persisted context: %+v, %v", stored, err)
	}
}

func TestTeamCreationRebaseRejectsNonDefaultIdentityConflicts(t *testing.T) {
	changes := []struct {
		name string
		edit func(*task.TaskRecord)
	}{
		{"brief digest", func(r *task.TaskRecord) { r.BriefSHA256 = task.ComputeSHA256([]byte("different brief")) }},
		{"brief length", func(r *task.TaskRecord) { r.BriefLength++ }},
		{"cwd", func(r *task.TaskRecord) { r.CanonicalCwd = filepath.Join(r.CanonicalCwd, "other") }},
		{"member", func(r *task.TaskRecord) { r.Team.Target.MemberID = "secondary" }},
		{"catalog", func(r *task.TaskRecord) { r.Team.Target.CatalogID = strings.Repeat("f", 32) }},
		{"raw model", func(r *task.TaskRecord) { r.Team.Intent.Config.Model = "member-model-v1" }},
		{"raw effort", func(r *task.TaskRecord) { r.Team.Intent.Config.Effort = "experimental" }},
		{"raw provider", func(r *task.TaskRecord) { r.Team.Intent.Provider = "fixture:test" }},
		{"raw auto", func(r *task.TaskRecord) { r.Team.Intent.Auto = true }},
		{"raw budget", func(r *task.TaskRecord) { r.Team.Intent.Config.Budget = "2m" }},
		{"workspace", func(r *task.TaskRecord) {
			r.Workspace = &task.WorkspaceBinding{ID: strings.Repeat("c", 32), Reservation: 1}
		}},
		{"prior session", func(r *task.TaskRecord) {
			r.PriorSession = &task.PriorSession{Provider: "fixture:test", ConversationID: "session", PredecessorTaskID: strings.Repeat("c", 32)}
		}},
	}
	fixture := newTeamDispatchFixture(t)
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			candidate := teamRaceCandidate(fixture)
			change.edit(&candidate)
			if err := rebaseExistingTeam(fixture.td, &candidate); !errors.Is(err, task.ErrRequestConflict) {
				t.Fatalf("conflicting creation error = %v, want ErrRequestConflict", err)
			}
			stored, err := fixture.td.RecordedRequest()
			if err != nil || !reflect.DeepEqual(stored, fixture.request) {
				t.Fatalf("conflict changed persisted winner: %+v, %v", stored, err)
			}
		})
	}
}

func TestTeamCreationRebaseClonesWinnerContext(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	old := *fixture.request
	old.Team = fixture.request.Team.Clone()
	candidate := teamRaceCandidate(fixture)
	if err := rebaseTeamRequest(&candidate, &old); err != nil {
		t.Fatal(err)
	}
	candidate.Team.Target.Team.Project.Path = filepath.Join(candidate.CanonicalCwd, "candidate")
	candidate.Team.Roles[0].Responsibility = "Candidate mutation."
	if !reflect.DeepEqual(old.Team, fixture.request.Team) {
		t.Fatal("rebased candidate aliases winning context")
	}
}

type teamCreateInput struct {
	request task.TaskRecord
	meta    task.MetaRecord
}

type teamCreateResult struct {
	td  *taskdir.TaskDir
	err error
}

type teamBeforeRecordFault struct {
	taskdir.BaseFaultInjector
	err error
}

func (fault teamBeforeRecordFault) OnStageWrite(string, []byte) (int, error) {
	return 0, fault.err
}

// Gate the first task admission lock attempt, before flock acquires it. This
// is outside the managed use owner and cannot block one owner waiting for another.
type teamCreateLockGate struct {
	taskdir.BaseFaultInjector
	ctx     context.Context
	entered chan<- struct{}
	release <-chan struct{}
	once    sync.Once
	err     error
}

var _ taskdir.LockFaultInjector = (*teamCreateLockGate)(nil)

func (*teamCreateLockGate) OnLockFileBarrier(string) error      { return nil }
func (*teamCreateLockGate) OnLockParentDirBarrier(string) error { return nil }

func (gate *teamCreateLockGate) OnFlock(path string, _ int) error {
	if filepath.Base(path) != ".admission.lock" {
		return nil
	}
	gate.once.Do(func() {
		gate.entered <- struct{}{}
		select {
		case <-gate.release:
		case <-gate.ctx.Done():
			gate.err = gate.ctx.Err()
		}
	})
	return gate.err
}

func teamConcurrentInputs(fixture teamDispatchFixture) []teamCreateInput {
	inputs := make([]teamCreateInput, 2)
	for index := range inputs {
		request, meta := *fixture.request, *fixture.meta
		request.TaskID, meta.TaskID = strings.Repeat("c", 32), strings.Repeat("c", 32)
		request.Team = fixture.request.Team.Clone()
		request.Team.TeamRevision += int64(index)
		request.RequestedConfig.Model = fmt.Sprintf("member-default-%d", index)
		meta.RequestedConfig = request.RequestedConfig
		inputs[index] = teamCreateInput{request: request, meta: meta}
	}
	return inputs
}

func openTeamCreationStore(t *testing.T, root string) *taskdir.Store {
	t.Helper()
	store, err := taskdir.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func runTeamConcurrentCreates(t *testing.T, stores []*taskdir.Store, inputs []teamCreateInput, brief []byte) []teamCreateResult {
	t.Helper()
	// The deadline is a hang watchdog, not a dispatch latency requirement.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered, release := make(chan struct{}, len(stores)), make(chan struct{})
	results := make([]teamCreateResult, len(stores))
	var workers sync.WaitGroup
	for index, store := range stores {
		store.SetFaultInjector(&teamCreateLockGate{ctx: ctx, entered: entered, release: release})
		workers.Go(func() {
			td, err := store.CreateTask(inputs[index].request.TaskID, &inputs[index].request, brief, &inputs[index].meta)
			results[index] = teamCreateResult{td: td, err: err}
		})
	}
	arrivals := 0
	for arrivals < len(stores) && ctx.Err() == nil {
		select {
		case <-entered:
			arrivals++
		case <-ctx.Done():
		}
	}
	close(release)
	workers.Wait()
	for index, store := range stores {
		store.SetFaultInjector(nil)
		if td := results[index].td; td != nil {
			t.Cleanup(func() {
				if err := td.Close(); err != nil {
					t.Error(err)
				}
			})
		}
	}
	if arrivals != len(stores) {
		t.Fatalf("only %d/%d creators reached task admission; results=%+v", arrivals, len(stores), results)
	}
	return results
}

func TestTeamCreationConcurrentCandidatesKeepOneImmutableWinner(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	inputs := teamConcurrentInputs(fixture)
	// Interrupt initialization before any prepared record, leaving production
	// stable locks in place so this case isolates record arbitration.
	interrupted := errors.New("interrupted before prepared records")
	fixture.store.SetFaultInjector(&teamBeforeRecordFault{err: interrupted})
	td, err := fixture.store.CreateTask(inputs[0].request.TaskID, &inputs[0].request, fixture.brief, &inputs[0].meta)
	fixture.store.SetFaultInjector(nil)
	if td != nil {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("interrupted initializer returned a prepared task")
	}
	if !errors.Is(err, interrupted) {
		t.Fatalf("initialization did not stop before prepared records: %v", err)
	}
	stores := []*taskdir.Store{openTeamCreationStore(t, fixture.store.Root), openTeamCreationStore(t, fixture.store.Root)}
	results := runTeamConcurrentCreates(t, stores, inputs, fixture.brief)
	winner := -1
	for index, result := range results {
		if result.err == nil {
			if result.td == nil {
				t.Fatal("successful creation returned no task handle")
			}
			if winner >= 0 {
				t.Fatal("different effective candidates both became immutable winners")
			}
			winner = index
		} else if !errors.Is(result.err, task.ErrLockBusy) && !teamCreationConflict(result.err) {
			t.Fatalf("unexpected create failure for candidate %d: %v", index, result.err)
		}
	}
	if winner < 0 {
		t.Fatalf("neither candidate became a winner: %+v", results)
	}
	requireTeamCreationWinner(t, stores, inputs, results[winner].td, winner, fixture.brief)
}

func requireTeamCreationWinner(t *testing.T, stores []*taskdir.Store, inputs []teamCreateInput, td *taskdir.TaskDir, winner int, brief []byte) {
	t.Helper()
	saved, meta, err := td.PreparedRecords()
	if err != nil || !reflect.DeepEqual(saved, &inputs[winner].request) {
		t.Fatalf("prepared winner differs from winning candidate: %+v, %v", saved, err)
	}
	before, err := os.ReadFile(filepath.Join(td.Dir, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.SpecSHA256 != task.ComputeSHA256(before) {
		t.Fatal("prepared metadata does not bind the immutable winner bytes")
	}
	loser := 1 - winner
	other, err := stores[loser].CreateTask(saved.TaskID, &inputs[loser].request, brief, &inputs[loser].meta)
	if other != nil {
		if closeErr := other.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("losing effective defaults replaced the winner")
	}
	if !teamCreationConflict(err) {
		t.Fatalf("loser did not preserve a pure request conflict: %v", err)
	}
	if err = rebaseTeamRequest(&inputs[loser].request, saved); err != nil || !reflect.DeepEqual(&inputs[loser].request, saved) {
		t.Fatalf("matching raw intent failed to adopt canonical winner: %+v, %v", inputs[loser].request, err)
	}
	after, err := os.ReadFile(filepath.Join(td.Dir, "task.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("losing retry rewrote immutable task bytes: %v", err)
	}
	for _, name := range []string{"submit.json", "provider.start"} {
		if _, err := os.Stat(filepath.Join(td.Dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("record arbitration acquired launch authority via %s: %v", name, err)
		}
	}
}

func TestTeamCreationReopensPersistedTaskBeforeCaptureRecovery(t *testing.T) {
	fixture := newTeamDispatchFixture(t)
	input := teamConcurrentInputs(fixture)[0]
	writer := openTeamCreationStore(t, fixture.store.Root)
	resource, reservation := teamCaptureReservation(t, writer, input.request)
	input.request.Workspace = &task.WorkspaceBinding{
		ID: resource.Request().WorkspaceID, Reservation: reservation.Sequence, RequestSHA256: reservation.Subject.SHA256, BudgetNanos: int64(time.Minute),
	}
	td, err := writer.CreateTask(input.request.TaskID, &input.request, fixture.brief, &input.meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, obligationErr := resource.CaptureObligation(t.Context(), reservation.Sequence); !workspace.OperationAbsent(obligationErr) {
		t.Fatalf("pre-capture crash fixture already has an obligation: %v", obligationErr)
	}
	if err = errors.Join(td.Close(), resource.Close(), writer.Close()); err != nil {
		t.Fatal(err)
	}
	reader := openTeamCreationStore(t, fixture.store.Root)
	reopened, err := reader.OpenTask(input.request.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reopened.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	saved, meta, err := reopened.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateExistingDispatchRequest("", "", reader, *saved, saved, meta); err != nil {
		t.Fatalf("existing-request consumer did not repair missing capture: %v", err)
	}
	requireTeamRecoveredCapture(t, reader, *saved, reservation.Sequence)
}

func teamCaptureReservation(t *testing.T, store *taskdir.Store, request task.TaskRecord) (*workspace.Resource, workspace.Reservation) {
	t.Helper()
	digest := task.ComputeSHA256([]byte("raw Team workspace intent"))
	resource, err := workspace.EstablishResource(t.Context(), store, workspace.ResourceRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: strings.Repeat("d", 32), TaskID: request.TaskID,
		RequestSHA256: digest, SourceCwd: request.CanonicalCwd, Container: workspace.ContainerPath(store.Root, store.RootID), BudgetNanos: int64(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := resource.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	reservation, err := resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: digest})
	if err != nil {
		t.Fatal(err)
	}
	return resource, reservation
}

func requireTeamRecoveredCapture(t *testing.T, store *taskdir.Store, request task.TaskRecord, sequence uint64) {
	t.Helper()
	resource, err := workspace.LoadResource(t.Context(), store, request.Workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := resource.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	obligation, err := resource.CaptureObligation(t.Context(), sequence)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(store.Root, "tasks", request.TaskID, "task.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := workspace.Subject{Kind: workspace.SubjectTask, ID: request.TaskID, SHA256: task.ComputeSHA256(data)}
	if obligation.Subject != want || obligation.BudgetNanos != request.Workspace.BudgetNanos {
		t.Fatalf("recovered obligation does not bind canonical winner: %+v, want %+v", obligation, want)
	}
}
