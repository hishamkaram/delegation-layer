package contract

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOnlyNewlyCommittedClaimAuthorizesInvocation(t *testing.T) {
	var zero ClaimResult
	cases := []struct {
		name   string
		result ClaimResult
		grant  bool
	}{
		{name: "zero value", result: zero},
		{name: "unknown", result: ClaimResult{Outcome: ClaimUnknown}},
		{name: "unknown with matching readback", result: ClaimResult{Outcome: ClaimUnknown, TaskID: testTaskID}},
		{name: "existing other task", result: ClaimResult{Outcome: ClaimExisting, TaskID: strings.Repeat("1", 32)}},
		{name: "existing matching task", result: ClaimResult{Outcome: ClaimExisting, TaskID: testTaskID}},
		{name: "undefined outcome", result: ClaimResult{Outcome: ClaimNewlyCommitted + 1, TaskID: testTaskID}},
		{name: "newly committed", result: ClaimResult{Outcome: ClaimNewlyCommitted, TaskID: testTaskID}, grant: true},
	}
	for _, testCase := range cases {
		if got := testCase.result.AuthorizesInvocation(); got != testCase.grant {
			t.Errorf("%s: authorizes=%v, want %v", testCase.name, got, testCase.grant)
		}
	}
}

func TestValidateClaimRequiresDueStrictlyAfterExpectedCursor(t *testing.T) {
	valid := Claim{ScheduleID: testScheduleID, ExpectedCursor: at(100), Due: at(100).Add(time.Nanosecond), TaskID: testTaskID, ArmedAt: at(101)}
	if err := ValidateClaim(valid); err != nil {
		t.Fatalf("valid claim rejected: %v", err)
	}
	cases := map[string]func(*Claim){
		"due at cursor":     func(c *Claim) { c.Due = c.ExpectedCursor },
		"due before cursor": func(c *Claim) { c.Due = at(99) },
		"invalid task id":   func(c *Claim) { c.TaskID = strings.ToUpper(testTaskID) },
		"invalid schedule":  func(c *Claim) { c.ScheduleID = testTaskID[:10] },
		"local due":         func(c *Claim) { c.Due = time.Unix(200, 0) },
		"zero armed_at":     func(c *Claim) { c.ArmedAt = time.Time{} },
		"zero cursor":       func(c *Claim) { c.ExpectedCursor = time.Time{} },
	}
	for name, mutate := range cases {
		claim := valid
		mutate(&claim)
		if err := ValidateClaim(claim); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestValidateObservationBindsOutcomeDigestToTerminalPublication(t *testing.T) {
	base := Observation{ObservedAt: at(300), Admission: task.AdmissionAdmitted, Liveness: task.LivenessEnded}
	valid := map[string]func(*Observation){
		"pending without digest": func(o *Observation) { o.Publication = task.PublicationPending },
		"unknown with fault":     func(o *Observation) { o.FaultCode = "provider_unavailable" },
		"committed with digest":  func(o *Observation) { o.Publication, o.OutcomeSHA256 = task.PublicationCommitted, digestOf('c') },
		"rejected with digest":   func(o *Observation) { o.Publication, o.OutcomeSHA256 = task.PublicationRejected, digestOf('d') },
	}
	for name, mutate := range valid {
		observation := base
		mutate(&observation)
		if err := ValidateOccurrenceObservation(OccurrenceObservation{TaskID: testTaskID, Observation: observation}); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
	invalid := map[string]func(*Observation){
		"committed without digest":  func(o *Observation) { o.Publication = task.PublicationCommitted },
		"pending with digest":       func(o *Observation) { o.Publication, o.OutcomeSHA256 = task.PublicationPending, digestOf('c') },
		"malformed digest":          func(o *Observation) { o.Publication, o.OutcomeSHA256 = task.PublicationRejected, "C" },
		"raw error text fault":      func(o *Observation) { o.FaultCode = "dial tcp: connection refused" },
		"oversized fault code":      func(o *Observation) { o.FaultCode = strings.Repeat("a", 65) },
		"unknown admission":         func(o *Observation) { o.Admission = task.AdmissionAdmitted + 1 },
		"unknown liveness":          func(o *Observation) { o.Liveness = task.LivenessEnded + 1 },
		"unknown publication":       func(o *Observation) { o.Publication = task.PublicationRejected + 1 },
		"local observation instant": func(o *Observation) { o.ObservedAt = time.Unix(300, 0) },
	}
	for name, mutate := range invalid {
		observation := base
		mutate(&observation)
		if err := ValidateObservation(observation); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestReleaseReferencesMatchTheirKind(t *testing.T) {
	cases := []struct {
		release Release
		valid   bool
	}{
		{release: Release{ReleasedAt: at(400), Kind: ReleaseNotCreated, Ref: "resource_pressure"}, valid: true},
		{release: Release{ReleasedAt: at(400), Kind: ReleaseClosed, Ref: digestOf('e')}, valid: true},
		{release: Release{ReleasedAt: at(400), Kind: ReleaseNotCreated, Ref: digestOf('1')}},
		{release: Release{ReleasedAt: at(400), Kind: ReleaseNotCreated, Ref: ""}},
		{release: Release{ReleasedAt: at(400), Kind: ReleaseNotCreated, Ref: "Provider-Failed"}},
		{release: Release{ReleasedAt: at(400), Kind: ReleaseClosed, Ref: "resource_pressure"}},
		{release: Release{ReleasedAt: at(400), Kind: "cancelled", Ref: digestOf('e')}},
		{release: Release{Kind: ReleaseClosed, Ref: digestOf('e')}},
	}
	for _, testCase := range cases {
		err := ValidateOccurrenceRelease(OccurrenceRelease{TaskID: testTaskID, Release: testCase.release})
		if (err == nil) != testCase.valid {
			t.Errorf("%+v: err=%v, want valid=%v", testCase.release, err, testCase.valid)
		}
	}
	if err := ValidateOccurrenceRelease(OccurrenceRelease{TaskID: "x", Release: cases[0].release}); err == nil {
		t.Fatal("invalid release task id accepted")
	}
}

func TestRepeatedReleaseIsIdempotentOnlyForTheSameFact(t *testing.T) {
	stored := Release{ReleasedAt: at(400), Kind: ReleaseClosed, Ref: digestOf('e')}
	retry := Release{ReleasedAt: at(450), Kind: ReleaseClosed, Ref: digestOf('e')}
	if err := CheckRepeatedRelease(stored, retry); err != nil {
		t.Fatalf("idempotent retry rejected: %v", err)
	}
	for name, requested := range map[string]Release{
		"other kind":      {ReleasedAt: at(400), Kind: ReleaseNotCreated, Ref: "resource_pressure"},
		"other reference": {ReleasedAt: at(400), Kind: ReleaseClosed, Ref: digestOf('f')},
	} {
		if err := CheckRepeatedRelease(stored, requested); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: err=%v, want ErrConflict", name, err)
		}
	}
}
