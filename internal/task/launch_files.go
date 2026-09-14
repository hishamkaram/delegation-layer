package task

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// MaxOutputArtifacts bounds the number of provider-owned files that a single
// launch can ask the core to stage and import. Output declarations are part of
// the immutable prepared plan, never public raw argv.
const MaxOutputArtifacts = 16

// MaxInputFiles bounds the number of provider-owned configuration files that
// can be prepared for one task.
const MaxInputFiles = 16

// MaxInputBytes bounds the aggregate content carried by immutable input-file
// declarations. These files are for small non-secret provider configuration,
// not prompts, transcripts, credentials, or arbitrary payloads.
const MaxInputBytes = 64 * 1024

// OutputWriterProcessExitEOF is the only v1 writer contract. A profile that
// declares it certifies that its declared output writers are complete when the
// provider has exited and the child's stdout/stderr pipes have reached EOF.
// File existence or a stable snapshot is not a substitute for this contract.
const OutputWriterProcessExitEOF = "process-exit-eof-v1"

var artifactNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// InputFile is a small, non-secret provider configuration file. The execution
// core writes Content to a task-owned path and fills the reserved argv slot;
// providers never supply an ambient path or a shell/template expression.
type InputFile struct {
	Name          string `json:"name"`
	ArgumentIndex int    `json:"argument_index"`
	Content       string `json:"content"`
}

// OutputArtifact is a typed provider output declaration. Name is a safe
// task-local basename used for the staging file and its imported raw entry;
// ArgumentIndex identifies one reserved empty argv slot that the execution
// core fills with the generated staging path.
type OutputArtifact struct {
	Name          string `json:"name"`
	ArgumentIndex int    `json:"argument_index"`
}

// ValidateArtifactName accepts one safe, task-local basename. stdout and
// stderr are reserved for the mandatory process streams.
func ValidateArtifactName(name string) error {
	if !artifactNamePattern.MatchString(name) || name == "stdout" || name == "stderr" {
		return fmt.Errorf("invalid task artifact name %q", name)
	}
	return nil
}

// NormalizeInputFiles validates and returns an independently owned,
// name-sorted copy of input declarations.
func NormalizeInputFiles(files []InputFile) ([]InputFile, error) {
	if len(files) > MaxInputFiles {
		return nil, fmt.Errorf("input file count exceeds %d", MaxInputFiles)
	}
	result := make([]InputFile, len(files))
	var total int
	for index, file := range files {
		if err := ValidateArtifactName(file.Name); err != nil {
			return nil, err
		}
		if file.ArgumentIndex < 0 {
			return nil, fmt.Errorf("input file %q has a negative argument index", file.Name)
		}
		if !utf8.ValidString(file.Content) {
			return nil, fmt.Errorf("input file %q contains invalid UTF-8", file.Name)
		}
		total += len(file.Content)
		if total > MaxInputBytes {
			return nil, fmt.Errorf("input file content exceeds %d bytes", MaxInputBytes)
		}
		result[index] = InputFile{Name: file.Name, ArgumentIndex: file.ArgumentIndex, Content: file.Content}
	}
	slices.SortFunc(result, func(left, right InputFile) int { return strings.Compare(left.Name, right.Name) })
	for index := 1; index < len(result); index++ {
		if result[index-1].Name == result[index].Name {
			return nil, fmt.Errorf("duplicate input file name %q", result[index].Name)
		}
	}
	return result, nil
}

// NormalizeOutputArtifacts validates and returns an independently owned,
// name-sorted copy of output declarations.
func NormalizeOutputArtifacts(artifacts []OutputArtifact) ([]OutputArtifact, error) {
	if len(artifacts) > MaxOutputArtifacts {
		return nil, fmt.Errorf("output artifact count exceeds %d", MaxOutputArtifacts)
	}
	result := slices.Clone(artifacts)
	for _, artifact := range result {
		if err := ValidateArtifactName(artifact.Name); err != nil {
			return nil, err
		}
		if artifact.ArgumentIndex < 0 {
			return nil, fmt.Errorf("output artifact %q has a negative argument index", artifact.Name)
		}
	}
	slices.SortFunc(result, func(left, right OutputArtifact) int { return strings.Compare(left.Name, right.Name) })
	for index := 1; index < len(result); index++ {
		if result[index-1].Name == result[index].Name {
			return nil, fmt.Errorf("duplicate output artifact name %q", result[index].Name)
		}
	}
	return result, nil
}

// ValidateOutputWriterContract validates the versioned completion contract
// advertised by a certified provider profile.
func ValidateOutputWriterContract(contract string) error {
	if contract != "" && contract != OutputWriterProcessExitEOF {
		return fmt.Errorf("unsupported output writer contract %q", contract)
	}
	return nil
}

// ValidateOutputArtifacts validates output declarations and requires the
// certified process-exit contract whenever any output is declared.
func ValidateOutputArtifacts(artifacts []OutputArtifact, writerContract string) error {
	if err := ValidateOutputWriterContract(writerContract); err != nil {
		return err
	}
	normalized, err := NormalizeOutputArtifacts(artifacts)
	if err != nil {
		return err
	}
	if len(normalized) == 0 && writerContract != "" {
		return errors.New("output writer contract requires at least one output artifact")
	}
	if len(normalized) > 0 && writerContract != OutputWriterProcessExitEOF {
		return errors.New("output artifacts require the process-exit-eof-v1 writer contract")
	}
	return nil
}

// ValidateInputOutputBindings validates names and reserved argv-slot identity
// across both declaration sets.
func ValidateInputOutputBindings(inputs []InputFile, outputs []OutputArtifact) error {
	normalizedInputs, err := NormalizeInputFiles(inputs)
	if err != nil {
		return err
	}
	normalizedOutputs, err := NormalizeOutputArtifacts(outputs)
	if err != nil {
		return err
	}
	names := make(map[string]struct{}, len(normalizedInputs)+len(normalizedOutputs))
	indices := make(map[int]string, len(names))
	for _, file := range normalizedInputs {
		if _, exists := names[file.Name]; exists {
			return fmt.Errorf("input/output artifact name shadows %q", file.Name)
		}
		names[file.Name] = struct{}{}
		if previous, exists := indices[file.ArgumentIndex]; exists {
			return fmt.Errorf("argv index %d is shared by %q and %q", file.ArgumentIndex, previous, file.Name)
		}
		indices[file.ArgumentIndex] = file.Name
	}
	for _, artifact := range normalizedOutputs {
		if _, exists := names[artifact.Name]; exists {
			return fmt.Errorf("input/output artifact name shadows %q", artifact.Name)
		}
		names[artifact.Name] = struct{}{}
		if previous, exists := indices[artifact.ArgumentIndex]; exists {
			return fmt.Errorf("argv index %d is shared by %q and %q", artifact.ArgumentIndex, previous, artifact.Name)
		}
		indices[artifact.ArgumentIndex] = artifact.Name
	}
	return nil
}

// CompareInputFiles compares declarations and content without sharing slices.
func CompareInputFiles(left, right []InputFile) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Name != right[index].Name || left[index].ArgumentIndex != right[index].ArgumentIndex || left[index].Content != right[index].Content {
			return false
		}
	}
	return true
}

// CompareOutputArtifacts compares output declarations by value.
func CompareOutputArtifacts(left, right []OutputArtifact) bool {
	return slices.Equal(left, right)
}

func validateCanonicalInputFiles(files []InputFile) error {
	normalized, err := NormalizeInputFiles(files)
	if err != nil {
		return err
	}
	if !CompareInputFiles(files, normalized) {
		return errors.New("input files must be strictly sorted by name")
	}
	return nil
}

func validateCanonicalOutputArtifacts(artifacts []OutputArtifact) error {
	normalized, err := NormalizeOutputArtifacts(artifacts)
	if err != nil {
		return err
	}
	if !CompareOutputArtifacts(artifacts, normalized) {
		return errors.New("output artifacts must be strictly sorted by name")
	}
	return nil
}
