package host

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"
)

// InstallationHealth is an observation of the original host's selected files,
// not launch authority. It never includes executable contents or environment.
type InstallationHealth struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Role   string `json:"role,omitempty"`
}

const installationObservationTimeout = 100 * time.Millisecond

type installedFile struct {
	path     string
	role     string
	info     os.FileInfo
	captured bool
}

type installationObservation struct {
	files  []installedFile
	health InstallationHealth
}

// The service owns this probe and joins its sole in-flight goroutine under H.
// Timeouts bound observations, never filesystem I/O or the probe's ownership.
type installationProbe struct {
	files   []installedFile
	pending <-chan installationObservation
	stat    func(string) (os.FileInfo, error)
}

func newInstallationProbe(config Config, stat func(string) (os.FileInfo, error)) *installationProbe {
	files := []installedFile{{path: config.Executable, role: "delegate"}, {path: config.RunnerExecutable, role: "delegate_run"}}
	return &installationProbe{files: files, stat: stat}
}

func (p *installationProbe) start() {
	if p.pending != nil {
		return
	}
	done := make(chan installationObservation, 1)
	p.pending = done
	files, stat := slices.Clone(p.files), p.stat
	go func() { done <- observeInstallation(files, stat) }()
}

func observeInstallation(files []installedFile, stat func(string) (os.FileInfo, error)) installationObservation {
	health := InstallationHealth{State: "available"}
	for index, file := range files {
		current, err := stat(file.path)
		observation := installationFileHealth(file, current, err)
		if !file.captured {
			files[index].captured = true
			if observation.State == "available" {
				files[index].info = current
			}
		}
		// Keep the first failed role in the immutable selection order.
		if health.State == "available" && observation.State != "available" {
			health = observation
		}
	}
	return installationObservation{files: files, health: health}
}

func installationFileHealth(file installedFile, current os.FileInfo, err error) InstallationHealth {
	observation := InstallationHealth{State: "degraded", Role: file.role}
	switch {
	case errors.Is(err, os.ErrNotExist):
		observation.Reason = "installation_missing"
	case err != nil:
		observation.State, observation.Reason = "unknown", "installation_observation_incomplete"
	case !current.Mode().IsRegular() || current.Mode().Perm()&0o111 == 0:
		observation.Reason = "installation_unusable"
	case file.captured && file.info == nil:
		observation.State, observation.Reason = "unknown", "installation_baseline_unavailable"
	case file.info != nil && !sameInstalledFile(file.info, current):
		observation.Reason = "installation_changed"
	default:
		return InstallationHealth{State: "available"}
	}
	return observation
}

func sameInstalledFile(before, after os.FileInfo) bool {
	return os.SameFile(before, after) && before.Mode() == after.Mode() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func (s *service) installationHealth(ctx context.Context) InstallationHealth {
	if s.status.StopAccepted {
		return InstallationHealth{State: "unknown", Reason: "installation_host_stopping"}
	}
	owned := s.installation.pending == nil
	s.installation.start()
	timer := time.NewTimer(installationObservationTimeout)
	defer timer.Stop()
	for {
		select {
		case result := <-s.installation.pending:
			baseline := !s.installation.files[0].captured
			s.installation.files, s.installation.pending = result.files, nil
			if baseline || !owned {
				s.installation.start()
				owned = true
				continue
			}
			return result.health
		case <-timer.C:
			return InstallationHealth{State: "unknown", Reason: "installation_probe_pending"}
		case <-ctx.Done():
			return InstallationHealth{State: "unknown", Reason: "installation_probe_pending"}
		}
	}
}

func (s *service) joinInstallationProbe() {
	if s.installation == nil || s.installation.pending == nil {
		return
	}
	for {
		select {
		case <-s.installation.pending:
			s.installation.pending = nil
			return
		case request := <-s.requests:
			s.replyStopping(request)
		}
	}
}

// applyInstallationHealth derives readiness while preserving lifecycle facts.
func applyInstallationHealth(status Status, health InstallationHealth) Status {
	status.Health = &health
	status.Ready = status.HostReady && health.State == "available"
	if status.HostReady && health.State != "available" && !status.Fault {
		status.State, status.LastError = "degraded", health.Reason
	}
	return status
}

func validInstallationHealth(health InstallationHealth) bool {
	role := health.Role == "delegate" || health.Role == "delegate_run"
	switch health.State {
	case "available":
		return health.Reason == "" && health.Role == ""
	case "degraded":
		return role && (health.Reason == "installation_missing" || health.Reason == "installation_changed" || health.Reason == "installation_unusable")
	case "unknown":
		switch health.Reason {
		case "installation_observation_incomplete", "installation_baseline_unavailable":
			return role
		case "installation_probe_pending", "installation_host_stopping":
			return health.Role == ""
		default:
			return false
		}
	default:
		return false
	}
}

func installationHealthFromResponse(response wireHealthResponse) (InstallationHealth, error) {
	if response.Code == "invalid_control" {
		return InstallationHealth{State: "unsupported", Reason: "installation_health_unsupported"}, nil
	}
	if response.Health == nil {
		return InstallationHealth{State: "unknown", Reason: "installation_health_missing"}, nil
	}
	health := *response.Health
	if !validInstallationHealth(health) {
		return InstallationHealth{State: "unknown", Reason: "installation_health_invalid"}, ErrIdentity
	}
	return health, nil
}
