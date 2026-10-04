package checkers

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hrygo/hotplex/internal/cli"
	"github.com/hrygo/hotplex/internal/dbutil"
	"github.com/hrygo/hotplex/internal/sqlutil"
)

// fencedExecutionsChecker reports executions whose runtime ended ambiguous
// and raised a fence (#877). Fenced executions block fresh input for their
// session until an operator resolves or abandons them, so any fence is at
// least a Warn.
//
// Read-only contract: the SQLite database is opened with PRAGMA query_only
// (same pattern as `hotplex audit verify`) and never written; PostgreSQL
// backends cannot be inspected locally and are pointed at the Admin API.
// There is deliberately no FixFunc — fence decisions are operator actions
// with audit consequences and must go through the Admin API / CLI.
type fencedExecutionsChecker struct {
	defaultDBPath string
}

func (c fencedExecutionsChecker) Name() string     { return "runtime.fenced_executions" }
func (c fencedExecutionsChecker) Category() string { return "runtime" }

func (c fencedExecutionsChecker) Check(ctx context.Context) cli.Diagnostic {
	cfg, err := loadConfig()
	if err != nil {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusWarn,
			Message:  "Cannot load config for fence inspection",
			Detail:   err.Error(),
			FixHint:  "Fix the config first (hotplex config validate), then re-run doctor",
		}
	}

	dbPath := c.defaultDBPath
	driver := ""
	if cfg != nil {
		driver = cfg.DB.Driver
		if cfg.DB.SQLite.Path != "" {
			dbPath = cfg.DB.SQLite.Path
		}
	}
	if dbutil.ParseDialect(driver) == dbutil.DialectPostgres {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusPass,
			Message:  "PostgreSQL backend: fences are not inspectable locally",
			FixHint:  "Use `hotplex runtime fences list` (Admin API) to inspect fenced executions",
		}
	}

	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return cli.Diagnostic{
				Name:     c.Name(),
				Category: c.Category(),
				Status:   cli.StatusPass,
				Message:  "No execution database yet: " + dbPath,
			}
		}
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusWarn,
			Message:  "Cannot stat execution database: " + dbPath,
			Detail:   err.Error(),
		}
	}

	count, earliestMs, err := countFencedExecutions(ctx, dbPath)
	if err != nil {
		msg := "Cannot inspect execution database"
		if strings.Contains(err.Error(), "no such table") {
			return cli.Diagnostic{
				Name:     c.Name(),
				Category: c.Category(),
				Status:   cli.StatusPass,
				Message:  "Execution tables not initialized yet (gateway never started)",
			}
		}
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusWarn,
			Message:  msg,
			Detail:   err.Error(),
		}
	}

	if count == 0 {
		return cli.Diagnostic{
			Name:     c.Name(),
			Category: c.Category(),
			Status:   cli.StatusPass,
			Message:  "No fenced executions",
		}
	}

	detail := fmt.Sprintf("%d fenced execution(s) blocking fresh input", count)
	if earliestMs > 0 {
		detail += "; earliest fence at " + time.UnixMilli(earliestMs).Format(time.RFC3339)
	}
	return cli.Diagnostic{
		Name:     c.Name(),
		Category: c.Category(),
		Status:   cli.StatusWarn,
		Message:  fmt.Sprintf("%d fenced execution(s) found", count),
		Detail:   detail,
		FixHint:  "Inspect with `hotplex runtime fences list`, then `hotplex runtime fences resolve|abandon <execution-id> --fence-version <n> --reason \"...\" --confirm`",
	}
}

// countFencedExecutions opens the SQLite database strictly read-only and
// counts fenced rows plus the earliest fence timestamp.
func countFencedExecutions(ctx context.Context, dbPath string) (int64, int64, error) {
	db, err := sql.Open(sqlutil.DriverName, dbPath)
	if err != nil {
		return 0, 0, fmt.Errorf("open sqlite db: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return 0, 0, fmt.Errorf("set sqlite busy_timeout: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return 0, 0, fmt.Errorf("set sqlite query_only: %w", err)
	}

	var count int64
	var earliest sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(fence_created_at) FROM execution_inputs WHERE fence_reason <> ''`).
		Scan(&count, &earliest)
	if err != nil {
		return 0, 0, err
	}
	return count, earliest.Int64, nil
}

func init() {
	cli.DefaultRegistry.Register(fencedExecutionsChecker{
		defaultDBPath: filepath.Join(hotplexHome(), "data", "hotplex.db"),
	})
}
