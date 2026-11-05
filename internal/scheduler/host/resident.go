package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// RunChild requires the caller to hold E and finish definition preflight first.
// It confirms the exact live host before run, then cancels run on host EOF.
// The callback owns joined runtime shutdown; this function joins its reader.
func RunChild(ctx context.Context, config ChildConfig, run func(context.Context) error) (resultErr error) {
	if ctx == nil || run == nil {
		return fmt.Errorf("%w: invalid resident configuration", ErrIdentity)
	}
	if err := validateChildConfig(config); err != nil {
		return err
	}
	reader, err := inheritedPipe(config.ReadFD, "scheduler-host-read", unix.O_RDONLY)
	if err != nil {
		return err
	}
	writer, err := inheritedPipe(config.WriteFD, "scheduler-host-write", unix.O_WRONLY)
	if err != nil {
		return errors.Join(err, reader.Close())
	}
	control := newChannel(reader, writer)
	defer func() { resultErr = errors.Join(resultErr, control.close()) }()
	ownerCtx, ownerCancel := context.WithCancel(ctx)
	eofDone := make(chan struct{})
	go func() {
		defer close(eofDone)
		select {
		case <-control.done:
			ownerCancel()
		case <-ownerCtx.Done():
		}
	}()
	defer func() { ownerCancel(); <-eofDone }()
	hello, err := confirmHost(ownerCtx, config, control)
	if err != nil {
		if cooperativeEOF(err) {
			return nil
		}
		return err
	}
	runCtx, cancel := context.WithCancel(ownerCtx)
	defer cancel()
	watchDone := make(chan error, 1)
	go func() {
		_, readErr := control.next(runCtx)
		cancel()
		if cooperativeEOF(readErr) {
			readErr = nil
		}
		watchDone <- readErr
	}()
	resultErr = reportChildFailure(control, hello, run(runCtx))
	cancel()
	return errors.Join(resultErr, <-watchDone)
}

// A genuine fault joined to cancellation/EOF remains a fault. This classifier
// is local to the resident's cooperative channel protocol, not task authority.
func cooperativeEOF(err error) bool {
	return task.AllErrorCauses(err, func(cause error) bool {
		return errors.Is(cause, io.EOF) || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, os.ErrClosed)
	})
}

func validateChildConfig(config ChildConfig) error {
	if config.RootID == "" || config.BuildID == "" || config.ReadFD == config.WriteFD {
		return ErrIdentity
	}
	return nil
}

func confirmHost(ctx context.Context, config ChildConfig, control *channel) (childFrame, error) {
	hello, err := control.next(ctx)
	if err != nil {
		return childFrame{}, err
	}
	if err = validateHello(hello, config); err != nil {
		return childFrame{}, err
	}
	present, err := identityPresent(hello.Host)
	if err != nil || !present {
		return childFrame{}, errors.Join(ErrIdentity, err)
	}
	child, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		return childFrame{}, err
	}
	hello.Kind, hello.Child = "ready", identity(child)
	if err = beforeReady(ctx, config, control, hello); err != nil {
		return childFrame{}, err
	}
	if err = writeFrame(control.writer, hello); err != nil {
		return childFrame{}, err
	}
	confirmed, err := control.next(ctx)
	if err != nil {
		return childFrame{}, err
	}
	expected := hello
	expected.Kind = "confirmed"
	if confirmed != expected {
		return childFrame{}, ErrIdentity
	}
	return hello, nil
}

func validateHello(hello childFrame, config ChildConfig) error {
	if hello.Kind != "hello" || hello.Protocol != ProtocolVersion || hello.RootID != config.RootID || hello.Epoch == "" || hello.Attempt == 0 {
		return ErrIdentity
	}
	if hello.BuildID != config.BuildID {
		return ErrIncompatible
	}
	return nil
}

func beforeReady(ctx context.Context, config ChildConfig, control *channel, hello childFrame) error {
	if config.BeforeReady == nil {
		return nil
	}
	return reportChildFailure(control, hello, config.BeforeReady(ctx))
}

func identity(ref lifetime.ProcessRef) Identity {
	return Identity{PID: ref.PID(), StartToken: ref.StartToken()}
}

func identityPresent(value Identity) (bool, error) {
	ref, err := lifetime.NewProcessRef(value.PID, value.StartToken)
	if err != nil {
		return false, err
	}
	return lifetime.ProcessPresent(ref)
}

// Cleanup ownership takes precedence over every other cause in the graph.
// The exact validated binding authenticates this finite terminal message.
func reportChildFailure(control *channel, binding childFrame, err error) error {
	switch {
	case errors.Is(err, ErrCleanupUnresolved):
		binding.Kind = "cleanup_unresolved"
	case errors.Is(err, ErrIncompatible):
		binding.Kind = "incompatible"
	default:
		return err
	}
	return errors.Join(err, writeFrame(control.writer, binding))
}
