package sshconfig

import (
	"errors"
	"fmt"
	"strings"
)

// Error describes the configuration source without printing configuration values.
// The parser owns the source and callers use Code to choose actionable guidance.
type Error struct {
	Code      string
	File      string
	Line      int
	Directive string
	cause     error
}

func (e *Error) Error() string {
	source := e.File
	if e.Line > 0 {
		source = fmt.Sprintf("%s:%d", source, e.Line)
	}
	if source != "" {
		source += ": "
	}
	if e.Directive != "" {
		return source + e.Code + " (" + e.Directive + ")"
	}
	return source + e.Code
}
func (e *Error) Unwrap() error { return e.cause }

func settingError(d directive, cause error) error {
	var configured *Error
	if errors.As(cause, &configured) {
		result := *configured
		if result.File == "" {
			result.File, result.Line = d.file, d.line
		}
		return &result
	}
	return &Error{Code: "invalid_ssh_setting", File: d.file, Line: d.line, Directive: d.key, cause: cause}
}

func authenticationMethods(value string) ([]string, error) {
	methods := strings.Split(value, ",")
	for _, method := range methods {
		switch method {
		case "gssapi-with-mic", "hostbased", "publickey", "keyboard-interactive", "password", "none":
		default:
			return nil, fmt.Errorf("invalid authentication method")
		}
	}
	return methods, nil
}

func applyAuthenticationToggle(c *Connection, key, value string) error {
	if value != "yes" && value != "no" {
		return fmt.Errorf("authentication setting requires yes or no")
	}
	if key == "gssapiauthentication" {
		c.GSSAPIAuthentication = value == "yes"
	} else {
		c.PubkeyAuthentication = value
	}
	return nil
}

// PublicKeyAllowed applies the effective method selection. Zero-value connections
// preserve normal key authentication for callers that construct them directly.
func (c Connection) PublicKeyAllowed() bool {
	if c.PubkeyAuthentication != "" && c.PubkeyAuthentication != "yes" {
		return false
	}
	return c.authenticationAllowed("publickey")
}

// GSSAPISelected reports whether effective configuration selects this method.
// It does not claim that the embedded transport implements Kerberos.
func (c Connection) GSSAPISelected() bool {
	return c.GSSAPIAuthentication && c.authenticationAllowed("gssapi-with-mic")
}

func (c Connection) authenticationAllowed(selected string) bool {
	if len(c.PreferredAuthentications) == 0 {
		return true
	}
	for _, method := range c.PreferredAuthentications {
		if method == selected {
			return true
		}
	}
	return false
}

func applySendEnv(c *Connection, patterns []string) error {
	for _, pattern := range patterns {
		remove := strings.HasPrefix(pattern, "-")
		if remove {
			pattern = strings.TrimPrefix(pattern, "-")
		}
		if pattern == "" || strings.ContainsAny(pattern, "=\x00\r\n") {
			return fmt.Errorf("invalid SendEnv pattern")
		}
		if !remove {
			c.SendEnv = append(c.SendEnv, pattern)
			continue
		}
		kept := c.SendEnv[:0]
		for _, previous := range c.SendEnv {
			if !hostPattern(pattern, previous) {
				kept = append(kept, previous)
			}
		}
		c.SendEnv = kept
	}
	return nil
}

// SendsEnvironment uses OpenSSH's case-sensitive * and ? name patterns.
func (c Connection) SendsEnvironment(name string) bool {
	for _, pattern := range c.SendEnv {
		if hostPattern(pattern, name) {
			return true
		}
	}
	return false
}

func expansionSettingError(sources map[string]directive, key, value string, cause error) error {
	source, ok := sources[key+"\x00"+value]
	if !ok {
		source = sources[key]
	}
	return &Error{Code: "invalid_ssh_expansion", File: source.file, Line: source.line, Directive: key, cause: cause}
}
