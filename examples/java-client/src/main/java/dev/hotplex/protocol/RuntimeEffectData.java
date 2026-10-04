package dev.hotplex.protocol;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * runtime.effect.* event payload (S->C additive).
 * Correlates an input acceptance to its external-delivery outcome.
 * Secret-free: no content, credentials, metadata values or raw errors.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class RuntimeEffectData {
    @JsonProperty("execution_id")
    private String executionId;

    @JsonProperty("effect_id")
    private String effectId;

    @JsonProperty("status")
    private String status;

    @JsonProperty("error_code")
    private String errorCode;

    @JsonProperty("evidence_ref")
    private String evidenceRef;

    @JsonProperty("finished_at")
    private Long finishedAt;

    public RuntimeEffectData() {}

    public String getExecutionId() {
        return executionId;
    }

    public String getEffectId() {
        return effectId;
    }

    public String getStatus() {
        return status;
    }
}
