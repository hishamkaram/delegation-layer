package host

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/jonboulle/clockwork"
)

// service is the sole writer of host state. Pipe readers and control handlers
// return immutable messages. Only an exact successful Wait permits replacement.
type service struct {
	installation *installationProbe
	config       Config
	clock        clockwork.Clock
	journal      *taskdir.ControlDir
	endpoint     *endpoint
	epoch        epochRecord
	status       Status
	child        *childProcess
	requests     chan controlRequest
	serverDone   chan error
	cancelServer context.CancelFunc
	readyAt      time.Time
	readiness    clockwork.Timer
	retry        clockwork.Timer
	failures     int
	permanent    bool
	stopIntent   *stopRecord
}

func Serve(ctx context.Context, config Config) error {
	return serve(ctx, config, clockwork.NewRealClock(), os.Stat)
}

func serve(ctx context.Context, config Config, clock clockwork.Clock, stat func(string) (os.FileInfo, error)) (resultErr error) {
	if ctx == nil || config.Store == nil || !filepath.IsAbs(config.Executable) || !filepath.IsAbs(config.RunnerExecutable) || !filepath.IsAbs(config.Directory) || config.Environment == nil {
		return fmt.Errorf("scheduler host requires an existing root and explicit executable, selected runner, context, cwd and environment")
	}
	config.Args, config.Environment = slices.Clone(config.Args), slices.Clone(config.Environment)
	lease, err := acquireServeLease(ctx, config.Store)
	if err != nil {
		return err
	}
	// Registered first: H is released last, after every owner below has joined.
	defer func() { resultErr = errors.Join(resultErr, lease.Close()) }()
	journal, err := config.Store.OpenSchedulerHostContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	s := &service{config: config, clock: clock, journal: journal, requests: make(chan controlRequest, 1), serverDone: make(chan error, 1)}
	if err = s.prepare(ctx); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, s.close(context.WithoutCancel(ctx))) }()
	serverCtx, cancelServer := context.WithCancel(context.WithoutCancel(ctx))
	s.cancelServer = cancelServer
	go s.endpoint.accept(serverCtx, s.epoch, s.requests, s.serverDone)
	s.installation = newInstallationProbe(config, stat)
	s.installation.start()
	return s.run(ctx)
}

func (s *service) prepare(ctx context.Context) (resultErr error) {
	sequence, err := nextSequence(ctx, s.journal)
	if err != nil {
		return err
	}
	epoch, err := randomid.NewRandomID()
	if err != nil {
		return err
	}
	host, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return err
	}
	endpoint, err := newEndpoint()
	if err != nil {
		return err
	}
	s.endpoint = endpoint
	s.epoch = epochRecord{Protocol: ProtocolVersion, RootID: s.config.Store.RootID, Sequence: sequence, Epoch: epoch, Host: identity(host), UID: os.Geteuid(), Endpoint: endpoint.path}
	if _, err = publishRecord(ctx, s.journal, epochName(sequence), s.epoch); err != nil {
		return errors.Join(err, endpoint.close())
	}
	s.status = Status{State: "starting", Epoch: epoch, Protocol: ProtocolVersion, Host: s.epoch.Host}
	return nil
}

func (s *service) run(ctx context.Context) error {
	s.launch(ctx)
	for {
		var events <-chan childEvent
		var waited <-chan error
		if s.child != nil {
			events, waited = s.child.control.events, s.child.wait
		}
		select {
		case <-ctx.Done():
			_, err := s.fenceStop(context.WithoutCancel(ctx))
			return err
		case request := <-s.requests:
			stop, err := s.control(ctx, request)
			if stop {
				return err
			}
		case event := <-events:
			s.childEvent(ctx, event)
		case err := <-waited:
			s.exited(err)
		case <-timerChannel(s.readiness):
			s.readiness.Stop()
			s.readiness = nil
			s.status.State, s.status.LastError = "degraded", "readiness_timeout"
		case <-timerChannel(s.retry):
			s.retry.Stop()
			s.retry = nil
			s.launch(ctx)
		}
	}
}

func (s *service) launch(ctx context.Context) {
	if s.permanent || s.status.StopAccepted {
		return
	}
	if s.status.Attempt == math.MaxUint64 {
		s.degrade("attempt_exhausted")
		return
	}
	s.status.Attempt++
	s.status.State, s.status.NextRetryAt = "starting", nil
	s.readyAt = time.Time{}
	if err := validateStore(ctx, s.config.Store); err != nil {
		s.degrade("root_identity_fault")
		return
	}
	intent := attemptRecord{RootID: s.epoch.RootID, Epoch: s.epoch.Epoch, Attempt: s.status.Attempt}
	name := fmt.Sprintf("attempt-%020d-%020d.json", s.epoch.Sequence, s.status.Attempt)
	if _, err := publishRecord(ctx, s.journal, name, intent); err != nil {
		s.degrade("storage_fault")
		return
	}
	child, err := startChild(s.config)
	s.child = child
	if child == nil {
		s.scheduleRetry("child_start_failed", 0)
		return
	}
	intent.Child = child.identity
	if err == nil {
		_, err = publishRecord(ctx, s.journal, attemptName(s.epoch.Sequence, s.status.Attempt), intent)
	}
	if err != nil {
		s.degrade("child_registration_failed")
		if closeErr := child.control.stopSending(); closeErr != nil {
			s.status.LastError = "child_channel_close_failed"
		}
		return
	}
	hello := s.binding("hello")
	if err = writeFrame(child.control.writer, hello); err != nil {
		s.status.State, s.status.LastError = "degraded", "child_channel_failed"
	}
	s.readiness = s.clock.NewTimer(10 * time.Second)
}

func timerChannel(timer clockwork.Timer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.Chan()
}
