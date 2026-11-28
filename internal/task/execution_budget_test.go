package task

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"
)

// The expected bytes below are written by hand from the record contracts.
// Finite records must keep the bytes earlier releases wrote; unlimited records
// add only an omitted-when-false marker and carry no deadline.
const (
	budgetFixtureRoot = "11111111111111111111111111111111"
	budgetFixtureTask = "22222222222222222222222222222222"
	budgetFixtureSpec = "3333333333333333333333333333333333333333333333333333333333333333"
	budgetFixtureMeta = "4444444444444444444444444444444444444444444444444444444444444444"
	budgetFixtureStop = "33333333333333333333333333333333"

	finiteTaskJSON = `{"schema_version":1,"root_id":"` + budgetFixtureRoot + `","task_id":"` + budgetFixtureTask +
		`","provider":"fixture:test","mode":"read-only","canonical_cwd":"/fixture/workspace",` +
		`"requested_config":{"permission":"read-only","budget":"1s"},"budget_nanos":1000000000,` +
		`"brief_sha256":"29a8825bd242f14386ee528d76e0e8f1e38f3c8c4047d7b2d6df7493368a17d0","brief_length":5}` + "\n"

	providerStartPrefixJSON = `{"schema_version":1,"root_id":"` + budgetFixtureRoot + `","task_id":"` + budgetFixtureTask +
		`","spec_sha256":"` + budgetFixtureSpec + `","meta_sha256":"` + budgetFixtureMeta + `",`
	finiteProviderStartJSON    = providerStartPrefixJSON + `"budget_nanos":60000000000,"created_at":"2026-10-11T11:00:00Z"}` + "\n"
	unlimitedProviderStartJSON = providerStartPrefixJSON + `"budget_nanos":0,"budget_unlimited":true,"created_at":"2026-10-11T11:00:00Z"}` + "\n"

	stopSupervisorJSON = `{"client_executable":"/fixture/pueue","client_sha256":"` + "5555555555555555555555555555555555555555555555555555555555555555" +
		`","resolved_config_sha256":"` + "6666666666666666666666666666666666666666666666666666666666666666" +
		`","endpoint":"fixture://supervisor","config_path":"/fixture/supervisor.json","config_digest":"` +
		"7777777777777777777777777777777777777777777777777777777777777777" + `","observed_version":"fixture-v1"}`
	finiteUserStopJSON = `{"meta_sha256":"` + budgetFixtureMeta + `","label":"delegate:` + budgetFixtureRoot + ":" + budgetFixtureTask +
		`","budget_nanos":60000000000,"schema_version":1,"root_id":"` + budgetFixtureRoot + `","task_id":"` + budgetFixtureTask +
		`","spec_sha256":"` + budgetFixtureSpec + `","request_id":"` + budgetFixtureStop +
		`","cause":"user","supervisor":` + stopSupervisorJSON + `,"requested_at":"2026-10-11T11:00:00Z"}` + "\n"
	unlimitedUserStopJSON = `{"meta_sha256":"` + budgetFixtureMeta + `","label":"delegate:` + budgetFixtureRoot + ":" + budgetFixtureTask +
		`","budget_unlimited":true,"schema_version":1,"root_id":"` + budgetFixtureRoot + `","task_id":"` + budgetFixtureTask +
		`","spec_sha256":"` + budgetFixtureSpec + `","request_id":"` + budgetFixtureStop +
		`","cause":"user","supervisor":` + stopSupervisorJSON + `,"requested_at":"2026-10-11T11:00:00Z"}` + "\n"

	lifetimeIntentPrefixJSON = `{"schema_version":1,"root_id":"` + budgetFixtureRoot + `","task_id":"` + budgetFixtureTask +
		`","owner_kind":"task","owner_sha256":"4c1029697ee358715d3a14a2add817c4b01651440de808371f78165ac90dc581",` +
		`"scope_kind":"cgroup-v2","assurance":"exact","scope_plan":"cgroup-plan-v1","boot_id":"boot-token",`
	finiteLifetimeIntentJSON    = lifetimeIntentPrefixJSON + `"deadline":"2026-10-11T12:00:00Z","created_at":"2026-10-11T11:00:00Z"}` + "\n"
	unlimitedLifetimeIntentJSON = lifetimeIntentPrefixJSON + `"deadline":"","unlimited":true,"created_at":"2026-10-11T11:00:00Z"}` + "\n"
)

func TestFiniteTaskRecordKeepsCanonicalBytesAndBudget(t *testing.T) {
	record := fixtureTask(t)
	assertCanonicalBytes(t, record, finiteTaskJSON)
	budget, err := record.ExecutionBudget()
	if duration, finite := budget.Finite(); err != nil || !finite || duration != time.Second {
		t.Fatalf("finite task budget = %v, %v", budget, err)
	}
}

func TestUnlimitedTaskRequestNormalizesWithoutDeadlineOrTimeoutBound(t *testing.T) {
	record := fixtureTask(t)
	record.RequestedConfig.Budget = policy.UnlimitedBudget
	record.RequestedConfig.NativeTimeout = "60000m"
	record.BudgetNanos = 0
	if err := NormalizeRequestedConfig(&record); err != nil {
		t.Fatal(err)
	}
	if record.RequestedConfig.Budget != "unlimited" || record.BudgetNanos != 0 || record.RequestedConfig.NativeTimeout != "1000h0m0s" {
		t.Fatalf("unlimited request normalized to %+v", record)
	}
	if err := ValidateTaskRecord(&record); err != nil {
		t.Fatalf("normalized unlimited task rejected: %v", err)
	}
	if budget, err := record.ExecutionBudget(); err != nil || budget != policy.UnlimitedExecutionBudget() {
		t.Fatalf("unlimited task budget = %v, %v", budget, err)
	}
	encoded, err := MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"budget":"unlimited","native_timeout":"1000h0m0s"},"budget_nanos":0,`)) {
		t.Fatalf("unlimited task bytes = %s", encoded)
	}
	finite := fixtureTask(t)
	if CompareRequests(&record, &finite) {
		t.Fatal("unlimited and finite requests compared equal")
	}
}

func TestUnlimitedTaskRejectsNanosProjectionMismatch(t *testing.T) {
	request := fixtureTask(t)
	request.RequestedConfig.Budget = policy.UnlimitedBudget
	request.BudgetNanos = 1
	before := request
	if err := NormalizeRequestedConfig(&request); err == nil || err.Error() != "requested budget does not match budget_nanos" {
		t.Fatalf("unlimited request with nanos error = %v", err)
	}
	if !CompareRequests(&request, &before) {
		t.Fatal("failed normalization altered the request")
	}
	if err := ValidateTaskRecord(&request); err == nil {
		t.Fatal("saved unlimited task with budget_nanos accepted")
	}
	if _, err := request.ExecutionBudget(); err == nil {
		t.Fatal("inconsistent unlimited projection decoded")
	}
	finiteZero := fixtureTask(t)
	finiteZero.BudgetNanos = 0
	if err := ValidateTaskRecord(&finiteZero); err == nil {
		t.Fatal("finite task without its nanosecond projection accepted")
	}
}

func TestMetaRecordAcceptsExplicitUnlimitedAndRejectsOmittedBudget(t *testing.T) {
	unlimited := fixtureMeta(t)
	unlimited.RequestedConfig.Budget = policy.UnlimitedBudget
	unlimited.RequestedConfig.NativeTimeout = "1000h0m0s"
	if err := ValidateMetaRecord(&unlimited); err != nil {
		t.Fatalf("unlimited meta rejected: %v", err)
	}
	omitted := fixtureMeta(t)
	omitted.RequestedConfig.Budget = ""
	if err := ValidateMetaRecord(&omitted); !errors.Is(err, policy.ErrBudgetRequired) {
		t.Fatalf("meta without budget error = %v, want ErrBudgetRequired", err)
	}
}

func TestProviderStartBudgetRepresentation(t *testing.T) {
	finite := decodeProviderStart(t, finiteProviderStartJSON)
	if budget, err := finite.ExecutionBudget(); err != nil || budget.Nanos() != int64(time.Minute) || budget.Unlimited() {
		t.Fatalf("finite provider.start budget = %v, %v", budget, err)
	}
	unlimited := decodeProviderStart(t, unlimitedProviderStartJSON)
	if budget, err := unlimited.ExecutionBudget(); err != nil || budget != policy.UnlimitedExecutionBudget() {
		t.Fatalf("unlimited provider.start budget = %v, %v", budget, err)
	}
	for name, mutate := range map[string]func(*ProviderStartRecord){
		"finite without nanos":     func(r *ProviderStartRecord) { r.BudgetNanos = 0 },
		"negative nanos":           func(r *ProviderStartRecord) { r.BudgetNanos = -1 },
		"unlimited with nanos":     func(r *ProviderStartRecord) { r.BudgetUnlimited = true },
		"unlimited with negatives": func(r *ProviderStartRecord) { r.BudgetUnlimited, r.BudgetNanos = true, -1 },
	} {
		record := finite
		mutate(&record)
		if err := ValidateProviderStartRecord(&record); err == nil {
			t.Errorf("%s: invalid provider.start budget accepted", name)
		}
	}
}

// releasedProviderStartRecord is the provider.start shape before unlimited
// budgets. Its strict decoder must reject the unlimited marker rather than
// read budget_nanos 0 as a budget.
type releasedProviderStartRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	TaskID        string `json:"task_id"`
	SpecSHA256    string `json:"spec_sha256"`
	MetaSHA256    string `json:"meta_sha256"`
	BudgetNanos   int64  `json:"budget_nanos"`
	CreatedAt     string `json:"created_at"`
}

func TestReleasedProviderStartReaderFailsClosedOnUnlimited(t *testing.T) {
	var released releasedProviderStartRecord
	if err := DecodeStrict([]byte(finiteProviderStartJSON), &released); err != nil {
		t.Fatalf("released reader rejected finite bytes: %v", err)
	}
	released = releasedProviderStartRecord{}
	if err := DecodeStrict([]byte(unlimitedProviderStartJSON), &released); err == nil {
		t.Fatalf("released reader accepted unlimited provider.start as %+v", released)
	}
}

func TestStopRequestBudgetRepresentation(t *testing.T) {
	finite := decodeStopRequest(t, finiteUserStopJSON)
	if budget, err := finite.ExecutionBudget(); err != nil || budget.Nanos() != int64(time.Minute) {
		t.Fatalf("finite stop budget = %v, %v", budget, err)
	}
	unlimited := decodeStopRequest(t, unlimitedUserStopJSON)
	if budget, err := unlimited.ExecutionBudget(); err != nil || !budget.Unlimited() {
		t.Fatalf("unlimited stop budget = %v, %v", budget, err)
	}

	silence := unlimited
	silence.RequestID, silence.Cause, silence.Deadline = StopCauseOutputSilence, StopCauseOutputSilence, "2026-10-11T12:00:00Z"
	if err := ValidateStopRequestRecord(&silence); err != nil {
		t.Fatalf("output-silence stop for unlimited budget rejected: %v", err)
	}
	finiteBudgetStop := finite
	finiteBudgetStop.RequestID, finiteBudgetStop.Cause, finiteBudgetStop.Deadline = "budget", "budget", "2026-10-11T12:00:00Z"
	if err := ValidateStopRequestRecord(&finiteBudgetStop); err != nil {
		t.Fatalf("finite budget stop rejected: %v", err)
	}
	unlimitedBudgetStop := finiteBudgetStop
	unlimitedBudgetStop.BudgetNanos, unlimitedBudgetStop.BudgetUnlimited = 0, true
	if err := ValidateStopRequestRecord(&unlimitedBudgetStop); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("budget stop for an unlimited budget error = %v, want ErrIdentityMismatch", err)
	}
	for name, mutate := range map[string]func(*StopRequestRecord){
		"finite without nanos": func(r *StopRequestRecord) { r.BudgetNanos = 0 },
		"unlimited with nanos": func(r *StopRequestRecord) { r.BudgetUnlimited = true },
	} {
		record := finite
		mutate(&record)
		if err := ValidateStopRequestRecord(&record); !errors.Is(err, ErrIdentityMismatch) {
			t.Errorf("%s: error = %v, want ErrIdentityMismatch", name, err)
		}
	}
}

func TestLifetimeIntentDeadlineRepresentation(t *testing.T) {
	finite := validLifetimeIntent()
	assertCanonicalBytes(t, finite, finiteLifetimeIntentJSON)

	var unlimited LifetimeIntentRecord
	if err := DecodeStrict([]byte(unlimitedLifetimeIntentJSON), &unlimited); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLifetimeIntentRecord(&unlimited); err != nil {
		t.Fatalf("unlimited task intent rejected: %v", err)
	}
	assertCanonicalBytes(t, unlimited, unlimitedLifetimeIntentJSON)

	for name, mutate := range map[string]func(*LifetimeIntentRecord){
		"unlimited with deadline":  func(r *LifetimeIntentRecord) { r.Deadline = "2026-10-11T12:00:00Z" },
		"inspection owner":         func(r *LifetimeIntentRecord) { r.OwnerKind = LifetimeOwnerInspection },
		"workspace owner":          func(r *LifetimeIntentRecord) { r.OwnerKind = LifetimeOwnerWorkspace },
		"bounded without deadline": func(r *LifetimeIntentRecord) { r.Unlimited = false },
	} {
		record := unlimited
		mutate(&record)
		if err := ValidateLifetimeIntentRecord(&record); err == nil {
			t.Errorf("%s: invalid lifetime intent accepted", name)
		}
	}
}

func TestLifetimeRegistrationDeadlineRepresentationAndIntentMatch(t *testing.T) {
	unlimitedBytes := strings.Replace(legacyLifetimeRegistrationJSON, `"deadline":"2026-10-11T12:00:00Z"`, `"deadline":"","unlimited":true`, 1) + "}\n"
	var registration LifetimeRegistrationRecord
	if err := DecodeStrict([]byte(unlimitedBytes), &registration); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLifetimeRegistrationRecord(&registration); err != nil {
		t.Fatalf("unlimited task registration rejected: %v", err)
	}
	assertCanonicalBytes(t, registration, unlimitedBytes)
	for name, mutate := range map[string]func(*LifetimeRegistrationRecord){
		"unlimited with deadline":  func(r *LifetimeRegistrationRecord) { r.Deadline = "2026-10-11T12:00:00Z" },
		"workspace owner":          func(r *LifetimeRegistrationRecord) { r.OwnerKind = LifetimeOwnerWorkspace },
		"bounded without deadline": func(r *LifetimeRegistrationRecord) { r.Unlimited = false },
	} {
		record := registration
		mutate(&record)
		if err := ValidateLifetimeRegistrationRecord(&record); err == nil {
			t.Errorf("%s: invalid lifetime registration accepted", name)
		}
	}

	intent := validLifetimeIntent()
	intent.Deadline, intent.Unlimited = "", true
	if !LifetimeIntentMatchesRegistration(&intent, &registration) {
		t.Fatal("unlimited intent did not match its unlimited registration")
	}
	bounded := registration
	bounded.Unlimited = false
	if LifetimeIntentMatchesRegistration(&intent, &bounded) {
		t.Fatal("intent matched a registration with a different unlimited marker")
	}
	finite := validLifetimeIntent()
	finiteRegistration := validLifetimeRegistration()
	if !LifetimeIntentMatchesRegistration(&finite, &finiteRegistration) || LifetimeIntentMatchesRegistration(&finite, &registration) {
		t.Fatal("finite intent matching changed")
	}
}

func decodeProviderStart(t *testing.T, data string) ProviderStartRecord {
	t.Helper()
	var record ProviderStartRecord
	if err := DecodeStrict([]byte(data), &record); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProviderStartRecord(&record); err != nil {
		t.Fatalf("provider.start rejected: %v", err)
	}
	assertCanonicalBytes(t, record, data)
	return record
}

func decodeStopRequest(t *testing.T, data string) StopRequestRecord {
	t.Helper()
	var record StopRequestRecord
	if err := DecodeStrict([]byte(data), &record); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStopRequestRecord(&record); err != nil {
		t.Fatalf("stop request rejected: %v", err)
	}
	assertCanonicalBytes(t, record, data)
	return record
}

func assertCanonicalBytes(t *testing.T, record any, want string) {
	t.Helper()
	got, err := MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical bytes changed:\n got %s\nwant %s", got, want)
	}
}
