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
			Runner: filepath.Join(origin.Root(), "runner"), RunnerSHA256: strings.Repeat("a", 64),
			Client: filepath.Join(origin.Root(), "client"), ClientSHA256: strings.Repeat("b", 64),
			Daemon: filepath.Join(origin.Root(), "daemon"), DaemonSHA256: strings.Repeat("c", 64),
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
		Protocol: ProtocolVersion, Sequence: 1, Epoch: originTestRootID,
		Inputs: inputs, InputsSHA256: digest,
		Starter:    StartupProvenance{Kind: StarterOutside, Root: origin.Root(), RootID: origin.RootID(), PID: 7, StartToken: "fixture:7"},
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

func TestStartupInputIdentityExcludesCallerProvenance(t *testing.T) {
	first := startupIntentFixture(t)
	second := first
	second.Starter.PID, second.Starter.StartToken = 8, "fixture:8"
	if err := second.Validate(); err != nil {
		t.Fatal(err)
	}
	if first.InputsSHA256 != second.InputsSHA256 {
		t.Fatal("equivalent target inputs changed with caller provenance")
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
		t.Fatal("immutable attempt digest lost its original starter identity")
	}
	second.Inputs.Artifacts.DaemonSHA256 = strings.Repeat("d", 64)
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
		{"artifact-digest", func(i *StartupIntent) { i.Inputs.Artifacts.RunnerSHA256 = "invalid" }},
		{"launch-context", func(i *StartupIntent) { i.Inputs.Context.SHA256 = "invalid" }},
		{"upgrade-choice", func(i *StartupIntent) { i.Inputs.UpgradeID = originTestRootID }},
		{"digest", func(i *StartupIntent) { i.InputsSHA256 = strings.Repeat("f", 64) }},
		{"caller", func(i *StartupIntent) { i.Starter.Kind = "unknown" }},
		{"caller-root", func(i *StartupIntent) { i.Starter.RootID = strings.Repeat("f", 32) }},
		{"caller-process", func(i *StartupIntent) { i.Starter.PID = 0 }},
		{"caller-epoch", func(i *StartupIntent) { i.Starter.Kind = StarterOwner; i.Starter.Epoch = "invalid" }},
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

func TestStartupUpgradeHasOneArtifactAuthority(t *testing.T) {
	intent := startupIntentFixture(t)
	intent.Inputs.Kind = StartupUpgrade
	intent.Inputs.Artifacts, intent.Inputs.Context = Artifacts{}, LaunchContextReference{}
	intent.Inputs.UpgradeID, intent.Inputs.UpgradeSHA256 = originTestRootID, strings.Repeat("d", 64)
	if err := intent.Inputs.Validate(); err != nil {
		t.Fatal(err)
	}
	var err error
	intent.InputsSHA256, err = intent.Inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(intent)
	if err != nil {
		t.Fatal(err)
	}
	var decoded StartupIntent
	if err = task.DecodeStrict(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if err = decoded.Validate(); err != nil {
		t.Fatalf("upgrade intent did not preserve its lifecycle reference: %v", err)
	}
	intent.Inputs.Artifacts.Runner = filepath.Join(intent.Inputs.Root, "another-choice")
	if err = intent.Inputs.Validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("upgrade made an independent artifact selection: %v", err)
	}
}

func TestStartupOwnerProvenanceRequiresAnotherRoot(t *testing.T) {
	intent := startupIntentFixture(t)
	caller := testOrigin(t)
	intent.Starter = StartupProvenance{
		Kind: StarterOwner, Root: caller.Root(), RootID: strings.Repeat("e", 32),
		Epoch: originTestRootID, PID: 7, StartToken: "fixture:7",
	}
	if err := intent.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, samePath := range []bool{false, true} {
		invalid := intent
		if samePath {
			invalid.Starter.Root = invalid.Inputs.Root
		} else {
			invalid.Starter.RootID = invalid.Inputs.RootID
		}
		if err := invalid.Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("same-root owner creation accepted: %v", err)
		}
	}
	upgrade := intent.Inputs
	upgrade.Kind, upgrade.Artifacts, upgrade.Context = StartupUpgrade, Artifacts{}, LaunchContextReference{}
	upgrade.UpgradeID, upgrade.UpgradeSHA256 = originTestRootID, strings.Repeat("d", 64)
	if err := intent.Starter.validate(upgrade); !errors.Is(err, ErrIdentity) {
		t.Fatalf("contained owner authorized owner-upgrade: %v", err)
	}
}

func TestStartupDigestsRefuseOversizedRecords(t *testing.T) {
	intent := startupIntentFixture(t)
	intent.Inputs.Artifacts.Runner = string(filepath.Separator) + strings.Repeat("x", task.MaxControlRecordSize)
	if _, err := intent.Inputs.Digest(); !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized startup inputs accepted: %v", err)
	}
	intent = startupIntentFixture(t)
	intent.Starter.StartToken = strings.Repeat("x", task.MaxControlRecordSize)
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
		bytes.Replace(data, []byte(`"protocol":1`), []byte(`"protocol":1,"protocol":1`), 1),
		bytes.Replace(data, []byte(`"protocol":1`), []byte(`"unknown":1`), 1),
	} {
		if err = task.DecodeStrict(corrupted, &decoded); err == nil {
			t.Fatal("corrupted startup JSON accepted")
		}
	}
}
