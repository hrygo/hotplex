package checkers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/hrygo/hotplex/internal/cli"
	"github.com/hrygo/hotplex/internal/config"
	"github.com/hrygo/hotplex/internal/skills/builtin"
	"github.com/hrygo/hotplex/internal/skills/reconcile"
)

type builtinSkillsChecker struct {
	statusFn func(context.Context) (reconcile.Report, error)
}

// NewBuiltinSkillsChecker creates the read-only built-in skills diagnostic.
// The callback is injected so callers and tests can choose the authoritative
// status source without giving the doctor checker a write capability.
func NewBuiltinSkillsChecker(statusFn func(context.Context) (reconcile.Report, error)) cli.Checker {
	return builtinSkillsChecker{statusFn: statusFn}
}

func (c builtinSkillsChecker) Name() string     { return "skills.builtin" }
func (c builtinSkillsChecker) Category() string { return "skills" }

func (c builtinSkillsChecker) Check(ctx context.Context) cli.Diagnostic {
	base := cli.Diagnostic{Name: c.Name(), Category: c.Category(), FixHint: "Run hotplex skills status for details; use hotplex skills sync explicitly to reconcile projections."}
	if c.statusFn == nil {
		base.Status = cli.StatusFail
		base.Message = "status_unavailable"
		return base
	}

	report, err := c.statusFn(ctx)
	if err != nil {
		base.Status = statusForError(err)
		base.Message = reasonForError(err)
		return base
	}

	reasons := make(map[string]struct{})
	unsafe := false
	drift := false
	for _, item := range report.Items {
		reason := strings.TrimSpace(item.ReasonCode)
		if reason != "" {
			reasons[reason] = struct{}{}
		}
		if reason == reconcile.ReasonUnsupportedWorker || reason == reconcile.ReasonInvalidReceipt {
			unsafe = true
			continue
		}
		switch item.Outcome {
		case reconcile.OutcomeConflict, reconcile.OutcomeFailed:
			unsafe = true
		case reconcile.OutcomeDrift:
			drift = true
		}
	}

	sortedReasons := make([]string, 0, len(reasons))
	for reason := range reasons {
		sortedReasons = append(sortedReasons, reason)
	}
	sort.Strings(sortedReasons)
	reasonText := strings.Join(sortedReasons, ",")
	if reasonText == "" {
		reasonText = "ok"
	}

	switch {
	case unsafe:
		base.Status = cli.StatusFail
		base.Message = reasonText
	case drift:
		base.Status = cli.StatusWarn
		base.Message = reasonText
	default:
		base.Status = cli.StatusPass
		base.Message = reasonText
	}
	base.Detail = fmt.Sprintf("profile=%s items=%d", report.Profile, len(report.Items))
	return base
}

func statusForError(err error) cli.Status {
	if errors.Is(err, reconcile.ErrNoWorkerTargets) {
		return cli.StatusWarn
	}
	return cli.StatusFail
}

func reasonForError(err error) string {
	switch {
	case errors.Is(err, reconcile.ErrNoWorkerTargets):
		return reconcile.ErrNoWorkerTargets.Error()
	case errors.Is(err, reconcile.ErrUnknownWorker):
		return reconcile.ErrUnknownWorker.Error()
	case errors.Is(err, reconcile.ErrUnknownProfile):
		return reconcile.ErrUnknownProfile.Error()
	case errors.Is(err, reconcile.ErrRootOutsideHome):
		return reconcile.ErrRootOutsideHome.Error()
	case errors.Is(err, reconcile.ErrInventoryOutsideHotplexHome):
		return reconcile.ErrInventoryOutsideHotplexHome.Error()
	case errors.Is(err, reconcile.ErrInvalidReceipt):
		return reconcile.ErrInvalidReceipt.Error()
	case errors.Is(err, reconcile.ErrReportActionRequired):
		return reconcile.ErrReportActionRequired.Error()
	default:
		return "status_unavailable"
	}
}

// skillsStatusProvider builds the reconcile status for the builtin-skills
// checker from explicitly injected environment roots (#978). Production
// resolves userHome/hotplexHome from the process; tests inject t.TempDir()
// trees to exercise the real construction path instead of mocking statusFn.
type skillsStatusProvider struct {
	// loadConfig resolves the active config; nil falls back to loadConfig.
	loadConfig func() (*config.Config, error)
	// userHome and hotplexHome override os.UserHomeDir/config.HotplexHome.
	userHome    string
	hotplexHome string
	// newRegistry and newRunner construct the reconcile pipeline; nil keeps
	// the production builtin.NewRegistry/reconcile.New behaviour.
	newRegistry func() (*builtin.Registry, error)
	newRunner   func(registry *builtin.Registry, paths reconcile.Paths) (skillsStatusRunner, error)
}

// skillsStatusRunner is the subset of reconcile behaviour the checker needs.
type skillsStatusRunner interface {
	Status(ctx context.Context, opts reconcile.Options) (reconcile.Report, error)
}

// Status is deliberately read-only. Synchronization remains available only via
// explicit skills sync/lifecycle flags.
func (p *skillsStatusProvider) Status(ctx context.Context) (reconcile.Report, error) {
	load := p.loadConfig
	if load == nil {
		load = loadConfig
	}
	cfg, err := load()
	if err != nil {
		return reconcile.Report{}, err
	}
	if cfg == nil {
		return reconcile.Report{}, reconcile.ErrNoWorkerTargets
	}
	workerTypes, err := parseConfiguredWorkerTypes(cfg.EnabledWorkerTypes())
	if err != nil {
		return reconcile.Report{}, err
	}
	userHome := p.userHome
	if userHome == "" {
		userHome, err = os.UserHomeDir()
		if err != nil {
			return reconcile.Report{}, err
		}
	}
	hotplexHome := p.hotplexHome
	if hotplexHome == "" {
		hotplexHome = config.HotplexHome()
	}
	newRegistry := p.newRegistry
	if newRegistry == nil {
		newRegistry = builtin.NewRegistry
	}
	registry, err := newRegistry()
	if err != nil {
		return reconcile.Report{}, err
	}
	paths := builtinSkillsPaths(userHome, hotplexHome)
	var runner skillsStatusRunner
	if p.newRunner != nil {
		runner, err = p.newRunner(registry, paths)
	} else {
		runner, err = reconcile.New(registry, paths, reconcile.NewOSFileSystem())
	}
	if err != nil {
		return reconcile.Report{}, err
	}
	return runner.Status(ctx, reconcile.Options{Profile: builtin.ProfileRuntime, WorkerTypes: workerTypes})
}

func builtinSkillsPaths(userHome, hotplexHome string) reconcile.Paths {
	return reconcile.DefaultPaths(userHome, hotplexHome)
}

func parseConfiguredWorkerTypes(values []string) ([]reconcile.WorkerType, error) {
	workers := make([]reconcile.WorkerType, 0, len(values))
	for _, value := range values {
		worker, err := reconcile.ParseWorkerType(value)
		if err != nil {
			return nil, err
		}
		workers = append(workers, worker)
	}
	return workers, nil
}

func init() {
	cli.DefaultRegistry.Register(builtinSkillsChecker{statusFn: (&skillsStatusProvider{}).Status})
}
