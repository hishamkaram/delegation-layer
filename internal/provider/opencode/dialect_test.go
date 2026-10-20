package opencode

import (
	"errors"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

func TestCandidateInspectionRefusesUnknownDialect(t *testing.T) {
	cli := commonprovider.CLIInfo{Path: "/usr/bin/opencode", SHA256: strings.Repeat("a", 64)}
	request := profileRequest(ModeWorkspaceWrite)
	if _, err := candidateInspection(cli, request, nil, dialectNativeV2); err != nil {
		t.Fatalf("known dialect rejected by the same inputs: %v", err)
	}
	for _, dialect := range []launchDialect{0, dialectPure + 1} {
		if _, err := candidateInspection(cli, request, nil, dialect); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("dialect %d error=%v want ErrUnsupportedProfile", dialect, err)
		}
	}
}
