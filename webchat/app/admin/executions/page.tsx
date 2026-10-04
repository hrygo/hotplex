'use client';

import { useCallback, useMemo, useState } from 'react';
import Link from 'next/link';
import { useTranslation } from 'react-i18next';

import { StatusBadge } from '@/components/admin/status-badge';
import {
  DELIVERY_STATUS_STYLES,
  RUNTIME_STATUS_STYLES,
  toStatusMap,
} from '@/components/admin/execution-status-map';
import { LoadingState, ErrorState, EmptyState } from '@/components/admin/resource-states';
import { useResource } from '@/hooks/use-resource';
import { useAdminUI } from '@/context/admin-ui-context';
import { listExecutions } from '@/lib/api/admin-executions';
import {
  EMPTY_EXECUTION_FILTERS,
  EMPTY_EXECUTION_PAGE,
  filtersAreClean,
  mergeExecutionPage,
  statusLabelKey,
  truncateMiddle,
  type ExecutionFilters,
  type ExecutionPage,
} from '@/lib/executions-console';
import { formatDateTime, formatRelative } from '@/lib/utils/format-time';

const PAGE_SIZE = 50;

const DELIVERY_OPTIONS = ['', 'accepted', 'delivered', 'unknown', 'failed'];
const RUNTIME_OPTIONS = ['', 'queued', 'pending', 'running', 'completed', 'failed', 'unknown'];

export default function ExecutionsPage() {
  const { t } = useTranslation('admin');
  const { showToast } = useAdminUI();

  // Filters are drafted here and applied on submit, so a half-typed session id
  // does not fire a request per keystroke.
  const [draft, setDraft] = useState<ExecutionFilters>(EMPTY_EXECUTION_FILTERS);
  const [filters, setFilters] = useState<ExecutionFilters>(EMPTY_EXECUTION_FILTERS);
  const [loadingMore, setLoadingMore] = useState(false);

  const query = useMemo(
    () => ({
      sessionId: filters.sessionId.trim() || undefined,
      deliveryStatus: filters.deliveryStatus || undefined,
      runtimeStatus: filters.runtimeStatus || undefined,
      sinceMs: filters.sinceMs || undefined,
      untilMs: filters.untilMs || undefined,
      limit: PAGE_SIZE,
    }),
    [filters],
  );
  const fetcher = useCallback(() => listExecutions(query, null), [query]);
  const { data, loading, error, reload } = useResource(fetcher, [query]);

  // Older pages accumulate under the filter set they were fetched with. Tagging
  // them with the filter key means a filter change drops them during render,
  // with no effect and no chance of paging a stale cursor into a new query.
  const filterKey = JSON.stringify(query);
  const [older, setOlder] = useState<{ key: string; page: ExecutionPage }>({
    key: '',
    page: EMPTY_EXECUTION_PAGE,
  });
  const carried = older.key === filterKey ? older.page : EMPTY_EXECUTION_PAGE;

  const base: ExecutionPage = useMemo(
    () => ({ rows: data?.executions ?? [], cursor: data?.next_cursor ?? null }),
    [data],
  );
  const view: ExecutionPage = useMemo(
    () => mergeExecutionPage(base, carried.rows, carried.cursor ?? base.cursor),
    [base, carried],
  );

  async function loadOlder() {
    if (!view.cursor) return;
    setLoadingMore(true);
    try {
      const res = await listExecutions(query, view.cursor);
      setOlder((prev) => {
        const prior = prev.key === filterKey ? prev.page : EMPTY_EXECUTION_PAGE;
        return {
          key: filterKey,
          page: mergeExecutionPage(prior, res.executions ?? [], res.next_cursor ?? null),
        };
      });
    } catch (e) {
      // Appending is best-effort: the rows already on screen stay valid, so a
      // failed "load older" reports itself instead of clearing the view.
      showToast(e instanceof Error ? e.message : String(e), 'error');
    } finally {
      setLoadingMore(false);
    }
  }

  const deliveryMap = useMemo(() => toStatusMap(DELIVERY_STATUS_STYLES, t), [t]);
  const runtimeMap = useMemo(() => toStatusMap(RUNTIME_STATUS_STYLES, t), [t]);

  const unfiltered = filtersAreClean(filters);

  // A status this build does not have a label for renders as its raw value,
  // which is more use to an operator than an empty cell.
  const statusLabel = (value: string) => {
    const key = statusLabelKey(value);
    return key ? t(key) : value;
  };

  return (
    <div className="p-6">
      <div className="mb-5">
        <h2 className="text-sm font-bold text-[var(--text-primary)]">{t('executions.title')}</h2>
        <p className="mt-1 text-xs text-[var(--text-muted)]">{t('executions.subtitle')}</p>
      </div>

      {/* Filters */}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setFilters(draft);
        }}
        className="mb-5 grid grid-cols-1 md:grid-cols-[2fr_1fr_1fr_auto] gap-3 items-end"
      >
        <label className="block">
          <span className="mb-1 block text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
            {t('executions.filters.session')}
          </span>
          <input
            value={draft.sessionId}
            onChange={(e) => setDraft({ ...draft, sessionId: e.target.value })}
            placeholder={t('executions.filters.session_placeholder')}
            className="w-full rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-base)] px-3 py-1.5 text-xs font-mono text-[var(--text-primary)] outline-none focus:border-[var(--accent-gold)]"
          />
        </label>

        <label className="block">
          <span className="mb-1 block text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
            {t('executions.filters.delivery')}
          </span>
          <select
            value={draft.deliveryStatus}
            onChange={(e) => setDraft({ ...draft, deliveryStatus: e.target.value })}
            className="w-full rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-base)] px-3 py-1.5 text-xs text-[var(--text-primary)] outline-none focus:border-[var(--accent-gold)]"
          >
            {DELIVERY_OPTIONS.map((value) => (
              <option key={value || 'all'} value={value}>
                {value === '' ? t('executions.filters.all') : statusLabel(value)}
              </option>
            ))}
          </select>
        </label>

        <label className="block">
          <span className="mb-1 block text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
            {t('executions.filters.runtime')}
          </span>
          <select
            value={draft.runtimeStatus}
            onChange={(e) => setDraft({ ...draft, runtimeStatus: e.target.value })}
            className="w-full rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-base)] px-3 py-1.5 text-xs text-[var(--text-primary)] outline-none focus:border-[var(--accent-gold)]"
          >
            {RUNTIME_OPTIONS.map((value) => (
              <option key={value || 'all'} value={value}>
                {value === '' ? t('executions.filters.all') : statusLabel(value)}
              </option>
            ))}
          </select>
        </label>

        <div className="flex items-center gap-2">
          <button
            type="submit"
            className="rounded-[var(--radius-sm)] bg-[var(--accent-gold)] px-3 py-1.5 text-xs font-bold text-[var(--bg-base)] transition-opacity hover:opacity-90"
          >
            {t('executions.filters.apply')}
          </button>
          <button
            type="button"
            onClick={() => {
              setDraft(EMPTY_EXECUTION_FILTERS);
              setFilters(EMPTY_EXECUTION_FILTERS);
            }}
            disabled={unfiltered && draft === EMPTY_EXECUTION_FILTERS}
            className="rounded-[var(--radius-sm)] border border-[var(--border-subtle)] px-3 py-1.5 text-xs font-bold text-[var(--text-secondary)] transition-colors hover:bg-[var(--bg-hover)] disabled:opacity-40 disabled:cursor-not-allowed"
          >
            {t('executions.filters.reset')}
          </button>
        </div>
      </form>

      {loading && <LoadingState label={t('executions.loading')} />}
      {error && <ErrorState message={error} onRetry={() => void reload()} />}
      {!loading && !error && view.rows.length === 0 && (
        <EmptyState
          title={t('executions.empty.title')}
          description={t('executions.empty.description')}
        />
      )}

      {/* Table */}
      {!loading && !error && view.rows.length > 0 && (
        <div className="overflow-hidden rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]">
          <div className="grid grid-cols-[120px_1.6fr_1.2fr_130px_130px_1fr_90px] gap-3 border-b border-[var(--border-subtle)] bg-[var(--bg-hover)]/60 px-4 py-3">
            {[
              t('executions.table.created'),
              t('executions.table.execution'),
              t('executions.table.session'),
              t('executions.table.delivery'),
              t('executions.table.runtime'),
              t('executions.table.worker_run'),
              t('executions.table.fence'),
            ].map((label) => (
              <div key={label} className="text-[10px] font-mono font-bold uppercase tracking-widest text-[var(--text-faint)]">
                {label}
              </div>
            ))}
          </div>
          {view.rows.map((row) => (
            <Link
              key={row.execution_id}
              href={`/admin/executions/detail?id=${encodeURIComponent(row.execution_id)}`}
              className="grid grid-cols-[120px_1.6fr_1.2fr_130px_130px_1fr_90px] items-center gap-3 border-t border-[var(--border-subtle)] px-4 py-2.5 transition-colors hover:bg-[var(--bg-hover)]/40"
            >
              <div className="text-[10px] font-mono text-[var(--text-faint)]" title={formatDateTime(row.created_at)}>
                {formatRelative(row.created_at)}
              </div>
              <div className="min-w-0 font-mono text-xs text-[var(--text-primary)]" title={row.execution_id}>
                {truncateMiddle(row.execution_id, 22)}
              </div>
              <div className="min-w-0 font-mono text-[10px] text-[var(--text-muted)]" title={row.session_id}>
                {truncateMiddle(row.session_id, 18)}
              </div>
              <div>
                <StatusBadge status={row.delivery_status} map={deliveryMap} />
              </div>
              <div>
                <StatusBadge status={row.runtime_status} map={runtimeMap} />
                {row.runtime_error_code && (
                  <div className="mt-0.5 truncate font-mono text-[9px] text-[var(--text-faint)]" title={row.runtime_error_code}>
                    {row.runtime_error_code}
                  </div>
                )}
              </div>
              <div className="min-w-0 truncate font-mono text-[10px] text-[var(--text-muted)]" title={row.worker_run_id}>
                {row.worker_run_id ? truncateMiddle(row.worker_run_id, 16) : '—'}
              </div>
              <div className="text-[10px] font-bold text-[var(--accent-amber)]">
                {row.fence_reason ? t('executions.fenced_badge') : '—'}
              </div>
            </Link>
          ))}
        </div>
      )}

      {/* Pagination — keyset, so this walks backwards through history instead of
          jumping to a page number that new runs would invalidate. */}
      {!loading && !error && view.rows.length > 0 && (
        <div className="mt-4 flex items-center justify-between rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/40 px-4 py-2 text-[11px] text-[var(--text-secondary)]">
          <span className="text-[var(--text-muted)]">
            {t('executions.showing', { count: view.rows.length })}
          </span>
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => void reload()}
              disabled={loadingMore}
              className="rounded border border-[var(--border-subtle)] bg-[var(--bg-surface)] px-2.5 py-1 text-[10px] font-bold uppercase tracking-wider text-[var(--text-secondary)] transition-colors hover:bg-[var(--bg-hover)] disabled:opacity-40"
            >
              {t('executions.refresh')}
            </button>
            {view.cursor && (
              <button
                type="button"
                onClick={() => void loadOlder()}
                disabled={loadingMore}
                className="rounded bg-[var(--bg-surface)] px-2.5 py-1 text-[10px] font-bold uppercase tracking-wider text-[var(--text-secondary)] transition-colors hover:bg-[var(--bg-hover)] disabled:opacity-40 disabled:cursor-not-allowed"
              >
                {loadingMore ? t('executions.loading_more') : t('executions.load_older')}
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
