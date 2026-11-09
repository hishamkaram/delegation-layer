package sshtransport

import (
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/sshconfig"
	"golang.org/x/crypto/ssh"
)

const (
	MaxEnvironmentVariables  = 128
	MaxEnvironmentEntryBytes = 32 << 10
	MaxEnvironmentBytes      = 256 << 10
)

// EnvironmentVariable is an explicitly selected SSH environment request.
// Values are transport data and must never be included in diagnostics.
type EnvironmentVariable struct{ Name, Value string }

// Diagnostic contains only a bounded classification and optional count.
type Diagnostic struct {
	Code  string `json:"code"`
	Count int    `json:"count,omitempty"`
}

// SelectEnvironment applies configuration names to an immutable invocation
// snapshot and validates all selected data before any network or agent access.
func SelectEnvironment(connection sshconfig.Connection, environment []string) ([]EnvironmentVariable, error) {
	var selected []EnvironmentVariable
	seen := make(map[string]bool)
	total := 0
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if !connection.SendsEnvironment(name) {
			continue
		}
		if !found || name == "" || strings.ContainsAny(name, "\x00\r\n") || strings.ContainsRune(value, '\x00') || seen[name] {
			return nil, &Error{Code: "ssh_environment_invalid"}
		}
		seen[name] = true
		size := len(name) + 1 + len(value)
		if size > MaxEnvironmentEntryBytes || len(selected) >= MaxEnvironmentVariables || total > MaxEnvironmentBytes-size {
			return nil, &Error{Code: "ssh_environment_limit"}
		}
		total += size
		selected = append(selected, EnvironmentVariable{Name: name, Value: value})
	}
	return selected, nil
}

// Diagnostics returns a copy of this exchange's observations. Call after Open
// to include environment requests the server declined.
func (c *Client) Diagnostics() []Diagnostic { return append([]Diagnostic(nil), c.diagnostics...) }

func (c *Client) sendEnvironment(session *ssh.Session) error {
	refused := 0
	for _, variable := range c.environment {
		payload := ssh.Marshal(struct{ Name, Value string }{variable.Name, variable.Value})
		accepted, err := session.SendRequest("env", true, payload)
		if err != nil {
			return err
		}
		if !accepted {
			refused++
		}
	}
	if refused > 0 {
		c.diagnostics = append(c.diagnostics, Diagnostic{Code: "ssh_env_refused", Count: refused})
	}
	return nil
}
