package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

func (e Engine[T]) Parse(args []string) (Parsed[T], error) {
	name, rest, err := e.separateCommand(args)
	if err != nil {
		return Parsed[T]{}, err
	}
	if name == "help" {
		return e.parseHelp(rest)
	}
	spec, _ := e.Root.Child(name)
	path := []string{name}
	for len(spec.Children) > 0 {
		action, remaining, actionErr := e.extractAction(spec, path, rest)
		if actionErr != nil {
			return Parsed[T]{}, actionErr
		}
		rest = remaining
		if action == "" {
			return e.parseGroupHelp(spec, path, rest)
		}
		child, found := spec.Child(action)
		if !found {
			return Parsed[T]{}, fmt.Errorf("unknown %s action %q; %s requires %s", strings.Join(path, " "), action, strings.Join(path, " "), spec.ChildNames())
		}
		path = append(path, action)
		spec = child
	}
	fs, state, err := e.flags(path)
	if err != nil {
		return Parsed[T]{}, err
	}
	if e.Prepare != nil {
		rest, err = e.Prepare(state, rest)
		if err != nil {
			return Parsed[T]{}, err
		}
	}
	ordered, err := orderFlags(fs, rest)
	if err != nil {
		return Parsed[T]{}, err
	}
	if err = fs.Parse(ordered); err != nil {
		return Parsed[T]{}, err
	}
	if helpSelected(fs) {
		return Parsed[T]{State: state, Help: true, HelpPath: path}, nil
	}
	if e.Validate != nil {
		if err = e.Validate(state, fs); err != nil {
			return Parsed[T]{}, err
		}
	}
	return Parsed[T]{State: state}, nil
}

func (e Engine[T]) separateCommand(args []string) (string, []string, error) {
	globals, _, err := e.flags(nil)
	if err != nil {
		return "", nil, err
	}
	for i := 0; i < len(args); i++ {
		name, _, assigned := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if strings.HasPrefix(args[i], "-") && name != "help" && name != "h" && globals.Lookup(name) != nil {
			if !assigned {
				i++
				if i == len(args) {
					return "", nil, fmt.Errorf("missing value for --%s", name)
				}
			}
			continue
		}
		name = args[i]
		switch name {
		case "-h", "--help":
			name = "help"
		case "--version":
			name = "version"
		}
		if _, found := e.Root.Child(name); !found {
			return "", nil, fmt.Errorf("unknown command or flag %q", args[i])
		}
		rest := append([]string(nil), args[:i]...)
		return name, append(rest, args[i+1:]...), nil
	}
	return "help", append([]string(nil), args...), nil
}

func (e Engine[T]) extractAction(spec Spec[T], path, rest []string) (string, []string, error) {
	flags, err := e.routingFlags(spec, path)
	if err != nil {
		return "", nil, err
	}
	help := false
	for i := 0; i < len(rest); {
		arg := rest[i]
		if arg == "--" {
			action, remaining := separatedAction(spec.AllowActionAfterSeparator, rest, i)
			if action != "" {
				return action, remaining, nil
			}
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			next, wantsHelp, flagErr := advanceActionFlag(flags, rest, i)
			if flagErr != nil {
				return "", nil, flagErr
			}
			help = help || wantsHelp
			i = next
			continue
		}
		remaining := append(append([]string(nil), rest[:i]...), rest[i+1:]...)
		return arg, remaining, nil
	}
	if help {
		return "", rest, nil
	}
	return "", nil, fmt.Errorf("%s requires %s", strings.Join(path, " "), spec.ChildNames())
}

// separatedAction preserves the legacy group grammar without reinterpreting
// the remaining post-delimiter values as flags.
func separatedAction(allowed bool, args []string, index int) (string, []string) {
	if !allowed || index+1 >= len(args) {
		return "", nil
	}
	remaining := append(append([]string(nil), args[:index+1]...), args[index+2:]...)
	return args[index+1], remaining
}

func advanceActionFlag(flags map[string]bool, args []string, index int) (int, bool, error) {
	consumes, wantsHelp, err := actionFlag(flags, args[index])
	if err != nil {
		return index, false, err
	}
	next := index + 1
	if !consumes {
		return next, wantsHelp, nil
	}
	if next == len(args) {
		return index, false, fmt.Errorf("missing value for %s", args[index])
	}
	return next + 1, wantsHelp, nil
}

func actionFlag(flags map[string]bool, arg string) (bool, bool, error) {
	name, value, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
	boolean, known := flags[name]
	if !known {
		return false, false, fmt.Errorf("unknown flag %q", arg)
	}
	wantsHelp := (name == "help" || name == "h") && (!assigned || value == "true")
	return !boolean && !assigned, wantsHelp, nil
}

func (e Engine[T]) routingFlags(spec Spec[T], path []string) (map[string]bool, error) {
	result := make(map[string]bool)
	if len(spec.Children) > 0 {
		for _, child := range spec.Children {
			nested, err := e.routingFlags(child, append(append([]string(nil), path...), child.Name))
			if err != nil {
				return nil, err
			}
			for name, boolean := range nested {
				result[name] = boolean
			}
		}
		return result, nil
	}
	fs, _, err := e.flags(path)
	if err != nil {
		return nil, err
	}
	fs.VisitAll(func(f *flag.Flag) {
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		result[f.Name] = ok && b.IsBoolFlag()
	})
	return result, nil
}

func helpSelected(fs *flag.FlagSet) bool {
	return fs.Lookup("help").Value.String() == "true" || fs.Lookup("h").Value.String() == "true"
}

func (e Engine[T]) parseHelp(rest []string) (Parsed[T], error) {
	fs, state, err := e.flags(nil)
	if err != nil {
		return Parsed[T]{}, err
	}
	ordered, err := orderFlags(fs, rest)
	if err != nil {
		return Parsed[T]{}, err
	}
	if err = fs.Parse(ordered); err != nil {
		return Parsed[T]{}, err
	}
	path := fs.Args()
	if _, err = e.resolve(path); err != nil {
		return Parsed[T]{}, fmt.Errorf("unknown help command %q", strings.Join(path, " "))
	}
	return Parsed[T]{State: state, Help: true, HelpPath: append([]string(nil), path...)}, nil
}

func (e Engine[T]) parseGroupHelp(spec Spec[T], path, rest []string) (Parsed[T], error) {
	fs, state, err := e.flags(firstLeafPath(spec, path))
	if err != nil {
		return Parsed[T]{}, err
	}
	group := flag.NewFlagSet(strings.Join(path, " "), flag.ContinueOnError)
	group.SetOutput(io.Discard)
	for _, name := range []string{"root", "json", "help", "h"} {
		if f := fs.Lookup(name); f != nil {
			group.Var(f.Value, name, f.Usage)
		}
	}
	ordered, err := orderFlags(group, rest)
	if err != nil {
		return Parsed[T]{}, err
	}
	if err = group.Parse(ordered); err != nil {
		return Parsed[T]{}, err
	}
	if len(group.Args()) > 0 {
		return Parsed[T]{}, errors.New("unexpected argument after group help")
	}
	return Parsed[T]{State: state, Help: true, HelpPath: append([]string(nil), path...)}, nil
}

func firstLeafPath[T any](spec Spec[T], path []string) []string {
	result := append([]string(nil), path...)
	for len(spec.Children) > 0 {
		spec = spec.Children[0]
		result = append(result, spec.Name)
	}
	return result
}
