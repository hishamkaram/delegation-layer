package infrastructure

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func selectedContextFixture(t *testing.T, journal *intentJournal, home string) (LaunchContext, LaunchContextReference) {
	t.Helper()
	record := contextForJournal(t, journal)
	record.OS = runtime.GOOS
	record.ControlEnvironment = []string{"HOME=" + home}
	record.MaintenanceEnvironment = []string{}
	reference, _, err := journal.stageContext(t.Context(), record)
	if err != nil {
		t.Fatal(err)
	}
	return record, reference
}

func TestSelectedContextDefaultAndRequestHaveOneOwner(t *testing.T) {
	journal, lease := journalFixture(t)
	defaultContext, defaultReference := selectedContextFixture(t, journal, "/epoch-home")
	requestedContext, requestedReference := selectedContextFixture(t, journal, "/request-home")
	intent := journalIntent(t, journal, 1)
	intent.Inputs.Context = defaultReference
	refreshIntentDigest(t, &intent)
	requireJournalPut(t, journal, lease, intent)
	path := filepath.Join(journal.pin.Root(), "infrastructure", intentName(intent.Sequence))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/changed-owner-home")
	for _, fixture := range []struct {
		name      string
		requested LaunchContextReference
		want      LaunchContext
		reference LaunchContextReference
	}{
		{"epoch default", LaunchContextReference{}, defaultContext, defaultReference},
		{"request selection", requestedReference, requestedContext, requestedReference},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			got, reference, resolveErr := resolveSelectedContext(t.Context(), journal, defaultReference, fixture.requested)
			if resolveErr != nil || reference != fixture.reference || !slices.Equal(got.ControlEnvironment, fixture.want.ControlEnvironment) {
				t.Fatalf("selected context substituted another source: %v", resolveErr)
			}
			got.ControlEnvironment[0] = "HOME=/consumer-mutation"
			again, _, resolveErr := resolveSelectedContext(t.Context(), journal, defaultReference, fixture.requested)
			if resolveErr != nil || !slices.Equal(again.ControlEnvironment, fixture.want.ControlEnvironment) {
				t.Fatalf("consumer changed canonical selected context: %v", resolveErr)
			}
		})
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("request selection rewrote epoch intent: %v", err)
	}
}

func TestSelectedContextRefusesWrongOSAndMissingRecords(t *testing.T) {
	journal, _ := journalFixture(t)
	_, defaultReference := selectedContextFixture(t, journal, "/epoch-home")
	foreignOS := contextForJournal(t, journal)
	foreignOS.OS = "darwin"
	if runtime.GOOS == "darwin" {
		foreignOS.OS = "linux"
	}
	foreignReference, _, err := journal.stageContext(t.Context(), foreignOS)
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []LaunchContextReference{
		foreignReference,
		{SHA256: "malformed"},
		{SHA256: strings.Repeat("f", 64)},
	} {
		record, resolved, resolveErr := resolveSelectedContext(t.Context(), journal, defaultReference, reference)
		if resolveErr == nil || resolved != (LaunchContextReference{}) || record.ControlEnvironment != nil {
			t.Fatalf("unverified selected context escaped: %v", resolveErr)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err = resolveSelectedContext(ctx, journal, defaultReference, LaunchContextReference{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("selection lost cancellation: %v", err)
	}
}
