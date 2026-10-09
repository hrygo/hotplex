SELECT id FROM sessions WHERE state = ? AND COALESCE(lifecycle_policy, 'legacy') <> 'v2' AND (
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
);
