package sshconfig

import (
	"fmt"
	"regexp"
	"strings"
)

// AlgorithmPolicy contains the first selected setting for each algorithm family.
type AlgorithmPolicy struct {
	Ciphers      AlgorithmList
	KeyExchanges AlgorithmList
	MACs         AlgorithmList
}

// AlgorithmList is a resolved configuration expression. A nil Names slice
// means no directive selected. Modifier is zero for replacement or one of
// '^', '+' and '-'. A configured list has at least one name or removal pattern.
type AlgorithmList struct {
	Modifier  byte
	Names     []string
	File      string
	Line      int
	Directive string
}

// Error reports a policy failure at the selected directive without printing
// the original configuration values.
func (l AlgorithmList) Error(code string, cause error) *Error {
	return &Error{Code: code, File: l.File, Line: l.Line, Directive: l.Directive, cause: cause}
}

var (
	algorithmName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._-]*$`)
	algorithmPattern = regexp.MustCompile(`^[A-Za-z0-9*?][A-Za-z0-9@._*?-]*$`)
)

func (p *AlgorithmPolicy) apply(d directive) error {
	list, err := parseAlgorithmList(d)
	if err != nil {
		return err
	}
	switch d.key {
	case "ciphers":
		p.Ciphers = list
	case "kexalgorithms":
		p.KeyExchanges = list
	case "macs":
		p.MACs = list
	}
	return nil
}

func parseAlgorithmList(d directive) (AlgorithmList, error) {
	value, err := d.singleValue()
	if err != nil {
		return AlgorithmList{}, err
	}
	list := AlgorithmList{File: d.file, Line: d.line, Directive: d.key}
	if len(value) > 0 {
		switch value[0] {
		case '+', '-', '^':
			list.Modifier = value[0]
			value = value[1:]
		}
	}
	match := algorithmName
	if list.Modifier == '-' {
		match = algorithmPattern
	}
	list.Names = strings.Split(value, ",")
	for _, name := range list.Names {
		if !match.MatchString(name) {
			return AlgorithmList{}, fmt.Errorf("invalid SSH algorithm list")
		}
	}
	return list, nil
}
