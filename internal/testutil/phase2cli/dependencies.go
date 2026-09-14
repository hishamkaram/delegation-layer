// Package phase2cli composes the acceptance-only command wrappers. It is not
// imported by the production command binaries.
package phase2cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/phase2fixture"
)

const (
	publisherVersion       = "fixture-v2"
	invalidSupervisorPath  = "/__phase2_fixture_invalid__/supervisor"
	delegateRunnerName     = "delegate-run"
	fixtureObservedVersion = "fixture-v2"
)

var acceptanceRegistry = buildAcceptanceRegistry()

// Main owns one wrapper process's immutable event recorder and its compiled
// application dependencies.
type Main struct {
	executable string
	configPath string
	deps       app.Dependencies
	recorder   *eventRecorder
}

// NewMain resolves the canonical wrapper executable and its sibling config.
// Config errors remain lazy so collect/status/logs can recover saved evidence
// without requiring current fixture files.
func NewMain(executable string) *Main {
	canonical, err := canonicalExecutable(executable)
	if err != nil {
		canonical = invalidSupervisorPath
	}
	configPath := filepath.Join(filepath.Dir(canonical), fixtureConfigName)
	m := &Main{executable: canonical, configPath: configPath}
	m.deps = app.Dependencies{
		PrepareProfile:              m.prepareProfile,
		PredicateRegistry:           func() predicate.Registry { return acceptanceRegistry },
		InitialSupervisorExecutable: invalidSupervisorPath,
		RunnerExecutable:            filepath.Join(filepath.Dir(canonical), delegateRunnerName),
		PublisherVersion:            publisherVersion,
	}
	if loaded, loadErr := loadHarnessConfig(configPath); loadErr == nil {
		m.deps.InitialSupervisorExecutable = loaded.Config.SupervisorExecutable
		m.recorder = newEventRecorder(loaded.Config)
		m.deps.SupervisorOptions = pueue.Options{
			Environment: cloneStrings(loaded.Config.Environment),
			Observer:    m.recorder.supervisorEvent,
		}
		m.deps.ExecutionHooks = m.recorder.executionHooks(loaded.Config.Hooks)
	}
	return m
}

// NewDependencies returns acceptance dependencies for the current executable.
// The profile constructor still reads the sibling config when authority is
// requested, so this function does not make fixture files part of recovery.
func NewDependencies() app.Dependencies { return NewMain("").Dependencies() }

// NewDependenciesForExecutable is useful to scoped tests and copied wrappers.
func NewDependenciesForExecutable(executable string) app.Dependencies {
	return NewMain(executable).Dependencies()
}

// Dependencies returns the immutable composition passed to app.Run or
// app.RunRunner.
func (m *Main) Dependencies() app.Dependencies { return m.deps }

// ConfigPath returns the canonical sibling config path used by this wrapper.
func (m *Main) ConfigPath() string { return m.configPath }

// RunDelegate executes app.Run and closes this wrapper's process receipt.
func (m *Main) RunDelegate(args []string, stdout, stderr io.Writer) int {
	return m.run(func() int { return app.Run(args, stdout, stderr, m.deps) }, stderr)
}

// RunRunner executes app.RunRunner and closes this wrapper's process receipt.
func (m *Main) RunRunner(args []string, stdout, stderr io.Writer) int {
	return m.run(func() int { return app.RunRunner(args, stdout, stderr, m.deps) }, stderr)
}

// Run is the convenience entry point for a copied delegate wrapper.
func Run(args []string, stdout, stderr io.Writer) int {
	return NewMain("").RunDelegate(args, stdout, stderr)
}

// RunRunner is the convenience entry point for a copied delegate-run wrapper.
func RunRunner(args []string, stdout, stderr io.Writer) int {
	return NewMain("").RunRunner(args, stdout, stderr)
}

func (m *Main) run(call func() int, stderr io.Writer) int {
	if m.recorder == nil {
		return call()
	}
	if err := m.recorder.begin(os.Args); err != nil {
		writeWrapperError(stderr, err)
		return 1
	}
	code := call()
	if err := m.recorder.finish(code, ""); err != nil {
		writeWrapperError(stderr, err)
		return 1
	}
	return code
}

func writeWrapperError(stderr io.Writer, err error) {
	if stderr != nil {
		if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
			return
		}
	}
}

func (m *Main) prepareProfile(request task.TaskRecord) (app.PreparedProfile, error) {
	if request.Provider != phase2fixture.Provider || request.Mode != phase2fixture.Mode {
		return app.PreparedProfile{}, fmt.Errorf("%w: fixture profile requires %s/%s", task.ErrIdentityMismatch, phase2fixture.Provider, phase2fixture.Mode)
	}
	if request.RequestedConfig.Model != "" || request.RequestedConfig.Effort != "" {
		return app.PreparedProfile{}, errors.New("fixture profile does not accept model or effort")
	}
	loaded, err := loadHarnessConfig(m.configPath)
	if err != nil {
		return app.PreparedProfile{}, err
	}
	providerExecutable, err := canonicalFileReference(loaded.Config.ProviderExecutable)
	if err != nil {
		return app.PreparedProfile{}, err
	}
	providerConfig, err := canonicalFileReference(loaded.Config.ProviderConfig)
	if err != nil {
		return app.PreparedProfile{}, err
	}
	providerDigest, err := hashRegular(providerExecutable)
	if err != nil {
		return app.PreparedProfile{}, err
	}
	if providerDigest != loaded.Config.ProviderSHA256 {
		return app.PreparedProfile{}, errors.New("phase2 provider executable digest mismatch")
	}
	configData, err := readRegular(providerConfig, maxConfigBytes)
	if err != nil {
		return app.PreparedProfile{}, err
	}
	if task.ComputeSHA256(configData) != loaded.Config.ProviderConfigSHA256 {
		return app.PreparedProfile{}, errors.New("phase2 provider config digest mismatch")
	}
	providerConfigValue, err := phase2fixture.ParseProviderConfig(configData)
	if err != nil {
		return app.PreparedProfile{}, err
	}
	if providerConfigValue.TaskID != request.TaskID {
		return app.PreparedProfile{}, task.ErrIdentityMismatch
	}
	predicateRef := phase2fixture.Predicate().Reference()
	plan := execution.Plan{
		Executable:  providerExecutable,
		Arguments:   append([]string{providerConfig}, providerConfigValue.Argv...),
		Directory:   request.CanonicalCwd,
		Environment: cloneStrings(loaded.Config.Environment),
		Predicate:   predicateRef,
	}
	effective := task.EffectiveConfig{Containment: "finite-fixture-process", Approval: "fixture-no-tools", Digest: task.ComputeSHA256(loaded.Raw)}
	return app.PreparedProfile{
		Plan:            plan,
		ObservedVersion: fixtureObservedVersion,
		Effective:       effective,
		Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return phase2fixture.NewIdentityObserver(request.TaskID, expected, record)
		},
	}, nil
}

func canonicalFileReference(path string) (string, error) {
	if err := validateCleanAbsolute(path); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if filepath.Clean(resolved) != path {
		return "", fmt.Errorf("phase2 path must be canonical: %w", errInvalidHarnessReference)
	}
	return path, nil
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func buildAcceptanceRegistry() predicate.Registry {
	phase1, err := predicate.Default().Resolve(task.FixturePredicateRef())
	if err != nil {
		panic(err)
	}
	registry, err := predicate.New(phase1, phase2fixture.Predicate())
	if err != nil {
		panic(err)
	}
	return registry
}

var _ app.PrepareProfile = (*Main)(nil).prepareProfile
