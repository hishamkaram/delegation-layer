package taskdir

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareStopContextCanceledCallerCannotPublishOrConsumeRequest(t *testing.T) {
	s := testStore(t)
	td := supervisorTask(t, s)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	id := strings.Repeat("9", 32)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	permit, err := td.PrepareStopContext(ctx, id, "user", time.Time{})
	if permit != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller gained stop authority: permit=%v err=%v", permit, err)
	}
	testAbsent(t, filepath.Join(td.Dir, "stop", id+".request.json"))
	permit, err = td.PrepareStopContext(t.Context(), id, "user", time.Time{})
	must(t, err)
	must(t, permit.Release())
	request, err := td.ReadStopRequest(id)
	must(t, err)
	if request.RequestID != id || request.Cause != "user" {
		t.Fatalf("uncanceled caller lost canonical request: %+v", request)
	}
}
