package checkers

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hrygo/hotplex/internal/cli"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/skills/builtin"
	"github.com/hrygo/hotplex/internal/skills/reconcile"
)

func TestBuiltinSkillsCheckerIsReadOnlyAndHasNoFix(t *testing.T) {
	t.Parallel()
	checker := NewBuiltinSkillsChecker(func(context.Context) (reconcile.Report, error) {
		return reconcile.Report{Items: []reconcile.Item{{
			Target:        "/private/native-root",
			BackupPath:    "/private/backup",
			Action:        reconcile.ActionUpdate,
			Outcome:       reconcile.OutcomeDrift,
			ReasonCode:    reconcile.ReasonDrift,
			WorkerAliases: []reconcile.WorkerType{reconcile.WorkerClaude},
		}}}, nil
	})

	diagnostic := checker.Check(context.Background())
	require.Equal(t, "skills.builtin", diagnostic.Name)
	require.Equal(t, "skills", diagnostic.Category)
	require.Equal(t, cli.StatusWarn, diagnostic.Status)
	require.Nil(t, diagnostic.FixFunc)
	require.Contains(t, diagnostic.Message, reconcile.ReasonDrift)
	require.NotContains(t, diagnostic.Message, "/private/native-root")
	require.NotContains(t, diagnostic.Detail, "/private/backup")
}

func TestBuiltinSkillsCheckerMapsUnsafeItemsToFailureWithoutLeakingPaths(t *testing.T) {
	t.Parallel()
	checker := NewBuiltinSkillsChecker(func(context.Context) (reconcile.Report, error) {
		return reconcile.Report{Items: []reconcile.Item{
			{Target: "/tmp/collision", Outcome: reconcile.OutcomeConflict, ReasonCode: reconcile.ReasonCollision},
			{Target: "/tmp/failed", Outcome: reconcile.OutcomeDrift, ReasonCode: reconcile.ReasonInvalidReceipt},
		}}, nil
	})

	diagnostic := checker.Check(context.Background())
	require.Equal(t, cli.StatusFail, diagnostic.Status)
	require.Contains(t, diagnostic.Message, reconcile.ReasonCollision)
	require.Contains(t, diagnostic.Message, reconcile.ReasonInvalidReceipt)
	require.NotContains(t, diagnostic.Message, "/tmp/collision")
	require.NotContains(t, diagnostic.Detail, "/tmp/failed")
}

func TestBuiltinSkillsCheckerMapsStatusErrorToStableFailure(t *testing.T) {
	t.Parallel()
	checker := NewBuiltinSkillsChecker(func(context.Context) (reconcile.Report, error) {
		return reconcile.Report{}, errors.New("sensitive path /Users/private/.hotplex/state")
	})

	diagnostic := checker.Check(context.Background())
	require.Equal(t, cli.StatusFail, diagnostic.Status)
	require.Contains(t, diagnostic.Message, "status_unavailable")
	require.NotContains(t, diagnostic.Message, "/Users/private")
	require.NotContains(t, diagnostic.Detail, "/Users/private")
}

func TestBuiltinSkillsPathsMatchesReconcileDefaultContract(t *testing.T) {
	t.Parallel()
	const (
		userHome    = "/home/user"
		hotplexHome = "/home/user/.hotplex"
	)
	paths := builtinSkillsPaths(userHome, hotplexHome)
	require.Equal(t, reconcile.DefaultPaths(userHome, hotplexHome), paths)
	// The .agents root is canonical; Claude receives per-package links under
	// .claude. Pointing Claude's native root at .claude/skills made normalizePaths
	// reject the layout and doctor fail with ErrRootOutsideHome after a clean sync.
	require.Equal(t, filepath.Join(userHome, ".agents", "skills"), paths.NativeRoots[reconcile.WorkerClaude])
	require.Equal(t, filepath.Join(userHome, ".claude", "skills"), paths.AliasRoots[reconcile.WorkerClaude])
}

func TestBuiltinSkillsCheckerIsRegisteredUnderSkills(t *testing.T) {
	checkers := cli.DefaultRegistry.ByCategory("skills")
	require.NotEmpty(t, checkers)
	found := false
	for _, checker := range checkers {
		if checker.Name() != "skills.builtin" {
			continue
		}
		found = true
		require.Equal(t, "skills", checker.Category())
	}
	require.True(t, found, "skills.builtin must be registered")
}

// #978: 真实构造逻辑经 provider 注入 TempDir 可单测，不再只能 mock statusFn。
func TestSkillsStatusProviderBuildsRealPipeline(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	hotplexHome := t.TempDir()
	var gotPaths reconcile.Paths
	provider := &skillsStatusProvider{
		loadConfig:  func() (*config.Config, error) { return &config.Config{}, nil },
		userHome:    home,
		hotplexHome: hotplexHome,
		newRegistry: func() (*builtin.Registry, error) { return &builtin.Registry{}, nil },
		newRunner: func(registry *builtin.Registry, paths reconcile.Paths) (skillsStatusRunner, error) {
			gotPaths = paths
			return stubSkillsRunner{}, nil
		},
	}
	report, err := provider.Status(context.Background())
	require.NoError(t, err)
	require.Empty(t, report.Items)
	require.Equal(t, builtinSkillsPaths(home, hotplexHome), gotPaths)
}

// #978: 无 worker 目标时沿用 ErrNoWorkerTargets，不触碰文件系统。
func TestSkillsStatusProviderRejectsEmptyWorkers(t *testing.T) {
	t.Parallel()
	provider := &skillsStatusProvider{
		loadConfig:  func() (*config.Config, error) { return &config.Config{}, nil },
		userHome:    t.TempDir(),
		hotplexHome: t.TempDir(),
		newRegistry: func() (*builtin.Registry, error) {
			return &builtin.Registry{}, nil
		},
	}
	_, err := provider.Status(context.Background())
	require.ErrorIs(t, err, reconcile.ErrNoWorkerTargets)
}

type stubSkillsRunner struct{}

func (stubSkillsRunner) Status(context.Context, reconcile.Options) (reconcile.Report, error) {
	return reconcile.Report{}, nil
}
