'use client';

import { useCallback, useEffect, useState } from 'react';
import { useSearchParams } from 'next/navigation';
import Link from 'next/link';
import {
  getSessionDetail,
  getSessionStats,
  getSessionDebug,
  terminateSession,
  deleteSession,
} from '@/lib/api/admin-sessions';
import { listActivity } from '@/lib/api/admin-activity';
import { SessionStatusBadge } from '@/components/admin/session-status-badge';
import { useAdminUI } from '@/context/admin-ui-context';
import type {
  AdminSessionDetailResponse,
  SessionStatsResponse,
  SessionDebugInfo,
  AuditActivity,
  AuditIdentityLink,
} from '@/lib/types/admin';
import { formatDateTime } from '@/lib/utils/format-time';
import { useTranslation } from 'react-i18next';

type TabKey = 'overview' | 'turns' | 'context' | 'activity';

function getUserDisplayInfo(
  userId: string,
  platform?: string,
  identityLink?: AuditIdentityLink,
) {
  const nameFromLink = identityLink?.display_name || identityLink?.DisplayName;
  const principalId = identityLink?.principal_user_id || identityLink?.PrincipalUserID;
  const email = identityLink?.email || identityLink?.Email;

  if (nameFromLink) {
    return {
      displayName: nameFromLink,
      subtitle: principalId ? `主账号: ${principalId}` : email || `User ID: ${userId}`,
      platformLabel: (platform || identityLink?.provider || 'USER').toUpperCase(),
      isResolved: true,
      avatarChar: nameFromLink.charAt(0).toUpperCase(),
    };
  }

  if (userId.startsWith('ou_') || platform === 'feishu') {
    return {
      displayName: `飞书平台用户 (${userId.length > 12 ? userId.slice(0, 10) + '...' : userId})`,
      subtitle: `User ID: ${userId}`,
      platformLabel: 'FEISHU',
      isResolved: false,
      avatarChar: '飞',
    };
  }

  if (userId.startsWith('U') || userId.startsWith('W') || platform === 'slack') {
    return {
      displayName: `Slack 平台用户 (${userId})`,
      subtitle: `User ID: ${userId}`,
      platformLabel: 'SLACK',
      isResolved: false,
      avatarChar: 'S',
    };
  }

  if (userId === 'anonymous' || userId.startsWith('anon_')) {
    return {
      displayName: 'WebChat 访客 / 匿名用户',
      subtitle: `User ID: ${userId}`,
      platformLabel: 'WEBCHAT',
      isResolved: false,
      avatarChar: '匿',
    };
  }

  if (userId === 'cron' || userId.startsWith('cron_')) {
    return {
      displayName: '系统 Cron 调度引擎',
      subtitle: `User ID: ${userId}`,
      platformLabel: 'CRON',
      isResolved: false,
      avatarChar: '⏰',
    };
  }

  return {
    displayName: `系统用户 (${userId})`,
    subtitle: `User ID: ${userId}`,
    platformLabel: (platform || 'SYSTEM').toUpperCase(),
    isResolved: false,
    avatarChar: userId.charAt(0).toUpperCase(),
  };
}

export default function SessionDetailPage() {
  const { t } = useTranslation();
  const { showToast, confirm } = useAdminUI();
  const searchParams = useSearchParams();
  const id = searchParams.get('id') ?? '';

  const [session, setSession] = useState<AdminSessionDetailResponse | null>(null);
  const [stats, setStats] = useState<SessionStatsResponse | null>(null);
  const [debug, setDebug] = useState<SessionDebugInfo | null>(null);
  const [activities, setActivities] = useState<AuditActivity[]>([]);
  
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notFound, setNotFound] = useState(false);
  const [activeTab, setActiveTab] = useState<TabKey>('overview');
  const [actionLoading, setActionLoading] = useState(false);
  const [copyFeedback, setCopyFeedback] = useState(false);

  const loadData = useCallback(async () => {
    if (!id) {
      setNotFound(true);
      setLoading(false);
      return;
    }
    try {
      setLoading(true);
      setError(null);
      setNotFound(false);

      const [detailRes, statsRes, debugRes, actRes] = await Promise.allSettled([
        getSessionDetail(id),
        getSessionStats(id),
        getSessionDebug(id),
        listActivity({ sessionId: id, limit: 50 }),
      ]);

      if (detailRes.status === 'fulfilled') {
        setSession(detailRes.value);
      } else {
        setNotFound(true);
        return;
      }

      if (statsRes.status === 'fulfilled') setStats(statsRes.value);
      else setStats(null);

      if (debugRes.status === 'fulfilled') setDebug(debugRes.value.debug);
      else setDebug(null);

      if (actRes.status === 'fulfilled') setActivities(actRes.value.rows ?? []);
      else setActivities([]);
    } catch (err) {
      setError(
        err instanceof Error
          ? err.message
          : t('admin:sessions.detail.error_load', { defaultValue: 'Failed to load session details' })
      );
    } finally {
      setLoading(false);
    }
  }, [id, t]);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- initial data fetch
    loadData();
  }, [loadData]);

  const handleCopyId = async () => {
    if (!id) return;
    try {
      await navigator.clipboard.writeText(id);
      setCopyFeedback(true);
      showToast(t('admin:sessions.toast.copied', { defaultValue: 'Copied Session ID to clipboard' }), 'info');
      setTimeout(() => setCopyFeedback(false), 2000);
    } catch {
      showToast(t('admin:sessions.toast.copy_failed', { defaultValue: 'Failed to copy ID' }), 'error');
    }
  };

  const copyToClipboard = async (text: string, label: string) => {
    try {
      await navigator.clipboard.writeText(text);
      showToast(t('admin:sessions.toast.copied_text', { label, defaultValue: `Copied ${label} to clipboard` }), 'info');
    } catch {
      showToast(t('admin:sessions.toast.copy_failed', { defaultValue: 'Failed to copy' }), 'error');
    }
  };

  const handleTerminate = async () => {
    if (!session || session.state === 'terminated') return;
    const confirmed = await confirm(
      t('admin:sessions.confirm.terminate_title', { defaultValue: 'Terminate Session?' }),
      t('admin:sessions.confirm.terminate_body', {
        id: session.id,
        defaultValue: `Are you sure you want to terminate session "${session.id}"? The running worker process will be stopped immediately.`,
      }),
      { confirmLabel: t('admin:sessions.action.terminate', { defaultValue: 'Terminate' }), destructive: true }
    );
    if (!confirmed) return;
    try {
      setActionLoading(true);
      await terminateSession(session.id);
      setSession((prev) => (prev ? { ...prev, state: 'terminated' } : prev));
      showToast(t('admin:sessions.toast.terminated', { id: session.id, defaultValue: `Session "${session.id}" successfully terminated.` }), 'success');
      loadData();
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('admin:sessions.toast.terminate_failed', { defaultValue: 'Failed to terminate session' }), 'error');
    } finally {
      setActionLoading(false);
    }
  };

  const handleDelete = async () => {
    if (!session) return;
    const confirmed = await confirm(
      t('admin:sessions.confirm.delete_title', { defaultValue: 'Delete Session?' }),
      t('admin:sessions.confirm.delete_body', {
        id: session.id,
        defaultValue: `Are you sure you want to permanently delete session "${session.id}"? All database records will be erased.`,
      }),
      { confirmLabel: t('admin:sessions.action.delete', { defaultValue: 'Delete Entry' }), destructive: true }
    );
    if (!confirmed) return;
    try {
      setActionLoading(true);
      await deleteSession(session.id);
      showToast(t('admin:sessions.toast.deleted', { id: session.id, defaultValue: `Session "${session.id}" successfully deleted.` }), 'success');
      window.location.href = '/admin/sessions';
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('admin:sessions.toast.delete_failed', { defaultValue: 'Failed to delete session' }), 'error');
      setActionLoading(false);
    }
  };

  // Helper container
  const wrapper = (children: React.ReactNode) => (
    <div className="max-w-6xl mx-auto px-6 py-8">
      <Link
        href="/admin/sessions"
        className="inline-flex items-center gap-1.5 text-xs text-[var(--text-faint)] hover:text-[var(--text-primary)] transition-colors mb-6 font-medium"
      >
        <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className="h-3.5 w-3.5">
          <path strokeLinecap="round" strokeLinejoin="round" d="M10.5 19.5 3 12m0 0 7.5-7.5M3 12h18" />
        </svg>
        {t('admin:sessions.action.back_to_sessions', { defaultValue: 'Back to Sessions' })}
      </Link>
      {children}
    </div>
  );

  if (!id) {
    return wrapper(
      <div className="flex items-center justify-center min-h-[50vh]">
        <p className="text-sm text-[var(--text-faint)]">{t('admin:sessions.detail.no_id', { defaultValue: 'No session ID specified' })}</p>
      </div>
    );
  }

  if (loading) {
    return wrapper(
      <div className="flex flex-col items-center justify-center min-h-[50vh] gap-3">
        <div className="w-7 h-7 border-2 border-[var(--accent-gold)] border-t-transparent rounded-full animate-spin" />
        <span className="text-xs text-[var(--text-faint)] font-medium">
          {t('admin:sessions.detail.loading', { defaultValue: 'Loading session metrics & details...' })}
        </span>
      </div>
    );
  }

  if (error) {
    return wrapper(
      <div className="rounded-[var(--radius-md)] bg-[rgba(244,63,94,0.08)] border border-[rgba(244,63,94,0.15)] p-5">
        <div className="flex items-center justify-between">
          <p className="text-sm font-medium text-[var(--text-coral)]">{error}</p>
          <button
            type="button"
            onClick={loadData}
            className="text-xs font-bold text-[var(--accent-coral)] underline underline-offset-2 hover:opacity-80 transition-opacity"
          >
            {t('common:action.retry', { defaultValue: 'Retry' })}
          </button>
        </div>
      </div>
    );
  }

  if (notFound || !session) {
    return wrapper(
      <div className="flex flex-col items-center justify-center min-h-[50vh] text-center">
        <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className="h-10 w-10 text-[var(--text-faint)] mb-3">
          <path strokeLinecap="round" strokeLinejoin="round" d="M12 9v3.75m9-.75a9 9 0 1 1-18 0 9 9 0 0 1 18 0Zm-9 3.75h.008v.008H12v-.008Z" />
        </svg>
        <p className="text-sm font-bold text-[var(--text-muted)]">{t('admin:sessions.detail.not_found', { defaultValue: 'Session not found' })}</p>
        <p className="text-xs font-mono text-[var(--text-faint)] mt-1">{id}</p>
      </div>
    );
  }

  // Derived metrics
  const totalTurns = stats?.total_turns ?? session.turn_count ?? 0;
  const totalTokens = (stats?.total_tokens_in ?? 0) + (stats?.total_tokens_out ?? 0);
  const totalCost = stats?.total_cost_usd ?? 0;
  const avgDurationMs = stats?.turns && stats.turns.length > 0
    ? Math.round(stats.turns.reduce((acc, t) => acc + t.duration_ms, 0) / stats.turns.length)
    : 0;

  return wrapper(
    <div className="space-y-6">
      {/* Hero Header */}
      <div className="flex flex-col md:flex-row md:items-center justify-between gap-4 p-6 rounded-[var(--radius-lg)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] shadow-sm">
        <div className="space-y-2">
          <div className="flex items-center gap-2.5">
            <h1 className="text-xl font-display font-bold text-[var(--text-primary)]">
              {session.title || t('admin:sessions.detail.untitled', { defaultValue: 'Untitled Session' })}
            </h1>
            <SessionStatusBadge state={session.state} />
            {debug?.has_worker && (
              <span className="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full text-[10px] font-bold text-[var(--accent-emerald)] bg-[var(--accent-emerald)]/10 border border-[var(--accent-emerald)]/20">
                <span className="w-1.5 h-1.5 rounded-full bg-[var(--accent-emerald)] animate-pulse" />
                Live Worker Active
              </span>
            )}
          </div>

          <div className="flex items-center gap-3">
            <code className="text-xs font-mono text-[var(--accent-gold)] bg-[var(--bg-hover)] px-2.5 py-1 rounded-[var(--radius-xs)] border border-[var(--border-subtle)] font-medium">
              {session.id}
            </code>
            <button
              type="button"
              onClick={handleCopyId}
              className="inline-flex items-center gap-1 text-[11px] font-bold uppercase tracking-wider text-[var(--accent-gold)] hover:underline"
            >
              {copyFeedback ? t('common:action.copied', { defaultValue: 'Copied' }) : t('common:action.copy', { defaultValue: 'Copy ID' })}
            </button>
          </div>
        </div>

        {/* Header Action Buttons */}
        <div className="flex items-center gap-2.5">
          <button
            type="button"
            onClick={loadData}
            className="p-2 rounded-[var(--radius-sm)] text-[var(--text-muted)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)] border border-[var(--border-subtle)] transition-all"
            title={t('admin:sessions.action.refresh', { defaultValue: 'Refresh' })}
          >
            <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className="w-4 h-4">
              <path strokeLinecap="round" strokeLinejoin="round" d="M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0 3.181 3.183a8.25 8.25 0 0 0 13.803-3.7M4.031 9.865a8.25 8.25 0 0 1 13.803-3.7l3.181 3.182m0-4.991v4.99" />
            </svg>
          </button>

          {session.state !== 'terminated' && (
            <button
              type="button"
              onClick={handleTerminate}
              disabled={actionLoading}
              className="inline-flex items-center gap-1.5 px-3.5 py-2 rounded-[var(--radius-sm)] text-[11px] font-bold uppercase tracking-wider text-[var(--accent-amber)] bg-[rgba(245,158,11,0.08)] border border-[rgba(245,158,11,0.18)] hover:bg-[rgba(245,158,11,0.15)] transition-all disabled:opacity-40"
            >
              <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor" className="h-3.5 w-3.5">
                <path strokeLinecap="round" strokeLinejoin="round" d="M5.636 5.636a9 9 0 1 0 12.728 0M12 3v9" />
              </svg>
              {t('admin:sessions.action.terminate', { defaultValue: 'Terminate Engine' })}
            </button>
          )}

          <button
            type="button"
            onClick={handleDelete}
            disabled={actionLoading}
            className="inline-flex items-center gap-1.5 px-3.5 py-2 rounded-[var(--radius-sm)] text-[11px] font-bold uppercase tracking-wider text-[var(--accent-coral)] bg-[rgba(244,63,94,0.06)] border border-[rgba(244,63,94,0.18)] hover:bg-[rgba(244,63,94,0.12)] transition-all disabled:opacity-40"
          >
            <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.8} stroke="currentColor" className="h-3.5 w-3.5">
              <path strokeLinecap="round" strokeLinejoin="round" d="m14.74 9-.346 9m-4.788 0L9.26 9m9.968-3.21c.342.052.682.107 1.022.166m-1.022-.165L18.16 19.673a2.25 2.25 0 0 1-2.244 2.077H8.084a2.25 2.25 0 0 1-2.244-2.077L4.772 5.79m14.456 0a48.108 48.108 0 0 0-3.478-.397m-12 .562c.34-.059.68-.114 1.022-.165m0 0a48.11 48.11 0 0 1 3.478-.397m7.5 0v-.916c0-1.18-.91-2.164-2.09-2.201a51.964 51.964 0 0 0-3.32 0c-1.18.037-2.09 1.022-2.09 2.201v.916m7.5 0a48.667 48.667 0 0 0-7.5 0" />
            </svg>
            {t('admin:sessions.action.delete', { defaultValue: 'Delete' })}
          </button>
        </div>
      </div>

      {/* KPI Cards Row (4 Grid) */}
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
        {/* Card 1: Worker State */}
        <div className="p-4 rounded-[var(--radius-md)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] space-y-1">
          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block">
            {t('admin:sessions.detail.kpi.worker_status', { defaultValue: 'Worker Process State' })}
          </span>
          <div className="flex items-center gap-2">
            <span className="text-base font-bold font-mono text-[var(--text-primary)]">
              {session.worker_type || 'claudecode'}
            </span>
          </div>
          <span className="text-xs text-[var(--text-faint)] block font-mono">
            DB Turns: {debug?.db_turn_count ?? totalTurns} • Seq: {debug?.last_seq_sent ?? '—'}
          </span>
        </div>

        {/* Card 2: Turn Count */}
        <div className="p-4 rounded-[var(--radius-md)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] space-y-1">
          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block">
            {t('admin:sessions.detail.kpi.turn_count', { defaultValue: 'Execution Turns' })}
          </span>
          <div className="flex items-baseline gap-2">
            <span className="text-2xl font-bold font-mono text-[var(--text-primary)]">
              {totalTurns}
            </span>
            <span className="text-xs text-[var(--text-muted)] font-medium">turns</span>
          </div>
          <span className="text-xs text-[var(--text-faint)] block">
            {t('admin:sessions.detail.kpi.turn_sub', {
              success: stats?.success_turns ?? totalTurns,
              failed: stats?.failed_turns ?? 0,
              defaultValue: `${stats?.success_turns ?? totalTurns} passed / ${stats?.failed_turns ?? 0} failed`,
            })}
          </span>
        </div>

        {/* Card 3: Total Tokens */}
        <div className="p-4 rounded-[var(--radius-md)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] space-y-1">
          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block">
            {t('admin:sessions.detail.kpi.token_usage', { defaultValue: 'Total Tokens' })}
          </span>
          <div className="flex items-baseline gap-2">
            <span className="text-2xl font-bold font-mono text-[var(--accent-gold)]">
              {totalTokens.toLocaleString()}
            </span>
          </div>
          <span className="text-xs text-[var(--text-faint)] block font-mono">
            In: {(stats?.total_tokens_in ?? 0).toLocaleString()} • Out: {(stats?.total_tokens_out ?? 0).toLocaleString()}
          </span>
        </div>

        {/* Card 4: Est USD Cost */}
        <div className="p-4 rounded-[var(--radius-md)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] space-y-1">
          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider block">
            {t('admin:sessions.detail.kpi.cost_est', { defaultValue: 'Est. API Cost' })}
          </span>
          <div className="flex items-baseline gap-2">
            <span className="text-2xl font-bold font-mono text-[var(--accent-emerald)]">
              ${totalCost.toFixed(4)}
            </span>
          </div>
          <span className="text-xs text-[var(--text-faint)] block">
            Avg turn latency: {avgDurationMs}ms
          </span>
        </div>
      </div>

      {/* Main Content Tabs Container */}
      <div className="rounded-[var(--radius-lg)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] shadow-sm">
        {/* Navigation Tabs Header */}
        <div className="flex items-center gap-6 px-6 pt-4 border-b border-[var(--border-subtle)] text-xs font-semibold">
          {(['overview', 'turns', 'context', 'activity'] as TabKey[]).map((tab) => (
            <button
              key={tab}
              type="button"
              onClick={() => setActiveTab(tab)}
              className={`pb-3 border-b-2 transition-colors ${
                activeTab === tab
                  ? 'border-[var(--accent-gold)] text-[var(--accent-gold)] font-bold'
                  : 'border-transparent text-[var(--text-faint)] hover:text-[var(--text-primary)]'
              }`}
            >
              {t(`admin:sessions.detail.tab_${tab}`, { defaultValue: tab })}
            </button>
          ))}
        </div>

        {/* Tab Content Panel */}
        <div className="p-6">
          {/* TAB 1: OVERVIEW & ENVIRONMENT */}
          {activeTab === 'overview' && (
            <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
              {/* User Identity & Owner Card */}
              {(() => {
                const uinfo = getUserDisplayInfo(session.user_id, session.platform, session.identity_link);
                return (
                  <div className="p-5 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] space-y-4">
                    <div className="flex items-center justify-between">
                      <h3 className="text-xs font-bold text-[var(--text-faint)] uppercase tracking-wider">
                        {t('admin:sessions.detail.system_info.identity_owner', { defaultValue: 'Executing User Identity' })}
                      </h3>
                      <span className="px-2 py-0.5 rounded text-[10px] font-bold font-mono bg-white/5 text-[var(--text-muted)] border border-[var(--border-subtle)]">
                        {uinfo.platformLabel}
                      </span>
                    </div>

                    <div className="flex items-start gap-3">
                      <div className="w-10 h-10 rounded-full bg-[var(--accent-gold)]/10 text-[var(--accent-gold)] border border-[var(--accent-gold)]/20 flex items-center justify-center font-bold text-sm shrink-0">
                        {uinfo.avatarChar}
                      </div>
                      <div className="min-w-0 flex-1">
                        <h4 className="text-sm font-bold text-[var(--text-primary)] truncate">
                          {uinfo.displayName}
                        </h4>
                        <p className="text-xs text-[var(--text-muted)] font-mono truncate">
                          {uinfo.subtitle}
                        </p>

                        <div className="mt-2.5 flex items-center gap-2">
                          <code className="text-[11px] font-mono text-[var(--text-secondary)] bg-[var(--bg-base)] px-2 py-1 rounded border border-[var(--border-subtle)] truncate max-w-[200px]">
                            {session.user_id}
                          </code>
                          <button
                            type="button"
                            onClick={() => copyToClipboard(session.user_id, 'User ID')}
                            className="px-2 py-1 text-[10px] font-semibold rounded bg-[var(--bg-base)] hover:bg-[var(--bg-hover)] border border-[var(--border-subtle)] text-[var(--text-muted)] hover:text-[var(--text-primary)] transition-all shrink-0"
                          >
                            {t('admin:sessions.detail.action.copy_id', { defaultValue: 'Copy ID' })}
                          </button>
                        </div>
                      </div>
                    </div>

                    <div className="pt-3 border-t border-[var(--border-subtle)] space-y-2 text-xs">
                      {session.owner_id && (
                        <div className="flex items-center justify-between">
                          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase">
                            {t('admin:sessions.detail.system_info.owner_id', { defaultValue: 'Owner ID' })}
                          </span>
                          <code className="font-mono text-[var(--text-primary)]">
                            {session.owner_id}
                          </code>
                        </div>
                      )}

                      {session.bot_id && (
                        <div className="flex items-center justify-between">
                          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase">
                            {t('admin:sessions.detail.system_info.bot_id', { defaultValue: 'Bot ID' })}
                          </span>
                          <code className="font-mono text-[var(--text-primary)]">
                            {session.bot_id}
                          </code>
                        </div>
                      )}

                      {session.workspace_id && (
                        <div className="flex items-center justify-between">
                          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase">
                            {t('admin:sessions.detail.system_info.workspace_id', { defaultValue: 'Workspace ID' })}
                          </span>
                          <code className="font-mono text-[var(--text-primary)]">
                            {session.workspace_id}
                          </code>
                        </div>
                      )}
                    </div>

                    {/* Dedicated Audit Filter Link Button */}
                    <div className="pt-2 border-t border-[var(--border-subtle)] flex items-center justify-end">
                      <Link
                        href={`/admin/activity?user_id=${encodeURIComponent(session.user_id)}`}
                        className="inline-flex items-center gap-1.5 text-xs font-semibold text-[var(--accent-gold)] hover:underline"
                        title={t('admin:sessions.detail.identity.view_activity_hint', { defaultValue: 'Search audit logs for this user' })}
                      >
                        <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className="w-3.5 h-3.5">
                          <path strokeLinecap="round" strokeLinejoin="round" d="m21 21-5.197-5.197m0 0A7.5 7.5 0 1 0 5.196 5.196a7.5 7.5 0 0 0 10.607 10.607Z" />
                        </svg>
                        {t('admin:sessions.detail.identity.view_user_activity', { defaultValue: 'Search User Audit Logs' })}
                      </Link>
                    </div>
                  </div>
                );
              })()}

              {/* Messaging Platform & Channel Card */}
              <div className="p-5 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] space-y-4">
                <h3 className="text-xs font-bold text-[var(--text-faint)] uppercase tracking-wider">
                  {t('admin:sessions.detail.system_info.messaging_platform', { defaultValue: 'Messaging Platform' })}
                </h3>

                <div className="space-y-3">
                  <div>
                    <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase block mb-1">
                      Platform Channel
                    </span>
                    <span className="text-xs font-bold uppercase text-[var(--text-primary)]">
                      {session.platform || 'direct (WebChat)'}
                    </span>
                  </div>

                  {session.platform_key && Object.keys(session.platform_key).length > 0 && (
                    <div>
                      <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase block mb-1">
                        {t('admin:sessions.detail.system_info.platform_keys', { defaultValue: 'Platform Key Parameters' })}
                      </span>
                      <div className="flex flex-wrap gap-2">
                        {Object.entries(session.platform_key).map(([k, v]) => (
                          <div key={k} className="px-2.5 py-1 rounded bg-[var(--bg-surface)] border border-[var(--border-subtle)] text-xs font-mono">
                            <span className="text-[var(--text-faint)]">{k}:</span>{' '}
                            <span className="text-[var(--text-primary)] font-bold">{v}</span>
                          </div>
                        ))}
                      </div>
                    </div>
                  )}
                </div>
              </div>

              {/* Worker Environment & Path Card */}
              <div className="p-5 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] space-y-4">
                <h3 className="text-xs font-bold text-[var(--text-faint)] uppercase tracking-wider">
                  {t('admin:sessions.detail.system_info.worker_env', { defaultValue: 'Worker Environment & Directory' })}
                </h3>

                <div className="space-y-3">
                  <div>
                    <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase block mb-1">
                      {t('admin:sessions.drawer.work_dir', { defaultValue: 'Working Directory' })}
                    </span>
                    <code className="text-xs font-mono text-[var(--text-primary)] break-all select-all block p-2 rounded bg-[var(--bg-surface)] border border-[var(--border-subtle)]">
                      {session.work_dir || '—'}
                    </code>
                  </div>

                  {session.worker_session_id && (
                    <div>
                      <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase block mb-1">
                        {t('admin:sessions.detail.system_info.worker_session_id', { defaultValue: 'Worker Session ID' })}
                      </span>
                      <code className="text-xs font-mono text-[var(--text-secondary)] break-all">
                        {session.worker_session_id}
                      </code>
                    </div>
                  )}

                  {session.client_key && (
                    <div>
                      <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase block mb-1">
                        {t('admin:sessions.detail.system_info.client_key', { defaultValue: 'Client Key' })}
                      </span>
                      <code className="text-xs font-mono text-[var(--text-secondary)] break-all">
                        {session.client_key}
                      </code>
                    </div>
                  )}

                  {session.source && (
                    <div>
                      <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase block mb-1">
                        {t('admin:sessions.detail.system_info.source', { defaultValue: 'Source Channel' })}
                      </span>
                      <span className="text-xs font-bold uppercase text-[var(--text-primary)]">
                        {session.source}
                      </span>
                    </div>
                  )}
                </div>
              </div>

              {/* Lifecycle & Expiration Timers Card */}
              <div className="p-5 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] space-y-4">
                <h3 className="text-xs font-bold text-[var(--text-faint)] uppercase tracking-wider">
                  {t('admin:sessions.detail.system_info.lifecycle_timers', { defaultValue: 'Lifecycle & Timers' })}
                </h3>

                <div className="grid grid-cols-2 gap-4">
                  <div>
                    <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase block mb-1">
                      {t('admin:sessions.drawer.started_at', { defaultValue: 'Created At' })}
                    </span>
                    <span className="text-xs text-[var(--text-primary)] font-medium">
                      {formatDateTime(session.created_at)}
                    </span>
                  </div>

                  <div>
                    <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase block mb-1">
                      {t('admin:sessions.drawer.last_active', { defaultValue: 'Last Active' })}
                    </span>
                    <span className="text-xs text-[var(--text-primary)] font-medium">
                      {formatDateTime(session.updated_at)}
                    </span>
                  </div>

                  {session.idle_expires_at && (
                    <div>
                      <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase block mb-1">
                        {t('admin:sessions.drawer.idle_expires', { defaultValue: 'Idle Expiration' })}
                      </span>
                      <span className="text-xs text-[var(--text-muted)]">
                        {formatDateTime(session.idle_expires_at)}
                      </span>
                    </div>
                  )}

                  {session.expires_at && (
                    <div>
                      <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase block mb-1">
                        {t('admin:sessions.drawer.expires_at', { defaultValue: 'Absolute Expiry' })}
                      </span>
                      <span className="text-xs text-[var(--text-muted)]">
                        {formatDateTime(session.expires_at)}
                      </span>
                    </div>
                  )}
                </div>
              </div>
            </div>
          )}

          {/* TAB 2: TURNS BREAKDOWN & PERFORMANCE */}
          {activeTab === 'turns' && (
            <div className="space-y-4">
              {stats?.turns && stats.turns.length > 0 ? (
                <div className="overflow-x-auto border border-[var(--border-subtle)] rounded-[var(--radius-md)]">
                  <table className="w-full text-left text-xs">
                    <thead className="bg-[var(--bg-hover)] border-b border-[var(--border-subtle)] text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                      <tr>
                        <th className="py-3 px-4">{t('admin:sessions.detail.turn_table.turn_num', { defaultValue: 'Turn #' })}</th>
                        <th className="py-3 px-4">{t('admin:sessions.detail.turn_table.model', { defaultValue: 'Model' })}</th>
                        <th className="py-3 px-4">{t('admin:sessions.detail.turn_table.status', { defaultValue: 'Status' })}</th>
                        <th className="py-3 px-4 text-right">{t('admin:sessions.detail.turn_table.duration', { defaultValue: 'Latency' })}</th>
                        <th className="py-3 px-4 text-right">{t('admin:sessions.detail.turn_table.tokens', { defaultValue: 'Tokens (In/Out/Cache)' })}</th>
                        <th className="py-3 px-4 text-right">{t('admin:sessions.detail.turn_table.cost', { defaultValue: 'Cost ($)' })}</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-[var(--border-subtle)] font-mono text-[11px]">
                      {stats.turns.map((turn) => (
                        <tr key={turn.seq} className="hover:bg-[var(--bg-hover)] transition-colors">
                          <td className="py-3 px-4 font-bold text-[var(--text-primary)]">#{turn.turn_num}</td>
                          <td className="py-3 px-4 text-[var(--text-secondary)]">{turn.model || '—'}</td>
                          <td className="py-3 px-4 font-sans">
                            <span className={`px-2 py-0.5 rounded text-[10px] font-bold ${turn.success ? 'bg-emerald-500/10 text-emerald-400' : 'bg-rose-500/10 text-rose-400'}`}>
                              {turn.success ? 'PASS' : 'FAIL'}
                            </span>
                          </td>
                          <td className="py-3 px-4 text-right text-[var(--text-primary)]">{turn.duration_ms}ms</td>
                          <td className="py-3 px-4 text-right text-[var(--text-secondary)]">
                            in:{turn.tokens_in} out:{turn.tokens_out} cache:{turn.tokens_cache_read}
                          </td>
                          <td className="py-3 px-4 text-right font-bold text-[var(--accent-emerald)]">${turn.cost_usd.toFixed(4)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ) : (
                <div className="p-12 text-center text-xs text-[var(--text-faint)]">
                  {t('admin:sessions.drawer.no_turns_data', { defaultValue: 'No turn details recorded for this session' })}
                </div>
              )}
            </div>
          )}

          {/* TAB 3: CONTEXT & TOOLS */}
          {activeTab === 'context' && (
            <div className="space-y-6">
              {/* Allowed Tools */}
              <div className="p-5 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] space-y-3">
                <h3 className="text-xs font-bold text-[var(--text-faint)] uppercase tracking-wider">
                  {t('admin:sessions.drawer.allowed_tools', { defaultValue: 'Allowed Tools' })}
                </h3>
                {session.allowed_tools && session.allowed_tools.length > 0 ? (
                  <div className="flex flex-wrap gap-2">
                    {session.allowed_tools.map((tool) => (
                      <span key={tool} className="px-3 py-1 rounded text-xs font-mono bg-[var(--accent-gold)]/10 text-[var(--accent-gold)] border border-[var(--accent-gold)]/20 font-medium">
                        {tool}
                      </span>
                    ))}
                  </div>
                ) : (
                  <p className="text-xs text-[var(--text-muted)] italic">
                    {t('admin:sessions.drawer.no_tools_limit', { defaultValue: 'No restriction (All tools allowed)' })}
                  </p>
                )}
              </div>

              {/* Context map */}
              {session.context && Object.keys(session.context).length > 0 && (
                <div className="p-5 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] space-y-3">
                  <h3 className="text-xs font-bold text-[var(--text-faint)] uppercase tracking-wider">
                    Raw Context Map
                  </h3>
                  <pre className="text-xs font-mono text-[var(--text-secondary)] p-4 rounded bg-[var(--bg-surface)] border border-[var(--border-subtle)] overflow-x-auto">
                    {JSON.stringify(session.context, null, 2)}
                  </pre>
                </div>
              )}
            </div>
          )}

          {/* TAB 4: AUDIT TRAIL */}
          {activeTab === 'activity' && (
            <div className="space-y-3">
              {activities.length > 0 ? (
                <div className="space-y-2.5">
                  {activities.map((act) => (
                    <div key={`${act.chain_epoch ?? 'legacy'}:${act.id}`} className="p-4 rounded-[var(--radius-md)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] flex flex-col md:flex-row md:items-center justify-between gap-3 text-xs">
                      <div className="space-y-1">
                        <div className="flex items-center gap-2">
                          <span className="font-bold text-[var(--text-primary)] font-mono">{act.action}</span>
                          <span className={`px-2 py-0.5 rounded text-[10px] font-bold ${act.outcome === 'success' ? 'bg-emerald-500/10 text-emerald-400' : 'bg-rose-500/10 text-rose-400'}`}>
                            {act.outcome}
                          </span>
                        </div>
                        <p className="text-[10px] text-[var(--text-faint)] font-mono">
                          Operator: {act.user_id} ({act.user_id_type || 'user'}) • Platform: {act.platform}
                        </p>
                      </div>
                      <div className="text-right text-[10px] text-[var(--text-faint)] font-mono shrink-0">
                        {formatDateTime(new Date(act.ts).toISOString())}
                      </div>
                    </div>
                  ))}
                </div>
              ) : (
                <div className="p-12 text-center text-xs text-[var(--text-faint)]">
                  {t('admin:sessions.drawer.no_activity_data', { defaultValue: 'No audit trail logs recorded for this session' })}
                </div>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
