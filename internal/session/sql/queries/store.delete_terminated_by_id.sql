DELETE FROM sessions WHERE id = ? AND state = ? AND COALESCE(lifecycle_policy, 'legacy') <> 'v2' AND (
    (source = 'cron' AND updated_at <= ?) OR
    (source != 'cron' AND updated_at <= ?)
)
AND NOT EXISTS (
    SELECT 1 FROM execution_inputs e
    WHERE e.session_id = sessions.id
      AND (
          e.status IN ('accepted', 'unknown')
          OR e.runtime_status IN ('queued', 'pending', 'running', 'unknown')
          OR e.fence_reason <> ''
      )
)
AND NOT EXISTS (
    SELECT 1 FROM execution_queue q WHERE q.session_id = sessions.id
)
AND NOT EXISTS (
    SELECT 1 FROM session_cleanup_tasks c WHERE c.session_id = sessions.id
)
RETURNING id, user_id, COALESCE(owner_id, user_id), COALESCE(worker_session_id, ''), worker_type, state,
          COALESCE(bot_id, ''), COALESCE(bot_name, ''), platform, platform_key_json,
          COALESCE(work_dir, ''), COALESCE(title, ''), created_at, updated_at,
          expires_at, idle_expires_at, context_json, source, COALESCE(client_key, ''),
          COALESCE(workspace_id, ''), COALESCE(permission_ceiling, ''),
          lifecycle_policy, lifecycle_policy_revision, last_input_at, runtime_finished_at,
          archive_at, conversation_expires_at, last_content_expires_at, history_expires_at,
          deleted_at;
