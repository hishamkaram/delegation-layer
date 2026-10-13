package pueue

import (
	"errors"
	"fmt"
)

type privateProcessIdentity struct {
	PID        int
	StartToken string
}

type privateProcessObservation struct {
	Present    bool
	StartToken string
}

func (c *Client) observePrivateProcess(pid int) (privateProcessObservation, error) {
	if pid <= 0 {
		return privateProcessObservation{}, fmt.Errorf("%w: private supervisor PID is invalid", ErrBinding)
	}
	if c != nil && c.options.processObserver != nil {
		observation, err := c.options.processObserver(pid)
		return validatePrivateProcessObservation(observation, err)
	}
	observation, err := observeNativePrivateProcess(pid)
	return validatePrivateProcessObservation(observation, err)
}

func validatePrivateProcessObservation(observation privateProcessObservation, err error) (privateProcessObservation, error) {
	if err != nil {
		return privateProcessObservation{}, errors.Join(ErrBinding, err)
	}
	if observation.Present && observation.StartToken == "" {
		return privateProcessObservation{}, fmt.Errorf("%w: private supervisor process identity is incomplete", ErrBinding)
	}
	if !observation.Present {
		observation.StartToken = ""
	}
	return observation, nil
}

func (c *Client) privateProcessIdentity(pid int) (privateProcessIdentity, bool, error) {
	observation, err := c.observePrivateProcess(pid)
	if err != nil {
		return privateProcessIdentity{}, false, err
	}
	if !observation.Present {
		return privateProcessIdentity{PID: pid}, false, nil
	}
	return privateProcessIdentity{PID: pid, StartToken: observation.StartToken}, true, nil
}

func (c *Client) privateDaemonProcessIdentity() (privateProcessIdentity, bool, error) {
	pid, present, err := c.privateDaemonPID()
	if err != nil || !present {
		return privateProcessIdentity{}, false, err
	}
	return c.privateProcessIdentity(pid)
}

func (c *Client) privateProcessUnavailable(identity privateProcessIdentity) (bool, error) {
	if identity.PID <= 0 {
		return false, fmt.Errorf("%w: private supervisor process identity is incomplete", ErrBinding)
	}
	// An empty token is created only after a native observation has already
	// proved the recorded PID absent. It lets the same locked transition remove
	// a refused state-rooted socket without consulting a reusable numeric PID.
	if identity.StartToken == "" {
		return true, nil
	}
	observation, err := c.observePrivateProcess(identity.PID)
	if err != nil {
		return false, err
	}
	return !observation.Present || observation.StartToken != identity.StartToken, nil
}
