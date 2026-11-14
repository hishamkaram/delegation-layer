package gatewaycommand

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func present(response Response, operationErr error, jsonOutput bool, stdout, stderr io.Writer) int {
	code := 0
	if operationErr != nil {
		response.Failure, response.NextAction, code = publicError(operationErr)
	}
	if jsonOutput {
		if err := writeJSON(stdout, response); err != nil {
			return 1
		}
		return code
	}
	if err := writeHuman(stdout, response); err != nil {
		return 1
	}
	if operationErr != nil {
		if _, err := fmt.Fprintf(stderr, "%s; next_action=%s\n", response.Failure, response.NextAction); err != nil {
			return 1
		}
	}
	return code
}

func writeJSON(w io.Writer, response Response) error {
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > task.MaxControlRecordSize {
		return errors.New("gateway_response_too_large")
	}
	count, err := w.Write(data)
	if err == nil && count != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func writeHuman(stdout io.Writer, response Response) error {
	if _, err := fmt.Fprintf(stdout, "gateway %s: profile=%s online=%s execution_readiness=%s\n", response.Action, response.Profile, response.Online, response.ExecutionReadiness); err != nil {
		return err
	}
	if response.Credential != nil {
		if _, err := fmt.Fprintf(stdout, "credential=%s identity_in_use=%t\n", response.Credential.CredentialState, response.Credential.IdentityInUse); err != nil {
			return err
		}
	}
	if response.Workspaces != nil {
		for _, workspace := range *response.Workspaces {
			if _, err := fmt.Fprintf(stdout, "workspace=%s id=%s revision=%d enabled=%t directory=%s\n", workspace.Alias, workspace.ID, workspace.Revision, workspace.Enabled, workspace.Directory); err != nil {
				return err
			}
		}
	}
	if response.Workspace != nil {
		w := response.Workspace
		if _, err := fmt.Fprintf(stdout, "workspace=%s id=%s revision=%d enabled=%t directory=%s\n", w.Alias, w.ID, w.Revision, w.Enabled, w.Directory); err != nil {
			return err
		}
	}
	if response.Service != nil {
		s := response.Service
		if _, err := fmt.Fprintf(stdout, "service=%s installed=%t state=%s stop_requested=%t\n", s.Name, s.Installed, s.State, s.StopRequested); err != nil {
			return err
		}
	}
	if response.Revoked {
		if _, err := io.WriteString(stdout, "device_credential_revoked=true\n"); err != nil {
			return err
		}
	}
	return nil
}
