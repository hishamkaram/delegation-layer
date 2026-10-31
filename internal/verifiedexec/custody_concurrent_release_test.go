package verifiedexec

// Concurrent release of one namespace. The canonical caller has already proven
// the owner closed, so every caller below releases a namespace whose commands
// were closed without ever being started. Pool.ReleaseOwner removes views and
// the witness outside the pool gate, so callers may overlap at any removal
// step; the contract is that every call returns nil, the namespace ends
// missing, a living owner keeps its views and image, and one later uncontended
// release leaves no idle image. A single release does not have to reclaim an
// image itself: its reclaim is best-effort and skips a busy gate.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

const (
	releaseRounds   = 5
	releaseChildren = 3
	// closedViews is the number of images, and so of views, each closed namespace holds.
	closedViews = 3

	releaseChildRootEnv  = "DELEGATE_CUSTODY_RELEASE_ROOT"
	releaseChildIDEnv    = "DELEGATE_CUSTODY_RELEASE_ID"
	releaseChildOwnerEnv = "DELEGATE_CUSTODY_RELEASE_OWNER"
	releaseChildTest     = "TestPoolReleaseChildProcess"
)

// releaseFixture is a pool with one living owner and three images. Image 0 is
// linked by the living owner and by every closed namespace; images 1 and 2 are
// linked by closed namespaces only, so only they are idle once a namespace is
// released.
type releaseFixture struct {
	rig     *poolRig
	entries [closedViews]string
	digests [closedViews]string
}

func newReleaseFixture(t *testing.T) *releaseFixture {
	t.Helper()
	f := &releaseFixture{rig: newPoolRig(t)}
	for i := range f.entries {
		f.entries[i], f.digests[i] = plainEntry(t, f.rig.dir, fmt.Sprintf("release-%d", i))
	}
	f.rig.seedView("living", plainRequest(f.entries[0], f.digests[0]))
	return f
}

// bindClosed leaves owner as a closed owner does: one view per image, every
// command closed. Close only closes local handles, so the views stay.
func (f *releaseFixture) bindClosed(owner string) {
	f.rig.t.Helper()
	for i := range f.entries {
		f.rig.seedView(owner, plainRequest(f.entries[i], f.digests[i]))
	}
	f.rig.requireViewDirs(owner, closedViews)
	for _, entry := range f.entries {
		f.rig.requireViewsLinkImages(owner, entry)
	}
}

// requireReleased asserts what every caller's return must have established:
// the namespace is gone for every pool handle and the living owner is intact.
func (f *releaseFixture) requireReleased(owner string, pools []*Pool) {
	rig := f.rig
	rig.t.Helper()
	for i, pool := range pools {
		if pending, err := pool.ReleasePending(rig.ns(owner)); err != nil || pending {
			rig.t.Fatalf("pool handle %d: %s pending %v (%v) after every release returned", i, owner, pending, err)
		}
	}
	if _, err := os.Lstat(rig.ownerDir(owner)); !errors.Is(err, os.ErrNotExist) {
		rig.t.Fatalf("witness of %s after every release returned: %v", owner, err)
	}
	rig.requireViewsLinkImages("living", f.entries[0])
}

// requireIdleImagesReclaimedBy runs the one uncontended release the contract
// leaves for reclaim, then requires that only the living owner's image remains.
func (f *releaseFixture) requireIdleImagesReclaimedBy(owner string) {
	rig := f.rig
	rig.t.Helper()
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns(owner)); err != nil {
		rig.t.Fatalf("uncontended release of the already released %s: %v", owner, err)
	}
	if got := rig.images(); !slices.Equal(got, []string{f.digests[0]}) {
		rig.t.Fatalf("images %v after the uncontended release; want only the living owner's %s", got, f.digests[0])
	}
	rig.requireImagesDir(1)
	entries, err := os.ReadDir(rig.pool.viewsPath())
	if err != nil || len(entries) != 1 || entries[0].Name() != "living" {
		rig.t.Fatalf("views directory holds %v (%v); want only the living owner", entries, err)
	}
	rig.requireViewsLinkImages("living", f.entries[0])
}

func (f *releaseFixture) releaseLiving() {
	rig := f.rig
	rig.t.Helper()
	if err := rig.pool.ReleaseOwner(context.Background(), rig.ns("living")); err != nil {
		rig.t.Fatal(err)
	}
	rig.requireImagesDir(0)
	if entries, err := os.ReadDir(rig.pool.viewsPath()); err != nil || len(entries) != 0 {
		rig.t.Fatalf("released pool still holds views: %v (%v)", entries, err)
	}
}

// siblingPools returns n pool handles on the rig's root and identity, each its
// own value with the rig's log and hook, as n independent callers would hold.
func siblingPools(rig *poolRig, n int, hook func(poolPoint, string)) []*Pool {
	rig.t.Helper()
	pools := make([]*Pool, n)
	for i := range pools {
		pool, err := NewPoolWithIdentity(rig.pool.root, rig.id)
		if err != nil {
			rig.t.Fatal(err)
		}
		pool.logf = rig.logf
		pool.faults.at = hook
		pools[i] = pool
	}
	return pools
}

// arrivalBarrier parks the first n arrivals until all n have arrived. Hooked
// on the first identity check of ReleaseOwner, which precedes every read of the
// owner, it makes all n callers observe the owner fully populated. release lets
// parked arrivals go without waiting for the rest.
type arrivalBarrier struct {
	t       *testing.T
	mu      sync.Mutex
	n       int
	arrived int
	all     chan struct{}
	aborted chan struct{}
	once    sync.Once
}

func newArrivalBarrier(t *testing.T, n int) *arrivalBarrier {
	return &arrivalBarrier{t: t, n: n, all: make(chan struct{}), aborted: make(chan struct{})}
}

func (b *arrivalBarrier) release() { b.once.Do(func() { close(b.aborted) }) }

func (b *arrivalBarrier) hook(point poolPoint, _ string) {
	if point != pointIdentityChecked {
		return
	}
	b.mu.Lock()
	b.arrived++
	mine := b.arrived
	if mine == b.n {
		close(b.all)
	}
	b.mu.Unlock()
	if mine > b.n {
		return
	}
	select {
	case <-b.all:
	case <-b.aborted:
	case <-time.After(handshakeTimeout):
		b.t.Error("the release callers never all reached their first identity check")
	}
}

// parkedAt parks the first caller to reach point until release is called and
// lets every other caller through.
type parkedAt struct {
	t      *testing.T
	point  poolPoint
	mu     sync.Mutex
	taken  bool
	parked chan struct{}
	resume chan struct{}
	once   sync.Once
}

func newParkedAt(t *testing.T, point poolPoint) *parkedAt {
	return &parkedAt{t: t, point: point, parked: make(chan struct{}), resume: make(chan struct{})}
}

func (p *parkedAt) release() { p.once.Do(func() { close(p.resume) }) }

func (p *parkedAt) hook(point poolPoint, _ string) {
	if point != p.point {
		return
	}
	p.mu.Lock()
	first := !p.taken
	p.taken = true
	p.mu.Unlock()
	if !first {
		return
	}
	close(p.parked)
	select {
	case <-p.resume:
	case <-time.After(handshakeTimeout):
		p.t.Errorf("the caller parked at %s was never resumed", p.point)
	}
}

// callResult is the outcome of one started call.
type callResult struct {
	caller int
	err    error
}

// releaseGroup owns every goroutine a test starts for concurrent releases. It
// is created after the fixture, so its cleanup runs before the fixture's
// scratch deletion: the cleanup cancels the calls' context, lets parked callers
// go and joins every started call, so no call outlives the pool files it uses
// and a failed test never abandons one. Each call receives the group's context,
// which the pool honors between view removals.
type releaseGroup struct {
	t       *testing.T
	ctx     context.Context
	cancel  context.CancelFunc
	unpark  func()
	results chan callResult
	wg      sync.WaitGroup

	mu       sync.Mutex
	started  int
	finished int
}

// newReleaseGroup returns a group that holds the results of up to capacity
// calls. unpark must be safe to call more than once and from any goroutine.
func newReleaseGroup(t *testing.T, capacity int, unpark func()) *releaseGroup {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	g := &releaseGroup{t: t, ctx: ctx, cancel: cancel, unpark: unpark, results: make(chan callResult, capacity)}
	t.Cleanup(g.cleanup)
	return g
}

// cleanup is the group's one teardown, registered with t.Cleanup: it cancels the
// calls, unparks them and does not return before every started call has.
func (g *releaseGroup) cleanup() {
	g.stop()
	g.wg.Wait()
}

func (g *releaseGroup) stop() {
	g.cancel()
	g.unpark()
}

func (g *releaseGroup) start(caller int, call func(context.Context) error) {
	g.mu.Lock()
	g.started++
	g.mu.Unlock()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.results <- callResult{caller: caller, err: call(g.ctx)}
		g.mu.Lock()
		g.finished++
		g.mu.Unlock()
	}()
}

// allJoined reports, without waiting, that at least one call started and every
// started call has returned.
func (g *releaseGroup) allJoined() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.started > 0 && g.started == g.finished
}

// join waits for every started call. At the deadline it cancels the calls and
// unparks them, still joins them, and only then fails the test.
func (g *releaseGroup) join(what string) {
	g.t.Helper()
	deadline := time.AfterFunc(handshakeTimeout, g.stop)
	g.wg.Wait()
	if !deadline.Stop() {
		g.t.Fatalf("%s did not finish within %v; the calls were canceled, unparked and joined", what, handshakeTimeout)
	}
}

// requireNext requires the next call to finish within the handshake bound with
// a nil error.
func (g *releaseGroup) requireNext(who string) {
	g.t.Helper()
	select {
	case result := <-g.results:
		if result.err != nil {
			g.t.Fatalf("%s (caller %d) returned %v; want nil", who, result.caller, result.err)
		}
	case <-time.After(handshakeTimeout):
		g.t.Fatalf("%s never returned", who)
	}
}

func releaseCall(pool *Pool, ns Namespace) func(context.Context) error {
	return func(ctx context.Context) error { return pool.ReleaseOwner(ctx, ns) }
}

func TestPoolConcurrentReleaseOfOneClosedNamespaceConverges(t *testing.T) {
	for _, callers := range []int{2, 3, 4} {
		t.Run(fmt.Sprintf("%d-callers", callers), func(t *testing.T) {
			f := newReleaseFixture(t)
			for round := range releaseRounds {
				owner := fmt.Sprintf("closed-%d", round)
				f.bindClosed(owner)
				barrier := newArrivalBarrier(t, callers)
				pools := siblingPools(f.rig, callers, barrier.hook)
				group := newReleaseGroup(t, callers, barrier.release)
				for caller, pool := range pools {
					group.start(caller, releaseCall(pool, f.rig.ns(owner)))
				}
				group.join(fmt.Sprintf("%d concurrent releases of %s", callers, owner))
				for range callers {
					group.requireNext("a concurrent release of " + owner)
				}
				f.requireReleased(owner, pools)
				f.requireIdleImagesReclaimedBy(owner)
			}
			f.releaseLiving()
			f.rig.rec.note("concurrent-release", "%d rounds x %d simultaneous callers of one closed namespace of %d views: every call returned nil, the living owner kept its image, the final uncontended release left only that image; logged %d lines", releaseRounds, callers, closedViews, len(f.rig.logged()))
		})
	}
}

// TestPoolReleaseOverlapsAnotherCallerAtEachRemovalStep parks one caller inside
// removeOwner, with the gate not held and its view list already read, while a
// second caller releases the whole namespace. The parked caller then resumes
// over a namespace that no longer exists.
func TestPoolReleaseOverlapsAnotherCallerAtEachRemovalStep(t *testing.T) {
	for _, point := range []poolPoint{pointAfterViewRemoved, pointBeforeWitnessRemove} {
		t.Run(string(point), func(t *testing.T) {
			f := newReleaseFixture(t)
			f.bindClosed("closed")
			parked := newParkedAt(t, point)
			pools := siblingPools(f.rig, 2, parked.hook)
			group := newReleaseGroup(t, len(pools), parked.release)
			for caller, pool := range pools {
				group.start(caller, releaseCall(pool, f.rig.ns("closed")))
			}
			awaitSignal(t, parked.parked, "a caller to park at "+string(point))
			group.requireNext("the unparked caller")
			f.requireReleased("closed", pools[:1])
			parked.release()
			group.requireNext("the resumed caller")
			group.join("both releases")
			f.requireReleased("closed", pools)
			f.requireIdleImagesReclaimedBy("closed")
			f.releaseLiving()
			f.rig.rec.note("overlap-"+string(point), "one caller parked at %s; the other released the namespace and returned nil; the parked one resumed over a missing owner and returned nil", point)
		})
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(handshakeTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

const (
	releaseFailureChildTest = "TestPoolReleaseFailureCleanupChild"
	releaseFailureChildEnv  = "DELEGATE_CUSTODY_RELEASE_FAILURE"
	// Modes of the failure child. The expected mode fails once, at the real
	// requireNext; the others add one more failure after the join marker.
	failureModeExpected     = "expected"
	failureModeCleanupError = "extra-cleanup-error"
	failureModeRaceStderr   = "race-stderr"
	extraCleanupError       = "extra cleanup error after the join marker"
	raceStderrReport        = "WARNING: DATA RACE"
	// releaseJoinedMarker is printed by the child's cleanup probe only when
	// every started call has been joined while the fixture still exists.
	releaseJoinedMarker = "custody-release-group: every started call joined before fixture deletion"
	failureChildSource  = "custody_concurrent_release_test.go"
)

var errReleaseSentinel = errors.New("sentinel result of the unparked caller")

// intendedFailureDiagnostic is the one diagnostic the expected failure child's
// requireNext prints for its sentinel result.
var intendedFailureDiagnostic = fmt.Sprintf("the unparked caller (caller 1) returned %v; want nil", errReleaseSentinel)

func isClosed(signal <-chan struct{}) bool {
	select {
	case <-signal:
		return true
	default:
		return false
	}
}

// TestReleaseGroupCleanupWaitsForCallsHeldByTheirOwnCompletion checks the
// group's canonical cleanup by itself in a synctest bubble. A call that has seen
// its context canceled is held by a completion gate the test owns, independent
// of cancel and unpark: the cleanup must have canceled and unparked yet must not
// return until the call completes. Without the join it returns while the call is
// held, deterministically.
func TestReleaseGroupCleanupWaitsForCallsHeldByTheirOwnCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		canceled := make(chan struct{})
		unparked := make(chan struct{})
		completion := make(chan struct{})
		var unparkOnce, completionOnce sync.Once
		group := newReleaseGroup(t, 1, func() { unparkOnce.Do(func() { close(unparked) }) })
		openCompletion := func() { completionOnce.Do(func() { close(completion) }) }
		// Registered after the group, so after a failed assertion the gate opens
		// before the group's own cleanup waits for the call.
		t.Cleanup(openCompletion)
		group.start(0, func(ctx context.Context) error {
			<-ctx.Done()
			close(canceled)
			<-completion
			return nil
		})
		returned := make(chan struct{})
		go func() {
			group.cleanup()
			close(returned)
		}()
		synctest.Wait()
		if !isClosed(canceled) || !isClosed(unparked) {
			t.Fatalf("cleanup left the call's context canceled=%v, parked callers unparked=%v; want both", isClosed(canceled), isClosed(unparked))
		}
		if isClosed(returned) {
			t.Fatal("cleanup returned while a started call was still held")
		}
		openCompletion()
		synctest.Wait()
		if !isClosed(returned) || !group.allJoined() {
			t.Fatalf("cleanup returned=%v, all joined=%v after the call completed; want both", isClosed(returned), group.allJoined())
		}
	})
}

// validateFailureChild requires exactly the output of the expected failure
// child, a Go 1.27 test binary run without -v: exit status 1, an empty stderr,
// and only these lines, each once: the join marker, the "--- FAIL" header with
// a duration, the intended diagnostic from this file with a line number, and the
// closing FAIL. When coverMode is not empty the instrumented child also prints
// exactly one framework coverage summary; when it is empty that line is as
// unexpected as any other. Any other line, including a second diagnostic, is an
// error.
func validateFailureChild(outcome workerOutcome, coverMode string) error {
	if outcome.Exit != 1 {
		return fmt.Errorf("exit status %d; want exactly 1", outcome.Exit)
	}
	if outcome.Stderr != "" {
		return fmt.Errorf("stderr %q; want it empty", outcome.Stderr)
	}
	seen := map[string]int{}
	for _, line := range strings.Split(strings.TrimSuffix(string(outcome.Stdout), "\n"), "\n") {
		kind, ok := failureChildLineKind(line)
		if !ok {
			return fmt.Errorf("unrecognized output line %q", line)
		}
		seen[kind]++
	}
	wantCoverage := 0
	if coverMode != "" {
		wantCoverage = 1
	}
	for kind, want := range map[string]int{"marker": 1, "header": 1, "diagnostic": 1, "closing": 1, "coverage": wantCoverage} {
		if seen[kind] != want {
			return fmt.Errorf("%d %s lines; want exactly %d", seen[kind], kind, want)
		}
	}
	return nil
}

func failureChildLineKind(line string) (kind string, ok bool) {
	if line == releaseJoinedMarker {
		return "marker", true
	}
	if line == "FAIL" {
		return "closing", true
	}
	if summary, found := strings.CutPrefix(line, "coverage: "); found {
		return "coverage", isCoverageSummary(summary)
	}
	if duration, found := strings.CutPrefix(line, "--- FAIL: "+releaseFailureChildTest+" ("); found {
		seconds, found := strings.CutSuffix(duration, "s)")
		_, parseErr := strconv.ParseFloat(seconds, 64)
		return "header", found && parseErr == nil
	}
	if rest, found := strings.CutPrefix(line, "    "+failureChildSource+":"); found {
		number, message, found := strings.Cut(rest, ": ")
		_, parseErr := strconv.Atoi(number)
		return "diagnostic", found && parseErr == nil && message == intendedFailureDiagnostic
	}
	return "", false
}

// isCoverageSummary reports whether summary, the text after "coverage: ", is
// one of the two forms the Go 1.27 framework prints (internal/coverage/cformat
// EmitPercent): "N% of statements" with an optional " in <patterns>" suffix
// that -coverpkg adds, or "[no statements]" for an empty selection. The suffix
// must hold a nonempty pattern list with no surrounding blanks.
func isCoverageSummary(summary string) bool {
	if summary == "[no statements]" {
		return true
	}
	percent, suffix, found := strings.Cut(summary, "% of statements")
	if !found || !isCoveragePercent(percent) {
		return false
	}
	if suffix == "" {
		return true
	}
	patterns, found := strings.CutPrefix(suffix, " in ")
	return found && patterns != "" && strings.TrimSpace(patterns) == patterns
}

// isCoveragePercent reports whether text is plain decimal digits with at most
// one inner point and a value from 0 to 100, the shape of the framework's
// "%.1f" percentage. ParseFloat alone would also accept signs, exponents, NaN
// and Inf.
func isCoveragePercent(text string) bool {
	const digits = "0123456789"
	whole, fraction, hasPoint := strings.Cut(text, ".")
	if whole == "" || strings.Trim(whole, digits) != "" {
		return false
	}
	if hasPoint && (fraction == "" || strings.Trim(fraction, digits) != "") {
		return false
	}
	value, err := strconv.ParseFloat(text, 64)
	return err == nil && value >= 0 && value <= 100
}

// runFailureChild re-executes this test binary as the failure child in mode and
// returns its captured outcome after waiting for that exact process.
func runFailureChild(t *testing.T, mode string) workerOutcome {
	t.Helper()
	cmd := selfCommand(t, releaseFailureChildTest, releaseFailureChildEnv+"="+mode)
	process := &workerProcess{cmd: cmd}
	cmd.Stdout, cmd.Stderr = &process.stdout, &process.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the failure child: %v", err)
	}
	t.Cleanup(func() {
		if _, err := process.reap(); err != nil {
			t.Errorf("cleanup of the failure child: %v", err)
		}
	})
	return process.wait(t)
}

// TestPoolReleaseFailureCleanupJoinsEveryCaller composes the real pieces: the
// child fails at the real requireNext while its other caller is parked, and
// must show exactly the intended failure and the join marker.
func TestPoolReleaseFailureCleanupJoinsEveryCaller(t *testing.T) {
	outcome := runFailureChild(t, failureModeExpected)
	if err := validateFailureChild(outcome, testing.CoverMode()); err != nil {
		t.Fatalf("the failure child: %v\nstdout:\n%s", err, outcome.Stdout)
	}
}

// TestFailureChildValidatorRejectsAdditionalFailures runs the child with one
// more failure after the join marker and before the fixture is deleted, and
// with a race report on stderr; neither may be accepted as the expected failure.
func TestFailureChildValidatorRejectsAdditionalFailures(t *testing.T) {
	for mode, injected := range map[string]string{failureModeCleanupError: extraCleanupError, failureModeRaceStderr: raceStderrReport} {
		t.Run(mode, func(t *testing.T) {
			outcome := runFailureChild(t, mode)
			err := validateFailureChild(outcome, testing.CoverMode())
			if err == nil {
				t.Fatalf("the validator accepted a child with an additional failure:\nstdout:\n%s\nstderr: %q", outcome.Stdout, outcome.Stderr)
			}
			if !strings.Contains(err.Error(), injected) {
				t.Fatalf("the validator rejected for another reason: %v (injected %q)", err, injected)
			}
		})
	}
}

// expectedFailureStdout is the stdout of a correct expected failure child,
// with extra lines appended after the closing FAIL.
func expectedFailureStdout(extra ...string) []byte {
	lines := append([]string{
		releaseJoinedMarker,
		"--- FAIL: " + releaseFailureChildTest + " (0.62s)",
		"    " + failureChildSource + ":579: " + intendedFailureDiagnostic,
		"FAIL",
	}, extra...)
	return []byte(strings.Join(lines, "\n") + "\n")
}

// TestFailureChildValidatorCoverageSummary pins the one coverage line the
// validator allows: exactly one well-formed framework summary when the test
// binary is instrumented, none otherwise, and every strict rule still applies
// with it.
func TestFailureChildValidatorCoverageSummary(t *testing.T) {
	const summary = "coverage: 54.7% of statements"
	child := func(lines ...string) workerOutcome {
		return workerOutcome{Exit: 1, Stdout: expectedFailureStdout(lines...)}
	}
	for _, line := range []string{
		summary, "coverage: 0.0% of statements", "coverage: 100.0% of statements",
		summary + " in ./internal/verifiedexec", summary + " in ./internal/verifiedexec, ./internal/provider/ccr",
		"coverage: [no statements]",
	} {
		for _, mode := range []string{"set", "count", "atomic"} {
			t.Run("enabled "+mode+" accepts "+line, func(t *testing.T) {
				requireValidatorVerdict(t, child(line), mode, "")
			})
		}
		t.Run("disabled rejects "+line, func(t *testing.T) {
			requireValidatorVerdict(t, child(line), "", "1 coverage lines; want exactly 0")
		})
	}
	for _, test := range []struct {
		name      string
		coverMode string
		outcome   workerOutcome
		wantErr   string
	}{
		{"disabled without a summary", "", child(), ""},
		{"enabled without a summary", "set", child(), "0 coverage lines; want exactly 1"},
		{"enabled with a duplicate summary", "atomic", child(summary, summary), "2 coverage lines; want exactly 1"},
		{"enabled with a duplicate suffixed summary", "set", child(summary+" in ./a", summary+" in ./a"), "2 coverage lines; want exactly 1"},
		{"enabled with a duplicate empty selection", "set", child("coverage: [no statements]", "coverage: [no statements]"), "2 coverage lines; want exactly 1"},
		{"enabled with a summary and an empty selection", "set", child(summary, "coverage: [no statements]"), "2 coverage lines; want exactly 1"},
		{"enabled with an extra diagnostic", "set", child(summary, "    "+failureChildSource+":580: "+extraCleanupError), "unrecognized output line"},
		{"enabled with an extra diagnostic after an empty selection", "set", child("coverage: [no statements]", "    "+failureChildSource+":580: "+extraCleanupError), "unrecognized output line"},
		{"enabled with stderr", "set", workerOutcome{Exit: 1, Stdout: expectedFailureStdout(summary), Stderr: raceStderrReport + "\n"}, "stderr"},
		{"enabled with exit zero", "set", workerOutcome{Exit: 0, Stdout: expectedFailureStdout(summary)}, "exit status 0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireValidatorVerdict(t, test.outcome, test.coverMode, test.wantErr)
		})
	}
	for _, malformed := range []string{
		"coverage: 100.1% of statements", "coverage: -1.0% of statements", "coverage: +5.0% of statements", "coverage: NaN% of statements",
		"coverage: Inf% of statements", "coverage: 1e1% of statements", "coverage: 5.% of statements", "coverage: .5% of statements",
		"coverage: % of statements", "coverage: 54.7%", "coverage: 54.7% of statements of ./...", "coverage: 54.7% of statements extra",
		summary + " in", summary + " in ", summary + " in  ", summary + " in ./a ", summary + "in ./a", summary + " in ./a\r",
		"coverage: [no statements] in ./a", "coverage: [no statements]x", "coverage: [no statements", "coverage: [none]", "coverage:", "coverage: ",
	} {
		t.Run("malformed "+malformed, func(t *testing.T) {
			requireValidatorVerdict(t, child(malformed), "set", "unrecognized output line")
		})
	}
}

// requireValidatorVerdict requires validateFailureChild to accept outcome when
// wantErr is empty, and otherwise to reject it with an error holding wantErr.
func requireValidatorVerdict(t *testing.T, outcome workerOutcome, coverMode, wantErr string) {
	t.Helper()
	err := validateFailureChild(outcome, coverMode)
	switch {
	case wantErr == "" && err != nil:
		t.Fatalf("the validator rejected a correct child: %v", err)
	case wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)):
		t.Fatalf("the validator gave %v; want a rejection holding %q", err, wantErr)
	}
}

// TestPoolReleaseFailureCleanupChild is the body of the failure child; a normal
// run skips it. One real release is parked while the other call returns the
// sentinel, so requireNext fails the test with the real Fatalf. The probe is
// registered after the fixture and before the group, so cleanup order runs the
// group's cleanup, then the probe, then any extra failure, then the fixture's
// deletion.
func TestPoolReleaseFailureCleanupChild(t *testing.T) {
	mode := os.Getenv(releaseFailureChildEnv)
	if mode == "" {
		t.Skip("body of the child of TestPoolReleaseFailureCleanupJoinsEveryCaller")
	}
	f := newReleaseFixture(t)
	f.bindClosed("closed")
	switch mode {
	case failureModeCleanupError:
		t.Cleanup(func() { t.Error(extraCleanupError) })
	case failureModeRaceStderr:
		t.Cleanup(func() { fmt.Fprintln(os.Stderr, raceStderrReport) })
	}
	var group *releaseGroup
	t.Cleanup(func() {
		_, dirErr := os.Stat(f.rig.dir)
		if group != nil && group.allJoined() && dirErr == nil {
			fmt.Println(releaseJoinedMarker)
		}
	})
	parked := newParkedAt(t, pointBeforeWitnessRemove)
	pool := siblingPools(f.rig, 1, parked.hook)[0]
	group = newReleaseGroup(t, 2, parked.release)
	group.start(0, releaseCall(pool, f.rig.ns("closed")))
	awaitSignal(t, parked.parked, "the real release to park")
	group.start(1, func(context.Context) error { return errReleaseSentinel })
	group.requireNext("the unparked caller")
}

// selfCommand prepares this test binary to run only test, with a minimal
// environment: the scratch settings the fixtures read, then env.
func selfCommand(t *testing.T, test string, env ...string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^"+test+"$")
	cmd.Env = append([]string{"TMPDIR=" + os.TempDir()}, env...)
	if scratchDir := os.Getenv(proofScratchEnv); scratchDir != "" {
		cmd.Env = append(cmd.Env, proofScratchEnv+"="+scratchDir)
	}
	return cmd
}

// childPipes are the four endpoints of the two pipes a release child needs:
// childIn and childOut belong to the child; start and ready stay with the
// parent.
type childPipes struct {
	childIn  *os.File
	start    *os.File
	ready    *os.File
	childOut *os.File
}

// openChildPipes allocates the child's stdin pipe, then its stdout pipe. When
// the second allocation fails it closes both endpoints of the first and returns
// every error.
func openChildPipes(pipe func() (*os.File, *os.File, error)) (childPipes, error) {
	childIn, start, err := pipe()
	if err != nil {
		return childPipes{}, fmt.Errorf("allocate the start pipe: %w", err)
	}
	ready, childOut, err := pipe()
	if err != nil {
		return childPipes{}, errors.Join(fmt.Errorf("allocate the ready pipe: %w", err), childIn.Close(), start.Close())
	}
	return childPipes{childIn: childIn, start: start, ready: ready, childOut: childOut}, nil
}

func (p childPipes) closeAll() error {
	return errors.Join(p.childIn.Close(), p.start.Close(), p.ready.Close(), p.childOut.Close())
}

func TestOpenChildPipesClosesTheFirstPairWhenTheSecondFails(t *testing.T) {
	errSecond := errors.New("sentinel failure of the second pipe")
	var firstIn, firstOut *os.File
	calls := 0
	_, err := openChildPipes(func() (*os.File, *os.File, error) {
		calls++
		if calls == 2 {
			return nil, nil, errSecond
		}
		var pipeErr error
		if firstIn, firstOut, pipeErr = os.Pipe(); pipeErr != nil {
			t.Fatalf("setup blocked: os.Pipe: %v", pipeErr)
		}
		return firstIn, firstOut, nil
	})
	if !errors.Is(err, errSecond) {
		t.Fatalf("openChildPipes gave %v; want the second pipe's sentinel", err)
	}
	for name, end := range map[string]*os.File{"child stdin end": firstIn, "start end": firstOut} {
		if _, statErr := end.Stat(); !errors.Is(statErr, os.ErrClosed) {
			t.Errorf("the %s of the first pair after the failure: %v; want it closed", name, statErr)
		}
	}
}

// releaseChild is one separate process parked inside ReleaseOwner at its first
// identity check, with the parent's ends of its start and ready lines.
type releaseChild struct {
	process *workerProcess
	start   *os.File
	ready   *bufio.Reader
}

// startReleaseChild re-executes this test binary as TestPoolReleaseChildProcess.
// The child is finite and sends no signal: the parent's Wait on that exact
// process is the oracle that it is gone. The cleanup is registered as soon as
// the child runs, so every later failure still closes the start line, waits for
// the child and then closes the ready line.
func startReleaseChild(t *testing.T, root, id, owner string) *releaseChild {
	t.Helper()
	cmd := selfCommand(t, releaseChildTest, releaseChildRootEnv+"="+root, releaseChildIDEnv+"="+id, releaseChildOwnerEnv+"="+owner)
	pipes, err := openChildPipes(os.Pipe)
	if err != nil {
		t.Fatalf("setup blocked: %v", err)
	}
	child := &releaseChild{process: &workerProcess{cmd: cmd}, start: pipes.start, ready: bufio.NewReader(pipes.ready)}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pipes.childIn, pipes.childOut, &child.process.stderr
	if err = cmd.Start(); err != nil {
		t.Fatalf("start release child: %v", errors.Join(err, pipes.closeAll()))
	}
	t.Cleanup(func() {
		if closeErr := pipes.start.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			t.Errorf("close the start line of a release child: %v", closeErr)
		}
		if _, reapErr := child.process.reap(); reapErr != nil {
			t.Errorf("cleanup of a release child: %v", reapErr)
		}
		if closeErr := pipes.ready.Close(); closeErr != nil {
			t.Errorf("close the ready line of a release child: %v", closeErr)
		}
	})
	if err = errors.Join(pipes.childIn.Close(), pipes.childOut.Close()); err != nil {
		t.Fatalf("close the child's ends of its pipes: %v", err)
	}
	return child
}

func (c *releaseChild) awaitReady(t *testing.T) {
	t.Helper()
	line := make(chan error, 1)
	go func() {
		text, err := c.ready.ReadString('\n')
		if err == nil && text != "READY\n" {
			err = fmt.Errorf("first line %q; want READY", text)
		}
		line <- err
	}()
	select {
	case err := <-line:
		if err != nil {
			t.Fatalf("a release child did not become ready: %v", err)
		}
	case <-time.After(handshakeTimeout):
		t.Fatal("a release child never became ready")
	}
}

func (c *releaseChild) sendGo(t *testing.T) {
	t.Helper()
	if _, err := fmt.Fprintln(c.start, "GO"); err != nil {
		t.Fatalf("send GO to a release child: %v", err)
	}
	if err := c.start.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestPoolReleaseChildProcess is the body of a release child; a normal run
// skips it. It parks at its first identity check, which precedes every read of
// the owner, until the parent has seen every child ready and sends GO.
func TestPoolReleaseChildProcess(t *testing.T) {
	root := os.Getenv(releaseChildRootEnv)
	if root == "" {
		t.Skip("body of the release children of TestPoolConcurrentReleaseOfOneClosedNamespaceAcrossProcesses")
	}
	if err := runReleaseChild(root, os.Getenv(releaseChildIDEnv), os.Getenv(releaseChildOwnerEnv)); err != nil {
		fmt.Fprintf(os.Stderr, "release child: %v\n", err)
		os.Exit(exitWorkerFailed)
	}
}

func runReleaseChild(root, id, owner string) error {
	pool, err := NewPoolWithIdentity(root, id)
	if err != nil {
		return err
	}
	ns, err := NewNamespace(owner)
	if err != nil {
		return err
	}
	var once sync.Once
	handshake := ""
	pool.faults.at = func(point poolPoint, _ string) {
		if point == pointIdentityChecked {
			once.Do(func() { handshake = helperHandshake() })
		}
	}
	releaseErr := pool.ReleaseOwner(context.Background(), ns)
	if handshake != "go" {
		return errors.Join(fmt.Errorf("start handshake ended %q; want go", handshake), releaseErr)
	}
	return releaseErr
}

func TestPoolConcurrentReleaseOfOneClosedNamespaceAcrossProcesses(t *testing.T) {
	f := newReleaseFixture(t)
	for round := range releaseRounds {
		owner := fmt.Sprintf("closed-%d", round)
		f.bindClosed(owner)
		children := make([]*releaseChild, releaseChildren)
		for i := range children {
			children[i] = startReleaseChild(t, f.rig.pool.root, f.rig.id, owner)
		}
		for _, child := range children {
			child.awaitReady(t)
		}
		for _, child := range children {
			child.sendGo(t)
		}
		for i, child := range children {
			if outcome := child.process.wait(t); outcome.Exit != 0 {
				t.Fatalf("round %d release child %d exited %d: %s", round, i, outcome.Exit, outcome.Stderr)
			}
		}
		f.requireReleased(owner, []*Pool{f.rig.pool})
		f.requireIdleImagesReclaimedBy(owner)
	}
	f.releaseLiving()
	f.rig.rec.note("concurrent-release-processes", "%d rounds x %d processes released one closed namespace of %d views while parked together inside ReleaseOwner: every process exited 0 and the final uncontended release left only the living owner's image", releaseRounds, releaseChildren, closedViews)
}
