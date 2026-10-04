package dev.hotplex.protocol;

import com.fasterxml.jackson.annotation.JsonInclude;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * runtime.operator.action payload (S->C additive).
 * Which operator decision moved which effect, and to what.
 */
@JsonInclude(JsonInclude.Include.NON_NULL)
public class RuntimeOperatorActionData {
    @JsonProperty("execution_id")
    private String executionId;

    @JsonProperty("effect_id")
    private String effectId;

    @JsonProperty("decision")
    private String decision;

    @JsonProperty("status")
    private String status;

    public RuntimeOperatorActionData() {}

    public String getDecision() {
        return decision;
    }

    public String getStatus() {
        return status;
    }
}
