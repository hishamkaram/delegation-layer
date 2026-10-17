package maintainability

import (
	"fmt"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

type Report struct {
	Sources  []policy.Source  `json:"sources,omitempty"`
	Findings []policy.Finding `json:"findings"`
	Deferred int              `json:"deferred"`
}

// Audit keeps fact collection separate from the single dependency/size policy.
func Audit(root string, strict, includeFacts bool) (Report, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return Report{}, err
	}
	root = canonical
	manifest, migration, err := Load(root)
	if err != nil {
		return Report{}, err
	}
	sources, err := Inventory(root, manifest.Generated)
	if err != nil {
		return Report{}, err
	}
	if err = collectGo(root, manifest.Module, sources); err != nil {
		return Report{}, err
	}
	if err = collectOther(root, sources); err != nil {
		return Report{}, err
	}
	packages, err := collectGoPackages(root, manifest.Module)
	if err != nil {
		return Report{}, err
	}
	all := append(append([]policy.Source(nil), sources...), packages...)
	report := Report{Findings: policy.Check(manifest, all)}
	functionFindings, err := collectFunlen(root, sources)
	if err != nil {
		return Report{}, err
	}
	current, err := Inventory(root, manifest.Generated)
	if err != nil {
		return Report{}, err
	}
	if changed := policy.ChangedSources(sources, current); len(changed) != 0 {
		return Report{}, fmt.Errorf("maintained sources changed during audit: %s; rerun after edits finish", changed[0])
	}
	report.Findings = append(report.Findings, functionFindings...)
	report.Findings, report.Deferred = policy.ApplyMigration(report.Findings, sources, migration, strict)
	if includeFacts {
		report.Sources = all
	}
	return report, nil
}

func canonicalRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve audit root: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("canonicalize audit root: %w", err)
	}
	return canonical, nil
}

func Describe(finding policy.Finding) string {
	location := finding.Path
	if finding.Line > 0 {
		location = fmt.Sprintf("%s:%d", location, finding.Line)
	}
	if finding.Actual > 0 {
		return fmt.Sprintf("%s: %s: %s (%d > %d)", location, finding.Kind, finding.Detail, finding.Actual, finding.Limit)
	}
	return fmt.Sprintf("%s: %s: %s", location, finding.Kind, finding.Detail)
}
