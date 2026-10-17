package maintainability

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func collectGo(root, module string, sources []policy.Source) error {
	for index := range sources {
		if !strings.HasSuffix(sources[index].Path, ".go") || sources[index].Kind == "generated" {
			continue
		}
		if err := collectGoFile(root, module, &sources[index]); err != nil {
			return err
		}
	}
	return collectModuleReflection(root, sources)
}

func collectGoFile(root, module string, source *policy.Source) error {
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, filepath.Join(root, source.Path), nil, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse Go source %s: %w", source.Path, err)
	}
	source.Module = "go:" + filepath.ToSlash(filepath.Dir(source.Path))
	if strings.HasSuffix(source.Path, "_test.go") {
		source.Module += "#test"
	}
	aliases := make(map[string]string)
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok {
			source.Functions = append(source.Functions, policy.Function{Name: function.Name.Name, Line: positions.Position(function.Pos()).Line})
		}
	}
	for _, imported := range file.Imports {
		path, unquoteErr := strconv.Unquote(imported.Path.Value)
		if unquoteErr != nil {
			return fmt.Errorf("decode Go import: %w", unquoteErr)
		}
		line := positions.Position(imported.Pos()).Line
		source.Imports = append(source.Imports, policy.Dependency{Target: goTarget(module, path), Line: line})
		alias := filepath.Base(path)
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		aliases[alias] = path
		if alias == "." {
			source.Effects = append(source.Effects, policy.Effect{Name: "dot import", Line: line})
		}
	}
	collectGoEffects(file, positions, aliases, source)
	return nil
}

func goTarget(module, imported string) string {
	return "go:" + strings.TrimPrefix(imported, module+"/")
}

func collectGoEffects(file *ast.File, positions *token.FileSet, aliases map[string]string, source *policy.Source) {
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.HasPrefix(comment.Text, "//go:linkname") {
				source.Effects = append(source.Effects, policy.Effect{Name: "go:linkname directive", Line: positions.Position(comment.Pos()).Line})
			}
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		name := aliases[identifier.Name] + "." + selector.Sel.Name
		if effectfulGoReference(name) || (aliases[identifier.Name] == "reflect" && !readOnlyReflectionName(selector.Sel.Name)) {
			source.Effects = append(source.Effects, policy.Effect{Name: name, Line: positions.Position(selector.Pos()).Line})
		}
		return true
	})
}

func effectfulGoReference(name string) bool {
	switch name {
	case "time.Now", "time.Sleep", "time.After", "time.AfterFunc", "time.NewTimer", "time.NewTicker", "time.Tick", "context.WithTimeout", "context.WithDeadline", "context.WithTimeoutCause", "context.WithDeadlineCause", "fmt.Print", "fmt.Printf", "fmt.Println", "fmt.Fprint", "fmt.Fprintf", "fmt.Fprintln", "fmt.Scan", "fmt.Scanf", "fmt.Scanln":
		return true
	default:
		return false
	}
}

func effectfulReflectionMethod(name string) bool {
	switch name {
	case "Call", "CallSlice", "Clear", "Close", "Send", "TrySend", "Recv", "TryRecv", "Grow", "Addr", "Method", "MethodByName", "Pointer", "InterfaceData":
		return true
	default:
		return strings.HasPrefix(name, "Set") || strings.HasPrefix(name, "Unsafe")
	}
}

func readOnlyReflectionName(name string) bool {
	switch name {
	case "TypeOf", "TypeFor", "ValueOf", "DeepEqual", "Type", "Value", "StructField", "StructTag", "Kind":
		return true
	case "Invalid", "Bool", "Int", "Int8", "Int16", "Int32", "Int64", "Uint", "Uint8", "Uint16", "Uint32", "Uint64", "Uintptr", "Float32", "Float64", "Complex64", "Complex128", "Array", "Chan", "Func", "Interface", "Map", "Pointer", "Ptr", "Slice", "String", "Struct", "UnsafePointer":
		return true
	default:
		return false
	}
}
