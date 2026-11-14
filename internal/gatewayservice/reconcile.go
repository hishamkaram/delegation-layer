package gatewayservice

import "context"

// mutationStatus is called once under the profile lock by Install or the
// initial Stop decision. Status/Start and later stop polls never reconcile.
func (m *Manager) mutationStatus(ctx context.Context) (Status, error) {
	if m.config.Platform != Linux {
		return m.status(ctx)
	}
	present, err := m.readUnit()
	if err != nil {
		return Status{Name: m.name, Installed: present, State: StateUnknown}, err
	}
	if present {
		return m.linuxManagerStatus(ctx, true)
	}
	return m.reconcileAbsentLinux(ctx)
}

func (m *Manager) reconcileAbsentLinux(ctx context.Context) (Status, error) {
	unknown := Status{Name: m.name, State: StateUnknown}
	properties, err := m.linuxProperties(ctx)
	if err != nil {
		return unknown, err
	}
	if !m.linuxReloadHint(properties) {
		return m.linuxObservedStatus(properties, false)
	}
	// The cache hint permits only a global refresh, never a targeted control
	// or adoption of the vanished definition. There is no retry loop.
	if err = m.requireCommand(ctx, "--user", "daemon-reload"); err != nil {
		return unknown, err
	}
	properties, err = m.linuxProperties(ctx)
	if err != nil {
		return unknown, err
	}
	// Check the file after the refreshed observation too: show can race a
	// separate writer, and fresh cache data never grants file ownership.
	present, err := m.readUnit()
	if err != nil {
		return Status{Name: m.name, Installed: present, State: StateUnknown}, err
	}
	if present {
		return Status{Name: m.name, Installed: true, State: StateUnknown}, ErrServiceChanged
	}
	return m.linuxObservedStatus(properties, false)
}

func (m *Manager) linuxReloadHint(properties map[string]string) bool {
	return properties["LoadState"] == "loaded" && properties["FragmentPath"] == m.unitPath() &&
		properties["DropInPaths"] == "" && linuxInactive(properties) &&
		properties["MainPID"] == "0" && properties["ControlPID"] == "0" &&
		properties["NeedDaemonReload"] == "yes" && validateLinuxSafety(properties) == nil
}
