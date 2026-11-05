package infrastructure

import (
	"context"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func unsetInvocationOrigin(t *testing.T) {
	t.Helper()
	previous, present := os.LookupEnv(OriginEnvironment)
	t.Cleanup(func() {
		var err error
		if present {
			err = os.Setenv(OriginEnvironment, previous)
		} else {
			err = os.Unsetenv(OriginEnvironment)
		}
		if err != nil {
			t.Error("restore invocation origin environment")
		}
	})
	if err := os.Unsetenv(OriginEnvironment); err != nil {
		t.Fatal("unset invocation origin environment")
	}
}

func setInvocationOrigin(t *testing.T, origin Origin) {
	t.Helper()
	environment, err := WithOrigin(nil, origin)
	if err != nil {
		t.Fatal(err)
	}
	_, value, found := strings.Cut(environment[0], "=")
	if !found {
		t.Fatal("encoded origin lacks environment value")
	}
	t.Setenv(OriginEnvironment, value)
}

func TestInvocationSourceCaptureContextPrecedesEnvironment(t *testing.T) {
	t.Setenv(OriginEnvironment, "malformed invocation locator")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "nil", want: os.ErrInvalid},
		{name: "canceled", ctx: canceled, want: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := CaptureInvocationSource(tc.ctx)
			_, found := source.Origin()
			if !errors.Is(err, tc.want) || found || source.outside.provenance != (StartupProvenance{}) {
				t.Fatalf("invalid context captured invocation: found=%t err=%v", found, err)
			}
		})
	}
}

func TestInvocationSourceOriginIsImmutableRoutingOnly(t *testing.T) {
	f := producerFixture(t)
	origin, err := NewOrigin(filepath.Join(f.journal.pin.Root(), "unopened-origin"), f.journal.pin.RootID())
	if err != nil {
		t.Fatal(err)
	}
	setInvocationOrigin(t, origin)
	// Positive containment is compatible with routing data, never outside birth.
	t.Setenv("DELEGATION_LAYER_PROCESS_SCOPE", "pg:"+strings.Repeat("a", 32))
	before := producerBudgetTree(t, f.journal.pin.Root())
	source, err := CaptureInvocationSource(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(OriginEnvironment, "later selected environment")
	observed, found := source.Origin()
	if !found || observed != origin {
		t.Fatal("source did not retain the original locator")
	}
	// Origin is a value copy; changing the returned value cannot change the source.
	observed = Origin{}
	if retained, ok := source.Origin(); !ok || retained != origin || observed == retained {
		t.Fatal("returned origin aliases source state")
	}
	attempt, err := source.EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, attempt)
	if !errors.Is(err, ErrIdentity) || attempt != nil || f.host.calls != 0 {
		t.Fatalf("origin minted outside authority: prepare=%d err=%v", f.host.calls, err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
		t.Fatal("routing capture/refusal changed target state or opened origin")
	}
}

func TestInvocationSourceMalformedOriginIsRedacted(t *testing.T) {
	const privateValue = "invocation-private-value"
	for _, raw := range []string{
		privateValue,
		`{"root":"/", "root_id":"invalid", "` + privateValue + `":"value"}`,
	} {
		t.Run("invalid locator", func(t *testing.T) {
			t.Setenv(OriginEnvironment, raw)
			source, err := CaptureInvocationSource(t.Context())
			_, found := source.Origin()
			if !errors.Is(err, ErrIdentity) || found || source.outside.provenance != (StartupProvenance{}) {
				t.Fatalf("malformed locator captured a source: found=%t err=%v", found, err)
			}
			if strings.Contains(err.Error(), privateValue) || err.Error() != "infrastructure identity mismatch: invalid invocation origin" {
				t.Fatal("malformed locator exposed environment content")
			}
		})
	}
}

func TestInvocationSourceActualScopeWithoutOriginRefusesCapture(t *testing.T) {
	unsetInvocationOrigin(t)
	t.Setenv("DELEGATION_LAYER_PROCESS_SCOPE", "pg:"+strings.Repeat("a", 32))
	source, err := CaptureInvocationSource(t.Context())
	_, found := source.Origin()
	if !errors.Is(err, lifetime.ErrTaskContained) || found || source.outside.provenance != (StartupProvenance{}) {
		t.Fatalf("actual task marker minted outside provenance: found=%t err=%v", found, err)
	}
}

func TestInvocationSourceInvalidEstablishmentLeavesTargetUntouched(t *testing.T) {
	f := producerFixture(t)
	unsetInvocationOrigin(t)
	source, err := CaptureInvocationSource(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	before := producerBudgetTree(t, f.journal.pin.Root())
	for _, tc := range []struct {
		name   string
		source InvocationSource
		ctx    context.Context
		target *taskdir.Store
		host   EstablishmentHost
		want   error
	}{
		{name: "zero", ctx: t.Context(), target: f.journal.store, host: f.host, want: os.ErrInvalid},
		{name: "nil context", source: source, target: f.journal.store, host: f.host, want: os.ErrInvalid},
		{name: "canceled", source: source, ctx: canceled, target: f.journal.store, host: f.host, want: context.Canceled},
		{name: "nil target", source: source, ctx: t.Context(), host: f.host, want: os.ErrInvalid},
		{name: "nil host", source: source, ctx: t.Context(), target: f.journal.store, want: os.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt, err := tc.source.EstablishOutside(tc.ctx, tc.target, f.request, tc.host)
			ownProducerAttempt(t, attempt)
			if !errors.Is(err, tc.want) || attempt != nil || f.host.calls != 0 {
				t.Fatalf("invalid source reached establishment: prepare=%d err=%v", f.host.calls, err)
			}
		})
	}
	if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
		t.Fatal("invalid invocation source changed target state")
	}
	if _, found := (InvocationSource{}).Origin(); found {
		t.Fatal("zero source invented an origin")
	}
}

// This component uses the existing fixed-argv test-image adapter only at
// Start/Wait. Public capture still observes actual environment/native placement.
func TestInvocationSourceNativeBirthRetainsEntrySnapshotAndSingleOwner(t *testing.T) {
	unsetInvocationOrigin(t)
	t.Setenv("INVOCATION_SOURCE_FIXTURE", "captured")
	f := newProducerNativeFixture(t)
	source, err := CaptureInvocationSource(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, found := source.Origin(); found {
		t.Fatal("outside capture invented origin routing")
	}
	t.Setenv("INVOCATION_SOURCE_FIXTURE", "rebased")
	t.Setenv(OriginEnvironment, "selected launch context")
	t.Setenv("DELEGATION_LAYER_PROCESS_SCOPE", "pg:"+strings.Repeat("a", 32))
	attempt, err := source.establishOutside(t.Context(), f.store, f.request, f.host, f.native(t, nil))
	if attempt != nil {
		f.attempts = append(f.attempts, attempt)
	}
	if err != nil || attempt == nil || attempt.Reference().Sequence != 1 || attempt.NativeCompletion() == nil {
		t.Fatalf("captured outside birth lost acceptance/Wait: %v", err)
	}
	f.ready(t, attempt)
	if !slices.Equal(f.command.Env, f.raw) || !slices.Contains(f.command.Env, "INVOCATION_SOURCE_FIXTURE=captured") {
		t.Fatal("native birth used later environment or selected launch overlay")
	}
	assertInvocationSourceLiveJoin(t, f, source, attempt)
}

func assertInvocationSourceLiveJoin(t *testing.T, f *producerNativeFixture, source InvocationSource, attempt *StartupAttempt) {
	t.Helper()
	if err := attempt.Close(); err != nil {
		t.Fatal(err)
	}
	joined, joinErr := source.EstablishOutside(t.Context(), f.store, f.request, f.host)
	if joined != nil {
		f.attempts = append(f.attempts, joined)
	}
	var contact *OwnerContactRequiredError
	if !errors.As(joinErr, &contact) || joined != nil || contact.Startup != attempt.Reference() || f.host.calls != 1 {
		t.Fatalf("same source failed to join sole resident: prepare=%d err=%v", f.host.calls, joinErr)
	}
	select {
	case <-attempt.NativeCompletion():
		t.Fatal("closing source attempt stopped the resident")
	default:
	}
}

func TestInvocationSourceUsesCanonicalTargetPin(t *testing.T) {
	unsetInvocationOrigin(t)
	f := producerFixture(t)
	releaseProducerFixtureLease(t, f)
	source, err := CaptureInvocationSource(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	originalID := f.journal.store.RootID
	f.host.prepare = func(context.Context, *taskdir.Store) error {
		f.journal.store.RootID = "replaced-target"
		return nil
	}
	t.Cleanup(func() { f.journal.store.RootID = originalID })
	attempt, err := source.EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, attempt)
	f.journal.store.RootID = originalID
	if err == nil || attempt != nil || f.host.calls != 1 {
		t.Fatalf("target changed during preparation reached birth: prepare=%d err=%v", f.host.calls, err)
	}
	if _, readErr := f.journal.readIntent(t.Context(), 1); !task.RecordAbsent(readErr) {
		t.Fatalf("target pin failure admitted startup or lost readback: %v", readErr)
	}
}

func TestInvocationSourceStartFailureRetainsAcceptanceAndBorrowedStore(t *testing.T) {
	unsetInvocationOrigin(t)
	f := producerFixture(t)
	releaseProducerFixtureLease(t, f)
	f.request.Initial = InitialOperation{Kind: InitialMigrateOwn}
	source, err := CaptureInvocationSource(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("fixture refuses native Start")
	starts := 0
	attempt, err := source.establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{
		start: func(*exec.Cmd) error { starts++; return refusal },
	})
	ownProducerAttempt(t, attempt)
	if !errors.Is(err, refusal) || attempt == nil || attempt.Reference().Sequence != 1 || attempt.NativeCompletion() != nil {
		t.Fatalf("failed Start lost accepted attempt: %v", err)
	}
	intent, err := f.journal.readIntent(t.Context(), 1)
	if err != nil || intent.Inputs.Initial != (InitialOperation{Kind: InitialMigrateOwn}) {
		t.Fatalf("source changed accepted purpose: %v", err)
	}
	joined, err := source.EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, joined)
	if err != nil || joined == nil || joined.Reference() != attempt.Reference() || starts != 1 || f.host.calls != 1 {
		t.Fatalf("same acceptance launched again: starts=%d prepare=%d err=%v", starts, f.host.calls, err)
	}
	if err = attempt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.journal.store.PinRootContext(t.Context()); err != nil {
		t.Fatalf("source closed borrowed target store: %v", err)
	}
}
