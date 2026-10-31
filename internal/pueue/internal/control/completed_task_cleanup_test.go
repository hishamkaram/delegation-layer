package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// labelFilterCleanupScript answers status like Pueue's exact label filter once
// the test installs its label-status directory: a label query returns only
// that label's prepared rows, or an empty queue when none exist, and an
// unfiltered query returns every prepared row. Every status and remove argv
// is logged. A status-hold marker delays the response past the observation
// window. A prepared label-status-next directory replaces the label responses
// once the first label query has been answered.
const labelFilterCleanupScript = `#!/bin/sh
set -eu
base=${2%/*}
if [ -d "$base/label-status" ]; then
  case "${3-}" in
    status|remove)
      {
        printf 'argc=%s\n' "$#"
        for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
        printf '%s\n' '---'
      } >> "$base/cleanup-commands.log"
      if [ "$3" = remove ]; then exit 0; fi
      if [ -f "$base/status-hold" ]; then sleep 1; fi
      case "${5-}" in
        label=*)
          rows="$base/label-status/${5#label=}.json"
          if [ -f "$rows" ]; then cat "$rows"; else printf '%s\n' '{"tasks":{},"groups":{"default":{"status":"Running","parallel_tasks":1}}}'; fi
          if [ -d "$base/label-status-next" ]; then
            rm -r "$base/label-status"
            mv "$base/label-status-next" "$base/label-status"
          fi ;;
        *) cat "$base/all-status.json" ;;
      esac
      exit 0 ;;
  esac
fi
`

type cleanupTestRow struct {
	id    int64
	label string
	state State
}

type labelFilterCleanupFixture struct {
	client   *Client
	base     string
	identity Identity
}

func newLabelFilterCleanupFixture(t *testing.T, options Options) labelFilterCleanupFixture {
	t.Helper()
	private := newPrivateSupervisorTestFixture(t)
	writeExecutable(t, private.clientPath, labelFilterCleanupScript+privateClientFixture)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	bound, err := BindPrivate(ctx, private.clientPath, private.daemonPath, private.stateRoot, Options{ObservationTimeout: 10 * time.Second})
	supervisorLeaseTestSettleClient(t, bound, err)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(bound.Binding(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	saved := int64(3)
	return labelFilterCleanupFixture{
		client: client, base: filepath.Dir(bound.Binding().ConfigPath),
		identity: Identity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("1", 32), SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64), NumericTaskID: &saved},
	}
}

func cleanupTestStatusJob(t *testing.T, row cleanupTestRow) map[string]any {
	t.Helper()
	states := map[State]map[string]any{
		StateRunning: {"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}},
		StateEnded:   {"Done": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": "Success"}},
		StateStashed: {"Stashed": map[string]any{"enqueue_at": nil}},
	}
	state, ok := states[row.state]
	if !ok {
		t.Fatalf("no status rendering for %q", row.state)
	}
	return statusTestJob(row.id, row.label, state)
}

// install writes the unfiltered queue and each label's filtered response.
func (fixture labelFilterCleanupFixture) install(t *testing.T, rows ...cleanupTestRow) {
	t.Helper()
	fixture.installLabelStatus(t, "label-status", rows)
}

// installAfterFirstQuery stages the label responses that replace the
// installed ones once the first label query has been answered.
func (fixture labelFilterCleanupFixture) installAfterFirstQuery(t *testing.T, rows ...cleanupTestRow) {
	t.Helper()
	fixture.installLabelStatus(t, "label-status-next", rows)
}

func (fixture labelFilterCleanupFixture) installLabelStatus(t *testing.T, directory string, rows []cleanupTestRow) {
	t.Helper()
	all := map[string]any{}
	byLabel := map[string]map[string]any{}
	for _, row := range rows {
		job := cleanupTestStatusJob(t, row)
		key := strconv.FormatInt(row.id, 10)
		all[key] = job
		if byLabel[row.label] == nil {
			byLabel[row.label] = map[string]any{}
		}
		byLabel[row.label][key] = job
	}
	groups := map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 1}}
	makePrivateTestDirectory(t, filepath.Join(fixture.base, directory))
	if directory == "label-status" {
		writePrivateTestFile(t, filepath.Join(fixture.base, "all-status.json"), string(statusTestPayload(t, all, groups)))
	}
	for label, jobs := range byLabel {
		writePrivateTestFile(t, filepath.Join(fixture.base, directory, label+".json"), string(statusTestPayload(t, jobs, groups)))
	}
}

// commands returns every logged status and remove argv after the config.
func (fixture labelFilterCleanupFixture) commands(t *testing.T) [][]string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(fixture.base, "cleanup-commands.log")); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var commands [][]string
	for _, argv := range readInvocations(t, filepath.Join(fixture.base, "cleanup-commands.log")) {
		commands = append(commands, argv[2:])
	}
	return commands
}

// TestCompletedTaskCleanupUsesOnlyExactLabelFacts drives one cleanup attempt
// per state against a supervisor that honors the exact label filter. Only the
// filtered query is issued: once under the shared lease, and once more under
// the exclusive lease whenever a label row was returned. An absent label is a
// clean logical fact that leaves the unrelated occupant of the saved numeric
// ID untouched; duplicate, mismatched, and unended rows stay unknown; a
// running row defers; only the exact ended row is removed. The legacy
// projection keeps its markers.
func TestCompletedTaskCleanupUsesOnlyExactLabelFacts(t *testing.T) {
	foreign := Identity{RootID: strings.Repeat("f", 32), TaskID: strings.Repeat("4", 32)}.Label()
	cases := []struct {
		name    string
		rows    func(label string) []cleanupTestRow
		want    CompletedTaskCleanup
		legacy  error
		queries int
		removed bool
	}{
		{name: "unrelated occupant at saved ID", want: CompletedTaskLogicalLabelAbsent, legacy: ErrUnknown, queries: 1, rows: func(string) []cleanupTestRow {
			return []cleanupTestRow{{id: 3, label: foreign, state: StateEnded}}
		}},
		{name: "no label rows", want: CompletedTaskLogicalLabelAbsent, legacy: ErrUnknown, queries: 1, rows: func(string) []cleanupTestRow { return nil }},
		{name: "duplicate exact labels", legacy: ErrUnknown, queries: 2, rows: func(label string) []cleanupTestRow {
			return []cleanupTestRow{{id: 3, label: label, state: StateEnded}, {id: 4, label: label, state: StateEnded}}
		}},
		{name: "exact label at another ID", legacy: ErrUnknown, queries: 2, rows: func(label string) []cleanupTestRow {
			return []cleanupTestRow{{id: 3, label: foreign, state: StateEnded}, {id: 5, label: label, state: StateEnded}}
		}},
		{name: "exact row not ended", legacy: ErrUnknown, queries: 2, rows: func(label string) []cleanupTestRow {
			return []cleanupTestRow{{id: 3, label: label, state: StateStashed}}
		}},
		{name: "exact row running", want: CompletedTaskCleanupDeferred, legacy: ErrCleanupDeferred, queries: 2, rows: func(label string) []cleanupTestRow {
			return []cleanupTestRow{{id: 3, label: label, state: StateRunning}}
		}},
		{name: "exact row ended", want: CompletedTaskRemoved, queries: 2, removed: true, rows: func(label string) []cleanupTestRow {
			return []cleanupTestRow{{id: 3, label: label, state: StateEnded}, {id: 4, label: foreign, state: StateEnded}}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			typed := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 10 * time.Second})
			typed.install(t, testCase.rows(typed.identity.Label())...)
			disposition, err := typed.client.AttemptCompletedTaskCleanup(t.Context(), typed.identity)
			if disposition != testCase.want || (testCase.want == 0) != errors.Is(err, ErrUnknown) || (testCase.want != 0 && err != nil) {
				t.Fatalf("attempt = %d, %v; want disposition %d", disposition, err, testCase.want)
			}
			assertCleanupTestCommands(t, typed, testCase.queries, testCase.removed)

			legacy := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 10 * time.Second})
			legacy.install(t, testCase.rows(legacy.identity.Label())...)
			// A disposition's marker is returned as the exact sentinel; an unknown
			// selection keeps its identity through the lock wrapper.
			err = legacy.client.RemoveCompletedTask(t.Context(), legacy.identity)
			if !errors.Is(err, testCase.legacy) || (testCase.want != 0 && !bareCleanupMarker(err)) {
				t.Fatalf("legacy projection = %v, want %v", err, testCase.legacy)
			}
			assertCleanupTestCommands(t, legacy, testCase.queries, testCase.removed)
		})
	}
}

// bareCleanupMarker reports that err wraps nothing, so a matching errors.Is
// identifies the marker itself rather than a joined failure.
func bareCleanupMarker(err error) bool {
	_, joined := err.(interface{ Unwrap() []error })
	return !joined && errors.Unwrap(err) == nil
}

// assertCleanupTestCommands requires exactly the given number of filtered
// status queries, no unfiltered scan, and a removal of saved ID 3 only when
// expected.
func assertCleanupTestCommands(t *testing.T, fixture labelFilterCleanupFixture, queries int, removed bool) {
	t.Helper()
	var want [][]string
	for range queries {
		want = append(want, []string{"status", "--json", "label=" + fixture.identity.Label()})
	}
	if removed {
		want = append(want, []string{"remove", "3"})
	}
	if got := fixture.commands(t); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("cleanup commands = %q, want %q", got, want)
	}
}

// TestCompletedTaskCleanupMasksDispositionOnOperationalFault keeps a timed-out
// status observation as an operational error with no disposition and no
// removal, and proves the attempt record's projections for a real lock
// release fault joined beside each disposition.
func TestCompletedTaskCleanupMasksDispositionOnOperationalFault(t *testing.T) {
	t.Run("status timeout", func(t *testing.T) {
		fixture := newLabelFilterCleanupFixture(t, Options{ObservationTimeout: 100 * time.Millisecond})
		fixture.install(t, cleanupTestRow{id: 3, label: fixture.identity.Label(), state: StateEnded})
		writePrivateTestFile(t, filepath.Join(fixture.base, "status-hold"), "")
		disposition, err := fixture.client.AttemptCompletedTaskCleanup(t.Context(), fixture.identity)
		for _, pending := range pendingHandles(err) {
			waitPending(t, pending)
		}
		if disposition != 0 || err == nil {
			t.Fatalf("timed-out cleanup = %d, %v; want an operational error", disposition, err)
		}
		assertCleanupTestCommands(t, fixture, 1, false)
	})
	t.Run("release fault beside disposition", func(t *testing.T) {
		releaseErr := closedBootstrapLockReleaseFault(t)
		for _, testCase := range []struct {
			disposition CompletedTaskCleanup
			marker      error
		}{
			{disposition: CompletedTaskRemoved},
			{disposition: CompletedTaskCleanupDeferred, marker: ErrCleanupDeferred},
			{disposition: CompletedTaskLogicalLabelAbsent, marker: ErrUnknown},
		} {
			attempt := completedTaskCleanupAttempt{disposition: testCase.disposition, err: releaseErr}
			if disposition, err := attempt.result(); disposition != 0 || !errors.Is(err, releaseErr) {
				t.Fatalf("typed result kept disposition %d beside a release fault: %d, %v", testCase.disposition, disposition, err)
			}
			legacy := attempt.legacyError()
			if !errors.Is(legacy, releaseErr) || (testCase.marker != nil && !errors.Is(legacy, testCase.marker)) {
				t.Fatalf("legacy projection of %d lost a cause: %v", testCase.disposition, legacy)
			}
		}
	})
}
