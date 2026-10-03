/**
 * HotPlex Runtime Adapter
 *
 * Adapts BrowserHotPlexClient (AEP v1 WebSocket) to assistant-ui ExternalStoreAdapter.
 * This is the core integration layer that bridges the two systems.
 */

import {
    useCallback,
    useEffect,
    useMemo,
    useRef,
    useState,
    useSyncExternalStore,
} from "react";
import type {
    ExternalStoreAdapter,
    ThreadMessageLike,
    AppendMessage,
} from "@assistant-ui/react";
import { BrowserHotPlexClient } from "@/lib/ai-sdk-transport";
import type {
    InitConfig,
    ContextUsageData,
    PermissionRequestData,
    PermissionResponseData,
    QuestionRequestData,
    QuestionResponseData,
    ElicitationRequestData,
    ElicitationResponseData,
    InputAckData,
} from "@/lib/ai-sdk-transport/client/types";
import {
    WorkerStdioCommand,
    type WorkerType,
} from "@/lib/ai-sdk-transport/client/constants";
import {
    wsUrl,
    workerType as defaultWorkerType,
    apiKey,
    isSameOrigin,
    allowedTools,
    type ConnectionState,
} from "@/lib/config";
import { TODO_TOOLS } from "@/lib/tool-categories";
import { useMetrics } from "@/lib/hooks/useMetrics";
import { getSessionHistory, type ConversationRecord } from "@/lib/api/sessions";
import { conversationTurnsToMessages } from "@/lib/utils/turn-replay";
import {
    isExpectedCommandRejection,
    mapErrorToNotice,
} from "@/lib/adapters/error-mapping";
import { isExpectedClientError } from "@/lib/ai-sdk-transport/client/error-policy";
import { classifyInputAck } from "@/lib/adapters/input-ack-receipt";
import { logger } from "@/lib/logger";
import i18n from "@/lib/i18n/config";
import type {
    Envelope,
    MessageDeltaData,
    MessageStartData,
    MessageData,
    DoneData,
    ErrorData,
    ReasoningData,
    ToolCallData,
    ToolResultData,
} from "@/lib/ai-sdk-transport";
import type {
    TextPart,
    ReasoningPart,
    ToolCallPart,
    ToolSummaryPart,
    ContextUsagePart,
    TurnSummaryPart,
    SkillListPart,
    MessagePart,
} from "@/lib/types/message-parts";
import type { HotPlexMessage } from "@/lib/types/message";
import {
    appendTextDelta,
    appendReasoningDelta,
} from "@/lib/adapters/merge-parts";
import {
    patchAuthoritativeAssistantContent,
    selectAuthoritativeAssistantContent,
} from "@/lib/adapters/reconcile-turn";
import { expireInteractions } from "@/lib/adapters/terminal-interactions";
import {
    completeStreamingAssistant,
    createPendingAssistantMessage,
    isVisibleAdapterMessage,
    matchesActiveInput,
    removePendingAssistant,
    updatePendingAssistant,
} from "@/lib/adapters/pending-assistant";
import {
    FollowUpQueueStore,
    type FollowUpQueueControls,
    type FollowUpQueueErrorKind,
} from "@/lib/adapters/follow-up-queue";

// Re-export for consumers
export type { HotPlexMessage };
export type {
    TextPart,
    ReasoningPart,
    ToolCallPart,
    ToolSummaryPart,
    ContextUsagePart,
    TurnSummaryPart,
    SkillListPart,
    MessagePart,
};

// ThreadSuggestion shape — matches @assistant-ui/core ThreadSuggestion
type ThreadSuggestion = { title: string; label: string; prompt: string };

export type InteractionKind = "permission" | "question" | "elicitation";
export type InteractionStatus =
    "pending" | "submitting" | "resolved" | "rejected" | "expired" | "failed";

export interface InteractionState {
    kind: InteractionKind;
    requestId: string;
    status: InteractionStatus;
    createdAt: number;
    expiresAt?: number;
    response?: any;
    error?: string;
}

// ============================================================================
// Types
// ============================================================================

export interface UseHotPlexRuntimeConfig {
    /** Initial session ID to resume (calls resume() instead of connect()). */
    sessionId?: string;
    /** Worker type of the selected session. Defaults to HOTPLEX_WEBCHAT_WORKER_TYPE. */
    workerType?: WorkerType;
    /** Active workspace ID (spec ⑥) */
    workspaceId?: string;
    /** Called when the server rejects the workspace during handshake
     * (access denied / not found / disabled). Lets the host reset its
     * workspace selection and reconnect instead of fatal-disconnecting. */
    onWorkspaceError?: () => void;
    /** Called when session metrics update (for dashboard display). */
    onMetricsChange?: (
        metrics: import("@/lib/hooks/useMetrics").SessionMetrics,
    ) => void;
    /** Called when skills list is fetched from the worker. */
    onSkillsChange?: (
        skills: import("@/lib/ai-sdk-transport/client/types").SkillEntry[],
    ) => void;
    /** Called when session lifecycle state changes (running/idle/terminated etc). */
    onSessionStateChange?: (state: string) => void;
    /** Custom welcome suggestions shown when thread is empty. */
    suggestions?: readonly ThreadSuggestion[];
    /** Page-scoped queue store that survives keyed session runtime remounts. */
    followUpQueueStore?: FollowUpQueueStore;
}

// Content-signature prefix length for dedup — covers most short/medium responses.
const CONTENT_SIG_PREFIX = 300;

const DEFAULT_SUGGESTIONS: readonly ThreadSuggestion[] = [];

// ============================================================================
// Gateway command ack detection
// ============================================================================

/**
 * Gateway-handled slash commands (help/control/worker commands intercepted by
 * `tryCommandDispatch`) are acknowledged with a synthetic `delivered` ack whose
 * execution id carries the "cmd-" prefix (internal/gateway/handler.go
 * ackControlCommand). The gateway deliberately never emits `done` for these
 * turns ("no InputAck or Done is ever sent"), so the delivered ack IS the
 * terminal event. Real worker turns never use the prefix — their acks are
 * always followed by streaming events and a `done`.
 */
export function isGatewayCommandAck(
    data: Pick<InputAckData, "execution_id" | "status">,
): boolean {
    return (
        data.status === "delivered" && data.execution_id.startsWith("cmd-")
    );
}

// ============================================================================
// Message Converter
// ============================================================================

/**
 * Converts HotPlex message to assistant-ui ThreadMessageLike format.
 * Handles both old format (content: string) and new format (parts: MessagePart[]).
 */
export function convertToThreadMessage(
    message: HotPlexMessage,
): ThreadMessageLike {
    // Filter out custom card parts — they are rendered from message metadata,
    // not passed into assistant-ui's ThreadMessageLike content union.
    const parts = message.parts ?? [];
    const content = parts.filter(
        (p): p is TextPart | ReasoningPart | ToolCallPart =>
            p.type !== "tool-summary" &&
            p.type !== "context-usage" &&
            p.type !== "turn-summary" &&
            p.type !== "skill-list",
    );

    const role = (message.role as string) === "user" ? "user" : "assistant";

    // Extract context usage data for card rendering
    const contextUsagePart = parts.find(
        (p): p is ContextUsagePart => p.type === "context-usage",
    );

    // Extract turn summary data for card rendering
    const turnSummaryPart = parts.find(
        (p): p is TurnSummaryPart => p.type === "turn-summary",
    );

    const skillListPart = parts.find(
        (p): p is SkillListPart => p.type === "skill-list",
    );

    // Extended ThreadMessageLike for HotPlex-specific metadata
    // (assistant-ui ThreadMessageLike.metadata is typed, but our custom keys need explicit typing)
    const result = {
        id: message.id,
        role,
        content,
        createdAt: message.createdAt,
        attachments: [] as const,
        metadata: {
            custom: {
                ...(contextUsagePart
                    ? { contextUsage: contextUsagePart.data }
                    : {}),
                ...(turnSummaryPart
                    ? { turnSummary: turnSummaryPart.data }
                    : {}),
                ...(skillListPart ? { skillsList: skillListPart.skills } : {}),
                ...(message.progress ? { progress: message.progress } : {}),
                ...(message.clientMessageId
                    ? { clientMessageId: message.clientMessageId }
                    : {}),
                ...(message.deliveryStatus
                    ? { deliveryStatus: message.deliveryStatus }
                    : {}),
            },
        } satisfies Record<string, unknown>,
    } as ThreadMessageLike & {
        status?: { type: "running" } | { type: "complete"; reason: string };
    };

    // Status is only supported for assistant messages
    if (message.role === "assistant") {
        result.status =
            message.status === "streaming"
                ? { type: "running" }
                : { type: "complete", reason: "stop" };
    }

    return result;
}

// ============================================================================
// History Conversion Helpers
// ============================================================================

// Extract min database turn ID from message IDs for cursor-based pagination.
// Message ID format: "turn:{dbId}:{role}".
function extractMinDbId(messages: { id: string }[]): number {
    if (messages.length === 0) return 0;
    let min = Number.MAX_SAFE_INTEGER;
    for (const m of messages) {
        if (!m.id.startsWith("turn:")) continue;
        const parts = m.id.split(":");
        const dbId = parts.length >= 2 ? parseInt(parts[1], 10) : 0;
        if (dbId > 0 && dbId < min) min = dbId;
    }
    return min === Number.MAX_SAFE_INTEGER ? 0 : min;
}

// Convert ConversationRecord[] from API to HotPlexMessage[]
function historyToMessages(records: ConversationRecord[]): HotPlexMessage[] {
    const turns = records.map((r) => ({
        ...r,
        success: r.success == null ? null : !!r.success,
    }));
    return conversationTurnsToMessages(turns).map((m) => ({
        id: m.id,
        role: m.role as "user" | "assistant",
        parts: (m.parts || [])
            .map((p) => {
                if (p.type === "tool-summary") {
                    return {
                        type: "tool-summary" as const,
                        toolNames: p.toolNames,
                        count: p.count,
                    };
                }
                if (p.type === "text") {
                    return { type: "text" as const, text: p.text || "" };
                }
                // reasoning not persisted in server history (streaming-only content, lost on reload)
                return null;
            })
            .filter((p): p is TextPart | ToolSummaryPart => p !== null),
        createdAt: m.createdAt,
        status: "complete" as const,
        clientMessageId: m.clientMessageId,
        deliveryStatus: m.deliveryStatus,
    }));
}

/**
 * Reconcile live optimistic messages with authoritative history by the stable
 * client identity. Durable history wins, while an optimistic user turn whose
 * delivery is still unknown remains visible until history proves its outcome.
 */
export function reconcileMessagesByClientMessageId(
    liveMessages: HotPlexMessage[],
    historyMessages: HotPlexMessage[],
): HotPlexMessage[] {
    const seenClientMessageIds = new Set<string>();
    const history = historyMessages.filter((message) => {
        if (message.role !== "user" || !message.clientMessageId) return true;
        if (seenClientMessageIds.has(message.clientMessageId)) return false;
        seenClientMessageIds.add(message.clientMessageId);
        return true;
    });
    const liveOnly = liveMessages.filter(
        (message) =>
            message.role !== "user" ||
            !message.clientMessageId ||
            !seenClientMessageIds.has(message.clientMessageId),
    );
    return [...history, ...liveOnly];
}

export function completeAssistantParts(
    parts: readonly MessagePart[],
    sessionStats?: TurnSummaryPart["data"],
): MessagePart[] {
    const completed = parts.map((part): MessagePart => {
        if (
            part.type === "tool-call" &&
            (!part.status || part.status.type === "running")
        ) {
            return {
                ...part,
                status: { type: "complete" as const },
            };
        }
        return part;
    });
    if (sessionStats) {
        completed.push({ type: "turn-summary", data: sessionStats });
    }
    return completed;
}

// ============================================================================
// HotPlex Runtime Adapter Hook
// ============================================================================

/**
 * Hook that creates an assistant-ui ExternalStoreAdapter for HotPlex WebSocket client.
 *
 * This adapter:
 * 1. Manages WebSocket connection lifecycle
 * 2. Converts AEP v1 events to assistant-ui messages
 * 3. Provides onNew handler for sending messages
 *
 * @param config - Configuration options for HotPlex client
 * @returns assistant-ui ExternalStoreAdapter
 */
export function useHotPlexRuntime({
    sessionId,
    workerType: sessionWorkerType,
    workspaceId,
    onWorkspaceError,
    onMetricsChange,
    onSkillsChange,
    onSessionStateChange,
    suggestions: configSuggestions,
    followUpQueueStore,
}: UseHotPlexRuntimeConfig = {}): ExternalStoreAdapter<HotPlexMessage> {
    // State
    const [messages, setMessages] = useState<HotPlexMessage[]>([]);
    const [isRunning, setIsRunning] = useState(false);
    const [historyHasMore, setHistoryHasMore] = useState(true);
    const [connectionState, setConnectionState] =
        useState<ConnectionState>("disconnected");
    const ownedQueueStoreRef = useRef<FollowUpQueueStore | null>(null);
    if (!ownedQueueStoreRef.current) {
        ownedQueueStoreRef.current = new FollowUpQueueStore();
    }
    const queueStore = followUpQueueStore ?? ownedQueueStoreRef.current;
    const getQueueSnapshot = useCallback(
        () => queueStore.getSnapshot(sessionId),
        [queueStore, sessionId],
    );
    const queuedItems = useSyncExternalStore(
        queueStore.subscribe,
        getQueueSnapshot,
        getQueueSnapshot,
    );

    // One-time cleanup of orphaned localStorage keys from removed message cache
    useEffect(() => {
        try {
            const keysToRemove: string[] = [];
            for (let i = 0; i < localStorage.length; i++) {
                const key = localStorage.key(i);
                if (key?.startsWith("hotplex_msgs_")) keysToRemove.push(key);
            }
            keysToRemove.forEach((k) => localStorage.removeItem(k));
        } catch {
            /* ignore */
        }
    }, []);
    const clientRef = useRef<BrowserHotPlexClient | null>(null);
    const pendingAssistantIdRef = useRef<string | null>(null);
    const activeAssistantIdRef = useRef<string | null>(null);
    const cancelledAssistantIdRef = useRef<string | null>(null);
    const activeInputMessageIdRef = useRef<string | null>(null);
    // Text of the currently dispatched input. Used to (a) render the
    // command-executed confirmation for gateway-handled commands and (b) tell
    // reconcile which persisted user record belongs to this turn — command
    // turns are never persisted, so a mismatch means "do not reconcile".
    const activeTurnInputRef = useRef<string | null>(null);
    const historyLoadingRef = useRef(false);
    const sessionIdRef = useRef(sessionId);
    const sessionAlreadyConnectedRef = useRef(false);
    const turnActiveRef = useRef(false);
    const drainInFlightRef = useRef(false);
    const scheduleQueueDrainRef = useRef<() => void>(() => undefined);
    const activeQueueDispatchRef = useRef<{
        sessionId: string;
        itemId: string;
        userMessageId: string;
        assistantMessageId: string;
        clientMessageId?: string;
        delivered: boolean;
        outcomeUnknown?: boolean;
        text: string;
    } | null>(null);
    const failActiveQueueDispatchRef = useRef<
        (kind: FollowUpQueueErrorKind, message: string) => boolean
    >(() => false);

    failActiveQueueDispatchRef.current = (kind, message) => {
        const active = activeQueueDispatchRef.current;
        if (!active || active.delivered) return false;
        queueStore.markFailed(active.sessionId, active.itemId, kind, message);
        if (kind === "unknown") {
            active.outcomeUnknown = true;
            turnActiveRef.current = false;
            setIsRunning(false);
            return true;
        }
        activeQueueDispatchRef.current = null;
        turnActiveRef.current = false;
        pendingAssistantIdRef.current = null;
        activeAssistantIdRef.current = null;
        activeInputMessageIdRef.current = null;
        activeTurnInputRef.current = null;
        setIsRunning(false);
        setMessages((previous) =>
            previous.filter(
                (messageItem) =>
                    messageItem.id !== active.userMessageId &&
                    messageItem.id !== active.assistantMessageId,
            ),
        );
        return true;
    };

    // Welcome suggestions — shown when thread is empty (use prop or default list)
    const suggestions: readonly ThreadSuggestion[] =
        configSuggestions ?? DEFAULT_SUGGESTIONS;

    // Stable ref for skills callback — avoids adding to useEffect deps
    const onSkillsChangeRef = useRef(onSkillsChange);
    const onSessionStateChangeRef = useRef(onSessionStateChange);
    const onWorkspaceErrorRef = useRef(onWorkspaceError);
    // Keep latest-value refs in sync with props (read inside effects/callbacks).
    useEffect(() => {
        sessionIdRef.current = sessionId;
        onSkillsChangeRef.current = onSkillsChange;
        onSessionStateChangeRef.current = onSessionStateChange;
        onWorkspaceErrorRef.current = onWorkspaceError;
    });

    // Track whether skills have been fetched (only after first turn completes)
    const skillsFetchedRef = useRef(false);

    // Track pending interaction requests for response routing
    const interactionMapRef = useRef<
        Map<string, { type: "permission" | "question" | "elicitation" }>
    >(new Map());
    const interactionAckTimersRef = useRef<
        Map<string, ReturnType<typeof setTimeout>>
    >(new Map());

    // Cache min turn ID for cursor-based pagination (avoid O(n) scan on each load)
    const minIdRef = useRef<number>(0);

    // Metrics tracking (spec §4.5 — Token & latency dashboard)
    const { sessionMetrics, startTurn, recordTurn } = useMetrics();

    // Sync metrics to parent (ChatContainer header dashboard)
    useEffect(() => {
        onMetricsChange?.(sessionMetrics);
    }, [sessionMetrics, onMetricsChange]);

    // Load history on session switch
    useEffect(() => {
        if (!sessionId) return;
        sessionIdRef.current = sessionId;
        setHistoryHasMore(true);

        const controller = new AbortController();

        // Fetch authoritative history from server
        getSessionHistory(sessionId, { limit: 50, signal: controller.signal })
            .then((res) => {
                if (res.records.length > 0) {
                    const serverMessages = historyToMessages(res.records);
                    // Build content signature set for dedup (live user messages have different IDs than server)
                    // Extract ALL visible parts (text, reasoning, tool-summary) for accurate dedup
                    const extractText = (
                        parts: MessagePart[] | undefined | null,
                    ) =>
                        (parts ?? [])
                            .filter(
                                (p) =>
                                    p.type === "text" ||
                                    p.type === "reasoning" ||
                                    p.type === "tool-summary",
                            )
                            .map((p) => {
                                if (p.type === "text")
                                    return (p as TextPart).text || "";
                                if (p.type === "reasoning")
                                    return `[THOUGHT]${(p as ReasoningPart).text || ""}`;
                                if (p.type === "tool-summary")
                                    return `[TOOL]${((p as ToolSummaryPart).toolNames || []).join(",")}`;
                                return "";
                            })
                            .join("");
                    // Stable client identity wins over content signatures. The
                    // signature fallback remains for older gateways that do not
                    // persist client_message_id yet.
                    setMessages((prev) => {
                        const serverIds = new Set(
                            serverMessages.map((m) => m.id),
                        );
                        const serverSigs = new Set(
                            serverMessages.map(
                                (m) => `${m.role}:${extractText(m.parts)}`,
                            ),
                        );
                        const identityMerged =
                            reconcileMessagesByClientMessageId(prev, serverMessages);
                        const liveOnly = identityMerged.filter((m) => {
                            if (serverIds.has(m.id)) return false;
                            const sig = `${m.role}:${extractText(m.parts)}`;
                            return !serverSigs.has(sig);
                        });
                        return [...serverMessages, ...liveOnly];
                    });
                    // Update minId cache for cursor-based pagination
                    if (serverMessages.length > 0) {
                        minIdRef.current = extractMinDbId(serverMessages);
                    }
                }
                setHistoryHasMore(res.has_more);
            })
            .catch((err) => {
                if (err instanceof DOMException && err.name === "AbortError")
                    return;
                logger.warn("RuntimeAdapter", "Failed to load history", {
                    error: String(err),
                });
                setMessages((prev) => [
                    ...prev,
                    {
                        id: `history-error-${Date.now()}`,
                        role: "assistant" as const,
                        parts: [
                            {
                                type: "text" as const,
                                text: "Failed to load conversation history. You can continue chatting, but previous messages may not be visible.",
                            },
                        ],
                        createdAt: new Date(),
                        status: "complete" as const,
                    },
                ]);
            });
        return () => controller.abort();
    }, [sessionId]);

    // Initialize WebSocket client
    useEffect(() => {
        // Guard: disconnect any lingering client from a previous effect run
        // (e.g., React Strict Mode double-render or stale closure from async reconnect)
        if (clientRef.current) {
            clientRef.current.disconnect();
            clientRef.current = null;
        }

        if (!sessionId) {
            logger.info("RuntimeAdapter", "No session ID, skipping connection");
            return;
        }

        skillsFetchedRef.current = false;
        sessionAlreadyConnectedRef.current = false;

        const initConfig: InitConfig = {};
        // work_dir is omitted: the server derives it from the bound workspace
        // (spec §6.2 — work_dir is immutable per workspace). Sending a client
        // value is a dead path the backend already ignores.
        if (allowedTools.length > 0) initConfig.allowed_tools = allowedTools;

        // Guard against setState after the effect cleans up (session switch /
        // remount). Async paths like reconcileTurnContent fetch then setMessages
        // — without this flag they'd write into the unmounted hook's state.
        let cancelled = false;

        const client = new BrowserHotPlexClient({
            url: wsUrl,
            workerType: sessionWorkerType ?? defaultWorkerType,
            apiKey: isSameOrigin() ? undefined : apiKey,
            authToken: isSameOrigin() ? undefined : apiKey,
            workspaceId,
            initConfig,
            heartbeat: {
                pingIntervalMs: 20000,
                pongTimeoutMs: 10000,
                maxMissedPongs: 3,
            },
        });

        clientRef.current = client;

        // Track the streaming fallback message ID (created by delta/reasoning before messageStart).
        // Used by handleMessage to adopt the fallback instead of creating a duplicate (#331).
        let streamingFallbackId: string | null = null;
        const processedDoneEventIds = new Set<string>();

        // Append delta content to the last text part of the last assistant message
        const appendDelta = (content: string) => {
            // Capture the target before queueing the updater. React may run the
            // updater after done has cleared the refs, so reading the ref there
            // can miss the pending placeholder.
            const pendingAssistantID = pendingAssistantIdRef.current;
            pendingAssistantIdRef.current = null;
            setMessages((prev) => {
                const pending = updatePendingAssistant(
                    prev,
                    pendingAssistantID,
                    (message) => ({
                        ...message,
                        progress: undefined,
                        parts: appendTextDelta(message.parts, content),
                    }),
                );
                if (pending !== prev) {
                    return pending;
                }
                const lastMessage = prev[prev.length - 1];
                if (
                    lastMessage?.role === "assistant" &&
                    lastMessage.status === "streaming"
                ) {
                    const parts = appendTextDelta(lastMessage.parts, content);
                    return [...prev.slice(0, -1), { ...lastMessage, parts }];
                }
                // No streaming message — create one (message.start may not have been sent)
                const fallbackId = `assistant-${Date.now()}`;
                streamingFallbackId = fallbackId;
                return [
                    ...prev,
                    {
                        id: fallbackId,
                        role: "assistant" as const,
                        parts: [{ type: "text", text: content }],
                        createdAt: new Date(),
                        status: "streaming" as const,
                    },
                ];
            });
        };

        // Note: deltas are committed synchronously in handleDelta. React 18
        // automatic batching coalesces multiple setMessages within the same
        // microtask, so per-delta re-renders are already batched without
        // requestAnimationFrame. RAF was previously used here but was removed:
        // browsers throttle/pause RAF in background tabs, which caused deltas
        // to accumulate unbounded in memory while flush callbacks never fired,
        // losing the streamed tail when the WebSocket was paused by the page
        // visibility policy. Synchronous commits have no such failure mode.

        // Handle reasoning/thinking content (appends to last reasoning part or creates one)
        const handleReasoning = (data: ReasoningData, _env: Envelope) => {
            if (!data) return;
            turnActiveRef.current = true;

            setMessages((prev) => {
                const pending = updatePendingAssistant(
                    prev,
                    pendingAssistantIdRef.current,
                    (message) => ({
                        ...message,
                        progress: undefined,
                        parts: appendReasoningDelta(
                            message.parts,
                            data.content || "",
                        ),
                    }),
                );
                if (pending !== prev) {
                    pendingAssistantIdRef.current = null;
                    return pending;
                }
                const lastMessage = prev[prev.length - 1];
                if (
                    lastMessage?.role === "assistant" &&
                    lastMessage.status === "streaming"
                ) {
                    const parts = appendReasoningDelta(
                        lastMessage.parts,
                        data.content || "",
                    );
                    return [...prev.slice(0, -1), { ...lastMessage, parts }];
                }
                const fallbackId = `assistant-${Date.now()}`;
                streamingFallbackId = fallbackId;
                return [
                    ...prev,
                    {
                        id: fallbackId,
                        role: "assistant" as const,
                        parts: [
                            { type: "reasoning", text: data.content || "" },
                        ],
                        createdAt: new Date(),
                        status: "streaming" as const,
                    },
                ];
            });
            setIsRunning(true);
        };

        const handleDelta = (data: MessageDeltaData, _env: Envelope) => {
            if (!data) return;
            turnActiveRef.current = true;
            // Synchronous commit — no RAF batching (see note above appendDelta).
            appendDelta(data.content || "");
        };

        const handleMessage = (data: MessageData, env: Envelope) => {
            const role: "user" | "assistant" =
                data?.role === "user" ? "user" : "assistant";
            logger.debug("RuntimeAdapter", "message received", {
                role,
                contentLen: (data?.content || "").length,
                envId: env.id,
            });
            // Always use env.id for consistency with handleMessageStart
            const msgId = env.id;
            setMessages((prev) => {
                const pending = updatePendingAssistant(
                    prev,
                    pendingAssistantIdRef.current,
                    (message) => ({
                        ...message,
                        role,
                        progress: undefined,
                        parts: [{ type: "text", text: data?.content || "" }],
                        status: "complete",
                    }),
                );
                if (pending !== prev) {
                    pendingAssistantIdRef.current = null;
                    return pending;
                }
                let existingIdx = prev.findIndex((m) => m.id === msgId);

                // Fallback: adopt streaming message with fallback ID instead of creating duplicate (#331).
                // When delta/reasoning arrives before messageStart, a placeholder message is created
                // with `assistant-${Date.now()}` ID. We keep that ID to avoid MessageRepository crash,
                // so handleMessage must find it by the tracked fallback ID.
                if (
                    existingIdx === -1 &&
                    role === "assistant" &&
                    streamingFallbackId
                ) {
                    existingIdx = prev.findIndex(
                        (m) => m.id === streamingFallbackId,
                    );
                    if (existingIdx !== -1) streamingFallbackId = null;
                }

                if (existingIdx !== -1) {
                    // Update existing message (e.g., streaming placeholder → complete)
                    const updated = [...prev];
                    updated[existingIdx] = {
                        ...prev[existingIdx],
                        role,
                        parts: [{ type: "text", text: data?.content || "" }],
                        status: "complete",
                    };
                    return updated;
                }
                return [
                    ...prev,
                    {
                        id: msgId,
                        role,
                        parts: [{ type: "text", text: data?.content || "" }],
                        createdAt: new Date(env.timestamp || Date.now()),
                        status: "complete",
                    },
                ];
            });
        };

        const handleToolCall = (data: ToolCallData, _env: Envelope) => {
            if (!data) return;
            setMessages((prev) => {
                const lastMessage = prev[prev.length - 1];
                if (lastMessage?.role === "assistant") {
                    const newPart = {
                        type: "tool-call" as const,
                        toolName: data.name,
                        args: data.input,
                        toolCallId: data.id,
                        status: { type: "running" as const },
                    };
                    // Replace previous todo tool-call instead of stacking duplicates
                    if (TODO_TOOLS.has(data.name?.toLowerCase())) {
                        const lastTodoIdx = lastMessage.parts.findLastIndex(
                            (p): p is ToolCallPart =>
                                p.type === "tool-call" &&
                                TODO_TOOLS.has(p.toolName.toLowerCase()),
                        );
                        if (lastTodoIdx !== -1) {
                            const parts = [...lastMessage.parts];
                            parts[lastTodoIdx] = newPart;
                            return [
                                ...prev.slice(0, -1),
                                { ...lastMessage, parts },
                            ];
                        }
                    }
                    const parts = [...lastMessage.parts, newPart];
                    return [...prev.slice(0, -1), { ...lastMessage, parts }];
                }
                return prev;
            });
        };

        const handleToolResult = (data: ToolResultData, _env: Envelope) => {
            if (!data) return;
            setMessages((prev) => {
                const lastMessage = prev[prev.length - 1];
                if (lastMessage?.role === "assistant") {
                    const parts = lastMessage.parts.map((p) =>
                        p.type === "tool-call" && p.toolCallId === data.id
                            ? {
                                  ...p,
                                  result: data.error ?? data.output,
                                  isError: !!data.error,
                                  status: {
                                      type: data.error
                                          ? ("error" as const)
                                          : ("complete" as const),
                                  },
                              }
                            : p,
                    );
                    return [...prev.slice(0, -1), { ...lastMessage, parts }];
                }
                return prev;
            });
        };

        const handleDone = (data: DoneData, env: Envelope) => {
            if (processedDoneEventIds.has(env.id)) return;
            processedDoneEventIds.add(env.id);
            if (processedDoneEventIds.size > 256) {
                const oldest = processedDoneEventIds.values().next().value;
                if (oldest) processedDoneEventIds.delete(oldest);
            }
            expireActiveInteractions();
            streamingFallbackId = null;
            turnActiveRef.current = false;
            const queuedDispatch = activeQueueDispatchRef.current;
            const convergedUnknown = queuedDispatch?.outcomeUnknown === true;
            if (queuedDispatch?.outcomeUnknown) {
                queueStore.remove(
                    queuedDispatch.sessionId,
                    queuedDispatch.itemId,
                );
                activeQueueDispatchRef.current = null;
            } else if (queuedDispatch && !queuedDispatch.delivered) {
                failActiveQueueDispatchRef.current(
                    "unknown",
                    "Turn completed before delivery was confirmed",
                );
            } else {
                activeQueueDispatchRef.current = null;
            }
            const pendingAssistantID = pendingAssistantIdRef.current;
            const activeAssistantID = activeAssistantIdRef.current;
            const reconciliationTargetID =
                queuedDispatch?.assistantMessageId ??
                pendingAssistantID ??
                activeAssistantID;
            // Kept for the cmd- ack handler (command confirmation) and for the
            // reconcile input-match guard below; cleared by handleInputAck.
            const inputContent = activeTurnInputRef.current;
            pendingAssistantIdRef.current = null;
            activeAssistantIdRef.current = null;
            activeInputMessageIdRef.current = null;

            if (data?.stats) {
                recordTurn(data.stats);
            } else {
                recordTurn({});
            }

            setMessages((prev) => {
                if (convergedUnknown) {
                    const convergenceAssistantID =
                        queuedDispatch?.assistantMessageId;
                    const assistantIndex = prev.findIndex(
                        (message) =>
                            message.id === convergenceAssistantID &&
                            message.role === "assistant",
                    );
                    if (assistantIndex === -1) return prev;
                    const message = prev[assistantIndex];
                    const parts = completeAssistantParts(
                        message.parts,
                        data?.stats?._session,
                    );
                    const next = [...prev];
                    next[assistantIndex] = {
                        ...message,
                        status: "complete" as const,
                        progress: undefined,
                        parts,
                    };
                    return next;
                }
                // A delta can be queued in the same browser task as done. The
                // delta updater may clear pendingAssistantIdRef before React
                // applies either updater, so removing by the ID captured above
                // would discard the just-arrived text. Finalize the exact
                // active turn message first whenever it already has content.
                const activeIndex = prev.findIndex(
                    (message) =>
                        message.id === activeAssistantID &&
                        message.role === "assistant" &&
                        message.status === "streaming",
                );
                const activeMessage =
                    activeIndex === -1 ? undefined : prev[activeIndex];
                if (activeMessage && activeMessage.parts.length > 0) {
                    const parts = completeAssistantParts(
                        activeMessage.parts,
                        data?.stats?._session,
                    );
                    const next = [...prev];
                    next[activeIndex] = {
                        ...activeMessage,
                        status: "complete" as const,
                        progress: undefined,
                        parts,
                    };
                    return next;
                }

                const withoutPending = removePendingAssistant(
                    prev,
                    pendingAssistantID,
                );
                if (withoutPending !== prev) {
                    return withoutPending;
                }
                const lastMessage = prev[prev.length - 1];
                if (lastMessage?.role === "assistant") {
                    const parts = completeAssistantParts(
                        lastMessage.parts,
                        data?.stats?._session,
                    );
                    return [
                        ...prev.slice(0, -1),
                        { ...lastMessage, status: "complete" as const, parts },
                    ];
                }
                return prev;
            });

            setIsRunning(false);
            setIsStopping(false);
            stoppingRef.current = false;
            queueMicrotask(() => scheduleQueueDrainRef.current());

            // Fetch skills after the first turn completes (worker conversation is now active)
            // Skip if the turn was stopped by the user, since the worker is detached.
            if (
                !skillsFetchedRef.current &&
                data?.reason !== "stopped_by_user"
            ) {
                skillsFetchedRef.current = true;
                try {
                    client.sendWorkerCommand(WorkerStdioCommand.Skills);
                } catch {
                    // Non-critical — skills list stays empty
                }
            }

            // Unconditionally reconcile against the authoritative turns table.
            // The server no longer annotates done with a dropped flag — that
            // cross-layer marker protocol was removed as over-engineering (it
            // had three P1 regressions and its only purpose was to save this
            // one cheap REST call). reconcileTurnContent has prefix + length
            // guards that make it a no-op when streaming was complete, so an
            // unconditional trigger is safe and correct.
            void reconcileTurnContent({
                targetAssistantId: reconciliationTargetID,
                terminalSeq: env.seq,
                inputContent: inputContent ?? undefined,
            });
        };

        // Re-fetch the authoritative content for the just-completed turn and
        // patch the last assistant message if it differs. Uses the turns table
        // (history API) rather than raw message events: a TurnRecord.content is
        // the complete, turn-scoped assistant text — no 4KB size-flush splits to
        // re-stitch and no turn-boundary ambiguity across interleaved messages.
        // The trade-off is write latency: captureAssistantTurn enqueues the
        // record on done, but the collector flushes async (~1s batch interval),
        // so the first fetch may miss it. We retry once after a short delay.
        const reconcileTurnContent = async ({
            targetAssistantId,
            terminalSeq,
            inputContent,
        }: {
            targetAssistantId: string | null;
            terminalSeq?: number;
            inputContent?: string;
        }) => {
            const sid = sessionIdRef.current;
            if (!sid || !targetAssistantId) return;
            const fetchLastAssistantContent = async (): Promise<
                string | null
            > => {
                const res = await getSessionHistory(sid, { limit: 20 });
                if (terminalSeq !== undefined) {
                    return selectAuthoritativeAssistantContent(res.records, {
                        terminalSeq,
                        // The input-match guard only works when the caller
                        // threads the input through. Gateway-handled command
                        // turns (e.g. /skills) are never persisted, so without
                        // this the guard sees undefined and reconciles the
                        // PREVIOUS turn's answer into the command turn.
                        inputContent: inputContent ?? undefined,
                    });
                }
                return selectAuthoritativeAssistantContent(res.records, {
                    inputContent: inputContent ?? "",
                });
            };
            try {
                let fullText = await fetchLastAssistantContent();
                if (!fullText) {
                    // Turn record may still be in the collector's async batch
                    // buffer — wait past the flush interval (1s) + DB round-trip
                    // margin and retry once.
                    await new Promise((r) => setTimeout(r, 2000));
                    if (cancelled) return;
                    fullText = await fetchLastAssistantContent();
                }
                if (!fullText || cancelled) return;
                const full = fullText; // const for closure narrowing (no `!` needed)
                setMessages((previous) =>
                    patchAuthoritativeAssistantContent(
                        previous,
                        targetAssistantId,
                        full,
                    ),
                );
            } catch (err) {
                logger.warn("RuntimeAdapter", "Reconcile turn content failed", {
                    error: String(err),
                });
            }
        };

        const updateInteractionState = (
            requestId: string,
            status: InteractionStatus,
            response?: unknown,
            error?: string,
            onlySubmitting = false,
        ) => {
            const timer = interactionAckTimersRef.current.get(requestId);
            if (timer) {
                clearTimeout(timer);
                interactionAckTimersRef.current.delete(requestId);
            }
            setMessages((prev) =>
                prev.map((msg) => {
                    if (msg.role !== "assistant") return msg;
                    let found = false;
                    const parts = msg.parts.map((part) => {
                        if (
                            part.type !== "tool-call" ||
                            part.toolCallId !== requestId
                        ) {
                            return part;
                        }
                        const interaction = part.args?.interaction;
                        if (
                            !interaction ||
                            (onlySubmitting &&
                                interaction.status !== "submitting")
                        ) {
                            return part;
                        }
                        found = true;
                        return {
                            ...part,
                            args: {
                                ...part.args,
                                interaction: {
                                    ...interaction,
                                    status,
                                    response,
                                    error,
                                },
                            },
                        };
                    });
                    return found ? { ...msg, parts } : msg;
                }),
            );
        };

        const expireActiveInteractions = () => {
            const interactionIDs = new Set(interactionMapRef.current.keys());
            for (const interactionID of interactionIDs) {
                const timer = interactionAckTimersRef.current.get(interactionID);
                if (timer !== undefined) {
                    clearTimeout(timer);
                    interactionAckTimersRef.current.delete(interactionID);
                }
            }
            interactionMapRef.current.clear();
            setMessages((prev) => expireInteractions(prev, interactionIDs));
        };

        const handlePermissionResponse = (data: PermissionResponseData) => {
            if (!data?.id || !interactionMapRef.current.has(data.id)) return;
            updateInteractionState(
                data.id,
                data.allowed ? "resolved" : "rejected",
                data,
            );
            interactionMapRef.current.delete(data.id);
        };

        const handleQuestionResponse = (data: QuestionResponseData) => {
            if (!data?.id || !interactionMapRef.current.has(data.id)) return;
            updateInteractionState(data.id, "resolved", data);
            interactionMapRef.current.delete(data.id);
        };

        const handleElicitationResponse = (data: ElicitationResponseData) => {
            if (!data?.id || !interactionMapRef.current.has(data.id)) return;
            updateInteractionState(
                data.id,
                data.action === "accept" ? "resolved" : "rejected",
                data,
            );
            interactionMapRef.current.delete(data.id);
        };

        const handleError = (data: ErrorData, env: Envelope) => {
            const isBusy = (data?.code as string) === "SESSION_BUSY";
            const isSessionAlreadyConnected =
                (data?.code as string) === "SESSION_ALREADY_CONNECTED";

            if (isSessionAlreadyConnected) {
                sessionAlreadyConnectedRef.current = true;
                setIsRunning(false);
                setConnectionState("already_connected");
                return;
            }

            // Mark any submitting interaction as failed if this is an interaction error
            const isInteractionError =
                (data?.message || "").includes("worker response failed") ||
                (data?.message || "").includes("invalid response data");
            if (isInteractionError) {
                const reqId = env?.metadata?.interaction_error?.request_id;
                if (reqId) {
                    updateInteractionState(
                        reqId,
                        "failed",
                        undefined,
                        data?.message || "Failed to deliver response to worker",
                    );
                }
            }

            const queuedDispatch = activeQueueDispatchRef.current;
            if (
                !isInteractionError &&
                queuedDispatch &&
                !queuedDispatch.delivered
            ) {
                logger.warn("RuntimeAdapter", "Queued input delivery failed", {
                    code: data?.code || "unknown",
                    eventId: env?.id,
                });
                failActiveQueueDispatchRef.current(
                    isBusy ? "busy" : "send",
                    data?.code || data?.message || "Input delivery failed",
                );
                return;
            }

            const isResumeRetry = (data?.code as string) === "RESUME_RETRY";
            const isShutdown = (data?.message || "").includes(
                "during shutdown",
            );
            const isTerminated =
                (data?.code as string) === "SESSION_TERMINATED";
            // User-fixable command rejections (invalid args, unsupported
            // capabilities, or legacy capability errors) are expected input
            // outcomes, not runtime faults — warn, don't error.
            const isExpectedCommandRejectionError = isExpectedCommandRejection(
                data?.code,
                data?.message,
            );
            const isExpectedClientErrorCode = isExpectedClientError(data?.code);
            const errorNotice = mapErrorToNotice(data?.code, data?.message);

            // SESSION_BUSY is a transient state handled internally by auto-retry, so do not show it to the user and don't log as error.
            if (isBusy) {
                return;
            }

            // SESSION_TERMINATED is a normal lifecycle event (user cancelled or server stopped).
            // Don't pollute the chat with error messages; just mark the run as stopped.
            if (isTerminated) {
                expireActiveInteractions();
                turnActiveRef.current = false;
                activeQueueDispatchRef.current = null;
                setIsRunning(false);
                const pendingAssistantID = pendingAssistantIdRef.current;
                const activeAssistantID = activeAssistantIdRef.current;
                pendingAssistantIdRef.current = null;
                activeAssistantIdRef.current = null;
                activeInputMessageIdRef.current = null;
                activeTurnInputRef.current = null;
                setMessages((prev) => {
                    const withoutPending = removePendingAssistant(
                        prev,
                        pendingAssistantID,
                    );
                    if (withoutPending !== prev) {
                        return withoutPending;
                    }
                    const completed = completeStreamingAssistant(
                        prev,
                        activeAssistantID,
                    );
                    if (completed !== prev) {
                        return completed;
                    }
                    const lastMessage = prev[prev.length - 1];
                    if (
                        lastMessage?.role === "assistant" &&
                        lastMessage.status === "streaming"
                    ) {
                        return [
                            ...prev.slice(0, -1),
                            {
                                ...lastMessage,
                                status: "complete",
                                parts: lastMessage.parts.map((p) =>
                                    p.type === "tool-call" &&
                                    p.status?.type === "running"
                                        ? {
                                              ...p,
                                              status: {
                                                  type: "complete" as const,
                                              },
                                          }
                                        : p,
                                ),
                            },
                        ];
                    }
                    return prev;
                });
                logger.info("RuntimeAdapter", "Session terminated", {
                    reason: data?.message,
                });
                queueMicrotask(() => scheduleQueueDrainRef.current());
                return;
            }

            // Shutdown errors are transient — gateway is restarting. Don't pollute the
            // chat with error messages; the client will auto-reconnect.
            if (isShutdown) {
                logger.info(
                    "RuntimeAdapter",
                    "Gateway shutdown, waiting for reconnect",
                );
                return;
            }

            // Workspace handshake rejection (access denied / not found / disabled).
            // The bound workspace is invalid for the current user — surface it to the
            // host so it can reset its workspace selection and reconnect, instead of
            // fatal-disconnecting or dumping a raw error into the chat.
            const wsMsg = (data?.message || "").toLowerCase();
            const isWorkspaceError =
                wsMsg === "workspace access denied" ||
                wsMsg === "workspace not found" ||
                wsMsg === "workspace is disabled";
            if (isWorkspaceError) {
                logger.warn(
                    "RuntimeAdapter",
                    "Workspace handshake rejected — notifying host to reset workspace",
                    { message: data?.message },
                );
                onWorkspaceErrorRef.current?.();
                return;
            }

            const hasData = data && (data.code || data.message);
            if (hasData) {
                if (isResumeRetry) {
                    logger.warn("RuntimeAdapter", "Worker recovery triggered", {
                        code: data.code,
                        message: data.message,
                        details: data.details,
                        eventId: env?.id,
                    });
                } else if (
                    isExpectedCommandRejectionError ||
                    isExpectedClientErrorCode
                ) {
                    logger.warn(
                        "RuntimeAdapter",
                        errorNotice.text,
                        {
                            code: data.code,
                            reason: data.message,
                            eventId: env?.id,
                        },
                    );
                } else {
                    logger.error("RuntimeAdapter", errorNotice.text, {
                        code: data.code || "unknown",
                        reason: data.message || "none",
                        details: data.details,
                        eventId: env?.id,
                    });
                }
            } else {
                logger.warn("RuntimeAdapter", "Empty error event", {
                    eventId: env?.id,
                });
            }

            // If it's a fatal error, stop the run and complete the streaming message
            if (!isResumeRetry) {
                turnActiveRef.current = false;
                activeQueueDispatchRef.current = null;
                setIsRunning(false);
                const pendingAssistantID = pendingAssistantIdRef.current;
                const activeAssistantID = activeAssistantIdRef.current;
                pendingAssistantIdRef.current = null;
                activeAssistantIdRef.current = null;
                activeInputMessageIdRef.current = null;
                activeTurnInputRef.current = null;

                setMessages((prev) => {
                    const withoutPending = removePendingAssistant(
                        prev,
                        pendingAssistantID,
                    );
                    if (withoutPending !== prev) {
                        return withoutPending;
                    }
                    const completed = completeStreamingAssistant(
                        prev,
                        activeAssistantID,
                    );
                    if (completed !== prev) {
                        return completed;
                    }
                    const lastMessage = prev[prev.length - 1];
                    if (
                        lastMessage?.role === "assistant" &&
                        lastMessage.status === "streaming"
                    ) {
                        return [
                            ...prev.slice(0, -1),
                            {
                                ...lastMessage,
                                status: "complete",
                                parts: lastMessage.parts.map((p) =>
                                    p.type === "tool-call" &&
                                    p.status?.type === "running"
                                        ? {
                                              ...p,
                                              status: {
                                                  type: "error" as const,
                                              },
                                          }
                                        : p,
                                ),
                            },
                        ];
                    }
                    return prev;
                });
                queueMicrotask(() => scheduleQueueDrainRef.current());
            }

            // Add error message to thread
            setMessages((prev) => [
                ...prev,
                {
                    id: `error-${Date.now()}`,
                    role: "assistant",
                    parts: [{ type: "text", text: errorNotice.text }],
                    createdAt: new Date(),
                    status: errorNotice.status,
                },
            ]);
        };

        const handleDisconnected = (reason: string) => {
            logger.info("RuntimeAdapter", "Disconnected", { reason });
            failActiveQueueDispatchRef.current(
                "connection",
                reason || "Disconnected",
            );
            for (const requestId of interactionMapRef.current.keys()) {
                updateInteractionState(
                    requestId,
                    "failed",
                    undefined,
                    "Connection closed before the response was confirmed",
                    true,
                );
            }
            setIsRunning(false);
            const pendingAssistantID = pendingAssistantIdRef.current;
            const activeAssistantID = activeAssistantIdRef.current;
            pendingAssistantIdRef.current = null;
            activeAssistantIdRef.current = null;
            activeInputMessageIdRef.current = null;
            activeTurnInputRef.current = null;
            setMessages((prev) => {
                const withoutPending = removePendingAssistant(
                    prev,
                    pendingAssistantID,
                );
                return withoutPending !== prev
                    ? withoutPending
                    : completeStreamingAssistant(prev, activeAssistantID);
            });
            if (!sessionAlreadyConnectedRef.current) {
                setConnectionState("disconnected");
            }
        };

        const handleSessionAlreadyConnected = () => {
            sessionAlreadyConnectedRef.current = true;
            failActiveQueueDispatchRef.current(
                "connection",
                "Session is connected elsewhere",
            );
            setIsRunning(false);
            setConnectionState("already_connected");
        };

        const handleReconnecting = (attempt: number) => {
            logger.info("RuntimeAdapter", "Reconnecting", { attempt });
            setConnectionState("reconnecting");
        };

        const handleReconnectFailed = (attempt: number) => {
            logger.warn("RuntimeAdapter", "Reconnect attempts exhausted", {
                attempt,
            });
            failActiveQueueDispatchRef.current(
                "connection",
                "Reconnect attempts exhausted",
            );
            setIsRunning(false);
            const pendingAssistantID = pendingAssistantIdRef.current;
            const activeAssistantID = activeAssistantIdRef.current;
            pendingAssistantIdRef.current = null;
            activeAssistantIdRef.current = null;
            activeInputMessageIdRef.current = null;
            activeTurnInputRef.current = null;
            setMessages((prev) => {
                const withoutPending = removePendingAssistant(
                    prev,
                    pendingAssistantID,
                );
                return withoutPending !== prev
                    ? withoutPending
                    : completeStreamingAssistant(prev, activeAssistantID);
            });
            setConnectionState("disconnected");
        };

        // Handle messageStart: confirm streaming status on existing message or create new.
        // IMPORTANT: never rename an existing message's ID — changing IDs between renders
        // causes assistant-ui MessageRepository orphaned-node crash (#331).
        const handleMessageStart = (data: MessageStartData, env: Envelope) => {
            if (!data) return;
            turnActiveRef.current = true;
            setMessages((prev) => {
                const pending = updatePendingAssistant(
                    prev,
                    pendingAssistantIdRef.current,
                    (message) => ({
                        ...message,
                        progress: undefined,
                        status: "streaming",
                    }),
                );
                if (pending !== prev) {
                    pendingAssistantIdRef.current = null;
                    return pending;
                }
                // Reasoning events may arrive before messageStart, creating env.id early.
                const existingIdx = prev.findIndex((m) => m.id === env.id);
                if (existingIdx !== -1) {
                    const updated = [...prev];
                    updated[existingIdx] = {
                        ...prev[existingIdx],
                        status: "streaming",
                    };
                    return updated;
                }
                // Delta/reasoning fallback already created a streaming message with a placeholder ID.
                // Keep it as-is — do NOT rename to env.id (causes #331).
                const pendingIdx = prev.findLastIndex(
                    (m) => m.role === "assistant" && m.status === "streaming",
                );
                if (pendingIdx !== -1) {
                    return prev;
                }
                // No prior message for this turn — create with real env.id.
                return [
                    ...prev,
                    {
                        id: env.id,
                        role: "assistant" as const,
                        parts: [],
                        createdAt: new Date(env.timestamp ?? Date.now()),
                        status: "streaming" as const,
                    },
                ];
            });
            setIsRunning(true);
        };

        // input.ack surfaces delivery outcomes the error stream does not. The
        // gateway sends NO error event for an `unknown` outcome (worker timeout
        // / restart), so without this handler the UI would spin forever on a
        // turn whose outcome is ambiguous.
        const handleInputAck = (data: InputAckData) => {
            // Receipt semantics are resolved once, independently of the Agent run
            // state: an input receipt must never be read as "the turn finished".
            const settlement = classifyInputAck(data);
            const queuedDispatch = activeQueueDispatchRef.current;
            const matchesQueuedDispatch =
                queuedDispatch?.clientMessageId === data.client_message_id;
            // Gateway-handled command acks arrive AFTER the synthetic done
            // (event order: skills_list → done → ack, ack sent by
            // ackControlCommand after handleWorkerCommand returns). handleDone
            // clears activeInputMessageIdRef, so the ordinary correlation
            // below would drop the cmd- ack and the command confirmation never
            // renders. The cmd- prefix is gateway-owned and unique per input,
            // so it is safe to admit these acks regardless of correlation.
            if (
                !matchesActiveInput(
                    activeInputMessageIdRef.current,
                    data.client_message_id,
                ) &&
                !matchesQueuedDispatch &&
                !isGatewayCommandAck(data)
            ) {
                return;
            }
            if (
                settlement.kind === "delivered" ||
                settlement.kind === "unknown" ||
                settlement.kind === "failed"
            ) {
                const deliveryStatus: "delivered" | "unknown" | "failed" =
                    settlement.kind === "delivered"
                        ? "delivered"
                        : settlement.kind === "unknown"
                          ? "unknown"
                          : "failed";
                setMessages((prev) =>
                    prev.map((message) =>
                        message.clientMessageId === data.client_message_id
                            ? {
                                  ...message,
                                  deliveryStatus,
                              }
                            : message,
                    ),
                );
            }
            if (queuedDispatch && matchesQueuedDispatch) {
                if (settlement.kind === "delivered") {
                    const wasUnknown = queuedDispatch.outcomeUnknown === true;
                    const deliveredItem = queueStore.markDelivered(
                        queuedDispatch.sessionId,
                        data.client_message_id,
                    );
                    if (deliveredItem) {
                        queuedDispatch.delivered = true;
                        queuedDispatch.outcomeUnknown = false;
                        if (wasUnknown) {
                            turnActiveRef.current = true;
                            setIsRunning(true);
                        }
                    }
                } else if (settlement.kind === "unknown") {
                    failActiveQueueDispatchRef.current(
                        "unknown",
                        settlement.errorCode || "Input outcome is unknown",
                    );
                    return;
                } else if (settlement.kind === "failed") {
                    failActiveQueueDispatchRef.current(
                        "send",
                        settlement.errorCode || "Input delivery failed",
                    );
                    return;
                }
            }
            if (
                queuedDispatch &&
                matchesQueuedDispatch &&
                settlement.kind === "volatile-accepted"
            ) {
                // The gateway staged the input in process memory (supplement
                // buffer). It is deliberately NOT recorded as delivered:
                // buffering is not a delivery proof, and the running parent
                // turn stays active.
                //
                // The release below only fires for a dispatch the queue store
                // still tracks in "sending" state, and no production path
                // creates one today: drainQueue pops every queued item through
                // popAllDispatchable before dispatching them merged, so
                // activeQueueDispatchRef is never set and a volatile
                // acceptance on that path is a deliberate no-op — the parent
                // turn's terminal envelope is what ends the turn. The branch
                // remains for a future per-item dispatch path and must not be
                // read as protection that is load-bearing today.
                const releasedItem = queueStore.releaseVolatileAccepted(
                    queuedDispatch.sessionId,
                    data.client_message_id,
                );
                if (releasedItem) {
                    activeQueueDispatchRef.current = null;
                }
            }
            if (
                settlement.kind === "durable-accepted" ||
                settlement.kind === "volatile-accepted" ||
                settlement.kind === "delivered"
            ) {
                if (isGatewayCommandAck(data)) {
                    // Gateway-handled command turn: the gateway sends no done
                    // for these, so this delivered ack terminates the turn.
                    // Without the reset turnActiveRef/isRunning wedge forever
                    // and every later input is routed to the follow-up queue,
                    // which can never drain (drainQueue guards on turnActive).
                    turnActiveRef.current = false;
                    activeQueueDispatchRef.current = null;
                    setIsRunning(false);
                    const pendingAssistantID = pendingAssistantIdRef.current;
                    const activeAssistantID = activeAssistantIdRef.current;
                    const commandText = activeTurnInputRef.current;
                    const isSkillsCommand = commandText
                        ?.trim()
                        .toLowerCase()
                        .startsWith("/skills");
                    pendingAssistantIdRef.current = null;
                    activeAssistantIdRef.current = null;
                    activeInputMessageIdRef.current = null;
                    activeTurnInputRef.current = null;
                    const confirmation: MessagePart = {
                        type: "text",
                        text: i18n.t("chat:command.executed", {
                            command: commandText ?? "",
                        }),
                    };
                    setMessages((prev) => {
                        // Pending placeholder still streaming (no reply event
                        // arrived yet) → replace it with the confirmation so
                        // the turn has visible feedback instead of reverting
                        // to the previous answer on screen.
                        if (pendingAssistantID) {
                            const pendingIndex = prev.findIndex(
                                (message) =>
                                    message.id === pendingAssistantID &&
                                    message.role === "assistant" &&
                                    message.status === "streaming",
                            );
                            if (pendingIndex !== -1) {
                                const next = [...prev];
                                const hasSkillList = next[pendingIndex].parts.some(
                                    (part) => part.type === "skill-list",
                                );
                                next[pendingIndex] = {
                                    ...next[pendingIndex],
                                    progress: undefined,
                                    parts: isSkillsCommand && hasSkillList
                                        ? next[pendingIndex].parts
                                        : [confirmation],
                                    status: "complete",
                                };
                                return next;
                            }
                        }
                        // Placeholder already holds content (a reply message
                        // beat the ack) → leave it untouched.
                        if (activeAssistantID) {
                            return prev;
                        }
                        // Done beat the ack and removed the placeholder →
                        // append the confirmation so the turn is still visible.
                        const lastMessage = prev[prev.length - 1];
                        if (
                            isSkillsCommand &&
                            lastMessage?.role === "assistant" &&
                            lastMessage.parts.some((part) => part.type === "skill-list")
                        ) {
                            return prev;
                        }
                        return [
                            ...prev,
                            {
                                id: `assistant-cmd-${Date.now()}`,
                                role: "assistant" as const,
                                parts: [confirmation],
                                createdAt: new Date(),
                                status: "complete",
                            },
                        ];
                    });
                    queueMicrotask(() => scheduleQueueDrainRef.current());
                    return;
                }
                setMessages((prev) =>
                    updatePendingAssistant(
                        prev,
                        pendingAssistantIdRef.current,
                        (message) => ({ ...message, progress: "accepted" }),
                    ),
                );
                return;
            }
            if (settlement.kind !== "unknown") return;
            logger.warn("RuntimeAdapter", "Input outcome unknown", {
                client_message_id: data.client_message_id,
                execution_id: data.execution_id,
                error_code: data.error_code,
            });
            setIsRunning(false);
            const pendingAssistantID = pendingAssistantIdRef.current;
            const activeAssistantID = activeAssistantIdRef.current;
            pendingAssistantIdRef.current = null;
            activeAssistantIdRef.current = null;
            activeInputMessageIdRef.current = null;
            activeTurnInputRef.current = null;
            setMessages((prev) => {
                const pending = updatePendingAssistant(
                    prev,
                    pendingAssistantID,
                    (message) => ({
                        ...message,
                        progress: undefined,
                        parts: [
                            {
                                type: "text" as const,
                                text: i18n.t("chat:error.input_unknown"),
                            },
                        ],
                        status: "complete",
                    }),
                );
                return pending !== prev
                    ? pending
                    : completeStreamingAssistant(prev, activeAssistantID);
            });
        };

        // Subscribe to events
        const handleConnected = () => {
            sessionAlreadyConnectedRef.current = false;
            setConnectionState("connected");

            // Decouple turn active state from connection session state.
            // On connection/reconnection, the turn is idle by default until/unless
            // incoming stream events or local dispatches happen.
            turnActiveRef.current = false;
            setIsRunning(false);

            let drainDeferredForReconcile = false;
            // A reconnect can miss the terminal Done after the queued input
            // was already acknowledged as delivered. The init ACK is the
            // authoritative session state, so idle completes that local
            // association and allows the next FIFO item to drain.
            const deliveredDispatch = activeQueueDispatchRef.current;
            if (deliveredDispatch?.delivered) {
                const pendingAssistantID = pendingAssistantIdRef.current;
                const activeAssistantID = activeAssistantIdRef.current;
                activeQueueDispatchRef.current = null;
                pendingAssistantIdRef.current = null;
                activeAssistantIdRef.current = null;
                activeInputMessageIdRef.current = null;
                activeTurnInputRef.current = null;
                setMessages((previous) => {
                    const completedPending = completeStreamingAssistant(
                        previous,
                        pendingAssistantID,
                    );
                    return completedPending !== previous
                        ? completedPending
                        : completeStreamingAssistant(
                              previous,
                              activeAssistantID,
                          );
                });
                drainDeferredForReconcile = true;
                void reconcileTurnContent({
                    targetAssistantId: deliveredDispatch.assistantMessageId,
                    inputContent: deliveredDispatch.text,
                }).finally(() => scheduleQueueDrainRef.current());
            }
            setIsStopping(false);
            stoppingRef.current = false;
            if (!drainDeferredForReconcile) {
                queueMicrotask(() => scheduleQueueDrainRef.current());
            }
        };

        client.on("connected", handleConnected);
        client.on("delta", handleDelta);
        client.on("message", handleMessage);
        client.on("inputAck", handleInputAck);
        client.on("done", handleDone);
        client.on("error", handleError);
        client.on("sessionAlreadyConnected", handleSessionAlreadyConnected);
        client.on("disconnected", handleDisconnected);
        client.on("reconnecting", handleReconnecting);
        client.on("reconnect_failed", handleReconnectFailed);
        client.on("reasoning", handleReasoning);
        client.on("messageStart", handleMessageStart);
        client.on("toolCall", handleToolCall);
        client.on("toolResult", handleToolResult);
        const handleState = (data: { state: string }) => {
            if (data.state === "terminated" || data.state === "deleted") {
                expireActiveInteractions();
            }
            onSessionStateChangeRef.current?.(data.state);
        };
        client.on("state", handleState);

        const handleContextUsage = (data: ContextUsageData) => {
            const names = data?.skills?.names ?? [];
            // ContextUsage only has names, not full entries — pass minimal entries
            const entries = names.map((name) => ({
                name,
                description: "",
                source: "context",
            }));
            onSkillsChangeRef.current?.(entries);

            // Inject into last assistant message's parts (same pattern as turn-summary in handleDone)
            setMessages((prev) => {
                const lastMessage = prev[prev.length - 1];
                if (lastMessage?.role === "assistant") {
                    return [
                        ...prev.slice(0, -1),
                        {
                            ...lastMessage,
                            parts: [
                                ...lastMessage.parts,
                                { type: "context-usage" as const, data },
                            ],
                        },
                    ];
                }
                return prev;
            });
        };
        client.on("contextUsage", handleContextUsage);
        const handleSkillsList = (
            data: import("@/lib/ai-sdk-transport/client/types").SkillsListData,
        ) => {
            skillsFetchedRef.current = true;
            const entries = data?.skills ?? [];
            onSkillsChangeRef.current?.(entries);
            const isSkillsCommand = activeTurnInputRef.current
                ?.trim()
                .toLowerCase()
                .startsWith("/skills");
            if (!isSkillsCommand) return;

            setMessages((prev) => {
                const lastMessage = prev[prev.length - 1];
                if (lastMessage?.role !== "assistant") return prev;
                const parts = lastMessage.parts.filter(
                    (part) => part.type !== "skill-list",
                );
                return [
                    ...prev.slice(0, -1),
                    {
                        ...lastMessage,
                        parts: [
                            ...parts,
                            { type: "skill-list" as const, skills: entries },
                        ],
                    },
                ];
            });
        };
        client.on("skillsList", handleSkillsList);

        // Interaction event handlers — inject as tool-call parts for PermissionCard rendering
        const handlePermissionRequest = (
            data: PermissionRequestData,
            _env: Envelope,
        ) => {
            if (!data) return;
            interactionMapRef.current.set(data.id, { type: "permission" });
            setMessages((prev) => {
                const lastMessage = prev[prev.length - 1];
                const newPart = {
                    type: "tool-call" as const,
                    toolName: "ask_permission",
                    args: {
                        description: data.description,
                        tool_name: data.tool_name,
                        args: data.args,
                        interaction: {
                            kind: "permission",
                            requestId: data.id,
                            status: "pending",
                            createdAt: Date.now(),
                            expiresAt: Date.now() + 5 * 60 * 1000,
                        },
                    },
                    toolCallId: data.id,
                };

                if (lastMessage?.role === "assistant") {
                    return [
                        ...prev.slice(0, -1),
                        {
                            ...lastMessage,
                            parts: [...lastMessage.parts, newPart],
                        },
                    ];
                }
                return [
                    ...prev,
                    {
                        id: `msg_assistant_${data.id}`,
                        role: "assistant" as const,
                        content: "",
                        parts: [newPart],
                        createdAt: new Date(),
                    },
                ];
            });
        };
        client.on("permissionRequest", handlePermissionRequest);
        client.on("permissionResponse", handlePermissionResponse);

        const handleQuestionRequest = (
            data: QuestionRequestData,
            _env: Envelope,
        ) => {
            if (!data) return;
            interactionMapRef.current.set(data.id, { type: "question" });
            setMessages((prev) => {
                const lastMessage = prev[prev.length - 1];
                const questionText =
                    data.questions?.map((q) => q.question).join("\n") || "";
                const newPart = {
                    type: "tool-call" as const,
                    toolName: "question_request",
                    args: {
                        description: questionText,
                        questions: data.questions,
                        interaction: {
                            kind: "question",
                            requestId: data.id,
                            status: "pending",
                            createdAt: Date.now(),
                            expiresAt: Date.now() + 5 * 60 * 1000,
                        },
                    },
                    toolCallId: data.id,
                };

                if (lastMessage?.role === "assistant") {
                    return [
                        ...prev.slice(0, -1),
                        {
                            ...lastMessage,
                            parts: [...lastMessage.parts, newPart],
                        },
                    ];
                }
                return [
                    ...prev,
                    {
                        id: `msg_assistant_${data.id}`,
                        role: "assistant" as const,
                        content: "",
                        parts: [newPart],
                        createdAt: new Date(),
                    },
                ];
            });
        };
        client.on("questionRequest", handleQuestionRequest);
        client.on("questionResponse", handleQuestionResponse);

        const handleElicitationRequest = (
            data: ElicitationRequestData,
            _env: Envelope,
        ) => {
            if (!data) return;
            interactionMapRef.current.set(data.id, { type: "elicitation" });
            setMessages((prev) => {
                const lastMessage = prev[prev.length - 1];
                const newPart = {
                    type: "tool-call" as const,
                    toolName: "elicitation",
                    args: {
                        message: data.message,
                        mcp_server_name: data.mcp_server_name,
                        url: data.url,
                        interaction: {
                            kind: "elicitation",
                            requestId: data.id,
                            status: "pending",
                            createdAt: Date.now(),
                            expiresAt: Date.now() + 5 * 60 * 1000,
                        },
                    },
                    toolCallId: data.id,
                };

                if (lastMessage?.role === "assistant") {
                    return [
                        ...prev.slice(0, -1),
                        {
                            ...lastMessage,
                            parts: [...lastMessage.parts, newPart],
                        },
                    ];
                }
                return [
                    ...prev,
                    {
                        id: `msg_assistant_${data.id}`,
                        role: "assistant" as const,
                        content: "",
                        parts: [newPart],
                        createdAt: new Date(),
                    },
                ];
            });
        };
        client.on("elicitationRequest", handleElicitationRequest);
        client.on("elicitationResponse", handleElicitationResponse);

        setConnectionState("connecting");
        client
            .connect(sessionId)
            .then(() => undefined)
            .catch((err) => {
                if (sessionAlreadyConnectedRef.current) {
                    logger.info("RuntimeAdapter", "Session already connected", {
                        error: String(err),
                    });
                    return;
                }
                setConnectionState("disconnected");
                const msg = String(err);
                // 'Client disconnected' and 'Client is closed' are normal during
                // session switching (React cleanup calls disconnect() which rejects
                // the pending connect promise). Only log unexpected failures.
                if (
                    msg.includes("Client disconnected") ||
                    msg.includes("Client is closed")
                ) {
                    logger.info(
                        "RuntimeAdapter",
                        "Connection aborted (session switch)",
                        { reason: msg },
                    );
                } else {
                    logger.error("RuntimeAdapter", "Connection failed", {
                        error: msg,
                    });
                }
            });

        return () => {
            // Mark this effect instance as torn down so async paths
            // (reconcileTurnContent) skip their setMessages after unmount.
            cancelled = true;
            const activeQueueDispatch = activeQueueDispatchRef.current;
            if (activeQueueDispatch && !activeQueueDispatch.delivered) {
                failActiveQueueDispatchRef.current(
                    "connection",
                    "Session connection was replaced",
                );
            }
            activeQueueDispatchRef.current = null;
            turnActiveRef.current = false;
            client.off("connected", handleConnected);
            client.off("delta", handleDelta);
            client.off("message", handleMessage);
            client.off("inputAck", handleInputAck);
            client.off("done", handleDone);
            client.off("error", handleError);
            client.off(
                "sessionAlreadyConnected",
                handleSessionAlreadyConnected,
            );
            client.off("disconnected", handleDisconnected);
            client.off("reconnecting", handleReconnecting);
            client.off("reconnect_failed", handleReconnectFailed);
            client.off("reasoning", handleReasoning);
            client.off("messageStart", handleMessageStart);
            client.off("toolCall", handleToolCall);
            client.off("toolResult", handleToolResult);
            client.off("state", handleState);
            client.off("contextUsage", handleContextUsage);
            client.off("skillsList", handleSkillsList);
            client.off("permissionRequest", handlePermissionRequest);
            client.off("permissionResponse", handlePermissionResponse);
            client.off("questionRequest", handleQuestionRequest);
            client.off("questionResponse", handleQuestionResponse);
            client.off("elicitationRequest", handleElicitationRequest);
            client.off("elicitationResponse", handleElicitationResponse);
            // eslint-disable-next-line react-hooks/exhaustive-deps -- interactionMapRef is a stable singleton map
            interactionMapRef.current.clear();
            // interactionAckTimersRef holds timers added dynamically by event
            // handlers after setup, so cleanup must drain the current map at
            // teardown — copying a setup-time snapshot would leak late timers.
            // eslint-disable-next-line react-hooks/exhaustive-deps
            for (const timer of interactionAckTimersRef.current.values()) {
                clearTimeout(timer);
            }
            interactionAckTimersRef.current.clear();
            client.disconnect();
            clientRef.current = null;
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps -- connection lifecycle effect; refs/recordTurn intentionally excluded to avoid reconnect churn
    }, [sessionId, workspaceId, sessionWorkerType]);

    // Track pending connection-wait state so useEffect cleanup can tear it down
    const connectionWaitRef = useRef<{
        timeout: ReturnType<typeof setTimeout>;
        onConnected: () => void;
        onDisconnected: (reason: string) => void;
    } | null>(null);

    // Cleanup: tear down any in-flight connection wait if the component unmounts
    useEffect(() => {
        return () => {
            const wait = connectionWaitRef.current;
            if (wait) {
                clearTimeout(wait.timeout);
                clientRef.current?.off("connected", wait.onConnected);
                clientRef.current?.off("disconnected", wait.onDisconnected);
                connectionWaitRef.current = null;
            }
        };
    }, []);

    const dispatchInput = useCallback(
        async (textContent: string, queueItemId?: string): Promise<string> => {
            const client = clientRef.current;
            if (!client) {
                throw new Error("HotPlex client not initialized.");
            }
            const localId =
                queueItemId ??
                `${Date.now()}-${Math.random().toString(36).slice(2)}`;
            const clientMessageId = client.createClientMessageId();
            const userMessage: HotPlexMessage = {
                id: `user-${localId}`,
                role: "user",
                clientMessageId,
                parts: [{ type: "text", text: textContent }],
                createdAt: new Date(),
                status: "complete",
                deliveryStatus: "pending",
            };
            const assistantID = `assistant-local-${localId}`;
            const pendingAssistant = createPendingAssistantMessage(
                assistantID,
                new Date(),
            );
            const rollbackOptimisticInput = () => {
                if (activeQueueDispatchRef.current?.itemId === queueItemId) {
                    activeQueueDispatchRef.current = null;
                }
                pendingAssistantIdRef.current = null;
                activeAssistantIdRef.current = null;
                activeInputMessageIdRef.current = null;
                activeTurnInputRef.current = null;
                turnActiveRef.current = false;
                setIsRunning(false);
                setMessages((prev) =>
                    prev.filter(
                        (current) =>
                            current.id !== userMessage.id &&
                            current.id !== assistantID,
                    ),
                );
            };

            // Insert feedback before reconnecting: WebSocket recovery can take
            // seconds, but it must not create a blank interval in the thread.
            pendingAssistantIdRef.current = assistantID;
            activeAssistantIdRef.current = assistantID;
            activeInputMessageIdRef.current = null;
            activeTurnInputRef.current = textContent;
            turnActiveRef.current = true;
            if (queueItemId && sessionIdRef.current) {
                activeQueueDispatchRef.current = {
                    sessionId: sessionIdRef.current,
                    itemId: queueItemId,
                    userMessageId: userMessage.id,
                    assistantMessageId: assistantID,
                    delivered: false,
                    text: textContent,
                };
            }
            setMessages((prev) => [...prev, userMessage, pendingAssistant]);
            setIsRunning(true);
            startTurn();

            // Handle disconnected state: attempt to reconnect if not already connecting
            if (!client.connected) {
                logger.info(
                    "RuntimeAdapter",
                    "Client not connected, attempting reconnect",
                );
                try {
                    if (!client.connecting) {
                        // Don't pass sessionId here — the client internally tracks the latest session ID,
                        // which may have been updated by a SessionNotFound retry in BrowserHotPlexClient.
                        client.connect().catch((err) => {
                            logger.error(
                                "RuntimeAdapter",
                                "Auto-connect failed",
                                {
                                    error: String(err),
                                },
                            );
                        });
                    }

                    // Wait for connection (up to 30s)
                    await new Promise<void>((resolve, reject) => {
                        let settled = false;
                        const settle = (fn: () => void) => {
                            if (settled) return;
                            settled = true;
                            clearTimeout(timeout);
                            client.off("connected", onConnected);
                            client.off("disconnected", onDisconnected);
                            connectionWaitRef.current = null;
                            fn();
                        };

                        const timeout = setTimeout(() => {
                            settle(() =>
                                reject(
                                    new Error(
                                        "Connection timeout. Please check your network.",
                                    ),
                                ),
                            );
                        }, 30000);

                        const onConnected = () => {
                            settle(() => resolve());
                        };
                        const onDisconnected = (reason: string) => {
                            settle(() =>
                                reject(
                                    new Error(`Connection failed: ${reason}`),
                                ),
                            );
                        };

                        connectionWaitRef.current = {
                            timeout,
                            onConnected,
                            onDisconnected,
                        };
                        client.on("connected", onConnected);
                        client.on("disconnected", onDisconnected);

                        // Check if it connected while we were setting up listeners
                        if (client.connected) {
                            settle(() => resolve());
                        }
                    });
                } catch (err) {
                    rollbackOptimisticInput();
                    throw new Error(
                        err instanceof Error
                            ? err.message
                            : "HotPlex client not connected. Please check your network.",
                    );
                }
            }

            // Send to HotPlex gateway with error handling
            try {
                const sentClientMessageId = client.sendInput(
                    textContent,
                    clientMessageId,
                );
                activeInputMessageIdRef.current = sentClientMessageId;
                // Keep the optimistic record correlated even if a caller
                // supplies a transport implementation that normalizes IDs.
                setMessages((prev) =>
                    prev.map((message) =>
                        message.id === userMessage.id
                            ? {
                                  ...message,
                                  clientMessageId: sentClientMessageId,
                              }
                            : message,
                    ),
                );
                const activeQueueDispatch = activeQueueDispatchRef.current;
                if (
                    queueItemId &&
                    activeQueueDispatch?.itemId === queueItemId &&
                    activeQueueDispatch.sessionId === sessionIdRef.current
                ) {
                    activeQueueDispatch.clientMessageId = sentClientMessageId;
                    if (
                        !queueStore.attachClientMessageId(
                            activeQueueDispatch.sessionId,
                            queueItemId,
                            sentClientMessageId,
                        )
                    ) {
                        failActiveQueueDispatchRef.current(
                            "unknown",
                            "Queue dispatch correlation was lost",
                        );
                        throw new Error("Queue dispatch correlation was lost");
                    }
                }
                return sentClientMessageId;
            } catch (err) {
                rollbackOptimisticInput();
                throw err;
            }
        },
        [queueStore, startTurn],
    );

    const drainQueue = useCallback(async () => {
        if (drainInFlightRef.current) return;
        const sid = sessionIdRef.current;
        const client = clientRef.current;
        if (
            !sid ||
            !client?.connected ||
            turnActiveRef.current ||
            stoppingRef.current ||
            activeQueueDispatchRef.current
        ) {
            return;
        }
        const items = queueStore.popAllDispatchable(sid);
        if (items.length === 0) return;

        drainInFlightRef.current = true;
        const mergedText = items.map((item) => item.text).join("\n\n");
        try {
            await dispatchInput(mergedText);
        } catch (err) {
            const message = err instanceof Error ? err.message : String(err);
            logger.error(
                "RuntimeAdapter",
                "Failed to send merged queued items",
                { error: message },
            );
            for (const item of items) {
                const result = queueStore.enqueue(sid, item.text);
                if (result.ok && result.item) {
                    queueStore.markFailed(sid, result.item.id, "send", message);
                }
            }
        } finally {
            drainInFlightRef.current = false;
        }
    }, [dispatchInput, queueStore]);

    scheduleQueueDrainRef.current = () => {
        queueMicrotask(() => void drainQueue());
    };

    const enqueueFollowUp = useCallback(
        (text: string) => {
            const sid = sessionIdRef.current;
            if (!sid) return { ok: false, reason: "blank" } as const;
            const result = queueStore.enqueue(sid, text);
            if (result.ok && !turnActiveRef.current && !stoppingRef.current) {
                scheduleQueueDrainRef.current();
            }
            return result;
        },
        [queueStore],
    );

    const updateFollowUp = useCallback(
        (itemId: string, text: string) => {
            const sid = sessionIdRef.current;
            return sid ? queueStore.updateText(sid, itemId, text) : false;
        },
        [queueStore],
    );

    const releaseUnknownQueueDispatch = useCallback((itemId: string) => {
        const active = activeQueueDispatchRef.current;
        if (active?.itemId === itemId && active.outcomeUnknown) {
            activeQueueDispatchRef.current = null;
            if (pendingAssistantIdRef.current === active.assistantMessageId) {
                pendingAssistantIdRef.current = null;
            }
            if (activeAssistantIdRef.current === active.assistantMessageId) {
                activeAssistantIdRef.current = null;
            }
            activeInputMessageIdRef.current = null;
            activeTurnInputRef.current = null;
            setMessages((previous) =>
                previous.filter(
                    (message) =>
                        message.id !== active.userMessageId &&
                        message.id !== active.assistantMessageId,
                ),
            );
            clientRef.current?.acknowledgeUnknownInputForRetry();
        }
    }, []);

    const removeFollowUp = useCallback(
        (itemId: string) => {
            const sid = sessionIdRef.current;
            if (!sid || !queueStore.remove(sid, itemId)) return false;
            releaseUnknownQueueDispatch(itemId);
            if (!turnActiveRef.current && !stoppingRef.current) {
                scheduleQueueDrainRef.current();
            }
            return true;
        },
        [queueStore, releaseUnknownQueueDispatch],
    );

    const retryFollowUp = useCallback(
        (itemId: string) => {
            const sid = sessionIdRef.current;
            if (!sid) return false;
            const item = queueStore
                .getSnapshot(sid)
                .find((current) => current.id === itemId);
            if (!item || item.status !== "failed") return false;
            if (item.errorKind === "unknown") {
                releaseUnknownQueueDispatch(itemId);
            }
            if (!queueStore.retry(sid, itemId)) return false;
            scheduleQueueDrainRef.current();
            return true;
        },
        [queueStore, releaseUnknownQueueDispatch],
    );

    const [isStopping, setIsStopping] = useState(false);
    const stoppingRef = useRef(false);

    const sendFollowUpNow = useCallback(
        async (itemId: string) => {
            const sid = sessionIdRef.current;
            const client = clientRef.current;
            if (!sid || stoppingRef.current) return;

            if (turnActiveRef.current && !client?.connected) {
                if (queueStore.prepareSendNow(sid, itemId)) {
                    queueStore.markFailed(
                        sid,
                        itemId,
                        "connection",
                        "Cannot stop the active turn while disconnected",
                    );
                }
                return;
            }
            const item = queueStore
                .getSnapshot(sid)
                .find((current) => current.id === itemId);
            if (!item || !queueStore.prepareSendNow(sid, itemId)) return;
            if (item.errorKind === "unknown") {
                releaseUnknownQueueDispatch(itemId);
            }

            if (!turnActiveRef.current) {
                scheduleQueueDrainRef.current();
                return;
            }
            if (!client?.connected) return;

            stoppingRef.current = true;
            setIsStopping(true);
            try {
                await client.stopCurrentTurn();
            } catch (err) {
                const message =
                    err instanceof Error ? err.message : String(err);
                queueStore.markFailed(sid, itemId, "stop", message);
                setIsStopping(false);
                stoppingRef.current = false;
            }
        },
        [queueStore, releaseUnknownQueueDispatch],
    );

    // Handler for new messages (from assistant-ui Composer). The custom
    // composer calls enqueueFollowUp while running because assistant-ui's
    // external-store runtime intentionally disables its built-in queue.
    const handleNew = useCallback(
        async (message: AppendMessage) => {
            const textContent = Array.isArray(message.content)
                ? message.content
                      .filter(
                          (part): part is { type: "text"; text: string } =>
                              part.type === "text",
                      )
                      .map((part) => part.text)
                      .join("")
                : "";
            if (!textContent.trim()) return;
            if (textContent.trim() === "/") {
                // Bare "/" is the composer's command-menu trigger, never a
                // message. Last-resort guard for submit paths that bypass
                // the composer guard (programmatic submit, future UI).
                logger.debug("RuntimeAdapter", "Ignoring bare slash input");
                return;
            }
            if (turnActiveRef.current || stoppingRef.current) {
                const result = enqueueFollowUp(textContent);
                if (!result.ok && result.reason === "limit") {
                    throw new Error(i18n.t("chat:follow_up.error.limit"));
                }
                return;
            }

            try {
                await dispatchInput(textContent);
            } catch (err) {
                const errMsg = err instanceof Error ? err.message : String(err);
                if (errMsg === "Input already pending") {
                    // A previous turn is still in flight (often an unknown-outcome
                    // tombstone). This is not a connection fault.
                    logger.warn(
                        "RuntimeAdapter",
                        "sendInput blocked: input already pending",
                    );
                    throw new Error(
                        i18n.t("chat:error.input_still_processing"),
                    );
                }
                logger.error("RuntimeAdapter", "sendInput failed", {
                    error: errMsg,
                });
                throw new Error(i18n.t("chat:error.send_failed_connection"));
            }
        },
        [dispatchInput, enqueueFollowUp],
    );

    const handleCancel = useCallback(async () => {
        if (stoppingRef.current) return;
        cancelledAssistantIdRef.current = activeAssistantIdRef.current;
        stoppingRef.current = true;
        setIsStopping(true);
        const client = clientRef.current;
        if (!client?.connected) {
            setIsStopping(false);
            stoppingRef.current = false;
            return;
        }
        try {
            await client.stopCurrentTurn();
        } catch (err) {
            logger.warn("RuntimeAdapter", "Stop did not reach terminal state", {
                error: String(err),
            });
            setIsStopping(false);
            stoppingRef.current = false;
        }
    }, []);

    // This is deliberately an explicit one-shot action. A duplicate-session
    // rejection never schedules reconnects; after the user closes the other
    // owner they may choose to try this connection again.
    const retrySessionConnection = useCallback(async () => {
        const client = clientRef.current;
        if (!client || !sessionIdRef.current) {
            return;
        }
        setConnectionState("connecting");
        try {
            await client.connect(sessionIdRef.current);
            sessionAlreadyConnectedRef.current = false;
            setConnectionState("connected");
        } catch (err) {
            sessionAlreadyConnectedRef.current = true;
            setConnectionState("already_connected");
            logger.info(
                "RuntimeAdapter",
                "Explicit duplicate-session retry failed",
                {
                    error: String(err),
                },
            );
        }
    }, []);

    // Handler for loading earlier messages (cursor-based pagination)
    const handleLoadHistory = useCallback(async (): Promise<{
        hasMore: boolean;
    }> => {
        const sid = sessionIdRef.current;
        if (!sid || historyLoadingRef.current) return { hasMore: false };
        historyLoadingRef.current = true;

        try {
            // Use cached minId for cursor (updated when loading history from server)
            const cursorId = minIdRef.current;
            if (!cursorId) return { hasMore: false };

            const res = await getSessionHistory(sid, {
                beforeId: cursorId,
                limit: 50,
            });
            if (res.records.length > 0) {
                const olderMessages = historyToMessages(res.records);
                setMessages((prev) => {
                    const existingIds = new Set(prev.map((m) => m.id));
                    const newOnly = olderMessages.filter(
                        (m) => !existingIds.has(m.id),
                    );
                    return [...newOnly, ...prev];
                });
                // Update minId cache for next page
                if (olderMessages.length > 0) {
                    const extracted = extractMinDbId(olderMessages);
                    minIdRef.current = extracted || cursorId;
                }
            }
            setHistoryHasMore(res.has_more);
            return { hasMore: res.has_more };
        } catch (err) {
            logger.warn("RuntimeAdapter", "Failed to load earlier messages", {
                error: String(err),
            });
            return { hasMore: false };
        } finally {
            historyLoadingRef.current = false;
        }
    }, []);

    // Interaction response callback — routes to the correct send method
    const handleInteractionRespond = useCallback(
        async (
            toolCallId: string,
            response: {
                type: "permission" | "question" | "elicitation";
                allowed?: boolean;
                reason?: string;
                answers?: Record<string, string | string[]>;
                action?: "accept" | "decline" | "cancel";
                content?: Record<string, unknown>;
            },
        ) => {
            const client = clientRef.current;
            if (!client) return;

            // Transition status to submitting
            setMessages((prev) => {
                return prev.map((msg) => {
                    if (msg.role !== "assistant") return msg;
                    let found = false;
                    const parts = msg.parts.map((p) => {
                        if (
                            p.type === "tool-call" &&
                            p.toolCallId === toolCallId
                        ) {
                            found = true;
                            const inter = p.args?.interaction;
                            return {
                                ...p,
                                args: {
                                    ...p.args,
                                    interaction: {
                                        ...inter,
                                        status: "submitting" as const,
                                    },
                                },
                            };
                        }
                        return p;
                    });
                    return found ? { ...msg, parts } : msg;
                });
            });

            try {
                switch (response.type) {
                    case "permission":
                        client.sendPermissionResponse(
                            toolCallId,
                            response.allowed ?? false,
                            response.reason,
                        );
                        break;
                    case "question":
                        client.sendQuestionResponse(
                            toolCallId,
                            response.answers ?? {},
                        );
                        break;
                    case "elicitation":
                        client.sendElicitationResponse(
                            toolCallId,
                            response.action ?? "cancel",
                            response.content,
                        );
                        break;
                }

                // WebSocket send only proves that the frame entered the socket.
                // Stay in submitting until Gateway echoes the correlated response
                // after the Worker native endpoint accepts it.
                const existingTimer =
                    interactionAckTimersRef.current.get(toolCallId);
                if (existingTimer) clearTimeout(existingTimer);
                const timer = setTimeout(() => {
                    interactionAckTimersRef.current.delete(toolCallId);
                    setMessages((prev) =>
                        prev.map((msg) => {
                            if (msg.role !== "assistant") return msg;
                            let found = false;
                            const parts = msg.parts.map((part) => {
                                if (
                                    part.type !== "tool-call" ||
                                    part.toolCallId !== toolCallId
                                )
                                    return part;
                                const interaction = part.args?.interaction;
                                if (
                                    !interaction ||
                                    interaction.status !== "submitting"
                                )
                                    return part;
                                found = true;
                                return {
                                    ...part,
                                    args: {
                                        ...part.args,
                                        interaction: {
                                            ...interaction,
                                            status: "failed" as const,
                                            error: "Timed out waiting for the Worker to confirm this response",
                                        },
                                    },
                                };
                            });
                            return found ? { ...msg, parts } : msg;
                        }),
                    );
                }, 15_000);
                interactionAckTimersRef.current.set(toolCallId, timer);
            } catch (err) {
                const timer = interactionAckTimersRef.current.get(toolCallId);
                if (timer) {
                    clearTimeout(timer);
                    interactionAckTimersRef.current.delete(toolCallId);
                }
                // Transition status to failed
                setMessages((prev) => {
                    return prev.map((msg) => {
                        if (msg.role !== "assistant") return msg;
                        let found = false;
                        const parts = msg.parts.map((p) => {
                            if (
                                p.type === "tool-call" &&
                                p.toolCallId === toolCallId
                            ) {
                                found = true;
                                const inter = p.args?.interaction;
                                return {
                                    ...p,
                                    args: {
                                        ...p.args,
                                        interaction: {
                                            ...inter,
                                            status: "failed" as const,
                                            error: String(err),
                                        },
                                    },
                                };
                            }
                            return p;
                        });
                        return found ? { ...msg, parts } : msg;
                    });
                });
            }
        },
        [],
    );

    // Deduped messages for assistant-ui. Two-layer dedup prevents the
    // MessageRepository "same id already exists" error:
    //   Layer 1: exact ID dedup
    //   Layer 2: content-signature dedup for assistant messages (catches live-vs-server
    //            duplicates where the streaming message has a different ID than the history record).
    //            Cleared on each user message so different turns with same content aren't deduped.
    // Also filters out internal-only parts (context-usage, turn-summary).
    const adapterMessages = useMemo(() => {
        const seenIds = new Set<string>();
        const seenAssistantSigs = new Set<string>();
        const result = messages
            .filter(
                (m): m is HotPlexMessage =>
                    !!m && (m.role === "user" || m.role === "assistant"),
            )
            .filter(isVisibleAdapterMessage)
            .filter((m) => {
                // Layer 1: exact ID dedup
                if (seenIds.has(m.id)) return false;
                seenIds.add(m.id);
                // Each user message starts a new turn — reset assistant dedup so
                // different turns with identical short responses (e.g. "汪汪") aren't merged.
                if (m.role === "user") {
                    seenAssistantSigs.clear();
                }
                // Layer 2: content-signature dedup for assistant messages
                if (m.role === "assistant") {
                    let sig = "";
                    for (const p of m.parts) {
                        if (p.type === "text") {
                            sig += (p as TextPart).text;
                            if (sig.length >= CONTENT_SIG_PREFIX) break;
                        }
                    }
                    sig = sig.slice(0, CONTENT_SIG_PREFIX);
                    if (sig) {
                        if (seenAssistantSigs.has(sig)) return false;
                        seenAssistantSigs.add(sig);
                    }
                }
                return true;
            });
        return result;
    }, [messages]);

    const threadMessages = useMemo(
        () => adapterMessages.map((m) => convertToThreadMessage(m)),
        [adapterMessages],
    );

    // Stable setMessages callback to prevent adapter churn
    const handleSetMessages = useCallback((msgs: readonly HotPlexMessage[]) => {
        setMessages((current) => {
            const cancelledAssistantID = cancelledAssistantIdRef.current;
            if (cancelledAssistantID) {
                const incomingMessage = msgs.find(
                    (message) => message.id === cancelledAssistantID,
                );
                // assistant-ui resynchronizes its pre-cancel repository on a
                // timer. If done wins that race, the snapshot is older than
                // the adapter state and would regress the stopped response to
                // an empty streaming placeholder.
                if (
                    incomingMessage?.role === "assistant" &&
                    incomingMessage.status === "streaming"
                ) {
                    cancelledAssistantIdRef.current = null;
                    return current;
                }
            }
            return [...msgs];
        });
    }, []);

    // Stable capabilities reference
    const capabilities = useMemo(
        () => ({
            copy: true,
            edit: true,
        }),
        [],
    );

    const followUpQueue = useMemo<FollowUpQueueControls>(
        () => ({
            items: queuedItems,
            enqueue: enqueueFollowUp,
            updateText: updateFollowUp,
            remove: removeFollowUp,
            retry: retryFollowUp,
            sendNow: sendFollowUpNow,
        }),
        [
            queuedItems,
            enqueueFollowUp,
            updateFollowUp,
            removeFollowUp,
            retryFollowUp,
            sendFollowUpNow,
        ],
    );

    // Stable extras reference — only changes when metrics or history state change
    const extras = useMemo(
        () => ({
            metrics: sessionMetrics,
            hasMore: historyHasMore,
            onLoadHistory: handleLoadHistory,
            onInteractionRespond: handleInteractionRespond,
            isStopping,
            connectionState,
            onRetryConnection: retrySessionConnection,
            followUpQueue,
        }),
        [
            sessionMetrics,
            historyHasMore,
            handleLoadHistory,
            handleInteractionRespond,
            isStopping,
            connectionState,
            retrySessionConnection,
            followUpQueue,
        ],
    );

    // Return ExternalStoreAdapter — memoized to prevent unnecessary setAdapter calls
    // connectionState is returned separately to avoid invalidating the adapter memo on reconnect.
    return useMemo(
        () =>
            ({
                // State
                isRunning,
                messages: adapterMessages,
                threadMessages,
                suggestions,
                setMessages: handleSetMessages,

                // Message conversion
                convertMessage: convertToThreadMessage,

                // Event handlers
                onNew: handleNew,
                onCancel: handleCancel,

                // Capabilities — Phase 3: branching and editing enabled
                unstable_capabilities: capabilities,

                // Metrics — exposed for session dashboard (spec §4.5)
                extras,

                // Connection state — separate from adapter to avoid memo churn
                connectionState,
            }) as ExternalStoreAdapter<HotPlexMessage> & {
                connectionState: ConnectionState;
            },
        // eslint-disable-next-line react-hooks/exhaustive-deps -- connectionState intentionally excluded to avoid memo churn (see comment above)
        [
            isRunning,
            adapterMessages,
            threadMessages,
            suggestions,
            handleSetMessages,
            handleNew,
            handleCancel,
            capabilities,
            extras,
        ],
    );
}
