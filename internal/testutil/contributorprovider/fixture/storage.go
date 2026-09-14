package fixture

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/contributorprovider/protocol"
)

type sessionRecord struct {
	SchemaVersion int    `json:"schema_version"`
	SessionID     string `json:"session_id"`
	Nonce         string `json:"nonce"`
}

type executionReceipt struct {
	SchemaVersion int    `json:"schema_version"`
	TaskID        string `json:"task_id"`
	SessionID     string `json:"session_id"`
	Status        string `json:"status"`
}

func ensurePrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("fixture directory is not private")
	}
	return nil
}

func writeOnce(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Sync(), file.Close())
}

func writeJSONOnce(path string, value any) error {
	data, err := task.MarshalCanonical(value)
	if err != nil {
		return err
	}
	return writeOnce(path, data)
}

func writeReceipt(runtimeDir, kind string, opts options, status string) error {
	receipt := executionReceipt{SchemaVersion: 1, TaskID: opts.taskID, SessionID: opts.sessionID, Status: status}
	return writeJSONOnce(filepath.Join(runtimeDir, kind, opts.taskID+".json"), receipt)
}

func sessionAnswer(runtimeDir string, opts options, brief protocol.Brief) (string, error) {
	path := filepath.Join(runtimeDir, "sessions", opts.sessionID+".json")
	if opts.resume {
		if brief.Answer != "" || brief.Nonce != "" {
			return "", errors.New("resume may not supply the remembered answer or nonce")
		}
		var recorded sessionRecord
		if err := readJSONFile(path, &recorded); err != nil {
			return "", err
		}
		if recorded.SchemaVersion != 1 || recorded.SessionID != opts.sessionID || recorded.Nonce == "" {
			return "", errors.New("invalid recorded synthetic session")
		}
		return recorded.Nonce, nil
	}
	if opts.sessionID != "session-"+opts.taskID {
		return "", errors.New("fresh synthetic session does not match task")
	}
	recorded := sessionRecord{SchemaVersion: 1, SessionID: opts.sessionID, Nonce: brief.Nonce}
	return brief.Answer, writeJSONOnce(path, recorded)
}
