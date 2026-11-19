package sshtransport

import (
	"errors"
	"path"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"golang.org/x/crypto/ssh"
)

const curve25519LibSSHAlias = "curve25519-sha256@libssh.org"

// compileAlgorithms prepares the exact lists the backend can send. An omitted
// family stays unset so the backend applies its usual defaults at handshake.
func compileAlgorithms(policy sshconfig.AlgorithmPolicy) (ssh.Config, error) {
	var config, defaults, normalized ssh.Config
	if policy.Ciphers.Names == nil && policy.KeyExchanges.Names == nil && policy.MACs.Names == nil {
		return config, nil
	}
	defaults.SetDefaults()
	supported := ssh.SupportedAlgorithms()
	// The backend knows this alias but only advertises the canonical name in
	// SupportedAlgorithms. Check the backend supports the wire alias.
	if slices.Contains(supported.KeyExchanges, ssh.KeyExchangeCurve25519) && slices.Contains(defaults.KeyExchanges, curve25519LibSSHAlias) {
		supported.KeyExchanges = append(supported.KeyExchanges, curve25519LibSSHAlias)
	}
	families := [...]struct {
		list                sshconfig.AlgorithmList
		defaults, supported []string
		result, wire        *[]string
	}{
		{policy.Ciphers, defaults.Ciphers, supported.Ciphers, &config.Ciphers, &normalized.Ciphers},
		{policy.KeyExchanges, defaults.KeyExchanges, supported.KeyExchanges, &config.KeyExchanges, &normalized.KeyExchanges},
		{policy.MACs, defaults.MACs, supported.MACs, &config.MACs, &normalized.MACs},
	}
	for _, family := range families {
		selected, err := applyAlgorithmList(family.list, family.defaults, family.supported)
		if err != nil {
			return ssh.Config{}, err
		}
		*family.result = selected
	}

	// SetDefaults also filters nonempty lists and may add a Curve25519 alias.
	// A configured family must survive exactly, including wire order.
	normalized = config
	normalized.SetDefaults()
	for _, family := range families {
		if *family.result != nil && !slices.Equal(*family.result, *family.wire) {
			return ssh.Config{}, family.list.Error("ssh_algorithm_policy_unrepresentable", errors.New("the SSH backend changes the configured wire list"))
		}
	}
	return config, nil
}

func applyAlgorithmList(list sshconfig.AlgorithmList, defaults, supported []string) ([]string, error) {
	if list.Names == nil {
		return nil, nil
	}
	if len(list.Names) == 0 {
		return nil, list.Error("invalid_ssh_setting", errors.New("SSH algorithm list is empty"))
	}
	if list.Modifier == '-' {
		remaining, err := removeAlgorithms(defaults, list.Names)
		if err != nil {
			return nil, list.Error("invalid_ssh_setting", err)
		}
		if len(remaining) == 0 {
			return nil, list.Error("invalid_ssh_setting", errors.New("SSH algorithm removal leaves no algorithms"))
		}
		return remaining, nil
	}
	for _, name := range list.Names {
		if !slices.Contains(supported, name) {
			return nil, list.Error("unsupported_ssh_algorithm_policy", errors.New("explicit algorithm is unsupported or insecure"))
		}
	}
	selected := list.Names
	switch list.Modifier {
	case 0:
	case '+':
		selected = slices.Concat(defaults, selected)
	case '^':
		selected = slices.Concat(selected, defaults)
	default:
		return nil, list.Error("invalid_ssh_setting", errors.New("invalid SSH algorithm list modifier"))
	}
	unique := make([]string, 0, len(selected))
	for _, name := range selected {
		if !slices.Contains(unique, name) {
			unique = append(unique, name)
		}
	}
	return unique, nil
}

func removeAlgorithms(defaults, patterns []string) ([]string, error) {
	remaining := make([]string, 0, len(defaults))
candidate:
	for _, name := range defaults {
		for _, pattern := range patterns {
			matches, err := path.Match(pattern, name)
			if err != nil {
				return nil, err
			}
			if matches {
				continue candidate
			}
		}
		remaining = append(remaining, name)
	}
	return remaining, nil
}
