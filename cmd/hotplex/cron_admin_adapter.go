package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hrygo/hotplex/internal/cron"
	"github.com/hrygo/hotplex/internal/eventstore"
	"github.com/hrygo/hotplex/internal/gateway"
	"github.com/hrygo/hotplex/internal/session"
)

// cronAdminAdapter bridges cron.Scheduler to admin.CronSchedulerProvider.
type cronAdminAdapter struct {
	scheduler  *cron.Scheduler
	turnsStore eventstore.TurnQuerier
	sessionMgr *session.Manager
}

// Fields that must not be overwritten via admin API UpdateJob.
var protectedFields = map[string]struct{}{
	"id":            {},
	"created_at_ms": {},
	"updated_at_ms": {},
	"state":         {},
}

func (a *cronAdminAdapter) CreateJob(ctx context.Context, raw any) error {
	data, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal job: %w", err)
	}
	var job cron.CronJob
	if err := json.Unmarshal(data, &job); err != nil {
		return fmt.Errorf("unmarshal job: %w", err)
	}

	if job.OwnerID == "" {
		job.OwnerID = "admin"
	}
	if job.BotID == "" {
		job.BotID = "system"
	}
	if job.Payload.Kind == "" {
		job.Payload.Kind = cron.PayloadIsolatedSession
	}
	if job.Schedule.Kind != cron.ScheduleAt {
		if job.MaxRuns <= 0 {
			job.MaxRuns = 1000
		}
		if job.ExpiresAt == "" {
			job.ExpiresAt = time.Now().UTC().AddDate(1, 0, 0).Format(time.RFC3339)
		}
	}

	return a.scheduler.CreateJob(ctx, &job)
}

func (a *cronAdminAdapter) UpdateJob(ctx context.Context, id string, updates map[string]any) error {
	job, err := a.scheduler.GetJob(ctx, id)
	if err != nil {
		return err
	}

	updated, err := mergeJobUpdates(job, updates)
	if err != nil {
		return err
	}
	return a.scheduler.UpdateJob(ctx, updated)
}

// mergeJobUpdates overlays user-supplied updates onto a CronJob via JSON merge.
// Nested objects (schedule, payload) are replaced entirely, not deep-merged.
// Protected fields (id, created_at_ms, updated_at_ms, state) are silently dropped.
func mergeJobUpdates(job *cron.CronJob, updates map[string]any) (*cron.CronJob, error) {
	base, err := json.Marshal(job)
	if err != nil {
		return nil, fmt.Errorf("marshal job: %w", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, fmt.Errorf("unmarshal job: %w", err)
	}
	for k, v := range updates {
		if _, ok := protectedFields[k]; ok {
			continue
		}
		merged[k] = v
	}
	data, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("re-marshal: %w", err)
	}
	var updated cron.CronJob
	if err := json.Unmarshal(data, &updated); err != nil {
		return nil, fmt.Errorf("unmarshal updated: %w", err)
	}
	return &updated, nil
}

func (a *cronAdminAdapter) DeleteJob(ctx context.Context, id string) error {
	return a.scheduler.DeleteJob(ctx, id)
}

func (a *cronAdminAdapter) GetJob(ctx context.Context, id string) (any, error) {
	return a.scheduler.GetJob(ctx, id)
}

func (a *cronAdminAdapter) ListJobs(ctx context.Context) (any, error) {
	return a.scheduler.ListJobs(ctx)
}

func (a *cronAdminAdapter) TriggerJob(ctx context.Context, id string) error {
	job, err := a.scheduler.GetJob(ctx, id)
	if err != nil {
		return err
	}
	// An operator pressing "run now" is a per-request firing, never a redelivery
	// of someone else's event: it carries no VerifiedEventID and so gets its own
	// nonce, which is what keeps two operator runs distinct.
	return a.scheduler.TriggerJob(ctx, job, cron.TriggerRequest{})
}

func (a *cronAdminAdapter) RunHistory(ctx context.Context, id string) (any, error) {
	job, err := a.scheduler.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}

	if a.turnsStore == nil {
		return nil, fmt.Errorf("eventstore not available")
	}

	var allTurns []map[string]any

	// 1. Query direct job.SessionKey()
	if stats, err := a.turnsStore.QueryTurnStats(ctx, job.SessionKey()); err == nil && stats != nil {
		for _, item := range stats.Turns {
			allTurns = append(allTurns, map[string]any{
				"id":          fmt.Sprintf("%s-%d", stats.SessionID, item.TurnNum),
				"session_id":  stats.SessionID,
				"turn_index":  item.TurnNum,
				"status":      map[bool]string{true: "success", false: "failed"}[item.Success],
				"duration_ms": item.DurationMs,
				"tokens_used": item.TokensIn + item.TokensOut,
				"created_at":  time.UnixMilli(item.CreatedAt).Format(time.RFC3339),
			})
		}
	}

	// 2. Query sessions derived for this cron job
	if a.sessionMgr != nil {
		sessions, err := a.sessionMgr.List(ctx, "", "", "", 200, 0)
		if err == nil {
			for _, s := range sessions {
				if s.ID == job.SessionKey() {
					continue
				}
				if s.PlatformKey != nil && s.PlatformKey["cron_job_id"] == id {
					if stats, err := a.turnsStore.QueryTurnStats(ctx, s.ID); err == nil && stats != nil {
						for _, item := range stats.Turns {
							allTurns = append(allTurns, map[string]any{
								"id":          fmt.Sprintf("%s-%d", s.ID, item.TurnNum),
								"session_id":  s.ID,
								"turn_index":  item.TurnNum,
								"status":      map[bool]string{true: "success", false: "failed"}[item.Success],
								"duration_ms": item.DurationMs,
								"tokens_used": item.TokensIn + item.TokensOut,
								"created_at":  time.UnixMilli(item.CreatedAt).Format(time.RFC3339),
							})
						}
					}
				}
			}
		}
	}

	if allTurns == nil {
		allTurns = []map[string]any{}
	}
	return allTurns, nil
}

// cronAttachedRouter implements cron.AttachedSessionRouter using Bridge + SessionManager.
type cronAttachedRouter struct {
	bridge *gateway.Bridge
	sm     *session.Manager
}

func (r *cronAttachedRouter) GetSessionInfo(ctx context.Context, id string) (*session.SessionInfo, error) {
	return r.sm.Get(ctx, id)
}

func (r *cronAttachedRouter) InjectInput(ctx context.Context, sessionID, prompt string, metadata map[string]any) error {
	w := r.sm.GetWorker(sessionID)
	if w == nil {
		return fmt.Errorf("no worker for session %s", sessionID)
	}
	return w.Input(ctx, prompt, metadata)
}

func (r *cronAttachedRouter) ResumeAndInput(ctx context.Context, sessionID, workDir, prompt string, metadata map[string]any) error {
	if err := r.bridge.ResumeSession(ctx, sessionID, workDir); err != nil {
		return fmt.Errorf("resume session: %w", err)
	}
	w := r.sm.GetWorker(sessionID)
	if w == nil {
		return fmt.Errorf("no worker after resume for session %s", sessionID)
	}
	return w.Input(ctx, prompt, metadata)
}
