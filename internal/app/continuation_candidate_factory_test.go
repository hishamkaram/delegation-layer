package app

import (
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func candidateFactoryCatalog(t *testing.T, faults map[string]error) commonprovider.Catalog {
	t.Helper()
	const id = "fixture:preflight"
	hook := func(name string) commonprovider.PrepareExistingCandidate {
		return func(task.TaskRecord, task.MetaRecord, []string) (commonprovider.ProfileCandidate, error) {
			return commonprovider.ProfileCandidate{}, faults[name]
		}
	}
	description := preflightFixtureCatalog(t, func(task.TaskRecord, []string) (commonprovider.ProfileCandidate, error) {
		return commonprovider.ProfileCandidate{}, nil
	}).Descriptions()[0]
	description.SupportedOptions = append(description.SupportedOptions, commonprovider.OptionInteractive)
	description.Interaction = task.InteractionCapabilities{Clarification: true, SupportedModes: []string{config.ModeReadOnly}}
	catalog, err := commonprovider.NewCatalog(commonprovider.Registration{
		Description: description,
		Prepare: func(request task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
			return hook("fresh")(request, task.MetaRecord{}, environment)
		},
		PrepareExisting:  hook("existing"),
		PrepareSuccessor: hook("successor"),
		Interpreters:     []predicate.Interpreter{autoTestInterpreter{reference: preflightFixturePredicate(id)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestContinuationFactoryAdmitsSuccessorsThroughOwnPreparation(t *testing.T) {
	faults := map[string]error{"fresh": errors.New("fresh"), "existing": errors.New("existing"), "successor": errors.New("successor")}
	deps := Dependencies{ResourceChecker: fixtureResources, Catalog: candidateFactoryCatalog(t, faults)}
	request := task.TaskRecord{
		Provider: "fixture:preflight", Mode: config.ModeReadOnly, CanonicalCwd: t.TempDir(),
		PriorSession:    &task.PriorSession{Provider: "fixture:preflight", ConversationID: "known", PredecessorTaskID: "predecessor"},
		RequestedConfig: task.TaskConfig{Permission: config.ModeReadOnly},
	}
	interactive := request
	interactive.RequestedConfig.Interactive = true
	tests := []struct {
		name    string
		prepare func() error
		want    string
	}{
		{name: "normal continuation", prepare: func() error {
			_, err := continuationDependencies(deps, task.MetaRecord{}).PrepareCandidate(request, os.Environ())
			return err
		}, want: "successor"},
		{name: "interactive continuation", prepare: func() error {
			_, err := continuationDependencies(deps, task.MetaRecord{}).PrepareCandidate(interactive, os.Environ())
			return err
		}, want: "fresh"},
		{name: "immutable existing task", prepare: func() error {
			_, _, err := admissionCandidateSource{dependencies: deps}.ForTask(request, &task.MetaRecord{}, os.Environ())
			return err
		}, want: "existing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.prepare(); !errors.Is(err, faults[test.want]) {
				t.Fatalf("wrong preparation selected: %v, want %s", err, test.want)
			}
		})
	}
}
