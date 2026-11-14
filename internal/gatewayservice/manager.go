package gatewayservice

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

func (m *Manager) invoke(ctx context.Context, args ...string) (Result, error) {
	if err := finite(ctx); err != nil {
		return Result{}, err
	}
	result, err := m.config.Runner.Run(ctx, m.config.ManagerExecutable, args)
	if err != nil {
		return Result{}, errors.Join(ErrManager, err)
	}
	if len(result.Stdout) > maxCommandOutput || result.ExitCode < 0 || result.ExitCode > 255 {
		return Result{}, ErrManager
	}
	return result, nil
}

func (m *Manager) requireCommand(ctx context.Context, args ...string) error {
	result, err := m.invoke(ctx, args...)
	if err != nil || result.ExitCode != 0 {
		return errors.Join(ErrManager, err)
	}
	return nil
}

func (m *Manager) gui(ctx context.Context) error {
	if m.config.Platform == Linux {
		return nil
	}
	name, err := m.invoke(ctx, "managername")
	if err != nil || name.ExitCode != 0 || strings.TrimSpace(string(name.Stdout)) != "Aqua" {
		return errors.Join(ErrGUIRequired, err)
	}
	uid, err := m.invoke(ctx, "manageruid")
	if err != nil || uid.ExitCode != 0 || strings.TrimSpace(string(uid.Stdout)) != strconv.Itoa(m.config.UID) {
		return errors.Join(ErrGUIRequired, err)
	}
	return nil
}

func (m *Manager) target() string { return "gui/" + strconv.Itoa(m.config.UID) + "/" + m.name }

// Install publishes only an exact private service definition. A failed manager
// reload/enable retains that definition for an explicit idempotent retry.
func (m *Manager) Install(ctx context.Context) (status Status, resultErr error) {
	resultErr = m.WithProfileLock(ctx, func() error {
		if err := m.requireSourceExecutable(); err != nil {
			return err
		}
		var err error
		status, err = m.mutationStatus(ctx)
		if err != nil {
			return err
		}
		if !status.Installed {
			if err = m.installUnit(); err != nil {
				return err
			}
		} else if err = m.barrierInstalledUnit(); err != nil {
			return err
		}
		status.Installed = true
		if m.config.Platform == Linux {
			if err = m.requireCommand(ctx, "--user", "daemon-reload"); err != nil {
				return err
			}
			return m.requireCommand(ctx, "--user", "enable", m.filename)
		}
		return nil
	})
	return status, resultErr
}

func (m *Manager) Start(ctx context.Context) (status Status, resultErr error) {
	resultErr = m.WithProfileLock(ctx, func() error {
		if err := m.requireSourceExecutable(); err != nil {
			return err
		}
		var err error
		status, err = m.status(ctx)
		if err != nil {
			return err
		}
		if !status.Installed {
			return ErrConfiguration
		}
		if m.config.Platform == Linux {
			err = m.requireCommand(ctx, "--user", "start", m.filename)
		} else if status.State == StateAbsent {
			err = m.requireCommand(ctx, "bootstrap", "gui/"+strconv.Itoa(m.config.UID), m.unitPath())
		} else {
			err = m.requireCommand(ctx, "kickstart", m.target())
		}
		if err != nil {
			return err
		}
		status, err = m.status(ctx)
		if err != nil {
			return err
		}
		if status.State != StateRunning && status.State != StateRegistered {
			return ErrManager
		}
		return nil
	})
	return status, resultErr
}

func (m *Manager) Status(ctx context.Context) (status Status, resultErr error) {
	resultErr = m.WithProfileLock(ctx, func() error {
		var err error
		status, err = m.status(ctx)
		return err
	})
	return status, resultErr
}

func (m *Manager) status(ctx context.Context) (Status, error) {
	if err := m.gui(ctx); err != nil {
		return Status{Name: m.name, State: StateUnknown}, err
	}
	present, err := m.readUnit()
	if err != nil {
		return Status{Name: m.name, Installed: present, State: StateUnknown}, err
	}
	return m.managerStatus(ctx, present)
}

// Stop joins confirmation of both exact manager inactivity and the profile
// owner's runtime lease. It never waits for, cancels or signals Native tasks.
func (m *Manager) Stop(ctx context.Context) (status Status, resultErr error) {
	resultErr = m.WithProfileLock(ctx, func() error {
		var err error
		status, err = m.stop(ctx)
		return err
	})
	return status, resultErr
}

func (m *Manager) stop(ctx context.Context) (Status, error) {
	status, err := m.mutationStatus(ctx)
	if err != nil {
		return status, err
	}
	if status.State != StateAbsent && status.State != StateInactive {
		if m.config.Platform == Linux {
			err = m.requireCommand(ctx, "--user", "--no-block", "stop", m.filename)
		} else {
			err = m.requireCommand(ctx, "bootout", m.target())
		}
		status.StopRequested = true
		if err != nil {
			status.State = StatePending
			return status, err
		}
	}
	for {
		observed, stopped, readErr := m.stoppedObservation(ctx, status)
		if readErr != nil {
			return observed, readErr
		}
		if stopped {
			return observed, nil
		}
		status = observed
		status.State = StatePending
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, errors.Join(ErrPending, ctx.Err())
		case <-timer.C:
		}
	}
}

func (m *Manager) stoppedObservation(ctx context.Context, previous Status) (Status, bool, error) {
	observed, err := m.status(ctx)
	observed.StopRequested = previous.StopRequested
	if err != nil {
		observed.State = StatePending
		return observed, false, errors.Join(ErrPending, err)
	}
	if !previous.Installed && observed.Installed {
		observed.State = StateUnknown
		return observed, false, ErrServiceChanged
	}
	inactive, err := m.config.RuntimeInactive(ctx)
	if err != nil {
		observed.State = StatePending
		return observed, false, errors.Join(ErrPending, err)
	}
	return observed, inactive && (observed.State == StateAbsent || observed.State == StateInactive), nil
}

// Uninstall removes only this exact inactive service. Pending or uncertain
// stop retains its unit, profile and Native state for safe reconciliation.
func (m *Manager) Uninstall(ctx context.Context) (status Status, resultErr error) {
	resultErr = m.WithProfileLock(ctx, func() error {
		var err error
		status, err = m.stop(ctx)
		if err != nil {
			return err
		}
		if !status.Installed {
			return m.barrierUnitAbsence()
		}
		if m.config.Platform == Linux {
			if err = m.requireCommand(ctx, "--user", "disable", m.filename); err != nil {
				return err
			}
		}
		removed, removeErr := m.removeUnit()
		if removed && m.config.Platform == Linux {
			removeErr = errors.Join(removeErr, m.requireCommand(ctx, "--user", "daemon-reload"))
		}
		if removeErr != nil {
			return removeErr
		}
		status.Installed = false
		return nil
	})
	return status, resultErr
}
