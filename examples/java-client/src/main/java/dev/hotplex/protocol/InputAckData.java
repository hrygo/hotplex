package dev.hotplex.protocol;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * input.ack event payload: durable input acceptance/delivery acknowledgement.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class InputAckData {
    @JsonProperty("client_message_id")
    private String clientMessageId;

    @JsonProperty("execution_id")
    private String executionId;

    @JsonProperty("status")
    private String status; // accepted / delivered / unknown / failed

    @JsonProperty("duplicate")
    private Boolean duplicate;

    @JsonProperty("error_code")
    private String errorCode;

    /** How the gateway handled the input. Absent on pre-receipt servers. */
    @JsonProperty("input_mode")
    private String inputMode; // primary / injected / buffered / queued

    /**
     * Recovery guarantee. {@code accepted} is only durable when this is
     * {@code durable}; a volatile acceptance is an in-memory staging decision,
     * not a delivery proof.
     */
    @JsonProperty("durability")
    private String durability; // durable / volatile

    /** Set on injected/buffered receipts to correlate with the running turn. */
    @JsonProperty("parent_execution_id")
    private String parentExecutionId;

    public InputAckData() {}

    public String getClientMessageId() {
        return clientMessageId;
    }

    public void setClientMessageId(String clientMessageId) {
        this.clientMessageId = clientMessageId;
    }

    public String getExecutionId() {
        return executionId;
    }

    public void setExecutionId(String executionId) {
        this.executionId = executionId;
    }

    public String getStatus() {
        return status;
    }

    public void setStatus(String status) {
        this.status = status;
    }

    public Boolean getDuplicate() {
        return duplicate;
    }

    public void setDuplicate(Boolean duplicate) {
        this.duplicate = duplicate;
    }

    public String getErrorCode() {
        return errorCode;
    }

    public void setErrorCode(String errorCode) {
        this.errorCode = errorCode;
    }

    public String getInputMode() {
        return inputMode;
    }

    public void setInputMode(String inputMode) {
        this.inputMode = inputMode;
    }

    public String getDurability() {
        return durability;
    }

    public void setDurability(String durability) {
        this.durability = durability;
    }

    public String getParentExecutionId() {
        return parentExecutionId;
    }

    public void setParentExecutionId(String parentExecutionId) {
        this.parentExecutionId = parentExecutionId;
    }
}
