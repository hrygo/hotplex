import { test, expect, type Page } from "@playwright/test";
import { installMockGateway } from "./fixtures/mock-gateway";

/**
 * Execution console (plan §5.9 / §8.2).
 *
 * The three properties this pins are the ones a unit test cannot prove,
 * because they are about what the operator is allowed to conclude from the
 * screen:
 *
 *  1. Absence of evidence renders as "not recorded for this run", never as a
 *     failure or a success.
 *  2. Actions come from the server. A response with no actions shows none —
 *     the console has no way to invent a "mark as delivered".
 *  3. A 409 from an action re-reads the record and says it was stale. It does
 *     not retry the write against a token that has already moved on.
 */

const EXEC_ID = "exec-console-e2e-0001";
const EFFECT_ID = "eff-console-e2e-0001";

/**
 * The gateway origin the console talks to (lib/config.ts adminUrl, run through
 * browserReachableUrl which rewrites localhost to the loopback IP). The glob
 * covers the whole origin rather than /admin/** because adminUrl ends in a
 * slash, so the console requests `//admin/...` with a doubled slash.
 */
const ADMIN_ORIGIN = "http://127.0.0.1:9999/**";

const normalize = (url: URL) => url.pathname.replace(/\/{2,}/g, "/");

const EXECUTION = {
    execution_id: EXEC_ID,
    session_id: "session-e2e",
    delivery_status: "delivered",
    runtime_status: "unknown",
    runtime_error_code: "WORKER_RESULT_LOST",
    worker_run_id: "wr-e2e-0001",
    fence_reason: "worker_response_lost",
    fence_version: 3,
    created_at: 1_700_000_000_000,
    updated_at: 1_700_000_060_000,
    started_at: 1_700_000_001_000,
};

const TIMELINE = {
    execution: EXECUTION,
    // Deliberately out of phase order on the wire; the console sorts by phase,
    // then fact time, then source.
    items: [
        {
            phase: "fenced",
            source: "execution_store",
            kind: "runtime.unknown",
            fact_time: 1_700_000_060_000,
            observed_at: 1_700_000_061_000,
            evidence_state: "recorded" as const,
        },
        {
            phase: "running",
            source: "event_store",
            kind: "worker.started",
            fact_time: 1_700_000_001_000,
            observed_at: 1_700_000_001_500,
            evidence_state: "recorded" as const,
        },
        {
            phase: "delivery",
            source: "effect_store",
            kind: "effect.receipt_unknown",
            fact_time: 1_700_000_059_000,
            observed_at: 1_700_000_059_500,
            effect_id: EFFECT_ID,
            evidence_state: "not_recorded_for_this_run" as const,
        },
    ],
    actions: [
        {
            kind: "fence_resolve",
            target: EXEC_ID,
            requires_version: 3,
            description: "确认 Worker 响应丢失后清除围栏",
        },
    ],
    plan_evidence: "recorded" as const,
    // The launch fingerprint for this run predates the field. The console must
    // say so rather than implying the plan was never applied.
    effect_evidence: "not_recorded_for_this_run" as const,
    truncated: false,
    notes: ["no_delivery_planned"],
};

const LIST = {
    executions: [EXECUTION],
    next_cursor: null,
};

type Route = Parameters<Parameters<Page["route"]>[1]>[0];

async function json(route: Route, body: unknown) {
    await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(body),
    });
}

/** Routes the admin API the console reads. Registered after installMockGateway
 *  so these win for /admin/** — the fixture only claims /api/**. */
async function mockAdminApi(page: Page, options?: { fenceStatus?: number }) {
    const fenceStatus = options?.fenceStatus ?? 200;
    // Scope the mock to the gateway origin only. The Next dev server runs on
    // :3000, so page navigations under /admin/* are never intercepted here.
    // The glob covers the whole origin because adminUrl ends in a slash and the
    // console therefore requests //admin/... with a doubled slash.
    await page.route(ADMIN_ORIGIN, async (route) => {
        const request = route.request();
        const url = new URL(request.url());
        // adminUrl ends in "/", so the console requests "//admin/..." — collapse it.
        const path = url.pathname.replace(/\/{2,}/g, "/");

        if (path === "/admin/executions" && request.method() === "GET") {
            await json(route, LIST);
            return;
        }
        if (path === `/admin/executions/${EXEC_ID}/timeline` && request.method() === "GET") {
            await json(route, TIMELINE);
            return;
        }
        if (
            path === `/admin/executions/${EXEC_ID}/fence-action` &&
            request.method() === "POST"
        ) {
            if (fenceStatus !== 200) {
                await route.fulfill({
                    status: fenceStatus,
                    contentType: "application/json",
                    body: JSON.stringify({
                        code: "EFFECT_CONFLICT",
                        message: "record moved",
                    }),
                });
                return;
            }
            await json(route, { ok: true });
            return;
        }
        // Health probe from the standalone token channel must not 404 the
        // shell into the login page.
        if (path === "/admin/health") {
            await json(route, { status: "ok" });
            return;
        }
        await json(route, {});
    });
}

test.describe("Execution console", () => {
    test.beforeEach(async ({ page }) => {
        // installMockGateway answers /api/auth/me with role=admin, so the shell
        // resolves to the cookie-admin channel and never asks for a token.
        await installMockGateway(page, "codex_cli");
        await mockAdminApi(page);
    });

    test("lists a run with its delivery, runtime and fence kept apart", async ({
        page,
    }) => {
        await page.goto("/admin/executions");

        await expect(
            page.getByRole("heading", { name: "执行记录", exact: true }),
        ).toBeVisible();

        // delivered (delivery) and unknown (runtime) are separate facts. A
        // console that collapsed them would render this row as "failed".
        // Scope to the row link: the filter <select> offers the same words.
        const row = page.getByRole("link").filter({ hasText: EXEC_ID });
        await expect(row).toHaveCount(1);
        await expect(row.getByText("已送达 Worker", { exact: true })).toBeVisible();
        await expect(row.getByText("不明", { exact: true })).toBeVisible();
        await expect(row.getByText("已围栏", { exact: true })).toBeVisible();
    });

    test("orders a mixed-source timeline by phase and states missing evidence", async ({
        page,
    }) => {
        await page.goto(`/admin/executions/detail?id=${EXEC_ID}`);

        await expect(page.getByText("清除围栏", { exact: true })).toBeVisible();

        // The wire order is fenced, running, delivery. PHASE_ORDER re-sorts it
        // to running, delivery, fenced, so read the rendered phase badges back
        // and pin that order — an append-in-arrival-order projection would
        // render 已围栏 first and fail here.
        const phaseBadges = page.locator(
            "span.inline-flex.rounded-full.uppercase",
        );
        const phases = (await phaseBadges.allInnerTexts()).map((t) => t.trim());
        const order = ["运行中", "外部交付", "已围栏"].map((label) =>
            phases.indexOf(label),
        );
        expect(order.every((i) => i >= 0)).toBe(true);
        expect(order).toEqual([...order].sort((a, b) => a - b));

        // The delivery row has no evidence string and no recorded plan for
        // this run: it must read as an explicit gap, not as a failure.
        // Two places say it (the evidence card and the delivery timeline row);
        // what matters is that both say "not recorded", never "failed".
        await expect(
            page.getByText("本次运行没有记录", { exact: true }),
        ).toHaveCount(2);
        await expect(
            page.getByText("本次运行没有记录", { exact: true }).first(),
        ).toBeVisible();
        await expect(
            page.getByText("失败", { exact: true }),
        ).toHaveCount(0);
        await expect(
            // The notes list prefixes each code with a bullet, so match loosely.
            page.getByText("本次运行没有计划任何外部交付。"),
        ).toBeVisible();
    });

    test("offers only the actions the server returned", async ({ page }) => {
        await page.goto(`/admin/executions/detail?id=${EXEC_ID}`);

        await expect(
            page.getByRole("button", { name: "选择", exact: true }),
        ).toHaveCount(1);
        // Nothing in the console's vocabulary means "mark this run successful".
        await expect(
            page.getByRole("button", { name: /成功/ }),
        ).toHaveCount(0);
    });

    test("shows no action section content when the server offers no action", async ({
        page,
    }) => {
        await page.route(ADMIN_ORIGIN, async (route) => {
            const request = route.request();
            const url = new URL(request.url());
            if (normalize(url).endsWith("/timeline")) {
                await json(route, { ...TIMELINE, actions: [] });
                return;
            }
            await route.fallback();
        });
        await page.goto(`/admin/executions/detail?id=${EXEC_ID}`);

        await expect(
            page.getByText("当前没有对该运行有效的动作。", { exact: true }),
        ).toBeVisible();
        await expect(
            page.getByRole("button", { name: "选择", exact: true }),
        ).toHaveCount(0);
    });

    test("re-reads the record and reports staleness on 409 instead of retrying", async ({
        page,
    }) => {
        await page.route(ADMIN_ORIGIN, async (route) => {
            const request = route.request();
            const url = new URL(request.url());
            if (
                normalize(url).endsWith("/fence-action") &&
                request.method() === "POST"
            ) {
                await route.fulfill({
                    status: 409,
                    contentType: "application/json",
                    body: JSON.stringify({ code: "EFFECT_CONFLICT", message: "moved" }),
                });
                return;
            }
            await route.fallback();
        });

        let timelineReads = 0;
        let fenceWrites = 0;
        page.on("request", (request) => {
            const path = normalize(new URL(request.url()));
            if (path.endsWith("/timeline")) timelineReads += 1;
            if (path.endsWith("/fence-action")) fenceWrites += 1;
        });

        await page.goto(`/admin/executions/detail?id=${EXEC_ID}`);
        await page.getByRole("button", { name: "选择", exact: true }).click();
        await page.getByRole("textbox").first().fill("核对上游日志后确认围栏");
        // The inline "执行" submits; the modal's confirm shares the label, so
        // confirm inside the modal (scoped by its heading) rather than by
        // another global click.
        await page
            .getByRole("button", { name: "执行", exact: true })
            .first()
            .click();
        await page
            .locator("div.fixed", {
                has: page.getByRole("heading", { name: "清除该围栏？" }),
            })
            .getByRole("button", { name: "执行", exact: true })
            .click();

        await expect(page.getByText(/该记录已发生变化/)).toBeVisible();
        // Re-read, not re-write: the POST count stays at one.
        expect(timelineReads).toBeGreaterThanOrEqual(2);
        expect(fenceWrites).toBe(1);
    });
});
