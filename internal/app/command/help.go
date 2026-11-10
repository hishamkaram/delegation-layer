package command

import (
	"flag"
	"fmt"
	"strings"
)

func (e Engine[T]) Help(path []string) (string, error) {
	spec, err := e.resolve(path)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n\nUsage: %s\n", spec.Summary, spec.Usage(path))
	if len(spec.Children) > 0 {
		out.WriteString("\nCommands:\n")
		for _, child := range spec.Children {
			if !child.Hidden {
				fmt.Fprintf(&out, "  %-14s %s\n", child.Name, child.Summary)
			}
		}
	}
	if err = e.renderFlags(&out, spec, path); err != nil {
		return "", err
	}
	if len(path) == 0 {
		out.WriteString("\nUse delegate help COMMAND [ACTION...] or COMMAND --help for details.\n")
		out.WriteString(e.Root.Notes)
	} else {
		out.WriteString(spec.Notes)
		if len(spec.Example) > 0 {
			fmt.Fprintf(&out, "\nExample:\n  %s\n", renderExample(spec.Example))
		}
		out.WriteString("\nCommon exit codes:\n  0 success; 1 operational failure; 2 invalid arguments.\n")
		out.WriteString(spec.ExitNotes)
	}
	return out.String(), nil
}

func (e Engine[T]) renderFlags(out *strings.Builder, spec Spec[T], path []string) error {
	if len(path) > 0 && len(spec.Children) > 0 {
		out.WriteString("\nUse an action's --help for its flags, defaults and required inputs.\n")
		return nil
	}
	fs, _, err := e.flags(path)
	if err != nil {
		return err
	}
	out.WriteString("\nFlags:\n")
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "h" || strings.HasPrefix(f.Name, "scheduler-host-") {
			return
		}
		kind, usage := flag.UnquoteUsage(f)
		label := "--" + f.Name
		if kind != "" {
			label += " " + strings.ToUpper(kind)
		}
		fmt.Fprintf(out, "  %-29s %s", label, usage)
		if f.DefValue != "" {
			fmt.Fprintf(out, " (default: %s)", f.DefValue)
		}
		out.WriteByte('\n')
	})
	return nil
}

func (e Engine[T]) UsageForArgs(argv []string) string {
	name, rest, err := e.separateCommand(argv)
	if err != nil {
		return "Usage: delegate COMMAND [flags]\nRun delegate --help for commands.\n"
	}
	spec, _ := e.Root.Child(name)
	path := []string{name}
	for len(spec.Children) > 0 {
		action, remaining, actionErr := e.extractAction(spec, path, rest)
		if actionErr != nil || action == "" {
			break
		}
		child, found := spec.Child(action)
		if !found {
			break
		}
		path = append(path, action)
		spec = child
		rest = remaining
	}
	return fmt.Sprintf("Usage: %s\nRun delegate %s --help for details.\n", spec.Usage(path), strings.Join(path, " "))
}

// Examples are argv, not shell programs. Quote only the displayed values;
// semantic tests consume the same typed arguments without another parser.
func renderExample(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if arg == "" || strings.ContainsAny(arg, " \t\r\n'\"$`\\;&|<>()*?![]{}#~") {
			arg = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
		}
		quoted[i] = arg
	}
	return strings.Join(quoted, " ")
}
