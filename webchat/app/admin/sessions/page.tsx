'use client';

import { useCallback, useEffect, useMemo, useState } from 'react';
import Link from 'next/link';
import { listSessions, terminateSession, deleteSession, getSessionDetail, getSessionStats, getSessionDebug } from '@/lib/api/admin-sessions';
import { listActivity } from '@/lib/api/admin-activity';
import { MetricCard } from '@/components/admin/metric-card';
import { SessionStatusBadge } from '@/components/admin/session-status-badge';
import { useAdminUI } from '@/context/admin-ui-context';
import type {
  AdminSessionInfo,
  AdminSessionDetailResponse,
  SessionStatsResponse,
  SessionDebugInfo,
  AuditActivity,
  AuditIdentityLink,
} from '@/lib/types/admin';
import { formatRelative as formatTime, formatDateTime } from '@/lib/utils/format-time';
import { useTranslation } from 'react-i18next';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type SessionState = AdminSessionInfo['state'];
type FilterOption = 'all' | SessionState;
type SortOption = 'last_active' | 'created';
type DrawerTab = 'overview' | 'stats' | 'context' | 'activity';

function truncateId(id: string): string {
  if (id.length <= 12) return id;
  return `${id.slice(0, 8)}...${id.slice(-4)}`;
}

// ---------------------------------------------------------------------------
// Page Component
// ---------------------------------------------------------------------------

export default function SessionsPage() {
  const { t } = useTranslation();
  const { showToast, confirm } = useAdminUI();
  const [sessions, setSessions] = useState<AdminSessionInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState<FilterOption>('all');
  const [sort, setSort] = useState<SortOption>('last_active');
  const [query, setQuery] = useState('');
  const [actionLoading, setActionLoading] = useState<string | null>(null);
  const [confirmId, setConfirmId] = useState<string | null>(null);
  const [drawerSession, setDrawerSession] = useState<AdminSessionInfo | null>(null);
  const [drawerDetail, setDrawerDetail] = useState<AdminSessionDetailResponse | null>(null);
  const [drawerStats, setDrawerStats] = useState<SessionStatsResponse | null>(null);
  const [drawerDebug, setDrawerDebug] = useState<SessionDebugInfo | null>(null);
  const [drawerActivities, setDrawerActivities] = useState<AuditActivity[]>([]);
  const [drawerLoading, setDrawerLoading] = useState(false);
  const [drawerTab, setDrawerTab] = useState<DrawerTab>('overview');
  const [copyIdFeedback, setCopyIdFeedback] = useState<string | null>(null);
  const [identityLinks, setIdentityLinks] = useState<Record<string, AuditIdentityLink>>({});

  const loadDrawerData = useCallback(async (sessionId: string) => {
    setDrawerLoading(true);
    try {
      const [detailRes, statsRes, debugRes, actRes] = await Promise.allSettled([
        getSessionDetail(sessionId),
        getSessionStats(sessionId),
        getSessionDebug(sessionId),
        listActivity({ sessionId, limit: 15 }),
      ]);

      if (detailRes.status === 'fulfilled') setDrawerDetail(detailRes.value);
      else setDrawerDetail(null);

      if (statsRes.status === 'fulfilled') setDrawerStats(statsRes.value);
      else setDrawerStats(null);

      if (debugRes.status === 'fulfilled') setDrawerDebug(debugRes.value.debug);
      else setDrawerDebug(null);

      if (actRes.status === 'fulfilled') setDrawerActivities(actRes.value.rows ?? []);
      else setDrawerActivities([]);
    } finally {
      setDrawerLoading(false);
    }
  }, []);

  const openDrawer = useCallback((session: AdminSessionInfo) => {
    setDrawerSession(session);
    setDrawerTab('overview');
    loadDrawerData(session.id);
  }, [loadDrawerData]);

  const closeDrawer = useCallback(() => {
    setDrawerSession(null);
    setDrawerDetail(null);
    setDrawerStats(null);
    setDrawerDebug(null);
    setDrawerActivities([]);
  }, []);

  const loadSessions = useCallback(async () => {
    try {
      setLoading(true);
      setError(null);
      setConfirmId(null);
      const data = await listSessions(100, 0);
      setSessions(data.sessions);
      setIdentityLinks(data.identity_links ?? {});

      // If drawer is open, update its session details in case of state changes
      if (drawerSession) {
        const updated = data.sessions.find((s) => s.id === drawerSession.id);
        if (updated) {
          setDrawerSession(updated);
        } else {
          closeDrawer(); // Session was deleted
        }
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : t('admin:sessions.error.load_failed', { defaultValue: 'Failed to load sessions' }));
    } finally {
      setLoading(false);
    }
  }, [drawerSession, t, closeDrawer]);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- mount-time fetch
    loadSessions();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // ---------------------------------------------------------------------------
  // Derived Stats
  // ---------------------------------------------------------------------------

  const stats = useMemo(() => {
    const total = sessions.length;
    const active = sessions.filter((s) => s.state === 'running' || s.state === 'created').length;
    const idle = sessions.filter((s) => s.state === 'idle').length;
    const terminated = sessions.filter((s) => s.state === 'terminated').length;
    return { total, active, idle, terminated };
  }, [sessions]);

  // ---------------------------------------------------------------------------
  // Filtering & Sorting
  // ---------------------------------------------------------------------------

  const filtered = useMemo(() => {
    let result = sessions;
    if (filter !== 'all') {
      result = result.filter((s) => s.state === filter);
    }
    if (query.trim()) {
      const q = query.toLowerCase();
      result = result.filter(
        (s) =>
          s.id.toLowerCase().includes(q) ||
          s.user_id?.toLowerCase().includes(q) ||
          s.worker_type?.toLowerCase().includes(q) ||
          s.title?.toLowerCase().includes(q)
      );
    }
    return result;
  }, [sessions, filter, query]);

  const sorted = useMemo(
    () =>
      [...filtered].sort((a, b) => {
        if (sort === 'created') {
          return new Date(b.created_at).getTime() - new Date(a.created_at).getTime();
        }
        return new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime();
      }),
    [filtered, sort]
  );

  // ---------------------------------------------------------------------------
  // Actions
  // ---------------------------------------------------------------------------

  const handleCopyId = async (e: React.MouseEvent, id: string) => {
    e.stopPropagation();
    e.preventDefault();
    try {
      await navigator.clipboard.writeText(id);
      setCopyIdFeedback(id);
      showToast(t('admin:sessions.toast.copied', { defaultValue: 'Copied Session ID to clipboard' }), 'success');
      setTimeout(() => setCopyIdFeedback(null), 2000);
    } catch {
      showToast(t('admin:sessions.toast.copy_failed', { defaultValue: 'Failed to copy ID' }), 'error');
    }
  };

  const handleTerminate = async (id: string, fromDrawer = false) => {
    const confirmed = await confirm(
      t('admin:sessions.confirm.terminate_title', { defaultValue: 'Terminate Session?' }),
      t('admin:sessions.confirm.terminate_body', { id: truncateId(id), defaultValue: `Are you sure you want to terminate session "${truncateId(id)}"? The running worker process will be stopped immediately.` }),
      { confirmLabel: t('admin:sessions.action.terminate', { defaultValue: 'Terminate' }), destructive: true }
    );
    if (!confirmed) return;
    try {
      setActionLoading(id);
      await terminateSession(id);

      setSessions((prev) =>
        prev.map((s) => (s.id === id ? { ...s, state: 'terminated' } : s))
      );

      if (fromDrawer && drawerSession && drawerSession.id === id) {
        setDrawerSession((prev) => (prev ? { ...prev, state: 'terminated' } : null));
      }

      showToast(t('admin:sessions.toast.terminated', { id: truncateId(id), defaultValue: `Session "${truncateId(id)}" successfully terminated.` }), 'success');
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('admin:sessions.toast.terminate_failed', { defaultValue: 'Failed to terminate session' }), 'error');
    } finally {
      setActionLoading(null);
    }
  };

  const handleDelete = async (id: string, fromDrawer = false) => {
    const confirmed = await confirm(
      t('admin:sessions.confirm.delete_title', { defaultValue: 'Delete Session?' }),
      t('admin:sessions.confirm.delete_body', { id: truncateId(id), defaultValue: `Are you sure you want to permanently delete session "${truncateId(id)}"? All database traces will be deleted. This action is irreversible.` }),
      { confirmLabel: t('admin:sessions.action.delete', { defaultValue: 'Delete' }), destructive: true }
    );
    if (!confirmed) return;

    try {
      setActionLoading(id);
      setConfirmId(null);
      await deleteSession(id);
      setSessions((prev) => prev.filter((s) => s.id !== id));

      if (fromDrawer || (drawerSession && drawerSession.id === id)) {
        closeDrawer();
      }

      showToast(t('admin:sessions.toast.deleted', { id: truncateId(id), defaultValue: `Session "${truncateId(id)}" successfully deleted.` }), 'success');
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('admin:sessions.toast.delete_failed', { defaultValue: 'Failed to delete session' }), 'error');
    } finally {
      setActionLoading(null);
    }
  };

  // ---------------------------------------------------------------------------
  // Grid layout structure
  // ---------------------------------------------------------------------------
  const gridCols = 'grid-cols-[1.5fr_1fr_1fr_120px_100px_100px_100px]';

  // Keyboard navigation for drawer (close on Esc)
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        closeDrawer();
      }
    };
    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, [closeDrawer]);

  return (
    <div className="relative min-h-screen bg-[var(--bg-base)] px-6 py-8">
      {/* Background ambient gradient glow */}
      <div className="pointer-events-none fixed inset-0 z-0 bg-mesh opacity-30" />
      <div className="pointer-events-none fixed inset-0 z-0 noise-overlay" />

      <div className="relative z-10 max-w-6xl mx-auto">
        {/* Header */}
        <div className="flex flex-col md:flex-row md:items-center justify-between gap-4 mb-8">
          <div>
            <h1 className="text-2xl font-display font-bold tracking-tight text-[var(--text-primary)]">
              {t('admin:sessions.title', { defaultValue: 'Sessions' })}
            </h1>
            <p className="text-xs text-[var(--text-muted)] mt-1">
              {t('admin:sessions.subtitle', { defaultValue: 'Monitor, audit, and manage real-time active MOSS/Claude Code workers and sessions.' })}
            </p>
          </div>

          <button
            type="button"
            onClick={loadSessions}
            disabled={loading}
            className="self-start md:self-auto inline-flex items-center gap-1.5 px-3.5 py-1.5 rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-surface)] text-[10px] font-bold uppercase tracking-wider text-[var(--text-muted)] hover:bg-[var(--bg-hover)] hover:text-[var(--text-primary)] transition-all active:scale-95 disabled:opacity-40 shadow-[var(--shadow-sm)]"
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              fill="none"
              viewBox="0 0 24 24"
              strokeWidth={2}
              stroke="currentColor"
              className={`h-3.5 w-3.5 ${loading ? 'animate-spin text-[var(--accent-gold)]' : ''}`}
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.992 0 3.181 3.183a8.25 8.25 0 0 0 13.803-3.7M4.031 9.865a8.25 8.25 0 0 1 13.803-3.7l3.181 3.182"
              />
            </svg>
            {t('admin:sessions.action.refresh', { defaultValue: 'Refresh List' })}
          </button>
        </div>

        {/* Stats Grid */}
        <div className="grid grid-cols-2 lg:grid-cols-4 gap-4 mb-8">
          {/* Card 1: Total */}
          <MetricCard
            label={t('admin:sessions.metrics.total', { defaultValue: 'Total Sessions' })}
            value={stats.total}
            suffix={t('admin:sessions.metrics.total_suffix', { defaultValue: 'sessions' })}
            sub={t('admin:sessions.metrics.total_sub', { defaultValue: 'Lifetime execution tracks' })}
            icon={<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className="w-12 h-12 text-[var(--text-primary)]"><path strokeLinecap="round" strokeLinejoin="round" d="M9 17.25v1.007a3 3 0 0 1-.879 2.122L7.5 21h9l-.621-.621A3 3 0 0 1 15 18.257V17.25m6-12V15a2.25 2.25 0 0 1-2.25 2.25H5.25A2.25 2.25 0 0 1 3 15V5.25m18 0A2.25 2.25 0 0 0 18.75 3H5.25A2.25 2.25 0 0 0 3 5.25m18 0V12a2.25 2.25 0 0 1-2.25 2.25H5.25A2.25 2.25 0 0 1 3 12V5.25" /></svg>}
          />

          {/* Card 2: Active */}
          <MetricCard
            label={t('admin:sessions.metrics.active', { defaultValue: 'Active Engines' })}
            value={stats.active}
            suffix={t('admin:sessions.metrics.active_suffix', { defaultValue: 'running' })}
            sub={t('admin:sessions.metrics.active_sub', { defaultValue: 'Consuming worker resources' })}
            accent="emerald"
            pulse
            icon={<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className="w-12 h-12 text-[var(--accent-emerald)]"><path strokeLinecap="round" strokeLinejoin="round" d="M3.75 13.5l10.5-11.25L12 10.5h8.25L9.75 21.75 12 13.5H3.75z" /></svg>}
          />

          {/* Card 3: Idle */}
          <MetricCard
            label={t('admin:sessions.metrics.idle', { defaultValue: 'Idle Workers' })}
            value={stats.idle}
            suffix={t('admin:sessions.metrics.idle_suffix', { defaultValue: 'waiting' })}
            sub={t('admin:sessions.metrics.idle_sub', { defaultValue: 'Suspended, waiting for input' })}
            accent="amber"
            icon={<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className="w-12 h-12 text-[var(--accent-amber)]"><path strokeLinecap="round" strokeLinejoin="round" d="M12 6v6h4.5m4.5 0a9 9 0 1 1-18 0 9 9 0 0 1 18 0z" /></svg>}
          />

          {/* Card 4: Terminated */}
          <MetricCard
            label={t('admin:sessions.metrics.terminated', { defaultValue: 'Terminated' })}
            value={stats.terminated}
            suffix={t('admin:sessions.metrics.terminated_suffix', { defaultValue: 'completed' })}
            sub={t('admin:sessions.metrics.terminated_sub', { defaultValue: 'Safely released & exited' })}
            accent="muted"
            icon={<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className="w-12 h-12 text-[var(--text-muted)]"><path strokeLinecap="round" strokeLinejoin="round" d="M5.636 5.636a9 9 0 1 0 12.728 0M12 3v9" /></svg>}
          />
        </div>

        {/* Toolbar Controls */}
        <div className="flex flex-col lg:flex-row items-stretch lg:items-center justify-between gap-4 mb-6 bg-[var(--bg-glass)] border border-[var(--border-subtle)] rounded-[var(--radius-md)] p-3 backdrop-blur-md shadow-[var(--shadow-sm)]">
          {/* Segmented Tabs Status Filter */}
          <div className="flex flex-wrap items-center gap-1 bg-[var(--bg-base)] border border-[var(--border-subtle)] p-1 rounded-[var(--radius-sm)]">
            <button
              type="button"
              onClick={() => setFilter('all')}
              className={`px-3 py-1 text-[11px] font-semibold rounded-[var(--radius-xs)] transition-all ${
                filter === 'all'
                  ? 'bg-[var(--bg-elevated)] text-[var(--accent-gold)] shadow-[var(--shadow-sm)]'
                  : 'text-[var(--text-muted)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)]'
              }`}
            >
              {t('admin:sessions.filter.all', { defaultValue: 'All' })} <span className="opacity-60 ml-0.5">({stats.total})</span>
            </button>
            <button
              type="button"
              onClick={() => setFilter('running')}
              className={`px-3 py-1 text-[11px] font-semibold rounded-[var(--radius-xs)] transition-all ${
                filter === 'running'
                  ? 'bg-[var(--bg-elevated)] text-[var(--accent-emerald)] shadow-[var(--shadow-sm)]'
                  : 'text-[var(--text-muted)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)]'
              }`}
            >
              {t('admin:sessions.filter.running', { defaultValue: 'Running' })} <span className="opacity-60 ml-0.5">({sessions.filter((s) => s.state === 'running').length})</span>
            </button>
            <button
              type="button"
              onClick={() => setFilter('created')}
              className={`px-3 py-1 text-[11px] font-semibold rounded-[var(--radius-xs)] transition-all ${
                filter === 'created'
                  ? 'bg-[var(--bg-elevated)] text-[var(--accent-blue)] shadow-[var(--shadow-sm)]'
                  : 'text-[var(--text-muted)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)]'
              }`}
            >
              {t('admin:sessions.filter.created', { defaultValue: 'Created' })} <span className="opacity-60 ml-0.5">({sessions.filter((s) => s.state === 'created').length})</span>
            </button>
            <button
              type="button"
              onClick={() => setFilter('idle')}
              className={`px-3 py-1 text-[11px] font-semibold rounded-[var(--radius-xs)] transition-all ${
                filter === 'idle'
                  ? 'bg-[var(--bg-elevated)] text-[var(--accent-amber)] shadow-[var(--shadow-sm)]'
                  : 'text-[var(--text-muted)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)]'
              }`}
            >
              {t('admin:sessions.filter.idle', { defaultValue: 'Idle' })} <span className="opacity-60 ml-0.5">({stats.idle})</span>
            </button>
            <button
              type="button"
              onClick={() => setFilter('terminated')}
              className={`px-3 py-1 text-[11px] font-semibold rounded-[var(--radius-xs)] transition-all ${
                filter === 'terminated'
                  ? 'bg-[var(--bg-elevated)] text-[var(--text-muted)] shadow-[var(--shadow-sm)]'
                  : 'text-[var(--text-muted)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)]'
              }`}
            >
              {t('admin:sessions.filter.terminated', { defaultValue: 'Terminated' })} <span className="opacity-60 ml-0.5">({stats.terminated})</span>
            </button>
          </div>

          <div className="flex flex-col sm:flex-row items-center gap-3 shrink-0">
            {/* Search Input */}
            <div className="relative w-full sm:w-56">
              <svg
                xmlns="http://www.w3.org/2000/svg"
                fill="none"
                viewBox="0 0 24 24"
                strokeWidth={2}
                stroke="currentColor"
                className="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-[var(--text-faint)]"
              >
                <path
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  d="m21 21-5.197-5.197m0 0A7.5 7.5 0 1 0 5.196 5.196a7.5 7.5 0 0 0 10.607 10.607Z"
                />
              </svg>
              <input
                type="text"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder={t('admin:sessions.placeholder.search', { defaultValue: 'Search session details...' })}
                className="w-full pl-8 pr-7 py-1.5 rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-base)] text-xs text-[var(--text-primary)] placeholder:text-[var(--text-faint)] outline-none transition-all focus:border-[var(--accent-gold)]/40"
              />
              {query && (
                <button
                  type="button"
                  onClick={() => setQuery('')}
                  className="absolute right-2 top-1/2 -translate-y-1/2 text-[var(--text-faint)] hover:text-[var(--text-primary)] transition-colors p-0.5"
                >
                  <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" className="w-3.5 h-3.5">
                    <path d="M6.28 5.22a.75.75 0 0 0-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 1 0 1.06 1.06L10 11.06l3.72 3.72a.75.75 0 1 0 1.06-1.06L11.06 10l3.72-3.72a.75.75 0 0 0-1.06-1.06L10 8.94 6.28 5.22Z" />
                  </svg>
                </button>
              )}
            </div>

            {/* Sort Dropdown */}
            <div className="relative w-full sm:w-auto shrink-0">
              <select
                value={sort}
                onChange={(e) => setSort(e.target.value as SortOption)}
                className="w-full sm:w-auto rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-base)] pl-3 pr-8 py-1.5 text-xs text-[var(--text-primary)] outline-none transition-all focus:border-[var(--accent-gold)]/40 appearance-none"
              >
                <option value="last_active">{t('admin:sessions.sort.last_active', { defaultValue: 'Last Active' })}</option>
                <option value="created">{t('admin:sessions.sort.created', { defaultValue: 'Created Date' })}</option>
              </select>
              <div className="pointer-events-none absolute right-2.5 top-1/2 -translate-y-1/2 text-[var(--text-faint)]">
                <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor" className="w-3 h-3">
                  <path strokeLinecap="round" strokeLinejoin="round" d="M19.5 8.25l-7.5 7.5-7.5-7.5" />
                </svg>
              </div>
            </div>
          </div>
        </div>

        {/* Loading Spinner */}
        {loading && sessions.length === 0 && (
          <div className="flex items-center justify-center py-32 bg-[var(--bg-surface)] border border-[var(--border-subtle)] rounded-[var(--radius-md)]">
            <div className="flex flex-col items-center gap-3">
              <div className="w-8 h-8 border-2 border-[var(--accent-gold)] border-t-transparent rounded-full animate-spin" />
              <span className="text-xs font-medium text-[var(--text-muted)] animate-pulse">{t('admin:sessions.loading', { defaultValue: 'Loading execution registry...' })}</span>
            </div>
          </div>
        )}

        {/* Error Callout */}
        {error && (
          <div className="rounded-[var(--radius-md)] bg-[rgba(244,63,94,0.08)] border border-[rgba(244,63,94,0.15)] p-4 shadow-[var(--shadow-sm)] mb-6">
            <div className="flex items-center justify-between">
              <div className="flex gap-2">
                <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="var(--accent-coral)" className="w-4 h-4 mt-0.5">
                  <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v3.75m9-.75a9 9 0 1 1-18 0 9 9 0 0 1 18 0Zm-9 3.75h.008v.008H12v-.008Z" />
                </svg>
                <p className="text-xs font-semibold text-[var(--accent-coral)]">{error}</p>
              </div>
              <button
                type="button"
                onClick={loadSessions}
                className="text-xs font-bold text-[var(--accent-coral)] underline underline-offset-4 hover:text-[var(--accent-coral)]/80 transition-colors"
              >
                {t('common:action.retry', { defaultValue: 'Retry Fetch' })}
              </button>
            </div>
          </div>
        )}

        {/* Empty State */}
        {!loading && !error && sorted.length === 0 && (
          <div className="flex flex-col items-center justify-center py-24 text-center bg-[var(--bg-surface)] border border-[var(--border-subtle)] rounded-[var(--radius-md)]">
            <svg
              xmlns="http://www.w3.org/2000/svg"
              fill="none"
              viewBox="0 0 24 24"
              strokeWidth={1.2}
              stroke="currentColor"
              className="h-12 w-12 text-[var(--text-faint)] mb-4 animate-float"
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M20.25 8.511c.884.284 1.5 1.128 1.5 2.097v4.286c0 1.136-.847 2.1-1.98 2.193-.34.027-.68.052-1.02.072v3.091l-3-3c-1.354 0-2.694-.055-4.02-.163a2.115 2.115 0 0 1-.825-.242m9.345-8.334a2.126 2.126 0 0 0-.476-.095 48.64 48.64 0 0 0-8.048 0c-1.131.094-1.976 1.057-1.976 2.192v4.286c0 .837.46 1.58 1.155 1.951m9.345-8.334V6.637c0-1.621-1.152-3.026-2.76-3.235A48.455 48.455 0 0 0 11.25 3c-2.115 0-4.198.137-6.24.402-1.608.209-2.76 1.614-2.76 3.235v6.226c0 1.621 1.152 3.026 2.76 3.235.577.075 1.157.14 1.74.194V21l4.155-4.155"
              />
            </svg>
            <p className="text-sm font-semibold text-[var(--text-muted)]">
              {filter !== 'all' || query.trim()
                ? t('admin:sessions.empty.no_match', { defaultValue: 'No matching execution channels found' })
                : t('admin:sessions.empty.no_sessions', { defaultValue: 'No session registry exists' })}
            </p>
            <p className="text-xs text-[var(--text-faint)] mt-1.5 max-w-xs leading-relaxed">
              {t('admin:sessions.empty.hint', { defaultValue: 'Try adjusting your search criteria, selecting another filter tab, or refreshing.' })}
            </p>
            {(filter !== 'all' || query.trim()) && (
              <button
                type="button"
                onClick={() => {
                  setFilter('all');
                  setQuery('');
                }}
                className="mt-4 px-3 py-1.5 rounded-[var(--radius-xs)] border border-[var(--border-subtle)] text-[10px] font-bold uppercase tracking-wider text-[var(--text-primary)] hover:bg-[var(--bg-hover)] transition-all"
              >
                {t('admin:sessions.action.reset_filter', { defaultValue: 'Reset Filter Parameters' })}
              </button>
            )}
          </div>
        )}

        {/* Interactive Grid Table */}
        {!loading && !error && sorted.length > 0 && (
          <div className="rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)] overflow-hidden animate-[fadeInScale_0.15s_ease-out]">
            {/* Grid Header */}
            <div
              className={`grid ${gridCols} gap-3 px-5 py-3.5 border-b border-[var(--border-subtle)] bg-[var(--bg-surface)] items-center`}
            >
              <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-widest font-mono">
                {t('admin:sessions.table.id_title', { defaultValue: 'ID / Title' })}
              </span>
              <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-widest font-mono">
                {t('admin:sessions.table.engine_type', { defaultValue: 'Engine type' })}
              </span>
              <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-widest font-mono">
                {t('admin:sessions.table.user', { defaultValue: 'Executing user' })}
              </span>
              <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-widest font-mono">
                {t('admin:sessions.table.status', { defaultValue: 'Status' })}
              </span>
              <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-widest font-mono">
                {t('admin:sessions.table.started', { defaultValue: 'Started' })}
              </span>
              <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-widest font-mono">
                {t('admin:sessions.table.active_time', { defaultValue: 'Active time' })}
              </span>
              <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-widest font-mono text-right">
                {t('admin:sessions.table.actions', { defaultValue: 'Actions' })}
              </span>
            </div>

            {/* Grid Rows */}
            <div className="divide-y divide-[var(--border-subtle)]">
              {sorted.map((session) => {
                const isSelected = drawerSession?.id === session.id;
                return (
                  <div
                    key={session.id}
                    onClick={() => openDrawer(session)}
                    className={`grid ${gridCols} gap-3 px-5 py-3.5 transition-all items-center cursor-pointer select-none hover:bg-[var(--bg-hover)] ${
                      isSelected
                        ? 'bg-[var(--bg-active)] border-l-2 border-l-[var(--accent-gold)] pl-[18px]'
                        : 'border-l-2 border-l-transparent'
                    }`}
                  >
                    {/* ID / Title */}
                    <div className="flex flex-col min-w-0 pr-2">
                      <div className="flex items-center gap-1.5 min-w-0">
                        <span
                          className="text-xs font-semibold text-[var(--text-primary)] truncate"
                          title={session.title || t('admin:sessions.detail.untitled', { defaultValue: 'Untitled Session' })}
                        >
                          {session.title || t('admin:sessions.detail.untitled', { defaultValue: 'Untitled Session' })}
                        </span>
                      </div>
                      <div className="flex items-center gap-1 mt-1 group">
                        <span className="text-[10px] font-mono text-[var(--accent-gold)] shrink-0 select-all">
                          {truncateId(session.id)}
                        </span>
                        <button
                          type="button"
                          onClick={(e) => handleCopyId(e, session.id)}
                          className="text-[var(--text-faint)] hover:text-[var(--accent-gold)] p-0.5 rounded-[var(--radius-sm)] transition-all opacity-0 group-hover:opacity-100 focus:opacity-100"
                          title={t('admin:sessions.action.copy_id', { defaultValue: 'Copy session ID' })}
                        >
                          {copyIdFeedback === session.id ? (
                            <svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="var(--accent-emerald)" className="w-3 h-3">
                              <path fillRule="evenodd" d="M16.704 4.153a.75.75 0 0 1 .143 1.052l-8 10.5a.75.75 0 0 1-1.127.075l-4.5-4.5a.75.75 0 0 1 1.06-1.06l3.894 3.893 7.48-9.817a.75.75 0 0 1 1.05-.143Z" clipRule="evenodd" />
                            </svg>
                          ) : (
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor" className="w-3 h-3">
                              <path strokeLinecap="round" strokeLinejoin="round" d="M8.25 7.5V6.108c0-1.135.845-2.098 1.976-2.192.373-.03.748-.057 1.123-.08M15.75 18H18a2.25 2.25 0 0 0 2.25-2.25V6.108c0-1.135-.845-2.098-1.976-2.192a48.424 48.424 0 0 0-1.123-.08M15.75 18.75v-1.875a3.375 3.375 0 0 0-3.375-3.375h-1.5a1.125 1.125 0 0 1-1.125-1.125v-1.5A3.375 3.375 0 0 0 6.375 7.5H5.25m11.9-3.664A2.251 2.251 0 0 0 15 2.25h-1.5a2.251 2.251 0 0 0-2.15 1.586m5.8 0c.065.21.1.433.1.664v.75h-6V4.5c0-.231.035-.454.1-.664M6.75 7.5H4.875c-.621 0-1.125.504-1.125 1.125v12c0 .621.504 1.125 1.125 1.125h9.75c.621 0 1.125-.504 1.125-1.125V16.5a9 9 0 0 0-9-9Z" />
                            </svg>
                          )}
                        </button>
                      </div>
                    </div>

                    {/* Worker */}
                    <div className="flex items-center gap-1.5 truncate">
                      <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className="w-3.5 h-3.5 text-[var(--text-faint)]">
                        <path strokeLinecap="round" strokeLinejoin="round" d="M9 17.25v1.007a3 3 0 0 1-.879 2.122L7.5 21h9l-.621-.621A3 3 0 0 1 15 18.257V17.25m6-12V15a2.25 2.25 0 0 1-2.25 2.25H5.25A2.25 2.25 0 0 1 3 15V5.25m18 0A2.25 2.25 0 0 0 18.75 3H5.25A2.25 2.25 0 0 0 3 5.25m18 0V12a2.25 2.25 0 0 1-2.25 2.25H5.25A2.25 2.25 0 0 1 3 12V5.25" />
                      </svg>
                      <span className="text-xs font-mono text-[var(--text-muted)] truncate" title={session.worker_type}>
                        {session.worker_type || '--'}
                      </span>
                    </div>

                    {/* User */}
                    <div className="flex items-center gap-1.5 min-w-0">
                      <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className="w-3.5 h-3.5 text-[var(--text-faint)] shrink-0">
                        <path strokeLinecap="round" strokeLinejoin="round" d="M15.75 6a3.75 3.75 0 1 1-7.5 0 3.75 3.75 0 0 1 7.5 0ZM4.501 20.118a7.5 7.5 0 0 1 14.998 0A17.933 17.933 0 0 1 12 21.75c-2.676 0-5.216-.584-7.499-1.632Z" />
                      </svg>
                      <div className="min-w-0 flex flex-col">
                        {(() => {
                          const link = identityLinks[session.user_id];
                          const name = link?.display_name || link?.DisplayName;
                          if (name) {
                            return (
                              <>
                                <span className="text-xs text-[var(--text-primary)] truncate font-medium" title={name}>
                                  {name}
                                </span>
                                <span className="text-[10px] font-mono text-[var(--text-faint)] truncate" title={session.user_id}>
                                  {truncateId(session.user_id)}
                                </span>
                              </>
                            );
                          }
                          return (
                            <span className="text-xs text-[var(--text-muted)] truncate font-medium" title={session.user_id}>
                              {session.user_id ? truncateId(session.user_id) : '--'}
                            </span>
                          );
                        })()}
                      </div>
                    </div>

                    {/* Status */}
                    <div onClick={(e) => e.stopPropagation()}>
                      <SessionStatusBadge state={session.state} />
                    </div>

                    {/* Created */}
                    <span className="text-xs text-[var(--text-muted)]" title={formatDateTime(session.created_at)}>
                      {formatTime(session.created_at)}
                    </span>

                    {/* Last active */}
                    <span className="text-xs text-[var(--text-muted)]" title={formatDateTime(session.updated_at)}>
                      {formatTime(session.updated_at)}
                    </span>

                    {/* Actions */}
                    <div
                      className="flex items-center justify-end gap-1.5"
                      onClick={(e) => e.stopPropagation()}
                    >
                      {confirmId === session.id ? (
                        <div className="flex items-center gap-1 animate-[fadeInScale_0.12s_ease-out]">
                          <button
                            type="button"
                            onClick={() => handleDelete(session.id)}
                            disabled={actionLoading === session.id}
                            className="px-2.5 py-1 rounded-[var(--radius-xs)] text-[9px] font-extrabold uppercase tracking-wide text-[var(--accent-coral)] bg-[rgba(244,63,94,0.12)] hover:bg-[rgba(244,63,94,0.22)] transition-colors disabled:opacity-40"
                          >
                            {actionLoading === session.id ? (
                              <span className="inline-block w-2.5 h-2.5 border border-current border-t-transparent rounded-full animate-spin" />
                            ) : (
                              t('common:action.delete', { defaultValue: 'Delete' })
                            )}
                          </button>
                          <button
                            type="button"
                            onClick={() => setConfirmId(null)}
                            disabled={actionLoading === session.id}
                            className="px-2 py-1 rounded-[var(--radius-xs)] text-[9px] font-bold text-[var(--text-faint)] hover:text-[var(--text-secondary)] hover:bg-[var(--bg-hover)] transition-colors disabled:opacity-40"
                          >
                            {t('common:action.cancel', { defaultValue: 'Cancel' })}
                          </button>
                        </div>
                      ) : (
                        <>
                          {session.state !== 'terminated' && (
                            <button
                              type="button"
                              onClick={() => handleTerminate(session.id)}
                              disabled={actionLoading === session.id}
                              className="p-2 rounded-[var(--radius-sm)] text-[var(--accent-amber)] bg-[rgba(245,158,11,0.08)] border border-transparent hover:border-[rgba(245,158,11,0.2)] hover:bg-[rgba(245,158,11,0.15)] transition-all disabled:opacity-40 disabled:cursor-not-allowed active:scale-95"
                              title={t('admin:sessions.action.terminate_title_hint', { defaultValue: 'Terminate active execution worker' })}
                            >
                              {actionLoading === session.id ? (
                                <div className="w-3.5 h-3.5 border border-current border-t-transparent rounded-full animate-spin" />
                              ) : (
                                <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor" className="h-3.5 w-3.5">
                                  <path strokeLinecap="round" strokeLinejoin="round" d="M5.636 5.636a9 9 0 1 0 12.728 0M12 3v9" />
                                </svg>
                              )}
                            </button>
                          )}
                          <button
                            type="button"
                            onClick={() => setConfirmId(session.id)}
                            disabled={actionLoading === session.id}
                            className="p-2 rounded-[var(--radius-sm)] text-[var(--accent-coral)] bg-[rgba(244,63,94,0.06)] border border-transparent hover:border-[rgba(244,63,94,0.18)] hover:bg-[rgba(244,63,94,0.12)] transition-all disabled:opacity-40 disabled:cursor-not-allowed active:scale-95"
                            title={t('admin:sessions.action.delete_title_hint', { defaultValue: 'Delete session database entry' })}
                          >
                            <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.8} stroke="currentColor" className="h-3.5 w-3.5">
                              <path strokeLinecap="round" strokeLinejoin="round" d="m14.74 9-.346 9m-4.788 0L9.26 9m9.968-3.21c.342.052.682.107 1.022.166m-1.022-.165L18.16 19.673a2.25 2.25 0 0 1-2.244 2.077H8.084a2.25 2.25 0 0 1-2.244-2.077L4.772 5.79m14.456 0a48.108 48.108 0 0 0-3.478-.397m-12 .562c.34-.059.68-.114 1.022-.165m0 0a48.11 48.11 0 0 1 3.478-.397m7.5 0v-.916c0-1.18-.91-2.164-2.09-2.201a51.964 51.964 0 0 0-3.32 0c-1.18.037-2.09 1.022-2.09 2.201v.916m7.5 0a48.667 48.667 0 0 0-7.5 0" />
                            </svg>
                          </button>
                        </>
                      )}
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
        )}
      </div>

      {/* ====== DETAIL SLIDE-OUT DRAWER ====== */}
      {drawerSession && (
        <div
          className="fixed inset-0 z-50 overflow-hidden"
          role="dialog"
          aria-modal="true"
        >
          {/* Backdrop blur overlay */}
          <div
            className="absolute inset-0 bg-black/60 backdrop-blur-sm transition-opacity duration-300 animate-[fadeInUp_0.2s_ease-out]"
            onClick={() => closeDrawer()}
          />

          <div className="absolute inset-y-0 right-0 max-w-full flex">
            <div className="relative w-screen max-w-xl bg-[var(--bg-surface)] border-l border-[var(--border-subtle)] shadow-2xl flex flex-col justify-between transform transition-transform duration-300 translate-x-0 animate-[slideIn_0.22s_cubic-bezier(0.2,0.8,0.2,1)]">
              
              {/* Drawer Header */}
              <div className="px-6 pt-6 pb-0 border-b border-[var(--border-subtle)] bg-[var(--bg-surface)]">
                <div className="flex items-center justify-between mb-3">
                  <div className="flex items-center gap-2 min-w-0">
                    <span className="w-2 h-2 rounded-full bg-[var(--accent-gold)] shrink-0 animate-pulse" />
                    <h1 className="text-base font-display font-bold text-[var(--text-primary)] truncate">
                      {drawerSession.title || t('admin:sessions.detail.untitled', { defaultValue: 'Untitled Session' })}
                    </h1>
                    <SessionStatusBadge state={drawerSession.state} />
                  </div>
                  <div className="flex items-center gap-1.5 shrink-0">
                    <button
                      type="button"
                      onClick={() => loadDrawerData(drawerSession.id)}
                      disabled={drawerLoading}
                      className="p-1.5 rounded-full text-[var(--text-faint)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)] transition-all disabled:opacity-50"
                      title={t('admin:sessions.action.refresh', { defaultValue: 'Refresh' })}
                    >
                      <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className={`w-4 h-4 ${drawerLoading ? 'animate-spin' : ''}`}>
                        <path strokeLinecap="round" strokeLinejoin="round" d="M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0 3.181 3.183a8.25 8.25 0 0 0 13.803-3.7M4.031 9.865a8.25 8.25 0 0 1 13.803-3.7l3.181 3.182m0-4.991v4.99" />
                      </svg>
                    </button>
                    <button
                      type="button"
                      onClick={() => closeDrawer()}
                      className="p-1.5 rounded-full text-[var(--text-faint)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)] transition-all"
                      title={t('admin:sessions.drawer.close', { defaultValue: 'Close inspector' })}
                    >
                      <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor" className="w-5 h-5">
                        <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
                      </svg>
                    </button>
                  </div>
                </div>

                {/* Session ID Banner */}
                <div className="flex items-center justify-between gap-3 px-3 py-1.5 rounded-[var(--radius-sm)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] mb-4">
                  <code className="text-xs font-mono text-[var(--accent-gold)] truncate font-medium">
                    {drawerSession.id}
                  </code>
                  <button
                    type="button"
                    onClick={(e) => handleCopyId(e, drawerSession.id)}
                    className="shrink-0 text-[10px] font-bold uppercase text-[var(--accent-gold)] hover:underline"
                  >
                    {copyIdFeedback === drawerSession.id ? t('common:action.copied', { defaultValue: 'Copied' }) : t('common:action.copy', { defaultValue: 'Copy' })}
                  </button>
                </div>

                {/* Tabs */}
                <div className="flex items-center gap-4 border-b border-transparent text-xs font-medium">
                  {(['overview', 'stats', 'context', 'activity'] as DrawerTab[]).map((tab) => (
                    <button
                      key={tab}
                      type="button"
                      onClick={() => setDrawerTab(tab)}
                      className={`pb-2.5 px-1 border-b-2 transition-colors capitalize ${
                        drawerTab === tab
                          ? 'border-[var(--accent-gold)] text-[var(--accent-gold)] font-bold'
                          : 'border-transparent text-[var(--text-faint)] hover:text-[var(--text-primary)]'
                      }`}
                    >
                      {t(`admin:sessions.drawer.tab_${tab}`, { defaultValue: tab })}
                    </button>
                  ))}
                </div>
              </div>

              {/* Drawer Body */}
              <div className="flex-1 overflow-y-auto px-6 py-5 space-y-5">
                {/* OVERVIEW TAB */}
                {drawerTab === 'overview' && (
                  <div className="space-y-4">
                    {/* Key Stats Cards */}
                    <div className="grid grid-cols-2 gap-3">
                      <div className="px-3.5 py-2.5 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)]">
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-0.5">
                          {t('admin:sessions.drawer.turns', { defaultValue: 'Total Turns' })}
                        </span>
                        <span className="text-sm font-bold text-[var(--text-primary)]">
                          {drawerStats?.total_turns ?? drawerSession.turn_count ?? 0}
                        </span>
                        {drawerStats && (
                          <span className="text-[10px] text-[var(--text-faint)] block mt-0.5">
                            {drawerStats.success_turns} pass / {drawerStats.failed_turns} fail
                          </span>
                        )}
                      </div>

                      <div className="px-3.5 py-2.5 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)]">
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-0.5">
                          {t('admin:sessions.drawer.total_cost', { defaultValue: 'Est. Total Cost' })}
                        </span>
                        <span className="text-sm font-bold font-mono text-[var(--accent-emerald)]">
                          ${(drawerStats?.total_cost_usd ?? 0).toFixed(4)}
                        </span>
                      </div>

                      <div className="px-3.5 py-2.5 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)]">
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-0.5">
                          {t('admin:sessions.drawer.total_tokens', { defaultValue: 'Total Tokens' })}
                        </span>
                        <span className="text-sm font-bold font-mono text-[var(--text-primary)]">
                          {((drawerStats?.total_tokens_in ?? 0) + (drawerStats?.total_tokens_out ?? 0)).toLocaleString()}
                        </span>
                      </div>

                      <div className="px-3.5 py-2.5 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)]">
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-0.5">
                          {t('admin:sessions.drawer.live_worker', { defaultValue: 'Live Worker Process' })}
                        </span>
                        {drawerDebug?.has_worker ? (
                          <span className="inline-flex items-center gap-1.5 text-xs font-bold text-[var(--accent-emerald)]">
                            <span className="w-2 h-2 rounded-full bg-[var(--accent-emerald)] animate-pulse" />
                            {t('admin:sessions.drawer.worker_running', { defaultValue: 'Running' })}
                          </span>
                        ) : (
                          <span className="text-xs font-medium text-[var(--text-faint)]">
                            {t('admin:sessions.drawer.worker_stopped', { defaultValue: 'Stopped' })}
                          </span>
                        )}
                      </div>
                    </div>

                    {/* Metadata Breakdown */}
                    <div className="px-4 py-3 rounded-[var(--radius-md)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] space-y-3">
                      <div>
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-1">
                          {t('admin:sessions.drawer.engine_type', { defaultValue: 'Worker Engine Type' })}
                        </span>
                        <span className="text-xs font-mono font-bold text-[var(--text-primary)]">
                          {drawerSession.worker_type || '—'}
                        </span>
                      </div>

                      <div>
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-1">
                          {t('admin:sessions.drawer.user_id', { defaultValue: 'Executing User Identity' })}
                        </span>
                        {(() => {
                          const uid = drawerSession.user_id;
                          const link = identityLinks[uid] || drawerDetail?.identity_link;
                          const name = link?.display_name || link?.DisplayName;
                          let friendlyName = name;
                          if (!friendlyName) {
                            if (uid.startsWith('ou_') || drawerDetail?.platform === 'feishu') {
                              friendlyName = `飞书用户 (${uid.slice(0, 8)}...)`;
                            } else if (uid.startsWith('U') || uid.startsWith('W') || drawerDetail?.platform === 'slack') {
                              friendlyName = `Slack 用户 (${uid})`;
                            } else if (uid === 'anonymous' || uid.startsWith('anon_')) {
                              friendlyName = 'WebChat 匿名用户';
                            } else if (uid === 'cron' || uid.startsWith('cron_')) {
                              friendlyName = '系统 Cron 引擎';
                            } else {
                              friendlyName = `系统用户 (${uid})`;
                            }
                          }
                          return (
                            <div className="min-w-0 space-y-1">
                              <div className="text-xs font-bold text-[var(--text-primary)] truncate" title={friendlyName}>
                                {friendlyName}
                              </div>
                              <div className="flex items-center gap-2">
                                <code className="text-[10px] font-mono text-[var(--text-muted)] bg-white/5 px-1.5 py-0.5 rounded border border-[var(--border-subtle)] truncate max-w-[150px]">
                                  {uid}
                                </code>
                                <Link
                                  href={`/admin/activity?user_id=${encodeURIComponent(uid)}`}
                                  className="text-[10px] font-semibold text-[var(--accent-gold)] hover:underline shrink-0"
                                  title="Search audit logs for this user"
                                >
                                  {t('admin:sessions.detail.identity.view_user_activity', { defaultValue: 'Audit Logs →' })}
                                </Link>
                              </div>
                            </div>
                          );
                        })()}
                      </div>

                      {drawerDetail?.platform && (
                        <div>
                          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-1">
                            {t('admin:sessions.drawer.platform_info', { defaultValue: 'Platform & Channel' })}
                          </span>
                          <span className="text-xs font-bold capitalize text-[var(--text-primary)]">
                            {drawerDetail.platform}
                          </span>
                          {drawerDetail.platform_key && (
                            <div className="mt-1 flex flex-wrap gap-1.5">
                              {Object.entries(drawerDetail.platform_key).map(([k, v]) => (
                                <span key={k} className="inline-flex items-center px-2 py-0.5 rounded text-[10px] font-mono bg-[var(--bg-hover)] text-[var(--text-secondary)] border border-[var(--border-subtle)]">
                                  {k}: {v}
                                </span>
                              ))}
                            </div>
                          )}
                        </div>
                      )}

                      <div>
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-1">
                          {t('admin:sessions.drawer.work_dir', { defaultValue: 'Working Directory' })}
                        </span>
                        <span className="text-xs font-mono text-[var(--text-muted)] break-all select-all">
                          {drawerSession.work_dir || '—'}
                        </span>
                      </div>
                    </div>

                    {/* Timestamps */}
                    <div className="grid grid-cols-2 gap-3.5 px-4 py-3 rounded-[var(--radius-md)] bg-[var(--bg-surface)] border border-[var(--border-subtle)]">
                      <div>
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-1">
                          {t('admin:sessions.drawer.started_at', { defaultValue: 'Started At' })}
                        </span>
                        <span className="text-xs text-[var(--text-secondary)] font-medium">
                          {formatDateTime(drawerSession.created_at)}
                        </span>
                      </div>
                      <div>
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-1">
                          {t('admin:sessions.drawer.last_active', { defaultValue: 'Last Active At' })}
                        </span>
                        <span className="text-xs text-[var(--text-secondary)] font-medium">
                          {formatDateTime(drawerSession.updated_at)}
                        </span>
                      </div>
                    </div>
                  </div>
                )}

                {/* STATS & TOKENS TAB */}
                {drawerTab === 'stats' && (
                  <div className="space-y-4">
                    {drawerStats ? (
                      <>
                        <div className="p-4 rounded-[var(--radius-md)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] space-y-3">
                          <h3 className="text-xs font-bold text-[var(--text-faint)] uppercase tracking-wider">
                            Token Split Summary
                          </h3>
                          <div className="grid grid-cols-2 gap-3 text-xs">
                            <div className="p-2.5 rounded bg-[var(--bg-hover)]">
                              <span className="text-[10px] text-[var(--text-faint)] uppercase block">{t('admin:sessions.drawer.token_input', { defaultValue: 'Input' })}</span>
                              <span className="font-mono font-bold text-[var(--text-primary)]">{drawerStats.total_tokens_in.toLocaleString()}</span>
                            </div>
                            <div className="p-2.5 rounded bg-[var(--bg-hover)]">
                              <span className="text-[10px] text-[var(--text-faint)] uppercase block">{t('admin:sessions.drawer.token_output', { defaultValue: 'Output' })}</span>
                              <span className="font-mono font-bold text-[var(--accent-emerald)]">{drawerStats.total_tokens_out.toLocaleString()}</span>
                            </div>
                            <div className="p-2.5 rounded bg-[var(--bg-hover)]">
                              <span className="text-[10px] text-[var(--text-faint)] uppercase block">{t('admin:sessions.drawer.cache_read', { defaultValue: 'Cache Read' })}</span>
                              <span className="font-mono font-bold text-[var(--accent-cyan)]">{drawerStats.total_tokens_cache_read.toLocaleString()}</span>
                            </div>
                            <div className="p-2.5 rounded bg-[var(--bg-hover)]">
                              <span className="text-[10px] text-[var(--text-faint)] uppercase block">{t('admin:sessions.drawer.cache_write', { defaultValue: 'Cache Write' })}</span>
                              <span className="font-mono font-bold text-[var(--accent-purple)]">{drawerStats.total_tokens_cache_write.toLocaleString()}</span>
                            </div>
                          </div>
                        </div>

                        {/* Turn list */}
                        <div className="space-y-2">
                          <h3 className="text-xs font-bold text-[var(--text-faint)] uppercase tracking-wider">
                            Per-Turn Breakdown ({drawerStats.turns?.length ?? 0})
                          </h3>
                          {drawerStats.turns && drawerStats.turns.length > 0 ? (
                            <div className="space-y-2 max-h-[300px] overflow-y-auto pr-1">
                              {drawerStats.turns.map((turn) => (
                                <div key={turn.seq} className="p-3 rounded-[var(--radius-sm)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] flex items-center justify-between text-xs">
                                  <div>
                                    <div className="flex items-center gap-2">
                                      <span className="font-bold text-[var(--text-primary)] font-mono">Turn #{turn.turn_num}</span>
                                      <span className={`px-1.5 py-0.5 rounded text-[9px] font-bold ${turn.success ? 'bg-emerald-500/10 text-emerald-400' : 'bg-rose-500/10 text-rose-400'}`}>
                                        {turn.success ? 'PASS' : 'FAIL'}
                                      </span>
                                    </div>
                                    <p className="text-[10px] text-[var(--text-faint)] font-mono mt-0.5">
                                      {turn.model || 'llm'} • {turn.duration_ms}ms
                                    </p>
                                  </div>
                                  <div className="text-right font-mono text-[11px]">
                                    <p className="font-bold text-[var(--accent-emerald)]">${turn.cost_usd.toFixed(4)}</p>
                                    <p className="text-[10px] text-[var(--text-faint)]">in:{turn.tokens_in} out:{turn.tokens_out}</p>
                                  </div>
                                </div>
                              ))}
                            </div>
                          ) : (
                            <p className="text-xs text-[var(--text-faint)] italic">{t('admin:sessions.drawer.no_turns_data', { defaultValue: 'No turn details recorded' })}</p>
                          )}
                        </div>
                      </>
                    ) : (
                      <div className="p-8 text-center text-xs text-[var(--text-faint)]">
                        {drawerLoading ? t('admin:sessions.detail.loading', { defaultValue: 'Loading stats...' }) : t('admin:sessions.drawer.no_turns_data', { defaultValue: 'No turn stats available' })}
                      </div>
                    )}
                  </div>
                )}

                {/* CONTEXT & CONFIG TAB */}
                {drawerTab === 'context' && (
                  <div className="space-y-4">
                    <div className="px-4 py-3 rounded-[var(--radius-md)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] space-y-3">
                      <div>
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-1">
                          {t('admin:sessions.drawer.allowed_tools', { defaultValue: 'Allowed Tools' })}
                        </span>
                        {drawerDetail?.allowed_tools && drawerDetail.allowed_tools.length > 0 ? (
                          <div className="flex flex-wrap gap-1.5">
                            {drawerDetail.allowed_tools.map((tool) => (
                              <span key={tool} className="px-2 py-0.5 rounded text-xs font-mono bg-[var(--accent-gold)]/10 text-[var(--accent-gold)] border border-[var(--accent-gold)]/20">
                                {tool}
                              </span>
                            ))}
                          </div>
                        ) : (
                          <span className="text-xs text-[var(--text-muted)] italic">
                            {t('admin:sessions.drawer.no_tools_limit', { defaultValue: 'No restriction (All allowed)' })}
                          </span>
                        )}
                      </div>

                      {drawerDetail?.worker_session_id && (
                        <div>
                          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-1">
                            Worker Session ID
                          </span>
                          <code className="text-xs font-mono text-[var(--text-primary)] break-all select-all">
                            {drawerDetail.worker_session_id}
                          </code>
                        </div>
                      )}

                      {drawerDetail?.source && (
                        <div>
                          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-1">
                            Source Channel
                          </span>
                          <span className="text-xs font-bold uppercase text-[var(--text-primary)]">
                            {drawerDetail.source}
                          </span>
                        </div>
                      )}

                      {drawerDetail?.client_key && (
                        <div>
                          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block mb-1">
                            Client Key
                          </span>
                          <code className="text-xs font-mono text-[var(--text-muted)] break-all">
                            {drawerDetail.client_key}
                          </code>
                        </div>
                      )}
                    </div>

                    {drawerDetail?.context && Object.keys(drawerDetail.context).length > 0 && (
                      <div className="p-4 rounded-[var(--radius-md)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] space-y-2">
                        <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block">
                          Session Context Map
                        </span>
                        <pre className="text-[11px] font-mono text-[var(--text-secondary)] overflow-x-auto p-3 rounded bg-[var(--bg-hover)] border border-[var(--border-subtle)]">
                          {JSON.stringify(drawerDetail.context, null, 2)}
                        </pre>
                      </div>
                    )}
                  </div>
                )}

                {/* AUDIT LOG TAB */}
                {drawerTab === 'activity' && (
                  <div className="space-y-3">
                    {drawerActivities.length > 0 ? (
                      <div className="space-y-2 max-h-[420px] overflow-y-auto pr-1">
                        {drawerActivities.map((act) => (
                          <div key={`${act.chain_epoch ?? 'legacy'}:${act.id}`} className="p-3 rounded-[var(--radius-sm)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] space-y-1">
                            <div className="flex items-center justify-between text-xs">
                              <span className="font-bold text-[var(--text-primary)] font-mono">{act.action}</span>
                              <span className={`px-1.5 py-0.5 rounded text-[9px] font-bold ${act.outcome === 'success' ? 'bg-emerald-500/10 text-emerald-400' : 'bg-rose-500/10 text-rose-400'}`}>
                                {act.outcome}
                              </span>
                            </div>
                            <div className="flex items-center justify-between text-[10px] text-[var(--text-faint)] font-mono">
                              <span>User: {act.user_id}</span>
                              <span>{formatDateTime(new Date(act.ts).toISOString())}</span>
                            </div>
                          </div>
                        ))}
                      </div>
                    ) : (
                      <div className="p-8 text-center text-xs text-[var(--text-faint)]">
                        {drawerLoading ? t('admin:sessions.detail.loading', { defaultValue: 'Loading audit logs...' }) : t('admin:sessions.drawer.no_activity_data', { defaultValue: 'No audit logs found for this session' })}
                      </div>
                    )}
                  </div>
                )}
              </div>

              {/* Bottom Actions Footer */}
              <div className="p-5 border-t border-[var(--border-subtle)] bg-[var(--bg-surface)] space-y-3">
                <div className="flex gap-3">
                  {drawerSession.state !== 'terminated' && (
                    <button
                      type="button"
                      onClick={() => handleTerminate(drawerSession.id, true)}
                      disabled={actionLoading === drawerSession.id}
                      className="flex-1 inline-flex items-center justify-center gap-1.5 px-4 py-2.5 rounded-[var(--radius-sm)] text-[11px] font-bold uppercase tracking-wider text-[var(--accent-amber)] bg-[rgba(245,158,11,0.08)] border border-[rgba(245,158,11,0.18)] hover:bg-[rgba(245,158,11,0.15)] transition-all active:scale-95 disabled:opacity-40"
                    >
                      {actionLoading === drawerSession.id ? (
                        <div className="w-3.5 h-3.5 border border-current border-t-transparent rounded-full animate-spin" />
                      ) : (
                        <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor" className="h-3.5 w-3.5">
                          <path strokeLinecap="round" strokeLinejoin="round" d="M5.636 5.636a9 9 0 1 0 12.728 0M12 3v9" />
                        </svg>
                      )}
                      {t('admin:sessions.action.terminate', { defaultValue: 'Terminate Engine' })}
                    </button>
                  )}
                  <button
                    type="button"
                    onClick={() => handleDelete(drawerSession.id, true)}
                    disabled={actionLoading === drawerSession.id}
                    className="flex-1 inline-flex items-center justify-center gap-1.5 px-4 py-2.5 rounded-[var(--radius-sm)] text-[11px] font-bold uppercase tracking-wider text-[var(--accent-coral)] bg-[rgba(244,63,94,0.06)] border border-[rgba(244,63,94,0.18)] hover:bg-[rgba(244,63,94,0.12)] transition-all active:scale-95 disabled:opacity-40"
                  >
                    {t('admin:sessions.action.delete', { defaultValue: 'Delete Entry' })}
                  </button>
                </div>

                <Link
                  href={`/admin/sessions/detail?id=${encodeURIComponent(drawerSession.id)}`}
                  className="w-full inline-flex items-center justify-center gap-1.5 px-4 py-2.5 rounded-[var(--radius-sm)] text-[11px] font-bold uppercase tracking-wider text-[var(--accent-gold)] bg-[var(--accent-gold)]/10 border border-[var(--accent-gold)]/20 hover:bg-[var(--accent-gold)]/20 transition-all text-center"
                >
                  <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor" className="w-3.5 h-3.5">
                    <path strokeLinecap="round" strokeLinejoin="round" d="M13.5 6H5.25A2.25 2.25 0 0 0 3 8.25v10.5A2.25 2.25 0 0 0 5.25 21h10.5A2.25 2.25 0 0 0 18 18.75V10.5m-10.5 6L21 3m0 0h-5.25M21 3v5.25" />
                  </svg>
                  {t('admin:sessions.action.open_details', { defaultValue: 'Open Full Detail View' })}
                </Link>
              </div>

            </div>
          </div>
        </div>
      )}
    </div>
  );
}
