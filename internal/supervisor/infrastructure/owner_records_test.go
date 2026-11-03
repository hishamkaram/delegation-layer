package infrastructure

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ownerRecordFixture(t *testing.T) (StartupIntent, StartupAssumption, OwnerPublication) {
	t.Helper()
	intent := startupIntentFixture(t)
	ref, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	assumption := StartupAssumption{
		Startup: ref, Process: ProcessIdentity{PID: 7, StartToken: "fixture:7"},
		UID: 1000, AssumedAt: "2026-11-03T00:00:29.999999999Z",
	}
	digest, err := assumption.Digest(intent)
	if err != nil {
		t.Fatal(err)
	}
	publication := OwnerPublication{
		Protocol: ProtocolVersion, Startup: ref, AssumptionSHA256: digest,
		RuntimeSHA256: intent.Inputs.Artifacts.RunnerSHA256,
		Endpoint: EndpointIdentity{
			Path:         filepath.Join(intent.Inputs.Root, ".i"),
			DirectoryDev: intent.Inputs.RootDevice, DirectoryInode: intent.Inputs.RootInode, SocketInode: 24,
		},
	}
	return intent, assumption, publication
}

func TestOwnerPublicationBindsOriginalAssumption(t *testing.T) {
	intent, assumption, publication := ownerRecordFixture(t)
	if err := publication.Validate(intent, assumption); err != nil {
		t.Fatal(err)
	}
	digest, err := publication.Digest(intent, assumption)
	if err != nil || task.ValidateSHA256(digest) != nil {
		t.Fatalf("publication digest: %v", err)
	}
	for _, changeProcess := range []bool{false, true} {
		changed := assumption
		if changeProcess {
			changed.Process.PID++
		} else {
			changed.UID++
		}
		if err = publication.Validate(intent, changed); !errors.Is(err, ErrIdentity) {
			t.Fatalf("changed child identity accepted: %v", err)
		}
	}
}

func TestOwnerPublicationRefusesInvalidReferences(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*OwnerPublication)
	}{
		{"protocol", func(p *OwnerPublication) { p.Protocol++ }},
		{"sequence", func(p *OwnerPublication) { p.Startup.Sequence++ }},
		{"epoch", func(p *OwnerPublication) { p.Startup.Epoch = strings.Repeat("e", 32) }},
		{"intent", func(p *OwnerPublication) { p.Startup.IntentSHA256 = strings.Repeat("e", 64) }},
		{"assumption", func(p *OwnerPublication) { p.AssumptionSHA256 = strings.Repeat("e", 64) }},
		{"runtime", func(p *OwnerPublication) { p.RuntimeSHA256 = strings.Repeat("e", 64) }},
		{"endpoint", func(p *OwnerPublication) { p.Endpoint.Path = "relative" }},
		{"foreign-root-endpoint", func(p *OwnerPublication) { p.Endpoint.Path = filepath.Join(t.TempDir(), ".i") }},
		{"endpoint-directory-device", func(p *OwnerPublication) { p.Endpoint.DirectoryDev++ }},
		{"endpoint-directory-inode", func(p *OwnerPublication) { p.Endpoint.DirectoryInode++ }},
		{"endpoint-inode", func(p *OwnerPublication) { p.Endpoint.SocketInode = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent, assumption, publication := ownerRecordFixture(t)
			test.mutate(&publication)
			if err := publication.Validate(intent, assumption); !errors.Is(err, ErrIdentity) {
				t.Fatalf("invalid publication accepted: %v", err)
			}
		})
	}
}

func TestStartupAssumptionRefusesExpiredOrInvalidChild(t *testing.T) {
	intent, assumption, _ := ownerRecordFixture(t)
	assumption.AssumedAt = intent.Deadline
	if err := assumption.Validate(intent); !errors.Is(err, ErrIdentity) {
		t.Fatalf("expired intent assumed: %v", err)
	}
	assumption.AssumedAt = "2026-11-03T00:00:01Z"
	assumption.Process.PID = 0
	if err := assumption.Validate(intent); !errors.Is(err, ErrIdentity) {
		t.Fatalf("invalid child assumed: %v", err)
	}
}

func TestStartupReferenceRetainsBootAndDeadline(t *testing.T) {
	intent := startupIntentFixture(t)
	ref, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, changeBoot := range []bool{false, true} {
		changed := intent
		if changeBoot {
			changed.Inputs.BootID = "linux:11111111-2222-3333-4444-666666666666"
			changed.InputsSHA256, err = changed.Inputs.Digest()
			if err != nil {
				t.Fatal(err)
			}
		} else {
			changed.Deadline = "2026-11-03T00:01:00Z"
		}
		if err = ref.validateIntent(changed); !errors.Is(err, ErrIdentity) {
			t.Fatalf("changed original attempt accepted: %v", err)
		}
	}
}
