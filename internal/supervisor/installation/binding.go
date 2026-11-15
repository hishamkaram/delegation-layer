package installation

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type BindingRelation string

const (
	BindingCurrent        BindingRelation = "current"
	BindingDifferent      BindingRelation = "different"
	BindingUnknown        BindingRelation = "unknown"
	BindingNotInitialized BindingRelation = "not_initialized"
)

type Availability string

const (
	AvailabilityPresent Availability = "present"
	AvailabilityMissing Availability = "missing"
	AvailabilityUnknown Availability = "unknown"
)

type BindingScope string

const (
	BindingRuntimeSelected BindingScope = "runtime_selected"
	BindingLoadedBundle    BindingScope = "loaded_bundle"
)

// SupervisorPairMetadata retains the resolver's selected role observations.
// It is local filesystem metadata, never permission to launch either role.
type SupervisorPairMetadata struct {
	Client         BundleRole
	Daemon         BundleRole
	SelectionError error
}

// BindingObservation compares current path metadata with recorded provenance.
// Even relation=current does not verify executing bytes or a live generation.
// Paths and raw errors deliberately remain outside this public control value.
type BindingObservation struct {
	Scope        BindingScope           `json:"scope"`
	Verification string                 `json:"verification"`
	Relation     BindingRelation        `json:"relation"`
	Client       RoleBindingObservation `json:"client"`
	Daemon       RoleBindingObservation `json:"daemon"`
}
type RoleBindingObservation struct {
	Relation             BindingRelation `json:"relation"`
	SelectedAvailability Availability    `json:"selected_availability"`
	RecordedAvailability Availability    `json:"recorded_availability"`
}

// ObserveSelectedSupervisorPair first uses runtime selection unchanged. When
// a selected role is unavailable, the same owner exposes the fixed candidate
// paths to preserve independent role availability without selecting a fallback.
func ObserveSelectedSupervisorPair(lookup ExecutableLookup) SupervisorPairMetadata {
	client, daemon, err := ResolvePrivateSupervisorExecutables(lookup)
	if err == nil {
		return SupervisorPairMetadata{Client: BundleRole{Path: client}, Daemon: BundleRole{Path: daemon}}
	}
	pair := unavailableSelectedPair(lookup, err)
	pair.SelectionError = err
	return pair
}

func unavailableSelectedPair(lookup ExecutableLookup, selectionErr error) SupervisorPairMetadata {
	if lookup.AcceptedBootstrapPair != (BootstrapPair{}) {
		return observePairCandidates(lookup.AcceptedBootstrapPair.Client, lookup.AcceptedBootstrapPair.Daemon)
	}
	directory, err := bundledSupervisorDirectory(lookup.InitialExecutable)
	if err != nil {
		return SupervisorPairMetadata{Client: BundleRole{Err: err}, Daemon: BundleRole{Err: err}}
	}
	selected := selectBundledSupervisorDirectory(directory, lookup.InitialExecutable != "")
	if lookup.InitialExecutable != "" || bundledSupervisorPairHasEntry(selected) {
		return observePairCandidates(filepath.Join(selected, "pueue"), filepath.Join(selected, "pueued"))
	}
	path, err := lookup.lookPath("pueue")
	if err != nil {
		return SupervisorPairMetadata{Client: BundleRole{Err: errors.Join(selectionErr, err)}, Daemon: BundleRole{Err: selectionErr}}
	}
	client, err := ResolveSupervisorExecutable(path, "pueue")
	if err != nil {
		return SupervisorPairMetadata{Client: BundleRole{Path: path, Err: err}, Daemon: BundleRole{Err: selectionErr}}
	}
	return observePairCandidates(client, filepath.Join(filepath.Dir(client), "pueued"))
}

func observePairCandidates(client, daemon string) SupervisorPairMetadata {
	return SupervisorPairMetadata{Client: observeBindingRole(client, "pueue"), Daemon: observeBindingRole(daemon, "pueued")}
}

func observeBindingRole(path, name string) BundleRole {
	if path == "" {
		return BundleRole{}
	}
	resolved, err := ResolveSupervisorExecutable(path, name)
	if err != nil {
		return BundleRole{Path: path, Err: err}
	}
	return BundleRole{Path: resolved}
}

// ObserveSupervisorBinding performs no execution or state mutation. A nil
// recorded reference is confirmed binding absence only when recordErr is nil.
func ObserveSupervisorBinding(scope BindingScope, selected SupervisorPairMetadata, recorded *task.SupervisorRef, recordErr error) BindingObservation {
	observation := BindingObservation{Scope: scope, Verification: "filesystem_metadata", Relation: BindingUnknown}
	observation.Client = compareBindingRole(selected.Client, "", "pueue")
	observation.Daemon = compareBindingRole(selected.Daemon, "", "pueued")
	if recordErr != nil {
		return observation
	}
	if recorded == nil {
		observation.Relation = BindingNotInitialized
		return observation
	}
	observation.Client = compareBindingRole(selected.Client, recorded.ClientExecutable, "pueue")
	observation.Daemon = compareBindingRole(selected.Daemon, recorded.DaemonExecutable, "pueued")
	if selected.SelectionError != nil || observation.Client.Relation == BindingUnknown || observation.Daemon.Relation == BindingUnknown {
		return observation
	}
	observation.Relation = BindingCurrent
	if observation.Client.Relation == BindingDifferent || observation.Daemon.Relation == BindingDifferent {
		observation.Relation = BindingDifferent
	}
	return observation
}

func compareBindingRole(selected BundleRole, recorded, name string) RoleBindingObservation {
	result := RoleBindingObservation{Relation: BindingUnknown, SelectedAvailability: roleAvailability(selected), RecordedAvailability: AvailabilityUnknown}
	if recorded == "" {
		return result
	}
	saved := observeBindingRole(recorded, name)
	result.RecordedAvailability = roleAvailability(saved)
	if result.SelectedAvailability != AvailabilityPresent || result.RecordedAvailability != AvailabilityPresent {
		return result
	}
	current, err := ResolveSupervisorExecutable(selected.Path, name)
	if err != nil {
		result.SelectedAvailability = roleAvailability(BundleRole{Path: selected.Path, Err: err})
		return result
	}
	result.Relation = BindingCurrent
	if current != saved.Path {
		result.Relation = BindingDifferent
	}
	return result
}

func roleAvailability(role BundleRole) Availability {
	if role.Path == "" {
		return AvailabilityUnknown
	}
	if role.Err == nil {
		return AvailabilityPresent
	}
	absent := errors.Is(role.Err, os.ErrNotExist) && task.AllErrorCauses(role.Err, func(cause error) bool {
		return errors.Is(cause, os.ErrNotExist) || errors.Is(cause, pueue.ErrConfiguration)
	})
	if absent {
		return AvailabilityMissing
	}
	return AvailabilityUnknown
}

// HandoffAdvised is advisory metadata only. Missing published files do not
// establish daemon death; an explicit owner handoff still validates live state.
func (o BindingObservation) HandoffAdvised() bool {
	if o.Client.SelectedAvailability != AvailabilityPresent || o.Daemon.SelectedAvailability != AvailabilityPresent {
		return false
	}
	return o.Relation == BindingDifferent || o.Client.RecordedAvailability == AvailabilityMissing || o.Daemon.RecordedAvailability == AvailabilityMissing
}
