/**
 * Admin panel type definitions.
 *
 * Covers auth, bot configs, sessions, cron jobs, and gateway stats.
 */

// --- Auth ---

export interface AdminConnection {
  url: string;
  token: string;
}

// --- Bot ---

export interface BotConfigEntry {
  name: string;
  platform: string;
  bot_id: string;
  status: string;
  connected_at?: string;
  config?: BotConfigAttrs;
  agent_configs?: AgentConfigSummary;
}

export interface BotConfigAttrs {
  worker_type?: string;
  work_dir?: string;
  dm_policy?: string;
  group_policy?: string;
  require_mention?: boolean;
  allow_from?: string[];
  allow_dm_from?: string[];
  allow_group_from?: string[];
  stt?: { provider?: string };
  tts?: { provider?: string; voice?: string };
}

export interface AgentConfigSummary {
  soul?: AgentConfigMeta;
  agents?: AgentConfigMeta;
  tools?: AgentConfigMeta;
  user?: AgentConfigMeta;
  memory?: AgentConfigMeta;
}

export interface AgentConfigMeta {
  source: string;
  size: number;
}

export interface AgentConfigFile {
  content: string;
  source: string;
  size: number;
  file: string;
}

// --- Session ---

export interface AdminSessionInfo {
  id: string;
  user_id: string;
  state: string;
  created_at: string;
  updated_at: string;
  worker_type?: string;
  work_dir?: string;
  title?: string;
  turn_count?: number;
}

export interface AdminSessionDetailResponse extends AdminSessionInfo {
  owner_id?: string;
  bot_id?: string;
  bot_name?: string;
  expires_at?: string;
  idle_expires_at?: string;
  context?: Record<string, unknown>;
  worker_session_id?: string;
  allowed_tools?: string[];
  platform?: string;
  platform_key?: Record<string, string>;
  source?: string;
  client_key?: string;
  workspace_id?: string;
  identity_link?: AuditIdentityLink;
  identity_links?: Record<string, AuditIdentityLink>;
}

export interface SessionStatsItem {
  turn_num: number;
  seq: number;
  success: boolean;
  duration_ms: number;
  cost_usd: number;
  tokens_in: number;
  tokens_input: number;
  tokens_cache_write: number;
  tokens_cache_read: number;
  tokens_out: number;
  model?: string;
  source?: string;
  created_at: number;
}

export interface SessionStatsResponse {
  session_id: string;
  generation: number;
  total_turns: number;
  success_turns: number;
  failed_turns: number;
  total_duration_ms: number;
  total_cost_usd: number;
  total_tokens_in: number;
  total_tokens_input: number;
  total_tokens_cache_write: number;
  total_tokens_cache_read: number;
  total_tokens_out: number;
  turns?: SessionStatsItem[];
}

export interface SessionDebugInfo {
  available: boolean;
  has_worker: boolean;
  turn_count: number;
  last_seq_sent: number;
  worker_health?: string;
  runtime_only?: boolean;
  db_turn_count?: number | null;
  db_last_seq?: number | null;
}

export interface SessionDebugResponse {
  session: AdminSessionDetailResponse;
  debug: SessionDebugInfo;
}

// --- Audit Activity ---

export interface AuditActivity {
  id: number;
  chain_epoch?: string;
  ts: number;
  user_id: string;
  user_id_type: string;
  platform: string;
  session_id?: string;
  action: string;
  resource_type?: string;
  resource_id?: string;
  outcome: string;
  detail_json: string;
  event_ref?: string;
  ip?: string;
  user_agent?: string;
  prev_hash: string;
  self_hash: string;
}

export interface AuditActivityResponse {
  rows: AuditActivity[];
  limit: number;
  offset: number;
  user_id?: string;
  principal_user_id?: string;
  resolved_user_ids?: string[];
  identity_links?: Record<string, AuditIdentityLink>;
}

// Audit activity aggregate counts returned by GET /admin/activity/stats.
// total reflects the full filtered set (not just the current page).
export interface ActivityStats {
  total: number;
  by_outcome: Record<string, number>;
  by_platform: Record<string, number>;
}

export interface AuditIdentityLink {
  ID?: string;
  id?: string;
  PrincipalUserID?: string;
  principal_user_id?: string;
  Provider?: string;
  provider?: string;
  Subject?: string;
  subject?: string;
  SubjectType?: string;
  subject_type?: string;
  DisplayName?: string;
  display_name?: string;
  Email?: string;
  email?: string;
  CreatedAt?: number;
  created_at?: number;
  UpdatedAt?: number;
  updated_at?: number;
}

// --- Cron ---

export interface CronSchedule {
  kind: 'at' | 'every' | 'cron';
  at?: string;
  every_ms?: number;
  expr?: string;
  tz?: string;
}

export interface CronPayload {
  kind: string;
  message: string;
  target_session_id?: string;
  allowed_tools?: string[];
  worker_type?: string;
}

export interface CronJobState {
  next_run_at_ms?: number;
  last_run_at_ms?: number;
  run_count?: number;
  last_status?: string;
  consecutive_errors?: number;
}

export interface CronJob {
  id: string;
  name: string;
  description?: string;
  schedule: CronSchedule;
  payload: CronPayload;
  enabled: boolean;
  bot_id?: string;
  owner_id?: string;
  max_runs?: number;
  expires_at?: string;
  state?: CronJobState;
}

export interface CronRunHistoryItem {
  id?: string;
  turn_index?: number;
  generation?: number;
  status?: string;
  error?: string;
  created_at?: string;
  duration_ms?: number;
  tokens_used?: number;
  [key: string]: unknown;
}

// --- API Key ---

export interface APIKeyUser {
  id?: number;
  api_key: string;
  user_id: string;
  description?: string;
  created_at?: string;
  updated_at?: string;
}

// --- Workspace (admin console, issue #807) ---

// AdminWorkspace is the admin console projection of a workspace: the full row
// plus the owner's readable identity (display_name + username) joined server-side
// (GET /admin/workspaces). permission_mode "" = no explicit override (config default).
export interface AdminWorkspace {
  id: string;
  owner_user_id: string;
  owner_display_name: string;
  owner_username: string;
  name: string;
  work_dir: string;
  agent_config_overrides: string;
  worker_preference: string;
  permission_mode: string;
  status: string;
  created_at: number;
  updated_at: number;
}

// --- Execution console (operator timeline) ---

// EvidenceState separates "we looked and there is nothing" from "we never
// recorded it". The console must render the latter as missing history, never
// as a success or a failure.
export type EvidenceState = 'recorded' | 'not_recorded_for_this_run' | 'unavailable';

// AdminExecutionSummary is the control-fact row for one run. delivery_status
// (was the input accepted and handed over) and runtime_status (did the Worker
// actually finish) are separate on purpose: conflating them is what makes
// "the agent answered" get read as "the message was delivered".
export interface AdminExecutionSummary {
  execution_id: string;
  session_id: string;
  delivery_status: string;
  runtime_status: string;
  runtime_error_code?: string;
  worker_run_id?: string;
  fence_reason?: string;
  fence_version: number;
  created_at: number;
  updated_at: number;
  started_at?: number;
  finished_at?: number;
  fence_created_at?: number;
}

// AdminExecutionCursor is a keyset cursor, not an offset: new rows arriving
// while an operator pages cannot make an earlier page repeat or skip rows.
export interface AdminExecutionCursor {
  before_created_at: number;
  before_execution_id: string;
}

export interface AdminExecutionListResponse {
  executions: AdminExecutionSummary[];
  next_cursor: AdminExecutionCursor | null;
}

// AdminTimelineItem is one bounded, redacted fact. fact_time is when the
// recording system saw it happen; observed_at is when this projection read it.
// They differ legitimately (a late receipt, a delayed Done) and collapsing
// them hides exactly the lag an operator is diagnosing.
export interface AdminTimelineItem {
  phase: string;
  source: string;
  kind: string;
  fact_time: number;
  observed_at: number;
  execution_id?: string;
  worker_run_id?: string;
  effect_id?: string;
  evidence?: string;
  evidence_state?: EvidenceState;
  truncated?: boolean;
}

// AdminAvailableAction is an action the SERVER says is valid right now, with
// the conditional token it must be submitted with. The console renders these
// and never derives an action of its own — there is no "mark as successful".
export interface AdminAvailableAction {
  kind: string;
  target: string;
  requires_version?: number;
  description: string;
}

export interface AdminExecutionTimeline {
  execution: AdminExecutionSummary;
  items: AdminTimelineItem[];
  actions: AdminAvailableAction[];
  plan_evidence: EvidenceState;
  effect_evidence: EvidenceState;
  truncated: boolean;
  notes?: string[];
}
