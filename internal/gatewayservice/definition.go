package gatewayservice

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
)

const maxServiceDefinition = 128 * 1024

// The installed file owns PATH. Recover only that value, then use the sole
// renderer to verify every byte of the complete saved service definition.
func (m *Manager) verifyDefinition(data []byte) error {
	path, err := installedPath(m.config.Platform, data)
	if err != nil || !validPath(path) {
		return ErrForeignService
	}
	installed := m.config
	installed.Path = path
	expected, err := render(installed, m.name)
	if err != nil || !bytes.Equal(data, expected) {
		return ErrForeignService
	}
	return nil
}

func installedPath(platform Platform, data []byte) (string, error) {
	if len(data) > maxServiceDefinition {
		return "", ErrForeignService
	}
	if platform == Linux {
		return installedLinuxPath(data)
	}
	const prefix = "<key>EnvironmentVariables</key><dict><key>PATH</key>"
	_, value, found := strings.Cut(string(data), prefix)
	if !found {
		return "", ErrForeignService
	}
	value, _, found = strings.Cut(value, "</dict><key>AbandonProcessGroup</key>")
	if !found {
		return "", ErrForeignService
	}
	var path string
	if err := xml.Unmarshal([]byte(value), &path); err != nil {
		return "", ErrForeignService
	}
	return path, nil
}

func installedLinuxPath(data []byte) (string, error) {
	var result string
	found := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "Environment=") {
			continue
		}
		if found {
			return "", ErrForeignService
		}
		value, err := strconv.Unquote(strings.TrimPrefix(line, "Environment="))
		if err != nil || !strings.HasPrefix(value, "PATH=") {
			return "", ErrForeignService
		}
		found = true
		result = strings.ReplaceAll(strings.TrimPrefix(value, "PATH="), "%%", "%")
	}
	if !found {
		return "", ErrForeignService
	}
	return result, nil
}
