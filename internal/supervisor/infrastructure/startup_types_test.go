package infrastructure

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func startupIntentFixture(t *testing.T) StartupIntent {
	t.Helper()
	origin := testOrigin(t)
	inputs := StartupInputs{
		Kind: StartupRootOwner, Root: origin.Root(), RootID: origin.RootID(),
		RootDevice: 23, RootInode: 41,
		BootID: "linux:11111111-2222-3333-4444-555555555555",
		Artifacts: Artifacts{
			Runner: filepath.Join(origin.Root(), "runner"),
		},
		Context: LaunchContextReference{SHA256: strings.Repeat("d", 64)},
	}
	digest, err := inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	// These values prove record policy only. No live process or artifact authority
	// is claimed by this pure fixture.
	return StartupIntent{
		Protocol: RecordProtocolVersion, Sequence: 1, Epoch: originTestRootID,
		Inputs: inputs, InputsSHA256: digest,
		AcceptedAt: "2026-11-03T00:00:00Z", Deadline: "2026-11-03T00:00:30Z",
	}
}

func TestStartupIntentObservationNeverRenewsDeadline(t *testing.T) {
	intent := startupIntentFixture(t)
	before := intent
	for _, test := range []struct {
		timestamp string
		expired   bool
	}{
		{"2026-11-03T00:00:29.999999999Z", false},
		{"2026-11-03T00:00:30Z", true},
		{"2026-11-03T00:30:00Z", true},
	} {
		now, err := time.Parse(time.RFC3339Nano, test.timestamp)
		if err != nil {
			t.Fatal(err)
		}
		expired, err := intent.Expired(now)
		if err != nil || expired != test.expired || intent != before {
			t.Fatalf("deadline observation: expired=%v err=%v changed=%v", expired, err, intent != before)
		}
	}
}

func TestStartupInputIdentityExcludesAttemptDeadline(t *testing.T) {
	first := startupIntentFixture(t)
	second := first
	second.Deadline = "2026-11-03T00:00:31Z"
	if err := second.Validate(); err != nil {
		t.Fatal(err)
	}
	if first.InputsSHA256 != second.InputsSHA256 {
		t.Fatal("equivalent target inputs changed with attempt deadline")
	}
	firstDigest, err := first.Digest()
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := second.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("immutable attempt digest lost its original deadline")
	}
	second.Inputs.Artifacts.Runner = filepath.Join(first.Inputs.Root, "other-runner")
	if err = second.Validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("changed inputs accepted under original digest: %v", err)
	}
}

func TestStartupInputBindsTargetDirectory(t *testing.T) {
	intent := startupIntentFixture(t)
	for _, changeDevice := range []bool{false, true} {
		replacement := intent.Inputs
		if changeDevice {
			replacement.RootDevice++
		} else {
			replacement.RootInode++
		}
		digest, err := replacement.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if digest == intent.InputsSHA256 {
			t.Fatal("cloned root metadata hid native directory replacement")
		}
	}
}

func TestStartupIntentRejectsInvalidRecords(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*StartupIntent)
	}{
		{"protocol", func(i *StartupIntent) { i.Protocol++ }},
		{"sequence", func(i *StartupIntent) { i.Sequence = 0 }},
		{"epoch", func(i *StartupIntent) { i.Epoch = "invalid" }},
		{"root", func(i *StartupIntent) { i.Inputs.RootID = "invalid" }},
		{"inode", func(i *StartupIntent) { i.Inputs.RootInode = 0 }},
		{"boot", func(i *StartupIntent) { i.Inputs.BootID = "unknown" }},
		{"kind", func(i *StartupIntent) { i.Inputs.Kind = "unknown" }},
		{"artifact", func(i *StartupIntent) { i.Inputs.Artifacts.Runner = "relative" }},
		{"launch-context", func(i *StartupIntent) { i.Inputs.Context.SHA256 = "invalid" }},
		{"digest", func(i *StartupIntent) { i.InputsSHA256 = strings.Repeat("f", 64) }},
		{"deadline", func(i *StartupIntent) { i.Deadline = i.AcceptedAt }},
		{"time-zone", func(i *StartupIntent) { i.Deadline = "2026-11-03T01:00:30+01:00" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := startupIntentFixture(t)
			test.mutate(&intent)
			if err := intent.Validate(); !errors.Is(err, ErrIdentity) {
				t.Fatalf("invalid record accepted: %v", err)
			}
		})
	}
}

func TestStartupDigestsRefuseOversizedRecords(t *testing.T) {
	intent := startupIntentFixture(t)
	intent.Inputs.Artifacts.Runner = string(filepath.Separator) + strings.Repeat("x", task.MaxControlRecordSize)
	if _, err := intent.Inputs.Digest(); !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized startup inputs accepted: %v", err)
	}
	if _, err := intent.Digest(); !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized startup intent accepted: %v", err)
	}
}

func TestStartupIntentStrictDecoding(t *testing.T) {
	intent := startupIntentFixture(t)
	data, err := task.MarshalCanonical(intent)
	if err != nil {
		t.Fatal(err)
	}
	var decoded StartupIntent
	if err = task.DecodeStrict(data, &decoded); err != nil || decoded != intent {
		t.Fatalf("canonical record did not round trip: %v", err)
	}
	for _, corrupted := range [][]byte{
		append(bytes.Clone(data), []byte("{}")...),
		bytes.Replace(data, []byte(`"protocol":3`), []byte(`"protocol":3,"protocol":3`), 1),
		bytes.Replace(data, []byte(`"protocol":3`), []byte(`"unknown":1`), 1),
	} {
		if err = task.DecodeStrict(corrupted, &decoded); err == nil {
			t.Fatal("corrupted startup JSON accepted")
		}
	}
}

func TestStartupRejectsRetiredEpochOperation(t *testing.T) {
	intent := startupIntentFixture(t)
	intent.Inputs.Kind = StartupKind("owner-upgrade")
	if err := intent.Inputs.Validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("retired operation became startup: %v", err)
	}
	if _, err := intent.Inputs.Digest(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("retired operation acquired an input digest: %v", err)
	}
}

func TestStartupRejectsRetiredEpochRecord(t *testing.T) {
	inputs := startupIntentFixture(t).Inputs
	inputs.Kind = StartupKind("owner-upgrade")
	inputs.Artifacts, inputs.Context = Artifacts{}, LaunchContextReference{}
	data, err := task.MarshalCanonical(inputs)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.TrimSpace(data)
	data = append(data[:len(data)-1], []byte(`,"upgrade_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","upgrade_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)...)
	var decoded StartupInputs
	err = task.DecodeStrict(data, &decoded)
	if err == nil {
		err = decoded.Validate()
	}
	if err == nil {
		t.Fatal("retired owner-upgrade record became an ordinary startup")
	}
}
