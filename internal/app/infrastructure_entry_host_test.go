package app

import (
	"errors"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
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
	runner := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, runner, "#!/bin/sh\nexit 0\n")
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
			// The independent native endpoint budget is root + slash + 13
			// sequence characters. The installed runner remains outside this root.
			err := (infrastructureEntryHost{}).ValidateRunner(t.Context(), root, runner, image)
			if fixture.tooLong {
				if !errors.Is(err, pueue.ErrConfiguration) || !strings.Contains(err.Error(), "unix socket path too long") {
					t.Fatalf("unrepresentable endpoint reached runner validation: %v", err)
				}
			} else if err != nil {
				t.Fatalf("representable endpoint refused installed runner: %v", err)
			}
		})
	}
}

func TestInfrastructureEntryHostRejectsUnmanagedObservedBuild(t *testing.T) {
	host := infrastructureEntryHost{}
	for _, image := range []*debug.BuildInfo{nil, {Path: "unrelated/main"}} {
		if err := host.ValidateRunner(t.Context(), "", "", image); !errors.Is(err, infrastructure.ErrIdentity) {
			t.Fatalf("unmanaged observed build reached custody validation: %v", err)
		}
	}
}

func TestInfrastructureEntryHostRejectsZeroOperation(t *testing.T) {
	host := infrastructureEntryHost{}
	if _, err := host.EnsureOwnRoot(t.Context(), infrastructure.Operation{}, infrastructure.LaunchContext{}, nil, nil); !errors.Is(err, infrastructure.ErrIdentity) {
		t.Fatalf("zero operation reached canonical preparation: %v", err)
	}
}
