package gatewayservice

import (
	"context"
	"errors"
	"strings"
)

func (m *Manager) managerStatus(ctx context.Context, installed bool) (Status, error) {
	if m.config.Platform == MacOS {
		return m.macManagerStatus(ctx, installed)
	}
	return m.linuxManagerStatus(ctx, installed)
}

func (m *Manager) macManagerStatus(ctx context.Context, installed bool) (Status, error) {
	status := Status{Name: m.name, Installed: installed, State: StateUnknown}
	result, err := m.invoke(ctx, "list", m.name)
	if err != nil {
		return status, err
	}
	switch result.ExitCode {
	case 0:
		if !installed {
			return status, ErrForeignService
		}
		status.State = StateRegistered
	case 113:
		// Exact-label list's service-not-found disposition is verified on
		// native macOS; other domain/permission/transport failures stay unknown.
		status.State = StateAbsent
	default:
		return status, ErrManager
	}
	return status, nil
}

func (m *Manager) linuxManagerStatus(ctx context.Context, installed bool) (Status, error) {
	properties, err := m.linuxProperties(ctx)
	if err != nil {
		return Status{Name: m.name, Installed: installed, State: StateUnknown}, err
	}
	return m.linuxObservedStatus(properties, installed)
}

func (m *Manager) linuxProperties(ctx context.Context) (map[string]string, error) {
	result, err := m.invoke(ctx, "--user", "--no-pager", "show", m.filename, "--property=LoadState,ActiveState,FragmentPath,DropInPaths,MainPID,ControlPID,NeedDaemonReload,Type,ExitType,Delegate,KillMode,SendSIGKILL,TimeoutStopUSec,Restart")
	if err != nil || result.ExitCode != 0 {
		return nil, errors.Join(ErrManager, err)
	}
	return parseProperties(result.Stdout)
}

func (m *Manager) linuxObservedStatus(properties map[string]string, installed bool) (Status, error) {
	status := Status{Name: m.name, Installed: installed, State: StateUnknown}
	if properties["LoadState"] == "not-found" && properties["FragmentPath"] == "" && properties["DropInPaths"] == "" {
		// A fresh reload can preserve failed exit history while clearing the
		// absent definition. Its safety properties then describe defaults.
		if !linuxInactive(properties) || properties["MainPID"] != "0" || properties["ControlPID"] != "0" {
			return status, ErrManager
		}
		status.State = StateAbsent
		return status, nil
	}
	if !installed && m.linuxReloadHint(properties) {
		return status, ErrManagerCacheStale
	}
	if err := m.validateLinuxUnit(properties, installed); err != nil {
		return status, err
	}
	switch properties["ActiveState"] {
	case "inactive", "failed":
		status.State = StateInactive
	case "active":
		status.State = StateRunning
	case "activating", "deactivating", "reloading", "maintenance":
		status.State = StatePending
	default:
		return status, ErrManager
	}
	return status, nil
}

func (m *Manager) validateLinuxUnit(properties map[string]string, installed bool) error {
	if !installed || properties["LoadState"] != "loaded" || properties["FragmentPath"] != m.unitPath() || properties["DropInPaths"] != "" {
		return ErrForeignService
	}
	return validateLinuxSafety(properties)
}

func validateLinuxSafety(properties map[string]string) error {
	for name, expected := range map[string]string{"Type": "exec", "ExitType": "main", "Delegate": "yes", "KillMode": "process", "SendSIGKILL": "no", "TimeoutStopUSec": "infinity", "Restart": "no"} {
		if properties[name] != expected {
			return ErrForeignService
		}
	}
	return nil
}

func linuxInactive(properties map[string]string) bool {
	return properties["ActiveState"] == "inactive" || properties["ActiveState"] == "failed"
}

func parseProperties(data []byte) (map[string]string, error) {
	properties := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found || key == "" {
			return nil, ErrManager
		}
		if _, duplicate := properties[key]; duplicate {
			return nil, ErrManager
		}
		properties[key] = value
	}
	for _, key := range []string{"LoadState", "ActiveState", "FragmentPath", "DropInPaths", "MainPID", "ControlPID", "NeedDaemonReload", "Type", "ExitType", "Delegate", "KillMode", "SendSIGKILL", "TimeoutStopUSec", "Restart"} {
		if _, found := properties[key]; !found {
			return nil, ErrManager
		}
	}
	return properties, nil
}
