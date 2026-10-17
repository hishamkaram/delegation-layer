package task

import (
	"errors"
	"fmt"
	"testing"
)

type causeJoinError struct {
	causes []error
}

func (joined causeJoinError) Error() string { return "joined causes" }

func (joined causeJoinError) Unwrap() []error { return joined.causes }

func TestAllErrorCauses(t *testing.T) {
	accepted := errors.New("accepted")
	rejected := errors.New("rejected")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"},
		{name: "matching leaf", err: accepted, want: true},
		{name: "different leaf", err: rejected},
		{name: "wrapped leaf", err: fmt.Errorf("wrapped: %w", accepted), want: true},
		{name: "matching join", err: errors.Join(accepted, accepted), want: true},
		{name: "mixed join", err: errors.Join(accepted, rejected)},
		{name: "wrapped matching join", err: fmt.Errorf("wrapped: %w", errors.Join(accepted, accepted)), want: true},
		{name: "nested mixed join", err: errors.Join(accepted, fmt.Errorf("nested: %w", errors.Join(accepted, rejected)))},
		{name: "nil standard join", err: errors.Join(nil, nil)},
		{name: "empty join", err: causeJoinError{causes: []error{}}},
		{name: "nil join causes", err: causeJoinError{}},
		{name: "join with nil child", err: causeJoinError{causes: []error{accepted, nil}}},
		{name: "join with empty child", err: errors.Join(accepted, causeJoinError{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AllErrorCauses(tc.err, func(err error) bool { return errors.Is(err, accepted) })
			if got != tc.want {
				t.Fatalf("all causes match = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestAllErrorCausesPassesOnlyLeavesToPredicate(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	joined := errors.Join(fmt.Errorf("wrapped: %w", first), second)
	var visited []error
	if !AllErrorCauses(joined, func(err error) bool {
		visited = append(visited, err)
		return true
	}) {
		t.Fatal("matching leaves rejected")
	}
	if len(visited) != 2 || !errors.Is(visited[0], first) || !errors.Is(visited[1], second) ||
		errors.Unwrap(visited[0]) != nil || errors.Unwrap(visited[1]) != nil {
		t.Fatalf("predicate received wrappers or lost leaf order: %v", visited)
	}
}

func TestAllErrorCausesDoesNotEvaluateAbsentCauses(t *testing.T) {
	for _, err := range []error{nil, causeJoinError{}} {
		if AllErrorCauses(err, nil) {
			t.Fatal("an absent cause matched")
		}
	}
}
