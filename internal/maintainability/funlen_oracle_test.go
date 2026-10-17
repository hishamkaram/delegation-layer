package maintainability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

type funlenIssue struct {
	FromLinter string
	Text       string
	Pos        struct {
		Filename string
		Line     int
	}
}

type funlenReport struct {
	Issues []funlenIssue
}

// collectFunlen is a test oracle, never a second production policy path.
func collectFunlen(root, binary, config string, sources []policy.Source) ([]policy.Finding, error) {
	command := exec.Command(binary, "run", "--config", config, "--allow-serial-runners", "--enable-only", "funlen", "--max-issues-per-linter", "0", "--max-same-issues", "0", "--output.json.path", "stdout", "--output.text.path", "/dev/null", "./...")
	command.Dir = root
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	output, err := command.Output()
	exitCode := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("run pinned Go function check: %w: %s", err, strings.TrimSpace(diagnostic.String()))
		}
		exitCode = 1
	}
	var report funlenReport
	if err = json.NewDecoder(bytes.NewReader(output)).Decode(&report); err != nil {
		return nil, fmt.Errorf("decode Go function check: %w", err)
	}
	if (exitCode == 0) != (len(report.Issues) == 0) {
		return nil, errors.New("go function check exit and findings disagree")
	}
	return funlenFindings(root, report.Issues, sources)
}

func funlenFindings(root string, issues []funlenIssue, sources []policy.Source) ([]policy.Finding, error) {
	byPath := make(map[string]policy.Source, len(sources))
	for _, source := range sources {
		byPath[source.Path] = source
	}
	var findings []policy.Finding
	for _, issue := range issues {
		if issue.FromLinter != "funlen" || issue.Text == "" {
			return nil, errors.New("unexpected Go function diagnostic")
		}
		path := issue.Pos.Filename
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !validRelativePath(relative) {
			return nil, errors.New("go function diagnostic escaped source root")
		}
		source, exists := byPath[filepath.ToSlash(relative)]
		if !exists {
			return nil, errors.New("go function diagnostic lacks maintained source")
		}
		name := functionAt(source.Functions, issue.Pos.Line)
		if name == "" {
			return nil, errors.New("go function diagnostic lacks an AST declaration")
		}
		findings = append(findings, policy.Finding{Path: source.Path, Line: issue.Pos.Line, Kind: "function", Symbol: name + "@" + strconv.Itoa(issue.Pos.Line), Detail: issue.Text})
	}
	return findings, nil
}

func functionAt(functions []policy.Function, line int) string {
	for _, function := range functions {
		if function.Line == line {
			return function.Name
		}
	}
	return ""
}
