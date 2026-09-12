package events

import (
	"encoding/json"
	"time"
)

// Version is the current AEP protocol version.
const Version = "aep/v1"

// Kind is the event type discriminator, matching the CloudEvents "type" field.
type Kind string

// AEP v1 defined event kinds.
const (
	Init                Kind = "init" // session initialization (client → gateway)
	Error               Kind = "error"
	State               Kind = "state"
	Input               Kind = "input"
	InputAck            Kind = "input.ack" // durable input acceptance/delivery acknowledgement (S→C)
	Done                Kind = "done"
	Message             Kind = "message"
	MessageStart        Kind = "message.start"
	MessageDelta        Kind = "message.delta"
	MessageEnd          Kind = "message.end"
	ToolCall            Kind = "tool_call"
	ToolResult          Kind = "tool_result"
	Reasoning           Kind = "reasoning"
	Step                Kind = "step"
	Raw                 Kind = "raw"
	PermissionRequest   Kind = "permission_request"
	PermissionResponse  Kind = "permission_response"
	QuestionRequest     Kind = "question_request"
	QuestionResponse    Kind = "question_response"
	ElicitationRequest  Kind = "elicitation_request"
	ElicitationResponse Kind = "elicitation_response"
	Ping                Kind = "ping"
	Pong                Kind = "pong"
	Control             Kind = "control"
	ContextUsage        Kind = "context_usage"  // Worker context usage report (S→C)
	SkillsList          Kind = "skills_list"    // Gateway-originated skill list (S→C)
	MCPStatus           Kind = "mcp_status"     // Worker MCP server status (S→C)
	WorkerCmd           Kind = "worker_command" // Gateway → Worker stdio command trigger (C→S)

	// Internal lifecycle kinds — emitted by workers into their event stream
	// for gateway-internal coordination. Never forwarded to clients.
	KindInternalReset Kind = "internal_reset" // worker in-place reset notification

	// ACP extension kinds — any ACP-compatible agent may emit these.
	ToolUpdate Kind = "tool_update" // intermediate tool call status (ACP tool_call_update)
	Plan       Kind = "plan"        // plan/todo update (ACP AgentPlanUpdate)
	ModeUpdate Kind = "mode_update" // agent mode switch (ACP CurrentModeUpdate)

	RuntimeExecutionStarted   Kind = "runtime.execution.started"   // execution dispatch started (S→C additive)
	RuntimeExecutionCompleted Kind = "runtime.execution.completed" // execution completed (S→C additive)
	RuntimeExecutionFailed    Kind = "runtime.execution.failed"    // execution failed (S→C additive)
)

// Priority levels for message delivery.
type Priority string

const (
	PriorityControl Priority = "control" // control messages bypass backpressure
	PriorityData    Priority = "data"    // default priority, subject to backpressure
)

// ErrorCode defines standardized error codes.
type ErrorCode string

const (
	ErrCodeWorkerStartFailed  ErrorCode = "WORKER_START_FAILED"
	ErrCodeWorkerCrash        ErrorCode = "WORKER_CRASH"
	ErrCodeWorkerTimeout      ErrorCode = "WORKER_TIMEOUT"
	ErrCodeWorkerOOM          ErrorCode = "WORKER_OOM"
	ErrCodeWorkerSIGKILL      ErrorCode = "PROCESS_SIGKILL"
	ErrCodeInvalidMessage     ErrorCode = "INVALID_MESSAGE"
	ErrCodeSessionNotFound    ErrorCode = "SESSION_NOT_FOUND"
	ErrCodeSessionExpired     ErrorCode = "SESSION_EXPIRED"
	ErrCodeSessionTerminated  ErrorCode = "SESSION_TERMINATED"
	ErrCodeSessionInvalidated ErrorCode = "SESSION_INVALIDATED"
	ErrCodeSessionBusy        ErrorCode = "SESSION_BUSY"
	// ErrCodeSessionAlreadyConnected rejects a second WebChat WebSocket owner
	// for the same session. Unlike SESSION_BUSY, the connection is unusable and
	// clients must wait for the existing owner to release it before retrying.
	ErrCodeSessionAlreadyConnected ErrorCode = "SESSION_ALREADY_CONNECTED"
	ErrCodeUnauthorized            ErrorCode = "UNAUTHORIZED"
	ErrCodeAuthRequired            ErrorCode = "AUTH_REQUIRED"
	ErrCodeInternalError           ErrorCode = "INTERNAL_ERROR"
	ErrCodeProtocolViolation       ErrorCode = "PROTOCOL_VIOLATION"
	ErrCodeVersionMismatch         ErrorCode = "VERSION_MISMATCH"
	ErrCodeConfigInvalid           ErrorCode = "CONFIG_INVALID"
	ErrCodeRateLimited             ErrorCode = "RATE_LIMITED"
	ErrCodeGatewayOverload         ErrorCode = "GATEWAY_OVERLOAD"
	ErrCodeExecutionTimeout        ErrorCode = "EXECUTION_TIMEOUT"
	ErrCodeReconnectRequired       ErrorCode = "RECONNECT_REQUIRED"
	ErrCodeWorkerOutputLimit       ErrorCode = "WORKER_OUTPUT_LIMIT"
	ErrCodeResumeRetry             ErrorCode = "RESUME_RETRY"
	ErrCodeNotSupported            ErrorCode = "NOT_SUPPORTED"
	ErrCodeTurnTimeout             ErrorCode = "TURN_TIMEOUT"
	// ErrCodeOperatorAbandoned marks a fenced execution the operator chose to
	// abandon (#877). Additive constant only — reuses the existing
	// runtime.execution.failed Kind, no wire-contract change.
	ErrCodeOperatorAbandoned ErrorCode = "OPERATOR_ABANDONED"
)

// Envelope is the unified AEP v1 message envelope, shared by both client→gateway and gateway→client.
type Envelope struct {
	Version   string   `json:"version"`
	ID        string   `json:"id"`
	Seq       int64    `json:"seq"`
	Priority  Priority `json:"priority,omitempty"`
	SessionID string   `json:"session_id"`
	Timestamp int64    `json:"timestamp"`
	Event     Event    `json:"event"`
	// Metadata carries opaque metadata injected by the gateway
	// (e.g., trace_id for distributed tracing context propagation).
	// TODO: inject trace_id via gateway hub.SendToSession (hub.go)
	Metadata map[string]any `json:"metadata,omitempty"`
	// OwnerID is the authenticated user who owns this envelope.
	// Set by the gateway at init time and used for ownership validation.
	// Not serialized over the wire.
	OwnerID string `json:"-"`
}

// Clone returns a copy of the Envelope safe for concurrent use.
// The copy has independent value-type fields (Version, Timestamp, etc.)
// so EncodeJSON's in-place mutations on the original do not affect the copy.
// Exported payload data, including typed structs, maps, slices and pointers,
// is recursively copied without a JSON round-trip. Cyclic reference graphs
// are supported. Opaque unexported state, functions and channels are not wire
// payload data and must be treated as immutable by their owners.
func Clone(env *Envelope) *Envelope {
	if env == nil {
		return &Envelope{}
	}
	c := *env
	c.Event.Data = deepCopyValue(env.Event.Data)
	if env.Metadata != nil {
		c.Metadata = deepCopyMap(env.Metadata)
	}
	return &c
}

// deepCopyMap returns a fully independent copy of a map[string]any,
// recursively copying nested maps and slices.
func deepCopyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	copied, ok := deepCopyValue(src).(map[string]any)
	if !ok {
		panic("events: clone changed map payload type")
	}
	return copied
}

// deepCopyValue preserves concrete Go payload types and mutable exported data.
// Functions, channels and opaque unexported state are not wire payloads and
// retain their original identity; callers must treat those values as immutable.
func deepCopyValue(v any) any {
	switch v.(type) {
	case nil, string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64, MessageDeltaData, MessageEndData, StateData, ErrorData, InputAckData:
		return v
	default:
		return clonePayloadGraph(v)
	}
}

// CloneDeep returns a deep copy via JSON round-trip for use cases that need
// full independence (e.g., event store persistence, replay).
func CloneDeep(env *Envelope) *Envelope {
	data, err := json.Marshal(env)
	if err != nil {
		return &Envelope{}
	}
	var c Envelope
	if err := json.Unmarshal(data, &c); err != nil {
		return &Envelope{}
	}
	return &c
}

// Event wraps a kind and its data payload.
type Event struct {
	Type Kind        `json:"type"`
	Data interface{} `json:"data"`
}

// ErrorData is the payload for Error events.
type ErrorData struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// StateData is the payload for State events.
type StateData struct {
	State   SessionState `json:"state"`
	Message string       `json:"message,omitempty"`
}

// InputData is the payload for Input events (client → gateway).
type InputData struct {
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// ExecutionStatus is the durable delivery state reported by InputAck.
type ExecutionStatus string

const (
	ExecutionStatusAccepted  ExecutionStatus = "accepted"
	ExecutionStatusDelivered ExecutionStatus = "delivered"
	ExecutionStatusUnknown   ExecutionStatus = "unknown"
	ExecutionStatusFailed    ExecutionStatus = "failed"
)

// InputAckData correlates a client input envelope with its durable execution.
// Duplicate is true when the gateway replayed the existing outcome instead of
// invoking Worker.Input again.
type InputAckData struct {
	ClientMessageID string          `json:"client_message_id"`
	ExecutionID     string          `json:"execution_id"`
	Status          ExecutionStatus `json:"status"`
	Duplicate       bool            `json:"duplicate,omitempty"`
	ErrorCode       ErrorCode       `json:"error_code,omitempty"`
}

// MessageStartData is the payload for MessageStart events.
type MessageStartData struct {
	ID          string         `json:"id"`
	Role        string         `json:"role"`
	ContentType string         `json:"content_type"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// MessageDeltaData is the payload for MessageDelta events.
type MessageDeltaData struct {
	MessageID string `json:"message_id"`
	Content   string `json:"content"`
}

// MessageEndData is the payload for MessageEnd events.
type MessageEndData struct {
	MessageID string `json:"message_id"`
}

// FileLocation references a position in a file.
type FileLocation struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

// FileDiff represents a structured file edit (unified diff equivalent).
type FileDiff struct {
	Path    string `json:"path"`
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

// ToolCallData is the payload for ToolCall events.
type ToolCallData struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
	// ACP extension fields — zero breaking change, omitted by existing workers.
	Title     string         `json:"title,omitempty"` // "read: main.go"
	Kind      string         `json:"kind,omitempty"`  // read/edit/delete/move/search/execute/think/fetch/switch_mode/other
	Locations []FileLocation `json:"locations,omitempty"`
}

// ToolResultData is the payload for ToolResult events.
type ToolResultData struct {
	ID     string `json:"id"`
	Output any    `json:"output"`
	Error  string `json:"error,omitempty"`
	// ACP extension fields — zero breaking change, omitted by existing workers.
	Status string    `json:"status,omitempty"` // completed / failed
	Diff   *FileDiff `json:"diff,omitempty"`
}

// RawData is the payload for Raw events (passthrough for agent-specific messages).
type RawData struct {
	Kind string `json:"kind"`
	Raw  any    `json:"raw"`
}

// DoneData is the payload for Done events.
type DoneData struct {
	Success bool           `json:"success"`
	Stats   map[string]any `json:"stats,omitempty"`
	// Dropped is true if the UI Reconciliation triggered due to silent backpressure drops
	Dropped bool   `json:"dropped,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// RuntimeExecutionData is the payload for runtime.execution.* events (S→C additive).
// These events correlate an input acceptance to its terminal Worker outcome via execution_id.
// Old clients that don't recognize these kinds silently ignore them.
type RuntimeExecutionData struct {
	ExecutionID string    `json:"execution_id"`
	Status      string    `json:"status"`
	ErrorCode   ErrorCode `json:"error_code,omitempty"`
	StartedAt   int64     `json:"started_at,omitempty"`
	FinishedAt  int64     `json:"finished_at,omitempty"`
}

// MessageData is the payload for Message events (S→C — complete message, non-streaming).
// In streaming scenarios, prefer MessageStartData/MessageDeltaData/MessageEndData.
// The gateway treats Message as a pass-through event for backward compatibility.
type MessageData struct {
	ID          string         `json:"id"`
	Role        string         `json:"role"`
	Content     string         `json:"content"`
	ContentType string         `json:"content_type,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// ReasoningData is the payload for Reasoning events (S→C — agent thinking/reasoning).
type ReasoningData struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Model   string `json:"model,omitempty"`
}

// StepData is the payload for Step events (S→C — execution step marker).
type StepData struct {
	ID       string         `json:"id"`
	StepType string         `json:"step_type"`
	Name     string         `json:"name,omitempty"`
	Input    map[string]any `json:"input,omitempty"`
	Output   map[string]any `json:"output,omitempty"`
	ParentID string         `json:"parent_id,omitempty"`
	Duration int64          `json:"duration,omitempty"` // milliseconds
}

// PermissionRequestData is the payload for PermissionRequest events (S→C — ask user for permission).
type PermissionRequestData struct {
	ID          string          `json:"id"`
	ToolName    string          `json:"tool_name"`
	Description string          `json:"description,omitempty"`
	Args        []string        `json:"args,omitempty"`
	InputRaw    json.RawMessage `json:"input_raw,omitempty"`
}

// PermissionResponseData is the payload for PermissionResponse events (C→S — user grants/denies).
type PermissionResponseData struct {
	ID      string `json:"id"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// QuestionOption represents a single selectable option in a question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Preview     string `json:"preview,omitempty"`
}

// Question represents a single question with options.
type Question struct {
	ID          string           `json:"id,omitempty"`
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multi_select"`
}

// UnmarshalJSON implements custom JSON unmarshaling to support aliases for MultiSelect.
func (q *Question) UnmarshalJSON(data []byte) error {
	type Alias Question
	aux := &struct {
		IsMultiSelect *bool `json:"is_multi_select,omitempty"`
		MultiSelect   *bool `json:"multi_select,omitempty"`
		Multiple      *bool `json:"multiple,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(q),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.IsMultiSelect != nil {
		q.MultiSelect = *aux.IsMultiSelect
	} else if aux.MultiSelect != nil {
		q.MultiSelect = *aux.MultiSelect
	} else if aux.Multiple != nil {
		q.MultiSelect = *aux.Multiple
	}
	return nil
}

// QuestionRequestData is the payload for QuestionRequest events (S→C — ask user a question).
type QuestionRequestData struct {
	ID        string     `json:"id"`
	ToolName  string     `json:"tool_name,omitempty"` // "AskUserQuestion" | "question"
	Questions []Question `json:"questions"`
}

// QuestionResponseData is the payload for QuestionResponse events (C→S — user answers).
type QuestionResponseData struct {
	ID      string            `json:"id"`
	Answers map[string]string `json:"answers"` // question text → selected label
}

// ElicitationRequestData is the payload for ElicitationRequest events (S→C — MCP server requests user input).
type ElicitationRequestData struct {
	ID              string         `json:"id"`
	MCPServerName   string         `json:"mcp_server_name"`
	Message         string         `json:"message"`
	Mode            string         `json:"mode,omitempty"`
	URL             string         `json:"url,omitempty"`
	ElicitationID   string         `json:"elicitation_id,omitempty"`
	RequestedSchema map[string]any `json:"requested_schema,omitempty"`
}

// ElicitationResponseData is the payload for ElicitationResponse events (C→S — user responds to MCP elicitation).
type ElicitationResponseData struct {
	ID      string         `json:"id"`
	Action  string         `json:"action"` // "accept" | "decline" | "cancel"
	Content map[string]any `json:"content,omitempty"`
}

// ControlAction identifies the type of server-originated control instruction.
type ControlAction string

const (
	ControlActionReconnect      ControlAction = "reconnect"
	ControlActionSessionInvalid ControlAction = "session_invalid"
	ControlActionThrottle       ControlAction = "throttle"
	ControlActionTerminate      ControlAction = "terminate"
	ControlActionDelete         ControlAction = "delete"
	ControlActionReset          ControlAction = "reset" // 清空上下文，Worker 自行决定 in-place 或 terminate+start
	ControlActionGC             ControlAction = "gc"    // 归档会话，Worker 终止，保留历史
	ControlActionCD             ControlAction = "cd"    // 切换工作目录，创建新会话
	ControlActionStop           ControlAction = "stop"  // 停止当前 turn，保留 session
)

// ControlData is the payload for Control events.
type ControlData struct {
	Action      ControlAction  `json:"action"`
	Reason      string         `json:"reason,omitempty"`
	DelayMs     int            `json:"delay_ms,omitempty"`
	Recoverable bool           `json:"recoverable,omitempty"`
	Suggestion  map[string]any `json:"suggestion,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
}

// WorkerStdioCommand identifies a stdio-level command sent directly
// to the worker subprocess. Unlike ControlAction, these do NOT change
// session state — they are in-place operations on a running worker.
type WorkerStdioCommand string

const (
	// Control Requests (structured request/response via SendControlRequest).
	StdioContextUsage WorkerStdioCommand = "context_usage"
	StdioMCPStatus    WorkerStdioCommand = "mcp_status"
	StdioSetModel     WorkerStdioCommand = "set_model"
	StdioSetPermMode  WorkerStdioCommand = "set_permission"
	StdioSkills       WorkerStdioCommand = "skills"

	// User Message Passthrough (slash command forwarded as user message via Input).
	StdioCompact WorkerStdioCommand = "compact"
	StdioClear   WorkerStdioCommand = "clear"
	StdioModel   WorkerStdioCommand = "model"
	StdioEffort  WorkerStdioCommand = "effort"
	StdioRewind  WorkerStdioCommand = "rewind"
	StdioCommit  WorkerStdioCommand = "commit"
)

// IsPassthrough returns true if the command is sent as a user message via Input.
func (c WorkerStdioCommand) IsPassthrough() bool {
	switch c {
	case StdioCompact, StdioClear, StdioModel, StdioEffort, StdioRewind, StdioCommit:
		return true
	default:
		return false
	}
}

// WorkerCommandData is the payload for worker stdio command events.
type WorkerCommandData struct {
	Command WorkerStdioCommand `json:"command"`
	Args    string             `json:"args,omitempty"`
	Extra   map[string]any     `json:"extra,omitempty"`
}

// ContextUsageData carries context window usage breakdown from a worker.
// TotalTokens semantics differ by worker:
//   - Claude Code: total input tokens (including cache) from the last API call.
//   - OCS: last assistant message's input + cache_read + cache_write.
//
// In both cases this represents the actual context window fill, not cumulative totals.
type ContextUsageData struct {
	TotalTokens int               `json:"total_tokens"`
	MaxTokens   int               `json:"max_tokens"`
	Percentage  int               `json:"percentage"`
	Model       string            `json:"model,omitempty"`
	Categories  []ContextCategory `json:"categories,omitempty"`
	MemoryFiles int               `json:"memory_files,omitempty"`
	MCPTools    int               `json:"mcp_tools,omitempty"`
	Agents      int               `json:"agents,omitempty"`
	Skills      ContextSkillInfo  `json:"skills,omitempty"`
}

// ContextCategory represents a named token usage bucket.
type ContextCategory struct {
	Name   string `json:"name"`
	Tokens int    `json:"tokens"`
}

// ToolUpdateData carries intermediate tool call status (ACP tool_call_update).
// Existing workers (ClaudeCode/Codex/OCS) do not emit this event.
type ToolUpdateData struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"` // pending / in_progress
	Content   any       `json:"content,omitempty"`
	Diff      *FileDiff `json:"diff,omitempty"`
	RawOutput string    `json:"raw_output,omitempty"`
}

// PlanData carries a plan/todo update (ACP AgentPlanUpdate).
type PlanData struct {
	Items []PlanItem `json:"items"`
}

// PlanItem represents a single item in a plan.
// Maps from ACP PlanEntry: content (description), priority, status.
type PlanItem struct {
	Content  string `json:"content"`  // task description (ACP PlanEntry.content)
	Priority string `json:"priority"` // high / medium / low (ACP PlanEntryPriority)
	Status   string `json:"status"`   // pending / in_progress / completed (ACP PlanEntryStatus)
}

// ModeUpdateData carries an agent execution mode switch notification.
// Maps from ACP CurrentModeUpdate: currentModeId references modes from session/new.
type ModeUpdateData struct {
	CurrentModeID string `json:"current_mode_id"`
}

// InternalResetData carries reset generation information for the internal_reset event.
// Emitted by in-place-reset workers (OCS, ACP) after resetting state.
type InternalResetData struct {
	Generation int64 `json:"generation"`
}

// ContextSkillInfo carries skill-related context usage.
type ContextSkillInfo struct {
	Total    int      `json:"total"`
	Included int      `json:"included"`
	Tokens   int      `json:"tokens"`
	Names    []string `json:"names,omitempty"`
}

// MCPStatusData carries MCP server connection status from a worker.
type MCPStatusData struct {
	Servers []MCPServerInfo `json:"servers"`
}

// MCPServerInfo describes a single MCP server's connection state.
type MCPServerInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// SkillsListData carries discovered skills to the client.
type SkillsListData struct {
	Skills []SkillEntry `json:"skills"`
	Total  int          `json:"total"`
	Filter string       `json:"filter,omitempty"`
}

// SkillEntry describes a single skill with name, description and source.
type SkillEntry struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Source      string      `json:"source"`            // "global" (home) or "project" (workspace/workDir)
	Managed     bool        `json:"managed,omitempty"` // issue #910: true = in .agents/skills (UI-manageable)
	Status      SkillStatus `json:"status,omitempty"`  // issue #957: invokability for the current session Worker
}

// SkillStatus reports whether the current session Worker can natively invoke a
// discovered Skill. Absent on the wire (omitempty) means the Worker cannot
// confirm invokability, which clients should treat as "discoverable".
type SkillStatus string

const (
	// SkillStatusCallable means the Worker's authoritative catalog contains the
	// Skill and is ready to run it natively.
	SkillStatusCallable SkillStatus = "callable"
	// SkillStatusDiscoverable means the Skill exists on disk but the Worker
	// cannot confirm native invocation (e.g. no catalog capability).
	SkillStatusDiscoverable SkillStatus = "discoverable"
	// SkillStatusUnavailable means the Worker's authoritative catalog explicitly
	// does not include the Skill.
	SkillStatusUnavailable SkillStatus = "unavailable"
)

// SessionState represents the state of a session.
type SessionState string

const (
	StateCreated    SessionState = "created"
	StateRunning    SessionState = "running"
	StateIdle       SessionState = "idle"
	StateTerminated SessionState = "terminated"
	StateDeleted    SessionState = "deleted"
)

// IsTerminal returns true if the session is in a terminal state.
func (s SessionState) IsTerminal() bool {
	return s == StateDeleted
}

// IsActive returns true if the session is in an active state.
func (s SessionState) IsActive() bool {
	return s == StateRunning || s == StateIdle || s == StateCreated
}

// validTransitions maps from a state to the set of valid next states.
var validTransitions = map[SessionState]map[SessionState]bool{
	StateCreated: {
		StateRunning:    true,
		StateTerminated: true,
	},
	StateRunning: {
		StateIdle:       true,
		StateTerminated: true,
		StateDeleted:    true,
	},
	StateIdle: {
		StateRunning:    true,
		StateTerminated: true,
		StateDeleted:    true,
	},
	StateTerminated: {
		StateRunning: true, // resume
		StateDeleted: true,
	},
	StateDeleted: {}, // terminal
}

// IsValidTransition returns true if transitioning from from → to is allowed.
func IsValidTransition(from, to SessionState) bool {
	if m, ok := validTransitions[from]; ok {
		return m[to]
	}
	return false
}

// NewEnvelope creates a new Envelope with timestamp and version set.
func NewEnvelope(id, sessionID string, seq int64, kind Kind, data interface{}) *Envelope {
	return &Envelope{
		Version:   Version,
		ID:        id,
		Seq:       seq,
		SessionID: sessionID,
		Timestamp: time.Now().UnixMilli(),
		Event: Event{
			Type: kind,
			Data: data,
		},
	}
}
