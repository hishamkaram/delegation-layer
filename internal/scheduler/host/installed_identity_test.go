package host

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func installedHostEpochFixture() epochRecord {
	return epochRecord{
		Protocol: ProtocolVersion, RootID: strings.Repeat("a", 32), Sequence: 1, Epoch: strings.Repeat("b", 32),
		Host: Identity{PID: 42, StartToken: "historical-process"}, UID: os.Geteuid(), Endpoint: "/fixture/s",
	}
}

func TestHostFreshEpochDoesNotRequireExecutableBuildIdentity(t *testing.T) {
	epoch := installedHostEpochFixture()
	if err := epoch.validate(epoch.RootID); err != nil {
		t.Fatalf("fresh installed host requires executable digest: %v", err)
	}
	data, err := task.MarshalCanonical(epoch)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"build_id"`)) {
		t.Fatalf("fresh epoch emitted executable digest field: %s", data)
	}
	var decoded epochRecord
	if err = task.DecodeStrict(data, &decoded); err != nil || decoded != epoch {
		t.Fatalf("fresh epoch did not retain exact subject: %v", err)
	}
}

func TestHostHistoricalBuildIdentityRemainsCanonicalEchoData(t *testing.T) {
	epoch := installedHostEpochFixture()
	epoch.BuildID = strings.Repeat("c", 64)
	want := fmt.Sprintf(`{"protocol":1,"root_id":"%s","sequence":1,"epoch":"%s","build_id":"%s","host":{"pid":42,"start_token":"historical-process"},"uid":%d,"endpoint":"/fixture/s"}`+"\n", epoch.RootID, epoch.Epoch, epoch.BuildID, os.Geteuid())
	data, err := task.MarshalCanonical(epoch)
	if err != nil || string(data) != want {
		t.Fatalf("legacy canonical record changed: %s, %v", data, err)
	}
	var decoded epochRecord
	if err = task.DecodeStrict([]byte(want), &decoded); err != nil || decoded != epoch {
		t.Fatalf("legacy record became unreadable: %v", err)
	}
	status := statusWire(Status{Epoch: epoch.Epoch, Protocol: epoch.Protocol, BuildID: epoch.BuildID, Host: epoch.Host})
	if err = validateResponse(status, epoch); err != nil {
		t.Fatal(err)
	}
	status.BuildID += "-replacement"
	if err = validateResponse(status, epoch); !errors.Is(err, ErrIdentity) {
		t.Fatalf("historical response echo lost its exact subject: %v", err)
	}
}

func TestHostChildProtocolUsesRootEpochAndInheritedChannelInsteadOfBuildDigest(t *testing.T) {
	config := ChildConfig{RootID: strings.Repeat("a", 32), ReadFD: 3, WriteFD: 4}
	if err := validateChildConfig(config); err != nil {
		t.Fatalf("fresh child requires executable digest: %v", err)
	}
	hello := childFrame{Kind: "hello", Protocol: ProtocolVersion, RootID: config.RootID, Epoch: strings.Repeat("b", 32), Attempt: 1}
	encoded, err := task.MarshalCanonical(hello)
	if err != nil || bytes.Contains(encoded, []byte(`"build_id"`)) {
		t.Fatalf("fresh child frame emitted a program digest: %s, %v", encoded, err)
	}
	for _, historical := range []string{"", strings.Repeat("c", 64)} {
		hello.BuildID = historical
		if err := validateHello(hello, config); err != nil {
			t.Fatalf("protocol-compatible historical data required program fingerprint: %v", err)
		}
	}
	for _, field := range []string{"protocol", "root", "epoch", "attempt"} {
		invalid := hello
		switch field {
		case "protocol":
			invalid.Protocol++
		case "root":
			invalid.RootID = strings.Repeat("d", 32)
		case "epoch":
			invalid.Epoch = ""
		case "attempt":
			invalid.Attempt = 0
		}
		if err := validateHello(invalid, config); !errors.Is(err, ErrIdentity) {
			t.Fatalf("invalid %s admitted: %v", field, err)
		}
	}
}

func TestHostCompatibilityUsesNativeProtocolWithoutExecutableDigest(t *testing.T) {
	status := Status{Protocol: ProtocolVersion, BuildID: strings.Repeat("a", 64)}
	if err := CheckCompatible(status); err != nil {
		t.Fatalf("historical build data blocked compatible protocol: %v", err)
	}
	status.Protocol++
	if err := CheckCompatible(status); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("unsupported protocol accepted: %v", err)
	}
}
