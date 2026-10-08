SELECT
    COALESCE(SUM(CASE WHEN overdue.is_blocked = 0 THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN overdue.is_blocked = 1 THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN overdue.has_unknown_execution = 1 THEN 1 ELSE 0 END), 0),
    MIN(CASE WHEN overdue.is_blocked = 0 THEN overdue.required_deadline END),
    MIN(CASE WHEN overdue.is_blocked = 1 THEN overdue.required_deadline END)
FROM (
    SELECT
        GREATEST(
            s.conversation_expires_at,
            COALESCE(s.last_content_expires_at, s.conversation_expires_at),
            s.history_expires_at
        ) AS required_deadline,
        CASE WHEN s.state = 'running'
                  OR EXISTS (
                      SELECT 1 FROM execution_inputs e
                      WHERE e.session_id = s.id
                        AND (
                            e.status IN ('accepted', 'unknown')
                            OR e.runtime_status IN ('queued', 'pending', 'running', 'unknown')
                            OR e.fence_reason <> ''
                        )
                  )
                  OR EXISTS (
                      SELECT 1 FROM execution_queue q
                      WHERE q.session_id = s.id
                  )
                  OR EXISTS (
                      SELECT 1 FROM session_cleanup_tasks c
                      WHERE c.session_id = s.id
                  )
             THEN 1 ELSE 0 END AS is_blocked,
        CASE WHEN EXISTS (
            SELECT 1 FROM execution_inputs e
            WHERE e.session_id = s.id
              AND (
                  e.status = 'unknown'
                  OR e.runtime_status = 'unknown'
                  OR e.fence_reason <> ''
              )
        ) THEN 1 ELSE 0 END AS has_unknown_execution
    FROM sessions s
    WHERE s.lifecycle_policy = 'v2'
      AND s.deleted_at IS NULL
      AND s.state <> 'deleted'
      AND s.conversation_expires_at IS NOT NULL
      AND s.conversation_expires_at <= $1
      AND (s.last_content_expires_at IS NULL OR s.last_content_expires_at <= $2)
      AND s.history_expires_at IS NOT NULL
      AND s.history_expires_at <= $3
) AS overdue;
