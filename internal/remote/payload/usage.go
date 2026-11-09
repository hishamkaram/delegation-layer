package payload

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/remotefiles"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type (
	FamilyUsage struct {
		Family       remotedata.Family `json:"family"`
		Path         string            `json:"path"`
		Bytes        int64             `json:"bytes"`
		Entries      int               `json:"entries"`
		Availability string            `json:"availability"`
		Truncated    bool              `json:"truncated,omitempty"`
	}
	UsageReport struct {
		Role          remotedata.Role `json:"role"`
		LayoutVersion int             `json:"layout_version"`
		ObservedAt    string          `json:"observed_at"`
		Families      []FamilyUsage   `json:"families"`
		Retired       bool            `json:"retired"`
	}
)

func layoutVersion(layout task.RemoteDataLayout) int {
	if layout == task.RemoteDataOrganized {
		return 2
	}
	return 1
}

func families(role remotedata.Role) []remotedata.Family {
	if role == remotedata.Origin {
		return []remotedata.Family{remotedata.Snapshot}
	}
	return []remotedata.Family{remotedata.Inputs, remotedata.Outputs}
}

func Usage(ctx context.Context, reader *taskdir.ReadOnlyStore, id string, role remotedata.Role) (report UsageReport, resultErr error) {
	report.Role = role
	report.Families = []FamilyUsage{}
	report.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if role == remotedata.Origin {
		without, e := originWithoutSnapshot(ctx, reader, id)
		if e != nil {
			return report, e
		}
		if without {
			report.Families = []FamilyUsage{}
			return report, nil
		}
	}
	layout, err := CurrentLayout(ctx, reader, id, role)
	if errors.Is(err, os.ErrNotExist) && role == remotedata.Origin {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	report.LayoutVersion = layoutVersion(layout)
	_, report.Retired, err = ReadRetirement(ctx, reader, id, role)
	if err != nil {
		return report, err
	}
	for _, family := range families(role) {
		loc := remotedata.Location{Root: reader.Root(), RootID: reader.RootID(), TaskID: id, Layout: layout, Family: family, Role: role}
		observed, e := observeFamilyUsage(ctx, loc, report.Retired)
		report.Families = append(report.Families, observed)
		if e != nil {
			return report, e
		}
	}
	return report, nil
}

func observeFamilyUsage(ctx context.Context, loc remotedata.Location, retired bool) (FamilyUsage, error) {
	observed := FamilyUsage{Family: loc.Family, Availability: "available"}
	path, err := remotedata.Path(loc.Layout, loc.Root, loc.RootID, loc.TaskID, loc.Family)
	observed.Path = path
	if err != nil {
		return observed, err
	}
	root, err := remotedata.Open(loc.Layout, loc.Root, loc.RootID, loc.TaskID, loc.Family, false)
	if errors.Is(err, os.ErrNotExist) {
		observed.Availability = "absent"
		if retired {
			observed.Availability = "retired"
		}
		return observed, nil
	}
	if err != nil {
		return observed, err
	}
	measured, err := remotedata.Measure(ctx, root, remotefiles.MaxEntries)
	observed.Bytes, observed.Entries, observed.Truncated = measured.Bytes, measured.Entries, measured.Truncated
	if measured.Changed {
		observed.Availability = "changed"
	}
	return observed, errors.Join(err, root.Close())
}
