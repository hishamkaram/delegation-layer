package git

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// BuildCapture builds a Git tree from the observed checkout without changing
// the checkout's real index. The private index starts at the observed HEAD,
// overlays current stage-zero entries, and then adds the current worktree.
func (g Git) BuildCapture(ctx context.Context, checkout contract.Checkout, scratchDir string) (candidate contract.CaptureCandidate, err error) {
	if ctx == nil {
		return contract.CaptureCandidate{}, errors.Join(contract.ErrInvalidGitContext, errors.New("nil capture context"))
	}
	if err = g.validateCheckout(ctx, checkout); err != nil {
		return contract.CaptureCandidate{}, err
	}
	scratch, scratchErr := existingDirectory(scratchDir)
	if scratchErr != nil {
		return contract.CaptureCandidate{}, fmt.Errorf("validate capture scratch directory: %w", scratchErr)
	}
	if config.PathsOverlap(scratch, checkout.CheckoutPath) || config.PathsOverlap(scratch, checkout.Workdir) || config.PathsOverlap(scratch, checkout.Source.Root) || config.PathsOverlap(scratch, checkout.Source.CommonDir) || config.PathsOverlap(scratch, checkout.Source.GitDir) {
		return contract.CaptureCandidate{}, fmt.Errorf("%w: capture scratch directory overlaps Git context", contract.ErrInvalidGitContext)
	}
	if err = g.verifyObject(ctx, checkout.Source.Root, checkout.Source.BaseOID, checkout.Source.ObjectFormat, "commit"); err != nil {
		return contract.CaptureCandidate{}, fmt.Errorf("validate capture source base: %w", err)
	}
	observedHead, observeErr := g.runScalar(ctx, checkout.CheckoutPath, []string{"rev-parse", "--verify", "--end-of-options", "HEAD^{commit}"}, nil)
	if observeErr != nil {
		return contract.CaptureCandidate{}, fmt.Errorf("observe checkout HEAD: %w", observeErr)
	}
	if validateErr := contract.ValidateOID(observedHead, checkout.Source.ObjectFormat); validateErr != nil {
		return contract.CaptureCandidate{}, fmt.Errorf("observe checkout HEAD: %w", validateErr)
	}
	scratchFiles, scratchErr := newCaptureScratch(scratch)
	if scratchErr != nil {
		return contract.CaptureCandidate{}, scratchErr
	}
	defer func() { err = errors.Join(err, removeScratchFiles(scratchFiles.all())) }()
	treeOID, treeErr := g.buildPrivateTree(ctx, checkout, scratchFiles)
	if treeErr != nil {
		return contract.CaptureCandidate{}, fmt.Errorf("write Git capture tree: %w", treeErr)
	}
	if err = g.verifyObject(ctx, checkout.CheckoutPath, treeOID, checkout.Source.ObjectFormat, "tree"); err != nil {
		return contract.CaptureCandidate{}, fmt.Errorf("validate capture tree: %w", err)
	}
	return contract.CaptureCandidate{BaseOID: checkout.Source.BaseOID, ObservedHEAD: observedHead, TreeOID: treeOID, ObjectFormat: checkout.Source.ObjectFormat}, nil
}

type captureScratch struct {
	index    string
	skip     string
	preserve string
	rules    string
	overlay  string
}

func newCaptureScratch(directory string) (captureScratch, error) {
	prefixes := []string{"delegate-index-", "delegate-skip-", "delegate-preserve-", "delegate-rules-", "delegate-overlay-"}
	paths := make([]string, len(prefixes))
	for index, prefix := range prefixes {
		path, err := temporaryPath(directory, prefix)
		if err != nil {
			return captureScratch{}, errors.Join(err, removeScratchFiles(paths[:index]))
		}
		paths[index] = path
	}
	return captureScratch{index: paths[0], skip: paths[1], preserve: paths[2], rules: paths[3], overlay: paths[4]}, nil
}

func (s captureScratch) all() []string {
	return []string{s.index, s.skip, s.preserve, s.rules, s.overlay}
}

func (g Git) buildPrivateTree(ctx context.Context, checkout contract.Checkout, scratch captureScratch) (string, error) {
	if err := g.run(ctx, checkout.CheckoutPath, []string{"read-tree", "HEAD"}, map[string]string{"GIT_INDEX_FILE": scratch.index}, nil, nil, nil); err != nil {
		return "", fmt.Errorf("seed private Git index: %w", err)
	}
	if err := g.collectStageZero(ctx, checkout, scratch.overlay); err != nil {
		return "", err
	}
	if err := applyIndexInput(ctx, g, checkout.CheckoutPath, scratch.index, scratch.overlay, []string{"update-index", "-z", "--index-info"}); err != nil {
		return "", fmt.Errorf("overlay current Git index: %w", err)
	}
	if err := g.collectAbsentSkipped(ctx, checkout, scratch.skip); err != nil {
		return "", err
	}
	if err := g.prepareSparsePreservation(ctx, checkout, scratch.skip, scratch.preserve, scratch.rules); err != nil {
		return "", err
	}
	if err := applyIndexInput(ctx, g, checkout.CheckoutPath, scratch.index, scratch.preserve, []string{"update-index", "--skip-worktree", "-z", "--stdin"}); err != nil {
		return "", fmt.Errorf("preserve sparse Git entries: %w", err)
	}
	if err := g.run(ctx, checkout.CheckoutPath, []string{"add", "--sparse", "-A"}, map[string]string{"GIT_INDEX_FILE": scratch.index}, nil, nil, nil); err != nil {
		return "", fmt.Errorf("add checkout content to private Git index: %w", err)
	}
	treeOID, err := g.runScalar(ctx, checkout.CheckoutPath, []string{"write-tree"}, map[string]string{"GIT_INDEX_FILE": scratch.index})
	if err != nil {
		return "", err
	}
	if err = contract.ValidateOID(treeOID, checkout.Source.ObjectFormat); err != nil {
		return "", err
	}
	return treeOID, nil
}

func (g Git) collectStageZero(ctx context.Context, checkout contract.Checkout, overlayPath string) (result error) {
	output, err := os.OpenFile(overlayPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open private Git index overlay: %w", err)
	}
	input, err := os.CreateTemp(filepath.Dir(overlayPath), "delegate-stage-source-")
	if err != nil {
		closeErr := output.Close()
		return errors.Join(fmt.Errorf("create Git stage source: %w", err), closeErr)
	}
	inputPath := input.Name()
	closeInput := func() error {
		closeErr := input.Close()
		removeErr := os.Remove(inputPath)
		return errors.Join(closeErr, removeErr)
	}
	defer func() { result = errors.Join(result, output.Close(), closeInput()) }()
	if err = g.run(ctx, checkout.CheckoutPath, []string{"ls-files", "--stage", "-z"}, nil, nil, input, nil); err != nil {
		return fmt.Errorf("read current Git index: %w", err)
	}
	if err = input.Sync(); err != nil {
		return fmt.Errorf("sync current Git index: %w", err)
	}
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind current Git index: %w", err)
	}
	reader := bufio.NewReader(input)
	for {
		record, readErr := readNULRecord(reader)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		stage, validErr := stageZeroRecord(record, checkout.Source.ObjectFormat)
		if validErr != nil {
			return validErr
		}
		if !stage {
			continue
		}
		if _, err = output.Write(append(append([]byte(nil), record...), 0)); err != nil {
			return fmt.Errorf("write private Git index overlay: %w", err)
		}
	}
	if err = output.Sync(); err != nil {
		return fmt.Errorf("sync private Git index overlay: %w", err)
	}
	return nil
}

func stageZeroRecord(record []byte, objectFormat string) (bool, error) {
	separator := indexRecordSeparator(record)
	if separator < 0 {
		return false, errors.New("malformed Git stage record")
	}
	header := string(record[:separator])
	fields := strings.Split(header, " ")
	if len(fields) != 3 || fields[0] == "" || fields[1] == "" || fields[2] == "" {
		return false, errors.New("malformed Git stage header")
	}
	if err := validateIndexMode(fields[0]); err != nil {
		return false, err
	}
	stage, err := strconv.Atoi(fields[2])
	if err != nil || stage < 0 || stage > 3 {
		return false, errors.New("invalid Git stage number")
	}
	if err = contract.ValidateOID(fields[1], objectFormat); err != nil {
		return false, err
	}
	if len(record[separator+1:]) == 0 {
		return false, errors.New("missing Git index path")
	}
	return stage == 0, nil
}

func validateIndexMode(mode string) error {
	switch mode {
	case "100644", "100755", "120000", "160000":
		return nil
	default:
		return fmt.Errorf("invalid Git index mode %q", mode)
	}
}

func indexRecordSeparator(record []byte) int {
	for index, value := range record {
		if value == '\t' {
			return index
		}
	}
	return -1
}

func (g Git) collectAbsentSkipped(ctx context.Context, checkout contract.Checkout, skipPath string) (result error) {
	output, err := os.OpenFile(skipPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open skipped Git entries: %w", err)
	}
	input, err := os.CreateTemp(filepath.Dir(skipPath), "delegate-skip-source-")
	if err != nil {
		closeErr := output.Close()
		return errors.Join(fmt.Errorf("create skipped Git source: %w", err), closeErr)
	}
	inputPath := input.Name()
	defer func() {
		result = errors.Join(result, output.Close(), input.Close(), os.Remove(inputPath))
	}()
	if err = g.run(ctx, checkout.CheckoutPath, []string{"ls-files", "-v", "-z"}, nil, nil, input, nil); err != nil {
		return fmt.Errorf("read Git skip-worktree flags: %w", err)
	}
	if err = input.Sync(); err != nil {
		return fmt.Errorf("sync Git skip-worktree flags: %w", err)
	}
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind Git skip-worktree flags: %w", err)
	}
	if err = copyAbsentSkippedRecords(bufio.NewReader(input), output, checkout.CheckoutPath); err != nil {
		return err
	}
	if err = output.Sync(); err != nil {
		return fmt.Errorf("sync skipped Git paths: %w", err)
	}
	return nil
}

func copyAbsentSkippedRecords(reader *bufio.Reader, output *os.File, workdir string) error {
	for {
		record, readErr := readNULRecord(reader)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		if len(record) < 3 || record[1] != ' ' {
			return errors.New("malformed Git skip-worktree record")
		}
		if record[0] != 'S' && record[0] != 's' {
			continue
		}
		path, pathErr := worktreeRelativePath(workdir, record[2:])
		if pathErr != nil {
			return pathErr
		}
		if _, statErr := os.Lstat(path); statErr == nil {
			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect skipped Git path: %w", statErr)
		}
		if _, writeErr := output.Write(append(append([]byte(nil), record[2:]...), 0)); writeErr != nil {
			return fmt.Errorf("write skipped Git path: %w", writeErr)
		}
	}
	return nil
}

func (g Git) prepareSparsePreservation(ctx context.Context, checkout contract.Checkout, skipPath, preservePath, rulesPath string) (result error) {
	if err := truncateFile(preservePath); err != nil {
		return err
	}
	paths, err := readNULFile(skipPath)
	if err != nil {
		return fmt.Errorf("read skipped Git paths: %w", err)
	}
	if len(paths) == 0 {
		return nil
	}
	sparse, err := g.runScalar(ctx, checkout.CheckoutPath, []string{"config", "--bool", "--default", "false", "core.sparseCheckout"}, nil)
	if err != nil {
		return fmt.Errorf("read sparse-checkout setting: %w", err)
	}
	if sparse != "true" {
		return nil
	}
	included, err := g.sparseIncludedPaths(ctx, checkout.CheckoutPath, skipPath, rulesPath)
	if err != nil {
		return fmt.Errorf("read sparse-checkout rules: %w", err)
	}
	if err = writeSparsePreservation(preservePath, paths, included); err != nil {
		return err
	}
	return nil
}

func (g Git) sparseIncludedPaths(ctx context.Context, workdir, skipPath, rulesPath string) (included map[string]struct{}, resultErr error) {
	input, err := os.Open(skipPath)
	if err != nil {
		return nil, fmt.Errorf("open sparse-checkout rules input: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, input.Close()) }()
	rules, err := os.OpenFile(rulesPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open sparse-checkout rules output: %w", err)
	}
	if err = g.run(ctx, workdir, []string{"sparse-checkout", "check-rules", "-z"}, nil, input, rules, nil); err != nil {
		closeErr := rules.Close()
		return nil, errors.Join(fmt.Errorf("check sparse-checkout rules: %w", err), closeErr)
	}
	if err = rules.Sync(); err != nil {
		closeErr := rules.Close()
		return nil, errors.Join(fmt.Errorf("sync sparse-checkout rules: %w", err), closeErr)
	}
	if err = rules.Close(); err != nil {
		return nil, fmt.Errorf("close sparse-checkout rules: %w", err)
	}
	return readNULSet(rulesPath)
}

func writeSparsePreservation(path string, paths [][]byte, included map[string]struct{}) error {
	preserve, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open sparse preservation: %w", err)
	}
	for _, entry := range paths {
		if _, inside := included[string(entry)]; inside {
			continue
		}
		if _, err = preserve.Write(append(append([]byte(nil), entry...), 0)); err != nil {
			closeErr := preserve.Close()
			return errors.Join(fmt.Errorf("write sparse preservation: %w", err), closeErr)
		}
	}
	if err = preserve.Sync(); err != nil {
		closeErr := preserve.Close()
		return errors.Join(fmt.Errorf("sync sparse preservation: %w", err), closeErr)
	}
	if err = preserve.Close(); err != nil {
		return fmt.Errorf("close sparse preservation: %w", err)
	}
	return nil
}

func applyIndexInput(ctx context.Context, git Git, workdir, indexPath, inputPath string, args []string) (result error) {
	input, err := os.Open(inputPath)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, input.Close()) }()
	if err = git.run(ctx, workdir, args, map[string]string{"GIT_INDEX_FILE": indexPath}, input, nil, nil); err != nil {
		return err
	}
	return nil
}

func readNULFile(path string) (records [][]byte, result error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	reader := bufio.NewReader(file)
	for {
		record, readErr := readNULRecord(reader)
		if errors.Is(readErr, io.EOF) {
			return records, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		records = append(records, append([]byte(nil), record...))
	}
}

func readNULSet(path string) (map[string]struct{}, error) {
	records, err := readNULFile(path)
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{}, len(records))
	for _, record := range records {
		result[string(record)] = struct{}{}
	}
	return result, nil
}

func truncateFile(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("truncate Git capture input: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close Git capture input: %w", err)
	}
	return nil
}

func worktreeRelativePath(workdir string, gitPath []byte) (string, error) {
	if len(gitPath) == 0 || strings.ContainsRune(string(gitPath), '\x00') {
		return "", errors.Join(contract.ErrIdentityMismatch, errors.New("invalid Git worktree path"))
	}
	relative := filepath.FromSlash(string(gitPath))
	if filepath.IsAbs(relative) {
		return "", errors.Join(contract.ErrIdentityMismatch, errors.New("absolute Git worktree path"))
	}
	joined := filepath.Join(workdir, relative)
	rel, err := filepath.Rel(workdir, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.Join(contract.ErrIdentityMismatch, errors.New("Git worktree path escapes checkout"))
	}
	return joined, nil
}

func temporaryPath(directory, pattern string) (string, error) {
	file, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return "", fmt.Errorf("create Git capture scratch file: %w", err)
	}
	path := file.Name()
	if err = file.Close(); err != nil {
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("close Git capture scratch file: %w", err), removeErr)
	}
	if err = os.Remove(path); err != nil {
		return "", fmt.Errorf("prepare Git capture scratch path: %w", err)
	}
	return path, nil
}

func removeScratchFiles(paths []string) error {
	var joined error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			joined = errors.Join(joined, fmt.Errorf("remove Git capture scratch file %s: %w", path, err))
		}
	}
	return joined
}

// ExportCapture writes the bounded binary patch and NUL-delimited name-status
// view for a capture candidate. Each stream is drained to completion; an
// artifact above ArtifactLimit is reported as omitted and no
// truncated file is retained.
func (g Git) ExportCapture(ctx context.Context, checkout contract.Checkout, candidate contract.CaptureCandidate, scratchDir string) (export contract.CaptureExport, err error) {
	if ctx == nil {
		return contract.CaptureExport{}, errors.Join(contract.ErrInvalidGitContext, errors.New("nil export context"))
	}
	if err = g.validateCheckout(ctx, checkout); err != nil {
		return contract.CaptureExport{}, err
	}
	if err = validateCaptureCandidate(candidate, checkout.Source); err != nil {
		return contract.CaptureExport{}, err
	}
	if err = g.validateCandidateObjects(ctx, checkout, candidate); err != nil {
		return contract.CaptureExport{}, err
	}
	scratch, scratchErr := existingDirectory(scratchDir)
	if scratchErr != nil {
		return contract.CaptureExport{}, fmt.Errorf("validate export scratch directory: %w", scratchErr)
	}
	if config.PathsOverlap(scratch, checkout.CheckoutPath) || config.PathsOverlap(scratch, checkout.Workdir) || config.PathsOverlap(scratch, checkout.Source.Root) || config.PathsOverlap(scratch, checkout.Source.CommonDir) || config.PathsOverlap(scratch, checkout.Source.GitDir) {
		return contract.CaptureExport{}, fmt.Errorf("%w: export scratch directory overlaps Git context", contract.ErrInvalidGitContext)
	}
	patch, patchErr := g.exportArtifact(ctx, checkout, scratch, "diff.patch", []string{"diff", "--binary", "--full-index", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/", "--end-of-options", candidate.BaseOID, candidate.TreeOID})
	if patchErr != nil {
		return contract.CaptureExport{Patch: patch}, patchErr
	}
	nameStatus, nameStatusErr := g.exportArtifact(ctx, checkout, scratch, "name-status.z", []string{"diff", "--name-status", "-z", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--end-of-options", candidate.BaseOID, candidate.TreeOID})
	if nameStatusErr != nil {
		return contract.CaptureExport{Patch: patch, NameStatus: nameStatus}, nameStatusErr
	}
	return contract.CaptureExport{Patch: patch, NameStatus: nameStatus}, nil
}

func validateCaptureCandidate(candidate contract.CaptureCandidate, source contract.Source) error {
	if candidate.ObjectFormat != "" && candidate.ObjectFormat != source.ObjectFormat {
		return fmt.Errorf("%w: capture object format differs from source", contract.ErrIdentityMismatch)
	}
	if err := contract.ValidateOID(candidate.BaseOID, source.ObjectFormat); err != nil {
		return fmt.Errorf("invalid capture base OID: %w", err)
	}
	if err := contract.ValidateOID(candidate.ObservedHEAD, source.ObjectFormat); err != nil {
		return fmt.Errorf("invalid observed checkout HEAD: %w", err)
	}
	if err := contract.ValidateOID(candidate.TreeOID, source.ObjectFormat); err != nil {
		return fmt.Errorf("invalid capture tree OID: %w", err)
	}
	if !strings.EqualFold(candidate.BaseOID, source.BaseOID) {
		return fmt.Errorf("%w: capture base differs from source base", contract.ErrIdentityMismatch)
	}
	return nil
}

func (g Git) exportArtifact(ctx context.Context, checkout contract.Checkout, scratchDir, name string, args []string) (contract.CaptureArtifact, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsRune(name, '\x00') {
		return contract.CaptureArtifact{}, errors.Join(contract.ErrInvalidGitContext, errors.New("invalid Git capture artifact name"))
	}
	path := filepath.Join(scratchDir, name)
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return contract.CaptureArtifact{}, fmt.Errorf("%w: Git capture artifact path is not a regular file", contract.ErrInvalidGitContext)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return contract.CaptureArtifact{}, fmt.Errorf("inspect Git capture artifact path: %w", statErr)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return contract.CaptureArtifact{}, fmt.Errorf("create Git capture artifact: %w", err)
	}
	writer := &artifactWriter{file: file, limit: contract.ArtifactLimit, hash: sha256.New()}
	runErr := g.run(ctx, checkout.CheckoutPath, args, nil, nil, writer, nil)
	syncErr := file.Sync()
	closeErr := file.Close()
	if runErr != nil || writer.writeErr != nil || syncErr != nil || closeErr != nil {
		removeErr := os.Remove(path)
		return contract.CaptureArtifact{Length: writer.length, SHA256: writer.digest(), OmittedReason: "write"}, errors.Join(runErr, writer.writeErr, syncErr, closeErr, removeErr)
	}
	if writer.overflow {
		if removeErr := os.Remove(path); removeErr != nil {
			return contract.CaptureArtifact{OmittedReason: "limit"}, fmt.Errorf("remove oversized Git capture artifact: %w", removeErr)
		}
		return contract.CaptureArtifact{OmittedReason: "limit"}, nil
	}
	return contract.CaptureArtifact{Path: path, Length: writer.length, SHA256: writer.digest()}, nil
}

type artifactWriter struct {
	file     *os.File
	hash     hashWriter
	length   int64
	limit    int64
	overflow bool
	writeErr error
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func (w *artifactWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if w.length > int64(^uint64(0)>>1)-int64(len(data)) {
		w.writeErr = errors.New("Git capture artifact length overflow")
		return len(data), nil
	}
	if _, err := w.hash.Write(data); err != nil && w.writeErr == nil {
		w.writeErr = err
	}
	remaining := w.limit - w.length
	if remaining <= 0 {
		w.overflow = true
		w.length += int64(len(data))
		return len(data), nil
	}
	writeData := data
	if int64(len(writeData)) > remaining {
		writeData = writeData[:remaining]
		w.overflow = true
	}
	if len(writeData) > 0 && w.writeErr == nil {
		written, err := w.file.Write(writeData)
		if err != nil && w.writeErr == nil {
			w.writeErr = err
		}
		if written != len(writeData) && w.writeErr == nil {
			w.writeErr = io.ErrShortWrite
		}
	}
	w.length += int64(len(data))
	return len(data), nil
}

func (w *artifactWriter) digest() string {
	return hex.EncodeToString(w.hash.Sum(nil))
}
