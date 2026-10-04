import type { EventKind, Priority, SessionState, ErrorCode, ControlAction, WorkerType } from './constants';

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
  metadata?: Record<string, any>;
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

export type ExecutionStatus = 'accepted' | 'delivered' | 'unknown' | 'failed';

/** How the gateway handled the input. Absent on pre-receipt servers. */
export type InputMode = 'primary' | 'injected' | 'buffered' | 'queued';

/**
 * Recovery guarantee. `accepted` is only durable when this is `durable`;
 * a volatile acceptance is an in-memory staging decision, not a delivery proof.
 */
export type InputDurability = 'durable' | 'volatile';

export interface InputAckData {
  client_message_id: string;
  execution_id: string;
  status: ExecutionStatus;
  duplicate?: boolean;
  error_code?: ErrorCode;
  input_mode?: InputMode;
  durability?: InputDurability;
  /** Set on injected/buffered receipts to correlate with the running turn. */
  parent_execution_id?: string;
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
}

export interface ToolResultData {
  id: string;
  output: unknown;
  error?: string;
}

export interface RawData {
  kind: string;
  raw: unknown;
}

export interface DoneData {
  success: boolean;
  stats?: DoneStats;
  dropped?: boolean;
  reason?: string;
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
  _session?: TurnSessionStats;
}

export interface TurnSessionStats {
  turn_count: number;
  tool_call_count: number;
  duration: string;
  duration_seconds: number;
  total_input_tok: number;
  total_output_tok: number;
  context_fill: number;
  context_window: number;
  context_pct: number;
  total_cost_usd: number;
  model_name: string;
  turn_duration_ms: number;
  turn_input_tok: number;
  turn_output_tok: number;
  turn_cost_usd: number;
  tool_names: Record<string, number> | null;
  work_dir: string;
  git_branch: string;
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

export interface QuestionOption {
  label: string;
  description?: string;
}

export interface QuestionItem {
  id?: string;
  header?: string;
  question: string;
  options?: QuestionOption[];
  multi_select?: boolean;
}

export interface QuestionRequestData {
  id: string;
  questions: QuestionItem[];
}

export interface QuestionResponseData {
  id: string;
  answers: Record<string, string | string[]>;
}

export interface ElicitationRequestData {
  id: string;
  mcp_server_name: string;
  message: string;
  url?: string;
}

export interface ElicitationResponseData {
  id: string;
  action: 'accept' | 'decline' | 'cancel';
  content?: Record<string, unknown>;
}

export interface PongData {
  state: SessionState;
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
// Worker Command & Context Usage (from pkg/events/events.go:331-365)
// ============================================================================

export interface WorkerCommandData {
  command: string;
  args?: string;
  extra?: Record<string, unknown>;
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

export interface SkillsListData {
  skills: SkillEntry[];
  total: number;
  filter?: string;
}

/** Invokability of a Skill for the current session worker (issue #957). */
export type SkillStatus = "callable" | "discoverable" | "unavailable";

export interface SkillEntry {
  name: string;
  description: string;
  source: string;
  /** Absent → treated as "discoverable" (worker cannot confirm invocation). */
  status?: SkillStatus;
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
  workspace_id?: string;
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
  server_version?: string;
  capabilities?: string[];
  error?: string;
  code?: ErrorCode;
  retryable?: boolean;
  retry_after_ms?: number;
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
  workspaceId?: string;
  reconnect?: ReconnectConfig;
  heartbeat?: HeartbeatConfig;
  /** Per-session init config forwarded to the gateway in the AEP init handshake. */
  initConfig?: InitConfig;
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
  [EventKind.InputAck]: InputAckData;
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
  [EventKind.QuestionRequest]: QuestionRequestData;
  [EventKind.ElicitationRequest]: ElicitationRequestData;
  [EventKind.Pong]: PongData;
  [EventKind.Control]: ControlData;
  [EventKind.ContextUsage]: ContextUsageData;
  [EventKind.SkillsList]: SkillsListData;
}

export interface ServerEventEnvelopeMap {
  [EventKind.Error]: Envelope<ErrorData>;
  [EventKind.State]: Envelope<StateData>;
  [EventKind.InputAck]: Envelope<InputAckData>;
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
  [EventKind.QuestionRequest]: Envelope<QuestionRequestData>;
  [EventKind.ElicitationRequest]: Envelope<ElicitationRequestData>;
  [EventKind.Pong]: Envelope<PongData>;
  [EventKind.Control]: Envelope<ControlData>;
  [EventKind.ContextUsage]: Envelope<ContextUsageData>;
  [EventKind.SkillsList]: Envelope<SkillsListData>;
}

// ============================================================================
// Utility Types

export type DeepPartial<T> = {
  [P in keyof T]?: T[P] extends object ? DeepPartial<T[P]> : T[P];
};
