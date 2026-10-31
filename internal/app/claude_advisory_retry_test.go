package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/provider/claude"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	testsupportcore "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// baselineClaudePrintReferences are the print references the baseline
// release (0b5a725) admitted fresh tasks under, pinned by their digests. They
// are the frozen advisory revisions now.
var baselineClaudePrintReferences = map[string]struct{ version, sha256 string }{
	config.ModeReadOnly:       {version: "native-permissions-v5", sha256: "a4f38219dcb1a105cf20c75f54341effde2310b6a15f61665a3b597eaa205fa8"},
	config.ModeWorkspaceWrite: {version: "native-permissions-v6", sha256: "7b64659e74a5b11e4717b5f942efa577544c2dd0730479e0f47a05ff0402bd8b"},
}

// TestSameIDDispatchRecoversUnsubmittedBaselineClaudePrintTask retries, with
// the same dispatch ID, a print task that the baseline release admitted but
// never submitted, after the CLI was updated without changing its
// capabilities. The retry must pass the recorded-profile gate and proceed to
// submission without launching the provider or rewriting the record. The
// hermetic supervisor is inert, so submission itself is not completed here.
func TestSameIDDispatchRecoversUnsubmittedBaselineClaudePrintTask(t *testing.T) {
	for _, mode := range []string{config.ModeReadOnly, config.ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) { testBaselineClaudePrintRetry(t, mode) })
	}
}

// baselineClaudePrintTask is one unsubmitted task recorded as the baseline
// release admitted it, in a state root whose claude CLI logs every call.
type baselineClaudePrintTask struct {
	store     *taskdir.Store
	td        *taskdir.TaskDir
	request   task.TaskRecord
	brief     []byte
	runner    string
	nativeLog string
}

func newBaselineClaudePrintTask(t *testing.T, mode string) *baselineClaudePrintTask {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Provider writable roots include /tmp, and placement rejects a state root
	// inside them, so the state root lives under the user's home.
	base, err := filepath.EvalSymlinks(mustCCRContinuationTempDir(t, home, ".delegate-advisory-retry-"))
	if err != nil {
		t.Fatal(err)
	}
	cliDir, workspace := testsupportcore.CanonicalTempDir(t), filepath.Join(base, "workspace")
	if err = os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &baselineClaudePrintTask{brief: []byte("baseline brief"), runner: filepath.Join(base, "delegate-run"), nativeLog: filepath.Join(cliDir, "native.log")}
	writeRunnerFixture(t, filepath.Join(cliDir, "claude"), fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> '%s'\nprintf '%%s\\n' 'Usage: --print'\n", f.nativeLog))
	writeRunnerFixture(t, f.runner, "custom runner")
	t.Setenv("HOME", cliDir)
	t.Setenv("PATH", cliDir)
	if f.store, err = taskdir.InitStoreWithPredicates(filepath.Join(base, "state"), NativeCatalog().Registry()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := f.store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if f.request, err = admission.BuildRequest(f.store.RootID, strings.Repeat("a", 32), claude.Provider, workspace, task.TaskConfig{Permission: mode, Budget: "1m0s"}, f.brief, nil); err != nil {
		t.Fatal(err)
	}
	meta := baselineClaudePrintMeta(t, f.request, mode)
	meta.RunnerExecutable, meta.RunnerOwnership = f.runner, task.RunnerOwnershipCustom
	if f.td, err = f.store.CreateTask(f.request.TaskID, &f.request, f.brief, &meta); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := f.td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return f
}

func testBaselineClaudePrintRetry(t *testing.T, mode string) {
	t.Helper()
	f := newBaselineClaudePrintTask(t, mode)
	before := readTaskFile(t, f.td.Dir, "meta.json")
	result := dispatch(Arguments{Root: f.store.Root, TaskID: f.request.TaskID, Provider: f.request.Provider, Cwd: f.request.CanonicalCwd, Config: f.request.RequestedConfig, BriefBytes: f.brief, Runner: f.runner},
		Dependencies{ResourceChecker: fixtureResources, Environment: os.Environ()})
	// The recorded profile gate refuses an unmatched capability digest with an
	// identity mismatch. Passing it, the retry reaches submission, where the
	// inert recorded supervisor refuses its binding.
	if errors.Is(result.err, task.ErrIdentityMismatch) || !errors.Is(result.err, pueue.ErrBinding) {
		t.Fatalf("same-ID retry did not pass the recorded profile gate: code=%d err=%v", result.code, result.err)
	}
	if !bytes.Equal(readTaskFile(t, f.td.Dir, "meta.json"), before) {
		t.Fatal("retry rewrote the admitted record")
	}
	if data, readErr := os.ReadFile(f.nativeLog); !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("retry invoked the provider CLI: %q %v", data, readErr)
	}
	for _, name := range []string{"provider.start", "provider.started.json", "outcome.json"} {
		if _, statErr := os.Stat(filepath.Join(f.td.Dir, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("retry created %s: %v", name, statErr)
		}
	}
}

// baselineClaudePrintMeta records the task as the baseline release admitted
// it: its then-current print reference and the pending capability digest of
// its --print-only probe, built by the shared provider constructor. The
// launch scope and native policy do not depend on the reference.
func baselineClaudePrintMeta(t *testing.T, request task.TaskRecord, mode string) task.MetaRecord {
	t.Helper()
	candidate, err := NativeCatalog().Candidate(request, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	profile, err := admission.ProfileForAdmission(candidate, request, true)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := commonprovider.LocateCLIPath(profile.Plan.Executable)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := commonprovider.NewRuntimeInspectionDefinition(cli, request.CanonicalCwd, profile.Plan.Environment, commonprovider.RuntimeCapability{RequiredFlags: []string{"--print"}})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := commonprovider.RuntimeCapabilityDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	// The recorded supervisor is inert: retry submission binds it only after
	// the recorded profile has been reconstructed and matched.
	supervisor := task.SupervisorRef{
		ClientExecutable: "/tmp/app-pueue", ClientSHA256: task.ComputeSHA256([]byte("app-test")), ResolvedConfigSHA256: task.ComputeSHA256([]byte("app-test")),
		ConfigPath: "/tmp/app-pueue.yml", ConfigDigest: task.ComputeSHA256([]byte("app-test")), Endpoint: "unix:/tmp/app-pueue.sock", ObservedVersion: pueue.FixtureVersion,
	}
	meta := tasklifecycle.NewMeta(request, profile, supervisor, "", "", "app-test")
	baseline := baselineClaudePrintReferences[mode]
	meta.Predicate = registeredReadOnlyOrWriteReference(t, claude.Provider, mode, baseline.version)
	if meta.Predicate.SHA256 != baseline.sha256 {
		t.Fatalf("%s %s digest=%s, want the baseline admission reference", mode, baseline.version, meta.Predicate.SHA256)
	}
	meta.RuntimePreflightSHA256 = capability
	return meta
}

// registeredReadOnlyOrWriteReference selects the registered reference with
// this exact provider, mode, and version from the production catalog.
func registeredReadOnlyOrWriteReference(t *testing.T, provider, mode, version string) task.PredicateRef {
	t.Helper()
	registration, err := NativeCatalog().Lookup(provider)
	if err != nil {
		t.Fatal(err)
	}
	for _, interpreter := range registration.Interpreters {
		if reference := interpreter.Reference(); reference.Mode == mode && reference.Version == version {
			return reference
		}
	}
	t.Fatalf("%s %s predicate %s is not registered", provider, mode, version)
	return task.PredicateRef{}
}
