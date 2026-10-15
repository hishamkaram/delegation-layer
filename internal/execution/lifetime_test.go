package execution

import (
	"context"
	"os/exec"
	"testing"
)

type recordingLifetime struct{ events *eventLog }

func (l recordingLifetime) StartCommand(_ *exec.Cmd, start func() error) error {
	l.events.add("lifetime-start")
	return start()
}

func (l recordingLifetime) FinishCommand(context.Context, *exec.Cmd) error {
	l.events.add("lifetime-finish")
	return nil
}

func (l recordingLifetime) Terminate(context.Context) error {
	l.events.add("lifetime-terminate")
	return nil
}

func (l recordingLifetime) CloseTask(context.Context) error {
	l.events.add("lifetime-close")
	return nil
}

func TestLifetimeFencesPublicationAfterOwnedWait(t *testing.T) {
	td, permit, plan, _ := fixtureTask(t, "echo")
	events := &eventLog{}
	result := Run(td, permit, plan, Options{
		Lifetime: recordingLifetime{events: events},
		Hooks:    Hooks{Event: events.add},
	})
	assertSuccess(t, result)
	observed := events.snapshot()
	assertOrder(t, observed, "start-authorized", "lifetime-start")
	assertOrder(t, observed, "lifetime-start", "started")
	assertOrder(t, observed, "wait-completed", "lifetime-finish")
	assertOrder(t, observed, "lifetime-finish", "lifetime-close")
	assertOrder(t, observed, "lifetime-close", "sealed")
}
