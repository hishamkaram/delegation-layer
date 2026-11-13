package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
)

const (
	nativePrivateObservationTimeout = 15 * time.Second
	nativeLabelQueryTimeout         = time.Minute
	// nativeStashedRowCommand is a dummy command for rows that stay stashed;
	// no row is ever enqueued, so it never runs.
	nativeStashedRowCommand = "/usr/bin/true"
)

// nativePrivateSupervisorFixture is a test-owned Delegate root for the real Pueue
// pair named by DELEGATE_TEST_PUEUE and DELEGATE_TEST_PUEUED. Every daemon it
// binds is stopped through its exact private config when the test ends.
type nativePrivateSupervisorFixture struct {
	clientPath string
	daemonPath string
	stateRoot  string
	lifetime   *nativePrivateFixtureLifetime
}

// The fixture, rather than an automatic ancestor TempDir cleanup, owns removal.
// Any failed daemon/client closure retains the entire root for diagnosis.
type nativePrivateFixtureLifetime struct{ retained bool }

func newNativePrivateSupervisorFixture(t *testing.T) nativePrivateSupervisorFixture {
	t.Helper()
	clientPath := os.Getenv("DELEGATE_TEST_PUEUE")
	daemonPath := os.Getenv("DELEGATE_TEST_PUEUED")
	if clientPath == "" || daemonPath == "" {
		t.Skip("native supervisor paths are not configured")
	}
	base, err := os.MkdirTemp("/tmp", "pyn-")
	if err != nil {
		t.Fatal(err)
	}
	lifetime := &nativePrivateFixtureLifetime{}
	t.Cleanup(func() {
		if lifetime.retained {
			t.Errorf("retained native private supervisor fixture after unresolved cleanup: %s", base)
			return
		}
		if removeErr := os.RemoveAll(base); removeErr != nil {
			t.Error(removeErr)
		}
	})
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(resolved, "state")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateStateRootRecord(t, stateRoot)
	return nativePrivateSupervisorFixture{clientPath: clientPath, daemonPath: daemonPath, stateRoot: stateRoot, lifetime: lifetime}
}

// bind owns cleanup before startup. Preparing the client under the bootstrap
// lock gives cleanup its exact endpoint even when StartPrivate fails after Start.
// The recorded identity and actual Wait survive later PID-record removal.
type nativePrivateBindingFixture struct {
	client   *Client
	process  privateProcessIdentity
	waits    []<-chan struct{}
	lifetime *nativePrivateFixtureLifetime
	// Captured at construction; fault tests may change client options but must
	// never revoke the creator's qualified cleanup selection.
	cleanupOptions Options
}

func (owned *nativePrivateBindingFixture) stop(ctx context.Context) error {
	if owned.client == nil || (owned.process.PID == 0 && len(owned.waits) == 0) {
		return nil
	}
	if owned.process.PID == 0 {
		var identityErr error
		owned.process, _, identityErr = owned.client.privateDaemonProcessIdentity()
		if identityErr != nil {
			return identityErr
		}
	}
	if owned.cleanupOptions.ControlOwnerExecutable != "" {
		return owned.stopManaged(ctx)
	}
	return stopNativePrivateSupervisor(ctx, owned.client, owned.process)
}

func (owned *nativePrivateBindingFixture) stopManaged(ctx context.Context) (resultErr error) {
	root, private := privateStateRoot(owned.client.Binding().ConfigPath)
	if !private {
		return ErrConfiguration
	}
	var client *Client
	defer func() {
		if client != nil {
			resultErr = errors.Join(resultErr, closeNativePrivateFixtureClient(ctx, client))
		}
	}()
	var refusal error
	if err := WithExistingBootstrapLockContext(ctx, root, func(locked context.Context) error {
		var prepareErr error
		client, prepareErr = owned.prepareManagedCleanup(locked, root)
		if prepareErr != nil {
			return prepareErr
		}
		var stopErr error
		refusal, stopErr = beginNativePrivateSupervisorStop(locked, client)
		return stopErr
	}); err != nil {
		return err
	}
	// The exact daemon and original Wait are observed only after EX release.
	return awaitNativePrivateSupervisorStop(ctx, client, owned.process, refusal)
}

func (owned *nativePrivateBindingFixture) prepareManagedCleanup(ctx context.Context, root string) (*Client, error) {
	published, found, err := loadPrivateSupervisorBinding(root)
	if err != nil || !found || published != owned.client.Binding() {
		return nil, errors.Join(ErrBinding, err)
	}
	client, pairErr, err := preparePublishedPrivatePair(ctx, published, owned.cleanupOptions)
	return client, errors.Join(pairErr, err)
}

func (owned *nativePrivateBindingFixture) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nativePrivateObservationTimeout)
	defer cancel()
	if stopErr := owned.stop(ctx); stopErr != nil {
		owned.lifetime.retained = true
		t.Errorf("native private supervisor cleanup failed: %v", stopErr)
	}
	if owned.client != nil {
		if closeErr := closeNativePrivateFixtureClient(ctx, owned.client); closeErr != nil {
			owned.lifetime.retained = true
			t.Errorf("native private supervisor client close failed or did not join: %v", closeErr)
		}
	}
	for _, done := range owned.waits {
		select {
		case <-done:
		case <-ctx.Done():
			owned.lifetime.retained = true
			t.Errorf("native daemon Wait did not finish: %v", ctx.Err())
		}
	}
}

func closeNativePrivateFixtureClient(ctx context.Context, client *Client) error {
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case closeErr := <-closed:
		return closeErr
	case <-ctx.Done():
		return errors.Join(errors.New("native fixture client close did not join; retain its directory"), ctx.Err())
	}
}

func (fixture nativePrivateSupervisorFixture) bind(t *testing.T) (*Client, error) {
	t.Helper()
	return fixture.bindWithDaemonWaitOwner(t, nil)
}

func (fixture nativePrivateSupervisorFixture) bindWithDaemonWaitOwner(t *testing.T, decorate func(DaemonWaitOwner) DaemonWaitOwner) (*Client, error) {
	t.Helper()
	return fixture.bindWithControlOwner(t, decorate, "")
}

func (fixture nativePrivateSupervisorFixture) bindWithControlOwner(t *testing.T, decorate func(DaemonWaitOwner) DaemonWaitOwner, controlOwner string) (*Client, error) {
	t.Helper()
	owned := &nativePrivateBindingFixture{lifetime: fixture.lifetime}
	t.Cleanup(func() { owned.cleanup(t) })
	options := Options{
		PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{},
		ObservationTimeout:     nativePrivateObservationTimeout,
		ControlOwnerExecutable: controlOwner,
		DaemonWaitOwner: daemonWaitOwnerFunc(func(_ string, _ int, done <-chan struct{}) error {
			owned.waits = append(owned.waits, done)
			return nil
		}),
	}
	if decorate != nil {
		options.DaemonWaitOwner = decorate(options.DaemonWaitOwner)
	}
	owned.cleanupOptions = options
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		var prepareErr error
		owned.client, prepareErr = PreparePrivateWithLock(ctx, fixture.clientPath, fixture.daemonPath, fixture.stateRoot, options)
		if prepareErr != nil {
			return prepareErr
		}
		return owned.client.StartPrivate(ctx, fixture.stateRoot)
	})
	if err != nil {
		return nil, err
	}
	var present bool
	var identityErr error
	owned.process, present, identityErr = owned.client.privateDaemonProcessIdentity()
	if identityErr != nil || !present {
		fixture.lifetime.retained = true
		t.Fatalf("native daemon process was not recorded at bind: present=%t err=%v", present, identityErr)
	}
	return owned.client, nil
}

func TestPrivateSupervisorNativeDaemonBirth(t *testing.T) {
	fixture := newNativePrivateSupervisorFixture(t)
	owner := &daemonBirthWaitOwnerForTest{}
	client, err := fixture.bindWithDaemonWaitOwner(t, func(legacy DaemonWaitOwner) DaemonWaitOwner {
		owner.legacy = legacy
		return owner
	})
	if err != nil {
		t.Fatal(err)
	}
	if owner.birthCalls != 1 || owner.legacyCalls != 0 {
		t.Fatalf("native daemon registration changed: birth=%d legacy=%d", owner.birthCalls, owner.legacyCalls)
	}
	birth, birthErr := awaitDaemonBirth(t, owner.birth)
	if birthErr != nil {
		t.Fatal(birthErr)
	}
	process, present, processErr := client.privateDaemonProcessIdentity()
	if processErr != nil || !present {
		t.Fatalf("native daemon identity missing: present=%t err=%v", present, processErr)
	}
	expected, identityErr := lifetime.NewProcessRef(process.PID, process.StartToken)
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	same, comparisonErr := lifetime.SameProcess(birth, expected)
	if comparisonErr != nil || !same {
		t.Fatalf("native birth differs from bound daemon: same=%t err=%v", same, comparisonErr)
	}
}

// stopNativePrivateSupervisor gracefully stops a daemon that still answers at
// the client's exact endpoint, then claims closure only after the canonical
// wait proves both the endpoint and the given process gone. A refused or
// absent endpoint is never closure by itself; its refusal stays attached to
// a failed wait.
func stopNativePrivateSupervisor(ctx context.Context, client *Client, process privateProcessIdentity) error {
	refusal, err := beginNativePrivateSupervisorStop(ctx, client)
	if err != nil {
		return err
	}
	return awaitNativePrivateSupervisorStop(ctx, client, process, refusal)
}

// beginNativePrivateSupervisorStop observes the client's exact endpoint and
// asks a daemon that still answers there to shut down. It returns the
// endpoint refusal, if any, for the later wait to keep; any other observation
// or shutdown fault is the error.
func beginNativePrivateSupervisorStop(ctx context.Context, client *Client) (refusal, err error) {
	reachable, refusal := client.Reachable(ctx)
	if refusal != nil && !errors.Is(refusal, ErrPrivateEndpointRefused) {
		return nil, refusal
	}
	if reachable {
		if err = client.Shutdown(ctx); err != nil {
			return nil, err
		}
	}
	return refusal, nil
}

// awaitNativePrivateSupervisorStop claims closure only after the canonical
// wait proves both the endpoint and the given process gone. A failed wait
// keeps the refusal observed when the stop began.
func awaitNativePrivateSupervisorStop(ctx context.Context, client *Client, process privateProcessIdentity, refusal error) error {
	if err := client.waitUnavailableWithProcess(ctx, process); err != nil {
		return errors.Join(err, refusal)
	}
	return nil
}

func (fixture nativePrivateSupervisorFixture) pidPath() string {
	return filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "run", "pueue.pid")
}

// stopRecordedDaemon gracefully stops the bound daemon through its exact
// config. It returns the recorded process identity and PID record after the
// endpoint and that exact process are both proven gone.
func (fixture nativePrivateSupervisorFixture) stopRecordedDaemon(t *testing.T, client *Client) (privateProcessIdentity, []byte) {
	t.Helper()
	process, present, err := client.privateDaemonProcessIdentity()
	if err != nil || !present {
		t.Fatalf("native daemon process was not recorded: present=%t err=%v", present, err)
	}
	recordedPID, err := os.ReadFile(fixture.pidPath())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativePrivateObservationTimeout)
	defer cancel()
	if err = client.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err = client.waitUnavailableWithProcess(ctx, process); err != nil {
		t.Fatal(err)
	}
	if absent, absentErr := client.privateProcessUnavailable(process); absentErr != nil || !absent {
		t.Fatalf("recorded native daemon remained: absent=%t err=%v", absent, absentErr)
	}
	return process, recordedPID
}

func TestPrivateSupervisorNativeBootstrap(t *testing.T) {
	fixture := newNativePrivateSupervisorFixture(t)
	client, err := fixture.bind(t)
	if err != nil {
		t.Fatal(err)
	}
	binding := client.Binding()
	if strings.TrimSpace(binding.ObservedVersion) == "" {
		t.Fatal("native private supervisor did not report a version")
	}
	privateBase := filepath.Join(fixture.stateRoot, privateSupervisorDirectory)
	if !strings.HasPrefix(binding.ConfigPath, privateBase+string(filepath.Separator)) {
		t.Fatalf("private config escaped state root: %q", binding.ConfigPath)
	}
	if err := client.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.stopRecordedDaemon(t, client)
}

// TestPrivateSupervisorNativeRefusedEndpointRepair models an unclean exit of a
// real daemon: its recorded PID is absent and a refused socket inode remains at
// the exact endpoint. Without a recorded PID the owner is unknown; with the
// exact dead PID recorded, the normal bind removes only that socket and starts
// the same generation.
func TestPrivateSupervisorNativeRefusedEndpointRepair(t *testing.T) {
	fixture := newNativePrivateSupervisorFixture(t)
	client, err := fixture.bind(t)
	if err != nil {
		t.Fatal(err)
	}
	binding := client.Binding()
	process, recordedPID := fixture.stopRecordedDaemon(t, client)
	refused := refusedPrivateEndpointFixture{
		privateSupervisorTestFixture: privateSupervisorTestFixture{stateRoot: fixture.stateRoot},
		socketPath:                   strings.TrimPrefix(binding.Endpoint, "unix:"),
	}
	refused.socket = listenRefusedPrivateTestSocket(t, refused.socketPath)

	removePrivateTestPID(t, fixture.stateRoot)
	if _, err = fixture.bind(t); err == nil {
		t.Fatal("refused endpoint without a recorded PID was repaired")
	}
	assertRefusedEndpointReason(t, err, privateEndpointOwnerUnknownReason)
	refused.assertSocketPreserved(t)

	// Restore the exact PID record an unclean exit would have left behind.
	if err = os.WriteFile(fixture.pidPath(), recordedPID, 0o600); err != nil {
		t.Fatal(err)
	}
	repaired, err := fixture.bind(t)
	if err != nil {
		t.Fatalf("refused endpoint of the absent recorded daemon was not repaired: %v", err)
	}
	if !privateFullGenerationMatches(repaired.Binding(), binding) {
		t.Fatalf("repair changed the binding: got %+v want %+v", repaired.Binding(), binding)
	}
	if err = repaired.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	successor, present, err := repaired.privateDaemonProcessIdentity()
	if err != nil || !present || successor == process {
		t.Fatalf("repair did not start a new recorded daemon: process=%+v present=%t err=%v", successor, present, err)
	}
	// Device/inode values may be reused after unlink; readiness and the new
	// daemon identity above prove repair, while this checks the endpoint type.
	info, statErr := os.Lstat(refused.socketPath)
	if statErr != nil {
		t.Fatalf("repaired endpoint could not be inspected: %v", statErr)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("repaired endpoint is not a Unix socket: mode=%s", info.Mode())
	}
}

// TestPrivateSupervisorNativeEndpointObservations records real Pueue status
// and group completions against one test-owned endpoint while it is live,
// absent after graceful shutdown, and refused, on the direct path and through
// the one-shot control owner of this binary. Absent and refused facts must
// come only from clean ordinary completions.
func TestPrivateSupervisorNativeEndpointObservations(t *testing.T) {
	fixture := newNativePrivateSupervisorFixture(t)
	direct, err := fixture.bind(t)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := NewClient(direct.Binding(), Options{PrivateGenerationGuard: allowPrivateGenerationForTest{}, DaemonStartGuard: allowDaemonStartForTest{}, ObservationTimeout: nativePrivateObservationTimeout, ControlOwnerExecutable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := owner.Close(); closeErr != nil {
			t.Errorf("native owner client close failed: %v", closeErr)
		}
	})
	clients := map[string]*Client{"direct": direct, "owner": owner}
	assertNativeEndpointObservations(t, clients, PrivateEndpointReady)
	fixture.stopRecordedDaemon(t, direct)
	assertNativeEndpointObservations(t, clients, PrivateEndpointAbsent)
	listenRefusedPrivateTestSocket(t, strings.TrimPrefix(direct.Binding().Endpoint, "unix:"))
	assertNativeEndpointObservations(t, clients, PrivateEndpointRefused)
}

func assertNativeEndpointObservations(t *testing.T, clients map[string]*Client, want PrivateEndpointState) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nativePrivateObservationTimeout)
	defer cancel()
	for path, client := range clients {
		status, statusErr := privateReadyAttempt(ctx, client)
		_, groupResult, groupErr := client.privateGroups(ctx)
		for command, result := range map[string]CommandResult{"status": status, "group": groupResult} {
			t.Logf("%s %s state %d: started=%t exit=%d stdout=%d completion=%+v err=%v", path, command, want, result.Started, result.ExitCode, len(result.Stdout), classifyCommandCompletion(result.Err), result.Err)
		}
		if want == PrivateEndpointReady {
			if statusErr != nil || groupErr != nil {
				t.Fatalf("%s live endpoint failed: status=%v group=%v", path, statusErr, groupErr)
			}
		} else if !privateRefusalCompletion(status, statusErr) || !privateRefusalCompletion(groupResult, groupErr) {
			t.Fatalf("%s endpoint state %d was not a clean ordinary completion: status=%v group=%v", path, want, statusErr, groupErr)
		}
		_, observation, err := privateClientObserveSnapshot(ctx, client, false)
		if err != nil || observation.State() != want {
			t.Fatalf("%s snapshot observation = %+v err %v, want state %d", path, observation, err, want)
		}
		_, observation, err = privateClientObserveGroups(ctx, client)
		if err != nil || observation.State() != want {
			t.Fatalf("%s group observation = %+v err %v, want state %d", path, observation, err, want)
		}
	}
}

// nativeCommandRecorder keeps the argv and exit code of every completed
// supervisor command reported by a client's Observer. It never retains
// command output or environments.
type nativeCommandRecorder struct {
	mu        sync.Mutex
	completed []CommandEvent
}

func (recorder *nativeCommandRecorder) observe(event CommandEvent) {
	if event.Stage != "completed" {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.completed = append(recorder.completed, event)
}

// lastStatus returns the most recent completed status command after the
// client executable and its -c config arguments.
func (recorder *nativeCommandRecorder) lastStatus(t *testing.T) (argv []string, exitCode int) {
	t.Helper()
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	for index := len(recorder.completed) - 1; index >= 0; index-- {
		event := recorder.completed[index]
		if len(event.Argv) > 3 && event.Argv[1] == "-c" && event.Argv[3] == "status" {
			return slices.Clone(event.Argv[3:]), event.ExitCode
		}
	}
	t.Fatal("no completed status command was observed")
	return nil, 0
}

// labelQueryClient binds a new owned daemon and returns a client for one
// command path over it. Every row it adds stores only the explicit
// environment: a fixed PATH and locale plus the binding's path resolution
// coordinates, which NewClient pins.
func (fixture nativePrivateSupervisorFixture) labelQueryClient(t *testing.T, path string, recorder *nativeCommandRecorder) *Client {
	t.Helper()
	bound, err := fixture.bind(t)
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		PrivateGenerationGuard: allowPrivateGenerationForTest{},
		DaemonStartGuard:       allowDaemonStartForTest{},
		ObservationTimeout:     nativePrivateObservationTimeout,
		Environment:            []string{"PATH=/usr/bin:/bin", "LANG=C"},
		Observer:               recorder.observe,
	}
	if path == "owner" {
		options.ControlOwnerExecutable = os.Args[0]
	}
	client, err := NewClient(bound.Binding(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("native %s label query client close failed: %v", path, closeErr)
		}
	})
	allowed := []string{"PATH", "LANG", "HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR"}
	keys := make([]string, 0, len(client.options.Environment))
	for _, entry := range client.options.Environment {
		key, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(allowed, key) {
			t.Fatalf("native %s command environment carries an unexpected variable %q", path, key)
		}
		keys = append(keys, key)
	}
	if !slices.Contains(keys, "HOME") {
		t.Fatalf("native %s command environment lost its HOME coordinate: keys %q", path, keys)
	}
	t.Logf("%s command environment keys: %q", path, keys)
	return client
}

// addNativeStashedRow sets up foreign, never-enqueued rows for label queries.
// It preserves the selected command owner and CLI I/O without claiming
// Delegate admission or adding an admission role.
func addNativeStashedRow(ctx context.Context, t *testing.T, client *Client, label string) int64 {
	t.Helper()
	var id int64
	err := client.withPrivateSupervisorLease(ctx, func(leased context.Context) error {
		if verifyErr := client.verify(leased); verifyErr != nil {
			return verifyErr
		}
		return client.withPrivateQueueIdentityLease(leased, func(identityContext context.Context) error {
			var addErr error
			id, _, addErr = client.stagedAdd(identityContext, nil, "add", "--escape", "--stashed", "--label", label, "--print-task-id", "--", nativeStashedRowCommand)
			return addErr
		})
	})
	if err != nil {
		t.Fatalf("native stashed add for %q failed: %v", label, err)
	}
	return id
}

// nativeStatusQuery runs the existing verified status snapshot and canonical
// stream parser with the given query.
func nativeStatusQuery(ctx context.Context, t *testing.T, client *Client, query ...string) QueueSnapshot {
	t.Helper()
	var snapshot QueueSnapshot
	err := client.withPrivateSupervisorLease(ctx, func(leased context.Context) error {
		if verifyErr := client.verify(leased); verifyErr != nil {
			return verifyErr
		}
		var queryErr error
		snapshot, queryErr = client.snapshotAfterVerify(leased, query...)
		return queryErr
	})
	if err != nil {
		t.Fatalf("native status query %q failed: %v", query, err)
	}
	return snapshot
}

func nativeJobIDs(snapshot QueueSnapshot) []int64 {
	ids := make([]int64, 0, len(snapshot.Jobs))
	for _, job := range snapshot.Jobs {
		ids = append(ids, job.ID)
	}
	slices.Sort(ids)
	return ids
}

// assertNativeLabelRows requires the exact label query to return exactly the
// wanted stashed rows, each carrying the queried label, from a status command
// that exited 0.
func assertNativeLabelRows(t *testing.T, recorder *nativeCommandRecorder, step string, snapshot QueueSnapshot, label string, want []int64) {
	t.Helper()
	argv, exitCode := recorder.lastStatus(t)
	got := nativeJobIDs(snapshot)
	t.Logf("%s: argv=%q exit=%d parsed_tasks=%d ids=%v", step, argv, exitCode, len(snapshot.Jobs), got)
	if exitCode != 0 || !slices.Equal(argv, []string{"status", "--json", "label=" + label}) {
		t.Fatalf("%s: status command argv %q exited %d", step, argv, exitCode)
	}
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		t.Fatalf("%s: label query returned IDs %v, want %v", step, got, want)
	}
	for _, job := range snapshot.Jobs {
		if job.Label == nil || *job.Label != label || job.State != StateStashed {
			t.Fatalf("%s: row %d is not a stashed row with the queried label (state %q)", step, job.ID, job.State)
		}
	}
}

// TestPrivateSupervisorNativeLabelQuery records what the real Pueue exact
// label query returns through the existing verified snapshot and canonical
// parser, on the direct path and through the one-shot control owner. Each path
// owns a new daemon whose rows all stay stashed.
func TestPrivateSupervisorNativeLabelQuery(t *testing.T) {
	for _, path := range []string{"direct", "owner"} {
		t.Run(path, func(t *testing.T) {
			fixture := newNativePrivateSupervisorFixture(t)
			recorder := &nativeCommandRecorder{}
			client := fixture.labelQueryClient(t, path, recorder)
			binding := client.Binding()
			t.Logf("%s binding: version=%s executable_identity=%s client=%s daemon=%s", path, binding.ObservedVersion, binding.ExecutableIdentity, binding.ClientExecutable, binding.DaemonExecutable)
			ctx, cancel := context.WithTimeout(context.Background(), nativeLabelQueryTimeout)
			defer cancel()

			expected := Identity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("1", 32)}.Label()
			unrelated := Identity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("2", 32)}.Label()
			assertNativeLabelRows(t, recorder, path+" empty queue", nativeStatusQuery(ctx, t, client, "label="+expected), expected, nil)

			known := addNativeStashedRow(ctx, t, client, expected)
			other := addNativeStashedRow(ctx, t, client, unrelated)
			assertNativeLabelRows(t, recorder, path+" one exact row", nativeStatusQuery(ctx, t, client, "label="+expected), expected, []int64{known})
			assertNativeLabelRows(t, recorder, path+" unrelated row", nativeStatusQuery(ctx, t, client, "label="+unrelated), unrelated, []int64{other})

			duplicate := addNativeStashedRow(ctx, t, client, expected)
			assertNativeLabelRows(t, recorder, path+" two exact rows", nativeStatusQuery(ctx, t, client, "label="+expected), expected, []int64{known, duplicate})

			assertForeignRowAtSavedID(ctx, t, client, recorder, path, []int64{known, other, duplicate})
		})
	}
}

// assertForeignRowAtSavedID adds a foreign row and treats its controlled ID
// as a saved numeric ID whose expected label has no row. The label query is
// empty, while the unfiltered queue shows that ID occupied: an empty filtered
// response never proves the saved ID is free.
func assertForeignRowAtSavedID(ctx context.Context, t *testing.T, client *Client, recorder *nativeCommandRecorder, path string, existing []int64) {
	t.Helper()
	foreign := Identity{RootID: strings.Repeat("f", 32), TaskID: strings.Repeat("4", 32)}.Label()
	savedID := addNativeStashedRow(ctx, t, client, foreign)
	saved := Identity{RootID: strings.Repeat("b", 32), TaskID: strings.Repeat("3", 32), NumericTaskID: &savedID}

	filtered := nativeStatusQuery(ctx, t, client, "label="+saved.Label())
	assertNativeLabelRows(t, recorder, path+" saved ID with foreign row", filtered, saved.Label(), nil)

	unfiltered := nativeStatusQuery(ctx, t, client)
	argv, exitCode := recorder.lastStatus(t)
	want := slices.Sorted(slices.Values(append(slices.Clone(existing), savedID)))
	got := nativeJobIDs(unfiltered)
	t.Logf("%s unfiltered counterpoint: argv=%q exit=%d parsed_tasks=%d ids=%v saved_id=%d", path, argv, exitCode, len(unfiltered.Jobs), got, savedID)
	if exitCode != 0 || !slices.Equal(argv, []string{"status", "--json"}) || !slices.Equal(got, want) {
		t.Fatalf("%s unfiltered queue = IDs %v from %q exit %d, want %v", path, got, argv, exitCode, want)
	}
	index := slices.IndexFunc(unfiltered.Jobs, func(job Job) bool { return job.ID == savedID })
	if index < 0 || unfiltered.Jobs[index].Label == nil || *unfiltered.Jobs[index].Label != foreign || unfiltered.Jobs[index].State != StateStashed {
		t.Fatalf("%s saved ID %d is not occupied by the stashed foreign row", path, savedID)
	}
	assertNativeCleanupKeepsRows(ctx, t, client, path, want)
}

// assertNativeCleanupKeepsRows runs the live maintenance path against the
// native queue. Its duplicate and foreign rows are all stashed, so the real
// terminal-only test policy grants no removal authority.
func assertNativeCleanupKeepsRows(ctx context.Context, t *testing.T, client *Client, path string, want []int64) {
	t.Helper()
	worker, err := client.WithObservationTimeout(nativePrivateObservationTimeout)
	if err != nil {
		t.Fatal(err)
	}
	worker.options.SuccessfulCleanupAuthorizer = allowSuccessfulCleanupForTest
	var removals int
	worker.options.Observer = func(event CommandEvent) {
		if event.Stage == "entry" && slices.Contains(event.Argv, "remove") {
			removals++
		}
	}
	startPrivateMaintenancePassContext(t, ctx, worker, func() {}).requireCompleted(t)
	if removals != 0 {
		t.Fatalf("%s cleanup attempted %d removals of stashed rows", path, removals)
	}
	if got := nativeJobIDs(nativeStatusQuery(ctx, t, client)); !slices.Equal(got, want) {
		t.Fatalf("%s cleanup changed the queue: IDs %v, want %v", path, got, want)
	}
}
