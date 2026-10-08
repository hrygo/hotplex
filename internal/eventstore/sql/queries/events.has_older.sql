SELECT 1
FROM events
WHERE session_id = ? AND id < ?
  AND ((expires_at > 0 AND expires_at > ?) OR (expires_at = 0 AND created_at > ?))
LIMIT 1
