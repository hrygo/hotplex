package cicheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// workflowsDir is the repository's workflow directory, relative to this
// package.
const workflowsDir = "../../.github/workflows"

// TestWorkflowsAreValidYAML is the guard that should have caught the release
// workflow before it was pushed: GitHub rejects an unparseable workflow, and
// nothing else in the local gate looks at these files.
func TestWorkflowsAreValidYAML(t *testing.T) {
	t.Parallel()

	paths, err := ListWorkflows(workflowsDir)
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no workflow files found; the path is wrong, not the repo empty")

	for _, path := range paths {
		wf, err := LoadWorkflow(path)
		require.NoErrorf(t, err, "%s must parse as YAML", filepath.Base(path))
		require.NotEmptyf(t, wf.Jobs, "%s declares no jobs", filepath.Base(path))
	}
}

// TestReleaseWorkflowHasExactlyOnePublisher pins the property the whole
// release gate rests on. It is asserted here as well as in the Python checker
// because this test reads the file with a real YAML parser: if the line-based
// checker ever stops seeing a job again, this one still sees it.
func TestReleaseWorkflowHasExactlyOnePublisher(t *testing.T) {
	t.Parallel()

	wf, err := LoadWorkflow(filepath.Join(workflowsDir, "release.yml"))
	require.NoError(t, err)

	publishers := 0
	for _, job := range wf.Jobs {
		publishers += publishersInJob(job)
	}
	require.Equal(t, 1, publishers,
		"expected exactly one publishing job; two publishers can ship while the gate is failing")
}

// publishersInJob counts steps that create a GitHub release.
func publishersInJob(job map[string]any) int {
	steps, ok := job["steps"].([]any)
	if !ok {
		return 0
	}
	count := 0
	for _, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		uses, _ := step["uses"].(string)
		if strings.Contains(uses, "action-gh-release") {
			count++
		}
	}
	return count
}

// TestLoadWorkflowRejectsBrokenYAML proves the check actually fails on the
// failure it exists for. A guard that cannot fail is not a guard.
func TestLoadWorkflowRejectsBrokenYAML(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "broken.yml")
	// The exact shape that shipped: a column-0 key inside a run block ends the
	// block scalar, and the document stops being YAML.
	require.NoError(t, os.WriteFile(path, []byte(
		"name: broken\njobs:\n  smoke:\n    steps:\n      - run: |\n          @\"\n"+
			"gateway:\n  addr: \"127.0.0.1\"\n\"@ | Set-Content x\n",
	), 0o600))

	_, err := LoadWorkflow(path)
	require.Error(t, err, "an unparseable workflow must be an error, not an empty one")
}
