package taskdir

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

type lockOpenCase struct {
	name   string
	create bool
	errors []error
	flags  []int
	want   error
}

func TestOpenLockInodeUsesExclusiveCreationAndExistingWinner(t *testing.T) {
	createFlags := unix.O_CREAT | unix.O_EXCL | unix.O_RDWR
	for _, tc := range []lockOpenCase{
		{"created", true, []error{nil}, []int{createFlags}, nil},
		{"existing winner", true, []error{fmt.Errorf("create: %w", os.ErrExist), nil}, []int{createFlags, unix.O_RDWR}, nil},
		{"creation denied", true, []error{os.ErrPermission}, []int{createFlags}, os.ErrPermission},
		{"creation parent missing", true, []error{os.ErrNotExist}, []int{createFlags}, os.ErrNotExist},
		{"winner disappeared", true, []error{os.ErrExist, os.ErrNotExist}, []int{createFlags, unix.O_RDWR}, os.ErrNotExist},
		{"reopened", false, []error{nil}, []int{unix.O_RDWR}, nil},
		{"missing existing lock", false, []error{os.ErrNotExist}, []int{unix.O_RDWR}, os.ErrNotExist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkLockOpenCase(t, tc)
		})
	}
}

func checkLockOpenCase(t *testing.T, tc lockOpenCase) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	var calls []int
	opened, err := openLockInode(func(flags int) (*os.File, error) {
		calls = append(calls, flags)
		if len(calls) > len(tc.errors) {
			t.Error("unexpected extra open attempt")
			return nil, os.ErrInvalid
		}
		if openErr := tc.errors[len(calls)-1]; openErr != nil {
			return nil, openErr
		}
		return file, nil
	}, tc.create)
	if !errors.Is(err, tc.want) || !slices.Equal(calls, tc.flags) {
		t.Fatalf("opens=%v error=%v; want opens=%v error=%v", calls, err, tc.flags, tc.want)
	}
	if tc.want == nil && opened != file || tc.want != nil && opened != nil {
		t.Fatal("open result did not preserve the selected file or failure")
	}
}
