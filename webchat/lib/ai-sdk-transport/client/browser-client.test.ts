import { afterEach, describe, expect, it, vi } from "vitest";

import { BrowserHotPlexClient } from "./browser-client";
import { AEP_VERSION, ErrorCode, EventKind, WorkerType } from "./constants";
import type { Envelope } from "./types";
import { logger } from "@/lib/logger";

afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
});

function route(client: BrowserHotPlexClient, env: Envelope): void {
    (client as unknown as { _routeEvent(value: Envelope): void })._routeEvent(env);
}

function envelope(type: string, data: unknown): Envelope {
    return {
        version: AEP_VERSION,
        id: "ack-envelope",
        seq: 1,
        session_id: "session-1",
        timestamp: Date.now(),
        event: { type, data },
    };
}

class ControlledWebSocket {
    static readonly CONNECTING = 0;
    static readonly OPEN = 1;
    static readonly CLOSING = 2;
    static readonly CLOSED = 3;
    static instances: ControlledWebSocket[] = [];

    readonly url: string;
    readyState = ControlledWebSocket.CONNECTING;
    sent: string[] = [];
    onclose: ((event: CloseEvent) => void) | null = null;
    private listeners = new Map<string, Set<(event: any) => void>>();

    constructor(url: string) {
        this.url = url;
        ControlledWebSocket.instances.push(this);
    }

    static reset(): void {
        ControlledWebSocket.instances = [];
    }

    addEventListener(type: string, listener: (event: any) => void): void {
        const listeners = this.listeners.get(type) ?? new Set();
        listeners.add(listener);
        this.listeners.set(type, listeners);
    }

    send(data: string): void {
        this.sent.push(data);
    }

    close(): void {
        if (this.readyState === ControlledWebSocket.CLOSED) return;
        this.readyState = ControlledWebSocket.CLOSING;
    }

    open(): void {
        this.readyState = ControlledWebSocket.OPEN;
        this.dispatch("open", {});
    }

    message(value: Envelope): void {
        this.dispatch("message", { data: JSON.stringify(value) });
    }

    finishClose(code = 1000, reason = "closed"): void {
        this.readyState = ControlledWebSocket.CLOSED;
        const event = { code, reason } as CloseEvent;
        this.dispatch("close", event);
        this.onclose?.(event);
    }

    private dispatch(type: string, event: any): void {
        for (const listener of this.listeners.get(type) ?? []) {
            listener(event);
        }
    }
}

function initAck(sessionId: string): Envelope {
    return {
        ...envelope(EventKind.InitAck, { state: "running" }),
        session_id: sessionId,
    };
}

/** A done envelope with an explicit event id, so replayed events are testable. */
function doneEnvelope(id: string, data: Record<string, unknown>): Envelope {
    return { ...envelope(EventKind.Done, data), id };
}

describe("BrowserHotPlexClient connection handoff", () => {
    afterEach(() => {
        ControlledWebSocket.reset();
    });

    it("coalesces concurrent connect calls for the same client", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });

        const first = client.connect("session-single-flight");
        const second = client.connect("session-single-flight");

        await Promise.resolve();
        expect(ControlledWebSocket.instances).toHaveLength(1);
        const socket = ControlledWebSocket.instances[0];
        socket.open();
        socket.message(initAck("session-single-flight"));

        await expect(first).resolves.toMatchObject({ state: "running" });
        await expect(second).resolves.toMatchObject({ state: "running" });
        client.disconnect();
        socket.finishClose();
    });

    it("records advertised gateway capabilities without requiring them from old servers", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });

        const connect = client.connect("session-capabilities");
        await Promise.resolve();
        const socket = ControlledWebSocket.instances[0];
        socket.open();
        socket.message({
            ...initAck("session-capabilities"),
            event: {
                type: EventKind.InitAck,
                data: {
                    state: "running",
                    server_version: "v1.test",
                    capabilities: ["control_stop_v1"],
                },
            },
        });

        await connect;
        const introspect = client as unknown as {
            capabilities: ReadonlySet<string>;
            serverVersion: string | null;
        };
        expect(introspect.capabilities.has("control_stop_v1")).toBe(true);
        expect(introspect.serverVersion).toBe("v1.test");
        client.disconnect();
        socket.finishClose();
    });

    it("does not recursively reconnect when init reports a missing session", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const warn = vi.spyOn(logger, "warn").mockImplementation(() => undefined);
        const error = vi.spyOn(logger, "error").mockImplementation(() => undefined);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
            reconnect: { enabled: true, maxAttempts: 2, baseDelayMs: 1, maxDelayMs: 1 },
        });

        const connect = client.connect("session-init-missing");
        void connect.catch(() => undefined);
        await Promise.resolve();
        const socket = ControlledWebSocket.instances[0];
        socket.open();
        socket.message({
            ...initAck("session-init-missing"),
            event: {
                type: EventKind.InitAck,
                data: {
                    state: "deleted",
                    error: "session not found",
                    code: ErrorCode.SessionNotFound,
                },
            },
        });

        await Promise.resolve();
        await Promise.resolve();
        expect(ControlledWebSocket.instances).toHaveLength(1);
        expect(client.connected).toBe(false);
        expect(client.connecting).toBe(false);
        expect(warn).toHaveBeenCalledWith(
            "BrowserClient",
            "Handshake session unavailable",
            { message: "session not found" },
        );
        expect(error).not.toHaveBeenCalled();

        client.disconnect();
        socket.finishClose();
    });

    it("does not accept a legacy deleted init acknowledgement as connected", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });

        const connect = client.connect("session-init-deleted");
        void connect.catch(() => undefined);
        await Promise.resolve();
        const socket = ControlledWebSocket.instances[0];
        socket.open();
        socket.message({
            ...initAck("session-init-deleted"),
            event: {
                type: EventKind.InitAck,
                data: { state: "deleted" },
            },
        });

        await Promise.resolve();
        expect(client.connected).toBe(false);
        expect(client.connecting).toBe(false);

        client.disconnect();
        socket.finishClose();
    });

    it("logs an authentication handshake rejection as a warning", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const warn = vi.spyOn(logger, "warn").mockImplementation(() => undefined);
        const error = vi.spyOn(logger, "error").mockImplementation(() => undefined);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });

        const connect = client.connect("session-auth-required");
        void connect.catch(() => undefined);
        await Promise.resolve();
        const socket = ControlledWebSocket.instances[0];
        socket.open();
        socket.message({
            ...initAck("session-auth-required"),
            event: {
                type: EventKind.InitAck,
                data: {
                    state: "idle",
                    error: "authentication required",
                    code: ErrorCode.AuthRequired,
                },
            },
        });

        await Promise.resolve();
        expect(warn).toHaveBeenCalledWith(
            "BrowserClient",
            "Handshake rejected",
            {
                code: ErrorCode.AuthRequired,
                message: "authentication required",
            },
        );
        expect(error).not.toHaveBeenCalled();

        client.disconnect();
        socket.finishClose();
    });

    it("clears reconnecting when the reconnect attempt budget is exhausted", () => {
        vi.useFakeTimers();
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
            reconnect: { enabled: true, maxAttempts: 1, baseDelayMs: 1, maxDelayMs: 1 },
        });
        const internal = client as unknown as {
            reconnectAttempt: number;
            _reconnecting: boolean;
            _scheduleReconnect(): void;
        };
        internal.reconnectAttempt = 1;
        internal._reconnecting = true;

        internal._scheduleReconnect();

        expect(client.reconnecting).toBe(false);
        client.disconnect();
    });

    it("preserves the gateway code and message when init fails", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });

        const connect = client.connect("session-init-error");
        await Promise.resolve();
        const socket = ControlledWebSocket.instances[0];
        socket.open();
        socket.message({
            ...initAck("session-init-error"),
            event: {
                type: EventKind.InitAck,
                data: {
                    state: "deleted",
                    error: "session no longer exists",
                    code: ErrorCode.SessionNotFound,
                },
            },
        });

        await expect(connect).rejects.toMatchObject({
            code: ErrorCode.SessionNotFound,
            message: "session no longer exists",
        });
    });

    it("does not let disconnect cleanup overwrite a gateway init error", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });

        const connect = client.connect("session-init-internal-error");
        await Promise.resolve();
        const socket = ControlledWebSocket.instances[0];
        socket.open();
        socket.message({
            ...initAck("session-init-internal-error"),
            event: {
                type: EventKind.InitAck,
                data: {
                    state: "running",
                    error: "gateway unavailable",
                    code: ErrorCode.InternalError,
                },
            },
        });

        // Cleanup is allowed to run after the handshake error, but it must not
        // replace the already-settled gateway error with "Client disconnected".
        client.disconnect();
        await expect(connect).rejects.toMatchObject({
            code: ErrorCode.InternalError,
            message: "gateway unavailable",
        });
    });

    it("routes retryable init errors through bounded reconnect", async () => {
        vi.useFakeTimers();
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
            reconnect: { enabled: true, maxAttempts: 2, baseDelayMs: 10, maxDelayMs: 10 },
        });

        const connect = client.connect("session-init-retryable");
        void connect.catch(() => undefined);
        await Promise.resolve();
        const socket = ControlledWebSocket.instances[0];
        socket.open();
        socket.message({
            ...initAck("session-init-retryable"),
            event: {
                type: EventKind.InitAck,
                data: {
                    state: "running",
                    error: "temporarily unavailable",
                    code: ErrorCode.InternalError,
                    retryable: true,
                    retry_after_ms: 10,
                },
            },
        });

        await Promise.resolve();
        expect(ControlledWebSocket.instances).toHaveLength(1);
        socket.finishClose();
        await vi.advanceTimersByTimeAsync(9);
        expect(ControlledWebSocket.instances).toHaveLength(1);
        await vi.advanceTimersByTimeAsync(1);
        expect(ControlledWebSocket.instances).toHaveLength(2);

        const retrySocket = ControlledWebSocket.instances[1];
        retrySocket.open();
        retrySocket.message(initAck("session-init-retryable"));
        await Promise.resolve();
        expect(client.connected).toBe(true);

        client.disconnect();
        retrySocket.finishClose();
    });

    it("does not let a rejected cross-session connect corrupt the active target", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });

        const activeConnect = client.connect("session-a");
        await expect(client.connect("session-b")).rejects.toThrow(
            "Connection already in progress for another session",
        );
        expect(client.sessionId).toBe("session-a");

        await Promise.resolve();
        const socket = ControlledWebSocket.instances[0];
        socket.open();
        socket.message(initAck("session-a"));
        await expect(activeConnect).resolves.toMatchObject({ state: "running" });
        expect(client.sessionId).toBe("session-a");
        client.disconnect();
        socket.finishClose();
    });

    it("waits for the previous instance to close before opening the same session", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const firstClient = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const firstConnect = firstClient.connect("session-remount");
        await Promise.resolve();
        const firstSocket = ControlledWebSocket.instances[0];
        firstSocket.open();
        firstSocket.message(initAck("session-remount"));
        await firstConnect;

        firstClient.disconnect();
        const nextClient = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const nextConnect = nextClient.connect("session-remount");
        void nextConnect.catch(() => undefined);

        await Promise.resolve();
        expect(ControlledWebSocket.instances).toHaveLength(1);

        firstSocket.finishClose();
        await vi.waitFor(() => {
            expect(ControlledWebSocket.instances).toHaveLength(2);
        });

        const nextSocket = ControlledWebSocket.instances[1];
        nextSocket.open();
        nextSocket.message(initAck("session-remount"));
        await expect(nextConnect).resolves.toMatchObject({ state: "running" });

        nextClient.sendPermissionResponse("permission-remount", true);
        nextClient.sendQuestionResponse("question-remount", { choice: "yes" });
        nextClient.sendElicitationResponse("elicitation-remount", "accept", { value: "ok" });
        expect(nextSocket.sent.slice(-3).map((line) => JSON.parse(line).event.type)).toEqual([
            EventKind.PermissionResponse,
            EventKind.QuestionResponse,
            EventKind.ElicitationResponse,
        ]);

        nextClient.disconnect();
        nextSocket.finishClose();
    });

    it("hands off an active same-page client before opening a replacement", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const firstClient = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const firstConnect = firstClient.connect("session-page-owner");
        await Promise.resolve();
        const firstSocket = ControlledWebSocket.instances[0];
        firstSocket.open();
        firstSocket.message(initAck("session-page-owner"));
        await firstConnect;

        const replacement = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const replacementConnect = replacement.connect("session-page-owner");
        void replacementConnect.catch(() => undefined);

        await Promise.resolve();
        expect(firstSocket.readyState).toBe(ControlledWebSocket.CLOSING);
        expect(ControlledWebSocket.instances).toHaveLength(1);

        firstSocket.finishClose();
        await vi.waitFor(() => {
            expect(ControlledWebSocket.instances).toHaveLength(2);
        });

        const replacementSocket = ControlledWebSocket.instances[1];
        replacementSocket.open();
        replacementSocket.message(initAck("session-page-owner"));
        await expect(replacementConnect).resolves.toMatchObject({
            state: "running",
        });

        replacement.disconnect();
        replacementSocket.finishClose();
    });

    it("reconnects instead of returning a stale ack for a closing socket", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const firstConnect = client.connect("session-closing");
        await Promise.resolve();
        const firstSocket = ControlledWebSocket.instances[0];
        firstSocket.open();
        firstSocket.message(initAck("session-closing"));
        await firstConnect;

        firstSocket.readyState = ControlledWebSocket.CLOSING;
        const reconnect = client.connect("session-closing");
        await Promise.resolve();
        expect(ControlledWebSocket.instances).toHaveLength(1);

        firstSocket.finishClose();
        await vi.waitFor(() => {
            expect(ControlledWebSocket.instances).toHaveLength(2);
        });
        const nextSocket = ControlledWebSocket.instances[1];
        nextSocket.open();
        nextSocket.message(initAck("session-closing"));
        await expect(reconnect).resolves.toMatchObject({ state: "running" });
        client.disconnect();
        nextSocket.finishClose();
    });

    it("backs off before retrying one locally handed-off duplicate", async () => {
        vi.useFakeTimers();
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const firstClient = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const firstConnect = firstClient.connect("session-local-retry");
        await Promise.resolve();
        const firstSocket = ControlledWebSocket.instances[0];
        firstSocket.open();
        firstSocket.message(initAck("session-local-retry"));
        await firstConnect;
        firstClient.disconnect();

        const nextClient = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const conflict = vi.fn();
        nextClient.on("sessionAlreadyConnected", conflict);
        const nextConnect = nextClient.connect("session-local-retry");

        firstSocket.finishClose();
        await Promise.resolve();
        await Promise.resolve();
        expect(ControlledWebSocket.instances).toHaveLength(2);
        const rejectedSocket = ControlledWebSocket.instances[1];
        rejectedSocket.open();
        rejectedSocket.message({
            ...initAck("session-local-retry"),
            event: {
                type: EventKind.InitAck,
                data: {
                    code: ErrorCode.SessionAlreadyConnected,
                    error: "session already has an active WebSocket connection",
                },
            },
        });

        expect(conflict).not.toHaveBeenCalled();
        rejectedSocket.finishClose();
        await Promise.resolve();
        await vi.advanceTimersByTimeAsync(99);
        expect(ControlledWebSocket.instances).toHaveLength(2);
        await vi.advanceTimersByTimeAsync(1);
        expect(ControlledWebSocket.instances).toHaveLength(3);

        const retrySocket = ControlledWebSocket.instances[2];
        retrySocket.open();
        retrySocket.message(initAck("session-local-retry"));
        await expect(nextConnect).resolves.toMatchObject({ state: "running" });
        expect(conflict).not.toHaveBeenCalled();
        nextClient.disconnect();
        retrySocket.finishClose();
    });

    it("invalidates a connect that was waiting when the client disconnects", async () => {
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const owner = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const ownerConnect = owner.connect("session-cancel-waiter");
        await Promise.resolve();
        const ownerSocket = ControlledWebSocket.instances[0];
        ownerSocket.open();
        ownerSocket.message(initAck("session-cancel-waiter"));
        await ownerConnect;
        owner.disconnect();

        const replacement = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const cancelledConnect = replacement.connect("session-cancel-waiter");
        const cancelled = expect(cancelledConnect).rejects.toThrow("Client is closed");
        replacement.disconnect();
        const activeConnect = replacement.connect("session-cancel-waiter");

        ownerSocket.finishClose();
        await cancelled;
        await vi.waitFor(() => {
            expect(ControlledWebSocket.instances).toHaveLength(2);
        });

        const activeSocket = ControlledWebSocket.instances[1];
        activeSocket.open();
        activeSocket.message(initAck("session-cancel-waiter"));
        await expect(activeConnect).resolves.toMatchObject({ state: "running" });
        replacement.disconnect();
        activeSocket.finishClose();
    });

    it("settles a failed reconnect flight before starting the next attempt", async () => {
        vi.useFakeTimers();
        vi.stubGlobal("WebSocket", ControlledWebSocket);
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
            reconnect: {
                enabled: true,
                maxAttempts: 3,
                baseDelayMs: 10,
                maxDelayMs: 10,
            },
        });

        const initialConnect = client.connect("session-reconnect-flight");
        await Promise.resolve();
        const initialSocket = ControlledWebSocket.instances[0];
        initialSocket.open();
        initialSocket.message(initAck("session-reconnect-flight"));
        await initialConnect;

        initialSocket.finishClose(1006, "network lost");
        await vi.advanceTimersByTimeAsync(10);
        expect(ControlledWebSocket.instances).toHaveLength(2);

        const failedReconnectSocket = ControlledWebSocket.instances[1];
        failedReconnectSocket.open();
        failedReconnectSocket.finishClose(1006, "handshake lost");
        await Promise.resolve();
        await vi.advanceTimersByTimeAsync(10);
        expect(ControlledWebSocket.instances).toHaveLength(3);

        const recoveredSocket = ControlledWebSocket.instances[2];
        recoveredSocket.open();
        recoveredSocket.message(initAck("session-reconnect-flight"));
        await Promise.resolve();
        expect(client.connected).toBe(true);
        client.disconnect();
        recoveredSocket.finishClose();
    });
});

describe("BrowserHotPlexClient interaction acknowledgements", () => {
    it.each([
        [EventKind.PermissionResponse, "permissionResponse", { id: "permission-1", allowed: true }],
        [EventKind.QuestionResponse, "questionResponse", { id: "question-1", answers: { q: "a" } }],
        [EventKind.ElicitationResponse, "elicitationResponse", { id: "elicitation-1", action: "accept" }],
    ] as const)("routes %s as %s", (kind, eventName, data) => {
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const listener = vi.fn();
        client.on(eventName, listener);

        const env = envelope(kind, data);
        route(client, env);

        expect(listener).toHaveBeenCalledOnce();
        expect(listener).toHaveBeenCalledWith(data, env);
    });

    it("routes durable input acknowledgements", () => {
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const listener = vi.fn();
        client.on("inputAck", listener);
        const data = {
            client_message_id: "evt-client-1",
            execution_id: "exec-1",
            status: "delivered" as const,
        };
        const env = envelope(EventKind.InputAck, data);

        route(client, env);

        expect(listener).toHaveBeenCalledOnce();
        expect(listener).toHaveBeenCalledWith(data, env);
    });
});

describe("BrowserHotPlexClient duplicate session rejection", () => {
    it("treats SESSION_ALREADY_CONNECTED as fatal and disables reconnect", () => {
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            shouldReconnect: boolean;
            sessionAlreadyConnectedRetryCount: number;
            _handleMessage(
                value: Envelope,
                resolve: (value: unknown) => void,
                reject: (err: Error) => void,
            ): void;
        };
        const duplicate = vi.fn();
        const genericError = vi.fn();
        const rejected = vi.fn();
        const consoleError = vi.spyOn(console, "error").mockImplementation(() => undefined);
        client.on("sessionAlreadyConnected", duplicate);
        client.on("error", genericError);

        internal.sessionAlreadyConnectedRetryCount = 2;
        internal._handleMessage(
            envelope(EventKind.InitAck, {
                code: ErrorCode.SessionAlreadyConnected,
                error: "session already has an active WebSocket connection",
            }),
            vi.fn(),
            rejected,
        );

        expect(duplicate).toHaveBeenCalledOnce();
        expect(genericError).not.toHaveBeenCalled();
        expect(consoleError).not.toHaveBeenCalled();
        expect(internal.shouldReconnect).toBe(false);
        expect(rejected).toHaveBeenCalledOnce();
    });
});

describe("BrowserHotPlexClient input retry identity", () => {
    it("reuses the pending envelope ID after an ambiguous reconnect", () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            _reconnecting: boolean;
            ws: { readyState: number };
            _send(value: Envelope): void;
            _handleMessage(
                value: Envelope,
                resolve: (value: unknown) => void,
                reject: (err: Error) => void,
            ): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const submittedID = client.sendInput("hello");
        const firstId = send.mock.calls[0][0].id;
        expect(submittedID).toBe(firstId);
        internal._reconnecting = true;

        internal._handleMessage(
            envelope(EventKind.InitAck, { state: "running" }),
            vi.fn(),
            vi.fn(),
        );

        expect(send).toHaveBeenCalledTimes(2);
        expect(send.mock.calls[1][0].id).toBe(firstId);
    });

    it("settles a SESSION_BUSY rejection without retrying the input", async () => {
        vi.useFakeTimers();
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            ws: { readyState: number };
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("hello");
        const rejected = expect(pending).rejects.toThrow("SESSION_BUSY");
        route(client, envelope(EventKind.Error, { code: "SESSION_BUSY", message: "busy" }));
        await vi.advanceTimersByTimeAsync(1_000);

        expect(send).toHaveBeenCalledOnce();
        await rejected;
        expect(() => client.sendInput("second")).not.toThrow();
    });

    it("does not resend a SESSION_BUSY-rejected input after reconnect", async () => {
        vi.useFakeTimers();
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            _reconnecting: boolean;
            ws: { readyState: number };
            _send(value: Envelope): void;
            _handleMessage(
                value: Envelope,
                resolve: (value: unknown) => void,
                reject: (err: Error) => void,
            ): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("hello");
        const rejected = expect(pending).rejects.toThrow("SESSION_BUSY");
        route(client, envelope(EventKind.Error, { code: "SESSION_BUSY", message: "busy" }));
        internal._reconnecting = true;
        internal._handleMessage(
            envelope(EventKind.InitAck, { state: "running" }),
            vi.fn(),
            vi.fn(),
        );
        await vi.advanceTimersByTimeAsync(1_000);

        expect(send).toHaveBeenCalledOnce();
        await rejected;
    });

    it("does not resend after a terminal acknowledgement", () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            _reconnecting: boolean;
            ws: { readyState: number };
            _send(value: Envelope): void;
            _handleMessage(
                value: Envelope,
                resolve: (value: unknown) => void,
                reject: (err: Error) => void,
            ): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        client.sendInput("hello");
        const clientMessageId = send.mock.calls[0][0].id;
        route(client, envelope(EventKind.InputAck, {
            client_message_id: clientMessageId,
            execution_id: "exec-1",
            status: "delivered",
            duplicate: true,
        }));
        internal._reconnecting = true;
        internal._handleMessage(
            envelope(EventKind.InitAck, { state: "running" }),
            vi.fn(),
            vi.fn(),
        );

        expect(send).toHaveBeenCalledOnce();
    });

    it("does not let sendInput orphan an async pending request", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            ws: { readyState: number };
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("first");
        expect(() => client.sendInput("second")).toThrow("Input already pending");
        route(client, envelope(EventKind.Done, { success: true }));
        await expect(pending).resolves.toBeUndefined();
    });

    it("settles and clears a failed terminal acknowledgement", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            ws: { readyState: number };
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("first");
        const clientMessageId = send.mock.calls[0][0].id;
        route(client, envelope(EventKind.InputAck, {
            client_message_id: clientMessageId,
            execution_id: "exec-1",
            status: "failed",
            error_code: "WORKER_UNAVAILABLE",
        }));

        await expect(pending).rejects.toThrow("WORKER_UNAVAILABLE");
        expect(() => client.sendInput("second")).not.toThrow();
    });

    it("tombstones an unknown acknowledgement but auto-clears after the grace window", async () => {
        vi.useFakeTimers();
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
            heartbeat: { pingIntervalMs: 1_000_000 },
        });
        const internal = client as unknown as {
            _sessionId: string;
            ws: { readyState: number };
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("first");
        const clientMessageId = send.mock.calls[0][0].id;
        route(client, envelope(EventKind.InputAck, {
            client_message_id: clientMessageId,
            execution_id: "exec-1",
            status: "unknown",
            error_code: "EXECUTION_TIMEOUT",
        }));

        await expect(pending).rejects.toThrow("EXECUTION_TIMEOUT");
        // Immediately after: tombstone blocks new sends (avoid double side-effects).
        expect(() => client.sendInput("second")).toThrow("Input already pending");

        // After the grace window the tombstone is force-cleared so an ambiguous
        // outcome can never permanently lock the chat.
        await vi.advanceTimersByTimeAsync(130_000);
        expect(() => client.sendInput("second")).not.toThrow();
    });

    it("releases an unknown tombstone only for an explicit user retry", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            ws: { readyState: number };
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("first");
        const clientMessageId = send.mock.calls[0][0].id;
        route(client, envelope(EventKind.InputAck, {
            client_message_id: clientMessageId,
            execution_id: "exec-1",
            status: "unknown",
            error_code: "EXECUTION_TIMEOUT",
        }));

        await expect(pending).rejects.toThrow("EXECUTION_TIMEOUT");
        expect(() => client.sendInput("automatic retry")).toThrow("Input already pending");
        expect(client.acknowledgeUnknownInputForRetry()).toBe(true);
        expect(client.acknowledgeUnknownInputForRetry()).toBe(false);
        expect(() => client.sendInput("explicit retry")).not.toThrow();
    });

    it("clears pendingInput on a delivered acknowledgement so a subsequent send works", () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            ws: { readyState: number };
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        client.sendInput("first");
        const clientMessageId = send.mock.calls[0][0].id;
        route(client, envelope(EventKind.InputAck, {
            client_message_id: clientMessageId,
            execution_id: "exec-1",
            status: "delivered",
        }));

        // delivered resolves the pending input — a new send must not throw.
        expect(() => client.sendInput("second")).not.toThrow();
    });

    it("clears pendingInput on a volatile accepted receipt so a volatile staging decision cannot deadlock the composer", () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            ws: { readyState: number };
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        client.sendInput("first");
        const clientMessageId = send.mock.calls[0][0].id;
        route(client, envelope(EventKind.InputAck, {
            client_message_id: clientMessageId,
            execution_id: `supplement-${clientMessageId}`,
            status: "accepted",
            input_mode: "buffered",
            durability: "volatile",
        }));

        // A volatile receipt is never followed by a second ACK, so the send
        // must settle here instead of waiting for the timeout.
        expect(() => client.sendInput("second")).not.toThrow();
    });

    it("keeps pendingInput on a durable accepted receipt that will be followed by delivered", () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            ws: { readyState: number };
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        client.sendInput("first");
        const clientMessageId = send.mock.calls[0][0].id;
        route(client, envelope(EventKind.InputAck, {
            client_message_id: clientMessageId,
            execution_id: "exec-1",
            status: "accepted",
            input_mode: "primary",
            durability: "durable",
        }));

        // durable accepted is an intermediate state — delivered still owes an ACK.
        expect(() => client.sendInput("second")).toThrow("Input already pending");
    });

    it("clears pendingInput on a non-SESSION_BUSY error so the client is not locked out", () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            ws: { readyState: number };
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1 };
        vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        client.sendInput("first");
        // SESSION_NOT_FOUND arrives with no Done and no terminal InputAck — the
        // error itself must release the pending slot.
        route(client, envelope(EventKind.Error, { code: "SESSION_NOT_FOUND", message: "gone" }));

        expect(() => client.sendInput("second")).not.toThrow();
    });

    it("settles a pending async input on explicit disconnect", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1, CONNECTING: 0 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
        });
        const internal = client as unknown as {
            _sessionId: string;
            ws: { readyState: number; close: () => void };
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal.ws = { readyState: 1, close: vi.fn() };
        vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("hello");
        client.disconnect();

        await expect(pending).rejects.toThrow("Client disconnected");
    });

    it("settles a pending async input when reconnect attempts are exhausted", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1, CONNECTING: 0 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
            reconnect: { enabled: true, maxAttempts: 1 },
        });
        const internal = client as unknown as {
            _sessionId: string;
            _connected: boolean;
            reconnectAttempt: number;
            pendingInput: unknown;
            ws: { readyState: number; close: () => void } | null;
            _send(value: Envelope): void;
            _handleClose(code: number, reason: string): void;
        };
        internal._sessionId = "session-1";
        internal._connected = true;
        internal.reconnectAttempt = 1;
        internal.ws = { readyState: 1, close: vi.fn() };
        vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("hello");
        internal._handleClose(4001, "Reconnect failed");

        expect(internal.pendingInput).toBeNull();
        await expect(pending).rejects.toThrow("Reconnect failed");
    });

    it("rejects a pending async input when the socket closes without reconnect", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1, CONNECTING: 0 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType: WorkerType.CodexCLI,
            reconnect: { enabled: false },
        });
        const internal = client as unknown as {
            _sessionId: string;
            _connected: boolean;
            _reconnecting: boolean;
            shouldReconnect: boolean;
            pendingInput: unknown;
            ws: { readyState: number; close: () => void } | null;
            _send(value: Envelope): void;
            _handleClose(code: number, reason: string): void;
        };
        internal._sessionId = "session-1";
        internal._connected = true;
        internal._reconnecting = false;
        internal.shouldReconnect = false;
        internal.ws = { readyState: 1, close: vi.fn() };
        vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("hello");
        // shouldReconnect is false → the 'disconnected' branch must reject the
        // pending input instead of leaving the promise hanging forever.
        internal._handleClose(1000, "link lost");

        expect(internal.pendingInput).toBeNull();
        await expect(pending).rejects.toThrow("link lost");
    });
});

// ============================================================================
// Stop waiter state machine — parameterized across every worker type so the
// stop contract (shared Promise, single send, done dedup, timeout, disconnect,
// next-turn) holds for all four workers, not just CodexCLI.
// ============================================================================

describe.each([
    ["W-C", WorkerType.ClaudeCode],
    ["W-O", WorkerType.OpenCodeServer],
    ["W-X", WorkerType.CodexCLI],
    ["W-A", WorkerType.ACP],
] as const)("%s browser stop contract", (_id, workerType) => {
    let lastStopClient: BrowserHotPlexClient | null = null;

    afterEach(() => {
        // Each worker row must end with the stop waiter fully settled — no
        // pending promise and no armed stop timer/listener left behind.
        const internal = lastStopClient as unknown as { pendingStop: unknown } | null;
        if (internal) {
            expect(internal.pendingStop).toBeNull();
        }
        lastStopClient = null;
    });

    it("keeps pending input until stop reaches a terminal done event", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType,
        });
        lastStopClient = client;
        const internal = client as unknown as {
            _sessionId: string;
            _connected: boolean;
            pendingInput: unknown;
            ws: { readyState: number } | null;
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal._connected = true;
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("hello");
        expect(internal.pendingInput).not.toBeNull();

        const stopped = client.stopCurrentTurn();

        expect(internal.pendingInput).not.toBeNull();
        expect(() => client.sendInput("too early")).toThrow("Input already pending");

        route(client, doneEnvelope("done-stop-1", { success: true, reason: "stopped_by_user" }));

        await expect(stopped).resolves.toMatchObject({
            reason: "stopped_by_user",
        });
        expect(internal.pendingInput).toBeNull();
        await expect(pending).resolves.toBeUndefined();

        // A settled turn frees the input slot: the next input can be sent
        // immediately and is resolved by the next turn's own Done.
        const next = client.sendInputAsync("after-done");
        expect(send).toHaveBeenLastCalledWith(expect.objectContaining({
            event: expect.objectContaining({
                type: EventKind.Input,
                data: expect.objectContaining({ content: "after-done" }),
            }),
        }));
        route(client, doneEnvelope("done-after-1", { success: true }));
        await expect(next).resolves.toBeUndefined();
    });

    it("coalesces repeated stop requests and accepts natural completion", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType,
        });
        lastStopClient = client;
        const internal = client as unknown as {
            _sessionId: string;
            _connected: boolean;
            ws: { readyState: number } | null;
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal._connected = true;
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const first = client.stopCurrentTurn();
        const second = client.stopCurrentTurn();

        expect(second).toBe(first);
        expect(send).toHaveBeenCalledTimes(1);
        route(client, envelope(EventKind.Done, { success: true, reason: "completed" }));

        await expect(first).resolves.toMatchObject({ reason: "completed" });
        await expect(second).resolves.toMatchObject({ reason: "completed" });
    });

    it("keeps waiting for done when an unrelated gateway error arrives", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType,
        });
        lastStopClient = client;
        const internal = client as unknown as {
            _sessionId: string;
            _connected: boolean;
            ws: { readyState: number } | null;
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal._connected = true;
        internal.ws = { readyState: 1 };
        vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const stopped = client.stopCurrentTurn();
        let settled = false;
        void stopped.finally(() => {
            settled = true;
        });
        route(client, envelope(EventKind.Error, {
            code: "WORKER_COMMAND_FAILED",
            message: "unrelated worker command failed",
        }));
        await Promise.resolve();
        expect(settled).toBe(false);

        route(client, envelope(EventKind.Done, { success: true, reason: "completed" }));
        await expect(stopped).resolves.toMatchObject({ reason: "completed" });
    });

    it("rejects a stop waiter on timeout without releasing the active input", async () => {
        vi.useFakeTimers();
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType,
        });
        lastStopClient = client;
        const internal = client as unknown as {
            _sessionId: string;
            _connected: boolean;
            pendingInput: unknown;
            ws: { readyState: number } | null;
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal._connected = true;
        internal.ws = { readyState: 1 };
        vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const pending = client.sendInputAsync("hello");
        void pending.catch(() => undefined);
        const stopped = client.stopCurrentTurn(500);
        const rejected = expect(stopped).rejects.toThrow("Stop timeout");

        await vi.advanceTimersByTimeAsync(500);

        await rejected;
        expect(internal.pendingInput).not.toBeNull();
        expect(() => client.sendInput("too early")).toThrow("Input already pending");
        route(client, envelope(EventKind.Done, { success: true }));
        await expect(pending).resolves.toBeUndefined();
    });

    it("rejects a stop waiter when the client disconnects", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1, CONNECTING: 0 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType,
        });
        lastStopClient = client;
        const internal = client as unknown as {
            _sessionId: string;
            _connected: boolean;
            ws: { readyState: number; close: () => void } | null;
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal._connected = true;
        internal.ws = { readyState: 1, close: vi.fn() };
        vi.spyOn(internal, "_send").mockImplementation(() => undefined);

        const stopped = client.stopCurrentTurn();
        client.disconnect();

        await expect(stopped).rejects.toThrow("Client disconnected");
    });

    it("resolves a stopped Done once, ignores a replayed Done, and opens the next turn", async () => {
        vi.stubGlobal("WebSocket", { OPEN: 1 });
        const client = new BrowserHotPlexClient({
            url: "ws://127.0.0.1:8888/ws",
            workerType,
        });
        lastStopClient = client;
        const internal = client as unknown as {
            _sessionId: string;
            _connected: boolean;
            pendingInput: unknown;
            pendingStop: unknown;
            ws: { readyState: number } | null;
            _send(value: Envelope): void;
        };
        internal._sessionId = "session-1";
        internal._connected = true;
        internal.ws = { readyState: 1 };
        const send = vi.spyOn(internal, "_send").mockImplementation(() => undefined);
        const doneListener = vi.fn();
        client.on("done", doneListener);

        const pending = client.sendInputAsync("first");
        const stopped = client.stopCurrentTurn();
        const stoppedDone = doneEnvelope("done-stop-1", { success: true, reason: "stopped_by_user" });
        route(client, stoppedDone);

        await expect(stopped).resolves.toMatchObject({ reason: "stopped_by_user" });
        await expect(pending).resolves.toBeUndefined();
        expect(doneListener).toHaveBeenCalledTimes(1);
        expect(internal.pendingStop).toBeNull();
        expect(internal.pendingInput).toBeNull();

        // The same Done envelope routed again (reconnect replay, or a worker
        // emitting done twice) must not re-emit 'done' or double-settle.
        route(client, stoppedDone);
        expect(doneListener).toHaveBeenCalledTimes(1);
        expect(internal.pendingStop).toBeNull();
        expect(internal.pendingInput).toBeNull();

        // The settled turn freed the input slot: the next turn starts a fresh
        // pending on the same client.
        const next = client.sendInputAsync("next");
        expect(send).toHaveBeenLastCalledWith(expect.objectContaining({
            event: expect.objectContaining({
                type: EventKind.Input,
                data: expect.objectContaining({ content: "next" }),
            }),
        }));
        let nextSettled = false;
        void next.finally(() => {
            nextSettled = true;
        });

        // A stale replay must not complete the newer turn either.
        route(client, stoppedDone);
        expect(nextSettled).toBe(false);
        expect(doneListener).toHaveBeenCalledTimes(1);

        // Only the next turn's own Done resolves it.
        route(client, doneEnvelope("done-next-1", { success: true, reason: "completed" }));
        await expect(next).resolves.toBeUndefined();
        expect(doneListener).toHaveBeenCalledTimes(2);
        expect(internal.pendingStop).toBeNull();
        expect(internal.pendingInput).toBeNull();
    });
});
