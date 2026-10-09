SELECT s.id
FROM sessions s
WHERE s.lifecycle_policy = 'v2'
  AND s.deleted_at IS NULL
  AND s.state NOT IN ('running', 'deleted')
  AND s.conversation_expires_at IS NOT NULL
  AND s.conversation_expires_at <= ?
  AND (s.last_content_expires_at IS NULL OR s.last_content_expires_at <= ?)
  AND s.history_expires_at IS NOT NULL
  AND s.history_expires_at <= ?
  AND NOT EXISTS (
      SELECT 1 FROM execution_inputs e
      WHERE e.session_id = s.id
        AND (
            e.status IN ('accepted', 'unknown')
            OR e.runtime_status IN ('queued', 'pending', 'running', 'unknown')
            OR e.fence_reason <> ''
        )
  )
  AND NOT EXISTS (
      SELECT 1 FROM execution_queue q WHERE q.session_id = s.id
  )
  AND NOT EXISTS (
      SELECT 1 FROM session_cleanup_tasks c WHERE c.session_id = s.id
  )
ORDER BY s.conversation_expires_at ASC, s.id ASC
LIMIT ?;
