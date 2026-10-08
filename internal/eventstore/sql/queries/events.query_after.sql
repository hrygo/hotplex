SELECT id, session_id, seq, type, data, direction, source, created_at, expires_at
FROM events
WHERE session_id = ? AND id > ?
  AND ((expires_at > 0 AND expires_at > ?) OR (expires_at = 0 AND created_at > ?))
ORDER BY id ASC
LIMIT ?
