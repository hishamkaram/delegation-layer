package remote

import (
	"context"
	"errors"
	"testing"
)

func silenceReference(t *testing.T, timeout string) (*Reference, Intent) {
	t.Helper()
	_, store := testReference(t)
	// testReference already records its baseline intent; use a separate ID.
	intent := testIntent()
	intent.TaskID = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	intent.Config.OutputSilenceTimeout = timeout
	candidate, err := OpenReference(t.Context(), store, intent.TaskID, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := candidate.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = candidate.SaveIntent(t.Context(), intent, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	saved, err := candidate.Intent()
	if err != nil {
		t.Fatal(err)
	}
	return candidate, saved
}

func TestRemoteSilenceRequiresCapabilityBeforeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                string
		prepared, supported bool
	}{
		{"direct-supported", false, true}, {"direct-legacy", false, false}, {"prepared-supported", true, true}, {"prepared-legacy", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) { checkRemoteSilenceAdmission(t, tc.prepared, tc.supported) })
	}
}

func checkRemoteSilenceAdmission(t *testing.T, prepared, supported bool) {
	t.Helper()
	ref, intent := silenceReference(t, "250ms")
	hello, admit := 0, 0
	c := Coordinator{Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) {
		response := responseFor(r)
		switch r.Operation {
		case Hello:
			hello++
			if supported {
				response.Capabilities = []string{OutputSilenceCapability}
			}
		case Admit:
			admit++
			if r.Intent.Config.OutputSilenceTimeout != "250ms" {
				t.Error("subsecond timeout lost")
			}
		case Prepare, Reconcile, Status, Collect, Cancel, Events, Reply, Steer, StageBegin, StageChunk, StageCommit, Download, Usage, Retire:
			t.Fatalf("unexpected remote operation: %s", r.Operation)
		}
		return response, nil
	})}
	var err error
	if prepared {
		c, err = c.PrepareDispatch(t.Context(), ref)
	}
	if err == nil {
		_, err = c.Dispatch(t.Context(), ref)
	}
	if supported {
		if err != nil || hello != 1 || admit != 1 {
			t.Fatalf("err=%v hello=%d admit=%d", err, hello, admit)
		}
		return
	}
	if !errors.Is(err, ErrCapabilityUnsupported) || admit != 0 {
		t.Fatalf("err=%v admit=%d", err, admit)
	}
	requireSilenceUnsent(t, ref, intent)
}

func requireSilenceUnsent(t *testing.T, ref *Reference, intent Intent) {
	t.Helper()
	digest, err := IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := ref.sendRecorded(digest)
	if err != nil || sent {
		t.Fatalf("unsupported sent=%t err=%v", sent, err)
	}
}

func TestRemoteSilenceDisableNormalizedBeforeIntentFreeze(t *testing.T) {
	ref, intent := silenceReference(t, "0s")
	if intent.Config.OutputSilenceTimeout != "" {
		t.Fatalf("off leaked unknown field: %+v", intent.Config)
	}
	c := Coordinator{Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) {
		if r.Operation != Admit {
			t.Errorf("disabled policy requested capability")
		}
		return responseFor(r), nil
	})}
	if _, err := c.Dispatch(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteSilenceRecordedSendUsesExistingOnlyWithoutCapability(t *testing.T) {
	ref, intent := silenceReference(t, "250ms")
	digest, err := IntentDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ref.beginSend(t.Context(), digest); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := Coordinator{Exchange: exchangeFunc(func(_ context.Context, r Request) (Response, error) {
		calls++
		if r.Operation != Reconcile {
			t.Errorf("recorded send got %s", r.Operation)
		}
		return responseFor(r), nil
	})}
	if _, err = c.Dispatch(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	saved, err := ref.Intent()
	if err != nil {
		t.Fatal(err)
	}
	after, err := IntentDigest(saved)
	if err != nil || after != digest || calls != 1 {
		t.Fatalf("digest changed or wrong calls: %v %d", err, calls)
	}
}
