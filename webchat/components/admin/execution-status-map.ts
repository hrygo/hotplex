// Execution status → badge styles for the execution console.
//
// Delivery and runtime get SEPARATE maps on purpose: delivery answers "was the
// input accepted and handed over", runtime answers "did the Worker finish".
// Sharing one map is how a console ends up painting a run green because the
// gateway merely took delivery of it.
//
// Labels are i18n keys rather than text so the console reads correctly in both
// languages; <StatusBadge> takes the finished map, so the colours stay shared
// with the rest of the admin panel while the words are translated.
import type { StatusMap } from '@/components/admin/status-badge';
import type { StatusLabelKey } from '@/lib/executions-console';
import type { TFunction } from 'i18next';

export interface ExecutionStatusStyle {
  bg: string;
  text: string;
  dot: string;
  labelKey: StatusLabelKey;
}

export type ExecutionStatusMap = Record<string, ExecutionStatusStyle>;

export const RUNTIME_STATUS_STYLES: ExecutionStatusMap = {
  queued: {
    bg: 'rgba(148, 163, 184, 0.12)',
    text: 'text-[var(--text-secondary)]',
    dot: 'bg-[var(--text-muted)]',
    labelKey: 'executions.status.queued',
  },
  pending: {
    bg: 'rgba(96, 165, 250, 0.12)',
    text: 'text-[var(--accent-gold)]',
    dot: 'bg-[var(--accent-gold)]',
    labelKey: 'executions.status.pending',
  },
  running: {
    bg: 'rgba(96, 165, 250, 0.12)',
    text: 'text-[var(--accent-gold)]',
    dot: 'bg-[var(--accent-gold)]',
    labelKey: 'executions.status.running',
  },
  completed: {
    bg: 'rgba(52, 211, 153, 0.12)',
    text: 'text-[var(--accent-emerald)]',
    dot: 'bg-[var(--accent-emerald)]',
    labelKey: 'executions.status.completed',
  },
  failed: {
    bg: 'rgba(244, 63, 94, 0.12)',
    text: 'text-[var(--accent-coral)]',
    dot: 'bg-[var(--accent-coral)]',
    labelKey: 'executions.status.failed',
  },
  // Amber, never green and never red: the outcome of an unknown run is
  // undetermined, and either colour states a conclusion the record refuses.
  unknown: {
    bg: 'rgba(245, 158, 11, 0.12)',
    text: 'text-[var(--accent-amber)]',
    dot: 'bg-[var(--accent-amber)]',
    labelKey: 'executions.status.unknown',
  },
};

export const DELIVERY_STATUS_STYLES: ExecutionStatusMap = {
  accepted: {
    bg: 'rgba(148, 163, 184, 0.12)',
    text: 'text-[var(--text-secondary)]',
    dot: 'bg-[var(--text-muted)]',
    labelKey: 'executions.status.accepted',
  },
  delivered: {
    bg: 'rgba(52, 211, 153, 0.12)',
    text: 'text-[var(--accent-emerald)]',
    dot: 'bg-[var(--accent-emerald)]',
    labelKey: 'executions.status.delivered',
  },
  unknown: {
    bg: 'rgba(245, 158, 11, 0.12)',
    text: 'text-[var(--accent-amber)]',
    dot: 'bg-[var(--accent-amber)]',
    labelKey: 'executions.status.unknown',
  },
  failed: {
    bg: 'rgba(244, 63, 94, 0.12)',
    text: 'text-[var(--accent-coral)]',
    dot: 'bg-[var(--accent-coral)]',
    labelKey: 'executions.status.failed',
  },
};

// PHASE_STYLES tints the timeline's phase column so an operator can scan a run
// without reading every row's kind.
export const PHASE_STYLES: Record<string, { bg: string; text: string; dot: string }> = {
  accepted: { bg: 'rgba(148, 163, 184, 0.12)', text: 'text-[var(--text-secondary)]', dot: 'bg-[var(--text-muted)]' },
  queued: { bg: 'rgba(148, 163, 184, 0.12)', text: 'text-[var(--text-secondary)]', dot: 'bg-[var(--text-muted)]' },
  dispatched: { bg: 'rgba(96, 165, 250, 0.12)', text: 'text-[var(--accent-gold)]', dot: 'bg-[var(--accent-gold)]' },
  running: { bg: 'rgba(96, 165, 250, 0.12)', text: 'text-[var(--accent-gold)]', dot: 'bg-[var(--accent-gold)]' },
  delivery: { bg: 'rgba(96, 165, 250, 0.12)', text: 'text-[var(--accent-blue)]', dot: 'bg-[var(--accent-blue)]' },
  terminal: { bg: 'rgba(52, 211, 153, 0.12)', text: 'text-[var(--accent-emerald)]', dot: 'bg-[var(--accent-emerald)]' },
  fenced: { bg: 'rgba(245, 158, 11, 0.12)', text: 'text-[var(--accent-amber)]', dot: 'bg-[var(--accent-amber)]' },
  truncated: { bg: 'rgba(148, 163, 184, 0.12)', text: 'text-[var(--text-muted)]', dot: 'bg-[var(--text-muted)]' },
};

// toStatusMap resolves the label keys against the active language. An
// unrecognised status falls through to the raw value, which is what an
// operator needs to see when a newer server introduces one.
export function toStatusMap(styles: ExecutionStatusMap, t: TFunction<'admin'>): StatusMap {
  const map: StatusMap = {};
  for (const [status, style] of Object.entries(styles)) {
    map[status] = {
      bg: style.bg,
      text: style.text,
      dot: style.dot,
      label: t(style.labelKey, { defaultValue: status }),
    };
  }
  return map;
}
