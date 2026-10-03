import type { InputAckData } from "@/lib/ai-sdk-transport/client/types";

/**
 * What an `input.ack` receipt means for the client, kept separate from the
 * Agent run state so an input receipt can never be mistaken for a finished turn.
 *
 * `accepted` alone is ambiguous: a durable acceptance is an intermediate state
 * that `delivered` still follows, while a volatile acceptance is the gateway
 * staging the input in process memory with no further ACK ever coming.
 */
export type InputAckSettlement =
    /** Terminal success: the Worker owns the input. */
    | { kind: "delivered" }
    /** Terminal for the client, but NOT a delivery proof: staged in memory. */
    | { kind: "volatile-accepted"; parentExecutionId?: string }
    /** Intermediate: a durable ledger entry exists and `delivered` is still owed. */
    | { kind: "durable-accepted" }
    | { kind: "unknown"; errorCode?: string }
    | { kind: "failed"; errorCode?: string };

export type InputAckReceipt = Pick<
    InputAckData,
    "status" | "durability" | "input_mode" | "error_code" | "parent_execution_id"
>;

/**
 * Classify an `input.ack` receipt. Receipt state is deliberately independent of
 * whether the Agent run is finished — buffering or queueing an input says
 * nothing about the effect it eventually produces.
 */
export function classifyInputAck(data: InputAckReceipt): InputAckSettlement {
    switch (data.status) {
        case "delivered":
            return { kind: "delivered" };
        case "accepted":
            return data.durability === "volatile"
                ? {
                      kind: "volatile-accepted",
                      parentExecutionId: data.parent_execution_id,
                  }
                : { kind: "durable-accepted" };
        case "unknown":
            return { kind: "unknown", errorCode: data.error_code };
        case "failed":
            return { kind: "failed", errorCode: data.error_code };
    }
}

/**
 * Whether the client may stop waiting for a terminal ACK after this receipt.
 *
 * A volatile acceptance is terminal for the client because the gateway holds no
 * durable record to emit a later `delivered` from; a durable acceptance is not,
 * because `delivered` is still owed.
 */
export function isTerminalForClient(settlement: InputAckSettlement): boolean {
    return (
        settlement.kind === "delivered" ||
        settlement.kind === "volatile-accepted" ||
        settlement.kind === "unknown" ||
        settlement.kind === "failed"
    );
}
