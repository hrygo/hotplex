"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import {
    formatRelativeTime,
    stateLabel,
    type SessionInfo,
} from "@/lib/api/sessions";
import { WORKER_DISPLAY, SearchIcon, WorkerIcon } from "@/components/icons";
import { useTranslation } from "react-i18next";

function getDisplayTitle(session: SessionInfo): string {
    return session.title || session.id.slice(0, 8);
}

function SessionRow({
    session,
    isActive,
    onSelect,
    onDelete,
}: {
    session: SessionInfo;
    isActive: boolean;
    onSelect: () => void;
    onDelete: () => void;
}) {
    const { t } = useTranslation(['chat', 'common']);
    const [confirmDelete, setConfirmDelete] = useState(false);
    const displayTitle = getDisplayTitle(session);
    const workerName =
        WORKER_DISPLAY[session.worker_type] ?? session.worker_type;
    const hasTurns =
        typeof session.turn_count === "number" && session.turn_count > 0;

    return (
        <div
            role="button"
            tabIndex={0}
            onClick={onSelect}
            onKeyDown={(e) => e.key === "Enter" && onSelect()}
            className={`group relative mx-2 mb-2 p-3.5 rounded-[var(--radius-md)] border transition-all duration-300 cursor-pointer overflow-hidden ${
                isActive
                    ? "bg-[var(--bg-active)] border-[var(--border-active)] shadow-[var(--shadow-glow)]"
                    : "bg-[var(--bg-surface)] border-[var(--border-subtle)] hover:bg-[var(--bg-hover)] hover:border-[var(--border-default)]"
            }`}
        >
            <div className="flex flex-col gap-2.5">
                {/* Title — primary */}
                <h3
                    className={`text-[13px] font-bold leading-snug line-clamp-2 ${isActive ? "text-[var(--text-primary)]" : "text-[var(--text-secondary)]"}`}
                >
                    {displayTitle}
                </h3>

                {/* Worker + turn count */}
                <div className="flex items-center justify-between gap-2">
                    <div className="flex items-center gap-1.5 min-w-0">
                        <div
                            className={`p-1 rounded-[var(--radius-xs)] transition-colors duration-300 shrink-0 ${isActive ? "bg-[var(--accent-gold)] text-[var(--text-contrast)]" : "bg-[var(--bg-elevated)] text-[var(--text-secondary)]"}`}
                        >
                            <WorkerIcon
                                type={session.worker_type}
                                className="w-3 h-3"
                            />
                        </div>
                        <span
                            className={`text-[11px] font-display font-bold uppercase tracking-wider truncate ${isActive ? "text-[var(--text-primary)]" : "text-[var(--text-muted)]"}`}
                        >
                            {workerName}
                        </span>
                    </div>
                    {hasTurns && (
                        <div
                            className={`flex items-center gap-1 text-[10px] font-mono font-medium shrink-0 ${isActive ? "text-[var(--text-muted)]" : "text-[var(--text-faint)]"}`}
                        >
                            <svg
                                className="w-3 h-3"
                                fill="none"
                                stroke="currentColor"
                                viewBox="0 0 24 24"
                            >
                                <path
                                    strokeLinecap="round"
                                    strokeLinejoin="round"
                                    strokeWidth={2}
                                    d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z"
                                />
                            </svg>
                            <span>{session.turn_count} {t('chat:label.turns')}</span>
                        </div>
                    )}
                </div>

                {/* Footer */}
                <div className="flex items-center justify-between pt-2 border-t border-[var(--border-subtle)]">
                    <div className="flex items-center gap-2 min-w-0">
                        <div className="flex items-center gap-1.5">
                            <div
                                className={`w-1.5 h-1.5 rounded-full ${
                                    session.state === "running"
                                        ? "bg-[var(--accent-emerald)] shadow-[0_0_8px_var(--accent-emerald)] animate-pulse"
                                        : session.state === "idle"
                                          ? "bg-[var(--accent-gold)]"
                                          : session.state === "terminated"
                                            ? "bg-[var(--text-faint)]"
                                            : "bg-[var(--accent-blue)]"
                                }`}
                            />
                            <span
                                className={`text-[11px] font-bold ${isActive ? "text-[var(--text-primary)]" : "text-[var(--text-muted)]"}`}
                            >
                                {t('chat:status.' + session.state, { defaultValue: stateLabel(session.state) })}
                            </span>
                            {session.archived && (
                                <span className="rounded px-1.5 py-0.5 text-[9px] font-bold uppercase tracking-wide text-[var(--text-muted)] bg-[var(--bg-elevated)]">
                                    {t('chat:status.archived')}
                                </span>
                            )}
                        </div>
                        <span className="text-[10px] text-[var(--text-faint)] opacity-40">
                            •
                        </span>
                        <span
                            className={`text-[11px] font-medium truncate ${isActive ? "text-[var(--text-secondary)]" : "text-[var(--text-faint)]"}`}
                        >
                            {formatRelativeTime(session.updated_at)}
                        </span>
                    </div>

                    <div className="flex items-center">
                        {confirmDelete ? (
                            <div className="flex items-center gap-2 animate-fade-in">
                                <button
                                    type="button"
                                    onClick={(e) => {
                                        e.stopPropagation();
                                        onDelete();
                                    }}
                                    className="text-[10px] font-bold text-[var(--accent-coral)] hover:underline"
                                >
                                    {t('common:action.confirm')}
                                </button>
                                <button
                                    type="button"
                                    onClick={(e) => {
                                        e.stopPropagation();
                                        setConfirmDelete(false);
                                    }}
                                    className="text-[10px] font-bold text-[var(--text-faint)] hover:text-[var(--text-secondary)]"
                                >
                                    {t('common:action.cancel')}
                                </button>
                            </div>
                        ) : (
                            <button
                                type="button"
                                onClick={(e) => {
                                    e.stopPropagation();
                                    setConfirmDelete(true);
                                }}
                                className="opacity-0 group-hover:opacity-100 p-1.5 rounded-[var(--radius-sm)] text-[var(--text-faint)] hover:text-[var(--accent-coral)] hover:bg-[var(--accent-coral)]/10 transition-all transform hover:scale-110"
                                title={t('chat:action.delete_session')}
                            >
                                <svg
                                    className="w-3.5 h-3.5"
                                    fill="none"
                                    stroke="currentColor"
                                    viewBox="0 0 24 24"
                                >
                                    <path
                                        strokeLinecap="round"
                                        strokeLinejoin="round"
                                        strokeWidth={2}
                                        d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16"
                                    />
                                </svg>
                            </button>
                        )}
                    </div>
                </div>
            </div>
        </div>
    );
}

interface SessionPanelProps {
    sessions: SessionInfo[];
    activeSession: SessionInfo | null;
    isLoading: boolean;
    onSelect: (session: SessionInfo) => void;
    onCreate: () => void;
    onDelete: (id: string) => Promise<void>;
    currentUserRole?: "admin" | "user";
}

export function SessionPanel({
    sessions,
    activeSession,
    isLoading,
    onSelect,
    onCreate,
    onDelete,
    currentUserRole,
}: SessionPanelProps) {
    const { t } = useTranslation(['chat', 'common']);
    const [searchQuery, setSearchQuery] = useState("");

    const filteredSessions = useMemo(
        () =>
            sessions
                .filter((s) =>
                    getDisplayTitle(s)
                        .toLowerCase()
                        .includes(searchQuery.toLowerCase()),
                )
                .sort(
                    (a, b) =>
                        new Date(b.updated_at).getTime() -
                        new Date(a.updated_at).getTime(),
                ),
        [sessions, searchQuery],
    );

    return (
        <div className="pc-sidebar flex flex-col h-full bg-[var(--bg-base)] border-r border-[var(--border-subtle)] w-[280px]">
            {/* Sidebar Header */}
            <div className="px-5 pt-6 pb-4">
                {/* New Session Button */}
                <button
                    onClick={() => onCreate()}
                    disabled={isLoading}
                    className="w-full flex items-center justify-center gap-2 py-2.5 rounded-xl bg-[var(--accent-gold)] text-[var(--bg-base)] hover:bg-[var(--accent-gold-bright)] hover:text-black active:scale-95 transition-all shadow-[0_4px_16px_rgba(251,191,36,0.15)] font-bold text-xs disabled:opacity-50 disabled:cursor-not-allowed group/newchat"
                >
                    {isLoading ? (
                        <div className="w-4 h-4 border-2 border-white/30 border-t-white rounded-full animate-spin" />
                    ) : (
                        <svg
                            className="w-4 h-4"
                            fill="none"
                            stroke="currentColor"
                            viewBox="0 0 24 24"
                        >
                            <path
                                strokeLinecap="round"
                                strokeLinejoin="round"
                                strokeWidth={2}
                                d="M12 4v16m8-8H4"
                            />
                        </svg>
                    )}
                    {isLoading ? t("common:action.creating") : t("chat:action.new_chat")}
                </button>
            </div>

            {/* Search */}
            <div className="px-5 mb-4">
                <div className="relative">
                    <SearchIcon className="absolute left-3 top-1/2 -translate-y-1/2 w-3.5 h-3.5 text-[var(--text-faint)]" />
                    <input
                        id="session-search"
                        name="session-search"
                        type="text"
                        aria-label={t("chat:placeholder.search")}
                        placeholder={t("chat:placeholder.search")}
                        value={searchQuery}
                        onChange={(e) => setSearchQuery(e.target.value)}
                        className="w-full bg-[var(--bg-elevated)] border border-transparent rounded-[var(--radius-md)] pl-9 pr-4 py-2 text-xs text-[var(--text-primary)] transition-all placeholder:text-[var(--text-faint)] session-search-input"
                    />
                </div>
            </div>

            {/* List */}
            <div className="flex-1 overflow-y-auto px-2 pb-6 custom-scrollbar">
                <div className="px-3 mb-2 text-[10px] font-bold text-[var(--text-faint)] uppercase tracking-widest">
                    {t("chat:title.recent_conversations")}
                </div>

                <div className="space-y-1 session-list-cascade">
                    {filteredSessions.map((session) => (
                        <SessionRow
                            key={session.id}
                            session={session}
                            isActive={activeSession?.id === session.id}
                            onSelect={() => onSelect(session)}
                            onDelete={() => onDelete(session.id)}
                        />
                    ))}

                    {filteredSessions.length === 0 && !isLoading && (
                        <div className="px-3 py-10 text-center flex flex-col items-center gap-2">
                            <SearchIcon className="w-6 h-6 text-[var(--text-faint)] opacity-50" strokeWidth={1.5} />
                            <p className="text-[11px] text-[var(--text-faint)]">
                                {t("chat:text.no_results")}
                            </p>
                        </div>
                    )}

                    {isLoading && (
                        <div className="px-3 py-4 flex justify-center">
                            <div className="w-4 h-4 border-2 border-[var(--accent-gold)] border-t-transparent rounded-full animate-spin" />
                        </div>
                    )}
                </div>
            </div>

            {/* Admin Entry — admin-only (spec G1, issue #788) */}
            {currentUserRole === "admin" && (
                <div className="mt-auto flex-shrink-0 border-t border-[var(--border-subtle)] p-3">
                    <Link
                        href="/admin"
                        className="flex items-center gap-3 w-full px-3 py-2.5 rounded-xl text-[var(--text-faint)] hover:text-[var(--text-primary)] hover:bg-[var(--bg-hover)] transition-all duration-200 group/admin"
                        title={t("chat:action.admin_dashboard")}
                    >
                        <div className="flex items-center justify-center w-7 h-7 rounded-lg bg-[var(--bg-elevated)] group-hover/admin:bg-[var(--accent-gold)]/15 transition-colors duration-200">
                            <svg
                                className="w-3.5 h-3.5 group-hover/admin:text-[var(--accent-gold)] transition-colors duration-200"
                                fill="none"
                                stroke="currentColor"
                                viewBox="0 0 24 24"
                            >
                                <path
                                    strokeLinecap="round"
                                    strokeLinejoin="round"
                                    strokeWidth={2}
                                    d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.066 2.573c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.573 1.066c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.066-2.573c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"
                                />
                                <path
                                    strokeLinecap="round"
                                    strokeLinejoin="round"
                                    strokeWidth={2}
                                    d="M15 12a3 3 0 11-6 0 3 3 0 016 0z"
                                />
                            </svg>
                        </div>
                        <div className="flex flex-col">
                            <span className="text-[11px] font-semibold leading-tight">
                                {t("chat:label.admin")}
                            </span>
                            <span className="text-[9px] text-[var(--text-faint)] leading-tight">
                                {t("chat:label.admin_subtitle")}
                            </span>
                        </div>
                    </Link>
                </div>
            )}
        </div>
    );
}
