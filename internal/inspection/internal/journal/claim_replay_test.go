package journal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type inspectionGuardClaim struct {
	name    string
	claim   func(context.Context, time.Time) (bool, error)
	already error
}

func inspectionGuardClaims(op *Operation) []inspectionGuardClaim {
	return []inspectionGuardClaim{
		{name: submissionRecordName, already: task.ErrAlreadySubmitted, claim: func(ctx context.Context, now time.Time) (bool, error) {
			permit, err := op.ClaimSubmissionContext(ctx, now)
			return permit != nil, err
		}},
		{name: startRecordName, already: task.ErrAlreadyStarted, claim: func(ctx context.Context, now time.Time) (bool, error) {
			permit, err := op.ClaimStartContext(ctx, now)
			return permit != nil, err
		}},
	}
}

func terminateInspectionForClaimTest(t *testing.T, op *Operation) {
	t.Helper()
	recordInspectionLifetimeForTest(t, op, inspectionLifetimeRegistrationFacts(op))
	termination, err := op.RecordLifetimeTermination(task.LifetimeTerminationCompleted)
	mustInspection(t, err)
	if !termination.ScopeEmpty {
		t.Fatal("inspection fixture did not publish empty-scope termination")
	}
}

func TestInspectionClosedClaimReplayRemainsReadbackOnly(t *testing.T) {
	for claimIndex := range 2 {
		store, op := openInspectionLifetimeTest(t)
		claim := inspectionGuardClaims(op)[claimIndex]
		t.Run(claim.name, func(t *testing.T) {
			now := time.Now().UTC()
			permit, err := claim.claim(t.Context(), now)
			mustInspection(t, err)
			if !permit {
				t.Fatal("initial claim did not yield its one permit")
			}
			path := filepath.Join(store.Root, "inspections", op.Request().TaskID, claim.name)
			before, err := os.ReadFile(path)
			mustInspection(t, err)
			terminateInspectionForClaimTest(t, op)

			if permit, err = claim.claim(t.Context(), now.Add(time.Nanosecond)); permit || !errors.Is(err, claim.already) {
				t.Fatalf("closed guard replay = permit %t, error %v; want readback-only %v", permit, err, claim.already)
			}
			if permit, err = claim.claim(t.Context(), op.Deadline()); permit || !errors.Is(err, ErrAdmissionExpired) {
				t.Fatalf("expired replay = permit %t, error %v; want admission expiry", permit, err)
			}
			after, err := os.ReadFile(path)
			mustInspection(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("readback changed the immutable guard")
			}
		})
	}
}

func TestInspectionClosedLifetimeRefusesMissingClaim(t *testing.T) {
	for claimIndex := range 2 {
		store, op := openInspectionLifetimeTest(t)
		claim := inspectionGuardClaims(op)[claimIndex]
		t.Run(claim.name, func(t *testing.T) {
			terminateInspectionForClaimTest(t, op)
			permit, err := claim.claim(t.Context(), time.Now().UTC())
			if permit || !errors.Is(err, task.ErrTerminalTask) {
				t.Fatalf("new closed-lifetime claim = permit %t, error %v; want terminal refusal", permit, err)
			}
			path := filepath.Join(store.Root, "inspections", op.Request().TaskID, claim.name)
			if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("closed lifetime published a new guard: %v", statErr)
			}
		})
	}
}

func TestInspectionClosedClaimReplayRejectsInvalidGuard(t *testing.T) {
	for claimIndex := range 2 {
		for _, invalid := range []string{"malformed", "wrong digest"} {
			store, op := openInspectionLifetimeTest(t)
			claim := inspectionGuardClaims(op)[claimIndex]
			t.Run(claim.name+"/"+invalid, func(t *testing.T) {
				now := time.Now().UTC()
				_, err := claim.claim(t.Context(), now)
				mustInspection(t, err)
				path := filepath.Join(store.Root, "inspections", op.Request().TaskID, claim.name)
				before, err := os.ReadFile(path)
				mustInspection(t, err)
				terminateInspectionForClaimTest(t, op)
				corrupt := []byte("not-json")
				if invalid == "wrong digest" {
					corrupt = bytes.Replace(before, []byte(op.Digest()), []byte(strings.Repeat("b", 64)), 1)
				}
				mustInspection(t, os.WriteFile(path, corrupt, 0o600))
				permit, err := claim.claim(t.Context(), now)
				if permit || !errors.Is(err, task.ErrEvidenceFault) || errors.Is(err, claim.already) {
					t.Fatalf("invalid closed guard = permit %t, error %v; want evidence fault", permit, err)
				}
				after, err := os.ReadFile(path)
				mustInspection(t, err)
				if !bytes.Equal(corrupt, after) {
					t.Fatal("rejected replay rewrote invalid guard")
				}
			})
		}
	}
}

func TestInspectionClosedClaimReplayHonorsCallerContext(t *testing.T) {
	for claimIndex := range 2 {
		_, op := openInspectionLifetimeTest(t)
		claim := inspectionGuardClaims(op)[claimIndex]
		t.Run(claim.name, func(t *testing.T) {
			now := time.Now().UTC()
			_, err := claim.claim(t.Context(), now)
			mustInspection(t, err)
			terminateInspectionForClaimTest(t, op)
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			expired, expire := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			defer expire()
			for _, caller := range []struct {
				ctx  context.Context
				want error
			}{{canceled, context.Canceled}, {expired, context.DeadlineExceeded}} {
				permit, err := claim.claim(caller.ctx, now)
				if permit || !errors.Is(err, caller.want) || errors.Is(err, claim.already) {
					t.Fatalf("closed guard context = permit %t, error %v; want %v", permit, err, caller.want)
				}
			}
		})
	}
}
