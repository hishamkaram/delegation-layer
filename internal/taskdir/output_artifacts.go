package taskdir

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

var errNativeOutputAbsent = errors.New("optional native output file absent")

// ImportOutputArtifacts is called by execution only after observed process
// termination and both stream EOFs. The declared writer contract requires no
// output writer to survive that boundary. Snapshot checks below detect faults.
func (td *TaskDir) ImportOutputArtifacts() (resultErr error) {
	state, err := td.ownedRunner()
	if err != nil {
		return err
	}
	if state.sealed || state.poisoned != nil || state.activeWriters != 0 || !state.filesPrepared || state.artifactsImported {
		state.mu.Unlock()
		return task.ErrEvidenceFault
	}
	state.activeWriters++ // fences Seal and lease release throughout import
	state.mu.Unlock()
	defer func() {
		state.mu.Lock()
		state.activeWriters--
		state.artifactsImported = resultErr == nil
		state.poisoned = errors.Join(state.poisoned, resultErr)
		state.mu.Unlock()
	}()
	_, meta, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	for _, output := range meta.OutputArtifacts {
		if err := td.importOutput(output.Name); err != nil {
			return errors.Join(task.ErrEvidenceFault, err)
		}
	}
	return nil
}

func (td *TaskDir) importOutput(name string) error {
	// Validate the directory independently: its disappearance is never an
	// optional file absence.
	dir, err := td.store.openDir(filepath.Join(td.Dir, "provider-output"))
	if err != nil {
		return err
	}
	if err = dir.Close(); err != nil {
		return err
	}
	f, err := td.openNativeOutput(name)
	if errors.Is(err, errNativeOutputAbsent) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return errors.Join(err, f.Close())
	}
	r := &artifactReader{
		td: td, name: name, initial: info,
		reader: io.NewSectionReader(f, 0, info.Size()+1), digest: sha256.New(),
	}
	_, cleanup, err := td.store.stageReader(filepath.Join(td.Dir, "raw"), name, r, td.store.faultInjector)
	return errors.Join(err, cleanup, f.Close())
}

type artifactReader struct {
	td      *TaskDir
	name    string
	initial os.FileInfo
	reader  io.Reader
	digest  hash.Hash
	count   int64
	checked bool
}

func (r *artifactReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		if _, hashErr := r.digest.Write(p[:n]); hashErr != nil {
			return n, hashErr
		}
		r.count += int64(n)
	}
	if errors.Is(err, io.EOF) && !r.checked {
		r.checked = true
		if checkErr := r.validateSnapshot(); checkErr != nil {
			return n, checkErr
		}
	}
	return n, err
}

func (r *artifactReader) validateSnapshot() (resultErr error) {
	if r.count != r.initial.Size() {
		return task.ErrEvidenceFault
	}
	current, err := r.td.openNativeOutput(r.name)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, current.Close()) }()
	info, err := current.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(r.initial, info) || info.Size() != r.initial.Size() || !info.ModTime().Equal(r.initial.ModTime()) {
		return task.ErrEvidenceFault
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.NewSectionReader(current, 0, info.Size()+1))
	if err != nil {
		return err
	}
	if n != r.count || !bytes.Equal(digest.Sum(nil), r.digest.Sum(nil)) {
		return task.ErrEvidenceFault
	}
	return nil
}
