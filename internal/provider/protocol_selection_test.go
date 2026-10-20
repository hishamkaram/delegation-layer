package provider

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// selectionProfile returns a finalized profile that panics if any configured
// factory is invoked: validation must refuse or accept without side effects.
func selectionProfile(t *testing.T, request task.TaskRecord, protocol, interaction, identity bool) PreparedProfile {
	t.Helper()
	profile := catalogTestProfile(executionPlanForCatalogTest(request))
	if protocol {
		profile.Protocol = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.Protocol, error) {
			t.Error("protocol factory was invoked during validation")
			return nil, nil
		}
	}
	if interaction {
		profile.Interaction = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
			t.Error("interaction factory was invoked during validation")
			return nil, nil
		}
	}
	if identity {
		profile.Identity = func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			t.Error("identity factory was invoked during validation")
			return nil, nil
		}
	}
	return profile
}

func admissionSelectionProfile(profile PreparedProfile) PreparedProfile {
	profile.ObservedVersion = ""
	profile.RuntimePreflightSHA256 = task.ComputeSHA256([]byte("capability"))
	profile.Plan.ExecutableSHA256 = task.ComputeSHA256([]byte("provider"))
	profile.Plan.Environment = []string{}
	return profile
}

func TestPreparedProfileSelectionFollowsCanonicalTable(t *testing.T) {
	cases := []struct {
		name                                    string
		interactive, protocol, interaction, ids bool
		want                                    error
	}{
		{name: "finite", want: nil},
		{name: "finite with identity", ids: true, want: nil},
		{name: "machine protocol", protocol: true, want: nil},
		{name: "machine protocol with identity", protocol: true, ids: true, want: ErrUnsupportedOption},
		{name: "interactive finite", interactive: true, want: ErrUnsupportedOption},
		{name: "interactive machine protocol", interactive: true, protocol: true, want: ErrUnsupportedOption},
		{name: "interactive user channel", interactive: true, interaction: true, want: nil},
		{name: "interactive user channel with historical identity", interactive: true, interaction: true, ids: true, want: nil},
		{name: "interactive both factories", interactive: true, protocol: true, interaction: true, want: ErrUnsupportedOption},
		{name: "non-interactive user channel", interaction: true, want: ErrUnsupportedOption},
		{name: "non-interactive both factories", protocol: true, interaction: true, want: ErrUnsupportedOption},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := artifactRequest()
			request.RequestedConfig.Interactive = testCase.interactive
			profile := selectionProfile(t, request, testCase.protocol, testCase.interaction, testCase.ids)
			admission := admissionSelectionProfile(profile)
			for name, validate := range map[string]func(task.TaskRecord) error{"Validate": profile.Validate, "ValidateAdmission": admission.ValidateAdmission} {
				err := validate(request)
				if testCase.want == nil && err != nil {
					t.Fatalf("%s refused a canonical selection: %v", name, err)
				}
				if testCase.want != nil && !errors.Is(err, testCase.want) {
					t.Fatalf("%s error=%v, want %v", name, err, testCase.want)
				}
			}
		})
	}
}

func TestPreparedProfileClonesKeepProtocolFactory(t *testing.T) {
	request := artifactRequest()
	profile := selectionProfile(t, request, true, false, false)
	if cloneCandidateProfile(profile).Protocol == nil {
		t.Fatal("candidate clone dropped the machine protocol factory")
	}
}

type openProtocolFixture struct{}

func (*openProtocolFixture) Start([]byte) ([][]byte, error) { return nil, nil }
func (*openProtocolFixture) Receive([]byte) (execution.InteractionUpdate, error) {
	return execution.InteractionUpdate{}, nil
}
func (*openProtocolFixture) Complete() error                                { return nil }
func (*openProtocolFixture) Encode(task.InteractionMessage) ([]byte, error) { return nil, nil }

// recordProtocolFixture and userRecordFixture opt into the current record
// trait by embedding the interfaces, so the test never calls their methods.
type recordProtocolFixture struct {
	execution.Protocol
	execution.CurrentRecordInteractionProtocol
}

func (*recordProtocolFixture) UseRecordCapture() bool { return true }

type userRecordFixture struct {
	execution.InteractionProtocol
	execution.CurrentRecordInteractionProtocol
}

func (*userRecordFixture) UseRecordCapture() bool { return true }

var errOpenProtocolFault = errors.New("factory fault")

func TestOpenProtocolPassesTheConfiguredValueThroughUnchanged(t *testing.T) {
	machine, user := &openProtocolFixture{}, &openProtocolFixture{}
	expected := task.SessionExpectation{Required: true, ID: "conversation"}
	var gotExpected task.SessionExpectation
	profile := PreparedProfile{Protocol: func(e task.SessionExpectation, _ func(task.SessionIdentity) error) (execution.Protocol, error) {
		gotExpected = e
		return machine, nil
	}}
	protocol, interaction, err := profile.OpenProtocol(expected, nil)
	if err != nil || protocol != execution.Protocol(machine) || interaction != nil || gotExpected != expected {
		t.Fatalf("machine factory value changed: protocol=%v interaction=%v expected=%+v err=%v", protocol, interaction, gotExpected, err)
	}
	profile = PreparedProfile{Interaction: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
		return user, nil
	}}
	protocol, interaction, err = profile.OpenProtocol(expected, nil)
	if err != nil || protocol != nil || interaction != execution.InteractionProtocol(user) {
		t.Fatalf("interaction factory value changed: protocol=%v interaction=%v err=%v", protocol, interaction, err)
	}
	protocol, interaction, err = PreparedProfile{}.OpenProtocol(expected, nil)
	if err != nil || protocol != nil || interaction != nil {
		t.Fatalf("finite profile selected a protocol: protocol=%v interaction=%v err=%v", protocol, interaction, err)
	}
}

func TestOpenProtocolKeepsOptionalRecordTraits(t *testing.T) {
	record := &recordProtocolFixture{}
	profile := PreparedProfile{Protocol: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.Protocol, error) {
		return record, nil
	}}
	protocol, _, err := profile.OpenProtocol(task.SessionExpectation{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, ok := protocol.(execution.CurrentRecordInteractionProtocol)
	if !ok || !current.UseRecordCapture() {
		t.Fatalf("record trait was hidden: %T", protocol)
	}
	profile = PreparedProfile{Interaction: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
		return &userRecordFixture{}, nil
	}}
	_, interaction, err := profile.OpenProtocol(task.SessionExpectation{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if current, ok = interaction.(execution.CurrentRecordInteractionProtocol); !ok || !current.UseRecordCapture() {
		t.Fatalf("interaction record trait was hidden: %T", interaction)
	}
}

func TestOpenProtocolRefusesUnusableFactoryResults(t *testing.T) {
	var typedNil *openProtocolFixture
	cases := []struct {
		name    string
		profile PreparedProfile
		want    error
	}{
		{name: "protocol nil", profile: PreparedProfile{Protocol: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.Protocol, error) {
			return nil, nil
		}}, want: task.ErrInteractionUnavailable},
		{name: "protocol typed nil", profile: PreparedProfile{Protocol: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.Protocol, error) {
			return typedNil, nil
		}}, want: task.ErrInteractionUnavailable},
		{name: "protocol error", profile: PreparedProfile{Protocol: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.Protocol, error) {
			return nil, errOpenProtocolFault
		}}, want: errOpenProtocolFault},
		{name: "interaction nil", profile: PreparedProfile{Interaction: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
			return nil, nil
		}}, want: task.ErrInteractionUnavailable},
		{name: "interaction typed nil", profile: PreparedProfile{Interaction: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
			return typedNil, nil
		}}, want: task.ErrInteractionUnavailable},
		{name: "interaction error", profile: PreparedProfile{Interaction: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
			return nil, errOpenProtocolFault
		}}, want: errOpenProtocolFault},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			protocol, interaction, err := testCase.profile.OpenProtocol(task.SessionExpectation{}, nil)
			if !errors.Is(err, testCase.want) || protocol != nil || interaction != nil {
				t.Fatalf("protocol=%v interaction=%v err=%v, want %v", protocol, interaction, err, testCase.want)
			}
		})
	}
}
