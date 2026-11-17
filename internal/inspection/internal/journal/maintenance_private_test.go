package journal

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// privateLifetimeReader decodes copied records only; the domain resolver below
// validates their identity/digest links. This adapter proves no live exclusion.
type privateLifetimeReader struct{ dir string }

func readPrivateLifetime[T any](ctx context.Context, dir, name string) (*T, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	var record T
	if err = task.DecodeStrict(data, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

func (r privateLifetimeReader) ReadLifetimeIntentContext(ctx context.Context) (*task.LifetimeIntentRecord, error) {
	return readPrivateLifetime[task.LifetimeIntentRecord](ctx, r.dir, "intent.json")
}

func (r privateLifetimeReader) ReadLifetimeRegistrationContext(ctx context.Context) (*task.LifetimeRegistrationRecord, error) {
	return readPrivateLifetime[task.LifetimeRegistrationRecord](ctx, r.dir, "registration.json")
}

func (r privateLifetimeReader) ReadLifetimeWorkerContext(ctx context.Context) (*task.LifetimeWorkerRecord, error) {
	return readPrivateLifetime[task.LifetimeWorkerRecord](ctx, r.dir, "worker.json")
}

func (r privateLifetimeReader) ReadLifetimeWorkerWaitContext(ctx context.Context) (*task.LifetimeWorkerWaitRecord, error) {
	return readPrivateLifetime[task.LifetimeWorkerWaitRecord](ctx, r.dir, "worker-wait.json")
}

func (r privateLifetimeReader) ReadLifetimeTerminationContext(ctx context.Context) (*task.LifetimeTerminationRecord, error) {
	return readPrivateLifetime[task.LifetimeTerminationRecord](ctx, r.dir, "terminated.json")
}

func (r privateLifetimeReader) ReadLifetimeLaunchesContext(ctx context.Context) ([]task.LifetimeLaunchRecord, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".launch.json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	records := make([]task.LifetimeLaunchRecord, 0, len(names))
	for _, name := range names {
		record, err := readPrivateLifetime[task.LifetimeLaunchRecord](ctx, r.dir, name)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	return records, nil
}

func TestPrivateStartedTaskClosureBindings(t *testing.T) {
	source := os.Getenv("DELEGATE_PRIVATE_LIFETIME_FIXTURES")
	if source == "" {
		t.Skip("private lifecycle copies not supplied")
	}
	cases := map[string]task.LifetimeClosureKind{
		"265a78031cd17e11159fc56676b76519": task.LifetimeClosureRegisteredNative,
		"a9047232968d70aa15ecf79a790caad9": task.LifetimeClosureRegisteredRecovered,
	}
	for id, want := range cases {
		t.Run(id, func(t *testing.T) {
			evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), privateLifetimeReader{dir: filepath.Join(source, id, "lifetime")})
			mustInspection(t, err)
			closure, err := task.ResolveLifetimeClosure(evidence)
			mustInspection(t, err)
			if closure.Kind != want {
				t.Fatalf("validated closure kind=%s, expected %s", closure.Kind, want)
			}
		})
	}
}
