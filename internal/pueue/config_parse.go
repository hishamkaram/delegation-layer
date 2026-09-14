package pueue

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	maxYAMLDepth         = 64
	maxYAMLNodes         = 16384
	maxYAMLAliases       = 128
	maxYAMLExpandedNodes = 65536
)

// ParseConfig reads one bounded document without applying YAML merge keys or
// selecting a profile. No filesystem or environment is consulted.
func ParseConfig(data []byte) (*Config, error) {
	if len(data) == 0 || len(data) > MaxControlBytes {
		return nil, ErrConfiguration
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := d.Decode(&document); err != nil {
		return nil, fmt.Errorf("%w: YAML syntax: %w", ErrConfiguration, err)
	}
	var extra yaml.Node
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: expected one complete document", ErrConfiguration)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: root must be a mapping", ErrConfiguration)
	}
	root := document.Content[0]
	if err := validateYAMLGraph(root); err != nil {
		return nil, err
	}
	if err := validateYAMLType(root, reflect.TypeFor[rawSettings](), false, "config"); err != nil {
		return nil, err
	}
	var raw rawSettings
	if err := root.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: typed YAML: %w", ErrConfiguration, err)
	}
	if err := validateEditModes(raw); err != nil {
		return nil, err
	}
	return &Config{Client: clientDefaults(raw.Client), Daemon: daemonDefaults(raw.Daemon), Shared: sharedDefaults(raw.Shared)}, nil
}

func yamlError(n *yaml.Node, path, reason string) error {
	return fmt.Errorf("%w: %s at line %d: %s", ErrConfiguration, path, n.Line, reason)
}

type yamlVisit struct {
	node  *yaml.Node
	depth int
}

func validateYAMLGraph(root *yaml.Node) error {
	stack := []yamlVisit{{root, 1}}
	anchors := map[string]bool{}
	nodes, aliases := 0, 0
	for len(stack) != 0 {
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		nodes++
		if nodes > maxYAMLNodes || v.depth > maxYAMLDepth {
			return yamlError(v.node, "config", "node/depth limit exceeded")
		}
		if err := validateYAMLNode(v.node, anchors); err != nil {
			return err
		}
		if v.node.Kind == yaml.AliasNode {
			aliases++
		}
		if aliases > maxYAMLAliases {
			return yamlError(v.node, "config", "alias limit exceeded")
		}
		for _, child := range v.node.Content {
			stack = append(stack, yamlVisit{child, v.depth + 1})
		}
	}
	count := 0
	return validateYAMLExpansion(root, 1, &count, map[*yaml.Node]bool{})
}

func validateYAMLNode(n *yaml.Node, anchors map[string]bool) error {
	if n.Anchor != "" {
		if anchors[n.Anchor] {
			return yamlError(n, "config", "duplicate anchor")
		}
		anchors[n.Anchor] = true
	}
	if n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return yamlError(n, "config", "undefined alias")
		}
		return nil
	}
	if !allowedYAMLTag(n.Tag) {
		return yamlError(n, "config", "unsupported YAML tag")
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	keys := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		key := n.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return yamlError(key, "config", "mapping keys must be literal strings")
		}
		if key.Value == "<<" {
			return yamlError(key, "config", "merge keys are unsupported")
		}
		if keys[key.Value] {
			return yamlError(key, "config", "duplicate mapping key")
		}
		keys[key.Value] = true
	}
	return nil
}

func allowedYAMLTag(tag string) bool {
	switch tag {
	case "!!str", "!!bool", "!!int", "!!null", "!!map", "!!seq":
		return true
	default:
		return false
	}
}

func validateYAMLExpansion(n *yaml.Node, depth int, count *int, active map[*yaml.Node]bool) error {
	*count++
	if *count > maxYAMLExpandedNodes || depth > maxYAMLDepth {
		return yamlError(n, "config", "expanded node/depth limit exceeded")
	}
	if active[n] {
		return yamlError(n, "config", "cyclic alias")
	}
	active[n] = true
	defer delete(active, n)
	if n.Kind == yaml.AliasNode {
		return validateYAMLExpansion(n.Alias, depth+1, count, active)
	}
	for _, child := range n.Content {
		if err := validateYAMLExpansion(child, depth+1, count, active); err != nil {
			return err
		}
	}
	return nil
}

func validateYAMLType(n *yaml.Node, typ reflect.Type, nullable bool, path string) error {
	if n.Kind == yaml.AliasNode {
		return validateYAMLType(n.Alias, typ, nullable, path)
	}
	if n.Tag == "!!null" {
		if nullable {
			return nil
		}
		return yamlError(n, path, "null is unsupported for this field")
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		return validateYAMLStruct(n, typ, path)
	case reflect.Map:
		return validateYAMLMap(n, typ, path)
	case reflect.Slice:
		return validateYAMLSequence(n, typ, path)
	case reflect.String:
		return validateYAMLScalar(n, "!!str", path)
	case reflect.Bool:
		return validateYAMLBool(n, path)
	case reflect.Uint32, reflect.Uint64:
		return validateYAMLInteger(n, typ.Bits(), path)
	case reflect.Invalid, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uintptr,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128,
		reflect.Array, reflect.Chan, reflect.Func, reflect.Interface, reflect.Pointer,
		reflect.UnsafePointer:
		return yamlError(n, path, "unsupported schema type")
	}
	return yamlError(n, path, "unsupported schema type")
}

func validateYAMLScalar(n *yaml.Node, tag, path string) error {
	if n.Kind != yaml.ScalarNode || n.Tag != tag {
		return yamlError(n, path, "incorrect scalar type")
	}
	return nil
}

func validateYAMLInteger(n *yaml.Node, bits int, path string) error {
	if err := validateYAMLScalar(n, "!!int", path); err != nil {
		return err
	}
	if quotedYAML(n) {
		return yamlError(n, path, "quoted integer is unsupported")
	}
	s := n.Value
	if strings.Contains(s, "_") || strings.HasPrefix(s, "-") {
		return yamlError(n, path, "unsigned canonical integer required")
	}
	s = strings.TrimPrefix(s, "+")
	base := 10
	if len(s) > 1 && s[0] == '0' {
		if !strings.ContainsRune("oxb", rune(s[1])) {
			return yamlError(n, path, "legacy leading-zero integer is unsupported")
		}
		base = 0
	}
	if _, err := strconv.ParseUint(s, base, bits); err != nil {
		return yamlError(n, path, "integer out of range or malformed")
	}
	return nil
}

func quotedYAML(n *yaml.Node) bool {
	return n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0
}

func validateYAMLBool(n *yaml.Node, path string) error {
	if err := validateYAMLScalar(n, "!!bool", path); err != nil {
		return err
	}
	if quotedYAML(n) {
		return yamlError(n, path, "quoted boolean is unsupported")
	}
	switch n.Value {
	case "true", "True", "TRUE", "false", "False", "FALSE":
		return nil
	default:
		return yamlError(n, path, "legacy boolean spelling is unsupported")
	}
}

func validateYAMLStruct(n *yaml.Node, typ reflect.Type, path string) error {
	if n.Kind != yaml.MappingNode {
		return yamlError(n, path, "mapping required")
	}
	fields := map[string]reflect.StructField{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		fields[f.Tag.Get("yaml")] = f
	}
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		field, ok := fields[key.Value]
		if !ok {
			return yamlError(key, path, "unknown field "+key.Value)
		}
		if err := validateYAMLType(value, field.Type, field.Tag.Get("nullable") == "true", path+"."+key.Value); err != nil {
			return err
		}
	}
	return nil
}

func validateYAMLMap(n *yaml.Node, typ reflect.Type, path string) error {
	if n.Kind != yaml.MappingNode {
		return yamlError(n, path, "mapping required")
	}
	for i := 0; i < len(n.Content); i += 2 {
		if err := validateYAMLType(n.Content[i+1], typ.Elem(), false, path+"."+n.Content[i].Value); err != nil {
			return err
		}
	}
	return nil
}

func validateYAMLSequence(n *yaml.Node, typ reflect.Type, path string) error {
	if n.Kind != yaml.SequenceNode {
		return yamlError(n, path, "sequence required")
	}
	for _, child := range n.Content {
		if err := validateYAMLType(child, typ.Elem(), false, path); err != nil {
			return err
		}
	}
	return nil
}

func validateEditModes(raw rawSettings) error {
	clients := []*rawClient{raw.Client}
	for _, p := range raw.Profiles {
		clients = append(clients, p.Client)
	}
	for _, c := range clients {
		if c != nil && c.EditMode != nil && *c.EditMode != "toml" && *c.EditMode != "files" {
			return fmt.Errorf("%w: edit_mode must be toml or files", ErrConfiguration)
		}
	}
	return nil
}
