// Package remotecommands owns remote command workflows over existing protocol,
// transport, task-store and local-command authorities.
package remotecommands

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// StoreAccess is the frontend's authority for opening root handles. Pinned
// calls must never initialize state and must validate the pin before returning.
type StoreAccess interface {
	Open(context.Context, string, bool, *taskdir.RootPin) (*taskdir.Store, error)
	Read(context.Context, string, *taskdir.RootPin) (*taskdir.ReadOnlyStore, error)
}

// LocalCommands retains canonical lifecycle execution and full public envelope
// encoding in the frontend. Each operation receives the selected root pin.
type LocalCommands interface {
	Probe(context.Context, taskdir.RootPin, remote.Request, remote.Response) remote.Response
	Admit(context.Context, taskdir.RootPin, remote.Request, *task.RemoteBinding, remote.Response) remote.Response
	Observe(context.Context, taskdir.RootPin, remote.Request, remote.Response, bool) remote.Response
	Retired(context.Context, taskdir.RootPin, remote.Request, remote.Response, error) remote.Response
}

// ObservationFacts are derived only after the frontend's full envelope decoder.
type ObservationFacts struct {
	Error, Outcome, TimedOut, Interrupted, InteractionClosed, InteractionPending, Events, Pending bool
}
type EnvelopeInspector interface {
	Inspect(json.RawMessage) (ObservationFacts, error)
}

type Client struct {
	environment []string
	Stores      StoreAccess
	Local       LocalCommands
	Envelopes   EnvelopeInspector
	pin         *taskdir.RootPin
}

type DispatchInput struct {
	TaskID           string
	Device           string
	SSHConfig        string
	RemoteExecutable string
	RemoteRoot       string
	RemoteCwd        string
	Provider         string
	Brief            string
	InputManifest    string
	OutputManifest   string
	Config           task.TaskConfig
	ResourceGuards   resourceadmission.GuardPolicy
}
type TaskInput struct {
	Logs            LogInput
	Operation       string
	TaskID          string
	OutputDirectory string
	Watch           time.Duration
	After           uint64
	Control         *task.InteractionMessage
	// ControlError preserves a frontend parsing failure until control submission.
	ControlError error
}
type DeviceInput struct {
	Action           string
	Device           string
	SSHConfig        string
	Fingerprint      string
	RemoteExecutable string
	RemoteRoot       string
	Provider         string
	RemoteCwd        string
}

// originRequest is the workflow's invocation-local selection. It contains only
// remote command inputs and is never stored as task intent.
type originRequest struct {
	Root             string
	Command          string
	RemoteAction     string
	RemoteAfter      string
	TaskID           string
	Device           string
	SSHConfig        string
	Fingerprint      string
	RemoteExecutable string
	RemoteRoot       string
	RemoteCwd        string
	Provider         string
	Brief            string
	InputManifest    string
	OutputManifest   string
	OutputDirectory  string
	Config           task.TaskConfig
	ResourceGuards   resourceadmission.GuardPolicy
	Watch            time.Duration
	After            uint64
	control          *task.InteractionMessage
	controlError     error
	remoteCancelID   string
}

func (a originRequest) message() (task.InteractionMessage, error) {
	if a.controlError != nil {
		return task.InteractionMessage{}, a.controlError
	}
	if a.control == nil {
		return task.InteractionMessage{}, remote.ErrProtocol
	}
	return *a.control, nil
}

func (c Client) Dispatch(ctx context.Context, root string, a DispatchInput) (Report, error) {
	c = c.captureEnvironment()
	return remoteCLI(ctx, originRequest{Root: root, Command: "dispatch", TaskID: a.TaskID, Device: a.Device, SSHConfig: a.SSHConfig, RemoteExecutable: a.RemoteExecutable, RemoteRoot: a.RemoteRoot, RemoteCwd: a.RemoteCwd, Provider: a.Provider, Brief: a.Brief, InputManifest: a.InputManifest, OutputManifest: a.OutputManifest, Config: a.Config, ResourceGuards: a.ResourceGuards}, c)
}

func (c Client) Task(ctx context.Context, root string, a TaskInput) (Report, error) {
	c = c.captureEnvironment()
	if a.Operation == "logs" {
		return c.remoteLogs(ctx, root, a.TaskID, a.Logs)
	}
	return remoteCLI(ctx, originRequest{Root: root, Command: a.Operation, TaskID: a.TaskID, OutputDirectory: a.OutputDirectory, Watch: a.Watch, After: a.After, control: a.Control, controlError: a.ControlError}, c)
}

func (c Client) Device(ctx context.Context, root string, a DeviceInput) (Report, error) {
	c = c.captureEnvironment()
	report, err := remoteCLI(ctx, originRequest{Root: root, Command: "devices", RemoteAction: a.Action, Device: a.Device, SSHConfig: a.SSHConfig, Fingerprint: a.Fingerprint, RemoteExecutable: a.RemoteExecutable, RemoteRoot: a.RemoteRoot, RemoteCwd: a.RemoteCwd, Provider: a.Provider}, c)
	if a.Action == "probe" {
		report.Stages = probeStages(report.Remote, err, a.Provider, a.RemoteCwd)
	}
	return report, err
}

func (c Client) List(ctx context.Context, root, after string) (Report, error) {
	return remoteCLI(ctx, originRequest{Root: root, Command: "remote", RemoteAction: "list", RemoteAfter: after}, c)
}

func (c Client) observationPending(command string, data json.RawMessage) bool {
	if len(data) == 0 || c.Envelopes == nil {
		return false
	}
	facts, err := c.Envelopes.Inspect(data)
	if err != nil {
		return false
	}
	return observationPending(command, facts)
}

func observationPending(command string, facts ObservationFacts) bool {
	if facts.Error || facts.Outcome || facts.TimedOut || facts.Interrupted || facts.InteractionClosed || facts.InteractionPending {
		return false
	}
	if command == "events" && facts.Events {
		return false
	}
	return facts.Pending
}

func (c Client) captureEnvironment() Client {
	if c.environment == nil {
		c.environment = append([]string{}, os.Environ()...)
	}
	return c
}
