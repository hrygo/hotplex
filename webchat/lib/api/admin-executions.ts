/**
 * Execution console API client.
 *
 * Read paths: a bounded page of executions, and one run's bounded, redacted
 * timeline. Write paths are only the three operator actions the server itself
 * offers — the console renders the action list, it never invents one.
 *
 * These endpoints live on the admin mux (`/admin/*`), which accepts either the
 * standalone Bearer token or the embedded cookie session, so adminFetch's
 * dual-channel behaviour applies unchanged.
 */

import { adminFetch } from './admin-client';
import type {
  AdminAvailableAction,
  AdminExecutionCursor,
  AdminExecutionListResponse,
  AdminExecutionTimeline,
} from '@/lib/types/admin';

export interface ExecutionListFilters {
  sessionId?: string;
  deliveryStatus?: string;
  runtimeStatus?: string;
  sinceMs?: number;
  untilMs?: number;
  limit?: number;
}

export function listExecutions(
  filters: ExecutionListFilters = {},
  cursor?: AdminExecutionCursor | null,
): Promise<AdminExecutionListResponse> {
  const params = new URLSearchParams();
  if (filters.sessionId) params.set('session_id', filters.sessionId);
  if (filters.deliveryStatus) params.set('delivery_status', filters.deliveryStatus);
  if (filters.runtimeStatus) params.set('runtime_status', filters.runtimeStatus);
  if (filters.sinceMs) params.set('since_ms', String(filters.sinceMs));
  if (filters.untilMs) params.set('until_ms', String(filters.untilMs));
  if (filters.limit) params.set('limit', String(filters.limit));
  // The cursor is only meaningful as a pair: the server rejects
  // before_created_at without before_execution_id rather than guessing.
  if (cursor) {
    params.set('before_created_at', String(cursor.before_created_at));
    params.set('before_execution_id', cursor.before_execution_id);
  }
  const query = params.toString();
  return adminFetch<AdminExecutionListResponse>(
    `/admin/executions${query ? `?${query}` : ''}`,
  );
}

export function getExecutionTimeline(
  executionId: string,
  window?: { sinceMs?: number; untilMs?: number },
): Promise<AdminExecutionTimeline> {
  const params = new URLSearchParams();
  if (window?.sinceMs) params.set('since_ms', String(window.sinceMs));
  if (window?.untilMs) params.set('until_ms', String(window.untilMs));
  const query = params.toString();
  return adminFetch<AdminExecutionTimeline>(
    `/admin/executions/${encodeURIComponent(executionId)}/timeline${query ? `?${query}` : ''}`,
  );
}

export type FenceDecision = 'resolve' | 'abandon';

export function applyFenceAction(
  executionId: string,
  body: {
    decision: FenceDecision;
    expectedFenceVersion: number;
    reason: string;
    evidenceRef?: string;
  },
): Promise<unknown> {
  return adminFetch(`/admin/executions/${encodeURIComponent(executionId)}/fence-action`, {
    method: 'POST',
    body: JSON.stringify({
      decision: body.decision,
      expected_fence_version: body.expectedFenceVersion,
      reason: body.reason,
      evidence_ref: body.evidenceRef ?? '',
    }),
  });
}

export function cancelQueuedInput(
  executionId: string,
  body: { reason: string; evidenceRef?: string },
): Promise<unknown> {
  return adminFetch(`/admin/executions/${encodeURIComponent(executionId)}/queue-cancel`, {
    method: 'POST',
    body: JSON.stringify({
      reason: body.reason,
      evidence_ref: body.evidenceRef ?? '',
    }),
  });
}

export type EffectDecision = 'abandon' | 'mark_delivered' | 'requeue';

export function applyEffectAction(
  effectId: string,
  body: { decision: EffectDecision; reason: string; evidenceRef?: string },
): Promise<unknown> {
  return adminFetch(`/admin/effects/${encodeURIComponent(effectId)}/action`, {
    method: 'POST',
    body: JSON.stringify({
      decision: body.decision,
      // Only an uncertain delivery accepts a decision, and the write is
      // refused unless it is still in exactly this state.
      expected_status: 'unknown',
      reason: body.reason,
      evidence_ref: body.evidenceRef ?? '',
    }),
  });
}

export interface ActionSubmission {
  reason: string;
  evidenceRef?: string;
}

export interface ActionRequest {
  path: string;
  body: Record<string, unknown>;
  decision: FenceDecision | EffectDecision | 'cancel';
}

// actionRequest maps one server-offered action onto the endpoint that performs
// it. It returns null for anything it does not recognise: an action the console
// cannot faithfully execute is shown as unavailable, never approximated with a
// different call that might mean something else.
export function actionRequest(
  action: AdminAvailableAction,
  submission: ActionSubmission,
): ActionRequest | null {
  const reason = submission.reason.trim();
  const evidenceRef = (submission.evidenceRef ?? '').trim();
  const common = { reason, evidence_ref: evidenceRef };

  switch (action.kind) {
    case 'fence_resolve':
      return {
        path: `/admin/executions/${encodeURIComponent(action.target)}/fence-action`,
        body: {
          decision: 'resolve',
          expected_fence_version: action.requires_version ?? 0,
          ...common,
        },
        decision: 'resolve',
      };
    case 'fence_abandon':
      return {
        path: `/admin/executions/${encodeURIComponent(action.target)}/fence-action`,
        body: {
          decision: 'abandon',
          expected_fence_version: action.requires_version ?? 0,
          ...common,
        },
        decision: 'abandon',
      };
    case 'queue_cancel':
      return {
        path: `/admin/executions/${encodeURIComponent(action.target)}/queue-cancel`,
        body: { ...common },
        decision: 'cancel',
      };
    case 'effect_abandon':
    case 'effect_mark_delivered':
    case 'effect_requeue': {
      const decision =
        action.kind === 'effect_abandon'
          ? 'abandon'
          : action.kind === 'effect_mark_delivered'
            ? 'mark_delivered'
            : 'requeue';
      return {
        path: `/admin/effects/${encodeURIComponent(action.target)}/action`,
        body: {
          decision,
          expected_status: 'unknown',
          ...common,
        },
        decision,
      };
    }
    default:
      return null;
  }
}

// submitAction performs a mapped action. Split from actionRequest so the
// mapping stays pure and testable without a network.
export async function submitAction(
  action: AdminAvailableAction,
  submission: ActionSubmission,
): Promise<void> {
  const request = actionRequest(action, submission);
  if (!request) {
    throw new Error(`unsupported action: ${action.kind}`);
  }
  await adminFetch(request.path, {
    method: 'POST',
    body: JSON.stringify(request.body),
  });
}
