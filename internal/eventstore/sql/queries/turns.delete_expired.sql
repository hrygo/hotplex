DELETE FROM turns
WHERE (expires_at > 0 AND expires_at <= ?)
   OR (expires_at = 0 AND created_at < ?)
