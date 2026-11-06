package control

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/pueue/protocol"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	repairEditorMode  = "--supervisor-repair-editor"
	repairEditorToken = "delegate-private-repair-v1"
	repairRootEnv     = "DELEGATE_PUEUE_EDIT_ROOT"
)

// RunRepairEditorMode must be the first dispatch in delegate-run, before other
// entry points or environment capture. It never starts a subprocess.
func RunRepairEditorMode(args []string) (int, bool) {
	if len(args) == 0 || args[0] != repairEditorMode {
		return 0, false
	}
	if len(args) != 2 {
		return 2, true
	}
	path := os.Getenv("PUEUE_EDIT_PATH")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || args[1] != repairEditorToken+" "+escapePueueArgument(path) {
		return 2, true
	}
	parent, err := repairParentFromEnvironment(os.Getenv(repairRootEnv), path)
	if err != nil {
		return 2, true
	}
	id := os.Getenv("DELEGATE_PUEUE_EDIT_TASK_ID")
	numericID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || numericID < 0 || strconv.FormatInt(numericID, 10) != id {
		return 2, true
	}
	if err = replaceRepairCommand(path, id, os.Getenv("DELEGATE_PUEUE_EDIT_EXPECTED_COMMAND"), os.Getenv("DELEGATE_PUEUE_EDIT_REPLACEMENT_COMMAND"), parent); err != nil {
		return 1, true
	}
	return 0, true
}

func privateRepairConfig(base, source, digest, editor string) (string, error) {
	canonical, err := canonicalExecutable(editor)
	if err != nil || canonical != editor {
		return "", errors.Join(ErrConfiguration, err)
	}
	data, err := readRegular(source, MaxControlBytes)
	if err != nil {
		return "", err
	}
	if task.ComputeSHA256(data) != digest {
		return "", ErrBinding
	}
	config, err := protocol.ConfigWithRepairEditor(data, []string{editor, repairEditorMode, "{{ pueue_command_string }}"})
	if err != nil {
		return "", err
	}
	path := filepath.Join(base, "pueue-repair-"+task.ComputeSHA256(config)+".yml")
	if err = publishPrivateRecord(base, path, config); err != nil {
		return "", fmt.Errorf("publish repair config: %w", err)
	}
	return path, nil
}
