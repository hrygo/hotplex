'use client';

import { useEffect, useState, useCallback } from 'react';
import Link from 'next/link';
import { listBots } from '@/lib/api/admin-bots';
import { listSessions } from '@/lib/api/admin-sessions';
import { listCronJobs } from '@/lib/api/admin-cron';
import { listAdminWorkspaces } from '@/lib/api/admin-workspaces';
import { listActivity, listActivityStats } from '@/lib/api/admin-activity';
import { MetricCard } from '@/components/admin/metric-card';
import { adminFetch } from '@/lib/api/admin-client';
import { useAdminUI } from '@/context/admin-ui-context';
import { useTranslation } from 'react-i18next';
import type { AuditActivity } from '@/lib/types/admin';
import { formatDateTime } from '@/lib/utils/format-time';

interface DashboardMetrics {
  botsTotal: number;
  botsConnected: number;
  botsDisconnected: number;
  sessionsTotal: number;
  sessionsActive: number;
  sessionsPoolMax: number;
  cronTotal: number;
  cronEnabled: number;
  workspacesTotal: number;
  auditTotal: number;
  auditSuccess: number;
  auditDenied: number;
  gatewayOnline: boolean;
  dbPath: string;
  dbStatus: string;
  dbDialect: string;
  version: string;
  workerChecks: Record<string, any>;
}

export default function DashboardPage() {
  const { t } = useTranslation();
  const { showToast, confirm } = useAdminUI();

  const [metrics, setMetrics] = useState<DashboardMetrics>({
    botsTotal: 0,
    botsConnected: 0,
    botsDisconnected: 0,
    sessionsTotal: 0,
    sessionsActive: 0,
    sessionsPoolMax: 100,
    cronTotal: 0,
    cronEnabled: 0,
    workspacesTotal: 0,
    auditTotal: 0,
    auditSuccess: 0,
    auditDenied: 0,
    gatewayOnline: false,
    dbPath: '',
    dbStatus: '',
    dbDialect: 'sqlite',
    version: '',
    workerChecks: {},
  });

  const [recentActivities, setRecentActivities] = useState<AuditActivity[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Uptime ticking state
  const [uptime, setUptime] = useState<number | null>(null);

  // Restart pipeline states
  const [isRestarting, setIsRestarting] = useState(false);
  const [restartStep, setRestartStep] = useState<'idle' | 'initiating' | 'offline' | 'polling' | 'completed' | 'failed'>('idle');
  const [pollingAttempts, setPollingAttempts] = useState(0);

  const fetchAllMetrics = useCallback(async () => {
    try {
      setError(null);

      // Fetch health first
      let healthData: any = null;
      try {
        healthData = await adminFetch<any>('/admin/health');
      } catch (e) {
        console.warn('Health probe failed', e);
      }

      // Fetch pool stats
      let poolStats: any = null;
      try {
        poolStats = await adminFetch<any>('/admin/sessions/pool');
      } catch (e) {
        console.warn('Pool stats probe failed', e);
      }

      const [botsRes, sessionsRes, cronRes, wsRes, actStreamRes, actStatsRes] = await Promise.allSettled([
        listBots(),
        listSessions(100, 0),
        listCronJobs(),
        listAdminWorkspaces(),
        listActivity({ limit: 6 }),
        listActivityStats({}),
      ]);

      const m: DashboardMetrics = {
        botsTotal: 0,
        botsConnected: 0,
        botsDisconnected: 0,
        sessionsTotal: 0,
        sessionsActive: 0,
        sessionsPoolMax: poolStats?.max || 100,
        cronTotal: 0,
        cronEnabled: 0,
        workspacesTotal: 0,
        auditTotal: 0,
        auditSuccess: 0,
        auditDenied: 0,
        gatewayOnline: false,
        dbPath: healthData?.checks?.database?.path || 'sqlite.db',
        dbStatus: healthData?.checks?.database?.status || 'healthy',
        dbDialect: healthData?.checks?.database?.dialect || 'sqlite',
        version: healthData?.version || 'v1.37.2',
        workerChecks: healthData?.checks?.workers || {},
      };

      if (botsRes.status === 'fulfilled') {
        const bots = botsRes.value;
        m.botsTotal = bots.length;
        m.botsConnected = bots.filter((b) => b.status === 'connected').length;
        m.botsDisconnected = bots.filter((b) => b.status !== 'connected').length;
        m.gatewayOnline = true;
      }

      if (sessionsRes.status === 'fulfilled') {
        const sessions = sessionsRes.value.sessions;
        m.sessionsTotal = sessions.length;
        m.sessionsActive = sessions.filter(
          (s) => s.state === 'running' || s.state === 'created' || s.state === 'active' || s.state === 'working'
        ).length;
        m.gatewayOnline = true;
      }

      if (cronRes.status === 'fulfilled') {
        const jobs = cronRes.value;
        m.cronTotal = jobs.length;
        m.cronEnabled = jobs.filter((j) => j.enabled).length;
        m.gatewayOnline = true;
      }

      if (wsRes.status === 'fulfilled') {
        m.workspacesTotal = wsRes.value.length;
      }

      if (actStreamRes.status === 'fulfilled') {
        setRecentActivities(actStreamRes.value.rows ?? []);
      }

      if (actStatsRes.status === 'fulfilled') {
        const ast = actStatsRes.value;
        m.auditTotal = ast.total ?? 0;
        m.auditSuccess = ast.by_outcome?.['success'] ?? 0;
        m.auditDenied = ast.by_outcome?.['denied'] ?? 0;
      }

      if (healthData?.checks?.gateway?.uptime_seconds !== undefined) {
        setUptime(healthData.checks.gateway.uptime_seconds);
      }

      // Check if all primary probes failed
      const allFailed =
        botsRes.status === 'rejected' &&
        sessionsRes.status === 'rejected' &&
        cronRes.status === 'rejected';

      if (allFailed) {
        const firstErr = botsRes.reason;
        setError(
          firstErr instanceof Error
            ? firstErr.message
            : t('admin:dashboard.error.unreachable', { defaultValue: 'Gateway unreachable' })
        );
        m.gatewayOnline = false;
        setUptime(null);
      }

      setMetrics(m);
    } catch (err) {
      setError(
        err instanceof Error
          ? err.message
          : t('admin:dashboard.error.load_failed', { defaultValue: 'Failed to load dashboard' })
      );
      setUptime(null);
    } finally {
      setLoading(false);
    }
  }, [t]);

  // Initial load
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- mount-time metrics fetch
    fetchAllMetrics();
  }, [fetchAllMetrics]);

  // Live ticking uptime
  useEffect(() => {
    if (uptime === null || !metrics.gatewayOnline || isRestarting) return;
    const timer = setInterval(() => {
      setUptime((prev) => (prev !== null ? prev + 1 : null));
    }, 1000);
    return () => clearInterval(timer);
  }, [uptime, metrics.gatewayOnline, isRestarting]);

  const formatUptime = (seconds: number | null): string => {
    if (seconds === null || seconds < 0) return t('admin:dashboard.offline', { defaultValue: 'Offline' });
    const d = Math.floor(seconds / (3600 * 24));
    const h = Math.floor((seconds % (3600 * 24)) / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    const s = seconds % 60;

    const parts = [];
    if (d > 0) parts.push(`${d}${t('admin:dashboard.uptime.days', { defaultValue: 'd' })}`);
    if (h > 0) parts.push(`${h}${t('admin:dashboard.uptime.hours', { defaultValue: 'h' })}`);
    if (m > 0) parts.push(`${m}${t('admin:dashboard.uptime.minutes', { defaultValue: 'm' })}`);
    parts.push(`${s}${t('admin:dashboard.uptime.seconds', { defaultValue: 's' })}`);
    return parts.join(' ');
  };

  const startRecoveryPolling = async () => {
    setRestartStep('polling');
    let delay = 500;
    const maxAttempts = 15;

    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      setPollingAttempts(attempt);
      try {
        const data = await adminFetch<{ status: string }>('/admin/health');
        if (data.status === 'healthy' || data.status === 'degraded') {
          setRestartStep('completed');
          showToast(t('admin:dashboard.success.restarted', { defaultValue: 'Gateway successfully restarted and recovered!' }), 'success');
          setTimeout(() => {
            setIsRestarting(false);
            setRestartStep('idle');
            fetchAllMetrics();
          }, 1500);
          return;
        }
      } catch {
        // Gateway is currently down / starting up
      }

      await new Promise((r) => setTimeout(r, delay));
      delay = Math.min(delay * 1.5, 6000);
    }

    setRestartStep('failed');
    showToast(t('admin:dashboard.error.timeout', { defaultValue: 'Gateway restart polling timed out.' }), 'error');
    setTimeout(() => {
      setIsRestarting(false);
      setRestartStep('idle');
      fetchAllMetrics();
    }, 3000);
  };

  const handleRestartGateway = async () => {
    const confirmed = await confirm(
      t('admin:dashboard.confirm.restart_title', { defaultValue: 'Restart HotPlex Gateway?' }),
      t('admin:dashboard.confirm.restart_body', { defaultValue: 'This will safely fork a detached process helper, flush existing HTTP connections, reload configuration, and reboot. Active WebSocket clients will temporarily disconnect.' }),
      {
        confirmLabel: t('admin:dashboard.action.restart', { defaultValue: 'Restart Gateway' }),
        cancelLabel: t('common:action.cancel', { defaultValue: 'Cancel' }),
        destructive: true,
      }
    );

    if (!confirmed) return;

    try {
      setIsRestarting(true);
      setRestartStep('initiating');

      await adminFetch<{ status: string }>('/admin/restart', {
        method: 'POST',
      });

      showToast(t('admin:dashboard.status.acknowledged', { defaultValue: 'Restart command acknowledged. Waiting for offline drop...' }), 'info');

      setRestartStep('offline');
      await new Promise((r) => setTimeout(r, 1200));

      await startRecoveryPolling();

    } catch (err) {
      setIsRestarting(false);
      setRestartStep('idle');
      showToast(err instanceof Error ? err.message : t('admin:dashboard.error.restart_failed', { defaultValue: 'Restart request failed' }), 'error');
    }
  };

  return (
    <div className="min-h-screen bg-[var(--bg-base)] px-6 py-8">
      <div className="max-w-6xl mx-auto space-y-8">
        
        {/* Top Header */}
        <div className="flex flex-col md:flex-row md:items-center justify-between gap-4">
          <div>
            <div className="flex items-center gap-3">
              <h1 className="text-2xl font-display font-bold text-[var(--text-primary)]">
                {t('admin:dashboard.title', { defaultValue: 'Dashboard' })}
              </h1>
              <span className="px-2.5 py-0.5 rounded-full text-xs font-mono font-bold bg-[var(--accent-gold)]/10 text-[var(--accent-gold)] border border-[var(--accent-gold)]/20">
                {metrics.version}
              </span>
            </div>
            <p className="mt-1 text-xs text-[var(--text-muted)]">
              {t('admin:dashboard.subtitle', { defaultValue: 'Gateway command center and real-time operational status' })}
            </p>
          </div>

          <div className="flex items-center gap-2.5">
            <button
              type="button"
              onClick={fetchAllMetrics}
              disabled={loading || isRestarting}
              className="p-2 rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)] text-[var(--text-muted)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)] transition-all disabled:opacity-50"
              title={t('admin:sessions.action.refresh', { defaultValue: 'Refresh' })}
            >
              <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={1.5} stroke="currentColor" className={`w-4 h-4 ${loading ? 'animate-spin' : ''}`}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0 3.181 3.183a8.25 8.25 0 0 0 13.803-3.7M4.031 9.865a8.25 8.25 0 0 1 13.803-3.7l3.181 3.182m0-4.991v4.99" />
              </svg>
            </button>

            {!isRestarting && (
              <button
                type="button"
                onClick={handleRestartGateway}
                disabled={loading || !metrics.gatewayOnline}
                className="px-4 py-2 text-xs font-bold rounded-[var(--radius-md)] border border-[var(--accent-gold)]/40 bg-[var(--bg-glass)] text-[var(--accent-gold)] hover:bg-[var(--accent-gold)] hover:text-[var(--text-contrast)] disabled:opacity-50 disabled:cursor-not-allowed hover:shadow-[var(--shadow-glow)] transition-all duration-300"
              >
                {t('admin:dashboard.action.restart_gateway', { defaultValue: 'Restart Go Gateway' })}
              </button>
            )}
          </div>
        </div>

        {/* Error banner */}
        {error && !isRestarting && (
          <div className="rounded-[var(--radius-md)] bg-[rgba(244,63,94,0.08)] border border-[rgba(244,63,94,0.15)] p-4">
            <p className="text-sm font-medium text-[var(--accent-coral)]">{error}</p>
          </div>
        )}

        {/* Reboot Lifecycle Panel Overlay */}
        {isRestarting && (
          <div className="rounded-[var(--radius-lg)] border border-[var(--border-active)] bg-[var(--bg-glass)] backdrop-blur-xl p-6 shadow-[var(--shadow-lg)] animate-fade-in-up space-y-4">
            <div className="flex items-center justify-between border-b border-[var(--border-subtle)] pb-4">
              <div className="flex items-center gap-3">
                <div className="w-2.5 h-2.5 rounded-full bg-[var(--accent-gold)] animate-pulse" />
                <h2 className="text-sm font-display font-bold text-[var(--text-primary)]">
                  {t('admin:dashboard.lifecycle.title', { defaultValue: 'Gateway Reboot Lifecycle' })}
                </h2>
              </div>
              <span className="text-[10px] font-mono text-[var(--text-faint)]">
                {t('admin:dashboard.lifecycle.pgid_active', { defaultValue: 'PGID Restart Handler Active' })}
              </span>
            </div>

            <div className="grid grid-cols-1 md:grid-cols-4 gap-4">
              <div className={`p-3 rounded-[var(--radius-md)] border transition-all ${restartStep === 'initiating' ? 'border-[var(--accent-gold)] bg-white/5' : 'border-[var(--border-subtle)] opacity-60'}`}>
                <span className="text-[10px] uppercase font-bold text-[var(--text-faint)]">{t('admin:dashboard.lifecycle.step1', { defaultValue: 'Step 1' })}</span>
                <span className="text-xs font-semibold text-[var(--text-primary)] block mt-1">{t('admin:dashboard.lifecycle.step1_title', { defaultValue: 'Initiating Handshake' })}</span>
                <p className="text-[10px] text-[var(--text-muted)] mt-1">{t('admin:dashboard.lifecycle.step1_desc', { defaultValue: 'Triggering POST /restart...' })}</p>
              </div>

              <div className={`p-3 rounded-[var(--radius-md)] border transition-all ${restartStep === 'offline' ? 'border-[var(--accent-gold)] bg-white/5' : 'border-[var(--border-subtle)] opacity-60'}`}>
                <span className="text-[10px] uppercase font-bold text-[var(--text-faint)]">{t('admin:dashboard.lifecycle.step2', { defaultValue: 'Step 2' })}</span>
                <span className="text-xs font-semibold text-[var(--text-primary)] block mt-1">{t('admin:dashboard.lifecycle.step2_title', { defaultValue: 'Connection Drop' })}</span>
                <p className="text-[10px] text-[var(--text-muted)] mt-1">{t('admin:dashboard.lifecycle.step2_desc', { defaultValue: 'Gracefully flushing sockets...' })}</p>
              </div>

              <div className={`p-3 rounded-[var(--radius-md)] border transition-all ${restartStep === 'polling' ? 'border-[var(--accent-gold)] bg-white/5 animate-pulse' : 'border-[var(--border-subtle)] opacity-60'}`}>
                <span className="text-[10px] uppercase font-bold text-[var(--text-faint)]">{t('admin:dashboard.lifecycle.step3', { defaultValue: 'Step 3' })}</span>
                <span className="text-xs font-semibold text-[var(--text-primary)] block mt-1">
                  {t('admin:dashboard.lifecycle.step3_title', { defaultValue: 'Polling Health' })} {pollingAttempts > 0 && `(x${pollingAttempts})`}
                </span>
                <p className="text-[10px] text-[var(--text-muted)] mt-1">{t('admin:dashboard.lifecycle.step3_desc', { defaultValue: 'Backing off recovery checks...' })}</p>
              </div>

              <div className={`p-3 rounded-[var(--radius-md)] border transition-all ${restartStep === 'completed' ? 'border-[var(--accent-emerald)] bg-white/5' : 'border-[var(--border-subtle)] opacity-60'}`}>
                <span className="text-[10px] uppercase font-bold text-[var(--text-faint)]">{t('admin:dashboard.lifecycle.step4', { defaultValue: 'Step 4' })}</span>
                <span className="text-xs font-semibold text-[var(--accent-emerald)] block mt-1">{t('admin:dashboard.lifecycle.step4_title', { defaultValue: 'Gateway Online' })}</span>
                <p className="text-[10px] text-[var(--text-muted)] mt-1">{t('admin:dashboard.lifecycle.step4_desc', { defaultValue: 'Dashboard metrics synced.' })}</p>
              </div>
            </div>
          </div>
        )}

        {/* Gateway Control & Live Uptime Banner */}
        {!loading && !isRestarting && (
          <div className="rounded-[var(--radius-lg)] border border-[var(--border-subtle)] bg-[var(--bg-surface)] p-6 shadow-sm">
            <div className="flex flex-col lg:flex-row lg:items-center justify-between gap-6">
              <div className="flex flex-col gap-1">
                <span className="text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">
                  {t('admin:dashboard.uptime.label', { defaultValue: 'Gateway Live Uptime' })}
                </span>
                <span className="text-4xl font-display font-bold text-[var(--text-primary)] tracking-tight tabular-nums">
                  {metrics.gatewayOnline ? formatUptime(uptime) : t('admin:dashboard.offline', { defaultValue: 'Offline' })}
                </span>
                <span className="text-xs text-[var(--text-muted)] flex items-center gap-2 mt-1">
                  <span className={`w-2 h-2 rounded-full inline-block ${
                    metrics.gatewayOnline && metrics.dbStatus === 'healthy'
                      ? 'bg-[var(--accent-emerald)] shadow-[0_0_8px_var(--accent-emerald)] animate-pulse'
                      : metrics.gatewayOnline
                      ? 'bg-[var(--accent-gold)] shadow-[0_0_8px_var(--accent-gold)]'
                      : 'bg-[var(--accent-coral)]'
                  }`} />
                  {metrics.gatewayOnline
                    ? t('admin:dashboard.uptime.active', { version: metrics.version, defaultValue: `Active (Version ${metrics.version})` })
                    : t('admin:dashboard.uptime.unreachable', { defaultValue: 'Unreachable' })}
                </span>
              </div>

              <div className="border-t lg:border-t-0 lg:border-l border-[var(--border-subtle)] pt-6 lg:pt-0 lg:pl-8 flex-1">
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 text-xs">
                  <div>
                    <span className="text-[10px] uppercase tracking-wider font-bold text-[var(--text-faint)] block mb-1">
                      {t('admin:dashboard.database.sqlite_label', { defaultValue: 'Database Path' })}
                    </span>
                    <span className="font-mono text-[var(--text-secondary)] break-all bg-[var(--bg-hover)] px-2.5 py-1 rounded-[var(--radius-xs)] select-all block border border-[var(--border-subtle)]">
                      {metrics.dbPath || 'sqlite.db'}
                    </span>
                  </div>

                  <div>
                    <span className="text-[10px] uppercase tracking-wider font-bold text-[var(--text-faint)] block mb-1">
                      {t('admin:dashboard.database.status_label', { defaultValue: 'Database Health' })}
                    </span>
                    <span className={`font-bold ${
                      metrics.dbStatus === 'healthy'
                        ? 'text-[var(--accent-emerald)]'
                        : 'text-[var(--accent-gold)]'
                    }`}>
                      {metrics.dbStatus === 'healthy' ? t('admin:dashboard.database.healthy', { defaultValue: 'Healthy (SQLite Core)' }) : t('admin:dashboard.database.degraded', { defaultValue: 'Degraded / Standby' })}
                    </span>
                  </div>
                </div>
              </div>
            </div>
          </div>
        )}

        {/* Loading Spinner */}
        {loading && !isRestarting && (
          <div className="flex items-center justify-center py-20">
            <div className="flex flex-col items-center gap-3">
              <div className="w-7 h-7 border-2 border-[var(--accent-gold)] border-t-transparent rounded-full animate-spin" />
              <span className="text-xs font-medium text-[var(--text-faint)]">
                {t('admin:dashboard.loading', { defaultValue: 'Loading dashboard metrics...' })}
              </span>
            </div>
          </div>
        )}

        {/* 5 Executive Metric Cards Grid */}
        {!loading && (
          <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-5 gap-4">
            {/* Bots */}
            <MetricCard
              label={t('admin:dashboard.metrics.bots_label', { defaultValue: 'Active Agents / Bots' })}
              value={metrics.botsTotal}
              sub={t('admin:dashboard.metrics.bots_sub', {
                count: metrics.botsConnected,
                standby: metrics.botsDisconnected,
                defaultValue: `${metrics.botsConnected} connected, ${metrics.botsDisconnected} standby`,
              })}
            />

            {/* Sessions Pool */}
            <MetricCard
              label={t('admin:dashboard.metrics.sessions_label', { defaultValue: 'Session Pool Capacity' })}
              value={metrics.sessionsActive}
              sub={t('admin:dashboard.metrics.sessions_sub', {
                active: metrics.sessionsActive,
                max: metrics.sessionsPoolMax,
                total: metrics.sessionsTotal,
                defaultValue: `${metrics.sessionsActive} active / ${metrics.sessionsPoolMax} max`,
              })}
            />

            {/* Cron Jobs */}
            <MetricCard
              label={t('admin:dashboard.metrics.cron_label', { defaultValue: 'Scheduled Jobs (Cron)' })}
              value={metrics.cronTotal}
              sub={t('admin:dashboard.metrics.cron_sub', {
                count: metrics.cronEnabled,
                defaultValue: `${metrics.cronEnabled} scheduler loops enabled`,
              })}
            />

            {/* Workspaces */}
            <MetricCard
              label={t('admin:dashboard.metrics.workspaces_label', { defaultValue: 'Workspaces' })}
              value={metrics.workspacesTotal}
              sub={t('admin:dashboard.metrics.workspaces_sub', {
                count: metrics.workspacesTotal,
                defaultValue: `${metrics.workspacesTotal} workspaces mounted`,
              })}
            />

            {/* Audit Log Events */}
            <MetricCard
              label={t('admin:dashboard.metrics.audit_label', { defaultValue: 'Audit Events' })}
              value={metrics.auditTotal}
              sub={t('admin:dashboard.metrics.audit_sub', {
                success: metrics.auditSuccess,
                denied: metrics.auditDenied,
                defaultValue: `${metrics.auditSuccess} pass / ${metrics.auditDenied} denied`,
              })}
            />
          </div>
        )}

        {/* Middle Section: Worker Engines + Live Audit Stream */}
        {!loading && (
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6">
            
            {/* Left: Worker Engines Status */}
            <div className="p-6 rounded-[var(--radius-lg)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] shadow-sm space-y-4">
              <h2 className="text-xs font-bold text-[var(--text-faint)] uppercase tracking-wider">
                {t('admin:dashboard.workers_health.title', { defaultValue: 'Worker Engine Runtimes' })}
              </h2>

              <div className="space-y-3 text-xs">
                {[
                  { name: 'Claude Code Engine', key: 'claudecode', defaultActive: true },
                  { name: 'OpenCode Server', key: 'opencode_server', defaultActive: false },
                  { name: 'Codex CLI', key: 'codex_cli', defaultActive: false },
                  { name: 'ACP Agent Runtime', key: 'acp', defaultActive: false },
                ].map((w) => {
                  const check = metrics.workerChecks[w.key];
                  const isHealthy = check?.status === 'ok' || check?.status === 'healthy' || w.defaultActive;
                  return (
                    <div key={w.key} className="p-3 rounded-[var(--radius-sm)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] flex items-center justify-between">
                      <span className="font-bold text-[var(--text-primary)] font-mono">{w.name}</span>
                      <span className={`inline-flex items-center gap-1.5 px-2 py-0.5 rounded text-[10px] font-bold ${isHealthy ? 'bg-emerald-500/10 text-emerald-400 border border-emerald-500/20' : 'bg-amber-500/10 text-amber-400 border border-amber-500/20'}`}>
                        <span className={`w-1.5 h-1.5 rounded-full ${isHealthy ? 'bg-emerald-400 animate-pulse' : 'bg-amber-400'}`} />
                        {isHealthy ? t('admin:dashboard.workers_health.status_running', { defaultValue: 'Running' }) : t('admin:dashboard.workers_health.status_standby', { defaultValue: 'Standby' })}
                      </span>
                    </div>
                  );
                })}
              </div>
            </div>

            {/* Right: Live Audit Stream */}
            <div className="p-6 rounded-[var(--radius-lg)] bg-[var(--bg-surface)] border border-[var(--border-subtle)] shadow-sm flex flex-col justify-between space-y-4">
              <div className="flex items-center justify-between">
                <h2 className="text-xs font-bold text-[var(--text-faint)] uppercase tracking-wider">
                  {t('admin:dashboard.activity_stream.title', { defaultValue: 'Live Audit Activity Feed' })}
                </h2>
                <Link
                  href="/admin/activity"
                  className="text-xs font-bold text-[var(--accent-gold)] hover:underline"
                >
                  {t('admin:dashboard.activity_stream.view_all', { defaultValue: 'View All →' })}
                </Link>
              </div>

              <div className="space-y-2 flex-1">
                {recentActivities.length > 0 ? (
                  recentActivities.map((act) => (
                    <div key={`${act.chain_epoch ?? 'legacy'}:${act.id}`} className="p-2.5 rounded-[var(--radius-sm)] bg-[var(--bg-hover)] border border-[var(--border-subtle)] flex items-center justify-between text-xs">
                      <div className="min-w-0 pr-2">
                        <div className="flex items-center gap-2">
                          <span className="font-mono font-bold text-[var(--text-primary)] truncate">{act.action}</span>
                          <span className={`px-1.5 py-0.2 rounded text-[9px] font-bold ${act.outcome === 'success' ? 'bg-emerald-500/10 text-emerald-400' : 'bg-rose-500/10 text-rose-400'}`}>
                            {act.outcome}
                          </span>
                        </div>
                        <span className="text-[10px] text-[var(--text-faint)] font-mono block truncate">
                          User: {act.user_id} • {act.platform}
                        </span>
                      </div>
                      <span className="text-[10px] font-mono text-[var(--text-faint)] shrink-0">
                        {formatDateTime(new Date(act.ts).toISOString())}
                      </span>
                    </div>
                  ))
                ) : (
                  <p className="text-xs text-[var(--text-faint)] italic p-4 text-center">
                    {t('admin:dashboard.activity_stream.no_recent_activity', { defaultValue: 'No recent audit activity' })}
                  </p>
                )}
              </div>
            </div>
          </div>
        )}

      </div>
    </div>
  );
}
