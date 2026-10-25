package workspaces

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestOperationGitValidationReadsCapturedScope(t *testing.T) {
	t.Setenv("GIT_DIR", "/ambient/redirected.git")
	tests := []struct {
		name        string
		environment []string
		want        error
	}{
		{name: "uncaptured", environment: nil, want: task.ErrEvidenceFault},
		{name: "clean scope ignores ambient", environment: []string{"PATH=/usr/bin"}, want: nil},
		{name: "explicit empty scope", environment: []string{}, want: nil},
		{name: "redirected scope", environment: []string{"PATH=/usr/bin", "GIT_WORK_TREE=/selected/tree"}, want: workspace.ErrInvalidGitContext},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (OperationOptions{Environment: test.environment}).ValidateGitEnvironment()
			if test.want == nil && err != nil || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("ValidateGitEnvironment = %v, want %v", err, test.want)
			}
		})
	}
}
