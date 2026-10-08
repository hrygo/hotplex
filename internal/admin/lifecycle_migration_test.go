package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type lifecycleMigrationTestProvider struct {
	preview        *LifecycleMigrationPreview
	previewErr     error
	applyResult    *LifecycleMigrationApplyResult
	applyErr       error
	applyPlanID    string
	applyConfirm   string
	applyCallCount int
}

func (p *lifecycleMigrationTestProvider) Preview(context.Context) (*LifecycleMigrationPreview, error) {
	return p.preview, p.previewErr
}

func (p *lifecycleMigrationTestProvider) Apply(_ context.Context, planID, confirmPlanID string) (*LifecycleMigrationApplyResult, error) {
	p.applyCallCount++
	p.applyPlanID = planID
	p.applyConfirm = confirmPlanID
	return p.applyResult, p.applyErr
}

func TestLifecycleMigrationPreviewRequiresAdminReadAndReturnsAggregatePlan(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, time.October, 8, 12, 0, 0, 0, time.UTC)
	provider := &lifecycleMigrationTestProvider{
		preview: &LifecycleMigrationPreview{
			PlanID:         "plan-1",
			CreatedAt:      createdAt,
			ExpiresAt:      createdAt.Add(30 * time.Minute),
			PolicyRevision: "v2-revision",
			BatchSize:      100,
			Sessions:       LifecycleMigrationSummary{Count: 10},
			Events:         LifecycleMigrationSummary{Count: 20, Bytes: 4096},
		},
	}
	api := New(Deps{})
	api.SetLifecycleMigration(provider)

	denied := httptest.NewRecorder()
	api.HandleLifecycleMigrationPreview(denied, httptest.NewRequest(http.MethodPost, "/admin/lifecycle/migration/preview", nil))
	require.Equal(t, http.StatusForbidden, denied.Code)

	request := withScope(httptest.NewRequest(http.MethodPost, "/admin/lifecycle/migration/preview", nil), ScopeAdminRead)
	response := httptest.NewRecorder()
	api.HandleLifecycleMigrationPreview(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"plan_id":"plan-1"`)
	require.Contains(t, response.Body.String(), `"bytes":4096`)
}

func TestLifecycleMigrationApplyRequiresWriteAndExactConfirmation(t *testing.T) {
	t.Parallel()

	provider := &lifecycleMigrationTestProvider{
		applyResult: &LifecycleMigrationApplyResult{Sessions: 1, Events: 2, Turns: 3},
	}
	api := New(Deps{})
	api.SetLifecycleMigration(provider)

	readOnly := withScope(httptest.NewRequest(http.MethodPost, "/admin/lifecycle/migration/apply",
		strings.NewReader(`{"plan_id":"plan-1","confirm_plan_id":"plan-1"}`)), ScopeAdminRead)
	readOnlyResponse := httptest.NewRecorder()
	api.HandleLifecycleMigrationApply(readOnlyResponse, readOnly)
	require.Equal(t, http.StatusForbidden, readOnlyResponse.Code)
	require.Zero(t, provider.applyCallCount)

	mismatch := withScope(httptest.NewRequest(http.MethodPost, "/admin/lifecycle/migration/apply",
		strings.NewReader(`{"plan_id":"plan-1","confirm_plan_id":"other"}`)), ScopeAdminWrite)
	mismatchResponse := httptest.NewRecorder()
	api.HandleLifecycleMigrationApply(mismatchResponse, mismatch)
	require.Equal(t, http.StatusBadRequest, mismatchResponse.Code)
	require.Zero(t, provider.applyCallCount)

	confirmed := withScope(httptest.NewRequest(http.MethodPost, "/admin/lifecycle/migration/apply",
		strings.NewReader(`{"plan_id":"plan-1","confirm_plan_id":"plan-1"}`)), ScopeAdminWrite)
	confirmedResponse := httptest.NewRecorder()
	api.HandleLifecycleMigrationApply(confirmedResponse, confirmed)
	require.Equal(t, http.StatusOK, confirmedResponse.Code)
	require.Equal(t, "plan-1", provider.applyPlanID)
	require.Equal(t, "plan-1", provider.applyConfirm)
	require.Contains(t, confirmedResponse.Body.String(), `"events":2`)
}

func TestLifecycleMigrationApplyMapsExpiredAndRejectsMalformedRequests(t *testing.T) {
	t.Parallel()

	provider := &lifecycleMigrationTestProvider{applyErr: ErrLifecycleMigrationPlanExpired}
	api := New(Deps{})
	api.SetLifecycleMigration(provider)

	malformed := withScope(httptest.NewRequest(http.MethodPost, "/admin/lifecycle/migration/apply",
		strings.NewReader(`{"plan_id":"plan-1","confirm_plan_id":"plan-1","unexpected":true}`)), ScopeAdminWrite)
	malformedResponse := httptest.NewRecorder()
	api.HandleLifecycleMigrationApply(malformedResponse, malformed)
	require.Equal(t, http.StatusBadRequest, malformedResponse.Code)
	require.Zero(t, provider.applyCallCount)

	expired := withScope(httptest.NewRequest(http.MethodPost, "/admin/lifecycle/migration/apply",
		strings.NewReader(`{"plan_id":"plan-1","confirm_plan_id":"plan-1"}`)), ScopeAdminWrite)
	expiredResponse := httptest.NewRecorder()
	api.HandleLifecycleMigrationApply(expiredResponse, expired)
	require.Equal(t, http.StatusConflict, expiredResponse.Code)
	require.Contains(t, expiredResponse.Body.String(), "PREVIEW_EXPIRED")
}

func TestLifecycleMigrationPreviewUnavailableAndFailuresAreSafe(t *testing.T) {
	t.Parallel()

	request := withScope(httptest.NewRequest(http.MethodPost, "/admin/lifecycle/migration/preview", nil), ScopeAdminRead)
	unavailable := httptest.NewRecorder()
	New(Deps{}).HandleLifecycleMigrationPreview(unavailable, request)
	require.Equal(t, http.StatusServiceUnavailable, unavailable.Code)

	api := New(Deps{})
	api.SetLifecycleMigration(&lifecycleMigrationTestProvider{previewErr: errors.New("database details")})
	failed := httptest.NewRecorder()
	api.HandleLifecycleMigrationPreview(failed, request)
	require.Equal(t, http.StatusInternalServerError, failed.Code)
	require.NotContains(t, failed.Body.String(), "database details")
}
