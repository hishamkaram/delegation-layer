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
}

func Load(options Options) (*Config, error) {
	if options.Home == "" {
		return nil, fmt.Errorf("SSH configuration requires a home directory")
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
			return nil, fmt.Errorf("SSH config file must be absolute")
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
		return fmt.Errorf("SSH Include exceeds depth %d", maxDepth)
	}
	name = filepath.Clean(name)
	if p.stack[name] {
		return fmt.Errorf("SSH Include cycle at %s", name)
	}
	data, err := p.config.options.ReadFile(name)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading SSH configuration %s: %w", name, err)
	}
	p.files++
	p.bytes += len(data)
	if p.files > maxFiles || p.bytes > maxBytes {
		return fmt.Errorf("SSH configuration exceeds file or byte limit")
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
			return fmt.Errorf("%s:%d: %w", name, line, parseErr)
		}
		if len(words) == 0 {
			continue
		}
		key, values := strings.ToLower(words[0]), words[1:]
		if len(values) == 0 {
			return fmt.Errorf("%s:%d: missing value", name, line)
		}
		if key == "include" {
			p.config.directives = append(p.config.directives, directive{key: "include-begin"})
			if includeErr := p.include(values, root, depth); includeErr != nil {
				return fmt.Errorf("%s:%d: %w", name, line, includeErr)
			}
			p.config.directives = append(p.config.directives, directive{key: "include-end"})
			continue
		}
		if key == "host" {
			p.addAliases(values)
		}
		p.config.directives = append(p.config.directives, directive{key: key, values: values, file: name, line: line})
	}
	return scanner.Err()
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
		return Connection{}, fmt.Errorf("invalid SSH alias")
	}
	r := Connection{Alias: alias, HostName: alias, User: c.options.LocalUser, Port: 22, IdentityAgent: os.Getenv("SSH_AUTH_SOCK")}
	seen := map[string]bool{}
	active, skipped := true, 0
	for _, d := range c.directives {
		if !selected(d, alias, &active, &skipped) {
			continue
		}
		if err := apply(&r, d, seen); err != nil {
			return Connection{}, fmt.Errorf("%s:%d: %w", d.file, d.line, err)
		}
	}
	if r.User == "" {
		return Connection{}, fmt.Errorf("SSH user is not configured")
	}
	var err error
	r.HostName, err = expand(r.HostName, r, c.options.Home, true)
	if err != nil {
		return Connection{}, err
	}
	if r.HostName == "" || strings.ContainsAny(r.HostName, " \t\r\n\x00") {
		return Connection{}, fmt.Errorf("invalid SSH hostname")
	}
	if !seen["identityfile"] {
		r.IdentityFiles = []string{filepath.Join(c.options.Home, ".ssh", "id_ed25519"), filepath.Join(c.options.Home, ".ssh", "id_ecdsa"), filepath.Join(c.options.Home, ".ssh", "id_rsa")}
	}
	if !seen["userknownhostsfile"] {
		r.UserKnownHostsFiles = []string{filepath.Join(c.options.Home, ".ssh", "known_hosts")}
	}
	if !seen["globalknownhostsfile"] {
		r.GlobalKnownHostsFiles = []string{"/etc/ssh/ssh_known_hosts"}
	}
	return c.expandConnection(r)
}

func (c *Config) expandConnection(r Connection) (Connection, error) {
	var err error
	for _, list := range []*[]string{&r.IdentityFiles, &r.UserKnownHostsFiles, &r.GlobalKnownHostsFiles} {
		for i, path := range *list {
			(*list)[i], err = expand(path, r, c.options.Home, false)
			if err != nil {
				return Connection{}, err
			}
		}
	}
	if r.IdentityAgent == "SSH_AUTH_SOCK" {
		r.IdentityAgent = os.Getenv("SSH_AUTH_SOCK")
	}
	if r.IdentityAgent != "" && r.IdentityAgent != "none" {
		r.IdentityAgent, err = expand(r.IdentityAgent, r, c.options.Home, false)
	}
	return r, err
}

func apply(r *Connection, d directive, seen map[string]bool) error {
	key := d.key
	if key != "identityfile" && seen[key] {
		return nil
	}
	single := func() (string, error) {
		if len(d.values) != 1 {
			return "", fmt.Errorf("%s requires one value", key)
		}
		return d.values[0], nil
	}
	switch key {
	case "hostname", "user", "port", "identityagent", "identitiesonly", "hostkeyalias":
		v, err := single()
		if err != nil {
			return err
		}
		seen[key] = true
		return applyScalar(r, key, v)

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
		return fmt.Errorf("unsupported SSH directive %s; use a direct supported SSH configuration", key)
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
