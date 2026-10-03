// Package cicheck holds local checks for the repository's CI definitions.
//
// The release workflow is the one file whose syntax error is invisible until
// a tag is cut: nothing compiles it, and the Python gating checker reads the
// file with its own line-based parser, which stops at the first column-0 line
// it does not recognise as a job. A PowerShell here-string written from
// column 0 therefore produced a release.yml GitHub refuses to load, while
// every local check stayed green.
package cicheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Workflow is the subset of a GitHub Actions workflow this check needs.
type Workflow struct {
	Name string                    `yaml:"name"`
	Jobs map[string]map[string]any `yaml:"jobs"`
}

// LoadWorkflow parses one workflow file. A syntax error is returned, not
// swallowed: an unparseable workflow cannot be reasoned about, and reporting
// "0 jobs" for it would read like an empty workflow rather than a broken one.
func LoadWorkflow(path string) (*Workflow, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- caller-supplied repo path
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	var wf Workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return &wf, nil
}

// ListWorkflows returns the workflow files in dir, sorted by name.
func ListWorkflows(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}
