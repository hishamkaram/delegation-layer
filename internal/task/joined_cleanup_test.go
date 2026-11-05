package task

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

func TestJoinedErrorNodeVisitsWrappersBeforeCauses(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	wrapper := fmt.Errorf("retained owner: %w", first)
	joined := errors.Join(wrapper, second)
	var visited []error
	if AnyErrorNode(joined, func(node error) bool {
		visited = append(visited, node)
		return false
	}) {
		t.Fatal("nonmatching predicate matched")
	}
	if !slices.Equal(visited, []error{joined, wrapper, first, second}) {
		t.Fatalf("wrong node order: %v", visited)
	}
	visited = nil
	if !AnyErrorNode(joined, func(node error) bool {
		visited = append(visited, node)
		return reflect.TypeOf(node) == reflect.TypeOf(wrapper) && errors.Is(node, first)
	}) || !slices.Equal(visited, []error{joined, wrapper}) {
		t.Fatal("wrapper match did not stop before unwrap")
	}
}

func TestJoinedErrorNodeHandlesAbsentCauses(t *testing.T) {
	if AnyErrorNode(nil, nil) {
		t.Fatal("nil error matched")
	}
	for _, cause := range []error{causeJoinError{}, causeJoinError{causes: []error{nil}}} {
		if AnyErrorNode(cause, func(error) bool { return false }) {
			t.Fatal("absent child matched")
		}
		if !AnyErrorNode(cause, func(error) bool { return true }) {
			t.Fatal("join node was skipped")
		}
	}
	accepted := errors.New("accepted")
	joined := causeJoinError{causes: []error{nil, fmt.Errorf("later: %w", accepted)}}
	if !AnyErrorNode(joined, func(node error) bool { return errors.Is(node, accepted) }) {
		t.Fatal("nil child hid later matching node")
	}
}
