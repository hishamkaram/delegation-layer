package remotefiles

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCaptureSeparatesInvalidSourcesFromIOFailures(t *testing.T) {
	for _, test := range []struct {
		name, status, reason string
		setup                func(*testing.T, *os.Root, *os.Root)
	}{
		{"directory", "rejected", "not_regular", func(t *testing.T, source, _ *os.Root) {
			if err := source.Mkdir("selected", 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink", "rejected", "hard_link", func(t *testing.T, source, _ *os.Root) {
			write(t, source, "original", "x")
			if err := source.Link("original", "selected"); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversize", "rejected", "too_large", func(t *testing.T, source, _ *os.Root) {
			f, err := source.OpenFile("selected", os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			err = errors.Join(f.Truncate(MaxFileBytes+1), f.Close())
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"source_io", "incomplete", "io_error", func(t *testing.T, source, _ *os.Root) {
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
		}},
		{"destination_io", "incomplete", "io_error", func(t *testing.T, source, destination *os.Root) {
			write(t, source, "selected", "x")
			if err := destination.Close(); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, destination := rooted(t), rooted(t)
			test.setup(t, source, destination)
			receipt, err := Capture(t.Context(), source, destination, []string{"selected"}, Provenance{})
			if err != nil {
				t.Fatal(err)
			}
			file := receipt.Files[0]
			if file.Status != test.status || file.Reason != test.reason || file.Entry != nil {
				t.Fatalf("disposition=%+v", file)
			}
		})
	}
}

func TestCaptureEnforcesRemainingAggregateBudgetAndContinues(t *testing.T) {
	source, destination := rooted(t), rooted(t)
	write(t, source, "first", "ab")
	write(t, source, "too-big", "cd")
	write(t, source, "last", "e")
	receipt, err := CaptureWithLimit(t.Context(), source, destination, []string{"first", "too-big", "last"}, Provenance{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Files[0].Status != "captured" || receipt.Files[1].Status != "over_budget" || receipt.Files[1].Reason != "aggregate_byte_limit" || receipt.Files[2].Status != "captured" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if _, err := destination.Lstat("too-big"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("over-budget file published: %v", err)
	}
}

func TestCaptureCopyRejectsGrowthWithoutWritingBeyondDescriptor(t *testing.T) {
	destination := rooted(t)
	entry := entryFor("selected", "data")
	err := writePartial(t.Context(), destination, "partial", entry, strings.NewReader("data grew"))
	var invalid *sourceValidityError
	if !errors.As(err, &invalid) || invalid.reason != "changed" {
		t.Fatalf("growth error=%v", err)
	}
	partial, err := destination.ReadFile("partial")
	if err != nil || string(partial) != "data" {
		t.Fatalf("copy exceeded descriptor: %q %v", partial, err)
	}
	if _, err := destination.Lstat("selected"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed file published: %v", err)
	}
}

func TestCaptureDeadlineIsIncomplete(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	receipt, err := Capture(ctx, rooted(t), rooted(t), []string{"selected"}, Provenance{})
	if err != nil || receipt.Files[0].Status != "incomplete" || receipt.Files[0].Reason != "capture_deadline" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestTransferDescriptorsUseLiteralPathJSONKey(t *testing.T) {
	for _, value := range []any{Entry{Path: "selected"}, CapturedFile{Path: "selected", Status: "absent"}} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["path"]) != `"selected"` {
			t.Fatalf("missing contractual path key: %s", data)
		}
		if _, bad := fields["name"]; bad {
			t.Fatalf("unexpected descriptor name key: %s", data)
		}
	}
}
