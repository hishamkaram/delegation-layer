// Package sshconfig resolves the explicitly supported, non-executing subset of
// OpenSSH configuration. It never contacts a host or invokes configuration helpers.
package sshconfig

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	maxDepth = 16
	maxFiles = 128
	maxBytes = 4 * 1024 * 1024
)

type Options struct {
	ConfigFile                                string
	Home, UserConfig, SystemConfig, LocalUser string
	ReadFile                                  func(string) ([]byte, error)
}
type directive struct {
	key    string
	values []string
	file   string
	line   int
}
type Config struct {
	options    Options
	directives []directive
	aliases    []string
}

// Connection is the resolved connection intent, not a readiness claim.
type Connection struct {
	Alias, HostName, User                      string
	Port                                       int
	IdentityFiles                              []string
	IdentitiesOnly                             bool
	IdentityAgent                              string
	UserKnownHostsFiles, GlobalKnownHostsFiles []string
	HostKeyAlias                               string
	SendEnv                                    []string
	GSSAPIAuthentication                       bool
	PubkeyAuthentication                       string
	PreferredAuthentications                   []string
}

func Load(options Options) (*Config, error) {
	if options.Home == "" {
		return nil, &Error{Code: "ssh_home_unavailable"}
	}
	if options.ReadFile == nil {
		options.ReadFile = readConfigFile
	}
	if options.UserConfig == "" {
		options.UserConfig = filepath.Join(options.Home, ".ssh", "config")
	}
	if options.SystemConfig == "" {
		options.SystemConfig = "/etc/ssh/ssh_config"
	}
	c := &Config{options: options}
	p := parser{config: c, stack: map[string]bool{}}
	sources := []struct {
		path, root string
		optional   bool
	}{{options.UserConfig, filepath.Join(options.Home, ".ssh"), true}, {options.SystemConfig, "/etc/ssh", true}}
	if options.ConfigFile != "" {
		if !filepath.IsAbs(options.ConfigFile) {
			return nil, &Error{Code: "invalid_ssh_config_path", File: options.ConfigFile}
		}
		sources = []struct {
			path, root string
			optional   bool
		}{{options.ConfigFile, filepath.Join(options.Home, ".ssh"), false}}
	}
	for _, source := range sources {
		c.directives = append(c.directives, directive{key: "source-start"})
		if err := p.file(source.path, source.root, 0, source.optional); err != nil {
			return nil, err
		}
	}

	sort.Strings(c.aliases)
	c.aliases = unique(c.aliases)
	return c, nil
}

func readConfigFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("SSH configuration file exceeds byte limit")
	}
	return data, nil
}

func (c *Config) Aliases() []string { return append([]string(nil), c.aliases...) }

type parser struct {
	config       *Config
	files, bytes int
	stack        map[string]bool
}

func (p *parser) file(name, root string, depth int, optional bool) error {
	if depth >= maxDepth {
		return &Error{Code: "ssh_config_include_depth", File: name}
	}
	name = filepath.Clean(name)
	if p.stack[name] {
		return &Error{Code: "ssh_config_include_cycle", File: name}
	}
	data, err := p.config.options.ReadFile(name)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return nil
		}
		return &Error{Code: "ssh_config_read_failed", File: name, cause: err}
	}
	p.files++
	p.bytes += len(data)
	if p.files > maxFiles || p.bytes > maxBytes {
		return &Error{Code: "ssh_config_limit", File: name}
	}
	p.stack[name] = true
	defer delete(p.stack, name)
	return p.parse(data, name, root, depth)
}

func (p *parser) parse(data []byte, name, root string, depth int) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxBytes)
	line := 0
	for scanner.Scan() {
		line++
		words, parseErr := split(scanner.Text())
		if parseErr != nil {
			return &Error{Code: "ssh_config_parse_failed", File: name, Line: line, cause: parseErr}
		}
		if len(words) == 0 {
			continue
		}
		key, values := strings.ToLower(words[0]), words[1:]
		if len(values) == 0 {
			return &Error{Code: "invalid_ssh_setting", File: name, Line: line, Directive: key}
		}
		if key == "include" {
			p.config.directives = append(p.config.directives, directive{key: "include-begin"})
			if includeErr := p.include(values, root, depth); includeErr != nil {
				return settingError(directive{key: key, file: name, line: line}, includeErr)
			}
			p.config.directives = append(p.config.directives, directive{key: "include-end"})
			continue
		}
		if key == "host" {
			p.addAliases(values)
		}
		p.config.directives = append(p.config.directives, directive{key: key, values: values, file: name, line: line})
	}
	if err := scanner.Err(); err != nil {
		return &Error{Code: "ssh_config_parse_failed", File: name, Line: line, cause: err}
	}
	return nil
}

func (p *parser) addAliases(values []string) {
	for _, alias := range values {
		if !strings.ContainsAny(alias, "*?!") {
			p.config.aliases = append(p.config.aliases, alias)
		}
	}
}

func (p *parser) include(values []string, root string, depth int) error {
	for _, pattern := range values {
		expanded, err := expandPath(pattern, p.config.options.Home)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(expanded) {
			expanded = filepath.Join(root, expanded)
		}
		paths, err := filepath.Glob(expanded)
		if err != nil {
			return fmt.Errorf("invalid Include: %w", err)
		}
		for _, path := range paths {
			if err = p.file(path, root, depth+1, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func split(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	quoted, escape, started := false, false, false
	for _, r := range line {
		if escape {
			word.WriteRune(r)
			escape = false
			started = true
			continue
		}
		if r == '\\' {
			escape = true
			started = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			started = true
			continue
		}
		if !quoted && r == '#' {
			break
		}
		if !quoted && (r == ' ' || r == '\t' || r == '=') {
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if quoted || escape {
		return nil, fmt.Errorf("unterminated quote or escape")
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}

func matches(patterns []string, host string) bool {
	positive := false
	for _, pattern := range patterns {
		neg := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(pattern, "!")
		ok := hostPattern(strings.ToLower(pattern), strings.ToLower(host))
		if ok && neg {
			return false
		}
		positive = positive || ok
	}
	return positive
}

// hostPattern implements only OpenSSH's * and ? wildcards. Brackets are literal.
func hostPattern(pattern, value string) bool {
	p, v, star, retry := 0, 0, -1, 0
	for v < len(value) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == value[v]):
			p++
			v++
		case p < len(pattern) && pattern[p] == '*':
			star = p
			p++
			retry = v
		case star >= 0:
			retry++
			v = retry
			p = star + 1
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// selected resolves the lexical Host scope, including conditional Includes.
// A selected Include can change the surrounding Host scope just like inline text.
func selected(d directive, alias string, active *bool, skipped *int) bool {
	switch d.key {
	case "source-start":
		*active = true
		return false
	case "include-begin":
		if !*active || *skipped > 0 {
			*skipped++
		}
		return false
	case "include-end":
		if *skipped > 0 {
			*skipped--
		}
		return false
	}
	if *skipped > 0 {
		return false
	}
	switch d.key {
	case "host":
		*active = matches(d.values, alias)
		return false
	case "match":
		return true
	}
	return *active
}

func (c *Config) Resolve(alias string) (Connection, error) {
	if alias == "" || strings.ContainsAny(alias, "/\\ \t\r\n\x00") {
		return Connection{}, &Error{Code: "invalid_ssh_alias"}
	}
	r := Connection{Alias: alias, HostName: alias, User: c.options.LocalUser, Port: 22, IdentityAgent: os.Getenv("SSH_AUTH_SOCK")}
	seen := map[string]bool{}
	sources := map[string]directive{}
	active, skipped := true, 0
	for _, d := range c.directives {
		if !selected(d, alias, &active, &skipped) {
			continue
		}
		previouslySeen := seen[d.key]
		if err := apply(&r, d, seen); err != nil {
			return Connection{}, settingError(d, err)
		}
		if !previouslySeen || d.key == "identityfile" || d.key == "sendenv" {
			sources[d.key] = d
			for _, value := range d.values {
				sources[d.key+"\x00"+value] = d
			}
		}
	}
	if r.User == "" {
		return Connection{}, &Error{Code: "ssh_user_unconfigured"}
	}
	var err error
	r.HostName, err = expand(r.HostName, r, c.options.Home, true)
	if err != nil {
		return Connection{}, expansionSettingError(sources, "hostname", r.HostName, err)
	}
	if r.HostName == "" || strings.ContainsAny(r.HostName, " \t\r\n\x00") {
		return Connection{}, expansionSettingError(sources, "hostname", r.HostName, fmt.Errorf("invalid SSH hostname"))
	}
	c.applyDefaultFiles(&r, seen)
	expanded, expandErr := c.expandConnection(r, sources)
	if expandErr != nil {
		return Connection{}, expandErr
	}
	return expanded, nil
}

func (c *Config) applyDefaultFiles(r *Connection, seen map[string]bool) {
	if !seen["identityfile"] {
		r.IdentityFiles = []string{filepath.Join(c.options.Home, ".ssh", "id_ed25519"), filepath.Join(c.options.Home, ".ssh", "id_ecdsa"), filepath.Join(c.options.Home, ".ssh", "id_rsa")}
	}
	if !seen["userknownhostsfile"] {
		r.UserKnownHostsFiles = []string{filepath.Join(c.options.Home, ".ssh", "known_hosts")}
	}
	if !seen["globalknownhostsfile"] {
		r.GlobalKnownHostsFiles = []string{"/etc/ssh/ssh_known_hosts"}
	}
}

func (c *Config) expandConnection(r Connection, sources map[string]directive) (Connection, error) {
	var err error
	for _, field := range []struct {
		key    string
		values *[]string
	}{{"identityfile", &r.IdentityFiles}, {"userknownhostsfile", &r.UserKnownHostsFiles}, {"globalknownhostsfile", &r.GlobalKnownHostsFiles}} {
		for i, path := range *field.values {
			(*field.values)[i], err = expand(path, r, c.options.Home, false)
			if err != nil {
				return Connection{}, expansionSettingError(sources, field.key, path, err)
			}
		}
	}
	if r.IdentityAgent == "SSH_AUTH_SOCK" {
		r.IdentityAgent = os.Getenv("SSH_AUTH_SOCK")
	}
	if r.IdentityAgent != "" && r.IdentityAgent != "none" {
		path := r.IdentityAgent
		r.IdentityAgent, err = expand(path, r, c.options.Home, false)
		if err != nil {
			return Connection{}, expansionSettingError(sources, "identityagent", path, err)
		}
	}
	return r, nil
}

func apply(r *Connection, d directive, seen map[string]bool) error {
	key := d.key
	if key != "identityfile" && key != "sendenv" && seen[key] {
		return nil
	}
	single := func() (string, error) {
		if len(d.values) != 1 {
			return "", fmt.Errorf("%s requires one value", key)
		}
		return d.values[0], nil
	}
	switch key {
	case "hostname", "user", "port", "identityagent", "identitiesonly", "hostkeyalias", "gssapiauthentication", "pubkeyauthentication", "preferredauthentications":
		v, err := single()
		if err != nil {
			return err
		}
		seen[key] = true
		return applyScalar(r, key, v)

	case "sendenv":
		return applySendEnv(r, d.values)
	case "identityfile":
		v, err := single()
		if err != nil {
			return err
		}
		if v != "none" {
			r.IdentityFiles = append(r.IdentityFiles, v)
		}
	case "userknownhostsfile":
		r.UserKnownHostsFiles = withoutNone(d.values)
	case "globalknownhostsfile":
		r.GlobalKnownHostsFiles = withoutNone(d.values)
	case "loglevel", "hashknownhosts", "controlmaster", "controlpersist", "controlpath": // Presentation and connection sharing do not affect this private channel.
	default:
		return &Error{Code: "unsupported_ssh_directive", Directive: key}
	}
	seen[key] = true
	return nil
}

func applyScalar(r *Connection, key, v string) error {
	switch key {
	case "hostname":
		r.HostName = v
	case "user":
		r.User = v
	case "port":
		port, e := strconv.Atoi(v)
		if e != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid SSH port")
		}
		r.Port = port
	case "identityagent":
		r.IdentityAgent = v
	case "hostkeyalias":
		r.HostKeyAlias = v
	case "gssapiauthentication", "pubkeyauthentication":
		return applyAuthenticationToggle(r, key, v)
	case "preferredauthentications":
		methods, err := authenticationMethods(v)
		if err != nil {
			return err
		}
		r.PreferredAuthentications = methods
	case "identitiesonly":
		if v != "yes" && v != "no" {
			return fmt.Errorf("invalid IdentitiesOnly")
		}
		r.IdentitiesOnly = v == "yes"
	}
	return nil
}

func withoutNone(v []string) []string {
	var r []string
	for _, s := range v {
		if s != "none" {
			r = append(r, s)
		}
	}
	return r
}

func expandPath(path, home string) (string, error) {
	if strings.Contains(path, "$") || strings.Contains(path, "%") {
		return "", fmt.Errorf("unsupported SSH path expansion")
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:]), nil
	}
	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("unsupported named-user home expansion")
	}
	return path, nil
}

func expand(value string, r Connection, home string, hostname bool) (string, error) {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '%' {
			b.WriteByte(value[i])
			continue
		}
		i++
		if i == len(value) {
			return "", fmt.Errorf("incomplete SSH token")
		}
		var replacement string
		switch value[i] {
		case '%':
			replacement = "%"
		case 'h':
			replacement = r.HostName
			if hostname {
				replacement = r.Alias
			}
		case 'n':
			replacement = r.Alias
		case 'p':
			replacement = strconv.Itoa(r.Port)
		case 'r':
			replacement = r.User
		case 'd':
			replacement = home
		default:
			return "", fmt.Errorf("unsupported SSH token %%%c", value[i])
		}
		if hostname && value[i] != '%' && value[i] != 'h' {
			return "", fmt.Errorf("unsupported HostName token")
		}
		b.WriteString(replacement)
	}
	return expandedPath(b.String(), home)
}

func expandedPath(result, home string) (string, error) {
	if strings.Contains(result, "$") {
		return "", fmt.Errorf("unsupported SSH environment expansion")
	}
	if strings.HasPrefix(result, "~/") {
		return filepath.Join(home, result[2:]), nil
	}
	if strings.HasPrefix(result, "~") {
		return "", fmt.Errorf("unsupported named-user home expansion")
	}
	return result, nil
}

func unique(values []string) []string {
	var result []string
	for _, v := range values {
		if len(result) == 0 || result[len(result)-1] != v {
			result = append(result, v)
		}
	}
	return result
}
