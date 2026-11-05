package app

import (
	"errors"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
)

func TestInfrastructureEntryHostRejectsUnrepresentableEndpointBeforeCustody(t *testing.T) {
	limit := 107
	switch runtime.GOOS {
	case "linux":
	case "darwin":
		limit = 103
	default:
		t.Fatalf("native endpoint bound is unverified on %s", runtime.GOOS)
	}
	image := &debug.BuildInfo{Path: "github.com/hishamkaram/delegation-layer/cmd/delegate-run"}
	for _, fixture := range []struct {
		name      string
		rootBytes int
		tooLong   bool
	}{
		{"exact native bound", limit - 14, false},
		{"one byte beyond native bound", limit - 13, true},
		{"far beyond native bound", 109, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root := "/" + strings.Repeat("r", fixture.rootBytes-1)
			// The independently specified endpoint budget is root + slash + 13
			// sequence characters. Empty artifacts distinguish custody from preflight.
			err := (infrastructureEntryHost{}).ValidateArtifacts(t.Context(), root, infrastructure.Artifacts{}, image)
			if !errors.Is(err, pueue.ErrConfiguration) {
				t.Fatalf("expected configuration refusal: %v", err)
			}
			if fixture.tooLong {
				if !strings.Contains(err.Error(), "unix socket path too long") {
					t.Fatalf("unrepresentable endpoint reached artifact custody: %v", err)
				}
			} else if !strings.Contains(err.Error(), "invalid infrastructure runtime custody identity") {
				t.Fatalf("representable endpoint did not reach custody: %v", err)
			}
		})
	}
}

func TestInfrastructureEntryHostRejectsUnmanagedObservedBuild(t *testing.T) {
	host := infrastructureEntryHost{}
	for _, image := range []*debug.BuildInfo{nil, {Path: "unrelated/main"}} {
		if err := host.ValidateArtifacts(t.Context(), "", infrastructure.Artifacts{}, image); !errors.Is(err, infrastructure.ErrIdentity) {
			t.Fatalf("unmanaged observed build reached custody validation: %v", err)
		}
	}
}

func TestInfrastructureEntryHostRejectsZeroOperation(t *testing.T) {
	host := infrastructureEntryHost{}
	if _, err := host.EnsureOwnRoot(t.Context(), infrastructure.Operation{}, infrastructure.LaunchContext{}, nil, nil); !errors.Is(err, infrastructure.ErrIdentity) {
		t.Fatalf("zero operation reached canonical preparation: %v", err)
	}
	if _, err := host.StageTargetArtifacts(t.Context(), infrastructure.Operation{}, infrastructure.ArtifactRequirements{
		RequiredRuntimeSHA256: strings.Repeat("a", 64),
		RequiredClientSHA256:  strings.Repeat("b", 64),
		RequiredDaemonSHA256:  strings.Repeat("c", 64),
	}, nil); !errors.Is(err, infrastructure.ErrIdentity) {
		t.Fatalf("zero operation reached artifact staging: %v", err)
	}
}
