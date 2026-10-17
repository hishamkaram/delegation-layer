package maintainability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

type goPackage struct {
	ImportPath   string
	Dir          string
	ForTest      string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

func collectGoPackages(root, module string) ([]policy.Source, error) {
	var facts []policy.Source
	for _, target := range []string{"linux", "darwin"} {
		command := exec.Command("go", "list", "-deps", "-test", "-json", "./...")
		command.Dir = root
		command.Env = append(command.Environ(), "GOOS="+target, "CGO_ENABLED=0")
		var diagnostic bytes.Buffer
		command.Stderr = &diagnostic
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("collect %s package dependencies: %w: %s", target, err, strings.TrimSpace(diagnostic.String()))
		}
		selected, err := decodePackages(root, module, target, output)
		if err != nil {
			return nil, err
		}
		facts = append(facts, selected...)
	}
	return facts, nil
}

func decodePackages(root, module, target string, output []byte) ([]policy.Source, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	var facts []policy.Source
	for {
		var current goPackage
		err := decoder.Decode(&current)
		if errors.Is(err, io.EOF) {
			return facts, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode Go package metadata: %w", err)
		}
		if !strings.HasPrefix(current.ImportPath, module+"/") || current.ForTest != "" || strings.HasSuffix(current.ImportPath, ".test") {
			continue
		}
		relative, err := filepath.Rel(root, current.Dir)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, errors.New("go package metadata escaped the source root")
		}
		base := "go:" + filepath.ToSlash(relative)
		facts = append(facts, packageFact(base, relative, target, module, current.Imports))
		testImports := append(append([]string(nil), current.TestImports...), current.XTestImports...)
		if len(testImports) > 0 {
			facts = append(facts, packageFact(base+"#test", relative, target+"-test", module, testImports))
		}
	}
}

func packageFact(name, directory, target, module string, imports []string) policy.Source {
	fact := policy.Source{Path: filepath.ToSlash(directory) + "/(go-list-" + target + ")", Module: name, Kind: "metadata"}
	for _, imported := range imports {
		path, _, _ := strings.Cut(imported, " [")
		fact.Imports = append(fact.Imports, policy.Dependency{Target: goTarget(module, path)})
	}
	return fact
}
