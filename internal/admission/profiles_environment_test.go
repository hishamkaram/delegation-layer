package admission

import (
	"errors"
	"slices"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type recordingProfileSource struct {
	received [][]string
}

func (source *recordingProfileSource) Current(_ task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	return source.record(environment)
}

func (source *recordingProfileSource) ForTask(_ task.TaskRecord, _ *task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, bool, error) {
	candidate, err := source.record(environment)
	return candidate, false, err
}

func (source *recordingProfileSource) record(environment []string) (commonprovider.ProfileCandidate, error) {
	source.received = append(source.received, slices.Clone(environment))
	if len(environment) > 0 {
		environment[0] = "PATH=/factory-mutation"
	}
	return commonprovider.ProfileCandidate{}, commonprovider.ErrProfileUnavailable
}

func TestProfilesOwnAnImmutableLaunchEnvironment(t *testing.T) {
	caller := []string{"PATH=/caller-bin", "HOME=/caller-home"}
	source := &recordingProfileSource{}
	profiles := NewProfiles(source, nil, caller)
	caller[0] = "PATH=/changed-after-binding"
	read := profiles.Environment()
	read[0] = "PATH=/changed-by-reader"
	if _, err := profiles.Prepare("", task.TaskRecord{}); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("prepare = %v", err)
	}
	if _, err := profiles.ForWorker("", task.TaskRecord{}, nil); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("worker = %v", err)
	}
	want := []string{"PATH=/caller-bin", "HOME=/caller-home"}
	for index, received := range source.received {
		if !slices.Equal(received, want) {
			t.Fatalf("factory call %d received %q, want %q", index, received, want)
		}
	}
	if len(source.received) != 2 || !slices.Equal(profiles.Environment(), want) {
		t.Fatalf("profiles scope changed to %q after %d calls", profiles.Environment(), len(source.received))
	}
}

func TestProfilesForRecordSelectsANewScope(t *testing.T) {
	base := []string{"PATH=/caller-bin"}
	tests := []struct {
		name     string
		values   []string
		recorded bool
		want     []string
	}{
		{name: "legacy unrecorded", want: base},
		{name: "unrecorded empty", values: []string{}, want: base},
		{name: "recorded empty", values: []string{}, recorded: true, want: []string{}},
		{name: "recorded nil", recorded: true, want: []string{}},
		{name: "recorded values", values: []string{"PATH=/recorded-bin"}, recorded: true, want: []string{"PATH=/recorded-bin"}},
		{name: "legacy values", values: []string{"PATH=/recorded-bin"}, want: []string{"PATH=/recorded-bin"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &recordingProfileSource{}
			profiles := NewProfiles(source, nil, base)
			selected, err := profiles.ForRecord(test.values, test.recorded)
			if err != nil {
				t.Fatal(err)
			}
			if got := selected.Environment(); got == nil || !slices.Equal(got, test.want) {
				t.Fatalf("selected scope = %#v, want %#v", got, test.want)
			}
			if !slices.Equal(profiles.Environment(), base) {
				t.Fatalf("selection changed the caller scope to %q", profiles.Environment())
			}
			if _, err = selected.Prepare("", task.TaskRecord{}); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
				t.Fatalf("prepare = %v", err)
			}
			if len(source.received) != 1 || !slices.Equal(source.received[0], test.want) {
				t.Fatalf("selected factory received %q", source.received)
			}
		})
	}
}

func TestProfilesForRecordRejectsInvalidSavedScope(t *testing.T) {
	profiles := NewProfiles(&recordingProfileSource{}, nil, []string{"PATH=/caller-bin"})
	for _, values := range [][]string{{"PATH=/a", "PATH=/b"}, {"MALFORMED"}} {
		if _, err := profiles.ForRecord(values, true); err == nil {
			t.Fatalf("invalid saved scope %q was selected", values)
		}
	}
}

func TestProfilesRejectUncapturedEnvironmentWithoutAmbientFallback(t *testing.T) {
	source := &recordingProfileSource{}
	profiles := NewProfiles(source, nil, nil)
	if _, err := profiles.Prepare("", task.TaskRecord{}); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("prepare = %v", err)
	}
	if _, err := profiles.ForWorker("", task.TaskRecord{}, nil); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("worker = %v", err)
	}
	if _, err := profiles.ForRecord([]string{"PATH=/recorded-bin"}, true); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("selection = %v", err)
	}
	if _, err := (Profiles{}).Prepare("", task.TaskRecord{}); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("zero profiles = %v", err)
	}
	if len(source.received) != 0 {
		t.Fatalf("uncaptured scope reached the factory: %q", source.received)
	}
	explicit := NewProfiles(source, nil, []string{})
	if _, err := explicit.Prepare("", task.TaskRecord{}); !errors.Is(err, commonprovider.ErrProfileUnavailable) {
		t.Fatalf("explicit empty scope = %v", err)
	}
	if len(source.received) != 1 || source.received[0] == nil || len(source.received[0]) != 0 {
		t.Fatalf("explicit empty scope reached the factory as %#v", source.received)
	}
}
