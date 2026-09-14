package taskdir

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PreparedIdentity returns the exact validated on-disk byte identities. A
// caller must not reserialize decoded records to reconstruct these digests.
func (td *TaskDir) PreparedIdentity() (*task.TaskRecord, *task.MetaRecord, string, string, error) {
	_, request, meta, specHash, metaHash, err := td.loadAndValidatePreparedSet()
	return request, meta, specHash, metaHash, err
}

// LogDescriptor describes a raw stream without returning its bytes or a file
// handle. A live stream has a location and availability, never a final digest.
type LogDescriptor struct {
	Path      string `json:"path"`
	Available bool   `json:"available"`
	Sealed    bool   `json:"sealed"`
	Size      *int64 `json:"size,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// LogDescriptors is observation-only and does not acquire an execution permit.
// A seal or validated terminal winner supplies final raw identities. Otherwise
// each regular raw file is only reported as an available, potentially live path.
func (td *TaskDir) LogDescriptors() ([]LogDescriptor, error) {
	inspection, err := td.Inspect()
	if err != nil {
		return nil, err
	}
	if inspection.SealExists || inspection.Outcome != nil {
		manifest, err := td.descriptorManifest(inspection)
		if err != nil {
			return nil, err
		}
		return td.sealedLogDescriptors(manifest), nil
	}
	descriptors := make([]LogDescriptor, 0, 2)
	for _, name := range []string{"stderr", "stdout"} {
		path := filepath.Join(td.Dir, "raw", name)
		file, err := td.store.openFile(path, os.O_RDONLY)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		descriptor := LogDescriptor{Path: path, Available: err == nil}
		if file != nil {
			if err = file.Close(); err != nil {
				return nil, err
			}
		}
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, nil
}

func (td *TaskDir) descriptorManifest(inspection *TaskInspection) ([]task.RawManifestEntry, error) {
	if inspection.Outcome != nil {
		manifest, digest, err := td.rawManifest()
		if err != nil {
			return nil, err
		}
		if digest != inspection.Outcome.EvidenceSHA256 {
			return nil, task.ErrEvidenceFault
		}
		return manifest, nil
	}
	_, meta, specHash, metaHash, err := td.PreparedIdentity()
	if err != nil {
		return nil, err
	}
	seal, err := td.readSeal(meta.Predicate, specHash, metaHash)
	if err != nil {
		return nil, err
	}
	return seal.RawManifest, nil
}

func (td *TaskDir) sealedLogDescriptors(manifest []task.RawManifestEntry) []LogDescriptor {
	descriptors := make([]LogDescriptor, 0, len(manifest))
	for _, entry := range manifest {
		size := entry.Size
		descriptors = append(descriptors, LogDescriptor{
			Path: filepath.Join(td.Dir, entry.Path), Available: true,
			Sealed: true, Size: &size, SHA256: entry.SHA256,
		})
	}
	return descriptors
}
