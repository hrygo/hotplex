import { describe, expect, it, vi } from "vitest";

import {
    FOLLOW_UP_QUEUE_LIMIT,
    FollowUpQueueStore,
} from "./follow-up-queue";

function createStore(limit = FOLLOW_UP_QUEUE_LIMIT) {
    let sequence = 0;
    return new FollowUpQueueStore({
        limit,
        createId: () => `queue-${++sequence}`,
        now: () => 1_700_000_000_000 + sequence,
    });
}

describe("FollowUpQueueStore", () => {
    it("keeps FIFO order and isolates queues by session", () => {
        const store = createStore();

        expect(store.enqueue("session-a", "first")).toMatchObject({ ok: true });
        expect(store.enqueue("session-b", "other")).toMatchObject({ ok: true });
        expect(store.enqueue("session-a", "second")).toMatchObject({ ok: true });

        expect(store.getSnapshot("session-a").map((item) => item.text)).toEqual([
            "first",
            "second",
        ]);
        expect(store.getSnapshot("session-b").map((item) => item.text)).toEqual([
            "other",
        ]);
    });

    it("preserves the submitted text and rejects blank items", () => {
        const store = createStore();

        expect(store.enqueue("session-a", "  keep spacing  ")).toMatchObject({
            ok: true,
        });
        expect(store.getSnapshot("session-a")[0]?.text).toBe("  keep spacing  ");
        expect(store.enqueue("session-a", " \n\t ")).toEqual({
            ok: false,
            reason: "blank",
        });
    });

    it("enforces the configured limit without mutating the queue", () => {
        const store = createStore(2);
        store.enqueue("session-a", "first");
        store.enqueue("session-a", "second");

        expect(store.enqueue("session-a", "third")).toEqual({
            ok: false,
            reason: "limit",
        });
        expect(store.getSnapshot("session-a").map((item) => item.text)).toEqual([
            "first",
            "second",
        ]);
    });

    it("edits queued items but never mutates a sending item", () => {
        const store = createStore();
        const result = store.enqueue("session-a", "draft");
        if (!result.ok) throw new Error("enqueue failed");

        expect(store.updateText("session-a", result.item.id, "final text")).toBe(
            true,
        );
        expect(store.getSnapshot("session-a")[0]?.text).toBe("final text");
        expect(store.markSending("session-a", result.item.id)).toBe(true);
        expect(store.updateText("session-a", result.item.id, "too late")).toBe(
            false,
        );
        expect(store.remove("session-a", result.item.id)).toBe(false);
    });

    it("moves an arbitrary item to the front without reordering the others", () => {
        const store = createStore();
        store.enqueue("session-a", "first");
        store.enqueue("session-a", "second");
        const third = store.enqueue("session-a", "third");
        if (!third.ok) throw new Error("enqueue failed");

        expect(store.prepareSendNow("session-a", third.item.id)).toBe(true);
        expect(store.getSnapshot("session-a").map((item) => item.text)).toEqual([
            "third",
            "first",
            "second",
        ]);
    });

    it("removes an item only after its delivered acknowledgement", () => {
        const store = createStore();
        const result = store.enqueue("session-a", "first");
        if (!result.ok) throw new Error("enqueue failed");

        store.markSending("session-a", result.item.id);
        store.attachClientMessageId("session-a", result.item.id, "client-1");

        expect(store.markDelivered("session-a", "different-client")).toBeNull();
        expect(store.getSnapshot("session-a")).toHaveLength(1);
        expect(store.markDelivered("session-a", "client-1")?.id).toBe(
            result.item.id,
        );
        expect(store.getSnapshot("session-a")).toHaveLength(0);
    });

    it("requires an explicit retry after unknown or failed delivery", () => {
        const store = createStore();
        const result = store.enqueue("session-a", "first");
        if (!result.ok) throw new Error("enqueue failed");

        store.markSending("session-a", result.item.id);
        store.markFailed("session-a", result.item.id, "unknown", "Outcome unknown");

        expect(store.getSnapshot("session-a")[0]).toMatchObject({
            status: "failed",
            errorKind: "unknown",
            errorMessage: "Outcome unknown",
        });
        expect(store.peekDispatchable("session-a")).toBeNull();
        expect(store.retry("session-a", result.item.id)).toBe(true);
        expect(store.peekDispatchable("session-a")).toMatchObject({
            id: result.item.id,
            status: "queued",
        });
    });

    it("releases a sending slot on a volatile acceptance without claiming delivery", () => {
        const store = createStore();
        const first = store.enqueue("session-a", "first");
        store.enqueue("session-a", "second");
        if (!first.ok) throw new Error("enqueue failed");

        store.markSending("session-a", first.item.id);
        store.attachClientMessageId("session-a", first.item.id, "client-1");

        expect(store.releaseVolatileAccepted("session-a", "different-client")).toBeNull();
        expect(store.getSnapshot("session-a")).toHaveLength(2);

        // The gateway owns the input now, so the slot must free up for the next
        // FIFO item — but nothing may claim the input was delivered.
        expect(store.releaseVolatileAccepted("session-a", "client-1")?.id).toBe(
            first.item.id,
        );
        expect(store.getSnapshot("session-a")).toHaveLength(1);
        expect(store.peekDispatchable("session-a")).toMatchObject({
            text: "second",
        });
    });

    it("ignores a volatile acceptance for an item that is not sending", () => {
        const store = createStore();
        const result = store.enqueue("session-a", "first");
        if (!result.ok) throw new Error("enqueue failed");

        expect(
            store.releaseVolatileAccepted("session-a", "client-1"),
        ).toBeNull();
        expect(store.getSnapshot("session-a")).toHaveLength(1);
    });

    it("converges an unknown item when its original delivery is confirmed late", () => {
        const store = createStore();
        const result = store.enqueue("session-a", "first");
        if (!result.ok) throw new Error("enqueue failed");

        store.markSending("session-a", result.item.id);
        store.attachClientMessageId("session-a", result.item.id, "client-1");
        store.markFailed("session-a", result.item.id, "unknown", "Outcome unknown");

        expect(store.getSnapshot("session-a")[0]?.clientMessageId).toBe("client-1");
        expect(store.markDelivered("session-a", "client-1")?.id).toBe(
            result.item.id,
        );
        expect(store.getSnapshot("session-a")).toHaveLength(0);
    });

    it("turns a failed item into an explicit send-now retry", () => {
        const store = createStore();
        const first = store.enqueue("session-a", "first");
        store.enqueue("session-a", "second");
        if (!first.ok) throw new Error("enqueue failed");
        store.markFailed("session-a", first.item.id, "connection", "offline");

        expect(store.prepareSendNow("session-a", first.item.id)).toBe(true);
        expect(store.getSnapshot("session-a")[0]).toMatchObject({
            id: first.item.id,
            status: "queued",
            errorKind: undefined,
            errorMessage: undefined,
        });
    });

    it("notifies subscribers only for real changes and clears a deleted session", () => {
        const store = createStore();
        const listener = vi.fn();
        const unsubscribe = store.subscribe(listener);

        store.enqueue("session-a", "first");
        expect(listener).toHaveBeenCalledTimes(1);
        expect(store.remove("session-a", "missing")).toBe(false);
        expect(listener).toHaveBeenCalledTimes(1);

        store.clearSession("session-a");
        expect(listener).toHaveBeenCalledTimes(2);
        expect(store.getSnapshot("session-a")).toEqual([]);

        unsubscribe();
        store.enqueue("session-a", "after unsubscribe");
        expect(listener).toHaveBeenCalledTimes(2);
    });

    it("returns a stable snapshot until that session changes", () => {
        const store = createStore();
        const empty = store.getSnapshot("session-a");
        expect(store.getSnapshot("session-a")).toBe(empty);

        store.enqueue("session-b", "other");
        expect(store.getSnapshot("session-a")).toBe(empty);

        store.enqueue("session-a", "first");
        expect(store.getSnapshot("session-a")).not.toBe(empty);
    });

    it("pops all queued/dispatchable items and leaves other items intact", () => {
        const store = createStore();
        const first = store.enqueue("session-a", "first");
        const second = store.enqueue("session-a", "second");
        const third = store.enqueue("session-a", "third");
        if (!first.ok || !second.ok || !third.ok) throw new Error("enqueue failed");

        // Mark second as failed
        store.markFailed("session-a", second.item.id, "connection", "offline");

        // Pop all dispatchable items
        const popped = store.popAllDispatchable("session-a");
        expect(popped.map((item) => item.text)).toEqual(["first", "third"]);

        // Verify remaining queue contains only the failed item
        const remaining = store.getSnapshot("session-a");
        expect(remaining).toHaveLength(1);
        expect(remaining[0]?.id).toBe(second.item.id);
        expect(remaining[0]?.status).toBe("failed");
    });
});
