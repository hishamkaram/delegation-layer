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

func TestDarwinProcInfoErrnoUsesLowWord(t *testing.T) {
	const raw = uintptr(0x6d88400000000003)
	if got := darwinProcInfoErrno(raw); got != syscall.ESRCH {
		t.Fatalf("Darwin proc_info errno = %v, want ESRCH", got)
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
	if runDarwinNestedSessionHelperMode(t) {
		return
	}
	fixture := startDarwinNestedSessionFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fixture.scope.Terminate(ctx, []LaunchRef{fixture.root.ref}); err != nil {
		t.Fatalf("nested-session Terminate: %v", err)
	}
	acceptDarwinTestCommandExit(t, "wait nested-session root", fixture.root.wait(t))
	assertDarwinProcessAbsent(t, fixture.root.ref.PID(), "nested-session root")
	assertDarwinProcessAbsent(t, fixture.child.PID(), "nested-session child")
	assertDarwinProcessPresent(t, fixture.unrelated, "unrelated same-user process did not survive process-scope termination")
}

func TestDarwinProcessScopeCleansReparentedSession(t *testing.T) {
	if runDarwinOrphanSessionHelperMode(t) {
		return
	}
	fixture := startDarwinOrphanSessionFixture(t)
	if waitErr := fixture.root.wait(t); waitErr != nil {
		t.Fatalf("orphan root helper: %v", waitErr)
	}
	assertDarwinProcessAbsent(t, fixture.root.ref.PID(), "root after Wait")
	empty, err := fixture.scope.Empty([]LaunchRef{fixture.root.ref})
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Fatal("marked reparented child was not retained by the process scope")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = fixture.scope.Terminate(ctx, []LaunchRef{fixture.root.ref}); err != nil {
		t.Fatalf("reparented-session Terminate: %v", err)
	}
	assertDarwinProcessAbsent(t, fixture.child.PID(), "reparented child")
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

// darwinTestCommand owns the fixture command's single Wait; its scope owns termination.
type darwinTestCommand struct {
	cmd      *exec.Cmd
	scope    *Scope
	ref      LaunchRef
	retained []LaunchRef
	waited   bool
}

type darwinNestedSessionFixture struct {
	scope     *Scope
	root      *darwinTestCommand
	child     LaunchRef
	unrelated ProcessRef
}

type darwinOrphanSessionFixture struct {
	scope *Scope
	root  *darwinTestCommand
	child LaunchRef
}

func assertDarwinCommandStartsPrivateSession(t *testing.T, scope *Scope, cmd *exec.Cmd) {
	t.Helper()
	command := startConfiguredDarwinTestCommand(t, scope, cmd)
	sessionID, err := syscall.Getsid(command.ref.PID())
	if err != nil {
		t.Fatal(err)
	}
	if command.ref.PGID() != command.ref.PID() || sessionID != command.ref.PID() {
		t.Fatalf(
			"provider session = PID %d PGID %d SID %d, want a private session",
			command.ref.PID(),
			command.ref.PGID(),
			sessionID,
		)
	}
	terminateDarwinTestScope(t, scope, []LaunchRef{command.ref}, "private-session command")
	acceptDarwinTestCommandExit(t, "wait private-session command", command.wait(t))
}

func runDarwinNestedSessionHelperMode(t *testing.T) bool {
	t.Helper()
	switch os.Getenv(darwinNestedHelperMode) {
	case "root":
		runDarwinNestedRootHelper(t)
	case "child":
		runDarwinTermResistantHelper()
	default:
		return false
	}
	return true
}

func runDarwinOrphanSessionHelperMode(t *testing.T) bool {
	t.Helper()
	switch os.Getenv(darwinNestedHelperMode) {
	case "orphan-root":
		runDarwinOrphanRootHelper(t)
	case "orphan-child":
		runDarwinTermResistantHelper()
	default:
		return false
	}
	return true
}

func runDarwinTermResistantHelper() {
	signal.Ignore(syscall.SIGTERM)
	for {
		time.Sleep(time.Hour)
	}
}

func startDarwinNestedSessionFixture(t *testing.T) darwinNestedSessionFixture {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "nested.pid")
	scope := openDarwinTestScope(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDarwinProcessScopeCleansNestedSessionAndPreservesUnrelatedProcess$")
	cmd.Env = append(os.Environ(), darwinNestedHelperMode+"=root", darwinNestedPIDFile+"="+pidFile)
	root := startDarwinScopedTestCommand(t, scope, cmd)
	child := observeDarwinNestedSession(t, pidFile, root.ref)
	root.retained = append(root.retained, child)
	registerDarwinScopeProcessCleanup(t, scope, []LaunchRef{root.ref, child}, "nested-session scope")

	return darwinNestedSessionFixture{
		scope:     scope,
		root:      root,
		child:     child,
		unrelated: startDarwinUnrelatedProcess(t),
	}
}

func startDarwinOrphanSessionFixture(t *testing.T) darwinOrphanSessionFixture {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	scope := openDarwinTestScope(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDarwinProcessScopeCleansReparentedSession$")
	cmd.Env = append(os.Environ(), darwinNestedHelperMode+"=orphan-root", darwinNestedPIDFile+"="+pidFile)
	root := startDarwinScopedTestCommand(t, scope, cmd)
	child := observeDarwinPublishedProcess(t, pidFile)
	root.retained = append(root.retained, child)
	registerDarwinScopeProcessCleanup(t, scope, []LaunchRef{root.ref, child}, "reparented-session scope")

	return darwinOrphanSessionFixture{scope: scope, root: root, child: child}
}

func openDarwinTestScope(t *testing.T) *Scope {
	t.Helper()
	scope, err := Open(testProcessGroupRef(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = scope.BindOwnerProcess(); err != nil {
		cleanupProcessGroupTestScope(t, scope)
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupProcessGroupTestScope(t, scope) })
	return scope
}

func startDarwinScopedTestCommand(t *testing.T, scope *Scope, cmd *exec.Cmd) *darwinTestCommand {
	t.Helper()
	if err := scope.Configure(cmd); err != nil {
		t.Fatal(err)
	}
	return startConfiguredDarwinTestCommand(t, scope, cmd)
}

func startConfiguredDarwinTestCommand(t *testing.T, scope *Scope, cmd *exec.Cmd) *darwinTestCommand {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	command := &darwinTestCommand{cmd: cmd, scope: scope}
	t.Cleanup(func() { command.cleanup(t) })
	ref, err := scope.ObserveStarted(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	command.ref = ref
	return command
}

func (c *darwinTestCommand) wait(t *testing.T) error {
	t.Helper()
	if c.waited {
		t.Fatal("Darwin test command Wait called more than once")
	}
	err := c.cmd.Wait()
	c.waited = true
	return err
}

func (c *darwinTestCommand) cleanup(t *testing.T) {
	t.Helper()
	if c.waited {
		return
	}
	refs := append([]LaunchRef(nil), c.retained...)
	if c.ref.PID() > 0 {
		refs = append([]LaunchRef{c.ref}, refs...)
	}
	terminateDarwinTestScope(t, c.scope, refs, "test command cleanup")
	acceptDarwinTestCommandExit(t, "wait test command cleanup", c.wait(t))
}

func startDarwinUnrelatedProcess(t *testing.T) ProcessRef {
	t.Helper()
	scope := openDarwinTestScope(t)
	command := startDarwinScopedTestCommand(t, scope, exec.Command("sleep", "60"))
	ref, err := NewProcessRef(command.ref.PID(), command.ref.StartToken())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		present, presentErr := ProcessPresent(ref)
		if presentErr != nil {
			t.Errorf("observe unrelated cleanup process: %v", presentErr)
		} else if !present {
			t.Error("unrelated same-user process was killed before test cleanup")
		}
	})
	return ref
}

func observeDarwinNestedSession(t *testing.T, pidFile string, root LaunchRef) LaunchRef {
	t.Helper()
	child := observeDarwinPublishedProcess(t, pidFile)
	sessionID, err := syscall.Getsid(child.PID())
	if err != nil {
		t.Fatal(err)
	}
	if child.PGID() != child.PID() || sessionID != child.PID() || child.PGID() == root.PGID() {
		t.Fatalf(
			"nested child scope = PID %d PGID %d SID %d, root PGID %d",
			child.PID(),
			child.PGID(),
			sessionID,
			root.PGID(),
		)
	}
	return child
}

func observeDarwinPublishedProcess(t *testing.T, pidFile string) LaunchRef {
	t.Helper()
	pid := waitForDarwinNestedPID(t, pidFile)
	return observeDarwinStartedCommand(t, pid)
}

func observeDarwinStartedCommand(t *testing.T, pid int) LaunchRef {
	t.Helper()
	present, token, err := observeStartedProcessIdentity(pid)
	if err != nil || !present {
		t.Fatalf("observe Darwin test process PID %d: present=%t err=%v", pid, present, err)
	}
	pgid, err := processGroupID(pid)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := NewLaunchRef(pid, token, pgid)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func assertDarwinProcessAbsent(t *testing.T, pid int, label string) {
	t.Helper()
	present, _, err := observeProcessIdentity(pid)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatalf("%s PID %d survived termination", label, pid)
	}
}

func assertDarwinProcessPresent(t *testing.T, ref ProcessRef, message string) {
	t.Helper()
	present, err := ProcessPresent(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal(message)
	}
}

func registerDarwinScopeProcessCleanup(t *testing.T, scope *Scope, refs []LaunchRef, label string) {
	t.Helper()
	t.Cleanup(func() { terminateDarwinTestScope(t, scope, refs, label) })
}

func terminateDarwinTestScope(t *testing.T, scope *Scope, refs []LaunchRef, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := scope.Terminate(ctx, refs); err != nil {
		t.Errorf("terminate %s: %v", label, err)
		for _, ref := range refs {
			killVerifiedTestProcess(t, ref)
		}
	}
}

func startDarwinIndependentTestCommand(t *testing.T, cmd *exec.Cmd) LaunchRef {
	t.Helper()
	if err := ConfigureIndependentSession(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return observeDarwinStartedCommand(t, cmd.Process.Pid)
}

func runDarwinNestedRootHelper(t *testing.T) {
	pidFile := os.Getenv(darwinNestedPIDFile)
	if pidFile == "" {
		t.Fatal("nested PID file is empty")
	}
	child := exec.Command(os.Args[0], "-test.run=^TestDarwinProcessScopeCleansNestedSessionAndPreservesUnrelatedProcess$")
	child.Env = append(os.Environ(), darwinNestedHelperMode+"=child")
	childRef := startDarwinIndependentTestCommand(t, child)
	if err := publishTestPID(pidFile, child.Process.Pid); err != nil {
		killVerifiedTestProcess(t, childRef)
		acceptDarwinTestCommandExit(t, "wait nested child after failed PID publication", child.Wait())
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
	childRef := startDarwinIndependentTestCommand(t, child)
	if err := publishTestPID(pidFile, child.Process.Pid); err != nil {
		killVerifiedTestProcess(t, childRef)
		acceptDarwinTestCommandExit(t, "wait orphan child after failed PID publication", child.Wait())
		t.Fatal(err)
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
