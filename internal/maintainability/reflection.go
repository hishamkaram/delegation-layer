package maintainability

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

// Inspect every file in a reflection-using module. A helper can return a
// reflect.Value to a sibling file which has no reflection import of its own.
// Selectors are checked conservatively, rather than claiming semantic purity.
func collectModuleReflection(root string, sources []policy.Source) error {
	modules := make(map[string]bool)
	for _, source := range sources {
		for _, dependency := range source.Imports {
			if dependency.Target == "go:reflect" {
				modules[source.Module] = true
			}
		}
	}
	for index := range sources {
		if !modules[sources[index].Module] {
			continue
		}
		if err := collectReflectionMethods(root, &sources[index]); err != nil {
			return err
		}
	}
	return nil
}

func collectReflectionMethods(root string, source *policy.Source) error {
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, filepath.Join(root, source.Path), nil, 0)
	if err != nil {
		return fmt.Errorf("parse reflection consumer %s: %w", source.Path, err)
	}
	aliases := reflectionAliases(file)
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok {
			if identifier, named := selector.X.(*ast.Ident); named && aliases[identifier.Name] {
				return true // Package-level references have the read-only name guard.
			}
		}
		if ok && effectfulReflectionMethod(selector.Sel.Name) {
			source.Effects = append(source.Effects, policy.Effect{
				Name: "reflection mutation or invocation: " + selector.Sel.Name,
				Line: positions.Position(selector.Pos()).Line,
			})
		}
		return true
	})
	return nil
}

func reflectionAliases(file *ast.File) map[string]bool {
	aliases := make(map[string]bool)
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || path != "reflect" {
			continue // The primary parser has already checked every import literal.
		}
		name := "reflect"
		if imported.Name != nil {
			name = imported.Name.Name
		}
		aliases[name] = true
	}
	return aliases
}
