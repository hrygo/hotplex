/**
 * Execution console presentation logic.
 *
 * Pure functions only, so the rules that matter — how a mixed-source timeline
 * is ordered, how pages are combined, and how an evidence gap is labelled —
 * are unit-testable without React or a network.
 */

import type {
  AdminExecutionCursor,
  AdminExecutionSummary,
  AdminTimelineItem,
  EvidenceState,
} from '@/lib/types/admin';

// The order a run actually happens in. Delivery sits between running and
// terminal because a message can be accepted while the turn is still finishing,
// and an operator needs to see that gap rather than have it sorted to the end.
export const PHASE_ORDER = [
  'accepted',
  'queued',
  'dispatched',
  'running',
  'delivery',
  'terminal',
  'fenced',
  'truncated',
] as const;

export function phaseRank(phase: string): number {
  const index = (PHASE_ORDER as readonly string[]).indexOf(phase);
  // An unknown phase from a newer server sorts last but stays visible, rather
  // than being dropped or silently folded into "terminal".
  return index === -1 ? PHASE_ORDER.length : index;
}

// sortTimelineItems orders a mixed-source projection: by phase, then by when
// the fact happened, then by source so two facts at the same instant keep a
// stable order across reloads.
export function sortTimelineItems<T extends AdminTimelineItem>(items: T[]): T[] {
  return items
    .map((item, index) => ({ item, index }))
    .sort((a, b) => {
      const byPhase = phaseRank(a.item.phase) - phaseRank(b.item.phase);
      if (byPhase !== 0) return byPhase;
      const byTime = a.item.fact_time - b.item.fact_time;
      if (byTime !== 0) return byTime;
      const bySource = a.item.source.localeCompare(b.item.source);
      if (bySource !== 0) return bySource;
      return a.index - b.index;
    })
    .map((entry) => entry.item);
}

export interface ExecutionFilters {
  sessionId: string;
  deliveryStatus: string;
  runtimeStatus: string;
  sinceMs: number;
  untilMs: number;
}

export const EMPTY_EXECUTION_FILTERS: ExecutionFilters = {
  sessionId: '',
  deliveryStatus: '',
  runtimeStatus: '',
  sinceMs: 0,
  untilMs: 0,
};

// filtersAreClean reports whether the current filters would return the
// unfiltered newest-first page. Paging with a keyset cursor while a filter is
// applied is fine; what must not happen is showing a stale cursor from a
// previous filter set.
export function filtersAreClean(filters: ExecutionFilters): boolean {
  return (
    filters.sessionId.trim() === '' &&
    filters.deliveryStatus === '' &&
    filters.runtimeStatus === '' &&
    filters.sinceMs === 0 &&
    filters.untilMs === 0
  );
}

export interface ExecutionPage {
  rows: AdminExecutionSummary[];
  cursor: AdminExecutionCursor | null;
}

// mergeExecutionPage appends a keyset page. Rows already held are dropped:
// between two requests a new run can be created, and an offset-free cursor can
// still hand back a row the operator is already looking at.
export function mergeExecutionPage(
  previous: ExecutionPage,
  next: AdminExecutionSummary[],
  cursor: AdminExecutionCursor | null,
): ExecutionPage {
  const seen = new Set(previous.rows.map((row) => row.execution_id));
  const merged = previous.rows.slice();
  for (const row of next) {
    if (seen.has(row.execution_id)) continue;
    seen.add(row.execution_id);
    merged.push(row);
  }
  return { rows: merged, cursor };
}

export const EMPTY_EXECUTION_PAGE: ExecutionPage = { rows: [], cursor: null };

export function isTerminalRuntime(status: string): boolean {
  return status === 'completed' || status === 'failed' || status === 'unknown';
}

// Label maps are declared with literal keys on purpose: i18next is configured
// with typed resources, so a computed `t(\`executions.status.${value}\`)` is a
// type error AND silently falls back at runtime. A lookup with an explicit
// union keeps the key set closed and the fallback visible.
export const STATUS_LABEL_KEYS = {
  accepted: 'executions.status.accepted',
  delivered: 'executions.status.delivered',
  unknown: 'executions.status.unknown',
  failed: 'executions.status.failed',
  queued: 'executions.status.queued',
  pending: 'executions.status.pending',
  running: 'executions.status.running',
  completed: 'executions.status.completed',
} as const;

export type StatusLabelKey = (typeof STATUS_LABEL_KEYS)[keyof typeof STATUS_LABEL_KEYS];

export function statusLabelKey(value: string): StatusLabelKey | null {
  const map: Record<string, StatusLabelKey> = STATUS_LABEL_KEYS;
  return map[value] ?? null;
}

export const PHASE_LABEL_KEYS = {
  accepted: 'executions.phase.accepted',
  queued: 'executions.phase.queued',
  dispatched: 'executions.phase.dispatched',
  running: 'executions.phase.running',
  delivery: 'executions.phase.delivery',
  terminal: 'executions.phase.terminal',
  fenced: 'executions.phase.fenced',
  truncated: 'executions.phase.truncated',
} as const;

export type PhaseLabelKey = (typeof PHASE_LABEL_KEYS)[keyof typeof PHASE_LABEL_KEYS];

export function phaseLabelKey(phase: string): PhaseLabelKey | null {
  const map: Record<string, PhaseLabelKey> = PHASE_LABEL_KEYS;
  return map[phase] ?? null;
}

// NOTE_LABEL_KEYS covers every code the server emits (internal/admin/executions.go).
// A code this build does not know about renders verbatim: a new server note is
// information, not something to swallow.
export const NOTE_LABEL_KEYS = {
  events_unavailable: 'executions.notes.events_unavailable',
  events_not_configured: 'executions.notes.events_not_configured',
  events_truncated: 'executions.notes.events_truncated',
  effects_unavailable: 'executions.notes.effects_unavailable',
  effects_not_configured: 'executions.notes.effects_not_configured',
  effects_truncated: 'executions.notes.effects_truncated',
  no_delivery_planned: 'executions.notes.no_delivery_planned',
} as const;

export type NoteLabelKey = (typeof NOTE_LABEL_KEYS)[keyof typeof NOTE_LABEL_KEYS];

export function noteLabelKey(note: string): NoteLabelKey | null {
  const map: Record<string, NoteLabelKey> = NOTE_LABEL_KEYS;
  return map[note] ?? null;
}

export const ACTION_LABEL_KEYS = {
  fence_resolve: 'executions.actions.kind.fence_resolve',
  fence_abandon: 'executions.actions.kind.fence_abandon',
  queue_cancel: 'executions.actions.kind.queue_cancel',
  effect_abandon: 'executions.actions.kind.effect_abandon',
  effect_mark_delivered: 'executions.actions.kind.effect_mark_delivered',
  effect_requeue: 'executions.actions.kind.effect_requeue',
} as const;

export type ActionLabelKey = (typeof ACTION_LABEL_KEYS)[keyof typeof ACTION_LABEL_KEYS];

export function actionLabelKey(kind: string): ActionLabelKey | null {
  const map: Record<string, ActionLabelKey> = ACTION_LABEL_KEYS;
  return map[kind] ?? null;
}

export const ACTION_CONFIRM_KEYS = {
  fence_resolve: 'executions.actions.confirm_title.fence_resolve',
  fence_abandon: 'executions.actions.confirm_title.fence_abandon',
  queue_cancel: 'executions.actions.confirm_title.queue_cancel',
  effect_abandon: 'executions.actions.confirm_title.effect_abandon',
  effect_mark_delivered: 'executions.actions.confirm_title.effect_mark_delivered',
  effect_requeue: 'executions.actions.confirm_title.effect_requeue',
} as const;

export type ActionConfirmKey = (typeof ACTION_CONFIRM_KEYS)[keyof typeof ACTION_CONFIRM_KEYS];

export function actionConfirmKey(kind: string): ActionConfirmKey | null {
  const map: Record<string, ActionConfirmKey> = ACTION_CONFIRM_KEYS;
  return map[kind] ?? null;
}

// evidenceLabelKey maps an evidence state onto an i18n key. Absence and
// "never recorded" are distinct strings on purpose: the console must be able
// to say which one it is showing.
export type EvidenceLabelKey =
  | 'executions.evidence.recorded'
  | 'executions.evidence.not_recorded'
  | 'executions.evidence.unavailable'
  | 'executions.evidence.unknown';

export function evidenceLabelKey(state: EvidenceState | undefined): EvidenceLabelKey {
  switch (state) {
    case 'recorded':
      return 'executions.evidence.recorded';
    case 'not_recorded_for_this_run':
      return 'executions.evidence.not_recorded';
    case 'unavailable':
      return 'executions.evidence.unavailable';
    default:
      return 'executions.evidence.unknown';
  }
}

export function evidenceTone(state: EvidenceState | undefined): 'ok' | 'warn' | 'muted' {
  switch (state) {
    case 'recorded':
      return 'ok';
    case 'not_recorded_for_this_run':
      return 'warn';
    default:
      return 'muted';
  }
}

// runtimeTone maps a runtime status onto a badge tone. "unknown" is deliberately
// amber rather than red or green: the run's outcome is undetermined, and
// painting it either way states a conclusion the record does not support.
export function runtimeTone(status: string): 'ok' | 'warn' | 'danger' | 'info' | 'muted' {
  switch (status) {
    case 'completed':
      return 'ok';
    case 'failed':
      return 'danger';
    case 'unknown':
      return 'warn';
    case 'running':
      return 'info';
    case 'pending':
    case 'queued':
      return 'muted';
    default:
      return 'muted';
  }
}

export function deliveryTone(status: string): 'ok' | 'warn' | 'danger' | 'muted' {
  switch (status) {
    case 'delivered':
      return 'ok';
    case 'unknown':
      return 'warn';
    case 'failed':
      return 'danger';
    case 'accepted':
      return 'muted';
    default:
      return 'muted';
  }
}

export function truncateMiddle(value: string, size = 20): string {
  if (!value || value.length <= size) return value;
  return `${value.slice(0, Math.max(0, size - 7))}...${value.slice(-4)}`;
}
