package supervisor

import (
	"errors"
	"fmt"
	"testing"

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
