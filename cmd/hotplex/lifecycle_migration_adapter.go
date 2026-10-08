package main

import (
	"context"
	"errors"

	"github.com/hrygo/hotplex/internal/admin"
	"github.com/hrygo/hotplex/internal/lifecycle"
)

type lifecycleMigrationAdapter struct {
	service *lifecycle.MigrationService
}

func (a *lifecycleMigrationAdapter) Preview(ctx context.Context) (*admin.LifecycleMigrationPreview, error) {
	preview, err := a.service.Preview(ctx)
	if err != nil {
		return nil, err
	}
	return &admin.LifecycleMigrationPreview{
		PlanID:         preview.PlanID,
		CreatedAt:      preview.CreatedAt,
		ExpiresAt:      preview.ExpiresAt,
		PolicyRevision: preview.PolicyRevision,
		BatchSize:      preview.BatchSize,
		Sessions:       toAdminLifecycleSummary(preview.Sessions),
		Events:         toAdminLifecycleSummary(preview.Events),
		Turns:          toAdminLifecycleSummary(preview.Turns),
		Blocked: admin.LifecycleMigrationBlocked{
			LegacySessionsMissingInputClock: preview.Blocked.LegacySessionsMissingInputClock,
			OrphanedEvents:                  preview.Blocked.OrphanedEvents,
			OrphanedTurns:                   preview.Blocked.OrphanedTurns,
			ContentOnSessionsMissingClock:   preview.Blocked.ContentOnSessionsMissingInputClock,
			UnknownExecutionRecords:         preview.Blocked.UnknownExecutionRecords,
			ActiveExecutionRecords:          preview.Blocked.ActiveExecutionRecords,
		},
	}, nil
}

func (a *lifecycleMigrationAdapter) Apply(
	ctx context.Context,
	planID string,
	confirmPlanID string,
) (*admin.LifecycleMigrationApplyResult, error) {
	result, err := a.service.Apply(ctx, planID, confirmPlanID)
	switch {
	case errors.Is(err, lifecycle.ErrConfirmationRequired):
		return nil, admin.ErrLifecycleMigrationConfirmationRequired
	case errors.Is(err, lifecycle.ErrPreviewExpired):
		return nil, admin.ErrLifecycleMigrationPlanExpired
	case errors.Is(err, lifecycle.ErrStalePreview):
		return nil, admin.ErrLifecycleMigrationPlanStale
	case err != nil:
		return nil, err
	default:
		return &admin.LifecycleMigrationApplyResult{
			Sessions: result.Sessions,
			Events:   result.Events,
			Turns:    result.Turns,
		}, nil
	}
}

func toAdminLifecycleSummary(summary lifecycle.MigrationSummary) admin.LifecycleMigrationSummary {
	return admin.LifecycleMigrationSummary{
		Count:          summary.Count,
		Bytes:          summary.Bytes,
		OldestDeadline: summary.OldestDeadline,
	}
}
