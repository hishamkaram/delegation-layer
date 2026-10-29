package sqlitestore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/internal/testchild"
)

// raceClaims starts one helper process per claim, releases them together
// after every helper has opened its Store, and returns their result lines.
func raceClaims(t *testing.T, root string, claims ...contract.Claim) []string {
	t.Helper()
	helpers := make([]*testchild.Child, 0, len(claims))
	for _, claim := range claims {
		helper := startHelper(t, helperClaim, root, testRootID, claim.ScheduleID,
			strconv.FormatInt(claim.ExpectedCursor.Unix(), 10), strconv.FormatInt(claim.Due.Unix(), 10), claim.TaskID)
		if line := helper.Line(t); line != "ready" {
			t.Fatalf("helper said %q before the barrier", line)
		}
		helpers = append(helpers, helper)
	}
	for _, helper := range helpers {
		helper.Release(t)
	}
	lines := make([]string, 0, len(helpers))
	for _, helper := range helpers {
		lines = append(lines, helper.Line(t))
		if code := helper.Finish(t); code != 0 {
			t.Fatalf("claim helper exited %d", code)
		}
	}
	return lines
}

func TestCompetingProcessesClaimADuplicateDueOnce(t *testing.T) {
	store, root := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	lines := raceClaims(t, root,
		testClaim(testScheduleID, testTaskID, 1_000, 2_000),
		testClaim(testScheduleID, otherTaskID, 1_000, 2_000))
	winner := requireOneGrant(t, lines, []string{testTaskID, otherTaskID})
	loserLine := fmt.Sprintf("outcome=%d task=%s conflict=false", contract.ClaimExisting, winner)
	for _, line := range lines {
		if line != grantLine(winner) && line != loserLine {
			t.Fatalf("results %q: the loser must see the winner as existing", lines)
		}
	}
	requireActiveTask(t, store, winner)
}

func TestCompetingProcessesForTheActiveReservationGrantOnce(t *testing.T) {
	store, root := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	lines := raceClaims(t, root,
		testClaim(testScheduleID, testTaskID, 1_000, 2_000),
		testClaim(testScheduleID, otherTaskID, 1_000, 3_000))
	winner := requireOneGrant(t, lines, []string{testTaskID, otherTaskID})
	for _, line := range lines {
		if line != grantLine(winner) && line != fmt.Sprintf("outcome=%d task= conflict=true", contract.ClaimUnknown) {
			t.Fatalf("results %q: the loser must conflict with zero ClaimUnknown", lines)
		}
	}
	requireActiveTask(t, store, winner)
}

func TestCompetingConnectionsInOneProcessGrantOnce(t *testing.T) {
	_, root := initializedStore(t)
	first, second := openStore(t, root), openStore(t, root)
	createSchedule(t, first, testScheduleID)
	type attempt struct {
		result contract.ClaimResult
		err    error
	}
	attempts := make(chan attempt, 2)
	start := make(chan struct{})
	for i, store := range []*Store{first, second} {
		claim := testClaim(testScheduleID, []string{testTaskID, otherTaskID}[i], 1_000, int64(2_000+i*1_000))
		ctx := testContext(t)
		go func() {
			<-start
			result, err := store.ClaimAndArm(ctx, claim)
			attempts <- attempt{result: result, err: err}
		}()
	}
	close(start)
	grants, conflicts := 0, 0
	for range 2 {
		got := <-attempts
		result, err := got.result, got.err
		switch {
		case err == nil && result.AuthorizesInvocation():
			grants++
		case errors.Is(err, contract.ErrConflict) && result == (contract.ClaimResult{}):
			conflicts++
		default:
			t.Fatalf("unexpected claim result %+v, %v", result, err)
		}
	}
	if grants != 1 || conflicts != 1 {
		t.Fatalf("%d grants and %d conflicts, want one each", grants, conflicts)
	}
}

func grantLine(taskID string) string {
	return fmt.Sprintf("outcome=%d task=%s conflict=false", contract.ClaimNewlyCommitted, taskID)
}

func requireOneGrant(t *testing.T, lines, candidates []string) string {
	t.Helper()
	winner, grants := "", 0
	for _, line := range lines {
		for _, candidate := range candidates {
			if line == grantLine(candidate) {
				winner = candidate
				grants++
			}
		}
	}
	if grants != 1 {
		t.Fatalf("results %q: %d grants, want exactly one", lines, grants)
	}
	return winner
}

func requireActiveTask(t *testing.T, store *Store, taskID string) {
	t.Helper()
	page, err := store.ActiveOccurrences(testContext(t), contract.ActivePage{Limit: contract.MaxPageLimit})
	if err != nil || len(page.Occurrences) != 1 || page.Occurrences[0].TaskID != taskID {
		t.Fatalf("active occurrences %+v, %v; want only %s", page, err, taskID)
	}
}

// interrupt runs a helper that leaves an uncommitted transaction behind a hot
// journal, and checks the journal it left.
func interrupt(t *testing.T, path, mode string) {
	t.Helper()
	helper := startHelper(t, helperInterrupt, path, mode)
	if code := helper.Finish(t); code != interruptExit {
		t.Fatalf("interrupt helper exited %d, want %d", code, interruptExit)
	}
	if err := requireHotJournal(path); err != nil {
		t.Fatal(err)
	}
	requireMode(t, path+"-journal", 0o600)
}

func TestInterruptedFreshInitializationIsRecoveredAsEmpty(t *testing.T) {
	root := newRoot(t)
	path := DatabasePath(root)
	if err := createEmptyDatabase(path); err != nil {
		t.Fatal(err)
	}
	interrupt(t, path, "fresh")
	if _, err := Open(testContext(t), root, testRootID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open after interrupted initialization err=%v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(path + "-journal"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("hot journal not rolled back: %v", err)
	}
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatalf("initialize after interruption: %v", err)
	}
	store := openStore(t, root)
	createSchedule(t, store, testScheduleID)
}

func TestInterruptedWriteToAPopulatedDatabaseIsRolledBack(t *testing.T) {
	store, root := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	createSchedule(t, store, otherSchedule)
	claimed(t, store, testClaim(testScheduleID, testTaskID, 1_000, 2_000))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	interrupt(t, DatabasePath(root), "populated")
	reopened := openStore(t, root)
	requireSchedule(t, reopened, contract.StateEnabled, 2_000)
	other, err := reopened.GetSchedule(testContext(t), otherSchedule)
	if err != nil || !other.Cursor.Equal(at(1_000)) {
		t.Fatalf("other schedule %+v, %v", other, err)
	}
	history, err := reopened.History(testContext(t), contract.HistoryPage{ScheduleID: testScheduleID, Limit: contract.MaxPageLimit})
	if err != nil || len(history.Entries) != 1 {
		t.Fatalf("history after recovery %+v, %v; want only the claimed occurrence", history, err)
	}
	if _, err := os.Stat(DatabasePath(root) + "-journal"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("hot journal not rolled back: %v", err)
	}
}
