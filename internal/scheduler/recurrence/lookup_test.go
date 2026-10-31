package recurrence

import (
	"errors"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"
)

type fixtureCron struct {
	validation error
	candidate  time.Time
	calledNext *bool
}

func (c fixtureCron) IsValid(string, *time.Location, time.Time) error { return c.validation }

func (c fixtureCron) Next(time.Time) time.Time {
	if c.calledNext != nil {
		*c.calledNext = true
	}
	return c.candidate
}

func instant(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestLookupInitializationErrorsNeverCallNext(t *testing.T) {
	fault := errors.New("lookup fixture fault")
	for _, test := range []struct {
		err, category error
	}{
		{errors.Join(gocron.ErrCronJobParse, errors.New("bad minute")), ErrInvalidExpression},
		{fault, ErrLookup},
		{errors.Join(gocron.ErrCronJobInvalid, fault), ErrLookup},
	} {
		called := false
		factory := func() gocron.Cron { return fixtureCron{validation: test.err, calledNext: &called} }
		_, _, err := lookupBetween("* * * * *", time.UTC, instant(t, "2026-01-01T00:00:00Z"), instant(t, "2026-01-02T00:00:00Z"), factory)
		if !errors.Is(err, test.category) || !errors.Is(err, test.err) || called {
			t.Fatalf("initialization error=%v, Next called=%v", err, called)
		}
	}
}

func TestLookupRejectsNonprogressAndFractionalCandidates(t *testing.T) {
	after := instant(t, "2026-01-01T00:00:00Z")
	for _, candidate := range []time.Time{{}, after, after.Add(-time.Minute), after.Add(time.Second), after.Add(time.Minute + time.Nanosecond)} {
		factory := func() gocron.Cron { return fixtureCron{candidate: candidate} }
		if _, _, err := lookupBetween("* * * * *", time.UTC, after, after.Add(time.Hour), factory); !errors.Is(err, ErrLookup) {
			t.Fatalf("candidate %s: %v", candidate, err)
		}
	}
}

func TestLookupOverlappingYearsAndFinalTail(t *testing.T) {
	after := instant(t, "2096-02-29T00:00:00Z")
	through := instant(t, "2104-02-29T00:00:00Z")
	got, found, err := lookupBetween("0 0 29 2 *", time.UTC, after, through, defaultCron)
	if err != nil || !found || !got.Equal(through) {
		t.Fatalf("leap century lookup = %s, %v, %v", got, found, err)
	}
	probes := 0
	called := false
	factory := func() gocron.Cron {
		probes++
		return fixtureCron{validation: gocron.ErrCronJobInvalid, calledNext: &called}
	}
	_, found, err = lookupBetween("0 0 31 2 *", time.UTC, after, after.AddDate(3, 0, 0), factory)
	if err != nil || found || probes != 1 || called {
		t.Fatalf("covered final tail: found=%v err=%v probes=%d Next=%v", found, err, probes, called)
	}
}

func TestLookupRejectsCandidateOutsideItsBound(t *testing.T) {
	after := instant(t, "2026-01-01T00:00:00Z")
	factory := func() gocron.Cron { return fixtureCron{candidate: after.Add(2 * time.Hour)} }
	if _, found, err := lookupBetween("* * * * *", time.UTC, after, after.Add(time.Hour), factory); err != nil || found {
		t.Fatalf("candidate beyond bound: found=%v err=%v", found, err)
	}
	if _, _, err := lookupBetween("* * * * *", time.UTC, after, after.Add(time.Hour), func() gocron.Cron { return nil }); !errors.Is(err, ErrLookup) {
		t.Fatalf("nil initialization: %v", err)
	}
}
