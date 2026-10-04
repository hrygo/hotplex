// Package schema provides the canonical AEP v1 schema, golden corpus fixtures,
// and conformance verification tools.
//
// The canonical schema (aep-v1.json) is the machine-readable source of truth for
// the wire contract. The golden corpus (corpus/*.json) contains one valid envelope
// per event kind plus edge-case fixtures for forward-compatibility testing.
//
// This file provides GenerateCorpus which deterministically produces all corpus
// fixtures from the Go type definitions, ensuring the corpus never drifts from
// the actual protocol implementation.
package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hrygo/hotplex/pkg/events"
)

// CorpusDir is the directory containing golden envelope fixtures.
const CorpusDir = "corpus"

// corpusFixture describes a single golden envelope fixture.
type corpusFixture struct {
	filename string
	envelope *events.Envelope
	// minimalDecode true means the fixture uses an unknown type or relaxed
	// validation (e.g., unknown kind, missing optional fields). These are
	// decoded via DecodeLineMinimal rather than strict Decode.
	minimalDecode bool
}

// envelopeTemplate holds shared fields for all corpus envelopes.
const (
	corpusSessionID = "sess_00000000-0000-4000-8000-000000000001"
	corpusEventID   = "evt_00000000-0000-4000-8000-000000000001"
	corpusTimestamp = 1710000000000
)

func mkEnv(seq int64, kind events.Kind, data any) *events.Envelope {
	return &events.Envelope{
		Version:   events.Version,
		ID:        corpusEventID,
		Seq:       seq,
		SessionID: corpusSessionID,
		Timestamp: corpusTimestamp,
		Event:     events.Event{Type: kind, Data: data},
	}
}

// allFixtures returns every golden corpus fixture, derived from the Go types.
func allFixtures() []corpusFixture {
	return []corpusFixture{
		// --- Stable: C2S ---
		{"00-init.json", mkEnv(1, events.Init, map[string]any{
			"version":     "aep/v1",
			"worker_type": "claude_code",
		}), false},
		{"01-input.json", mkEnv(1, events.Input, events.InputData{
			Content: "hello world",
		}), false},
		{"02-permission_response.json", mkEnv(1, events.PermissionResponse, events.PermissionResponseData{
			ID:      "perm_1",
			Allowed: true,
		}), false},
		{"03-question_response.json", mkEnv(1, events.QuestionResponse, events.QuestionResponseData{
			ID:      "q_1",
			Answers: map[string]string{"What framework?": "React"},
		}), false},
		{"04-elicitation_response.json", mkEnv(1, events.ElicitationResponse, events.ElicitationResponseData{
			ID:      "el_1",
			Action:  "accept",
			Content: map[string]any{"value": "confirmed"},
		}), false},
		{"05-ping.json", mkEnv(0, events.Ping, struct{}{}), true},
		{"06-worker_command.json", mkEnv(0, events.WorkerCmd, events.WorkerCommandData{
			Command: events.StdioContextUsage,
		}), true},

		// --- Stable: S2C ---
		{"10-error.json", mkEnv(1, events.Error, events.ErrorData{
			Code:    events.ErrCodeWorkerTimeout,
			Message: "worker exceeded turn timeout",
		}), false},
		{"11-state.json", mkEnv(1, events.State, events.StateData{
			State:   events.StateRunning,
			Message: "session started",
		}), false},
		{"12-input_ack.json", mkEnv(1, events.InputAck, events.InputAckData{
			ClientMessageID: "evt_client_1",
			ExecutionID:     "exec_1",
			Status:          events.ExecutionStatusDelivered,
			InputMode:       events.InputModePrimary,
			Durability:      events.InputDurabilityDurable,
		}), false},
		{"13-done.json", mkEnv(1, events.Done, events.DoneData{
			Success: true,
			Stats:   map[string]any{"duration_ms": float64(1234)},
		}), false},
		{"14-message.json", mkEnv(1, events.Message, events.MessageData{
			ID:          "msg_1",
			Role:        "assistant",
			Content:     "Hello!",
			ContentType: "text/plain",
		}), false},
		{"15-message_start.json", mkEnv(1, events.MessageStart, events.MessageStartData{
			ID:          "msg_1",
			Role:        "assistant",
			ContentType: "text/plain",
		}), false},
		{"16-message_delta.json", mkEnv(1, events.MessageDelta, events.MessageDeltaData{
			MessageID: "msg_1",
			Content:   "Hello",
		}), false},
		{"17-message_end.json", mkEnv(1, events.MessageEnd, events.MessageEndData{
			MessageID: "msg_1",
		}), false},
		{"18-tool_call.json", mkEnv(1, events.ToolCall, events.ToolCallData{
			ID:    "tc_1",
			Name:  "read_file",
			Input: map[string]any{"path": "main.go"},
		}), false},
		{"19-tool_result.json", mkEnv(1, events.ToolResult, events.ToolResultData{
			ID:     "tc_1",
			Output: "file contents here",
		}), false},
		{"20-reasoning.json", mkEnv(1, events.Reasoning, events.ReasoningData{
			ID:      "r_1",
			Content: "I should read the file first.",
			Model:   "claude-sonnet-4",
		}), false},
		{"21-step.json", mkEnv(1, events.Step, events.StepData{
			ID:       "step_1",
			StepType: "tool",
			Name:     "read_file",
		}), false},
		{"22-raw.json", mkEnv(1, events.Raw, events.RawData{
			Kind: "custom",
			Raw:  map[string]any{"custom": "data"},
		}), false},
		{"23-permission_request.json", mkEnv(1, events.PermissionRequest, events.PermissionRequestData{
			ID:       "perm_1",
			ToolName: "write_file",
		}), false},
		{"24-question_request.json", mkEnv(1, events.QuestionRequest, events.QuestionRequestData{
			ID: "q_1",
			Questions: []events.Question{{
				Question: "Which framework?",
				Header:   "Framework",
				Options:  []events.QuestionOption{{Label: "React"}, {Label: "Vue"}},
			}},
		}), false},
		{"25-elicitation_request.json", mkEnv(1, events.ElicitationRequest, events.ElicitationRequestData{
			ID:            "el_1",
			MCPServerName: "github",
			Message:       "Authorize GitHub access?",
		}), false},
		{"26-pong.json", mkEnv(0, events.Pong, struct{}{}), true},
		{"27-control.json", mkEnv(1, events.Control, events.ControlData{
			Action: events.ControlActionReset,
			Reason: "user requested reset",
		}), false},
		{"28-context_usage.json", mkEnv(1, events.ContextUsage, events.ContextUsageData{
			TotalTokens: 50000,
			MaxTokens:   200000,
			Percentage:  25,
		}), false},
		{"29-skills_list.json", mkEnv(1, events.SkillsList, events.SkillsListData{
			Skills: []events.SkillEntry{{Name: "code-review", Description: "Reviews code", Source: "project"}},
			Total:  1,
		}), false},
		{"30-mcp_status.json", mkEnv(1, events.MCPStatus, events.MCPStatusData{
			Servers: []events.MCPServerInfo{{Name: "github", Status: "connected"}},
		}), false},
		{"31-tool_update.json", mkEnv(1, events.ToolUpdate, events.ToolUpdateData{
			ID:     "tc_1",
			Status: "in_progress",
		}), false},
		{"32-plan.json", mkEnv(1, events.Plan, events.PlanData{
			Items: []events.PlanItem{{Content: "Read file", Priority: "high", Status: "pending"}},
		}), false},
		{"33-mode_update.json", mkEnv(1, events.ModeUpdate, events.ModeUpdateData{
			CurrentModeID: "default",
		}), false},

		// --- Additive: runtime/internal (S2C) ---
		{"40-internal_reset.json", mkEnv(1, events.KindInternalReset, events.InternalResetData{
			Generation: 2,
		}), false},
		{"41-runtime_execution_started.json", mkEnv(1, events.RuntimeExecutionStarted, events.RuntimeExecutionData{
			ExecutionID: "exec_1",
			Status:      "started",
			StartedAt:   1710000000000,
		}), false},
		{"42-runtime_execution_completed.json", mkEnv(1, events.RuntimeExecutionCompleted, events.RuntimeExecutionData{
			ExecutionID: "exec_1",
			Status:      "completed",
			FinishedAt:  1710000001000,
		}), false},
		{"43-runtime_execution_failed.json", mkEnv(1, events.RuntimeExecutionFailed, events.RuntimeExecutionData{
			ExecutionID: "exec_1",
			Status:      "failed",
			ErrorCode:   events.ErrCodeWorkerCrash,
			FinishedAt:  1710000001000,
		}), false},
		{"44-runtime_effect_planned.json", mkEnv(44, events.RuntimeEffectPlanned, events.RuntimeEffectData{
			ExecutionID: "exec_1",
			EffectID:    "eff_1",
			Status:      "planned",
		}), false},
		{"45-runtime_effect_reconciled.json", mkEnv(45, events.RuntimeEffectReconciled, events.RuntimeEffectData{
			ExecutionID: "exec_1",
			EffectID:    "eff_1",
			Status:      "reconciled_succeeded",
			EvidenceRef: "lookup-1",
		}), false},
		{"46-runtime_effect_fenced.json", mkEnv(46, events.RuntimeEffectFenced, events.RuntimeEffectData{
			ExecutionID: "exec_1",
			EffectID:    "eff_1",
			Status:      "fenced",
		}), false},
		{"47-runtime_operator_action.json", mkEnv(47, events.RuntimeOperatorAction, events.RuntimeOperatorActionData{
			ExecutionID: "exec_1",
			EffectID:    "eff_1",
			Decision:    "abandon",
			Status:      "failed",
		}), false},

		// --- Edge cases: forward compatibility ---
		{"90-compatibility-unknown-kind.json", mkEnv(0, events.Kind("custom.future_event"), map[string]any{
			"future_field": "future_value",
		}), true},
		{"91-compatibility-additive-fields.json", mkEnv(1, events.Message, map[string]any{
			"id":           "msg_1",
			"role":         "assistant",
			"content":      "Hello!",
			"content_type": "text/plain",
			"future_tag":   "unknown to old clients",
		}), true},
		{"92-compatibility-missing-optional.json", mkEnv(1, events.Done, map[string]any{
			"success": true,
		}), true},
		// A supplement buffered in process memory: accepted is NOT delivered, and
		// the volatility is what tells the client no later ACK is coming.
		{"93-compatibility-input-ack-buffered.json", mkEnv(1, events.InputAck, events.InputAckData{
			ClientMessageID:   "evt_client_2",
			ExecutionID:       "supplement-evt_client_2",
			Status:            events.ExecutionStatusAccepted,
			InputMode:         events.InputModeBuffered,
			Durability:        events.InputDurabilityVolatile,
			ParentExecutionID: "exec_parent_1",
		}), false},
		// A pre-receipt server payload: the mode/durability fields are absent and
		// must still strict-decode to the legacy meaning.
		{"94-compatibility-input-ack-legacy.json", mkEnv(1, events.InputAck, events.InputAckData{
			ClientMessageID: "evt_client_3",
			ExecutionID:     "exec_3",
			Status:          events.ExecutionStatusDelivered,
		}), false},
	}
}

// GenerateCorpus writes all golden envelope fixtures to the given directory.
// Output is deterministic: stable JSON key ordering via json.MarshalIndent.
// Returns the number of fixtures written.
func GenerateCorpus(dir string) (int, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	fixtures := AllFixtures()
	for _, f := range fixtures {
		data, err := json.MarshalIndent(f.Envelope, "", "  ")
		if err != nil {
			return 0, fmt.Errorf("marshal %s: %w", f.Filename, err)
		}
		data = append(data, '\n')

		path := filepath.Join(dir, f.Filename)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return 0, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return len(fixtures), nil
}

// CorpusFixture is the exported version of corpusFixture for test consumers.
type CorpusFixture struct {
	Filename      string
	Envelope      *events.Envelope
	MinimalDecode bool
}

// AllFixtures returns every golden corpus fixture, derived from the Go types.
func AllFixtures() []CorpusFixture {
	out := make([]CorpusFixture, len(allFixtures()))
	for i, f := range allFixtures() {
		out[i] = CorpusFixture{
			Filename:      f.filename,
			Envelope:      f.envelope,
			MinimalDecode: f.minimalDecode,
		}
	}
	return out
}

// LoadCorpusDir returns all fixture filenames and their raw JSON bytes from
// the given directory. Used by conformance tests across SDKs.
func LoadCorpusDir(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read corpus dir %s: %w", dir, err)
	}

	result := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		result[name] = data
	}
	return result, nil
}
