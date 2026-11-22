package gatewayservice

// The GUI invocation adapter shares Manager's private profile coordination,
// validated service configuration and launchd primitives; it stays owner-local.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
	"golang.org/x/sys/unix"
)

var (
	ErrGUIUpgradeUnknown = errors.New("gateway GUI upgrade invocation requires observation")
	ErrGUIUpgradeRecord  = errors.New("gateway GUI upgrade private record is invalid")
)

// GUIUpgradeNotStartedError reports a positively observed preclaim job exit.
// It proves no upgrade effects, not closure of arbitrary shim descendants.
type GUIUpgradeNotStartedError struct{ ExitCode int }

func (*GUIUpgradeNotStartedError) Error() string {
	return "gateway GUI upgrade job exited before claim"
}

type guiNotStartedProof struct {
	Invocation string `json:"invocation"`
	ExitCode   int    `json:"exit_code"`
}

func (p guiNotStartedProof) validate(request GUIUpgradeRequest) error {
	if p.Invocation != request.Invocation || p.ExitCode < 1 || p.ExitCode > 255 {
		return ErrGUIUpgradeRecord
	}
	return nil
}

// GUIUpgradeSource is an immutable comparison with SDK-owned saved selection.
// It grants no authority to change that selection or the Native root.
type GUIUpgradeSource struct {
	Executable string `json:"executable"`
	Root       string `json:"root"`
	Context    string `json:"context"`
}

// GUIUpgradeRequest describes exactly one finite accepted maintenance call.
// Neither argv, provider options nor captured invocation authority are data.
type GUIUpgradeRequest struct {
	Version              int              `json:"version"`
	Invocation           string           `json:"invocation"`
	Profile              string           `json:"profile"`
	Executable           string           `json:"executable"`
	Root                 string           `json:"root"`
	ExpectedRoot         string           `json:"expected_root"`
	SavedSource          GUIUpgradeSource `json:"saved_source"`
	ExpectedOperation    string           `json:"expected_operation,omitempty"`
	NoNewNativeOperation bool             `json:"no_new_native_operation,omitempty"`
	RuntimePATH          string           `json:"runtime_path"`
	PATHSource           string           `json:"path_source"`
	Accepted             bool             `json:"accepted"`
	Deadline             time.Time        `json:"deadline"`
}

// GUIUpgradeResult transports only the bounded existing public response and
// actual exit status. Operation/phase are references to Native evidence.
type GUIUpgradeResult struct {
	Version     int             `json:"version"`
	Invocation  string          `json:"invocation"`
	ExitCode    int             `json:"exit_code"`
	Response    json.RawMessage `json:"response"`
	OperationID string          `json:"operation_id,omitempty"`
	Phase       string          `json:"phase,omitempty"`
	Pending     bool            `json:"pending"`
}

type GUIUpgradeControl interface {
	RunGUIUpgrade(context.Context, GUIUpgradeRequest) (GUIUpgradeResult, error)
}

func (r GUIUpgradeRequest) validateScope() error {
	if r.Version != 1 || !guiInvocationID(r.Invocation) || !r.Accepted || r.Deadline.IsZero() {
		return ErrGUIUpgradeRecord
	}
	if r.ExpectedRoot == "" || r.SavedSource.Context != r.ExpectedRoot || r.SavedSource.Root != r.Root {
		return ErrGUIUpgradeRecord
	}
	if r.NoNewNativeOperation && r.ExpectedOperation != "" {
		return ErrGUIUpgradeRecord
	}
	return nil
}

func (r GUIUpgradeRequest) validate() error {
	if err := r.validateScope(); err != nil {
		return err
	}
	for _, path := range []string{r.Profile, r.Executable, r.Root, r.SavedSource.Executable} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || !validText(path) {
			return ErrGUIUpgradeRecord
		}
	}
	if !utf8.ValidString(r.ExpectedRoot) || strings.IndexByte(r.ExpectedRoot, 0) >= 0 || !validPath(r.RuntimePATH) {
		return ErrGUIUpgradeRecord
	}
	switch r.PATHSource {
	case "existing_definition", "invoking_user", "prior_receipt":
		return nil
	default:
		return ErrGUIUpgradeRecord
	}
}

func guiInvocationID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

type guiPublicUpgradeSupervisor struct {
	OperationID   string `json:"operation_id"`
	Phase         string `json:"phase"`
	Pending       bool   `json:"pending"`
	HandoffNeeded bool   `json:"handoff_needed"`
}

type guiPublicUpgradeProgress struct {
	Phase  string `json:"phase"`
	Native struct {
		RootContextMatches   bool `json:"root_context_matches"`
		CapabilitiesVerified bool `json:"capabilities_verified"`
	} `json:"native"`
	Supervisor guiPublicUpgradeSupervisor `json:"supervisor"`
}

type guiPublicUpgrade struct {
	SchemaVersion int                       `json:"schema_version"`
	Command       string                    `json:"command"`
	Action        string                    `json:"action"`
	Profile       string                    `json:"profile"`
	Failure       string                    `json:"failure"`
	Upgrade       *guiPublicUpgradeProgress `json:"upgrade"`
}

func (r GUIUpgradeResult) publicUpgrade() (guiPublicUpgrade, error) {
	var value guiPublicUpgrade
	err := json.Unmarshal(r.Response, &value)
	return value, err
}

func (r GUIUpgradeResult) validate(request GUIUpgradeRequest) error {
	if err := r.validateEnvelope(request); err != nil {
		return err
	}
	value, err := r.publicUpgrade()
	if err != nil {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	if value.SchemaVersion != 1 || value.Command != "gateway" || value.Action != "upgrade" || value.Profile != request.Profile {
		return ErrGUIUpgradeRecord
	}
	if !r.matchesGUITransition(value.Upgrade) {
		return ErrGUIUpgradeRecord
	}
	return r.validateGUIResultIntent(request, value.Failure)
}

func (r GUIUpgradeResult) validateEnvelope(request GUIUpgradeRequest) error {
	if r.Version != 1 || r.Invocation != request.Invocation || r.ExitCode < 0 || r.ExitCode > 255 || len(r.Response) == 0 || len(r.Response) > task.MaxControlRecordSize || r.Pending && r.OperationID == "" {
		return ErrGUIUpgradeRecord
	}
	return nil
}

func (r GUIUpgradeResult) matchesGUITransition(value *guiPublicUpgradeProgress) bool {
	if value == nil {
		return r.OperationID == "" && r.Phase == "" && !r.Pending
	}
	return value.Supervisor.OperationID == r.OperationID && value.Supervisor.Phase == r.Phase && value.Supervisor.Pending == r.Pending
}

func (r GUIUpgradeResult) validateGUIResultIntent(request GUIUpgradeRequest, failure string) error {
	if request.ExpectedOperation != "" && r.OperationID != "" && r.OperationID != request.ExpectedOperation {
		return ErrGUIUpgradeRecord
	}
	if request.NoNewNativeOperation && r.OperationID != "" {
		return ErrGUIUpgradeRecord
	}
	if r.ExitCode == 0 && (failure != "" || r.Pending) {
		return ErrGUIUpgradeRecord
	}
	return nil
}

// RunGUIUpgrade owns one profile control slot. All profile coordination ends
// before launch or waiting: the GUI child needs those same owner locks.
func (m *Manager) RunGUIUpgrade(ctx context.Context, request GUIUpgradeRequest) (GUIUpgradeResult, error) {
	if err := finite(ctx); err != nil {
		return GUIUpgradeResult{}, err
	}
	if m.config.Platform != MacOS || request.Profile != m.config.ProfileDirectory {
		return GUIUpgradeResult{}, ErrConfiguration
	}
	if err := m.gui(ctx); err != nil {
		return GUIUpgradeResult{}, err
	}
	var selected GUIUpgradeRequest
	var launch bool
	err := m.WithProfileLock(ctx, func() error {
		var err error
		selected, launch, err = m.prepareGUIUpgrade(ctx, request)
		return err
	})
	if err != nil {
		return GUIUpgradeResult{}, err
	}
	if launch {
		if err = m.bootstrapGUIUpgrade(ctx, selected); err != nil {
			return GUIUpgradeResult{}, errors.Join(ErrGUIUpgradeUnknown, err)
		}
	}
	return m.observeGUIUpgrade(ctx, selected)
}

func (m *Manager) prepareGUIUpgrade(ctx context.Context, requested GUIUpgradeRequest) (GUIUpgradeRequest, bool, error) {
	if err := ctx.Err(); err != nil {
		return requested, false, err
	}
	previous, found, err := m.activeGUIUpgrade()
	if err != nil {
		return requested, false, err
	}
	if found {
		return m.reconcileGUIUpgrade(ctx, requested, previous)
	}
	path, present, err := m.runtimePATHLocked()
	if err != nil {
		return requested, false, err
	}
	requested.RuntimePATH, requested.PATHSource = m.config.Path, "invoking_user"
	if present {
		requested.RuntimePATH, requested.PATHSource = path, "existing_definition"
	}
	return m.publishGUIUpgrade(ctx, requested, "")
}

func (m *Manager) reconcileGUIUpgrade(ctx context.Context, requested, previous GUIUpgradeRequest) (GUIUpgradeRequest, bool, error) {
	if requested.Profile != previous.Profile || requested.Root != previous.Root || requested.ExpectedRoot != previous.ExpectedRoot {
		return previous, false, task.ErrRequestConflict
	}
	result, found, err := m.readGUIResult(previous)
	if err != nil {
		return previous, false, err
	}
	if found {
		if _, proof, proofErr := m.readGUINotStarted(previous); proofErr != nil || proof {
			return previous, false, errors.Join(ErrGUIUpgradeRecord, proofErr)
		}
		return m.reconcileGUIResult(ctx, requested, previous, result)
	}
	if _, found, err = m.readGUINotStarted(previous); err != nil {
		return previous, false, err
	} else if found {
		return m.reconcileGUINotStarted(ctx, requested, previous)
	}
	if requested.Executable != previous.Executable {
		return previous, false, task.ErrRequestConflict
	}
	attempted, err := m.guiMarker(previous, "attempted")
	if err != nil || attempted {
		return previous, false, err
	}
	// No attempted marker: this staging invocation was never launchable.
	requested.SavedSource = previous.SavedSource
	requested.ExpectedOperation = previous.ExpectedOperation
	requested.NoNewNativeOperation = previous.NoNewNativeOperation
	requested.RuntimePATH, requested.PATHSource = previous.RuntimePATH, "prior_receipt"
	return m.publishGUIUpgrade(ctx, requested, previous.Invocation)
}

func (m *Manager) reconcileGUINotStarted(ctx context.Context, requested, previous GUIUpgradeRequest) (GUIUpgradeRequest, bool, error) {
	cleaned, err := m.guiMarker(previous, "cleaned")
	if err != nil || !cleaned {
		return previous, false, err
	}
	if err = m.validateGUIReleasedNoClaim(previous); err != nil {
		return previous, false, err
	}
	if err = requested.retainGUINoStartIntent(previous); err != nil {
		return previous, false, err
	}
	requested.RuntimePATH, requested.PATHSource = previous.RuntimePATH, "prior_receipt"
	return m.publishGUIUpgrade(ctx, requested, previous.Invocation)
}

func (r *GUIUpgradeRequest) retainGUINoStartIntent(previous GUIUpgradeRequest) error {
	if r.SavedSource != previous.SavedSource || (previous.ExpectedOperation != "" || previous.NoNewNativeOperation) && r.Executable != previous.Executable {
		return task.ErrRequestConflict
	}
	r.ExpectedOperation, r.NoNewNativeOperation = previous.ExpectedOperation, previous.NoNewNativeOperation
	return nil
}

func (m *Manager) reconcileGUIResult(ctx context.Context, requested, previous GUIUpgradeRequest, result GUIUpgradeResult) (GUIUpgradeRequest, bool, error) {
	cleaned, err := m.guiMarker(previous, "cleaned")
	if err != nil || !cleaned {
		return previous, false, err
	}
	if result.ExitCode == 0 && !result.Pending {
		return m.reconcileCompletedGUIUpgrade(ctx, requested, previous)
	}
	if err = requested.retainGUIRetryIntent(previous, result); err != nil {
		return previous, false, err
	}
	requested.RuntimePATH, requested.PATHSource = previous.RuntimePATH, "prior_receipt"
	return m.publishGUIUpgrade(ctx, requested, previous.Invocation)
}

func (r *GUIUpgradeRequest) retainGUIRetryIntent(previous GUIUpgradeRequest, result GUIUpgradeResult) error {
	fresh, err := result.initialVerificationFailure(previous)
	if err != nil {
		return err
	}
	if !fresh && r.Executable != previous.Executable {
		return task.ErrRequestConflict
	}
	if !fresh {
		return r.retainGUIIntent(previous, result)
	}
	if r.SavedSource != previous.SavedSource {
		return task.ErrRequestConflict
	}
	r.ExpectedOperation, r.NoNewNativeOperation = "", false
	return nil
}

func (r GUIUpgradeResult) initialVerificationFailure(previous GUIUpgradeRequest) (bool, error) {
	if r.ExitCode == 0 || r.Pending || r.OperationID != "" || previous.ExpectedOperation != "" || previous.NoNewNativeOperation {
		return false, nil
	}
	value, err := r.publicUpgrade()
	if err != nil {
		return false, errors.Join(ErrGUIUpgradeRecord, err)
	}
	progress := value.Upgrade
	return progress != nil && progress.Phase == "verify_initial_installation" && progress.Supervisor == (guiPublicUpgradeSupervisor{}) && !progress.Native.RootContextMatches && !progress.Native.CapabilitiesVerified, nil
}

func (r *GUIUpgradeRequest) retainGUIIntent(previous GUIUpgradeRequest, result GUIUpgradeResult) error {
	intended := GUIUpgradeSource{Executable: previous.Executable, Root: previous.Root, Context: previous.ExpectedRoot}
	if r.SavedSource != previous.SavedSource && r.SavedSource != intended {
		return task.ErrRequestConflict
	}
	if handled, err := r.retainGUIConstraints(previous, result); handled || err != nil {
		return err
	}
	if result.Pending {
		if r.SavedSource != previous.SavedSource {
			return task.ErrRequestConflict
		}
		r.ExpectedOperation = result.OperationID
		return nil
	}
	if result.OperationID != "" {
		r.ExpectedOperation = result.OperationID
		return nil
	}
	return r.retainGUIWorkflow(result)
}

func (r *GUIUpgradeRequest) retainGUIConstraints(previous GUIUpgradeRequest, result GUIUpgradeResult) (bool, error) {
	r.NoNewNativeOperation, r.ExpectedOperation = previous.NoNewNativeOperation, previous.ExpectedOperation
	if r.ExpectedOperation != "" && result.OperationID != "" && result.OperationID != r.ExpectedOperation {
		return false, ErrGUIUpgradeRecord
	}
	if r.NoNewNativeOperation {
		if result.OperationID != "" || result.Pending {
			return false, ErrGUIUpgradeRecord
		}
		return true, nil
	}
	return r.ExpectedOperation != "", nil
}

func (r *GUIUpgradeRequest) retainGUIWorkflow(result GUIUpgradeResult) error {
	value, err := result.publicUpgrade()
	if err != nil {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	if value.Upgrade == nil {
		return nil
	}
	if value.Upgrade.Native.RootContextMatches && value.Upgrade.Native.CapabilitiesVerified {
		r.NoNewNativeOperation = true
		return nil
	}
	switch value.Upgrade.Phase {
	case "preview", "quiesce_connector", "migrate_installation":
		return nil
	default:
		return ErrGUIUpgradeUnknown
	}
}

func (m *Manager) reconcileCompletedGUIUpgrade(ctx context.Context, requested, previous GUIUpgradeRequest) (GUIUpgradeRequest, bool, error) {
	if requested.SavedSource.Executable != previous.Executable {
		return previous, false, task.ErrRequestConflict
	}
	if requested.Executable == previous.Executable {
		return previous, false, nil
	}
	path, present, err := m.runtimePATHLocked()
	if err != nil {
		return previous, false, err
	}
	requested.RuntimePATH, requested.PATHSource = m.config.Path, "invoking_user"
	if present {
		requested.RuntimePATH, requested.PATHSource = path, "existing_definition"
	}
	return m.publishGUIUpgrade(ctx, requested, previous.Invocation)
}

func (m *Manager) publishGUIUpgrade(ctx context.Context, request GUIUpgradeRequest, previous string) (GUIUpgradeRequest, bool, error) {
	invocation, err := guiRandomID()
	if err != nil {
		return request, false, err
	}
	request.Version, request.Invocation = 1, invocation
	if deadline, ok := ctx.Deadline(); ok {
		request.Deadline = deadline.UTC()
	}
	if err := request.validate(); err != nil {
		return request, false, err
	}
	if path, err := verifiedexec.LocatePath(request.Executable); err != nil || path != request.Executable {
		return request, false, errors.Join(ErrGUIUpgradeRecord, err)
	}
	if err := m.stageGUIUpgrade(request); err != nil {
		return request, false, err
	}
	// Publication precedes retirement of the old pending seed. Under the
	// profile lock, no attempted marker can exist without an active reference.
	if err := m.writeGUIActive(request); err != nil {
		return request, false, err
	}
	if previous != "" {
		if err := m.removeGUIRecord(previous); err != nil {
			return request, false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return request, false, err
	}
	if err := m.writeGUIMarker(request, "attempted"); err != nil {
		return request, false, err
	}
	return request, true, nil
}

func (m *Manager) runtimePATHLocked() (string, bool, error) {
	data, found, err := m.readUnitDefinition()
	if err != nil || !found {
		return "", found, err
	}
	path, err := installedPath(m.config.Platform, data)
	return path, true, err
}

func guiRandomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

type guiUpgradeReference struct {
	Invocation string `json:"invocation"`
}

func (m *Manager) guiSlot() string { return filepath.Join(m.config.ProfileDirectory, ".gui-upgrade") }

func (m *Manager) guiRecord(id string) string { return filepath.Join(m.guiSlot(), id) }

func (m *Manager) guiFile(r GUIUpgradeRequest, name string) string {
	return filepath.Join(m.guiRecord(r.Invocation), name)
}

func guiDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return privateDirectory(info, os.Getuid(), true)
}

func guiReadJSON[T any](path string) (value T, resultErr error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return value, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil || privateUnit(info, os.Getuid()) != nil || info.Size() > task.MaxControlRecordSize {
		return value, errors.Join(ErrGUIUpgradeRecord, err)
	}
	decoder := json.NewDecoder(io.LimitReader(file, task.MaxControlRecordSize+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&value); err != nil {
		return value, errors.Join(ErrGUIUpgradeRecord, err)
	}
	var extra json.RawMessage
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return value, errors.Join(ErrGUIUpgradeRecord, err)
	}
	return value, nil
}

func guiWriteJSON(path string, value any) (resultErr error) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > task.MaxControlRecordSize {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	id, err := guiRandomID()
	if err != nil {
		return err
	}
	temporary := ".publish-" + id
	temporaryPath := filepath.Join(filepath.Dir(path), temporary)
	file, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		removeErr := os.Remove(temporaryPath)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		resultErr = errors.Join(resultErr, removeErr)
	}()
	_, writeErr := io.Copy(file, bytes.NewReader(append(data, '\n')))
	if err = errors.Join(writeErr, taskdir.BarrierFile(file), file.Close()); err != nil {
		return err
	}
	if err = renameNoReplace(int(directory.Fd()), temporary, filepath.Base(path)); err != nil {
		return err
	}
	return taskdir.BarrierFile(directory)
}

func guiBarrier(path string) (resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	return taskdir.BarrierFile(file)
}

func (m *Manager) activeGUIUpgrade() (GUIUpgradeRequest, bool, error) {
	if err := guiDirectory(m.guiSlot()); errors.Is(err, os.ErrNotExist) {
		return GUIUpgradeRequest{}, false, nil
	} else if err != nil {
		return GUIUpgradeRequest{}, false, err
	}
	ref, err := guiReadJSON[guiUpgradeReference](filepath.Join(m.guiSlot(), "active.json"))
	if errors.Is(err, os.ErrNotExist) {
		return GUIUpgradeRequest{}, false, nil
	}
	if err != nil || !guiInvocationID(ref.Invocation) {
		return GUIUpgradeRequest{}, false, errors.Join(ErrGUIUpgradeRecord, err)
	}
	request, err := m.readGUIRequest(ref.Invocation)
	return request, true, err
}

func (m *Manager) readGUIRequest(id string) (GUIUpgradeRequest, error) {
	if !guiInvocationID(id) {
		return GUIUpgradeRequest{}, ErrGUIUpgradeRecord
	}
	if err := guiDirectory(m.guiRecord(id)); err != nil {
		return GUIUpgradeRequest{}, err
	}
	request, err := guiReadJSON[GUIUpgradeRequest](filepath.Join(m.guiRecord(id), "request.json"))
	if err != nil {
		return request, err
	}
	if err = request.validate(); err != nil || request.Invocation != id || request.Profile != m.config.ProfileDirectory {
		return request, errors.Join(ErrGUIUpgradeRecord, err)
	}
	return request, nil
}

func (m *Manager) stageGUIUpgrade(request GUIUpgradeRequest) error {
	if err := os.Mkdir(m.guiSlot(), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := guiDirectory(m.guiSlot()); err != nil {
		return err
	}
	if err := guiBarrier(m.config.ProfileDirectory); err != nil {
		return err
	}
	if err := m.cleanGUIStaging(); err != nil {
		return err
	}
	if err := os.Mkdir(m.guiRecord(request.Invocation), 0o700); err != nil {
		return err
	}
	if err := guiWriteJSON(m.guiFile(request, "request.json"), request); err != nil {
		return err
	}
	return guiBarrier(m.guiSlot())
}

// Unreferenced staging records have no launch authority. A claimed/attempted
// record is retired only with its durable cleanup proof, never from absence.
func (m *Manager) cleanGUIStaging() (resultErr error) {
	active, found, err := m.activeGUIUpgrade()
	if err != nil {
		return err
	}
	directory, err := os.Open(m.guiSlot())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	entries, err := directory.ReadDir(5)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) == 5 {
		return ErrGUIUpgradeRecord
	}
	for _, entry := range entries {
		if entry.Name() == "active.json" || found && entry.Name() == active.Invocation {
			continue
		}
		if err = m.cleanGUIStagingEntry(entry.Name(), active, found); err != nil {
			return err
		}

	}
	return guiBarrier(m.guiSlot())
}

func (m *Manager) cleanGUIStagingEntry(name string, active GUIUpgradeRequest, found bool) error {
	if guiPublicationName(name) {
		path := filepath.Join(m.guiSlot(), name)
		info, err := os.Lstat(path)
		if err != nil || privateUnit(info, os.Getuid()) != nil {
			return errors.Join(ErrGUIUpgradeRecord, err)
		}
		return os.Remove(path)
	}
	if name == "active.next" {
		if _, err := guiReadJSON[guiUpgradeReference](filepath.Join(m.guiSlot(), name)); err != nil {
			return err
		}
		return os.Remove(filepath.Join(m.guiSlot(), name))
	}
	return m.cleanGUIOrphan(name, active, found)
}

func (m *Manager) cleanGUIOrphan(id string, active GUIUpgradeRequest, activeFound bool) error {
	if !guiInvocationID(id) {
		return ErrGUIUpgradeRecord
	}
	if err := guiDirectory(m.guiRecord(id)); err != nil {
		return err
	}
	request := GUIUpgradeRequest{Invocation: id}
	attempted, err := m.guiMarker(request, "attempted")
	if err != nil {
		return err
	}
	claimed, err := m.guiMarker(request, "claim")
	if err != nil {
		return err
	}
	cleaned, err := m.guiMarker(request, "cleaned")
	if err != nil || (attempted || claimed) && !cleaned {
		return errors.Join(ErrGUIUpgradeUnknown, err)
	}
	if attempted || claimed {
		if err = m.validateRetiredGUISeed(id, active, activeFound); err != nil {
			return err
		}
	}
	return m.removeGUIRecord(id)
}

func (m *Manager) validateRetiredGUISeed(id string, active GUIUpgradeRequest, activeFound bool) error {
	request, err := m.readGUIRequest(id)
	if err != nil {
		return err
	}
	if !activeFound || active.Root != request.Root || active.ExpectedRoot != request.ExpectedRoot {
		return ErrGUIUpgradeRecord
	}
	if _, found, proofErr := m.readGUINotStarted(request); proofErr != nil {
		return proofErr
	} else if found {
		return m.validateRetiredGUINoStart(request, active)
	}
	result, found, err := m.readGUIResult(request)
	if err != nil || !found {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	return validateRetiredGUIResult(request, active, result)
}

func validateRetiredGUIResult(request, active GUIUpgradeRequest, result GUIUpgradeResult) error {
	if result.ExitCode == 0 && !result.Pending {
		if active.SavedSource.Executable != request.Executable {
			return ErrGUIUpgradeRecord
		}
		return nil
	}
	if active.RuntimePATH != request.RuntimePATH {
		return ErrGUIUpgradeRecord
	}
	expected := active
	if err := expected.retainGUIRetryIntent(request, result); err != nil {
		return err
	}
	if expected.ExpectedOperation != active.ExpectedOperation || expected.NoNewNativeOperation != active.NoNewNativeOperation {
		return ErrGUIUpgradeRecord
	}
	return nil
}

func (m *Manager) validateRetiredGUINoStart(previous, active GUIUpgradeRequest) error {
	if err := m.validateGUIReleasedNoClaim(previous); err != nil {
		return err
	}
	if active.RuntimePATH != previous.RuntimePATH {
		return ErrGUIUpgradeRecord
	}
	expected := active
	if err := expected.retainGUINoStartIntent(previous); err != nil {
		return err
	}
	if expected.ExpectedOperation != active.ExpectedOperation || expected.NoNewNativeOperation != active.NoNewNativeOperation {
		return ErrGUIUpgradeRecord
	}
	return nil
}

func (m *Manager) writeGUIActive(request GUIUpgradeRequest) error {
	next := filepath.Join(m.guiSlot(), "active.next")
	if err := guiWriteJSON(next, guiUpgradeReference{Invocation: request.Invocation}); err != nil {
		return err
	}
	if err := os.Rename(next, filepath.Join(m.guiSlot(), "active.json")); err != nil {
		return err
	}
	return guiBarrier(m.guiSlot())
}

func (m *Manager) guiMarker(request GUIUpgradeRequest, name string) (bool, error) {
	ref, err := guiReadJSON[guiUpgradeReference](m.guiFile(request, name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || ref.Invocation != request.Invocation {
		return false, errors.Join(ErrGUIUpgradeRecord, err)
	}
	return true, nil
}

func (m *Manager) writeGUIMarker(request GUIUpgradeRequest, name string) error {
	err := guiWriteJSON(m.guiFile(request, name), guiUpgradeReference{Invocation: request.Invocation})
	if errors.Is(err, os.ErrExist) {
		_, err = m.guiMarker(request, name)
	}
	return err
}

func (m *Manager) readGUIResult(request GUIUpgradeRequest) (GUIUpgradeResult, bool, error) {
	result, err := guiReadJSON[GUIUpgradeResult](m.guiFile(request, "result.json"))
	if errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	return result, true, result.validate(request)
}

func guiOwnedName(name string) bool {
	if guiPublicationName(name) {
		return true
	}
	switch name {
	case "request.json", "attempted", "job.plist", "alive", "claim", "result.json", "joined", "cleaned", "not_started":
		return true
	default:
		return false
	}
}

func guiPublicationName(name string) bool {
	return strings.HasPrefix(name, ".publish-") && guiInvocationID(strings.TrimPrefix(name, ".publish-"))
}

func (m *Manager) removeGUIRecord(id string) error {
	if !guiInvocationID(id) {
		return ErrGUIUpgradeRecord
	}
	directory := m.guiRecord(id)
	if err := guiDirectory(directory); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	identities := make(map[string]os.FileInfo, len(entries))
	for _, entry := range entries {
		info, statErr := os.Lstat(filepath.Join(directory, entry.Name()))
		if statErr != nil || !guiOwnedName(entry.Name()) || privateUnit(info, os.Getuid()) != nil {
			return errors.Join(ErrGUIUpgradeRecord, statErr)
		}
		identities[entry.Name()] = info
	}
	for _, entry := range entries {
		current, statErr := os.Lstat(filepath.Join(directory, entry.Name()))
		if statErr != nil || !os.SameFile(identities[entry.Name()], current) {
			return errors.Join(ErrGUIUpgradeRecord, statErr)
		}
		if err = os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			return err
		}
	}
	if err = os.Remove(directory); err != nil {
		return err
	}
	return guiBarrier(m.guiSlot())
}

func (m *Manager) guiUpgradeLabel(request GUIUpgradeRequest) string {
	return m.name + ".upgrade." + request.Invocation
}

func (m *Manager) guiUpgradeTarget(request GUIUpgradeRequest) string {
	return m.guiDomain() + "/" + m.guiUpgradeLabel(request)
}

func (m *Manager) bootstrapGUIUpgrade(ctx context.Context, request GUIUpgradeRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data := m.renderGUIUpgrade(request)
	path := m.guiFile(request, "job.plist")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, taskdir.BarrierFile(file), file.Close(), guiBarrier(m.guiRecord(request.Invocation)))
	if err != nil {
		return err
	}
	return m.requireCommand(ctx, "bootstrap", m.guiDomain(), path)
}

func (m *Manager) renderGUIUpgrade(request GUIUpgradeRequest) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString("<plist version=\"1.0\"><dict><key>Label</key>")
	writeXMLString(&b, m.guiUpgradeLabel(request))
	b.WriteString("<key>ProgramArguments</key><array>")
	for _, argument := range []string{request.Executable, "gateway", "upgrade", "--root", request.Root, "--profile", request.Profile, "--accept-execution-policy-removal", "--json", "--gui-upgrade-request", m.guiFile(request, "request.json")} {
		writeXMLString(&b, argument)
	}
	b.WriteString("</array><key>EnvironmentVariables</key><dict><key>PATH</key>")
	writeXMLString(&b, request.RuntimePATH)
	b.WriteString("</dict><key>WorkingDirectory</key>")
	writeXMLString(&b, request.Profile)
	b.WriteString("<key>RunAtLoad</key><true/><key>KeepAlive</key><false/><key>AbandonProcessGroup</key><true/><key>StandardOutPath</key><string>/dev/null</string><key>StandardErrorPath</key><string>/dev/null</string></dict></plist>\n")
	return b.Bytes()
}

type guiJobState struct {
	state string
	pid   int
	exit  *int
}

var guiJobScalar = regexp.MustCompile(`(?m)^\s*(state|pid|last exit code) = ([a-z 0-9-]+)$`)

func parseGUIJobState(data []byte) guiJobState {
	var state guiJobState
	for _, match := range guiJobScalar.FindAllSubmatch(data, -1) {
		switch string(match[1]) {
		case "state":
			if state.state == "" {
				state.state = strings.TrimSpace(string(match[2]))
			}
		case "pid":
			if value, err := strconv.Atoi(string(match[2])); err == nil {
				state.pid = value
			}
		case "last exit code":
			if value, err := strconv.Atoi(string(match[2])); err == nil {
				state.exit = &value
			}
		}
	}
	return state
}

func (s guiJobState) exited() bool {
	return s.exit != nil && (s.state == "not running" || s.state == "waiting" || s.state == "exited")
}

func (m *Manager) observeGUIUpgrade(ctx context.Context, request GUIUpgradeRequest) (GUIUpgradeResult, error) {
	delay := 25 * time.Millisecond
	for {
		result, err := m.observeGUIUpgradeStep(ctx, request)
		if err == nil || !IsPending(err) {
			return result, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, errors.Join(ErrGUIUpgradeUnknown, ctx.Err())
		case <-timer.C:
			delay = min(2*delay, time.Second)
		}
	}
}

func (m *Manager) observeGUIUpgradeStep(ctx context.Context, request GUIUpgradeRequest) (GUIUpgradeResult, error) {
	result, found, err := m.readGUIResult(request)
	if err != nil {
		return result, err
	}
	if found {
		if _, proof, proofErr := m.readGUINotStarted(request); proofErr != nil || proof {
			return result, errors.Join(ErrGUIUpgradeRecord, proofErr)
		}
		return result, m.finishGUIObservation(ctx, request, result)
	}
	if proof, found, proofErr := m.readGUINotStarted(request); proofErr != nil {
		return result, proofErr
	} else if found {
		return result, m.finishGUINotStarted(ctx, request, proof)
	}
	return m.observeGUIWithoutResult(ctx, request)
}

func (m *Manager) observeGUIWithoutResult(ctx context.Context, request GUIUpgradeRequest) (GUIUpgradeResult, error) {
	observed, readErr := m.invoke(ctx, "print", m.guiUpgradeTarget(request))
	readErr = guiManagerObservationError(observed, readErr)
	if readErr != nil || observed.ExitCode != 0 {
		// A concurrent observer may have joined and removed the exact job.
		latest, published, resultErr := m.readGUIResult(request)
		if readErr == nil && resultErr == nil && published {
			return latest, ErrPending
		}
		return latest, errors.Join(ErrGUIUpgradeUnknown, readErr, resultErr)
	}
	state := parseGUIJobState(observed.Stdout)
	if !state.exited() {
		return GUIUpgradeResult{}, ErrPending
	}
	proof := guiNotStartedProof{Invocation: request.Invocation, ExitCode: *state.exit}
	if err := m.recordGUINotStarted(ctx, request, proof); err != nil {
		return GUIUpgradeResult{}, err
	}
	return GUIUpgradeResult{}, m.finishGUINotStarted(ctx, request, proof)
}

func (m *Manager) readGUINotStarted(request GUIUpgradeRequest) (guiNotStartedProof, bool, error) {
	proof, err := guiReadJSON[guiNotStartedProof](m.guiFile(request, "not_started"))
	if errors.Is(err, os.ErrNotExist) {
		return proof, false, nil
	}
	if err != nil {
		return proof, false, err
	}
	return proof, true, proof.validate(request)
}

func (m *Manager) validateGUINoClaim(request GUIUpgradeRequest) error {
	claimed, err := m.guiMarker(request, "claim")
	if err != nil || claimed {
		return errors.Join(ErrGUIUpgradeUnknown, err)
	}
	_, result, err := m.readGUIResult(request)
	if err != nil || result {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	return nil
}

func (m *Manager) validateGUIReleasedNoClaim(request GUIUpgradeRequest) error {
	if err := m.validateGUINoClaim(request); err != nil {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	released, err := guiAliveReleased(m.guiFile(request, "alive"))
	if err != nil {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	if !released {
		return ErrGUIUpgradeUnknown
	}
	return nil
}

func (m *Manager) recordGUINotStarted(ctx context.Context, request GUIUpgradeRequest, proof guiNotStartedProof) error {
	if err := proof.validate(request); err != nil {
		return err
	}
	return m.WithProfileLock(ctx, func() error {
		if err := m.validateGUIActiveRequest(request); err != nil {
			return err
		}
		if _, result, err := m.readGUIResult(request); err != nil || result {
			return errors.Join(ErrPending, err)
		}
		if err := m.validateGUINoClaim(request); err != nil {
			return err
		}
		if previous, found, err := m.readGUINotStarted(request); err != nil {
			return err
		} else if found {
			if previous != proof {
				return ErrGUIUpgradeRecord
			}
			return nil
		}
		return m.publishGUINotStarted(request, proof)
	})
}

func (m *Manager) publishGUINotStarted(request GUIUpgradeRequest, proof guiNotStartedProof) (resultErr error) {
	file, err := os.OpenFile(m.guiFile(request, "alive"), os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if errors.Is(err, os.ErrExist) {
		file, err = os.OpenFile(m.guiFile(request, "alive"), os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil || privateUnit(info, os.Getuid()) != nil {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); errors.Is(err, unix.EWOULDBLOCK) {
		return ErrGUIUpgradeUnknown
	} else if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Flock(int(file.Fd()), unix.LOCK_UN)) }()
	if err = taskdir.BarrierFile(file); err != nil {
		return err
	}
	if err = m.validateGUINoClaim(request); err != nil {
		return err
	}
	return guiWriteJSON(m.guiFile(request, "not_started"), proof)
}

func (m *Manager) finishGUINotStarted(ctx context.Context, request GUIUpgradeRequest, proof guiNotStartedProof) error {
	if err := m.validateGUINoClaim(request); err != nil {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	released, err := guiAliveReleased(m.guiFile(request, "alive"))
	if err != nil || !released {
		return errors.Join(ErrPending, err)
	}
	cleaned, err := m.guiMarker(request, "cleaned")
	if err == nil && !cleaned {
		err = m.cleanupGUIJob(ctx, request)
	}
	return errors.Join(&GUIUpgradeNotStartedError{ExitCode: proof.ExitCode}, err)
}

func (m *Manager) finishGUIObservation(ctx context.Context, request GUIUpgradeRequest, result GUIUpgradeResult) error {
	cleaned, err := m.guiMarker(request, "cleaned")
	if err != nil || cleaned {
		return err
	}
	closed, err := guiAliveReleased(m.guiFile(request, "alive"))
	if err != nil || !closed {
		return errors.Join(ErrPending, err)
	}
	if err = m.joinGUIUpgrade(ctx, request, result); err != nil {
		return err
	}
	return m.cleanupGUIJob(ctx, request)
}

func (m *Manager) joinGUIUpgrade(ctx context.Context, request GUIUpgradeRequest, result GUIUpgradeResult) error {
	joined, err := m.guiMarker(request, "joined")
	if err != nil || joined {
		return err
	}
	observed, readErr := m.invoke(ctx, "print", m.guiUpgradeTarget(request))
	readErr = guiManagerObservationError(observed, readErr)
	if readErr != nil || observed.ExitCode != 0 {
		return m.concurrentGUIJoin(request, observed, readErr)
	}
	state := parseGUIJobState(observed.Stdout)
	if !state.exited() {
		return ErrPending
	}
	if *state.exit != result.ExitCode {
		return ErrGUIUpgradeUnknown
	}
	return m.writeGUIMarker(request, "joined")
}

func (m *Manager) concurrentGUIJoin(request GUIUpgradeRequest, observed Result, readErr error) error {
	if readErr == nil && observed.ExitCode == 113 {
		joined, err := m.guiMarker(request, "joined")
		if err == nil && joined {
			return nil
		}
		readErr = errors.Join(readErr, err)
	}
	return errors.Join(ErrGUIUpgradeUnknown, readErr)
}

func guiManagerObservationError(observed Result, err error) error {
	if observed.ExitCode != 0 && observed.ExitCode != 113 {
		return errors.Join(ErrManager, err)
	}
	return err
}

func (m *Manager) cleanupGUIJob(ctx context.Context, request GUIUpgradeRequest) error {
	code, err := m.probe(ctx, "print", m.guiUpgradeTarget(request))
	if err != nil {
		return err
	}
	if code == 0 {
		observed, bootoutErr := m.invoke(ctx, "bootout", m.guiUpgradeTarget(request))
		if bootoutErr != nil {
			return bootoutErr
		}
		if observed.ExitCode != 0 && observed.ExitCode != 113 {
			return ErrManager
		}
	} else if code != 113 {
		return ErrManager
	}
	code, err = m.probe(ctx, "print", m.guiUpgradeTarget(request))
	if err != nil || code != 113 {
		return errors.Join(ErrManager, err)
	}
	if err = m.gui(ctx); err != nil {
		return err
	}
	return m.writeGUIMarker(request, "cleaned")
}

func guiAliveReleased(path string) (closed bool, resultErr error) {
	file, err := os.OpenFile(path, os.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, statErr := file.Stat()
	if statErr != nil || privateUnit(info, os.Getuid()) != nil {
		return false, errors.Join(ErrGUIUpgradeRecord, statErr)
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

// ReadGUIUpgradeRequest accepts only the fixed private locator under this
// selected profile. Request contents never certify invocation provenance.
func ReadGUIUpgradeRequest(locator, profile string) (GUIUpgradeRequest, error) {
	var empty GUIUpgradeRequest
	id := filepath.Base(filepath.Dir(locator))
	if !guiInvocationID(id) || locator != filepath.Join(profile, ".gui-upgrade", id, "request.json") {
		return empty, ErrGUIUpgradeRecord
	}
	for _, directory := range []string{profile, filepath.Join(profile, ".gui-upgrade"), filepath.Dir(locator)} {
		if err := guiDirectory(directory); err != nil {
			return empty, err
		}
	}
	request, err := guiReadJSON[GUIUpgradeRequest](locator)
	if err != nil {
		return request, err
	}
	if err = request.validate(); err != nil || request.Profile != profile || request.Invocation != id {
		return request, errors.Join(ErrGUIUpgradeRecord, err)
	}
	return request, nil
}

// GUIUpgradeClaim owns the child's alive lock through terminal publication.
// Duplicate claims are refused before SDK, service or Native mutation.
type GUIUpgradeClaim struct {
	request GUIUpgradeRequest
	manager *Manager
	alive   *os.File
}

func (m *Manager) validateGUIClaim(ctx context.Context, request GUIUpgradeRequest) error {
	if err := finite(ctx); err != nil {
		return err
	}
	if err := request.validate(); err != nil || request.Profile != m.config.ProfileDirectory || !request.Deadline.After(time.Now()) {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	// This supplementary lookup precedes the atomic profile-coordinated fence.
	state, err := m.invoke(ctx, "print", m.guiUpgradeTarget(request))
	if err != nil || state.ExitCode != 0 || parseGUIJobState(state.Stdout).pid != os.Getpid() {
		return errors.Join(ErrGUIRequired, err)
	}
	return nil
}

func (m *Manager) validateGUIActiveRequest(request GUIUpgradeRequest) error {
	active, found, err := m.activeGUIUpgrade()
	if err != nil || !found || active != request {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	attempted, err := m.guiMarker(request, "attempted")
	if err != nil || !attempted {
		return errors.Join(ErrGUIUpgradeRecord, err)
	}
	return nil
}

func (m *Manager) ClaimGUIUpgrade(ctx context.Context, request GUIUpgradeRequest) (*GUIUpgradeClaim, error) {
	if err := m.validateGUIClaim(ctx, request); err != nil {
		return nil, err
	}
	var claim *GUIUpgradeClaim
	err := m.WithProfileLock(ctx, func() error {
		if err := m.validateGUIActiveRequest(request); err != nil {
			return err
		}
		if !request.Deadline.After(time.Now()) {
			return ErrGUIUpgradeRecord
		}
		if _, found, err := m.readGUINotStarted(request); err != nil || found {
			return errors.Join(ErrGUIUpgradeRecord, err)
		}
		var err error
		claim, err = m.publishGUIClaim(request)
		return err
	})
	if err != nil && claim != nil {
		return nil, errors.Join(err, unix.Flock(int(claim.alive.Fd()), unix.LOCK_UN), claim.alive.Close())
	}
	return claim, err
}

func (m *Manager) publishGUIClaim(request GUIUpgradeRequest) (*GUIUpgradeClaim, error) {
	alive, err := os.OpenFile(m.guiFile(request, "alive"), os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(alive.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(err, alive.Close())
	}
	if err = guiWriteJSON(m.guiFile(request, "claim"), guiUpgradeReference{Invocation: request.Invocation}); err != nil {
		return nil, errors.Join(err, unix.Flock(int(alive.Fd()), unix.LOCK_UN), alive.Close())
	}
	return &GUIUpgradeClaim{request: request, manager: m, alive: alive}, nil
}

func (c *GUIUpgradeClaim) Finish(result GUIUpgradeResult) error {
	if c == nil || c.alive == nil {
		return ErrGUIUpgradeRecord
	}
	file := c.alive
	c.alive = nil
	validation := result.validate(c.request)
	var writeErr error
	if validation == nil {
		writeErr = guiWriteJSON(c.manager.guiFile(c.request, "result.json"), result)
	}
	return errors.Join(validation, writeErr, unix.Flock(int(file.Fd()), unix.LOCK_UN), file.Close())
}
