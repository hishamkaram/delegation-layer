//go:build linux || darwin

package lifetime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// publishTestPID closes the complete payload before atomically exposing its
// final name. Readers must never observe the empty create-before-write window
// that race instrumentation can make visible.
func publishTestPID(path string, pid int) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func cleanupProcessGroupTestScope(t *testing.T, scope *Scope) {
	t.Helper()
	if destroyErr := scope.DestroyEmpty(); destroyErr != nil {
		t.Errorf("scope DestroyEmpty: %v", destroyErr)
	}
	if releaseErr := scope.Release(); releaseErr != nil {
		t.Errorf("scope Release: %v", releaseErr)
	}
}

func TestProcessGroupConfigureRejectsConflictingSysProcAttr(t *testing.T) {
	scope, _ := commandForProcessGroup(t, "sleep 1")
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })

	cmd := exec.Command("sleep", "1")
	attr := &syscall.SysProcAttr{Setpgid: true, Pgid: 1234}
	cmd.SysProcAttr = attr
	if err := scope.Configure(cmd); !errors.Is(err, ErrConflictingSysProc) {
		t.Fatalf("Configure error = %v, want ErrConflictingSysProc", err)
	}
	if cmd.SysProcAttr != attr || attr.Pgid != 1234 {
		t.Fatalf("Configure changed conflicting SysProcAttr: %#v", cmd.SysProcAttr)
	}
}

func TestProcessGroupConfigureReplacesReservedScopeMarker(t *testing.T) {
	ref, err := NewRef(KindProcessGroup, AssuranceCooperative, "pg:"+strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	scope, err := Open(ref)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("process-group scopes are unsupported on this platform")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.BindOwnerProcess(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })

	prefix := processScopeMarkerEnvironment + "="
	cmd := exec.Command("sleep", "1")
	cmd.Env = []string{"EXAMPLE=value", prefix + "stale", prefix + "older"}
	if err = scope.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	markerCount := 0
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, prefix) {
			markerCount++
			if entry != prefix+ref.Identifier() {
				t.Fatalf("scope marker = %q, want %q", entry, prefix+ref.Identifier())
			}
		}
	}
	if markerCount != 1 {
		t.Fatalf("scope marker count = %d, want 1", markerCount)
	}
}

func TestProcessGroupRejectsStaleLaunchIdentityBeforeSignal(t *testing.T) {
	scope, cmd := commandForProcessGroup(
		t, `trap ':' TERM; i=0; while [ "$i" -lt 5 ]; do sleep 1; i=$((i + 1)); done`,
	)
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	launchRef, err := scope.ObserveStarted(cmd.Process.Pid)
	if err != nil {
		waitErr := cmd.Wait()
		t.Logf("failed launch exited with: %v", waitErr)
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()
	staleRef, err := NewLaunchRef(launchRef.PID(), launchRef.StartToken()+":stale", launchRef.PGID())
	if err != nil {
		t.Fatal(err)
	}
	staleCtx, cancelStale := context.WithTimeout(context.Background(), 6*time.Second)
	if termErr := scope.Terminate(staleCtx, []LaunchRef{staleRef}); !errors.Is(termErr, ErrUnverifiableLaunch) {
		cancelStale()
		t.Fatalf("stale Terminate error = %v, want ErrUnverifiableLaunch", termErr)
	}
	cancelStale()
	exists, err := processGroupExists(launchRef.PGID())
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("stale identity termination removed the process group")
	}
	validCtx, cancelValid := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancelValid()
	if termErr := scope.Terminate(validCtx, []LaunchRef{launchRef}); termErr != nil {
		t.Fatalf("valid Terminate error = %v", termErr)
	}
	if waitErr := <-waitDone; waitErr == nil {
		t.Fatal("terminated command returned nil Wait error")
	}
}

func TestProcessGroupIdentityAllowsHistoricalPIDReuseWithCurrentRecordedOwner(t *testing.T) {
	const pid = 4242
	stale, err := NewLaunchRef(pid, "old-incarnation", pid)
	if err != nil {
		t.Fatal(err)
	}
	current, err := NewLaunchRef(pid, "current-incarnation", pid)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyLaunchIdentitiesWith(
		pid,
		[]LaunchRef{stale, current},
		func(observedPID int) (bool, string, error) {
			if observedPID != pid {
				t.Fatalf("observed PID = %d, want %d", observedPID, pid)
			}
			return true, "current-incarnation", nil
		},
		func(observedPID int) (int, error) {
			if observedPID != pid {
				t.Fatalf("group PID = %d, want %d", observedPID, pid)
			}
			return pid, nil
		},
	); err != nil {
		t.Fatalf("current recorded owner rejected after PID reuse: %v", err)
	}
}

func TestProcessGroupIdentityFailsClosedWhenOnlyHistoricalIdentityRemains(t *testing.T) {
	const pid = 4343
	stale, err := NewLaunchRef(pid, "old-incarnation", pid)
	if err != nil {
		t.Fatal(err)
	}
	err = verifyLaunchIdentitiesWith(
		pid,
		[]LaunchRef{stale},
		func(int) (bool, string, error) { return true, "unrecorded-incarnation", nil },
		func(int) (int, error) { return pid, nil },
	)
	if !errors.Is(err, ErrUnverifiableLaunch) {
		t.Fatalf("historical-only identity error = %v, want ErrUnverifiableLaunch", err)
	}
}

func TestProcessGroupTerminateIsIdempotentAfterEmpty(t *testing.T) {
	scope, cmd := commandForProcessGroup(t, "sleep 0.05")
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	launchRef, err := scope.ObserveStarted(cmd.Process.Pid)
	if err != nil {
		if waitErr := cmd.Wait(); waitErr != nil {
			t.Logf("failed launch exited with: %v", waitErr)
		}
		t.Fatal(err)
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		t.Fatal(waitErr)
	}
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 6*time.Second)
	if termErr := scope.Terminate(firstCtx, []LaunchRef{launchRef}); termErr != nil {
		cancelFirst()
		t.Fatalf("first empty Terminate error = %v", termErr)
	}
	cancelFirst()
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancelSecond()
	if termErr := scope.Terminate(secondCtx, []LaunchRef{launchRef}); termErr != nil {
		t.Fatalf("second empty Terminate error = %v", termErr)
	}
	empty, err := scope.Empty([]LaunchRef{launchRef})
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Fatal("Empty reported a terminated process group as populated")
	}
}

func TestProcessGroupEmptyRejectsMalformedLaunchRef(t *testing.T) {
	scope, _ := commandForProcessGroup(t, "sleep 1")
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })
	badRef, err := NewLaunchRef(12345, "linux:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scope.Empty([]LaunchRef{badRef}); err == nil {
		t.Fatal("Empty accepted an unverifiable launch reference")
	}
}

func TestProcessGroupDestroyEmptyIsNonDestructive(t *testing.T) {
	launchRef, err := NewLaunchRef(12345, "test:1", 12345)
	if err != nil {
		t.Fatal(err)
	}
	scope := &processGroupScope{
		identifier: "pg:" + strings.Repeat("c", 32),
		ownership:  processScopeOwnershipLedger{registered: []LaunchRef{launchRef}},
	}

	if err := scope.destroyEmpty(); err != nil {
		t.Fatalf("destroyEmpty: %v", err)
	}
	if len(scope.ownership.registered) != 1 || scope.ownership.registered[0] != launchRef {
		t.Fatalf("destroyEmpty changed process-group state: %#v", scope.ownership)
	}
}

func TestProcessGroupOwnerlessObservationNeverAuthorizesEmpty(t *testing.T) {
	scope := &processGroupScope{identifier: "pg:" + strings.Repeat("d", 32)}
	empty, err := scope.emptyContext(t.Context(), nil)
	if empty || !errors.Is(err, ErrUnverifiableLaunch) {
		t.Fatalf("ownerless Empty = (%t, %v), want false/ErrUnverifiableLaunch", empty, err)
	}
	if err = scope.terminate(t.Context(), nil); !errors.Is(err, ErrUnverifiableLaunch) {
		t.Fatalf("ownerless Terminate = %v, want ErrUnverifiableLaunch", err)
	}
}

func TestRequireConsecutiveEmptyCensusesUsesTwoFreshObservations(t *testing.T) {
	observations := 0
	empty, err := requireConsecutiveEmptyCensuses(t.Context(), func(context.Context) (bool, error) {
		observations++
		return true, nil
	})
	if err != nil || !empty || observations != 2 {
		t.Fatalf("two empty observations = (%t, %v), calls=%d", empty, err, observations)
	}

	observations = 0
	empty, err = requireConsecutiveEmptyCensuses(t.Context(), func(context.Context) (bool, error) {
		observations++
		return observations == 1, nil
	})
	if err != nil || empty || observations != 2 {
		t.Fatalf("nonconsecutive empty observations = (%t, %v), calls=%d", empty, err, observations)
	}
}

func TestDiscoverOwnedProcessLaunchesCrossesNestedGroupsAndSessions(t *testing.T) {
	root, err := NewLaunchRef(100, "root", 100)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []processScopeSnapshot{
		{pid: 100, parentPID: 10, pgid: 100, sessionID: 100, startToken: "root"},
		{pid: 101, parentPID: 100, pgid: 101, sessionID: 100, startToken: "child-group"},
		{pid: 102, parentPID: 101, pgid: 102, sessionID: 102, startToken: "child-session"},
		{pid: 103, parentPID: 102, pgid: 103, sessionID: 102, startToken: "grandchild-group"},
		{pid: 200, parentPID: 10, pgid: 200, sessionID: 200, startToken: "unrelated"},
	}

	refs, err := discoverOwnedProcessLaunches([]LaunchRef{root}, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int{100: 100, 101: 101, 102: 102, 103: 103}
	if len(refs) != len(want) {
		t.Fatalf("discovered refs = %#v, want %d owned processes", refs, len(want))
	}
	for _, ref := range refs {
		pgid, exists := want[ref.PID()]
		if !exists || ref.PGID() != pgid {
			t.Fatalf("unexpected discovered ref: pid=%d pgid=%d", ref.PID(), ref.PGID())
		}
		delete(want, ref.PID())
	}
	if len(want) != 0 {
		t.Fatalf("missing discovered refs: %#v", want)
	}
}

func TestDiscoverOwnedProcessLaunchesDoesNotClaimSharedCallerSession(t *testing.T) {
	root, err := NewLaunchRef(100, "root", 200)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []processScopeSnapshot{
		{pid: 100, parentPID: 10, pgid: 200, sessionID: 50, startToken: "root"},
		{pid: 20, parentPID: 1, pgid: 50, sessionID: 50, startToken: "caller"},
		{pid: 21, parentPID: 20, pgid: 50, sessionID: 50, startToken: "caller-child"},
	}

	refs, err := discoverOwnedProcessLaunches([]LaunchRef{root}, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != root {
		t.Fatalf("shared-session refs = %#v, want only the durable root", refs)
	}
}

func TestDiscoverOwnedProcessLaunchesRetainsDurableRootGroup(t *testing.T) {
	root, err := NewLaunchRef(100, "root", 200)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []processScopeSnapshot{
		{pid: 100, parentPID: 10, pgid: 200, sessionID: 50, startToken: "root"},
		{pid: 101, parentPID: 1, pgid: 200, sessionID: 50, startToken: "reparented-group-member"},
		{pid: 20, parentPID: 1, pgid: 50, sessionID: 50, startToken: "caller"},
	}

	refs, err := discoverOwnedProcessLaunches([]LaunchRef{root}, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0] != root || refs[1].PID() != 101 {
		t.Fatalf("root-group refs = %#v, want root and reparented group member", refs)
	}
}

func TestDiscoverOwnedProcessLaunchesTrustsOnlyGroupAndSessionLeaders(t *testing.T) {
	root, err := NewLaunchRef(100, "root", 200)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []processScopeSnapshot{
		{pid: 100, parentPID: 10, pgid: 200, sessionID: 50, startToken: "root"},
		{pid: 101, parentPID: 100, pgid: 101, sessionID: 50, startToken: "group-leader"},
		{pid: 102, parentPID: 1, pgid: 101, sessionID: 50, startToken: "reparented-group-member"},
		{pid: 103, parentPID: 100, pgid: 103, sessionID: 103, startToken: "session-leader"},
		{pid: 104, parentPID: 1, pgid: 104, sessionID: 103, startToken: "reparented-session-member"},
		{pid: 105, parentPID: 100, pgid: 900, sessionID: 50, startToken: "joined-shared-group"},
		{pid: 106, parentPID: 1, pgid: 900, sessionID: 50, startToken: "unrelated-shared-group-member"},
	}

	refs, err := discoverOwnedProcessLaunches([]LaunchRef{root}, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]struct{}{100: {}, 101: {}, 102: {}, 103: {}, 104: {}, 105: {}}
	if len(refs) != len(want) {
		t.Fatalf("leader-bound refs = %#v, want %d owned processes", refs, len(want))
	}
	for _, ref := range refs {
		if _, exists := want[ref.PID()]; !exists {
			t.Fatalf("unexpected leader-bound ref: pid=%d pgid=%d", ref.PID(), ref.PGID())
		}
		delete(want, ref.PID())
	}
	if len(want) != 0 {
		t.Fatalf("missing leader-bound refs: %#v", want)
	}
}

func TestDiscoverOwnedProcessLaunchesRetainedDescendantDoesNotPromoteSharedGroup(t *testing.T) {
	root, err := NewLaunchRef(100, "root", 100)
	if err != nil {
		t.Fatal(err)
	}
	firstSnapshots := []processScopeSnapshot{
		{pid: 100, parentPID: 10, pgid: 100, sessionID: 100, startToken: "root"},
		{pid: 101, parentPID: 100, pgid: 400, sessionID: 50, startToken: "descendant"},
		{pid: 102, parentPID: 1, pgid: 400, sessionID: 50, startToken: "unrelated-peer"},
	}
	first, err := discoverOwnedProcessLaunchesWithOwner([]LaunchRef{root}, nil, nil, nil, firstSnapshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0] != root {
		t.Fatalf("first census refs = %#v, want root and owned descendant", first)
	}
	var retained LaunchRef
	for _, ref := range first {
		if ref.PID() == 101 {
			retained = ref
		}
		if ref.PID() == 102 {
			t.Fatalf("first census claimed shared-group peer: %#v", first)
		}
	}
	if retained.PID() != 101 {
		t.Fatalf("first census omitted retained descendant: %#v", first)
	}

	secondSnapshots := []processScopeSnapshot{
		{pid: 100, parentPID: 10, pgid: 100, sessionID: 100, startToken: "root"},
		{pid: 101, parentPID: 1, pgid: 400, sessionID: 50, startToken: "descendant"},
		{pid: 102, parentPID: 1, pgid: 400, sessionID: 50, startToken: "unrelated-peer"},
	}
	second, err := discoverOwnedProcessLaunchesWithOwner([]LaunchRef{root}, []LaunchRef{retained}, nil, nil, secondSnapshots)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range second {
		if ref.PID() == 102 {
			t.Fatalf("second census promoted retained shared-group peer: %#v", second)
		}
	}
	if len(second) != 2 {
		t.Fatalf("second census refs = %#v, want root and retained descendant", second)
	}
}

func TestDiscoverOwnedProcessLaunchesIgnoresStaleRetainedReference(t *testing.T) {
	root, err := NewLaunchRef(110, "root", 110)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := NewLaunchRef(111, "old-descendant", 400)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := discoverOwnedProcessLaunchesWithOwner([]LaunchRef{root}, []LaunchRef{stale}, nil, nil, []processScopeSnapshot{
		{pid: 110, parentPID: 10, pgid: 110, sessionID: 110, startToken: "root"},
		{pid: 111, parentPID: 1, pgid: 400, sessionID: 50, startToken: "new-descendant"},
		{pid: 112, parentPID: 1, pgid: 400, sessionID: 50, startToken: "unrelated-peer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != root {
		t.Fatalf("stale retained refs = %#v, want only root", refs)
	}
}

func TestDiscoverOwnedProcessLaunchesRetainedGroupLeaderDoesNotExtendOwnGroup(t *testing.T) {
	root, err := NewLaunchRef(120, "root", 120)
	if err != nil {
		t.Fatal(err)
	}
	first, err := discoverOwnedProcessLaunchesWithOwner([]LaunchRef{root}, nil, nil, nil, []processScopeSnapshot{
		{pid: 120, parentPID: 10, pgid: 120, sessionID: 120, startToken: "root"},
		{pid: 121, parentPID: 120, pgid: 121, sessionID: 50, startToken: "group-leader"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var retained LaunchRef
	for _, ref := range first {
		if ref.PID() == 121 {
			retained = ref
		}
	}
	if retained.PID() != 121 {
		t.Fatalf("first census omitted group leader: %#v", first)
	}

	second, err := discoverOwnedProcessLaunchesWithOwner([]LaunchRef{root}, []LaunchRef{retained}, nil, nil, []processScopeSnapshot{
		{pid: 120, parentPID: 10, pgid: 120, sessionID: 120, startToken: "root"},
		{pid: 121, parentPID: 1, pgid: 121, sessionID: 50, startToken: "group-leader", scopeMarked: true},
		{pid: 122, parentPID: 1, pgid: 121, sessionID: 50, startToken: "group-member"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if containsLaunchRefPID(second, 122) {
		t.Fatalf("retained group leader extended its group: %#v", second)
	}
}

func containsLaunchRefPID(refs []LaunchRef, pid int) bool {
	for _, ref := range refs {
		if ref.PID() == pid {
			return true
		}
	}
	return false
}

func TestProcessIdentityTokensMatchCheckedDarwinLegacyTimestamp(t *testing.T) {
	if !processIdentityTokensMatch("darwin-v2:10:20:7", "darwin:10:20") {
		t.Fatal("v2 Darwin identity did not match its checked legacy timestamp")
	}
	if !processIdentityTokensMatch("darwin:10:20", "darwin-v2:10:20:7") {
		t.Fatal("legacy Darwin identity did not match its checked v2 timestamp")
	}
	if processIdentityTokensMatch("darwin-v2:10:21:7", "darwin:10:20") {
		t.Fatal("different Darwin timestamps were accepted")
	}
	if !processIdentityTokensMatch("darwin-v2:10:20:7", "darwin-v2:11:21:7") {
		t.Fatal("stable Darwin uniqueid did not survive a timestamp change")
	}
	if processIdentityTokensMatch("darwin-v2:10:20:7", "darwin-v2:10:20:8") {
		t.Fatal("different Darwin uniqueids were accepted")
	}
	if processIdentityTokensMatch("darwin-v2:10:20:not-a-number", "darwin-v2:10:20:not-a-number") {
		t.Fatal("malformed Darwin v2 identity was accepted")
	}
}

func TestDiscoverOwnedProcessLaunchesOwnerlessUsesDurableTopology(t *testing.T) {
	root, err := NewLaunchRef(110, "root", 110)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := discoverOwnedProcessLaunchesWithOwner(nil, nil, nil, nil, []processScopeSnapshot{
		{pid: 110, parentPID: 1, pgid: 110, sessionID: 110, startToken: "root"},
		{pid: 111, parentPID: 110, pgid: 111, sessionID: 110, startToken: "child"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("ownerless discovery without durable root = %#v, want no roots", refs)
	}
	refs, err = discoverOwnedProcessLaunchesWithOwner([]LaunchRef{root}, nil, nil, nil, []processScopeSnapshot{
		{pid: 110, parentPID: 1, pgid: 110, sessionID: 110, startToken: "root"},
		{pid: 111, parentPID: 110, pgid: 111, sessionID: 110, startToken: "child"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0] != root || refs[1].PID() != 111 {
		t.Fatalf("ownerless durable topology = %#v, want root and child", refs)
	}
}

func TestDiscoverOwnedProcessLaunchesDoesNotClaimLeaderlessSessionWithoutAnchor(t *testing.T) {
	root, err := NewLaunchRef(300, "exited-root", 300)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []processScopeSnapshot{
		{pid: 301, parentPID: 1, pgid: 301, sessionID: 300, startToken: "reparented-child"},
		{pid: 302, parentPID: 301, pgid: 302, sessionID: 302, startToken: "nested-session"},
	}

	refs, err := discoverOwnedProcessLaunches([]LaunchRef{root}, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != root {
		t.Fatalf("discovered refs = %#v, want only historical root", refs)
	}
}

func TestDiscoverOwnedProcessLaunchesMarkedReparentedProcessDoesNotClaimSharedTopology(t *testing.T) {
	root, err := NewLaunchRef(300, "exited-root", 300)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []processScopeSnapshot{
		{pid: 301, parentPID: 1, pgid: 400, sessionID: 50, startToken: "marked-reparented", scopeMarked: true},
		{pid: 302, parentPID: 301, pgid: 400, sessionID: 50, startToken: "marked-child"},
		{pid: 303, parentPID: 1, pgid: 400, sessionID: 50, startToken: "unrelated-group-peer"},
		{pid: 304, parentPID: 1, pgid: 500, sessionID: 50, startToken: "unrelated-session-peer"},
	}

	refs, err := discoverOwnedProcessLaunches([]LaunchRef{root}, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("discovered refs = %#v, want historical root, marked process, and child", refs)
	}
	for _, ref := range refs {
		if ref.PID() == 303 || ref.PID() == 304 {
			t.Fatalf("shared topology peer was claimed: %#v", refs)
		}
	}
}

func TestDiscoverOwnedProcessLaunchesRejectsReusedRootInsideScope(t *testing.T) {
	root, err := NewLaunchRef(400, "original", 400)
	if err != nil {
		t.Fatal(err)
	}
	_, err = discoverOwnedProcessLaunches([]LaunchRef{root}, []processScopeSnapshot{
		{pid: 400, parentPID: 1, pgid: 400, sessionID: 400, startToken: "reused"},
	})
	if !errors.Is(err, ErrUnverifiableLaunch) {
		t.Fatalf("reused root error = %v, want ErrUnverifiableLaunch", err)
	}
}

func TestDiscoverOwnedProcessLaunchesIgnoresIncompleteUnrelatedSnapshot(t *testing.T) {
	root, err := NewLaunchRef(500, "root", 500)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := discoverOwnedProcessLaunches([]LaunchRef{root}, []processScopeSnapshot{
		{pid: 500, parentPID: 10, pgid: 500, sessionID: 500, startToken: "root"},
		{pid: 600, parentPID: 1, pgid: 600, sessionID: 1, startToken: "unrelated"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != root {
		t.Fatalf("discovered refs = %#v, want only root", refs)
	}
}

func TestDiscoverOwnedProcessLaunchesRejectsIncompleteOwnedSnapshot(t *testing.T) {
	root, err := NewLaunchRef(700, "root", 700)
	if err != nil {
		t.Fatal(err)
	}
	_, err = discoverOwnedProcessLaunches([]LaunchRef{root}, []processScopeSnapshot{
		{pid: 700, parentPID: 10, pgid: 700, sessionID: 700, startToken: "root"},
		{pid: 701, parentPID: 700, pgid: 701, sessionID: 1, startToken: "child"},
	})
	if !errors.Is(err, ErrUnverifiableLaunch) {
		t.Fatalf("incomplete owned snapshot error = %v, want ErrUnverifiableLaunch", err)
	}
}

func TestDiscoverOwnedProcessLaunchesUsesOwnerTreeWithoutSignallingInfrastructure(t *testing.T) {
	root, err := NewLaunchRef(300, "exited-root", 300)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewProcessRef(10, "owner")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := NewProcessRef(11, "worker")
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []processScopeSnapshot{
		{pid: 10, parentPID: 1, pgid: 10, sessionID: 10, startToken: "owner"},
		{pid: 11, parentPID: 10, pgid: 10, sessionID: 10, startToken: "worker"},
		{pid: 301, parentPID: 11, pgid: 301, sessionID: 301, startToken: "provider-child"},
		{pid: 600, parentPID: 1, pgid: 600, sessionID: 600, startToken: "protected-unrelated"},
	}

	refs, err := discoverOwnedProcessLaunchesWithOwner([]LaunchRef{root}, nil, &owner, []ProcessRef{worker}, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0] != root || refs[1].PID() != 301 {
		t.Fatalf("owner-tree refs = %#v, want historical root plus provider child", refs)
	}
}

func TestDiscoverOwnedProcessLaunchesRequiresLiveOwnerWhenBound(t *testing.T) {
	root, err := NewLaunchRef(300, "exited-root", 300)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewProcessRef(10, "owner")
	if err != nil {
		t.Fatal(err)
	}
	_, err = discoverOwnedProcessLaunchesWithOwner([]LaunchRef{root}, nil, &owner, nil, []processScopeSnapshot{
		{pid: 600, parentPID: 1, pgid: 600, sessionID: 600, startToken: "protected-unrelated"},
	})
	if !errors.Is(err, ErrUnverifiableLaunch) {
		t.Fatalf("owner-bound discovery without a live owner error = %v, want ErrUnverifiableLaunch", err)
	}
}
