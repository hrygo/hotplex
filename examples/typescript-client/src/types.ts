import type { EventKind, Priority, SessionState, ErrorCode, ControlAction, WorkerType } from './constants.js';

// ============================================================================
// Envelope and Event Structures (from pkg/events/events.go)
// ============================================================================

export interface Envelope<T = unknown> {
  version: string;
  id: string;
  seq: number;
  priority?: Priority;
  session_id: string;
  timestamp: number;
  event: Event<T>;
}

export interface Event<T = unknown> {
  type: string;
  data: T;
}

// ============================================================================
// Event Data Types (from pkg/events/events.go:109-216)
// ============================================================================

export interface ErrorData {
  code: ErrorCode;
  message: string;
  event_id?: string;
  details?: Record<string, unknown>;
}

export interface StateData {
  state: SessionState;
  message?: string;
}

export interface InputData {
  content: string;
  metadata?: Record<string, unknown>;
}

export interface MessageStartData {
  id: string;
  role: string;
  content_type: string;
  metadata?: Record<string, unknown>;
}

export interface MessageDeltaData {
  message_id: string;
  content: string;
}

export interface MessageEndData {
  message_id: string;
}

export interface ToolCallData {
  id: string;
  name: string;
  input: Record<string, unknown>;
  title?: string;           // e.g. "read: main.go"
  kind?: string;            // read/edit/delete/move/search/execute/think/fetch/switch_mode/other
  locations?: FileLocation[];
}

export interface ToolResultData {
  id: string;
  output: unknown;
  error?: string;
  status?: string;  // completed / failed
  diff?: FileDiff;
}

// ACP extension: file location reference
export interface FileLocation {
  path: string;
  line?: number;
}

// ACP extension: structured file diff
export interface FileDiff {
  path: string;
  old_text: string;
  new_text: string;
}

export interface QuestionOption {
  label: string;
  description?: string;
  preview?: string;
}

export interface Question {
  question: string;
  header: string;
  options: QuestionOption[];
  multi_select: boolean;
}

export interface QuestionRequestData {
  id: string;
  tool_name?: string;
  questions: Question[];
}

export interface QuestionResponseData {
  id: string;
  answers: Record<string, string>;
}

export interface ElicitationRequestData {
  id: string;
  mcp_server_name: string;
  message: string;
  mode?: string;
  url?: string;
  elicitation_id?: string;
  requested_schema?: Record<string, unknown>;
}

export interface ElicitationResponseData {
  id: string;
  action: string;
  content?: Record<string, unknown>;
}

export interface RawData {
  kind: string;
  raw: unknown;
}

export interface DoneData {
  success: boolean;
  stats?: DoneStats;
  dropped?: boolean;
}

export interface DoneStats {
  duration_ms?: number;
  tool_calls?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
  total_tokens?: number;
  cost_usd?: number;
  model?: string;
  context_used_percent?: number;
}

export interface MessageData {
  id: string;
  role: string;
  content: string;
  content_type?: string;
  metadata?: Record<string, unknown>;
}

export interface ReasoningData {
  id: string;
  content: string;
  model?: string;
}

export interface StepData {
  id: string;
  step_type: string;
  name?: string;
  input?: Record<string, unknown>;
  output?: Record<string, unknown>;
  parent_id?: string;
  duration?: number;
}

export interface PermissionRequestData {
  id: string;
  tool_name: string;
  description?: string;
  args?: string[];
}

export interface PermissionResponseData {
  id: string;
  allowed: boolean;
  reason?: string;
}

export interface PongData {
  state: SessionState;
}

export interface ContextCategory {
  name: string;
  tokens: number;
}

export interface ContextSkillInfo {
  total: number;
  included: number;
  tokens: number;
  names?: string[];
}

export interface ContextUsageData {
  total_tokens: number;
  max_tokens: number;
  percentage: number;
  model?: string;
  categories?: ContextCategory[];
  memory_files?: number;
  mcp_tools?: number;
  agents?: number;
  skills?: ContextSkillInfo;
}

export interface SkillsListData {
  skills: SkillEntry[];
  total: number;
  filter?: string;
}

export interface SkillEntry {
  name: string;
  description: string;
  source: string;
  /** Invokability for the current session worker: "callable" | "discoverable" | "unavailable". */
  status?: string;
}

export type SkillStatus = "callable" | "discoverable" | "unavailable";

export interface MCPStatusData {
  servers: MCPServerInfo[];
}

export interface MCPServerInfo {
  name: string;
  status: string;
}

export interface WorkerCommandData {
  command: string;
  args?: string;
  extra?: Record<string, unknown>;
}

export interface ToolUpdateData {
  id: string;
  status: string;
  content?: unknown;
  diff?: FileDiff;
  raw_output?: string;
}

export interface PlanData {
  items: PlanItem[];
}

export interface PlanItem {
  content: string;
  priority: string;
  status: string;
}

export interface ModeUpdateData {
  current_mode_id: string;
}

// Durable input acceptance/delivery acknowledgement (from pkg/events/events.go)
export interface InputAckData {
  client_message_id: string;
  execution_id: string;
  status: 'accepted' | 'delivered' | 'unknown' | 'failed';
  duplicate?: boolean;
  error_code?: string;
  /** How the gateway handled the input. Absent on pre-receipt servers. */
  input_mode?: InputMode;
  /**
   * Whether the gateway can recover this input after process failure.
   * `accepted` only means durable when this is `durable`; a `volatile`
   * acceptance is an in-memory staging decision, not a delivery proof.
   */
  durability?: InputDurability;
  /** Set on injected/buffered receipts to correlate with the running turn. */
  parent_execution_id?: string;
}

/** Input handling mode reported by input.ack. */
export type InputMode = 'primary' | 'injected' | 'buffered' | 'queued';

/** Recovery guarantee reported by input.ack. */
export type InputDurability = 'durable' | 'volatile';

// Runtime execution event payload (from pkg/events/events.go)
export interface RuntimeExecutionData {
  execution_id: string;
  status: string;
  error_code?: string;
  started_at?: number;
  finished_at?: number;
}

// Worker in-place reset notification (from pkg/events/events.go)
export interface InternalResetData {
  generation: number;
}

// ============================================================================
// Control Data (from pkg/events/events.go:229-237)
// ============================================================================

export interface ControlData {
  action: ControlAction;
  reason?: string;
  delay_ms?: number;
  recoverable?: boolean;
  suggestion?: ControlSuggestion;
  details?: Record<string, unknown>;
}

export interface ControlSuggestion {
  max_message_rate?: number;
  backoff_ms?: number;
  retry_after?: number;
}

// ============================================================================
// Init Handshake Types (from internal/gateway/init.go)
// ============================================================================

export interface InitData {
  version: string;
  worker_type: WorkerType;
  session_id?: string;
  auth?: InitAuth;
  config?: InitConfig;
  client_caps?: ClientCaps;
}

export interface InitAuth {
  token?: string;
  bot_id?: string;
}

export interface InitConfig {
  model?: string;
  system_prompt?: string;
  allowed_tools?: string[];
  disallowed_tools?: string[];
  max_turns?: number;
  work_dir?: string;
  metadata?: Record<string, unknown>;
}

export interface ClientCaps {
  supports_delta?: boolean;
  supports_tool_call?: boolean;
  supported_kinds?: string[];
}

export interface InitAckData {
  session_id: string;
  state: SessionState;
  server_caps: ServerCaps;
  error?: string;
  code?: ErrorCode;
}

export interface ServerCaps {
  protocol_version: string;
  worker_type: WorkerType;
  supports_resume: boolean;
  supports_delta: boolean;
  supports_tool_call: boolean;
  supports_ping: boolean;
  max_frame_size: number;
  max_turns?: number;
  modalities?: string[];
  tools?: string[];
}

// ============================================================================
// Client Configuration and State
// ============================================================================

export interface HotPlexClientConfig {
  url: string;
  workerType: WorkerType;
  apiKey?: string;
  authToken?: string;
  reconnect?: ReconnectConfig;
  heartbeat?: HeartbeatConfig;
}

export interface ReconnectConfig {
  enabled: boolean;
  maxAttempts?: number;
  baseDelayMs?: number;
  maxDelayMs?: number;
}

export interface HeartbeatConfig {
  pingIntervalMs?: number;
  pongTimeoutMs?: number;
  maxMissedPongs?: number;
}

export interface ClientState {
  sessionId: string | null;
  state: SessionState;
  connected: boolean;
  reconnecting: boolean;
}

// ============================================================================
// Event Maps for Type Safety
// ============================================================================

export interface ServerEventDataMap {
  [EventKind.Error]: ErrorData;
  [EventKind.State]: StateData;
  [EventKind.Done]: DoneData;
  [EventKind.Message]: MessageData;
  [EventKind.MessageStart]: MessageStartData;
  [EventKind.MessageDelta]: MessageDeltaData;
  [EventKind.MessageEnd]: MessageEndData;
  [EventKind.ToolCall]: ToolCallData;
  [EventKind.ToolResult]: ToolResultData;
  [EventKind.Reasoning]: ReasoningData;
  [EventKind.Step]: StepData;
  [EventKind.Raw]: RawData;
  [EventKind.PermissionRequest]: PermissionRequestData;
  [EventKind.Pong]: PongData;
  [EventKind.Control]: ControlData;
  [EventKind.QuestionRequest]: QuestionRequestData;
  [EventKind.QuestionResponse]: QuestionResponseData;
  [EventKind.ElicitationRequest]: ElicitationRequestData;
  [EventKind.ElicitationResponse]: ElicitationResponseData;
  [EventKind.ContextUsage]: ContextUsageData;
  [EventKind.SkillsList]: SkillsListData;
  [EventKind.MCPStatus]: MCPStatusData;
  [EventKind.WorkerCmd]: WorkerCommandData;
  [EventKind.ToolUpdate]: ToolUpdateData;
  [EventKind.Plan]: PlanData;
  [EventKind.ModeUpdate]: ModeUpdateData;
  [EventKind.InputAck]: InputAckData;
  [EventKind.InternalReset]: InternalResetData;
  [EventKind.RuntimeExecutionStarted]: RuntimeExecutionData;
  [EventKind.RuntimeExecutionCompleted]: RuntimeExecutionData;
  [EventKind.RuntimeExecutionFailed]: RuntimeExecutionData;
}

export interface ServerEventEnvelopeMap {
  [EventKind.Error]: Envelope<ErrorData>;
  [EventKind.State]: Envelope<StateData>;
  [EventKind.Done]: Envelope<DoneData>;
  [EventKind.Message]: Envelope<MessageData>;
  [EventKind.MessageStart]: Envelope<MessageStartData>;
  [EventKind.MessageDelta]: Envelope<MessageDeltaData>;
  [EventKind.MessageEnd]: Envelope<MessageEndData>;
  [EventKind.ToolCall]: Envelope<ToolCallData>;
  [EventKind.ToolResult]: Envelope<ToolResultData>;
  [EventKind.Reasoning]: Envelope<ReasoningData>;
  [EventKind.Step]: Envelope<StepData>;
  [EventKind.Raw]: Envelope<RawData>;
  [EventKind.PermissionRequest]: Envelope<PermissionRequestData>;
  [EventKind.Pong]: Envelope<PongData>;
  [EventKind.Control]: Envelope<ControlData>;
  [EventKind.QuestionRequest]: Envelope<QuestionRequestData>;
  [EventKind.QuestionResponse]: Envelope<QuestionResponseData>;
  [EventKind.ElicitationRequest]: Envelope<ElicitationRequestData>;
  [EventKind.ElicitationResponse]: Envelope<ElicitationResponseData>;
  [EventKind.ContextUsage]: Envelope<ContextUsageData>;
  [EventKind.SkillsList]: Envelope<SkillsListData>;
  [EventKind.MCPStatus]: Envelope<MCPStatusData>;
  [EventKind.WorkerCmd]: Envelope<WorkerCommandData>;
  [EventKind.ToolUpdate]: Envelope<ToolUpdateData>;
  [EventKind.Plan]: Envelope<PlanData>;
  [EventKind.ModeUpdate]: Envelope<ModeUpdateData>;
}

// ============================================================================
// Utility Types
// ============================================================================

export type DeepPartial<T> = {
  [P in keyof T]?: T[P] extends object ? DeepPartial<T[P]> : T[P];
};
