// Package pulse provides rescan hints without owning schedules, claims or tasks.
package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
)

// ErrClosed means the source has completed its shutdown attempt. Any shutdown
// fault is retained alongside it; closure alone does not prove a clean shutdown.
var ErrClosed = errors.New("scheduler pulse source closed")

// producer is the source's boundary to GoCron. Only Source starts or shuts it
// down; its sole job can only offer a nonblocking wake hint.
type producer interface {
	NewJob(gocron.JobDefinition, gocron.Task, ...gocron.JobOption) (gocron.Job, error)
	Start()
	Shutdown() error
}

// Source owns one interval producer and at most one pending wake. The caller
// owns Start/Close; the runtime only consumes Wait. Lifecycle calls serialize,
// and Close retains its first result for every caller.
type Source struct {
	producer producer
	wake     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	started  bool
	closed   bool
	closeErr error // Immutable after done closes.
}

// New prepares one one-second DurationJob without starting wake production.
// The cadence matches runtime.IdleInterval; calendar authority stays in SQLite.
func New() (*Source, error) {
	scheduler, err := gocron.NewScheduler(gocron.WithClock(newIntervalClock()), gocron.WithLocation(time.UTC))
	if err != nil {
		return nil, fmt.Errorf("create scheduler pulse producer: %w", err)
	}
	return newSource(scheduler)
}

func newSource(scheduler producer) (*Source, error) {
	source := &Source{producer: scheduler, wake: make(chan struct{}, 1), done: make(chan struct{})}
	if _, err := scheduler.NewJob(gocron.DurationJob(time.Second), gocron.NewTask(source.signal)); err != nil {
		return nil, errors.Join(fmt.Errorf("register scheduler pulse: %w", err), scheduler.Shutdown())
	}
	return source, nil
}

func (s *Source) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Start starts wake production once. Repeated calls, including after Close,
// have no effect. A source cannot be restarted after shutdown.
func (s *Source) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started && !s.closed {
		s.producer.Start()
		s.started = true
	}
}

// Close shuts down the producer and joins its callbacks before successful
// return. It also releases Wait callers, retaining any shutdown fault.
func (s *Source) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		if err := s.producer.Shutdown(); err != nil {
			s.closeErr = fmt.Errorf("shut down scheduler pulse producer: %w", err)
		}
		s.closed = true
		close(s.done)
	}
	return s.closeErr
}

// Wait consumes one coalesced hint. Cancellation affects only this wait, never
// the shared producer. After Close it returns ErrClosed and any shutdown fault.
func (s *Source) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.done:
		return errors.Join(ErrClosed, s.closeErr)
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.Join(ErrClosed, s.closeErr)
	case <-s.wake:
		return nil
	}
}
