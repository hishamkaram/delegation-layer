package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// protocolFactoryScript reads the one frame a machine protocol writes and
// answers only when it arrives, so finite brief input cannot satisfy it.
const protocolFactoryScript = `IFS= read -r frame && [ "$frame" = start ] && echo answer`

var errProtocolFactoryFault = errors.New("protocol factory fault")

// appMachineProtocol is a machine Protocol. It records the provider identity
// through the supplied callback when the answer arrives.
type appMachineProtocol struct {
	identity  task.SessionIdentity
	record    func(task.SessionIdentity) error
	recordErr error
	brief     []byte
	done      bool
}

func (p *appMachineProtocol) Start(brief []byte) ([][]byte, error) {
	p.brief = append([]byte(nil), brief...)
	return [][]byte{[]byte("start")}, nil
}

func (p *appMachineProtocol) Receive([]byte) (execution.InteractionUpdate, error) {
	p.recordErr = p.record(p.identity)
	p.done = true
	return execution.InteractionUpdate{Done: true}, nil
}

func (p *appMachineProtocol) Complete() error {
	if !p.done {
		return errors.New("fixture protocol did not observe the answer")
	}
	return nil
}

type protocolFactoryProbe struct {
	calls    int
	expected task.SessionExpectation
	protocol *appMachineProtocol
}

// factory returns the production-shaped Protocol factory under test.
func (p *protocolFactoryProbe) factory(identity task.SessionIdentity) func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.Protocol, error) {
	return func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.Protocol, error) {
		p.calls++
		p.expected = expected
		p.protocol = &appMachineProtocol{identity: identity, record: record}
		return p.protocol, nil
	}
}

// protocolFactoryDependencies lets the fresh preflight rebuild the same
// admitted plan; it cannot replace the admitted factory.
func protocolFactoryDependencies(profile PreparedProfile, launches *launchCounter) Dependencies {
	return Dependencies{
		ResourceChecker: fixtureResources,
		Environment:     os.Environ(),
		PrepareProfile:  func(task.TaskRecord) (PreparedProfile, error) { return profile, nil },
		ExecutionHooks:  launches.hooks(),
	}
}

type launchCounter struct{ starts int }

func (c *launchCounter) hooks() execution.Hooks {
	return execution.Hooks{Start: func(cmd *exec.Cmd) error {
		c.starts++
		return cmd.Start()
	}}
}

func protocolFactoryTask(t *testing.T) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord, PreparedProfile) {
	t.Helper()
	store, td, req := newAppTestTaskWithPrior(t, false, testPriorSession())
	if err := td.ClaimSession(req.Provider, req.PriorSession.ConversationID); err != nil {
		t.Fatal(err)
	}
	rewriteAppProviderExecutable(t, td, "/bin/sh")
	return store, td, req, appTestExecutionProfile(req, "/bin/sh", []string{"-c", protocolFactoryScript})
}

func assertNoStartEvidence(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	inspection, err := td.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if inspection.StartedExists || inspection.SealExists || inspection.OutcomeExists {
		t.Fatalf("refused factory left launch evidence: %+v", inspection)
	}
	if _, err = os.Stat(filepath.Join(td.Dir, "interaction")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("machine selection created an interaction journal: %v", err)
	}
}

func TestRunProviderComposesMachineProtocolFactoryOnTheSharedRunner(t *testing.T) {
	store, td, req, profile := protocolFactoryTask(t)
	defer closeAppTestTask(t, store, td)
	probe := &protocolFactoryProbe{}
	identity := task.SessionIdentity{Provider: req.Provider, ConversationID: req.PriorSession.ConversationID}
	profile.Protocol = probe.factory(identity)
	launches := &launchCounter{}
	result := runProvider(newResponse("dispatch"), store, td, req, profile, nil, nil, protocolFactoryDependencies(profile, launches))
	if result.code != 0 || result.err != nil {
		t.Fatalf("machine protocol run failed: %+v", result)
	}
	if result.response.Publication != task.PublicationCommitted.String() || result.response.Outcome == nil {
		t.Fatalf("ordinary seal did not commit the answer: %+v", result.response)
	}
	if launches.starts != 1 || probe.calls != 1 {
		t.Fatalf("launches=%d factory calls=%d, want one each", launches.starts, probe.calls)
	}
	wantExpected := task.SessionExpectation{Required: true, ID: req.PriorSession.ConversationID}
	if probe.expected != wantExpected {
		t.Fatalf("expected session=%+v, want %+v", probe.expected, wantExpected)
	}
	if string(probe.protocol.brief) != "brief" || probe.protocol.recordErr != nil {
		t.Fatalf("brief=%q record error=%v", probe.protocol.brief, probe.protocol.recordErr)
	}
	recorded, err := td.ReadProviderIdentity()
	if err != nil || recorded.Provider != identity.Provider || recorded.ConversationID != identity.ConversationID {
		t.Fatalf("canonical identity record=%+v err=%v", recorded, err)
	}
	if _, err = os.Stat(filepath.Join(td.Dir, "interaction")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("machine selection created an interaction journal: %v", err)
	}
}

func TestRunProviderRefusesUnusableProtocolFactoriesBeforeStart(t *testing.T) {
	var typedNil *appMachineProtocol
	cases := []struct {
		name    string
		profile func(*PreparedProfile)
		want    error
	}{
		{name: "protocol nil", profile: func(p *PreparedProfile) {
			p.Protocol = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.Protocol, error) {
				return nil, nil
			}
		}, want: task.ErrInteractionUnavailable},
		{name: "protocol typed nil", profile: func(p *PreparedProfile) {
			p.Protocol = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.Protocol, error) {
				return typedNil, nil
			}
		}, want: task.ErrInteractionUnavailable},
		{name: "protocol error", profile: func(p *PreparedProfile) {
			p.Protocol = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.Protocol, error) {
				return nil, errProtocolFactoryFault
			}
		}, want: errProtocolFactoryFault},
		{name: "interaction nil", profile: func(p *PreparedProfile) {
			p.Interaction = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
				return nil, nil
			}
		}, want: task.ErrInteractionUnavailable},
		{name: "interaction typed nil", profile: func(p *PreparedProfile) {
			p.Interaction = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
				return typedNilInteraction, nil
			}
		}, want: task.ErrInteractionUnavailable},
		{name: "interaction error", profile: func(p *PreparedProfile) {
			p.Interaction = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
				return nil, errProtocolFactoryFault
			}
		}, want: errProtocolFactoryFault},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store, td, req, profile := protocolFactoryTask(t)
			defer closeAppTestTask(t, store, td)
			testCase.profile(&profile)
			launches := &launchCounter{}
			result := runProvider(newResponse("dispatch"), store, td, req, profile, nil, nil, protocolFactoryDependencies(profile, launches))
			if result.code != 1 || !errors.Is(result.err, testCase.want) {
				t.Fatalf("unusable factory result=%+v, want %v", result, testCase.want)
			}
			if launches.starts != 0 {
				t.Fatalf("provider started %d times after factory refusal", launches.starts)
			}
			assertNoStartEvidence(t, td)
			// The start intent is spent; reaching it proves the runner lock was released.
			if _, err := td.PrepareStart(0); !errors.Is(err, task.ErrAlreadyStarted) {
				t.Fatalf("refusal left the start permit held: %v", err)
			}
		})
	}
}

// typedNilInteraction is a nil pointer inside a non-nil InteractionProtocol.
var typedNilInteraction execution.InteractionProtocol = (*appInteractionFixture)(nil)

type appInteractionFixture struct{ appMachineProtocol }

func (*appInteractionFixture) Encode(task.InteractionMessage) ([]byte, error) { return nil, nil }

func TestRunRunnerRefusesInvalidProtocolSelectionBeforeAnyFactory(t *testing.T) {
	fixture := newRunnerRefreshFixture(t)
	defer fixture.close(t)
	recordRunnerSupervisorReceipt(t, fixture, 7)
	probe := &protocolFactoryProbe{}
	interactionCalls := 0
	launches := &launchCounter{}
	deps := Dependencies{
		ResourceChecker: fixtureResources,
		Environment:     os.Environ(),
		PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
			profile := fixture.profile()
			profile.Protocol = probe.factory(task.SessionIdentity{})
			profile.Interaction = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
				interactionCalls++
				return nil, nil
			}
			return profile, nil
		},
		SupervisorOptions: pueue.Options{Environment: fixture.environment},
		ExecutionHooks:    launches.hooks(),
	}
	result := runRunner(runnerArguments{Root: fixture.store.Root, TaskID: fixture.req.TaskID}, deps)
	if result.code == 0 || !errors.Is(result.err, commonprovider.ErrUnsupportedOption) {
		t.Fatalf("invalid selection was admitted: %+v", result)
	}
	if probe.calls != 0 || interactionCalls != 0 || launches.starts != 0 {
		t.Fatalf("factories=%d/%d starts=%d after invalid selection", probe.calls, interactionCalls, launches.starts)
	}
	assertNoStartEvidence(t, fixture.td)
	for _, name := range []string{"provider.start", "provider.started.json", "provider.exit"} {
		if _, err := os.Stat(filepath.Join(fixture.td.Dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid selection created %s: %v", name, err)
		}
	}
}

func TestSealedRecoveryAndCollectInvokeNoProtocolFactory(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)
	probe := &protocolFactoryProbe{}
	deps := Dependencies{Environment: os.Environ(), PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
		profile := appTestExecutionProfile(req, "/bin/sh", nil)
		profile.Protocol = probe.factory(task.SessionIdentity{})
		return profile, nil
	}}
	recovered := runRunner(runnerArguments{Root: store.Root, TaskID: req.TaskID}, deps)
	if recovered.code != 0 || recovered.err != nil || recovered.response.Publication != task.PublicationCommitted.String() {
		t.Fatalf("sealed recovery failed: %+v", recovered)
	}
	arguments := Arguments{Root: store.Root, TaskID: req.TaskID}
	if collected := collect(arguments, deps.storeDependencies()); collected.code != 0 || collected.err != nil {
		t.Fatalf("collect failed: %+v", collected)
	}
	if probe.calls != 0 {
		t.Fatalf("sealed recovery and collect invoked the protocol factory %d times", probe.calls)
	}
}

func TestRecheckInvokesNoProtocolFactory(t *testing.T) {
	target := task.PredicateRef{Adapter: "codex:exec", Mode: "read-only", Version: "app-recovery-v1", SHA256: task.ComputeSHA256([]byte("app-recovery-v1"))}
	registry, err := predicate.New(&appRecoveryInterpreter{ref: task.FixturePredicateRef()}, &appRecoveryInterpreter{ref: target, answer: "offline answer"})
	if err != nil {
		t.Fatal(err)
	}
	root, taskID := appRejectedRecoveryTask(t, registry)
	probe := &protocolFactoryProbe{}
	deps := Dependencies{
		Environment:       os.Environ(),
		PredicateRegistry: func() predicate.Registry { return registry },
		PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
			profile := appTestExecutionProfile(&task.TaskRecord{}, "/bin/sh", nil)
			profile.Protocol = probe.factory(task.SessionIdentity{})
			return profile, nil
		},
	}
	result := recheck(Arguments{Root: root, TaskID: taskID}, deps.storeDependencies())
	if result.code != 0 || result.err != nil || result.response.Recheck == nil || result.response.Recheck.Status != task.RecheckStatusRecovered {
		t.Fatalf("recheck failed: %+v", result)
	}
	if probe.calls != 0 {
		t.Fatalf("recheck invoked the protocol factory %d times", probe.calls)
	}
}
