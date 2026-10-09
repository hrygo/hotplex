'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import Link from 'next/link';
import { motion, AnimatePresence } from 'framer-motion';

import { StatusBadge } from '@/components/admin/status-badge';
import { ActionIcon, actionCategory } from '@/components/admin/activity/action-icon';
import { JsonViewer } from '@/components/admin/activity/json-viewer';
import { OUTCOME_STATUS_MAP } from '@/components/admin/activity/outcome-map';
import { LoadingState, ErrorState, EmptyState } from '@/components/admin/resource-states';
import { useAdminUI } from '@/context/admin-ui-context';
import { downloadActivity, listActivity, listActivityStats, type ActivityFilters } from '@/lib/api/admin-activity';
import { adminListUsers, type User } from '@/lib/api/auth';
import type { ActivityStats, AuditActivity } from '@/lib/types/admin';
import { formatDateTime, formatRelative } from '@/lib/utils/format-time';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';

type OutcomeFilter = '' | 'success' | 'failure' | 'denied';

// Action options grouped by category for the filter dropdown. The values are
// sent verbatim as action_prefix (the category root + ".") so selecting
// "Tool Call" returns every tool.* action. A specific action can still be
// typed in the user_id-style box via the action input when needed.
const ACTION_GROUPS: Array<{ groupKey: string; prefix: string }> = [
  { groupKey: 'auth', prefix: 'auth.' },
  { groupKey: 'session', prefix: 'session.' },
  { groupKey: 'message', prefix: 'message.' },
  { groupKey: 'tool', prefix: 'tool.' },
  { groupKey: 'permission', prefix: 'permission.' },
  { groupKey: 'question', prefix: 'question.' },
  { groupKey: 'elicitation', prefix: 'elicitation.' },
  { groupKey: 'admin', prefix: 'admin.' },
  { groupKey: 'system', prefix: 'system.' },
];

const PLATFORM_OPTIONS = ['webchat', 'feishu', 'slack', 'admin', 'api', 'cron'];

function truncate(value: string, size = 18): string {
  if (!value || value.length <= size) return value;
  return `${value.slice(0, Math.max(0, size - 7))}...${value.slice(-4)}`;
}

// detailSummary renders a single-line, action-aware digest for the table cell.
// Each action has a differently-shaped detail_json (see backend builders), so
// we pull the most informative field per category rather than dumping keys.
function detailSummary(row: AuditActivity, t: TFunction<'admin'>): string {
  if (!row.detail_json) return '';
  let parsed: Record<string, unknown>;
  try {
    parsed = JSON.parse(row.detail_json);
    if (typeof parsed !== 'object' || parsed === null) return row.detail_json;
  } catch {
    return row.detail_json;
  }
  const get = (k: string) => (typeof parsed[k] === 'string' ? (parsed[k] as string) : '');
  switch (actionCategory(row.action)) {
    case 'message': {
      const content = get('content');
      if (content) return content;
      return '';
    }
    case 'tool': {
      const name = get('name');
      const title = get('title');
      if (name && title) return `${name} · ${title}`;
      return name || JSON.stringify(parsed).slice(0, 80);
    }
    case 'permission': {
      if (row.action === 'permission.request') {
        const toolName = get('tool_name') || get('tool');
        const desc = get('description');
        const text = toolName ? `请求工具授权 · ${toolName}` : '请求授权';
        return desc ? `${text} · ${desc}` : text;
      }
      const allowed = parsed.allowed;
      const reason = get('reason');
      const text = allowed === true ? '允许授权' : allowed === false ? '拒绝授权' : '权限响应';
      return reason ? `${text} · ${reason}` : text;
    }
    case 'question': {
      const answers = parsed.answers;
      if (answers && typeof answers === 'object') {
        const vals = Object.values(answers).filter(Boolean);
        if (vals.length > 0) return vals.join(', ');
      }
      return '回答问题';
    }
    case 'elicitation': {
      const act = get('action');
      return act ? `MCP 响应 · ${act}` : 'MCP 响应';
    }
    case 'auth':
      return `${get('method') || ''} ${get('path') || ''}`.trim();
    case 'session': {
      const wt = get('worker_type');
      return wt ? t('activity.summary.worker_prefix', { type: wt }) : '';
    }
    case 'admin':
      return `${get('method') || ''} ${get('path') || ''} → ${get('status') || ''}`.trim();
    case 'system': {
      const fmt = get('format');
      const rows = parsed.rows as number | undefined;
      if (fmt) return rows !== undefined ? `${fmt} · ${t('activity.summary.rows', { count: rows })}` : fmt;
      return '';
    }
    default:
      return Object.entries(parsed)
        .slice(0, 2)
        .map(([k, v]) => `${k}=${String(v)}`)
        .join(' · ');
  }
}

export default function AdminActivityPage() {
  const { t } = useTranslation('admin');
  const { t: tCommon } = useTranslation('common');
  const { showToast } = useAdminUI();

  const [rows, setRows] = useState<AuditActivity[]>([]);
  const [stats, setStats] = useState<ActivityStats | null>(null);
  const [resolvedUserIDs, setResolvedUserIDs] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [exporting, setExporting] = useState<'json' | 'csv' | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [drawerRow, setDrawerRow] = useState<AuditActivity | null>(null);
  const [copiedField, setCopiedField] = useState('');
  const [identityLinks, setIdentityLinks] = useState<Record<string, any>>({});

  // User picker state for user_id filter
  const [allUsers, setAllUsers] = useState<User[]>([]);
  const [showUserPicker, setShowUserPicker] = useState(false);
  const [userSearch, setUserSearch] = useState('');
  const userPickerRef = useRef<HTMLDivElement>(null);

  const messageContent = useMemo(() => {
    if (!drawerRow || !drawerRow.detail_json) return '';
    try {
      const parsed = JSON.parse(drawerRow.detail_json);
      if (parsed && typeof parsed.content === 'string') {
        return parsed.content;
      }
    } catch {}
    return '';
  }, [drawerRow]);

  const [userId, setUserId] = useState('');
  const [selectedUserLabel, setSelectedUserLabel] = useState('');
  const [principalUserId, setPrincipalUserId] = useState('');
  const [actionPrefix, setActionPrefix] = useState('');
  const [outcome, setOutcome] = useState<OutcomeFilter>('');
  const [platform, setPlatform] = useState('');
  const [from, setFrom] = useState('');
  const [to, setTo] = useState('');

  const [currentPage, setCurrentPage] = useState(1);
  const [pageSize] = useState(50);

  // Reset page when filter search params change (avoid out-of-bounds offset queries)
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setCurrentPage(1);
  }, [userId, principalUserId, actionPrefix, outcome, platform, from, to]);

  const filters = useMemo<ActivityFilters>(
    () => ({
      userId: userId.trim(),
      principalUserId: principalUserId.trim(),
      actionPrefix: actionPrefix.trim(),
      outcome,
      platform,
      from,
      to,
      limit: pageSize,
      offset: (currentPage - 1) * pageSize,
    }),
    [actionPrefix, from, outcome, platform, principalUserId, to, userId, pageSize, currentPage],
  );

  const load = useCallback(async () => {
    try {
      setLoading(true);
      setError(null);
      const [data, statsData] = await Promise.all([listActivity(filters), listActivityStats(filters)]);
      setRows(data.rows ?? []);
      setResolvedUserIDs(data.resolved_user_ids ?? []);
      setIdentityLinks(data.identity_links ?? {});
      setStats(statsData);
    } catch (err) {
      setError(err instanceof Error ? err.message : t('activity.error.load_failed'));
    } finally {
      setLoading(false);
    }
  }, [filters, t]);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- initial remote load
    load();
  }, [load]);

  // Fetch all users once for the user picker dropdown
  useEffect(() => {
    const controller = new AbortController();
    adminListUsers(1000, 0, controller.signal)
      .then((res) => setAllUsers(res.users ?? []))
      .catch(() => {}); // ignore errors — picker is optional
    return () => controller.abort();
  }, []);

  // Close user picker on outside click
  useEffect(() => {
    if (!showUserPicker) return;
    const onClick = (e: MouseEvent) => {
      if (userPickerRef.current && !userPickerRef.current.contains(e.target as Node)) {
        setShowUserPicker(false);
      }
    };
    document.addEventListener('mousedown', onClick);
    return () => document.removeEventListener('mousedown', onClick);
  }, [showUserPicker]);

  // Esc closes the drawer (matches the sessions page pattern).
  useEffect(() => {
    if (!drawerRow) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setDrawerRow(null);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [drawerRow]);

  const handleExport = async (format: 'json' | 'csv') => {
    try {
      setExporting(format);
      await downloadActivity(filters, format);
      showToast(t('activity.toast.exported'), 'success');
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('activity.error.export_failed'), 'error');
    } finally {
      setExporting(null);
    }
  };

  const copyText = async (text: string, field: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopiedField(field);
      setTimeout(() => setCopiedField(''), 1500);
    } catch {
      showToast(t('activity.error.export_failed'), 'error');
    }
  };

  const tableHeaders = [
    t('activity.table.time'),
    t('activity.table.user'),
    t('activity.table.platform'),
    t('activity.table.action'),
    t('activity.table.outcome'),
    t('activity.table.detail'),
  ];

  const successCount = stats?.by_outcome?.success ?? 0;
  const failureCount = stats?.by_outcome?.failure ?? 0;
  const deniedCount = stats?.by_outcome?.denied ?? 0;
  const totalCount = stats?.total ?? 0;

  const totalPages = Math.ceil(totalCount / pageSize);

  const renderPageNumbers = () => {
    if (totalPages <= 1) return null;

    const pages: (number | string)[] = [];
    const maxVisible = 5;

    if (totalPages <= maxVisible) {
      for (let i = 1; i <= totalPages; i++) {
        pages.push(i);
      }
    } else {
      pages.push(1);
      if (currentPage > 3) {
        pages.push('...');
      }

      const start = Math.max(2, currentPage - 1);
      const end = Math.min(totalPages - 1, currentPage + 1);

      for (let i = start; i <= end; i++) {
        pages.push(i);
      }

      if (currentPage < totalPages - 2) {
        pages.push('...');
      }
      pages.push(totalPages);
    }

    return pages.map((p, idx) => {
      if (p === '...') {
        return (
          <span key={`dots-${idx}`} className="px-1 text-[var(--text-faint)] select-none">
            ...
          </span>
        );
      }
      const active = p === currentPage;
      return (
        <button
          key={`page-${p}`}
          type="button"
          onClick={() => setCurrentPage(Number(p))}
          className={`w-6 h-6 rounded text-[11px] font-mono font-bold flex items-center justify-center transition-all ${
            active
              ? 'bg-[var(--accent-gold)]/10 text-[var(--accent-gold)] border border-[var(--accent-gold)]/20 shadow-sm'
              : 'bg-[var(--bg-surface)] border border-[var(--border-subtle)] text-[var(--text-secondary)] hover:bg-[var(--bg-hover)]'
          }`}
        >
          {p}
        </button>
      );
    });
  };

  return (
    <div className="min-h-screen bg-[var(--bg-base)] p-6">
      <div className="max-w-7xl mx-auto">
        {/* Header */}
        <div className="flex flex-col gap-4 md:flex-row md:items-center md:justify-between mb-5">
          <div className="flex items-center gap-3">
            <h1 className="text-xl font-display font-bold text-[var(--text-primary)]">{t('activity.title')}</h1>
            {!loading && !error && (
              <span className="text-[11px] font-mono text-[var(--text-faint)] px-2 py-0.5 rounded-full bg-[var(--bg-hover)]">
                {totalCount}
              </span>
            )}
          </div>
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => handleExport('json')}
              disabled={!!exporting || loading}
              className="px-3 py-1.5 rounded-[var(--radius-sm)] border border-[var(--border-subtle)] text-xs text-[var(--text-secondary)] hover:bg-[var(--bg-hover)] disabled:opacity-40"
            >
              {exporting === 'json' ? t('activity.action.exporting') : t('activity.action.export_json')}
            </button>
            <button
              type="button"
              onClick={() => handleExport('csv')}
              disabled={!!exporting || loading}
              className="px-3 py-1.5 rounded-[var(--radius-sm)] border border-[var(--border-subtle)] text-xs text-[var(--text-secondary)] hover:bg-[var(--bg-hover)] disabled:opacity-40"
            >
              {exporting === 'csv' ? t('activity.action.exporting') : t('activity.action.export_csv')}
            </button>
            <button
              type="button"
              onClick={load}
              disabled={loading}
              className="inline-flex items-center justify-center w-8 h-8 rounded-[var(--radius-sm)] border border-[var(--border-subtle)] text-[var(--text-faint)] hover:text-[var(--text-secondary)] hover:bg-[var(--bg-hover)] disabled:opacity-50"
              title={tCommon('action.refresh')}
            >
              <svg
                width="14"
                height="14"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                strokeLinecap="round"
                strokeLinejoin="round"
                className={loading ? 'animate-spin' : ''}
              >
                <path d="M21 2v6h-6" />
                <path d="M3 12a9 9 0 0 1 15-6.7L21 8" />
                <path d="M3 22v-6h6" />
                <path d="M21 12a9 9 0 0 1-15 6.7L3 16" />
              </svg>
            </button>
          </div>
        </div>

        {/* Stats overview row */}
        {!error && (
          <div className="mb-5 grid grid-cols-2 md:grid-cols-4 gap-3">
            <StatChip label={t('activity.stats.total')} value={totalCount} accent="gold" />
            <StatChip label={t('activity.stats.success')} value={successCount} accent="emerald" />
            <StatChip label={t('activity.stats.failure')} value={failureCount} accent="coral" />
            <StatChip label={t('activity.stats.denied')} value={deniedCount} accent="amber" />
          </div>
        )}

        {/* Filters */}
        <div className="mb-6 p-4 rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/40 backdrop-blur-sm grid grid-cols-1 md:grid-cols-2 xl:grid-cols-7 gap-3">
          <div className="relative group flex items-center" ref={userPickerRef}>
            <input
              value={selectedUserLabel || userId}
              onChange={(e) => {
                setUserId(e.target.value);
                setSelectedUserLabel('');
                if (e.target.value.length > 0) setShowUserPicker(true);
              }}
              onFocus={() => { if (allUsers.length > 0) setShowUserPicker(true); }}
              placeholder={t('activity.filters.user_id')}
              className="w-full rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/80 backdrop-blur-sm pl-3 pr-16 py-2 text-xs text-[var(--text-primary)] outline-none transition-all focus:border-[var(--accent-gold)] focus:ring-2 focus:ring-[var(--accent-gold)]/20"
            />
            {selectedUserLabel && (
              <button
                type="button"
                onClick={() => { setUserId(''); setSelectedUserLabel(''); }}
                className="absolute right-8 text-[var(--text-faint)] hover:text-[var(--text-primary)] transition-colors"
                title={tCommon('action.clear')}
              >
                <svg className="w-3 h-3" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2.5}>
                  <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            )}
            <button
              type="button"
              onClick={() => setShowUserPicker((v) => !v)}
              className="absolute right-2.5 text-[var(--text-faint)] hover:text-[var(--text-secondary)] transition-colors"
              title={t('activity.filters.pick_user')}
            >
              <svg className="w-3.5 h-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2.5}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M15.75 6a3.75 3.75 0 11-7.5 0 3.75 3.75 0 017.5 0zM4.501 20.118a7.5 7.5 0 0114.998 0A17.933 17.933 0 0112 21.75c-2.676 0-5.216-.584-7.499-1.632z" />
              </svg>
            </button>
            {/* User picker dropdown */}
            {showUserPicker && allUsers.length > 0 && (
              <div className="absolute z-40 top-full left-0 mt-1 w-full max-h-64 overflow-y-auto rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/95 backdrop-blur-sm shadow-xl">
                <div className="sticky top-0 p-1.5 bg-[var(--bg-surface)]/95 backdrop-blur-sm border-b border-[var(--border-subtle)]/50">
                  <input
                    value={userSearch}
                    onChange={(e) => setUserSearch(e.target.value)}
                    placeholder={t('activity.filters.search_user')}
                    className="w-full rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-hover)]/40 px-2.5 py-1.5 text-[11px] text-[var(--text-primary)] outline-none focus:border-[var(--accent-gold)]/40"
                    autoFocus
                  />
                </div>
                {allUsers
                  .filter((u) => {
                    if (!userSearch.trim()) return true;
                    const q = userSearch.toLowerCase();
                    return (
                      (u.display_name || '').toLowerCase().includes(q) ||
                      u.username.toLowerCase().includes(q) ||
                      u.id.toLowerCase().includes(q)
                    );
                  })
                  .map((u) => (
                    <button
                      key={u.id}
                      type="button"
                      onClick={() => {
                        setUserId(u.id);
                        setSelectedUserLabel(u.display_name || u.username);
                        setShowUserPicker(false);
                        setUserSearch('');
                      }}
                      className="w-full text-left px-3 py-2 flex items-center gap-2 hover:bg-[var(--bg-hover)]/60 transition-colors border-b border-[var(--border-subtle)]/20 last:border-b-0"
                    >
                      <div className="w-5 h-5 rounded-full bg-[var(--accent-gold)]/10 border border-[var(--accent-gold)]/20 flex items-center justify-center shrink-0">
                        <span className="text-[9px] font-bold text-[var(--accent-gold)] uppercase">
                          {(u.display_name || u.username || '?')[0]}
                        </span>
                      </div>
                      <div className="min-w-0 flex-1">
                        <div className="text-xs font-medium text-[var(--text-primary)] truncate">
                          {u.display_name || u.username}
                        </div>
                        <div className="text-[10px] font-mono text-[var(--text-faint)] truncate">
                          {u.username} · {truncate(u.id, 16)}
                        </div>
                      </div>
                      <span className={`text-[9px] px-1 py-0.5 rounded border leading-none font-medium ${
                        u.role === 'admin'
                          ? 'bg-[var(--accent-gold)]/10 text-[var(--accent-gold)] border-[var(--accent-gold)]/20'
                          : 'bg-[var(--bg-hover)]/60 text-[var(--text-faint)] border-[var(--border-subtle)]/40'
                      }`}>
                        {u.role}
                      </span>
                    </button>
                  ))}
              </div>
            )}
          </div>
          <div className="relative group flex items-center">
            <input
              value={principalUserId}
              onChange={(e) => setPrincipalUserId(e.target.value)}
              placeholder={t('activity.filters.principal_user_id')}
              className="w-full rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/80 backdrop-blur-sm pl-3 pr-8 py-2 text-xs text-[var(--text-primary)] outline-none transition-all focus:border-[var(--accent-gold)] focus:ring-2 focus:ring-[var(--accent-gold)]/20"
            />
            <div className="absolute right-2.5 text-[var(--text-faint)] hover:text-[var(--text-secondary)] cursor-help">
              <svg className="w-3.5 h-3.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2.5}>
                <path strokeLinecap="round" strokeLinejoin="round" d="M9.879 7.519c1.171-1.025 3.071-1.025 4.242 0 1.172 1.025 1.172 2.687 0 3.712-.203.179-.43.326-.67.442-.745.361-1.45.999-1.45 1.827v.75M21 12a9 9 0 11-18 0 9 9 0 0118 0zm-9 5.25h.008v.008H12v-.008z" />
              </svg>
            </div>
            <div className="absolute z-30 bottom-full left-0 mb-2 hidden group-hover:block w-64 p-3 bg-[var(--bg-surface)]/95 backdrop-blur-sm border border-[var(--border-subtle)] rounded-lg shadow-xl text-[11px] text-[var(--text-secondary)] leading-normal pointer-events-none transition-all duration-200">
              {t('activity.hints.principal_tooltip')}
            </div>
          </div>
          <select
            value={actionPrefix}
            onChange={(e) => setActionPrefix(e.target.value)}
            className="rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/80 backdrop-blur-sm px-3 py-2 text-xs text-[var(--text-primary)] outline-none transition-all focus:border-[var(--accent-gold)] focus:ring-2 focus:ring-[var(--accent-gold)]/20"
          >
            <option value="">{t('activity.filters.any_action')}</option>
            {ACTION_GROUPS.map((g) => (
              <option key={g.prefix} value={g.prefix}>
                {t(`activity.action_groups.${g.groupKey}`, { defaultValue: g.groupKey })}
              </option>
            ))}
          </select>
          <select
            value={platform}
            onChange={(e) => setPlatform(e.target.value)}
            className="rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/80 backdrop-blur-sm px-3 py-2 text-xs text-[var(--text-primary)] outline-none transition-all focus:border-[var(--accent-gold)] focus:ring-2 focus:ring-[var(--accent-gold)]/20"
          >
            <option value="">{t('activity.filters.any_platform')}</option>
            {PLATFORM_OPTIONS.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
          <select
            value={outcome}
            onChange={(e) => setOutcome(e.target.value as OutcomeFilter)}
            className="rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/80 backdrop-blur-sm px-3 py-2 text-xs text-[var(--text-primary)] outline-none transition-all focus:border-[var(--accent-gold)] focus:ring-2 focus:ring-[var(--accent-gold)]/20"
          >
            <option value="">{t('activity.filters.any_outcome')}</option>
            <option value="success">{t('activity.outcomes.success')}</option>
            <option value="failure">{t('activity.outcomes.failure')}</option>
            <option value="denied">{t('activity.outcomes.denied')}</option>
          </select>
          <div className="xl:col-span-2 flex gap-2">
            <div className="flex-1 min-w-0 relative flex items-center">
              <span className="absolute left-2.5 text-[9px] uppercase font-bold text-[var(--text-faint)] pointer-events-none tracking-wider">
                {t('activity.filters.from_short')}
              </span>
              <input
                type="datetime-local"
                value={from}
                onChange={(e) => setFrom(e.target.value)}
                className="w-full rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/80 backdrop-blur-sm pl-8 pr-2 py-2 text-[11px] text-[var(--text-primary)] outline-none transition-all focus:border-[var(--accent-gold)] focus:ring-2 focus:ring-[var(--accent-gold)]/20"
              />
            </div>
            <div className="flex-1 min-w-0 relative flex items-center">
              <span className="absolute left-2.5 text-[9px] uppercase font-bold text-[var(--text-faint)] pointer-events-none tracking-wider">
                {t('activity.filters.to_short')}
              </span>
              <input
                type="datetime-local"
                value={to}
                onChange={(e) => setTo(e.target.value)}
                className="w-full rounded-[var(--radius-sm)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]/80 backdrop-blur-sm pl-8 pr-2 py-2 text-[11px] text-[var(--text-primary)] outline-none transition-all focus:border-[var(--accent-gold)] focus:ring-2 focus:ring-[var(--accent-gold)]/20"
              />
            </div>
          </div>
        </div>

        {resolvedUserIDs.length > 0 && (
          <div className="mb-5 text-xs text-[var(--text-muted)]">
            {t('activity.resolved_ids')}: <span className="font-mono text-[var(--text-secondary)]">{resolvedUserIDs.join(', ')}</span>
          </div>
        )}

        {loading && <LoadingState label={t('activity.loading')} />}
        {error && <ErrorState message={error} onRetry={load} />}
        {!loading && !error && rows.length === 0 && (
          <EmptyState title={t('activity.empty.title')} description={t('activity.empty.description')} />
        )}

        {/* Table */}
        {!loading && !error && rows.length > 0 && (
          <div className="overflow-hidden rounded-[var(--radius-md)] border border-[var(--border-subtle)] bg-[var(--bg-surface)]">
            <div className="grid grid-cols-[140px_1.4fr_100px_1.5fr_100px_2.5fr] gap-3 px-4 py-3 bg-[var(--bg-hover)]/60 border-b border-[var(--border-subtle)]">
              {tableHeaders.map((label) => (
                <div key={label} className="text-[10px] font-mono font-bold text-[var(--text-faint)] uppercase tracking-widest">
                  {label}
                </div>
              ))}
            </div>
            {rows.map((row) => {
              const rowEpoch = row.chain_epoch ?? 'legacy';
              const selected = drawerRow?.id === row.id && (drawerRow.chain_epoch ?? 'legacy') === rowEpoch;
              return (
                <button
                  key={`${rowEpoch}:${row.id}`}
                  type="button"
                  onClick={() => setDrawerRow(row)}
                  className={`w-full text-left grid grid-cols-[140px_1.4fr_100px_1.5fr_100px_2.5fr] items-center gap-3 px-4 py-2 border-t border-[var(--border-subtle)] transition-all duration-200 hover:translate-x-0.5 odd:bg-[var(--bg-surface)]/10 even:bg-[var(--bg-surface)]/40 hover:bg-[var(--bg-hover)]/30 ${
                    selected ? 'bg-[var(--bg-active)] border-l-2 border-l-[var(--accent-gold)]' : ''
                  }`}
                >
                  <div className="text-[10px] font-mono text-[var(--text-faint)]" title={formatDateTime(row.ts)}>
                    {formatRelative(row.ts)}
                  </div>
                  <div className="min-w-0">
                    {identityLinks[row.user_id] ? (
                      <>
                        <div className="text-xs font-bold text-[var(--text-primary)] truncate" title={identityLinks[row.user_id].display_name || identityLinks[row.user_id].DisplayName || row.user_id}>
                          {identityLinks[row.user_id].display_name || identityLinks[row.user_id].DisplayName}
                        </div>
                        <div className="text-[10px] font-mono text-[var(--text-faint)] truncate" title={row.user_id}>
                          {truncate(row.user_id, 16)}
                        </div>
                      </>
                    ) : (
                      <>
                        <div className="text-xs font-mono text-[var(--text-primary)] truncate" title={row.user_id}>
                          {truncate(row.user_id, 24)}
                        </div>
                        <div className="text-[10px] text-[var(--text-faint)]">{row.user_id_type}</div>
                      </>
                    )}
                  </div>
                  <div>
                    <PlatformBadge platform={row.platform} />
                  </div>
                  <div className="min-w-0 flex items-center gap-1.5">
                    <ActionIcon action={row.action} />
                    <div className="min-w-0">
                      <div className="text-xs font-mono text-[var(--text-primary)] truncate" title={row.action}>
                        {row.action}
                      </div>
                      {row.session_id && (
                        <Link
                          href={`/admin/sessions/detail?id=${encodeURIComponent(row.session_id)}`}
                          onClick={(e) => e.stopPropagation()}
                          className="text-[10px] text-[var(--accent-gold)] hover:underline truncate block max-w-[160px]"
                          title={row.session_id}
                        >
                          {truncate(row.session_id, 16)}
                        </Link>
                      )}
                      {(row.resource_type || row.resource_id) && !row.session_id && (
                        <div className="text-[10px] text-[var(--text-faint)] truncate">
                          {row.resource_type}
                          {row.resource_id ? `:${truncate(row.resource_id, 16)}` : ''}
                        </div>
                      )}
                    </div>
                  </div>
                  <div>
                    <StatusBadge status={row.outcome} map={OUTCOME_STATUS_MAP} />
                  </div>
                  <div className="text-[11px] text-[var(--text-muted)] truncate flex items-center gap-1.5" title={detailSummary(row, t)}>
                    {actionCategory(row.action) === 'message' ? (
                      <span className="shrink-0 flex items-center gap-1 px-1.5 py-0.5 rounded bg-[var(--accent-gold)]/10 text-[var(--accent-gold)] border border-[var(--accent-gold)]/20 text-[9px] font-medium leading-none">
                        <svg className="w-2.5 h-2.5" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2.5}>
                          <path strokeLinecap="round" strokeLinejoin="round" d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z" />
                        </svg>
                        {t('activity.table.msg_badge')}
                      </span>
                    ) : null}
                    <span className="truncate">{detailSummary(row, t)}</span>
                  </div>
                </button>
              );
            })}
          </div>
        )}

        {/* Pagination */}
        {!loading && !error && rows.length > 0 && totalPages > 1 && (
          <div className="mt-4 px-4 py-2 flex items-center justify-between border border-[var(--border-subtle)] rounded-[var(--radius-md)] bg-[var(--bg-surface)]/40 backdrop-blur-sm text-[11px] text-[var(--text-secondary)] shadow-sm">
            <div className="flex items-center gap-1 text-[var(--text-muted)]">
              <span>{t('activity.pagination.showing')}</span>
              <span className="font-mono font-bold text-[var(--text-primary)]">
                {Math.min(totalCount, (currentPage - 1) * pageSize + 1)}
              </span>
              <span>{t('activity.pagination.to')}</span>
              <span className="font-mono font-bold text-[var(--text-primary)]">
                {Math.min(totalCount, currentPage * pageSize)}
              </span>
              <span>{t('activity.pagination.of')}</span>
              <span className="font-mono font-bold text-[var(--text-primary)]">{totalCount}</span>
              <span>{t('activity.pagination.entries')}</span>
            </div>

            <div className="flex items-center gap-1.5">
              <button
                type="button"
                disabled={currentPage === 1}
                onClick={() => setCurrentPage((prev) => Math.max(1, prev - 1))}
                className="px-2.5 py-1 rounded bg-[var(--bg-surface)] border border-[var(--border-subtle)] text-[10px] font-bold uppercase tracking-wider text-[var(--text-secondary)] disabled:opacity-40 disabled:cursor-not-allowed hover:bg-[var(--bg-hover)] transition-all duration-200"
              >
                {t('activity.pagination.prev')}
              </button>

              <div className="flex items-center gap-1">
                {renderPageNumbers()}
              </div>

              <button
                type="button"
                disabled={currentPage >= totalPages}
                onClick={() => setCurrentPage((prev) => Math.min(totalPages, prev + 1))}
                className="px-2.5 py-1 rounded bg-[var(--bg-surface)] border border-[var(--border-subtle)] text-[10px] font-bold uppercase tracking-wider text-[var(--text-secondary)] disabled:opacity-40 disabled:cursor-not-allowed hover:bg-[var(--bg-hover)] transition-all duration-200"
              >
                {t('activity.pagination.next')}
              </button>
            </div>
          </div>
        )}

        {/* Detail Drawer */}
        <AnimatePresence>
          {drawerRow && (
            <div className="fixed inset-0 z-50 overflow-hidden" role="dialog" aria-modal="true">
              <motion.div
                initial={{ opacity: 0 }}
                animate={{ opacity: 1 }}
                exit={{ opacity: 0 }}
                transition={{ duration: 0.2 }}
                className="absolute inset-0 bg-black/40 backdrop-blur-sm"
                onClick={() => setDrawerRow(null)}
              />
              <div className="absolute inset-y-0 right-0 max-w-full flex">
                <motion.div
                  initial={{ x: '100%' }}
                  animate={{ x: 0 }}
                  exit={{ x: '100%' }}
                  transition={{ type: 'spring', damping: 26, stiffness: 220 }}
                  className="relative w-screen max-w-md bg-[var(--bg-surface)] border-l border-[var(--border-subtle)] shadow-2xl flex flex-col"
                >
                  {/* Header */}
                  <div className="px-6 pt-7 pb-4 border-b border-[var(--border-subtle)] flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <ActionIcon action={drawerRow.action} className="px-0" />
                        <h2 className="text-sm font-bold text-[var(--text-faint)] uppercase tracking-wider font-mono truncate">
                          {drawerRow.action}
                        </h2>
                      </div>
                      <div className="mt-2 flex items-center gap-2">
                        <StatusBadge status={drawerRow.outcome} map={OUTCOME_STATUS_MAP} />
                        <span className="text-[11px] text-[var(--text-faint)]" title={formatDateTime(drawerRow.ts)}>
                          {formatRelative(drawerRow.ts)}
                        </span>
                      </div>
                    </div>
                    <button
                      type="button"
                      onClick={() => setDrawerRow(null)}
                      className="shrink-0 p-1.5 rounded-full text-[var(--text-faint)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)] transition-all"
                    >
                      <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={2} stroke="currentColor" className="w-5 h-5">
                        <path strokeLinecap="round" strokeLinejoin="round" d="M6 18L18 6M6 6l12 12" />
                      </svg>
                    </button>
                  </div>

                  {/* Body */}
                  <div className="flex-1 overflow-y-auto px-4 py-4 space-y-3.5">
                    {/* Message Content Section */}
                    {messageContent && (
                      <DrawerSection
                        title={t('activity.drawer.message_content')}
                        action={
                          <button
                            type="button"
                            onClick={() => copyText(messageContent, 'content')}
                            className="text-[9px] font-bold uppercase text-[var(--accent-gold)] hover:underline"
                          >
                            {copiedField === 'content' ? t('activity.drawer.copied') : tCommon('action.copy')}
                          </button>
                        }
                      >
                        <div className="p-3 rounded-md bg-[var(--accent-gold)]/[0.02] border border-[var(--accent-gold)]/20 text-[11px] text-[var(--text-primary)] whitespace-pre-wrap break-all leading-normal font-sans mt-1">
                          {messageContent}
                        </div>
                      </DrawerSection>
                    )}

                    {/* Identity */}
                    <DrawerSection title={t('activity.drawer.identity')}>
                      {identityLinks[drawerRow.user_id] && (
                        <>
                          <KV label={t('activity.drawer.field.name')} value={identityLinks[drawerRow.user_id].display_name || identityLinks[drawerRow.user_id].DisplayName || '—'} />
                          {(identityLinks[drawerRow.user_id].email || identityLinks[drawerRow.user_id].Email) && (
                            <KV label={t('activity.drawer.field.email')} value={identityLinks[drawerRow.user_id].email || identityLinks[drawerRow.user_id].Email || ''} />
                          )}
                          <KV label={t('activity.drawer.field.principal')} value={identityLinks[drawerRow.user_id].principal_user_id || identityLinks[drawerRow.user_id].PrincipalUserID || '—'} mono />
                        </>
                      )}
                      <KV label={t('activity.table.user')} value={drawerRow.user_id} mono copyable />
                      <KV label={t('activity.drawer.field.type')} value={drawerRow.user_id_type} />
                      <KV label={t('activity.table.platform')} value={drawerRow.platform} />
                    </DrawerSection>

                    {/* Context */}
                    <DrawerSection title={t('activity.drawer.context')}>
                      {drawerRow.session_id ? (
                        <KV
                          label={t('activity.table.session')}
                          value={drawerRow.session_id}
                          mono
                          link={`/admin/sessions/detail?id=${encodeURIComponent(drawerRow.session_id)}`}
                        />
                      ) : (
                        <KV label={t('activity.table.session')} value="—" />
                      )}
                      {(drawerRow.resource_type || drawerRow.resource_id) && (
                        <KV
                          label={t('activity.drawer.field.resource')}
                          value={`${drawerRow.resource_type || ''}${drawerRow.resource_id ? ':' + drawerRow.resource_id : ''}`}
                          mono
                        />
                      )}
                      {drawerRow.event_ref && <KV label={t('activity.drawer.field.event_ref')} value={drawerRow.event_ref} mono />}
                      <KV label={t('activity.table.time')} value={formatDateTime(drawerRow.ts)} />
                    </DrawerSection>

                    {/* Network */}
                    <DrawerSection title={t('activity.drawer.network')}>
                      <KV label={t('activity.drawer.field.ip')} value={drawerRow.ip || '—'} mono hint={drawerRow.ip ? t('activity.drawer.ip_masked') : undefined} />
                      {drawerRow.user_agent && <KV label={t('activity.drawer.field.user_agent')} value={drawerRow.user_agent} mono />}
                    </DrawerSection>

                    {/* Detail JSON */}
                    <DrawerSection
                      title={t('activity.drawer.detail_json')}
                      action={
                        drawerRow.detail_json && drawerRow.detail_json !== '{}' && (
                          <button
                            type="button"
                            onClick={() => copyText(drawerRow.detail_json || '', 'json')}
                            className="text-[9px] font-bold uppercase text-[var(--accent-gold)] hover:underline"
                          >
                            {copiedField === 'json' ? t('activity.drawer.copied') : t('activity.drawer.copy_json')}
                          </button>
                        )
                      }
                    >
                      <div className="mt-1">
                        <JsonViewer json={drawerRow.detail_json} />
                      </div>
                    </DrawerSection>

                    <DrawerSection title={t('activity.drawer.hash_chain')} hint={t('activity.drawer.hash_hint')}>
                      <div className="mb-3 flex items-center gap-2 text-xs">
                        <span className="text-[var(--text-faint)]">{t('activity.drawer.chain_epoch')}</span>
                        <span className="rounded border border-[var(--border-subtle)] px-2 py-0.5 font-mono text-[var(--text-secondary)]">
                          {drawerRow.chain_epoch ?? 'legacy'}
                        </span>
                      </div>
                      <div className="flex flex-col gap-4 relative pl-3 before:absolute before:left-[17px] before:top-2 before:bottom-2 before:w-[2px] before:bg-[color-mix(in_srgb,var(--border-subtle)_70%,transparent)]">
                        <div className="flex items-start gap-3 relative">
                          <div className="w-2.5 h-2.5 rounded-full border-2 border-[var(--border-subtle)] bg-[var(--bg-surface)] mt-1.5 z-10 shrink-0" />
                          <div className="flex-1 min-w-0">
                            <div className="text-[9px] font-bold text-[var(--text-faint)] uppercase tracking-wider">{t('activity.drawer.prev_hash')}</div>
                            <div className="text-xs font-mono text-[var(--text-secondary)] break-all bg-[var(--bg-hover)]/30 px-3 py-1.5 rounded-md border border-[var(--border-subtle)]/40 mt-1" title={drawerRow.prev_hash || '—'}>
                              {drawerRow.prev_hash || '—'}
                            </div>
                          </div>
                        </div>
                        <div className="flex items-start gap-3 relative">
                          <div className="w-2.5 h-2.5 rounded-full border-2 border-[var(--accent-gold)] bg-[var(--bg-surface)] mt-1.5 z-10 shrink-0 shadow-[0_0_8px_rgba(218,165,32,0.3)]" />
                          <div className="flex-1 min-w-0">
                            <div className="text-[9px] font-bold text-[var(--accent-gold)] uppercase tracking-wider flex items-center gap-1">
                              <span>{t('activity.drawer.self_hash')}</span>
                              <svg className="w-3 h-3 text-[var(--accent-gold)] animate-pulse" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2.5}>
                                <path strokeLinecap="round" strokeLinejoin="round" d="M16.5 10.5V6.75a4.5 4.5 0 10-9 0v3.75m-.75 11.25h10.5a2.25 2.25 0 002.25-2.25v-6.75a2.25 2.25 0 00-2.25-2.25H6.75a2.25 2.25 0 00-2.25 2.25v6.75a2.25 2.25 0 002.25 2.25z" />
                              </svg>
                            </div>
                            <div className="text-xs font-mono text-[var(--text-primary)] break-all bg-[var(--accent-gold)]/5 px-3 py-1.5 rounded-md border border-[var(--accent-gold)]/20 mt-1 shadow-[inset_0_1px_2px_rgba(0,0,0,0.05)]" title={drawerRow.self_hash}>
                              {drawerRow.self_hash}
                            </div>
                          </div>
                        </div>
                      </div>
                    </DrawerSection>
                  </div>
 
                  {/* Footer */}
                  {drawerRow.session_id && (
                    <div className="p-5 border-t border-[var(--border-subtle)] bg-[var(--bg-hover)]/30">
                      <Link
                        href={`/admin/sessions/detail?id=${encodeURIComponent(drawerRow.session_id)}`}
                        className="w-full inline-flex items-center justify-center gap-2 px-4 py-3 rounded-[var(--radius-md)] text-[11px] font-bold uppercase tracking-widest text-[var(--accent-gold)] bg-[var(--accent-gold)]/[0.03] border border-[var(--accent-gold)]/20 hover:bg-[var(--accent-gold)]/[0.08] hover:border-[var(--accent-gold)]/40 hover:shadow-[0_0_12px_rgba(218,165,32,0.1)] transition-all duration-200 text-center"
                      >
                        <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" strokeWidth={2.2} stroke="currentColor" className="w-3.5 h-3.5">
                          <path strokeLinecap="round" strokeLinejoin="round" d="M13.5 6H5.25A2.25 2.25 0 0 0 3 8.25v10.5A2.25 2.25 0 0 0 5.25 21h10.5A2.25 2.25 0 0 0 18 18.75V10.5m-10.5 6L21 3m0 0h-5.25M21 3v5.25" />
                        </svg>
                        {t('activity.drawer.open_session')}
                      </Link>
                    </div>
                  )}
                </motion.div>
              </div>
            </div>
          )}
        </AnimatePresence>
      </div>
    </div>
  );
}

// StatChip — compact stat card for the overview row. Local to this page to
// keep the surface small; reuse MetricCard if richer affordances are needed.
function StatChip({ label, value, accent }: { label: string; value: number; accent: 'gold' | 'emerald' | 'coral' | 'amber' }) {
  const color =
    accent === 'gold'
      ? 'text-[var(--accent-gold)]'
      : accent === 'emerald'
        ? 'text-[var(--accent-emerald)]'
        : accent === 'coral'
          ? 'text-[var(--accent-coral)]'
          : 'text-[var(--accent-amber)]';

  const getIcon = () => {
    switch (accent) {
      case 'gold':
        return (
          <svg className="w-5 h-5 opacity-60 text-[var(--accent-gold)]" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2m-6 9l2 2 4-4" />
          </svg>
        );
      case 'emerald':
        return (
          <svg className="w-5 h-5 opacity-60 text-[var(--accent-emerald)]" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" />
          </svg>
        );
      case 'coral':
        return (
          <svg className="w-5 h-5 opacity-60 text-[var(--accent-coral)]" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M10 14l2-2m0 0l2-2m-2 2l-2-2m2 2l2 2m7-2a9 9 0 11-18 0 9 9 0 0118 0z" />
          </svg>
        );
      case 'amber':
        return (
          <svg className="w-5 h-5 opacity-60 text-[var(--accent-amber)]" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
            <path strokeLinecap="round" strokeLinejoin="round" d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z" />
          </svg>
        );
    }
  };

  const glowBg =
    accent === 'gold'
      ? 'hover:shadow-[0_0_20px_rgba(218,165,32,0.06)] border-[var(--accent-gold)]/20'
      : accent === 'emerald'
        ? 'hover:shadow-[0_0_20px_rgba(16,185,129,0.06)] border-[var(--accent-emerald)]/20'
        : accent === 'coral'
          ? 'hover:shadow-[0_0_20px_rgba(239,68,68,0.06)] border-[var(--accent-coral)]/20'
          : 'hover:shadow-[0_0_20px_rgba(245,158,11,0.06)] border-[var(--accent-amber)]/20';

  return (
    <motion.div
      whileHover={{ y: -2, scale: 1.01 }}
      transition={{ type: 'spring', stiffness: 400, damping: 17 }}
      className={`rounded-[var(--radius-md)] border bg-[var(--bg-surface)]/80 backdrop-blur-sm px-5 py-4 flex items-center justify-between transition-all duration-300 ${glowBg}`}
    >
      <div>
        <div className="text-[10px] font-bold uppercase tracking-wider text-[var(--text-faint)]">{label}</div>
        <div className={`text-2xl font-mono font-bold mt-1.5 ${color}`}>{value}</div>
      </div>
      <div className="p-2 rounded-lg bg-[var(--bg-hover)]/40 border border-[var(--border-subtle)]/40 shrink-0">
        {getIcon()}
      </div>
    </motion.div>
  );
}

// DrawerSection — titled block inside the drawer body.
function DrawerSection({
  title,
  hint,
  children,
  action,
}: {
  title: string;
  hint?: string;
  children: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <div className="px-4 py-3.5 rounded-[var(--radius-md)] bg-[var(--bg-surface)]/60 backdrop-blur-md border border-[var(--border-subtle)] space-y-2.5 shadow-[0_2px_8px_rgba(0,0,0,0.01)]">
      <div className="flex items-center justify-between border-b border-[var(--border-subtle)]/30 pb-2 mb-1">
        <div className="flex items-center gap-1.5">
          <span className="text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-wider">{title}</span>
          {hint && <span className="text-[9px] text-[var(--text-faint)] italic">· {hint}</span>}
        </div>
        {action && <div className="shrink-0">{action}</div>}
      </div>
      <div className="space-y-1">{children}</div>
    </div>
  );
}

// KV — a single key/value row inside a DrawerSection. Supports mono font,
// optional copy, optional link, and an optional hint tooltip.
function KV({
  label,
  value,
  mono,
  link,
  copyable,
  hint,
}: {
  label: string;
  value: string;
  mono?: boolean;
  link?: string;
  copyable?: boolean;
  hint?: string;
}) {
  const { t } = useTranslation('common');
  const [kvCopied, setKvCopied] = useState(false);

  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setKvCopied(true);
      setTimeout(() => setKvCopied(false), 1500);
    } catch {}
  };

  return (
    <div className="flex items-center justify-between gap-4 py-1 border-b border-[var(--border-subtle)]/30 last:border-b-0 transition-all hover:bg-[var(--bg-hover)]/[0.04] -mx-2 px-2 rounded-[var(--radius-sm)]">
      <span className="text-[10px] font-mono font-bold text-[var(--text-faint)] uppercase tracking-wider shrink-0 min-w-[95px] text-left">{label}</span>
      <div className="min-w-0 flex-1 text-right">
        <div className="inline-flex items-center justify-end gap-1.5 flex-nowrap w-full">
          {link ? (
            <Link href={link} className={`text-[11px] ${mono ? 'font-mono' : ''} text-[var(--accent-gold)] hover:underline truncate block max-w-[220px] text-right`} title={value}>
              {value}
            </Link>
          ) : (
            <span className={`text-[11px] ${mono ? 'font-mono' : ''} text-[var(--text-secondary)] truncate block max-w-[220px] text-right`} title={value}>
              {value}
            </span>
          )}
          {hint && <span className="text-[9px] text-[var(--text-faint)] italic shrink-0">({hint})</span>}
          {copyable && (
            <button type="button" onClick={handleCopy} className="shrink-0 text-[9px] font-bold uppercase text-[var(--accent-gold)] hover:underline ml-1">
              {kvCopied ? t('action.copied') : t('action.copy')}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

// PlatformBadge — a beautifully styled micro badge for communication platforms.
function PlatformBadge({ platform }: { platform: string }) {
  const lower = platform.toLowerCase();
  let classes = "bg-[var(--bg-hover)]/60 text-[var(--text-muted)] border-[var(--border-subtle)]/60";
  
  if (lower === 'feishu') {
    classes = "bg-blue-500/10 text-blue-400 border-blue-500/20";
  } else if (lower === 'slack') {
    classes = "bg-purple-500/10 text-purple-400 border-purple-500/20";
  } else if (lower === 'webchat') {
    classes = "bg-[var(--accent-gold)]/10 text-[var(--accent-gold)] border-[var(--accent-gold)]/20";
  } else if (lower === 'cron') {
    classes = "bg-emerald-500/10 text-emerald-400 border-emerald-500/20";
  } else if (lower === 'admin') {
    classes = "bg-amber-500/10 text-amber-400 border-amber-500/20";
  } else if (lower === 'api') {
    classes = "bg-cyan-500/10 text-cyan-400 border-cyan-500/20";
  }

  return (
    <span className={`inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-semibold border leading-none shrink-0 ${classes}`}>
      {platform}
    </span>
  );
}
