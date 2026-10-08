-- WorkerSessionID write semantics:
--   INSERT path: writes worker_session_id on initial session creation (value is
--     empty at this point — the Worker hasn't started yet).
--   UPDATE path (ON CONFLICT SET): intentionally excludes worker_session_id.
--     The authoritative value is set post-launch by UpdateWorkerSessionIDSQL.
--     Including it here would overwrite the targeted UPDATE on every Upsert call
--     (e.g. heartbeat), silently breaking the transitionState guard + safety-net
--     two-layer persistence design.
--   See also: manager.go transitionState guard (WorkerSessionID empty→nonempty).
INSERT INTO sessions (id, user_id, owner_id, bot_id, bot_name, worker_session_id, worker_type, state, platform, platform_key_json, work_dir, title, created_at, updated_at, expires_at, idle_expires_at, context_json, source, client_key, workspace_id, lifecycle_policy, lifecycle_policy_revision, last_input_at, runtime_finished_at, archive_at, conversation_expires_at, last_content_expires_at, history_expires_at, deleted_at)
 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
 WHERE NOT EXISTS (SELECT 1 FROM session_cleanup_tasks WHERE session_id = ?)
 ON CONFLICT(id) DO UPDATE SET
  state=excluded.state,
  owner_id=CASE WHEN excluded.owner_id != '' THEN excluded.owner_id ELSE sessions.owner_id END,
  bot_name=CASE WHEN excluded.bot_name != '' THEN excluded.bot_name ELSE sessions.bot_name END,
  updated_at=excluded.updated_at,
  expires_at=excluded.expires_at,
  idle_expires_at=excluded.idle_expires_at,
  title=CASE WHEN excluded.title != '' THEN excluded.title ELSE sessions.title END,
  context_json=excluded.context_json,
  source=CASE WHEN excluded.source != '' THEN excluded.source ELSE sessions.source END,
  client_key=CASE WHEN excluded.client_key != '' THEN excluded.client_key ELSE sessions.client_key END,
  -- workspace_id 创建后不可变：仅当原值为 NULL 或空串（未绑定）时接受，防后续 upsert 覆盖。
 workspace_id=CASE WHEN sessions.workspace_id IS NULL OR sessions.workspace_id = '' THEN excluded.workspace_id ELSE sessions.workspace_id END
 WHERE sessions.deleted_at IS NULL
   AND sessions.state != 'deleted'
   AND NOT EXISTS (SELECT 1 FROM session_cleanup_tasks WHERE session_id = excluded.id);
