package dev.hotplex.protocol;

/**
 * Event kinds for AEP v1 protocol.
 */
public enum EventKind {
    Error("error"),
    State("state"),
    Input("input"),
    InputAck("input.ack"),
    Done("done"),
    Message("message"),
    MessageStart("message.start"),
    MessageDelta("message.delta"),
    MessageEnd("message.end"),
    ToolCall("tool_call"),
    ToolResult("tool_result"),
    Reasoning("reasoning"),
    Step("step"),
    Raw("raw"),
    PermissionRequest("permission_request"),
    PermissionResponse("permission_response"),
    Ping("ping"),
    Pong("pong"),
    Control("control"),
    InitAck("init_ack"),
    QuestionRequest("question_request"),
    QuestionResponse("question_response"),
    ElicitationRequest("elicitation_request"),
    ElicitationResponse("elicitation_response"),
    ContextUsage("context_usage"),
    SkillsList("skills_list"),
    MCPStatus("mcp_status"),
    WorkerCommand("worker_command"),
    ToolUpdate("tool_update"),
    Plan("plan"),
    ModeUpdate("mode_update"),
    InternalReset("internal_reset"),
    RuntimeExecutionStarted("runtime.execution.started"),
    RuntimeExecutionCompleted("runtime.execution.completed"),
    RuntimeExecutionFailed("runtime.execution.failed"),
    RuntimeEffectPlanned("runtime.effect.planned"),
    RuntimeEffectReconciled("runtime.effect.reconciled"),
    RuntimeEffectFenced("runtime.effect.fenced"),
    RuntimeOperatorAction("runtime.operator.action"),
    Init("init");

    private final String value;

    EventKind(String value) {
        this.value = value;
    }

    public String getValue() {
        return value;
    }

    public static EventKind fromValue(String value) {
        for (EventKind kind : values()) {
            if (kind.value.equals(value)) {
                return kind;
            }
        }
        throw new IllegalArgumentException("Unknown event kind: " + value);
    }
}
