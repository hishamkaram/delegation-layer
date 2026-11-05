package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareAdmissionExecutablesHonorsCallerBeforeResolution(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer expire()
	for _, test := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "nil", want: context.Canceled},
		{name: "canceled", ctx: canceled, want: context.Canceled},
		{name: "expired", ctx: expired, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "uncreated-state")
			// The nonexistent runner would produce a resolution error if the
			// canceled caller were replaced with a fresh background context.
			selection := AdmissionSelection{Runner: filepath.Join(root, "missing-runner")}
			prepared, err := PrepareAdmissionExecutablesContext(test.ctx, selection, AdmissionOptions{}, root)
			if !errors.Is(err, test.want) || prepared.Executable != "" {
				t.Fatalf("ended preparation returned %+v, %v; want %v", prepared, err, test.want)
			}
			if _, err = os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ended preparation created state: %v", err)
			}
		})
	}
}
