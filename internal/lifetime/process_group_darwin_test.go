//go:build darwin

package lifetime

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	darwinNestedHelperMode = "DELEGATE_DARWIN_NESTED_HELPER"
	darwinNestedPIDFile    = "DELEGATE_DARWIN_NESTED_PID_FILE"
)

func TestMain(m *testing.M) {
	if code, handled := RunProcessGroupPinMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func TestDarwinProcessGroupRegistrationUsesPlanAndPrivateSession(t *testing.T) {
	plan := processGroupPlan("root", "task", "darwin-session-registration")
	scope, err := CreatePrepared(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.BindOwnerProcess(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })

	ref := scope.Reference()
	if ref.Identifier() != plan.Identifier() {
		t.Fatalf("registered Darwin scope identifier = %q, want plan %q", ref.Identifier(), plan.Identifier())
	}
	reopened, err := OpenRegistered(plan, ref)
	if err != nil {
		t.Fatalf("OpenRegistered session scope: %v", err)
	}
	t.Cleanup(func() {
		if releaseErr := reopened.Release(); releaseErr != nil {
			t.Errorf("reopened scope Release: %v", releaseErr)
		}
	})
	cmd := exec.Command("true")
	if err = reopened.Configure(cmd); err != nil {
		t.Fatalf("Configure through pinned registration: %v", err)
	}
	implementation, ok := reopened.impl.(*processGroupScope)
	if !ok {
		t.Fatal("reopened scope is not a process-group scope")
	}
	if implementation.pin != nil {
		t.Fatal("new Darwin scope retained a process-group pin")
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid || cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pgid != 0 {
		t.Fatalf("provider session settings = %#v, want Setsid without Setpgid", cmd.SysProcAttr)
	}
}

func TestDarwinProcessGroupRejectsNumericSignalAuthority(t *testing.T) {
	if err := signalProcessGroup(4343, syscall.SIGTERM); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("numeric process-group signal error = %v, want ErrUnsupported", err)
	}
}

func TestDarwinProcessGroupAllowsVerifiedNestedSignalTargets(t *testing.T) {
	ref, err := NewProcessRef(4242, "darwin:1:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProcessGroupSignalTargets(&processGroupScope{}, map[int][]LaunchRef{4343: nil}); err != nil {
		t.Fatalf("nested target validation = %v, want nil", err)
	}
	if ref.PID() == 0 {
		t.Fatal("test identity was not constructed")
	}
}

func TestDarwinProcessGroupPinReferenceRejectsChangedPlan(t *testing.T) {
	ref, err := NewProcessRef(4242, "darwin:1:1")
	if err != nil {
		t.Fatal(err)
	}
	pin := &darwinProcessGroupPin{
		ref: ref, address: "127.0.0.1:4242",
		token: strings.Repeat("a", 64),
	}
	first := processGroupPlan("root", "task", "first")
	second := processGroupPlan("root", "task", "second")
	identifier := encodeDarwinProcessGroupPinReference(first.Identifier(), pin)
	base, _, err := decodeDarwinProcessGroupPinReference(identifier)
	if err != nil || base != first.Identifier() {
		t.Fatalf("pin reference round trip = (%q, %v)", base, err)
	}
	dynamic, err := NewRef(KindProcessGroup, AssuranceCooperative, identifier)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenRegistered(second, dynamic); err == nil {
		t.Fatal("OpenRegistered accepted a Darwin pin from another plan")
	}
}

func TestDarwinProcessInfoRejectsMalformedUniqueIdentifierResponse(t *testing.T) {
	for _, returned := range []int{darwinProcUniqIdentifierInfoSize - 1, darwinProcUniqIdentifierInfoSize + 1} {
		t.Run(strconv.Itoa(returned), func(t *testing.T) {
			_, _, err := readDarwinUniqueIdentifierInfo(42, func(callNum int32, pid int32, flavor uint32, arg uint64, buffer []byte) (int, error) {
				if callNum != darwinProcInfoCallPIDInfo || pid != 42 || flavor != darwinProcPIDUniqIdentifierInfo || arg != 0 {
					t.Fatalf("proc_info identity call = (%d, %d, %d, %d)", callNum, pid, flavor, arg)
				}
				copy(buffer, darwinUniqueIdentifierFixture(7, 11))
				return returned, nil
			})
			if !errors.Is(err, ErrUnverifiableLaunch) {
				t.Fatalf("malformed identity response error = %v, want ErrUnverifiableLaunch", err)
			}
		})
	}
}

func TestDarwinProcessInfoRejectsNegativePIDVersion(t *testing.T) {
	_, _, err := readDarwinUniqueIdentifierInfo(42, func(_ int32, _ int32, _ uint32, _ uint64, buffer []byte) (int, error) {
		copy(buffer, darwinUniqueIdentifierFixture(7, -1))
		return darwinProcUniqIdentifierInfoSize, nil
	})
	if !errors.Is(err, ErrUnverifiableLaunch) {
		t.Fatalf("negative pidversion error = %v, want ErrUnverifiableLaunch", err)
	}
}

func TestDarwinKinfoDecodeDistinguishesExitFromABIMismatch(t *testing.T) {
	process, err := decodeDarwinKinfoProc(nil)
	if err != nil || process != nil {
		t.Fatalf("zero-byte exited kinfo result = (%#v, %v), want nil/nil", process, err)
	}

	process, err = decodeDarwinKinfoProc(make([]byte, unix.SizeofKinfoProc-1))
	if process != nil || !errors.Is(err, ErrUnverifiableLaunch) {
		t.Fatalf("short nonzero kinfo result = (%#v, %v), want nil/ErrUnverifiableLaunch", process, err)
	}
}

func TestDarwinProcessIdentityTreatsKinfoEIOThenEmptyAsExit(t *testing.T) {
	lookups := 0
	procInfoCalls := 0
	present, identity, err := inspectDarwinProcessIdentityWith(
		42,
		func(int32, int32, uint32, uint64, []byte) (int, error) {
			procInfoCalls++
			return -1, errors.New("unexpected proc_info call")
		},
		func(int) (*unix.KinfoProc, error) {
			lookups++
			if lookups == 1 {
				return nil, unix.EIO
			}
			return nil, nil
		},
	)
	if err != nil || present || identity != (darwinProcessIdentity{}) {
		t.Fatalf("EIO-to-exit identity = (%t, %#v, %v), want false/zero/nil", present, identity, err)
	}
	if lookups != 2 || procInfoCalls != 0 {
		t.Fatalf("EIO-to-exit calls = lookups %d proc_info %d, want 2/0", lookups, procInfoCalls)
	}
}

func TestDarwinProcessIdentityRetriesProcInfoEIOForSameProcess(t *testing.T) {
	lookups := 0
	procInfoCalls := 0
	present, identity, err := inspectDarwinProcessIdentityWith(
		42,
		func(_ int32, _ int32, _ uint32, _ uint64, buffer []byte) (int, error) {
			procInfoCalls++
			if procInfoCalls == 1 {
				return -1, unix.EIO
			}
			copy(buffer, darwinUniqueIdentifierFixture(7, 11))
			return darwinProcUniqIdentifierInfoSize, nil
		},
		func(int) (*unix.KinfoProc, error) {
			lookups++
			return darwinKinfoFixture(42), nil
		},
	)
	if err != nil || !present {
		t.Fatalf("EIO-to-same-process identity = (%t, %#v, %v), want present/nil", present, identity, err)
	}
	if identity.startToken != darwinProcessLaunchToken(unix.Timeval{Sec: 10, Usec: 20}, 7) || identity.pidVersion != 11 {
		t.Fatalf("refreshed identity = %#v", identity)
	}
	if lookups != 2 || procInfoCalls != 2 {
		t.Fatalf("EIO refresh calls = lookups %d proc_info %d, want 2/2", lookups, procInfoCalls)
	}
}

func TestDarwinProcessIdentityDoesNotHidePersistentEIO(t *testing.T) {
	lookups := 0
	present, _, err := inspectDarwinProcessIdentityWith(
		42,
		func(int32, int32, uint32, uint64, []byte) (int, error) {
			return -1, errors.New("unexpected proc_info call")
		},
		func(int) (*unix.KinfoProc, error) {
			lookups++
			return nil, unix.EIO
		},
	)
	if present || !errors.Is(err, unix.EIO) {
		t.Fatalf("persistent EIO identity = (%t, %v), want false/EIO", present, err)
	}
	if lookups != darwinProcessIdentityObservationAttempts {
		t.Fatalf("persistent EIO lookups = %d, want %d", lookups, darwinProcessIdentityObservationAttempts)
	}
}

func TestDarwinAuditTokenCarriesExactPIDAndPIDVersion(t *testing.T) {
	token, err := darwinAuditToken(42, 11)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != darwinAuditTokenSize {
		t.Fatalf("audit token size = %d, want %d", len(token), darwinAuditTokenSize)
	}
	if got := binary.LittleEndian.Uint32(token[5*4 : 6*4]); got != 42 {
		t.Fatalf("audit token PID = %d, want 42", got)
	}
	if got := binary.LittleEndian.Uint32(token[7*4 : 8*4]); got != 11 {
		t.Fatalf("audit token pidversion = %d, want 11", got)
	}
	for index, value := range token {
		if (index >= 5*4 && index < 6*4) || (index >= 7*4 && index < 8*4) {
			continue
		}
		if value != 0 {
			t.Fatalf("audit token byte %d = %d, want zero", index, value)
		}
	}
}

func TestDarwinSignalRejectsUniqueIDMismatchWithoutMutation(t *testing.T) {
	ref, err := NewLaunchRef(42, darwinProcessLaunchToken(unix.Timeval{Sec: 10, Usec: 20}, 7), 42)
	if err != nil {
		t.Fatal(err)
	}
	sends := 0
	present, err := signalDarwinProcessWith(
		context.Background(),
		ref,
		syscall.SIGTERM,
		func(int) (bool, darwinProcessIdentity, error) {
			return true, darwinProcessIdentity{
				startToken: darwinProcessLaunchToken(unix.Timeval{Sec: 10, Usec: 20}, 8),
				uniqueID:   8,
				pidVersion: 11,
			}, nil
		},
		func(int, int32, syscall.Signal) error {
			sends++
			return nil
		},
	)
	if err != nil || present {
		t.Fatalf("uniqueid mismatch result = (%t, %v), want false/nil", present, err)
	}
	if sends != 0 {
		t.Fatalf("audit-token sends = %d, want 0", sends)
	}
}

func TestDarwinSignalRefreshesStalePIDVersion(t *testing.T) {
	ref, err := NewLaunchRef(42, darwinProcessLaunchToken(unix.Timeval{Sec: 10, Usec: 20}, 7), 42)
	if err != nil {
		t.Fatal(err)
	}
	identity := darwinProcessLaunchToken(unix.Timeval{Sec: 10, Usec: 20}, 7)
	versions := []int32{11, 12}
	sends := make([]int32, 0, 2)
	present, err := signalDarwinProcessWith(
		context.Background(),
		ref,
		syscall.SIGTERM,
		func(int) (bool, darwinProcessIdentity, error) {
			version := versions[0]
			versions = versions[1:]
			return true, darwinProcessIdentity{startToken: identity, uniqueID: 7, pidVersion: version}, nil
		},
		func(_ int, pidVersion int32, _ syscall.Signal) error {
			sends = append(sends, pidVersion)
			if len(sends) == 1 {
				return unix.ESRCH
			}
			return nil
		},
	)
	if err != nil || !present {
		t.Fatalf("stale pidversion result = (%t, %v), want true/nil", present, err)
	}
	if len(sends) != 2 || sends[0] != 11 || sends[1] != 12 {
		t.Fatalf("pidversions sent = %#v, want [11 12]", sends)
	}
}

func TestDarwinSignalTreatsESRCHAfterPIDReuseAsAbsent(t *testing.T) {
	ref, err := NewLaunchRef(42, darwinProcessLaunchToken(unix.Timeval{Sec: 10, Usec: 20}, 7), 42)
	if err != nil {
		t.Fatal(err)
	}
	identities := []darwinProcessIdentity{
		{startToken: ref.StartToken(), uniqueID: 7, pidVersion: 11},
		{startToken: darwinProcessLaunchToken(unix.Timeval{Sec: 10, Usec: 20}, 8), uniqueID: 8, pidVersion: 12},
	}
	sends := 0
	present, err := signalDarwinProcessWith(
		context.Background(),
		ref,
		syscall.SIGKILL,
		func(int) (bool, darwinProcessIdentity, error) {
			identity := identities[0]
			identities = identities[1:]
			return true, identity, nil
		},
		func(int, int32, syscall.Signal) error {
			sends++
			return unix.ESRCH
		},
	)
	if err != nil || present {
		t.Fatalf("PID reuse result = (%t, %v), want false/nil", present, err)
	}
	if sends != 1 {
		t.Fatalf("audit-token sends = %d, want 1", sends)
	}
}

func TestDarwinExecKeepsStableLaunchTokenWhilePIDVersionChanges(t *testing.T) {
	start := unix.Timeval{Sec: 10, Usec: 20}
	first := darwinProcessIdentity{startToken: darwinProcessLaunchToken(start, 7), uniqueID: 7, pidVersion: 11}
	second := darwinProcessIdentity{startToken: darwinProcessLaunchToken(start, 7), uniqueID: 7, pidVersion: 12}
	if first.startToken != second.startToken || first.pidVersion == second.pidVersion {
		t.Fatalf("exec identity changed stable token or kept pidversion: first=%#v second=%#v", first, second)
	}
	if !processIdentityTokensMatch(second.startToken, first.startToken) {
		t.Fatal("stable launch identity did not survive exec")
	}
}

func TestDarwinSignalTargetsEachVerifiedPIDWithoutNumericFallback(t *testing.T) {
	first, err := NewLaunchRef(42, darwinProcessLaunchToken(unix.Timeval{Sec: 10, Usec: 20}, 7), 42)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewLaunchRef(43, darwinProcessLaunchToken(unix.Timeval{Sec: 10, Usec: 21}, 8), 42)
	if err != nil {
		t.Fatal(err)
	}
	observed := map[int]darwinProcessIdentity{
		42: {startToken: first.StartToken(), uniqueID: 7, pidVersion: 11},
		43: {startToken: second.StartToken(), uniqueID: 8, pidVersion: 12},
	}
	signaled := make([]int, 0, 2)
	err = signalDarwinProcessGroupWith(
		context.Background(),
		42,
		[]LaunchRef{first, second},
		syscall.SIGTERM,
		func(pid int) (bool, darwinProcessIdentity, error) {
			return true, observed[pid], nil
		},
		func(pid int, _ int32, _ syscall.Signal) error {
			signaled = append(signaled, pid)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(signaled) != 2 || signaled[0] != 42 || signaled[1] != 43 {
		t.Fatalf("audit-token target PIDs = %#v, want [42 43]", signaled)
	}
}

func TestDarwinLegacyTimestampTokenRemainsCompatible(t *testing.T) {
	if !processIdentityTokensMatch("darwin-v2:10:20:7", "darwin:10:20") {
		t.Fatal("v2 identity did not match checked legacy timestamp token")
	}
	if processIdentityTokensMatch("darwin-v2:10:21:7", "darwin:10:20") {
		t.Fatal("different legacy timestamp was accepted")
	}
	if processIdentityTokensMatch("darwin-v2:10:20:not-a-number", "darwin:10:20") {
		t.Fatal("malformed v2 identity was accepted as legacy")
	}
}

func TestDarwinDiscoveryDoesNotClaimUnrelatedObserverChild(t *testing.T) {
	root, err := NewLaunchRef(100, "darwin-v2:10:20:100", 100)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewProcessRef(10, "darwin-v2:1:2:10")
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []processScopeSnapshot{
		{pid: 10, parentPID: 1, pgid: 10, sessionID: 10, startToken: owner.StartToken()},
		{pid: 100, parentPID: 10, pgid: 100, sessionID: 100, startToken: root.StartToken(), scopeMarked: true},
		{pid: 101, parentPID: 100, pgid: 101, sessionID: 101, startToken: "darwin-v2:10:21:101", scopeMarked: true},
		{pid: 200, parentPID: 10, pgid: 10, sessionID: 10, startToken: "darwin-v2:10:22:200"},
	}

	refs, err := discoverDarwinOwnedProcessLaunches([]LaunchRef{root}, nil, &owner, nil, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || !containsLaunchRefPID(refs, 100) || !containsLaunchRefPID(refs, 101) {
		t.Fatalf("Darwin owned refs = %#v, want root and nested child", refs)
	}
	if containsLaunchRefPID(refs, 200) {
		t.Fatalf("Darwin discovery claimed unrelated observer child: %#v", refs)
	}
}

func TestDarwinProcessScopeCleansNestedSessionAndPreservesUnrelatedProcess(t *testing.T) {
	switch os.Getenv(darwinNestedHelperMode) {
	case "root":
		runDarwinNestedRootHelper(t)
		return
	case "child":
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Hour)
		}
	}

	pidFile := filepath.Join(t.TempDir(), "nested.pid")
	scope, err := Open(testProcessGroupRef(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.BindOwnerProcess(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })

	cmd := exec.Command(os.Args[0], "-test.run=^TestDarwinProcessScopeCleansNestedSessionAndPreservesUnrelatedProcess$")
	cmd.Env = append(os.Environ(), darwinNestedHelperMode+"=root", darwinNestedPIDFile+"="+pidFile)
	if err = scope.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	rootRef, err := scope.ObserveStarted(cmd.Process.Pid)
	if err != nil {
		killDarwinTestCommand(t, cmd, "kill nested-session root after failed observation")
		acceptDarwinTestCommandExit(t, "wait nested-session root after failed observation", <-waitDone)
		t.Fatal(err)
	}
	childPID := waitForDarwinNestedPID(t, pidFile)
	childPresent, childToken, err := observeStartedProcessIdentity(childPID)
	if err != nil || !childPresent {
		t.Fatalf("observe nested child PID %d: present=%t err=%v", childPID, childPresent, err)
	}
	childPGID, err := processGroupID(childPID)
	if err != nil {
		t.Fatal(err)
	}
	childSessionID, err := syscall.Getsid(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if childPGID != childPID || childSessionID != childPID || childPGID == rootRef.PGID() {
		t.Fatalf("nested child scope = PID %d PGID %d SID %d, root PGID %d", childPID, childPGID, childSessionID, rootRef.PGID())
	}
	childRef, err := NewLaunchRef(childPID, childToken, childPGID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killVerifiedTestProcess(t, rootRef)
		killVerifiedTestProcess(t, childRef)
	})
	unrelated := exec.Command("sleep", "60")
	if err = unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	unrelatedRef, err := ObserveStartedProcess(unrelated.Process.Pid)
	if err != nil {
		killAndWaitDarwinTestCommand(t, unrelated, "unrelated process after failed observation")
		t.Fatal(err)
	}
	t.Cleanup(func() {
		alive, presentErr := ProcessPresent(unrelatedRef)
		if presentErr != nil {
			t.Errorf("observe unrelated cleanup process: %v", presentErr)
		} else if !alive {
			t.Error("unrelated same-user process was killed before test cleanup")
		}
		if alive {
			if killErr := unrelated.Process.Kill(); killErr != nil {
				t.Errorf("kill unrelated cleanup process: %v", killErr)
			}
		}
		acceptDarwinTestCommandExit(t, "wait unrelated cleanup process", unrelated.Wait())
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = scope.Terminate(ctx, []LaunchRef{rootRef}); err != nil {
		t.Fatalf("nested-session Terminate: %v", err)
	}
	rootAlive, _, err := observeProcessIdentity(rootRef.PID())
	if err != nil {
		t.Fatal(err)
	}
	if rootAlive {
		t.Fatal("nested-session root survived termination")
	}
	childAlive, _, err := observeProcessIdentity(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if childAlive {
		t.Fatalf("nested-session child PID %d survived termination", childPID)
	}
	unrelatedAlive, err := ProcessPresent(unrelatedRef)
	if err != nil {
		t.Fatal(err)
	}
	if !unrelatedAlive {
		t.Fatal("unrelated same-user process did not survive process-scope termination")
	}
}

func TestDarwinProcessScopeCleansReparentedSession(t *testing.T) {
	switch os.Getenv(darwinNestedHelperMode) {
	case "orphan-root":
		runDarwinOrphanRootHelper(t)
		return
	case "orphan-child":
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Hour)
		}
	}

	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	scope, err := Open(testProcessGroupRef(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.BindOwnerProcess(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })

	cmd := exec.Command(os.Args[0], "-test.run=^TestDarwinProcessScopeCleansReparentedSession$")
	cmd.Env = append(os.Environ(), darwinNestedHelperMode+"=orphan-root", darwinNestedPIDFile+"="+pidFile)
	if err = scope.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rootRef, err := scope.ObserveStarted(cmd.Process.Pid)
	if err != nil {
		killAndWaitDarwinTestCommand(t, cmd, "orphan root after failed observation")
		t.Fatal(err)
	}
	childPID := waitForDarwinNestedPID(t, pidFile)
	childPresent, childToken, err := observeStartedProcessIdentity(childPID)
	if err != nil || !childPresent {
		t.Fatalf("observe orphan child PID %d: present=%t err=%v", childPID, childPresent, err)
	}
	childPGID, err := processGroupID(childPID)
	if err != nil {
		t.Fatal(err)
	}
	childRef, err := NewLaunchRef(childPID, childToken, childPGID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killVerifiedTestProcess(t, childRef) })
	if waitErr := cmd.Wait(); waitErr != nil {
		t.Fatalf("orphan root helper: %v", waitErr)
	}
	rootPresent, _, err := observeProcessIdentity(rootRef.PID())
	if err != nil {
		t.Fatal(err)
	}
	if rootPresent {
		t.Fatalf("root PID %d remained present after Wait", rootRef.PID())
	}
	empty, err := scope.Empty([]LaunchRef{rootRef})
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Fatal("marked reparented child was not retained by the process scope")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = scope.Terminate(ctx, []LaunchRef{rootRef}); err != nil {
		t.Fatalf("reparented-session Terminate: %v", err)
	}
	childAlive, _, err := observeProcessIdentity(childPID)
	if err != nil {
		t.Fatal(err)
	}
	if childAlive {
		t.Fatalf("reparented child PID %d survived termination", childPID)
	}
}

func TestDarwinProcessEnvironmentContainsScopeMarkerOnlyInEnvironment(t *testing.T) {
	expected := processScopeMarkerEnvironment + "=pg:" + strings.Repeat("e", 32)
	argumentOnly := darwinProcargsFixture([]string{"provider", expected}, []string{"OTHER=value"})
	marked, err := darwinProcessEnvironmentContains(argumentOnly, expected)
	if err != nil {
		t.Fatal(err)
	}
	if marked {
		t.Fatal("argument text was accepted as an environment marker")
	}

	environment := darwinProcargsFixture([]string{"provider", "task"}, []string{"OTHER=value", expected})
	marked, err = darwinProcessEnvironmentContains(environment, expected)
	if err != nil {
		t.Fatal(err)
	}
	if !marked {
		t.Fatal("exact environment marker was not found")
	}
}

func TestDarwinProcessEnvironmentRejectsMalformedRecords(t *testing.T) {
	expected := processScopeMarkerEnvironment + "=pg:" + strings.Repeat("f", 32)
	cases := [][]byte{
		{1, 2, 3},
		append([]byte{1, 0, 0, 0}, []byte("unterminated")...),
		darwinProcargsFixtureWithTail(2, "/bin/provider\x00\x00provider\x00"),
		darwinProcargsFixtureWithTail(1, "/bin/provider\x00\x00provider\x00"+expected),
	}
	for index, data := range cases {
		if _, err := darwinProcessEnvironmentContains(data, expected); err == nil {
			t.Fatalf("malformed case %d was accepted", index)
		}
	}
}

func TestDarwinProcessMarkerPermissionIsStrictOnlyForPendingStart(t *testing.T) {
	marked, err := classifyDarwinProcessMarkerError(42, unix.EACCES, true)
	if marked || !errors.Is(err, ErrUnverifiableLaunch) {
		t.Fatalf("strict marker permission result = (%t, %v), want false/ErrUnverifiableLaunch", marked, err)
	}

	marked, err = classifyDarwinProcessMarkerError(42, unix.EACCES, false)
	if marked || err != nil {
		t.Fatalf("ownerless marker permission result = (%t, %v), want false/nil", marked, err)
	}
}

func TestDarwinProcessMarkerExitIsKnownAbsent(t *testing.T) {
	marked, err := classifyDarwinProcessMarkerError(42, unix.ESRCH, true)
	if marked || err != nil {
		t.Fatalf("exited marker result = (%t, %v), want false/nil", marked, err)
	}
}

func TestDarwinProcessMarkerRetriesTransientReadForSameProcess(t *testing.T) {
	expectedToken := "123:456"
	expectedMarker := processScopeMarkerEnvironment + "=scope"
	reads := 0
	marked, err := darwinProcessHasScopeMarkerWith(
		context.Background(),
		42,
		expectedToken,
		"scope",
		true,
		func(int) ([]byte, error) {
			reads++
			if reads == 1 {
				return nil, unix.EIO
			}
			return darwinProcargsFixture([]string{"provider"}, []string{expectedMarker}), nil
		},
		func(int) (bool, bool, string, error) {
			return true, false, expectedToken, nil
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil || !marked {
		t.Fatalf("transient marker result = (%t, %v), want true/nil", marked, err)
	}
	if reads != 2 {
		t.Fatalf("procargs reads = %d, want 2", reads)
	}
}

func TestDarwinProcessMarkerTreatsExitedTransientReadAsAbsent(t *testing.T) {
	reads := 0
	marked, err := darwinProcessHasScopeMarkerWith(
		context.Background(),
		42,
		"123:456",
		"scope",
		true,
		func(int) ([]byte, error) {
			reads++
			return nil, unix.EINVAL
		},
		func(int) (bool, bool, string, error) {
			return false, false, "", nil
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil || marked {
		t.Fatalf("exited transient marker result = (%t, %v), want false/nil", marked, err)
	}
	if reads != 1 {
		t.Fatalf("procargs reads = %d, want 1", reads)
	}
}

func TestDarwinProcessMarkerTreatsReusedPIDAsAbsent(t *testing.T) {
	marked, err := darwinProcessHasScopeMarkerWith(
		context.Background(),
		42,
		"123:456",
		"scope",
		true,
		func(int) ([]byte, error) {
			return nil, unix.EIO
		},
		func(int) (bool, bool, string, error) {
			return true, false, "789:012", nil
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil || marked {
		t.Fatalf("reused PID marker result = (%t, %v), want false/nil", marked, err)
	}
}

func TestDarwinProcessMarkerFailsAfterBoundedTransientReads(t *testing.T) {
	reads := 0
	waits := 0
	marked, err := darwinProcessHasScopeMarkerWith(
		context.Background(),
		42,
		"123:456",
		"scope",
		true,
		func(int) ([]byte, error) {
			reads++
			return nil, unix.EIO
		},
		func(int) (bool, bool, string, error) {
			return true, false, "123:456", nil
		},
		func(context.Context, time.Duration) error {
			waits++
			return nil
		},
	)
	if marked || !errors.Is(err, unix.EIO) {
		t.Fatalf("persistent transient marker result = (%t, %v), want false/EIO", marked, err)
	}
	if reads != darwinProcessMarkerReadAttempts {
		t.Fatalf("procargs reads = %d, want %d", reads, darwinProcessMarkerReadAttempts)
	}
	if waits != darwinProcessMarkerReadAttempts-1 {
		t.Fatalf("retry waits = %d, want %d", waits, darwinProcessMarkerReadAttempts-1)
	}
}

func TestDarwinProcessMarkerTreatsPersistentTransientReadAsUnmarkedAfterLaunch(t *testing.T) {
	reads := 0
	marked, err := darwinProcessHasScopeMarkerWith(
		context.Background(),
		42,
		"123:456",
		"scope",
		false,
		func(int) ([]byte, error) {
			reads++
			return nil, unix.EINVAL
		},
		func(int) (bool, bool, string, error) {
			return true, false, "123:456", nil
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if err != nil || marked {
		t.Fatalf("post-launch transient marker result = (%t, %v), want false/nil", marked, err)
	}
	if reads != darwinProcessMarkerReadAttempts {
		t.Fatalf("procargs reads = %d, want %d", reads, darwinProcessMarkerReadAttempts)
	}
}

func TestDarwinProcessMarkerReportsIdentityInspectionFailure(t *testing.T) {
	identityErr := errors.New("identity unavailable")
	marked, err := darwinProcessHasScopeMarkerWith(
		context.Background(),
		42,
		"123:456",
		"scope",
		true,
		func(int) ([]byte, error) {
			return nil, unix.EIO
		},
		func(int) (bool, bool, string, error) {
			return false, false, "", identityErr
		},
		func(context.Context, time.Duration) error { return nil },
	)
	if marked || !errors.Is(err, identityErr) {
		t.Fatalf("identity failure marker result = (%t, %v), want false/identity error", marked, err)
	}
}

func runDarwinNestedRootHelper(t *testing.T) {
	pidFile := os.Getenv(darwinNestedPIDFile)
	if pidFile == "" {
		t.Fatal("nested PID file is empty")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestDarwinProcessScopeCleansNestedSessionAndPreservesUnrelatedProcess$")
	child.Env = append(os.Environ(), darwinNestedHelperMode+"=child")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := publishTestPID(pidFile, child.Process.Pid); err != nil {
		killAndWaitDarwinTestCommand(t, child, "nested child after failed PID publication")
		t.Fatal(err)
	}
	// The parent scope escalates this TERM-resistant child to KILL. Its wait
	// error is therefore expected when cleanup reaches the nested session.
	if waitErr := child.Wait(); waitErr == nil {
		t.Fatal("nested child exited successfully; want signal termination")
	} else {
		acceptDarwinTestCommandExit(t, "wait terminated nested child", waitErr)
	}
}

func runDarwinOrphanRootHelper(t *testing.T) {
	pidFile := os.Getenv(darwinNestedPIDFile)
	if pidFile == "" {
		t.Fatal("orphan PID file is empty")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestDarwinProcessScopeCleansReparentedSession$")
	child.Env = append(os.Environ(), darwinNestedHelperMode+"=orphan-child")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := publishTestPID(pidFile, child.Process.Pid); err != nil {
		killAndWaitDarwinTestCommand(t, child, "orphan child after failed PID publication")
		t.Fatal(err)
	}
}

func killAndWaitDarwinTestCommand(t *testing.T, cmd *exec.Cmd, label string) {
	t.Helper()
	killDarwinTestCommand(t, cmd, "kill "+label)
	acceptDarwinTestCommandExit(t, "wait "+label, cmd.Wait())
}

func killDarwinTestCommand(t *testing.T, cmd *exec.Cmd, label string) {
	t.Helper()
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("%s: %v", label, err)
	}
}

func acceptDarwinTestCommandExit(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("%s: %v", label, err)
	}
}

func darwinProcargsFixture(arguments, environment []string) []byte {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, uint32(len(arguments)))
	data = append(data, []byte("/bin/provider")...)
	data = append(data, 0, 0)
	for _, argument := range arguments {
		data = append(data, []byte(argument)...)
		data = append(data, 0)
	}
	for _, entry := range environment {
		data = append(data, []byte(entry)...)
		data = append(data, 0)
	}
	return data
}

func darwinProcargsFixtureWithTail(argumentCount int, tail string) []byte {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, uint32(argumentCount))
	return append(data, []byte(tail)...)
}

func darwinUniqueIdentifierFixture(uniqueID uint64, pidVersion int32) []byte {
	data := make([]byte, darwinProcUniqIdentifierInfoSize)
	binary.LittleEndian.PutUint64(data[16:24], uniqueID)
	binary.LittleEndian.PutUint32(data[32:36], uint32(pidVersion))
	return data
}

func darwinKinfoFixture(pid int) *unix.KinfoProc {
	process := &unix.KinfoProc{}
	process.Proc.P_pid = int32(pid)
	process.Proc.P_starttime = unix.Timeval{Sec: 10, Usec: 20}
	return process
}

func waitForDarwinNestedPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil || pid <= 1 {
				t.Fatalf("parse nested PID %q: %v", data, parseErr)
			}
			return pid
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("nested child PID was not published")
	return 0
}

func killVerifiedTestProcess(t *testing.T, ref LaunchRef) {
	t.Helper()
	present, identity, err := inspectDarwinProcessIdentityInfo(ref.PID())
	if err != nil {
		t.Logf("observe cleanup PID %d: %v", ref.PID(), err)
		return
	}
	if !present || !processIdentityTokensMatch(identity.startToken, ref.StartToken()) {
		return
	}
	if _, err = signalDarwinProcessWith(context.Background(), ref, syscall.SIGKILL, inspectDarwinProcessIdentityInfo, signalDarwinProcess); err != nil && !errors.Is(err, errProcessNoSuchEntity) {
		t.Logf("kill cleanup PID %d: %v", ref.PID(), err)
	}
}
