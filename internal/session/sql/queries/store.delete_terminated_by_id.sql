DELETE FROM sessions WHERE id = ? AND state = ? AND (
    (source = 'cron' AND updated_at <= ?) OR
    (source != 'cron' AND updated_at <= ?)
)
RETURNING id, user_id, COALESCE(owner_id, user_id), COALESCE(worker_session_id, ''), worker_type, state,
          COALESCE(bot_id, ''), COALESCE(bot_name, ''), platform, platform_key_json,
          COALESCE(work_dir, ''), COALESCE(title, ''), created_at, updated_at,
          expires_at, idle_expires_at, context_json, source, COALESCE(client_key, ''),
          COALESCE(workspace_id, ''), COALESCE(permission_ceiling, '');
