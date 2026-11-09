// Package command owns pure command syntax and help. Application-supplied typed
// registrations are the sole flag definitions; semantic rules remain with their
// application/domain owners. There is no I/O, reflection or dynamic request map.
package command

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

type Spec[T any] struct {
	Name, Summary, Operands, Notes, ExitNotes string
	Example                                   []string
	Children                                  []Spec[T]
	AllowActionAfterSeparator                 bool
	Hidden                                    bool
	Register                                  func(*flag.FlagSet, *T) error
}

func (s Spec[T]) Child(name string) (Spec[T], bool) {
	for _, child := range s.Children {
		if child.Name == name {
			return child, true
		}
	}
	return Spec[T]{}, false
}

func (s Spec[T]) ChildNames() string {
	var names []string
	for _, child := range s.Children {
		if !child.Hidden {
			names = append(names, child.Name)
		}
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

func (s Spec[T]) Usage(path []string) string {
	usage := "delegate"
	if len(path) > 0 {
		usage += " " + strings.Join(path, " ")
	}
	if len(s.Children) > 0 {
		return usage + " COMMAND [flags]"
	}
	if s.Operands != "" {
		usage += " " + s.Operands
	}
	return usage + " [flags]"
}

// Engine receives one invocation's immutable grammar and pure typed callbacks.
// Prepare handles capability-owned literal argv tails, not another flag parser.
type Engine[T any] struct {
	Root            Spec[T]
	NewState        func([]string) T
	DefaultRegister func(*flag.FlagSet, *T) error
	Validate        func(*T, *flag.FlagSet) error
	Prepare         func(*T, []string) ([]string, error)
}

type Parsed[T any] struct {
	State    *T
	Help     bool
	HelpPath []string
}

func (e Engine[T]) resolve(path []string) (Spec[T], error) {
	spec := e.Root
	for _, name := range path {
		child, found := spec.Child(name)
		if !found {
			return Spec[T]{}, fmt.Errorf("unknown command %q", strings.Join(path, " "))
		}
		spec = child
	}
	return spec, nil
}

func (e Engine[T]) flags(path []string) (*flag.FlagSet, *T, error) {
	var state T
	if e.NewState != nil {
		state = e.NewState(path)
	}
	fs := flag.NewFlagSet(strings.Join(path, " "), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registration := e.Root.Register
	if len(path) > 0 {
		registration = e.DefaultRegister
		spec := e.Root
		for _, name := range path {
			child, found := spec.Child(name)
			if !found {
				return nil, nil, fmt.Errorf("unknown command %q", strings.Join(path, " "))
			}
			if child.Register != nil {
				registration = child.Register
			}
			spec = child
		}
	}
	if registration != nil {
		if err := registration(fs, &state); err != nil {
			return nil, nil, err
		}
	}
	fs.Bool("help", false, "show help for this command")
	fs.Bool("h", false, "show help for this command")
	return fs, &state, nil
}

func (e Engine[T]) Defaults(path []string) (*T, error) {
	spec, err := e.resolve(path)
	if err != nil {
		return nil, err
	}
	if len(path) == 0 || len(spec.Children) > 0 {
		return nil, fmt.Errorf("a complete command path is required")
	}
	_, state, err := e.flags(path)
	return state, err
}
