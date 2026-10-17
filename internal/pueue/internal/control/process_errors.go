package control

import (
	"errors"
	"syscall"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/transport"
)

func statusObservationError(err error) error {
	if err == nil || transport.PendingFrom(err) != nil {
		return err
	}
	classified := errors.Join(ErrStatusOperational, err)
	if transientStatusCommandError(err) {
		classified = errors.Join(classified, ErrStatusTransient)
	}
	return classified
}

func transientStatusCommandError(err error) bool {
	if err == nil || errors.Is(err, ErrControlLimit) {
		return false
	}
	return completedExit(err) || errors.Is(err, syscall.EAGAIN) ||
		errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOMEM)
}

func completedExit(err error) bool {
	var exited interface {
		ExitCode() int
		Exited() bool
	}
	return errors.As(err, &exited) && exited.Exited()
}
