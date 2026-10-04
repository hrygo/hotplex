import type { Page } from "@playwright/test";

/**
 * AEP v1 envelope shape, shared by client→server sends and server→client
 * emissions of the mock gateway.
 */
export type Envelope = {
    version: string;
    id: string;
    seq: number;
    session_id: string;
    timestamp: number;
    event: { type: string; data: Record<string, unknown> };
};

/** Subset of an Envelope recorded for client→server sends. */
export type SentEnvelope = {
    id: string;
    event: { type: string; data: Record<string, unknown> };
};

/**
 * Test seam installed on `window` by installMockGateway's init script.
 *
 * The mock is fully parameterized by the workerType passed to
 * installMockGateway — the init script and every API route closure receive
 * it as an argument. There is no process env and no module-level mutable
 * state in this fixture.
 */
export type MockGatewayWindow = Window & {
    __aepEvents: Envelope[];
    __mockAEP: {
        emit(type: string, data: Record<string, unknown>, id?: string): void;
        disconnect(): void;
        pauseNextConnect(): void;
        setNextInitState(state: "idle" | "running"): void;
        setNextInputOutcome(
            outcome: InputOutcome,
        ): void;
    };
};

/**
 * Terminal-or-not outcome the mock gateway reports for the next client input.
 *
 * - `delivered` / `unknown` / `failed`: the ordinary terminal receipts.
 * - `volatile`: one `accepted` receipt with `durability=volatile`, mirroring a
 *   supplement the gateway staged in process memory. No `delivered` ever
 *   follows, so a client that waits for one would hang forever.
 * - `queued`: one `accepted` receipt with `durability=durable`. This is an
 *   INTERMEDIATE state — `delivered` is still owed — so the client must keep
 *   the queue head in flight rather than treating acceptance as settlement.
 */
export type InputOutcome =
    | "delivered"
    | "unknown"
    | "failed"
    | "volatile"
    | "queued";

/**
 * Installs a parameterized in-page mock gateway plus a fake `/api/**` backend.
 *
 * workerType is plumbed into the browser init script (as the addInitScript
 * argument) and into the route closure (lexical capture). All fakes return
 * complete, mutually consistent data for that worker:
 *
 * - workspace `worker_preference` equals workerType;
 * - session `worker_type` equals workerType;
 * - `/api/workers` lists all four registered workers as installed;
 * - the `/ws` handshake echoes `server_caps.worker_type = workerType`, and the
 *   client→server init envelope (recorded in `__aepEvents`) carries
 *   `event.data.worker_type = workerType` for tests to assert.
 */
export async function installMockGateway(page: Page, workerType: string) {
    await page.addInitScript((workerType: string) => {
        const testWindow = window as unknown as MockGatewayWindow;
        const NativeWebSocket = window.WebSocket;
        testWindow.__aepEvents = [];
        let serverSequence = 0;
        let activeSocket: MockWebSocket | null = null;
        let nextInitState: "idle" | "running" = "idle";
        let nextInputOutcome: InputOutcome = "delivered";
        let pauseNextSocketOpen = false;

        class MockWebSocket extends EventTarget {
            static readonly CONNECTING = 0;
            static readonly OPEN = 1;
            static readonly CLOSING = 2;
            static readonly CLOSED = 3;

            readonly url: string;
            readyState = MockWebSocket.CONNECTING;
            onopen: ((event: Event) => void) | null = null;
            onmessage: ((event: MessageEvent) => void) | null = null;
            onerror: ((event: Event) => void) | null = null;
            onclose: ((event: CloseEvent) => void) | null = null;

            constructor(url: string | URL) {
                super();
                this.url = String(url);
                activeSocket = this;
                queueMicrotask(() => {
                    if (pauseNextSocketOpen) {
                        pauseNextSocketOpen = false;
                        return;
                    }
                    this.readyState = MockWebSocket.OPEN;
                    const event = new Event("open");
                    this.dispatchEvent(event);
                    this.onopen?.(event);
                });
            }

            send(payload: string) {
                const envelope = JSON.parse(payload) as Envelope;
                testWindow.__aepEvents.push(envelope);
                if (envelope.event.type === "init") {
                    // Mirrors internal/gateway/init.go InitAckData +
                    // DefaultServerCaps: the ack echoes the negotiated
                    // worker_type so tests can assert the handshake used the
                    // requested worker.
                    this.emit("init_ack", {
                        session_id: "session-e2e",
                        state: nextInitState,
                        server_caps: {
                            protocol_version: "aep/v1",
                            worker_type: workerType,
                            supports_resume: true,
                            supports_delta: true,
                            supports_tool_call: true,
                            supports_ping: true,
                            max_frame_size: 32 * 1024,
                            max_turns: 0,
                            modalities: ["text", "code"],
                        },
                    });
                    return;
                }
                if (envelope.event.type === "input") {
                    const outcome = nextInputOutcome;
                    nextInputOutcome = "delivered";
                    queueMicrotask(() => {
                        const base = {
                            client_message_id: envelope.id,
                            execution_id: `execution-${envelope.id}`,
                        };
                        if (outcome === "volatile") {
                            // Mirrors internal/gateway ackSupplement for a
                            // buffered supplement: staged in process memory,
                            // explicitly NOT a delivery proof.
                            this.emit("input.ack", {
                                ...base,
                                status: "accepted",
                                input_mode: "buffered",
                                durability: "volatile",
                                parent_execution_id: "execution-parent-turn",
                            });
                            return;
                        }
                        if (outcome === "queued") {
                            this.emit("input.ack", {
                                ...base,
                                status: "accepted",
                                input_mode: "queued",
                                durability: "durable",
                            });
                            return;
                        }
                        this.emit("input.ack", { ...base, status: "accepted" });
                        this.emit("input.ack", {
                            ...base,
                            status: outcome,
                            ...(outcome === "delivered"
                                ? {}
                                : {
                                      error_code: `TEST_${outcome.toUpperCase()}`,
                                  }),
                        });
                    });
                    return;
                }
                if (envelope.event.type === "ping") {
                    this.emit("pong", {});
                }
            }

            close(code = 1000, reason = "mock closed") {
                if (this.readyState === MockWebSocket.CLOSED) return;
                this.readyState = MockWebSocket.CLOSED;
                if (activeSocket === this) activeSocket = null;
                const event = new CloseEvent("close", { code, reason });
                this.dispatchEvent(event);
                this.onclose?.(event);
            }

            emit(type: string, data: Record<string, unknown>, id?: string) {
                serverSequence += 1;
                const envelope: Envelope = {
                    version: "aep/v1",
                    id: id ?? `server-${serverSequence}`,
                    seq: serverSequence,
                    session_id: "session-e2e",
                    timestamp: Date.now(),
                    event: { type, data },
                };
                const event = new MessageEvent("message", {
                    data: JSON.stringify(envelope),
                });
                this.dispatchEvent(event);
                this.onmessage?.(event);
            }
        }

        testWindow.__mockAEP = {
            emit(type, data, id) {
                activeSocket?.emit(type, data, id);
            },
            disconnect() {
                activeSocket?.close(1012, "mock reconnect");
            },
            pauseNextConnect() {
                pauseNextSocketOpen = true;
            },
            setNextInitState(state) {
                nextInitState = state;
            },
            setNextInputOutcome(outcome) {
                nextInputOutcome = outcome;
            },
        };
        const RoutedWebSocket = new Proxy(NativeWebSocket, {
            construct(Target, args) {
                const [rawUrl] = args as [string | URL];
                const url = new URL(String(rawUrl), window.location.href);
                if (url.pathname !== "/ws") {
                    return Reflect.construct(Target, args);
                }
                return new MockWebSocket(rawUrl);
            },
        });
        Object.defineProperty(window, "WebSocket", {
            configurable: true,
            writable: true,
            value: RoutedWebSocket,
        });
    }, workerType);

    await page.route("**/api/**", async (route) => {
        const request = route.request();
        const url = new URL(request.url());
        const json = (body: unknown, status = 200) =>
            route.fulfill({
                status,
                contentType: "application/json",
                body: JSON.stringify(body),
            });

        if (url.pathname === "/api/auth/me") {
            await json({
                id: "user-e2e",
                username: "e2e",
                display_name: "E2E User",
                role: "admin",
                status: "active",
                created_at: 1,
                updated_at: 1,
            });
            return;
        }
        if (url.pathname === "/api/workspaces" && request.method() === "GET") {
            await json({
                workspaces: [
                    {
                        id: "workspace-e2e",
                        name: "E2E Workspace",
                        work_dir: "/tmp/e2e",
                        owner_user_id: "user-e2e",
                        worker_preference: workerType,
                        permission_mode: "workspace",
                        agent_config_overrides: "{}",
                        status: "active",
                        created_at: 1,
                        updated_at: 1,
                    },
                ],
                limit: 100,
                offset: 0,
            });
            return;
        }
        if (url.pathname === "/api/sessions" && request.method() === "GET") {
            await json({
                sessions: [
                    {
                        id: "session-e2e",
                        user_id: "user-e2e",
                        worker_type: workerType,
                        state: "idle",
                        title: "Queue E2E",
                        work_dir: "/tmp/e2e",
                        created_at: new Date(1).toISOString(),
                        updated_at: new Date(2).toISOString(),
                    },
                ],
                limit: 20,
                offset: 0,
            });
            return;
        }
        if (url.pathname === "/api/sessions/session-e2e/history") {
            await json({ records: [], has_more: false });
            return;
        }
        if (url.pathname === "/api/workers") {
            // All four registered workers (internal/worker/types.go), not
            // just the current one: the page must see the full matrix.
            await json([
                { type: "claude_code", installed: true },
                { type: "opencode_server", installed: true },
                { type: "codex_cli", installed: true },
                { type: "acp", installed: true },
            ]);
            return;
        }
        if (request.method() === "DELETE") {
            await route.fulfill({ status: 204 });
            return;
        }
        await json({});
    });
}

/** All client→server envelopes captured so far, including init/ping/control. */
export async function sentEvents(page: Page): Promise<SentEnvelope[]> {
    return page.evaluate(
        () => (window as unknown as MockGatewayWindow).__aepEvents ?? [],
    );
}

/** Client→server `input` envelopes captured so far. */
export async function sentInputs(page: Page): Promise<SentEnvelope[]> {
    return page.evaluate(
        () =>
            (window as unknown as MockGatewayWindow).__aepEvents
                ?.filter((event) => event.event.type === "input") ?? [],
    );
}

/** Emits a gateway→client envelope (e.g. a terminal `done`) from the test. */
export async function emitGatewayEvent(
    page: Page,
    type: string,
    data: Record<string, unknown>,
    id?: string,
) {
    await page.evaluate(
        ({ eventType, eventData, eventId }) => {
            (window as unknown as MockGatewayWindow).__mockAEP.emit(
                eventType,
                eventData,
                eventId,
            );
        },
        { eventType: type, eventData: data, eventId: id },
    );
}

/** Emits a terminal `done` envelope carrying `success: true`. */
export async function emitDone(
    page: Page,
    id: string,
    reason = "completed",
) {
    await page.evaluate(
        ({ eventId, doneReason }) => {
            (window as unknown as MockGatewayWindow).__mockAEP.emit(
                "done",
                { success: true, reason: doneReason },
                eventId,
            );
        },
        { eventId: id, doneReason: reason },
    );
}
