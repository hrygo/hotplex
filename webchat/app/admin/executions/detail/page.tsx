'use client';

import { Suspense, useCallback, useMemo, useState } from 'react';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';

import { StatusBadge } from '@/components/admin/status-badge';
import {
  DELIVERY_STATUS_STYLES,
  PHASE_STYLES,
  RUNTIME_STATUS_STYLES,
  toStatusMap,
} from '@/components/admin/execution-status-map';
import { LoadingState, ErrorState } from '@/components/admin/resource-states';
import { useAdminUI } from '@/context/admin-ui-context';
import { useResource } from '@/hooks/use-resource';
import {
  actionRequest,
  getExecutionTimeline,
  submitAction,
} from '@/lib/api/admin-executions';
import {
  actionConfirmKey,
  actionLabelKey,
  evidenceLabelKey,
  evidenceTone,
  noteLabelKey,
  phaseLabelKey,
  sortTimelineItems,
  truncateMiddle,
} from '@/lib/executions-console';
import { formatDateTime, formatRelative } from '@/lib/utils/format-time';
import type {
  AdminAvailableAction,
  AdminExecutionTimeline,
} from '@/lib/types/admin';

const REASON_MAX = 512;
const EVIDENCE_REF_MAX = 256;

function ExecutionsDetailInner() {
  const { t } = useTranslation('admin');
  const { showToast, confirm } = useAdminUI();
  const params = useSearchParams();
  const executionId = params.get('id') ?? '';

  const fetcher = useCallback(async () => {
    if (!executionId) {
      throw new Error(t('executions.detail.missing_id'));
    }
    return getExecutionTimeline(executionId);
  }, [executionId, t]);
  const { data: timeline, loading, error, reload } = useResource<AdminExecutionTimeline>(
    fetcher,
    [executionId],
  );

  const [openAction, setOpenAction] = useState<AdminAvailableAction | null>(null);
  const [reason, setReason] = useState('');
  const [evidenceRef, setEvidenceRef] = useState('');
  const [submitting, setSubmitting] = useState(false);

  const deliveryMap = useMemo(() => toStatusMap(DELIVERY_STATUS_STYLES, t), [t]);
  const runtimeMap = useMemo(() => toStatusMap(RUNTIME_STATUS_STYLES, t), [t]);
  const items = useMemo(
    () => (timeline ? sortTimelineItems(timeline.items ?? []) : []),
    [timeline],
  );

  // A code this build has no label for renders verbatim. A note, phase or
  // action from a newer server is information, not something to drop.
  const noteLabel = (note: string) => {
    const key = noteLabelKey(note);
    return key ? t(key) : note;
  };
  const phaseLabel = (phase: string) => {
    const key = phaseLabelKey(phase);
    return key ? t(key) : phase;
  };
  const actionLabel = (kind: string) => {
    const key = actionLabelKey(kind);
    return key ? t(key) : kind;
  };

  async function runAction(action: AdminAvailableAction) {
    const request = actionRequest(action, { reason, evidenceRef });
    if (!request) {
      showToast(t('executions.actions.unsupported'), 'error');
      return;
    }
    if (reason.trim().length === 0) {
      showToast(t('executions.actions.reason_required'), 'error');
      return;
    }
    if (reason.trim().length > REASON_MAX) {
      showToast(t('executions.actions.reason_too_long', { max: REASON_MAX }), 'error');
      return;
    }
    if (evidenceRef.trim().length > EVIDENCE_REF_MAX) {
      showToast(t('executions.actions.evidence_ref_too_long', { max: EVIDENCE_REF_MAX }), 'error');
      return;
    }

    const confirmKey = actionConfirmKey(action.kind);
    const confirmed = await confirm(
      confirmKey ? t(confirmKey) : action.description,
      action.description,
      {
        confirmLabel: t('executions.actions.submit'),
        cancelLabel: t('executions.actions.cancel'),
        // Abandoning a run and recording a delivery as sent both end a
        // question the record left open, so both are destructive.
        destructive: action.kind !== 'effect_requeue' && action.kind !== 'fence_resolve',
      },
    );
    if (!confirmed) return;

    setSubmitting(true);
    try {
      await submitAction(action, { reason, evidenceRef });
      showToast(t('executions.actions.done'), 'success');
      setOpenAction(null);
      setReason('');
      setEvidenceRef('');
      await reload();
    } catch (e) {
      const status = (e as { status?: number }).status;
      if (status === 409) {
        // Someone else moved the record first. Say so plainly and re-read it
        // rather than showing the operator a retry against a stale token.
        showToast(t('executions.actions.stale'), 'error');
        await reload();
      } else {
        showToast(e instanceof Error ? e.message : String(e), 'error');
      }
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="p-6">
      <div className="mb-5 flex items-start justify-between gap-3">
        <div className="min-w-0">
          <Link
            href="/admin/executions"
            className="text-[10px] font-bold uppercase tracking-wider text-[var(--accent-gold)] hover:underline"
          >
            ← {t('executions.detail.back')}
          </Link>
          <h2 className="mt-1.5 font-mono text-sm font-bold text-[var(--text-primary)]" title={executionId}>
            {truncateMiddle(executionId, 32) || t('executions.detail.untitled')}
          </h2>
        </div>
        <button
          type="button"
          onClick={() => void reload()}
          disabled={loading}
          className="rounded border border-[var(--border-subtle)] px-2.5 py-1 text-[10px] font-bold uppercase tracking-wider text-[var(--text-secondary)] transition-colors hover:bg-[var(--bg-hover)] disabled:opacity-40"
        >
          {t('executions.refresh')}
        </button>
      </div>

      {loading && <LoadingState label={t('executions.loading')} />}
      {error && <ErrorState message={error} onRetry={() => void reload()} />}

      {!loading && !error && timeline && (
        <div className="space-y-5">
          {/* Control facts */}
          <div className="rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)] p-4">
            <div className="grid grid-cols-2 gap-4 md:grid-cols-4">
              <div>
                <div className="text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                  {t('executions.table.delivery')}
                </div>
                <div className="mt-1.5">
                  <StatusBadge status={timeline.execution.delivery_status} map={deliveryMap} />
                </div>
              </div>
              <div>
                <div className="text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                  {t('executions.table.runtime')}
                </div>
                <div className="mt-1.5">
                  <StatusBadge status={timeline.execution.runtime_status} map={runtimeMap} />
                </div>
              </div>
              <div className="min-w-0">
                <div className="text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                  {t('executions.table.session')}
                </div>
                <Link
                  href={`/admin/sessions/detail?id=${encodeURIComponent(timeline.execution.session_id)}`}
                  className="mt-1.5 block truncate font-mono text-xs text-[var(--accent-gold)] hover:underline"
                  title={timeline.execution.session_id}
                >
                  {truncateMiddle(timeline.execution.session_id, 22)}
                </Link>
              </div>
              <div className="min-w-0">
                <div className="text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                  {t('executions.table.worker_run')}
                </div>
                <div
                  className="mt-1.5 truncate font-mono text-xs text-[var(--text-secondary)]"
                  title={timeline.execution.worker_run_id ?? ''}
                >
                  {timeline.execution.worker_run_id
                    ? truncateMiddle(timeline.execution.worker_run_id, 20)
                    : '—'}
                </div>
              </div>
            </div>

            <div className="mt-4 grid grid-cols-2 gap-4 border-t border-[var(--border-subtle)] pt-4 md:grid-cols-4">
              {([
                ['executions.detail.created', timeline.execution.created_at],
                ['executions.detail.started', timeline.execution.started_at],
                ['executions.detail.finished', timeline.execution.finished_at],
                ['executions.detail.updated', timeline.execution.updated_at],
              ] as const).map(([key, value]) => (
                <div key={key}>
                  <div className="text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                    {t(key)}
                  </div>
                  <div className="mt-1 font-mono text-[11px] text-[var(--text-secondary)]" title={value ? formatDateTime(value) : ''}>
                    {value ? formatDateTime(value) : '—'}
                  </div>
                </div>
              ))}
            </div>

            {(timeline.execution.fence_reason || timeline.execution.runtime_error_code) && (
              <div className="mt-4 space-y-2 border-t border-[var(--border-subtle)] pt-4">
                {timeline.execution.fence_reason && (
                  <div className="flex items-start gap-2 text-[11px]">
                    <span className="shrink-0 font-bold uppercase tracking-wider text-[var(--accent-amber)]">
                      {t('executions.table.fence')} v{timeline.execution.fence_version}
                    </span>
                    <span className="font-mono text-[var(--text-secondary)]">{timeline.execution.fence_reason}</span>
                  </div>
                )}
                {timeline.execution.runtime_error_code && (
                  <div className="flex items-start gap-2 text-[11px]">
                    <span className="shrink-0 font-bold uppercase tracking-wider text-[var(--text-faint)]">
                      {t('executions.detail.error_code')}
                    </span>
                    <span className="font-mono text-[var(--text-secondary)]">
                      {timeline.execution.runtime_error_code}
                    </span>
                  </div>
                )}
              </div>
            )}
          </div>

          {/* Evidence completeness — absence is stated, never guessed at */}
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            {([
              ['plan', timeline.plan_evidence],
              ['delivery', timeline.effect_evidence],
            ] as const).map(([key, state]) => {
              const tone = evidenceTone(state);
              const toneClass =
                tone === 'ok'
                  ? 'border-[var(--accent-emerald)]/25 bg-[var(--accent-emerald)]/5'
                  : tone === 'warn'
                    ? 'border-[var(--accent-amber)]/25 bg-[var(--accent-amber)]/5'
                    : 'border-[var(--border-subtle)] bg-[var(--bg-surface)]/40';
              return (
                <div key={key} className={`rounded-[var(--radius-md)] border p-3 ${toneClass}`}>
                  <div className="text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                    {key === 'plan'
                      ? t('executions.detail.plan_evidence')
                      : t('executions.detail.effect_evidence')}
                  </div>
                  <div className="mt-1 text-xs text-[var(--text-secondary)]">
                    {t(evidenceLabelKey(state))}
                  </div>
                </div>
              );
            })}
          </div>

          {timeline.truncated && (
            <div className="rounded-[var(--radius-md)] border border-[var(--accent-amber)]/25 bg-[var(--accent-amber)]/5 px-4 py-2.5 text-xs text-[var(--text-secondary)]">
              {t('executions.detail.truncated')}
            </div>
          )}

          {timeline.notes && timeline.notes.length > 0 && (
            <div className="rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/40 p-4">
              <div className="mb-2 text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                {t('executions.detail.notes')}
              </div>
              <ul className="space-y-1">
                {timeline.notes.map((note) => (
                  <li key={note} className="text-[11px] text-[var(--text-muted)]">
                    · {noteLabel(note)}
                  </li>
                ))}
              </ul>
            </div>
          )}

          {/* Actions — rendered from the server's list, never derived here */}
          <div className="rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)] p-4">
            <div className="mb-3 text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
              {t('executions.actions.title')}
            </div>
            {timeline.actions.length === 0 ? (
              <p className="text-xs text-[var(--text-muted)]">{t('executions.actions.none')}</p>
            ) : (
              <div className="space-y-2.5">
                {timeline.actions.map((action) => {
                  const supported = actionRequest(action, { reason: 'probe' }) !== null;
                  const isOpen = openAction?.kind === action.kind && openAction?.target === action.target;
                  return (
                    <div
                      key={`${action.kind}:${action.target}`}
                      className="rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-base)]/50 p-3"
                    >
                      <div className="flex flex-wrap items-start justify-between gap-2">
                        <div className="min-w-0">
                          <div className="text-xs font-bold text-[var(--text-primary)]">
                            {actionLabel(action.kind)}
                          </div>
                          <div className="mt-0.5 text-[11px] text-[var(--text-muted)]">{action.description}</div>
                          <div className="mt-1 font-mono text-[10px] text-[var(--text-faint)]">
                            {t('executions.actions.target', { target: truncateMiddle(action.target, 24) })}
                            {action.requires_version !== undefined &&
                              ` · ${t('executions.actions.requires_version', { version: action.requires_version })}`}
                          </div>
                        </div>
                        <button
                          type="button"
                          disabled={!supported}
                          onClick={() => {
                            setOpenAction(isOpen ? null : action);
                            setReason('');
                            setEvidenceRef('');
                          }}
                          className="shrink-0 rounded border border-[var(--border-subtle)] px-2.5 py-1 text-[10px] font-bold uppercase tracking-wider text-[var(--text-secondary)] transition-colors hover:bg-[var(--bg-hover)] disabled:opacity-40 disabled:cursor-not-allowed"
                        >
                          {supported ? t('executions.actions.choose') : t('executions.actions.unsupported')}
                        </button>
                      </div>

                      {isOpen && supported && (
                        <div className="mt-3 space-y-2.5 border-t border-[var(--border-subtle)] pt-3">
                          <label className="block">
                            <span className="mb-1 block text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                              {t('executions.actions.reason')}
                            </span>
                            <textarea
                              value={reason}
                              onChange={(e) => setReason(e.target.value)}
                              rows={2}
                              maxLength={REASON_MAX}
                              placeholder={t('executions.actions.reason_placeholder')}
                              className="w-full resize-none rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-base)] px-3 py-1.5 text-xs text-[var(--text-primary)] outline-none focus:border-[var(--accent-gold)]"
                            />
                          </label>
                          <label className="block">
                            <span className="mb-1 block text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                              {t('executions.actions.evidence_ref')}
                            </span>
                            <input
                              value={evidenceRef}
                              onChange={(e) => setEvidenceRef(e.target.value)}
                              maxLength={EVIDENCE_REF_MAX}
                              placeholder={t('executions.actions.evidence_ref_placeholder')}
                              className="w-full rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-base)] px-3 py-1.5 text-xs font-mono text-[var(--text-primary)] outline-none focus:border-[var(--accent-gold)]"
                            />
                          </label>
                          <div className="flex items-center gap-2">
                            <button
                              type="button"
                              disabled={submitting}
                              onClick={() => void runAction(action)}
                              className="rounded-[var(--radius-sm)] bg-[var(--accent-gold)] px-3 py-1.5 text-xs font-bold text-[var(--bg-base)] transition-opacity hover:opacity-90 disabled:opacity-40"
                            >
                              {t('executions.actions.submit')}
                            </button>
                            <button
                              type="button"
                              onClick={() => setOpenAction(null)}
                              className="rounded-[var(--radius-sm)] border border-[var(--border-subtle)] px-3 py-1.5 text-xs font-bold text-[var(--text-secondary)] transition-colors hover:bg-[var(--bg-hover)]"
                            >
                              {t('executions.actions.cancel')}
                            </button>
                          </div>
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            )}
          </div>

          {/* Timeline */}
          <div>
            <div className="mb-2 flex items-center justify-between">
              <h3 className="text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                {t('executions.timeline.title')}
              </h3>
              <span className="text-[10px] text-[var(--text-faint)]">
                {t('executions.timeline.lag_hint')}
              </span>
            </div>
            {items.length === 0 ? (
              <p className="rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/40 p-4 text-xs text-[var(--text-muted)]">
                {t('executions.timeline.empty')}
              </p>
            ) : (
              <div className="overflow-hidden rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]">
                <div className="grid grid-cols-[110px_120px_1.3fr_150px_150px_1.2fr] gap-3 border-b border-[var(--border-subtle)] bg-[var(--bg-hover)]/60 px-4 py-2.5">
                  {[
                    t('executions.timeline.table.phase'),
                    t('executions.timeline.table.source'),
                    t('executions.timeline.table.kind'),
                    t('executions.timeline.table.fact_time'),
                    t('executions.timeline.table.observed_at'),
                    t('executions.timeline.table.evidence'),
                  ].map((label) => (
                    <div
                      key={label}
                      className="text-[10px] font-mono font-bold uppercase tracking-widest text-[var(--text-faint)]"
                    >
                      {label}
                    </div>
                  ))}
                </div>
                {items.map((row, index) => {
                  const phase = PHASE_STYLES[row.phase] ?? PHASE_STYLES.truncated;
                  return (
                    <div
                      key={`${row.source}:${row.kind}:${row.fact_time}:${index}`}
                      className="grid grid-cols-[110px_120px_1.3fr_150px_150px_1.2fr] items-center gap-3 border-t border-[var(--border-subtle)] px-4 py-2"
                    >
                      <div>
                        <span
                          className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-[10px] font-bold uppercase tracking-wider ${phase.text}`}
                          style={{ background: phase.bg }}
                        >
                          <span className={`h-1.5 w-1.5 rounded-full ${phase.dot}`} />
                          {phaseLabel(row.phase)}
                        </span>
                      </div>
                      <div className="truncate font-mono text-[10px] text-[var(--text-faint)]">
                        {row.source}
                      </div>
                      <div className="truncate font-mono text-[11px] text-[var(--text-secondary)]" title={row.kind}>
                        {row.kind}
                      </div>
                      <div className="font-mono text-[10px] text-[var(--text-muted)]" title={formatDateTime(row.fact_time)}>
                        {row.fact_time ? formatRelative(row.fact_time) : '—'}
                      </div>
                      <div className="font-mono text-[10px] text-[var(--text-faint)]" title={formatDateTime(row.observed_at)}>
                        {row.observed_at ? formatRelative(row.observed_at) : '—'}
                      </div>
                      <div className="min-w-0">
                        {row.evidence ? (
                          <div className="truncate font-mono text-[10px] text-[var(--text-muted)]" title={row.evidence}>
                            {row.evidence}
                          </div>
                        ) : row.evidence_state ? (
                          <div className="text-[10px] text-[var(--text-faint)]">
                            {t(evidenceLabelKey(row.evidence_state))}
                          </div>
                        ) : (
                          <span className="text-[10px] text-[var(--text-faint)]">—</span>
                        )}
                        {row.effect_id && (
                          <div
                            className="truncate font-mono text-[9px] text-[var(--text-faint)]"
                            title={row.effect_id}
                          >
                            {truncateMiddle(row.effect_id, 20)}
                          </div>
                        )}
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

export default function ExecutionsDetailPage() {
  return (
    <Suspense fallback={<LoadingState />}>
      <ExecutionsDetailInner />
    </Suspense>
  );
}
