package task

import (
	"bytes"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

func TestNativeTimeoutNormalizesAndBindsRequest(t *testing.T) {
	request := nativeTimeoutRequest("3000ms")
	if err := NormalizeRequestedConfig(&request); err != nil {
		t.Fatal(err)
	}
	if request.RequestedConfig.NativeTimeout != "3s" || request.BudgetNanos != int64(2*time.Minute) {
		t.Fatalf("native timeout changed the outer budget: %+v", request)
	}
	other := request
	other.RequestedConfig.NativeTimeout = "4s"
	if CompareRequests(&request, &other) {
		t.Fatal("request identity ignored native timeout")
	}
	other = request
	other.RequestedConfig.NativeTimeout = "3000ms"
	if err := validateTaskConfiguration(&other); err == nil {
		t.Fatal("saved noncanonical timeout accepted")
	}
	request.RequestedConfig.NativeTimeout = ""
	encoded, err := MarshalCanonical(request.RequestedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("native_timeout")) {
		t.Fatal("omitted native timeout changed existing request encoding")
	}
}

func TestNativeTimeoutRejectsInvalidButKeepsProviderSupportStructural(t *testing.T) {
	for _, value := range []string{"0s", "-1s", "121s", "NaN", "infinity", "999999999999999999999999h"} {
		t.Run(value, func(t *testing.T) {
			request := nativeTimeoutRequest(value)
			before := request
			if err := NormalizeRequestedConfig(&request); err == nil {
				t.Fatal("invalid native timeout accepted")
			}
			if !CompareRequests(&request, &before) {
				t.Fatal("failed normalization mutated request")
			}
		})
	}
	for _, provider := range []string{config.ProviderCodexExec, config.ProviderClaudePrint, "fixture:test"} {
		request := nativeTimeoutRequest("3s")
		request.Provider = provider
		if err := NormalizeRequestedConfig(&request); err != nil {
			t.Errorf("structural normalization rejected %s: %v", provider, err)
		}
	}
}

func nativeTimeoutRequest(value string) TaskRecord {
	return TaskRecord{Provider: config.ProviderAntigravityPrint, Mode: config.ModeWorkspaceWrite, CanonicalCwd: "/workspace", RequestedConfig: TaskConfig{Permission: config.ModeWorkspaceWrite, Budget: "2m", NativeTimeout: value}}
}
