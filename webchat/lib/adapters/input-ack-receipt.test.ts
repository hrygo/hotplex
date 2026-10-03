import { describe, expect, it } from "vitest";

import {
    classifyInputAck,
    isTerminalForClient,
} from "@/lib/adapters/input-ack-receipt";

describe("classifyInputAck", () => {
    it("treats a durable primary acceptance as intermediate, not terminal", () => {
        const settlement = classifyInputAck({
            status: "accepted",
            input_mode: "primary",
            durability: "durable",
        });

        expect(settlement).toEqual({ kind: "durable-accepted" });
        expect(isTerminalForClient(settlement)).toBe(false);
    });

    it("treats a buffered volatile acceptance as terminal for the client but not a delivery", () => {
        const settlement = classifyInputAck({
            status: "accepted",
            input_mode: "buffered",
            durability: "volatile",
            parent_execution_id: "exec-parent",
        });

        expect(settlement).toEqual({
            kind: "volatile-accepted",
            parentExecutionId: "exec-parent",
        });
        expect(isTerminalForClient(settlement)).toBe(true);
    });

    it("never reports a buffered acceptance as delivered", () => {
        expect(
            classifyInputAck({
                status: "accepted",
                input_mode: "buffered",
                durability: "volatile",
            }).kind,
        ).not.toBe("delivered");
    });

    it("keeps legacy payloads without durability on the durable intermediate path", () => {
        // A pre-receipt server sends accepted with no mode/durability for the
        // ordinary input path, which did have a durable ledger entry.
        const settlement = classifyInputAck({ status: "accepted" });

        expect(settlement).toEqual({ kind: "durable-accepted" });
        expect(isTerminalForClient(settlement)).toBe(false);
    });

    it("treats an injected delivered receipt as delivered", () => {
        const settlement = classifyInputAck({
            status: "delivered",
            input_mode: "injected",
            durability: "volatile",
            parent_execution_id: "exec-parent",
        });

        expect(settlement).toEqual({ kind: "delivered" });
        expect(isTerminalForClient(settlement)).toBe(true);
    });

    it("carries the error code for unknown and failed receipts", () => {
        expect(
            classifyInputAck({
                status: "unknown",
                error_code: "EXECUTION_TIMEOUT",
            }),
        ).toEqual({ kind: "unknown", errorCode: "EXECUTION_TIMEOUT" });
        expect(classifyInputAck({ status: "failed" })).toEqual({
            kind: "failed",
            errorCode: undefined,
        });
    });
});
