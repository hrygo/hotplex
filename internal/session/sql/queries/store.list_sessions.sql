-- list_sessions lists sessions with pagination, excluding soft-deleted.
-- Filters by user_id and platform if provided.
SELECT id, user_id, COALESCE(owner_id, user_id), COALESCE(worker_session_id, ''), worker_type, state, COALESCE(bot_id, ''), COALESCE(bot_name, ''), platform, platform_key_json, COALESCE(work_dir, ''), COALESCE(title, ''), created_at, updated_at, expires_at, idle_expires_at, context_json, source, COALESCE(client_key, ''), COALESCE(workspace_id, ''), COALESCE(permission_ceiling, ''), lifecycle_policy, lifecycle_policy_revision, last_input_at, runtime_finished_at, archive_at, conversation_expires_at, last_content_expires_at, history_expires_at, deleted_at
 FROM sessions
 WHERE state != 'deleted'
   AND (? = '' OR user_id = ?)
   AND (? = '' OR platform = ?)
   AND (? = '' OR workspace_id = ?)
 ORDER BY created_at DESC LIMIT ? OFFSET ?;
