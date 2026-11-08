package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remote"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"github.com/hishamkaram/delegation-layer/internal/sshtransport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

type remoteCLIResponse struct {
	References    []string               `json:"references,omitempty"`
	NextCursor    string                 `json:"next_cursor,omitempty"`
	SchemaVersion int                    `json:"schema_version"`
	Command       string                 `json:"command"`
	TaskID        string                 `json:"task_id,omitempty"`
	Connectivity  string                 `json:"connectivity"`
	ObservedAt    string                 `json:"observed_at,omitempty"`
	Remote        *remote.Response       `json:"remote,omitempty"`
	Downloads     []remoteDownloadReport `json:"downloads,omitempty"`
	Cached        *remote.Observation    `json:"last_observation,omitempty"`
	Devices       []string               `json:"devices,omitempty"`
	Diagnostics   []string               `json:"diagnostics,omitempty"`
	Error         string                 `json:"error,omitempty"`
}

func runRemoteCLI(a Arguments, deps Dependencies, stdout, stderr io.Writer) int {
	if a.Command == "remote" && a.RemoteAction == "serve" {
		return serveRemote(deps.Input, stdout, deps.normalized())
	}
	ctx, cancel := context.WithTimeout(context.Background(), remote.DefaultTransferTimeout)
	defer cancel()
	report, err := remoteCLI(ctx, a, deps.normalized())
	if err != nil {
		report.Error = err.Error()
	}
	report.SchemaVersion, report.Command = OutputSchemaVersion, a.Command
	encoder := json.NewEncoder(stdout)
	if encodeErr := encoder.Encode(report); encodeErr != nil {
		return writeCLIError(stderr, encodeErr, 1)
	}
	return remoteCLIResultCode(a.Command, report, err)
}

func remoteCLIResultCode(command string, report remoteCLIResponse, err error) int {
	if err != nil {
		if remote.Transient(err) || errors.Is(err, remote.ErrAcceptanceUnknown) {
			return 3
		}
		return 1
	}
	if report.Remote != nil && len(report.Remote.Envelope) > 0 {
		var envelope Response
		if decodeErr := json.Unmarshal(report.Remote.Envelope, &envelope); decodeErr != nil {
			return 1
		}
		if envelope.Error != "" {
			return 1
		}
		if envelope.Publication == "rejected" {
			return 4
		}
		if command == "collect" && envelope.Outcome == nil {
			return 3
		}
	}
	return 0
}

func loadSSHConfiguration(file string) (*sshconfig.Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	current, err := user.Current()
	if err != nil {
		return nil, err
	}
	return sshconfig.Load(sshconfig.Options{Home: home, LocalUser: current.Username, ConfigFile: file})
}

func remoteCLI(ctx context.Context, a Arguments, deps Dependencies) (remoteCLIResponse, error) {
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return remoteCLIResponse{}, err
	}
	if a.Command == "remote" && a.RemoteAction == "list" {
		return listRemoteReferences(ctx, root, a.RemoteAfter, deps)
	}
	if a.Command == "devices" {
		configuration, loadErr := loadSSHConfiguration(a.SSHConfig)
		if loadErr != nil {
			return remoteCLIResponse{}, loadErr
		}
		return remoteDeviceCLI(ctx, a, deps, root, configuration)
	}
	return remoteTaskCLI(ctx, a, deps, root)
}

func remoteDeviceCLI(ctx context.Context, a Arguments, deps Dependencies, root string, configuration *sshconfig.Config) (report remoteCLIResponse, resultErr error) {
	report.Connectivity = "not_connected"
	if a.RemoteAction == "list" {
		report.Devices = configuration.Aliases()
		return report, nil
	}
	connection, err := configuration.Resolve(a.Device)
	if err != nil {
		return report, err
	}
	options := sshtransport.Options{KnownHostsFile: filepath.Join(root, "remote-known_hosts"), DelegateExecutable: a.RemoteExecutable}
	if a.RemoteAction == "trust" {
		return report, enrollRemoteDevice(ctx, connection, options, a.Fingerprint, deps, root)
	}
	response, err := (remote.Coordinator{Exchange: sshExchange{connection: connection, options: options}}).Call(ctx, remote.Request{Version: remote.ProtocolVersion, Operation: remote.Hello, Root: a.RemoteRoot, Provider: a.Provider, Cwd: a.RemoteCwd})
	report.Remote = &response
	if err == nil {
		report.Connectivity = "connected"
	}
	return report, err
}

func remoteTaskCLI(ctx context.Context, a Arguments, deps Dependencies, root string) (report remoteCLIResponse, resultErr error) {
	report.Connectivity = "not_connected"
	create := a.Command == "dispatch"
	store, err := openStoreContext(ctx, root, deps.storeDependencies(), create)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	id, err := remoteCLIIdentifier(a)
	if err != nil {
		return report, err
	}
	report.TaskID = remote.Handle(id)
	reference, err := remote.OpenReference(ctx, store, id, create)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, reference.Close()) }()
	intent, err := loadOrPrepareRemoteIntent(ctx, a, store, id, reference)
	if err != nil {
		return report, err
	}
	configuration, err := loadSSHConfiguration(intent.SSHConfig)
	if err != nil {
		return report, err
	}
	coordinator, err := recordedRemoteCoordinator(configuration, root, intent)
	if err != nil {
		return report, err
	}
	a, err = prepareRemoteCall(ctx, a, store, intent, coordinator)
	if err != nil {
		return report, err
	}
	readRemoteCacheReport(ctx, reference, &report)
	response, err := runRemoteTaskOperation(ctx, a, coordinator, reference)
	report.Remote = &response
	var operationErr *remote.OperationError
	if err == nil || errors.As(err, &operationErr) {
		report.Connectivity = "connected"
		report.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if ctx.Err() == nil {
		readRemoteCacheReport(ctx, reference, &report)
	}
	if err == nil && a.Command == "collect" && a.OutputDirectory != "" {
		report.Downloads, err = collectRemoteFiles(ctx, store, coordinator, intent, a.OutputDirectory)
	}
	return report, err
}

func remoteCLIIdentifier(a Arguments) (string, error) {
	if a.Command != "dispatch" {
		return remote.ParseHandle(a.TaskID)
	}
	if a.TaskID == "" {
		return randomid.NewTaskID()
	}
	return a.TaskID, task.ValidateTaskID(a.TaskID)
}

func prepareRemoteReference(ctx context.Context, a Arguments, store *taskdir.Store, id string, configuration *sshconfig.Config, reference *remote.Reference) error {
	intent, err := reference.Intent()
	if errors.Is(err, os.ErrNotExist) {
		candidate, brief, prepareErr := prepareRemoteIntent(ctx, a, store, id, configuration, store.Root)
		if prepareErr != nil {
			return prepareErr
		}
		return reference.SaveIntent(ctx, candidate, brief)
	}
	if err != nil {
		return err
	}
	return validateRemoteReplay(ctx, a, intent)
}

func validateRemoteReplay(ctx context.Context, a Arguments, intent remote.Intent) error {
	brief, err := tasklifecycle.ReadBrief(a.Brief)
	if err != nil {
		return err
	}
	if a.Device != intent.Device || a.Provider != intent.Provider || task.ComputeSHA256(brief) != intent.Brief.SHA256 || !sameRemoteConfig(a.Config, intent.Config) || a.RemoteExecutable != intent.Executable || a.SSHConfig != intent.SSHConfig {
		return remote.ErrConflict
	}
	if a.RemoteRoot != "" && a.RemoteRoot != intent.Destination.Root {
		return remote.ErrConflict
	}
	outputs, err := remoteOutputPaths(a.OutputManifest)
	if err != nil {
		return err
	}
	if !slices.Equal(outputs, intent.OutputPaths) {
		return remote.ErrConflict
	}
	return validateRemoteReplayInputs(ctx, a, intent)
}

func validateRemoteReplayInputs(ctx context.Context, a Arguments, intent remote.Intent) error {
	if a.InputManifest == "" {
		if intent.InputSHA256 != "" || a.RemoteCwd != intent.RemoteCwd {
			return remote.ErrConflict
		}
		return nil
	}
	source, paths, err := readRemoteInputSelection(a.InputManifest)
	if err != nil {
		return err
	}
	digest, err := remoteSelectionDigest(source, paths)
	if err != nil {
		return err
	}
	if digest != intent.InputSelectionSHA256 {
		return remote.ErrConflict
	}

	return nil
}

func recordedRemoteCoordinator(configuration *sshconfig.Config, root string, intent remote.Intent) (remote.Coordinator, error) {
	connection, err := configuration.Resolve(intent.Device)
	if err != nil {
		return remote.Coordinator{}, err
	}
	if connection.HostName != intent.Destination.Host || connection.Port != intent.Destination.Port || connection.User != intent.Destination.User {
		return remote.Coordinator{}, remote.ErrIdentity
	}
	exchange := sshExchange{connection: connection, options: sshtransport.Options{KnownHostsFile: filepath.Join(root, "remote-known_hosts"), PinnedHostKey: intent.Destination.HostKey, DelegateExecutable: intent.Executable}}
	return remote.Coordinator{Exchange: exchange}, nil
}

func uploadPendingRemoteSnapshot(ctx context.Context, store *taskdir.Store, intent remote.Intent, coordinator remote.Coordinator) (resultErr error) {
	if intent.InputSHA256 == "" {
		return nil
	}
	journal, err := store.OpenRemoteReferenceContext(ctx, intent.TaskID, false)
	if err != nil {
		return err
	}
	var sent remote.SendIntent
	markerErr := journal.Read("send-intent.json", &sent)
	if err = journal.Close(); err != nil {
		return err
	}
	if markerErr == nil {
		return nil
	}
	if !errors.Is(markerErr, os.ErrNotExist) {
		return markerErr
	}
	manifest, snapshot, err := loadRemoteSnapshot(ctx, store, intent.TaskID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, snapshot.Close()) }()
	digest, err := remotefiles.ManifestSHA256(manifest)
	if err != nil {
		return err
	}
	if digest != intent.InputSHA256 {
		return remote.ErrConflict
	}
	_, err = uploadRemoteInputs(ctx, coordinator, intent, manifest, snapshot)
	return err
}

func runRemoteTaskOperation(ctx context.Context, a Arguments, coordinator remote.Coordinator, reference *remote.Reference) (remote.Response, error) {
	switch a.Command {
	case "dispatch":
		return coordinator.Dispatch(ctx, reference)
	case "remote":
		return coordinator.Reconcile(ctx, reference)
	case "status", "collect", "events":
		return observeRemoteTask(ctx, a, coordinator, reference)
	case "cancel":
		return coordinator.Control(ctx, reference, remote.Cancel, remote.Control{ID: a.remoteCancelID})
	case "reply", "steer":
		message, err := interactionMessage(a)
		if err != nil {
			return remote.Response{}, err
		}
		return coordinator.Control(ctx, reference, remote.Operation(a.Command), remote.Control{ID: message.ID, Message: &message})
	default:
		return remote.Response{}, fmt.Errorf("%s is unsupported for remote references", a.Command)
	}
}

func prepareRemoteIntent(ctx context.Context, a Arguments, store *taskdir.Store, id string, configuration *sshconfig.Config, root string) (remote.Intent, []byte, error) {
	connection, err := configuration.Resolve(a.Device)
	if err != nil {
		return remote.Intent{}, nil, err
	}
	brief, err := tasklifecycle.ReadBrief(a.Brief)
	if err != nil {
		return remote.Intent{}, nil, err
	}
	exchange := sshExchange{connection: connection, options: sshtransport.Options{KnownHostsFile: filepath.Join(root, "remote-known_hosts"), DelegateExecutable: a.RemoteExecutable}}
	response, err := (remote.Coordinator{Exchange: exchange}).Call(ctx, remote.Request{Version: remote.ProtocolVersion, Operation: remote.Prepare, Root: a.RemoteRoot})
	if err != nil {
		return remote.Intent{}, nil, err
	}
	outputs, err := remoteOutputPaths(a.OutputManifest)
	if err != nil {
		return remote.Intent{}, nil, err
	}
	intent := remote.Intent{SSHConfig: a.SSHConfig, Version: remote.ProtocolVersion, OriginRootID: store.RootID, TaskID: id, Device: a.Device, Executable: a.RemoteExecutable, Destination: response.Identity, Provider: a.Provider, Config: a.Config, RemoteCwd: a.RemoteCwd, Brief: remote.Descriptor{Name: "brief", Length: int64(len(brief)), SHA256: task.ComputeSHA256(brief)}, OutputPaths: outputs}
	if a.InputManifest != "" {
		source, paths, readErr := readRemoteInputSelection(a.InputManifest)
		if readErr != nil {
			return intent, nil, readErr
		}
		intent.InputSelectionSHA256, err = remoteSelectionDigest(source, paths)
		if err != nil {
			return intent, nil, err
		}
		manifest, frozen, freezeErr := prepareRemoteSnapshot(ctx, store, id, source, paths)
		if freezeErr != nil {
			return intent, nil, freezeErr
		}
		if closeErr := frozen.Close(); closeErr != nil {
			return intent, nil, closeErr
		}
		intent.InputSHA256, err = remotefiles.ManifestSHA256(manifest)
		if err != nil {
			return intent, nil, err
		}
		intent.RemoteCwd = remoteInputWorkspace(intent.Destination.Root, intent.Destination.RootID, id)
	}
	return intent, brief, intent.Validate()
}

func remoteOutputPaths(file string) ([]string, error) {
	if file == "" {
		return nil, nil
	}
	input, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	data, err := task.ReadBounded(input, task.MaxControlRecordSize)
	err = errors.Join(err, input.Close())
	if err != nil {
		return nil, err
	}
	var paths []string
	if err = task.DecodeStrict(data, &paths); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if err = remotefiles.ValidateSelectors(paths); err != nil {
		return nil, err
	}
	return paths, nil
}

func sameRemoteConfig(a, b task.TaskConfig) bool {
	left, err := task.MarshalCanonical(a)
	if err != nil {
		return false
	}
	right, err := task.MarshalCanonical(b)
	return err == nil && string(left) == string(right)
}

type remoteInputSelection struct {
	Source string   `json:"source"`
	Paths  []string `json:"paths"`
}

func readRemoteInputSelection(file string) (string, []string, error) {
	input, err := os.Open(file)
	if err != nil {
		return "", nil, err
	}
	data, err := task.ReadBounded(input, task.MaxControlRecordSize)
	err = errors.Join(err, input.Close())
	if err != nil {
		return "", nil, err
	}
	var selection remoteInputSelection
	if err = task.DecodeStrict(data, &selection); err != nil {
		return "", nil, err
	}
	if !filepath.IsAbs(selection.Source) {
		return "", nil, errors.New("snapshot source must be absolute")
	}
	return selection.Source, selection.Paths, nil
}

func prepareRemoteCall(ctx context.Context, a Arguments, store *taskdir.Store, intent remote.Intent, coordinator remote.Coordinator) (Arguments, error) {
	if a.Command == "dispatch" {
		if err := uploadPendingRemoteSnapshot(ctx, store, intent, coordinator); err != nil {
			return a, err
		}
	}
	if a.Command == "cancel" {
		var err error
		a.remoteCancelID, err = remoteCancellationID(ctx, store, intent.TaskID)
		if err != nil {
			return a, err
		}
	}
	return a, nil
}

func observeRemoteTask(ctx context.Context, a Arguments, coordinator remote.Coordinator, reference *remote.Reference) (response remote.Response, resultErr error) {
	if a.Watch <= 0 || a.Command == "status" {
		return coordinator.Observe(ctx, reference, remote.Operation(a.Command), a.After, 0)
	}
	watchCtx, cancel := context.WithTimeout(ctx, a.Watch)
	defer cancel()
	deadline, ok := watchCtx.Deadline()
	if !ok {
		return response, remote.ErrProtocol
	}
	roundTrip := 100 * time.Millisecond
	first := true
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return finishRemoteWatch(response, context.DeadlineExceeded)
		}
		wait := time.Duration(0)
		if !first {
			wait = min(max(remaining-2*roundTrip, 0), remote.DefaultOperationTimeout)
		}
		first = false
		started := time.Now()
		observed, err := coordinator.Observe(watchCtx, reference, remote.Operation(a.Command), a.After, int64(wait))
		if err != nil {
			if watchCtx.Err() != nil {
				return finishRemoteWatch(response, err)
			}
			return response, err
		}
		if wait == 0 {
			roundTrip = max(time.Since(started), 100*time.Millisecond)
		}
		response = observed
		if !remoteObservationPending(a.Command, response.Envelope) {
			return response, nil
		}
		if wait == 0 && !waitRemotePoll(watchCtx) {
			return finishRemoteWatch(response, watchCtx.Err())
		}
	}
}

func finishRemoteWatch(response remote.Response, cause error) (remote.Response, error) {
	if response.Version == remote.ProtocolVersion {
		response.Diagnostics = append(response.Diagnostics, "observation_watch_expired")
		return response, nil
	}
	return response, cause
}

func waitRemotePoll(ctx context.Context) bool {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func remoteObservationPending(command string, data json.RawMessage) bool {
	var envelope Response
	if len(data) == 0 || json.Unmarshal(data, &envelope) != nil {
		return false
	}
	if envelope.Error != "" || envelope.Outcome != nil || envelope.Status == "timed_out" || envelope.Status == "interrupted" {
		return false
	}
	if envelope.Interaction != nil && (envelope.Interaction.Closed || len(envelope.Interaction.PendingRequests) > 0) {
		return false
	}
	if command == "events" && len(envelope.Events) > 0 {
		return false
	}
	return envelope.Pending != nil
}

func loadOrPrepareRemoteIntent(ctx context.Context, a Arguments, store *taskdir.Store, id string, reference *remote.Reference) (remote.Intent, error) {
	if a.Command == "dispatch" {
		configuration, err := loadSSHConfiguration(a.SSHConfig)
		if err != nil {
			return remote.Intent{}, err
		}
		if err = prepareRemoteReference(ctx, a, store, id, configuration, reference); err != nil {
			return remote.Intent{}, err
		}
	}
	return reference.Intent()
}

func readRemoteCachedObservation(ctx context.Context, reference *remote.Reference) (*remote.Observation, error) {
	cached, err := reference.LastObservation(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cached, nil
}

// The dated cache is optional evidence. Its availability never gates an
// authoritative remote operation or changes a validated remote result.
func readRemoteCacheReport(ctx context.Context, reference *remote.Reference, report *remoteCLIResponse) {
	cached, err := readRemoteCachedObservation(ctx, reference)
	if err != nil {
		const diagnostic = "observation_cache_unavailable"
		if !slices.Contains(report.Diagnostics, diagnostic) {
			report.Diagnostics = append(report.Diagnostics, diagnostic)
		}
		return
	}
	report.Cached = cached
}

func remoteSelectionDigest(source string, paths []string) (string, error) {
	selected := slices.Clone(paths)
	sort.Strings(selected)
	if err := remotefiles.ValidateSelectors(selected); err != nil {
		return "", err
	}
	data, err := task.MarshalCanonical(remoteInputSelection{Source: filepath.Clean(source), Paths: selected})
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}
