package pueue

import (
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type inspectionQueueTestPermit struct {
	inspectionTestPermit
	group string
}

func (p *inspectionQueueTestPermit) InspectionQueueGroup() string { return p.group }

func TestConcurrentInspectionRequiresSavedPrivateQueueAuthority(t *testing.T) {
	_, client := supervisorLeaseTestBind(t)
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	legacy := identity
	legacy.QueueGroup = ""
	if identity.Label() != legacy.Label() {
		t.Fatal("scheduling changed the immutable inspection label")
	}
	permit := &inspectionQueueTestPermit{
		inspectionTestPermit: inspectionTestPermit{
			submissionRoot: identity.RootID, submissionTask: identity.TaskID, submissionSupervisor: client.Binding(),
		},
		group: "default",
	}
	if err := validateInspectionSubmissionPermit(permit, identity, client.Binding()); err != nil {
		t.Fatalf("saved private default queue rejected: %v", err)
	}
	if err := validateInspectionSubmissionPermit(&permit.inspectionTestPermit, identity, client.Binding()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("legacy permit authorized a different queue: %v", err)
	}
	if err := validateInspectionSubmissionPermit(permit, legacy, client.Binding()); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("default queue permit authorized legacy queue: %v", err)
	}
	if err := validateInspectionQueueBinding(identity, validBindingForTest()); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("external supervisor accepted private scheduling: %v", err)
	}
	identity.QueueGroup = "unrelated"
	if err := validateInspectionIdentity(identity); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("arbitrary inspection queue accepted: %v", err)
	}
}

func TestConcurrentInspectionObservationRequiresUnlimitedExactQueue(t *testing.T) {
	_, client := supervisorLeaseTestBind(t)
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	label := identity.Label()
	job := Job{ID: 7, Label: &label, State: StateRunning, Group: "default"}
	for _, tc := range []struct {
		name      string
		group     Group
		jobs      []Job
		wantMatch bool
	}{
		{name: "independent running worker", group: Group{Status: "Running", ParallelTasks: 0}, jobs: []Job{job}, wantMatch: true},
		{name: "paused queue", group: Group{Status: "Paused", ParallelTasks: 0}, jobs: []Job{job}},
		{name: "serialized queue", group: Group{Status: "Running", ParallelTasks: 1}, jobs: []Job{job}},
		{name: "duplicate label", group: Group{Status: "Running", ParallelTasks: 0}, jobs: []Job{job, job}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation, err := matchingInspectionObservation(QueueSnapshot{Jobs: tc.jobs, Groups: map[string]Group{"default": tc.group}}, identity, client.Binding())
			if observation.Matched != tc.wantMatch || (err == nil) != tc.wantMatch {
				t.Fatalf("match=%t err=%v, want match=%t", observation.Matched, err, tc.wantMatch)
			}
		})
	}
}
