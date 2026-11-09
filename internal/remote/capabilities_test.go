package remote

import (
	"context"
	"errors"
	"testing"
)

func TestPassiveCapabilityRequiresVerifiedPeerAndAllowsStoppedResident(t *testing.T) {
	for _, tc := range []struct {
		name       string
		capability bool
		foreign    bool
		want       error
	}{
		{name: "supported with stopped resident", capability: true},
		{name: "legacy peer", want: ErrCapabilityUnsupported},
		{name: "foreign peer", capability: true, foreign: true, want: ErrIdentity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			coordinator := Coordinator{Exchange: exchangeFunc(func(_ context.Context, request Request) (Response, error) {
				calls++
				if request.Operation != Hello {
					t.Fatalf("capability observation sent %s", request.Operation)
				}
				response := responseFor(request)
				response.Failure = &Failure{Code: "resident_not_ready", Message: "stopped"}
				if tc.capability {
					response.Capabilities = []string{LogsCapability}
				}
				if tc.foreign {
					response.Identity.RootID = "dddddddddddddddddddddddddddddddd"
				}
				return response, nil
			})}
			err := coordinator.RequireCapability(t.Context(), testIntent().Destination, LogsCapability)
			if !errors.Is(err, tc.want) || calls != 1 {
				t.Fatalf("capability error=%v calls=%d, want %v and one Hello", err, calls, tc.want)
			}
		})
	}
}
