package remote

import (
	"context"
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func unlimitedReference(t *testing.T) (*Reference, Intent) {
	t.Helper()
	_, store := testReference(t)
	intent := testIntent()
	intent.TaskID = "ffffffffffffffffffffffffffffffff"
	intent.Config.Budget = "unlimited"
	ref, err := OpenReference(t.Context(), store, intent.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := ref.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = ref.SaveIntent(t.Context(), intent, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	saved, err := ref.Intent()
	if err != nil {
		t.Fatal(err)
	}
	return ref, saved
}

func TestUnlimitedRemoteRequiresCapabilityBeforeAdmission(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		for _, supported := range []bool{false, true} {
			t.Run(testBudgetCase(prepared, supported), func(t *testing.T) {
				checkUnlimitedRemoteAdmission(t, prepared, supported)
			})
		}
	}
}

func testBudgetCase(prepared, supported bool) string {
	name := "direct"
	if prepared {
		name = "prepared"
	}
	if supported {
		return name + "/supported"
	}
	return name + "/legacy"
}

func checkUnlimitedRemoteAdmission(t *testing.T, prepared, supported bool) {
	t.Helper()
	ref, intent := unlimitedReference(t)
	hello, admit := 0, 0
	coordinator := Coordinator{Exchange: exchangeFunc(func(_ context.Context, request Request) (Response, error) {
		response := responseFor(request)
		switch request.Operation {
		case Hello:
			hello++
			if supported {
				response.Capabilities = []string{task.ExecutionBudgetFeature}
			}
		case Admit:
			admit++
			if request.Intent.Config.Budget != "unlimited" {
				t.Error("unlimited budget lost")
			}
		case Prepare, Reconcile, Status, Collect, Cancel, Events, Reply, Steer, StageBegin, StageChunk, StageCommit, Download, Usage, Retire:
			t.Fatalf("unexpected operation: %s", request.Operation)
		}
		return response, nil
	})}
	var err error
	if prepared {
		coordinator, err = coordinator.PrepareDispatch(t.Context(), ref)
	}
	if err == nil {
		_, err = coordinator.Dispatch(t.Context(), ref)
	}
	if supported {
		if err != nil || hello != 1 || admit != 1 {
			t.Fatalf("err=%v hello=%d admit=%d", err, hello, admit)
		}
	} else {
		if !errors.Is(err, ErrCapabilityUnsupported) || admit != 0 {
			t.Fatalf("err=%v admit=%d", err, admit)
		}
		requireSilenceUnsent(t, ref, intent)
	}
}

func TestUnlimitedRemoteRecordedSendReconcilesWithoutCapability(t *testing.T) {
	ref, intent := unlimitedReference(t)
	digest, err := IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ref.beginSend(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	coordinator := Coordinator{Exchange: exchangeFunc(func(_ context.Context, request Request) (Response, error) {
		if request.Operation != Reconcile {
			t.Fatalf("unexpected operation: %s", request.Operation)
		}
		return responseFor(request), nil
	})}
	if _, err := coordinator.Dispatch(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareUnlimitedRequiresAdvertisedSupport(t *testing.T) {
	config := task.TaskConfig{Budget: "unlimited"}
	if err := PrepareExecutionBudgetConfig(config, nil); !errors.Is(err, ErrCapabilityUnsupported) {
		t.Fatal(err)
	}
	if err := PrepareExecutionBudgetConfig(config, []string{task.ExecutionBudgetFeature}); err != nil {
		t.Fatal(err)
	}
	if err := PrepareExecutionBudgetConfig(task.TaskConfig{Budget: "1m"}, nil); err != nil {
		t.Fatal(err)
	}
}
