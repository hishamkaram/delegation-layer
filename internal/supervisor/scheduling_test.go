package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSchedulingReceiptVersionsPreserveAuthorization(t *testing.T) {
	binding := generationBinding(t.TempDir(), "active")
	encoded, err := task.MarshalCanonical(binding)
	if err != nil {
		t.Fatal(err)
	}
	current := fmt.Sprintf(`{"schema_version":1,"revision":"unlimited-v4","supervisor":%s,"staged_launch":{"add_stashed":true,"enqueue_by_id":true}}`, encoded)
	tests := []struct {
		name     string
		receipt  string
		binding  task.SupervisorRef
		admitted bool
		fault    bool
	}{
		{"v1_requires_repair", fmt.Sprintf(`{"schema_version":1,"revision":"unlimited-v1","config_digest":%q}`, binding.ConfigDigest), binding, false, false},
		{"v2_requires_repair", fmt.Sprintf(`{"schema_version":1,"revision":"unlimited-v2","supervisor":{"executable":%q,"sha256":%q}}`, binding.DaemonExecutable, binding.DaemonSHA256), binding, false, false},
		{"v3_requires_capability", fmt.Sprintf(`{"schema_version":1,"revision":"unlimited-v3","supervisor":%s}`, encoded), binding, false, false},
		{"v4_exact_generation", current, binding, true, false},
		{"v4_changed_generation", current, generationBinding(t.TempDir(), "other"), false, false},
		{"v4_missing_enqueue", fmt.Sprintf(`{"schema_version":1,"revision":"unlimited-v4","supervisor":%s,"staged_launch":{"add_stashed":true,"enqueue_by_id":false}}`, encoded), binding, false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorization, recorded, err := decodePrivateOrdinaryScheduling([]byte(test.receipt), test.binding)
			if errors.Is(err, task.ErrEvidenceFault) != test.fault || (!test.fault && err != nil) {
				t.Fatalf("receipt error=%v, want evidence fault=%t", err, test.fault)
			}
			if recorded != test.admitted || (authorization.StagedLaunchCapability != nil) != test.admitted {
				t.Fatalf("receipt authority changed: recorded=%t authorization=%+v", recorded, authorization)
			}
			if test.admitted && (!authorization.StagedLaunchCapability.AddStashed || !authorization.StagedLaunchCapability.EnqueueByID) {
				t.Fatalf("staged capability changed: %+v", authorization.StagedLaunchCapability)
			}
		})
	}
}

func TestSchedulingReceiptDistinguishesInstalledAndLegacyIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*task.SupervisorRef)
		valid  bool
	}{
		{"installed without executable digests", func(ref *task.SupervisorRef) {}, true},
		{"installed with client digest", func(ref *task.SupervisorRef) { ref.ClientSHA256 = strings.Repeat("a", 64) }, false},
		{"installed with daemon digest", func(ref *task.SupervisorRef) { ref.DaemonSHA256 = strings.Repeat("b", 64) }, false},
		{"installed without daemon path", func(ref *task.SupervisorRef) { ref.DaemonExecutable = "" }, false},
		{"installed with relative daemon path", func(ref *task.SupervisorRef) { ref.DaemonExecutable = "pueued" }, false},
		{"unknown identity", func(ref *task.SupervisorRef) { ref.ExecutableIdentity = "unknown" }, false},
		{"legacy complete", legacySchedulingReceiptBinding, true},
		{"legacy without daemon digest", func(ref *task.SupervisorRef) { legacySchedulingReceiptBinding(ref); ref.DaemonSHA256 = "" }, false},
		{"legacy malformed daemon digest", func(ref *task.SupervisorRef) { legacySchedulingReceiptBinding(ref); ref.DaemonSHA256 = "invalid" }, false},
		{"legacy without client digest", func(ref *task.SupervisorRef) { legacySchedulingReceiptBinding(ref); ref.ClientSHA256 = "" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			testSchedulingReceiptIdentity(t, test.mutate, test.valid)
		})
	}
}

func testSchedulingReceiptIdentity(t *testing.T, mutate func(*task.SupervisorRef), valid bool) {
	t.Helper()
	store := newGenerationStore(t)
	if err := os.Mkdir(filepath.Dir(pueue.PrivateConfigPath(store.Root)), 0o700); err != nil {
		t.Fatal(err)
	}
	binding := generationBinding(store.Root, "identity")
	binding.ExecutableIdentity = task.ExecutableIdentityInstalledPathV1
	binding.ClientSHA256, binding.DaemonSHA256 = "", ""
	mutate(&binding)
	capability := pueue.StagedLaunchCapability{AddStashed: true, EnqueueByID: true}
	err := publishPrivateOrdinaryScheduling(store.Root, binding, capability)
	if (err == nil) != valid {
		t.Fatalf("receipt publication valid=%t: %v", valid, err)
	}
	if !valid {
		_, present, readErr := pueue.ReadPrivateSchedulingEvidence(store.Root)
		if readErr != nil || present {
			t.Fatalf("invalid identity published scheduling authority: present=%t err=%v", present, readErr)
		}
		return
	}
	authorization, recorded, readErr := privateOrdinarySchedulingAuthorization(store.Root, binding)
	if readErr != nil || !recorded || authorization.StagedLaunchCapability == nil || *authorization.StagedLaunchCapability != capability {
		t.Fatalf("exact generation receipt lost authority: recorded=%t authorization=%+v err=%v", recorded, authorization, readErr)
	}
}

// Frozen legacy fields are record data; no executable is opened or hashed.
func legacySchedulingReceiptBinding(ref *task.SupervisorRef) {
	ref.ExecutableIdentity = ""
	ref.ClientSHA256, ref.DaemonSHA256 = strings.Repeat("a", 64), strings.Repeat("b", 64)
}
