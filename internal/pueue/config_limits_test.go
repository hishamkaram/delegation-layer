package pueue

import (
	"errors"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The configuration schema cannot reach arbitrary nesting depths. These pairs
// exercise the resource gate itself; compiled CLI cases separately verify the
// full parser and schema. Each negative differs by exactly one counted node.
func TestYAMLGraphExactResourceBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		build func() *yaml.Node
		grow  func(*yaml.Node)
	}{
		{"depth", depthBoundary, func(root *yaml.Node) {
			leaf := root
			for len(leaf.Content) > 0 {
				leaf = leaf.Content[0]
			}
			leaf.Kind, leaf.Tag = yaml.SequenceNode, "!!seq"
			leaf.Content = []*yaml.Node{scalarNode()}
		}},
		{"original nodes", func() *yaml.Node { return sequenceNodes(maxYAMLNodes - 1) }, appendScalar},
		{"expanded nodes", expansionBoundary, appendScalar},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.build()
			if err := validateYAMLGraph(root); err != nil {
				t.Fatalf("exact limit refused: %v", err)
			}
			tc.grow(root)
			if err := validateYAMLGraph(root); !errors.Is(err, ErrConfiguration) {
				t.Fatalf("limit plus one accepted: %v", err)
			}
		})
	}
}

func scalarNode() *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "x"} }

func appendScalar(root *yaml.Node) { root.Content = append(root.Content, scalarNode()) }

func sequenceNodes(children int) *yaml.Node {
	root := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for range children {
		appendScalar(root)
	}
	return root
}

func depthBoundary() *yaml.Node {
	root := scalarNode()
	for depth := 1; depth < maxYAMLDepth; depth++ {
		root = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{root}}
	}
	return root
}

func expansionBoundary() *yaml.Node {
	// One root + 1023 shared nodes + 63*(one alias + 1023 nodes)
	// gives exactly 65536 expanded nodes with only 1087 original nodes.
	shared := sequenceNodes(1022)
	shared.Anchor = "shared"
	root := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{shared}}
	for range 63 {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.AliasNode, Alias: shared, Value: "shared"})
	}
	return root
}
