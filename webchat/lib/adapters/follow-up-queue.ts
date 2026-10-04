export const FOLLOW_UP_QUEUE_LIMIT = 20;

export type FollowUpQueueStatus = "queued" | "sending" | "failed";

export type FollowUpQueueErrorKind =
    | "unknown"
    | "connection"
    | "busy"
    | "send"
    | "stop";

export interface FollowUpQueueItem {
    readonly id: string;
    readonly text: string;
    readonly createdAt: number;
    readonly sequence: number;
    readonly status: FollowUpQueueStatus;
    readonly clientMessageId?: string;
    readonly errorKind?: FollowUpQueueErrorKind;
    readonly errorMessage?: string;
}

export type FollowUpQueueEnqueueResult =
    | { readonly ok: true; readonly item: FollowUpQueueItem }
    | { readonly ok: false; readonly reason: "blank" | "limit" };

export interface FollowUpQueueControls {
    readonly items: readonly FollowUpQueueItem[];
    enqueue(text: string): FollowUpQueueEnqueueResult;
    updateText(itemId: string, text: string): boolean;
    remove(itemId: string): boolean;
    retry(itemId: string): boolean;
    sendNow(itemId: string): Promise<void>;
}

interface FollowUpQueueStoreOptions {
    readonly limit?: number;
    readonly createId?: () => string;
    readonly now?: () => number;
}

const EMPTY_QUEUE: readonly FollowUpQueueItem[] = Object.freeze([]);
let fallbackIdSequence = 0;

function defaultCreateId(): string {
    if (typeof globalThis.crypto?.randomUUID === "function") {
        return globalThis.crypto.randomUUID();
    }
    fallbackIdSequence += 1;
    return `queue-${Date.now()}-${fallbackIdSequence}`;
}

/**
 * Page-local, session-isolated storage for prompts that have not been
 * dispatched to the Gateway yet. The store deliberately has no persistence,
 * logging, or transport dependencies.
 */
export class FollowUpQueueStore {
    private readonly queues = new Map<
        string,
        readonly FollowUpQueueItem[]
    >();
    private readonly listeners = new Set<() => void>();
    private readonly limit: number;
    private readonly createId: () => string;
    private readonly now: () => number;
    private nextSequence = 0;

    constructor(options: FollowUpQueueStoreOptions = {}) {
        this.limit = options.limit ?? FOLLOW_UP_QUEUE_LIMIT;
        this.createId = options.createId ?? defaultCreateId;
        this.now = options.now ?? Date.now;
    }

    subscribe = (listener: () => void): (() => void) => {
        this.listeners.add(listener);
        return () => this.listeners.delete(listener);
    };

    getSnapshot(sessionId: string | undefined): readonly FollowUpQueueItem[] {
        if (!sessionId) return EMPTY_QUEUE;
        return this.queues.get(sessionId) ?? EMPTY_QUEUE;
    }

    enqueue(sessionId: string, text: string): FollowUpQueueEnqueueResult {
        if (!text.trim()) return { ok: false, reason: "blank" };
        const queue = this.getSnapshot(sessionId);
        if (queue.length >= this.limit) {
            return { ok: false, reason: "limit" };
        }

        this.nextSequence += 1;
        const item: FollowUpQueueItem = {
            id: this.createId(),
            text,
            createdAt: this.now(),
            sequence: this.nextSequence,
            status: "queued",
        };
        this.setQueue(sessionId, [...queue, item]);
        return { ok: true, item };
    }

    updateText(sessionId: string, itemId: string, text: string): boolean {
        if (!text.trim()) return false;
        return this.updateItem(sessionId, itemId, (item) => {
            if (item.status === "sending" || item.text === text) return null;
            return { ...item, text };
        });
    }

    remove(sessionId: string, itemId: string): boolean {
        const queue = this.getSnapshot(sessionId);
        const index = queue.findIndex((item) => item.id === itemId);
        if (index === -1 || queue[index]?.status === "sending") return false;
        this.setQueue(sessionId, [
            ...queue.slice(0, index),
            ...queue.slice(index + 1),
        ]);
        return true;
    }

    prepareSendNow(sessionId: string, itemId: string): boolean {
        const queue = this.getSnapshot(sessionId);
        const index = queue.findIndex((item) => item.id === itemId);
        const item = queue[index];
        if (!item || item.status === "sending") return false;

        const prepared: FollowUpQueueItem = {
            ...item,
            status: "queued",
            clientMessageId: undefined,
            errorKind: undefined,
            errorMessage: undefined,
        };
        const rest = queue.filter((current) => current.id !== itemId);
        const next = [prepared, ...rest];
        const changed =
            index !== 0 ||
            item.status !== "queued" ||
            item.clientMessageId !== undefined ||
            item.errorKind !== undefined ||
            item.errorMessage !== undefined;
        if (changed) this.setQueue(sessionId, next);
        return true;
    }

    peekDispatchable(sessionId: string): FollowUpQueueItem | null {
        const head = this.getSnapshot(sessionId)[0];
        return head?.status === "queued" ? head : null;
    }

    markSending(sessionId: string, itemId: string): boolean {
        const head = this.getSnapshot(sessionId)[0];
        if (!head || head.id !== itemId || head.status !== "queued") {
            return false;
        }
        return this.updateItem(sessionId, itemId, (item) => ({
            ...item,
            status: "sending",
            clientMessageId: undefined,
            errorKind: undefined,
            errorMessage: undefined,
        }));
    }

    attachClientMessageId(
        sessionId: string,
        itemId: string,
        clientMessageId: string,
    ): boolean {
        return this.updateItem(sessionId, itemId, (item) => {
            if (item.status !== "sending") return null;
            return { ...item, clientMessageId };
        });
    }

    markDelivered(
        sessionId: string,
        clientMessageId: string,
    ): FollowUpQueueItem | null {
        const queue = this.getSnapshot(sessionId);
        const index = queue.findIndex(
            (item) =>
                (item.status === "sending" ||
                    (item.status === "failed" && item.errorKind === "unknown")) &&
                item.clientMessageId === clientMessageId,
        );
        if (index === -1) return null;
        const delivered = queue[index] ?? null;
        this.setQueue(sessionId, [
            ...queue.slice(0, index),
            ...queue.slice(index + 1),
        ]);
        return delivered;
    }

    markFailed(
        sessionId: string,
        itemId: string,
        errorKind: FollowUpQueueErrorKind,
        errorMessage: string,
    ): boolean {
        return this.updateItem(sessionId, itemId, (item) => ({
            ...item,
            status: "failed",
            clientMessageId:
                errorKind === "unknown" ? item.clientMessageId : undefined,
            errorKind,
            errorMessage,
        }));
    }

    /**
     * Release a slot whose input the gateway accepted into volatile (in-process)
     * storage. Unlike markDelivered this asserts nothing about Worker delivery:
     * the supplement buffer is not a delivery proof, it only means the gateway
     * owns the input now and the queue must stop waiting on it. Without this the
     * head-of-line item would stay "sending" forever and block every later item.
     */
    releaseVolatileAccepted(
        sessionId: string,
        clientMessageId: string,
    ): FollowUpQueueItem | null {
        const queue = this.getSnapshot(sessionId);
        const index = queue.findIndex(
            (item) =>
                item.status === "sending" &&
                item.clientMessageId === clientMessageId,
        );
        if (index === -1) return null;
        const released = queue[index] ?? null;
        this.setQueue(sessionId, [
            ...queue.slice(0, index),
            ...queue.slice(index + 1),
        ]);
        return released;
    }

    retry(sessionId: string, itemId: string): boolean {
        return this.updateItem(sessionId, itemId, (item) => {
            if (item.status !== "failed") return null;
            return {
                ...item,
                status: "queued",
                clientMessageId: undefined,
                errorKind: undefined,
                errorMessage: undefined,
            };
        });
    }

    popAllDispatchable(sessionId: string): readonly FollowUpQueueItem[] {
        const queue = this.getSnapshot(sessionId);
        const dispatchable = queue.filter((item) => item.status === "queued");
        if (dispatchable.length === 0) return EMPTY_QUEUE;
        const remaining = queue.filter((item) => item.status !== "queued");
        this.setQueue(sessionId, remaining);
        return dispatchable;
    }

    clearSession(sessionId: string): void {
        if (!this.queues.has(sessionId)) return;
        this.queues.delete(sessionId);
        this.emitChange();
    }

    private updateItem(
        sessionId: string,
        itemId: string,
        update: (
            item: FollowUpQueueItem,
        ) => FollowUpQueueItem | null,
    ): boolean {
        const queue = this.getSnapshot(sessionId);
        const index = queue.findIndex((item) => item.id === itemId);
        const item = queue[index];
        if (!item) return false;
        const nextItem = update(item);
        if (!nextItem) return false;
        const next = [...queue];
        next[index] = nextItem;
        this.setQueue(sessionId, next);
        return true;
    }

    private setQueue(
        sessionId: string,
        queue: readonly FollowUpQueueItem[],
    ): void {
        if (queue.length === 0) {
            this.queues.delete(sessionId);
        } else {
            this.queues.set(sessionId, Object.freeze(queue));
        }
        this.emitChange();
    }

    private emitChange(): void {
        for (const listener of this.listeners) listener();
    }
}
