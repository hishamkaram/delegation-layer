package protocol

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ConfigWithRepairEditor derives a file-edit config whose first process is the
// managed editor. Profiles and the original document remain unchanged.
func ConfigWithRepairEditor(data []byte, shellCommand []string) ([]byte, error) {
	if len(shellCommand) != 3 || shellCommand[0] == "" || shellCommand[1] == "" || shellCommand[2] != "{{ pueue_command_string }}" {
		return nil, ErrConfiguration
	}
	for _, argument := range shellCommand[:2] {
		if strings.Contains(argument, "{{") || strings.ContainsRune(argument, 0) {
			return nil, ErrConfiguration
		}
	}
	files, err := ConfigWithFileEditMode(data)
	if err != nil {
		return nil, err
	}
	var document yaml.Node
	if err = yaml.Unmarshal(files, &document); err != nil {
		return nil, fmt.Errorf("%w: decode repair config: %w", ErrConfiguration, err)
	}
	root := document.Content[0]
	daemon := repairConfigMapping(root, "daemon")
	shell := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, argument := range shellCommand {
		shell.Content = append(shell.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: argument})
	}
	setRepairConfigValue(daemon, "shell_command", shell)
	setRepairConfigValue(daemon, "env_vars", &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
	return marshalFileEditConfig(&document)
}

func repairConfigMapping(parent *yaml.Node, name string) *yaml.Node {
	for index := 0; index < len(parent.Content); index += 2 {
		if parent.Content[index].Value == name && parent.Content[index+1].Kind == yaml.MappingNode {
			return parent.Content[index+1]
		}
	}
	value := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setRepairConfigValue(parent, name, value)
	return value
}

func setRepairConfigValue(parent *yaml.Node, name string, value *yaml.Node) {
	for index := 0; index < len(parent.Content); index += 2 {
		if parent.Content[index].Value == name {
			parent.Content[index+1] = value
			return
		}
	}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, value)
}
