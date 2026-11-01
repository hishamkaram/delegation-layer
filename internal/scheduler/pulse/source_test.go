package pulse

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"
)

func TestSourceCoalescesActualProducerAndCloseQuiesces(t *testing.T) {
	f := producerFixture(t, &completionMonitor{completed: make(chan struct{}, 8)})
	f.monotonic.Advance(time.Hour)
	select {
	case <-f.source.wake:
		t.Fatal("source produced a wake before Start")
	default:
	}
	if jobs := f.scheduler.Jobs(); len(jobs) != 1 {
		t.Fatalf("producer has %d jobs, want one interval job", len(jobs))
	}
	f.source.Start()
	f.source.Start()
	for range 4 {
		f.step(t) // Complete real GoCron runs without consuming any wakes.
	}
	if err := f.source.Wait(waitContext(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.source.wake:
		t.Fatal("slow consumer accumulated a backlog instead of one hint")
	default:
	}
	next := await(t, f.armed)
	if err := f.source.Close(); err != nil {
		t.Fatal(err)
	}
	await(t, next.stopped)
	if next.Stop() {
		t.Fatal("Close returned with the next producer timer still armed")
	}
	f.source.Start()
	f.monotonic.Advance(24 * time.Hour)
	if err := f.source.Wait(waitContext(t)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Wait after Close: %v", err)
	}
	select {
	case <-f.monitor.completed:
		t.Fatal("producer ran after completed Close")
	default:
	}
}

func TestSourceCloseJoinsActualProducerRun(t *testing.T) {
	monitor := &completionMonitor{
		completed: make(chan struct{}, 1), entered: make(chan struct{}, 1), release: make(chan struct{}),
	}
	f := producerFixture(t, monitor)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(monitor.release) }) }
	t.Cleanup(release)
	f.source.Start()
	await(t, f.armed)
	f.monotonic.Advance(time.Second)
	await(t, monitor.entered)
	next := await(t, f.armed)
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() { defer close(joined); done <- f.source.Close() }()
	t.Cleanup(func() { release(); await(t, joined) })
	await(t, next.stopped) // Shutdown reached GoCron's live producer boundary.
	select {
	case err := <-done:
		t.Fatalf("Close returned before the active run settled: %v", err)
	default:
	}
	release()
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
	await(t, monitor.completed)
}

func TestSourceNativeProducerAndCanceledWait(t *testing.T) {
	source, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := source.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	canceled, cancel := context.WithCancel(waitContext(t))
	ctx := &observedContext{Context: canceled, entered: make(chan struct{})}
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() { defer close(joined); done <- source.Wait(ctx) }()
	t.Cleanup(func() { cancel(); await(t, joined) })
	await(t, ctx.entered)
	cancel()
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: %v", err)
	}
	started := time.Now()
	source.Start()
	if err := source.Wait(waitContext(t)); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < time.Second {
		t.Fatalf("native pulse fired before its one-second interval: %s", elapsed)
	}
}

func TestSourceCloseReleasesWaitersBeforeStart(t *testing.T) {
	source, err := New()
	if err != nil {
		t.Fatal(err)
	}
	ctx := &observedContext{Context: waitContext(t), entered: make(chan struct{})}
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() { defer close(joined); done <- source.Wait(ctx) }()
	t.Cleanup(func() {
		if closeErr := source.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		await(t, joined)
	})
	await(t, ctx.entered)
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := await(t, done); !errors.Is(err, ErrClosed) {
		t.Fatalf("waiting consumer was not released: %v", err)
	}
}

type faultyProducer struct {
	registerErr error
	shutdownErr error
	starts      int
	shutdowns   int
}

func (p *faultyProducer) NewJob(gocron.JobDefinition, gocron.Task, ...gocron.JobOption) (gocron.Job, error) {
	return nil, p.registerErr
}

func (p *faultyProducer) Start() { p.starts++ }

func (p *faultyProducer) Shutdown() error {
	p.shutdowns++
	return p.shutdownErr
}

func TestSourceRetainsRegistrationAndShutdownErrors(t *testing.T) {
	registerFault := errors.New("register interval job")
	shutdownFault := errors.New("producer shutdown")
	producer := &faultyProducer{registerErr: registerFault, shutdownErr: shutdownFault}
	source, err := newSource(producer)
	if source != nil || !errors.Is(err, registerFault) || !errors.Is(err, shutdownFault) || producer.shutdowns != 1 {
		t.Fatalf("failed registration lost cleanup: source=%v err=%v shutdowns=%d", source, err, producer.shutdowns)
	}
	producer = &faultyProducer{shutdownErr: shutdownFault}
	source, err = newSource(producer)
	if err != nil {
		t.Fatal(err)
	}
	source.Start()
	source.Start()
	for range 2 {
		if err := source.Close(); !errors.Is(err, shutdownFault) {
			t.Fatalf("Close lost original shutdown fault: %v", err)
		}
	}
	source.Start()
	if producer.starts != 1 || producer.shutdowns != 1 {
		t.Fatalf("lifecycle repeated: starts=%d shutdowns=%d", producer.starts, producer.shutdowns)
	}
	if err := source.Wait(waitContext(t)); !errors.Is(err, ErrClosed) || !errors.Is(err, shutdownFault) {
		t.Fatalf("Wait hid producer fault: %v", err)
	}
}
