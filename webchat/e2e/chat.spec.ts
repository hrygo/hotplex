import { test, expect, type Page } from "@playwright/test";
import {
    installMockGateway,
    sentEvents,
    sentInputs,
    emitGatewayEvent,
    emitDone,
    type InputOutcome,
    type MockGatewayWindow,
} from "./fixtures/mock-gateway";

const COMPOSER_PLACEHOLDER = "输入消息，或输入 '/' 使用命令...";

async function waitForChatReady(page: Page) {
    await page.getByPlaceholder(COMPOSER_PLACEHOLDER).waitFor({
        state: "visible",
        timeout: 20_000,
    });
}

function composerInput(page: Page) {
    return page.getByPlaceholder(COMPOSER_PLACEHOLDER);
}

function sendButton(page: Page) {
    return page.getByRole("button", { name: "发送消息" });
}

async function setNextInputOutcome(
    page: Page,
    outcome: InputOutcome,
) {
    await page.evaluate((nextOutcome) => {
        (window as unknown as MockGatewayWindow).__mockAEP.setNextInputOutcome(
            nextOutcome,
        );
    }, outcome);
}

async function disconnectGateway(
    page: Page,
    reconnectState: "idle" | "running",
    pauseReconnect = false,
) {
    await page.evaluate(
        ({ state, pause }) => {
            const mock = (window as unknown as MockGatewayWindow).__mockAEP;
            mock.setNextInitState(state);
            if (pause) mock.pauseNextConnect();
            mock.disconnect();
        },
        { state: reconnectState, pause: pauseReconnect },
    );
}

test.describe("Chat Page", () => {
    test.beforeEach(async ({ page }) => {
        await installMockGateway(page, "codex_cli");
        await page.goto("/");
        await waitForChatReady(page);
    });

    test("renders an authenticated chat with a usable composer", async ({
        page,
    }) => {
        await expect(
            page.getByRole("heading", { name: "HotPlex" }),
        ).toBeVisible();
        await expect(composerInput(page)).toBeEditable();
        await expect(sendButton(page)).toBeDisabled();

        await composerInput(page).fill("Hello");
        await expect(sendButton(page)).toBeEnabled();
        await composerInput(page).press("Shift+Enter");
        await composerInput(page).pressSequentially("world");
        await expect(composerInput(page)).toHaveValue(/Hello\nworld/);
    });

    test("queues follow-ups and drains them as one merged input per terminal turn", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(1);

        await input.fill("second");
        await input.press("Enter");
        await input.fill("third");
        await page.getByRole("button", { name: "将消息加入后续队列" }).click();

        const panel = page.getByRole("region", { name: "后续消息队列" });
        await expect(panel).toContainText("second");
        await expect(panel).toContainText("third");
        expect(await sentInputs(page)).toHaveLength(1);

        await emitDone(page, "done-first");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(2);
        const merged = (await sentInputs(page))[1]?.event.data.content;
        expect(merged).toContain("second");
        expect(merged).toContain("third");
        await expect(panel).toHaveCount(0);

        // A duplicate terminal envelope must not dispatch anything further.
        await emitDone(page, "done-first");
        await expect.poll(async () => (await sentInputs(page)).length, {
            timeout: 1_000,
        }).toBe(2);
    });

    test("drains queued follow-ups as one merged input after an idle reconnect", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(1);

        await input.fill("delivered before disconnect");
        await input.press("Enter");
        await input.fill("must drain after idle reconnect");
        await input.press("Enter");

        await disconnectGateway(page, "idle");

        await expect
            .poll(async () => (await sentInputs(page)).length, {
                timeout: 10_000,
            })
            .toBe(2);
        const drained = (await sentInputs(page))[1]?.event.data.content ?? "";
        expect(drained).toContain("delivered before disconnect");
        expect(drained).toContain("must drain after idle reconnect");
    });

    test("drops an unknown delivery outcome and keeps the queue draining", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await input.fill("ambiguous follow-up");
        await input.press("Enter");

        await setNextInputOutcome(page, "unknown");
        await emitDone(page, "done-before-unknown");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(2);

        // The current runtime does not preserve unknown-dispatched items for
        // manual retry: the queue empties and the outcome surfaces in the
        // transcript instead of a failed queue entry.
        await expect(
            page.getByRole("region", { name: "后续消息队列" }),
        ).toHaveCount(0);
        await expect.poll(async () => (await sentInputs(page)).length, {
            timeout: 1_000,
        }).toBe(2);

        // The stale turn is still considered active, so new inputs queue
        // instead of dispatching immediately.
        await input.fill("must stay behind the unknown turn");
        await input.press("Enter");
        const panel = page.getByRole("region", { name: "后续消息队列" });
        await expect(panel).toContainText("must stay behind the unknown turn");
        expect(await sentInputs(page)).toHaveLength(2);

        // The next terminal envelope ends the stale turn and drains the queue.
        await emitDone(page, "done-after-unknown");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(3);
        expect((await sentInputs(page))[2]?.event.data.content).toContain(
            "must stay behind the unknown turn",
        );
    });

    test("keeps a durably accepted queue head in flight until delivered arrives", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(1);

        await input.fill("durable supplement");
        await input.press("Enter");
        const panel = page.getByRole("region", { name: "后续消息队列" });
        await expect(panel).toContainText("durable supplement");
        const assistantBodies = page.locator(".msg-assistant-body");
        const bodiesBefore = await assistantBodies.count();

        // The gateway durably accepted the drained follow-up but has NOT handed
        // it to the Worker yet: `delivered` is still owed, so acceptance alone
        // must not settle the turn.
        await setNextInputOutcome(page, "queued");
        await emitDone(page, "done-before-durable-ack");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(2);

        // The turn keeps running: a new input queues behind it and no
        // completion was invented for the merely accepted input.
        await input.fill("behind a durable acceptance");
        await input.press("Enter");
        await expect(panel).toContainText("behind a durable acceptance");
        await expect
            .poll(async () => (await sentInputs(page)).length, {
                timeout: 1_000,
            })
            .toBe(2);
        expect(await assistantBodies.count()).toBe(bodiesBefore);

        // The real terminal envelope still completes the turn and drains what
        // followed — the intermediate acceptance did not wedge anything.
        await emitDone(page, "done-after-durable-ack");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(3);
        expect((await sentInputs(page))[2]?.event.data.content).toContain(
            "behind a durable acceptance",
        );
        await expect(panel).toHaveCount(0);
    });

    test("releases a volatile supplement acceptance without faking a completion", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(1);

        await input.fill("buffered supplement");
        await input.press("Enter");
        const panel = page.getByRole("region", { name: "后续消息队列" });
        await expect(panel).toContainText("buffered supplement");

        const assistantBodies = page.locator(".msg-assistant-body");
        const bodiesBefore = await assistantBodies.count();

        // The gateway staged the drained follow-up in process memory: one
        // volatile accepted receipt and NO delivered ever follows.
        await setNextInputOutcome(page, "volatile");
        await emitDone(page, "done-before-volatile-ack");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(2);

        // The head-of-line item is released rather than left "sending"
        // forever, and the receipt does not conjure another input.
        await expect(panel).toHaveCount(0);
        await expect
            .poll(async () => (await sentInputs(page)).length, {
                timeout: 1_000,
            })
            .toBe(2);

        // Buffering is not a finished turn: the parent stays active, so a new
        // input queues instead of dispatching, the transcript still shows the
        // user message exactly once, and no assistant turn was invented.
        await input.fill("after volatile accept");
        await input.press("Enter");
        await expect(panel).toContainText("after volatile accept");
        expect(await sentInputs(page)).toHaveLength(2);
        await expect(
            page.getByText("buffered supplement", { exact: true }),
        ).toHaveCount(1);
        expect(await assistantBodies.count()).toBe(bodiesBefore);

        // The real terminal envelope still ends the turn and drains what
        // followed — the release did not wedge the queue.
        await emitDone(page, "done-after-volatile-ack");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(3);
        expect((await sentInputs(page))[2]?.event.data.content).toContain(
            "after volatile accept",
        );
    });

    test("deletes a failed send-now item from the queue", async ({ page }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await input.fill("urgent to delete");
        await input.press("Enter");

        await disconnectGateway(page, "running", true);
        const urgent = page
            .getByRole("listitem")
            .filter({ hasText: "urgent to delete" });
        await urgent.getByRole("button", { name: /展开队列第/ }).click();
        await urgent
            .getByRole("button", { name: /停止当前轮次并立即发送队列第/ })
            .click();
        await expect(urgent).toContainText("需要处理");

        await urgent.getByRole("button", { name: /删除队列第/ }).click();

        await expect(
            page.getByRole("region", { name: "后续消息队列" }),
        ).toHaveCount(0);
        expect(await sentInputs(page)).toHaveLength(1);
    });

    test("converges unknown delivery when the original turn finishes late", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await input.fill("late terminal follow-up");
        await input.press("Enter");

        await setNextInputOutcome(page, "unknown");
        await emitDone(page, "done-before-late-terminal");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(2);
        // The current runtime drops unknown-dispatched items, so the queue is
        // empty; convergence is verified on the transcript below.
        const panel = page.getByRole("region", { name: "后续消息队列" });
        await expect(panel).toHaveCount(0);

        await emitGatewayEvent(
            page,
            "message.start",
            { id: "late-assistant", role: "assistant", content_type: "text" },
            "late-message-start",
        );
        await emitGatewayEvent(page, "message.delta", {
            message_id: "late-assistant",
            content: "late response content",
        });
        await emitGatewayEvent(
            page,
            "done",
            {
                success: true,
                reason: "late-terminal-for-unknown",
                stats: {
                    _session: {
                        model_name: "late-convergence-model",
                        turn_count: 2,
                        tool_call_count: 0,
                        duration: "1s",
                        duration_seconds: 1,
                        total_input_tok: 10,
                        total_output_tok: 5,
                        context_fill: 10,
                        context_window: 100,
                        context_pct: 10,
                        total_cost_usd: 0,
                        turn_duration_ms: 1000,
                        turn_input_tok: 6,
                        turn_output_tok: 5,
                        turn_cost_usd: 0,
                        tool_names: null,
                        work_dir: "/tmp/hotplex-e2e",
                    },
                },
            },
            "late-terminal-for-unknown",
        );
        await expect(panel).toHaveCount(0);
        await expect(
            page.getByText("late terminal follow-up", { exact: true }),
        ).toHaveCount(1);
        const lateResponse = page.getByText("late response content", {
            exact: true,
        });
        await expect(lateResponse).toBeVisible();
        const lateResponseBody = lateResponse.locator(
            "xpath=ancestor::div[contains(concat(' ', normalize-space(@class), ' '), ' msg-assistant-body ')][1]",
        );
        await expect(lateResponseBody).toContainText("late-convergence-model");
        await expect(lateResponseBody.locator(".streaming-cursor")).toHaveCount(
            0,
        );
        await expect.poll(async () => (await sentInputs(page)).length, {
            timeout: 1_000,
        }).toBe(2);
        await expect(
            page.getByRole("button", { name: /重试队列第/ }),
        ).toHaveCount(0);
    });

    test("converges unknown delivery when its delivered ACK arrives late", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await input.fill("late acknowledgement follow-up");
        await input.press("Enter");

        await setNextInputOutcome(page, "unknown");
        await emitDone(page, "done-before-late-ack");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(2);
        const originalDispatch = (await sentInputs(page))[1];
        // The current runtime drops unknown-dispatched items, so the queue is
        // already empty; the late delivered ACK must remain a no-op.
        const panel = page.getByRole("region", { name: "后续消息队列" });
        await expect(panel).toHaveCount(0);

        await emitGatewayEvent(page, "input.ack", {
            client_message_id: originalDispatch.id,
            execution_id: `execution-${originalDispatch.id}`,
            status: "delivered",
        });
        await expect(panel).toHaveCount(0);
        expect(await sentInputs(page)).toHaveLength(2);
    });

    test("reports send-now failure instead of silently degrading while reconnecting", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await input.fill("urgent while reconnecting");
        await input.press("Enter");

        await disconnectGateway(page, "running", true);
        const urgent = page
            .getByRole("listitem")
            .filter({ hasText: "urgent while reconnecting" });
        await urgent.getByRole("button", { name: /展开队列第/ }).click();
        await urgent
            .getByRole("button", {
                name: /停止当前轮次并立即发送队列第/,
            })
            .click();

        await expect(urgent).toContainText("需要处理");
        await expect(urgent).toContainText("连接中断");
        expect(
            (await sentEvents(page)).filter(
                (event) => event.event.type === "control",
            ),
        ).toHaveLength(0);
    });

    test("closes failed popovers without ping-pong and re-surfaces re-failed items", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await input.fill("fail alpha");
        await input.press("Enter");
        await input.fill("fail beta");
        await input.press("Enter");

        await disconnectGateway(page, "running", true);

        const alpha = page
            .getByRole("listitem")
            .filter({ hasText: "fail alpha" });
        const beta = page
            .getByRole("listitem")
            .filter({ hasText: "fail beta" });

        // Fail both items via send-now while the turn is disconnected.
        await alpha.getByRole("button", { name: /展开队列第/ }).click();
        await alpha
            .getByRole("button", { name: /停止当前轮次并立即发送队列第/ })
            .click();
        await expect(alpha).toContainText("需要处理");
        await beta.getByRole("button", { name: /展开队列第/ }).click();
        await beta
            .getByRole("button", { name: /停止当前轮次并立即发送队列第/ })
            .click();
        await expect(beta).toContainText("需要处理");

        // ✕-closing one failed popover must be end-state-reaching even with
        // another failed item present — no ping-pong back to the other one.
        await beta.getByRole("button", { name: "✕", exact: true }).click();
        await expect(
            page.getByRole("button", { name: /停止当前轮次并立即发送队列第/ }),
        ).toHaveCount(0);
        await expect(page.getByRole("listitem")).toHaveCount(2);

        // A new failure episode on the same item works again: retry moves it
        // out of the failed set, and the next send-now failure is a fresh
        // transition that re-surfaces its popover.
        await beta.getByRole("button", { name: /展开队列第/ }).click();
        await beta.getByRole("button", { name: /重试队列第/ }).click();
        await expect(
            page.getByRole("button", { name: /停止当前轮次并立即发送队列第/ }),
        ).toHaveCount(0);

        await beta.getByRole("button", { name: /展开队列第/ }).click();
        await beta
            .getByRole("button", { name: /停止当前轮次并立即发送队列第/ })
            .click();
        await expect(beta).toContainText("需要处理");

        // The re-opened popover closes cleanly again.
        await beta.getByRole("button", { name: "✕", exact: true }).click();
        await expect(
            page.getByRole("button", { name: /停止当前轮次并立即发送队列第/ }),
        ).toHaveCount(0);
    });

    test("edits and deletes queued prompts before their only dispatch", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await input.fill("draft prompt");
        await input.press("Enter");
        await input.fill("remove me");
        await input.press("Enter");

        const draftItem = page
            .getByRole("listitem")
            .filter({ hasText: "draft prompt" });
        await draftItem.getByRole("button", { name: /展开队列第/ }).click();
        await draftItem.getByRole("button", { name: /编辑队列第/ }).click();
        const editInput = draftItem.getByRole("textbox", {
            name: /编辑队列第/,
        });
        await editInput.fill("final prompt");
        await page.getByRole("button", { name: "保存" }).click();

        const removedItem = page
            .getByRole("listitem")
            .filter({ hasText: "remove me" });
        await removedItem.getByRole("button", { name: /删除队列第/ }).click();
        await expect(page.getByText("remove me")).toHaveCount(0);

        await emitDone(page, "done-edit");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(2);
        const inputs = await sentInputs(page);
        expect(inputs[1]?.event.data.content).toBe("final prompt");
        expect(
            inputs.some((event) => event.event.data.content === "draft prompt"),
        ).toBe(false);
        expect(
            inputs.some((event) => event.event.data.content === "remove me"),
        ).toBe(false);
    });

    test("send now stops the current turn and promotes the selected item", async ({
        page,
    }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");
        await input.fill("normal follow-up");
        await input.press("Enter");
        await input.fill("urgent follow-up");
        await input.press("Enter");

        const urgent = page
            .getByRole("listitem")
            .filter({ hasText: "urgent follow-up" });
        await urgent.getByRole("button", { name: /展开队列第/ }).click();
        await urgent
            .getByRole("button", {
                name: /停止当前轮次并立即发送队列第/,
            })
            .click();

        await expect
            .poll(
                async () =>
                    (await sentEvents(page)).filter(
                        (event) => event.event.type === "control",
                    ).length,
            )
            .toBe(1);
        expect(await sentInputs(page)).toHaveLength(1);

        await emitDone(page, "done-stopped", "stopped_by_user");
        await expect.poll(async () => (await sentInputs(page)).length).toBe(2);
        const drained = (await sentInputs(page))[1]?.event.data.content ?? "";
        expect(drained).toContain("urgent follow-up");
        expect(drained).toContain("normal follow-up");
    });

    test("keeps the draft when the 20-item queue is full", async ({ page }) => {
        const input = composerInput(page);
        await input.fill("first");
        await input.press("Enter");

        for (let index = 1; index <= 20; index += 1) {
            await input.fill(`queued-${index}`);
            await input.press("Enter");
        }
        await expect(page.getByRole("listitem")).toHaveCount(20);

        await emitGatewayEvent(
            page,
            "message",
            {
                role: "assistant",
                content: "final answer before terminal done",
            },
            "message-before-done",
        );
        await input.fill("overflow draft");
        await input.press("Enter");
        await expect(input).toHaveValue("overflow draft");
        await expect(
            page.getByText("队列最多容纳 20 条消息", { exact: false }),
        ).toBeVisible();
        await expect(page.getByRole("listitem")).toHaveCount(20);
    });

    test("collapses and restores the desktop session sidebar", async ({
        page,
    }) => {
        const collapse = page.getByRole("button", { name: "折叠侧边栏" });
        await collapse.click();
        const expand = page.getByRole("button", { name: "展开侧边栏" });
        await expect(expand).toBeVisible();
        await expand.click();
        await expect(page.getByText("Queue E2E")).toBeVisible();
    });
});
