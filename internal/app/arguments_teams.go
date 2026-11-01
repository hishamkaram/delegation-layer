package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	defaultTeamsPageSize = 50
	maxTeamsPageSize     = 100
)

func parseTeamsArguments(a Arguments, rest []string) (Arguments, error) {
	var err error
	a.TeamAction, rest, err = teamsAction(rest)
	if err != nil {
		return Arguments{}, err
	}
	if a.TeamAction == "default" {
		a.TeamDefaultAction, rest, err = teamsAction(rest)
		if err != nil {
			return Arguments{}, errors.New("teams default requires show, set, or clear")
		}
	}
	fs := flag.NewFlagSet("teams", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err = registerTeamsFlags(fs, &a); err != nil {
		return Arguments{}, err
	}
	ordered, err := orderFlags(fs, rest)
	if err != nil {
		return Arguments{}, err
	}
	if err = fs.Parse(ordered); err != nil {
		return Arguments{}, err
	}
	if err = validateNoPositional("teams "+a.TeamAction, fs.Args()); err != nil {
		return Arguments{}, err
	}
	if err = validateTeamsArguments(a); err != nil {
		return Arguments{}, err
	}
	return a, nil
}

// Only root and JSON may precede an action, matching the other command groups.
func teamsAction(args []string) (string, []string, error) {
	for i := 0; i < len(args); i++ {
		name, _, assigned := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if strings.HasPrefix(args[i], "-") && (name == "root" || name == "json") {
			if name == "root" && !assigned {
				i++
				if i == len(args) {
					return "", nil, errors.New("missing value for --root")
				}
			}
			continue
		}
		rest := append(append([]string(nil), args[:i]...), args[i+1:]...)
		return args[i], rest, nil
	}
	return "", nil, errors.New("teams requires register, update, delete, list, show, or default")
}

func registerTeamsFlags(fs *flag.FlagSet, a *Arguments) error {
	fs.StringVar(&a.Root, "root", a.Root, "absolute execution state root; never selects the Team catalog")
	fs.BoolVar(&a.JSON, "json", a.JSON, "emit versioned JSON")
	fs.StringVar(&a.Cwd, "cwd", "", "explicit absolute project context")
	fs.StringVar(&a.ProjectRoot, "project-root", "", "absolute non-Git project root")
	switch a.TeamAction {
	case "register", "update", "delete", "show":
		fs.StringVar(&a.TeamScope, "scope", "", "user or project")
		if a.TeamAction != "register" {
			fs.StringVar(&a.TeamID, "team", "", "Team ID")
		}
		if a.TeamAction == "register" || a.TeamAction == "update" {
			fs.StringVar(&a.TeamDefinition, "definition", "", "definition JSON file, or - for stdin")
		}
		if a.TeamAction == "update" || a.TeamAction == "delete" {
			fs.Int64Var(&a.TeamExpectedRevision, "expected-revision", 0, "positive expected Team revision")
		}
	case "list":
		fs.IntVar(&a.TeamLimit, "limit", defaultTeamsPageSize, "Teams per page (1-100)")
		fs.StringVar(&a.TeamAfter, "after", "", "exclusive Team cursor")
	case "default":
		switch a.TeamDefaultAction {
		case "set":
			fs.StringVar(&a.TeamScope, "team-scope", "", "user or project target Team")
			fs.StringVar(&a.TeamID, "team", "", "Team ID")
		case "show", "clear":
		default:
			return fmt.Errorf("unknown teams default action %q", a.TeamDefaultAction)
		}
	default:
		return fmt.Errorf("unknown teams action %q", a.TeamAction)
	}
	return nil
}

func validateTeamsArguments(a Arguments) error {
	if err := validateTeamsPaths(a); err != nil {
		return err
	}
	if a.TeamAction == "list" {
		if a.TeamLimit < 1 || a.TeamLimit > maxTeamsPageSize {
			return errors.New("--limit must be between 1 and 100")
		}
		return validateTeamsCursor(a.TeamAfter)
	}
	if a.TeamAction == "default" {
		if a.Cwd == "" {
			return errors.New("teams default requires an explicit --cwd")
		}
		if a.TeamDefaultAction != "set" {
			return nil
		}
	}
	if err := validateTeamsScope(a); err != nil {
		return err
	}
	if a.TeamAction != "register" {
		if err := task.ValidateTeamID(a.TeamID); err != nil {
			return fmt.Errorf("--team: %w", err)
		}
	}
	return validateTeamsMutation(a)
}

func validateTeamsPaths(a Arguments) error {
	for _, path := range []string{a.Root, a.Cwd, a.ProjectRoot} {
		if path != "" && !filepath.IsAbs(path) {
			return errors.New("--root, --cwd and --project-root must be absolute paths")
		}
	}
	if a.ProjectRoot != "" && a.Cwd == "" {
		return errors.New("--project-root requires an explicit --cwd")
	}
	return nil
}

func validateTeamsScope(a Arguments) error {
	if a.TeamScope != "user" && a.TeamScope != "project" {
		return errors.New("explicit scope must be user or project")
	}
	if a.TeamScope == "project" && a.Cwd == "" {
		return errors.New("project scope requires an explicit --cwd")
	}
	if a.TeamAction != "default" && a.TeamScope == "user" && a.Cwd != "" {
		return errors.New("user-scoped management does not take project context")
	}
	return nil
}

func validateTeamsMutation(a Arguments) error {
	if (a.TeamAction == "update" || a.TeamAction == "delete") && a.TeamExpectedRevision < 1 {
		return errors.New("--expected-revision must be positive")
	}
	if (a.TeamAction == "register" || a.TeamAction == "update") && a.TeamDefinition == "" {
		return errors.New("--definition is required; use - for stdin")
	}
	return nil
}

func validateTeamsCursor(cursor string) error {
	if cursor == "" {
		return nil
	}
	scope, id, found := strings.Cut(cursor, ":")
	if !found || (scope != "user" && scope != "project") {
		return errors.New("--after must be a cursor returned by teams list")
	}
	if err := task.ValidateTeamID(id); err != nil {
		return fmt.Errorf("--after: %w", err)
	}
	return nil
}
